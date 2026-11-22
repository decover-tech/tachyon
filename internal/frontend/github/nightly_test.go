package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
	"github.com/decover-tech/tachyon/internal/prebuild"
)

type fakeNightlyAPI struct {
	mu        sync.Mutex
	reruns    []int64
	checks    []CheckRun
	rerunErr  error
	checkRepo string
}

func (f *fakeNightlyAPI) RerunWorkflow(_ context.Context, _ int64, _ string, runID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rerunErr != nil {
		return f.rerunErr
	}
	f.reruns = append(f.reruns, runID)
	return nil
}

func (f *fakeNightlyAPI) CreateCheckRun(_ context.Context, _ int64, repo string, cr CheckRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkRepo = repo
	f.checks = append(f.checks, cr)
	return nil
}

type nightlyHarness struct {
	*harness
	napi    *fakeNightlyAPI
	matches map[string]prebuild.Match
}

func newNightlyHarness(t *testing.T) *nightlyHarness {
	h := &nightlyHarness{harness: newHarness(t), napi: &fakeNightlyAPI{}, matches: map[string]prebuild.Match{}}
	h.clock = time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	h.a.Nightly = &Nightly{
		API:           h.napi,
		Hour:          3,
		Repos:         func(repo string) bool { return repo == "acme/api" },
		PrebuildMatch: func(id string) prebuild.Match { return h.matches[id] },
	}
	return h
}

// commitJobs is runJobs for a run on a given commit.
func (h *nightlyHarness) commitJobs(runID int64, sha string, jobs map[int64][2]string) {
	h.t.Helper()
	for id, nc := range jobs {
		e := nightlyEvent("queued", id, runID, 1, "CI", nc[0], "")
		e["workflow_job"].(map[string]any)["head_sha"] = sha
		h.post("workflow_job", e, testSecret)
	}
	for id, nc := range jobs {
		e := nightlyEvent("completed", id, runID, 1, "CI", nc[0], nc[1])
		e["workflow_job"].(map[string]any)["head_sha"] = sha
		h.post("workflow_job", e, testSecret)
	}
}

func TestNightlyIsOptInPerRepo(t *testing.T) {
	for name, repos := range map[string]func(string) bool{
		"no repos":        nil,
		"other repo only": func(r string) bool { return r == "acme/web" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newNightlyHarness(t)
			h.a.Nightly.Repos = repos
			h.matches["gh_1"] = prebuild.MatchFull
			h.runJobs(77, 1, "CI", map[int64][2]string{1: {"test", "success"}})
			h.clock = time.Date(2026, 11, 21, 4, 0, 0, 0, time.UTC)
			h.a.Reconcile(context.Background())
			if len(h.napi.reruns) != 0 {
				t.Fatalf("re-ran a repo that did not opt in: %v", h.napi.reruns)
			}
		})
	}
}

// At the nightly hour the run for the newest commit B is still going. The
// finished run for the older commit A must not be re-run: its deploy job
// would ship A over B.
func TestNightlyNeverRerunsAnOlderCommit(t *testing.T) {
	h := newNightlyHarness(t)
	h.matches["gh_1"], h.matches["gh_2"] = prebuild.MatchFull, prebuild.MatchFull
	h.commitJobs(70, "aaaaaaa", map[int64][2]string{1: {"test", "success"}})

	h.clock = time.Date(2026, 11, 21, 2, 55, 0, 0, time.UTC)
	e := nightlyEvent("queued", 2, 71, 1, "CI", "test", "")
	e["workflow_job"].(map[string]any)["head_sha"] = "bbbbbbb"
	h.post("workflow_job", e, testSecret)

	h.clock = time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	h.a.Reconcile(context.Background())
	if len(h.napi.reruns) != 0 {
		t.Fatalf("re-ran an older commit's run while the newest was in progress: %v", h.napi.reruns)
	}

	e = nightlyEvent("completed", 2, 71, 1, "CI", "test", "success")
	e["workflow_job"].(map[string]any)["head_sha"] = "bbbbbbb"
	h.post("workflow_job", e, testSecret)
	h.clock = h.clock.Add(20 * time.Minute)
	h.a.Reconcile(context.Background())
	if len(h.napi.reruns) != 1 || h.napi.reruns[0] != 71 {
		t.Fatalf("reruns = %v, want [71]", h.napi.reruns)
	}
}

