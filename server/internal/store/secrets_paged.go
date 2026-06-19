package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// SecretsPage is a paginated slice of the secrets list (names + source/ref,
// never a value), mirroring the audit/webhook pagination envelope.
type SecretsPage struct {
	Secrets []Secret
	Total   int64
	Limit   int32
	Offset  int32
}

// ListSecretsPaged returns one page of a project's secrets (ORDER BY name)
// plus the total count for the pager.
func (s *Store) ListSecretsPaged(ctx context.Context, projectID uuid.UUID, limit, offset int32) (SecretsPage, error) {
	total, err := s.q.CountSecretsByProject(ctx, pgUUID(projectID))
	if err != nil {
		return SecretsPage{}, fmt.Errorf("store: count secrets: %w", err)
	}
	rows, err := s.q.ListSecretsByProjectPaged(ctx, db.ListSecretsByProjectPagedParams{
		ProjectID: pgUUID(projectID),
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		return SecretsPage{}, fmt.Errorf("store: list secrets paged: %w", err)
	}
	out := make([]Secret, 0, len(rows))
	for _, r := range rows {
		out = append(out, secretView(r.Name, r.Source, r.RefPath, r.RefKey, r.CreatedAt.Time, r.UpdatedAt.Time))
	}
	return SecretsPage{Secrets: out, Total: total, Limit: limit, Offset: offset}, nil
}

// ListGlobalSecretsPaged is the global-scope twin.
func (s *Store) ListGlobalSecretsPaged(ctx context.Context, limit, offset int32) (SecretsPage, error) {
	total, err := s.q.CountGlobalSecrets(ctx)
	if err != nil {
		return SecretsPage{}, fmt.Errorf("store: count global secrets: %w", err)
	}
	rows, err := s.q.ListGlobalSecretsPaged(ctx, db.ListGlobalSecretsPagedParams{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return SecretsPage{}, fmt.Errorf("store: list global secrets paged: %w", err)
	}
	out := make([]Secret, 0, len(rows))
	for _, r := range rows {
		out = append(out, secretView(r.Name, r.Source, r.RefPath, r.RefKey, r.CreatedAt.Time, r.UpdatedAt.Time))
	}
	return SecretsPage{Secrets: out, Total: total, Limit: limit, Offset: offset}, nil
}
