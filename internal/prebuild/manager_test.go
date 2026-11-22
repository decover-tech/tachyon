package prebuild

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

const (
	sha1 = "1111111111111111111111111111111111111111"
	sha2 = "2222222222222222222222222222222222222222"
	sha3 = "3333333333333333333333333333333333333333"
	shaH = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

const testConfig = "version: 1\nkey_files: [go.sum, \"*/package-lock.json\"]\npaths: [~/go/pkg/mod, node_modules]\nwarm:\n  - run: go mod download\n"

// fakeSource serves file trees per commit.
type fakeSource struct {
	mu        sync.Mutex
	trees     map[string]map[string]string // commit -> path -> content
	protected map[string]bool
	head      string
	failFiles bool
	tokens    int
}

func (s *fakeSource) DefaultHead(context.Context, string, string) (string, error) {
	return s.head, nil
}

func (s *fakeSource) Protected(_ context.Context, _, _, branch string) (bool, error) {
	return s.protected[branch], nil
}

func (s *fakeSource) Files(_ context.Context, _, _, commit string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failFiles {
		return nil, errors.New("api down")
	}
	var out []string
	for p := range s.trees[commit] {
		out = append(out, p)
	}
	return out, nil
}

func (s *fakeSource) ReadFile(_ context.Context, _, _, commit, path string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.trees[commit][path]
	if !ok {
		return nil, fmt.Errorf("%s: %w", path, fs.ErrNotExist)
	}
	return []byte(c), nil
}

func (s *fakeSource) CloneToken(context.Context, string, string) (CloneToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens++
	return CloneToken{URL: "https://github.com/acme/api.git", Token: []byte("ghs_tok"), ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type pinner struct{}

func (pinner) Pin(name string) (string, error) { return name + "@v1", nil }

type fakeRegistry struct {
	mu     sync.Mutex
	latest *Snapshot
	config *Config
}

func (r *fakeRegistry) Latest(string) (*Snapshot, *Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.latest, r.config
}

func (r *fakeRegistry) SetConfig(_ string, c *Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.config = c
}

type fakeBuilder struct {
	mu   sync.Mutex
	reqs []BuildRequest
	fail bool
}

func (b *fakeBuilder) BuildPrebuild(_ context.Context, req BuildRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail {
		return errors.New("no capacity")
	}
	b.reqs = append(b.reqs, req)
	return nil
}

func (b *fakeBuilder) take() []BuildRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.reqs
	b.reqs = nil
	return r
}

type harness struct {
	m   *Manager
	src *fakeSource
	reg *fakeRegistry
	b   *fakeBuilder
	now time.Time
}

func newHarness() *harness {
	h := &harness{
		src: &fakeSource{trees: map[string]map[string]string{}, protected: map[string]bool{"release": true}},
		reg: &fakeRegistry{},
		b:   &fakeBuilder{},
		// 10:00 UTC; nightly hour is 3.
		now: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC),
	}
	h.m = &Manager{
		Source: h.src, Images: pinner{}, Registry: h.reg, Builder: h.b,
		MinInterval: 10 * time.Minute, NightlyHour: 3,
		EgressAllowlist: []string{"github.com"},
		Now:             func() time.Time { return h.now },
	}
	for _, c := range []string{sha1, sha2, sha3, shaH} {
		h.src.trees[c] = map[string]string{
			ConfigPath:               testConfig,
			"go.sum":                 "gosum-1",
			"web/package-lock.json":  "lock",
			"node_modules/x/go.sum":  "ignored",
			"main.go":                "package main",
			"vendor/package-lock.no": "x",
		}
	}
	return h
}

func push(commit, ref string, trust jobapi.Trust) jobapi.Hint {
	return jobapi.Hint{Tenant: "gh_inst_1", Repo: "github.com/acme/api", Commit: commit, Ref: ref, Event: "push", Trust: trust}
}

// finish completes req as the control plane would: register and report.
func (h *harness) finish(req BuildRequest, ok bool) {
	if ok {
		h.reg.mu.Lock()
		h.reg.latest = &Snapshot{Repo: req.Repo, Key: req.Key, Commit: req.Commit, BaseImage: req.BaseImage, Healthy: true}
		h.reg.mu.Unlock()
	}
	h.m.Finished(req.Repo, req.JobID, ok)
}

func TestPushBuildsFullThenRefresh(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	if err := h.m.OnPush(ctx, push(sha1, "refs/heads/main", jobapi.Trusted)); err != nil {
		t.Fatal(err)
	}
	reqs := h.b.take()
	if len(reqs) != 1 {
		t.Fatalf("builds = %d", len(reqs))
	}
	r := reqs[0]
	if r.Repo != "acme/api" || r.Tenant != "gh_inst_1" || r.Commit != sha1 || r.BaseImage != "ubuntu-24.04@v1" || r.Trigger != TriggerPush {
		t.Fatalf("req = %v", r)
	}
	if r.Refresh || r.From != nil {
		t.Error("first build must run the warm-up")
	}
	if !reflect.DeepEqual(r.WarmCommands, []string{"go mod download"}) || !reflect.DeepEqual(r.KeepPaths, []string{"~/go/pkg/mod", "node_modules"}) {
		t.Errorf("warm %v keep %v", r.WarmCommands, r.KeepPaths)
	}
	if !reflect.DeepEqual(r.ScrubPaths, CredentialFiles) || !reflect.DeepEqual(r.EgressAllowlist, []string{"github.com"}) {
		t.Errorf("scrub %v egress %v", r.ScrubPaths, r.EgressAllowlist)
	}
	if string(r.CloneToken) != "ghs_tok" || len(r.CloneCommands) == 0 || !strings.Contains(strings.Join(r.CloneCommands, "\n"), sha1) {
		t.Errorf("clone = %v", r.CloneCommands)
	}
	if strings.Contains(r.String(), "ghs_tok") {
		t.Error("String() leaks the clone token")
	}
	// The key matches what `tachyon prebuild key` computes on a checkout.
	want := Key("ubuntu-24.04@v1", []byte(testConfig), map[string][]byte{"go.sum": []byte("gosum-1"), "web/package-lock.json": []byte("lock")})
	if r.Key != want {
		t.Errorf("key = %s, want %s", r.Key, want)
	}
	if h.m.Inflight("acme/api") != r.JobID || !strings.HasPrefix(r.JobID, "pb_acme_api_") {
		t.Errorf("inflight = %q, job %q", h.m.Inflight("acme/api"), r.JobID)
	}
	h.finish(r, true)
	if h.reg.config == nil || len(h.reg.config.Warm) != 1 {
		t.Error("config not recorded on success")
	}

	// Next push after the interval with the same key files: refresh only.
	h.now = h.now.Add(11 * time.Minute)
	h.m.OnPush(ctx, push(sha2, "refs/heads/main", jobapi.Trusted))
	reqs = h.b.take()
	if len(reqs) != 1 || !reqs[0].Refresh || reqs[0].From == nil || reqs[0].From.Commit != sha1 || reqs[0].Key != r.Key {
		t.Fatalf("want refresh from %s, got %+v", sha1, reqs)
	}
	h.finish(reqs[0], true)

	// A key file changes: the warm-up reruns.
	h.src.trees[sha3]["go.sum"] = "gosum-2"
	h.now = h.now.Add(11 * time.Minute)
	h.m.OnPush(ctx, push(sha3, "refs/heads/main", jobapi.Trusted))
	reqs = h.b.take()
	if len(reqs) != 1 || reqs[0].Refresh || reqs[0].Key == r.Key {
		t.Fatalf("want full rebuild with a new key, got %+v", reqs)
	}
}

func TestPushTriggerFilter(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		hint jobapi.Hint
		want int
	}{
		{"default branch", push(sha1, "refs/heads/main", jobapi.Trusted), 1},
		{"protected branch", push(sha1, "refs/heads/release", jobapi.Untrusted), 1},
		{"unprotected branch", push(sha1, "refs/heads/feature", jobapi.Untrusted), 0},
		{"fork", push(sha1, "refs/heads/release", jobapi.UntrustedFork), 0},
		{"tag", push(sha1, "refs/tags/v1", jobapi.Trusted), 0},
		{"pull request", func() jobapi.Hint {
			h := push(sha1, "refs/heads/release", jobapi.Untrusted)
			h.Event = "pull_request"
			return h
		}(), 0},
		{"bad commit", push("main", "refs/heads/main", jobapi.Trusted), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			if err := h.m.OnPush(ctx, tc.hint); err != nil {
				t.Fatal(err)
			}
			if got := len(h.b.take()); got != tc.want {
				t.Fatalf("builds = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPushesCoalescedPerInterval(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	h.m.OnPush(ctx, push(sha1, "refs/heads/main", jobapi.Trusted))
	first := h.b.take()[0]

	// While the build runs, and inside the interval, pushes only queue.
	h.m.OnPush(ctx, push(sha2, "refs/heads/main", jobapi.Trusted))
	h.m.OnPush(ctx, push(sha3, "refs/heads/main", jobapi.Trusted))
	h.m.Tick(ctx)
	if n := len(h.b.take()); n != 0 {
		t.Fatalf("built %d while in flight", n)
	}
	h.finish(first, true)
	h.now = h.now.Add(5 * time.Minute)
	h.m.Tick(ctx)
	if n := len(h.b.take()); n != 0 {
		t.Fatalf("built %d inside the interval", n)
	}
	// Once the interval passes, only the newest commit is built.
	h.now = h.now.Add(6 * time.Minute)
	h.m.Tick(ctx)
	reqs := h.b.take()
	if len(reqs) != 1 || reqs[0].Commit != sha3 {
		t.Fatalf("want one build of %s, got %+v", sha3, reqs)
	}
	h.m.Tick(ctx)
	if n := len(h.b.take()); n != 0 {
		t.Fatal("pending build ran twice")
	}
}

func TestNightlyRefresh(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	h.src.head = shaH
	h.m.OnPush(ctx, push(sha1, "refs/heads/main", jobapi.Trusted))
	h.finish(h.b.take()[0], true)

	// Same day: no nightly yet.
	h.now = h.now.Add(10 * time.Hour) // 20:00
	h.m.Tick(ctx)
	if n := len(h.b.take()); n != 0 {
		t.Fatalf("nightly ran early (%d)", n)
	}
	// Past midnight but before NightlyHour.
	h.now = h.now.Add(6 * time.Hour) // 02:00 next day
	h.m.Tick(ctx)
	if n := len(h.b.take()); n != 0 {
		t.Fatal("nightly ran before NightlyHour")
	}
	h.now = h.now.Add(time.Hour + time.Minute) // 03:01
	h.m.Tick(ctx)
	reqs := h.b.take()
	if len(reqs) != 1 || reqs[0].Trigger != TriggerNightly || reqs[0].Commit != shaH || !reqs[0].Refresh {
		t.Fatalf("nightly = %+v", reqs)
	}
	h.finish(reqs[0], true)
	h.now = h.now.Add(time.Hour)
	h.m.Tick(ctx)
	if n := len(h.b.take()); n != 0 {
		t.Fatal("nightly ran twice in a day")
	}

	// Next night, nothing changed: already up to date, nothing built.
	h.now = h.now.Add(24 * time.Hour)
	h.m.Tick(ctx)
	if n := len(h.b.take()); n != 0 {
		t.Fatal("nightly rebuilt an up-to-date prebuild")
	}
	if h.m.Inflight("acme/api") != "" {
		t.Error("reservation not released")
	}
}

func TestFailuresRetryOnTick(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	h.src.failFiles = true
	h.m.OnPush(ctx, push(sha1, "refs/heads/main", jobapi.Trusted))
	if n := len(h.b.take()); n != 0 || h.m.Inflight("acme/api") != "" {
		t.Fatal("failed plan should not leave a build in flight")
	}
	h.src.failFiles = false
	h.b.fail = true
	h.now = h.now.Add(11 * time.Minute)
	h.m.Tick(ctx) // retried, dispatch fails
	h.b.fail = false
	h.now = h.now.Add(11 * time.Minute)
	h.m.Tick(ctx)
	reqs := h.b.take()
	if len(reqs) != 1 || reqs[0].Commit != sha1 {
		t.Fatalf("retry = %+v", reqs)
	}
	// A failed build leaves no registration and releases the repo.
	h.finish(reqs[0], false)
	if h.reg.latest != nil || h.reg.config != nil || h.m.Inflight("acme/api") != "" {
		t.Error("failed build registered or still in flight")
	}
}

func TestLostBuildTimesOut(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	h.m.BuildTimeout = time.Hour
	h.m.OnPush(ctx, push(sha1, "refs/heads/main", jobapi.Trusted))
	lost := h.b.take()[0]
	h.m.OnPush(ctx, push(sha2, "refs/heads/main", jobapi.Trusted))
	h.now = h.now.Add(30 * time.Minute)
	h.m.Tick(ctx)
	if len(h.b.take()) != 0 {
		t.Fatal("built while previous build in flight")
	}
	h.now = h.now.Add(31 * time.Minute)
	h.m.Tick(ctx)
	reqs := h.b.take()
	if len(reqs) != 1 || reqs[0].Commit != sha2 {
		t.Fatalf("after timeout = %+v", reqs)
	}
	// A late result for the lost build is ignored.
	h.m.Finished("acme/api", lost.JobID, true)
	if h.m.Inflight("acme/api") != reqs[0].JobID {
		t.Error("late result cleared the current build")
	}
}

func TestNoConfigNoBuild(t *testing.T) {
	h := newHarness()
	delete(h.src.trees[sha1], ConfigPath)
	h.m.OnPush(context.Background(), push(sha1, "refs/heads/main", jobapi.Trusted))
	if len(h.b.take()) != 0 || h.src.tokens != 0 {
		t.Fatal("built or minted a token for a repo without config")
	}
	h.src.trees[sha2][ConfigPath] = "version: 2\n"
	h.now = h.now.Add(time.Hour)
	h.m.OnPush(context.Background(), push(sha2, "refs/heads/main", jobapi.Trusted))
	if len(h.b.take()) != 0 {
		t.Fatal("built with an invalid config")
	}
}

func TestKeyFilesInMatchesReadKeyFiles(t *testing.T) {
	c, err := Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"go.sum": "a", "web/package-lock.json": "b", "node_modules/x/package-lock.json": "c", "a/b/package-lock.json": "d", ".git/go.sum": "e"}
	var names []string
	for n := range files {
		names = append(names, n)
	}
	got, err := KeyFilesIn(c, names, func(rel string) ([]byte, error) { return []byte(files[rel]), nil })
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{"go.sum": []byte("a"), "web/package-lock.json": []byte("b")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}
