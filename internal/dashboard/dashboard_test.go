package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/telemetry"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

type job struct {
	id, tenant, repo, match string
	vcpu                    int
	start                   time.Duration // offset from t0
	run                     time.Duration
	prebuildBuild, burst    bool
	hits, misses            int
	unfinished              bool
}

func (j job) record() *telemetry.Record {
	r := telemetry.NewRecord(j.id)
	r.Tenant, r.Repo, r.VCPU, r.PrebuildMatch, r.Burst = j.tenant, j.repo, j.vcpu, j.match, j.burst
	if j.tenant == "" {
		r.Tenant = "acme"
	}
	if j.repo == "" {
		r.Repo = "github.com/acme/api"
	}
	start := t0.Add(j.start)
	r.Mark(telemetry.VMResumed, start.Add(-2*time.Second))
	r.Mark(telemetry.JobStarted, start)
	if !j.unfinished {
		r.Mark(telemetry.JobFinished, start.Add(j.run))
	}
	for i := 0; i < j.hits; i++ {
		r.CacheResult(true)
	}
	for i := 0; i < j.misses; i++ {
		r.CacheResult(false)
	}
	if j.prebuildBuild {
		r.MarkPrebuildBuild()
	}
	return r
}

func records(js ...job) []*telemetry.Record {
	out := make([]*telemetry.Record, len(js))
	for i, j := range js {
		out[i] = j.record()
	}
	return out
}

func jobByID(t *testing.T, r Report, id string) Job {
	t.Helper()
	for _, j := range r.Jobs {
		if j.JobID == id {
			return j
		}
	}
	t.Fatalf("job %s not in report", id)
	return Job{}
}

func TestGitHubMinutes(t *testing.T) {
	for d, want := range map[time.Duration]int{
		0:                                1,
		time.Second:                      1,
		time.Minute:                      1,
		time.Minute + time.Second:        2,
		90 * time.Second:                 2,
		10*time.Minute - time.Nanosecond: 10,
	} {
		if got := GitHubMinutes(d); got != want {
			t.Errorf("GitHubMinutes(%v) = %d, want %d", d, got, want)
		}
	}
}

