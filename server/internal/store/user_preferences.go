package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// UserPreferences mirrors the JSONB document written to
// user_preferences.preferences. New keys go here as needed — the
// wire format is the document itself (no per-field endpoints),
// which keeps the server out of merge logic and leaves it to the
// client to PUT a complete document.
type UserPreferences struct {
	// HiddenProjects are project UUIDs the user has toggled off on
	// the projects list page. Empty / missing = show all. Using a
	// hide-list (not show-list) so newly-created projects appear
	// by default instead of being invisible until explicitly
	// selected.
	HiddenProjects []uuid.UUID `json:"hidden_projects,omitempty"`
}

// GetUserPreferences returns the saved preferences for a user,
// or a zero value (empty document) when the user has never
// written preferences yet. The "no row" case is normal, not an
// error — the UI always has a preferences document to render.
func (s *Store) GetUserPreferences(
	ctx context.Context,
	userID uuid.UUID,
) (UserPreferences, time.Time, error) {
	row, err := s.q.GetUserPreferences(ctx, pgUUID(userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UserPreferences{}, time.Time{}, nil
		}
		return UserPreferences{}, time.Time{}, fmt.Errorf("store: get user preferences: %w", err)
	}
	var prefs UserPreferences
	if len(row.Preferences) > 0 {
		if err := json.Unmarshal(row.Preferences, &prefs); err != nil {
			// Malformed JSONB in the DB is operator error — log via
			// the returned error but don't crash the account page.
			// Callers fall back to zero-value preferences.
			return UserPreferences{}, row.UpdatedAt.Time, fmt.Errorf(
				"store: decode user preferences: %w", err)
		}
	}
	return prefs, row.UpdatedAt.Time, nil
}

// SetUserPreferences replaces the entire preferences document
// for a user. Full-document PUT semantics — partial updates live
// on the client (load, merge, save). Returns the persisted
// document + timestamp so the caller can round-trip the fresh
// value to the UI without a second read.
func (s *Store) SetUserPreferences(
	ctx context.Context,
	userID uuid.UUID,
	prefs UserPreferences,
) (UserPreferences, time.Time, error) {
	payload, err := json.Marshal(prefs)
	if err != nil {
		return UserPreferences{}, time.Time{}, fmt.Errorf(
			"store: encode user preferences: %w", err)
	}
	row, err := s.q.UpsertUserPreferences(ctx, db.UpsertUserPreferencesParams{
		UserID:      pgUUID(userID),
		Preferences: payload,
	})
	if err != nil {
		return UserPreferences{}, time.Time{}, fmt.Errorf(
			"store: upsert user preferences: %w", err)
	}
	var out UserPreferences
	if len(row.Preferences) > 0 {
		_ = json.Unmarshal(row.Preferences, &out)
	}
	return out, row.UpdatedAt.Time, nil
}
