package github

import "github.com/decover-tech/tachyon/internal/jobapi"

// RunContext is what the adapter knows about the workflow run behind a job.
// workflow_job payloads don't carry the triggering event or head repository,
// so this comes from the workflow run API.
type RunContext struct {
	Event         string // push, pull_request, schedule, ...
	HeadBranch    string
	HeadRepo      string // owner/name the code comes from
	BaseRepo      string // owner/name the workflow runs in
	DefaultBranch string
	Protected     bool // HeadBranch is a protected branch of BaseRepo
	// For pull request events: the PR number and target branch, when the
	// run API lists them (it doesn't for forks).
	PRNumber   int
	BaseBranch string
}

// DeriveTrust: default and protected branches are trusted, same-repo pull
// requests are untrusted, and pull requests from forks are untrusted-fork.
// Anything that runs code from a pull request, including pull_request_target,
// is treated as a pull request.
func DeriveTrust(rc RunContext) jobapi.Trust {
	switch rc.Event {
	case "pull_request", "pull_request_target", "pull_request_review", "pull_request_review_comment":
		if rc.HeadRepo != "" && rc.HeadRepo != rc.BaseRepo {
			return jobapi.UntrustedFork
		}
		return jobapi.Untrusted
	}
	if rc.HeadRepo != "" && rc.HeadRepo != rc.BaseRepo {
		return jobapi.UntrustedFork
	}
	if rc.HeadBranch != "" && (rc.HeadBranch == rc.DefaultBranch || rc.Protected) {
		return jobapi.Trusted
	}
	return jobapi.Untrusted
}