func TestBuildPerJobEquivalentAndMinutesSaved(t *testing.T) {
	recs := records(
		// Prebuild builds: baseline is the median of 4m, 6m, 5m = 5m.
		job{id: "pb1", prebuildBuild: true, vcpu: 4, start: -3 * time.Hour, run: 4 * time.Minute},
		job{id: "pb2", prebuildBuild: true, vcpu: 4, start: -2 * time.Hour, run: 6 * time.Minute},
		job{id: "pb3", prebuildBuild: true, vcpu: 4, start: 0, run: 5 * time.Minute},
		// Full match: GitHub would also have run the 5m setup.
		job{id: "full", match: "full", vcpu: 4, start: time.Hour, run: 2*time.Minute + 10*time.Second, hits: 3, misses: 1},
		// Partial and none save nothing.
		job{id: "partial", match: "partial", vcpu: 2, start: 2 * time.Hour, run: 7 * time.Minute, hits: 1},
		job{id: "none", match: "none", vcpu: 8, start: 3 * time.Hour, run: 30 * time.Second, misses: 2},
		job{id: "burst", burst: true, vcpu: 16, start: 4 * time.Hour, run: 61 * time.Second},
		// No GitHub price for this size.
		job{id: "odd", match: "full", vcpu: 3, start: 5 * time.Hour, run: time.Minute},
		// Ignored: other tenant, unfinished.
		job{id: "other", tenant: "globex", match: "full", vcpu: 4, run: time.Hour},
		job{id: "running", match: "full", vcpu: 4, unfinished: true},
	)
	rep := Build(recs, Options{Tenant: "acme"})

	if len(rep.Jobs) != 5 {
		t.Fatalf("jobs = %d, want 5 (prebuild builds, other tenants and unfinished excluded): %+v", len(rep.Jobs), rep.Jobs)
	}
	if rep.Jobs[0].JobID != "odd" {
		t.Errorf("jobs not newest first: %s", rep.Jobs[0].JobID)
	}

	full := jobByID(t, rep, "full")
	if full.PrebuildMatch != "full" || full.CacheHits != 3 || full.CacheMisses != 1 {
		t.Errorf("full match/cache = %+v", full)
	}
	if full.SavedSeconds != 300 || full.RunSeconds != 130 {
		t.Errorf("full saved/run = %v %v", full.SavedSeconds, full.RunSeconds)
	}
	// 2m10s + 5m = 7m10s -> 8 GitHub minutes at $0.016.
	if full.GitHubMinutes != 8 || full.GitHubUSD == nil || *full.GitHubUSD != 0.128 {
		t.Errorf("full github = %d %v", full.GitHubMinutes, full.GitHubUSD)
	}

	partial := jobByID(t, rep, "partial")
	if partial.PrebuildMatch != "partial" || partial.SavedSeconds != 0 || partial.GitHubMinutes != 7 || *partial.GitHubUSD != 0.056 {
		t.Errorf("partial = %+v", partial)
	}
	none := jobByID(t, rep, "none")
	if none.PrebuildMatch != "none" || none.GitHubMinutes != 1 || *none.GitHubUSD != 0.032 || none.CacheMisses != 2 {
		t.Errorf("none = %+v", none)
	}
	burst := jobByID(t, rep, "burst")
	if burst.PrebuildMatch != "none" || !burst.Burst || burst.GitHubMinutes != 2 || *burst.GitHubUSD != 0.128 {
		t.Errorf("burst = %+v", burst)
	}
	if odd := jobByID(t, rep, "odd"); odd.GitHubUSD != nil || odd.GitHubMinutes != 6 {
		t.Errorf("odd = %+v", odd)
	}

	if len(rep.Repos) != 1 {
		t.Fatalf("repos = %+v", rep.Repos)
	}
	r := rep.Repos[0]
	if r.Repo != "github.com/acme/api" || r.Jobs != 5 || r.MatchFull != 2 || r.MatchPartial != 1 || r.MatchNone != 2 {
		t.Errorf("repo counts = %+v", r)
	}
	if r.CacheHits != 4 || r.CacheMisses != 3 {
		t.Errorf("repo cache = %d %d", r.CacheHits, r.CacheMisses)
	}
	if r.SetupBaselineSeconds == nil || *r.SetupBaselineSeconds != 300 {
		t.Errorf("baseline = %v", r.SetupBaselineSeconds)
	}
	if r.MinutesSaved != 10 || r.PrebuildBuilds != 3 || r.PrebuildMinutes != 15 || r.NetMinutesSaved != -5 {
		t.Errorf("saved = %v builds=%d %v net=%v", r.MinutesSaved, r.PrebuildBuilds, r.PrebuildMinutes, r.NetMinutesSaved)
	}
	if r.GitHubMinutes != 8+7+1+2+6 || r.GitHubUSD != 0.34 || !r.GitHubUSDPartial {
		t.Errorf("repo github = %d %v partial=%v", r.GitHubMinutes, r.GitHubUSD, r.GitHubUSDPartial)
	}
}

