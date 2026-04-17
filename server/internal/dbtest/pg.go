// Package dbtest boots a single Postgres container per test binary, applies
// goose migrations once, and hands each test a clean database via
// snapshot/restore. Import only from _test.go files.
package dbtest

import (
	"context"
	"database/sql"
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
	sharedOnce sync.Once
	sharedCtr  *postgres.PostgresContainer
	sharedDSN  string
	sharedErr  error
)

// SetupPool returns a clean *pgxpool.Pool scoped to this test. The underlying
// container is shared across the test binary; snapshot/restore keeps state
// isolated between tests. Pool is closed in t.Cleanup.
func SetupPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ensureContainer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sharedCtr.Restore(ctx); err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), sharedDSN)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
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

		if err := ctr.Snapshot(ctx); err != nil {
			sharedErr = err
			return
		}

		sharedCtr = ctr
		sharedDSN = dsn
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
