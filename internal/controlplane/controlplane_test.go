package controlplane

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/jobapi"
	"github.com/decover-tech/tachyon/internal/prebuild"
	"github.com/decover-tech/tachyon/internal/scheduler"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

// fakeStream is an in-process hostd stream. Recv signals on waiting each
// time Link asks for the next message, which tells the test the previous
// message has been fully handled.
type fakeStream struct {
	grpc.ServerStream
	ctx     context.Context
	in      chan *hostlinkv1.LinkRequest
	out     chan *hostlinkv1.LinkResponse
	waiting chan struct{}
}

func (s *fakeStream) Context() context.Context { return s.ctx }

func (s *fakeStream) Recv() (*hostlinkv1.LinkRequest, error) {
	select {
	case s.waiting <- struct{}{}:
	case <-s.ctx.Done():
		return nil, io.EOF
	}
	select {
	case m := <-s.in:
		return m, nil
	case <-s.ctx.Done():
		return nil, io.EOF
	}
}

func (s *fakeStream) Send(m *hostlinkv1.LinkResponse) error {
	s.out <- m
	return nil
}

// push delivers m and waits until Link has processed it.
func (s *fakeStream) push(t *testing.T, m *hostlinkv1.LinkRequest) {
	t.Helper()
	s.in <- m
	select {
	case <-s.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("Link did not process message")
	}
}

func (s *fakeStream) next(t *testing.T) *hostlinkv1.LinkResponse {
	t.Helper()
	select {
	case m := <-s.out:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no message sent to host")
		return nil
	}
}

func (s *fakeStream) none(t *testing.T) {
	t.Helper()
	select {
	case m := <-s.out:
		t.Fatalf("unexpected message to host: %v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

type env struct {
	core      *Core
	link      *HostLink
	prebuilds *MemPrebuilds
	secrets   *MemSecrets
}

func newEnv() *env {
	pb := &MemPrebuilds{}
	sec := &MemSecrets{}
	core := &Core{
		Scheduler: scheduler.New(nil),
		Images:    StaticImages{"ubuntu-24.04": "v1"},
		Prebuilds: pb,
		Secrets:   sec,
	}
	return &env{core: core, link: &HostLink{Core: core}, prebuilds: pb, secrets: sec}
}

// connect opens a host stream and sends Hello.
func (e *env) connect(t *testing.T, id, region string, vcpu, mem uint32) *fakeStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &fakeStream{
		ctx:     ctx,
		in:      make(chan *hostlinkv1.LinkRequest),
		out:     make(chan *hostlinkv1.LinkResponse, 16),
		waiting: make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() { done <- e.link.Link(s) }()
	<-s.waiting // first Recv
	s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_Hello{Hello: &hostlinkv1.Hello{
		HostId: id, Region: region, FailureDomain: id, Arch: "x86_64", Vcpu: vcpu, MemoryGb: mem,
	}}})
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return s
}

func heartbeat(draining bool, refs ...*hostlinkv1.SnapshotRef) *hostlinkv1.LinkRequest {
	return &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_Heartbeat{Heartbeat: &hostlinkv1.Heartbeat{
		Snapshots: refs, Draining: draining,
	}}}
}

func prebuildRef(repo, id string) *hostlinkv1.SnapshotRef {
	return &hostlinkv1.SnapshotRef{Kind: hostlinkv1.SnapshotRef_KIND_PREBUILD, Repo: repo, Key: "pb1-x", Id: id}
}

func (e *env) submission(t *testing.T, id string, vcpu int) jobapi.Submission {
	t.Helper()
	ref, err := e.secrets.Put(context.Background(), "jit/"+id, []byte("JIT-"+id))
	if err != nil {
		t.Fatal(err)
	}
	return jobapi.Submission{
		Spec: jobapi.Spec{
			JobID:     id,
			Tenant:    "gh_inst_1",
			Repo:      "github.com/acme/api",
			Commit:    "abc",
			Hardware:  jobapi.NewHardware(jobapi.ArchX86_64, vcpu),
			Image:     "ubuntu-24.04",
			Prebuild:  jobapi.PrebuildRef{Repo: "acme/api", Policy: jobapi.PrebuildLatestTrusted},
			Volumes:   []jobapi.Volume{jobapi.DockerVolume},
			Bootstrap: jobapi.Bootstrap{Kind: jobapi.BootstrapGitHubRunner, JITConfig: ref},
			Trust:     jobapi.Trusted,
		},
		Profile:  "acme/api/CI/test",
		QueuedAt: time.Now(),
	}
}

