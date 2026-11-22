package controlplane

import (
	"context"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/types/known/timestamppb"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

type memRows struct {
	mu   sync.Mutex
	rows []telemetry.JobRow
}

func (m *memRows) Add(r telemetry.JobRow) {
	m.mu.Lock()
	m.rows = append(m.rows, r)
	m.mu.Unlock()
}

func jobEvent(id string, p hostlinkv1.Phase, at time.Time) *hostlinkv1.LinkRequest {
	return &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobEvent{JobEvent: &hostlinkv1.JobEvent{
		JobId: id, Phase: p, At: timestamppb.New(at),
	}}}
}

func TestJobRecordExportedOnResult(t *testing.T) {
	e := newEnv()
	reader := sdkmetric.NewManualReader()
	m, err := telemetry.NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t"))
	if err != nil {
		t.Fatal(err)
	}
	rows := &memRows{}
	e.core.Metrics, e.core.JobRecords = m, rows
	e.core.Pricing = telemetry.Pricing{OwnedVCPUHour: 3600}
	if err := e.core.RegisterMetrics(); err != nil {
		t.Fatal(err)
	}

	s := e.connect(t, "h1", "fra", 4, 16)
	s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_Heartbeat{Heartbeat: &hostlinkv1.Heartbeat{
		CpuBusyFraction: 0.6, CpuStealFraction: 0.02, WarmVmsByVcpu: map[uint32]uint32{4: 1},
	}}})

	sub := e.submission(t, "j1", 4)
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	sub.QueuedAt = t0
	if err := e.core.Submit(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	s.next(t)

	s.push(t, jobEvent("j1", hostlinkv1.Phase_PHASE_VM_RESUMED, t0.Add(500*time.Millisecond)))
	s.push(t, jobEvent("j1", hostlinkv1.Phase_PHASE_RUNNER_ONLINE, t0.Add(2*time.Second)))
	// Resent after a reconnect: must not count a second pickup.
	s.push(t, jobEvent("j1", hostlinkv1.Phase_PHASE_RUNNER_ONLINE, t0.Add(9*time.Second)))
	s.push(t, jobEvent("j1", hostlinkv1.Phase_PHASE_JOB_STARTED, t0.Add(3*time.Second)))
	s.push(t, jobEvent("j1", hostlinkv1.Phase_PHASE_JOB_FINISHED, t0.Add(60*time.Second+500*time.Millisecond)))
	s.push(t, jobEvent("j1", hostlinkv1.Phase_PHASE_VM_DESTROYED, t0.Add(61*time.Second)))
	result := &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobResult{JobResult: &hostlinkv1.JobResult{
		JobId: "j1", Outcome: hostlinkv1.JobResult_OUTCOME_INFRA_ERROR,
	}}}
	s.push(t, result)
	s.push(t, result) // duplicate result is ignored

	rows.mu.Lock()
	got := append([]telemetry.JobRow(nil), rows.rows...)
	rows.mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	r := got[0]
	if r.JobID != "j1" || r.Region != "fra" || r.HostID != "h1" || r.Outcome != "infra_error" || r.Capacity != "owned" {
		t.Errorf("row = %+v", r)
	}
	if r.PickupSeconds == nil || *r.PickupSeconds != 2 || r.BillableSeconds == nil || *r.BillableSeconds != 60 {
		t.Errorf("pickup/billable = %v/%v", r.PickupSeconds, r.BillableSeconds)
	}
	if r.HostLoad != 1 {
		t.Errorf("host load = %v, want 1 (4 of 4 vCPU reserved)", r.HostLoad)
	}
	if r.CostUSD != 60*4 {
		t.Errorf("cost = %v", r.CostUSD)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	byName := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, x := range sm.Metrics {
			byName[x.Name] = x
		}
	}
	pickup := byName[telemetry.MetricPickup].Data.(metricdata.Histogram[float64]).DataPoints
	if len(pickup) != 1 || pickup[0].Count != 1 || pickup[0].Sum != 2 {
		t.Errorf("pickup = %+v", pickup)
	}
	if q := byName[telemetry.MetricJobsQueued].Data.(metricdata.Sum[int64]).DataPoints[0].Value; q != 1 {
		t.Errorf("queued = %d", q)
	}
	busy := byName[telemetry.MetricHostCPUBusy].Data.(metricdata.Gauge[float64]).DataPoints
	if len(busy) != 1 || busy[0].Value != 0.6 {
		t.Errorf("host busy = %+v", busy)
	}
	if n := byName[telemetry.MetricHostsConnected].Data.(metricdata.Gauge[int64]).DataPoints[0].Value; n != 1 {
		t.Errorf("hosts = %d", n)
	}
}

func TestOutcomeName(t *testing.T) {
	for o, want := range map[hostlinkv1.JobResult_Outcome]string{
		hostlinkv1.JobResult_OUTCOME_SUCCEEDED:   "succeeded",
		hostlinkv1.JobResult_OUTCOME_INFRA_ERROR: "infra_error",
		hostlinkv1.JobResult_OUTCOME_UNSPECIFIED: "unknown",
	} {
		if got := outcomeName(o); got != want {
			t.Errorf("%v -> %q", o, got)
		}
	}
}
