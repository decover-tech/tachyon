// Package cache is Tachyon's GitHub-compatible Actions cache: same-datacenter
// restores for actions/cache and the setup-* actions with no YAML changes.
//
// This file holds the scoping and quota rules, which copy GitHub's: entries
// from the default branch are readable by every branch, pull-request entries
// stay with that pull request, and forks can read but never write. The
// protocol server (a Twirp API handing out signed blob URLs, as of mid-2026)
// and the object store behind it (decision D4) are not built yet.
package cache

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

// Scope is who is asking: the job's repo, ref and trust.
type Scope struct {
	Repo          string
	Ref           string // refs/heads/x, or refs/pull/N/merge for pull requests
	BaseRef       string // for pull requests, the target branch
	DefaultBranch string // refs/heads/main
	Trust         jobapi.Trust
}

var ErrForkWrite = errors.New("cache writes are not allowed from fork pull requests")

// readableRefs lists the refs whose entries s may read, most specific first.
func (s Scope) readableRefs() []string {
	refs := []string{s.Ref}
	if s.BaseRef != "" && s.BaseRef != s.Ref {
		refs = append(refs, s.BaseRef)
	}
	if s.DefaultBranch != s.Ref && s.DefaultBranch != s.BaseRef {
		refs = append(refs, s.DefaultBranch)
	}
	return refs
}

// Entry is one cache entry's metadata. Blob bytes live in the object store.
type Entry struct {
	Repo      string
	Ref       string
	Key       string
	Version   string // hash of paths + compression, as the toolkit sends
	SizeBytes int64
	BlobID    string
	CreatedAt time.Time
	LastUsed  time.Time
}

// Index is an in-memory cache index with per-repo quotas and LRU eviction.
type Index struct {
	// QuotaBytes per repo; larger than GitHub's default 10 GB.
	QuotaBytes int64
	// OnEvict is called for evicted entries so their blobs can be deleted.
	OnEvict func(Entry)
	Now     func() time.Time

	mu      sync.Mutex
	entries map[string][]*Entry // repo -> entries
}

func (ix *Index) now() time.Time {
	if ix.Now != nil {
		return ix.Now()
	}
	return time.Now()
}

// Lookup resolves a restore: an exact key match first, then each restore
// key as a prefix (newest entry wins), searching readable refs in order.
func (ix *Index) Lookup(s Scope, version, key string, restoreKeys []string) (*Entry, bool) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	candidates := ix.entries[s.Repo]
	for _, ref := range s.readableRefs() {
		var inRef []*Entry
		for _, e := range candidates {
			if e.Ref == ref && e.Version == version {
				inRef = append(inRef, e)
			}
		}
		sort.Slice(inRef, func(i, j int) bool { return inRef[i].CreatedAt.After(inRef[j].CreatedAt) })
		for _, e := range inRef {
			if e.Key == key {
				e.LastUsed = ix.now()
				c := *e
				return &c, true
			}
		}
		for _, prefix := range restoreKeys {
			for _, e := range inRef {
				if strings.HasPrefix(e.Key, prefix) {
					e.LastUsed = ix.now()
					c := *e
					return &c, true
				}
			}
		}
	}
	return nil, false
}

// Put records a new entry written by s. Entries are immutable: writing an
// existing key in the same ref and version is a conflict.
func (ix *Index) Put(s Scope, e Entry) error {
	if s.Trust == jobapi.UntrustedFork {
		return ErrForkWrite
	}
	e.Repo, e.Ref = s.Repo, s.Ref
	if ix.QuotaBytes > 0 && e.SizeBytes > ix.QuotaBytes {
		return fmt.Errorf("entry of %d bytes exceeds repo quota", e.SizeBytes)
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.entries == nil {
		ix.entries = map[string][]*Entry{}
	}
	for _, x := range ix.entries[e.Repo] {
		if x.Ref == e.Ref && x.Key == e.Key && x.Version == e.Version {
			return fmt.Errorf("cache entry %q already exists for %s", e.Key, e.Ref)
		}
	}
	now := ix.now()
	e.CreatedAt, e.LastUsed = now, now
	ix.entries[e.Repo] = append(ix.entries[e.Repo], &e)
	ix.evictLocked(e.Repo)
	return nil
}

func (ix *Index) evictLocked(repo string) {
	if ix.QuotaBytes <= 0 {
		return
	}
	es := ix.entries[repo]
	var total int64
	for _, e := range es {
		total += e.SizeBytes
	}
	if total <= ix.QuotaBytes {
		return
	}
	sort.Slice(es, func(i, j int) bool { return es[i].LastUsed.Before(es[j].LastUsed) })
	for len(es) > 0 && total > ix.QuotaBytes {
		victim := es[0]
		es = es[1:]
		total -= victim.SizeBytes
		if ix.OnEvict != nil {
			ix.OnEvict(*victim)
		}
	}
	ix.entries[repo] = es
}

// Usage returns bytes stored for repo.
func (ix *Index) Usage(repo string) int64 {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var total int64
	for _, e := range ix.entries[repo] {
		total += e.SizeBytes
	}
	return total
}
