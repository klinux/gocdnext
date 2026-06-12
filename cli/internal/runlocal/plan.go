// Package runlocal executes a pipeline on the developer's machine —
// gocdnext's `woodpecker exec` equivalent. Jobs run as plain docker
// containers sharing one mounted workspace; stages run in declared
// order; needs are honored by topological order inside each stage.
//
// Deliberate fidelity boundaries (v1): no cache restore/store (local
// state IS the cache), no artifact backend (jobs share the mounted
// workspace, which covers the common artifact flows implicitly), no
// id_tokens (no issuer to mint from — fail loud), approval gates
// auto-skip with a loud warning, kubernetes-only knobs (profiles,
// resources, tags) are ignored.
package runlocal

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// PlannedJob is one executable unit — a job, or one matrix cell of
// a job.
type PlannedJob struct {
	Name      string // display name; matrix cells carry a [k=v ...] suffix
	BaseName  string // YAML job name — what --job matches against
	Stage     string
	Image     string
	Script    []string          // task scripts, run in order inside ONE container
	PluginEnv map[string]string // PLUGIN_* env (plugin jobs)
	Variables map[string]string // pipeline + job variables
	MatrixKey string            // "K=V,K2=V2" (sorted) — GOCDNEXT_MATRIX, same as dispatch
	Secrets   []string          // names; resolved by the executor
	Docker    bool
	Approval  bool
	Needs     []string
}

// PlannedStage groups the jobs of one stage in execution order.
type PlannedStage struct {
	Name string
	Jobs []PlannedJob
}

// Plan is the fully-expanded execution order for one pipeline.
type Plan struct {
	Pipeline string
	Stages   []PlannedStage
	Services []domain.Service
}

// Build expands a parsed pipeline into an executable plan: stages in
// declared order, needs-respecting job order inside each stage,
// matrix jobs expanded into one PlannedJob per cell.
//
// `only` (the --job flag) filters BEFORE the unsupported-feature
// checks: "run a single job, skip everything else" must hold even
// when a non-selected job declares id_tokens or has no local image
// — those jobs are skipped entirely, not validated. Matches the
// YAML name (all matrix cells) or the expanded cell name.
func Build(p *domain.Pipeline, only string) (*Plan, error) {
	plan := &Plan{Pipeline: p.Name, Services: p.Services}
	byStage := make(map[string][]domain.Job)
	for _, j := range p.Jobs {
		byStage[j.Stage] = append(byStage[j.Stage], j)
	}
	matched := false
	for _, stage := range p.Stages {
		jobs := byStage[stage]
		ordered, err := topoSort(jobs)
		if err != nil {
			return nil, fmt.Errorf("stage %s: %w", stage, err)
		}
		ps := PlannedStage{Name: stage}
		for _, j := range ordered {
			if only != "" && !jobSelected(j.Name, only) {
				continue
			}
			expanded, err := expand(p, j)
			if err != nil {
				return nil, fmt.Errorf("job %s: %w", j.Name, err)
			}
			if only != "" {
				// Narrow to one cell when `only` targets an
				// expanded matrix name.
				kept := expanded[:0]
				for _, c := range expanded {
					if c.Name == only || c.BaseName == only {
						kept = append(kept, c)
					}
				}
				expanded = kept
			}
			matched = matched || len(expanded) > 0
			ps.Jobs = append(ps.Jobs, expanded...)
		}
		plan.Stages = append(plan.Stages, ps)
	}
	if only != "" && !matched {
		return nil, fmt.Errorf("job %q not found in pipeline %s", only, p.Name)
	}
	return plan, nil
}

// jobSelected reports whether --job=only selects the YAML job named
// name: exact YAML name (runs every matrix cell) or an expanded cell
// name like "unit [GO=1.25]".
func jobSelected(name, only string) bool {
	return only == name || strings.HasPrefix(only, name+" [")
}

