package prebuild

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// A nightly clean run per repo, without a prebuild, catches drift that only
// shows on a clean build: a dependency that no longer installs, a tool the
// prebuild still carries but the base image dropped, a test that only passes
// because of state left behind by the warm-up. The clean run repeats a
// recent trusted run at the same commit with every job forked from the base
// image, and its results are compared job by job with the prebuild run's.

// NightlyDue reports whether a repo's nightly clean run should start now:
// once per UTC day, at or after hour (0-23), and not again the same day
// once one has been started.
func NightlyDue(last, now time.Time, hour int) bool {
	now = now.UTC()
	if now.Hour() < hour {
		return false
	}
	if last.IsZero() {
		return true
	}
	ly, lm, ld := last.UTC().Date()
	ny, nm, nd := now.Date()
	return ly != ny || lm != nm || ld != nd
}

// Conclusion is a job's final result as the CI platform reports it.
type Conclusion string

const (
	ConclusionSuccess Conclusion = "success"
	ConclusionFailure Conclusion = "failure"
	// Anything else (cancelled, skipped, timed out, ...) is not comparable.
)

// Comparable reports whether c says something about the code: a pass or a
// fail, not a cancellation or a skip. A timeout counts as a failure.
func (c Conclusion) Comparable() bool {
	return c == ConclusionSuccess || c == ConclusionFailure || c == "timed_out"
}

func (c Conclusion) passed() bool { return c == ConclusionSuccess }

// JobOutcome is one job's result in a run.
type JobOutcome struct {
	Name       string // stable across attempts of the same run
	Conclusion Conclusion
	Match      Match // how the job used its prebuild; "" if unknown
}

// DivergenceKind says which side of the comparison failed.
type DivergenceKind string

const (
	// CleanFailed: the job passed on the prebuild and fails on a clean
	// build. The prebuild is hiding a real problem.
	CleanFailed DivergenceKind = "clean-failed"
	// PrebuildFailed: the job fails on the prebuild and passes clean. The
	// prebuild itself is stale or broken.
	PrebuildFailed DivergenceKind = "prebuild-failed"
)

// Divergence is one job whose result depends on the prebuild.
type Divergence struct {
	Job      string
	Kind     DivergenceKind
	Prebuild Conclusion
	Clean    Conclusion
	Match    Match // prebuild-side match
}

// DriftReport compares a nightly clean run with the prebuild run it repeats.
type DriftReport struct {
	Repo        string
	Commit      string
	Compared    []string // jobs compared, sorted
	Divergences []Divergence
	// Skipped jobs could not be compared: missing on one side, not
	// comparable (cancelled, skipped), or the prebuild side did not fork a
	// prebuild, so both runs were clean.
	Skipped []string
}

// CompareClean compares the prebuild run's job outcomes with the clean run's.
func CompareClean(repo, commit string, prebuild, clean []JobOutcome) DriftReport {
	r := DriftReport{Repo: repo, Commit: commit}
	cleanBy := map[string]JobOutcome{}
	for _, o := range clean {
		cleanBy[o.Name] = o
	}
	seen := map[string]bool{}
	for _, p := range prebuild {
		seen[p.Name] = true
		c, ok := cleanBy[p.Name]
		if !ok || !p.Conclusion.Comparable() || !c.Conclusion.Comparable() || p.Match == MatchNone {
			r.Skipped = append(r.Skipped, p.Name)
			continue
		}
		r.Compared = append(r.Compared, p.Name)
		switch {
		case p.Conclusion.passed() && !c.Conclusion.passed():
			r.Divergences = append(r.Divergences, Divergence{Job: p.Name, Kind: CleanFailed, Prebuild: p.Conclusion, Clean: c.Conclusion, Match: p.Match})
		case !p.Conclusion.passed() && c.Conclusion.passed():
			r.Divergences = append(r.Divergences, Divergence{Job: p.Name, Kind: PrebuildFailed, Prebuild: p.Conclusion, Clean: c.Conclusion, Match: p.Match})
		}
	}
	for _, c := range clean {
		if !seen[c.Name] {
			r.Skipped = append(r.Skipped, c.Name)
		}
	}
	sort.Strings(r.Compared)
	sort.Strings(r.Skipped)
	sort.Slice(r.Divergences, func(i, j int) bool { return r.Divergences[i].Job < r.Divergences[j].Job })
	return r
}

// Has reports whether the report contains a divergence of kind k.
func (r DriftReport) Has(k DivergenceKind) bool {
	for _, d := range r.Divergences {
		if d.Kind == k {
			return true
		}
	}
	return false
}

// Title is a one-line summary for the customer.
func (r DriftReport) Title() string {
	switch n := len(r.Divergences); {
	case len(r.Compared) == 0:
		return "Nightly clean run: no jobs could be compared"
	case n == 0 && len(r.Compared) == 1:
		return "Nightly clean run: 1 job matches the prebuild run"
	case n == 0:
		return fmt.Sprintf("Nightly clean run: %d jobs match the prebuild run", len(r.Compared))
	case n == 1:
		return "Nightly clean run: 1 job diverges from the prebuild run"
	default:
		return fmt.Sprintf("Nightly clean run: %d jobs diverge from the prebuild run", n)
	}
}

// Summary is a Markdown explanation for the customer.
func (r DriftReport) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Tachyon re-ran the jobs of commit `%s` from the base image, without a prebuild, and compared the results with the run that used the prebuild.\n\n", r.Commit)
	if len(r.Divergences) > 0 {
		b.WriteString("| Job | With prebuild | Clean | Meaning |\n| --- | --- | --- | --- |\n")
		for _, d := range r.Divergences {
			meaning := "The prebuild hides a failure a clean build shows. Check for dependencies or tools the warm-up installs that the repo does not declare."
			if d.Kind == PrebuildFailed {
				meaning = "The job fails only on the prebuild, which may be stale. Add the files that changed to `key_files`, or use the `tachyon-clean` label for this job."
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", mdCell(d.Job), d.Prebuild, d.Clean, meaning)
		}
		b.WriteString("\n")
	} else if len(r.Compared) > 0 {
		b.WriteString("Every compared job had the same result with and without the prebuild.\n\n")
	}
	if len(r.Compared) > 0 {
		fmt.Fprintf(&b, "Compared: %s\n", mdList(r.Compared))
	}
	if len(r.Skipped) > 0 {
		fmt.Fprintf(&b, "\nNot compared (missing on one side, cancelled, skipped, or did not use a prebuild): %s\n", mdList(r.Skipped))
	}
	return b.String()
}

func mdCell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

func mdList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = "`" + strings.ReplaceAll(n, "`", "'") + "`"
	}
	return strings.Join(q, ", ")
}
