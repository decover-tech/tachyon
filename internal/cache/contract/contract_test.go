package contract

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/cache"
	"github.com/decover-tech/tachyon/internal/jobapi"
)

// fakeClient is a cache client and service in one, with GitHub's rules
// taken from cache.Index. Its knobs break one rule each, so the tests can
// show the suite catches that break.
type fakeClient struct {
	target Target

	leakPRs       bool // PR entries readable by everyone
	forkWrites    bool // forks may write
	noPrefix      bool // restore keys ignored
	oldestWins    bool // prefix match picks the oldest entry
	mutable       bool // a second save replaces the entry
	corrupt       bool // restores return different bytes
	warnOnRestore bool // every restore prints a warning
	failSave      bool // saves exit non-zero
	dropSaves     bool // saves store nothing and only log it, as a refused reservation does
	hitOnPrefix   bool // cache-hit=true on a prefix match

	mu    sync.Mutex
	ix    *cache.Index
	blobs map[string]map[string][]byte
	seq   int
	clock time.Time
}

func newFake(target Target) *fakeClient {
	f := &fakeClient{target: target, blobs: map[string]map[string][]byte{}, clock: time.Unix(1e9, 0)}
	f.ix = &cache.Index{Now: func() time.Time { f.clock = f.clock.Add(time.Second); return f.clock }}
	return f
}

func (f *fakeClient) Name() string { return "fake" }

func (f *fakeClient) scope(job JobSpec) cache.Scope {
	sc := cache.Scope{Repo: f.target.Repo, Ref: job.Ref, BaseRef: job.BaseRef, DefaultBranch: f.target.DefaultBranch, Trust: job.Trust}
	if f.leakPRs {
		sc.Ref, sc.BaseRef = f.target.DefaultBranch, ""
	}
	if f.forkWrites && sc.Trust == jobapi.UntrustedFork {
		sc.Trust = jobapi.Untrusted
	}
	return sc
}

func (f *fakeClient) Save(_ context.Context, job JobSpec, ws, key string, paths []string) (StepResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSave {
		return StepResult{Log: []byte("boom")}, errors.New("exit status 1")
	}
	if f.dropSaves {
		return StepResult{Log: []byte("Failed to save: Unable to reserve cache")}, nil
	}
	files := map[string][]byte{}
	root := filepath.Join(ws, paths[0])
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		files[rel] = b
		return err
	})
	if err != nil {
		return StepResult{}, err
	}
	f.seq++
	id := fmt.Sprint(f.seq)
	sc := f.scope(job)
	if f.mutable {
		if e, ok := f.ix.Lookup(sc, "v", key, nil); ok && e.Ref == sc.Ref {
			f.blobs[e.BlobID] = files
			return StepResult{}, nil
		}
	}
	if err := f.ix.Put(sc, cache.Entry{Key: key, Version: "v", BlobID: id}); err != nil {
		return StepResult{Annotations: []Annotation{{"warning", "Failed to save: " + err.Error()}}}, nil
	}
	f.blobs[id] = files
	return StepResult{}, nil
}

func (f *fakeClient) Restore(_ context.Context, job JobSpec, ws, key string, restoreKeys, paths []string) (StepResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := StepResult{Outputs: map[string]string{}}
	if f.warnOnRestore {
		res.Annotations = append(res.Annotations, Annotation{"warning", "Failed to restore: 500"})
	}
	if f.noPrefix {
		restoreKeys = nil
	}
	sc := f.scope(job)
	var e *cache.Entry
	var ok bool
	if f.oldestWins {
		// Exact key, then "<prefix>1" before "<prefix>2": the suite's
		// keys end in a sequence number, so this picks the oldest.
		e, ok = f.ix.Lookup(sc, "v", key, nil)
		for _, rk := range restoreKeys {
			for _, cand := range []string{rk + "1", rk + "2"} {
				if !ok {
					e, ok = f.ix.Lookup(sc, "v", cand, nil)
				}
			}
		}
	} else {
		e, ok = f.ix.Lookup(sc, "v", key, restoreKeys)
	}
	if !ok {
		res.Outputs["cache-hit"] = "false"
		return res, nil
	}
	for name, b := range f.blobs[e.BlobID] {
		if f.corrupt {
			b = append([]byte("x"), b...)
		}
		p := filepath.Join(ws, paths[0], name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return res, err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return res, err
		}
	}
	res.Outputs["cache-hit"] = fmt.Sprint(e.Key == key || f.hitOnPrefix)
	res.Outputs["cache-matched-key"] = e.Key
	return res, nil
}

