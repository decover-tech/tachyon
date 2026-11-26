package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

func TestClientRateLimitHeadersAndRerun(t *testing.T) {
	reset := time.Unix(1_900_000_000, 0)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"token": "inst-tok", "expires_at": time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("POST /repos/acme/api/actions/jobs/{id}/rerun", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "1":
			w.WriteHeader(http.StatusCreated)
		case "2":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset.Unix()))
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		case "3":
			w.Header().Set("Retry-After", "30")
			http.Error(w, `{"message":"secondary"}`, http.StatusTooManyRequests)
		default:
			w.Header().Set("X-RateLimit-Remaining", "4999")
			http.Error(w, `{"message":"run not completed"}`, http.StatusForbidden)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{AppID: 1, PrivateKey: testKey(t), BaseURL: srv.URL, Features: []Feature{FeatureInfraRerun}}
	ctx := context.Background()

	if err := c.RerunJob(ctx, 42, "acme/api", 1); err != nil {
		t.Fatalf("RerunJob: %v", err)
	}
	var ae *APIError
	if err := c.RerunJob(ctx, 42, "acme/api", 2); !errors.As(err, &ae) || !ae.RateLimited() || ae.Remaining != 0 || !ae.Reset.Equal(reset) {
		t.Errorf("primary limit: %v %+v", err, ae)
	}
	if err := c.RerunJob(ctx, 42, "acme/api", 3); !errors.As(err, &ae) || !ae.RateLimited() || ae.RetryAfter != 30*time.Second || ae.Remaining != -1 {
		t.Errorf("secondary limit: %v %+v", err, ae)
	}
	if err := c.RerunJob(ctx, 42, "acme/api", 4); !errors.As(err, &ae) || ae.RateLimited() || IsThrottled(err) {
		t.Errorf("plain 403 treated as a rate limit: %v %+v", err, ae)
	}
}

func rateLimited(after time.Duration) error {
	return &APIError{Status: http.StatusTooManyRequests, Remaining: -1, RetryAfter: after}
}

func TestAdapterThrottledJobIsReconciledAfterRateLimit(t *testing.T) {
	h := newHarness(t)
	h.api.err = rateLimited(30 * time.Second)
	if code := h.post("workflow_job", jobEvent("queued", 1, tachyonLabels, ""), testSecret); code != http.StatusAccepted {
		t.Fatalf("status %d, want 202: a rate-limited job is kept for reconcile", code)
	}
	if len(h.core.subs) != 0 || h.api.calls != 1 {
		t.Fatalf("subs=%d calls=%d", len(h.core.subs), h.api.calls)
	}
	// A second job for the same installation makes no API call while it backs off.
	h.post("workflow_job", jobEvent("queued", 2, tachyonLabels, ""), testSecret)
	h.a.Reconcile(context.Background())
	if h.api.calls != 1 {
		t.Fatalf("API called %d times while throttled", h.api.calls)
	}

	h.api.err = nil
	h.clock = h.clock.Add(31 * time.Second)
	h.a.Reconcile(context.Background())
	if len(h.core.subs) != 2 {
		t.Fatalf("subs = %d after the limit lifted, want 2", len(h.core.subs))
	}
	for _, s := range h.core.subs {
		if s.Spec.Bootstrap.JITConfig == "" || !s.QueuedAt.Equal(h.clock.Add(-31*time.Second)) {
			t.Errorf("submission %s: bootstrap=%+v queuedAt=%v", s.Spec.JobID, s.Spec.Bootstrap, s.QueuedAt)
		}
	}
	if fmt.Sprint(h.api.created) != "[tachyon-gh_1-1 tachyon-gh_2-1]" && fmt.Sprint(h.api.created) != "[tachyon-gh_2-1 tachyon-gh_1-1]" {
		t.Errorf("created = %v: throttled tries must not count as attempts", h.api.created)
	}
	// Dispatched now; nothing more until they go stale.
	h.a.Reconcile(context.Background())
	if len(h.core.subs) != 2 {
		t.Fatalf("re-dispatched fresh jobs: subs=%d", len(h.core.subs))
	}
}

