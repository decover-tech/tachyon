package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
	"github.com/decover-tech/tachyon/internal/prebuild"
)

// NightlyAPI is what the nightly clean run needs from GitHub beyond API. It
// needs two App permissions beyond the read-only set: actions:write (re-run
// a workflow run) and checks:write (report the result on the commit).
type NightlyAPI interface {
	// RerunWorkflow re-runs every job of a workflow run as a new attempt.
	RerunWorkflow(ctx context.Context, installationID int64, repo string, runID int64) error
	CreateCheckRun(ctx context.Context, installationID int64, repo string, cr CheckRun) error
}

// CheckRun is a completed check run reported on a commit.
type CheckRun struct {
	HeadSHA    string
	Name       string
	Conclusion string // success | neutral | failure
	Title      string
	Summary    string // Markdown
}

// CheckRunName is the check the nightly clean run reports under.
const CheckRunName = "Tachyon nightly clean run"

// DefaultNightlyExclude skips workflows whose names suggest side effects: a
// re-run repeats every job of the run, including ones that ship things. It
// is a backstop only; a name says nothing about the jobs inside, which is
// why repos must opt in (Nightly.Repos).
var DefaultNightlyExclude = regexp.MustCompile(`(?i)deploy|release|publish|rollout|promote`).MatchString

// Nightly runs one clean run per repo per night. It picks the repo's latest
// finished default-branch run whose jobs forked a prebuild, re-runs it as a
// new attempt with every Tachyon job forced onto the base image, and once
// the clean attempt finishes compares the two job by job. The result is
// reported to the customer as a check run on the commit.
//
// Re-running the same commit keeps the comparison honest: the only
// difference between the two attempts is the prebuild.
//
// A re-run repeats every job of the workflow run, Tachyon or not, including
// any deploy job. So it is opt-in per repo (Repos), and only a run for the
// newest commit seen on the default branch is ever re-run: re-running an
// older commit's run could ship that commit over a newer one.
type Nightly struct {
	API NightlyAPI
	// Repos reports whether a repo (owner/name) opted in to the nightly
	// clean run. nil means no repo did, and nothing is ever re-run.
	Repos func(repo string) bool
	// Hour (UTC, 0-23) after which each repo's clean run may start.
	Hour int
	// PrebuildMatch returns how a job (job API ID) used its prebuild. Jobs
	// that did not fork one are not compared. Optional.
	PrebuildMatch func(jobID string) prebuild.Match
	// Exclude skips workflows by name; nil means DefaultNightlyExclude.
	Exclude func(workflow string) bool
	// Settle is how long a run must be quiet before it counts as finished,
	// so jobs queued later by `needs:` are not missed. Default 15m.
	Settle time.Duration
	// Timeout bounds a clean attempt; whatever finished is compared then.
	// Default 6h.
	Timeout time.Duration

	mu    sync.Mutex
	repos map[string]*nightlyRepo
	jobs  map[int64]*nightlyJob // GitHub job ID -> tracked job
}

// maxTrackedRuns bounds the recent runs kept per repo as candidates.
const maxTrackedRuns = 8

// maxReports bounds the reports kept per repo.
const maxReports = 7

type nightlyRepo struct {
	inst    int64
	last    time.Time // last clean run started
	runs    map[string]*nightlyRun
	clean   *nightlyRun // in-flight clean attempt
	from    *nightlyRun // the prebuild run it repeats
	reports []prebuild.DriftReport
}

type nightlyRun struct {
	repo     string
	runID    int64
	attempt  int
	commit   string
	workflow string
	clean    bool
	jobs     map[string]*nightlyJob // by job name
	ids      []int64                // GitHub job IDs
	started  time.Time
	activity time.Time
}

type nightlyJob struct {
	run        *nightlyRun
	name       string
	jobID      string
	done       bool
	conclusion prebuild.Conclusion
	match      prebuild.Match
}

func (n *Nightly) settle() time.Duration {
	if n.Settle > 0 {
		return n.Settle
	}
	return 15 * time.Minute
}

func (n *Nightly) timeout() time.Duration {
	if n.Timeout > 0 {
		return n.Timeout
	}
	return 6 * time.Hour
}

func (n *Nightly) excluded(workflow string) bool {
	if n.Exclude != nil {
		return n.Exclude(workflow)
	}
	return DefaultNightlyExclude(workflow)
}

func (n *Nightly) enabled(repo string) bool {
	return n.Repos != nil && n.Repos(repo)
}

func (n *Nightly) repo(name string, inst int64) *nightlyRepo {
	if n.repos == nil {
		n.repos = map[string]*nightlyRepo{}
		n.jobs = map[int64]*nightlyJob{}
	}
	r := n.repos[name]
	if r == nil {
		r = &nightlyRepo{runs: map[string]*nightlyRun{}}
		n.repos[name] = r
	}
	r.inst = inst
	return r
}

