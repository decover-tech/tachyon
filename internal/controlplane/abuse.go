package controlplane

import (
	"context"
	"time"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/abuse"
	"github.com/decover-tech/tachyon/internal/jobapi"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

// trackAbuse starts anomaly monitoring for a job placed on hostID.
func (c *Core) trackAbuse(ctx context.Context, spec *jobapi.Spec, hostID string) {
	if c.Anomalies == nil {
		return
	}
	var plan abuse.Plan
	if c.Limits != nil {
		plan = c.Limits.PlanOf(ctx, spec.Tenant)
	}
	c.Anomalies.Track(abuse.Job{
		ID:     spec.JobID,
		Tenant: spec.Tenant,
		Repo:   spec.Repo,
		HostID: hostID,
		VCPU:   spec.Hardware.VCPU,
		Plan:   plan,
	})
}

func (c *Core) releaseLimits(jobID string) {
	if c.Limits != nil {
		c.Limits.Release(jobID)
	}
}

// onJobUsage feeds a running job's resource sample to the anomaly monitor
// and cancels the job if an alert calls for it.
func (c *Core) onJobUsage(ctx context.Context, u *hostlinkv1.JobUsage) {
	if c.Anomalies == nil {
		return
	}
	at := c.now()
	if u.GetAt() != nil {
		at = u.GetAt().AsTime()
	}
	alerts := c.Anomalies.Observe(ctx, abuse.Sample{
		JobID:                u.GetJobId(),
		At:                   at,
		Interval:             time.Duration(u.GetIntervalMs()) * time.Millisecond,
		BusyVCPU:             u.GetBusyVcpu(),
		TxBytes:              u.GetTxBytes(),
		RxBytes:              u.GetRxBytes(),
		TxPackets:            u.GetTxPackets(),
		NewConnections:       u.GetNewConnections(),
		DistinctDestinations: u.GetDistinctDestinations(),
	})
	for _, a := range alerts {
		if a.Action.CancelJob {
			c.cancelJob(a.Job.HostID, a.Job.ID)
			return
		}
	}
}

// cancelJob asks the job's host to stop it.
func (c *Core) cancelJob(hostID, jobID string) {
	c.mu.Lock()
	hc := c.hosts[hostID]
	c.mu.Unlock()
	if hc == nil {
		c.log().Warn("cancel: host not connected", "job", jobID, "host", hostID)
		return
	}
	c.log().Warn("cancelling job for abuse", "job", jobID, "host", hostID)
	hc.send(&hostlinkv1.LinkResponse{Msg: &hostlinkv1.LinkResponse_Cancel{Cancel: &hostlinkv1.CancelJob{JobId: jobID}}})
}

// finishAbuse charges a finished job's runner time to its tenant's plan and
// runs the whole-run anomaly checks.
func (c *Core) finishAbuse(r *hostlinkv1.JobResult) {
	if c.Limits == nil && c.Anomalies == nil {
		return
	}
	var ran time.Duration
	if rec := c.Record(r.GetJobId()); rec != nil {
		ran = runTime(rec, c.now())
	}
	if c.Limits != nil {
		c.Limits.Finish(r.GetJobId(), ran)
	}
	if c.Anomalies != nil {
		c.Anomalies.Finish(context.Background(), r.GetJobId(), r.GetAvgBusyVcpu(), ran)
	}
}

// runTime is the job's billable time, or host assignment to now when the
// host did not report the phases billing needs.
func runTime(rec *telemetry.Record, now time.Time) time.Duration {
	if d, ok := rec.Billable(); ok {
		return d
	}
	if t, ok := rec.At(telemetry.HostAssigned); ok {
		return now.Sub(t).Round(time.Second)
	}
	return 0
}
