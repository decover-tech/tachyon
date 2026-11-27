package guest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const jitConfig = `{"jit":"not-a-real-config"}`

type fakeSys struct {
	mu        sync.Mutex
	disks     map[string]string // serial -> device
	formatted []string
	mounts    []string
	chowns    map[string]bool
	runs      []Command
	run       func(c Command) (int, error)
	poweroff  bool
	// quiesced is the dirs passed to Quiesce; quiesceErr is its result.
	quiesced   []string
	quiesceErr error
	// stopped is the dirs passed to StopProcesses; onStop runs inside it.
	stopped []string
	onStop  func()
}

func newFakeSys() *fakeSys {
	return &fakeSys{
		disks:  map[string]string{"job": "/dev/vdb", "sticky-docker": "/dev/vdc", "sticky-cache": "/dev/vdd"},
		chowns: map[string]bool{},
	}
}

func (f *fakeSys) DiskBySerial(_ context.Context, serial string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.disks[serial]; ok {
		return d, nil
	}
	return "", errors.New("timed out")
}

func (f *fakeSys) EnsureFilesystem(_ context.Context, dev string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.formatted = append(f.formatted, dev)
	return nil
}

func (f *fakeSys) Mount(source, target, fstype, data string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounts = append(f.mounts, strings.TrimSpace(fmt.Sprintf("%s %s %s %s", fstype, source, target, data)))
	return nil
}

func (f *fakeSys) Chown(path string, uid, gid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if uid != 1001 || gid != 1001 {
		return fmt.Errorf("unexpected owner %d:%d", uid, gid)
	}
	f.chowns[path] = true
	return nil
}

func (f *fakeSys) Run(_ context.Context, c Command) (int, error) {
	f.mu.Lock()
	f.runs = append(f.runs, c)
	run := f.run
	f.mu.Unlock()
	if run == nil {
		return 0, nil
	}
	return run(c)
}

func (f *fakeSys) Quiesce(_ context.Context, dirs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quiesced = append(f.quiesced, dirs...)
	return f.quiesceErr
}

func (f *fakeSys) StopProcesses(_ context.Context, dirs []string) {
	f.mu.Lock()
	f.stopped = append(f.stopped, dirs...)
	onStop := f.onStop
	f.mu.Unlock()
	if onStop != nil {
		onStop()
	}
}

func (f *fakeSys) PowerOff() error {
	f.poweroff = true
	return nil
}

// runnerOutput fakes the GitHub runner printing its console lines.
func runnerOutput(lines string, code int) func(Command) (int, error) {
	return func(c Command) (int, error) {
		// Write in awkward pieces to exercise line reassembly.
		for _, part := range strings.SplitAfter(lines, "o") {
			_, _ = io.WriteString(c.Stdout, part)
		}
		return code, nil
	}
}

const successLines = "√ Connected to GitHub\n" +
	"2026-11-21 10:00:00Z: Listening for Jobs\n" +
	"2026-11-21 10:00:01Z: Running job: build\n" +
	"2026-11-21 10:00:09Z: Job build completed with result: Succeeded\n"

type testEnv struct {
	agent *Agent
	sys   *fakeSys
	logs  *bytes.Buffer
	out   *bytes.Buffer
	dir   string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	l := DefaultLayout()
	l.BaseRoot = filepath.Join(dir, "base")
	l.JobMount = filepath.Join(dir, "job")
	l.Root = filepath.Join(dir, "root")
	sys := newFakeSys()
	logs, out := &bytes.Buffer{}, &bytes.Buffer{}
	clock := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	return &testEnv{
		agent: &Agent{
			Sys:    sys,
			Layout: l,
			Log:    slog.New(slog.NewTextHandler(logs, nil)),
			Output: out,
			Now: func() time.Time {
				clock = clock.Add(time.Second)
				return clock
			},
		},
		sys:  sys,
		logs: logs,
		out:  out,
		dir:  dir,
	}
}

func (e *testEnv) root(p string) string { return e.agent.host(p) }

