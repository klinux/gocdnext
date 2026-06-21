package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/pkg/compliance"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// reconcileGovernance brings a project's enforced surface in line with its
// current compliance policies. Called at the end of BOTH ApplyProject and the
// recompute fan-out, so the same end state holds whether enforcement changed
// via a repo push or an admin policy/framework mutation. It:
//
//   - guarantees a run path exists: a governed project with NO pipeline of its
//     own gets the server-owned synthetic `_compliance` pipeline (so policies
//     run even with no repo CI — the GitLab compliance guarantee);
//   - makes the trigger non-suppressible: every governed pipeline gets a
//     compliance-owned default-branch push material (no path/event narrowing),
//     so the repo's `when.*` can't stop enforcement from firing;
//   - tears the synthetic pipeline down when the project stops being governed or
//     grows a pipeline of its own.
//
// A governed project with neither a pipeline nor a repo binding can't be
// enforced at all → the operation is refused (rolls back) rather than silently
// registering toothless governance.
func reconcileGovernance(ctx context.Context, q *db.Queries, projectID pgtype.UUID, configPath string, policies []compliance.Policy) error {
	governed := len(policies) > 0
	if !governed {
		// Not governed: tear down any synthetic pipeline left from before. (Repo
		// triggers stay as overridden until the next repo sync re-derives them —
		// documented; un-governance is the admin removing the policy/framework.)
		return dropSyntheticIfPresent(ctx, q, projectID)
	}

	scmURL, defaultBranch, hasSCM, err := scmForProject(ctx, q, projectID)
	if err != nil {
		return err
	}
	// Compliance enforcement is push-triggered, which requires a registered repo
	// binding the webhook can match. Without one, NOTHING the project declares
	// can be made non-suppressible — refuse rather than register toothless
	// governance (covers both the no-pipeline and the explicit-material-only
	// legacy/manual project).
	if !hasSCM {
		return fmt.Errorf(
			"%w: a governed project requires a registered SCM source (push-triggered enforcement)",
			ErrComplianceWouldDropEnforcement)
	}

	pipes, err := q.ListPipelineKindsByProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("store: reconcile governance: list pipelines: %w", err)
	}
	var synthID pgtype.UUID
	synthExists := false
	repoCount := 0
	for _, p := range pipes {
		if p.SystemManaged {
			synthExists = true
			synthID = p.ID
		} else {
			repoCount++
		}
	}

	// A governed project with no pipeline of its own runs policies via the
	// synthetic pipeline; one that has a pipeline doesn't need it.
	needSynth := repoCount == 0
	switch {
	case needSynth:
		// Always upsert (not only on first create): refreshes the effective
		// definition AND the material when policies or the scm_source
		// (url/default-branch) change — a stale material would point the
		// compliance trigger at the wrong branch/repo.
		if err := createSyntheticPipeline(ctx, q, projectID, configPath, scmURL, defaultBranch, policies); err != nil {
			return err
		}
	case synthExists:
		if err := q.DeletePipeline(ctx, synthID); err != nil {
			return fmt.Errorf("store: reconcile governance: drop synthetic: %w", err)
		}
	}

	// Non-suppressible trigger on every governed repo pipeline. We MERGE onto any
	// existing material on the same (url, default-branch) fingerprint so a repo's
	// secret_ref / poll interval / extra events (tag, pull_request) survive — only
	// path/branch narrowing of the compliance push is overridden.
	complianceFP := domain.GitFingerprint(scmURL, defaultBranch)
	for _, p := range pipes {
		if p.SystemManaged {
			continue
		}
		mat, err := mergedComplianceMaterial(ctx, q, p.ID, scmURL, defaultBranch, complianceFP)
		if err != nil {
			return err
		}
		cfg, err := marshalMaterialConfig(mat)
		if err != nil {
			return fmt.Errorf("store: reconcile governance: marshal material: %w", err)
		}
		if _, err := q.UpsertMaterial(ctx, db.UpsertMaterialParams{
			PipelineID:  p.ID,
			Type:        string(mat.Type),
			Config:      cfg,
			Fingerprint: mat.Fingerprint,
			AutoUpdate:  mat.AutoUpdate,
		}); err != nil {
			return fmt.Errorf("store: reconcile governance: override material: %w", err)
		}
	}
	return nil
}

// dropSyntheticIfPresent removes the server-owned synthetic pipeline if one
// exists for the project (used when a project stops being governed).
func dropSyntheticIfPresent(ctx context.Context, q *db.Queries, projectID pgtype.UUID) error {
	pipes, err := q.ListPipelineKindsByProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("store: reconcile governance: list pipelines: %w", err)
	}
	for _, p := range pipes {
		if p.SystemManaged {
			if err := q.DeletePipeline(ctx, p.ID); err != nil {
				return fmt.Errorf("store: reconcile governance: drop synthetic: %w", err)
			}
		}
	}
	return nil
}

