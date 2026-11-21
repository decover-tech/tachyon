// Package controlplane is the core behind the job API: it plans each job's
// fork, places it with the scheduler, and dispatches it to a host over the
// hostlink stream. It implements jobapi.Core.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/jobapi"
	"github.com/decover-tech/tachyon/internal/mirrors"
	"github.com/decover-tech/tachyon/internal/prebuild"
	"github.com/decover-tech/tachyon/internal/scheduler"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

// Images pins an image name (ubuntu-24.04) to its live version.
type Images interface {
	Pin(name string) (string, error) // -> ubuntu-24.04@2026-11-16
}

// Prebuilds is the prebuild registry.
type Prebuilds interface {
	// Latest returns the newest healthy trusted prebuild and the config it
	// was built from; nil if the repo has none.
	Latest(repo string) (*prebuild.Snapshot, *prebuild.Config)
	// Register records a new prebuild reported by a host.
	Register(s prebuild.Snapshot)
}

// SecretResolver turns a secret:// reference into its value.
type SecretResolver interface {
	Get(ctx context.Context, ref string) ([]byte, error)
}

// Burst runs jobs on rented on-demand capacity when owned hosts are full.
type Burst interface {
	Submit(ctx context.Context, sub jobapi.Submission) error
}

// Core implements jobapi.Core.
type Core struct {
	Scheduler *scheduler.Scheduler
	Images    Images
	Prebuilds Prebuilds
	Secrets   SecretResolver
	Burst     Burst
	Prebuild  PrebuildManager
	Mirrors   mirrors.Endpoints
	CacheURL  string
	Log       *slog.Logger
	Now       func() time.Time

	mu       sync.Mutex
	pbJobs   map[string]string // prebuild build job ID -> repo (prebuilds.go)
	hosts    map[string]*hostConn
	hints    map[string]hintEntry // repo@commit -> changed files
	deferred []jobapi.Submission
	records  map[string]*telemetry.Record
}

type hintEntry struct {
	changed []string
	at      time.Time
}

const hintTTL = 30 * time.Minute

var _ jobapi.Core = (*Core)(nil)

func (c *Core) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Core) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c *Core) init() {
	if c.hosts == nil {
		c.hosts = map[string]*hostConn{}
		c.hints = map[string]hintEntry{}
		c.records = map[string]*telemetry.Record{}
	}
}

// Hint stores the changed files for a commit and pre-forks a job disk on
// the host most likely to get its jobs.
func (c *Core) Hint(ctx context.Context, h jobapi.Hint) error {
	c.mu.Lock()
	c.init()
	c.hints[h.Repo+"@"+h.Commit] = hintEntry{changed: h.ChangedFiles, at: c.now()}
	for k, e := range c.hints {
		if c.now().Sub(e.at) > hintTTL {
			delete(c.hints, k)
		}
	}
	c.mu.Unlock()
	c.prebuildHint(ctx, h)

	repo := shortRepo(h.Repo)
	snap, _ := c.Prebuilds.Latest(repo)
	if snap == nil {
		return nil
	}
	hc, ref := c.hostWithPrebuild(repo)
	if hc == nil {
		return nil
	}
	hc.send(&hostlinkv1.LinkResponse{Msg: &hostlinkv1.LinkResponse_PreFork{PreFork: &hostlinkv1.PreFork{
		PreForkId:          h.Repo + "@" + h.Commit,
		Repo:               repo,
		PrebuildSnapshotId: ref.GetId(),
	}}})
	return nil
}

// Submit plans, places and dispatches a job.
func (c *Core) Submit(ctx context.Context, sub jobapi.Submission) error {
	spec := &sub.Spec
	pinned, err := c.Images.Pin(spec.Image)
	if err != nil {
		return fmt.Errorf("pin image %s: %w", spec.Image, err)
	}
	spec.Image = pinned
	if err := spec.Validate(); err != nil {
		return err
	}

	rec := telemetry.NewRecord(spec.JobID)
	rec.Tenant, rec.Repo, rec.VCPU = spec.Tenant, spec.Repo, spec.Hardware.VCPU
	rec.Mark(telemetry.WebhookReceived, sub.QueuedAt)

	c.mu.Lock()
	c.init()
	if len(sub.ChangedFiles) == 0 {
		sub.ChangedFiles = c.hints[spec.Repo+"@"+spec.Commit].changed
	}
	c.records[spec.JobID] = rec
	c.mu.Unlock()

	a, err := c.Scheduler.Place(sub)
	switch {
	case errors.Is(err, scheduler.ErrDeferred):
		c.mu.Lock()
		c.deferred = append(c.deferred, sub)
		c.mu.Unlock()
		return nil
	case errors.Is(err, scheduler.ErrNoCapacity):
		rec.Burst = true
		if c.Burst == nil {
			return err
		}
		return c.Burst.Submit(ctx, sub)
	case err != nil:
		return err
	}
	rec.HostID = a.HostID
	rec.Mark(telemetry.HostAssigned, c.now())

	msg, err := c.buildAssign(ctx, sub, a)
	if err != nil {
		c.Scheduler.Release(spec.JobID, nil)
		return err
	}
	rec.PrebuildMatch = msg.PrebuildMatch

	c.mu.Lock()
	hc := c.hosts[a.HostID]
	c.mu.Unlock()
	if hc == nil {
		c.Scheduler.Release(spec.JobID, nil)
		return fmt.Errorf("host %s not connected", a.HostID)
	}
	hc.send(&hostlinkv1.LinkResponse{Msg: &hostlinkv1.LinkResponse_Assign{Assign: msg}})
	return nil
}

