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
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		for _, k := range c.KeyFiles {
			if ok, _ := path.Match(k, rel); ok || k == rel {
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				out[rel] = b
				break
			}
		}
		return nil
	})
	return out, err
}
