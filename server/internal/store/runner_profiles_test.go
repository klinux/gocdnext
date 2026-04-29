package store_test

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func newProfileCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

func newProfileStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	return store.New(pool), context.Background()
}

func TestRunnerProfiles_CRUD(t *testing.T) {
	s, ctx := newProfileStore(t)

	created, err := s.InsertRunnerProfile(ctx, nil, store.RunnerProfileInput{
		Name:              "default",
		Description:       "vanilla pool",
		Engine:            "kubernetes",
		DefaultImage:      "alpine:3.20",
		DefaultCPURequest: "100m",
		DefaultMemRequest: "256Mi",
		MaxCPU:            "4",
		MaxMem:            "8Gi",
		Tags:              []string{"linux"},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if created.ID.String() == "" {
		t.Fatalf("expected generated id")
	}

	got, err := s.GetRunnerProfileByName(ctx, "default")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if got.DefaultImage != "alpine:3.20" || got.MaxCPU != "4" {
		t.Fatalf("got = %+v", got)
	}

	if err := s.UpdateRunnerProfile(ctx, nil, created.ID, store.RunnerProfileInput{
		Name:         "default",
		Description:  "now with budget",
		Engine:       "kubernetes",
		DefaultImage: "alpine:3.20",
		MaxCPU:       "2",
		MaxMem:       "4Gi",
		Tags:         []string{"linux"},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = s.GetRunnerProfile(ctx, created.ID)
	if got.MaxCPU != "2" || got.Description != "now with budget" {
		t.Fatalf("update did not persist: %+v", got)
	}

	if err := s.DeleteRunnerProfile(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = s.GetRunnerProfile(ctx, created.ID)
	if !errors.Is(err, store.ErrRunnerProfileNotFound) {
		t.Fatalf("expected ErrRunnerProfileNotFound, got %v", err)
	}
}

func TestResolveProfiles_FillsDefaultsAndMergesTags(t *testing.T) {
	s, ctx := newProfileStore(t)

	if _, err := s.InsertRunnerProfile(ctx, nil, store.RunnerProfileInput{
		Name:              "default",
		Engine:            "kubernetes",
		DefaultImage:      "alpine:3.20",
		DefaultCPURequest: "100m",
		DefaultMemRequest: "256Mi",
		DefaultCPULimit:   "1",
		DefaultMemLimit:   "1Gi",
		MaxCPU:            "4",
		MaxMem:            "8Gi",
		Tags:              []string{"linux", "amd64"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pipelines := []*domain.Pipeline{{
		Name: "p1",
		Jobs: []domain.Job{{
			Name:    "build",
			Profile: "default",
			Tags:    []string{"linux"}, // user already declared one of the profile tags
		}},
	}}

	if err := s.ResolveProfiles(ctx, pipelines); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	j := pipelines[0].Jobs[0]
	if j.Image != "alpine:3.20" {
		t.Fatalf("expected default image filled, got %q", j.Image)
	}
	if j.Resources.Requests.CPU != "100m" || j.Resources.Requests.Memory != "256Mi" {
		t.Fatalf("requests not filled: %+v", j.Resources.Requests)
	}
	if j.Resources.Limits.CPU != "1" || j.Resources.Limits.Memory != "1Gi" {
		t.Fatalf("limits not filled: %+v", j.Resources.Limits)
	}
	want := []string{"linux", "amd64"}
	if len(j.Tags) != len(want) || j.Tags[0] != want[0] || j.Tags[1] != want[1] {
		t.Fatalf("tags = %v, want %v (de-duped union)", j.Tags, want)
	}
}

func TestResolveProfiles_RejectsUnknownProfile(t *testing.T) {
	s, ctx := newProfileStore(t)

	pipelines := []*domain.Pipeline{{
		Name: "p1",
		Jobs: []domain.Job{{Name: "build", Profile: "ghost"}},
	}}
	err := s.ResolveProfiles(ctx, pipelines)
	if err == nil {
		t.Fatal("expected error for unknown profile")
	}
	if !contains(err.Error(), "unknown runner profile") {
		t.Fatalf("error %q lacks user-friendly hint", err)
	}
}

func TestResolveProfiles_EnforcesCap(t *testing.T) {
	s, ctx := newProfileStore(t)

	if _, err := s.InsertRunnerProfile(ctx, nil, store.RunnerProfileInput{
		Name:   "small",
		Engine: "kubernetes",
		MaxCPU: "1",
		MaxMem: "1Gi",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cases := []struct {
		name string
		res  domain.ResourceSpec
		want string
	}{
		{
			name: "cpu request over cap",
			res:  domain.ResourceSpec{Requests: domain.ResourceQuantities{CPU: "2"}},
			want: "max_cpu",
		},
		{
			name: "memory limit over cap",
			res:  domain.ResourceSpec{Limits: domain.ResourceQuantities{Memory: "8Gi"}},
			want: "max_mem",
		},
		{
			name: "request greater than limit",
			res: domain.ResourceSpec{
				Requests: domain.ResourceQuantities{CPU: "500m"},
				Limits:   domain.ResourceQuantities{CPU: "100m"},
			},
			want: "must be ≤",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			pipelines := []*domain.Pipeline{{
				Name: "p1",
				Jobs: []domain.Job{{Name: "build", Profile: "small", Resources: tt.res}},
			}}
			err := s.ResolveProfiles(ctx, pipelines)
			if err == nil {
				t.Fatal("expected error")
			}
			if !contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestResolveProfiles_NoProfileLeavesJobUntouched(t *testing.T) {
	s, ctx := newProfileStore(t)

	original := domain.Job{
		Name:  "build",
		Image: "myorg/builder:1",
		Tags:  []string{"linux"},
	}
	pipelines := []*domain.Pipeline{{
		Name: "p1",
		Jobs: []domain.Job{original},
	}}
	if err := s.ResolveProfiles(ctx, pipelines); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := pipelines[0].Jobs[0]
	if got.Image != original.Image || len(got.Tags) != 1 {
		t.Fatalf("legacy job mutated: %+v", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestRunnerProfile_EnvAndSecrets_RoundTrip(t *testing.T) {
	s, ctx := newProfileStore(t)
	cipher := newProfileCipher(t)

	created, err := s.InsertRunnerProfile(ctx, cipher, store.RunnerProfileInput{
		Name:   "fast-builds",
		Engine: "kubernetes",
		Env: map[string]string{
			"GOCDNEXT_LAYER_CACHE_BUCKET": "ci-cache",
			"GOCDNEXT_LAYER_CACHE_REGION": "us-east-1",
		},
		Secrets: map[string]string{
			"AWS_ACCESS_KEY_ID":     "AKIA-TEST",
			"AWS_SECRET_ACCESS_KEY": "super-secret-value",
		},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Read path: env round-trips plainly, secret VALUES never come
	// back — only the key list, sorted.
	got, err := s.GetRunnerProfile(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Env["GOCDNEXT_LAYER_CACHE_BUCKET"] != "ci-cache" {
		t.Errorf("env not round-tripped: %+v", got.Env)
	}
	wantKeys := []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}
	if !sort.StringsAreSorted(got.SecretKeys) {
		t.Errorf("secret keys not sorted: %+v", got.SecretKeys)
	}
	if len(got.SecretKeys) != len(wantKeys) {
		t.Fatalf("secret keys = %+v, want %+v", got.SecretKeys, wantKeys)
	}
	for i, k := range wantKeys {
		if got.SecretKeys[i] != k {
			t.Errorf("secret key[%d] = %q, want %q", i, got.SecretKeys[i], k)
		}
	}

	// Resolver path (used by scheduler at dispatch): merged env
	// includes decrypted secret values + secret VALUES echoed for
	// LogMasks redaction.
	env, masks, err := s.ResolveProfileEnvByName(ctx, cipher, "fast-builds")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if env["AWS_ACCESS_KEY_ID"] != "AKIA-TEST" {
		t.Errorf("decrypted secret missing from env: %+v", env)
	}
	if env["GOCDNEXT_LAYER_CACHE_BUCKET"] != "ci-cache" {
		t.Errorf("plain env missing: %+v", env)
	}
	sort.Strings(masks)
	wantMasks := []string{"AKIA-TEST", "super-secret-value"}
	if len(masks) != len(wantMasks) || masks[0] != wantMasks[0] || masks[1] != wantMasks[1] {
		t.Errorf("masks = %+v, want %+v", masks, wantMasks)
	}
}

func TestRunnerProfile_SecretsWithoutCipher_FailsClosed(t *testing.T) {
	s, ctx := newProfileStore(t)

	// Insert with secrets and no cipher → encrypt helper refuses.
	_, err := s.InsertRunnerProfile(ctx, nil, store.RunnerProfileInput{
		Name:    "broken",
		Engine:  "kubernetes",
		Secrets: map[string]string{"X": "y"},
	})
	if err == nil {
		t.Fatalf("expected cipher-required error")
	}

	// Empty secrets map is fine without a cipher (fast path).
	if _, err := s.InsertRunnerProfile(ctx, nil, store.RunnerProfileInput{
		Name:   "ok",
		Engine: "kubernetes",
	}); err != nil {
		t.Fatalf("empty-secrets path: %v", err)
	}
}

func TestRunnerProfile_SecretTemplate_ResolvesAgainstGlobals(t *testing.T) {
	s, ctx := newProfileStore(t)
	cipher := newProfileCipher(t)

	// Seed a global secret the profile will reference.
	if _, err := s.SetGlobalSecret(ctx, cipher, "AWS_ACCESS_KEY_ID", []byte("AKIA-FROM-GLOBAL")); err != nil {
		t.Fatalf("seed global: %v", err)
	}
	if _, err := s.SetGlobalSecret(ctx, cipher, "AWS_SECRET_ACCESS_KEY", []byte("super-secret-global")); err != nil {
		t.Fatalf("seed global 2: %v", err)
	}

	// Profile mixes one literal with two template references.
	if _, err := s.InsertRunnerProfile(ctx, cipher, store.RunnerProfileInput{
		Name:   "fast-builds-with-refs",
		Engine: "kubernetes",
		Secrets: map[string]string{
			"AWS_ACCESS_KEY_ID":     "{{secret:AWS_ACCESS_KEY_ID}}",
			"AWS_SECRET_ACCESS_KEY": "{{secret:AWS_SECRET_ACCESS_KEY}}",
			"LITERAL_VALUE":         "kept-as-typed",
		},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	env, masks, err := s.ResolveProfileEnvByName(ctx, cipher, "fast-builds-with-refs")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if env["AWS_ACCESS_KEY_ID"] != "AKIA-FROM-GLOBAL" {
		t.Errorf("template not expanded: %q", env["AWS_ACCESS_KEY_ID"])
	}
	if env["AWS_SECRET_ACCESS_KEY"] != "super-secret-global" {
		t.Errorf("second template not expanded: %q", env["AWS_SECRET_ACCESS_KEY"])
	}
	if env["LITERAL_VALUE"] != "kept-as-typed" {
		t.Errorf("literal mutated: %q", env["LITERAL_VALUE"])
	}
	// Resolved global values must land in LogMasks so the runner
	// redacts them — same contract as literal profile secrets.
	resolvedMasked := map[string]bool{}
	for _, v := range masks {
		resolvedMasked[v] = true
	}
	if !resolvedMasked["AKIA-FROM-GLOBAL"] || !resolvedMasked["super-secret-global"] {
		t.Errorf("resolved global values missing from masks: %+v", masks)
	}
}

func TestRunnerProfile_SecretTemplate_MissingGlobalFailsClosed(t *testing.T) {
	s, ctx := newProfileStore(t)
	cipher := newProfileCipher(t)

	if _, err := s.InsertRunnerProfile(ctx, cipher, store.RunnerProfileInput{
		Name:   "broken-ref",
		Engine: "kubernetes",
		Secrets: map[string]string{
			"X": "{{secret:DOES_NOT_EXIST}}",
		},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, _, err := s.ResolveProfileEnvByName(ctx, cipher, "broken-ref")
	if err == nil {
		t.Fatal("expected error for unresolvable template, got nil")
	}
	if !contains(err.Error(), "DOES_NOT_EXIST") {
		t.Errorf("error %q does not name the missing global", err)
	}
}

func TestRunnerProfile_SecretRefs_ExposesCleanReferences(t *testing.T) {
	s, ctx := newProfileStore(t)
	cipher := newProfileCipher(t)

	created, err := s.InsertRunnerProfile(ctx, cipher, store.RunnerProfileInput{
		Name:   "mixed",
		Engine: "kubernetes",
		Secrets: map[string]string{
			"PURE_REF":     "{{secret:DB_PASSWORD}}",
			"MIXED_REF":    "prefix-{{secret:DB_PASSWORD}}-suffix",
			"LITERAL_ONLY": "plain-value",
		},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	refs, err := s.ProfileSecretRefs(ctx, cipher, created.ID)
	if err != nil {
		t.Fatalf("refs: %v", err)
	}
	if refs["PURE_REF"] != "DB_PASSWORD" {
		t.Errorf("clean ref not surfaced: got %q", refs["PURE_REF"])
	}
	if _, mixed := refs["MIXED_REF"]; mixed {
		t.Errorf("mixed value should not surface as clean ref: %+v", refs)
	}
	if _, literal := refs["LITERAL_ONLY"]; literal {
		t.Errorf("literal value should not surface as ref: %+v", refs)
	}
}
