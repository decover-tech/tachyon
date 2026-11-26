package prebuild

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

// The example from docs/architecture.md.
const docExample = `# .tachyon/prebuild.yml
version: 1
key_files: [package-lock.json, go.sum, .tool-versions]
paths: [~/.npm, ~/go/pkg/mod, ~/.cache/go-build, node_modules]
warm:
  - run: npm ci
  - run: go mod download
  - run: docker pull postgres:16
memory_snapshot: false
`

func TestParseDocExample(t *testing.T) {
	c, err := Parse([]byte(docExample))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 1 || len(c.KeyFiles) != 3 || len(c.Paths) != 4 || len(c.Warm) != 3 || c.Warm[2].Run != "docker pull postgres:16" {
		t.Fatalf("parsed = %+v", c)
	}
	if got := c.WorkspacePaths(); !reflect.DeepEqual(got, []string{"node_modules"}) {
		t.Errorf("WorkspacePaths = %v", got)
	}
}

func TestParseRejects(t *testing.T) {
	tests := map[string]string{
		"unknown field":   "version: 1\nwarmup: []\n",
		"memory snapshot": "version: 1\nmemory_snapshot: true\n",
		"version 2":       "version: 2\n",
		"missing version": "paths: [a]\n",
		"empty run":       "version: 1\nwarm:\n  - run: \"  \"\n",
		"absolute key":    "version: 1\nkey_files: [/etc/passwd]\n",
		"escaping key":    "version: 1\nkey_files: [../x]\n",
		"bad glob":        "version: 1\nkey_files: [\"[\"]\n",
	}
	for name, src := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestWorkspacePaths(t *testing.T) {
	c := &Config{Paths: []string{"~/.npm", "/opt/cache", "$HOME/x", "node_modules", "./web/node_modules/", "vendor"}}
	want := []string{"node_modules", "web/node_modules", "vendor"}
	if got := c.WorkspacePaths(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestTouchesKeyFiles(t *testing.T) {
	c := &Config{KeyFiles: []string{"go.sum", "*/package-lock.json"}}
	tests := []struct {
		changed []string
		want    bool
	}{
		{nil, false},
		{[]string{"main.go"}, false},
		{[]string{"main.go", "go.sum"}, true},
		{[]string{"web/package-lock.json"}, true},
		{[]string{"a/b/package-lock.json"}, false}, // path.Match * doesn't cross /
		{[]string{ConfigPath}, true},
	}
	for _, tt := range tests {
		if got := c.TouchesKeyFiles(tt.changed); got != tt.want {
			t.Errorf("TouchesKeyFiles(%v) = %v", tt.changed, got)
		}
	}
}

func TestKey(t *testing.T) {
	files := map[string][]byte{"go.sum": []byte("a"), "package-lock.json": []byte("b")}
	k1 := Key("img@1", []byte("cfg"), files)
	if !strings.HasPrefix(k1, "pb1-") || len(k1) != 4+32 {
		t.Fatalf("key format %q", k1)
	}
	// Map iteration order must not matter.
	for i := 0; i < 20; i++ {
		f2 := map[string][]byte{"package-lock.json": []byte("b"), "go.sum": []byte("a")}
		if Key("img@1", []byte("cfg"), f2) != k1 {
			t.Fatal("key not order-independent")
		}
	}
	variants := map[string]string{
		"image":   Key("img@2", []byte("cfg"), files),
		"config":  Key("img@1", []byte("cfg2"), files),
		"content": Key("img@1", []byte("cfg"), map[string][]byte{"go.sum": []byte("A"), "package-lock.json": []byte("b")}),
		"missing": Key("img@1", []byte("cfg"), map[string][]byte{"go.sum": []byte("a")}),
		// Length-prefixing stops boundary shifts from colliding.
		"shift": Key("img@1", []byte("cfg"), map[string][]byte{"go.sum": []byte("ab"), "package-lock.json": nil}),
	}
	for name, k := range variants {
		if k == k1 {
			t.Errorf("%s change did not change key", name)
		}
	}
}

func TestReadKeyFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.sum", "gosum")
	write("web/package-lock.json", "lock")
	write("node_modules/x/package-lock.json", "ignored")
	write(".git/go.sum", "ignored")
	write("main.go", "x")
	write(ConfigPath, "version: 1\nkey_files: [go.sum, \"*/package-lock.json\", .tool-versions]\n")

	c, raw, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("Load returned no raw bytes")
	}
	got, err := ReadKeyFiles(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{"go.sum": []byte("gosum"), "web/package-lock.json": []byte("lock")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestPlanFork(t *testing.T) {
	cfg := &Config{Version: 1, KeyFiles: []string{"go.sum"}}
	spec := jobapi.Spec{Image: "ubuntu-24.04@1", Prebuild: jobapi.PrebuildRef{Repo: "acme/api", Policy: jobapi.PrebuildLatestTrusted}}
	snap := &Snapshot{Repo: "acme/api", BaseImage: "ubuntu-24.04@1", Healthy: true}
	clean := spec
	clean.Prebuild.Policy = jobapi.PrebuildNone
	damaged := *snap
	damaged.Healthy = false
	oldImage := *snap
	oldImage.BaseImage = "ubuntu-24.04@0"
	sameCommit := *snap
	sameCommit.Commit = "c1"
	atCommit := spec
	atCommit.Commit = "c1"

	tests := []struct {
		name     string
		spec     jobapi.Spec
		cfg      *Config
		changed  []string
		snap     *Snapshot
		want     Match
		wantSnap bool
	}{
		{"clean label", clean, cfg, nil, snap, MatchNone, false},
		{"no config", spec, nil, nil, snap, MatchNone, false},
		{"no prebuild", spec, cfg, nil, nil, MatchNone, false},
		{"damaged", spec, cfg, nil, &damaged, MatchNone, false},
		{"other image", spec, cfg, nil, &oldImage, MatchNone, false},
		{"key file changed", spec, cfg, []string{"go.sum"}, snap, MatchPartial, true},
		{"full", spec, cfg, []string{"main.go"}, snap, MatchFull, true},
		{"key file changed by the prebuilt commit", atCommit, cfg, []string{"go.sum"}, &sameCommit, MatchFull, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := PlanFork(tt.spec, tt.cfg, tt.changed, tt.snap)
			if f.Match != tt.want || (f.Snapshot != nil) != tt.wantSnap {
				t.Fatalf("got %+v", f)
			}
			if f.Match != MatchFull && f.Reason == "" {
				t.Error("non-full fork should give a reason")
			}
		})
	}
}

func TestShouldBuild(t *testing.T) {
	now := time.Now()
	if !ShouldBuild(jobapi.Trusted, now.Add(-time.Hour), now, 10*time.Minute) {
		t.Error("trusted after interval should build")
	}
	if ShouldBuild(jobapi.Trusted, now.Add(-time.Minute), now, 10*time.Minute) {
		t.Error("within interval should not build")
	}
	if ShouldBuild(jobapi.Untrusted, time.Time{}, now, 0) {
		t.Error("untrusted must never build")
	}
}
