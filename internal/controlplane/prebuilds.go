package controlplane

import (
	"context"
	"fmt"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/jobapi"
	"github.com/decover-tech/tachyon/internal/prebuild"
)

// PrebuildManager decides when prebuilds are built (prebuild.Manager). Core
// forwards push hints to it and reports the result of each build it started
// through BuildPrebuild.
type PrebuildManager interface {
	OnPush(ctx context.Context, h jobapi.Hint) error
	Finished(repo, jobID string, ok bool)
}

var _ PrebuildManager = (*prebuild.Manager)(nil)
var _ prebuild.Builder = (*Core)(nil)

func (c *Core) prebuildHint(ctx context.Context, h jobapi.Hint) {
	if c.Prebuild == nil {
		return
	}
	if err := c.Prebuild.OnPush(ctx, h); err != nil {
		c.log().Warn("prebuild trigger", "repo", h.Repo, "commit", h.Commit, "err", err)
	}
}

// BuildPrebuild places a prebuild build and sends it to the chosen host. The
// VM gets no runner config and no repo secrets: only mirror settings, and
// the clone token inside PrebuildBuild for the clone commands. A refresh
// forks the previous prebuild when the host holds it under the same key;
// otherwise the build runs the full warm-up on an empty disk.
func (c *Core) BuildPrebuild(ctx context.Context, req prebuild.BuildRequest) error {
	spec := jobapi.Spec{
		JobID:    req.JobID,
		Tenant:   req.Tenant,
		Repo:     req.Repo,
		Commit:   req.Commit,
		Hardware: req.Hardware,
		Image:    req.BaseImage,
		Prebuild: jobapi.PrebuildRef{Repo: req.Repo, Policy: jobapi.PrebuildLatestTrusted},
		Trust:    jobapi.Trusted,
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	if !prebuild.ValidCommit(req.Commit) || req.Key == "" {
		return fmt.Errorf("prebuild %s: commit and key required", req.JobID)
	}
	a, err := c.Scheduler.Place(jobapi.Submission{Spec: spec, Profile: req.Repo + "/tachyon-prebuild", QueuedAt: c.now()})
	if err != nil {
		return fmt.Errorf("place prebuild: %w", err)
	}

	build := &hostlinkv1.PrebuildBuild{
		Key:             req.Key,
		KeepPaths:       req.KeepPaths,
		ScrubPaths:      req.ScrubPaths,
		EgressAllowlist: req.EgressAllowlist,
		CloneCommands:   req.CloneCommands,
		CloneToken:      req.CloneToken,
	}
	snapshotID, match := "", string(prebuild.MatchNone)
	if ref, ok := c.hostSnapshot(a.HostID, req.Repo); ok && req.Refresh && ref.GetKey() == req.Key {
		snapshotID, match = ref.GetId(), string(prebuild.MatchFull)
		build.Refresh = true
	} else {
		build.WarmCommands = req.WarmCommands
	}

	env := c.Mirrors.Env()
	env["TACHYON_JOB_ID"] = req.JobID
	env["TACHYON_PREBUILD_KEY"] = req.Key
	msg := &hostlinkv1.AssignJob{
		JobId:              req.JobID,
		Tenant:             req.Tenant,
		Repo:               req.Repo,
		Commit:             req.Commit,
		Vcpu:               uint32(req.Hardware.VCPU),
		MemoryGb:           uint32(req.Hardware.MemoryGB),
		BaseImage:          req.BaseImage,
		PrebuildSnapshotId: snapshotID,
		PrebuildMatch:      match,
		Trust:              hostlinkv1.Trust_TRUST_TRUSTED,
		CpuWeight:          uint32(a.CPU*100 + 0.5),
		Guest:              &hostlinkv1.GuestConfig{Env: env},
		PrebuildBuild:      build,
	}

	c.mu.Lock()
	hc := c.hosts[a.HostID]
	if hc != nil {
		if c.pbJobs == nil {
			c.pbJobs = map[string]string{}
		}
		c.pbJobs[req.JobID] = req.Repo
	}
	c.mu.Unlock()
	if hc == nil {
		c.Scheduler.Release(req.JobID, nil)
		return fmt.Errorf("host %s not connected", a.HostID)
	}
	hc.send(&hostlinkv1.LinkResponse{Msg: &hostlinkv1.LinkResponse_Assign{Assign: msg}})
	c.log().Info("prebuild dispatched", "job", req.JobID, "repo", req.Repo, "host", a.HostID, "refresh", build.Refresh)
	return nil
}

// prebuildResult tells the prebuild manager how a build it started ended.
// A build succeeded only if the host snapshotted it, which hostd does only
// after the guest's credential scrub and a clean secret scan.
func (c *Core) prebuildResult(r *hostlinkv1.JobResult) {
	c.mu.Lock()
	repo, ok := c.pbJobs[r.GetJobId()]
	delete(c.pbJobs, r.GetJobId())
	c.mu.Unlock()
	if !ok || c.Prebuild == nil {
		return
	}
	pb := r.GetPrebuild()
	success := r.GetOutcome() == hostlinkv1.JobResult_OUTCOME_SUCCEEDED && pb.GetKey() != "" && pb.GetRepo() == repo
	c.Prebuild.Finished(repo, r.GetJobId(), success)
}
