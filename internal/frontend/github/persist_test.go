package github

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/store"
)

type failingStore struct{ store.Store }

func (failingStore) PutPendingRunner(context.Context, store.PendingRunner) error {
	return errors.New("db down")
}

func TestAdapterRestartKeepsPendingAndActiveRunners(t *testing.T) {
	ctx := context.Background()
	st := &store.Mem{}
	h := newHarness(t)
	h.a.Store = st
	h.post("workflow_job", jobEvent("queued", 9, tachyonLabels, ""), testSecret)
	h.post("workflow_job", jobEvent("queued", 10, tachyonLabels, ""), testSecret)
	pendingRunner, activeRunner := h.api.created[0], h.api.created[1]
	h.post("workflow_job", jobEvent("in_progress", 10, tachyonLabels, activeRunner), testSecret)

	pend, _ := st.PendingRunners(ctx, "github")
	if len(pend) != 1 || pend[0].ExternalID != 9 || pend[0].RunnerName != pendingRunner || pend[0].Submission.Spec.JobID != "gh_9" {
		t.Fatalf("persisted pending = %+v", pend)
	}
	if act, _ := st.ActiveRunners(ctx, "github"); len(act) != 1 || act[0].Name != activeRunner {
		t.Fatalf("persisted active = %+v", act)
	}

	// Restart: a fresh adapter on the same store.
	h2 := newHarness(t)
	h2.clock = h.clock
	h2.a.Store = st
	if n, err := h2.a.Restore(ctx); err != nil || n != 1 {
		t.Fatalf("Restore = %d %v", n, err)
	}
	// The orphan sweep knows both runners; only the true orphan goes.
	h2.api.runners = []Runner{
		{ID: 1, Name: pendingRunner},
		{ID: 2, Name: activeRunner},
		{ID: 100, Name: "tachyon-orphan"},
	}
	h2.a.Reconcile(ctx)
	if fmt.Sprint(h2.api.deleted) != "[100]" {
		t.Fatalf("deleted = %v, want only the orphan [100]", h2.api.deleted)
	}
	// The pending job is still retried once stale.
	h2.api.deleted, h2.api.runners = nil, nil
	h2.clock = h2.clock.Add(2 * time.Minute)
	h2.a.Reconcile(ctx)
	if len(h2.core.subs) != 1 || h2.core.subs[0].Spec.JobID != "gh_9-2" {
		t.Fatalf("restored job not re-dispatched: %+v", h2.core.subs)
	}
	if fmt.Sprint(h2.api.deleted) != "[1]" {
		t.Fatalf("old runner not cleaned up: %v", h2.api.deleted)
	}
	pend, _ = st.PendingRunners(ctx, "github")
	if len(pend) != 1 || pend[0].Attempts != 2 || pend[0].RunnerName == pendingRunner {
		t.Fatalf("re-dispatch not persisted: %+v", pend)
	}

	// Jobs leaving the pending and active sets leave the store.
	h2.post("workflow_job", jobEvent("completed", 10, tachyonLabels, activeRunner), testSecret)
	h2.post("workflow_job", jobEvent("completed", 9, tachyonLabels, ""), testSecret)
	if act, _ := st.ActiveRunners(ctx, "github"); len(act) != 0 {
		t.Fatalf("active after completed = %+v", act)
	}
	if pend, _ := st.PendingRunners(ctx, "github"); len(pend) != 0 {
		t.Fatalf("pending after completed = %+v", pend)
	}
}

func TestAdapterGiveUpForgetsPending(t *testing.T) {
	ctx := context.Background()
	st := &store.Mem{}
	h := newHarness(t)
	h.a.Store = st
	h.a.MaxAttempts = 1
	h.post("workflow_job", jobEvent("queued", 9, tachyonLabels, ""), testSecret)
	h.clock = h.clock.Add(2 * time.Minute)
	h.a.Reconcile(ctx)
	if pend, _ := st.PendingRunners(ctx, "github"); len(pend) != 0 {
		t.Fatalf("abandoned job still persisted: %+v", pend)
	}
}

func TestAdapterRefusesJobItCannotPersist(t *testing.T) {
	h := newHarness(t)
	h.a.Store = failingStore{&store.Mem{}}
	if code := h.post("workflow_job", jobEvent("queued", 9, tachyonLabels, ""), testSecret); code != 500 {
		t.Fatalf("status %d, want 500 so GitHub redelivers", code)
	}
	if len(h.core.subs) != 0 {
		t.Fatal("submitted a job that wasn't recorded")
	}
	if fmt.Sprint(h.api.deleted) != "[1]" {
		t.Fatalf("runner not cleaned up: %v", h.api.deleted)
	}
}
