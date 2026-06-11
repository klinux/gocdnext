package store

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrOIDCKeyNotFound signals no active signing key exists. Callers
// that hit this should EnsureActiveOIDCKey first — it only escapes
// when something deleted rows out-of-band.
var ErrOIDCKeyNotFound = errors.New("store: no active oidc signing key")

// OIDCKeysChannel is the PostgreSQL NOTIFY channel rotation fires
// on. Every server replica's issuer LISTENs here and converges its
// in-memory caches on receipt — without it, a replica that didn't
// handle the rotate request would keep signing with the previous
// (possibly revoked) key until its cache TTL expired. Payload is
// the new active kid: receivers treat it idempotently (a replica
// already holding that signing key preserves it and only refreshes
// the JWKS document — see oidcissuer.HandleRotationNotice).
const OIDCKeysChannel = "oidc_keys_rotated"

// OIDCSigningKey is the decrypted active key the issuer signs with.
// Private is in memory only — never logged, never serialized.
type OIDCSigningKey struct {
	ID        uuid.UUID
	Kid       string
	Alg       string
	Private   *rsa.PrivateKey
	CreatedAt time.Time
}

// OIDCPublicKey is one JWKS entry — public material only, no cipher
// involvement on the serving path.
type OIDCPublicKey struct {
	Kid       string
	Alg       string
	Public    *rsa.PublicKey
	RetiredAt *time.Time
}

// EnsureActiveOIDCKey returns the active signing key, generating and
// persisting one if the table has none. Concurrency-safe across
// replicas: the partial unique index oidc_signing_keys_one_active
// makes the INSERT ... ON CONFLICT DO NOTHING race resolve to a
// single winner; losers re-read the winner's row. The generated key
// is RSA-2048 (the GHA/GitLab interop baseline) sealed with the
// store's authCipher.
func (s *Store) EnsureActiveOIDCKey(ctx context.Context) (OIDCSigningKey, error) {
	if s.authCipher == nil {
		return OIDCSigningKey{}, fmt.Errorf("store: oidc key: auth cipher not configured (set GOCDNEXT_SECRET_KEY)")
	}

	if key, err := s.getActiveOIDCKey(ctx); err == nil {
		return key, nil
	} else if !errors.Is(err, ErrOIDCKeyNotFound) {
		return OIDCSigningKey{}, err
	}

	_, kid, privEnc, pubDER, err := s.generateOIDCKeyMaterial()
	if err != nil {
		return OIDCSigningKey{}, err
	}
	// 0 rows affected = another replica won the race; either way the
	// re-SELECT below returns the canonical active key (which may be
	// the OTHER replica's — our generated private key is discarded).
	if _, err := s.q.InsertOIDCKey(ctx, db.InsertOIDCKeyParams{
		Kid:           kid,
		Alg:           "RS256",
		PrivateKeyEnc: privEnc,
		PublicKeyDer:  pubDER,
	}); err != nil {
		return OIDCSigningKey{}, fmt.Errorf("store: insert oidc key: %w", err)
	}
	return s.getActiveOIDCKey(ctx)
}

