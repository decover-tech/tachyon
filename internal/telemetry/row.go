package telemetry

import (
	"encoding/json"
	"time"
)

// Pricing is the cost of capacity per vCPU-hour, used to compute each
// job's cost. Owned is amortized hardware and hosting; Burst is the rented
// on-demand rate. Both are configuration, not prices charged to customers.
type Pricing struct {
	OwnedVCPUHour float64
	BurstVCPUHour float64
}

// Capacity labels.
const (
	CapacityOwned = "owned"
	CapacityBurst = "burst"
)

// JobRow is one finished job as stored in ClickHouse (tachyon.job_records;
// see deploy/observability/clickhouse). Field names are the column names.
type JobRow struct {
	JobID         string `json:"job_id"`
	Tenant        string `json:"tenant"`
	Repo          string `json:"repo"`
	Region        string `json:"region"`
	HostID        string `json:"host_id"`
	VCPU          int    `json:"vcpu"`
	PrebuildMatch string `json:"prebuild_match"`
	Capacity      string `json:"capacity"`
	Outcome       string `json:"outcome"`

	WebhookReceivedAt *CHTime `json:"webhook_received_at"`
	HostAssignedAt    *CHTime `json:"host_assigned_at"`
	VMResumedAt       *CHTime `json:"vm_resumed_at"`
	RunnerOnlineAt    *CHTime `json:"runner_online_at"`
	JobStartedAt      *CHTime `json:"job_started_at"`
	JobFinishedAt     *CHTime `json:"job_finished_at"`
	VMDestroyedAt     *CHTime `json:"vm_destroyed_at"`

	// Derived durations; nil when the phases they span were not reached.
	PickupSeconds   *float64 `json:"pickup_seconds"`
	RunSeconds      *float64 `json:"run_seconds"`
	BillableSeconds *float64 `json:"billable_seconds"`

	StepNames   []string  `json:"step_names"`
	StepSeconds []float64 `json:"step_seconds"`

	CacheHits   int     `json:"cache_hits"`
	CacheMisses int     `json:"cache_misses"`
	HostLoad    float64 `json:"host_load"`
	CostUSD     float64 `json:"cost_usd"`
}

// CHTime marshals as a ClickHouse DateTime64(3) literal in UTC, which
// JSONEachRow parses without best-effort settings.
type CHTime time.Time

const chLayout = "2006-01-02 15:04:05.000"

func (t CHTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(t).UTC().Format(chLayout))
}

func (t *CHTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseInLocation(chLayout, s, time.UTC)
	if err != nil {
		return err
	}
	*t = CHTime(v)
	return nil
}

// Run is job started to job finished: the job's own wall time, which is
// what oversubscription slows down.
func (r *Record) Run() (time.Duration, bool) { return r.between(JobStarted, JobFinished) }

// Cache returns the job's cache hit and miss counts.
func (r *Record) Cache() (hits, misses int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cacheHits, r.cacheMiss
}

// Capacity is owned or burst.
func (r *Record) Capacity() string {
	if r.Burst {
		return CapacityBurst
	}
	return CapacityOwned
}

// Cost is billable vCPU-seconds priced at the job's capacity class.
func (r *Record) Cost(p Pricing) float64 {
	b, ok := r.Billable()
	if !ok {
		return 0
	}
	rate := p.OwnedVCPUHour
	if r.Burst {
		rate = p.BurstVCPUHour
	}
	return b.Seconds() * float64(r.VCPU) * rate / 3600
}

// Row flattens the record for export.
func (r *Record) Row(p Pricing) JobRow {
	row := JobRow{
		JobID:         r.JobID,
		Tenant:        r.Tenant,
		Repo:          r.Repo,
		Region:        r.Region,
		HostID:        r.HostID,
		VCPU:          r.VCPU,
		PrebuildMatch: r.PrebuildMatch,
		Capacity:      r.Capacity(),
		Outcome:       r.Outcome,
		HostLoad:      r.HostLoad,
		CostUSD:       r.Cost(p),
		StepNames:     []string{},
		StepSeconds:   []float64{},
	}
	at := func(ph Phase) *CHTime {
		if t, ok := r.At(ph); ok {
			c := CHTime(t)
			return &c
		}
		return nil
	}
	row.WebhookReceivedAt = at(WebhookReceived)
	row.HostAssignedAt = at(HostAssigned)
	row.VMResumedAt = at(VMResumed)
	row.RunnerOnlineAt = at(RunnerOnline)
	row.JobStartedAt = at(JobStarted)
	row.JobFinishedAt = at(JobFinished)
	row.VMDestroyedAt = at(VMDestroyed)

	secs := func(d time.Duration, ok bool) *float64 {
		if !ok {
			return nil
		}
		s := d.Seconds()
		return &s
	}
	row.PickupSeconds = secs(r.Pickup())
	row.RunSeconds = secs(r.Run())
	row.BillableSeconds = secs(r.Billable())

	row.CacheHits, row.CacheMisses = r.Cache()
	r.mu.Lock()
	for _, s := range r.steps {
		row.StepNames = append(row.StepNames, s.Name)
		row.StepSeconds = append(row.StepSeconds, s.End.Sub(s.Start).Seconds())
	}
	r.mu.Unlock()
	return row
}
