// Package github is the GitHub Actions frontend: it turns workflow_job,
// push and pull_request webhooks into job API submissions and hints.
package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
	"github.com/decover-tech/tachyon/internal/store"
)

const maxWebhookBytes = 5 << 20

// Adapter implements jobapi.Frontend for GitHub Actions.
type Adapter struct {
	WebhookSecret []byte
	API           API
	Secrets       Secrets
	Log           *slog.Logger
	Now           func() time.Time

	// ReconcileAfter is how long a job may stay queued before it is retried.
	ReconcileAfter time.Duration
	// MaxAttempts bounds retries of one job.
	MaxAttempts int
	// Store, if set, persists pending and active runners so a restart
	// keeps retrying queued jobs and doesn't sweep live runners
	// (see persist.go).
	Store store.Store
	// Nightly, when set, runs one clean run per repo per night and reports
	// divergence from the prebuild runs. Optional.
	Nightly *Nightly
	// AllowedOwners, when non-empty, limits work to repositories owned by
	// these GitHub accounts (org or user logins, case-insensitive). A public
	// App can be installed by anyone; during the pilot only listed orgs get
	// runners. Events from other accounts are acknowledged and ignored.
	AllowedOwners []string

	// Budget limits GitHub API calls per installation; nil uses a Budget
	// with default limits.
	Budget *Budget

	// RerunInfra opts an installation's repo in to re-running jobs whose
	// result was jobapi.OutcomeInfra. Nil (the default) never re-runs. It
	// is called without the adapter's lock held.
	RerunInfra func(installationID int64, repo string) bool
	// MaxInfraReruns bounds automatic re-runs: a job is re-run only while
	// its workflow run attempt is at most this. Default 1.
	MaxInfraReruns int
	// RerunWindow is how long to keep retrying a re-run request (GitHub
	// accepts it only once the workflow run completes). Default 1h.
	RerunWindow time.Duration

	mu      sync.Mutex
	core    jobapi.Core
	pending map[int64]*pendingJob // GitHub job ID -> job waiting for a runner
	active  map[string]bool       // runner names of jobs in progress
	repos   map[string]int64      // repos seen -> installation, for orphan sweeps
	jobs    map[string]*jobRef    // spec job ID -> GitHub job, until its result arrives
	reruns  map[string]*rerunReq  // spec job ID -> infra re-run to request
}

type pendingJob struct {
	id             int64 // GitHub job ID
	installationID int64
	repo           string
	labels         []string
	event          workflowJobEvent
	runsOn         RunsOn
	prepared       bool // sub is built
	dispatched     bool // a runner exists and sub was submitted
	busy           bool // a webhook or reconcile pass is working on it
	sub            jobapi.Submission
	runnerID       int64
	runnerName     string
	secretRef      string
	submittedAt    time.Time
	attempts       int
	robbed         bool // another job took its runner
}

var (
	_ jobapi.Frontend      = (*Adapter)(nil)
	_ jobapi.ResultHandler = (*Adapter)(nil)
)

func (a *Adapter) Name() string { return "github" }

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Adapter) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// Handler serves GitHub App webhooks.
func (a *Adapter) Handler(core jobapi.Core) http.Handler {
	a.mu.Lock()
	a.core = core
	a.initLocked()
	a.mu.Unlock()
	return http.HandlerFunc(a.serveWebhook)
}

// initLocked allocates the adapter's state. a.mu must be held.
func (a *Adapter) initLocked() {
	if a.pending == nil {
		a.pending = map[int64]*pendingJob{}
		a.active = map[string]bool{}
		a.repos = map[string]int64{}
		a.jobs = map[string]*jobRef{}
		a.reruns = map[string]*rerunReq{}
	}
	if a.Budget == nil {
		a.Budget = &Budget{}
	}
}

func (a *Adapter) budget() *Budget {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.initLocked()
	return a.Budget
}

// call makes one GitHub API call for inst within its budget.
func (a *Adapter) call(inst int64, fn func() error) error {
	b := a.budget()
	if err := b.Take(inst, a.now()); err != nil {
		return err
	}
	err := fn()
	b.Observe(inst, a.now(), err)
	return err
}

func specJobID(githubJobID int64) string { return fmt.Sprintf("gh_%d", githubJobID) }

