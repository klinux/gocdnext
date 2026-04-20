package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// ApplyProjectInput is the declarative payload from `gocdnext
// apply`: a project and the full list of pipelines that should
// exist under it. Pipelines not in this list are removed;
// materials not in a pipeline's list are removed.
type ApplyProjectInput struct {
	Slug        string
	Name        string
	Description string
	ConfigRepo  string
	// ConfigPath is the repo-relative folder that holds the
	// pipeline YAMLs. Empty = preserve existing (on update) or
	// fall back to the column default `.gocdnext` (on insert).
	// The Apply handler validates the shape before calling.
	ConfigPath string
	Pipelines  []*domain.Pipeline
	// SCMSource, when non-nil, binds this project to an SCM
	// repository that carries the pipeline folder. Persisted
	// alongside the project in the same tx so Apply stays atomic.
	// Omit for legacy / config-less flows.
	SCMSource *SCMSourceInput
}

// SCMSourceInput is the subset of scm_sources fields a caller can set. The
// server fills in timestamps and id.
type SCMSourceInput struct {
	Provider       string // "github" | "gitlab" | "bitbucket" | "manual"
	URL            string
	DefaultBranch  string
	WebhookSecret  string
	AuthRef        string
}

type SCMSourceApplied struct {
	ID            uuid.UUID
	Provider      string
	URL           string
	DefaultBranch string
	Created       bool
	// GeneratedWebhookSecret is the plaintext of a freshly-minted
	// per-repo secret — set ONLY on the apply that created or
	// rotated the stored ciphertext. Callers surface this in the
	// HTTP response exactly once so the operator copies it into
	// the GitHub webhook config; subsequent reads never see it
	// again.
	GeneratedWebhookSecret string
}

type PipelineApplyStatus struct {
	Name             string
	PipelineID       uuid.UUID
	Created          bool
	MaterialsAdded   int
	MaterialsRemoved int
}

type ApplyProjectResult struct {
	ProjectID        uuid.UUID
	ProjectCreated   bool
	Pipelines        []PipelineApplyStatus
	PipelinesRemoved []string
	SCMSource        *SCMSourceApplied
}

