// Package dashboard builds the customer dashboard from job telemetry: what
// each job would have cost on GitHub-hosted runners, the minutes prebuilds
// saved per repo, and each job's prebuild match and cache hits.
//
// Estimates follow a deliberately conservative model:
//
//   - A job's GitHub-hosted runtime is its own run time (job started to job
//     finished) plus the setup a prebuild saved it. VM boot and pickup are
//     not counted; GitHub does not bill for them either.
//   - A full prebuild match saved the repo's setup baseline: the median run
//     time of its recent prebuild builds, which run the same warm-up steps a
//     job would otherwise run. Partial matches rerun their installs and no
//     match saves nothing, so both count as zero saved.
//   - GitHub bills each job rounded up to a whole minute, at a per-minute
//     list price by runner size.
package dashboard

import (
	"math"
	"sort"
	"time"

	"github.com/decover-tech/tachyon/internal/telemetry"
)

// Rates are GitHub-hosted per-minute list prices in USD, keyed by vCPU.
type Rates map[int]float64

// DefaultRates are GitHub's Linux x64 per-minute list prices (2-core
// standard runner, and larger runners scaling linearly). GitHub changes its
// prices; operators should override these from the current price list.
var DefaultRates = Rates{2: 0.008, 4: 0.016, 8: 0.032, 16: 0.064}

// baselineWindow is how many recent prebuild builds feed a repo's baseline.
const baselineWindow = 10

// Options select and price a report.
type Options struct {
	Tenant string
	Repo   string    // optional: one repo only
	Since  time.Time // jobs finished at or after; zero = no bound
	Until  time.Time // jobs finished before; zero = no bound
	Rates  Rates     // nil = DefaultRates
}

// Job is one finished customer job.
type Job struct {
	JobID         string    `json:"job_id"`
	Repo          string    `json:"repo"`
	VCPU          int       `json:"vcpu"`
	Burst         bool      `json:"burst"`
	FinishedAt    time.Time `json:"finished_at"`
	PrebuildMatch string    `json:"prebuild_match"` // full | partial | none
	CacheHits     int       `json:"cache_hits"`
	CacheMisses   int       `json:"cache_misses"`
	// RunSeconds is the job's run time on Tachyon.
	RunSeconds float64 `json:"run_seconds"`
	// SavedSeconds is setup time the prebuild saved this job.
	SavedSeconds float64 `json:"saved_seconds"`
	// GitHubMinutes is what GitHub-hosted would have billed.
	GitHubMinutes int `json:"github_hosted_minutes"`
	// GitHubUSD is nil when there is no GitHub price for this size.
	GitHubUSD *float64 `json:"github_hosted_usd,omitempty"`
}

// Repo summarises one repo's jobs.
type Repo struct {
	Repo         string `json:"repo"`
	Jobs         int    `json:"jobs"`
	MatchFull    int    `json:"prebuild_full"`
	MatchPartial int    `json:"prebuild_partial"`
	MatchNone    int    `json:"prebuild_none"`
	CacheHits    int    `json:"cache_hits"`
	CacheMisses  int    `json:"cache_misses"`
	// SetupBaselineSeconds is nil until the repo has a finished prebuild build.
	SetupBaselineSeconds *float64 `json:"setup_baseline_seconds,omitempty"`
	MinutesSaved         float64  `json:"minutes_saved"`
	// PrebuildMinutes is time spent building prebuilds in the window, so
	// customers see the net saving, not only the gross.
	PrebuildBuilds  int     `json:"prebuild_builds"`
	PrebuildMinutes float64 `json:"prebuild_minutes"`
	NetMinutesSaved float64 `json:"net_minutes_saved"`
	GitHubMinutes   int     `json:"github_hosted_minutes"`
	GitHubUSD       float64 `json:"github_hosted_usd"`
	// GitHubUSDPartial is set when some jobs had no GitHub price.
	GitHubUSDPartial bool `json:"github_hosted_usd_partial,omitempty"`
}

// Report is a tenant's dashboard for a time window.
type Report struct {
	Tenant string `json:"tenant"`
	Jobs   []Job  `json:"jobs"`
	Repos  []Repo `json:"repos"`
}

