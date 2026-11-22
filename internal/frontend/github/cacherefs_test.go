package github

import "testing"

func TestCacheRefs(t *testing.T) {
	cases := []struct {
		name                string
		rc                  RunContext
		ref, base, defaultB string
	}{
		{"push", RunContext{Event: "push", HeadBranch: "feat", DefaultBranch: "main", PRNumber: 9, BaseBranch: "main"},
			"refs/heads/feat", "", "refs/heads/main"},
		{"same-repo PR", RunContext{Event: "pull_request", HeadBranch: "feat", HeadRepo: "acme/api", DefaultBranch: "main", PRNumber: 9, BaseBranch: "release"},
			"refs/pull/9/merge", "refs/heads/release", "refs/heads/main"},
		{"fork PR without number", RunContext{Event: "pull_request", HeadBranch: "main", HeadRepo: "evil/api", DefaultBranch: "main"},
			"refs/pull/unknown/evil/api/main", "", "refs/heads/main"},
		{"nothing known", RunContext{Event: "schedule"}, "", "", ""},
	}
	for _, c := range cases {
		ref, base, def := c.rc.CacheRefs()
		if ref != c.ref || base != c.base || def != c.defaultB {
			t.Errorf("%s: got %q %q %q", c.name, ref, base, def)
		}
	}
}
