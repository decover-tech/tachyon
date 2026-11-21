package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/decover-tech/tachyon/internal/prebuild"
)

// In-memory implementations for development and tests. Production needs a
// database for prebuilds and a managed key store for secrets.

// StaticImages pins every image name to one version.
type StaticImages map[string]string

func (s StaticImages) Pin(name string) (string, error) {
	if strings.Contains(name, "@") {
		return name, nil
	}
	v, ok := s[name]
	if !ok {
		return "", fmt.Errorf("unknown image %q", name)
	}
	return name + "@" + v, nil
}

// MemPrebuilds keeps the latest prebuild per repo.
type MemPrebuilds struct {
	mu      sync.Mutex
	latest  map[string]prebuild.Snapshot
	configs map[string]*prebuild.Config
}

func (m *MemPrebuilds) Latest(repo string) (*prebuild.Snapshot, *prebuild.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.latest[repo]
	if !ok {
		return nil, m.configs[repo]
	}
	return &s, m.configs[repo]
}

func (m *MemPrebuilds) Register(s prebuild.Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.latest == nil {
		m.latest = map[string]prebuild.Snapshot{}
	}
	m.latest[s.Repo] = s
}

// SetConfig records the prebuild config for a repo.
func (m *MemPrebuilds) SetConfig(repo string, c *prebuild.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.configs == nil {
		m.configs = map[string]*prebuild.Config{}
	}
	m.configs[repo] = c
}

// MemSecrets stores secrets in memory under random references.
type MemSecrets struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *MemSecrets) Put(_ context.Context, path string, value []byte) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ref := "secret://" + path + "#" + hex.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	s.m[ref] = append([]byte(nil), value...)
	return ref, nil
}

func (s *MemSecrets) Get(_ context.Context, ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[ref]
	if !ok {
		return nil, fmt.Errorf("secret %s not found", ref)
	}
	return v, nil
}

func (s *MemSecrets) Delete(_ context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, ref)
	return nil
}