// mergedComplianceMaterial builds the non-suppressible material for a governed
// repo pipeline, preserving any existing same-fingerprint material's
// credentials / poll interval / extra events while forcing a push trigger with
// no path filter.
func mergedComplianceMaterial(ctx context.Context, q *db.Queries, pipelineID pgtype.UUID, scmURL, defaultBranch, complianceFP string) (domain.Material, error) {
	base := compliance.ComplianceMaterial(scmURL, defaultBranch)
	existing, err := q.ListMaterialsByPipeline(ctx, pipelineID)
	if err != nil {
		return domain.Material{}, fmt.Errorf("store: reconcile governance: list materials: %w", err)
	}
	for _, m := range existing {
		if m.Fingerprint != complianceFP || m.Type != string(domain.MaterialGit) {
			continue
		}
		var prev domain.GitMaterial
		if err := json.Unmarshal(m.Config, &prev); err != nil {
			return domain.Material{}, fmt.Errorf("store: reconcile governance: decode material: %w", err)
		}
		// Preserve the repo's own clone URL (e.g. an SSH remote the agent has a
		// key for — the runner clones exactly what we send), credentials,
		// polling and extra events; force push, drop paths.
		if prev.URL != "" {
			base.Git.URL = prev.URL
		}
		base.Git.SecretRef = prev.SecretRef
		base.Git.PollInterval = prev.PollInterval
		if prev.AutoRegisterWebhook {
			base.Git.AutoRegisterWebhook = true
		}
		base.Git.Events = unionEvents(prev.Events, "push")
		base.Git.Paths = nil
		break
	}
	return base, nil
}

// unionEvents returns the existing events plus `want`, order-stable, deduped.
func unionEvents(existing []string, want string) []string {
	out := make([]string, 0, len(existing)+1)
	seen := map[string]struct{}{}
	for _, e := range append(append([]string{}, existing...), want) {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}

// createSyntheticPipeline writes the server-owned `_compliance` pipeline (policy
// jobs as its effective definition) plus its non-suppressible material.
func createSyntheticPipeline(ctx context.Context, q *db.Queries, projectID pgtype.UUID, configPath, scmURL, defaultBranch string, policies []compliance.Policy) error {
	if configPath == "" {
		configPath = ".gocdnext"
	}
	raw := compliance.SyntheticPipeline(scmURL, defaultBranch)
	rawBytes, err := marshalPipelineDefinition(&raw)
	if err != nil {
		return fmt.Errorf("store: synthetic pipeline: marshal raw: %w", err)
	}
	eff := compliance.ApplyPolicies(raw, policies)
	effBytes, err := marshalPipelineDefinition(&eff)
	if err != nil {
		return fmt.Errorf("store: synthetic pipeline: marshal effective: %w", err)
	}
	row, err := q.UpsertPipeline(ctx, db.UpsertPipelineParams{
		ProjectID:     projectID,
		Name:          compliance.PipelineName,
		Definition:    effBytes,
		DefinitionRaw: rawBytes,
		ConfigRepo:    nil,
		ConfigPath:    configPath,
		SystemManaged: true,
	})
	if err != nil {
		return fmt.Errorf("store: synthetic pipeline: upsert: %w", err)
	}
	mat := raw.Materials[0]
	cfg, err := marshalMaterialConfig(mat)
	if err != nil {
		return fmt.Errorf("store: synthetic pipeline: marshal material: %w", err)
	}
	if _, err := q.UpsertMaterial(ctx, db.UpsertMaterialParams{
		PipelineID:  row.ID,
		Type:        string(mat.Type),
		Config:      cfg,
		Fingerprint: mat.Fingerprint,
		AutoUpdate:  mat.AutoUpdate,
	}); err != nil {
		return fmt.Errorf("store: synthetic pipeline: material: %w", err)
	}
	// Prune any stale material left from a previous default-branch / repo URL
	// (the upsert above only refreshes the CURRENT fingerprint) so the
	// synthetic fires on exactly one — the current — default-branch push.
	existing, err := q.ListMaterialsByPipeline(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("store: synthetic pipeline: list materials: %w", err)
	}
	for _, m := range existing {
		if m.Fingerprint != mat.Fingerprint {
			if err := q.DeleteMaterial(ctx, m.ID); err != nil {
				return fmt.Errorf("store: synthetic pipeline: prune material: %w", err)
			}
		}
	}
	return nil
}

// scmForProject returns the project's repo URL + default branch, or ok=false
// when the project has no scm_source binding.
func scmForProject(ctx context.Context, q *db.Queries, projectID pgtype.UUID) (url, defaultBranch string, ok bool, err error) {
	row, err := q.GetScmSourceByProject(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: scm for project: %w", err)
	}
	return row.Url, row.DefaultBranch, true, nil
}
