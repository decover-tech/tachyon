package controlplane

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/prebuild"
	"github.com/decover-tech/tachyon/internal/scheduler"
	"github.com/decover-tech/tachyon/internal/store"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

// countingStore counts host writes.
type countingStore struct {
	store.Store
	hostPuts atomic.Int32
}

func (s *countingStore) PutHost(ctx context.Context, h store.Host) error {
	s.hostPuts.Add(1)
	return s.Store.PutHost(ctx, h)
}

// failingStore refuses job writes.
type failingStore struct{ store.Store }

func (failingStore) PutJob(context.Context, store.Job) error { return errors.New("db down") }

// restarted builds a fresh core (new scheduler, no in-memory state) on the
// same store and secrets, as after a control plane restart.
func (e *env) restarted(st store.Store) *env {
	core := &Core{
		Scheduler: scheduler.New(nil),
		Images:    e.core.Images,
		Prebuilds: e.prebuilds,
		Secrets:   e.secrets,
		Store:     st,
	}
	return &env{core: core, link: &HostLink{Core: core}, prebuilds: e.prebuilds, secrets: e.secrets}
}

func jobStates(t *testing.T, st store.Store) map[string]store.JobState {
	t.Helper()
	jobs, err := st.OpenJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.JobState{}
	for _, j := range jobs {
		out[j.ID] = j.State
	}
	return out
}

func TestRestartKeepsQueuedAndRunningJobs(t *testing.T) {
	ctx := context.Background()
	st := &store.Mem{}
	e := newEnv()
	e.core.Store = st
	h1 := e.connect(t, "h1", "us", 4, 16)
	h1.push(t, heartbeat(false))

	// j1 runs on h1 and fills it; j2 is deferred until the fleet is quiet;
	// j3 was recorded but the control plane died before placing it.
	if err := e.core.Submit(ctx, e.submission(t, "j1", 4)); err != nil {
		t.Fatal(err)
	}
	if h1.next(t).GetAssign().GetJobId() != "j1" {
		t.Fatal("j1 not assigned")
	}
	j2 := e.submission(t, "j2", 2)
	j2.Deferrable = true
	if err := e.core.Submit(ctx, j2); err != nil {
		t.Fatal(err)
	}
	j3 := e.submission(t, "j3", 2)
	j3.Spec.Image = "ubuntu-24.04@v1"
	if err := st.PutJob(ctx, store.Job{ID: "j3", State: store.JobQueued, Submission: j3}); err != nil {
		t.Fatal(err)
	}
	h1.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobEvent{JobEvent: &hostlinkv1.JobEvent{
		JobId: "j1", Phase: hostlinkv1.Phase_PHASE_VM_RESUMED,
	}}})
	want := map[string]store.JobState{"j1": store.JobAssigned, "j2": store.JobDeferred, "j3": store.JobQueued}
	if got := jobStates(t, st); len(got) != 3 || got["j1"] != want["j1"] || got["j2"] != want["j2"] || got["j3"] != want["j3"] {
		t.Fatalf("persisted = %v", got)
	}

	// Restart.
	e2 := e.restarted(st)
	stats, err := e2.core.Restore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (RestoreStats{Hosts: 1, Running: 1, Requeued: 2}) {
		t.Fatalf("stats = %+v", stats)
	}
	rec := e2.core.Record("j1")
	if rec == nil || rec.HostID != "h1" {
		t.Fatalf("j1 record not restored: %+v", rec)
	}
	if _, ok := rec.At(telemetry.VMResumed); !ok {
		t.Error("j1 phases not restored")
	}
	// Nothing is placed on h1 before it reconnects.
	if err := e2.core.Submit(ctx, e2.submission(t, "j4", 2)); !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("placed on a host with no stream: %v", err)
	}
	if got := jobStates(t, st)["j4"]; got != "" {
		t.Fatalf("refused job j4 left open as %q", got)
	}

	// h1 reconnects with j1 still running: its reservation holds.
	h1b := e2.connect(t, "h1", "us", 4, 16)
	h1b.push(t, heartbeat(false))
	if _, err := e2.core.Scheduler.Place(e2.submission(t, "probe", 2)); !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("restored reservation lost on reconnect: %v", err)
	}

	// j1 finishes; its capacity frees and the requeued jobs run.
	h1b.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobResult{JobResult: &hostlinkv1.JobResult{
		JobId: "j1", Outcome: hostlinkv1.JobResult_OUTCOME_SUCCEEDED, AvgBusyVcpu: 3, PeakMemoryGb: 8,
	}}})
	if _, ok := jobStates(t, st)["j1"]; ok {
		t.Fatal("j1 still open after its result")
	}
	if got, err := st.Record(ctx, "j1"); err != nil || got.HostID != "h1" {
		t.Fatalf("j1 record = %+v %v", got, err)
	}
	e2.core.RetryDeferred(ctx)
	assigned := map[string]bool{}
	for range 2 {
		assigned[h1b.next(t).GetAssign().GetJobId()] = true
	}
	if !assigned["j2"] || !assigned["j3"] {
		t.Fatalf("assigned after restart = %v", assigned)
	}
	got := jobStates(t, st)
	if got["j2"] != store.JobAssigned || got["j3"] != store.JobAssigned {
		t.Fatalf("persisted after requeue = %v", got)
	}
}

