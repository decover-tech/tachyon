package gitmirror

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(baseEnv(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// upstreamRepo creates <root>/acme/api.git with one commit and returns the
// work tree used to push more.
func upstreamRepo(t *testing.T, root string) string {
	bare := filepath.Join(root, "acme", "api.git")
	os.MkdirAll(bare, 0o755)
	git(t, bare, "init", "--bare", "--quiet", "-b", "main")
	work := t.TempDir()
	git(t, work, "init", "--quiet", "-b", "main")
	os.WriteFile(filepath.Join(work, "README"), []byte("v1\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", "v1")
	git(t, work, "remote", "add", "origin", bare)
	git(t, work, "push", "--quiet", "origin", "main")
	return work
}

func TestMirrorServesFetchesAndRefreshes(t *testing.T) {
	needGit(t)
	up := t.TempDir()
	work := upstreamRepo(t, up)
	m := &Mirror{
		Dir:        t.TempDir(),
		Upstream:   "file://" + up,
		Allowed:    AllowList([]string{"Acme/API"}),
		MinRefresh: time.Nanosecond,
	}
	srv := httptest.NewServer(m)
	defer srv.Close()

	clone := filepath.Join(t.TempDir(), "c")
	git(t, "", "clone", "--quiet", srv.URL+"/acme/api", clone)
	if b, _ := os.ReadFile(filepath.Join(clone, "README")); string(b) != "v1\n" {
		t.Fatalf("README = %q", b)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "acme", "api.git", "HEAD")); err != nil {
		t.Fatal("mirror not created on disk")
	}

	// A commit pushed upstream is visible on the next fetch through the
	// mirror, including fetch by SHA as actions/checkout does.
	os.WriteFile(filepath.Join(work, "README"), []byte("v2\n"), 0o644)
	git(t, work, "commit", "--quiet", "-am", "v2")
	git(t, work, "push", "--quiet", "origin", "main")
	sha := git(t, work, "rev-parse", "HEAD")
	git(t, clone, "fetch", "--quiet", "origin", sha)
	if got := git(t, clone, "rev-parse", "FETCH_HEAD"); got != sha {
		t.Errorf("fetched %s, want %s", got, sha)
	}
}

func TestMirrorRedirectsWhatItCannotServe(t *testing.T) {
	needGit(t)
	up := t.TempDir()
	upstreamRepo(t, up)
	m := &Mirror{
		Dir:      t.TempDir(),
		Upstream: "https://github.example",
		Allowed:  AllowList([]string{"acme/api", "acme/gone"}),
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	for _, c := range []struct {
		path, why string
	}{
		{"/other/repo/info/refs?service=git-upload-pack", "not on the allowlist"},
		// Upstream unreachable (or private): clone fails, client goes upstream.
		{"/acme/gone.git/info/refs?service=git-upload-pack", "refresh failed"},
		{"/acme/api/info/refs", "dumb protocol"},
	} {
		resp, err := client.Get(srv.URL + c.path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc := resp.Header.Get("Location")
		if resp.StatusCode != http.StatusTemporaryRedirect || !strings.HasPrefix(loc, "https://github.example/") {
			t.Errorf("%s: %d %q, want redirect upstream", c.why, resp.StatusCode, loc)
		}
	}
	resp, _ := client.Get(srv.URL + "/other/repo/info/refs?service=git-upload-pack")
	if got := resp.Header.Get("Location"); got != "https://github.example/other/repo.git/info/refs?service=git-upload-pack" {
		t.Errorf("Location = %q", got)
	}

	for _, p := range []string{
		"/acme/api/info/refs?service=git-receive-pack",
		"/acme/api/git-receive-pack",
	} {
		resp, err := client.Post(srv.URL+p, "application/x-git-receive-pack-request", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("push %s = %d, want 403", p, resp.StatusCode)
		}
	}
	for _, p := range []string{"/../etc/info/refs", "/acme/.hidden/info/refs", "/acme/api/config"} {
		resp, err := client.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p, resp.StatusCode)
		}
	}
}

func TestNoAllowListMirrorsNothing(t *testing.T) {
	m := &Mirror{Dir: t.TempDir(), Upstream: "https://github.com"}
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/acme/api/info/refs?service=git-upload-pack", nil))
	if rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("code = %d", rec.Code)
	}
}

// With no allowlist (tachyon-mirror without -git-repos) every request is
// redirected upstream rather than answered with a 404.
func TestMirrorWithoutAllowListRedirectsEverything(t *testing.T) {
	m := &Mirror{Dir: t.TempDir(), Upstream: "https://github.com"}
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/acme/api/info/refs?service=git-upload-pack"},
		{http.MethodPost, "/acme/api/git-upload-pack"},
	} {
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))
		if rec.Code != http.StatusTemporaryRedirect {
			t.Fatalf("%s %s = %d, want 307", tc.method, tc.target, rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://github.com/acme/api.git/") {
			t.Errorf("Location = %q", loc)
		}
	}
}
