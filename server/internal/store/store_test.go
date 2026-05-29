package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestFingerprintFor_StableAndDistinct(t *testing.T) {
	t.Parallel()

	fp1 := store.FingerprintFor("https://github.com/x/y.git", "main")
	fp2 := store.FingerprintFor("https://github.com/x/y.git", "main")
	if fp1 != fp2 {
		t.Fatalf("fingerprint not stable: %s vs %s", fp1, fp2)
	}

	other := store.FingerprintFor("https://github.com/x/y.git", "dev")
	if fp1 == other {
		t.Fatalf("fingerprint collided across branches: %s", fp1)
	}

	empty := store.FingerprintFor("", "")
	if empty == fp1 {
		t.Fatalf("empty fingerprint matched non-empty")
	}
}

func TestFingerprintFor_NormalizesURL(t *testing.T) {
	t.Parallel()

	a := store.FingerprintFor("https://github.com/x/y.git", "main")
	b := store.FingerprintFor("https://github.com/x/y", "main")
	c := store.FingerprintFor("https://GitHub.com/x/y.git", "main")
	if a != b || a != c {
		t.Fatalf("URL not normalized: a=%s b=%s c=%s", a, b, c)
	}
}

func TestStore_FindMaterialsByFingerprint_Empty(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	out, err := s.FindMaterialsByFingerprint(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Empty slice — not ErrNotFound — so the caller treats "no
	// material" as a routine outcome (most pushes don't match any
	// pipeline) instead of error handling.
	if len(out) != 0 {
		t.Fatalf("len(out) = %d, want 0", len(out))
	}
}

func TestStore_FindMaterialsByFingerprint_FanOut(t *testing.T) {
	// Several pipelines that watch the same (repo, branch) share
	// the same fingerprint. Lookup must return ALL of them so the
	// webhook handler can fan a run out to each. Pre-fix the query
	// was `:one LIMIT 1` and silently dropped every pipeline except
	// the first heap-scan row.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	fp := store.FingerprintFor("https://github.com/gocdnext/gocdnext.git", "main")

	// One project, three pipelines, each with its own material row
	// sharing the fingerprint — the real-world shape: ci-core,
	// ci-web, security on the same repo+branch.
	var projectID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (slug, name) VALUES ($1, $2) RETURNING id`,
		"fanout-test", "fanout test project",
	).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	want := map[string]bool{}
	for _, name := range []string{"ci-core", "ci-web", "security"} {
		var pipelineID uuid.UUID
		if err := pool.QueryRow(ctx,
			`INSERT INTO pipelines (project_id, name, definition) VALUES ($1, $2, $3) RETURNING id`,
			projectID, name, []byte(`{}`),
		).Scan(&pipelineID); err != nil {
			t.Fatalf("seed pipeline %s: %v", name, err)
		}
		var materialID uuid.UUID
		if err := pool.QueryRow(ctx,
			`INSERT INTO materials (pipeline_id, type, config, fingerprint)
			 VALUES ($1, 'git', $2, $3) RETURNING id`,
			pipelineID, []byte(`{"url":"https://github.com/x/y.git","branch":"main"}`), fp,
		).Scan(&materialID); err != nil {
			t.Fatalf("seed material for %s: %v", name, err)
		}
		want[materialID.String()] = true
	}

	got, err := s.FindMaterialsByFingerprint(ctx, fp)
	if err != nil {
		t.Fatalf("FindMaterialsByFingerprint: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (one per pipeline)", len(got))
	}
	for _, m := range got {
		if m.Fingerprint != fp {
			t.Errorf("fingerprint = %q, want %q", m.Fingerprint, fp)
		}
		if !want[m.ID.String()] {
			t.Errorf("unexpected material id %s in results", m.ID)
		}
		delete(want, m.ID.String())
	}
	if len(want) > 0 {
		t.Errorf("missing material ids: %v", want)
	}
}

func TestStore_InsertModification_InsertsThenDedupes(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	fp := store.FingerprintFor("https://github.com/gocdnext/gocdnext.git", "main")
	materialID := seedMaterial(t, pool, fp)

	committed := time.Date(2026, 4, 17, 10, 15, 30, 0, time.UTC)
	mod := store.Modification{
		MaterialID:  materialID,
		Revision:    "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1",
		Branch:      "main",
		Author:      "alice",
		Message:     "fix bug",
		Payload:     json.RawMessage(`{"ref":"refs/heads/main"}`),
		CommittedAt: committed,
	}

	first, err := s.InsertModification(context.Background(), mod)
	if err != nil {
		t.Fatalf("first InsertModification: %v", err)
	}
	if !first.Created {
		t.Fatalf("Created = false on first insert")
	}
	if first.ID == 0 {
		t.Fatalf("ID = 0 on first insert")
	}

	second, err := s.InsertModification(context.Background(), mod)
	if err != nil {
		t.Fatalf("second InsertModification: %v", err)
	}
	if second.Created {
		t.Fatalf("Created = true on duplicate insert")
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate id = %d, want %d", second.ID, first.ID)
	}
}

func TestStore_InsertModification_DifferentRevisions(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	fp := store.FingerprintFor("https://github.com/gocdnext/gocdnext.git", "main")
	materialID := seedMaterial(t, pool, fp)

	base := store.Modification{
		MaterialID:  materialID,
		Branch:      "main",
		Payload:     json.RawMessage(`{}`),
		CommittedAt: time.Now().UTC(),
	}

	a := base
	a.Revision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	b := base
	b.Revision = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	ra, err := s.InsertModification(context.Background(), a)
	if err != nil || !ra.Created {
		t.Fatalf("insert a: %+v %v", ra, err)
	}
	rb, err := s.InsertModification(context.Background(), b)
	if err != nil || !rb.Created {
		t.Fatalf("insert b: %+v %v", rb, err)
	}
	if ra.ID == rb.ID {
		t.Fatalf("distinct revisions got same id: %d", ra.ID)
	}
}

// seedMaterial inserts a project, a pipeline and a git material tied to the
// given fingerprint, returning the material's UUID. Used by integration tests
// that need a valid material_id FK.
func seedMaterial(t *testing.T, pool *pgxpool.Pool, fingerprint string) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var projectID uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO projects (slug, name) VALUES ($1, $2) RETURNING id`,
		"test-"+fingerprint[:8], "test project",
	).Scan(&projectID)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}

	var pipelineID uuid.UUID
	err = pool.QueryRow(ctx,
		`INSERT INTO pipelines (project_id, name, definition) VALUES ($1, $2, $3) RETURNING id`,
		projectID, "test-pipeline", []byte(`{}`),
	).Scan(&pipelineID)
	if err != nil {
		t.Fatalf("seed pipeline: %v", err)
	}

	var materialID uuid.UUID
	err = pool.QueryRow(ctx,
		`INSERT INTO materials (pipeline_id, type, config, fingerprint)
		 VALUES ($1, 'git', $2, $3) RETURNING id`,
		pipelineID, []byte(`{"url":"https://github.com/x/y.git","branch":"main"}`), fingerprint,
	).Scan(&materialID)
	if err != nil {
		t.Fatalf("seed material: %v", err)
	}
	return materialID
}
