package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func newProfileStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	return store.New(pool), context.Background()
}

func TestRunnerProfiles_CRUD(t *testing.T) {
	s, ctx := newProfileStore(t)

	created, err := s.InsertRunnerProfile(ctx, store.RunnerProfileInput{
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

	if err := s.UpdateRunnerProfile(ctx, created.ID, store.RunnerProfileInput{
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

	if _, err := s.InsertRunnerProfile(ctx, store.RunnerProfileInput{
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

	if _, err := s.InsertRunnerProfile(ctx, store.RunnerProfileInput{
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
