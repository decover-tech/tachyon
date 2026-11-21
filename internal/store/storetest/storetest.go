// Package storetest is a conformance suite every store.Store must pass.
package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
	"github.com/decover-tech/tachyon/internal/prebuild"
	"github.com/decover-tech/tachyon/internal/scheduler"
	"github.com/decover-tech/tachyon/internal/store"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

// t0 is microsecond-aligned so it survives a Postgres timestamptz.
var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// Submission returns a representative job submission.
func Submission(id string) jobapi.Submission {
	return jobapi.Submission{
		Spec: jobapi.Spec{
			JobID:     id,
			Tenant:    "gh_inst_1",
			Repo:      "github.com/acme/api",
			Commit:    "abc",
			Hardware:  jobapi.NewHardware(jobapi.ArchX86_64, 4),
			Image:     "ubuntu-24.04@v1",
			Prebuild:  jobapi.PrebuildRef{Repo: "acme/api", Policy: jobapi.PrebuildLatestTrusted},
			Volumes:   []jobapi.Volume{jobapi.DockerVolume},
			Bootstrap: jobapi.Bootstrap{Kind: jobapi.BootstrapGitHubRunner, JITConfig: "secret://jit/x#1"},
			Trust:     jobapi.Trusted,
		},
		Profile:      "acme/api/CI/test",
		ChangedFiles: []string{"go.sum"},
		QueuedAt:     t0,
	}
}

// Run runs the suite. newStore must return an empty store each call.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("Jobs", func(t *testing.T) { testJobs(t, newStore(t)) })
	t.Run("Hosts", func(t *testing.T) { testHosts(t, newStore(t)) })
	t.Run("Prebuilds", func(t *testing.T) { testPrebuilds(t, newStore(t)) })
	t.Run("Records", func(t *testing.T) { testRecords(t, newStore(t)) })
	t.Run("Runners", func(t *testing.T) { testRunners(t, newStore(t)) })
}

func testJobs(t *testing.T, s store.Store) {
	ctx := context.Background()
	q := store.Job{ID: "j1", State: store.JobQueued, Submission: Submission("j1"), UpdatedAt: t0}
	if err := s.PutJob(ctx, q); err != nil {
		t.Fatal(err)
	}
	a := store.Job{ID: "j2", State: store.JobAssigned, Submission: Submission("j2"), HostID: "h1", CPU: 2.5, MemGB: 9, UpdatedAt: t0}
	if err := s.PutJob(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := store.Job{ID: "j3", State: store.JobBurst, Submission: Submission("j3"), UpdatedAt: t0}
	if err := s.PutJob(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := s.OpenJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !reflect.DeepEqual(got[0], q) || !reflect.DeepEqual(got[1], a) {
		t.Fatalf("open jobs = %+v", got)
	}
	// Upsert moves a job between states.
	q.State = store.JobDeferred
	if err := s.PutJob(ctx, q); err != nil {
		t.Fatal(err)
	}
	if err := s.SetJobState(ctx, "j2", store.JobFinished); err != nil {
		t.Fatal(err)
	}
	if err := s.SetJobState(ctx, "missing", store.JobFinished); err != nil {
		t.Fatalf("unknown job: %v", err)
	}
	got, _ = s.OpenJobs(ctx)
	if len(got) != 1 || !reflect.DeepEqual(got[0], q) {
		t.Fatalf("after finish = %+v", got)
	}
	if err := s.SetJobState(ctx, "j2", store.JobAssigned); err != nil {
		t.Fatal(err)
	}
	got, _ = s.OpenJobs(ctx)
	if len(got) != 2 || !reflect.DeepEqual(got[1], a) {
		t.Fatalf("SetJobState lost job data: %+v", got)
	}
}

func testHosts(t *testing.T, s store.Store) {
	ctx := context.Background()
	h := store.Host{
		Host: scheduler.Host{
			ID: "h1", Region: "us", FailureDomain: "r1", Arch: jobapi.ArchX86_64, VCPU: 16, MemGB: 64,
			Snapshots: map[string]bool{"acme/api": true},
		},
		Ratio: 1.3, LastSeen: t0,
	}
	if err := s.PutHost(ctx, h); err != nil {
		t.Fatal(err)
	}
	h.Ratio = 1.4
	if err := s.PutHost(ctx, h); err != nil {
		t.Fatal(err)
	}
	got, err := s.Hosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0], h) {
		t.Fatalf("hosts = %+v", got)
	}
}