func TestBuildWindowRepoFilterAndNoBaseline(t *testing.T) {
	recs := records(
		job{id: "pb", repo: "github.com/acme/web", prebuildBuild: true, vcpu: 4, start: -48 * time.Hour, run: 3 * time.Minute},
		job{id: "old", repo: "github.com/acme/web", match: "full", vcpu: 4, start: -24 * time.Hour, run: time.Minute},
		job{id: "new", repo: "github.com/acme/web", match: "full", vcpu: 4, start: time.Hour, run: time.Minute},
		job{id: "api", match: "full", vcpu: 4, start: time.Hour, run: time.Minute},
	)
	rep := Build(recs, Options{Tenant: "acme", Since: t0, Until: t0.Add(2 * time.Hour)})
	if len(rep.Jobs) != 2 {
		t.Fatalf("window jobs = %+v", rep.Jobs)
	}
	// The baseline comes from a prebuild built before the window.
	if web := jobByID(t, rep, "new"); web.SavedSeconds != 180 {
		t.Errorf("web saved = %v", web.SavedSeconds)
	}
	// A full match with no prebuild build on record saves nothing claimed.
	if api := jobByID(t, rep, "api"); api.SavedSeconds != 0 || api.GitHubMinutes != 1 {
		t.Errorf("api = %+v", api)
	}
	for _, r := range rep.Repos {
		if r.Repo == "github.com/acme/api" && r.SetupBaselineSeconds != nil {
			t.Error("api should have no baseline")
		}
		if r.Repo == "github.com/acme/web" && r.PrebuildBuilds != 0 {
			t.Error("prebuild build outside window counted")
		}
	}

	only := Build(recs, Options{Tenant: "acme", Repo: "github.com/acme/api"})
	if len(only.Jobs) != 1 || only.Jobs[0].JobID != "api" || len(only.Repos) != 1 {
		t.Errorf("repo filter = %+v", only)
	}
	if empty := Build(nil, Options{Tenant: "acme"}); empty.Jobs == nil || empty.Repos == nil {
		t.Error("empty report should have empty, not nil, slices")
	}
}

func TestBaselineUsesRecentBuilds(t *testing.T) {
	var js []job
	// 10 old slow builds, then 10 recent fast ones; only the recent count.
	for i := 0; i < 20; i++ {
		run := 20 * time.Minute
		if i >= 10 {
			run = 2 * time.Minute
		}
		js = append(js, job{id: "pb" + string(rune('a'+i)), prebuildBuild: true, vcpu: 4, start: time.Duration(i-30) * time.Hour, run: run})
	}
	js = append(js, job{id: "j", match: "full", vcpu: 4, run: time.Minute})
	if j := jobByID(t, Build(records(js...), Options{Tenant: "acme"}), "j"); j.SavedSeconds != 120 {
		t.Errorf("saved = %v, want 120", j.SavedSeconds)
	}
}

type src []*telemetry.Record

func (s src) Records() []*telemetry.Record { return s }

func TestHandler(t *testing.T) {
	recs := src(records(
		job{id: "a", match: "full", vcpu: 4, run: time.Minute},
		job{id: "g", tenant: "globex", match: "none", vcpu: 4, run: time.Minute},
		job{id: "old", match: "none", vcpu: 4, start: -40 * 24 * time.Hour, run: time.Minute},
	))
	h := &Handler{
		Source: recs,
		Now:    func() time.Time { return t0.Add(time.Hour) },
		Tenant: func(r *http.Request) (string, bool) {
			v := r.Header.Get("X-Tenant")
			return v, v != ""
		},
	}

	get := func(h http.Handler, target, tenant string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if tenant != "" {
			req.Header.Set("X-Tenant", tenant)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	w := get(h, "/", "acme")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d", w.Code)
	}
	var rep Report
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Tenant != "acme" || len(rep.Jobs) != 1 || rep.Jobs[0].JobID != "a" {
		t.Errorf("default window report = %+v", rep)
	}

	w = get(h, "/?since=2026-01-01T00:00:00Z", "acme")
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil || len(rep.Jobs) != 2 {
		t.Errorf("since report = %+v %v", rep, err)
	}

	if w := get(h, "/?until=yesterday", "acme"); w.Code != http.StatusBadRequest {
		t.Errorf("bad until: %d", w.Code)
	}
	if w := get(h, "/", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no tenant: %d", w.Code)
	}
	if w := get(&Handler{Source: recs}, "/", "acme"); w.Code != http.StatusUnauthorized {
		t.Errorf("handler without Tenant must refuse: %d", w.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Tenant", "acme")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}
