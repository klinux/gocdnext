package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// VSM is what the frontend renders as a pipeline-level dependency
// graph (Value Stream Map, in GoCD-speak). Nodes are pipelines in the
// project; edges are `upstream:` materials pointing from one pipeline
// to another. A pipeline with no runs yet still shows up as a node
// with LatestRun == nil.
type VSM struct {
	ProjectID   uuid.UUID  `json:"project_id"`
	ProjectSlug string     `json:"project_slug"`
	ProjectName string     `json:"project_name"`
	Nodes       []VSMNode  `json:"nodes"`
	Edges       []VSMEdge  `json:"edges"`
	GeneratedAt time.Time  `json:"generated_at"`
}

// VSMNode carries enough for the graph to label + colour a pipeline
// without a second roundtrip. Stages are pulled from the latest run
// when present (accurate to what actually executed) and left nil
// otherwise — the frontend falls back to "unknown structure".
type VSMNode struct {
	PipelineID        uuid.UUID        `json:"pipeline_id"`
	Name              string           `json:"name"`
	DefinitionVersion int              `json:"definition_version"`
	GitMaterials      []GitRef         `json:"git_materials,omitempty"`
	LatestRun         *RunSummary      `json:"latest_run,omitempty"`
	// Metrics mirror the PipelineSummary.Metrics shape so the VSM
	// node can render p50 + pass rate + bottleneck indicators with
	// the same client-side formatting helpers as the pipeline card.
	Metrics *PipelineMetrics `json:"metrics,omitempty"`
}

// GitRef is the minimum the UI needs to surface "this pipeline feeds
// off github.com/org/repo @ main" on the node label.
type GitRef struct {
	URL    string `json:"url"`
	Branch string `json:"branch,omitempty"`
}

// VSMEdge is one upstream relationship. From is the pipeline name
// whose stage completion triggers To. Stage + Status make the edge
// self-descriptive on hover. WaitTimeP50Sec surfaces the observed
// median wait between upstream.finished and downstream.started so
// the VSM can label arrows with real-world latency ("18s artifact
// upload", "3m env warm-up" from the design reference).
type VSMEdge struct {
	FromPipeline   string  `json:"from_pipeline"`
	ToPipeline     string  `json:"to_pipeline"`
	Stage          string  `json:"stage"`
	Status         string  `json:"status,omitempty"` // "success" etc.
	WaitTimeP50Sec float64 `json:"wait_time_p50_seconds,omitempty"`
	WaitSamples    int     `json:"wait_samples,omitempty"`
}