func testPrebuilds(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.LatestPrebuild(ctx, "acme/api"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty registry: %v", err)
	}
	if _, err := s.PrebuildConfig(ctx, "acme/api"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty config: %v", err)
	}
	old := prebuild.Snapshot{Repo: "acme/api", Key: "pb1-a", Commit: "c1", BaseImage: "ubuntu-24.04@v1", CreatedAt: t0, Healthy: true}
	newer := prebuild.Snapshot{Repo: "acme/api", Key: "pb1-b", Commit: "c2", BaseImage: "ubuntu-24.04@v1", CreatedAt: t0.Add(time.Hour), Healthy: true}
	broken := prebuild.Snapshot{Repo: "acme/api", Key: "pb1-c", Commit: "c3", BaseImage: "ubuntu-24.04@v1", CreatedAt: t0.Add(2 * time.Hour)}
	other := prebuild.Snapshot{Repo: "acme/web", Key: "pb1-d", Commit: "c4", CreatedAt: t0.Add(3 * time.Hour), Healthy: true}
	for _, p := range []prebuild.Snapshot{old, newer, broken, other} {
		if err := s.PutPrebuild(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LatestPrebuild(ctx, "acme/api")
	if err != nil || !reflect.DeepEqual(got, newer) {
		t.Fatalf("latest = %+v %v; want the newest healthy one", got, err)
	}
	// Marking the newest unhealthy falls back to the older one.
	newer.Healthy = false
	if err := s.PutPrebuild(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LatestPrebuild(ctx, "acme/api"); got.Key != "pb1-a" {
		t.Fatalf("after unhealthy = %+v", got)
	}

	cfg := &prebuild.Config{Version: 1, KeyFiles: []string{"go.sum"}, Paths: []string{"node_modules"}, Warm: []prebuild.Step{{Run: "make deps"}}}
	if err := s.PutPrebuildConfig(ctx, "acme/api", cfg); err != nil {
		t.Fatal(err)
	}
	gc, err := s.PrebuildConfig(ctx, "acme/api")
	if err != nil || !reflect.DeepEqual(gc, cfg) {
		t.Fatalf("config = %+v %v", gc, err)
	}
}

func testRecords(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.Record(ctx, "j1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing record: %v", err)
	}
	r := telemetry.NewRecord("j1")
	r.Tenant, r.Repo, r.HostID, r.VCPU, r.PrebuildMatch = "t", "github.com/acme/api", "h1", 4, "full"
	r.Mark(telemetry.WebhookReceived, t0)
	if err := s.PutRecord(ctx, r.Data()); err != nil {
		t.Fatal(err)
	}
	r.Mark(telemetry.RunnerOnline, t0.Add(1500*time.Millisecond))
	if err := s.PutRecord(ctx, r.Data()); err != nil {
		t.Fatal(err)
	}
	got, err := s.Record(ctx, "j1")
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := telemetry.FromData(got).Pickup(); !ok || d != 1500*time.Millisecond || got.HostID != "h1" {
		t.Fatalf("record = %+v", got)
	}
	list, err := s.RecordsFor(ctx, []string{"j1", "nope"})
	if err != nil || len(list) != 1 || list[0].JobID != "j1" {
		t.Fatalf("RecordsFor = %+v %v", list, err)
	}
}

func testRunners(t *testing.T, s store.Store) {
	ctx := context.Background()
	p := store.PendingRunner{
		Frontend: "github", ExternalID: 42, InstallationID: 7, Repo: "acme/api",
		Labels: []string{"tachyon-4cpu"}, Submission: Submission("gh_42"),
		RunnerID: 9, RunnerName: "tachyon-gh_42-1", SecretRef: "secret://jit/x#1",
		SubmittedAt: t0, Attempts: 1,
	}
	if err := s.PutPendingRunner(ctx, p); err != nil {
		t.Fatal(err)
	}
	p.Attempts = 2
	if err := s.PutPendingRunner(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPendingRunner(ctx, store.PendingRunner{Frontend: "gitlab", ExternalID: 42}); err != nil {
		t.Fatal(err)
	}
	got, err := s.PendingRunners(ctx, "github")
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], p) {
		t.Fatalf("pending = %+v %v", got, err)
	}
	if err := s.DeletePendingRunner(ctx, "github", 42); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.PendingRunners(ctx, "github"); len(got) != 0 {
		t.Fatalf("after delete = %+v", got)
	}
	if got, _ := s.PendingRunners(ctx, "gitlab"); len(got) != 1 {
		t.Fatal("delete crossed frontends")
	}

	ar := store.ActiveRunner{Frontend: "github", Name: "tachyon-gh_42-1", InstallationID: 7, Repo: "acme/api"}
	if err := s.PutActiveRunner(ctx, ar); err != nil {
		t.Fatal(err)
	}
	if err := s.PutActiveRunner(ctx, ar); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ActiveRunners(ctx, "github"); err != nil || len(got) != 1 || got[0] != ar {
		t.Fatalf("active = %+v %v", got, err)
	}
	if err := s.DeleteActiveRunner(ctx, "github", ar.Name); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ActiveRunners(ctx, "github"); len(got) != 0 {
		t.Fatalf("after delete = %+v", got)
	}
}
