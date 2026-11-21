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

	mu      sync.Mutex
	core    jobapi.Core
	pending map[int64]*pendingJob // GitHub job ID -> job waiting for a runner
	active  map[string]bool       // runner names of jobs in progress
	repos   map[string]int64      // repos seen -> installation, for orphan sweeps
}

type pendingJob struct {
	installationID int64
	repo           string
	labels         []string
	sub            jobapi.Submission
	runnerID       int64
	runnerName     string
	secretRef      string
	submittedAt    time.Time
	attempts       int
}

var _ jobapi.Frontend = (*Adapter)(nil)

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
	if a.pending == nil {
		a.pending = map[int64]*pendingJob{}
		a.active = map[string]bool{}
		a.repos = map[string]int64{}
	}
	a.mu.Unlock()
	return http.HandlerFunc(a.serveWebhook)
}

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
		// 5xx makes GitHub keep the delivery for redelivery; the reconcile
		// loop also covers jobs we never saw succeed.
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
		a.mu.Unlock()
	case "completed":
		a.mu.Lock()
		if p, ok := a.pending[j.ID]; ok { // cancelled before it started
			delete(a.pending, j.ID)
			a.mu.Unlock()
			a.cleanupRunner(ctx, p)
			return nil
		}
		delete(a.active, j.RunnerName)
		a.mu.Unlock()
	}
	return nil
}

func (a *Adapter) onQueued(ctx context.Context, e workflowJobEvent) error {
	j := e.WorkflowJob
	// Reserve the job ID before any API call, so concurrent redeliveries of
	// the same webhook can't both create a runner.
	a.mu.Lock()
	if _, dup := a.pending[j.ID]; dup {
		a.mu.Unlock()
		return nil
	}
	p := &pendingJob{installationID: e.Installation.ID, repo: e.Repository.FullName, labels: j.Labels}
	a.pending[j.ID] = p
	a.repos[p.repo] = p.installationID
	a.mu.Unlock()
	ok := false
	defer func() {
		if !ok {
			a.mu.Lock()
			if a.pending[j.ID] == p {
				delete(a.pending, j.ID)
			}
			a.mu.Unlock()
		}
	}()

	runsOn, err := ParseLabels(j.Labels)
	if err != nil {
		// A bad label is the customer's config error; retrying won't help.
		a.log().Warn("ignoring job with invalid Tachyon labels", "repo", e.Repository.FullName, "job", j.ID, "err", err)
		return nil
	}
	rc, err := a.API.RunContext(ctx, e.Installation.ID, e.Repository.FullName, j.RunID)
	if err != nil {
		return fmt.Errorf("run context: %w", err)
	}
	if rc.BaseRepo == "" {
		rc.BaseRepo = e.Repository.FullName
	}
	if rc.DefaultBranch == "" {
		rc.DefaultBranch = e.Repository.DefaultBranch
	}

	policy := jobapi.PrebuildLatestTrusted
	if runsOn.Clean {
		policy = jobapi.PrebuildNone
	}
	sub := jobapi.Submission{
		Spec: jobapi.Spec{
			JobID:    fmt.Sprintf("gh_%d", j.ID),
			Tenant:   fmt.Sprintf("gh_inst_%d", e.Installation.ID),
			Repo:     "github.com/" + e.Repository.FullName,
			Commit:   j.HeadSHA,
			Hardware: runsOn.Hardware,
			Image:    runsOn.Image,
			Prebuild: jobapi.PrebuildRef{Repo: e.Repository.FullName, Policy: policy},
			Volumes:  []jobapi.Volume{jobapi.DockerVolume},
			Trust:    DeriveTrust(rc),
		},
		Profile:    fmt.Sprintf("%s/%s/%s", e.Repository.FullName, j.WorkflowName, j.Name),
		Deferrable: runsOn.Deferrable,
		QueuedAt:   a.now(),
	}
	p.sub = sub
	if err := a.dispatch(ctx, p); err != nil {
		return err
	}
	ok = true
	return nil
}

// dispatch mints a fresh JIT runner for p and submits it to the core.
func (a *Adapter) dispatch(ctx context.Context, p *pendingJob) error {
	p.attempts++
	p.runnerName = fmt.Sprintf("tachyon-%s-%d", p.sub.Spec.JobID, p.attempts)
	jit, err := a.API.CreateJITConfig(ctx, p.installationID, p.repo, p.runnerName, p.labels)
	if err != nil {
		return fmt.Errorf("create JIT runner: %w", err)
	}
	ref, err := a.Secrets.Put(ctx, "jit/"+p.runnerName, jit.JITConfig)
	if err != nil {
		_ = a.API.DeleteRunner(ctx, p.installationID, p.repo, jit.RunnerID)
		return fmt.Errorf("store JIT config: %w", err)
	}
	p.runnerID, p.secretRef = jit.RunnerID, ref
	p.sub.Spec.Bootstrap = jobapi.Bootstrap{Kind: jobapi.BootstrapGitHubRunner, JITConfig: ref}
	p.submittedAt = a.now()
	a.mu.Lock()
	core := a.core
	a.mu.Unlock()
	if err := core.Submit(ctx, p.sub); err != nil {
		a.cleanupRunner(ctx, p)
		return fmt.Errorf("submit: %w", err)
	}
	return nil
}

func (a *Adapter) cleanupRunner(ctx context.Context, p *pendingJob) {
	if err := a.API.DeleteRunner(ctx, p.installationID, p.repo, p.runnerID); err != nil {
		a.log().Warn("delete runner", "runner", p.runnerName, "err", err)
	}
	if p.secretRef != "" {
		_ = a.Secrets.Delete(ctx, p.secretRef)
	}
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

// Reconcile retries jobs still queued after ReconcileAfter and removes
// orphaned Tachyon runner registrations. Run it periodically.
func (a *Adapter) Reconcile(ctx context.Context) {
	timeout := a.ReconcileAfter
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	maxAttempts := a.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 3
	}
	now := a.now()

	a.mu.Lock()
	var stale []*pendingJob
	var staleIDs []int64
	repos := map[string]int64{}
	for r, inst := range a.repos {
		repos[r] = inst
	}
	known := map[string]bool{}
	for id, p := range a.pending {
		known[p.runnerName] = true
		if now.Sub(p.submittedAt) >= timeout {
			stale = append(stale, p)
			staleIDs = append(staleIDs, id)
		}
	}
	for name := range a.active {
		known[name] = true
	}
	a.mu.Unlock()

	for i, p := range stale {
		a.cleanupRunner(ctx, p)
		delete(known, p.runnerName)
		if p.attempts >= maxAttempts {
			a.log().Error("job not picked up, giving up", "job", p.sub.Spec.JobID, "attempts", p.attempts)
			a.mu.Lock()
			delete(a.pending, staleIDs[i])
			a.mu.Unlock()
			continue
		}
		if err := a.dispatch(ctx, p); err != nil {
			a.log().Warn("re-dispatch failed", "job", p.sub.Spec.JobID, "err", err)
		}
		known[p.runnerName] = true
	}

	for repo, inst := range repos {
		runners, err := a.API.ListRunners(ctx, inst, repo)
		if err != nil {
			a.log().Warn("list runners", "repo", repo, "err", err)
			continue
		}
		for _, r := range runners {
			if strings.HasPrefix(r.Name, LabelPrefix) && !known[r.Name] && !r.Busy {
				if err := a.API.DeleteRunner(ctx, inst, repo, r.ID); err != nil {
					a.log().Warn("delete orphan runner", "runner", r.Name, "err", err)
				}
			}
		}
	}
}
