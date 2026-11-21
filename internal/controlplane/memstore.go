package controlplane

import (
	"fmt"
	"strings"
	"sync"

	"github.com/decover-tech/tachyon/internal/prebuild"
	"github.com/decover-tech/tachyon/internal/secrets"
)

// In-memory implementations for development and tests. Production needs a
// database for prebuilds; secrets go to the key store in internal/secrets.

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

// MemSecrets is the in-memory key store, kept under its old name for
// existing callers. Production uses secrets.Vault.
type MemSecrets = secrets.Mem
