package guest

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
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
	// markers are literals every match contains. The regexp runs only on
	// chunks holding one, as it is far slower than a byte search and a
	// warm target/ or toolchain is gigabytes of binaries.
	markers [][]byte
	// sample reports whether a match is a documented placeholder.
	sample func(match []byte) bool
}

func markers(m ...string) [][]byte {
	out := make([][]byte, len(m))
	for i, s := range m {
		out[i] = []byte(s)
	}
	return out
}

// secretRules are high-confidence token formats. A prebuild is shared by
// every job on the repo, including untrusted pull requests, so a hit fails
// the prebuild rather than being reported and kept.
var secretRules = []secretRule{
	{"github-token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b`), markers("ghp_", "gho_", "ghu_", "ghs_", "ghr_"), nil},
	{"github-fine-grained-pat", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{82}\b`), markers("github_pat_"), nil},
	// AWS's documentation and SDKs use keys ending in EXAMPLE
	// (AKIAIOSFODNN7EXAMPLE); test fixtures and the SDKs carry them.
	{"aws-access-key-id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), markers("AKIA", "ASIA"), func(m []byte) bool {
		return bytes.HasSuffix(m, []byte("EXAMPLE"))
	}},
	{"npm-token", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`), markers("npm_"), nil},
	{"slack-token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), markers("xox"), nil},
	// A key, not just its header: PEM/PGP parsers (rustls, pem, cargo)
	// contain the header line as a string constant. Any body line of 16+
	// base64 characters counts, so keys wrapped narrower than 64 are seen.
	{"private-key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED |PGP )?PRIVATE KEY(?: BLOCK)?-----\r?\n(?:[A-Za-z-]+: [^\r\n]*\r?\n)*(?:\r?\n)?[A-Za-z0-9+/=]{16,}\r?\n`), markers("-----BEGIN "), nil},
}

// registryDirs hold packages downloaded from public registries, whose docs
// and test fixtures are full of sample keys (dotenv's README, selfsigned,
// the AWS SDK's AKIA...EXAMPLE), as is Node's code cache of them. Only the literal secrets the guest actually
// handed the build (clone token, runner config) are looked for in them.
var registryDirs = []string{
	"/node_modules/", "/site-packages/", "/dist-packages/",
	"/.npm/_cacache/", "/.cache/pip/", "/.cargo/registry/",
	"/node-compile-cache/",
}

// compiledExts are Rust build outputs. Compiling public crates embeds
// their doc and test sample keys (pem-rfc7468's example key, for one).
var compiledExts = []string{".rlib", ".rmeta"}

func fromRegistry(rel string) bool {
	for _, e := range compiledExts {
		if strings.HasSuffix(rel, e) {
			return true
		}
	}
	rel = "/" + filepath.ToSlash(rel)
	for _, d := range registryDirs {
		if strings.Contains(rel, d) {
			return true
		}
	}
	return false
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
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // removed while walking, e.g. a pid file
		}
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Files are scanned in parallel: a prebuild's writable layer can be
	// gigabytes (a Rust target/), and one core reads it at ~12 MB/s.
	var (
		mu    sync.Mutex
		out   []Finding
		first error
		wg    sync.WaitGroup
	)
	paths := make(chan string)
	for range max(1, runtime.NumCPU()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range paths {
				rel, _ := filepath.Rel(dir, p)
				hits, err := scanFile(p, literals, !fromRegistry(rel))
				mu.Lock()
				if err != nil && first == nil {
					first = err
				}
				for _, h := range hits {
					out = append(out, Finding{Path: "/" + filepath.ToSlash(rel), Rule: h})
				}
				mu.Unlock()
			}
		}()
	}
	for _, p := range files {
		paths <- p
	}
	close(paths)
	wg.Wait()
	if first != nil {
		return nil, first
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (r secretRule) matches(b []byte) bool {
	if r.sample == nil {
		return r.re.Match(b)
	}
	for _, m := range r.re.FindAll(b, -1) {
		if !r.sample(m) {
			return true
		}
	}
	return false
}

func containsAny(b []byte, needles [][]byte) bool {
	for _, n := range needles {
		if bytes.Contains(b, n) {
			return true
		}
	}
	return false
}

func scanFile(p string, literals [][]byte, rules bool) ([]string, error) {
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // removed since the walk: not in the layer
	}
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
			if rules && !seen[r.name] && containsAny(buf, r.markers) && r.matches(buf) {
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
