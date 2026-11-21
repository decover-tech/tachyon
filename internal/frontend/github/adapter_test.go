package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

type fakeAPI struct {
	mu      sync.Mutex
	rc      RunContext
	nextID  int64
	created []string // runner names
	deleted []int64
	runners []Runner
	reruns  []int64 // GitHub job IDs
	calls   int

	// err, if set, is returned by every call (and counted in calls).
	err error
}

func (f *fakeAPI) fail() error {
	f.calls++
	return f.err
}

func (f *fakeAPI) RunContext(context.Context, int64, string, int64) (RunContext, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return RunContext{}, err
	}
	return f.rc, nil
}

func (f *fakeAPI) RerunJob(_ context.Context, _ int64, _ string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return err
	}
	f.reruns = append(f.reruns, id)
	return nil
}

func (f *fakeAPI) CreateJITConfig(_ context.Context, _ int64, _, name string, _ []string) (JITRunner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return JITRunner{}, err
	}
	f.nextID++
	f.created = append(f.created, name)
	return JITRunner{RunnerID: f.nextID, JITConfig: []byte("jit-" + name)}, nil
}

func (f *fakeAPI) ListRunners(context.Context, int64, string) ([]Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	return append([]Runner(nil), f.runners...), nil
}

func (f *fakeAPI) DeleteRunner(_ context.Context, _ int64, _ string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return err
	}
	f.deleted = append(f.deleted, id)
	return nil
}

type fakeSecrets struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *fakeSecrets) Put(_ context.Context, path string, v []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	ref := "secret://" + path
	s.m[ref] = v
	return ref, nil
}

func (s *fakeSecrets) Delete(_ context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, ref)
	return nil
}

type fakeCore struct {
	mu    sync.Mutex
	subs  []jobapi.Submission
	hints []jobapi.Hint
}

func (c *fakeCore) Submit(_ context.Context, s jobapi.Submission) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subs = append(c.subs, s)
	return nil
}

func (c *fakeCore) Hint(_ context.Context, h jobapi.Hint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hints = append(c.hints, h)
	return nil
}

var testSecret = []byte("whsec")

type harness struct {
	t     *testing.T
	a     *Adapter
	api   *fakeAPI
	sec   *fakeSecrets
	core  *fakeCore
	srv   *httptest.Server
	clock time.Time
}