// GetProjectVSM assembles the graph for a project. Three DB queries
// (pipelines / materials / latest runs) keyed by slug, stitched in
// Go. N is small (tens of pipelines per project) so N+1 isn't a
// concern here — worst case on dogfood one extra query per pipeline
// would still be fine.
func (s *Store) GetProjectVSM(ctx context.Context, slug string) (VSM, error) {
	proj, err := s.q.GetProjectBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return VSM{}, ErrProjectNotFound
	}
	if err != nil {
		return VSM{}, fmt.Errorf("store: get project: %w", err)
	}

	pipelines, err := s.q.ListPipelinesByProjectSlug(ctx, slug)
	if err != nil {
		return VSM{}, fmt.Errorf("store: list pipelines for vsm: %w", err)
	}

	mats, err := s.q.ListMaterialsByProjectSlug(ctx, slug)
	if err != nil {
		return VSM{}, fmt.Errorf("store: list materials for vsm: %w", err)
	}

	latest, err := s.q.LatestRunPerPipelineByProjectSlug(ctx, slug)
	if err != nil {
		return VSM{}, fmt.Errorf("store: latest runs for vsm: %w", err)
	}

	// Share the metrics query with project-detail — same window,
	// same shape. Empty map when the project has no terminal runs,
	// which keeps the VSM usable for fresh projects.
	metricsByID, err := s.pipelineMetricsByID(ctx, slug)
	if err != nil {
		return VSM{}, fmt.Errorf("store: pipeline metrics for vsm: %w", err)
	}

	// pipeline_id → index in nodes, so we can attach materials + runs
	// without quadratic lookups.
	nodes := make([]VSMNode, 0, len(pipelines))
	idx := make(map[uuid.UUID]int, len(pipelines))
	byName := make(map[string]uuid.UUID, len(pipelines))
	for _, pl := range pipelines {
		pid := fromPgUUID(pl.ID)
		idx[pid] = len(nodes)
		byName[pl.Name] = pid
		nodes = append(nodes, VSMNode{
			PipelineID:        pid,
			Name:              pl.Name,
			DefinitionVersion: int(pl.DefinitionVersion),
			Metrics:           metricsByID[pid],
		})
	}

	// Latest run per pipeline; pipelines with no runs stay LatestRun == nil.
	for _, r := range latest {
		pid := fromPgUUID(r.PipelineID)
		i, ok := idx[pid]
		if !ok {
			continue
		}
		// Pipeline name is available from the nodes slice; don't
		// redo the work.
		nodes[i].LatestRun = &RunSummary{
			ID:           fromPgUUID(r.ID),
			PipelineID:   pid,
			PipelineName: nodes[i].Name,
			Counter:      r.Counter,
			Cause:        r.Cause,
			Status:       r.Status,
			CreatedAt:    r.CreatedAt.Time,
			StartedAt:    pgTimePtr(r.StartedAt),
			FinishedAt:   pgTimePtr(r.FinishedAt),
			TriggeredBy:  stringValue(r.TriggeredBy),
		}
	}

	// Materials: decode config, populate GitMaterials on the node, or
	// build an upstream edge. Unknown types are ignored — future proof
	// additions don't break the VSM.
	edges := make([]VSMEdge, 0, len(mats))
	for _, m := range mats {
		pid := fromPgUUID(m.PipelineID)
		i, ok := idx[pid]
		if !ok {
			continue
		}
		switch domain.MaterialType(m.Type) {
		case domain.MaterialGit:
			var g domain.GitMaterial
			if err := json.Unmarshal(m.Config, &g); err == nil {
				nodes[i].GitMaterials = append(nodes[i].GitMaterials,
					GitRef{URL: g.URL, Branch: g.Branch})
			}
		case domain.MaterialUpstream:
			var u domain.UpstreamMaterial
			if err := json.Unmarshal(m.Config, &u); err == nil {
				edges = append(edges, VSMEdge{
					FromPipeline: u.Pipeline,
					ToPipeline:   nodes[i].Name,
					Stage:        u.Stage,
					Status:       u.Status,
				})
			}
		}
	}

	// Attach median wait times to each edge. Edge timing is a
	// "nice-to-have" — a missing row just leaves WaitTimeP50Sec at
	// 0 and the UI falls back to rendering the stage label only.
	// Keyed by (from_pipeline_id, to_pipeline_id) so fan-out from a
	// single upstream to multiple downstreams gets a wait per arrow.
	timingRows, err := s.q.VSMEdgeTimingByProjectSlug(ctx, db.VSMEdgeTimingByProjectSlugParams{
		Slug:        slug,
		SinceWindow: intervalDays(MetricsWindowDays),
	})
	if err != nil {
		return VSM{}, fmt.Errorf("store: vsm edge timing: %w", err)
	}
	type edgeKey struct{ from, to uuid.UUID }
	waitByKey := make(map[edgeKey]struct {
		wait    float64
		samples int
	}, len(timingRows))
	for _, r := range timingRows {
		waitByKey[edgeKey{from: fromPgUUID(r.FromPipelineID), to: fromPgUUID(r.ToPipelineID)}] = struct {
			wait    float64
			samples int
		}{wait: r.WaitP50S, samples: int(r.Samples)}
	}
	for i := range edges {
		fromID, fromOk := byName[edges[i].FromPipeline]
		toID, toOk := byName[edges[i].ToPipeline]
		if !fromOk || !toOk {
			continue
		}
		if t, ok := waitByKey[edgeKey{from: fromID, to: toID}]; ok {
			edges[i].WaitTimeP50Sec = t.wait
			edges[i].WaitSamples = t.samples
		}
	}

	return VSM{
		ProjectID:   fromPgUUID(proj.ID),
		ProjectSlug: proj.Slug,
		ProjectName: proj.Name,
		Nodes:       nodes,
		Edges:       edges,
		GeneratedAt: time.Now().UTC(),
	}, nil
}