// When the newest commit's run did not use a prebuild, nothing is re-run,
// not an older commit's run that did.
func TestNightlyNoFallbackToOlderCommit(t *testing.T) {
	h := newNightlyHarness(t)
	h.matches["gh_1"], h.matches["gh_2"] = prebuild.MatchFull, prebuild.MatchNone
	h.commitJobs(70, "aaaaaaa", map[int64][2]string{1: {"test", "success"}})
	h.clock = h.clock.Add(time.Hour)
	h.commitJobs(71, "bbbbbbb", map[int64][2]string{2: {"test", "success"}})
	h.clock = time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	h.a.Reconcile(context.Background())
	if len(h.napi.reruns) != 0 {
		t.Fatalf("fell back to an older commit: %v", h.napi.reruns)
	}
}

func nightlyEvent(action string, id, runID int64, attempt int, workflow, name, conclusion string) map[string]any {
	e := jobEvent(action, id, tachyonLabels, "")
	j := e["workflow_job"].(map[string]any)
	j["run_id"], j["run_attempt"], j["workflow_name"], j["name"], j["conclusion"] = runID, attempt, workflow, name, conclusion
	return e
}

// runJobs queues and completes jobs of one attempt.
func (h *nightlyHarness) runJobs(runID int64, attempt int, workflow string, jobs map[int64][2]string) {
	h.t.Helper()
	for id, nc := range jobs {
		h.post("workflow_job", nightlyEvent("queued", id, runID, attempt, workflow, nc[0], ""), testSecret)
	}
	for id, nc := range jobs {
		h.post("workflow_job", nightlyEvent("completed", id, runID, attempt, workflow, nc[0], nc[1]), testSecret)
	}
}

func (h *nightlyHarness) lastSub() jobapi.Submission {
	h.core.mu.Lock()
	defer h.core.mu.Unlock()
	return h.core.subs[len(h.core.subs)-1]
}

func TestNightlyCleanRunEndToEnd(t *testing.T) {
	h := newNightlyHarness(t)
	h.matches["gh_1"], h.matches["gh_2"] = prebuild.MatchFull, prebuild.MatchFull

	// A trusted default-branch push run that used the prebuild.
	h.runJobs(77, 1, "CI", map[int64][2]string{1: {"test", "success"}, 2: {"lint", "success"}})
	if h.lastSub().Spec.Prebuild.Policy != jobapi.PrebuildLatestTrusted {
		t.Fatal("ordinary run must use the prebuild")
	}

	// Before the nightly hour: nothing. (Settled, but 14:00 on day 1 is
	// after 03:00, so move to before the hour on day 2 first.)
	h.clock = time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC)
	h.a.Reconcile(context.Background())
	if len(h.napi.reruns) != 0 {
		t.Fatal("started before the nightly hour")
	}

	h.clock = time.Date(2026, 11, 21, 3, 5, 0, 0, time.UTC)
	h.a.Reconcile(context.Background())
	if len(h.napi.reruns) != 1 || h.napi.reruns[0] != 77 {
		t.Fatalf("reruns = %v, want [77]", h.napi.reruns)
	}
	// Once per night.
	h.a.Reconcile(context.Background())
	if len(h.napi.reruns) != 1 {
		t.Fatalf("started twice: %v", h.napi.reruns)
	}

	// The new attempt's jobs are forced clean.
	h.post("workflow_job", nightlyEvent("queued", 11, 77, 2, "CI", "test", ""), testSecret)
	if p := h.lastSub().Spec.Prebuild.Policy; p != jobapi.PrebuildNone {
		t.Fatalf("clean attempt policy = %s", p)
	}
	h.post("workflow_job", nightlyEvent("queued", 12, 77, 2, "CI", "lint", ""), testSecret)
	h.post("workflow_job", nightlyEvent("completed", 11, 77, 2, "CI", "test", "failure"), testSecret)
	h.a.Reconcile(context.Background())
	if len(h.napi.checks) != 0 {
		t.Fatal("reported before the clean attempt finished")
	}
	h.post("workflow_job", nightlyEvent("completed", 12, 77, 2, "CI", "lint", "success"), testSecret)
	h.a.Reconcile(context.Background())

	if len(h.napi.checks) != 1 {
		t.Fatalf("checks = %d, want 1", len(h.napi.checks))
	}
	cr := h.napi.checks[0]
	if h.napi.checkRepo != "acme/api" || cr.HeadSHA != "9f1c2ab" || cr.Name != CheckRunName || cr.Conclusion != "failure" {
		t.Errorf("check = %+v on %s", cr, h.napi.checkRepo)
	}
	if !strings.Contains(cr.Summary, "| test | success | failure |") {
		t.Errorf("summary:\n%s", cr.Summary)
	}
	reps := h.a.Nightly.Reports("acme/api")
	if len(reps) != 1 || len(reps[0].Divergences) != 1 || reps[0].Divergences[0].Job != "test" {
		t.Fatalf("reports = %+v", reps)
	}

	// A later attempt of the same run is ordinary again.
	h.post("workflow_job", nightlyEvent("queued", 21, 77, 3, "CI", "test", ""), testSecret)
	if p := h.lastSub().Spec.Prebuild.Policy; p != jobapi.PrebuildLatestTrusted {
		t.Fatalf("after the clean run, policy = %s", p)
	}
}

