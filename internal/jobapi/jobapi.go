// Package jobapi is Tachyon's platform-neutral job model. The core accepts
// one kind of request — run this spec, from this image and prebuild, with
// these volumes, on this hardware — and frontends (GitHub Actions first) turn
// their own events into it. No platform-specific logic lives below this
// package.
package jobapi

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"time"
)

// Arch is a guest CPU architecture.
type Arch string

const (
	ArchX86_64 Arch = "x86_64"
	ArchARM64  Arch = "arm64" // Phase 3
)

// MemGBPerVCPU is the fixed RAM-to-vCPU ratio for every VM size.
const MemGBPerVCPU = 4

// VMSizes are the supported vCPU counts.
var VMSizes = []int{2, 4, 8, 16}

// ValidSize reports whether vcpu is a supported VM size.
func ValidSize(vcpu int) bool {
	for _, s := range VMSizes {
		if s == vcpu {
			return true
		}
	}
	return false
}

// Trust classifies how far a job's writes may propagate.
type Trust string

const (
	// Trusted: default and protected branches. May build prebuilds, promote
	// sticky volumes and write repo-wide cache entries.
	Trusted Trust = "trusted"
	// Untrusted: same-repo pull requests. Cache writes stay with the PR.
	Untrusted Trust = "untrusted"
	// UntrustedFork: pull requests from forks. Read-only everywhere.
	UntrustedFork Trust = "untrusted-fork"
)

// Hardware is the VM shape a job needs.
type Hardware struct {
	Arch     Arch `yaml:"arch" json:"arch"`
	VCPU     int  `yaml:"vcpu" json:"vcpu"`
	MemoryGB int  `yaml:"memory_gb" json:"memory_gb"`
}

// NewHardware returns the standard shape for vcpu.
func NewHardware(arch Arch, vcpu int) Hardware {
	return Hardware{Arch: arch, VCPU: vcpu, MemoryGB: vcpu * MemGBPerVCPU}
}

// PrebuildPolicy selects which prebuild a job forks.
type PrebuildPolicy string

const (
	PrebuildLatestTrusted PrebuildPolicy = "latest-trusted"
	PrebuildNone          PrebuildPolicy = "none" // tachyon-clean
)

type PrebuildRef struct {
	Repo   string         `yaml:"repo" json:"repo"`
	Policy PrebuildPolicy `yaml:"policy" json:"policy"`
}

type VolumeKind string

const VolumeSticky VolumeKind = "sticky"

type Volume struct {
	Kind  VolumeKind `yaml:"kind" json:"kind"`
	Key   string     `yaml:"key" json:"key"`
	Mount string     `yaml:"mount" json:"mount"`
}

// DockerVolume is always attached: Docker's data directory must be on a real
// filesystem, not the overlay.
var DockerVolume = Volume{Kind: VolumeSticky, Key: "docker", Mount: "/var/lib/docker"}

// WithDockerVolume returns vols with DockerVolume appended unless some volume
// is already mounted at /var/lib/docker. The guest's root is an overlay, and
// Docker's overlay2 storage driver cannot run on top of one, so every job
// gets the docker sticky volume whether or not its spec asked for it.
func WithDockerVolume(vols []Volume) []Volume {
	for _, v := range vols {
		if path.Clean(v.Mount) == DockerVolume.Mount {
			return vols
		}
	}
	out := make([]Volume, 0, len(vols)+1)
	out = append(out, vols...)
	return append(out, DockerVolume)
}

// validateVolumes checks that every mount is an absolute, clean path other
// than the root (the root is the overlay), that the reserved docker key is
// used only at /var/lib/docker, and that keys and mounts are unique. These
// are the same rules hostd's plan_volumes (hostd/src/rootfs.rs) enforces.
func validateVolumes(vols []Volume) error {
	keys := map[string]bool{}
	mounts := map[string]bool{}
	for _, v := range vols {
		switch {
		case v.Key == "":
			return fmt.Errorf("volume key required (mount %q)", v.Mount)
		case !path.IsAbs(v.Mount) || path.Clean(v.Mount) != v.Mount:
			return fmt.Errorf("volume %q: mount %q must be a clean absolute path", v.Key, v.Mount)
		case v.Mount == "/":
			return fmt.Errorf("volume %q: cannot mount over the overlay root", v.Key)
		case v.Key == DockerVolume.Key && v.Mount != DockerVolume.Mount:
			return fmt.Errorf("volume key %q is reserved for %s, not %q", DockerVolume.Key, DockerVolume.Mount, v.Mount)
		case keys[v.Key]:
			return fmt.Errorf("duplicate volume key %q", v.Key)
		case mounts[v.Mount]:
			return fmt.Errorf("duplicate volume mount %q", v.Mount)
		}
		keys[v.Key] = true
		mounts[v.Mount] = true
	}
	return nil
}

