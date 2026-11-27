package images

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRef(t *testing.T) {
	r, err := ParseRef("ubuntu-24.04@2026-11-16")
	if err != nil || r.Name != "ubuntu-24.04" || r.Version != "2026-11-16" {
		t.Fatalf("got %+v, %v", r, err)
	}
	if _, err := ParseRef("ubuntu-24.04@2026-11-16.2"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"ubuntu-24.04", "Ubuntu@2026-11-16", "ubuntu@latest", "@2026-11-16"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestWeeklyVersion(t *testing.T) {
	for in, want := range map[string]string{
		"2026-11-16T00:00:00Z": "2026-11-16", // Monday
		"2026-11-21T23:59:00Z": "2026-11-16", // Saturday
		"2026-11-22T10:00:00Z": "2026-11-16", // Sunday
		"2026-11-23T01:00:00Z": "2026-11-23",
		"2027-01-01T00:00:00Z": "2026-12-28", // across a year boundary
	} {
		ts, _ := time.Parse(time.RFC3339, in)
		if got := WeeklyVersion(ts); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
	if !lessVersion("2026-11-16", "2026-11-16.1") || !lessVersion("2026-11-16.9", "2026-11-16.10") || lessVersion("2026-11-23", "2026-11-16.3") {
		t.Fatal("lessVersion order")
	}
}

const sha = "0e63d4f9d2c29043b4cf1d2f40d1812a7cde1369"

func testManifest(t *testing.T, dir, version string) *Manifest {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	for name, data := range map[string]string{"rootfs.ext4": "ext4 bytes " + version, "sbom.spdx.json": "{}"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rootfs, err := HashFile(dir, "rootfs.ext4")
	if err != nil {
		t.Fatal(err)
	}
	sbom, _ := HashFile(dir, "sbom.spdx.json")
	return &Manifest{
		Name: "ubuntu-24.04", Version: version, Arch: "x86_64",
		Source: Source{
			RunnerImagesRepo: "https://github.com/actions/runner-images", RunnerImagesCommit: sha,
			Template: "images/ubuntu/templates/build.ubuntu-24_04.pkr.hcl", APTSnapshot: "20261116T000000Z",
			PlanSHA256: strings.Repeat("a", 64), BaseRootfsSHA256: strings.Repeat("b", 64),
		},
		SourceDateEpoch: 1763251200,
		Rootfs:          rootfs,
		SBOM:            sbom,
		Sizes:           Sizes,
	}
}

func TestManifestVerifyFiles(t *testing.T) {
	dir := t.TempDir()
	m := testManifest(t, dir, "2026-11-16")
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := m.VerifyFiles(dir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "rootfs.ext4"), []byte("tampered"), 0o644)
	if err := m.VerifyFiles(dir); err == nil {
		t.Fatal("tampered rootfs verified")
	}

	bad := *m
	bad.Rootfs.Path = "../etc/passwd"
	if bad.Validate() == nil {
		t.Error("path escape accepted")
	}
	bad = *m
	bad.Sizes = []int{3}
	if bad.Validate() == nil {
		t.Error("odd VM size accepted")
	}
}

func TestDiff(t *testing.T) {
	a := testManifest(t, "", "2026-11-16")
	b := *a
	if d := Diff(a, &b); len(d) != 0 {
		t.Fatalf("identical builds differ: %v", d)
	}
	b.Rootfs.SHA256 = strings.Repeat("b", 64)
	if d := Diff(a, &b); len(d) != 1 || !strings.HasPrefix(d[0], "rootfs:") {
		t.Fatalf("got %v", d)
	}
}

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestSignVerify(t *testing.T) {
	pub, priv := keypair(t)
	other, _ := keypair(t)
	m := testManifest(t, "", "2026-11-16")
	s, err := Sign(m, priv)
	if err != nil {
		t.Fatal(err)
	}
	// Round-trip through JSON, as it is stored next to the image.
	raw, _ := json.MarshalIndent(s, "", "  ")
	var got Signed
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	vm, err := got.Verify([]ed25519.PublicKey{other, pub})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(vm, m) {
		t.Fatalf("verified manifest differs:\n%+v\n%+v", vm, m)
	}
	if _, err := got.Verify([]ed25519.PublicKey{other}); err == nil {
		t.Fatal("verified with an untrusted key")
	}
	tampered := got
	tampered.Manifest = []byte(strings.Replace(string(got.Manifest), m.Rootfs.SHA256, strings.Repeat("0", 64), 1))
	if string(tampered.Manifest) == string(got.Manifest) {
		t.Fatal("test did not tamper")
	}
	if _, err := tampered.Verify([]ed25519.PublicKey{pub}); err == nil {
		t.Fatal("tampered manifest verified")
	}

	invalid := *m
	invalid.Source.RunnerImagesCommit = "main"
	if _, err := Sign(&invalid, priv); err == nil {
		t.Fatal("signed a manifest with an unpinned source")
	}
}

// hostd verifies what this package signs. The fixture is checked by both
// sides: here it must match what Sign produces (ed25519 is deterministic),
// and hostd/src/baseimage.rs verifies it with the same seed's public key.
// Regenerate with TACHYON_UPDATE_FIXTURES=1 go test ./internal/images.
func TestCrossLanguageFixture(t *testing.T) {
	const fixture = "../../hostd/testdata/signed-manifest.json"
	priv := ed25519.NewKeyFromSeed([]byte(strings.Repeat("\x07", 32)))
	sum := func(c string) string { return strings.Repeat(c, 64) }
	m := &Manifest{
		Name: "ubuntu-24.04", Version: "2026-11-16", Arch: "x86_64",
		Source: Source{
			RunnerImagesRepo: "https://github.com/actions/runner-images", RunnerImagesCommit: sha,
			Template:         "images/ubuntu/templates/build.ubuntu-24_04.pkr.hcl",
			BaseRootfsSHA256: sum("b"), ActionsRunnerSHA256: sum("c"), APTSnapshot: "20261116T000000Z", PlanSHA256: sum("a"),
		},
		SourceDateEpoch: 1763251200,
		Rootfs:          Blob{Path: "rootfs.ext4", SHA256: sum("1"), Size: 68719476736},
		Kernel:          &Blob{Path: "vmlinux", SHA256: sum("2"), Size: 40000000},
		SBOM:            Blob{Path: "sbom.spdx.json", SHA256: sum("3"), Size: 1234},
		Sizes:           Sizes,
	}
	s, err := Sign(m, priv)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.MarshalIndent(s, "", "  ")
	got = append(got, '\n')
	if os.Getenv("TACHYON_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(fixture, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s is stale; regenerate with TACHYON_UPDATE_FIXTURES=1", fixture)
	}
}

func TestParseKeys(t *testing.T) {
	pub, priv := keypair(t)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	k, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil || !k.Equal(priv) {
		t.Fatalf("private key: %v", err)
	}
	pder, _ := x509.MarshalPKIXPublicKey(pub)
	block := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pder})
	keys, err := ParsePublicKeys(append(block, block...))
	if err != nil || len(keys) != 2 || !keys[0].Equal(pub) {
		t.Fatalf("public keys: %v %d", err, len(keys))
	}
	if _, err := ParsePublicKeys([]byte("nope")); err == nil {
		t.Fatal("parsed garbage")
	}
}