func (e *env) registerPrebuild() {
	e.prebuilds.SetConfig("acme/api", &prebuild.Config{Version: 1, KeyFiles: []string{"go.sum"}, Paths: []string{"node_modules"}})
	e.prebuilds.Register(prebuild.Snapshot{Repo: "acme/api", Key: "pb1-x", BaseImage: "ubuntu-24.04@v1", Healthy: true})
}

func TestSubmitForksPrebuildOnHost(t *testing.T) {
	e := newEnv()
	e.registerPrebuild()
	s := e.connect(t, "h1", "us", 16, 64)
	s.push(t, heartbeat(false, prebuildRef("acme/api", "prebuilds/acme_api@pb1-x")))

	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 4)); err != nil {
		t.Fatal(err)
	}
	a := s.next(t).GetAssign()
	if a == nil {
		t.Fatal("expected AssignJob")
	}
	if a.GetPrebuildSnapshotId() != "prebuilds/acme_api@pb1-x" || a.GetPrebuildMatch() != "full" {
		t.Errorf("fork = %q %q", a.GetPrebuildSnapshotId(), a.GetPrebuildMatch())
	}
	if a.GetRepo() != "acme/api" || a.GetBaseImage() != "ubuntu-24.04@v1" || a.GetVcpu() != 4 || a.GetMemoryGb() != 16 {
		t.Errorf("assign = %v", a)
	}
	if a.GetTrust() != hostlinkv1.Trust_TRUST_TRUSTED {
		t.Errorf("trust = %v", a.GetTrust())
	}
	if string(a.GetGuest().GetRunnerConfig()) != "JIT-j1" {
		t.Errorf("runner config = %q", a.GetGuest().GetRunnerConfig())
	}
	envs := a.GetGuest().GetEnv()
	if envs["TACHYON_PREBUILD_MATCH"] != "full" || envs["TACHYON_WARM_PATHS"] != "node_modules" || envs["TACHYON_JOB_ID"] != "j1" {
		t.Errorf("guest env = %v", envs)
	}
	if len(a.GetVolumes()) != 1 || a.GetVolumes()[0].GetMount() != "/var/lib/docker" {
		t.Errorf("volumes = %v", a.GetVolumes())
	}
	rec := e.core.Record("j1")
	if rec == nil || rec.HostID != "h1" || rec.PrebuildMatch != "full" {
		t.Fatalf("record = %+v", rec)
	}
	if _, ok := rec.At(telemetry.HostAssigned); !ok {
		t.Error("host_assigned not marked")
	}

	// A job event from the host lands in the record.
	at := time.Date(2026, 11, 21, 0, 0, 1, 0, time.UTC)
	s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobEvent{JobEvent: &hostlinkv1.JobEvent{
		JobId: "j1", Phase: hostlinkv1.Phase_PHASE_RUNNER_ONLINE, At: timestamppb.New(at),
	}}})
	if got, ok := rec.At(telemetry.RunnerOnline); !ok || !got.Equal(at) {
		t.Errorf("runner_online = %v %v", got, ok)
	}
}

func TestSubmitPartialWhenKeyFileChanged(t *testing.T) {
	e := newEnv()
	e.registerPrebuild()
	s := e.connect(t, "h1", "us", 16, 64)
	s.push(t, heartbeat(false, prebuildRef("acme/api", "snap")))
	// Changed files arrive via the push hint, before the job.
	if err := e.core.Hint(context.Background(), jobapi.Hint{Repo: "github.com/acme/api", Commit: "abc", ChangedFiles: []string{"go.sum"}}); err != nil {
		t.Fatal(err)
	}
	if pf := s.next(t).GetPreFork(); pf == nil || pf.GetPrebuildSnapshotId() != "snap" || pf.GetRepo() != "acme/api" {
		t.Fatalf("expected PreFork, got %v", pf)
	}
	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 2)); err != nil {
		t.Fatal(err)
	}
	if a := s.next(t).GetAssign(); a.GetPrebuildMatch() != "partial" || a.GetPrebuildSnapshotId() != "snap" {
		t.Errorf("assign = %v", a)
	}
}

func TestSubmitFallsBackWhenHostLacksPrebuild(t *testing.T) {
	e := newEnv()
	e.registerPrebuild()
	h1 := e.connect(t, "h1", "us", 16, 64)
	h1.push(t, heartbeat(false))
	// h2 holds the prebuild but is draining, so the job lands on h1.
	h2 := e.connect(t, "h2", "us", 16, 64)
	h2.push(t, heartbeat(true, prebuildRef("acme/api", "h2-snap")))

	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 4)); err != nil {
		t.Fatal(err)
	}
	// h1 is asked to pull the prebuild from its regional peer, then runs the
	// job from the base image.
	pull := h1.next(t).GetPullSnapshot()
	if pull == nil || pull.GetFromHostId() != "h2" || pull.GetSnapshot().GetId() != "h2-snap" {
		t.Fatalf("expected PullSnapshot from h2, got %v", pull)
	}
	a := h1.next(t).GetAssign()
	if a == nil || a.GetPrebuildSnapshotId() != "" || a.GetPrebuildMatch() != "none" {
		t.Fatalf("assign = %v", a)
	}
	if _, ok := a.GetGuest().GetEnv()["TACHYON_WARM_PATHS"]; ok {
		t.Error("warm paths set for a base-image fork")
	}
	h2.none(t)
}

