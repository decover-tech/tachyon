// Package store is the control plane's durable state: jobs, pending
// runners, hosts, the prebuild registry and job records. The control plane
// writes through to a Store as state changes and reloads it on start, so a
// restart loses no queued jobs and running jobs keep their reservations.
//
// Mem is the in-memory implementation for development and tests;
// store/postgres is the production one.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
	"github.com/decover-tech/tachyon/internal/prebuild"
	"github.com/decover-tech/tachyon/internal/scheduler"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

// ErrNotFound is returned when a keyed lookup finds nothing.
var ErrNotFound = errors.New("not found")

// JobState is where a job is in the control plane.
type JobState string

const (
	// JobQueued: accepted, not yet placed (or placement in progress).
	JobQueued JobState = "queued"
	// JobDeferred: waiting for a quieter fleet; RetryDeferred resubmits it.
	JobDeferred JobState = "deferred"
	// JobAssigned: placed on a host and dispatched; running or about to.
	JobAssigned JobState = "assigned"
	// JobBurst: handed to burst capacity.
	JobBurst JobState = "burst"
	// JobFinished: the host reported a result.
	JobFinished JobState = "finished"
	// JobRejected: Submit failed after the job was recorded; the frontend
	// owns the retry.
	JobRejected JobState = "rejected"
)

// Open reports whether the control plane still has to act on or track a
// job in state s after a restart.
func (s JobState) Open() bool {
	return s == JobQueued || s == JobDeferred || s == JobAssigned
}

// Job is one submitted job and, once placed, its reservation.
type Job struct {
	ID         string            `json:"id"`
	State      JobState          `json:"state"`
	Submission jobapi.Submission `json:"submission"`
	HostID     string            `json:"host_id,omitempty"`
	CPU        float64           `json:"cpu,omitempty"`    // reserved vCPUs
	MemGB      float64           `json:"mem_gb,omitempty"` // reserved memory
	UpdatedAt  time.Time         `json:"updated_at"`
}

// Host is a host's last known shape and its learned oversubscription ratio.
type Host struct {
	Host     scheduler.Host `json:"host"`
	Ratio    float64        `json:"ratio"`
	LastSeen time.Time      `json:"last_seen"`
}

// PendingRunner is a frontend job waiting for its runner to come online:
// a JIT runner was minted and the job submitted, but the platform has not
// yet reported it in progress.
type PendingRunner struct {
	Frontend       string            `json:"frontend"` // "github"
	ExternalID     int64             `json:"external_id"`
	InstallationID int64             `json:"installation_id"`
	Repo           string            `json:"repo"`
	Labels         []string          `json:"labels"`
	Submission     jobapi.Submission `json:"submission"`
	RunnerID       int64             `json:"runner_id"`
	RunnerName     string            `json:"runner_name"`
	SecretRef      string            `json:"secret_ref"`
	SubmittedAt    time.Time         `json:"submitted_at"`
	Attempts       int               `json:"attempts"`
}

// ActiveRunner is a runner whose job is in progress, kept so orphan sweeps
// after a restart don't delete it.
type ActiveRunner struct {
	Frontend       string `json:"frontend"`
	Name           string `json:"name"`
	InstallationID int64  `json:"installation_id"`
	Repo           string `json:"repo"`
}

// Store is the control plane's durable state. Implementations are safe for
// concurrent use. Put methods upsert.
type Store interface {
	PutJob(ctx context.Context, j Job) error
	// SetJobState changes a recorded job's state; unknown IDs are ignored.
	SetJobState(ctx context.Context, id string, state JobState) error
	// OpenJobs returns the jobs whose state is Open, ordered by ID.
	OpenJobs(ctx context.Context) ([]Job, error)

	PutHost(ctx context.Context, h Host) error
	Hosts(ctx context.Context) ([]Host, error)

	// PutPrebuild records a prebuild; LatestPrebuild returns the newest
	// healthy one for repo, or ErrNotFound.
	PutPrebuild(ctx context.Context, s prebuild.Snapshot) error
	LatestPrebuild(ctx context.Context, repo string) (prebuild.Snapshot, error)
	PutPrebuildConfig(ctx context.Context, repo string, c *prebuild.Config) error
	// PrebuildConfig returns repo's config, or ErrNotFound.
	PrebuildConfig(ctx context.Context, repo string) (*prebuild.Config, error)

	PutRecord(ctx context.Context, r telemetry.RecordData) error
	// Record returns a job's record, or ErrNotFound.
	Record(ctx context.Context, jobID string) (telemetry.RecordData, error)
	// RecordsFor returns the records of the given jobs that exist.
	RecordsFor(ctx context.Context, jobIDs []string) ([]telemetry.RecordData, error)

	PutPendingRunner(ctx context.Context, p PendingRunner) error
	DeletePendingRunner(ctx context.Context, frontend string, externalID int64) error
	PendingRunners(ctx context.Context, frontend string) ([]PendingRunner, error)

	PutActiveRunner(ctx context.Context, r ActiveRunner) error
	DeleteActiveRunner(ctx context.Context, frontend, name string) error
	ActiveRunners(ctx context.Context, frontend string) ([]ActiveRunner, error)
}
