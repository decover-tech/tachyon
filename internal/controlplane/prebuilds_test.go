package controlplane

import (
	"context"
	"fmt"
	"io/fs"
	"sync"
	"testing"
	"time"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/jobapi"
	"github.com/decover-tech/tachyon/internal/prebuild"
)

const (
	pbSHA1 = "1111111111111111111111111111111111111111"
	pbSHA2 = "2222222222222222222222222222222222222222"
)

// repoSource serves the same files at every commit.
type repoSource struct{ files map[string]string }

func (s repoSource) DefaultHead(context.Context, string, string) (string, error) { return pbSHA1, nil }
func (s repoSource) Protected(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (s repoSource) Files(context.Context, string, string, string) ([]string, error) {
	var out []string
	for p := range s.files {
		out = append(out, p)
	}
	return out, nil
}
func (s repoSource) ReadFile(_ context.Context, _, _, _, p string) ([]byte, error) {
	if c, ok := s.files[p]; ok {
		return []byte(c), nil
	}
	return nil, fmt.Errorf("%s: %w", p, fs.ErrNotExist)
}
func (s repoSource) CloneToken(context.Context, string, string) (prebuild.CloneToken, error) {
	return prebuild.CloneToken{URL: "https://github.com/acme/api.git", Token: []byte("ghs_clone"), ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func pushHint(commit string) jobapi.Hint {
	return jobapi.Hint{Tenant: "gh_inst_1", Repo: "github.com/acme/api", Commit: commit, Ref: "refs/heads/main", Event: "push", Trust: jobapi.Trusted}
}

func TestPrebuildPushBuildRegisterRefresh(t *testing.T) {
	e := newEnv()
	clk := &clock{t: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)}
	mgr := &prebuild.Manager{
		Source: repoSource{files: map[string]string{
			prebuild.ConfigPath: "version: 1\nkey_files: [go.sum]\npaths: [node_modules]\nwarm:\n  - run: npm ci\n",
			"go.sum":            "x",
		}},
		Images:          e.core.Images,
		Registry:        e.prebuilds,
		Builder:         e.core,
		EgressAllowlist: []string{"github.com", "npm.mirror"},
		Now:             clk.now,
		NightlyHour:     23,
	}
	e.core.Prebuild = mgr
	ctx := context.Background()
	s := e.connect(t, "h1", "us", 16, 64)
	s.push(t, heartbeat(false))

	// A pull request hint never builds.
	pr := pushHint(pbSHA1)
	pr.Event = "pull_request"
	if err := e.core.Hint(ctx, pr); err != nil {
		t.Fatal(err)
	}
	s.none(t)

	if err := e.core.Hint(ctx, pushHint(pbSHA1)); err != nil {
		t.Fatal(err)
	}
	a := s.next(t).GetAssign()
	b := a.GetPrebuildBuild()
	if a == nil || b == nil {
		t.Fatalf("want a prebuild AssignJob, got %v", a)
	}
	if a.GetTrust() != hostlinkv1.Trust_TRUST_TRUSTED || a.GetCommit() != pbSHA1 || a.GetRepo() != "acme/api" || a.GetBaseImage() != "ubuntu-24.04@v1" {
		t.Errorf("assign = %v", a)
	}
	if a.GetPrebuildSnapshotId() != "" || b.GetRefresh() || len(b.GetWarmCommands()) != 1 || b.GetWarmCommands()[0] != "npm ci" {
		t.Errorf("first build should warm on an empty disk: snap=%q build=%v", a.GetPrebuildSnapshotId(), b)
	}
	if env := a.GetGuest().GetEnv(); env["TACHYON_PREBUILD_COMMIT"] != pbSHA1 || env["TACHYON_WORKSPACE"] != "/home/runner/actions-runner/_work/api/api" {
		t.Errorf("prebuild env commit=%q workspace=%q", env["TACHYON_PREBUILD_COMMIT"], env["TACHYON_WORKSPACE"])
	}
	if len(a.GetGuest().GetRunnerConfig()) != 0 {
		t.Error("prebuild VM got a runner config")
	}
	if string(b.GetCloneToken()) != "ghs_clone" || len(b.GetCloneCommands()) == 0 {
		t.Errorf("clone = %v", b.GetCloneCommands())
	}
	for k, v := range a.GetGuest().GetEnv() {
		if v == "ghs_clone" {
			t.Errorf("clone token in guest env %s", k)
		}
	}
	if len(b.GetScrubPaths()) != len(prebuild.CredentialFiles) || len(b.GetEgressAllowlist()) != 2 || b.GetKeepPaths()[0] != "node_modules" {
		t.Errorf("build = %v", b)
	}
	key := b.GetKey()

	s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobResult{JobResult: &hostlinkv1.JobResult{
		JobId:     a.GetJobId(),
		Outcome:   hostlinkv1.JobResult_OUTCOME_SUCCEEDED,
		Prebuild:  &hostlinkv1.SnapshotRef{Kind: hostlinkv1.SnapshotRef_KIND_PREBUILD, Repo: "acme/api", Key: key, Id: "jobs/x@" + key},
		BaseImage: a.GetBaseImage(),
		Commit:    a.GetCommit(),
	}}})
	snap, cfg := e.prebuilds.Latest("acme/api")
	if snap == nil || snap.Key != key || snap.Commit != pbSHA1 || cfg == nil || cfg.Warm[0].Run != "npm ci" {
		t.Fatalf("registered %+v cfg %+v", snap, cfg)
	}
	if mgr.Inflight("acme/api") != "" {
		t.Error("manager still has the build in flight")
	}
	s.push(t, heartbeat(false, &hostlinkv1.SnapshotRef{Kind: hostlinkv1.SnapshotRef_KIND_PREBUILD, Repo: "acme/api", Key: key, Id: "jobs/x@" + key}))

	// Inside the interval the push waits; after it, Tick builds a refresh
	// that forks the previous prebuild and runs no warm steps.
	e.core.Hint(ctx, pushHint(pbSHA2))
	if s.next(t).GetPreFork() == nil { // the hint's usual pre-fork, no build
		t.Fatal("expected only a PreFork")
	}
	s.none(t)
	clk.add(11 * time.Minute)
	mgr.Tick(ctx)
	a = s.next(t).GetAssign()
	b = a.GetPrebuildBuild()
	if !b.GetRefresh() || a.GetPrebuildSnapshotId() != "jobs/x@"+key || len(b.GetWarmCommands()) != 0 || b.GetKey() != key || a.GetCommit() != pbSHA2 {
		t.Fatalf("refresh assign = %v", a)
	}

	// A failed build registers nothing and frees the repo.
	s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobResult{JobResult: &hostlinkv1.JobResult{
		JobId: a.GetJobId(), Outcome: hostlinkv1.JobResult_OUTCOME_INFRA_ERROR, Error: "prebuild secret scan failed",
	}}})
	if snap, _ := e.prebuilds.Latest("acme/api"); snap.Commit != pbSHA1 {
		t.Errorf("failed build changed the prebuild: %+v", snap)
	}
	if mgr.Inflight("acme/api") != "" {
		t.Error("failed build still in flight")
	}
}

func TestBuildPrebuildFallsBackToFullWhenHostLacksSnapshot(t *testing.T) {
	e := newEnv()
	s := e.connect(t, "h1", "us", 16, 64)
	s.push(t, heartbeat(false))
	req := prebuild.BuildRequest{
		JobID: "pb_1", Tenant: "gh_inst_1", Repo: "acme/api", Commit: pbSHA1,
		BaseImage: "ubuntu-24.04@v1", Key: "pb1-k", Hardware: jobapi.NewHardware(jobapi.ArchX86_64, 4),
		Refresh: true, From: &prebuild.Snapshot{Repo: "acme/api", Key: "pb1-k"},
		WarmCommands: []string{"make deps"},
	}
	if err := e.core.BuildPrebuild(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	a := s.next(t).GetAssign()
	if a.GetPrebuildBuild().GetRefresh() || a.GetPrebuildSnapshotId() != "" || len(a.GetPrebuildBuild().GetWarmCommands()) != 1 {
		t.Errorf("want full build, got %v", a)
	}

	bad := req
	bad.JobID, bad.Commit = "pb_2", "main"
	if err := e.core.BuildPrebuild(context.Background(), bad); err == nil {
		t.Error("expected error for a non-SHA commit")
	}
}
