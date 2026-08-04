package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// PRHeadBinding is one scm_source bound to a clone URL, carrying the owning
// project's PR-head toggle + config path. The clone URL is NOT unique, so the
// wiring must require exactly one binding before consulting the head.
type PRHeadBinding struct {
	Source     SCMSource
	Trust      bool
	ConfigPath string
}

// FindPRHeadBindingsByURL returns every scm_source bound to the URL, each with
// the owning project's PR-head toggle + config path. The caller fails closed
// unless exactly one is returned (0 = no binding, >1 = ambiguous), never
// silently picking a winner.
func (s *Store) FindPRHeadBindingsByURL(ctx context.Context, rawURL string) ([]PRHeadBinding, error) {
	rows, err := s.q.FindScmSourcesByURL(ctx, domain.NormalizeGitURL(rawURL))
	if err != nil {
		return nil, fmt.Errorf("store: find pr-head bindings: %w", err)
	}
	out := make([]PRHeadBinding, 0, len(rows))
	for _, row := range rows {
		out = append(out, PRHeadBinding{
			Source: SCMSource{
				ID:            fromPgUUID(row.ID),
				ProjectID:     fromPgUUID(row.ProjectID),
				Provider:      row.Provider,
				URL:           domain.HTTPCloneURL(row.Url),
				DefaultBranch: row.DefaultBranch,
				AuthRef:       stringValue(row.AuthRef),
			},
			Trust:      row.TrustSameRepoPrConfig,
			ConfigPath: row.ConfigPath,
		})
	}
	return out, nil
}

// MaterialPipelineIdentity resolves a matched PR material to its pipeline +
// owning project, so the wiring can partition system_managed pipelines (which
// always run on the base flow) from repo pipelines (candidates for the head
// flow), and build the base-authorized set for the resolver.
type MaterialPipelineIdentity struct {
	MaterialID    uuid.UUID
	PipelineID    uuid.UUID
	PipelineName  string
	SystemManaged bool
	ProjectID     uuid.UUID
}

// ListMaterialPipelineIdentities resolves each material id to its pipeline
// identity + project. Read-only, pre-transaction.
func (s *Store) ListMaterialPipelineIdentities(ctx context.Context, materialIDs []uuid.UUID) ([]MaterialPipelineIdentity, error) {
	if len(materialIDs) == 0 {
		return nil, nil
	}
	pg := make([]pgtype.UUID, len(materialIDs))
	for i, id := range materialIDs {
		pg[i] = pgUUID(id)
	}
	rows, err := s.q.ListMaterialPipelineIdentities(ctx, pg)
	if err != nil {
		return nil, fmt.Errorf("store: list material pipeline identities: %w", err)
	}
	out := make([]MaterialPipelineIdentity, 0, len(rows))
	for _, row := range rows {
		out = append(out, MaterialPipelineIdentity{
			MaterialID:    fromPgUUID(row.MaterialID),
			PipelineID:    fromPgUUID(row.PipelineID),
			PipelineName:  row.PipelineName,
			SystemManaged: row.SystemManaged,
			ProjectID:     fromPgUUID(row.ProjectID),
		})
	}
	return out, nil
}