func (a *Adapter) serveWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if !VerifySignature(a.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if err := a.handleEvent(r.Context(), event, body); err != nil {
		a.log().Warn("webhook failed", "event", event, "delivery", r.Header.Get("X-GitHub-Delivery"), "err", err)
		// 5xx marks the delivery failed in the App's log. GitHub does not
		// redeliver it on its own, so retriable failures must stay pending
		// for Reconcile instead of returning here.
		http.Error(w, "processing failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// VerifySignature checks GitHub's X-Hub-Signature-256 header.
func VerifySignature(secret, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	got, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	gotMAC, err := hex.DecodeString(got)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(gotMAC, mac.Sum(nil))
}

type repository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

type installation struct {
	ID int64 `json:"id"`
}

type workflowJobEvent struct {
	Action      string `json:"action"`
	WorkflowJob struct {
		ID           int64    `json:"id"`
		RunID        int64    `json:"run_id"`
		RunAttempt   int      `json:"run_attempt"`
		Conclusion   string   `json:"conclusion"`
		WorkflowName string   `json:"workflow_name"`
		Name         string   `json:"name"`
		HeadSHA      string   `json:"head_sha"`
		HeadBranch   string   `json:"head_branch"`
		Labels       []string `json:"labels"`
		RunnerName   string   `json:"runner_name"`
	} `json:"workflow_job"`
	Repository   repository   `json:"repository"`
	Installation installation `json:"installation"`
}

type pushEvent struct {
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
	Commits []struct {
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Removed  []string `json:"removed"`
	} `json:"commits"`
	Repository   repository   `json:"repository"`
	Installation installation `json:"installation"`
}

type pullRequestEvent struct {
	Action      string `json:"action"`
	PullRequest struct {
		Head struct {
			SHA  string     `json:"sha"`
			Ref  string     `json:"ref"`
			Repo repository `json:"repo"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository   repository   `json:"repository"`
	Installation installation `json:"installation"`
}

func (a *Adapter) handleEvent(ctx context.Context, event string, body []byte) error {
	if !a.ownerAllowed(event, body) {
		return nil
	}
	switch event {
	case "workflow_job":
		var e workflowJobEvent
		if err := json.Unmarshal(body, &e); err != nil {
			return fmt.Errorf("decode workflow_job: %w", err)
		}
		return a.onWorkflowJob(ctx, e)
	case "push":
		var e pushEvent
		if err := json.Unmarshal(body, &e); err != nil {
			return fmt.Errorf("decode push: %w", err)
		}
		return a.onPush(ctx, e)
	case "pull_request":
		var e pullRequestEvent
		if err := json.Unmarshal(body, &e); err != nil {
			return fmt.Errorf("decode pull_request: %w", err)
		}
		return a.onPullRequest(ctx, e)
	default:
		return nil // ping, installation, ... ignored
	}
}

// ownerAllowed reports whether an event's repository belongs to an account in
// AllowedOwners. Events without a repository (ping, installation) pass; they
// start no work.
func (a *Adapter) ownerAllowed(event string, body []byte) bool {
	if len(a.AllowedOwners) == 0 {
		return true
	}
	var env struct {
		Repository *repository `json:"repository"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Repository == nil {
		return true // malformed bodies fail in the typed decode below
	}
	owner, _, _ := strings.Cut(env.Repository.FullName, "/")
	for _, o := range a.AllowedOwners {
		if strings.EqualFold(o, owner) {
			return true
		}
	}
	a.log().Info("ignoring event from account not in the pilot allowlist", "event", event, "repo", env.Repository.FullName)
	return false
}

func (a *Adapter) onWorkflowJob(ctx context.Context, e workflowJobEvent) error {
	j := e.WorkflowJob
	if !IsTachyonJob(j.Labels) {
		return nil
	}
	switch e.Action {
	case "queued":
		return a.onQueued(ctx, e)
	case "in_progress":
		a.mu.Lock()
		delete(a.pending, j.ID)
		a.active[j.RunnerName] = true
		// GitHub gives a job to any idle runner whose labels match, so this
		// may be another pending job's runner. That job has lost it:
		// re-dispatch it on the next reconcile, and leave the runner alone.
		for _, y := range a.pending {
			if y.runnerName == j.RunnerName && y.dispatched && !y.busy {
				y.runnerID, y.submittedAt, y.robbed = 0, time.Time{}, true
			}
		}
		a.mu.Unlock()
		a.forgetPending(ctx, j.ID)
		a.saveActive(ctx, j.RunnerName, e.Installation.ID, e.Repository.FullName)
	case "completed":
		a.Nightly.completed(j.ID, j.Conclusion, a.now())
		a.mu.Lock()
		if p, ok := a.pending[j.ID]; ok { // cancelled before it started
			delete(a.pending, j.ID)
			delete(a.jobs, specJobID(j.ID))
			busy := p.busy // if so, whoever holds it cleans up
			a.mu.Unlock()
			a.forgetPending(ctx, j.ID)
			if !busy {
				a.cleanupRunner(ctx, p)
			}
			return nil
		}
		delete(a.active, j.RunnerName)
		a.mu.Unlock()
		a.forgetActive(ctx, j.RunnerName)
	}
	return nil
}

func (a *Adapter) onQueued(ctx context.Context, e workflowJobEvent) error {
	j := e.WorkflowJob
	runsOn, err := ParseLabels(j.Labels)
	if err != nil {
		// A bad label is the customer's config error; retrying won't help.
		a.log().Warn("ignoring job with invalid Tachyon labels", "repo", e.Repository.FullName, "job", j.ID, "err", err)
		return nil
	}
	// Reserve the job ID before any API call, so concurrent redeliveries of
	// the same webhook can't both create a runner.
	a.mu.Lock()
	if _, dup := a.pending[j.ID]; dup {
		a.mu.Unlock()
		return nil
	}
	p := &pendingJob{
		id:             j.ID,
		installationID: e.Installation.ID,
		repo:           e.Repository.FullName,
		labels:         j.Labels,
		event:          e,
		runsOn:         runsOn,
		busy:           true,
	}
	p.sub.QueuedAt = a.now()
	a.pending[j.ID] = p
	a.repos[p.repo] = p.installationID
	a.jobs[specJobID(j.ID)] = &jobRef{
		installationID: p.installationID,
		repo:           p.repo,
		githubJobID:    j.ID,
		runAttempt:     j.RunAttempt,
		seen:           a.now(),
	}
	a.mu.Unlock()

	err = a.prepare(ctx, p)
	if err == nil {
		err = a.dispatch(ctx, p)
	}
	switch {
	case err == nil:
	case IsThrottled(err) || IsTransient(err):
		// The job stays pending; Reconcile dispatches it once the
		// installation's budget recovers or GitHub answers again. GitHub
		// does not redeliver failed webhooks, so dropping it would leave
		// the job queued forever.
		a.log().Warn("GitHub API unavailable; job left for reconcile", "repo", p.repo, "job", j.ID, "throttled", IsThrottled(err), "err", err)
		err = nil
	default:
		a.mu.Lock()
		mine := a.pending[j.ID] == p
		if mine {
			delete(a.pending, j.ID)
			delete(a.jobs, specJobID(j.ID))
		}
		a.mu.Unlock()
		if mine {
			a.forgetPending(ctx, j.ID)
		}
		return err
	}
	a.release(ctx, j.ID, p)
	return nil
}

// release ends a webhook's or reconcile pass's work on p. If the job left
// pending meanwhile (it started or was cancelled), the runner p may have
// just created is no longer wanted.
func (a *Adapter) release(ctx context.Context, id int64, p *pendingJob) {
	a.mu.Lock()
	p.busy = false
	gone := a.pending[id] != p
	a.mu.Unlock()
	if gone && p.dispatched && !a.isActive(p.runnerName) {
		a.cleanupRunner(ctx, p)
	}
}

func (a *Adapter) isActive(runner string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active[runner]
}

// prepare looks up the job's run and builds its submission.
func (a *Adapter) prepare(ctx context.Context, p *pendingJob) error {
	if p.prepared {
		return nil
	}
	e, runsOn := p.event, p.runsOn
	j := e.WorkflowJob
	var rc RunContext
	err := a.call(p.installationID, func() (err error) {
		rc, err = a.API.RunContext(ctx, e.Installation.ID, e.Repository.FullName, j.RunID)
		return err
	})
	if err != nil {
		return fmt.Errorf("run context: %w", err)
	}
	if rc.BaseRepo == "" {
		rc.BaseRepo = e.Repository.FullName
	}
	if rc.DefaultBranch == "" {
		rc.DefaultBranch = e.Repository.DefaultBranch
	}

	trust := DeriveTrust(rc)
	jobID := specJobID(j.ID)
	policy := jobapi.PrebuildLatestTrusted
	if runsOn.Clean || a.Nightly.queued(e, rc, trust, jobID, a.now()) {
		policy = jobapi.PrebuildNone
	}
	ref, baseRef, defaultRef := rc.CacheRefs()
	sub := jobapi.Submission{
		Spec: jobapi.Spec{
			JobID:    jobID,
			Tenant:   fmt.Sprintf("gh_inst_%d", e.Installation.ID),
			Repo:     "github.com/" + e.Repository.FullName,
			Commit:   j.HeadSHA,
			Hardware: runsOn.Hardware,
			Image:    runsOn.Image,
			Prebuild: jobapi.PrebuildRef{Repo: e.Repository.FullName, Policy: policy},
			Volumes:  []jobapi.Volume{jobapi.DockerVolume},
			Trust:    trust,

			Ref:           ref,
			BaseRef:       baseRef,
			DefaultBranch: defaultRef,
		},
		Profile:    fmt.Sprintf("%s/%s/%s", e.Repository.FullName, j.WorkflowName, j.Name),
		Deferrable: runsOn.Deferrable,
		QueuedAt:   p.sub.QueuedAt,
	}
	p.sub = sub
	p.prepared = true
	return nil
}

// dispatch mints a fresh JIT runner for p and submits it to the core. A
// throttled attempt does not count towards MaxAttempts.
func (a *Adapter) dispatch(ctx context.Context, p *pendingJob) error {
	attempt := p.attempts + 1
	base := specJobID(p.id)
	name := fmt.Sprintf("tachyon-%s-%d", base, attempt)
	if p.robbed {
		// The core ignores a job ID it has placed, and the robbed VM is
		// still running the other job, so the retry needs a new ID. Its
		// result maps back to this GitHub job even if the robbed VM's
		// result already removed the original mapping.
		p.robbed = false
		p.sub.Spec.JobID = fmt.Sprintf("%s-%d", base, attempt)
		a.mu.Lock()
		a.initLocked()
		ref := a.jobs[base]
		if ref == nil {
			ref = &jobRef{
				installationID: p.installationID,
				repo:           p.repo,
				githubJobID:    p.id,
				runAttempt:     p.event.WorkflowJob.RunAttempt,
			}
		}
		ref.seen = a.now()
		a.jobs[p.sub.Spec.JobID] = ref
		a.mu.Unlock()
	}
	var jit JITRunner
	err := a.call(p.installationID, func() (err error) {
		jit, err = a.API.CreateJITConfig(ctx, p.installationID, p.repo, name, p.labels)
		return err
	})
	if err != nil {
		if !IsThrottled(err) {
			p.attempts = attempt
		}
		return fmt.Errorf("create JIT runner: %w", err)
	}
	p.attempts, p.runnerName = attempt, name
	ref, err := a.Secrets.Put(ctx, "jit/"+p.runnerName, jit.JITConfig)
	if err != nil {
		_ = a.call(p.installationID, func() error {
			return a.API.DeleteRunner(ctx, p.installationID, p.repo, jit.RunnerID)
		})
		return fmt.Errorf("store JIT config: %w", err)
	}
	p.runnerID, p.secretRef = jit.RunnerID, ref
	p.sub.Spec.Bootstrap = jobapi.Bootstrap{Kind: jobapi.BootstrapGitHubRunner, JITConfig: ref}
	p.submittedAt = a.now()
	// Recorded before submitting, so a restart can't orphan a runner the
	// core already knows about.
	if err := a.savePending(ctx, p); err != nil {
		a.cleanupRunner(ctx, p)
		return fmt.Errorf("persist pending runner: %w", err)
	}
	a.mu.Lock()
	core := a.core
	a.mu.Unlock()
	if err := core.Submit(ctx, p.sub); err != nil {
		a.cleanupRunner(ctx, p)
		return fmt.Errorf("submit: %w", err)
	}
	p.dispatched = true
	return nil
}

// cleanupRunner removes p's runner registration and JIT secret. If the
// budget prevents the delete, the orphan sweep removes the runner later.
func (a *Adapter) cleanupRunner(ctx context.Context, p *pendingJob) {
	if p.runnerID != 0 {
		err := a.call(p.installationID, func() error {
			return a.API.DeleteRunner(ctx, p.installationID, p.repo, p.runnerID)
		})
		if err != nil {
			a.log().Warn("delete runner", "runner", p.runnerName, "err", err)
		}
	}
	if p.secretRef != "" {
		_ = a.Secrets.Delete(ctx, p.secretRef)
	}
	p.runnerID, p.secretRef, p.dispatched = 0, "", false
}

func (a *Adapter) onPush(ctx context.Context, e pushEvent) error {
	if e.Deleted || !strings.HasPrefix(e.Ref, "refs/heads/") {
		return nil
	}
	branch := strings.TrimPrefix(e.Ref, "refs/heads/")
	var changed []string
	for _, c := range e.Commits {
		changed = append(changed, c.Added...)
		changed = append(changed, c.Modified...)
		changed = append(changed, c.Removed...)
	}
	// Protected-branch status is resolved when the job arrives; for hints
	// only the default branch is assumed trusted.
	trust := jobapi.Untrusted
	if branch == e.Repository.DefaultBranch {
		trust = jobapi.Trusted
	}
	return a.hint(ctx, jobapi.Hint{
		Tenant:       fmt.Sprintf("gh_inst_%d", e.Installation.ID),
		Repo:         "github.com/" + e.Repository.FullName,
		Commit:       e.After,
		Ref:          e.Ref,
		Event:        "push",
		ChangedFiles: changed,
		Trust:        trust,
		ReceivedAt:   a.now(),
	})
}

func (a *Adapter) onPullRequest(ctx context.Context, e pullRequestEvent) error {
	switch e.Action {
	case "opened", "synchronize", "reopened":
	default:
		return nil
	}
	trust := jobapi.Untrusted
	if e.PullRequest.Head.Repo.FullName != e.Repository.FullName {
		trust = jobapi.UntrustedFork
	}
	return a.hint(ctx, jobapi.Hint{
		Tenant:     fmt.Sprintf("gh_inst_%d", e.Installation.ID),
		Repo:       "github.com/" + e.Repository.FullName,
		Commit:     e.PullRequest.Head.SHA,
		Ref:        "refs/heads/" + e.PullRequest.Head.Ref,
		Event:      "pull_request",
		Trust:      trust,
		ReceivedAt: a.now(),
	})
}

func (a *Adapter) hint(ctx context.Context, h jobapi.Hint) error {
	a.mu.Lock()
	core := a.core
	a.mu.Unlock()
	return core.Hint(ctx, h)
}

// Reconcile retries jobs still queued after ReconcileAfter, dispatches jobs
// that were left pending while their installation was throttled, requests
// opted-in re-runs of infrastructure failures, and removes orphaned Tachyon
// runner registrations. Installations whose budget is spent or backing off
// are skipped until they recover. Run it periodically.
func (a *Adapter) Reconcile(ctx context.Context) {
	timeout := a.ReconcileAfter
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	maxAttempts := a.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 3
	}
	b := a.budget()
	now := a.now()

	type work struct {
		id int64
		p  *pendingJob
	}
	a.mu.Lock()
	a.initLocked()
	var stale []work
	repos := map[string]int64{}
	for r, inst := range a.repos {
		repos[r] = inst
	}
	known := map[string]bool{}
	for id, p := range a.pending {
		known[p.runnerName] = true
		if p.busy || (p.dispatched && now.Sub(p.submittedAt) < timeout) {
			continue
		}
		if !b.Ready(p.installationID, now) {
			continue // reconciled once the installation recovers
		}
		p.busy = true
		stale = append(stale, work{id, p})
	}
	for name := range a.active {
		known[name] = true
	}
	for id, j := range a.jobs {
		if now.Sub(j.seen) > jobRefTTL {
			delete(a.jobs, id)
		}
	}
	a.mu.Unlock()

	for _, w := range stale {
		p := w.p
		if p.dispatched {
			a.cleanupRunner(ctx, p)
			delete(known, p.runnerName)
		}
		if p.attempts >= maxAttempts {
			a.log().Error("job not picked up, giving up", "job", specJobID(w.id), "attempts", p.attempts)
			a.mu.Lock()
			mine := a.pending[w.id] == p
			if mine {
				delete(a.pending, w.id)
				delete(a.jobs, specJobID(w.id))
			}
			a.mu.Unlock()
			if mine {
				a.forgetPending(ctx, w.id)
			}
			continue
		}
		err := a.prepare(ctx, p)
		if err == nil {
			err = a.dispatch(ctx, p)
		}
		if err != nil {
			a.log().Warn("re-dispatch failed", "job", specJobID(w.id), "throttled", IsThrottled(err), "err", err)
		}
		if p.dispatched {
			known[p.runnerName] = true
		}
		a.release(ctx, w.id, p)
	}

	a.requestReruns(ctx)
	a.Nightly.Tick(ctx, now, a.log())

	for repo, inst := range repos {
		if !b.Ready(inst, a.now()) {
			continue
		}
		var runners []Runner
		err := a.call(inst, func() (err error) {
			runners, err = a.API.ListRunners(ctx, inst, repo)
			return err
		})
		if err != nil {
			a.log().Warn("list runners", "repo", repo, "err", err)
			continue
		}
		for _, r := range runners {
			if strings.HasPrefix(r.Name, LabelPrefix) && !known[r.Name] && !r.Busy {
				err := a.call(inst, func() error { return a.API.DeleteRunner(ctx, inst, repo, r.ID) })
				if err != nil {
					a.log().Warn("delete orphan runner", "runner", r.Name, "err", err)
					if IsThrottled(err) {
						break
					}
				}
			}
		}
	}
}
