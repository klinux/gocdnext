package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// DeletionCounts is the pre-delete snapshot of child-row counts
// under a project. Returned alongside the delete so the API can
// echo "deleted N pipelines, M runs, K secrets" — collecting the
// counts after the cascading delete would see zeros.
type DeletionCounts struct {
	Pipelines   int64
	Runs        int64
	Secrets     int64
	SCMSources  int64
}

// DeleteProject removes the project and cascades to pipelines,
// materials, runs, artifacts, secrets and scm_sources via the
// ON DELETE CASCADE constraints wired in the schema. The child
// counts are captured in the same transaction before the delete
// so the response can describe the blast radius accurately.
//
// Returns ErrProjectNotFound when no project matches the slug.
func (s *Store) DeleteProject(ctx context.Context, slug string) (DeletionCounts, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return DeletionCounts{}, fmt.Errorf("store: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	row, err := q.GetProjectDeletionCounts(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeletionCounts{}, ErrProjectNotFound
		}
		return DeletionCounts{}, fmt.Errorf("store: delete counts: %w", err)
	}

	affected, err := q.DeleteProjectBySlug(ctx, slug)
	if err != nil {
		return DeletionCounts{}, fmt.Errorf("store: delete project: %w", err)
	}
	// Belt-and-suspenders: a row disappearing between the count
	// and the delete would manifest here. Treat as not-found so
	// the caller 404s consistently instead of returning phantom
	// counts.
	if affected == 0 {
		return DeletionCounts{}, ErrProjectNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return DeletionCounts{}, fmt.Errorf("store: commit delete: %w", err)
	}

	return DeletionCounts{
		Pipelines:  row.PipelineCount,
		Runs:       row.RunCount,
		Secrets:    row.SecretCount,
		SCMSources: row.ScmSourceCount,
	}, nil
}
