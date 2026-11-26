package controlplane

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
	"github.com/decover-tech/tachyon/internal/prebuild"
	"github.com/decover-tech/tachyon/internal/scheduler"
	"github.com/decover-tech/tachyon/internal/store"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

// Persistence. With Core.Store set, the core writes jobs, hosts and job
// records through to the store as they change, and Restore reloads them
// after a restart: queued and deferred jobs are resubmitted, and jobs still
// running on hosts keep their capacity reservations, so the hosts' results
// release them as usual when they reconnect. With Store nil, state lives
// only in memory.

// saveJob records a job's state. Without a store it is a no-op.
func (c *Core) saveJob(ctx context.Context, sub jobapi.Submission, state store.JobState, a *scheduler.Assignment) error {
	if c.Store == nil {
		return nil
	}
	j := store.Job{ID: sub.Spec.JobID, State: state, Submission: sub, UpdatedAt: c.now()}
	if a != nil {
		j.HostID, j.CPU, j.MemGB = a.HostID, a.CPU, a.MemGB
	}
	return c.Store.PutJob(ctx, j)
}

// noteJob is saveJob for paths where the job is already in flight: a failed
// write is logged, not returned.
func (c *Core) noteJob(ctx context.Context, sub jobapi.Submission, state store.JobState, a *scheduler.Assignment) {
	if err := c.saveJob(ctx, sub, state, a); err != nil {
		c.log().Warn("persist job", "job", sub.Spec.JobID, "state", state, "err", err)
	}
}

// finishJob marks a job finished.
func (c *Core) finishJob(ctx context.Context, jobID string) {
	if c.Store == nil {
		return
	}
	if err := c.Store.SetJobState(ctx, jobID, store.JobFinished); err != nil {
		c.log().Warn("persist job", "job", jobID, "state", store.JobFinished, "err", err)
	}
}

// saveRecord persists a job record.
func (c *Core) saveRecord(ctx context.Context, rec *telemetry.Record) {
	if c.Store == nil || rec == nil {
		return
	}
	if err := c.Store.PutRecord(ctx, rec.Data()); err != nil {
		c.log().Warn("persist record", "job", rec.JobID, "err", err)
	}
}

// saveHost persists a host's shape and ratio when they changed since the
// last write, so steady heartbeats don't write.
func (c *Core) saveHost(ctx context.Context, hc *hostConn, h scheduler.Host) {
	if c.Store == nil {
		return
	}
	ratio := c.Scheduler.Ratio(h.ID)
	c.mu.Lock()
	same := hc.saved != nil && hc.savedRatio == ratio && reflect.DeepEqual(*hc.saved, h)
	c.mu.Unlock()
	if same {
		return
	}
	if err := c.Store.PutHost(ctx, store.Host{Host: h, Ratio: ratio, LastSeen: c.now()}); err != nil {
		c.log().Warn("persist host", "host", h.ID, "err", err)
		return
	}
	c.mu.Lock()
	hc.saved, hc.savedRatio = &h, ratio
	c.mu.Unlock()
}

// RestoreStats says what Restore reloaded.
type RestoreStats struct {
	Hosts    int // known hosts, waiting to reconnect
	Running  int // jobs running on hosts, reservations restored
	Requeued int // queued or deferred jobs, resubmitted by RetryDeferred
}

// Restore reloads persisted state after a restart. Call it once, before
// serving webhooks or hostd streams. Restored hosts are marked draining
// until they reconnect, so no new job is placed on a host with no stream.
func (c *Core) Restore(ctx context.Context) (RestoreStats, error) {
	var st RestoreStats
	if c.Store == nil {
		return st, nil
	}
	hosts, err := c.Store.Hosts(ctx)
	if err != nil {
		return st, fmt.Errorf("restore hosts: %w", err)
	}
	for _, h := range hosts {
		sh := h.Host
		sh.Draining = true
		c.Scheduler.RestoreHost(sh, h.Ratio)
	}
	st.Hosts = len(hosts)

	jobs, err := c.Store.OpenJobs(ctx)
	if err != nil {
		return st, fmt.Errorf("restore jobs: %w", err)
	}
	ids := make([]string, 0, len(jobs))
	var requeue []jobapi.Submission
	for _, j := range jobs {
		ids = append(ids, j.ID)
		switch j.State {
		case store.JobAssigned:
			if err := c.Scheduler.RestoreAllocation(j.Submission, j.HostID, j.CPU, j.MemGB); err != nil {
				c.log().Warn("restore running job", "job", j.ID, "err", err)
				continue
			}
			st.Running++
		case store.JobQueued, store.JobDeferred:
			requeue = append(requeue, j.Submission)
			st.Requeued++
		}
	}
	recs, err := c.Store.RecordsFor(ctx, ids)
	if err != nil {
		return st, fmt.Errorf("restore records: %w", err)
	}

	c.mu.Lock()
	c.init()
	for _, r := range recs {
		c.records[r.JobID] = telemetry.FromData(r)
	}
	c.deferred = append(c.deferred, requeue...)
	c.mu.Unlock()
	return st, nil
}

// StorePrebuilds is the prebuild registry on a store.Store.
type StorePrebuilds struct {
	Store store.Store
	// OnError, if set, is told about store failures; lookups that fail
	// report no prebuild, so jobs fall back to the base image.
	OnError func(op string, err error)
}

var _ Prebuilds = (*StorePrebuilds)(nil)

func (p *StorePrebuilds) fail(op string, err error) {
	if p.OnError != nil && err != nil && !errors.Is(err, store.ErrNotFound) {
		p.OnError(op, err)
	}
}

func (p *StorePrebuilds) Latest(repo string) (*prebuild.Snapshot, *prebuild.Config) {
	ctx := context.Background()
	cfg, err := p.Store.PrebuildConfig(ctx, repo)
	p.fail("prebuild config", err)
	s, err := p.Store.LatestPrebuild(ctx, repo)
	if err != nil {
		p.fail("latest prebuild", err)
		return nil, cfg
	}
	return &s, cfg
}

func (p *StorePrebuilds) Register(s prebuild.Snapshot) {
	p.fail("register prebuild", p.Store.PutPrebuild(context.Background(), s))
}

// SetConfig records the prebuild config for a repo.
func (p *StorePrebuilds) SetConfig(repo string, c *prebuild.Config) error {
	return p.Store.PutPrebuildConfig(context.Background(), repo, c)
}

// ReplayMeter meters the jobs that finished at or after since, from the
// store, so a restart keeps the period's usage. Jobs are replayed in the
// order they finished, which is the order the prebuild allowance was drawn
// down. The meter ignores jobs it already has, so replaying is safe at any
// time. It returns how many records it read.
func (c *Core) ReplayMeter(ctx context.Context, since time.Time) (int, error) {
	if c.Store == nil || c.Meter == nil {
		return 0, nil
	}
	recs, err := c.Store.RecordsFinishedSince(ctx, since)
	if err != nil {
		return 0, fmt.Errorf("replay meter: %w", err)
	}
	sort.SliceStable(recs, func(i, j int) bool {
		return recs[i].Phases[telemetry.JobFinished].Before(recs[j].Phases[telemetry.JobFinished])
	})
	for _, r := range recs {
		if err := c.Meter.RecordJob(telemetry.FromData(r)); err != nil {
			c.log().Warn("job not metered", "job", r.JobID, "err", err)
		}
	}
	return len(recs), nil
}
