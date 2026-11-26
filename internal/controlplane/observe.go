package controlplane

import (
	"context"
	"strings"
	"time"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

// JobRecordSink receives each finished job's record for export
// (telemetry.Batcher in front of ClickHouse).
type JobRecordSink interface {
	Add(telemetry.JobRow)
}

// RegisterMetrics registers the host and fleet gauges on c.Metrics. Call
// once at startup, after setting Metrics and Scheduler.
func (c *Core) RegisterMetrics() error {
	return c.Metrics.ObserveHosts(c.hostSamples, c.Scheduler.Utilization)
}

func (c *Core) hostSamples() []telemetry.HostSample {
	c.mu.Lock()
	type snap struct {
		id, region string
		hb         *hostlinkv1.Heartbeat
	}
	hosts := make([]snap, 0, len(c.hosts))
	for _, hc := range c.hosts {
		hosts = append(hosts, snap{hc.id, hc.region, hc.hb})
	}
	c.mu.Unlock()
	out := make([]telemetry.HostSample, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, telemetry.HostSample{
			HostID:   h.id,
			Region:   h.region,
			CPUBusy:  h.hb.GetCpuBusyFraction(),
			CPUSteal: h.hb.GetCpuStealFraction(),
			WarmVMs:  h.hb.GetWarmVmsByVcpu(),
			Ratio:    c.Scheduler.Ratio(h.id),
			Load:     c.Scheduler.Load(h.id),
		})
	}
	return out
}

// markPhase records a phase reported by hostd. Only the first report
// counts, so a resent event after a reconnect is not measured twice.
func (c *Core) markPhase(rec *telemetry.Record, p telemetry.Phase, at time.Time) {
	if _, seen := rec.At(p); seen {
		return
	}
	rec.Mark(p, at)
	if p == telemetry.RunnerOnline {
		c.Metrics.RunnerOnline(context.Background(), rec)
	}
}

// observeJobFinished closes a job's record: fleet metrics, then one row for
// ClickHouse. A duplicate result for the same job is ignored.
func (c *Core) observeJobFinished(r *hostlinkv1.JobResult) {
	rec := c.Record(r.GetJobId())
	if rec == nil {
		return
	}
	c.mu.Lock()
	done := rec.Outcome != ""
	if !done {
		rec.Outcome = outcomeName(r.GetOutcome())
	}
	c.mu.Unlock()
	if done {
		return
	}
	c.Metrics.JobFinished(context.Background(), rec, c.Pricing)
	if c.JobRecords != nil {
		c.JobRecords.Add(rec.Row(c.Pricing))
	}
	c.logBreakdown(rec)
}

// outcomeName maps OUTCOME_INFRA_ERROR to infra_error.
func outcomeName(o hostlinkv1.JobResult_Outcome) string {
	if o == hostlinkv1.JobResult_OUTCOME_UNSPECIFIED {
		return "unknown"
	}
	return strings.ToLower(strings.TrimPrefix(o.String(), "OUTCOME_"))
}

// perf-only: addHostSpans adds the host's and guest's spans from a job's
// result to its record, and marks runner_online and job_started from the
// guest's marks, anchored on the host clock (hostd does not send those two
// phases as events).
func (c *Core) addHostSpans(r *hostlinkv1.JobResult) {
	rec := c.Record(r.GetJobId())
	if rec == nil || len(r.GetSpans()) == 0 {
		return
	}
	spans := make([]telemetry.Span, 0, len(r.GetSpans()))
	for _, s := range r.GetSpans() {
		spans = append(spans, telemetry.Span{
			Component: s.GetComponent(),
			Name:      s.GetName(),
			Start:     s.GetStart().AsTime(),
			Duration:  s.GetDuration().AsDuration(),
			Depth:     int(s.GetDepth()),
			Attrs:     s.GetAttrs(),
		})
		switch s.GetName() {
		case "guest.runner_online":
			c.markPhase(rec, telemetry.RunnerOnline, s.GetStart().AsTime())
		case "guest.job_started":
			c.markPhase(rec, telemetry.JobStarted, s.GetStart().AsTime())
		}
	}
	rec.AddSpans(spans...)
}

// perf-only: logBreakdown writes the job's latency breakdown in one line.
func (c *Core) logBreakdown(rec *telemetry.Record) {
	attrs := append([]any{"job", rec.JobID, "host", rec.HostID, "outcome", rec.Outcome,
		"prebuild_match", rec.PrebuildMatch}, rec.BreakdownLogAttrs(8)...)
	c.log().Info("job breakdown", attrs...)
}
