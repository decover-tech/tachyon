package github

import "fmt"

// CacheRefs returns the git refs that scope the job's Actions cache entries,
// as GitHub scopes them: pull requests write to refs/pull/<n>/merge and read
// their base branch, everything else uses its branch. Any of the three may
// be empty when the run API didn't say.
func (rc RunContext) CacheRefs() (ref, baseRef, defaultBranch string) {
	if rc.DefaultBranch != "" {
		defaultBranch = "refs/heads/" + rc.DefaultBranch
	}
	if isPullRequestEvent(rc.Event) {
		if rc.BaseBranch != "" {
			baseRef = "refs/heads/" + rc.BaseBranch
		}
		if rc.PRNumber > 0 {
			return fmt.Sprintf("refs/pull/%d/merge", rc.PRNumber), baseRef, defaultBranch
		}
		// The run API omits pull_requests for forks. Use a ref no branch
		// build can have, so whatever this job reads or writes stays with
		// this pull request.
		return fmt.Sprintf("refs/pull/unknown/%s/%s", rc.HeadRepo, rc.HeadBranch), baseRef, defaultBranch
	}
	if rc.HeadBranch != "" {
		ref = "refs/heads/" + rc.HeadBranch
	}
	return ref, "", defaultBranch
}

func isPullRequestEvent(event string) bool {
	switch event {
	case "pull_request", "pull_request_target", "pull_request_review", "pull_request_review_comment":
		return true
	}
	return false
}
