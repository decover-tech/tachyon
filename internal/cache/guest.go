package cache

import (
	"fmt"
	"strings"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

// JobTokenTTL covers queueing plus GitHub's longest self-hosted job.
const JobTokenTTL = 5*24*time.Hour + 12*time.Hour

// JobScope is the cache scope a job runs with.
func JobScope(spec jobapi.Spec) Scope {
	return Scope{
		Repo:          spec.Repo,
		Ref:           spec.Ref,
		BaseRef:       spec.BaseRef,
		DefaultBranch: spec.DefaultBranch,
		Trust:         spec.Trust,
	}
}

// Guest environment variables carrying the job's cache endpoint.
const (
	EnvURL   = "TACHYON_CACHE_URL"
	EnvToken = "TACHYON_CACHE_TOKEN"
)

// GuestEnv returns the environment that points a VM at the regional cache
// service: its URL and a token scoped to this job. The guest image routes
// the toolkit's cache calls with them (ACTIONS_RESULTS_URL and the runtime
// token are set by the runner per step, so that happens in the image).
func GuestEnv(serviceURL string, signer *Signer, spec jobapi.Spec) (map[string]string, error) {
	if serviceURL == "" {
		return nil, nil
	}
	env := map[string]string{EnvURL: strings.TrimRight(serviceURL, "/")}
	if signer == nil {
		return env, nil
	}
	tok, err := signer.MintJobToken(spec.JobID, JobScope(spec), JobTokenTTL)
	if err != nil {
		return env, fmt.Errorf("mint cache token for %s: %w", spec.JobID, err)
	}
	env[EnvToken] = tok
	return env, nil
}
