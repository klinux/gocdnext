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
//
// # Running without a container runtime
//
// Set GOCDNEXT_TEST_DSN to an existing, EMPTY Postgres database and the
// container is skipped entirely — migrations and the per-test TRUNCATE run
// against it exactly as they would against the container:
//
//	createdb gocdnext_test
//	GOCDNEXT_TEST_ALLOW_TRUNCATE=1 \
//	  GOCDNEXT_TEST_DSN='postgres://…/gocdnext_test?sslmode=disable' go test -p 1 ./...
//
// BOTH variables are required, and the database name must end in `_test`: the
// first SetupPool TRUNCATEs every table in `public`, so a stale export pointing
// at a development database would otherwise be silent, total data loss.
//
// This exists so the suite is reproducible on a machine with no Docker (a plain
// `brew install postgresql` is enough). Two caveats, both of which the container
// flow handles for you:
//
//   - Run packages SERIALLY (`-p 1`). Each package normally gets its own
//     container; pointed at one shared database they truncate each other's rows
//     mid-run and fail in ways that look like real bugs.
//   - The database is not disposable. A package that DROPs schema objects (the
//     retention suite drops log_lines partitions) leaves it unusable for the
//     next run — recreate it.
//
// CI keeps using testcontainers; this is a local-development affordance.
package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/gocdnext/gocdnext/server/migrations"
)

var (
	sharedOnce   sync.Once
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

		// Operator-supplied database: skip the container entirely. Everything
		// downstream (migrations, table list, TRUNCATE) is identical, so the
		// tests cannot tell the difference.
		if dsn := os.Getenv("GOCDNEXT_TEST_DSN"); dsn != "" {
			if err := guardOperatorDSN(dsn); err != nil {
				sharedErr = err
				return
			}
			if err := runMigrations(dsn); err != nil {
				sharedErr = fmt.Errorf("GOCDNEXT_TEST_DSN migrations: %w", err)
				return
			}
			tables, err := readTableList(ctx, dsn)
			if err != nil {
				sharedErr = fmt.Errorf("GOCDNEXT_TEST_DSN table list: %w", err)
				return
			}
			sharedDSN, sharedTables = dsn, tables
			return
		}

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

// truncateAllowEnv is the second, INDEPENDENT thing an operator must set before
// dbtest will wipe a database it did not create.
const truncateAllowEnv = "GOCDNEXT_TEST_ALLOW_TRUNCATE"

// guardOperatorDSN refuses to touch a database that does not look disposable.
//
// The first SetupPool against this DSN runs TRUNCATE … CASCADE over EVERY table
// in `public`. Pointed at a development or staging database by a stale shell
// export, that is total data loss with no confirmation step — so a comment in
// the package doc is not an adequate control. Two independent signals are
// required, because either one alone is easy to satisfy by accident:
//
//  1. the database NAME ends in `_test` (a `psql` URL copied from a real
//     environment will not), and
//  2. GOCDNEXT_TEST_ALLOW_TRUNCATE=1 is set (a deliberate, per-shell act).
//
// The container path is unaffected: it owns the database it wipes.
func guardOperatorDSN(dsn string) error {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("dbtest: GOCDNEXT_TEST_DSN is not a valid connection string: %w", err)
	}
	name := cfg.ConnConfig.Database
	if !strings.HasSuffix(name, "_test") {
		return fmt.Errorf(
			"dbtest: refusing to run against database %q — GOCDNEXT_TEST_DSN wipes EVERY table in `public`, "+
				"so the database name must end in `_test` (e.g. createdb gocdnext_test)", name)
	}
	if os.Getenv(truncateAllowEnv) != "1" {
		return fmt.Errorf(
			"dbtest: refusing to run against database %q — every table in `public` will be TRUNCATEd. "+
				"Set %s=1 to confirm this database is disposable", name, truncateAllowEnv)
	}
	return nil
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
		// pgx.Identifier handles quoting + escaping; hand-rolled `"`+t+`"`
		// breaks on any name containing a quote.
		quoted[i] = pgx.Identifier{t}.Sanitize()
	}
	stmt := "TRUNCATE TABLE " + strings.Join(quoted, ", ") + " RESTART IDENTITY CASCADE"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("dbtest: truncate: %w", err)
	}
	return nil
}
