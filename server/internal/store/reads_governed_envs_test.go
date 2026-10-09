package store_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func runJob(t *testing.T, s *store.Store, runID uuid.UUID, name string) store.JobDetail {
	t.Helper()
	d, err := s.GetRunDetailWithLogs(context.Background(), runID, store.LogWindow{})
	if err != nil {
		t.Fatalf("run detail: %v", err)
	}
	for _, st := range d.Stages {
		for _, j := range st.Jobs {
			if j.Name == name {
				return j
			}
		}
	}
	t.Fatalf("no job %q in run detail", name)
	return store.JobDetail{}
}

func projectJob(t *testing.T, s *store.Store, slug, pipelineName, name string) store.JobRunSummaryLite {
	t.Helper()
	d, err := s.GetProjectDetail(context.Background(), slug, 20)
	if err != nil {
		t.Fatalf("project detail: %v", err)
	}
	for _, p := range d.Pipelines {
		if p.Name != pipelineName {
			continue
		}
		for _, st := range p.LatestRunStages {
			for _, j := range st.Jobs {
				if j.Name == name {
					return j
				}
			}
		}
	}
	t.Fatalf("no job %q on the strip for pipeline %q", name, pipelineName)
	return store.JobRunSummaryLite{}
}

func decideGate(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE job_runs SET status='success', decision='approved', decided_by='someone'
		 WHERE run_id=$1 AND name='gate'`, runID); err != nil {
		t.Fatalf("decide gate: %v", err)
	}
}

// governedCase is one gate shape: the downstream jobs, the envs to freeze, and
// whether the gate is decided before the read. The want* fields hold for both the
// run detail and the project strip — they read the same snapshot the same way.
type governedCase struct {
	name         string
	downstream   []domain.Job
	freeze       []string
	decided      bool
	wantGoverned []string
	wantFrozen   []string
}

var governedCases = []governedCase{
	{
		name:         "deploy job, no freeze: governed without any freeze signal",
		downstream:   []domain.Job{deployJob("ship-production", "production")},
		wantGoverned: []string{"production"},
	},
	{
		name:         "bare environment: migration, no freeze",
		downstream:   []domain.Job{migrationJob("migrate-db", "production")},
		wantGoverned: []string{"production"},
	},
	{
		name: "several envs, one frozen: governed complete, frozen the subset",
		downstream: []domain.Job{
			deployJob("ship-staging", "staging"),
			deployJob("ship-production", "production"),
			migrationJob("migrate-production", "production"),
		},
		freeze:       []string{"production"},
		wantGoverned: []string{"production", "staging"},
		wantFrozen:   []string{"production"},
	},
	{
		name:         "decided gate keeps governed envs and never carries freeze",
		downstream:   []domain.Job{deployJob("ship-production", "production")},
		freeze:       []string{"production"},
		decided:      true,
		wantGoverned: []string{"production"},
	},
	{
		name: "plain gate with nothing env-bound downstream: absent",
		downstream: []domain.Job{{
			Name: "work", Stage: "deploy", Image: "alpine:3.19", Tasks: []domain.Task{{Script: "true"}},
		}},
		freeze: []string{"production"},
	},
}

func caseSlug(prefix string, i int) string {
	return prefix + "-" + string(rune('a'+i))
}

func TestGetRunDetail_GovernedEnvs(t *testing.T) {
	for i, tc := range governedCases {
		t.Run(tc.name, func(t *testing.T) {
			pool := dbtest.SetupPool(t)
			s := store.New(pool)
			runID, projectID := seedGatePipeline(t, pool, caseSlug("gov-run", i), tc.downstream)
			if tc.decided {
				decideGate(t, pool, runID)
			}
			for _, env := range tc.freeze {
				freeze(t, s, projectID, env)
			}

			g := runJob(t, s, runID, "gate")
			if !slices.Equal(g.GovernedEnvs, tc.wantGoverned) {
				t.Fatalf("governed_envs=%v, want %v", g.GovernedEnvs, tc.wantGoverned)
			}
			if !slices.Equal(g.FrozenEnvs, tc.wantFrozen) || g.HeldByFreeze != (len(tc.wantFrozen) > 0) {
				t.Fatalf("frozen_envs=%v held=%v, want %v", g.FrozenEnvs, g.HeldByFreeze, tc.wantFrozen)
			}
		})
	}
}

func TestGetProjectDetail_GovernedEnvs(t *testing.T) {
	for i, tc := range governedCases {
		t.Run(tc.name, func(t *testing.T) {
			pool := dbtest.SetupPool(t)
			s := store.New(pool)
			slug := caseSlug("gov-pd", i)
			projectID, runIDs := seedGatedProject(t, pool, slug, map[string][]domain.Job{"release": tc.downstream})
			if tc.decided {
				decideGate(t, pool, runIDs["release"])
			}
			for _, env := range tc.freeze {
				freeze(t, s, projectID, env)
			}

			g := projectJob(t, s, slug, "release", "gate")
			if !slices.Equal(g.GovernedEnvs, tc.wantGoverned) {
				t.Fatalf("governed_envs=%v, want %v", g.GovernedEnvs, tc.wantGoverned)
			}
			if !slices.Equal(g.FrozenEnvs, tc.wantFrozen) || g.HeldByFreeze != (len(tc.wantFrozen) > 0) {
				t.Fatalf("frozen_envs=%v held=%v, want %v", g.FrozenEnvs, g.HeldByFreeze, tc.wantFrozen)
			}
		})
	}
}

// On the wire: the gate carries governed_envs, the downstream deploy job (not a
// gate) never does — asserted on the encoded bytes, since an empty slice and an
// absent key decode the same.
func TestGovernedEnvs_WireOnlyOnGates(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	runID, _ := seedGatePipeline(t, pool, "gov-wire", []domain.Job{deployJob("ship-production", "production")})

	d, err := s.GetRunDetailWithLogs(context.Background(), runID, store.LogWindow{})
	if err != nil {
		t.Fatalf("run detail: %v", err)
	}
	pd, err := s.GetProjectDetail(context.Background(), "gov-wire", 20)
	if err != nil {
		t.Fatalf("project detail: %v", err)
	}
	var seen int
	check := func(surface, name string, v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		has := strings.Contains(string(raw), `"governed_envs":["production"]`)
		hasKey := strings.Contains(string(raw), `"governed_envs"`)
		switch name {
		case "gate":
			if !has {
				t.Fatalf("%s gate: want governed_envs [production] on the wire, got %s", surface, raw)
			}
		default:
			if hasKey {
				t.Fatalf("%s %s (not a gate): governed_envs must be absent, got %s", surface, name, raw)
			}
		}
		seen++
	}
	for _, st := range d.Stages {
		for _, j := range st.Jobs {
			check("run_detail", j.Name, j)
		}
	}
	for _, p := range pd.Pipelines {
		for _, st := range p.LatestRunStages {
			for _, j := range st.Jobs {
				check("project_detail", j.Name, j)
			}
		}
	}
	// gate + ship-production on each surface.
	if seen != 4 {
		t.Fatalf("checked %d jobs, want 4 (gate + deploy on run and project detail)", seen)
	}
}
