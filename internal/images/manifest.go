// Package images is the base image pipeline: a trimmed Ubuntu 24.04 built
// from GitHub's public actions/runner-images templates as an ext4 file,
// rebuilt weekly, signed, with an SBOM per image, and rolled out to canary
// hosts before the fleet.
//
// The build itself (debootstrap, the upstream install scripts, mkfs.ext4)
// runs on a Linux builder; see images/ubuntu-24.04/build.sh. This package
// holds everything above it: the build plan, the manifest, signing, the
// SBOM and the rollout state the control plane pins jobs with. Per-size
// paused Cloud Hypervisor snapshots are made on each host by hostd
// (hostd/src/baseimage.rs) from the verified ext4 file.
package images

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Ref is an image name pinned to a version: ubuntu-24.04@2026-11-16.
type Ref struct {
	Name    string
	Version string
}

var (
	nameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
	versionRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(\.\d+)?$`)
)

// ParseRef parses name@version.
func ParseRef(s string) (Ref, error) {
	n, v, ok := strings.Cut(s, "@")
	if !ok {
		return Ref{}, fmt.Errorf("image ref %q: want name@version", s)
	}
	r := Ref{Name: n, Version: v}
	return r, r.Validate()
}

func (r Ref) String() string { return r.Name + "@" + r.Version }

func (r Ref) Validate() error {
	if !nameRe.MatchString(r.Name) {
		return fmt.Errorf("image name %q is invalid", r.Name)
	}
	if !versionRe.MatchString(r.Version) {
		return fmt.Errorf("image version %q: want YYYY-MM-DD or YYYY-MM-DD.N", r.Version)
	}
	return nil
}

// WeeklyVersion is the version of a build made at t: the date of that
// week's Monday in UTC. Rebuilds within the same week get a ".N" suffix
// from the caller.
func WeeklyVersion(t time.Time) string {
	t = t.UTC()
	offset := (int(t.Weekday()) + 6) % 7 // days since Monday
	return t.AddDate(0, 0, -offset).Format("2006-01-02")
}

// lessVersion orders versions by date, then by rebuild suffix.
func lessVersion(a, b string) bool {
	ad, as, _ := strings.Cut(a, ".")
	bd, bs, _ := strings.Cut(b, ".")
	if ad != bd {
		return ad < bd
	}
	if len(as) != len(bs) {
		return len(as) < len(bs)
	}
	return as < bs
}

// Blob is one file shipped with an image.
type Blob struct {
	Path   string `json:"path"` // relative to the image directory
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Source records exactly what an image was built from, so a second build
// from the same manifest can be compared byte for byte.
type Source struct {
	RunnerImagesRepo   string `json:"runner_images_repo"`
	RunnerImagesCommit string `json:"runner_images_commit"`
	Template           string `json:"template"` // e.g. images/ubuntu/templates/build.ubuntu-24_04.pkr.hcl
	// BaseRootfsSHA256 is the digest of the Ubuntu cloud image root
	// filesystem the build started from.
	BaseRootfsSHA256 string `json:"base_rootfs_sha256"`
	// ActionsRunnerSHA256 is the digest of the GitHub Actions runner
	// release installed in the image. Empty in images built before it was
	// recorded.
	ActionsRunnerSHA256 string `json:"actions_runner_sha256,omitempty"`
	// APTSnapshot pins every apt install to snapshot.ubuntu.com at this
	// timestamp (upstream's APT_SNAPSHOT_TIMESTAMP).
	APTSnapshot string `json:"apt_snapshot"`
	// PlanSHA256 is the digest of the build plan (scripts, order, env).
	PlanSHA256 string `json:"plan_sha256"`
}

// Manifest describes one built image. It is what gets signed.
type Manifest struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	Arch            string `json:"arch"`
	Source          Source `json:"source"`
	SourceDateEpoch int64  `json:"source_date_epoch"`
	Rootfs          Blob   `json:"rootfs"`
	Kernel          *Blob  `json:"kernel,omitempty"`
	SBOM            Blob   `json:"sbom"`
	// Sizes are the VM sizes (vCPU) hostd makes paused base snapshots for.
	Sizes []int `json:"sizes"`
}

// Sizes are the VM sizes Tachyon offers, in vCPU (4 GB RAM per vCPU).
var Sizes = []int{2, 4, 8, 16}

func (m *Manifest) Ref() Ref { return Ref{Name: m.Name, Version: m.Version} }

var hexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (b Blob) validate(what string) error {
	if b.Path == "" || filepath.IsAbs(b.Path) || strings.Contains(b.Path, "..") {
		return fmt.Errorf("%s: path %q must be relative to the image directory", what, b.Path)
	}
	if !hexRe.MatchString(b.SHA256) {
		return fmt.Errorf("%s: sha256 %q is invalid", what, b.SHA256)
	}
	if b.Size <= 0 {
		return fmt.Errorf("%s: empty", what)
	}
	return nil
}

// Validate checks a manifest is complete enough to roll out.
func (m *Manifest) Validate() error {
	if err := m.Ref().Validate(); err != nil {
		return err
	}
	if m.Arch != "x86_64" && m.Arch != "arm64" {
		return fmt.Errorf("arch %q: want x86_64 or arm64", m.Arch)
	}
	s := m.Source
	if s.RunnerImagesRepo == "" || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(s.RunnerImagesCommit) {
		return errors.New("source: runner-images repo and full commit sha are required")
	}
	if s.APTSnapshot == "" || !hexRe.MatchString(s.PlanSHA256) || !hexRe.MatchString(s.BaseRootfsSHA256) {
		return errors.New("source: apt snapshot, plan digest and base rootfs digest are required")
	}
	if s.ActionsRunnerSHA256 != "" && !hexRe.MatchString(s.ActionsRunnerSHA256) {
		return errors.New("source: actions runner digest is not a sha256")
	}
	if m.SourceDateEpoch <= 0 {
		return errors.New("source_date_epoch is required")
	}
	if err := m.Rootfs.validate("rootfs"); err != nil {
		return err
	}
	if m.Kernel != nil {
		if err := m.Kernel.validate("kernel"); err != nil {
			return err
		}
	}
	if err := m.SBOM.validate("sbom"); err != nil {
		return err
	}
	if len(m.Sizes) == 0 {
		return errors.New("sizes: at least one VM size is required")
	}
	for _, v := range m.Sizes {
		if v <= 0 || v&(v-1) != 0 {
			return fmt.Errorf("sizes: %d is not a power of two", v)
		}
	}
	return nil
}

// Canonical is the manifest's signed encoding. Struct fields encode in a
// fixed order, so equal manifests give equal bytes.
func (m *Manifest) Canonical() ([]byte, error) {
	return json.Marshal(m)
}

// HashFile returns a Blob for dir/rel.
func HashFile(dir, rel string) (Blob, error) {
	f, err := os.Open(filepath.Join(dir, rel))
	if err != nil {
		return Blob{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Blob{}, err
	}
	return Blob{Path: rel, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// VerifyFiles re-hashes every file the manifest names under dir.
func (m *Manifest) VerifyFiles(dir string) error {
	blobs := []Blob{m.Rootfs, m.SBOM}
	if m.Kernel != nil {
		blobs = append(blobs, *m.Kernel)
	}
	for _, want := range blobs {
		got, err := HashFile(dir, want.Path)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("%s: sha256 %s (%d bytes), manifest says %s (%d bytes)", want.Path, got.SHA256, got.Size, want.SHA256, want.Size)
		}
	}
	return nil
}

// Diff lists what differs between two builds of the same plan. An empty
// result means the build reproduced.
func Diff(a, b *Manifest) []string {
	var d []string
	add := func(what, x, y string) {
		if x != y {
			d = append(d, fmt.Sprintf("%s: %s != %s", what, x, y))
		}
	}
	add("source.runner_images_commit", a.Source.RunnerImagesCommit, b.Source.RunnerImagesCommit)
	add("source.base_rootfs_sha256", a.Source.BaseRootfsSHA256, b.Source.BaseRootfsSHA256)
	add("source.actions_runner_sha256", a.Source.ActionsRunnerSHA256, b.Source.ActionsRunnerSHA256)
	add("source.apt_snapshot", a.Source.APTSnapshot, b.Source.APTSnapshot)
	add("source.plan_sha256", a.Source.PlanSHA256, b.Source.PlanSHA256)
	add("source_date_epoch", fmt.Sprint(a.SourceDateEpoch), fmt.Sprint(b.SourceDateEpoch))
	add("rootfs", a.Rootfs.SHA256, b.Rootfs.SHA256)
	add("sbom", a.SBOM.SHA256, b.SBOM.SHA256)
	ka, kb := "", ""
	if a.Kernel != nil {
		ka = a.Kernel.SHA256
	}
	if b.Kernel != nil {
		kb = b.Kernel.SHA256
	}
	add("kernel", ka, kb)
	return d
}