// queued tracks a queued Tachyon job and reports whether it belongs to a
// nightly clean attempt and must fork the base image.
func (n *Nightly) queued(e workflowJobEvent, rc RunContext, trust jobapi.Trust, jobID string, now time.Time) bool {
	if n == nil {
		return false
	}
	j := e.WorkflowJob
	attempt := j.RunAttempt
	if attempt == 0 {
		attempt = 1
	}
	if !n.enabled(e.Repository.FullName) {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, dup := n.jobs[j.ID]; dup {
		return n.jobs[j.ID].run.clean
	}
	r := n.repo(e.Repository.FullName, e.Installation.ID)

	var run *nightlyRun
	switch {
	case r.clean != nil && r.clean.runID == j.RunID && (attempt == r.clean.attempt || (r.clean.attempt == 0 && attempt > r.from.attempt)):
		r.clean.attempt = attempt
		run = r.clean
	case trust == jobapi.Trusted && rc.HeadBranch != "" && rc.HeadBranch == rc.DefaultBranch &&
		(rc.Event == "push" || rc.Event == "schedule") && !n.excluded(j.WorkflowName):
		key := fmt.Sprintf("%d/%d", j.RunID, attempt)
		run = r.runs[key]
		if run == nil {
			run = &nightlyRun{repo: e.Repository.FullName, runID: j.RunID, attempt: attempt, commit: j.HeadSHA, workflow: j.WorkflowName, jobs: map[string]*nightlyJob{}, started: now}
			r.runs[key] = run
			n.evict(r)
		}
	default:
		return false
	}
	nj := &nightlyJob{run: run, name: j.Name, jobID: jobID}
	run.jobs[j.Name] = nj
	run.ids = append(run.ids, j.ID)
	run.activity = now
	n.jobs[j.ID] = nj
	return run.clean
}

// evict drops the oldest tracked runs beyond maxTrackedRuns, never the one
// an in-flight clean attempt repeats.
func (n *Nightly) evict(r *nightlyRepo) {
	for len(r.runs) > maxTrackedRuns {
		var oldKey string
		var old *nightlyRun
		for k, run := range r.runs {
			if run == r.from {
				continue
			}
			if old == nil || run.started.Before(old.started) {
				oldKey, old = k, run
			}
		}
		if old == nil {
			return
		}
		n.forget(old)
		delete(r.runs, oldKey)
	}
}

func (n *Nightly) forget(run *nightlyRun) {
	for _, id := range run.ids {
		delete(n.jobs, id)
	}
}

// completed records a tracked job's conclusion.
func (n *Nightly) completed(githubJobID int64, conclusion string, now time.Time) {
	if n == nil {
		return
	}
	n.mu.Lock()
	nj := n.jobs[githubJobID]
	n.mu.Unlock()
	if nj == nil {
		return
	}
	var match prebuild.Match
	if n.PrebuildMatch != nil && !nj.run.clean {
		match = n.PrebuildMatch(nj.jobID)
	}
	n.mu.Lock()
	nj.done = true
	nj.conclusion = prebuild.Conclusion(conclusion)
	nj.match = match
	nj.run.activity = now
	n.mu.Unlock()
}

// finished reports whether every job of run has completed and the run has
// been quiet for the settle time.
func (n *Nightly) finished(run *nightlyRun, now time.Time) bool {
	if len(run.jobs) == 0 || now.Sub(run.activity) < n.settle() {
		return false
	}
	for _, j := range run.jobs {
		if !j.done {
			return false
		}
	}
	return true
}

// usedPrebuild reports whether any job of run forked a prebuild. Unknown
// matches count, so the comparison decides.
func usedPrebuild(run *nightlyRun) bool {
	for _, j := range run.jobs {
		if j.match != prebuild.MatchNone {
			return true
		}
	}
	return false
}

func outcomes(run *nightlyRun) []prebuild.JobOutcome {
	var out []prebuild.JobOutcome
	for _, j := range run.jobs {
		if j.done {
			out = append(out, prebuild.JobOutcome{Name: j.name, Conclusion: j.conclusion, Match: j.match})
		}
	}
	return out
}

type nightlyStart struct {
	repo string
	inst int64
	run  *nightlyRun
}

type nightlyFinish struct {
	repo   string
	inst   int64
	report prebuild.DriftReport
}

// Tick starts due clean runs and finishes completed ones. The adapter calls
// it from Reconcile.
func (n *Nightly) Tick(ctx context.Context, now time.Time, log *slog.Logger) {
	if n == nil {
		return
	}
	var starts []nightlyStart
	var finishes []nightlyFinish
	n.mu.Lock()
	for name, r := range n.repos {
		if r.clean != nil {
			if !n.cleanDone(r, now) {
				continue
			}
			clean, from := r.clean, r.from
			r.clean, r.from = nil, nil
			n.forget(clean)
			if len(clean.jobs) == 0 {
				log.Warn("nightly clean run never started", "repo", name, "run", from.runID)
				continue
			}
			rep := prebuild.CompareClean(name, from.commit, outcomes(from), outcomes(clean))
			r.reports = append(r.reports, rep)
			if len(r.reports) > maxReports {
				r.reports = r.reports[len(r.reports)-maxReports:]
			}
			finishes = append(finishes, nightlyFinish{repo: name, inst: r.inst, report: rep})
			continue
		}
		if !prebuild.NightlyDue(r.last, now, n.Hour) {
			continue
		}
		if c := n.candidate(r, now); c != nil {
			r.last = now
			r.from = c
			r.clean = &nightlyRun{repo: name, runID: c.runID, commit: c.commit, workflow: c.workflow, clean: true, jobs: map[string]*nightlyJob{}, started: now, activity: now}
			starts = append(starts, nightlyStart{repo: name, inst: r.inst, run: c})
		}
	}
	n.mu.Unlock()

	for _, s := range starts {
		err := n.API.RerunWorkflow(ctx, s.inst, s.repo, s.run.runID)
		if err != nil {
			// Tried once tonight; try again tomorrow rather than hammer
			// a repo whose installation lacks actions:write.
			log.Warn("nightly clean run: re-run failed", "repo", s.repo, "run", s.run.runID, "err", err)
			n.mu.Lock()
			if r := n.repos[s.repo]; r != nil && r.from == s.run {
				r.clean, r.from = nil, nil
			}
			n.mu.Unlock()
			continue
		}
		log.Info("nightly clean run started", "repo", s.repo, "run", s.run.runID, "workflow", s.run.workflow, "commit", s.run.commit)
	}
	for _, f := range finishes {
		rep := f.report
		log.Info("nightly clean run finished", "repo", f.repo, "commit", rep.Commit, "compared", len(rep.Compared), "divergences", len(rep.Divergences))
		if err := n.API.CreateCheckRun(ctx, f.inst, f.repo, checkRunFor(rep)); err != nil {
			log.Warn("nightly clean run: report failed", "repo", f.repo, "err", err)
		}
	}
}

// cleanDone: every job of the repeated run has a finished clean
// counterpart, or the attempt timed out.
func (n *Nightly) cleanDone(r *nightlyRepo, now time.Time) bool {
	if now.Sub(r.clean.started) >= n.timeout() {
		return true
	}
	if len(r.clean.jobs) == 0 {
		return false
	}
	for name := range r.from.jobs {
		if j := r.clean.jobs[name]; j == nil || !j.done {
			return false
		}
	}
	for _, j := range r.clean.jobs {
		if !j.done {
			return false
		}
	}
	return true
}

// candidate picks the most recent finished run that used a prebuild, among
// the runs for the newest commit seen on the default branch. It never picks
// an older commit's run: its jobs may deploy, and re-running them would roll
// the newer commit back. While any run for the newest commit is unfinished
// it picks nothing, so the next tick tries again.
func (n *Nightly) candidate(r *nightlyRepo, now time.Time) *nightlyRun {
	var newest *nightlyRun
	for _, run := range r.runs {
		if newest == nil || run.started.After(newest.started) {
			newest = run
		}
	}
	if newest == nil {
		return nil
	}
	var runs []*nightlyRun
	for _, run := range r.runs {
		if run.commit != newest.commit {
			continue
		}
		if !n.finished(run, now) {
			return nil
		}
		if usedPrebuild(run) {
			runs = append(runs, run)
		}
	}
	if len(runs) == 0 {
		return nil
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].activity.After(runs[j].activity) })
	return runs[0]
}

