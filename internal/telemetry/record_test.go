package telemetry

import (
	"testing"
	"time"
)

func TestPickupAndBillable(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	r := NewRecord("j")
	if _, ok := r.Pickup(); ok {
		t.Fatal("pickup without phases")
	}
	r.Mark(WebhookReceived, t0)
	r.Mark(VMResumed, t0.Add(800*time.Millisecond))
	r.Mark(RunnerOnline, t0.Add(1500*time.Millisecond))
	r.Mark(RunnerOnline, t0.Add(time.Hour)) // first mark wins
	if d, ok := r.Pickup(); !ok || d != 1500*time.Millisecond {
		t.Fatalf("Pickup = %v %v", d, ok)
	}
	if _, ok := r.Billable(); ok {
		t.Fatal("billable before job finished")
	}
	r.Mark(JobFinished, t0.Add(800*time.Millisecond+90*time.Second+400*time.Millisecond))
	if d, ok := r.Billable(); !ok || d != 90*time.Second {
		t.Fatalf("Billable = %v %v, want 1m30s rounded", d, ok)
	}
	r.CacheResult(true)
	r.CacheResult(false)
	r.AddStep(Step{Name: "test"})
	if at, ok := r.At(WebhookReceived); !ok || !at.Equal(t0) {
		t.Error("At")
	}
}

func TestRecordDataRoundTrip(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	r := NewRecord("j1")
	r.Tenant, r.Repo, r.HostID, r.VCPU, r.PrebuildMatch, r.Burst = "t", "acme/api", "h1", 4, "full", true
	r.Mark(WebhookReceived, t0)
	r.Mark(RunnerOnline, t0.Add(2*time.Second))
	r.AddStep(Step{Name: "test", Start: t0, End: t0.Add(time.Second)})
	r.CacheResult(true)
	r.CacheResult(false)

	got := FromData(r.Data())
	if d, ok := got.Pickup(); !ok || d != 2*time.Second {
		t.Errorf("pickup = %v %v", d, ok)
	}
	gd := got.Data()
	if gd.HostID != "h1" || gd.VCPU != 4 || !gd.Burst || gd.PrebuildMatch != "full" || len(gd.Steps) != 1 || gd.CacheHits != 1 || gd.CacheMisses != 1 {
		t.Errorf("data = %+v", gd)
	}
	// The copy is independent of the original.
	gd.Phases[JobFinished] = t0
	if _, ok := got.At(JobFinished); ok {
		t.Error("Data shares the phase map")
	}
}