type BootstrapKind string

const BootstrapGitHubRunner BootstrapKind = "github-runner"

type Bootstrap struct {
	Kind BootstrapKind `yaml:"kind" json:"kind"`
	// JITConfig is a secret reference (secret://...), never the secret.
	JITConfig string `yaml:"jit_config" json:"jit_config"`
}

// Spec is the internal job API request (see docs/architecture.md).
type Spec struct {
	JobID     string      `yaml:"job_id" json:"job_id"`
	Tenant    string      `yaml:"tenant" json:"tenant"`
	Repo      string      `yaml:"repo" json:"repo"`
	Commit    string      `yaml:"commit" json:"commit"`
	Hardware  Hardware    `yaml:"hardware" json:"hardware"`
	Image     string      `yaml:"image" json:"image"` // name@version once pinned
	Prebuild  PrebuildRef `yaml:"prebuild" json:"prebuild"`
	Volumes   []Volume    `yaml:"volumes" json:"volumes"`
	Bootstrap Bootstrap   `yaml:"bootstrap" json:"bootstrap"`
	Trust     Trust       `yaml:"trust" json:"trust"`
}

// Validate checks the fields the core relies on.
func (s Spec) Validate() error {
	switch {
	case s.JobID == "":
		return fmt.Errorf("spec: job_id required")
	case s.Tenant == "" || s.Repo == "" || s.Commit == "":
		return fmt.Errorf("spec %s: tenant, repo and commit required", s.JobID)
	case !ValidSize(s.Hardware.VCPU):
		return fmt.Errorf("spec %s: unsupported vcpu %d", s.JobID, s.Hardware.VCPU)
	case s.Hardware.MemoryGB != s.Hardware.VCPU*MemGBPerVCPU:
		return fmt.Errorf("spec %s: memory_gb must be %d for %d vcpu", s.JobID, s.Hardware.VCPU*MemGBPerVCPU, s.Hardware.VCPU)
	case s.Image == "":
		return fmt.Errorf("spec %s: image required", s.JobID)
	}
	switch s.Trust {
	case Trusted, Untrusted, UntrustedFork:
	default:
		return fmt.Errorf("spec %s: invalid trust %q", s.JobID, s.Trust)
	}
	if err := validateVolumes(s.Volumes); err != nil {
		return fmt.Errorf("spec %s: %w", s.JobID, err)
	}
	return nil
}

// Submission is a spec plus the scheduling metadata the frontend knows.
type Submission struct {
	Spec Spec
	// Profile groups jobs for usage-based packing: repo/workflow/job name.
	Profile string
	// ChangedFiles, when known, lets the prebuild planner mark the fork
	// partial if a key file changed.
	ChangedFiles []string
	// Deferrable jobs run in the discounted off-peak class.
	Deferrable bool
	Region     string // empty = any
	QueuedAt   time.Time
}

// Hint is an early signal (push, pull request) that jobs for this commit are
// likely coming, so the scheduler can pre-fork a job disk before `queued`.
type Hint struct {
	Tenant       string
	Repo         string
	Commit       string
	Ref          string
	ChangedFiles []string
	Trust        Trust
	ReceivedAt   time.Time
}

// Core is what frontends submit to.
type Core interface {
	Submit(ctx context.Context, sub Submission) error
	Hint(ctx context.Context, hint Hint) error
}

// Frontend adapts one CI platform to Tachyon.
type Frontend interface {
	Name() string
	// Handler serves the platform's webhooks and forwards work to core.
	Handler(core Core) http.Handler
}
