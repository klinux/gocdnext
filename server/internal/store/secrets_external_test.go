package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// TestSecret_ExternalReference_RoundTrip: an external reference stores
// source+ref and NO value; ResolveSecretEntries returns the raw entry (no
// decryption) for the composite resolver to dispatch.
func TestSecret_ExternalReference_RoundTrip(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := testCipher(t)
	ctx := context.Background()
	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})

	// A db value and an external reference coexist in the same project.
	if _, err := s.SetSecret(ctx, cipher, store.SecretSet{ProjectID: applied.ProjectID, Name: "DBV", Value: []byte("v")}); err != nil {
		t.Fatalf("db set: %v", err)
	}
	if _, err := s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: applied.ProjectID, Name: "VREF", Source: store.SecretSourceVault,
		RefPath: "secret/myapp", RefKey: "PASSWORD",
	}); err != nil {
		t.Fatalf("vault ref set: %v", err)
	}

	entries, err := s.ResolveSecretEntries(ctx, applied.ProjectID, []string{"DBV", "VREF"})
	if err != nil {
		t.Fatalf("resolve entries: %v", err)
	}
	byName := map[string]store.SecretEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if byName["DBV"].Source != store.SecretSourceDB || len(byName["DBV"].ValueEnc) == 0 {
		t.Fatalf("db entry = %+v", byName["DBV"])
	}
	v := byName["VREF"]
	if v.Source != store.SecretSourceVault || v.RefPath != "secret/myapp" || v.RefKey != "PASSWORD" || v.ValueEnc != nil {
		t.Fatalf("vault entry = %+v (want source vault, ref, nil value)", v)
	}

	// The list view exposes source+ref, never a value.
	list, _ := s.ListSecrets(ctx, applied.ProjectID)
	for _, sec := range list {
		if sec.Name == "VREF" {
			if sec.Source != store.SecretSourceVault || sec.Ref == nil || sec.Ref.Path != "secret/myapp" || sec.Ref.Key != "PASSWORD" {
				t.Fatalf("list view of vault ref = %+v", sec)
			}
		}
	}
}

// TestResolveSecrets_ExternalRow_FailsLoud: the pure-DB resolver path must NOT
// silently omit an external-source row (that would surface as the misleading
// "secrets not set on project"). It means config drift — a backend was
// configured when the reference was created and is no longer enabled — so it
// fails loud, citing the NAME, never a value.
func TestResolveSecrets_ExternalRow_FailsLoud(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := testCipher(t)
	ctx := context.Background()
	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})

	if _, err := s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: applied.ProjectID, Name: "VREF", Source: store.SecretSourceVault,
		RefPath: "secret/myapp", RefKey: "PASSWORD",
	}); err != nil {
		t.Fatalf("vault ref set: %v", err)
	}

	_, err := s.ResolveSecrets(ctx, cipher, applied.ProjectID, []string{"VREF"})
	if err == nil {
		t.Fatal("DBResolver must fail loud on an external-source row, not omit it")
	}
	if !strings.Contains(err.Error(), "VREF") || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("error %q should name VREF and say the backend is not configured", err)
	}
	if strings.Contains(err.Error(), "secret/myapp") || strings.Contains(err.Error(), "PASSWORD") {
		t.Fatalf("error %q must not leak the ref path/key", err)
	}
}

// TestSecret_ExternalShape_Rejected: the shape invariant is enforced.
func TestSecret_ExternalShape_Rejected(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := testCipher(t)
	ctx := context.Background()
	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})

	// external + a value → rejected.
	if _, err := s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: applied.ProjectID, Name: "BAD1", Source: store.SecretSourceVault,
		Value: []byte("v"), RefPath: "p",
	}); err == nil {
		t.Fatal("external secret with a value was accepted")
	}
	// external + no ref_path → rejected.
	if _, err := s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: applied.ProjectID, Name: "BAD2", Source: store.SecretSourceAWS,
	}); err == nil {
		t.Fatal("external secret with no ref_path was accepted")
	}
	// db + a ref → rejected.
	if _, err := s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: applied.ProjectID, Name: "BAD3", Value: []byte("v"), RefPath: "p",
	}); err == nil {
		t.Fatal("db secret with a ref was accepted")
	}
	// unknown source → rejected.
	if _, err := s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: applied.ProjectID, Name: "BAD4", Source: "azure", RefPath: "p",
	}); err == nil {
		t.Fatal("unknown source was accepted")
	}
}

// TestSecret_ProjectShadowsGlobal_MixedSources: a project external ref
// shadows a global db secret of the same name.
func TestSecret_ProjectShadowsGlobal_MixedSources(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := testCipher(t)
	ctx := context.Background()
	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})

	if _, err := s.SetGlobalSecret(ctx, cipher, store.SecretSet{Name: "SHARED", Value: []byte("global-v")}); err != nil {
		t.Fatalf("global set: %v", err)
	}
	if _, err := s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: applied.ProjectID, Name: "SHARED", Source: store.SecretSourceGCP, RefPath: "shared-secret",
	}); err != nil {
		t.Fatalf("project ref set: %v", err)
	}
	entries, _ := s.ResolveSecretEntries(ctx, applied.ProjectID, []string{"SHARED"})
	if len(entries) != 1 || entries[0].Source != store.SecretSourceGCP {
		t.Fatalf("project entry should shadow global: %+v", entries)
	}
}

// TestSecretsPaged covers the pagination edges.
func TestSecretsPaged(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := testCipher(t)
	ctx := context.Background()
	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})

	for i := 0; i < 5; i++ {
		name := "S" + string(rune('A'+i))
		if _, err := s.SetSecret(ctx, cipher, store.SecretSet{ProjectID: applied.ProjectID, Name: name, Value: []byte("v")}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	page, err := s.ListSecretsPaged(ctx, applied.ProjectID, 2, 0)
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if page.Total != 5 || len(page.Secrets) != 2 || page.Secrets[0].Name != "SA" {
		t.Fatalf("page1 = %+v (total %d)", page.Secrets, page.Total)
	}
	last, _ := s.ListSecretsPaged(ctx, applied.ProjectID, 2, 4)
	if last.Total != 5 || len(last.Secrets) != 1 || last.Secrets[0].Name != "SE" {
		t.Fatalf("last page = %+v", last.Secrets)
	}
	beyond, _ := s.ListSecretsPaged(ctx, applied.ProjectID, 2, 100)
	if len(beyond.Secrets) != 0 || beyond.Total != 5 {
		t.Fatalf("offset past end = %+v", beyond.Secrets)
	}
}

// TestValidateSecretRef pins the API-layer validation (configured set +
// vault-key rule).
func TestValidateSecretRef(t *testing.T) {
	configured := map[string]bool{store.SecretSourceVault: true}
	cases := []struct {
		name              string
		source, path, key string
		wantErr           string
	}{
		{"db ok", store.SecretSourceDB, "", "", ""},
		{"db with ref", store.SecretSourceDB, "p", "", "no ref"},
		{"vault ok", store.SecretSourceVault, "secret/x", "K", ""},
		{"vault no key", store.SecretSourceVault, "secret/x", "", "ref key"},
		{"vault no path", store.SecretSourceVault, "", "K", "ref path"},
		{"unconfigured", store.SecretSourceAWS, "p", "", "not configured"},
		{"unknown", "azure", "p", "", "unknown secret source"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := store.ValidateSecretRef(tt.source, tt.path, tt.key, configured)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
