package github

import (
	"context"
	"fmt"

	"github.com/decover-tech/tachyon/internal/store"

	"github.com/decover-tech/tachyon/internal/telemetry"
)

// Persistence of pending and active runners. With Adapter.Store set, a job
// waiting for its runner survives a control plane restart: Reconcile keeps
// re-dispatching it, and the orphan sweep knows its runner (and every
// in-progress runner) is not an orphan.

const frontendName = "github"

func (a *Adapter) savePending(ctx context.Context, p *pendingJob) error {
	if a.Store == nil {
		return nil
	}
	defer telemetry.StartSpan(ctx, "store.save_pending")()
	return a.Store.PutPendingRunner(ctx, store.PendingRunner{
		Frontend:       frontendName,
		ExternalID:     p.id,
		InstallationID: p.installationID,
		Repo:           p.repo,
		Labels:         p.labels,
		Submission:     p.sub,
		RunnerID:       p.runnerID,
		RunnerName:     p.runnerName,
		SecretRef:      p.secretRef,
		SubmittedAt:    p.submittedAt,
		Attempts:       p.attempts,
	})
}

func (a *Adapter) forgetPending(ctx context.Context, id int64) {
	if a.Store == nil {
		return
	}
	if err := a.Store.DeletePendingRunner(ctx, frontendName, id); err != nil {
		a.log().Warn("forget pending runner", "job", id, "err", err)
	}
}

func (a *Adapter) saveActive(ctx context.Context, name string, inst int64, repo string) {
	if a.Store == nil || name == "" {
		return
	}
	if err := a.Store.PutActiveRunner(ctx, store.ActiveRunner{Frontend: frontendName, Name: name, InstallationID: inst, Repo: repo}); err != nil {
		a.log().Warn("persist active runner", "runner", name, "err", err)
	}
}

func (a *Adapter) forgetActive(ctx context.Context, name string) {
	if a.Store == nil || name == "" {
		return
	}
	if err := a.Store.DeleteActiveRunner(ctx, frontendName, name); err != nil {
		a.log().Warn("forget active runner", "runner", name, "err", err)
	}
}

// Restore reloads pending and active runners after a restart. Call it once,
// before serving webhooks. It returns how many pending jobs it restored.
func (a *Adapter) Restore(ctx context.Context) (int, error) {
	if a.Store == nil {
		return 0, nil
	}
	pending, err := a.Store.PendingRunners(ctx, frontendName)
	if err != nil {
		return 0, fmt.Errorf("restore pending runners: %w", err)
	}
	active, err := a.Store.ActiveRunners(ctx, frontendName)
	if err != nil {
		return 0, fmt.Errorf("restore active runners: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.initLocked()
	for _, p := range pending {
		a.pending[p.ExternalID] = &pendingJob{
			id:             p.ExternalID,
			installationID: p.InstallationID,
			repo:           p.Repo,
			labels:         p.Labels,
			sub:            p.Submission,
			// Rows are written once a runner exists and the job was
			// submitted, so there is nothing left to prepare.
			prepared:    true,
			dispatched:  true,
			runnerID:    p.RunnerID,
			runnerName:  p.RunnerName,
			secretRef:   p.SecretRef,
			submittedAt: p.SubmittedAt,
			attempts:    p.Attempts,
		}
		a.repos[p.Repo] = p.InstallationID
	}
	for _, r := range active {
		a.active[r.Name] = true
		a.repos[r.Repo] = r.InstallationID
	}
	return len(pending), nil
}
