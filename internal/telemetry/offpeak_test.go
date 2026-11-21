package telemetry

import (
	"math"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestOffPeakUtilizationGain(t *testing.T) {
	var m OffPeak
	if r := m.Report(); r.Window != 0 || r.AvgUtil != 0 || r.P95Wait != 0 {
		t.Fatalf("empty report = %+v", r)
	}
	t0 := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	// Day: 80% busy, no deferrable jobs, for 1h.
	m.Sample(t0, 0.8, 0)
	// Night: 30% base load plus 40% deferrable, for 1h.
	m.Sample(t0.Add(time.Hour), 0.7, 0.4)
	m.Sample(t0.Add(30*time.Minute), 0.1, 0.1) // out of order, ignored
	m.Sample(t0.Add(2*time.Hour), 0, 0)

	r := m.Report()
	if r.Window != 2*time.Hour {
		t.Fatalf("window = %v", r.Window)
	}
	if !near(r.AvgUtil, 0.75) || !near(r.Gain, 0.2) || !near(r.AvgBaseUtil, 0.55) {
		t.Fatalf("report = %+v", r)
	}
	if !near(r.RelativeGain, 0.2/0.55) {
		t.Fatalf("relative gain = %v", r.RelativeGain)
	}
}

func TestOffPeakWaits(t *testing.T) {
	m := OffPeak{WaitWindow: 20}
	for i := 1; i <= 40; i++ {
		m.Started(time.Duration(i)*time.Minute, i == 40)
	}
	r := m.Report()
	if r.Started != 40 || r.Overdue != 1 {
		t.Fatalf("counts = %+v", r)
	}
	if r.MaxWait != 40*time.Minute || r.MeanWait != 20*time.Minute+30*time.Second {
		t.Fatalf("max/mean = %v / %v", r.MaxWait, r.MeanWait)
	}
	// Window keeps waits 21..40 minutes; nearest-rank p95 of 20 is the 19th.
	if r.P95Wait != 39*time.Minute {
		t.Fatalf("p95 = %v", r.P95Wait)
	}
}
