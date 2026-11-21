// Package telemetry is the per-job phase record. One record per job drives
// billing, the customer's "minutes saved" report and fleet alerts.
package telemetry

import (
	"sync"
	"time"
)

type Phase string

const (
	WebhookReceived Phase = "webhook_received"
	HostAssigned    Phase = "host_assigned"
	VMResumed       Phase = "vm_resumed"
	RunnerOnline    Phase = "runner_online"
	JobStarted      Phase = "job_started"
	JobFinished     Phase = "job_finished"
	VMDestroyed     Phase = "vm_destroyed"
)

// Record is a job's timeline. Safe for concurrent use.
type Record struct {
	JobID         string
	Tenant        string
	Repo          string
	HostID        string
	VCPU          int
	PrebuildMatch string // full | partial | none
	Burst         bool   // ran on burst rather than owned capacity

	mu        sync.Mutex
	phases    map[Phase]time.Time
	steps     []Step
	cacheHits int
	cacheMiss int
}

type Step struct {
	Name       string
	Start, End time.Time
}

func NewRecord(jobID string) *Record {
	return &Record{JobID: jobID, phases: map[Phase]time.Time{}}
}

// Mark records the first time a phase is reached.
func (r *Record) Mark(p Phase, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.phases[p]; !ok {
		r.phases[p] = t
	}
}

func (r *Record) At(p Phase) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.phases[p]
	return t, ok
}

func (r *Record) AddStep(s Step) {
	r.mu.Lock()
	r.steps = append(r.steps, s)
	r.mu.Unlock()
}

func (r *Record) CacheResult(hit bool) {
	r.mu.Lock()
	if hit {
		r.cacheHits++
	} else {
		r.cacheMiss++
	}
	r.mu.Unlock()
}

func (r *Record) between(a, b Phase) (time.Duration, bool) {
	ta, ok1 := r.At(a)
	tb, ok2 := r.At(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	return tb.Sub(ta), true
}

// Pickup is webhook received to runner online (target p50 < 2s, p95 < 5s).
func (r *Record) Pickup() (time.Duration, bool) { return r.between(WebhookReceived, RunnerOnline) }

// RecordData is a Record's serializable form, for persistence.
type RecordData struct {
	JobID         string              `json:"job_id"`
	Tenant        string              `json:"tenant"`
	Repo          string              `json:"repo"`
	HostID        string              `json:"host_id"`
	VCPU          int                 `json:"vcpu"`
	PrebuildMatch string              `json:"prebuild_match"`
	Burst         bool                `json:"burst"`
	Phases        map[Phase]time.Time `json:"phases"`
	Steps         []Step              `json:"steps,omitempty"`
	CacheHits     int                 `json:"cache_hits"`
	CacheMisses   int                 `json:"cache_misses"`
}

// Data returns a copy of the record's contents.
func (r *Record) Data() RecordData {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := RecordData{
		JobID: r.JobID, Tenant: r.Tenant, Repo: r.Repo, HostID: r.HostID, VCPU: r.VCPU,
		PrebuildMatch: r.PrebuildMatch, Burst: r.Burst,
		Phases:    make(map[Phase]time.Time, len(r.phases)),
		Steps:     append([]Step(nil), r.steps...),
		CacheHits: r.cacheHits, CacheMisses: r.cacheMiss,
	}
	for p, t := range r.phases {
		d.Phases[p] = t
	}
	return d
}

// FromData rebuilds a Record from its persisted form.
func FromData(d RecordData) *Record {
	r := NewRecord(d.JobID)
	r.Tenant, r.Repo, r.HostID, r.VCPU = d.Tenant, d.Repo, d.HostID, d.VCPU
	r.PrebuildMatch, r.Burst = d.PrebuildMatch, d.Burst
	for p, t := range d.Phases {
		r.phases[p] = t
	}
	r.steps = append(r.steps, d.Steps...)
	r.cacheHits, r.cacheMiss = d.CacheHits, d.CacheMisses
	return r
}

// Billable is VM resumed to job finished: runner time, billed per second.
func (r *Record) Billable() (time.Duration, bool) {
	d, ok := r.between(VMResumed, JobFinished)
	if !ok {
		return 0, false
	}
	return d.Round(time.Second), true
}