// RetryDeferred re-submits deferred jobs; call it periodically.
func (c *Core) RetryDeferred(ctx context.Context) {
	c.mu.Lock()
	pending := c.deferred
	c.deferred = nil
	c.mu.Unlock()
	for _, sub := range pending {
		if err := c.Submit(ctx, sub); err != nil {
			c.log().Warn("deferred job", "job", sub.Spec.JobID, "err", err)
		}
	}
}

func (c *Core) buildAssign(ctx context.Context, sub jobapi.Submission, a scheduler.Assignment) (*hostlinkv1.AssignJob, error) {
	spec := sub.Spec
	repo := shortRepo(spec.Repo)

	snap, cfg := c.Prebuilds.Latest(repo)
	fork := prebuild.PlanFork(spec, cfg, sub.ChangedFiles, snap)
	snapshotID := ""
	if fork.Snapshot != nil {
		ref, ok := c.hostSnapshot(a.HostID, repo)
		if ok {
			snapshotID = ref.GetId()
		} else {
			// The host lacks the prebuild. Until lazy loading is settled
			// (open question), start from the base image and pull it for
			// next time: slower, never failed.
			fork = prebuild.Fork{Match: prebuild.MatchNone, Reason: "prebuild not on assigned host"}
			c.requestPull(a.HostID, repo)
		}
	}

	runnerCfg, err := c.Secrets.Get(ctx, spec.Bootstrap.JITConfig)
	if err != nil {
		return nil, fmt.Errorf("resolve runner config: %w", err)
	}

	env := c.Mirrors.Env()
	env["TACHYON_JOB_ID"] = spec.JobID
	env["TACHYON_PREBUILD_MATCH"] = string(fork.Match)
	if cfg != nil && fork.Snapshot != nil {
		env["TACHYON_WARM_PATHS"] = strings.Join(cfg.WorkspacePaths(), ":")
	}
	if c.CacheURL != "" {
		env["TACHYON_CACHE_URL"] = c.CacheURL
	}
	files := map[string][]byte{}
	if b, err := c.Mirrors.DockerDaemonConfig(nil); err == nil && c.Mirrors.Registry != "" {
		files["/etc/docker/daemon.json"] = b
	}
	if s := c.Mirrors.AptConf(); s != "" {
		files["/etc/apt/apt.conf.d/01tachyon-proxy"] = []byte(s)
	}
	if s := c.Mirrors.GradleInit(); s != "" {
		files["~/.gradle/init.d/tachyon.gradle"] = []byte(s)
	}
	if s := c.Mirrors.MavenSettings(); s != "" {
		files["~/.m2/settings.xml"] = []byte(s)
	}
	if s := c.Mirrors.GitConfig([]string{repo}); s != "" {
		files["/etc/gitconfig.d/tachyon"] = []byte(s)
	}

	var vols []*hostlinkv1.Volume
	for _, v := range spec.Volumes {
		src := ""
		if ref, ok := c.hostSticky(a.HostID, repo, v.Key); ok {
			src = ref.GetId()
		}
		vols = append(vols, &hostlinkv1.Volume{Key: v.Key, Mount: v.Mount, SourceSnapshotId: src})
	}

	return &hostlinkv1.AssignJob{
		JobId:              spec.JobID,
		Tenant:             spec.Tenant,
		Repo:               repo,
		Commit:             spec.Commit,
		Vcpu:               uint32(spec.Hardware.VCPU),
		MemoryGb:           uint32(spec.Hardware.MemoryGB),
		BaseImage:          spec.Image,
		PrebuildSnapshotId: snapshotID,
		PrebuildMatch:      string(fork.Match),
		Volumes:            vols,
		Trust:              trustToProto(spec.Trust),
		// cgroup cpu.weight: proportional to the reserved CPU (100 per vCPU).
		CpuWeight: uint32(a.CPU*100 + 0.5),
		Guest: &hostlinkv1.GuestConfig{
			RunnerConfig: runnerCfg,
			Env:          env,
			Files:        files,
		},
	}, nil
}

func trustToProto(t jobapi.Trust) hostlinkv1.Trust {
	switch t {
	case jobapi.Trusted:
		return hostlinkv1.Trust_TRUST_TRUSTED
	case jobapi.Untrusted:
		return hostlinkv1.Trust_TRUST_UNTRUSTED
	case jobapi.UntrustedFork:
		return hostlinkv1.Trust_TRUST_UNTRUSTED_FORK
	}
	return hostlinkv1.Trust_TRUST_UNSPECIFIED
}

// shortRepo strips the host: github.com/acme/api -> acme/api.
func shortRepo(repo string) string {
	if i := strings.Index(repo, "/"); i >= 0 && strings.Contains(repo[:i], ".") {
		return repo[i+1:]
	}
	return repo
}

// Record returns a job's telemetry record.
func (c *Core) Record(jobID string) *telemetry.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.records[jobID]
}
