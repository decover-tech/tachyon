package prebuild

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNightlyDue(t *testing.T) {
	day := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, time.UTC) }
	cases := []struct {
		last, now time.Time
		want      bool
	}{
		{time.Time{}, day(21, 2), false}, // before the hour
		{time.Time{}, day(21, 3), true},  // never run
		{day(21, 3), day(21, 23), false}, // already today
		{day(20, 4), day(21, 3), true},   // yesterday
		{day(20, 4), day(21, 1), false},  // new day, before the hour
	}
	for i, c := range cases {
		if got := NightlyDue(c.last, c.now, 3); got != c.want {
			t.Errorf("case %d: NightlyDue(%v, %v) = %v", i, c.last, c.now, got)
		}
	}
	// Non-UTC inputs are compared in UTC.
	pst := time.FixedZone("PST", -8*3600)
	if NightlyDue(day(21, 3), time.Date(2026, 11, 21, 20, 0, 0, 0, pst), 3) != true {
		t.Error("20:00 PST on the 21st is 04:00 on the 22nd UTC: should be due")
	}
}

func TestCompareClean(t *testing.T) {
	pre := []JobOutcome{
		{Name: "lint", Conclusion: "success", Match: MatchFull},
		{Name: "test", Conclusion: "success", Match: MatchFull},
		{Name: "build", Conclusion: "failure", Match: MatchPartial},
		{Name: "e2e", Conclusion: "cancelled", Match: MatchFull},
		{Name: "docs", Conclusion: "success", Match: MatchNone}, // was clean already
		{Name: "gone", Conclusion: "success", Match: MatchFull},
		{Name: "slow", Conclusion: "success"}, // match unknown: compared
	}
	clean := []JobOutcome{
		{Name: "lint", Conclusion: "success"},
		{Name: "test", Conclusion: "failure"},
		{Name: "build", Conclusion: "success"},
		{Name: "e2e", Conclusion: "success"},
		{Name: "docs", Conclusion: "failure"},
		{Name: "new", Conclusion: "success"},
		{Name: "slow", Conclusion: "timed_out"},
	}
	r := CompareClean("acme/api", "abc", pre, clean)
	if got := fmt.Sprint(r.Compared); got != "[build lint slow test]" {
		t.Errorf("compared = %s", got)
	}
	if got := fmt.Sprint(r.Skipped); got != "[docs e2e gone new]" {
		t.Errorf("skipped = %s", got)
	}
	want := []Divergence{
		{Job: "build", Kind: PrebuildFailed, Prebuild: "failure", Clean: "success", Match: MatchPartial},
		{Job: "slow", Kind: CleanFailed, Prebuild: "success", Clean: "timed_out"},
		{Job: "test", Kind: CleanFailed, Prebuild: "success", Clean: "failure", Match: MatchFull},
	}
	if fmt.Sprint(r.Divergences) != fmt.Sprint(want) {
		t.Errorf("divergences =\n%v\nwant\n%v", r.Divergences, want)
	}
	if !r.Has(CleanFailed) || !r.Has(PrebuildFailed) {
		t.Error("Has")
	}
	if r.Title() != "Nightly clean run: 3 jobs diverge from the prebuild run" {
		t.Errorf("title = %q", r.Title())
	}
	s := r.Summary()
	for _, want := range []string{"`abc`", "| test | success | failure |", "| build | failure | success |", "key_files", "Not compared"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
}

func TestCompareCleanTitles(t *testing.T) {
	ok := CompareClean("r", "c", []JobOutcome{{Name: "a", Conclusion: "success"}}, []JobOutcome{{Name: "a", Conclusion: "success"}})
	if len(ok.Divergences) != 0 || ok.Title() != "Nightly clean run: 1 job matches the prebuild run" {
		t.Errorf("ok = %+v %q", ok, ok.Title())
	}
	if !strings.Contains(ok.Summary(), "same result") {
		t.Error("summary should say results matched")
	}
	none := CompareClean("r", "c", nil, nil)
	if none.Title() != "Nightly clean run: no jobs could be compared" {
		t.Errorf("none title = %q", none.Title())
	}
	one := CompareClean("r", "c", []JobOutcome{{Name: "a|b", Conclusion: "success"}}, []JobOutcome{{Name: "a|b", Conclusion: "failure"}})
	if one.Title() != "Nightly clean run: 1 job diverges from the prebuild run" || !strings.Contains(one.Summary(), `a\|b`) {
		t.Errorf("one = %q\n%s", one.Title(), one.Summary())
	}
}
