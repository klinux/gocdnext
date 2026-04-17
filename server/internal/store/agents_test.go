package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestHashToken_StableAndDistinct(t *testing.T) {
	t.Parallel()

	a := store.HashToken("secret-token")
	b := store.HashToken("secret-token")
	if a != b {
		t.Fatalf("HashToken not deterministic: %s vs %s", a, b)
	}
	if store.HashToken("other") == a {
		t.Fatalf("HashToken collided on distinct inputs")
	}
}

func TestVerifyToken(t *testing.T) {
	t.Parallel()

	h := store.HashToken("super-secret")
	if !store.VerifyToken("super-secret", h) {
		t.Fatalf("VerifyToken rejected matching plain")
	}
	if store.VerifyToken("super-secret-X", h) {
		t.Fatalf("VerifyToken accepted mismatched plain")
	}
	if store.VerifyToken("super-secret", "") {
		t.Fatalf("VerifyToken accepted empty hash")
	}
}

func TestStore_FindAgentByName_NotFound(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	_, err := s.FindAgentByName(context.Background(), "ghost")
	if !errors.Is(err, store.ErrAgentNotFound) {
		t.Fatalf("err = %v, want ErrAgentNotFound", err)
	}
}

func TestStore_FindAgentByName_Found(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	agentID := seedAgent(t, pool, "runner-01", store.HashToken("tok-xyz"))

	a, err := s.FindAgentByName(context.Background(), "runner-01")
	if err != nil {
		t.Fatalf("FindAgentByName: %v", err)
	}
	if a.ID != agentID {
		t.Fatalf("id = %s, want %s", a.ID, agentID)
	}
	if a.Name != "runner-01" {
		t.Fatalf("name = %q", a.Name)
	}
	if !store.VerifyToken("tok-xyz", a.TokenHash) {
		t.Fatalf("token hash does not verify against original plain text")
	}
	if a.Status != "offline" {
		t.Fatalf("status = %q, want offline", a.Status)
	}
}

func TestStore_MarkAgentOnline_UpdatesMetadata(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	id := seedAgent(t, pool, "runner-02", store.HashToken("tok"))

	err := s.MarkAgentOnline(context.Background(), id, store.RegisterUpdate{
		Version:  "0.1.0",
		OS:       "linux",
		Arch:     "amd64",
		Tags:     []string{"docker", "linux"},
		Capacity: 4,
	})
	if err != nil {
		t.Fatalf("MarkAgentOnline: %v", err)
	}

	a, err := s.FindAgentByName(context.Background(), "runner-02")
	if err != nil {
		t.Fatalf("FindAgentByName: %v", err)
	}
	if a.Status != "online" {
		t.Fatalf("status = %q, want online", a.Status)
	}
	if a.Version != "0.1.0" || a.OS != "linux" || a.Arch != "amd64" || a.Capacity != 4 {
		t.Fatalf("register fields not persisted: %+v", a)
	}
	if len(a.Tags) != 2 || a.Tags[0] != "docker" || a.Tags[1] != "linux" {
		t.Fatalf("tags = %v", a.Tags)
	}
	if a.LastSeenAt.IsZero() {
		t.Fatalf("last_seen_at not set")
	}
	if time.Since(a.LastSeenAt) > 5*time.Second {
		t.Fatalf("last_seen_at too old: %v", a.LastSeenAt)
	}
}

func seedAgent(t *testing.T, pool *pgxpool.Pool, name, tokenHash string) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ($1, $2) RETURNING id`,
		name, tokenHash,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return id
}
