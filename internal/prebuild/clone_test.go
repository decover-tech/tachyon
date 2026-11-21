package prebuild

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloneCommandsRejects(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	bad := map[string][2]string{
		"http":        {"http://github.com/acme/api.git", sha},
		"userinfo":    {"https://x:tok@github.com/acme/api.git", sha},
		"query":       {"https://github.com/acme/api.git?x=1", sha},
		"quote":       {"https://github.com/acme/a'pi.git", sha},
		"space":       {"https://github.com/acme/a%20pi.git", sha},
		"short sha":   {"https://github.com/acme/api.git", "abc123"},
		"branch name": {"https://github.com/acme/api.git", "main"},
		"injection":   {"https://github.com/acme/api.git", sha + ";rm -rf /"},
	}
	for name, in := range bad {
		if _, err := CloneCommands(in[0], in[1]); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	cmds, err := CloneCommands("https://github.com/acme/api.git", sha)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cmds {
		if strings.Contains(c, "git config") && !strings.Contains(c, "--get-regexp") {
			t.Errorf("clone must not write git config: %q", c)
		}
	}
}

// git runs git in dir with a hermetic config.
func git(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func runClone(t *testing.T, ws string, env []string, cmds []string) error {
	t.Helper()
	for _, c := range cmds {
		cmd := exec.Command("bash", "-euo", "pipefail", "-c", c)
		cmd.Dir = ws
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			return &cloneErr{c, err, out}
		}
	}
	return nil
}

type cloneErr struct {
	cmd string
	err error
	out []byte
}

func (e *cloneErr) Error() string { return e.cmd + ": " + e.err.Error() + ": " + string(e.out) }

// TestCloneCommandsLeaveNoCredentials runs the clone commands with real git
// against a local repo (the https URL is rewritten to it with insteadOf) and
// checks the token is nowhere on disk afterwards, then that a refresh on the
// same workspace fetches a newer commit.
func TestCloneCommandsLeaveNoCredentials(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	src := filepath.Join(root, "src")
	ws := filepath.Join(root, "ws")
	for _, d := range []string{home, src, ws} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const url = "https://github.com/acme/api.git"
	gitconfig := "[url \"file://" + src + "\"]\n\tinsteadOf = " + url + "\n[user]\n\tname = t\n\temail = t@example.com\n[init]\n\tdefaultBranch = main\n[protocol \"file\"]\n\tallow = always\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gitconfig), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1"}
	gitEnv := append([]string{}, env...)

	git(t, src, gitEnv, "init", "-q")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one"), 0o644)
	git(t, src, gitEnv, "add", ".")
	git(t, src, gitEnv, "commit", "-qm", "one")
	c1 := git(t, src, gitEnv, "rev-parse", "HEAD")

	const token = "ghs_SUPERSECRETTOKEN0123456789abcdefABCD"
	cloneEnv := append(append([]string{}, env...), CloneTokenEnv+"="+token)

	cmds, err := CloneCommands(url, c1)
	if err != nil {
		t.Fatal(err)
	}
	if err := runClone(t, ws, cloneEnv, cmds); err != nil {
		t.Fatal(err)
	}
	if got := git(t, ws, gitEnv, "rev-parse", "HEAD"); got != c1 {
		t.Fatalf("HEAD = %s, want %s", got, c1)
	}
	assertNoToken(t, ws, token)

	// Refresh: a newer commit on the same workspace (a fork of the previous
	// prebuild), with an untracked warm path that must survive.
	os.WriteFile(filepath.Join(ws, "node_modules"), []byte("warm"), 0o644)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("two"), 0o644)
	git(t, src, gitEnv, "commit", "-qam", "two")
	c2 := git(t, src, gitEnv, "rev-parse", "HEAD")
	cmds, _ = CloneCommands(url, c2)
	if err := runClone(t, ws, cloneEnv, cmds); err != nil {
		t.Fatal(err)
	}
	if got := git(t, ws, gitEnv, "rev-parse", "HEAD"); got != c2 {
		t.Fatalf("after refresh HEAD = %s, want %s", got, c2)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "node_modules")); string(b) != "warm" {
		t.Error("refresh removed an untracked warm path")
	}
	assertNoToken(t, ws, token)

	// The final check fails the build if credentials did end up in
	// .git/config.
	git(t, ws, gitEnv, "config", "--local", "http.https://github.com/.extraheader", "AUTHORIZATION: basic x")
	if err := runClone(t, ws, cloneEnv, cmds[len(cmds)-1:]); err == nil {
		t.Error("verify step passed with an extraheader in .git/config")
	}
	git(t, ws, gitEnv, "config", "--local", "--unset", "http.https://github.com/.extraheader")
	git(t, ws, gitEnv, "remote", "set-url", "origin", "https://x-access-token:abc@github.com/acme/api.git")
	if err := runClone(t, ws, cloneEnv, cmds[len(cmds)-1:]); err == nil {
		t.Error("verify step passed with credentials in the remote URL")
	}
}

func assertNoToken(t *testing.T, ws, token string) {
	t.Helper()
	cfg, err := os.ReadFile(filepath.Join(ws, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(cfg), []byte("extraheader")) {
		t.Errorf(".git/config has an auth header:\n%s", cfg)
	}
	filepath.WalkDir(ws, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		if bytes.Contains(b, []byte(token)) {
			t.Errorf("token written to %s", p)
		}
		return nil
	})
}
