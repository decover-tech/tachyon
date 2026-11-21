// Package gitmirror is the per-region git mirror of repos Tachyon builds.
// Guests fetch through it via url.insteadOf (mirrors.Endpoints.GitConfig).
//
// The mirror serves fetches only (smart HTTP, git-upload-pack) and fetches
// upstream anonymously, so only public repos are ever mirrored. For any
// repo it can't serve — not on the allowlist, private, upstream refused, or
// the refresh failed — it redirects the client to the upstream repo, which
// git follows on its first request. A mirror problem never fails a fetch.
package gitmirror

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Mirror serves mirrored repos over git's smart HTTP protocol.
type Mirror struct {
	// Dir holds the bare mirrors, as <owner>/<name>.git.
	Dir string
	// Upstream is the base URL repos are fetched from and redirected to,
	// e.g. https://github.com.
	Upstream string
	// Allowed reports whether repo (owner/name) may be mirrored. nil
	// mirrors nothing: every request is redirected upstream.
	Allowed func(repo string) bool
	// MinRefresh coalesces upstream fetches: a repo fetched less than this
	// long ago is served as is. Default 2s. Kept short because a job may
	// ask for a commit pushed seconds before it started.
	MinRefresh time.Duration
	// FetchTimeout bounds one clone or fetch. Default 2m.
	FetchTimeout time.Duration
	// Git is the git binary. Default "git".
	Git string
	Log *slog.Logger
	Now func() time.Time

	mu    sync.Mutex
	repos map[string]*repoState
}

type repoState struct {
	mu      sync.Mutex
	fetched time.Time
}

var (
	segment = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	// /<owner>/<name>[.git]/<service path>
	reqPath = regexp.MustCompile(`^/([^/]+)/([^/]+?)(?:\.git)?/(info/refs|git-upload-pack|git-receive-pack|HEAD|objects/.*)$`)
)

func (m *Mirror) git() string {
	if m.Git != "" {
		return m.Git
	}
	return "git"
}

func (m *Mirror) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Mirror) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.New(slog.DiscardHandler)
}

// AllowList returns an Allowed func for a fixed set of owner/name repos
// (case-insensitive, as on GitHub).
func AllowList(repos []string) func(string) bool {
	set := map[string]bool{}
	for _, r := range repos {
		if r = strings.TrimSpace(r); r != "" {
			set[strings.ToLower(r)] = true
		}
	}
	return func(repo string) bool { return set[strings.ToLower(repo)] }
}

func (m *Mirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sm := reqPath.FindStringSubmatch(r.URL.Path)
	if sm == nil || !segment.MatchString(sm[1]) || !segment.MatchString(sm[2]) ||
		strings.HasPrefix(sm[1], ".") || strings.HasPrefix(sm[2], ".") {
		http.NotFound(w, r)
		return
	}
	owner, name, rest := sm[1], sm[2], sm[3]
	repo := owner + "/" + name
	service := r.URL.Query().Get("service")

	// Pushes never touch the mirror. Guests are configured with
	// pushInsteadOf, so this only catches misconfiguration.
	if rest == "git-receive-pack" || service == "git-receive-pack" {
		http.Error(w, "the git mirror is fetch-only; push to upstream", http.StatusForbidden)
		return
	}
	smart := (rest == "info/refs" && service == "git-upload-pack") ||
		(rest == "git-upload-pack" && r.Method == http.MethodPost)
	if !smart || m.Allowed == nil || !m.Allowed(repo) {
		m.redirect(w, r, repo, rest)
		return
	}

	// Refresh on ref advertisement, the start of every fetch; the
	// upload-pack POSTs that follow use what that refresh brought in.
	if rest == "info/refs" {
		if err := m.refresh(r.Context(), owner, name); err != nil {
			m.log().Info("git mirror refresh failed; redirecting upstream", "repo", repo, "err", err)
			m.redirect(w, r, repo, rest)
			return
		}
	} else if !m.exists(owner, name) {
		m.redirect(w, r, repo, rest)
		return
	}
	m.serve(w, r, owner, name, rest)
}

