package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/auth/apitoken"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedUser is shared across the store_test package — its
// definition lives in quorum_approvals_test.go (signature:
// (*testing.T, *pgxpool.Pool, email, name) → uuid.UUID).

func TestAPIToken_UserRoundTrip(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	uid := seedUser(t, pool, "alice@example.com", "alice")

	gen, err := apitoken.NewUser()
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}
	created, err := s.CreateUserAPIToken(ctx, uid, "alice-laptop", gen.Hash, gen.Prefix, nil)
	if err != nil {
		t.Fatalf("CreateUserAPIToken: %v", err)
	}
	if created.Subject != store.TokenSubjectUser || created.SubjectID != uid {
		t.Errorf("subject mismatch: %+v", created)
	}

	hit, err := s.LookupAPITokenByHash(ctx, gen.Hash)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if hit.ID != created.ID {
		t.Errorf("Lookup returned different token")
	}
}

func TestAPIToken_RevokedHidden(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	uid := seedUser(t, pool, "bob@example.com", "bob")
	gen, _ := apitoken.NewUser()
	tok, _ := s.CreateUserAPIToken(ctx, uid, "bob-ci", gen.Hash, gen.Prefix, nil)

	if err := s.RevokeUserAPIToken(ctx, tok.ID, uid); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	_, err := s.LookupAPITokenByHash(ctx, gen.Hash)
	if !errors.Is(err, store.ErrAPITokenNotFound) {
		t.Errorf("Lookup of revoked token: err = %v, want ErrAPITokenNotFound", err)
	}
}

func TestAPIToken_ExpiredHidden(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	uid := seedUser(t, pool, "carol@example.com", "carol")
	gen, _ := apitoken.NewUser()
	past := time.Now().Add(-1 * time.Minute)
	if _, err := s.CreateUserAPIToken(ctx, uid, "carol-old", gen.Hash, gen.Prefix, &past); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err := s.LookupAPITokenByHash(ctx, gen.Hash)
	if !errors.Is(err, store.ErrAPITokenNotFound) {
		t.Errorf("Lookup of expired token: err = %v, want ErrAPITokenNotFound", err)
	}
}

func TestAPIToken_RevokeWrongOwnerNotFound(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	a := seedUser(t, pool, "alice@example.com", "alice")
	b := seedUser(t, pool, "bob@example.com", "bob")
	gen, _ := apitoken.NewUser()
	tok, _ := s.CreateUserAPIToken(ctx, a, "alice-token", gen.Hash, gen.Prefix, nil)

	// Bob tries to revoke Alice's token. Should look like the
	// token doesn't exist — no leakage.
	err := s.RevokeUserAPIToken(ctx, tok.ID, b)
	if !errors.Is(err, store.ErrAPITokenNotFound) {
		t.Errorf("cross-owner revoke: err = %v, want ErrAPITokenNotFound", err)
	}
}

func TestServiceAccount_TokenRoundTrip(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	creator := seedUser(t, pool, "admin@example.com", "admin")
	sa, err := s.CreateServiceAccount(ctx, "ci-bot", "automation", "maintainer", &creator)
	if err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}
	gen, _ := apitoken.NewSA()
	tok, err := s.CreateSAAPIToken(ctx, sa.ID, "primary", gen.Hash, gen.Prefix, nil)
	if err != nil {
		t.Fatalf("CreateSAAPIToken: %v", err)
	}
	if tok.Subject != store.TokenSubjectServiceAccount || tok.SubjectID != sa.ID {
		t.Errorf("SA subject mismatch: %+v", tok)
	}
	hit, err := s.LookupAPITokenByHash(ctx, gen.Hash)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if hit.Subject != store.TokenSubjectServiceAccount {
		t.Errorf("Lookup subject = %s, want service_account", hit.Subject)
	}
}

// TestAPIToken_XORConstraint guards the migration's CHECK that
// exactly one of (user_id, service_account_id) is set. The store
// helpers can't violate this on their own — the test goes
// straight to the SQL to make sure the schema-level check fires.
func TestAPIToken_XORConstraint(t *testing.T) {
	pool := dbtest.SetupPool(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx,
		`INSERT INTO api_tokens (user_id, service_account_id, name, hash, prefix)
		 VALUES (NULL, NULL, 'broken', 'h', 'p')`)
	if err == nil {
		t.Fatalf("expected CHECK constraint to reject NULL/NULL, got success")
	}

	uid := uuid.New()
	saID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO api_tokens (user_id, service_account_id, name, hash, prefix)
		 VALUES ($1, $2, 'both', 'h', 'p')`, uid, saID)
	if err == nil {
		t.Fatalf("expected CHECK constraint to reject both set, got success")
	}
}

// TestServiceAccount_DeleteCascades ensures token rows go away
// when the SA is deleted.
func TestServiceAccount_DeleteCascades(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	sa, _ := s.CreateServiceAccount(ctx, "transient", "...", "viewer", nil)
	gen, _ := apitoken.NewSA()
	if _, err := s.CreateSAAPIToken(ctx, sa.ID, "t", gen.Hash, gen.Prefix, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.DeleteServiceAccount(ctx, sa.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := s.LookupAPITokenByHash(ctx, gen.Hash)
	if !errors.Is(err, store.ErrAPITokenNotFound) {
		t.Errorf("token survived SA delete: err = %v", err)
	}
}