func TestSubmitRefusedWhenStoreFails(t *testing.T) {
	e := newEnv()
	e.core.Store = failingStore{&store.Mem{}}
	s := e.connect(t, "h1", "us", 16, 64)
	s.push(t, heartbeat(false))
	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 2)); err == nil {
		t.Fatal("job accepted without being recorded")
	}
	s.none(t)
	if _, err := e.core.Scheduler.Place(e.submission(t, "j1", 2)); err != nil {
		t.Fatalf("refused job kept a reservation: %v", err)
	}
}

func TestHostWritesOnlyOnChange(t *testing.T) {
	e := newEnv()
	st := &countingStore{Store: &store.Mem{}}
	e.core.Store = st
	s := e.connect(t, "h1", "us", 16, 64)
	s.push(t, heartbeat(false))
	s.push(t, heartbeat(false))
	s.push(t, heartbeat(false))
	if n := st.hostPuts.Load(); n != 2 { // Hello, first heartbeat
		t.Fatalf("host writes = %d, want 2", n)
	}
	s.push(t, heartbeat(true))
	if n := st.hostPuts.Load(); n != 3 {
		t.Fatalf("drain not persisted: %d writes", n)
	}
	hosts, _ := st.Hosts(context.Background())
	if len(hosts) != 1 || !hosts[0].Host.Draining || hosts[0].Ratio != scheduler.StartRatio || hosts[0].Host.VCPU != 16 {
		t.Fatalf("hosts = %+v", hosts)
	}
}

func TestStorePrebuilds(t *testing.T) {
	var errs []string
	p := &StorePrebuilds{Store: &store.Mem{}, OnError: func(op string, err error) { errs = append(errs, op) }}
	if s, c := p.Latest("acme/api"); s != nil || c != nil {
		t.Fatalf("empty registry = %v %v", s, c)
	}
	if err := p.SetConfig("acme/api", &prebuild.Config{Version: 1, KeyFiles: []string{"go.sum"}}); err != nil {
		t.Fatal(err)
	}
	p.Register(prebuild.Snapshot{Repo: "acme/api", Key: "pb1-x", Commit: "c1", Healthy: true})
	s, c := p.Latest("acme/api")
	if s == nil || s.Key != "pb1-x" || c == nil || c.KeyFiles[0] != "go.sum" {
		t.Fatalf("latest = %+v %+v", s, c)
	}
	if len(errs) != 0 {
		t.Fatalf("not-found reported as errors: %v", errs)
	}
}

// hookStore runs onPut while a job write is in flight, before it lands.
type hookStore struct {
	store.Store
	onPut func(store.Job)
}

