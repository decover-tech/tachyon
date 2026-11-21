package github

import "context"

// API is the subset of the GitHub REST API the adapter uses, authenticated as
// the Tachyon GitHub App installation. The App requests only: self-hosted
// runner management, Actions read, and contents read (for prebuilds).
// Installations that opt in to re-runs of infrastructure failures also grant
// Actions write, which RerunJob needs.
type API interface {
	// RunContext looks up the workflow run behind a job.
	RunContext(ctx context.Context, installationID int64, repo string, runID int64) (RunContext, error)
	// CreateJITConfig creates a single-use, repository-scoped just-in-time
	// runner. Repository scope matters: GitHub hands a queued job to any
	// matching idle runner, so org-wide runners would land jobs on hosts
	// without that repo's prebuild.
	CreateJITConfig(ctx context.Context, installationID int64, repo, name string, labels []string) (JITRunner, error)
	ListRunners(ctx context.Context, installationID int64, repo string) ([]Runner, error)
	DeleteRunner(ctx context.Context, installationID int64, repo string, runnerID int64) error
	// RerunJob re-runs one failed workflow job. GitHub accepts it only once
	// the job's workflow run has completed.
	RerunJob(ctx context.Context, installationID int64, repo string, jobID int64) error
}

type JITRunner struct {
	RunnerID  int64
	JITConfig []byte // encoded_jit_config; a secret
}

type Runner struct {
	ID   int64
	Name string
	Busy bool
}

// Secrets stores secret material and returns a reference (secret://...) that
// travels in job specs instead of the value.
type Secrets interface {
	Put(ctx context.Context, path string, value []byte) (ref string, err error)
	Delete(ctx context.Context, ref string) error
}
