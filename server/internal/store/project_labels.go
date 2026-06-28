package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ProjectLabel is one key:value label on a project — the grouping primitive for
// cross-project views (team:payments, tier:critical, …).
type ProjectLabel struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ReplaceProjectLabels sets a project's labels to exactly `labels` in one tx
// (delete-all + re-insert), deduping by (key,value). Empty slice clears them.
func (s *Store) ReplaceProjectLabels(ctx context.Context, projectID uuid.UUID, labels []ProjectLabel) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: labels begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := q.DeleteProjectLabels(ctx, pgUUID(projectID)); err != nil {
		return fmt.Errorf("store: clear project labels: %w", err)
	}
	seen := make(map[ProjectLabel]bool, len(labels))
	for _, l := range labels {
		if seen[l] {
			continue
		}
		seen[l] = true
		if err := q.InsertProjectLabel(ctx, db.InsertProjectLabelParams{
			ProjectID: pgUUID(projectID),
			Key:       l.Key,
			Value:     l.Value,
		}); err != nil {
			return fmt.Errorf("store: insert project label: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// ProjectLabels returns one project's labels, ordered by key/value.
func (s *Store) ProjectLabels(ctx context.Context, projectID uuid.UUID) ([]ProjectLabel, error) {
	rows, err := s.q.ListProjectLabels(ctx, pgUUID(projectID))
	if err != nil {
		return nil, fmt.Errorf("store: list project labels: %w", err)
	}
	out := make([]ProjectLabel, 0, len(rows))
	for _, r := range rows {
		out = append(out, ProjectLabel{Key: r.Key, Value: r.Value})
	}
	return out, nil
}

// AllProjectLabels returns every project's labels keyed by project id — one
// read for the list page (no N+1) and the analytics group-by.
func (s *Store) AllProjectLabels(ctx context.Context) (map[uuid.UUID][]ProjectLabel, error) {
	rows, err := s.q.ListAllProjectLabels(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: all project labels: %w", err)
	}
	m := make(map[uuid.UUID][]ProjectLabel)
	for _, r := range rows {
		id := uuid.UUID(r.ProjectID.Bytes)
		m[id] = append(m[id], ProjectLabel{Key: r.Key, Value: r.Value})
	}
	return m, nil
}
