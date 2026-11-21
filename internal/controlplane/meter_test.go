package controlplane

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/billing"
)

func TestJobResultMetersFromTelemetryRecord(t *testing.T) {
	e := newEnv()
	meter := billing.NewMeter(billing.PriceBook{
		RunnerPerMinute:   map[int]billing.Money{4: 6_000},
		PrebuildAllowance: time.Minute,
	})
	e.core.Meter = meter
	s := e.connect(t, "h1", "us", 8, 32)
	s.push(t, heartbeat(false))

	t0 := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	event := func(job string, p hostlinkv1.Phase, at time.Time) {
		s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobEvent{JobEvent: &hostlinkv1.JobEvent{
			JobId: job, Phase: p, At: timestamppb.New(at),
		}}})
	}
	result := func(r *hostlinkv1.JobResult) {
		s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobResult{JobResult: r}})
	}

	for _, id := range []string{"j1", "pb"} {
		if err := e.core.Submit(context.Background(), e.submission(t, id, 4)); err != nil {
			t.Fatal(err)
		}
		s.next(t)
		event(id, hostlinkv1.Phase_PHASE_VM_RESUMED, t0)
		event(id, hostlinkv1.Phase_PHASE_JOB_FINISHED, t0.Add(90*time.Second))
	}
	result(&hostlinkv1.JobResult{JobId: "j1", Outcome: hostlinkv1.JobResult_OUTCOME_SUCCEEDED})
	result(&hostlinkv1.JobResult{
		JobId: "pb", Outcome: hostlinkv1.JobResult_OUTCOME_SUCCEEDED,
		Prebuild: &hostlinkv1.SnapshotRef{Kind: hostlinkv1.SnapshotRef_KIND_PREBUILD, Repo: "acme/api", Key: "pb1-new", Id: "x"},
	})
	// A duplicate result does not double-bill.
	result(&hostlinkv1.JobResult{JobId: "j1", Outcome: hostlinkv1.JobResult_OUTCOME_SUCCEEDED})

	if !e.core.Record("pb").Prebuild {
		t.Error("prebuild record not flagged")
	}
	inv := meter.Invoice("gh_inst_1", "2026-11")
	got := map[billing.Kind]float64{}
	for _, l := range inv.Lines {
		got[l.Kind] += l.Quantity
	}
	if got[billing.KindRunner] != 90 || got[billing.KindPrebuildIncluded] != 60 || got[billing.KindPrebuildOverage] != 30 {
		t.Errorf("metered = %v", got)
	}
	if inv.Total != 9_000+3_000 {
		t.Errorf("total = %v", inv.Total)
	}
}
