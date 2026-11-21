package telemetry

import (
	"math"
	"sort"
	"sync"
	"time"
)

// OffPeak measures what the deferrable off-peak class adds to the fleet:
// time-weighted CPU utilization with and without deferrable jobs, and how
// long deferred jobs waited before they started. Safe for concurrent use.
type OffPeak struct {
	// WaitWindow is how many recent waits feed the percentile; default 1000.
	WaitWindow int

	mu        sync.Mutex
	have      bool
	last      time.Time
	lastTotal float64
	lastDef   float64
	span      time.Duration
	totalArea float64 // utilization x seconds
	defArea   float64

	started int
	overdue int
	maxWait time.Duration
	sumWait time.Duration
	waits   []time.Duration
}

// Sample records fleet utilization at a point in time: total is reserved
// CPU over physical vCPUs, deferrable the part of it held by off-peak jobs.
// Utilization is treated as constant between samples. Samples older than
// the previous one are ignored.
func (m *OffPeak) Sample(at time.Time, total, deferrable float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.have {
		if at.Before(m.last) {
			return
		}
		dt := at.Sub(m.last)
		m.span += dt
		m.totalArea += m.lastTotal * dt.Seconds()
		m.defArea += m.lastDef * dt.Seconds()
	}
	m.have, m.last, m.lastTotal, m.lastDef = true, at, total, deferrable
}

// Started records a deferrable job leaving the deferred queue after wait.
// overdue means it reached the class's start bound and was placed even
// though the fleet was busy.
func (m *OffPeak) Started(wait time.Duration, overdue bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started++
	if overdue {
		m.overdue++
	}
	m.sumWait += wait
	m.maxWait = max(m.maxWait, wait)
	w := m.WaitWindow
	if w == 0 {
		w = 1000
	}
	m.waits = append(m.waits, wait)
	if len(m.waits) > w {
		m.waits = m.waits[len(m.waits)-w:]
	}
}

// OffPeakReport summarizes an OffPeak meter.
type OffPeakReport struct {
	Window time.Duration // time covered by utilization samples
	// AvgUtil is mean fleet utilization; AvgBaseUtil the same without
	// deferrable jobs.
	AvgUtil, AvgBaseUtil float64
	// Gain is AvgUtil - AvgBaseUtil: fleet capacity, as a fraction of
	// physical vCPUs, that the off-peak class filled.
	Gain float64
	// RelativeGain is Gain / AvgBaseUtil (0 when the base is 0).
	RelativeGain float64

	Started, Overdue int
	MeanWait         time.Duration
	P95Wait          time.Duration // over the last WaitWindow starts
	MaxWait          time.Duration
}

func (m *OffPeak) Report() OffPeakReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := OffPeakReport{Window: m.span, Started: m.started, Overdue: m.overdue, MaxWait: m.maxWait}
	if s := m.span.Seconds(); s > 0 {
		r.AvgUtil = m.totalArea / s
		r.Gain = m.defArea / s
		r.AvgBaseUtil = r.AvgUtil - r.Gain
		if r.AvgBaseUtil > 0 {
			r.RelativeGain = r.Gain / r.AvgBaseUtil
		}
	}
	if m.started > 0 {
		r.MeanWait = m.sumWait / time.Duration(m.started)
	}
	if len(m.waits) > 0 {
		w := append([]time.Duration(nil), m.waits...)
		sort.Slice(w, func(i, j int) bool { return w[i] < w[j] })
		r.P95Wait = w[int(math.Ceil(0.95*float64(len(w))))-1]
	}
	return r
}