// ApplyProject upserts the project and synchronizes its pipelines and materials
// to match the input. The whole operation runs inside one transaction: either
// every row reflects the input or nothing changes.
func (s *Store) ApplyProject(ctx context.Context, in ApplyProjectInput) (ApplyProjectResult, error) {
	if in.Slug == "" {
		return ApplyProjectResult{}, fmt.Errorf("store: apply project: slug is required")
	}
	if in.Name == "" {
		in.Name = in.Slug
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ApplyProjectResult{}, fmt.Errorf("store: apply project: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)

	proj, err := q.UpsertProject(ctx, db.UpsertProjectParams{
		Slug:        in.Slug,
		Name:        in.Name,
		Description: nullableString(in.Description),
		ConfigPath:  in.ConfigPath, // empty → SQL keeps existing / defaults to .gocdnext
	})
	if err != nil {
		return ApplyProjectResult{}, fmt.Errorf("store: upsert project: %w", err)
	}

	result := ApplyProjectResult{
		ProjectID:      fromPgUUID(proj.ID),
		ProjectCreated: proj.Created,
	}

	if in.SCMSource != nil {
		applied, err := s.upsertSCMSource(ctx, q, proj.ID, in.SCMSource)
		if err != nil {
			return ApplyProjectResult{}, err
		}
		result.SCMSource = applied
	}

	wanted := make(map[string]*domain.Pipeline, len(in.Pipelines))
	for _, p := range in.Pipelines {
		if p.Name == "" {
			return ApplyProjectResult{}, fmt.Errorf("store: pipeline without name")
		}
		if _, dup := wanted[p.Name]; dup {
			return ApplyProjectResult{}, fmt.Errorf("store: pipeline %q listed twice", p.Name)
		}
		wanted[p.Name] = p
	}

	existing, err := q.ListPipelinesByProject(ctx, proj.ID)
	if err != nil {
		return ApplyProjectResult{}, fmt.Errorf("store: list pipelines: %w", err)
	}
	for _, row := range existing {
		if _, keep := wanted[row.Name]; keep {
			continue
		}
		if err := q.DeletePipeline(ctx, row.ID); err != nil {
			return ApplyProjectResult{}, fmt.Errorf("store: delete pipeline %s: %w", row.Name, err)
		}
		result.PipelinesRemoved = append(result.PipelinesRemoved, row.Name)
	}

	// Pipelines inherit the project's persisted config_path (what
	// the upsert just wrote) so a drift re-apply without an
	// explicit ConfigPath still stamps the right value on each
	// pipeline row.
	for _, p := range in.Pipelines {
		status, err := applyPipeline(ctx, q, proj.ID, p, in.ConfigRepo, proj.ConfigPath)
		if err != nil {
			return ApplyProjectResult{}, err
		}
		result.Pipelines = append(result.Pipelines, status)
	}

	if err := tx.Commit(ctx); err != nil {
		return ApplyProjectResult{}, fmt.Errorf("store: apply project: commit: %w", err)
	}
	return result, nil
}

// upsertSCMSource validates the input and upserts the scm_sources
// row bound to the given project. Called inside ApplyProject's tx
// so the project + scm_source land atomically.
//
// Per-repo webhook secret policy:
//   - caller sends a plaintext secret → we seal it with the
//     store's cipher and pass the ciphertext to Upsert. The
//     plaintext is echoed back via GeneratedWebhookSecret so the
//     HTTP response can hand it to the operator (once).
//   - caller sends empty AND this is a fresh row → we generate
//     32 random bytes, seal, surface the plaintext.
//   - caller sends empty AND the row already has a secret → we
//     pass nil, the COALESCE in the query preserves the existing
//     ciphertext, GeneratedWebhookSecret stays empty.
func (s *Store) upsertSCMSource(ctx context.Context, q *db.Queries, projectID pgtype.UUID, in *SCMSourceInput) (*SCMSourceApplied, error) {
	if in.URL == "" {
		return nil, fmt.Errorf("store: scm_source: url is required")
	}
	if in.Provider == "" {
		return nil, fmt.Errorf("store: scm_source: provider is required")
	}
	if s.authCipher == nil {
		return nil, ErrAuthProviderCipherUnset
	}
	defaultBranch := in.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	// Is there already a row? Decides whether empty input means
	// "preserve" (existing row) or "generate" (fresh row).
	existing, err := q.GetScmSourceByProject(ctx, projectID)
	alreadyExists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: scm_source lookup: %w", err)
	}
	_ = existing

	var sealed []byte
	var generated string
	switch {
	case in.WebhookSecret != "":
		sealed, err = s.authCipher.Encrypt([]byte(in.WebhookSecret))
		if err != nil {
			return nil, fmt.Errorf("store: seal webhook secret: %w", err)
		}
	case !alreadyExists:
		plain, err := newWebhookSecret()
		if err != nil {
			return nil, err
		}
		generated = plain
		sealed, err = s.authCipher.Encrypt([]byte(plain))
		if err != nil {
			return nil, fmt.Errorf("store: seal generated secret: %w", err)
		}
	default:
		// Existing row + empty input → preserve the ciphertext via
		// the COALESCE path in UpsertScmSource.
		sealed = nil
	}

	row, err := q.UpsertScmSource(ctx, db.UpsertScmSourceParams{
		ProjectID:     projectID,
		Provider:      in.Provider,
		Url:           domain.NormalizeGitURL(in.URL),
		DefaultBranch: defaultBranch,
		WebhookSecret: sealed,
		AuthRef:       nullableString(in.AuthRef),
	})
	if err != nil {
		return nil, fmt.Errorf("store: upsert scm_source: %w", err)
	}
	return &SCMSourceApplied{
		ID:                     fromPgUUID(row.ID),
		Provider:               row.Provider,
		URL:                    row.Url,
		DefaultBranch:          row.DefaultBranch,
		Created:                row.Created,
		GeneratedWebhookSecret: generated,
	}, nil
}

// newWebhookSecret returns a fresh 32-byte hex-encoded secret.
// Long enough that brute-force against the HMAC is infeasible;
// hex-encoded so the operator can paste it into the GitHub
// webhook form without worrying about special chars.
func newWebhookSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("store: random: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func applyPipeline(ctx context.Context, q *db.Queries, projectID pgtype.UUID, p *domain.Pipeline, configRepo, configPath string) (PipelineApplyStatus, error) {
	definition, err := marshalPipelineDefinition(p)
	if err != nil {
		return PipelineApplyStatus{}, fmt.Errorf("store: marshal pipeline %s: %w", p.Name, err)
	}

	if configPath == "" {
		configPath = ".gocdnext"
	}
	row, err := q.UpsertPipeline(ctx, db.UpsertPipelineParams{
		ProjectID:  projectID,
		Name:       p.Name,
		Definition: definition,
		ConfigRepo: nullableString(configRepo),
		ConfigPath: configPath,
	})
	if err != nil {
		return PipelineApplyStatus{}, fmt.Errorf("store: upsert pipeline %s: %w", p.Name, err)
	}

	status := PipelineApplyStatus{
		Name:       row.Name,
		PipelineID: fromPgUUID(row.ID),
		Created:    row.Created,
	}

	existing, err := q.ListMaterialsByPipeline(ctx, row.ID)
	if err != nil {
		return PipelineApplyStatus{}, fmt.Errorf("store: list materials %s: %w", p.Name, err)
	}
	existingByFP := make(map[string]db.Material, len(existing))
	for _, m := range existing {
		existingByFP[m.Fingerprint] = m
	}

	wantedFPs := make(map[string]struct{}, len(p.Materials))
	for _, m := range p.Materials {
		wantedFPs[m.Fingerprint] = struct{}{}
		cfg, err := marshalMaterialConfig(m)
		if err != nil {
			return PipelineApplyStatus{}, fmt.Errorf("store: marshal material %s/%s: %w", p.Name, m.Fingerprint, err)
		}
		res, err := q.UpsertMaterial(ctx, db.UpsertMaterialParams{
			PipelineID:  row.ID,
			Type:        string(m.Type),
			Config:      cfg,
			Fingerprint: m.Fingerprint,
			AutoUpdate:  m.AutoUpdate,
		})
		if err != nil {
			return PipelineApplyStatus{}, fmt.Errorf("store: upsert material %s: %w", m.Fingerprint, err)
		}
		if res.Created {
			status.MaterialsAdded++
		}
	}

	for fp, m := range existingByFP {
		if _, keep := wantedFPs[fp]; keep {
			continue
		}
		if err := q.DeleteMaterial(ctx, m.ID); err != nil {
			return PipelineApplyStatus{}, fmt.Errorf("store: delete material %s: %w", fp, err)
		}
		status.MaterialsRemoved++
	}

	return status, nil
}

func marshalPipelineDefinition(p *domain.Pipeline) ([]byte, error) {
	clone := *p
	clone.ID = ""
	clone.ProjectID = ""
	for i := range clone.Materials {
		clone.Materials[i].ID = ""
	}
	return json.Marshal(clone)
}

func marshalMaterialConfig(m domain.Material) ([]byte, error) {
	switch m.Type {
	case domain.MaterialGit:
		return json.Marshal(m.Git)
	case domain.MaterialUpstream:
		return json.Marshal(m.Upstream)
	case domain.MaterialCron:
		return json.Marshal(m.Cron)
	case domain.MaterialManual:
		return []byte(`{}`), nil
	default:
		return nil, fmt.Errorf("unknown material type %q", m.Type)
	}
}
