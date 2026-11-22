package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/decover-tech/tachyon/internal/store"
	"github.com/decover-tech/tachyon/internal/store/storetest"
)

// These tests need a Postgres server. Set TACHYON_TEST_POSTGRES_DSN, e.g.
//
//	TACHYON_TEST_POSTGRES_DSN=postgres://localhost:5432/postgres?sslmode=disable
//
// Each test gets its own schema, dropped afterwards.
const dsnEnv = "TACHYON_TEST_POSTGRES_DSN"

func TestMigrationsParse(t *testing.T) {
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 || ms[0].version != 1 {
		t.Fatalf("migrations = %+v", ms)
	}
}

// freshDSN creates an empty schema and returns a DSN whose search_path is it.
func freshDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("%s not set", dsnEnv)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "tachyon_test_" + hex.EncodeToString(b[:])
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	})
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " search_path=" + schema
}

func open(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestPostgres(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return open(t, freshDSN(t)) })
}

func TestReopenKeepsStateAndMigrationsAreIdempotent(t *testing.T) {
	dsn := freshDSN(t)
	ctx := context.Background()
	s1 := open(t, dsn)
	if err := s1.PutJob(ctx, store.Job{ID: "j1", State: store.JobQueued, Submission: storetest.Submission("j1")}); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2 := open(t, dsn) // runs Migrate again
	jobs, err := s2.OpenJobs(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].Submission.Spec.Commit != "abc" {
		t.Fatalf("after reopen = %+v %v", jobs, err)
	}
}
