package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/telemetry"
)

func TestDeferrableWaitsForTroughThenStartsWithinBound(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	now := time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC)
	e.core.Now = func() time.Time { return now }
	e.core.OffPeak = &telemetry.OffPeak{}
	burst := &fakeBurst{}
	e.core.Burst = burst
	s := e.connect(t, "h1", "us", 16, 64)
	s.push(t, heartbeat(false))

	busy := e.submission(t, "busy", 8) // 50% of the fleet: not quiet
	busy.QueuedAt = now
	if err := e.core.Submit(ctx, busy); err != nil {
		t.Fatal(err)
	}
	if s.next(t).GetAssign() == nil {
		t.Fatal("expected AssignJob for busy")
	}

	d1 := e.submission(t, "d1", 4)
	d1.Deferrable, d1.QueuedAt = true, now
	d2 := e.submission(t, "d2", 16) // never fits beside d1
	d2.Deferrable, d2.QueuedAt = true, now
	if err := e.core.Submit(ctx, d1); err != nil {
		t.Fatal(err)
	}
	if err := e.core.Submit(ctx, d2); err != nil {
		t.Fatal(err)
	}
	s.none(t)
	if rec := e.core.Record("d1"); rec == nil || !rec.Deferrable {
		t.Fatalf("d1 record = %+v", rec)
	}

	// Still busy an hour later: both stay deferred.
	now = now.Add(time.Hour)
	e.core.RetryDeferred(ctx)
	s.none(t)

	// The fleet goes quiet: d1 starts in the trough. d2 does not fit on an
	// owned host but is within its bound, so it waits instead of bursting.
	e.core.Scheduler.Release("busy", nil)
	now = now.Add(time.Hour)
	e.core.RetryDeferred(ctx)
	if a := s.next(t).GetAssign(); a == nil || a.GetJobId() != "d1" {
		t.Fatalf("expected d1 assigned, got %v", a)
	}
	if len(burst.subs) != 0 {
		t.Fatalf("deferrable job burst before its bound: %+v", burst.subs)
	}

	// At the bound d2 is placed regardless; owned capacity is full, so it
	// spills to burst.
	now = now.Add(DefaultMaxDeferral - 2*time.Hour)
	e.core.RetryDeferred(ctx)
	if len(burst.subs) != 1 || burst.subs[0].Spec.JobID != "d2" {
		t.Fatalf("burst = %+v", burst.subs)
	}
	e.core.RetryDeferred(ctx) // nothing left
	s.none(t)

	r := e.core.OffPeak.Report()
	if r.Started != 2 || r.Overdue != 1 || r.MaxWait != DefaultMaxDeferral {
		t.Fatalf("waits = %+v", r)
	}
	if r.Window != DefaultMaxDeferral-time.Hour || r.Gain <= 0 {
		t.Fatalf("utilization = %+v", r)
	}
}

func TestOverdueDeferrableSkipsBusyGate(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	now := time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC)
	e.core.Now = func() time.Time { return now }
	e.core.MaxDeferral = 30 * time.Minute
	s := e.connect(t, "h1", "us", 16, 64)
	s.push(t, heartbeat(false))

	busy := e.submission(t, "busy", 8)
	if err := e.core.Submit(ctx, busy); err != nil {
		t.Fatal(err)
	}
	s.next(t)
	d := e.submission(t, "d", 2)
	d.Deferrable, d.QueuedAt = true, now
	if err := e.core.Submit(ctx, d); err != nil {
		t.Fatal(err)
	}
	s.none(t)
	now = now.Add(30 * time.Minute)
	e.core.RetryDeferred(ctx) // fleet still busy, but d is due
	if a := s.next(t).GetAssign(); a == nil || a.GetJobId() != "d" {
		t.Fatalf("expected d assigned at its bound, got %v", a)
	}
}