func TestAdapterBudgetSpreadsCalls(t *testing.T) {
	h := newHarness(t)
	h.a.Budget = &Budget{PerHour: 1, Burst: 2}
	h.a.ReconcileAfter = 100 * time.Hour
	h.post("workflow_job", jobEvent("queued", 1, tachyonLabels, ""), testSecret) // RunContext + JIT: 2 calls
	h.post("workflow_job", jobEvent("queued", 2, tachyonLabels, ""), testSecret) // budget spent
	if len(h.core.subs) != 1 || h.api.calls != 2 {
		t.Fatalf("subs=%d calls=%d, want 1 and 2", len(h.core.subs), h.api.calls)
	}
	h.clock = h.clock.Add(time.Hour) // one token: RunContext only
	h.a.Reconcile(context.Background())
	if len(h.core.subs) != 1 || h.api.calls != 3 {
		t.Fatalf("subs=%d calls=%d, want 1 and 3", len(h.core.subs), h.api.calls)
	}
	h.clock = h.clock.Add(time.Hour) // one more: the JIT runner
	h.a.Reconcile(context.Background())
	if len(h.core.subs) != 2 || h.api.calls != 4 {
		t.Fatalf("subs=%d calls=%d, want 2 and 4", len(h.core.subs), h.api.calls)
	}
}

func TestAdapterBacksOffAfterServerErrors(t *testing.T) {
	h := newHarness(t)
	h.a.Budget = &Budget{BackoffBase: 10 * time.Second}
	h.api.err = &APIError{Status: http.StatusBadGateway, Remaining: -1}
	if code := h.post("workflow_job", jobEvent("queued", 1, tachyonLabels, ""), testSecret); code != http.StatusAccepted {
		t.Fatalf("status %d, want 202: GitHub never redelivers, so the job is kept for reconcile", code)
	}
	// Within the backoff the next job is held, not sent to a failing API.
	if code := h.post("workflow_job", jobEvent("queued", 2, tachyonLabels, ""), testSecret); code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", code)
	}
	if h.api.calls != 1 {
		t.Fatalf("calls = %d during backoff", h.api.calls)
	}
	h.api.err = nil
	h.clock = h.clock.Add(10 * time.Second)
	h.a.Reconcile(context.Background())
	if len(h.core.subs) != 2 {
		t.Fatalf("subs = %+v, want both jobs", h.core.subs)
	}
}

func runAttempt(ev map[string]any, n int) map[string]any {
	ev["workflow_job"].(map[string]any)["run_attempt"] = n
	return ev
}

func (h *harness) startJob(id int64, attempt int) {
	h.t.Helper()
	h.post("workflow_job", runAttempt(jobEvent("queued", id, tachyonLabels, ""), attempt), testSecret)
	name := h.api.created[len(h.api.created)-1]
	h.post("workflow_job", jobEvent("in_progress", id, tachyonLabels, name), testSecret)
}

func TestAdapterInfraRerunOptIn(t *testing.T) {
	h := newHarness(t)
	var asked []string
	h.a.RerunInfra = func(inst int64, repo string) bool {
		asked = append(asked, fmt.Sprintf("%d/%s", inst, repo))
		return inst == 42
	}
	h.startJob(6, 1)
	h.startJob(7, 1)
	h.startJob(8, 2) // already a re-run
	ctx := context.Background()
	h.a.OnResult(ctx, jobapi.Result{JobID: "gh_6", Outcome: jobapi.OutcomeInfra, Error: "host lost"})
	h.a.OnResult(ctx, jobapi.Result{JobID: "gh_7", Outcome: jobapi.OutcomeFailed})
	h.a.OnResult(ctx, jobapi.Result{JobID: "gh_8", Outcome: jobapi.OutcomeInfra})
	h.a.OnResult(ctx, jobapi.Result{JobID: "gh_6", Outcome: jobapi.OutcomeInfra}) // duplicate
	h.a.OnResult(ctx, jobapi.Result{JobID: "other", Outcome: jobapi.OutcomeInfra})
	if len(h.api.reruns) != 0 {
		t.Fatal("OnResult must not call the API")
	}

	h.a.Reconcile(ctx)
	if fmt.Sprint(h.api.reruns) != "[6]" {
		t.Fatalf("reruns = %v, want [6]", h.api.reruns)
	}
	if fmt.Sprint(asked) != "[42/acme/api 42/acme/api]" {
		t.Errorf("RerunInfra asked for %v", asked)
	}
	h.a.Reconcile(ctx)
	if len(h.api.reruns) != 1 {
		t.Fatalf("re-ran twice: %v", h.api.reruns)
	}
}

