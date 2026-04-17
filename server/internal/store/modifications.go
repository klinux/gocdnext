package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// Modification is what callers hand to InsertModification. Empty-string fields
// are persisted as NULL (matches the schema's nullable columns).
type Modification struct {
	MaterialID  uuid.UUID
	Revision    string
	Branch      string
	Author      string
	Message     string
	Payload     json.RawMessage
	CommittedAt time.Time
}

// ModificationResult reports whether the row was newly inserted. Insertion is
// idempotent on (material_id, revision, branch): repeat calls return the same
// ID with Created=false, no error.
type ModificationResult struct {
	ID      int64
	Created bool
}

// InsertModification inserts a modification with ON CONFLICT DO NOTHING.
// When the unique key is already present, fetches the existing row to return
// its id — callers typically want to know which modification they ended up
// referencing, not just whether it was new.
func (s *Store) InsertModification(ctx context.Context, mod Modification) (ModificationResult, error) {
	params := db.InsertModificationParams{
		MaterialID:  pgUUID(mod.MaterialID),
		Revision:    mod.Revision,
		Branch:      nullableString(mod.Branch),
		Author:      nullableString(mod.Author),
		Message:     nullableString(mod.Message),
		Payload:     payloadOrNull(mod.Payload),
		CommittedAt: timestampOrNull(mod.CommittedAt),
	}

	row, err := s.q.InsertModification(ctx, params)
	switch {
	case err == nil:
		return ModificationResult{ID: row.ID, Created: true}, nil
	case errors.Is(err, pgx.ErrNoRows):
		// Conflict on the unique key — look up the pre-existing row.
		existing, lookupErr := s.q.GetModificationByKey(ctx, db.GetModificationByKeyParams{
			MaterialID: params.MaterialID,
			Revision:   params.Revision,
			Branch:     params.Branch,
		})
		if lookupErr != nil {
			return ModificationResult{}, fmt.Errorf("store: lookup existing modification: %w", lookupErr)
		}
		return ModificationResult{ID: existing.ID, Created: false}, nil
	default:
		return ModificationResult{}, fmt.Errorf("store: insert modification: %w", err)
	}
}

func payloadOrNull(p json.RawMessage) []byte {
	if len(p) == 0 {
		return nil
	}
	return p
}

func timestampOrNull(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}
