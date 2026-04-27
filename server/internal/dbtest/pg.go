// Package dbtest boots a single Postgres container per test binary,
// applies goose migrations once, and hands each test a clean database
// via TRUNCATE … RESTART IDENTITY CASCADE on every user table. Import
// only from _test.go files.
//
// We deliberately avoid testcontainers' Snapshot/Restore here: under
// load on CI runners (slower IO, contended scheduling) the implicit
// DROP DATABASE / CREATE DATABASE FROM TEMPLATE flow flips between
// "already exists" and "does not exist" mid-run, failing the suite
// from no fault of the tests. TRUNCATE is fully transactional, hits
// every table the migrations created, and runs in <1ms on the data
// shapes our tests build — so the reset path becomes deterministic.
package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/gocdnext/gocdnext/server/migrations"
)

var (
	sharedOnce   sync.Once
	sharedCtr    *postgres.PostgresContainer
	sharedDSN    string
	sharedTables []string
	sharedErr    error
)

// SetupPool returns a clean *pgxpool.Pool scoped to this test. The
// underlying container is shared across the test binary; truncation
// keeps state isolated between tests. Pool is closed in t.Cleanup.
func SetupPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ensureContainer(t)

	pool, err := pgxpool.New(context.Background(), sharedDSN)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := truncateAll(pool); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// DSN returns the shared container's connection string. Tests that
// need a dedicated pgx.Conn (e.g. LISTEN loops) use this instead of
// the pgxpool.
func DSN() string {
	return sharedDSN
}

func ensureContainer(t *testing.T) {
	t.Helper()
	sharedOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		ctr, err := postgres.Run(ctx,
			"postgres:16-alpine",
			postgres.WithDatabase("gocdnext_test"),
			postgres.WithUsername("gocdnext"),
			postgres.WithPassword("gocdnext"),
			postgres.BasicWaitStrategies(),
		)
		if err != nil {
			sharedErr = err
			return
		}

		dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			sharedErr = err
			return
		}

		if err := runMigrations(dsn); err != nil {
			sharedErr = err
			return
		}

		tables, err := readTableList(ctx, dsn)
		if err != nil {
			sharedErr = err
			return
		}

		sharedCtr = ctr
		sharedDSN = dsn
		sharedTables = tables
	})
	if sharedErr != nil {
		t.Fatalf("dbtest container setup: %v", sharedErr)
	}
}

func runMigrations(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(db, ".")
}

// readTableList enumerates every user-created table in the public
// schema after migrations land — captured once at setup so the
// per-test TRUNCATE doesn't re-query the catalog.
//
// Excludes goose's bookkeeping table; truncating it would force
// migrations to re-run on the next test, defeating the cache.
func readTableList(ctx context.Context, dsn string) ([]string, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
        SELECT tablename FROM pg_tables
        WHERE schemaname = 'public'
          AND tablename <> 'goose_db_version'
        ORDER BY tablename
    `)
	if err != nil {
		return nil, fmt.Errorf("dbtest: list tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("dbtest: migrations applied but no public tables found")
	}
	return out, nil
}

// truncateAll wipes every user table in one statement so foreign
// keys collapse with CASCADE without an order dance, and identity
// columns reset so generated IDs are deterministic across tests.
func truncateAll(pool *pgxpool.Pool) error {
	if len(sharedTables) == 0 {
		return fmt.Errorf("dbtest: empty table list — call ensureContainer first")
	}
	quoted := make([]string, len(sharedTables))
	for i, t := range sharedTables {
		quoted[i] = `"` + t + `"`
	}
	stmt := "TRUNCATE TABLE " + strings.Join(quoted, ", ") + " RESTART IDENTITY CASCADE"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("dbtest: truncate: %w", err)
	}
	return nil
}