// RotateOIDCKey retires (graceful) or revokes (emergency) the active
// key and inserts a fresh one — in ONE transaction, so there is no
// window where the table has no active key and a concurrent dispatch
// would fail to mint. Graceful keys keep verifying via the JWKS
// until the caller-defined overlap elapses; revoked keys vanish from
// the JWKS immediately (the key-compromise kill switch).
func (s *Store) RotateOIDCKey(ctx context.Context, emergency bool) (OIDCSigningKey, error) {
	if s.authCipher == nil {
		return OIDCSigningKey{}, fmt.Errorf("store: oidc key: auth cipher not configured (set GOCDNEXT_SECRET_KEY)")
	}

	priv, kid, privEnc, pubDER, err := s.generateOIDCKeyMaterial()
	if err != nil {
		return OIDCSigningKey{}, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return OIDCSigningKey{}, fmt.Errorf("store: rotate oidc key: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if emergency {
		if _, err := q.RevokeActiveOIDCKey(ctx); err != nil {
			return OIDCSigningKey{}, fmt.Errorf("store: rotate oidc key: revoke: %w", err)
		}
	} else {
		if _, err := q.RetireActiveOIDCKey(ctx); err != nil {
			return OIDCSigningKey{}, fmt.Errorf("store: rotate oidc key: retire: %w", err)
		}
	}
	// RETURNING insert (the active slot is now free within this tx).
	// A concurrent rotate racing on the partial index will make one
	// of the two transactions fail at commit — loud beats
	// split-brain. The returned row + the private key already in
	// memory form the complete result, so NOTHING is read after
	// commit: a post-commit re-SELECT could fail (context canceled,
	// transient error) and make a rotation that ALREADY HAPPENED
	// look like a 500 — skipping the audit event with it.
	row, err := q.InsertOIDCKeyReturning(ctx, db.InsertOIDCKeyReturningParams{
		Kid:           kid,
		Alg:           "RS256",
		PrivateKeyEnc: privEnc,
		PublicKeyDer:  pubDER,
	})
	if err != nil {
		return OIDCSigningKey{}, fmt.Errorf("store: rotate oidc key: insert: %w", err)
	}
	// NOTIFY rides the same tx: it fires on commit only, so a
	// rolled-back rotation never tells replicas to dump a cache
	// that still reflects reality. Receivers (every replica's
	// issuer) converge their caches on the notice — typically
	// milliseconds after commit, shrinking the remote window that
	// would otherwise last until the cache TTL.
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, OIDCKeysChannel, kid); err != nil {
		return OIDCSigningKey{}, fmt.Errorf("store: rotate oidc key: notify: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return OIDCSigningKey{}, fmt.Errorf("store: rotate oidc key: commit: %w", err)
	}
	return OIDCSigningKey{
		ID:        fromPgUUID(row.ID),
		Kid:       kid,
		Alg:       "RS256",
		Private:   priv,
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

// ListOIDCJWKSKeys returns the public keys the JWKS endpoint serves:
// the active key plus gracefully-retired keys whose retired_at is
// after the caller's cutoff (now - tokenTTL - margin). Revoked keys
// never appear.
func (s *Store) ListOIDCJWKSKeys(ctx context.Context, retiredCutoff time.Time) ([]OIDCPublicKey, error) {
	rows, err := s.q.ListOIDCJWKSKeys(ctx, pgtype.Timestamptz{Time: retiredCutoff, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("store: list oidc jwks keys: %w", err)
	}
	out := make([]OIDCPublicKey, 0, len(rows))
	for _, r := range rows {
		pub, err := parseRSAPublicDER(r.PublicKeyDer)
		if err != nil {
			return nil, fmt.Errorf("store: oidc key %s: %w", r.Kid, err)
		}
		out = append(out, OIDCPublicKey{
			Kid:       r.Kid,
			Alg:       r.Alg,
			Public:    pub,
			RetiredAt: pgTimePtr(r.RetiredAt),
		})
	}
	return out, nil
}

// OIDCKeyMeta is the admin-facing lifecycle view — no key material.
type OIDCKeyMeta struct {
	ID        uuid.UUID
	Kid       string
	Alg       string
	CreatedAt time.Time
	RetiredAt *time.Time
	RevokedAt *time.Time
}

// ListOIDCKeysAdmin returns every signing key's lifecycle metadata,
// newest first. Material never leaves the store through this path.
func (s *Store) ListOIDCKeysAdmin(ctx context.Context) ([]OIDCKeyMeta, error) {
	rows, err := s.q.ListOIDCKeysAdmin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list oidc keys: %w", err)
	}
	out := make([]OIDCKeyMeta, 0, len(rows))
	for _, r := range rows {
		out = append(out, OIDCKeyMeta{
			ID:        fromPgUUID(r.ID),
			Kid:       r.Kid,
			Alg:       r.Alg,
			CreatedAt: r.CreatedAt.Time,
			RetiredAt: pgTimePtr(r.RetiredAt),
			RevokedAt: pgTimePtr(r.RevokedAt),
		})
	}
	return out, nil
}

func (s *Store) getActiveOIDCKey(ctx context.Context) (OIDCSigningKey, error) {
	row, err := s.q.GetActiveOIDCKey(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return OIDCSigningKey{}, ErrOIDCKeyNotFound
		}
		return OIDCSigningKey{}, fmt.Errorf("store: get active oidc key: %w", err)
	}
	der, err := s.authCipher.Decrypt(row.PrivateKeyEnc)
	if err != nil {
		return OIDCSigningKey{}, fmt.Errorf("store: oidc key %s: decrypt: %w", row.Kid, err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return OIDCSigningKey{}, fmt.Errorf("store: oidc key %s: parse pkcs8: %w", row.Kid, err)
	}
	priv, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return OIDCSigningKey{}, fmt.Errorf("store: oidc key %s: not an RSA key (%T)", row.Kid, parsed)
	}
	return OIDCSigningKey{
		ID:        fromPgUUID(row.ID),
		Kid:       row.Kid,
		Alg:       row.Alg,
		Private:   priv,
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

// generateOIDCKeyMaterial mints a fresh RSA-2048 key pair and
// returns (private key, kid, encrypted PKCS#8 private DER, public
// PKIX DER). The plaintext private key rides along so the rotation
// path can build its result without any post-commit read; kid is
// the RFC 7638 thumbprint so verifiers match it deterministically.
func (s *Store) generateOIDCKeyMaterial() (priv *rsa.PrivateKey, kid string, privEnc, pubDER []byte, err error) {
	priv, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("store: generate oidc key: %w", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("store: marshal oidc private key: %w", err)
	}
	privEnc, err = s.authCipher.Encrypt(privDER)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("store: encrypt oidc private key: %w", err)
	}
	pubDER, err = x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("store: marshal oidc public key: %w", err)
	}
	kid, err = JWKThumbprint(&priv.PublicKey)
	if err != nil {
		return nil, "", nil, nil, err
	}
	return priv, kid, privEnc, pubDER, nil
}

// JWKThumbprint computes the RFC 7638 thumbprint of an RSA public
// key: base64url(SHA-256(canonical JWK JSON)). The canonical form
// for RSA is the lexicographically-ordered members {"e","kty","n"}
// with base64url-unpadded values. Exported because the oidcissuer
// package reuses it to assert kid integrity in tests.
func JWKThumbprint(pub *rsa.PublicKey) (string, error) {
	if pub == nil {
		return "", fmt.Errorf("store: jwk thumbprint: nil key")
	}
	canonical, err := json.Marshal(map[string]string{
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		"kty": "RSA",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
	})
	if err != nil {
		return "", fmt.Errorf("store: jwk thumbprint: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// parseRSAPublicDER decodes a PKIX DER blob into an *rsa.PublicKey.
func parseRSAPublicDER(der []byte) (*rsa.PublicKey, error) {
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse pkix public key: %w", err)
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA public key (%T)", parsed)
	}
	return pub, nil
}