func (e *testEnv) touch(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jobPayload() *Payload {
	return &Payload{
		JobID:           "job-1",
		RunnerConfigB64: base64.StdEncoding.EncodeToString([]byte(jitConfig)),
		Env: map[string]string{
			"TACHYON_JOB_ID":         "job-1",
			"TACHYON_PREBUILD_MATCH": "full",
			"TACHYON_WARM_PATHS":     "node_modules:vendor/cache:../escape",
			"NPM_CONFIG_REGISTRY":    "http://mirror/npm",
		},
		Files: map[string]string{
			"/etc/apt/apt.conf.d/01tachyon-proxy": base64.StdEncoding.EncodeToString([]byte("proxy\n")),
			"~/.m2/settings.xml":                  base64.StdEncoding.EncodeToString([]byte("<settings/>\n")),
		},
		Disks: []Disk{
			{Serial: "job", Mount: "/"},
			{Serial: "sticky-docker", Mount: "/var/lib/docker"},
		},
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func frame(t *testing.T, body []byte) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteFrame(&buf, body); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// The golden files are also read by hostd's tests (hypervisor.rs), so the
// two sides agree on field names.
func TestPayloadMatchesHostdWireFormat(t *testing.T) {
	p, err := ReadPayload(frame(t, readFixture(t, "payload.json")))
	if err != nil {
		t.Fatal(err)
	}
	want := &Payload{
		JobID:           "job-1",
		RunnerConfigB64: base64.StdEncoding.EncodeToString([]byte(jitConfig)),
		Env: map[string]string{
			"TACHYON_JOB_ID":         "job-1",
			"TACHYON_PREBUILD_MATCH": "full",
			"TACHYON_WARM_PATHS":     "node_modules:vendor/cache",
		},
		Files: map[string]string{
			"/etc/apt/apt.conf.d/01tachyon-proxy": base64.StdEncoding.EncodeToString([]byte("Acquire::http::Proxy \"http://mirror:3142\";\n")),
			"~/.m2/settings.xml":                  base64.StdEncoding.EncodeToString([]byte("<settings/>\n")),
		},
		Disks: []Disk{{Serial: "job", Mount: "/"}, {Serial: "sticky-docker", Mount: "/var/lib/docker"}},
		Prebuild: &Prebuild{
			WarmCommands:  []string{"npm ci", "go mod download"},
			KeepPaths:     []string{"node_modules", "~/.npm"},
			ScrubPaths:    []string{".npmrc", ".docker/config.json"},
			CloneCommands: []string{"git init -q", "git fetch --depth 1 origin abc"},
			CloneTokenB64: "ZmFrZS1jbG9uZS10b2tlbg==",
			Refresh:       true,
		},
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("payload = %#v", *p)
	}
	cfg, err := p.RunnerConfig()
	if err != nil || string(cfg) != jitConfig {
		t.Fatalf("RunnerConfig = %q, %v", cfg, err)
	}
}

func TestResultMatchesHostdWireFormat(t *testing.T) {
	clean := true
	var buf bytes.Buffer
	err := WriteResult(&buf, Result{
		ExitCode: 0,
		Phases: map[string]string{
			PhaseRunnerOnline: "2026-11-21T10:00:00Z",
			PhaseJobStarted:   "2026-11-21T10:00:01Z",
		},
		SecretScanClean: &clean,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := ReadFrame(&buf, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(readFixture(t, "result.json"), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %s", body)
	}
}

func TestResultOmitsScanForJobs(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteResult(&buf, Result{ExitCode: 3}); err != nil {
		t.Fatal(err)
	}
	body, _ := ReadFrame(&buf, 1<<20)
	if string(body) != `{"exit_code":3,"phases":{}}` {
		t.Fatalf("result = %s", body)
	}
}

func TestFrameLimits(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0, 10})
	buf.WriteString("short")
	if _, err := ReadFrame(&buf, 100); err == nil {
		t.Fatal("truncated frame accepted")
	}
	if _, err := ReadFrame(frame(t, make([]byte, 101)), 100); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if _, err := ReadPayload(frame(t, []byte("not json "+jitConfig))); err == nil || strings.Contains(err.Error(), "jit") {
		t.Fatalf("bad JSON: %v", err)
	}
}

func TestPayloadFormattingHidesRunnerConfig(t *testing.T) {
	p := jobPayload()
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, v := range []any{p, *p} {
			s := fmt.Sprintf(verb, v)
			if strings.Contains(s, p.RunnerConfigB64) || strings.Contains(s, "not-a-real-config") {
				t.Errorf("%s leaks runner config: %s", verb, s)
			}
		}
	}
}

func TestRunJob(t *testing.T) {
	e := newTestEnv(t)
	l := e.agent.Layout
	// The prebuild left node_modules and vendor/cache in the workspace.
	e.touch(t, e.root(l.Workspace+"/node_modules/left-pad/index.js"), "x")
	e.touch(t, e.root(l.Workspace+"/vendor/cache/gem"), "x")
	e.sys.run = runnerOutput(successLines, 0)

	res := e.agent.Run(context.Background(), jobPayload())

	if res.ExitCode != 0 {
		t.Fatalf("exit code %d; logs:\n%s", res.ExitCode, e.logs)
	}
	wantPhases := map[string]string{
		PhaseRunnerOnline: "2026-11-21T10:00:01Z",
		PhaseJobStarted:   "2026-11-21T10:00:02Z",
	}
	if !reflect.DeepEqual(res.Phases, wantPhases) {
		t.Errorf("phases = %v", res.Phases)
	}
	if res.SecretScanClean != nil {
		t.Error("secret scan reported for a job")
	}

	// Disks: job disk under the overlay, sticky volume inside it.
	wantMounts := []string{
		"ext4 /dev/vdb " + l.JobMount,
		fmt.Sprintf("overlay overlay %s lowerdir=%s,upperdir=%s/upper,workdir=%s/work", l.Root, l.BaseRoot, l.JobMount, l.JobMount),
		"bind /proc " + e.root("/proc"),
		"bind /sys " + e.root("/sys"),
		"bind /dev " + e.root("/dev"),
		"ext4 /dev/vdc " + e.root("/var/lib/docker"),
	}
	if !reflect.DeepEqual(e.sys.mounts, wantMounts) {
		t.Errorf("mounts:\n%s\nwant:\n%s", strings.Join(e.sys.mounts, "\n"), strings.Join(wantMounts, "\n"))
	}
	if !reflect.DeepEqual(e.sys.formatted, []string{"/dev/vdb", "/dev/vdc"}) {
		t.Errorf("formatted = %v", e.sys.formatted)
	}

	// Config files.
	if b, _ := os.ReadFile(e.root("/etc/apt/apt.conf.d/01tachyon-proxy")); string(b) != "proxy\n" {
		t.Errorf("apt proxy = %q", b)
	}
	m2 := e.root("/home/runner/.m2/settings.xml")
	if b, _ := os.ReadFile(m2); string(b) != "<settings/>\n" {
		t.Errorf("maven settings = %q", b)
	}
	if !e.sys.chowns[m2] || !e.sys.chowns[e.root("/home/runner/.m2")] {
		t.Error("home files not handed to the runner user")
	}
	if e.sys.chowns[e.root("/etc/apt")] {
		t.Error("/etc chowned to the runner user")
	}

	// Warm paths stashed out of the workspace, where tachyon/warm finds them.
	for _, p := range []string{"node_modules/left-pad/index.js", "vendor/cache/gem"} {
		if _, err := os.Stat(e.root(l.WarmStash + "/" + p)); err != nil {
			t.Errorf("%s not stashed: %v", p, err)
		}
		if _, err := os.Stat(e.root(l.Workspace + "/" + p)); err == nil {
			t.Errorf("%s still in workspace", p)
		}
	}
	if !e.sys.chowns[e.root(l.WarmStash)] {
		t.Error("warm stash not owned by the runner user")
	}

	// Runner: chrooted, as the runner user, with the JIT config on argv only.
	if len(e.sys.runs) != 1 {
		t.Fatalf("runs = %d", len(e.sys.runs))
	}
	c := e.sys.runs[0]
	if c.Path != l.RunnerCmd || c.Root != l.Root || c.UID != 1001 || c.Dir != l.Home {
		t.Errorf("runner command = %+v", c)
	}
	if !slices.Equal(c.Args, []string{"--jitconfig", jitConfig}) {
		t.Errorf("runner args = %q", c.Args)
	}
	for _, want := range []string{"TACHYON_JOB_ID=job-1", "NPM_CONFIG_REGISTRY=http://mirror/npm", "HOME=/home/runner", "TACHYON_WARM_STASH=/opt/tachyon/warm"} {
		if !slices.Contains(c.Env, want) {
			t.Errorf("env missing %s", want)
		}
	}
	for _, kv := range c.Env {
		if strings.Contains(kv, "not-a-real-config") || strings.Contains(kv, jobPayload().RunnerConfigB64) {
			t.Error("runner config in env")
		}
	}
	if !strings.Contains(e.out.String(), "Listening for Jobs") {
		t.Error("runner output not passed to the console")
	}

	assertNoSecretOnDisk(t, e.dir)
	assertNoSecretInLogs(t, e.logs.String())
}

func assertNoSecretOnDisk(t *testing.T, dir string) {
	t.Helper()
	enc := jobPayload().RunnerConfigB64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if bytes.Contains(b, []byte("not-a-real-config")) || bytes.Contains(b, []byte(enc)) {
			t.Errorf("runner config written to %s", p)
		}
		return nil
	})
}

func assertNoSecretInLogs(t *testing.T, logs string) {
	t.Helper()
	if strings.Contains(logs, "not-a-real-config") || strings.Contains(logs, jobPayload().RunnerConfigB64) {
		t.Errorf("runner config in logs:\n%s", logs)
	}
}

func TestRunJobExitCodes(t *testing.T) {
	tests := []struct {
		name  string
		lines string
		code  int
		want  int
	}{
		{"succeeded", successLines, 0, 0},
		{"failed", "Listening for Jobs\nRunning job: x\nJob x completed with result: Failed\n", 0, 1},
		{"canceled", "Running job: x\nJob x completed with result: Canceled", 0, 1},
		{"runner crashed", "Listening for Jobs\n", 3, 3},
		{"no job ran", "Listening for Jobs\n", 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.sys.run = runnerOutput(tt.lines, tt.code)
			if got := e.agent.Run(context.Background(), jobPayload()).ExitCode; got != tt.want {
				t.Fatalf("exit code = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNoWarmStashWithoutFullMatch(t *testing.T) {
	e := newTestEnv(t)
	l := e.agent.Layout
	e.touch(t, e.root(l.Workspace+"/node_modules/a"), "x")
	p := jobPayload()
	p.Env["TACHYON_PREBUILD_MATCH"] = "partial"
	if res := e.agent.Run(context.Background(), p); res.ExitCode != 1 {
		// The fake runner prints nothing, so no job completed.
		t.Fatalf("exit code %d", res.ExitCode)
	}
	if _, err := os.Stat(e.root(l.Workspace + "/node_modules/a")); err != nil {
		t.Error("partial match must leave the workspace alone")
	}
}

func TestStickyVolumeInHome(t *testing.T) {
	e := newTestEnv(t)
	p := jobPayload()
	p.Disks = append(p.Disks, Disk{Serial: "sticky-cache", Mount: "~/.cache/bazel"})
	e.sys.run = runnerOutput(successLines, 0)
	if res := e.agent.Run(context.Background(), p); res.ExitCode != 0 {
		t.Fatalf("exit code %d\n%s", res.ExitCode, e.logs)
	}
	target := e.root("/home/runner/.cache/bazel")
	if !slices.Contains(e.sys.mounts, "ext4 /dev/vdd "+target) {
		t.Errorf("mounts = %v", e.sys.mounts)
	}
	if !e.sys.chowns[target] || !e.sys.chowns[e.root("/home/runner/.cache")] {
		t.Error("sticky volume in home not owned by the runner user")
	}
}

func TestSetupFailures(t *testing.T) {
	tests := map[string]func(p *Payload){
		"no job disk":        func(p *Payload) { p.Disks = p.Disks[1:] },
		"two job disks":      func(p *Payload) { p.Disks = append(p.Disks, Disk{Serial: "x", Mount: "/"}) },
		"disk never appears": func(p *Payload) { p.Disks[1].Serial = "sticky-missing" },
		"relative volume":    func(p *Payload) { p.Disks[1].Mount = "var/lib/docker" },
		"volume escapes":     func(p *Payload) { p.Disks[1].Mount = "/a/../../etc" },
		"relative file":      func(p *Payload) { p.Files["etc/x"] = "" },
		"file not base64":    func(p *Payload) { p.Files["/etc/x"] = "!!" },
		"no runner config":   func(p *Payload) { p.RunnerConfigB64 = "" },
		"bad runner config":  func(p *Payload) { p.RunnerConfigB64 = "!!" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			e.sys.run = runnerOutput(successLines, 0)
			p := jobPayload()
			mutate(p)
			if res := e.agent.Run(context.Background(), p); res.ExitCode != ExitAgentError {
				t.Fatalf("exit code %d", res.ExitCode)
			}
			assertNoSecretInLogs(t, e.logs.String())
		})
	}
}

func prebuildPayload() *Payload {
	p := jobPayload()
	p.Env["TACHYON_PREBUILD_MATCH"] = "none"
	p.Prebuild = &Prebuild{
		WarmCommands: []string{"npm ci", "go mod download"},
		KeepPaths:    []string{"node_modules", "~/.npm"},
		ScrubPaths:   []string{".npmrc", "~/.docker/config.json"},
	}
	return p
}

func TestPrebuild(t *testing.T) {
	e := newTestEnv(t)
	l := e.agent.Layout
	e.touch(t, e.root("/home/runner/.npmrc"), "//registry/:_authToken=secret")
	e.touch(t, e.root("/home/runner/.docker/config.json"), "{}")
	// What the warm steps left in the writable layer.
	e.touch(t, filepath.Join(l.JobMount, "upper/home/runner/.npm/_cacache/index"), "harmless")

	res := e.agent.Run(context.Background(), prebuildPayload())

	if res.ExitCode != 0 {
		t.Fatalf("exit code %d\n%s", res.ExitCode, e.logs)
	}
	if res.SecretScanClean == nil || !*res.SecretScanClean {
		t.Fatalf("secret scan = %v", res.SecretScanClean)
	}
	var cmds []string
	for _, c := range e.sys.runs {
		if c.Path != "/bin/bash" || c.Dir != l.Workspace || c.Root != l.Root || c.UID != 1001 {
			t.Errorf("warm step command = %+v", c)
		}
		cmds = append(cmds, c.Args[len(c.Args)-1])
	}
	if !slices.Equal(cmds, []string{"npm ci", "go mod download"}) {
		t.Errorf("warm steps = %q (the runner must not start)", cmds)
	}
	for _, p := range []string{"/home/runner/.npmrc", "/home/runner/.docker/config.json"} {
		if _, err := os.Stat(e.root(p)); err == nil {
			t.Errorf("%s not scrubbed", p)
		}
	}
	if !e.sys.chowns[e.root(l.Workspace)] {
		t.Error("workspace not owned by the runner user")
	}
	assertNoSecretOnDisk(t, e.dir)
}

func TestPrebuildCloneTokenOnlyReachesCloneSteps(t *testing.T) {
	e := newTestEnv(t)
	p := prebuildPayload()
	p.Prebuild.CloneCommands = []string{"git fetch origin"}
	p.Prebuild.CloneTokenB64 = base64.StdEncoding.EncodeToString([]byte("clone-secret-token"))

	res := e.agent.Run(context.Background(), p)

	if res.ExitCode != 0 {
		t.Fatalf("exit code %d\n%s", res.ExitCode, e.logs)
	}
	var cmds []string
	for _, c := range e.sys.runs {
		cmd := c.Args[len(c.Args)-1]
		cmds = append(cmds, cmd)
		hasToken := slices.Contains(c.Env, "TACHYON_CLONE_TOKEN=clone-secret-token")
		if want := cmd == "git fetch origin"; hasToken != want {
			t.Errorf("%q: clone token present = %v, want %v", cmd, hasToken, want)
		}
	}
	if !slices.Equal(cmds, []string{"git fetch origin", "npm ci", "go mod download"}) {
		t.Errorf("steps = %q (clone first, then warm)", cmds)
	}
	if strings.Contains(e.logs.String(), "clone-secret-token") {
		t.Error("clone token logged")
	}
}

func TestPrebuildCloneFailureSkipsWarmSteps(t *testing.T) {
	e := newTestEnv(t)
	e.sys.run = func(c Command) (int, error) { return 9, nil }
	p := prebuildPayload()
	p.Prebuild.CloneCommands = []string{"git fetch origin"}
	p.Prebuild.CloneTokenB64 = base64.StdEncoding.EncodeToString([]byte("t"))
	res := e.agent.Run(context.Background(), p)
	if res.ExitCode != 9 {
		t.Fatalf("exit code %d", res.ExitCode)
	}
	if len(e.sys.runs) != 1 {
		t.Errorf("ran %d steps after a failed clone", len(e.sys.runs))
	}
}

func TestPrebuildWarmStepFails(t *testing.T) {
	e := newTestEnv(t)
	e.sys.run = func(c Command) (int, error) { return 7, nil }
	res := e.agent.Run(context.Background(), prebuildPayload())
	if res.ExitCode != 7 {
		t.Fatalf("exit code %d", res.ExitCode)
	}
	if len(e.sys.runs) != 1 {
		t.Errorf("ran %d steps after a failure", len(e.sys.runs))
	}
}

func TestPrebuildFailureReportsStepOutput(t *testing.T) {
	e := newTestEnv(t)
	e.sys.run = func(c Command) (int, error) {
		fmt.Fprintf(c.Stderr, "%s\nfatal: auth failed for tok-123\n", strings.Repeat("x", 3*failureTail))
		return 128, nil
	}
	p := prebuildPayload()
	p.Prebuild.CloneCommands = []string{"git fetch origin"}
	p.Prebuild.CloneTokenB64 = base64.StdEncoding.EncodeToString([]byte("tok-123"))
	res := e.agent.Run(context.Background(), p)
	if !strings.HasPrefix(res.Failure, "clone step 1 exited 128: ") {
		t.Fatalf("failure %q", res.Failure)
	}
	if !strings.HasSuffix(res.Failure, "fatal: auth failed for [redacted]\n") || strings.Contains(res.Failure, "tok-123") {
		t.Errorf("failure tail %q", res.Failure[len(res.Failure)-60:])
	}
	if len(res.Failure) > failureTail+64 {
		t.Errorf("failure is %d bytes", len(res.Failure))
	}
}

func TestPrebuildSuccessHasNoFailure(t *testing.T) {
	e := newTestEnv(t)
	e.sys.run = func(c Command) (int, error) {
		fmt.Fprintln(c.Stdout, "installed")
		return 0, nil
	}
	if res := e.agent.Run(context.Background(), prebuildPayload()); res.Failure != "" {
		t.Errorf("failure %q", res.Failure)
	}
}

func TestPrebuildSecretScanFails(t *testing.T) {
	for name, body := range map[string]string{
		"token":         "token=ghp_" + strings.Repeat("a", 36),
		"runner config": "cached: " + jitConfig,
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			e.touch(t, filepath.Join(e.agent.Layout.JobMount, "upper/home/runner/leak.txt"), body)
			res := e.agent.Run(context.Background(), prebuildPayload())
			if res.SecretScanClean == nil || *res.SecretScanClean {
				t.Fatalf("secret scan = %v", res.SecretScanClean)
			}
			if !strings.Contains(e.logs.String(), "/home/runner/leak.txt") {
				t.Error("finding not logged")
			}
			if !strings.HasPrefix(res.Failure, "secret scan: 1 finding(s): /home/runner/leak.txt (") || strings.Contains(res.Failure, strings.Repeat("a", 36)) {
				t.Errorf("failure %q", res.Failure)
			}
			assertNoSecretInLogs(t, e.logs.String())
			if strings.Contains(e.logs.String(), strings.Repeat("a", 36)) {
				t.Error("matched token logged")
			}
		})
	}
}

// The warm steps' processes are stopped before the scan: a file one of them
// moves until it is killed is scanned where it ends up.
func TestPrebuildScansAfterStoppingProcesses(t *testing.T) {
	e := newTestEnv(t)
	l := e.agent.Layout
	e.sys.onStop = func() {
		e.touch(t, filepath.Join(l.JobMount, "upper/home/runner/moved.txt"), "token=ghp_"+strings.Repeat("a", 36))
	}
	res := e.agent.Run(context.Background(), prebuildPayload())
	if !slices.Equal(e.sys.stopped, []string{l.Root, l.JobMount}) {
		t.Errorf("stopped = %q", e.sys.stopped)
	}
	if res.SecretScanClean == nil || *res.SecretScanClean {
		t.Fatalf("secret scan = %v; the scan ran before the processes were stopped", res.SecretScanClean)
	}
}

type rw struct {
	io.Reader
	io.Writer
}

func TestServe(t *testing.T) {
	e := newTestEnv(t)
	e.sys.run = runnerOutput(successLines, 0)
	body, _ := json.Marshal(jobPayload())
	var out bytes.Buffer
	if err := e.agent.Serve(context.Background(), rw{frame(t, body), &out}); err != nil {
		t.Fatal(err)
	}
	b, err := ReadFrame(&out, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var res Result
	if err := json.Unmarshal(b, &res); err != nil || res.ExitCode != 0 || len(res.Phases) != 2 {
		t.Fatalf("result = %s (%v)", b, err)
	}
}

// checkQuiesced fails the test if the result is written before the job's
// filesystems were quiesced: hostd snapshots the disks once it reads it.
type checkQuiesced struct {
	t    *testing.T
	sys  *fakeSys
	want []string
	buf  bytes.Buffer
}

func (c *checkQuiesced) Write(b []byte) (int, error) {
	c.sys.mu.Lock()
	got := slices.Clone(c.sys.quiesced)
	c.sys.mu.Unlock()
	if !slices.Equal(got, c.want) {
		c.t.Errorf("result written with quiesced = %q, want %q", got, c.want)
	}
	return c.buf.Write(b)
}

func TestServeQuiescesBeforeResult(t *testing.T) {
	for _, prebuild := range []bool{false, true} {
		e := newTestEnv(t)
		e.sys.run = runnerOutput(successLines, 0)
		p := jobPayload()
		if prebuild {
			p.Prebuild = &Prebuild{WarmCommands: []string{"npm ci"}}
		}
		body, _ := json.Marshal(p)
		out := &checkQuiesced{t: t, sys: e.sys, want: []string{e.agent.Layout.Root, e.agent.Layout.JobMount}}
		if err := e.agent.Serve(context.Background(), rw{frame(t, body), out}); err != nil {
			t.Fatal(err)
		}
		b, err := ReadFrame(&out.buf, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		var res Result
		if err := json.Unmarshal(b, &res); err != nil || res.ExitCode != 0 {
			t.Fatalf("prebuild=%v: result = %s (%v)", prebuild, b, err)
		}
	}
}

// If the disks can't be made consistent, the job must not look successful,
// or hostd would keep a torn snapshot as a prebuild or sticky volume.
func TestServeQuiesceFailureFailsJob(t *testing.T) {
	e := newTestEnv(t)
	e.sys.run = runnerOutput(successLines, 0)
	e.sys.quiesceErr = errors.New("/dev/vdb busy and could not be frozen")
	body, _ := json.Marshal(jobPayload())
	var out bytes.Buffer
	if err := e.agent.Serve(context.Background(), rw{frame(t, body), &out}); err != nil {
		t.Fatal(err)
	}
	b, err := ReadFrame(&out, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var res Result
	if err := json.Unmarshal(b, &res); err != nil || res.ExitCode != ExitAgentError {
		t.Fatalf("result = %s (%v)", b, err)
	}
	if !strings.Contains(e.logs.String(), "quiesce disks") {
		t.Fatalf("logs = %s", e.logs)
	}
}

func TestServeBadPayloadStillReports(t *testing.T) {
	e := newTestEnv(t)
	var out bytes.Buffer
	if err := e.agent.Serve(context.Background(), rw{frame(t, []byte("{")), &out}); err == nil {
		t.Fatal("expected error")
	}
	b, err := ReadFrame(&out, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != fmt.Sprintf(`{"exit_code":%d,"phases":{}}`, ExitAgentError) {
		t.Fatalf("result = %s", b)
	}
}

func TestPhaseWatcherKeepsFirstTimestamp(t *testing.T) {
	n := 0
	w := newPhaseWatcher(io.Discard, func() time.Time {
		n++
		return time.Date(2026, 1, 1, 0, 0, n, 0, time.UTC)
	})
	_, _ = io.WriteString(w, "Listening for Jobs\nListening for Jobs\nRunning job: a\nJob a completed with result: Succeeded")
	w.Flush()
	want := map[string]string{PhaseRunnerOnline: "2026-01-01T00:00:01Z", PhaseJobStarted: "2026-01-01T00:00:02Z"}
	if !reflect.DeepEqual(w.Phases(), want) || w.Result() != "Succeeded" {
		t.Fatalf("phases = %v result = %q", w.Phases(), w.Result())
	}
}

func TestRunJobStartsDockerInRoot(t *testing.T) {
	e := newTestEnv(t)
	l := e.agent.Layout
	e.touch(t, e.root(l.Dockerd), "")
	// Left by the prebuild's daemon, which quiesce killed.
	e.touch(t, e.root("/run/docker.sock"), "")
	runner := runnerOutput(successLines, 0)
	e.sys.run = func(c Command) (int, error) {
		if c.Path == l.Dockerd {
			if _, err := os.Stat(e.root("/run/docker.sock")); err == nil {
				t.Error("stale docker socket still present when dockerd starts")
			}
			e.touch(t, e.root("/run/docker.sock"), "")
			return 0, nil
		}
		return runner(c)
	}

	if res := e.agent.Run(context.Background(), jobPayload()); res.ExitCode != 0 {
		t.Fatalf("exit code %d; logs:\n%s", res.ExitCode, e.logs)
	}
	e.sys.mu.Lock()
	defer e.sys.mu.Unlock()
	var dockerd *Command
	for i := range e.sys.runs {
		if e.sys.runs[i].Path == l.Dockerd {
			dockerd = &e.sys.runs[i]
		}
	}
	if dockerd == nil {
		t.Fatal("dockerd not started")
	}
	if dockerd.Root != l.Root || dockerd.UID != 0 {
		t.Errorf("dockerd root=%q uid=%d, want %q as root", dockerd.Root, dockerd.UID, l.Root)
	}
	if want := []string{"--group", "1001"}; !reflect.DeepEqual(dockerd.Args, want) {
		t.Errorf("dockerd args = %q, want %q (socket group the runner has)", dockerd.Args, want)
	}
}

func TestPrebuildRunsInRunnerWorkspace(t *testing.T) {
	for ws, want := range map[string]string{
		"/home/runner/actions-runner/_work/kosha/kosha": "/home/runner/actions-runner/_work/kosha/kosha",
		"/etc/kosha":              "",
		"/home/runner/../etc":     "",
		"/home/runnerx/_work/a/a": "",
	} {
		e := newTestEnv(t)
		if want == "" {
			want = e.agent.Layout.Workspace
		}
		p := prebuildPayload()
		p.Env["TACHYON_WORKSPACE"] = ws
		if res := e.agent.Run(context.Background(), p); res.ExitCode != 0 {
			t.Fatalf("%s: exit code %d\n%s", ws, res.ExitCode, e.logs)
		}
		for _, c := range e.sys.runs {
			if c.Dir != want {
				t.Errorf("TACHYON_WORKSPACE=%s: warm step ran in %s, want %s", ws, c.Dir, want)
			}
		}
	}
}

const warmCommit = "0123456789abcdef0123456789abcdef01234567"

func TestPrebuildRecordsWarmBuild(t *testing.T) {
	e := newTestEnv(t)
	ws := "/home/runner/actions-runner/_work/kosha/kosha"
	p := prebuildPayload()
	p.Env["TACHYON_WORKSPACE"] = ws
	p.Env["TACHYON_PREBUILD_COMMIT"] = warmCommit
	if res := e.agent.Run(context.Background(), p); res.ExitCode != 0 {
		t.Fatalf("exit code %d\n%s", res.ExitCode, e.logs)
	}
	if r := e.agent.readWarmRecord(); r.Workspace != ws || r.Commit != warmCommit {
		t.Errorf("warm record = %+v", r)
	}
}

func TestRefreshKeepsWarmRecord(t *testing.T) {
	for _, recorded := range []string{"/home/runner/actions-runner/_work/kosha/kosha", ""} {
		e := newTestEnv(t)
		old := "workspace=" + recorded + "\ncommit=" + warmCommit + "\n"
		if recorded != "" {
			e.touch(t, e.root("/home/runner/.tachyon/prebuild"), old)
		}
		p := prebuildPayload()
		p.Env["TACHYON_WORKSPACE"] = "/home/runner/actions-runner/_work/new/new"
		p.Env["TACHYON_PREBUILD_COMMIT"] = strings.Repeat("f", 40)
		p.Prebuild.Refresh = true
		p.Prebuild.WarmCommands = nil
		p.Prebuild.CloneCommands = []string{"git fetch"}
		p.Prebuild.CloneTokenB64 = base64.StdEncoding.EncodeToString([]byte("tok"))
		if res := e.agent.Run(context.Background(), p); res.ExitCode != 0 {
			t.Fatalf("exit code %d\n%s", res.ExitCode, e.logs)
		}
		want := recorded
		if want == "" {
			want = e.agent.Layout.Workspace
		}
		for _, c := range e.sys.runs {
			if c.Dir != want {
				t.Errorf("recorded %q: refresh ran in %s, want %s", recorded, c.Dir, want)
			}
		}
		b, _ := os.ReadFile(e.root("/home/runner/.tachyon/prebuild"))
		if recorded != "" && string(b) != old {
			t.Errorf("refresh rewrote the warm record: %q", b)
		}
		if recorded == "" && len(b) != 0 {
			t.Errorf("refresh wrote a warm record: %q", b)
		}
	}
}

func TestStashFromRecordedWorkspace(t *testing.T) {
	e := newTestEnv(t)
	l := e.agent.Layout
	ws := "/home/runner/actions-runner/_work/kosha/kosha"
	e.touch(t, e.root("/home/runner/.tachyon/prebuild"), "workspace="+ws+"\ncommit="+warmCommit+"\n")
	e.touch(t, e.root(ws+"/node_modules/a"), "x")
	e.sys.run = runnerOutput(successLines, 0)
	if res := e.agent.Run(context.Background(), jobPayload()); res.ExitCode != 0 {
		t.Fatalf("exit code %d\n%s", res.ExitCode, e.logs)
	}
	if _, err := os.Stat(e.root(l.WarmStash + "/node_modules/a")); err != nil {
		t.Errorf("node_modules not stashed from %s: %v", ws, err)
	}
}

func TestCloneOnlyPrebuildRecordsWorkspace(t *testing.T) {
	e := newTestEnv(t)
	ws := "/home/runner/actions-runner/_work/kosha/kosha"
	p := prebuildPayload()
	p.Env["TACHYON_WORKSPACE"] = ws
	p.Env["TACHYON_PREBUILD_COMMIT"] = warmCommit
	p.Prebuild.WarmCommands = nil
	p.Prebuild.CloneCommands = []string{"git fetch"}
	p.Prebuild.CloneTokenB64 = base64.StdEncoding.EncodeToString([]byte("tok"))
	if res := e.agent.Run(context.Background(), p); res.ExitCode != 0 {
		t.Fatalf("exit code %d\n%s", res.ExitCode, e.logs)
	}
	if r := e.agent.readWarmRecord(); r.Workspace != ws || r.Commit != warmCommit {
		t.Errorf("warm record = %+v", r)
	}
}
