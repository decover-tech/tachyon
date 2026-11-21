// Package prebuild defines per-repo prebuilds: snapshots of a repo's fully
// prepared job environment at a trusted commit, which every job forks
// copy-on-write instead of re-checking-out, restoring and reinstalling.
package prebuild

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigPath is where a repo declares its prebuild, relative to its root.
const ConfigPath = ".tachyon/prebuild.yml"

// Config is .tachyon/prebuild.yml.
type Config struct {
	Version int `yaml:"version"`
	// KeyFiles feed the prebuild key; a change reruns the warm-up. Entries
	// may be globs (path.Match syntax).
	KeyFiles []string `yaml:"key_files"`
	// Paths are kept from the warm-up. Paths outside the workspace are
	// present when a job starts; workspace paths need the tachyon/warm step.
	Paths []string `yaml:"paths"`
	Warm  []Step   `yaml:"warm"`
	// MemorySnapshot is a Phase 3 experiment and must be false in v1.
	MemorySnapshot bool `yaml:"memory_snapshot"`
}

type Step struct {
	Run string `yaml:"run"`
}

// Parse decodes and validates a config. Unknown fields are rejected.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigPath, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Load reads and parses the config in repoDir.
func Load(repoDir string) (*Config, []byte, error) {
	data, err := os.ReadFile(path.Join(repoDir, ConfigPath))
	if err != nil {
		return nil, nil, err
	}
	c, err := Parse(data)
	return c, data, err
}

func (c *Config) Validate() error {
	var errs []error
	if c.Version != 1 {
		errs = append(errs, fmt.Errorf("version: must be 1, got %d", c.Version))
	}
	for i, k := range c.KeyFiles {
		if k == "" || path.IsAbs(k) || strings.HasPrefix(path.Clean(k), "..") {
			errs = append(errs, fmt.Errorf("key_files[%d]: %q must be a path inside the repo", i, k))
		} else if _, err := path.Match(k, ""); err != nil {
			errs = append(errs, fmt.Errorf("key_files[%d]: bad pattern %q", i, k))
		}
	}
	for i, p := range c.Paths {
		if strings.TrimSpace(p) == "" {
			errs = append(errs, fmt.Errorf("paths[%d]: empty", i))
		}
	}
	for i, s := range c.Warm {
		if strings.TrimSpace(s.Run) == "" {
			errs = append(errs, fmt.Errorf("warm[%d]: run is empty", i))
		}
	}
	if c.MemorySnapshot {
		errs = append(errs, errors.New("memory_snapshot: not supported in v1"))
	}
	return errors.Join(errs...)
}

// WorkspacePaths returns the paths inside the workspace (relative paths),
// which actions/checkout would clean and the tachyon/warm step restores.
func (c *Config) WorkspacePaths() []string {
	var out []string
	for _, p := range c.Paths {
		if !path.IsAbs(p) && !strings.HasPrefix(p, "~") && !strings.HasPrefix(p, "$") {
			out = append(out, path.Clean(p))
		}
	}
	return out
}

// TouchesKeyFiles reports whether any changed file matches a key file.
func (c *Config) TouchesKeyFiles(changed []string) bool {
	for _, f := range changed {
		if f == ConfigPath {
			return true
		}
		for _, k := range c.KeyFiles {
			if f == k {
				return true
			}
			if ok, _ := path.Match(k, f); ok {
				return true
			}
		}
	}
	return false
}
