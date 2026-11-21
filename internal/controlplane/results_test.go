package controlplane

import (
	"context"
	"sync"
	"testing"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/jobapi"
)

type recordResults struct {
	mu  sync.Mutex
	got []jobapi.Result
}

func (r *recordResults) OnResult(_ context.Context, res jobapi.Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, res)
}

func TestJobResultForwardedToResultHandler(t *testing.T) {
	e := newEnv()
	rec := &recordResults{}
	e.core.Results = rec
	s := e.connect(t, "h1", "us", 4, 16)

	for _, tc := range []struct {
		id string
		in hostlinkv1.JobResult_Outcome
	}{
		{"j1", hostlinkv1.JobResult_OUTCOME_SUCCEEDED},
		{"j2", hostlinkv1.JobResult_OUTCOME_FAILED},
		{"j3", hostlinkv1.JobResult_OUTCOME_INFRA_ERROR},
		{"j4", hostlinkv1.JobResult_OUTCOME_CANCELLED},
		{"j5", hostlinkv1.JobResult_OUTCOME_UNSPECIFIED},
	} {
		s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobResult{JobResult: &hostlinkv1.JobResult{
			JobId: tc.id, Outcome: tc.in, Error: "e-" + tc.id,
		}}})
	}

	want := []jobapi.Result{
		{JobID: "j1", Outcome: jobapi.OutcomeSucceeded, Error: "e-j1"},
		{JobID: "j2", Outcome: jobapi.OutcomeFailed, Error: "e-j2"},
		{JobID: "j3", Outcome: jobapi.OutcomeInfra, Error: "e-j3"},
		{JobID: "j4", Outcome: jobapi.OutcomeCancelled, Error: "e-j4"},
		{JobID: "j5", Outcome: "", Error: "e-j5"},
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.got) != len(want) {
		t.Fatalf("got %d results, want %d: %+v", len(rec.got), len(want), rec.got)
	}
	for i := range want {
		if rec.got[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, rec.got[i], want[i])
		}
	}
}