func TestSBOM(t *testing.T) {
	debs, err := ParseDpkg(strings.NewReader("zlib1g\t1:1.3.dfsg-3.1ubuntu2\tamd64\ngit\t1:2.43.0-1ubuntu7\tamd64\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	tools, err := ParseToolcache(strings.NewReader("node\t22.11.0\nPython\t3.12.7\n"))
	if err != nil {
		t.Fatal(err)
	}
	ref := Ref{"ubuntu-24.04", "2026-11-16"}
	a, err := SBOM(ref, 1763251200, append(debs, tools...))
	if err != nil {
		t.Fatal(err)
	}
	// Input order must not matter.
	b, _ := SBOM(ref, 1763251200, append(tools, debs[1], debs[0]))
	if string(a) != string(b) {
		t.Fatal("SBOM depends on input order")
	}

	var doc struct {
		SPDXVersion  string `json:"spdxVersion"`
		CreationInfo struct {
			Created string `json:"created"`
		} `json:"creationInfo"`
		Packages []struct {
			Name         string `json:"name"`
			SPDXID       string `json:"SPDXID"`
			ExternalRefs []struct {
				Locator string `json:"referenceLocator"`
			} `json:"externalRefs"`
		} `json:"packages"`
		Relationships []json.RawMessage `json:"relationships"`
	}
	if err := json.Unmarshal(a, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SPDXVersion != "SPDX-2.3" || doc.CreationInfo.Created != "2025-11-16T00:00:00Z" {
		t.Fatalf("header: %+v", doc)
	}
	if len(doc.Packages) != 5 || len(doc.Relationships) != 5 {
		t.Fatalf("got %d packages, %d relationships", len(doc.Packages), len(doc.Relationships))
	}
	if got := doc.Packages[1].ExternalRefs[0].Locator; got != "pkg:deb/ubuntu/git@1:2.43.0-1ubuntu7?arch=amd64&distro=ubuntu-24.04" {
		t.Errorf("purl %s", got)
	}
	if got := doc.Packages[1].SPDXID; got != "SPDXRef-deb-git-1-2.43.0-1ubuntu7-amd64" {
		t.Errorf("SPDXID %s", got)
	}

	if _, err := ParseDpkg(strings.NewReader("git 2.43\n")); err == nil {
		t.Fatal("parsed a malformed dpkg line")
	}
}

func testConfig(t *testing.T, skip ...Skip) *Config {
	t.Helper()
	c, err := ParseConfig([]byte(`
name: ubuntu-24.04
arch: x86_64
runner_images:
  repo: https://github.com/actions/runner-images
  commit: ` + sha + `
  template: images/ubuntu/templates/build.ubuntu-24_04.pkr.hcl
  toolset: images/ubuntu/toolsets/toolset-2404.json
base_rootfs:
  url: https://cloud-images.ubuntu.com/releases/noble/release-20261017/ubuntu-24.04-server-cloudimg-amd64-root.tar.xz
  sha256: ec6972af8ea2bd05c55299d204e5ae3f91769b84cfaec7395639d236a39f9a03
actions_runner:
  url: https://github.com/actions/runner/releases/download/v2.338.2/actions-runner-linux-x64-2.338.2.tar.gz
  sha256: baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40
rootfs_size_gb: 64
vars:
  helper_script_folder: /imagegeneration/helpers
  installer_script_folder: /imagegeneration/installers
  image_os: ubuntu24
`))
	if err != nil {
		t.Fatal(err)
	}
	c.Skip = skip
	return c
}

func TestPlan(t *testing.T) {
	tmpl, err := os.ReadFile("testdata/build.ubuntu-24_04.pkr.hcl")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, Skip{"install-android-sdk.sh", "big"}, Skip{"Install-PowerShellAzModules.ps1", "azure"})
	bv := BuildVars{ImageVersion: "2026-11-16", APTSnapshot: "20261116T000000Z"}
	p, err := MakePlan(cfg, tmpl, bv)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range p.Steps {
		names = append(names, s.Script)
	}
	want := []string{
		"install-ms-repos.sh", "configure-apt-sources.sh", "configure-apt.sh",
		"configure-environment.sh", "Install-PowerShellModules.ps1",
		"install-git.sh", "install-nodejs.sh", "install-homebrew.sh", "cleanup.sh",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("steps:\n got %v\nwant %v", names, want)
	}
	if !reflect.DeepEqual(p.Skipped, []string{"Install-PowerShellAzModules.ps1", "install-android-sdk.sh"}) {
		t.Fatalf("skipped %v", p.Skipped)
	}

	apt := p.Steps[0]
	if !apt.Root || apt.Shell != "sh" || !reflect.DeepEqual(apt.Env, []string{
		"HELPER_SCRIPTS=/imagegeneration/helpers", "DEBIAN_FRONTEND=noninteractive", "APT_SNAPSHOT_TIMESTAMP=20261116T000000Z",
	}) {
		t.Fatalf("apt step %+v", apt)
	}
	if p.Steps[3].Env[0] != "IMAGE_VERSION=2026-11-16" || p.Steps[3].Env[1] != "IMAGE_OS=ubuntu24" {
		t.Fatalf("env %v", p.Steps[3].Env)
	}
	if s := p.Steps[4]; s.Shell != "pwsh" || !s.Root {
		t.Fatalf("pwsh step %+v", s)
	}
	if s := p.Steps[7]; s.Root {
		t.Fatalf("homebrew runs as the user upstream: %+v", s)
	}
	// cleanup.sh has no environment_vars.
	if s := p.Steps[8]; len(s.Env) != 0 || !s.Root {
		t.Fatalf("cleanup %+v", s)
	}

	sh := p.Shell()
	if !strings.Contains(sh, "run_step root sh 'install-git.sh' 'HELPER_SCRIPTS=/imagegeneration/helpers'") ||
		!strings.Contains(sh, "run_step user sh 'install-homebrew.sh'") ||
		!strings.Contains(sh, "# skipped: install-android-sdk.sh") {
		t.Fatalf("shell plan:\n%s", sh)
	}

	// Same inputs, same digest; different apt snapshot, different digest.
	p2, _ := MakePlan(cfg, tmpl, bv)
	if p.Digest() != p2.Digest() {
		t.Fatal("plan digest not stable")
	}
	p3, _ := MakePlan(cfg, tmpl, BuildVars{ImageVersion: "2026-11-16", APTSnapshot: "20261117T000000Z"})
	if p.Digest() == p3.Digest() {
		t.Fatal("plan digest ignores the apt snapshot")
	}
}

func TestPlanRejectsStaleSkipAndMissingVars(t *testing.T) {
	tmpl, _ := os.ReadFile("testdata/build.ubuntu-24_04.pkr.hcl")
	bv := BuildVars{ImageVersion: "2026-11-16", APTSnapshot: "x"}
	if _, err := MakePlan(testConfig(t, Skip{"install-renamed.sh", "gone"}), tmpl, bv); err == nil || !strings.Contains(err.Error(), "install-renamed.sh") {
		t.Fatalf("stale skip: %v", err)
	}
	cfg := testConfig(t)
	delete(cfg.Vars, "image_os")
	if _, err := MakePlan(cfg, tmpl, bv); err == nil || !strings.Contains(err.Error(), "var.image_os") {
		t.Fatalf("missing var: %v", err)
	}
	if _, err := MakePlan(testConfig(t), []byte(`build { }`), bv); err == nil {
		t.Fatal("empty template planned")
	}
}

func TestShellEnv(t *testing.T) {
	c := testConfig(t)
	c.Name = "it's"
	env := c.ShellEnv()
	for _, want := range []string{
		"IMAGE_NAME='it'\\''s'\n",
		"RUNNER_IMAGES_COMMIT='" + sha + "'\n",
		"ROOTFS_SIZE_GB='64'\n",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %q in\n%s", want, env)
		}
	}
}

func TestParseConfig(t *testing.T) {
	data, err := os.ReadFile("../../images/ubuntu-24.04/image.yml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "ubuntu-24.04" || len(c.Skip) == 0 {
		t.Fatalf("%+v", c)
	}
	for _, bad := range []string{
		strings.Replace(string(data), "commit: "+c.RunnerImages.Commit, "commit: main", 1),
		string(data) + "\nunknown_field: 1\n",
		strings.Replace(string(data), "skip:\n", "skip:\n  - script: install-julia.sh\n    reason: twice\n", 1),
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
}
