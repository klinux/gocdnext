package scheduler

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// benchPipeline builds a realistic wide pipeline: n build jobs plus two deploy
// jobs. The dispatch loop visits every one of them per tick.
func benchPipeline(n int) []byte {
	jobs := make([]domain.Job, 0, n+2)
	for i := range n {
		jobs = append(jobs, domain.Job{
			Name: "build-" + strconv.Itoa(i), Stage: "build", Image: "alpine:3.19",
			Tasks: []domain.Task{{Script: "make build"}},
		})
	}
	jobs = append(jobs,
		domain.Job{
			Name: "ship-prod", Stage: "deploy", Image: "alpine:3.19",
			Tasks:  []domain.Task{{Script: "true"}},
			Deploy: &domain.DeploySpec{Environment: "production", Version: "v1"},
		},
		domain.Job{
			Name: "ship-staging", Stage: "deploy", Image: "alpine:3.19",
			Tasks:  []domain.Task{{Script: "true"}},
			Deploy: &domain.DeploySpec{Environment: "staging", Version: "v1"},
		},
	)
	b, err := json.Marshal(domain.Pipeline{Name: "wide", Stages: []string{"build", "deploy"}, Jobs: jobs})
	if err != nil {
		panic(err)
	}
	return b
}

// BenchmarkFreezeEnvLookup_IndexOnce is what the freeze pre-scan does today: ONE
// decode of the pipeline snapshot, then a map probe per job.
func BenchmarkFreezeEnvLookup_IndexOnce(b *testing.B) {
	def := benchPipeline(50)
	names := jobNames(def)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		idx, err := deployEnvIndex(def)
		if err != nil {
			b.Fatal(err)
		}
		var found int
		for _, n := range names {
			if idx[n] != "" {
				found++
			}
		}
		if found != 2 {
			b.Fatalf("found = %d, want 2", found)
		}
	}
}

// BenchmarkFreezeEnvLookup_DecodePerJob is the shape this code had first, and
// the one any future contributor will reach for: jobDefFromDefinition per
// candidate. It unmarshals the WHOLE pipeline every call, so the cost is
// quadratic in pipeline width — on the dispatch hot path, every tick.
//
// Run both to see the gap:
//
//	go test -run '^$' -bench BenchmarkFreezeEnvLookup -benchmem ./internal/scheduler/
func BenchmarkFreezeEnvLookup_DecodePerJob(b *testing.B) {
	def := benchPipeline(50)
	names := jobNames(def)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var found int
		for _, n := range names {
			jobDef, err := jobDefFromDefinition(def, n)
			if err != nil {
				b.Fatal(err)
			}
			if jobDef.Deploy != nil && jobDef.Deploy.Environment != "" {
				found++
			}
		}
		if found != 2 {
			b.Fatalf("found = %d, want 2", found)
		}
	}
}

func jobNames(def []byte) []string {
	var p domain.Pipeline
	if err := json.Unmarshal(def, &p); err != nil {
		panic(err)
	}
	out := make([]string, 0, len(p.Jobs))
	for _, j := range p.Jobs {
		out = append(out, j.Name)
	}
	return out
}
