package telemetry

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func finishedRecord() *Record {
	r := NewRecord("job-1")
	r.Tenant, r.Repo, r.Region, r.HostID, r.VCPU = "acme", "acme/api", "fra", "h1", 4
	r.PrebuildMatch, r.HostLoad, r.Outcome = "full", 1.2, "succeeded"
	r.Mark(WebhookReceived, t0)
	r.Mark(HostAssigned, t0.Add(100*time.Millisecond))
	r.Mark(VMResumed, t0.Add(400*time.Millisecond))
	r.Mark(RunnerOnline, t0.Add(1500*time.Millisecond))
	r.Mark(JobStarted, t0.Add(2*time.Second))
	r.Mark(JobFinished, t0.Add(62*time.Second+400*time.Millisecond))
	r.AddStep(Step{Name: "test", Start: t0.Add(3 * time.Second), End: t0.Add(33 * time.Second)})
	r.CacheResult(true)
	r.CacheResult(true)
	r.CacheResult(false)
	return r
}

func TestRow(t *testing.T) {
	r := finishedRecord()
	row := r.Row(Pricing{OwnedVCPUHour: 0.036, BurstVCPUHour: 0.1})
	if row.Capacity != CapacityOwned || row.Outcome != "succeeded" || row.Region != "fra" {
		t.Fatalf("row = %+v", row)
	}
	if *row.PickupSeconds != 1.5 || *row.RunSeconds != 60.4 || *row.BillableSeconds != 62 {
		t.Errorf("durations pickup=%v run=%v billable=%v", *row.PickupSeconds, *row.RunSeconds, *row.BillableSeconds)
	}
	// 62 s * 4 vCPU * $0.036/h
	if want := 62.0 * 4 * 0.036 / 3600; math.Abs(row.CostUSD-want) > 1e-12 {
		t.Errorf("cost = %v, want %v", row.CostUSD, want)
	}
	if row.CacheHits != 2 || row.CacheMisses != 1 {
		t.Errorf("cache = %d/%d", row.CacheHits, row.CacheMisses)
	}
	if row.VMDestroyedAt != nil {
		t.Error("vm_destroyed_at should be null")
	}
	if len(row.StepNames) != 1 || row.StepSeconds[0] != 30 {
		t.Errorf("steps = %v %v", row.StepNames, row.StepSeconds)
	}

	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["webhook_received_at"] != "2026-11-21 12:00:00.000" || m["vm_destroyed_at"] != nil {
		t.Errorf("json times: %v / %v", m["webhook_received_at"], m["vm_destroyed_at"])
	}
	var back JobRow
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !time.Time(*back.RunnerOnlineAt).Equal(t0.Add(1500 * time.Millisecond)) {
		t.Error("CHTime round trip")
	}

	r.Burst = true
	if c := r.Cost(Pricing{OwnedVCPUHour: 1, BurstVCPUHour: 3600}); c != 62*4 {
		t.Errorf("burst cost = %v", c)
	}
	if NewRecord("x").Row(Pricing{}).PickupSeconds != nil {
		t.Error("empty record should have null pickup")
	}
}

func TestClickHouseWriteRows(t *testing.T) {
	var gotQuery, gotUser string
	var lines []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotQuery = req.URL.Query().Get("query")
		gotUser = req.Header.Get("X-ClickHouse-User")
		sc := bufio.NewScanner(req.Body)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
	}))
	defer srv.Close()
	ch := &ClickHouse{URL: srv.URL, User: "tachyon", Password: "pw"}
	rows := []JobRow{finishedRecord().Row(Pricing{}), NewRecord("job-2").Row(Pricing{})}
	if err := ch.WriteRows(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "INSERT INTO tachyon.job_records FORMAT JSONEachRow" || gotUser != "tachyon" {
		t.Errorf("query %q user %q", gotQuery, gotUser)
	}
	if len(lines) != 2 || !strings.Contains(lines[0], `"job_id":"job-1"`) {
		t.Errorf("body = %q", lines)
	}

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Code: 60. Table does not exist", http.StatusNotFound)
	}))
	defer fail.Close()
	err := (&ClickHouse{URL: fail.URL}).WriteRows(context.Background(), rows)
	if err == nil || !strings.Contains(err.Error(), "Table does not exist") {
		t.Errorf("err = %v", err)
	}
}

type fakeSink struct {
	mu      sync.Mutex
	batches [][]JobRow
	fail    bool
}

func (f *fakeSink) WriteRows(_ context.Context, rows []JobRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("down")
	}
	f.batches = append(f.batches, rows)
	return nil
}

func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.batches {
		n += len(b)
	}
	return n
}

