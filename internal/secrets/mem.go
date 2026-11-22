package secrets

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Mem is an in-memory Store for development and tests. The zero value is
// ready to use with DefaultPolicy. Values expire like they do in Vault.
type Mem struct {
	Policy Policy           // zero means DefaultPolicy
	Now    func() time.Time // for tests; nil means time.Now

	mu sync.Mutex
	m  map[string]memEntry
}

type memEntry struct {
	value   []byte
	expires time.Time
}

var _ Store = (*Mem)(nil)

func (s *Mem) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Mem) Put(_ context.Context, path string, value []byte) (string, error) {
	ref, err := NewRef(path)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]memEntry{}
	}
	s.m[ref] = memEntry{value: append([]byte(nil), value...), expires: s.now().Add(s.Policy.TTL(path))}
	return ref, nil
}

func (s *Mem) Get(_ context.Context, ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[ref]
	if !ok {
		return nil, fmt.Errorf("%s: %w", ref, ErrNotFound)
	}
	if !s.now().Before(e.expires) {
		delete(s.m, ref)
		return nil, fmt.Errorf("%s: %w", ref, ErrExpired)
	}
	return append([]byte(nil), e.value...), nil
}

func (s *Mem) Delete(_ context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, ref)
	return nil
}

// Sweep drops expired values and returns how many it dropped.
func (s *Mem) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now, n := s.now(), 0
	for ref, e := range s.m {
		if !now.Before(e.expires) {
			delete(s.m, ref)
			n++
		}
	}
	return n
}

// Len returns the number of stored values, expired or not.
func (s *Mem) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
