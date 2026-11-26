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
		"private-key":             "-----BEGIN OPENSSH PRIVATE KEY-----\nabc",
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
	key := "-----BEGIN RSA PRIVATE KEY-----\nabc"
	files := map[string]string{
		"home/runner/work/app/node_modules/selfsigned/README.md": key,
		"opt/py/lib/python3.11/site-packages/x/tests/key.pem":    key,
		"home/runner/work/app/node_modules/x/cache.json":         "cached: runner-secret",
		"home/runner/work/app/certs/dev.pem":                     key,
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