func (s *hookStore) PutJob(ctx context.Context, j store.Job) error {
	if s.onPut != nil {
		s.onPut(j)
	}
	return s.Store.PutJob(ctx, j)
}

func openJob(t *testing.T, st store.Store, id string) store.Job {
	t.Helper()
	jobs, err := st.OpenJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("job %s not open", id)
	return store.Job{}
}

// A deferred job is persisted as deferred before RetryDeferred can see it,
// so a retry that places it is never overwritten by the late write.
func TestDeferredWriteDoesNotOverwriteAssigned(t *testing.T) {
	ctx := context.Background()
	st := &hookStore{Store: &store.Mem{}}
	e := newEnv()
	e.core.Store = st
	h1 := e.connect(t, "h1", "us", 4, 16)
	h1.push(t, heartbeat(false))
	if err := e.core.Submit(ctx, e.submission(t, "j1", 4)); err != nil {
		t.Fatal(err)
	}
	if h1.next(t).GetAssign().GetJobId() != "j1" {
		t.Fatal("j1 not assigned")
	}

	// While j2's deferred write is in flight, j1 finishes and the retry
	// loop runs, as the ticker goroutine could.
	fired := false
	st.onPut = func(j store.Job) {
		if j.ID != "j2" || j.State != store.JobDeferred || fired {
			return
		}
		fired = true
		e.core.Scheduler.Release("j1", nil)
		e.core.RetryDeferred(ctx)
	}
	j2 := e.submission(t, "j2", 2)
	j2.Deferrable = true
	if err := e.core.Submit(ctx, j2); err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("j2 not deferred")
	}
	e.core.RetryDeferred(ctx)
	if h1.next(t).GetAssign().GetJobId() != "j2" {
		t.Fatal("j2 not assigned")
	}
	h1.none(t)
	if j := openJob(t, st, "j2"); j.State != store.JobAssigned || j.HostID != "h1" {
		t.Fatalf("j2 persisted as %q on %q", j.State, j.HostID)
	}
}

// Re-submitting a job that is running or deferred (the frontend's
// reconcile does this) leaves it and its persisted row alone, including
// after a restart restored its reservation.
func TestDuplicateSubmitKeepsJob(t *testing.T) {
	ctx := context.Background()
	st := &store.Mem{}
	e := newEnv()
	e.core.Store = st
	h1 := e.connect(t, "h1", "us", 4, 16)
	h1.push(t, heartbeat(false))
	j1 := e.submission(t, "j1", 4)
	if err := e.core.Submit(ctx, j1); err != nil {
		t.Fatal(err)
	}
	if h1.next(t).GetAssign().GetJobId() != "j1" {
		t.Fatal("j1 not assigned")
	}
	j2 := e.submission(t, "j2", 2)
	j2.Deferrable = true
	for range 2 {
		if err := e.core.Submit(ctx, j1); err != nil {
			t.Fatalf("duplicate j1: %v", err)
		}
		if err := e.core.Submit(ctx, j2); err != nil {
			t.Fatalf("j2: %v", err)
		}
	}
	h1.none(t)
	if j := openJob(t, st, "j1"); j.State != store.JobAssigned || j.HostID != "h1" || j.CPU != 4 {
		t.Fatalf("j1 persisted as %+v", j)
	}
	e.core.mu.Lock()
	n := len(e.core.deferred)
	e.core.mu.Unlock()
	if n != 1 {
		t.Fatalf("deferred queue has %d entries, want 1", n)
	}

	// After a restart, the restored j1 is re-submitted before its host
	// reconnects.
	e2 := e.restarted(st)
	if _, err := e2.core.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e2.core.Submit(ctx, e2.submission(t, "j1", 4)); err != nil {
		t.Fatalf("duplicate j1 after restart: %v", err)
	}
	if j := openJob(t, st, "j1"); j.State != store.JobAssigned || j.HostID != "h1" {
		t.Fatalf("j1 after restart persisted as %+v", j)
	}
	if !e2.core.Scheduler.Placed("j1") {
		t.Fatal("j1 reservation lost")
	}
}
