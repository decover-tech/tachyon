package guest

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestScanSecretsRules(t *testing.T) {
	tests := map[string]string{
		"github-token":            "ghs_" + strings.Repeat("A1", 18),
		"github-fine-grained-pat": "github_pat_" + strings.Repeat("a", 82),
		"aws-access-key-id":       "AKIA" + strings.Repeat("Z", 16),
		"npm-token":               "//registry.npmjs.org/:_authToken=npm_" + strings.Repeat("b", 36),
		"slack-token":             "xoxb-1234567890-abcdef",
		"private-key":             "-----BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Repeat("b3BlbnNzaC1rZXktdjEAAAAA", 3) + "\n",
	}
	for rule, body := range tests {
		t.Run(rule, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "f"), []byte("prefix "+body+" suffix"), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := ScanSecrets(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			if want := []Finding{{Path: "/f", Rule: rule}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("findings = %v", got)
			}
		})
	}
}

func TestScanSecretsClean(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"node_modules/x/index.js": "module.exports = 'ghp_short'; // AKIA lower",
		"go/pkg/mod/cache":        strings.Repeat("0123456789", 1000),
		"README":                  "-----BEGIN PUBLIC KEY-----",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink to a secret outside the layer is not followed.
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("AKIA"+strings.Repeat("Z", 16)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := ScanSecrets(dir, [][]byte{[]byte("runner-secret")})
	if err != nil || len(got) != 0 {
		t.Fatalf("findings = %v, %v", got, err)
	}
}

func TestScanSecretsRegistryDirs(t *testing.T) {
	dir := t.TempDir()
	key := "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("MIIEpAIBAAKCAQEA", 4) + "\n"
	files := map[string]string{
		"home/runner/work/app/node_modules/selfsigned/README.md": key,
		"opt/py/lib/python3.11/site-packages/x/tests/key.pem":    key,
		"tmp/node-compile-cache/v23.11.0-x64-1-1001/3325a846":    key,
		"home/runner/work/app/node_modules/x/cache.json":         "cached: runner-secret",
		"home/runner/work/app/certs/dev.pem":                     key,
		"home/runner/work/app/target/debug/deps/libpem.rlib":     key,
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ScanSecrets(dir, [][]byte{[]byte("runner-secret")})
	if err != nil {
		t.Fatal(err)
	}
	want := []Finding{
		{Path: "/home/runner/work/app/certs/dev.pem", Rule: "private-key"},
		{Path: "/home/runner/work/app/node_modules/x/cache.json", Rule: "runner-config"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %v", got)
	}
}

func TestScanSecretsAcrossChunkBoundary(t *testing.T) {
	dir := t.TempDir()
	token := "ghp_" + strings.Repeat("c", 36)
	literal := []byte("this-is-the-runner-config")
	body := strings.Repeat(" ", scanChunk-10) + token + strings.Repeat(" ", scanChunk-35) + string(literal)
	if err := os.WriteFile(filepath.Join(dir, "big"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ScanSecrets(dir, [][]byte{literal})
	if err != nil {
		t.Fatal(err)
	}
	want := []Finding{{Path: "/big", Rule: "github-token"}, {Path: "/big", Rule: "runner-config"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %v", got)
	}
}

func TestScanSecretsMissingDir(t *testing.T) {
	got, err := ScanSecrets(filepath.Join(t.TempDir(), "nope"), nil)
	if err != nil || got != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestScanSecretsIgnoresSamplesAndBareHeaders(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"faults.py":      `key = "AKIAIOSFODNN7EXAMPLE"`,
		"librustls.rlib": "\x00-----BEGIN PRIVATE KEY-----\x00-----END PRIVATE KEY-----",
		"narrow.pem":     "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("MIIEvQIBADANBgkqhkiG\n", 4) + "-----END PRIVATE KEY-----\n",
		"pgp.asc":        "-----BEGIN PGP PRIVATE KEY BLOCK-----\nVersion: x\n\n" + strings.Repeat("lQOYBF", 12) + "\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ScanSecrets(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Finding{{Path: "/narrow.pem", Rule: "private-key"}, {Path: "/pgp.asc", Rule: "private-key"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %v, want only the real keys", got)
	}
}

func TestScanFileRemovedSinceWalk(t *testing.T) {
	hits, err := scanFile(filepath.Join(t.TempDir(), "docker.pid"), nil, true)
	if err != nil || hits != nil {
		t.Fatalf("scanFile(missing) = %v, %v; want nil, nil", hits, err)
	}
}
