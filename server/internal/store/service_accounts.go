package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ServiceAccount mirrors the row shape with nullable timestamps
// surfaced as pointers — same convention as User.
type ServiceAccount struct {
	ID          uuid.UUID
	Name        string
	Description string
	Role        string
	CreatedBy   *uuid.UUID
	DisabledAt  *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

var ErrServiceAccountNotFound = errors.New("store: service account not found")

// CreateServiceAccount mints a new SA. Role must already be one
// of the canonical values — the handler validates before this
// gets called. createdBy may be nil when the caller is itself a
// service account (machines bootstrapping more machines is rare
// but legal).
func (s *Store) CreateServiceAccount(ctx context.Context, name, description, role string, createdBy *uuid.UUID) (ServiceAccount, error) {
	var by pgtype.UUID
	if createdBy != nil {
		by = pgtype.UUID{Bytes: *createdBy, Valid: true}
	}
	row, err := s.q.InsertServiceAccount(ctx, db.InsertServiceAccountParams{
		Name:        name,
		Description: description,
		Role:        role,
		CreatedBy:   by,
	})
	if err != nil {
		return ServiceAccount{}, fmt.Errorf("store: insert service account: %w", err)
	}
	return saFromRow(saQueryRow{
		ID:          row.ID,
		Name:        row.Name,
		Description: row.Description,
		Role:        row.Role,
		CreatedBy:   row.CreatedBy,
		DisabledAt:  row.DisabledAt,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}), nil
}

// GetServiceAccount returns a SA by ID — used by the bearer
// middleware after a token lookup says "this is an SA token".
func (s *Store) GetServiceAccount(ctx context.Context, id uuid.UUID) (ServiceAccount, error) {
	row, err := s.q.GetServiceAccountByID(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ServiceAccount{}, ErrServiceAccountNotFound
		}
		return ServiceAccount{}, fmt.Errorf("store: get service account: %w", err)
	}
	return saFromRow(saQueryRow{
		ID:          row.ID,
		Name:        row.Name,
		Description: row.Description,
		Role:        row.Role,
		CreatedBy:   row.CreatedBy,
		DisabledAt:  row.DisabledAt,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}), nil
}

// ListServiceAccounts returns every SA, newest first — admin UI
// path.
func (s *Store) ListServiceAccounts(ctx context.Context) ([]ServiceAccount, error) {
	rows, err := s.q.ListServiceAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list service accounts: %w", err)
	}
	out := make([]ServiceAccount, 0, len(rows))
	for _, r := range rows {
		out = append(out, saFromRow(saQueryRow{
			ID:          r.ID,
			Name:        r.Name,
			Description: r.Description,
			Role:        r.Role,
			CreatedBy:   r.CreatedBy,
			DisabledAt:  r.DisabledAt,
			CreatedAt:   r.CreatedAt,
			UpdatedAt:   r.UpdatedAt,
		}))
	}
	return out, nil
}

// UpdateServiceAccount mutates description + role.
func (s *Store) UpdateServiceAccount(ctx context.Context, id uuid.UUID, description, role string) error {
	if err := s.q.UpdateServiceAccount(ctx, db.UpdateServiceAccountParams{
		ID:          pgUUID(id),
		Description: description,
		Role:        role,
	}); err != nil {
		return fmt.Errorf("store: update service account: %w", err)
	}
	return nil
}

// SetServiceAccountDisabled flips the disabled_at flag. Pass nil
// to re-enable.
func (s *Store) SetServiceAccountDisabled(ctx context.Context, id uuid.UUID, disabledAt *time.Time) error {
	if err := s.q.SetServiceAccountDisabled(ctx, db.SetServiceAccountDisabledParams{
		ID:         pgUUID(id),
		DisabledAt: nullableTimestamp(disabledAt),
	}); err != nil {
		return fmt.Errorf("store: disable service account: %w", err)
	}
	return nil
}

// DeleteServiceAccount removes the row + cascades to api_tokens.
func (s *Store) DeleteServiceAccount(ctx context.Context, id uuid.UUID) error {
	if err := s.q.DeleteServiceAccount(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("store: delete service account: %w", err)
	}
	return nil
}

// saQueryRow is the union of the SQLC-generated row types — they
// all carry the same fields but as separate types per query, so
// we collapse to one shape before mapping to ServiceAccount.
type saQueryRow struct {
	ID          pgtype.UUID
	Name        string
	Description string
	Role        string
	CreatedBy   pgtype.UUID
	DisabledAt  pgtype.Timestamptz
	CreatedAt   pgtype.Timestamptz
	UpdatedAt   pgtype.Timestamptz
}

func saFromRow(r saQueryRow) ServiceAccount {
	out := ServiceAccount{
		ID:          fromPgUUID(r.ID),
		Name:        r.Name,
		Description: r.Description,
		Role:        r.Role,
		DisabledAt:  pgTimePtr(r.DisabledAt),
		CreatedAt:   r.CreatedAt.Time,
		UpdatedAt:   r.UpdatedAt.Time,
	}
	if r.CreatedBy.Valid {
		v := fromPgUUID(r.CreatedBy)
		out.CreatedBy = &v
	}
	return out
}
