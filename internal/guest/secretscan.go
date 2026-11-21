package guest

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// Finding is one secret-scan hit. It names the file and rule, never the
// matched text.
type Finding struct {
	Path string
	Rule string
}

type secretRule struct {
	name string
	re   *regexp.Regexp
}

// secretRules are high-confidence token formats. A prebuild is shared by
// every job on the repo, including untrusted pull requests, so a hit fails
// the prebuild rather than being reported and kept.
var secretRules = []secretRule{
	{"github-token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b`)},
	{"github-fine-grained-pat", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{82}\b`)},
	{"aws-access-key-id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"npm-token", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"private-key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED |PGP )?PRIVATE KEY(?: BLOCK)?-----`)},
}

const (
	scanChunk = 1 << 20
	// scanOverlap is longer than any rule's match, so a token split across
	// two chunks is still seen whole.
	scanOverlap = 256
)

// ScanSecrets walks dir (the job disk's writable layer) and reports files
// matching a secret rule or containing one of the literal byte strings.
// Symlinks, devices and overlay whiteouts are skipped.
func ScanSecrets(dir string, literals [][]byte) ([]Finding, error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		// No writable layer at all: nothing to leak.
		return nil, nil
	}
	var out []Finding
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		hits, err := scanFile(p, literals)
		if err != nil {
			return err
		}
		for _, h := range hits {
			out = append(out, Finding{Path: "/" + filepath.ToSlash(rel), Rule: h})
		}
		return nil
	})
	return out, err
}

func scanFile(p string, literals [][]byte) ([]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	var hits []string
	hit := func(rule string) {
		if !seen[rule] {
			seen[rule] = true
			hits = append(hits, rule)
		}
	}
	buf := make([]byte, 0, scanChunk+scanOverlap)
	chunk := make([]byte, scanChunk)
	for {
		n, rerr := io.ReadFull(f, chunk)
		buf = append(buf, chunk[:n]...)
		for _, r := range secretRules {
			if r.re.Match(buf) {
				hit(r.name)
			}
		}
		for _, l := range literals {
			if len(l) > 0 && bytes.Contains(buf, l) {
				hit("runner-config")
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return hits, nil
		}
		if rerr != nil {
			return nil, rerr
		}
		keep := scanOverlap
		for _, l := range literals {
			keep = max(keep, len(l))
		}
		if len(buf) > keep {
			buf = append(buf[:0], buf[len(buf)-keep:]...)
		}
	}
}