func checkRunFor(rep prebuild.DriftReport) CheckRun {
	conclusion := "success"
	switch {
	case rep.Has(prebuild.CleanFailed):
		conclusion = "failure"
	case len(rep.Divergences) > 0 || len(rep.Compared) == 0:
		conclusion = "neutral"
	}
	return CheckRun{HeadSHA: rep.Commit, Name: CheckRunName, Conclusion: conclusion, Title: rep.Title(), Summary: rep.Summary()}
}

// Reports returns the recent nightly drift reports for repo (owner/name),
// oldest first.
func (n *Nightly) Reports(repo string) []prebuild.DriftReport {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if r := n.repos[repo]; r != nil {
		return append([]prebuild.DriftReport(nil), r.reports...)
	}
	return nil
}

func (c *Client) RerunWorkflow(ctx context.Context, inst int64, repo string, runID int64) error {
	return c.asInstallation(ctx, inst, http.MethodPost, fmt.Sprintf("/repos/%s/actions/runs/%d/rerun", repo, runID), map[string]any{}, nil)
}

func (c *Client) CreateCheckRun(ctx context.Context, inst int64, repo string, cr CheckRun) error {
	body := map[string]any{
		"name":       cr.Name,
		"head_sha":   cr.HeadSHA,
		"status":     "completed",
		"conclusion": cr.Conclusion,
		"output": map[string]any{
			"title":   cr.Title,
			"summary": cr.Summary,
		},
	}
	return c.asInstallation(ctx, inst, http.MethodPost, fmt.Sprintf("/repos/%s/check-runs", repo), body, nil)
}

var _ NightlyAPI = (*Client)(nil)
