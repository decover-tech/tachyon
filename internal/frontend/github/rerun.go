package github

import (
	"context"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

// jobRefTTL is how long the adapter remembers a job waiting for its result.
const jobRefTTL = 24 * time.Hour

// maxRerunTries bounds failed (non-throttled) re-run requests for one job.
const maxRerunTries = 10

// jobRef maps a spec job ID back to the GitHub job, so a result reported by
// a host can be acted on.
type jobRef struct {
	installationID int64
	repo           string
	githubJobID    int64
	runAttempt     int
	seen           time.Time
}

// rerunReq is a re-run the adapter still has to request from GitHub.
type rerunReq struct {
	ref         jobRef
	requestedAt time.Time
	tries       int
}

func (a *Adapter) maxInfraReruns() int {
	if a.MaxInfraReruns > 0 {
		return a.MaxInfraReruns
	}
	return 1
}

func (a *Adapter) rerunWindow() time.Duration {
	if a.RerunWindow > 0 {
		return a.RerunWindow
	}
	return time.Hour
}

// OnResult implements jobapi.ResultHandler. It makes no API calls: a job
// whose runner never picked it up is re-dispatched on the next Reconcile,
// and an opted-in re-run of an infrastructure failure is requested there
// too (GitHub only accepts it once the workflow run has completed).
func (a *Adapter) OnResult(_ context.Context, r jobapi.Result) {
	a.mu.Lock()
	a.initLocked()
	ref, ok := a.jobs[r.JobID]
	if !ok {
		a.mu.Unlock()
		return // not ours, or already forgotten
	}
	if r.Outcome == jobapi.OutcomeInfra {
		if p, queued := a.pending[ref.githubJobID]; queued {
			// GitHub never handed the job to the runner, so it is still
			// queued there: dispatch a fresh runner now instead of waiting
			// for ReconcileAfter.
			if !p.busy {
				p.submittedAt = time.Time{}
			}
			a.mu.Unlock()
			return
		}
	}
	delete(a.jobs, r.JobID)
	a.mu.Unlock()
	if r.Outcome != jobapi.OutcomeInfra {
		return
	}

	log := a.log().With("job", r.JobID, "repo", ref.repo, "installation", ref.installationID, "err", r.Error)
	if a.RerunInfra == nil || !a.RerunInfra(ref.installationID, ref.repo) {
		log.Info("job failed for infrastructure reasons; installation has not opted in to re-runs")
		return
	}
	if attempt := max(ref.runAttempt, 1); attempt > a.maxInfraReruns() {
		log.Warn("job failed for infrastructure reasons; not re-running, run attempt limit reached", "run_attempt", attempt)
		return
	}
	a.mu.Lock()
	if _, dup := a.reruns[r.JobID]; !dup {
		a.reruns[r.JobID] = &rerunReq{ref: *ref, requestedAt: a.now()}
	}
	a.mu.Unlock()
	log.Info("job failed for infrastructure reasons; re-run scheduled")
}

// requestReruns asks GitHub to re-run jobs recorded by OnResult. Throttled
// requests wait for the next pass; other failures (typically "run not yet
// completed") are retried until RerunWindow or maxRerunTries runs out.
func (a *Adapter) requestReruns(ctx context.Context) {
	b := a.budget()
	a.mu.Lock()
	ids := make([]string, 0, len(a.reruns))
	reqs := make([]*rerunReq, 0, len(a.reruns))
	for id, rr := range a.reruns {
		ids = append(ids, id)
		reqs = append(reqs, rr)
	}
	a.mu.Unlock()

	for i, rr := range reqs {
		now := a.now()
		ref := rr.ref
		if !b.Ready(ref.installationID, now) {
			continue
		}
		err := a.call(ref.installationID, func() error {
			return a.API.RerunJob(ctx, ref.installationID, ref.repo, ref.githubJobID)
		})
		done := err == nil
		switch {
		case err == nil:
			a.log().Info("re-ran job after infrastructure failure", "job", ids[i], "repo", ref.repo)
		case IsThrottled(err):
		default:
			rr.tries++
			if rr.tries >= maxRerunTries || now.Sub(rr.requestedAt) > a.rerunWindow() {
				a.log().Error("giving up re-running job", "job", ids[i], "repo", ref.repo, "tries", rr.tries, "err", err)
				done = true
			}
		}
		if done {
			a.mu.Lock()
			delete(a.reruns, ids[i])
			a.mu.Unlock()
		}
	}
}