func TestNoPrebuildMatchNoneWithoutPull(t *testing.T) {
	e := newEnv() // no prebuild registered
	s := e.connect(t, "h1", "us", 16, 64)
	s.push(t, heartbeat(false))
	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 4)); err != nil {
		t.Fatal(err)
	}
	if a := s.next(t).GetAssign(); a == nil || a.GetPrebuildMatch() != "none" {
		t.Fatalf("assign = %v", a)
	}
	s.none(t)
}

func TestJobResultReleasesAndRegistersPrebuild(t *testing.T) {
	e := newEnv()
	s := e.connect(t, "h1", "us", 4, 16)
	s.push(t, heartbeat(false))

	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 4)); err != nil {
		t.Fatal(err)
	}
	s.next(t)
	if err := e.core.Submit(context.Background(), e.submission(t, "j2", 4)); !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("want ErrNoCapacity with no burst, got %v", err)
	}
	if rec := e.core.Record("j2"); rec == nil || !rec.Burst {
		t.Error("j2 record should be marked burst")
	}

	s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobResult{JobResult: &hostlinkv1.JobResult{
		JobId:       "j1",
		Outcome:     hostlinkv1.JobResult_OUTCOME_SUCCEEDED,
		AvgBusyVcpu: 1.5, PeakMemoryGb: 3,
		Prebuild:  &hostlinkv1.SnapshotRef{Kind: hostlinkv1.SnapshotRef_KIND_PREBUILD, Repo: "acme/api", Key: "pb1-new", Id: "x"},
		BaseImage: "ubuntu-24.04@v1",
		Commit:    "abc",
	}}})

	if err := e.core.Submit(context.Background(), e.submission(t, "j3", 4)); err != nil {
		t.Fatalf("capacity not released: %v", err)
	}
	snap, _ := e.prebuilds.Latest("acme/api")
	if snap == nil || snap.Key != "pb1-new" || snap.BaseImage != "ubuntu-24.04@v1" || snap.Commit != "abc" || !snap.Healthy {
		t.Fatalf("prebuild = %+v", snap)
	}
}

type fakeBurst struct{ subs []jobapi.Submission }

func (b *fakeBurst) Submit(_ context.Context, s jobapi.Submission) error {
	b.subs = append(b.subs, s)
	return nil
}

func TestSubmitSpillsToBurstAndRejectsUnknownImage(t *testing.T) {
	e := newEnv()
	burst := &fakeBurst{}
	e.core.Burst = burst
	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 4)); err != nil {
		t.Fatal(err)
	}
	if len(burst.subs) != 1 || burst.subs[0].Spec.Image != "ubuntu-24.04@v1" {
		t.Fatalf("burst = %+v", burst.subs)
	}
	bad := e.submission(t, "j2", 4)
	bad.Spec.Image = "windows-2025"
	if err := e.core.Submit(context.Background(), bad); err == nil {
		t.Fatal("unknown image accepted")
	}
}

func TestStaticImagesPin(t *testing.T) {
	im := StaticImages{"ubuntu-24.04": "2026-11-16"}
	if got, err := im.Pin("ubuntu-24.04"); err != nil || got != "ubuntu-24.04@2026-11-16" {
		t.Errorf("Pin = %q %v", got, err)
	}
	if got, err := im.Pin("ubuntu-24.04@old"); err != nil || got != "ubuntu-24.04@old" {
		t.Errorf("already pinned: %q %v", got, err)
	}
	if _, err := im.Pin("nope"); err == nil {
		t.Error("unknown image should fail")
	}
}

func TestLinkRequiresHello(t *testing.T) {
	e := newEnv()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &fakeStream{ctx: ctx, in: make(chan *hostlinkv1.LinkRequest, 1), out: make(chan *hostlinkv1.LinkResponse, 1), waiting: make(chan struct{}, 1)}
	s.in <- heartbeat(false)
	if err := e.link.Link(s); err == nil {
		t.Fatal("stream without Hello accepted")
	}
}

func TestShortRepo(t *testing.T) {
	for in, want := range map[string]string{
		"github.com/acme/api": "acme/api",
		"acme/api":            "acme/api",
	} {
		if got := shortRepo(in); got != want {
			t.Errorf("shortRepo(%q) = %q", in, got)
		}
	}
}