func testTarget() Target {
	return Target{URL: "http://cache.test", Repo: "acme/api", DefaultBranch: "refs/heads/main", Jobs: DefaultJobs("refs/heads/main")}
}

func runFake(t *testing.T, f *fakeClient) Report {
	t.Helper()
	return Run(context.Background(), f, f.target, Config{KeyPrefix: "t", BlobBytes: 1024, TempDir: t.TempDir(), Settle: time.Nanosecond})
}

func TestSuitePassesAgainstGitHubRules(t *testing.T) {
	r := runFake(t, newFake(testTarget()))
	if r.Failed() {
		t.Fatalf("suite failed against a correct service:\n%s", r.Markdown())
	}
	if len(r.Checks) != 12 {
		t.Fatalf("ran %d checks, want 12:\n%s", len(r.Checks), r.Markdown())
	}
}

func TestSuiteCatchesBrokenService(t *testing.T) {
	cases := []struct {
		name    string
		breakIt func(*fakeClient)
		check   string // a check that must fail
	}{
		{"saves silently dropped", func(f *fakeClient) { f.dropSaves = true }, "save from the default branch"},
		{"PR entries leak", func(f *fakeClient) { f.leakPRs = true }, "other pull requests can't read it"},
		{"forks can write", func(f *fakeClient) { f.forkWrites = true }, "fork can't write"},
		{"restore keys ignored", func(f *fakeClient) { f.noPrefix = true }, "restore-key prefix match"},
		{"oldest prefix match", func(f *fakeClient) { f.oldestWins = true }, "newest entry wins a prefix match"},
		{"entries mutable", func(f *fakeClient) { f.mutable = true }, "entries are immutable"},
		{"corrupt download", func(f *fakeClient) { f.corrupt = true }, "save from the default branch"},
		{"restore warns", func(f *fakeClient) { f.warnOnRestore = true }, "save from the default branch"},
		{"prefix reported as hit", func(f *fakeClient) { f.hitOnPrefix = true }, "restore-key prefix match"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(testTarget())
			tc.breakIt(f)
			r := runFake(t, f)
			for _, c := range r.Checks {
				if c.Name == tc.check {
					if c.Err == nil {
						t.Fatalf("check %q passed against a broken service:\n%s", tc.check, r.Markdown())
					}
					return
				}
			}
			t.Fatalf("check %q did not run:\n%s", tc.check, r.Markdown())
		})
	}
}

func TestSuiteStopsWhenFirstSaveFails(t *testing.T) {
	f := newFake(testTarget())
	f.failSave = true
	r := runFake(t, f)
	if len(r.Checks) != 1 || r.Checks[0].Err == nil || !strings.Contains(r.Checks[0].Err.Error(), "boom") {
		t.Fatalf("want one failed check carrying the step log, got:\n%s", r.Markdown())
	}
}

func TestSuiteNeedsKeyPrefix(t *testing.T) {
	r := Run(context.Background(), newFake(testTarget()), testTarget(), Config{})
	if !r.Failed() {
		t.Fatal("missing KeyPrefix accepted")
	}
}

func TestMarkdownEscapesTableCells(t *testing.T) {
	r := Report{Client: "c", Checks: []Check{{"ok", nil}, {"bad", errors.New("a|b\nc")}}}
	md := r.Markdown()
	if !strings.Contains(md, "| ok | pass |") || !strings.Contains(md, `**FAIL**: a\|b c`) {
		t.Fatalf("markdown:\n%s", md)
	}
}
