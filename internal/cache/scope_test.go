package cache

import (
	"errors"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

const (
	repo = "acme/api"
	main = "refs/heads/main"
)

func mainScope() Scope {
	return Scope{Repo: repo, Ref: main, DefaultBranch: main, Trust: jobapi.Trusted}
}

func prScope(n string) Scope {
	return Scope{Repo: repo, Ref: "refs/pull/" + n + "/merge", BaseRef: main, DefaultBranch: main, Trust: jobapi.Untrusted}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { c.t = c.t.Add(time.Second); return c.t }

func newIndex(quota int64) (*Index, *[]Entry) {
	var evicted []Entry
	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return &Index{QuotaBytes: quota, Now: c.now, OnEvict: func(e Entry) { evicted = append(evicted, e) }}, &evicted
}

func TestLookupExactThenPrefix(t *testing.T) {
	ix, _ := newIndex(0)
	must(t, ix.Put(mainScope(), Entry{Key: "npm-linux-aaa", Version: "v1", BlobID: "old"}))
	must(t, ix.Put(mainScope(), Entry{Key: "npm-linux-bbb", Version: "v1", BlobID: "new"}))
	must(t, ix.Put(mainScope(), Entry{Key: "npm-linux-ccc", Version: "v2", BlobID: "otherversion"}))

	e, ok := ix.Lookup(mainScope(), "v1", "npm-linux-aaa", []string{"npm-linux-"})
	if !ok || e.BlobID != "old" {
		t.Fatalf("exact match: %+v %v", e, ok)
	}
	e, ok = ix.Lookup(mainScope(), "v1", "npm-linux-zzz", []string{"npm-linux-"})
	if !ok || e.BlobID != "new" {
		t.Fatalf("prefix should pick newest: %+v %v", e, ok)
	}
	if _, ok := ix.Lookup(mainScope(), "v3", "npm-linux-aaa", []string{"npm-"}); ok {
		t.Fatal("version mismatch must miss")
	}
	if _, ok := ix.Lookup(mainScope(), "v1", "pip", []string{"pip-"}); ok {
		t.Fatal("unrelated key must miss")
	}
	if _, ok := ix.Lookup(Scope{Repo: "other/repo", Ref: main, DefaultBranch: main}, "v1", "npm-linux-aaa", nil); ok {
		t.Fatal("other repo must miss")
	}
}

func TestScoping(t *testing.T) {
	ix, _ := newIndex(0)
	must(t, ix.Put(mainScope(), Entry{Key: "k-main", Version: "v"}))
	must(t, ix.Put(prScope("1"), Entry{Key: "k-pr1", Version: "v"}))

	if _, ok := ix.Lookup(prScope("1"), "v", "k-main", nil); !ok {
		t.Error("PR should read default-branch entries")
	}
	if _, ok := ix.Lookup(prScope("1"), "v", "k-pr1", nil); !ok {
		t.Error("PR should read its own entries")
	}
	if _, ok := ix.Lookup(prScope("2"), "v", "k-pr1", nil); ok {
		t.Error("PR 2 read PR 1's entry")
	}
	if _, ok := ix.Lookup(mainScope(), "v", "k-pr1", nil); ok {
		t.Error("default branch read a PR entry")
	}
	// PR's own entry wins over the default branch for the same prefix.
	e, ok := ix.Lookup(prScope("1"), "v", "zzz", []string{"k-"})
	if !ok || e.Key != "k-pr1" {
		t.Errorf("PR scope should be searched first: %+v", e)
	}
	feature := Scope{Repo: repo, Ref: "refs/heads/feat", DefaultBranch: main, Trust: jobapi.Untrusted}
	if _, ok := ix.Lookup(feature, "v", "k-main", nil); !ok {
		t.Error("feature branch should read default branch")
	}
}

func TestPutRules(t *testing.T) {
	ix, _ := newIndex(0)
	fork := prScope("3")
	fork.Trust = jobapi.UntrustedFork
	if err := ix.Put(fork, Entry{Key: "k", Version: "v"}); !errors.Is(err, ErrForkWrite) {
		t.Fatalf("fork write: %v", err)
	}
	must(t, ix.Put(mainScope(), Entry{Key: "k", Version: "v"}))
	if err := ix.Put(mainScope(), Entry{Key: "k", Version: "v"}); err == nil {
		t.Fatal("duplicate key accepted")
	}
	must(t, ix.Put(prScope("1"), Entry{Key: "k", Version: "v"})) // same key, other ref: fine
	// Put ignores caller-provided Repo/Ref and uses the scope's.
	must(t, ix.Put(prScope("2"), Entry{Repo: "evil/x", Ref: main, Key: "spoof", Version: "v"}))
	if _, ok := ix.Lookup(mainScope(), "v", "spoof", nil); ok {
		t.Fatal("entry written into default branch via spoofed Ref")
	}
}

func TestQuotaEvictsLRU(t *testing.T) {
	ix, evicted := newIndex(100)
	must(t, ix.Put(mainScope(), Entry{Key: "a", Version: "v", SizeBytes: 40}))
	must(t, ix.Put(mainScope(), Entry{Key: "b", Version: "v", SizeBytes: 40}))
	// Touch a so b is least recently used.
	if _, ok := ix.Lookup(mainScope(), "v", "a", nil); !ok {
		t.Fatal("a missing")
	}
	must(t, ix.Put(mainScope(), Entry{Key: "c", Version: "v", SizeBytes: 40}))
	if len(*evicted) != 1 || (*evicted)[0].Key != "b" {
		t.Fatalf("evicted = %+v, want b", *evicted)
	}
	if ix.Usage(repo) != 80 {
		t.Fatalf("usage = %d", ix.Usage(repo))
	}
	if err := ix.Put(mainScope(), Entry{Key: "huge", Version: "v", SizeBytes: 101}); err == nil {
		t.Fatal("entry over quota accepted")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