func TestBatcher(t *testing.T) {
	sink := &fakeSink{fail: true}
	b := &Batcher{Sink: sink, BatchSize: 2, MaxBuffered: 3}
	for i := range 5 {
		b.Add(JobRow{JobID: string(rune('a' + i))})
	}
	if b.Pending() != 3 || b.Dropped() != 2 {
		t.Fatalf("pending %d dropped %d", b.Pending(), b.Dropped())
	}
	if err := b.Flush(context.Background()); err == nil {
		t.Fatal("flush should fail while sink is down")
	}
	if b.Pending() != 3 {
		t.Fatal("rows lost on failed flush")
	}
	sink.fail = false
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.Pending() != 0 || len(sink.batches) != 2 || sink.batches[0][0].JobID != "c" {
		t.Fatalf("batches = %+v", sink.batches)
	}
}

func TestBatcherRunFlushesFullBatchAndOnShutdown(t *testing.T) {
	sink := &fakeSink{}
	b := &Batcher{Sink: sink, BatchSize: 2, Interval: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	b.Add(JobRow{JobID: "a"})
	b.Add(JobRow{JobID: "b"}) // full batch: flushed without waiting an hour
	deadline := time.Now().Add(5 * time.Second)
	for sink.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sink.count() != 2 {
		t.Fatal("full batch not flushed")
	}
	b.Add(JobRow{JobID: "c"})
	cancel()
	<-done
	if sink.count() != 3 {
		t.Fatal("final flush on shutdown")
	}
}

func collect(t *testing.T, r *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func TestMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := NewMetrics(mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := finishedRecord()
	m.JobQueued(ctx, "", 4)
	m.JobBurst(ctx, "", 4)
	m.RunnerOnline(ctx, r)
	m.JobFinished(ctx, r, Pricing{OwnedVCPUHour: 3600})
	m.CacheRestore(ctx, "fra", true, 1<<20, 2*time.Second)
	m.CacheRestore(ctx, "fra", false, 0, 0)
	if err := m.ObserveHosts(func() []HostSample {
		return []HostSample{{HostID: "h1", Region: "fra", CPUBusy: 0.7, CPUSteal: 0.01, Ratio: 1.25, WarmVMs: map[uint32]uint32{4: 2}}}
	}, func() float64 { return 0.8 }); err != nil {
		t.Fatal(err)
	}
	b := &Batcher{Sink: &fakeSink{}}
	b.Add(JobRow{})
	if err := m.ObserveBatcher(b); err != nil {
		t.Fatal(err)
	}

	got := collect(t, reader)
	for _, name := range MetricNames {
		if _, ok := got[name]; !ok {
			t.Errorf("metric %s not exported", name)
		}
	}

	pickup := got[MetricPickup].Data.(metricdata.Histogram[float64]).DataPoints[0]
	if pickup.Sum != 1.5 || pickup.Count != 1 {
		t.Errorf("pickup = %v/%v", pickup.Sum, pickup.Count)
	}
	if len(pickup.Bounds) != len(PickupBuckets) {
		t.Errorf("pickup bounds = %v", pickup.Bounds)
	}
	if v, _ := pickup.Attributes.Value(attribute.Key("prebuild_match")); v.AsString() != "full" {
		t.Errorf("pickup attrs = %v", pickup.Attributes)
	}
	queued := got[MetricJobsQueued].Data.(metricdata.Sum[int64]).DataPoints[0]
	if v, _ := queued.Attributes.Value("region"); v.AsString() != "any" {
		t.Errorf("queued region = %v", v.AsString())
	}
	run := got[MetricJobRun].Data.(metricdata.Histogram[float64]).DataPoints[0]
	if v, _ := run.Attributes.Value("oversubscribed"); !v.AsBool() {
		t.Error("run should be labelled oversubscribed (load 1.2)")
	}
	cost := got[MetricCost].Data.(metricdata.Sum[float64]).DataPoints[0]
	if cost.Value != 62*4 {
		t.Errorf("cost = %v", cost.Value)
	}
	cacheBytes := got[MetricCacheBytes].Data.(metricdata.Sum[int64]).DataPoints[0]
	if cacheBytes.Value != 1<<20 {
		t.Errorf("cache bytes = %v", cacheBytes.Value)
	}
	if n := len(got[MetricCacheRequests].Data.(metricdata.Sum[int64]).DataPoints); n != 2 {
		t.Errorf("cache request series = %d, want hit and miss", n)
	}
	warm := got[MetricHostWarmVMs].Data.(metricdata.Gauge[int64]).DataPoints[0]
	if warm.Value != 2 {
		t.Errorf("warm = %v", warm.Value)
	}
	if v := got[MetricRowsPending].Data.(metricdata.Gauge[int64]).DataPoints[0].Value; v != 1 {
		t.Errorf("pending = %v", v)
	}
}

func TestNilMetricsIsNoop(t *testing.T) {
	var m *Metrics
	ctx := context.Background()
	m.JobQueued(ctx, "fra", 2)
	m.JobBurst(ctx, "fra", 2)
	m.RunnerOnline(ctx, finishedRecord())
	m.JobFinished(ctx, finishedRecord(), Pricing{})
	m.CacheRestore(ctx, "", true, 1, time.Second)
	if err := m.ObserveHosts(nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMetrics(nil); err != nil {
		t.Fatal(err)
	}
}
