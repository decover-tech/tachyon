package prebuild

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Key identifies a prebuild's warm-up inputs: the base image version, the
// prebuild config, and the contents of the repo's key files. If the key is
// unchanged a refresh only fetches new git objects; otherwise the warm-up
// reruns.
func Key(baseImage string, configBytes []byte, keyFiles map[string][]byte) string {
	h := sha256.New()
	field := func(tag string, b []byte) {
		fmt.Fprintf(h, "%s:%d:", tag, len(b))
		h.Write(b)
	}
	field("image", []byte(baseImage))
	field("config", configBytes)
	names := make([]string, 0, len(keyFiles))
	for n := range keyFiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		field("name", []byte(n))
		field("file", keyFiles[n])
	}
	return "pb1-" + hex.EncodeToString(h.Sum(nil))[:32]
}

// ReadKeyFiles collects the files in repoDir matching the config's key
// files. Missing files are simply absent from the key.
func ReadKeyFiles(repoDir string, c *Config) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := filepath.WalkDir(repoDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(repoDir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipKeyDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if c.IsKeyFile(rel) {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = b
		}
		return nil
	})
	return out, err
}

// skipKeyDir names directories never searched for key files.
func skipKeyDir(name string) bool {
	return name == ".git" || name == "node_modules"
}

// IsKeyFile reports whether rel (slash-separated, relative to the repo
// root) matches one of the config's key files.
func (c *Config) IsKeyFile(rel string) bool {
	for _, k := range c.KeyFiles {
		if ok, _ := path.Match(k, rel); ok || k == rel {
			return true
		}
	}
	return false
}

// KeyFilesIn picks the key files out of a repo's file list (as from a git
// tree) and reads them with read. It selects the same files as
// ReadKeyFiles does on a checkout, so both give the same key.
func KeyFilesIn(c *Config, files []string, read func(rel string) ([]byte, error)) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, rel := range files {
		if !c.IsKeyFile(rel) || inSkippedDir(rel) {
			continue
		}
		b, err := read(rel)
		if err != nil {
			return nil, fmt.Errorf("read key file %s: %w", rel, err)
		}
		out[rel] = b
	}
	return out, nil
}

func inSkippedDir(rel string) bool {
	parts := strings.Split(rel, "/")
	for _, p := range parts[:len(parts)-1] {
		if skipKeyDir(p) {
			return true
		}
	}
	return false
}
