package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrBlobNotFound is returned by BlobStore.Open and Delete for unknown ids.
var ErrBlobNotFound = errors.New("blob not found")

// BlobStore is the object store behind the cache: an S3-compatible store on
// NVMe servers in each region (decision D4, not yet made). The service only
// needs whole-object put, ranged reads and delete, which any S3-compatible
// store provides. Ids are opaque, URL-safe strings chosen by the service.
type BlobStore interface {
	Put(ctx context.Context, id string, r io.Reader) (int64, error)
	Open(ctx context.Context, id string) (Blob, error)
	Delete(ctx context.Context, id string) error
}

// Blob is an open, immutable object.
type Blob interface {
	io.ReaderAt
	io.Closer
	Size() int64
}

// validBlobID keeps ids safe as path segments and object keys.
func validBlobID(id string) bool {
	if id == "" || len(id) > 256 || strings.Contains(id, "..") {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '/', c == '.':
		default:
			return false
		}
	}
	return !strings.HasPrefix(id, "/")
}

// MemStore is an in-memory BlobStore for tests and development.
type MemStore struct {
	mu    sync.Mutex
	blobs map[string][]byte
	// Fail, if set, makes every call return it (simulates the store being
	// down).
	Fail error
}

type memBlob struct{ *bytes.Reader }

func (memBlob) Close() error { return nil }

func (m *MemStore) Put(_ context.Context, id string, r io.Reader) (int64, error) {
	if !validBlobID(id) {
		return 0, fmt.Errorf("invalid blob id %q", id)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return 0, m.Fail
	}
	if m.blobs == nil {
		m.blobs = map[string][]byte{}
	}
	m.blobs[id] = b
	return int64(len(b)), nil
}

func (m *MemStore) Open(_ context.Context, id string) (Blob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	b, ok := m.blobs[id]
	if !ok {
		return nil, ErrBlobNotFound
	}
	return memBlob{bytes.NewReader(b)}, nil
}

func (m *MemStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, ok := m.blobs[id]; !ok {
		return ErrBlobNotFound
	}
	delete(m.blobs, id)
	return nil
}

// Len returns the number of stored blobs.
func (m *MemStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.blobs)
}

// DirStore keeps blobs as files under Root. It is for a single-node
// development service; production uses the D4 object store.
type DirStore struct {
	Root string
}

func (d DirStore) path(id string) (string, error) {
	if !validBlobID(id) {
		return "", fmt.Errorf("invalid blob id %q", id)
	}
	return filepath.Join(d.Root, filepath.FromSlash(id)), nil
}

func (d DirStore) Put(_ context.Context, id string, r io.Reader) (int64, error) {
	p, err := d.path(id)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), p)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return 0, err
	}
	return n, nil
}

type fileBlob struct {
	*os.File
	size int64
}

func (f fileBlob) Size() int64 { return f.size }

func (d DirStore) Open(_ context.Context, id string) (Blob, error) {
	p, err := d.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrBlobNotFound
	}
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return fileBlob{f, st.Size()}, nil
}

func (d DirStore) Delete(_ context.Context, id string) error {
	p, err := d.path(id)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return ErrBlobNotFound
	}
	return err
}