// Build computes the report for opts.Tenant from records. Records of other
// tenants and unfinished jobs are ignored.
func Build(records []*telemetry.Record, opts Options) Report {
	rates := opts.Rates
	if rates == nil {
		rates = DefaultRates
	}
	baselines := setupBaselines(records, opts.Tenant)

	rep := Report{Tenant: opts.Tenant, Jobs: []Job{}, Repos: []Repo{}}
	repos := map[string]*Repo{}
	repoRow := func(name string) *Repo {
		r := repos[name]
		if r == nil {
			r = &Repo{Repo: name}
			if b, ok := baselines[name]; ok {
				s := b.Seconds()
				r.SetupBaselineSeconds = &s
			}
			repos[name] = r
		}
		return r
	}

	for _, rec := range records {
		if rec.Tenant != opts.Tenant || (opts.Repo != "" && rec.Repo != opts.Repo) {
			continue
		}
		finished, ok := rec.At(telemetry.JobFinished)
		if !ok || (!opts.Since.IsZero() && finished.Before(opts.Since)) || (!opts.Until.IsZero() && !finished.Before(opts.Until)) {
			continue
		}
		run, ok := runTime(rec)
		if !ok {
			continue
		}
		row := repoRow(rec.Repo)
		if rec.IsPrebuildBuild() {
			row.PrebuildBuilds++
			row.PrebuildMinutes += run.Minutes()
			continue
		}

		j := Job{
			JobID:         rec.JobID,
			Repo:          rec.Repo,
			VCPU:          rec.VCPU,
			Burst:         rec.Burst,
			FinishedAt:    finished,
			PrebuildMatch: match(rec.PrebuildMatch),
			RunSeconds:    run.Seconds(),
		}
		j.CacheHits, j.CacheMisses = rec.CacheCounts()
		var saved time.Duration
		if j.PrebuildMatch == "full" {
			saved = baselines[rec.Repo]
		}
		j.SavedSeconds = saved.Seconds()
		j.GitHubMinutes = GitHubMinutes(run + saved)
		if rate, ok := rates[rec.VCPU]; ok {
			usd := float64(j.GitHubMinutes) * rate
			row.GitHubUSD += usd
			usd = math.Round(usd*1e4) / 1e4
			j.GitHubUSD = &usd
		} else {
			row.GitHubUSDPartial = true
		}
		rep.Jobs = append(rep.Jobs, j)

		row.Jobs++
		switch j.PrebuildMatch {
		case "full":
			row.MatchFull++
		case "partial":
			row.MatchPartial++
		default:
			row.MatchNone++
		}
		row.CacheHits += j.CacheHits
		row.CacheMisses += j.CacheMisses
		row.MinutesSaved += saved.Minutes()
		row.GitHubMinutes += j.GitHubMinutes
	}

	for _, r := range repos {
		r.MinutesSaved = round2(r.MinutesSaved)
		r.PrebuildMinutes = round2(r.PrebuildMinutes)
		r.NetMinutesSaved = round2(r.MinutesSaved - r.PrebuildMinutes)
		r.GitHubUSD = round2(r.GitHubUSD)
		rep.Repos = append(rep.Repos, *r)
	}
	sort.Slice(rep.Repos, func(a, b int) bool { return rep.Repos[a].Repo < rep.Repos[b].Repo })
	sort.Slice(rep.Jobs, func(a, b int) bool {
		if !rep.Jobs[a].FinishedAt.Equal(rep.Jobs[b].FinishedAt) {
			return rep.Jobs[a].FinishedAt.After(rep.Jobs[b].FinishedAt)
		}
		return rep.Jobs[a].JobID < rep.Jobs[b].JobID
	})
	return rep
}

// GitHubMinutes is what GitHub-hosted bills for d: whole minutes, rounded
// up, at least one.
func GitHubMinutes(d time.Duration) int {
	m := int(math.Ceil(d.Minutes()))
	if m < 1 {
		m = 1
	}
	return m
}

// runTime is the job's run time, falling back to billable runner time when
// the job-started phase was not reported.
func runTime(rec *telemetry.Record) (time.Duration, bool) {
	if d, ok := rec.Run(); ok && d >= 0 {
		return d, true
	}
	if d, ok := rec.Billable(); ok && d >= 0 {
		return d, true
	}
	return 0, false
}

// setupBaselines is, per repo, the median run time of the tenant's most
// recent finished prebuild builds. It ignores the report window so a quiet
// window still gets a baseline.
func setupBaselines(records []*telemetry.Record, tenant string) map[string]time.Duration {
	type build struct {
		at  time.Time
		run time.Duration
	}
	builds := map[string][]build{}
	for _, rec := range records {
		if rec.Tenant != tenant || !rec.IsPrebuildBuild() {
			continue
		}
		at, ok := rec.At(telemetry.JobFinished)
		if !ok {
			continue
		}
		if d, ok := runTime(rec); ok {
			builds[rec.Repo] = append(builds[rec.Repo], build{at, d})
		}
	}
	out := map[string]time.Duration{}
	for repo, bs := range builds {
		sort.Slice(bs, func(a, b int) bool { return bs[a].at.After(bs[b].at) })
		if len(bs) > baselineWindow {
			bs = bs[:baselineWindow]
		}
		ds := make([]time.Duration, len(bs))
		for i, b := range bs {
			ds[i] = b.run
		}
		out[repo] = median(ds)
	}
	return out
}

func median(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(a, b int) bool { return ds[a] < ds[b] })
	n := len(ds)
	if n%2 == 1 {
		return ds[n/2]
	}
	return (ds[n/2-1] + ds[n/2]) / 2
}

func match(m string) string {
	switch m {
	case "full", "partial":
		return m
	}
	return "none" // includes burst jobs, which never fork a prebuild
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