// expand turns one domain.Job into its planned cells (1 for plain
// jobs, N for matrix jobs).
func expand(p *domain.Pipeline, j domain.Job) ([]PlannedJob, error) {
	// id_tokens only exist in the cluster (the issuer mints them at
	// dispatch). Running the job WITHOUT the token would turn local
	// green into cluster red on exactly the auth-critical path —
	// fail loud instead of silently degrading.
	if len(j.IDTokens) > 0 {
		return nil, fmt.Errorf("declares id_tokens: — not supported in run-local (no OIDC issuer); exercise this job in the cluster")
	}
	base := PlannedJob{
		Name:      j.Name,
		BaseName:  j.Name,
		Stage:     j.Stage,
		Image:     j.Image,
		Secrets:   append([]string(nil), j.Secrets...),
		Docker:    j.Docker,
		Approval:  j.Approval != nil,
		Needs:     append([]string(nil), j.Needs...),
		Variables: map[string]string{},
	}
	for k, v := range p.Variables {
		base.Variables[k] = v
	}
	for k, v := range j.Variables {
		base.Variables[k] = v
	}
	for _, t := range j.Tasks {
		switch {
		case t.Plugin != nil:
			if base.Image != "" && base.Image != t.Plugin.Image {
				return nil, fmt.Errorf("both image: and uses: resolved — unsupported")
			}
			base.Image = t.Plugin.Image
			base.PluginEnv = map[string]string{}
			for k, v := range t.Plugin.Settings {
				base.PluginEnv["PLUGIN_"+pluginEnvKey(k)] = v
			}
		case t.Script != "":
			base.Script = append(base.Script, t.Script)
		}
	}
	if !base.Approval && base.Image == "" {
		return nil, fmt.Errorf("no image: or uses: — run-local has no default-image fallback (the cluster's is profile-specific)")
	}

	if len(j.Matrix) == 0 {
		return []PlannedJob{base}, nil
	}

	cells := cartesian(j.Matrix)
	out := make([]PlannedJob, 0, len(cells))
	for _, cell := range cells {
		c := base
		// Dims do NOT become individual env vars: the dispatch only
		// exposes GOCDNEXT_MATRIX (decomposition is explicitly
		// deferred upstream — assignment.go). Injecting $GO locally
		// would green-light scripts that break in the cluster.
		keys := make([]string, 0, len(cell))
		for k := range cell {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+cell[k])
		}
		c.MatrixKey = strings.Join(parts, ",")
		c.Name = fmt.Sprintf("%s [%s]", j.Name, strings.Join(parts, " "))
		out = append(out, c)
	}
	return out, nil
}

// cartesian expands {"GO": ["1.24","1.25"], "OS": ["alpine"]} into
// one map per combination, deterministically ordered by key.
func cartesian(matrix map[string][]string) []map[string]string {
	keys := make([]string, 0, len(matrix))
	for k := range matrix {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cells := []map[string]string{{}}
	for _, k := range keys {
		var next []map[string]string
		for _, cell := range cells {
			for _, v := range matrix[k] {
				c := make(map[string]string, len(cell)+1)
				for ck, cv := range cell {
					c[ck] = cv
				}
				c[k] = v
				next = append(next, c)
			}
		}
		cells = next
	}
	return cells
}

// topoSort orders jobs so every job runs after its needs (Kahn's
// algorithm, stable by declaration order). The parser already
// validated that needs reference real same-stage jobs and carry no
// cycles — the error paths here are defensive.
func topoSort(jobs []domain.Job) ([]domain.Job, error) {
	index := make(map[string]int, len(jobs))
	for i, j := range jobs {
		index[j.Name] = i
	}
	indeg := make([]int, len(jobs))
	dependents := make(map[int][]int)
	for i, j := range jobs {
		for _, n := range j.Needs {
			if di, ok := index[n]; ok {
				indeg[i]++
				dependents[di] = append(dependents[di], i)
			}
		}
	}
	var queue []int
	for i := range jobs {
		if indeg[i] == 0 {
			queue = append(queue, i)
		}
	}
	out := make([]domain.Job, 0, len(jobs))
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		out = append(out, jobs[i])
		for _, d := range dependents[i] {
			indeg[d]--
			if indeg[d] == 0 {
				queue = append(queue, d)
			}
		}
	}
	if len(out) != len(jobs) {
		return nil, fmt.Errorf("needs cycle detected")
	}
	return out, nil
}

// pluginEnvKey mirrors the agent's transform (runner/plugin.go):
// kebab/dot/space → underscore, camelCase → SNAKE, uppercased — so
// `node-version` becomes NODE_VERSION exactly like in the cluster.
func pluginEnvKey(k string) string {
	var b strings.Builder
	b.Grow(len(k))
	prevLower := false
	for _, r := range k {
		switch {
		case r == '-' || r == '.' || r == ' ':
			b.WriteByte('_')
			prevLower = false
		case unicode.IsUpper(r):
			if prevLower {
				b.WriteByte('_')
			}
			b.WriteRune(r)
			prevLower = false
		default:
			b.WriteRune(unicode.ToUpper(r))
			prevLower = unicode.IsLower(r)
		}
	}
	return b.String()
}