func newHarness(t *testing.T) *harness {
	h := &harness{
		t:     t,
		api:   &fakeAPI{rc: RunContext{Event: "push", HeadBranch: "main", HeadRepo: "acme/api", BaseRepo: "acme/api", DefaultBranch: "main"}},
		sec:   &fakeSecrets{},
		core:  &fakeCore{},
		clock: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC),
	}
	h.a = &Adapter{
		WebhookSecret:  testSecret,
		API:            h.api,
		Secrets:        h.sec,
		Now:            func() time.Time { return h.clock },
		ReconcileAfter: time.Minute,
	}
	h.srv = httptest.NewServer(h.a.Handler(h.core))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) post(event string, payload any, secret []byte) int {
	h.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func jobEvent(action string, id int64, labels []string, runner string) map[string]any {
	return map[string]any{
		"action": action,
		"workflow_job": map[string]any{
			"id": id, "run_id": 77, "workflow_name": "CI", "name": "test",
			"head_sha": "9f1c2ab", "head_branch": "main", "labels": labels, "runner_name": runner,
		},
		"repository":   map[string]any{"full_name": "acme/api", "default_branch": "main"},
		"installation": map[string]any{"id": 42},
	}
}

var tachyonLabels = []string{"tachyon-ubuntu-24.04-8x"}

func TestAdapterQueuedSubmits(t *testing.T) {
	h := newHarness(t)
	if code := h.post("workflow_job", jobEvent("queued", 1, tachyonLabels, ""), testSecret); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	if len(h.core.subs) != 1 {
		t.Fatalf("got %d submissions", len(h.core.subs))
	}
	s := h.core.subs[0]
	sp := s.Spec
	if sp.JobID != "gh_1" || sp.Tenant != "gh_inst_42" || sp.Repo != "github.com/acme/api" || sp.Commit != "9f1c2ab" {
		t.Errorf("ids: %+v", sp)
	}
	if sp.Trust != jobapi.Trusted {
		t.Errorf("trust = %s", sp.Trust)
	}
	if sp.Image != "ubuntu-24.04" || sp.Hardware != jobapi.NewHardware(jobapi.ArchX86_64, 8) {
		t.Errorf("image/hardware = %s %+v", sp.Image, sp.Hardware)
	}
	if s.Profile != "acme/api/CI/test" {
		t.Errorf("profile = %s", s.Profile)
	}
	if sp.Prebuild != (jobapi.PrebuildRef{Repo: "acme/api", Policy: jobapi.PrebuildLatestTrusted}) {
		t.Errorf("prebuild = %+v", sp.Prebuild)
	}
	if len(sp.Volumes) != 1 || sp.Volumes[0] != jobapi.DockerVolume {
		t.Errorf("volumes = %+v", sp.Volumes)
	}
	if sp.Bootstrap.Kind != jobapi.BootstrapGitHubRunner || !strings.HasPrefix(sp.Bootstrap.JITConfig, "secret://") {
		t.Errorf("bootstrap = %+v", sp.Bootstrap)
	}
	if string(h.sec.m[sp.Bootstrap.JITConfig]) != "jit-"+h.api.created[0] {
		t.Errorf("secret not stored under bootstrap ref")
	}
	if !s.QueuedAt.Equal(h.clock) {
		t.Errorf("queued at = %v", s.QueuedAt)
	}

	// Redelivery is deduped.
	h.post("workflow_job", jobEvent("queued", 1, tachyonLabels, ""), testSecret)
	if len(h.core.subs) != 1 || len(h.api.created) != 1 {
		t.Fatalf("redelivery submitted again: subs=%d created=%d", len(h.core.subs), len(h.api.created))
	}
}

func TestAdapterCleanLabelAndForkTrust(t *testing.T) {
	h := newHarness(t)
	h.api.rc = RunContext{Event: "pull_request", HeadBranch: "main", HeadRepo: "evil/api", BaseRepo: "acme/api"}
	h.post("workflow_job", jobEvent("queued", 2, []string{"tachyon-ubuntu-24.04-2x", "tachyon-clean"}, ""), testSecret)
	if len(h.core.subs) != 1 {
		t.Fatal("no submission")
	}
	sp := h.core.subs[0].Spec
	if sp.Trust != jobapi.UntrustedFork || sp.Prebuild.Policy != jobapi.PrebuildNone {
		t.Errorf("trust=%s policy=%s", sp.Trust, sp.Prebuild.Policy)
	}
}

func TestAdapterRejectsBadSignature(t *testing.T) {
	h := newHarness(t)
	if code := h.post("workflow_job", jobEvent("queued", 1, tachyonLabels, ""), []byte("wrong")); code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", code)
	}
	if len(h.core.subs) != 0 {
		t.Fatal("submitted despite bad signature")
	}
}

func TestAdapterIgnoresNonTachyonAndBadLabels(t *testing.T) {
	h := newHarness(t)
	if code := h.post("workflow_job", jobEvent("queued", 1, []string{"ubuntu-latest"}, ""), testSecret); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	if code := h.post("workflow_job", jobEvent("queued", 2, []string{"tachyon-bogus"}, ""), testSecret); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	if len(h.core.subs) != 0 || len(h.api.created) != 0 {
		t.Fatal("non-Tachyon or invalid job was dispatched")
	}
}

func TestAdapterCompletedBeforeStartDeletesRunner(t *testing.T) {
	h := newHarness(t)
	h.post("workflow_job", jobEvent("queued", 5, tachyonLabels, ""), testSecret)
	ref := h.core.subs[0].Spec.Bootstrap.JITConfig
	h.post("workflow_job", jobEvent("completed", 5, tachyonLabels, ""), testSecret)
	if len(h.api.deleted) != 1 || h.api.deleted[0] != 1 {
		t.Fatalf("deleted = %v, want [1]", h.api.deleted)
	}
	if _, ok := h.sec.m[ref]; ok {
		t.Fatal("JIT secret not deleted")
	}
}

