package contract

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// recorder is a fake Runner: it records the call, writes outputs the way
// @actions/core does, and prints log.
type recorder struct {
	dir     string
	env     map[string]string
	args    []string
	outputs string
	log     string
	err     error
}

func (r *recorder) Run(_ context.Context, dir string, env []string, args ...string) ([]byte, error) {
	r.dir, r.args, r.env = dir, args, map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		r.env[k] = v
	}
	if r.outputs != "" {
		if err := os.WriteFile(r.env["GITHUB_OUTPUT"], []byte(r.outputs), 0o600); err != nil {
			return nil, err
		}
	}
	return []byte(r.log), r.err
}

func actionCheckout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"save-only", "restore-only"} {
		p := filepath.Join(dir, "dist", d, "index.js")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestActionRestoreStep(t *testing.T) {
	t.Setenv("ACTIONS_RUNTIME_TOKEN", "the-real-runner-token")
	t.Setenv("ACTIONS_RESULTS_URL", "https://results.github.example/")
	rec := &recorder{
		outputs: "cache-hit=false\ncache-matched-key<<ghadelimiter_1\nk-old\nghadelimiter_1\n",
		log:     "Cache restored\n::warning::Failed to restore: 500%0Aretry\n",
	}
	target := testTarget()
	target.Jobs.PR1.Token = "tok-pr1"
	dir := actionCheckout(t)
	c := &ActionClient{Dir: dir, Version: "v9.9.9", Runner: rec, Target: target}
	// The step sees the resolved path (on macOS /var -> /private/var).
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Restore(context.Background(), target.Jobs.PR1, ws, "k-new", []string{"k-", "j-"}, []string{"data", "more"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != "actions/cache@v9.9.9" {
		t.Errorf("name %q", c.Name())
	}
	if rec.dir != ws || !reflect.DeepEqual(rec.args, []string{filepath.Join(dir, "dist", "restore-only", "index.js")}) {
		t.Errorf("ran %v in %s", rec.args, rec.dir)
	}
	want := map[string]string{
		"ACTIONS_RESULTS_URL":        "http://cache.test/",
		"ACTIONS_RUNTIME_TOKEN":      "tok-pr1",
		"ACTIONS_CACHE_SERVICE_V2":   "true",
		"GITHUB_REF":                 "refs/pull/1/merge",
		"GITHUB_EVENT_NAME":          "pull_request",
		"GITHUB_SERVER_URL":          "https://github.com",
		"GITHUB_REPOSITORY":          "acme/api",
		"GITHUB_WORKSPACE":           ws,
		"INPUT_KEY":                  "k-new",
		"INPUT_PATH":                 "data\nmore",
		"INPUT_RESTORE-KEYS":         "k-\nj-",
		"INPUT_FAIL-ON-CACHE-MISS":   "false",
		"INPUT_LOOKUP-ONLY":          "false",
		"INPUT_ENABLECROSSOSARCHIVE": "false",
	}
	for k, v := range want {
		if rec.env[k] != v {
			t.Errorf("%s = %q, want %q", k, rec.env[k], v)
		}
	}
	for _, k := range []string{"GITHUB_STATE", "RUNNER_TEMP", "PATH"} {
		if rec.env[k] == "" {
			t.Errorf("%s not set", k)
		}
	}
	if res.Hit() || res.MatchedKey() != "k-old" {
		t.Errorf("outputs %v", res.Outputs)
	}
	if p := res.Problems(); len(p) != 1 || p[0].Message != "Failed to restore: 500\nretry" {
		t.Errorf("problems %v", p)
	}
}

func TestActionSaveStep(t *testing.T) {
	rec := &recorder{}
	target := testTarget()
	c := &ActionClient{Dir: actionCheckout(t), Runner: rec, Target: target}
	if _, err := c.Save(context.Background(), target.Jobs.Main, t.TempDir(), "k", []string{"data"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(rec.args[0], filepath.Join("save-only", "index.js")) {
		t.Errorf("ran %v", rec.args)
	}
	if rec.env["GITHUB_REF"] != "refs/heads/main" || rec.env["GITHUB_EVENT_NAME"] != "push" || rec.env["INPUT_KEY"] != "k" {
		t.Errorf("env %v", rec.env)
	}
	if _, ok := rec.env["INPUT_RESTORE-KEYS"]; ok {
		t.Error("save step given restore-keys")
	}
}

func TestActionClientNeedsCheckout(t *testing.T) {
	c := &ActionClient{Dir: t.TempDir(), Runner: &recorder{}, Target: testTarget()}
	if _, err := c.Save(context.Background(), testTarget().Jobs.Main, t.TempDir(), "k", paths); err == nil {
		t.Fatal("ran without a checkout")
	}
}

func TestStepFailureKeepsLogAndOutputs(t *testing.T) {
	rec := &recorder{outputs: "cache-hit=false\n", log: "::error::boom\n", err: errors.New("exit status 1")}
	c := &ActionClient{Dir: actionCheckout(t), Runner: rec, Target: testTarget()}
	res, err := c.Restore(context.Background(), testTarget().Jobs.Main, t.TempDir(), "k", nil, paths)
	if err == nil || !strings.Contains(err.Error(), "restore-only/index.js") {
		t.Fatalf("err = %v", err)
	}
	if string(res.Log) != "::error::boom\n" || res.Outputs["cache-hit"] != "false" || len(res.Problems()) != 1 {
		t.Fatalf("result %+v", res)
	}
}

func TestToolkitClient(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "node_modules", "@actions", "cache", "package.json")
	if err := os.MkdirAll(filepath.Dir(pkg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pkg, []byte(`{"name":"@actions/cache","version":"6.3.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{outputs: "cache-hit=true\ncache-matched-key=k\n"}
	target := testTarget()
	target.Jobs.Fork.Token = "tok-fork"
	c := &ToolkitClient{Dir: dir, Runner: rec, Target: target}
	if c.Name() != "@actions/cache@6.3.0" {
		t.Errorf("name %q", c.Name())
	}
	res, err := c.Restore(context.Background(), target.Jobs.Fork, t.TempDir(), "k", []string{"k-"}, []string{"data"})
	if err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(dir, driverName)
	if !reflect.DeepEqual(rec.args, []string{driver, "restore", "data", "k", "k-"}) {
		t.Errorf("args %q", rec.args)
	}
	if b, err := os.ReadFile(driver); err != nil || !strings.Contains(string(b), "@actions/cache") {
		t.Errorf("driver not written next to node_modules: %v", err)
	}
	if rec.env["ACTIONS_RUNTIME_TOKEN"] != "tok-fork" || rec.env["GITHUB_REF"] != "refs/pull/3/merge" {
		t.Errorf("env %v", rec.env)
	}
	if !res.Hit() || res.MatchedKey() != "k" {
		t.Errorf("outputs %v", res.Outputs)
	}
	if _, err := c.Save(context.Background(), target.Jobs.Main, t.TempDir(), "k", []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rec.args, []string{driver, "save", "a\nb", "k"}) {
		t.Errorf("save args %q", rec.args)
	}
	if (&ToolkitClient{Dir: t.TempDir()}).Name() != "@actions/cache" {
		t.Error("name without an install")
	}
}

func TestParseOutputFile(t *testing.T) {
	got, err := parseOutputFile([]byte("a=1\r\nb<<EOF\nx\ny=z\nEOF\n\nc=with=equals\nd=\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "1", "b": "x\ny=z", "c": "with=equals", "d": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	for _, bad := range []string{"noequals\n", "b<<EOF\nx\n", "b<<\nx\n", "=v\n"} {
		if _, err := parseOutputFile([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseAnnotations(t *testing.T) {
	log := strings.Join([]string{
		"plain line",
		"::warning::w1",
		"  ::error file=a.js,line=3::e1 %25 done",
		"::warningish::not one",
		"::debug::not one",
		"::notice::not one",
		"::warning no-terminator",
	}, "\n")
	got := parseAnnotations([]byte(log))
	want := []Annotation{{"warning", "w1"}, {"error", "e1 % done"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestInputEnv(t *testing.T) {
	for in, want := range map[string]string{"key": "INPUT_KEY", "restore-keys": "INPUT_RESTORE-KEYS", "my input": "INPUT_MY_INPUT"} {
		if got := inputEnv(in); got != want {
			t.Errorf("inputEnv(%q) = %q, want %q", in, got, want)
		}
	}
}
