-- name: GetUserPreferences :one
-- Returns the stored preferences blob for a user. Callers treat
-- the "no row" case as "empty preferences" (pgx.ErrNoRows); this
-- query stays minimal so the shape follows the JSONB document
-- rather than forcing a struct on every new key.
SELECT preferences, updated_at
FROM user_preferences
WHERE user_id = $1;

-- name: UpsertUserPreferences :one
-- Writes the full preferences document for a user. Upsert so the
-- caller doesn't have to branch on "first save vs. update" — the
-- web action uses a PUT-style full-document replace, not a
-- field-level merge, so this is the only write the domain needs.
INSERT INTO user_preferences (user_id, preferences, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (user_id) DO UPDATE
   SET preferences = EXCLUDED.preferences,
       updated_at  = NOW()
RETURNING preferences, updated_at;
