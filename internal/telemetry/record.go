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
	Prebuild      bool   // built a prebuild snapshot; billed against the repo's allowance

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

// Billable is VM resumed to job finished: runner time, billed per second.
func (r *Record) Billable() (time.Duration, bool) {
	d, ok := r.between(VMResumed, JobFinished)
	if !ok {
		return 0, false
	}
	return d.Round(time.Second), true
}