func TestAdapterInProgressThenCompleted(t *testing.T) {
	h := newHarness(t)
	h.post("workflow_job", jobEvent("queued", 6, tachyonLabels, ""), testSecret)
	name := h.api.created[0]
	h.post("workflow_job", jobEvent("in_progress", 6, tachyonLabels, name), testSecret)
	h.post("workflow_job", jobEvent("completed", 6, tachyonLabels, name), testSecret)
	if len(h.api.deleted) != 0 {
		t.Fatalf("runner of a finished job should be removed by GitHub, not us: %v", h.api.deleted)
	}
}

func TestAdapterReconcile(t *testing.T) {
	h := newHarness(t)
	h.post("workflow_job", jobEvent("queued", 9, tachyonLabels, ""), testSecret)
	firstRunner := h.api.created[0]

	// Not stale yet: nothing happens except the orphan sweep.
	h.api.runners = []Runner{
		{ID: 100, Name: "tachyon-orphan"},
		{ID: 101, Name: "tachyon-busy", Busy: true},
		{ID: 102, Name: "customer-runner"},
		{ID: 1, Name: firstRunner},
	}
	h.a.Reconcile(context.Background())
	if len(h.core.subs) != 1 {
		t.Fatalf("re-dispatched too early")
	}
	if fmt.Sprint(h.api.deleted) != "[100]" {
		t.Fatalf("deleted = %v, want only the orphan [100]", h.api.deleted)
	}

	// Stale: old runner deleted, new runner created, job re-submitted.
	h.api.deleted = nil
	h.api.runners = nil
	h.clock = h.clock.Add(2 * time.Minute)
	h.a.Reconcile(context.Background())
	if len(h.core.subs) != 2 {
		t.Fatalf("subs = %d, want 2", len(h.core.subs))
	}
	if len(h.api.created) != 2 || h.api.created[1] == firstRunner {
		t.Fatalf("created = %v", h.api.created)
	}
	if fmt.Sprint(h.api.deleted) != "[1]" {
		t.Fatalf("deleted = %v, want [1]", h.api.deleted)
	}
	if h.core.subs[1].Spec.Bootstrap.JITConfig == h.core.subs[0].Spec.Bootstrap.JITConfig {
		t.Fatal("re-dispatch reused the old JIT config")
	}

	// After MaxAttempts (default 3) the job is dropped.
	h.clock = h.clock.Add(2 * time.Minute)
	h.a.Reconcile(context.Background())
	h.clock = h.clock.Add(2 * time.Minute)
	h.a.Reconcile(context.Background())
	if len(h.core.subs) != 3 {
		t.Fatalf("subs = %d, want 3 (initial + 2 retries)", len(h.core.subs))
	}
	h.clock = h.clock.Add(2 * time.Minute)
	h.a.Reconcile(context.Background())
	if len(h.core.subs) != 3 {
		t.Fatal("kept retrying past MaxAttempts")
	}
}

func TestAdapterPushAndPRHints(t *testing.T) {
	h := newHarness(t)
	h.post("push", map[string]any{
		"ref": "refs/heads/main", "after": "abc",
		"commits":      []map[string]any{{"added": []string{"a"}, "modified": []string{"go.sum"}, "removed": []string{"c"}}},
		"repository":   map[string]any{"full_name": "acme/api", "default_branch": "main"},
		"installation": map[string]any{"id": 42},
	}, testSecret)
	h.post("pull_request", map[string]any{
		"action": "opened",
		"pull_request": map[string]any{"head": map[string]any{
			"sha": "def", "ref": "feat", "repo": map[string]any{"full_name": "evil/api"},
		}},
		"repository":   map[string]any{"full_name": "acme/api"},
		"installation": map[string]any{"id": 42},
	}, testSecret)
	if len(h.core.hints) != 2 {
		t.Fatalf("hints = %d", len(h.core.hints))
	}
	push, pr := h.core.hints[0], h.core.hints[1]
	if push.Trust != jobapi.Trusted || push.Commit != "abc" || fmt.Sprint(push.ChangedFiles) != "[a go.sum c]" {
		t.Errorf("push hint = %+v", push)
	}
	if pr.Trust != jobapi.UntrustedFork || pr.Commit != "def" {
		t.Errorf("pr hint = %+v", pr)
	}
}
