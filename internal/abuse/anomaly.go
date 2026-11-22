package abuse

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Kind is an anomaly pattern.
type Kind string

const (
	// CPUSustained: every vCPU near-saturated for a long stretch, the
	// signature of cryptocurrency mining.
	CPUSustained Kind = "cpu-sustained"
	// EgressSustained: a high outbound byte rate held for a long stretch
	// (bulk exfiltration, volumetric attacks, proxying).
	EgressSustained Kind = "egress-sustained"
	// PacketFlood: a very high rate of small outbound packets (SYN or UDP
	// floods).
	PacketFlood Kind = "packet-flood"
	// Scan: connections fanning out to many destinations (port or host
	// scanning, spam).
	Scan Kind = "scan"
)

// Severity of an alert.
type Severity string

const (
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

// Thresholds define when a pattern fires. Zero disables a rule.
type Thresholds struct {
	// CPUBusyFraction of the job's vCPUs counts as saturated...
	CPUBusyFraction float64
	// ...and CPUFor of continuous saturation fires CPUSustained.
	CPUFor time.Duration
	// EgressBytesPerSec held for EgressFor fires EgressSustained.
	EgressBytesPerSec float64
	EgressFor         time.Duration
	// PacketsPerSec with an average packet below SmallPacketBytes fires
	// PacketFlood in a single sample.
	PacketsPerSec    float64
	SmallPacketBytes float64
	// NewConnectionsPerSec, or DistinctDestinations within one sample,
	// fires Scan.
	NewConnectionsPerSec float64
	DistinctDestinations uint32
}

// DefaultThresholds apply to paid plans. CI jobs legitimately peg every
// core while compiling, but rarely for half an hour straight; real
// workloads open connections to a handful of registries and mirrors.
var DefaultThresholds = Thresholds{
	CPUBusyFraction:      0.9,
	CPUFor:               30 * time.Minute,
	EgressBytesPerSec:    50 << 20, // 50 MiB/s
	EgressFor:            5 * time.Minute,
	PacketsPerSec:        50_000,
	SmallPacketBytes:     200,
	NewConnectionsPerSec: 200,
	DistinctDestinations: 1000,
}

// RestrictedThresholds apply to free and trial plans.
var RestrictedThresholds = Thresholds{
	CPUBusyFraction:      0.9,
	CPUFor:               10 * time.Minute,
	EgressBytesPerSec:    10 << 20, // 10 MiB/s
	EgressFor:            2 * time.Minute,
	PacketsPerSec:        20_000,
	SmallPacketBytes:     200,
	NewConnectionsPerSec: 50,
	DistinctDestinations: 200,
}

// Job identifies a tracked job.
type Job struct {
	ID     string
	Tenant string
	Repo   string
	HostID string
	VCPU   int
	Plan   Plan
}

// Sample is one resource sample of a running job, deltas over Interval.
// Network fields are from the guest's point of view.
type Sample struct {
	JobID                string
	At                   time.Time
	Interval             time.Duration
	BusyVCPU             float64
	TxBytes              uint64
	RxBytes              uint64
	TxPackets            uint64
	NewConnections       uint32
	DistinctDestinations uint32
}

// Action is the response to an alert.
type Action struct {
	CancelJob     bool
	SuspendTenant bool
}

// Alert is one anomaly detected on a job.
type Alert struct {
	Kind     Kind
	Severity Severity
	Job      Job
	At       time.Time
	Detail   string
	Action   Action
}

// Alerter delivers alerts to people (pager, chat, ticket).
type Alerter interface {
	Alert(ctx context.Context, a Alert)
}

// LogAlerter logs alerts.
type LogAlerter struct{ Log *slog.Logger }

func (l LogAlerter) Alert(_ context.Context, a Alert) {
	log := l.Log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("abuse alert", "kind", a.Kind, "severity", a.Severity, "job", a.Job.ID,
		"tenant", a.Job.Tenant, "repo", a.Job.Repo, "host", a.Job.HostID, "plan", a.Job.Plan,
		"detail", a.Detail, "cancel", a.Action.CancelJob, "suspend", a.Action.SuspendTenant)
}

// Suspender suspends tenants. *Gate implements it.
type Suspender interface {
	Suspend(ctx context.Context, tenant, reason string) error
}

// Monitor tracks running jobs, detects anomaly patterns in their samples,
// and alerts once per job and pattern. Free and trial plans get stricter
// thresholds and an automatic response: any alert cancels the job and a
// critical one suspends the tenant. Paid plans are alerted on, and only
// critical patterns cancel the job; people decide the rest. Safe for
// concurrent use.
type Monitor struct {
	Alerter Alerter
	// Suspender is called for alerts whose action suspends the tenant; nil
	// skips suspension (the alert still says it was warranted).
	Suspender Suspender
	// Thresholds by plan; plans not listed use DefaultThresholds, or
	// RestrictedThresholds for free and trial.
	Thresholds map[Plan]Thresholds
	Log        *slog.Logger

	mu   sync.Mutex
	jobs map[string]*jobState
}

type jobState struct {
	job         Job
	cpuSince    time.Time // start of the current saturated stretch
	egressSince time.Time
	fired       map[Kind]bool
}

func (m *Monitor) thresholds(p Plan) Thresholds {
	if t, ok := m.Thresholds[p]; ok {
		return t
	}
	if p.Restricted() {
		return RestrictedThresholds
	}
	return DefaultThresholds
}

// Track starts watching a job.
func (m *Monitor) Track(j Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs == nil {
		m.jobs = map[string]*jobState{}
	}
	if st, ok := m.jobs[j.ID]; ok {
		st.job = j
		return
	}
	m.jobs[j.ID] = &jobState{job: j, fired: map[Kind]bool{}}
}

// Tracked reports whether a job is being watched.
func (m *Monitor) Tracked(jobID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.jobs[jobID]
	return ok
}

// Observe feeds one sample and returns the alerts it raised, after
// delivering them and suspending tenants as their actions say. Samples for
// untracked jobs are ignored.
func (m *Monitor) Observe(ctx context.Context, s Sample) []Alert {
	m.mu.Lock()
	st := m.jobs[s.JobID]
	var alerts []Alert
	if st != nil && s.Interval > 0 {
		alerts = m.detect(st, s)
	}
	m.mu.Unlock()
	m.respond(ctx, alerts)
	return alerts
}

// Finish stops watching a job. avgBusyVCPU and ran are its whole-run
// average CPU and duration (from the job result), which catch sustained
// saturation even when no live samples arrived.
func (m *Monitor) Finish(ctx context.Context, jobID string, avgBusyVCPU float64, ran time.Duration) []Alert {
	m.mu.Lock()
	st := m.jobs[jobID]
	delete(m.jobs, jobID)
	var alerts []Alert
	if st != nil && !st.fired[CPUSustained] {
		th := m.thresholds(st.job.Plan)
		if th.CPUFor > 0 && st.job.VCPU > 0 && ran >= th.CPUFor &&
			avgBusyVCPU >= th.CPUBusyFraction*float64(st.job.VCPU) {
			alerts = append(alerts, m.alert(st, CPUSustained, time.Now(),
				fmt.Sprintf("averaged %.1f of %d vCPU busy over %s", avgBusyVCPU, st.job.VCPU, ran.Round(time.Second))))
		}
	}
	m.mu.Unlock()
	m.respond(ctx, alerts)
	return alerts
}

// detect runs every rule against s. Caller holds m.mu.
func (m *Monitor) detect(st *jobState, s Sample) []Alert {
	th := m.thresholds(st.job.Plan)
	secs := s.Interval.Seconds()
	start := s.At.Add(-s.Interval)
	var out []Alert
	fire := func(k Kind, detail string) {
		if !st.fired[k] {
			out = append(out, m.alert(st, k, s.At, detail))
		}
	}

	// streak tracks a condition held across consecutive samples and returns
	// how long it has held.
	streak := func(since *time.Time, on bool) time.Duration {
		if !on {
			*since = time.Time{}
			return 0
		}
		if since.IsZero() {
			*since = start
		}
		return s.At.Sub(*since)
	}

	if th.CPUFor > 0 && st.job.VCPU > 0 {
		on := s.BusyVCPU >= th.CPUBusyFraction*float64(st.job.VCPU)
		if d := streak(&st.cpuSince, on); d >= th.CPUFor {
			fire(CPUSustained, fmt.Sprintf("%.1f of %d vCPU busy for %s", s.BusyVCPU, st.job.VCPU, d.Round(time.Second)))
		}
	}
	if th.EgressFor > 0 {
		rate := float64(s.TxBytes) / secs
		if d := streak(&st.egressSince, rate >= th.EgressBytesPerSec); d >= th.EgressFor {
			fire(EgressSustained, fmt.Sprintf("%.1f MiB/s outbound for %s", rate/(1<<20), d.Round(time.Second)))
		}
	}
	if th.PacketsPerSec > 0 && s.TxPackets > 0 {
		pps := float64(s.TxPackets) / secs
		avg := float64(s.TxBytes) / float64(s.TxPackets)
		if pps >= th.PacketsPerSec && avg < th.SmallPacketBytes {
			fire(PacketFlood, fmt.Sprintf("%.0f packets/s outbound, %.0f bytes average", pps, avg))
		}
	}
	cps := float64(s.NewConnections) / secs
	switch {
	case th.NewConnectionsPerSec > 0 && cps >= th.NewConnectionsPerSec:
		fire(Scan, fmt.Sprintf("%.0f new outbound connections/s", cps))
	case th.DistinctDestinations > 0 && s.DistinctDestinations >= th.DistinctDestinations:
		fire(Scan, fmt.Sprintf("%d distinct destinations in %s", s.DistinctDestinations, s.Interval))
	}
	return out
}

// alert builds an alert and marks it fired. Caller holds m.mu.
func (m *Monitor) alert(st *jobState, k Kind, at time.Time, detail string) Alert {
	st.fired[k] = true
	sev := Warning
	if k == PacketFlood || k == Scan {
		sev = Critical
	}
	var act Action
	if st.job.Plan.Restricted() {
		act = Action{CancelJob: true, SuspendTenant: sev == Critical}
	} else {
		act = Action{CancelJob: sev == Critical}
	}
	return Alert{Kind: k, Severity: sev, Job: st.job, At: at, Detail: detail, Action: act}
}

func (m *Monitor) respond(ctx context.Context, alerts []Alert) {
	for _, a := range alerts {
		if m.Alerter != nil {
			m.Alerter.Alert(ctx, a)
		}
		if a.Action.SuspendTenant && m.Suspender != nil {
			reason := fmt.Sprintf("%s on job %s: %s", a.Kind, a.Job.ID, a.Detail)
			if err := m.Suspender.Suspend(ctx, a.Job.Tenant, reason); err != nil {
				m.log().Error("suspend tenant", "tenant", a.Job.Tenant, "err", err)
			}
		}
	}
}

func (m *Monitor) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}
