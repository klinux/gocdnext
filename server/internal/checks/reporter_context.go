package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// runContext is the shape reporter needs: triggering material URL,
// head SHA, pipeline name, counter, branch. Separated into a struct
// so resolveRunContext can return nil cleanly when the run shouldn't
// report.
type runContext struct {
	owner, repo   string
	headSHA       string
	projectSlug   string
	pipelineName  string
	branch        string
	counter       int64
	reportingMode string // project's current check_reporting_mode
	cause         string
}

func (r *Reporter) resolveRunContext(ctx context.Context, runID uuid.UUID) (*runContext, error) {
	detail, err := r.store.GetRunDetail(ctx, runID, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("get run detail: %w", err)
	}
	// Only report for webhook-driven runs. Manual/upstream runs
	// don't have a specific head SHA to report against.
	switch detail.Cause {
	case string(domain.CauseWebhook), string(domain.CausePullRequest), string(domain.CauseMergeGroup):
	default:
		return nil, nil
	}
	if len(detail.Revisions) == 0 {
		return nil, nil
	}
	var revisions map[string]struct {
		Revision string `json:"revision"`
		Branch   string `json:"branch"`
	}
	if err := json.Unmarshal(detail.Revisions, &revisions); err != nil {
		return nil, fmt.Errorf("decode revisions: %w", err)
	}
	if len(revisions) == 0 {
		return nil, nil
	}

	// Pick the first material that has a revision (usually the only
	// one on a webhook-driven run). We also need its URL — query the
	// store for the materials so we can resolve owner/repo.
	mats, err := r.store.ListPipelineMaterials(ctx, detail.PipelineID)
	if err != nil {
		return nil, fmt.Errorf("list materials: %w", err)
	}

	var triggeringID uuid.UUID
	var headSHA, branch string
	for id, rev := range revisions {
		if rev.Revision == "" {
			continue
		}
		u, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		triggeringID = u
		headSHA = rev.Revision
		branch = rev.Branch
		break
	}
	if triggeringID == uuid.Nil {
		return nil, nil
	}

	// For PR runs, head SHA from cause_detail is authoritative (the
	// PR head commit, not the material's internal "revision" field).
	if detail.Cause == string(domain.CausePullRequest) && len(detail.CauseDetail) > 0 {
		var cd map[string]any
		if err := json.Unmarshal(detail.CauseDetail, &cd); err == nil {
			if sha, ok := cd["pr_head_sha"].(string); ok && sha != "" {
				headSHA = sha
			}
		}
	}

	var repoURL string
	for _, m := range mats {
		if m.ID == triggeringID {
			var cfg domain.GitMaterial
			if err := json.Unmarshal(m.Config, &cfg); err == nil {
				repoURL = cfg.URL
			}
			break
		}
	}
	if repoURL == "" {
		if err := mergeGroupContextError(detail.Cause, "merge_group run has no triggering git material URL"); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if !isGitHubHost(repoURL) {
		// ParseRepoURL also accepts gitlab/bitbucket shaped URLs;
		// Checks API is github-specific so skip anything else.
		if err := mergeGroupContextError(detail.Cause, "merge_group run material is not github: %s", repoURL); err != nil {
			return nil, err
		}
		return nil, nil
	}
	owner, repo, err := ghscm.ParseRepoURL(repoURL)
	if err != nil {
		if err := mergeGroupContextError(detail.Cause, "parse merge_group github repo url: %w", err); err != nil {
			return nil, err
		}
		return nil, nil
	}

	return &runContext{
		owner:         owner,
		repo:          repo,
		headSHA:       headSHA,
		projectSlug:   detail.ProjectSlug,
		pipelineName:  detail.PipelineName,
		branch:        branch,
		counter:       detail.Counter,
		reportingMode: detail.CheckReportingMode,
		cause:         detail.Cause,
	}, nil
}

// isGitHubHost returns true for URLs whose host is github.com. We
// keep the check narrow — GitHub Enterprise host validation belongs
// at a higher level where the operator configures the enterprise
// APIBase, not here.
func isGitHubHost(repoURL string) bool {
	s := strings.ToLower(repoURL)
	switch {
	case strings.HasPrefix(s, "https://github.com/"),
		strings.HasPrefix(s, "http://github.com/"),
		strings.HasPrefix(s, "git@github.com:"):
		return true
	}
	return false
}
