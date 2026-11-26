package prebuild

import (
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

// Snapshot is a registered prebuild.
type Snapshot struct {
	Repo      string
	Key       string
	Commit    string
	BaseImage string // name@version it was built on
	CreatedAt time.Time
	Healthy   bool // replicated and passed integrity checks
}

// Match records how a job used its prebuild; it is part of every job record.
type Match string

const (
	MatchFull    Match = "full"
	MatchPartial Match = "partial"
	MatchNone    Match = "none"
)

// Fork says what a job's writable disk is cloned from.
type Fork struct {
	Snapshot *Snapshot // nil = fork the base image
	Match    Match
	Reason   string
}

// PlanFork decides what a job forks. A Tachyon problem must make a job
// slower, never failed: a missing or damaged prebuild falls back to the base
// image. A job whose commit touches a key file still forks the latest
// prebuild but is marked partial, so its installs run, unless the prebuild
// was built from that very commit.
func PlanFork(spec jobapi.Spec, cfg *Config, changed []string, latest *Snapshot) Fork {
	switch {
	case spec.Prebuild.Policy == jobapi.PrebuildNone:
		return Fork{Match: MatchNone, Reason: "tachyon-clean"}
	case cfg == nil:
		return Fork{Match: MatchNone, Reason: "repo has no prebuild config"}
	case latest == nil:
		return Fork{Match: MatchNone, Reason: "no prebuild yet"}
	case !latest.Healthy:
		return Fork{Match: MatchNone, Reason: "prebuild damaged; using base image"}
	case latest.BaseImage != spec.Image:
		return Fork{Match: MatchNone, Reason: "prebuild built on a different base image"}
	case (latest.Commit == "" || latest.Commit != spec.Commit) && cfg.TouchesKeyFiles(changed):
		return Fork{Snapshot: latest, Match: MatchPartial, Reason: "commit changes a key file"}
	}
	return Fork{Snapshot: latest, Match: MatchFull}
}

// ShouldBuild reports whether a trusted push may trigger a prebuild now:
// at most once per interval per repo. Nightly refreshes bypass this.
func ShouldBuild(trust jobapi.Trust, last time.Time, now time.Time, minInterval time.Duration) bool {
	return trust == jobapi.Trusted && now.Sub(last) >= minInterval
}

// CredentialFiles are scrubbed from the writable layer before a prebuild
// snapshot is registered, before the secret scanner runs. Paths are relative
// to the runner user's home.
var CredentialFiles = []string{
	".npmrc",
	".yarnrc.yml",
	".pypirc",
	".netrc",
	".git-credentials",
	".docker/config.json",
	".config/gh/hosts.yml",
	".gradle/gradle.properties",
	".m2/settings.xml",
	".aws/credentials",
}
