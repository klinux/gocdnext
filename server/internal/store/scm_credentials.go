package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrSCMCredentialNotFound signals no org-level credential row
// matches. Callers silently fall back to scm_source.auth_ref.
var ErrSCMCredentialNotFound = errors.New("store: scm_credential not found")

// SCMCredential is the non-secret view of an org-level token.
// AuthRef is the decrypted plaintext; populated only by the
// resolver path and ListWithPlaintext — the normal admin list
// returns an empty AuthRef so ciphertext doesn't ride JSON
// responses.
type SCMCredential struct {
	ID          uuid.UUID
	Provider    string
	Host        string
	APIBase     string
	DisplayName string
	AuthRef     string // plaintext; empty unless the caller asked for it
	CreatedBy   *uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// SCMCredentialInput is the write shape. AuthRef is plaintext;
// the store seals it before hitting the DB.
type SCMCredentialInput struct {
	Provider    string
	Host        string
	APIBase     string
	DisplayName string
	AuthRef     string
	CreatedBy   *uuid.UUID
}

// ListSCMCredentials returns every org-level credential
// (decrypted). Admin-only at the API layer. When the cipher
// isn't configured we refuse — the caller should surface a 503
// and point at GOCDNEXT_SECRET_KEY.
func (s *Store) ListSCMCredentials(ctx context.Context) ([]SCMCredential, error) {
	if s.authCipher == nil {
		return nil, ErrAuthProviderCipherUnset
	}
	rows, err := s.q.ListSCMCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list scm_credentials: %w", err)
	}
	out := make([]SCMCredential, 0, len(rows))
	for _, r := range rows {
		plain, err := s.authCipher.Decrypt(r.AuthRefEncrypted)
		if err != nil {
			return nil, fmt.Errorf("store: decrypt scm_credential %s: %w", fromPgUUID(r.ID), err)
		}
		out = append(out, SCMCredential{
			ID:          fromPgUUID(r.ID),
			Provider:    r.Provider,
			Host:        r.Host,
			APIBase:     r.ApiBase,
			DisplayName: r.DisplayName,
			AuthRef:     string(plain),
			CreatedBy:   pgUUIDPtr(r.CreatedBy),
			CreatedAt:   r.CreatedAt.Time,
			UpdatedAt:   r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// UpsertSCMCredential seals the plaintext AuthRef and writes the
// row. ON CONFLICT (provider, host) rotates existing rows in
// place so admins editing a credential don't drop and recreate.
func (s *Store) UpsertSCMCredential(ctx context.Context, in SCMCredentialInput) (SCMCredential, error) {
	if s.authCipher == nil {
		return SCMCredential{}, ErrAuthProviderCipherUnset
	}
	if in.Provider != "gitlab" && in.Provider != "bitbucket" {
		return SCMCredential{}, fmt.Errorf("store: scm_credential: unsupported provider %q (use gitlab or bitbucket)", in.Provider)
	}
	host := strings.TrimSpace(strings.ToLower(in.Host))
	if host == "" {
		return SCMCredential{}, fmt.Errorf("store: scm_credential: host is required")
	}
	if in.AuthRef == "" {
		return SCMCredential{}, fmt.Errorf("store: scm_credential: auth_ref is required")
	}
	sealed, err := s.authCipher.Encrypt([]byte(in.AuthRef))
	if err != nil {
		return SCMCredential{}, fmt.Errorf("store: seal scm_credential: %w", err)
	}
	row, err := s.q.UpsertSCMCredential(ctx, db.UpsertSCMCredentialParams{
		Provider:          in.Provider,
		Host:              host,
		ApiBase:           strings.TrimSpace(in.APIBase),
		DisplayName:       strings.TrimSpace(in.DisplayName),
		AuthRefEncrypted:  sealed,
		CreatedBy:         pgUUIDFromPtr(in.CreatedBy),
	})
	if err != nil {
		return SCMCredential{}, fmt.Errorf("store: upsert scm_credential: %w", err)
	}
	return SCMCredential{
		ID:          fromPgUUID(row.ID),
		Provider:    row.Provider,
		Host:        row.Host,
		APIBase:     row.ApiBase,
		DisplayName: row.DisplayName,
		CreatedBy:   pgUUIDPtr(row.CreatedBy),
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}, nil
}

// DeleteSCMCredential removes a credential. Idempotent at the
// SQL level; callers map "row not found" to 204 No Content.
func (s *Store) DeleteSCMCredential(ctx context.Context, id uuid.UUID) error {
	if err := s.q.DeleteSCMCredential(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("store: delete scm_credential: %w", err)
	}
	return nil
}

// ResolveSCMCredential returns the decrypted auth_ref + API base
// for (provider, host). ErrSCMCredentialNotFound when no row
// matches OR when the cipher isn't wired (treated as "nothing
// usable" so the caller falls back to per-project auth_ref and
// the poller doesn't crash the ticker loop).
//
// Host is case-normalised before lookup. Empty APIBase passes
// through — the caller keeps its provider default.
func (s *Store) ResolveSCMCredential(ctx context.Context, provider, host string) (SCMCredential, error) {
	if s.authCipher == nil {
		return SCMCredential{}, ErrSCMCredentialNotFound
	}
	host = strings.ToLower(strings.TrimSpace(host))
	row, err := s.q.GetSCMCredentialByProviderHost(ctx, db.GetSCMCredentialByProviderHostParams{
		Provider: provider,
		Host:     host,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SCMCredential{}, ErrSCMCredentialNotFound
		}
		return SCMCredential{}, fmt.Errorf("store: resolve scm_credential: %w", err)
	}
	plain, err := s.authCipher.Decrypt(row.AuthRefEncrypted)
	if err != nil {
		return SCMCredential{}, fmt.Errorf("store: decrypt scm_credential: %w", err)
	}
	return SCMCredential{
		ID:          fromPgUUID(row.ID),
		Provider:    row.Provider,
		Host:        row.Host,
		APIBase:     row.ApiBase,
		DisplayName: row.DisplayName,
		AuthRef:     string(plain),
		CreatedBy:   pgUUIDPtr(row.CreatedBy),
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}, nil
}

// ResolveAuthRef is the CredentialResolver interface the fetcher
// + auto-register callers plug into. It consults both the
// per-project override (passed in as scmAuthRef) and the org-
// level scm_credentials table.
//
// Priority: per-project scmAuthRef wins when non-empty. Only
// when it's empty do we fall back to the org-level row for the
// host derived from repoURL. Returns "" when neither is set —
// providers that tolerate unauth access (public repos) still
// work; providers that don't get a clean 401 at the call site.
func (s *Store) ResolveAuthRef(
	ctx context.Context, provider, repoURL, scmAuthRef string,
) (authRef, apiBase string) {
	if scmAuthRef != "" {
		return scmAuthRef, ""
	}
	host := hostFromRepoURL(repoURL)
	if host == "" {
		return "", ""
	}
	cred, err := s.ResolveSCMCredential(ctx, provider, host)
	if err != nil {
		return "", ""
	}
	return cred.AuthRef, cred.APIBase
}

// hostFromRepoURL extracts the lowercased hostname from both
// https:// and git@host:owner/repo forms. Returns "" on parse
// failure — caller treats as "no match".
func hostFromRepoURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "git@") {
		after := strings.TrimPrefix(raw, "git@")
		if idx := strings.Index(after, ":"); idx > 0 {
			return strings.ToLower(after[:idx])
		}
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
