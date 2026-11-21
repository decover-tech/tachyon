package proxy

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Store is an on-disk cache of upstream responses. Each entry is one file:
// a JSON metadata line followed by the body. Entries are written to a temp
// file and renamed into place, so readers never see a partial entry.
type Store struct {
	Dir string
}

// Meta is what the store keeps besides the body.
type Meta struct {
	Header http.Header `json:"header"`
	Stored time.Time   `json:"stored"`
	// Size is the body length, filled in by Get.
	Size int64 `json:"-"`
}

// keptHeaders are the upstream response headers worth replaying from cache.
var keptHeaders = []string{
	"Content-Type",
	"ETag",
	"Last-Modified",
	"Docker-Content-Digest",
	"Docker-Distribution-Api-Version",
}

func (s *Store) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(s.Dir, h[:2], h)
}

// Get opens the entry for key. The caller closes the body.
func (s *Store) Get(key string) (Meta, io.ReadCloser, bool) {
	p := s.path(key)
	f, err := os.Open(p)
	if err != nil {
		return Meta{}, nil, false
	}
	br := bufio.NewReader(f)
	line, err := br.ReadBytes('\n')
	var m Meta
	if err != nil || json.Unmarshal(line, &m) != nil {
		f.Close()
		return Meta{}, nil, false
	}
	if info, err := f.Stat(); err == nil {
		m.Size = info.Size() - int64(len(line))
	}
	// Bump mtime so Prune evicts least recently used entries first.
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return m, struct {
		io.Reader
		io.Closer
	}{br, f}, true
}

// Writer collects one entry. Commit publishes it; Abort discards it.
type Writer struct {
	f    *os.File
	dest string
	done bool
}

// Put starts writing an entry for key.
func (s *Store) Put(key string, m Meta) (*Writer, error) {
	dest := s.path(key)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(dest), ".tmp-*")
	if err != nil {
		return nil, err
	}
	kept := http.Header{}
	for _, h := range keptHeaders {
		if v := m.Header.Get(h); v != "" {
			kept.Set(h, v)
		}
	}
	m.Header = kept
	b, _ := json.Marshal(m)
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return &Writer{f: f, dest: dest}, nil
}

func (w *Writer) Write(p []byte) (int, error) { return w.f.Write(p) }

// Commit makes the entry visible.
func (w *Writer) Commit() error {
	if w.done {
		return errors.New("store: entry already finished")
	}
	w.done = true
	if err := w.f.Close(); err != nil {
		os.Remove(w.f.Name())
		return err
	}
	return os.Rename(w.f.Name(), w.dest)
}

// Abort discards the entry. Safe to call after Commit.
func (w *Writer) Abort() {
	if w.done {
		return
	}
	w.done = true
	w.f.Close()
	os.Remove(w.f.Name())
}

// Prune deletes least recently used entries until the store holds at most
// maxBytes. Leftover temp files older than an hour are removed too.
func (s *Store) Prune(maxBytes int64) (removed int, err error) {
	type ent struct {
		path string
		size int64
		mod  time.Time
	}
	var ents []ent
	var total int64
	stale := time.Now().Add(-time.Hour)
	err = filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if filepath.Base(p)[0] == '.' {
			if info.ModTime().Before(stale) {
				os.Remove(p)
			}
			return nil
		}
		ents = append(ents, ent{p, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].mod.Before(ents[j].mod) })
	for _, e := range ents {
		if total <= maxBytes {
			break
		}
		if os.Remove(e.path) == nil {
			total -= e.size
			removed++
		}
	}
	return removed, nil
}

// Writable reports whether the store can take new entries (health check).
func (s *Store) Writable() error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".health-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}
