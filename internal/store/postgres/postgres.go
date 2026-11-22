// Package postgres is the Postgres-backed store.Store.
package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/decover-tech/tachyon/internal/prebuild"
	"github.com/decover-tech/tachyon/internal/store"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLock is the advisory lock key held while migrating, so two
// control plane replicas starting together don't race.
const migrationLock = 0x7461636879 // "tachy"

// Store is a store.Store on a Postgres connection pool.
type Store struct {
	pool *pgxpool.Pool
}

var _ store.Store = (*Store)(nil)

// Open connects to dsn and applies any pending migrations.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

type migration struct {
	version int
	name    string
	sql     string
}

func migrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		n, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(n)
		if !ok || err != nil {
			return nil, fmt.Errorf("migration %s: name must be NNNN_description.sql", e.Name())
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("duplicate migration version %d", out[i].version)
		}
	}
	return out, nil
}

// Migrate applies pending migrations, each in its own transaction.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	ms, err := migrations()
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer PRIMARY KEY,
		name       text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	for _, m := range ms {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
				return err
			}
			var done bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, m.version).Scan(&done); err != nil {
				return err
			}
			if done {
				return nil
			}
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}

func (s *Store) PutJob(ctx context.Context, j store.Job) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO jobs (id, state, host_id, data, updated_at) VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (id) DO UPDATE SET state = $2, host_id = $3, data = $4, updated_at = now()`,
		j.ID, string(j.State), j.HostID, data)
	return wrap("put job", err)
}

func (s *Store) SetJobState(ctx context.Context, id string, state store.JobState) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs SET state = $2, data = jsonb_set(data, '{state}', to_jsonb($2::text)), updated_at = now()
		WHERE id = $1`, id, string(state))
	return wrap("set job state", err)
}

func (s *Store) OpenJobs(ctx context.Context) ([]store.Job, error) {
	return queryJSON[store.Job](ctx, s.pool,
		`SELECT data FROM jobs WHERE state IN ('queued', 'deferred', 'assigned') ORDER BY id`)
}

func (s *Store) PutHost(ctx context.Context, h store.Host) error {
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO hosts (id, data, last_seen) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET data = $2, last_seen = $3`,
		h.Host.ID, data, h.LastSeen)
	return wrap("put host", err)
}

func (s *Store) Hosts(ctx context.Context) ([]store.Host, error) {
	return queryJSON[store.Host](ctx, s.pool, `SELECT data FROM hosts ORDER BY id`)
}

func (s *Store) PutPrebuild(ctx context.Context, p prebuild.Snapshot) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO prebuilds (repo, key, commit, created_at, healthy, data) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (repo, key, commit) DO UPDATE SET created_at = $4, healthy = $5, data = $6`,
		p.Repo, p.Key, p.Commit, p.CreatedAt, p.Healthy, data)
	return wrap("put prebuild", err)
}

func (s *Store) LatestPrebuild(ctx context.Context, repo string) (prebuild.Snapshot, error) {
	return getJSON[prebuild.Snapshot](ctx, s.pool, `
		SELECT data FROM prebuilds WHERE repo = $1 AND healthy
		ORDER BY created_at DESC, seq DESC LIMIT 1`, repo)
}

func (s *Store) PutPrebuildConfig(ctx context.Context, repo string, c *prebuild.Config) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO prebuild_configs (repo, data, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (repo) DO UPDATE SET data = $2, updated_at = now()`, repo, data)
	return wrap("put prebuild config", err)
}

func (s *Store) PrebuildConfig(ctx context.Context, repo string) (*prebuild.Config, error) {
	c, err := getJSON[prebuild.Config](ctx, s.pool, `SELECT data FROM prebuild_configs WHERE repo = $1`, repo)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) PutRecord(ctx context.Context, r telemetry.RecordData) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO job_records (job_id, tenant, repo, data, updated_at) VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (job_id) DO UPDATE SET tenant = $2, repo = $3, data = $4, updated_at = now()`,
		r.JobID, r.Tenant, r.Repo, data)
	return wrap("put record", err)
}

func (s *Store) Record(ctx context.Context, jobID string) (telemetry.RecordData, error) {
	return getJSON[telemetry.RecordData](ctx, s.pool, `SELECT data FROM job_records WHERE job_id = $1`, jobID)
}

func (s *Store) RecordsFor(ctx context.Context, jobIDs []string) ([]telemetry.RecordData, error) {
	return queryJSON[telemetry.RecordData](ctx, s.pool, `SELECT data FROM job_records WHERE job_id = ANY($1) ORDER BY job_id`, jobIDs)
}

func (s *Store) PutPendingRunner(ctx context.Context, p store.PendingRunner) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO pending_runners (frontend, external_id, data) VALUES ($1, $2, $3)
		ON CONFLICT (frontend, external_id) DO UPDATE SET data = $3`,
		p.Frontend, p.ExternalID, data)
	return wrap("put pending runner", err)
}

func (s *Store) DeletePendingRunner(ctx context.Context, frontend string, externalID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM pending_runners WHERE frontend = $1 AND external_id = $2`, frontend, externalID)
	return wrap("delete pending runner", err)
}

func (s *Store) PendingRunners(ctx context.Context, frontend string) ([]store.PendingRunner, error) {
	return queryJSON[store.PendingRunner](ctx, s.pool, `SELECT data FROM pending_runners WHERE frontend = $1 ORDER BY external_id`, frontend)
}

func (s *Store) PutActiveRunner(ctx context.Context, r store.ActiveRunner) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO active_runners (frontend, name, installation_id, repo) VALUES ($1, $2, $3, $4)
		ON CONFLICT (frontend, name) DO UPDATE SET installation_id = $3, repo = $4`,
		r.Frontend, r.Name, r.InstallationID, r.Repo)
	return wrap("put active runner", err)
}

func (s *Store) DeleteActiveRunner(ctx context.Context, frontend, name string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM active_runners WHERE frontend = $1 AND name = $2`, frontend, name)
	return wrap("delete active runner", err)
}

func (s *Store) ActiveRunners(ctx context.Context, frontend string) ([]store.ActiveRunner, error) {
	rows, err := s.pool.Query(ctx, `SELECT frontend, name, installation_id, repo FROM active_runners WHERE frontend = $1 ORDER BY name`, frontend)
	if err != nil {
		return nil, wrap("active runners", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (store.ActiveRunner, error) {
		var a store.ActiveRunner
		err := r.Scan(&a.Frontend, &a.Name, &a.InstallationID, &a.Repo)
		return a, err
	})
	return out, wrap("active runners", err)
}

func queryJSON[T any](ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) ([]T, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, wrap("query", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (T, error) {
		var b []byte
		var v T
		if err := r.Scan(&b); err != nil {
			return v, err
		}
		return v, json.Unmarshal(b, &v)
	})
	return out, wrap("query", err)
}

func getJSON[T any](ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) (T, error) {
	var b []byte
	var v T
	err := pool.QueryRow(ctx, sql, args...).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, store.ErrNotFound
	}
	if err != nil {
		return v, wrap("get", err)
	}
	return v, json.Unmarshal(b, &v)
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("postgres %s: %w", op, err)
}