func TestNightlySkipsIneligibleRuns(t *testing.T) {
	cases := map[string]func(h *nightlyHarness){
		"pull request": func(h *nightlyHarness) {
			h.api.rc = RunContext{Event: "pull_request", HeadBranch: "feat", HeadRepo: "acme/api", BaseRepo: "acme/api", DefaultBranch: "main"}
		},
		"protected non-default branch": func(h *nightlyHarness) {
			h.api.rc = RunContext{Event: "push", HeadBranch: "release/1", HeadRepo: "acme/api", BaseRepo: "acme/api", DefaultBranch: "main", Protected: true}
		},
		"manual dispatch": func(h *nightlyHarness) { h.api.rc.Event = "workflow_dispatch" },
		"deploy workflow": func(h *nightlyHarness) {},
		"no prebuild used": func(h *nightlyHarness) {
			h.matches["gh_1"] = prebuild.MatchNone
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newNightlyHarness(t)
			h.matches["gh_1"] = prebuild.MatchFull
			setup(h)
			wf := "CI"
			if name == "deploy workflow" {
				wf = "Deploy production"
			}
			h.runJobs(77, 1, wf, map[int64][2]string{1: {"test", "success"}})
			h.clock = time.Date(2026, 11, 21, 4, 0, 0, 0, time.UTC)
			h.a.Reconcile(context.Background())
			if len(h.napi.reruns) != 0 {
				t.Fatalf("re-ran an ineligible run: %v", h.napi.reruns)
			}
		})
	}
}

func TestNightlyWaitsForRunToSettle(t *testing.T) {
	h := newNightlyHarness(t)
	h.clock = time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	h.post("workflow_job", nightlyEvent("queued", 1, 77, 1, "CI", "test", ""), testSecret)
	h.a.Reconcile(context.Background())
	if len(h.napi.reruns) != 0 {
		t.Fatal("re-ran a run still in progress")
	}
	h.post("workflow_job", nightlyEvent("completed", 1, 77, 1, "CI", "test", "success"), testSecret)
	h.clock = h.clock.Add(time.Minute)
	h.a.Reconcile(context.Background())
	if len(h.napi.reruns) != 0 {
		t.Fatal("re-ran a run before it settled (a needs: job may still be queued)")
	}
	h.clock = h.clock.Add(20 * time.Minute)
	h.a.Reconcile(context.Background())
	if len(h.napi.reruns) != 1 {
		t.Fatal("settled run was not re-run")
	}
}

