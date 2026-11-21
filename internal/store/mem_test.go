package store_test

import (
	"context"
	"testing"

	"github.com/decover-tech/tachyon/internal/store"
	"github.com/decover-tech/tachyon/internal/store/storetest"
)

func TestMem(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return &store.Mem{} })
}

func TestMemCopies(t *testing.T) {
	ctx := context.Background()
	m := &store.Mem{}
	j := store.Job{ID: "j1", State: store.JobQueued, Submission: storetest.Submission("j1")}
	if err := m.PutJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	j.Submission.ChangedFiles[0] = "mutated"
	got, _ := m.OpenJobs(ctx)
	if got[0].Submission.ChangedFiles[0] != "go.sum" {
		t.Fatal("Mem aliases caller memory")
	}
}