func (m *Mirror) redirect(w http.ResponseWriter, r *http.Request, repo, rest string) {
	u := strings.TrimSuffix(m.Upstream, "/") + "/" + repo + ".git/" + rest
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	// 307 keeps POST bodies intact; git follows redirects on its first
	// (info/refs) request and then talks to upstream directly.
	http.Redirect(w, r, u, http.StatusTemporaryRedirect)
}

func (m *Mirror) repoDir(owner, name string) string {
	return filepath.Join(m.Dir, strings.ToLower(owner), strings.ToLower(name)+".git")
}

func (m *Mirror) exists(owner, name string) bool {
	_, err := os.Stat(filepath.Join(m.repoDir(owner, name), "HEAD"))
	return err == nil
}

func (m *Mirror) state(key string) *repoState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.repos == nil {
		m.repos = map[string]*repoState{}
	}
	s, ok := m.repos[key]
	if !ok {
		s = &repoState{}
		m.repos[key] = s
	}
	return s
}

// refresh clones the repo on first use and fetches it after that,
// coalescing concurrent requests.
func (m *Mirror) refresh(ctx context.Context, owner, name string) error {
	dir := m.repoDir(owner, name)
	s := m.state(dir)
	s.mu.Lock()
	defer s.mu.Unlock()
	minRefresh := m.MinRefresh
	if minRefresh <= 0 {
		minRefresh = 2 * time.Second
	}
	if !s.fetched.IsZero() && m.now().Sub(s.fetched) < minRefresh && m.exists(owner, name) {
		return nil
	}
	timeout := m.FetchTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	src := strings.TrimSuffix(m.Upstream, "/") + "/" + owner + "/" + name + ".git"
	if m.exists(owner, name) {
		// The --mirror refspec (+refs/*:refs/*) includes refs/pull/*, which
		// actions/checkout fetches for pull_request jobs.
		if err := m.run(ctx, dir, "fetch", "--prune", "--quiet", "origin"); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		tmp, err := os.MkdirTemp(filepath.Dir(dir), ".clone-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		if err := m.run(ctx, "", "clone", "--mirror", "--quiet", src, tmp); err != nil {
			return err
		}
		// Clients fetch by commit SHA (actions/checkout does).
		if err := m.run(ctx, tmp, "config", "uploadpack.allowReachableSHA1InWant", "true"); err != nil {
			return err
		}
		if err := m.run(ctx, tmp, "config", "uploadpack.allowFilter", "true"); err != nil {
			return err
		}
		if err := os.Rename(tmp, dir); err != nil {
			return err
		}
	}
	s.fetched = m.now()
	return nil
}

// run runs git anonymously: no credential helpers, no prompts, no system or
// user config. Only public repos can be cloned this way.
func (m *Mirror) run(ctx context.Context, dir string, args ...string) error {
	full := append([]string{"-c", "credential.helper=", "-c", "core.askPass="}, args...)
	cmd := exec.CommandContext(ctx, m.git(), full...)
	cmd.Dir = dir
	cmd.Env = append(baseEnv(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func baseEnv() []string {
	env := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "HOME=" + os.TempDir()}
	if p := os.Getenv("PATH"); p != "" {
		env = append(env, "PATH="+p)
	}
	return env
}

// serve hands the request to git http-backend.
func (m *Mirror) serve(w http.ResponseWriter, r *http.Request, owner, name, rest string) {
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + strings.ToLower(owner) + "/" + strings.ToLower(name) + ".git/" + rest
	bin, err := exec.LookPath(m.git()) // CGI needs a path, not a name
	if err != nil {
		m.redirect(w, r, owner+"/"+name, rest)
		return
	}
	h := &cgi.Handler{
		Path: bin,
		Root: "/",
		Args: []string{"http-backend"},
		Env: append(baseEnv(),
			"GIT_PROJECT_ROOT="+m.Dir,
			"GIT_HTTP_EXPORT_ALL=1",
		),
		Stderr: os.Stderr,
	}
	h.ServeHTTP(w, r2)
}