func TestNightlyPicksLatestRunAndRetriesTomorrowOnError(t *testing.T) {
	h := newNightlyHarness(t)
	h.runJobs(70, 1, "CI", map[int64][2]string{1: {"test", "success"}})
	h.clock = h.clock.Add(time.Hour)
	h.runJobs(71, 1, "CI", map[int64][2]string{2: {"test", "success"}})

	h.napi.rerunErr = errors.New("403: resource not accessible by integration")
	h.clock = time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	h.a.Reconcile(context.Background())
	h.a.Reconcile(context.Background())
	h.napi.rerunErr = nil
	h.a.Reconcile(context.Background())
	if len(h.napi.reruns) != 0 {
		t.Fatal("retried the same night after an error")
	}
	// A job queued for run 71 now is an ordinary job, not a clean one.
	h.post("workflow_job", nightlyEvent("queued", 3, 71, 2, "CI", "test", ""), testSecret)
	if h.lastSub().Spec.Prebuild.Policy != jobapi.PrebuildLatestTrusted {
		t.Fatal("failed re-run left the run in clean mode")
	}
	h.post("workflow_job", nightlyEvent("completed", 3, 71, 2, "CI", "test", "success"), testSecret)

	h.clock = time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	h.a.Reconcile(context.Background())
	if len(h.napi.reruns) != 1 || h.napi.reruns[0] != 71 {
		t.Fatalf("reruns = %v, want [71]", h.napi.reruns)
	}
}

func TestNightlyTimeoutReportsPartialResults(t *testing.T) {
	h := newNightlyHarness(t)
	h.runJobs(77, 1, "CI", map[int64][2]string{1: {"test", "failure"}, 2: {"lint", "success"}})
	h.clock = time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	h.a.Reconcile(context.Background())
	h.post("workflow_job", nightlyEvent("queued", 11, 77, 2, "CI", "test", ""), testSecret)
	h.post("workflow_job", nightlyEvent("completed", 11, 77, 2, "CI", "test", "success"), testSecret)
	// lint never shows up.
	h.clock = h.clock.Add(7 * time.Hour)
	h.a.Reconcile(context.Background())
	if len(h.napi.checks) != 1 {
		t.Fatalf("checks = %d", len(h.napi.checks))
	}
	if cr := h.napi.checks[0]; cr.Conclusion != "neutral" || !strings.Contains(cr.Summary, "`lint`") {
		t.Errorf("check = %+v", cr)
	}
}

func TestNightlyBoundsTrackedRuns(t *testing.T) {
	h := newNightlyHarness(t)
	for i := int64(0); i < 20; i++ {
		h.clock = h.clock.Add(time.Minute)
		h.runJobs(100+i, 1, "CI", map[int64][2]string{i + 1: {"test", "success"}})
	}
	n := h.a.Nightly
	n.mu.Lock()
	defer n.mu.Unlock()
	if got := len(n.repos["acme/api"].runs); got != maxTrackedRuns {
		t.Errorf("runs = %d, want %d", got, maxTrackedRuns)
	}
	if len(n.jobs) != maxTrackedRuns {
		t.Errorf("jobs = %d, want %d", len(n.jobs), maxTrackedRuns)
	}
}

func TestClientNightlyEndpoints(t *testing.T) {
	var got map[string]any
	var rerun bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"token": "inst-tok", "expires_at": time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("POST /repos/acme/api/actions/runs/77/rerun", func(w http.ResponseWriter, r *http.Request) {
		rerun = r.Header.Get("Authorization") == "token inst-tok"
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST /repos/acme/api/check-runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":1}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{AppID: 1, PrivateKey: testKey(t), BaseURL: srv.URL}
	ctx := context.Background()
	if err := c.RerunWorkflow(ctx, 42, "acme/api", 77); err != nil || !rerun {
		t.Fatalf("RerunWorkflow: err=%v authed=%v", err, rerun)
	}
	err := c.CreateCheckRun(ctx, 42, "acme/api", CheckRun{HeadSHA: "abc", Name: CheckRunName, Conclusion: "failure", Title: "t", Summary: "s"})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := got["output"].(map[string]any)
	if got["head_sha"] != "abc" || got["status"] != "completed" || got["conclusion"] != "failure" || got["name"] != CheckRunName || out["title"] != "t" || out["summary"] != "s" {
		t.Errorf("check run body = %v", got)
	}
}