func TestAdapterInfraRerunNotOptedIn(t *testing.T) {
	h := newHarness(t)
	h.startJob(6, 1)
	h.a.OnResult(context.Background(), jobapi.Result{JobID: "gh_6", Outcome: jobapi.OutcomeInfra})
	h.a.Reconcile(context.Background())
	if len(h.api.reruns) != 0 {
		t.Fatalf("re-ran without opt-in: %v", h.api.reruns)
	}
}

func TestAdapterInfraRerunRetriesUntilAccepted(t *testing.T) {
	h := newHarness(t)
	h.a.RerunInfra = func(int64, string) bool { return true }
	h.a.RerunWindow = 5 * time.Minute
	h.startJob(6, 1)
	h.startJob(7, 1)
	ctx := context.Background()
	h.a.OnResult(ctx, jobapi.Result{JobID: "gh_6", Outcome: jobapi.OutcomeInfra})

	// The run is still in progress: GitHub refuses, we try again next pass.
	h.api.err = &APIError{Status: http.StatusForbidden, Remaining: 4000, Body: "run not completed"}
	h.a.Reconcile(ctx)
	h.api.err = nil
	h.a.Reconcile(ctx) // backing off: not retried on the very next pass
	if len(h.api.reruns) != 0 {
		t.Fatalf("reruns = %v, want none during backoff", h.api.reruns)
	}
	h.clock = h.clock.Add(rerunBackoffMin)
	h.a.Reconcile(ctx)
	if fmt.Sprint(h.api.reruns) != "[6]" {
		t.Fatalf("reruns = %v, want [6]", h.api.reruns)
	}

	// A request that keeps failing is dropped after RerunWindow.
	h.a.OnResult(ctx, jobapi.Result{JobID: "gh_7", Outcome: jobapi.OutcomeInfra})
	h.api.err = &APIError{Status: http.StatusForbidden, Remaining: 4000}
	h.a.Reconcile(ctx)
	h.clock = h.clock.Add(6 * time.Minute)
	h.a.Reconcile(ctx)
	h.api.err = nil
	h.a.Reconcile(ctx)
	if fmt.Sprint(h.api.reruns) != "[6]" {
		t.Fatalf("reruns = %v: expired request was still sent", h.api.reruns)
	}
}

// With the default RerunWindow and the controlplane's 30s Reconcile tick, a
// run that stays incomplete for most of the window is still re-run once it
// completes, and the refused requests are spaced out rather than sent every
// tick.
func TestAdapterInfraRerunWaitsForLongRunningRun(t *testing.T) {
	h := newHarness(t)
	h.a.RerunInfra = func(int64, string) bool { return true }
	h.startJob(6, 1)
	ctx := context.Background()
	h.a.OnResult(ctx, jobapi.Result{JobID: "gh_6", Outcome: jobapi.OutcomeInfra})

	refused := 0
	h.api.err = &APIError{Status: http.StatusForbidden, Remaining: 4000, Body: "run not completed"}
	for range 50 * 2 { // 50 minutes of 30s ticks
		before := h.api.rerunCalls
		h.a.Reconcile(ctx)
		refused += h.api.rerunCalls - before
		h.clock = h.clock.Add(30 * time.Second)
	}
	if refused == 0 || refused > 15 {
		t.Fatalf("%d refused re-run requests in 50m, want a backed-off handful", refused)
	}
	h.api.err = nil
	for range 2 * 5 { // the run completes; within one max backoff we re-run
		h.a.Reconcile(ctx)
		h.clock = h.clock.Add(30 * time.Second)
	}
	if fmt.Sprint(h.api.reruns) != "[6]" {
		t.Fatalf("reruns = %v, want [6] after the run completed", h.api.reruns)
	}
}

func TestRerunBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 4: 4 * time.Minute, 5: 5 * time.Minute, 50: 5 * time.Minute} {
		if got := rerunBackoff(n); got != want {
			t.Errorf("rerunBackoff(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestAdapterInfraErrorBeforeStartRedispatches(t *testing.T) {
	h := newHarness(t)
	h.a.RerunInfra = func(int64, string) bool { return true }
	h.post("workflow_job", jobEvent("queued", 9, tachyonLabels, ""), testSecret)
	// The VM died before the runner picked the job up: GitHub still has it
	// queued, so a fresh runner is dispatched without waiting for the
	// reconcile timeout, and no re-run is requested.
	h.a.OnResult(context.Background(), jobapi.Result{JobID: "gh_9", Outcome: jobapi.OutcomeInfra})
	h.a.Reconcile(context.Background())
	if len(h.core.subs) != 2 || len(h.api.created) != 2 {
		t.Fatalf("subs=%d created=%d, want 2 and 2", len(h.core.subs), len(h.api.created))
	}
	if fmt.Sprint(h.api.deleted) != "[1]" {
		t.Errorf("deleted = %v, want the dead runner [1]", h.api.deleted)
	}
	if len(h.api.reruns) != 0 {
		t.Errorf("reruns = %v", h.api.reruns)
	}
}

func TestAdapterServerErrorJobIsReconciled(t *testing.T) {
	h := newHarness(t)
	h.api.err = &APIError{Status: http.StatusInternalServerError, Remaining: -1}
	if code := h.post("workflow_job", jobEvent("queued", 1, tachyonLabels, ""), testSecret); code != http.StatusAccepted {
		t.Fatalf("status %d, want 202: a job hit by a GitHub 5xx is kept for reconcile", code)
	}
	if len(h.core.subs) != 0 {
		t.Fatalf("subs = %d", len(h.core.subs))
	}
	h.api.err = nil
	h.clock = h.clock.Add(time.Minute) // past the budget's 5xx backoff
	h.a.Reconcile(context.Background())
	if len(h.core.subs) != 1 {
		t.Fatalf("subs = %d after GitHub recovered, want 1", len(h.core.subs))
	}
}

func TestAdapterRedispatchesJobWhoseRunnerWasTaken(t *testing.T) {
	h := newHarness(t)
	h.post("workflow_job", jobEvent("queued", 1, tachyonLabels, ""), testSecret)
	h.post("workflow_job", jobEvent("queued", 2, tachyonLabels, ""), testSecret)
	taken := h.api.created[1] // job 2's runner
	h.post("workflow_job", jobEvent("in_progress", 1, tachyonLabels, taken), testSecret)

	h.a.Reconcile(context.Background()) // no wait for ReconcileAfter
	if len(h.core.subs) != 3 {
		t.Fatalf("subs = %d, want job 2 re-dispatched at once", len(h.core.subs))
	}
	if id := h.core.subs[2].Spec.JobID; id != "gh_2-2" {
		t.Fatalf("retry job ID = %q, want a new ID the core has not placed", id)
	}
	if len(h.api.deleted) != 0 {
		t.Fatalf("deleted %v: the taken runner is running job 1", h.api.deleted)
	}
	if _, ok := h.a.jobs["gh_2-2"]; !ok {
		t.Fatal("retry ID not mapped back to the GitHub job")
	}
}
