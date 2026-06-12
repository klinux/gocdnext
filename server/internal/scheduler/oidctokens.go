package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/gocdnext/gocdnext/server/internal/oidcissuer"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// IDTokenMinter is the issuer surface the scheduler needs: one call
// per declared token at dispatch time. Satisfied by
// *oidcissuer.Issuer; fakes in tests count invocations.
type IDTokenMinter interface {
	Mint(ctx context.Context, jc oidcissuer.JobClaims, aud []string) (string, error)
}

// WithIDTokenMinter wires the OIDC issuer. nil (the default) keeps
// the feature off: jobs declaring id_tokens then FAIL at dispatch
// with a configuration error — never a silent dispatch without the
// token the pipeline asked for.
func (s *Scheduler) WithIDTokenMinter(m IDTokenMinter) *Scheduler {
	s.idTokens = m
	return s
}

// mintIDTokens resolves a job's id_tokens declaration into
// env-name → JWT. Returns (nil, nil) when the job declares nothing —
// the fast path costs one bytes.Contains over the definition blob,
// no JSON decode, no minter call. A declaration with no minter wired
// is a configuration error the caller surfaces via failJobWithError
// (same contract as secrets/artifact deps).
func (s *Scheduler) mintIDTokens(ctx context.Context, run store.RunForDispatch, job store.DispatchableJob) (map[string]string, error) {
	// Cheap gate: pipelines without any id_tokens (the overwhelming
	// majority) skip the skinny decode entirely. False positives
	// (the literal appearing in a script string) just pay the decode
	// and find no specs — correct either way.
	if !bytes.Contains(run.Definition, []byte(`"IDTokens"`)) {
		return nil, nil
	}
	specs := idTokenSpecsForJob(run.Definition, job.Name)
	if len(specs) == 0 {
		return nil, nil
	}
	if s.idTokens == nil {
		return nil, fmt.Errorf("job declares id_tokens but the OIDC issuer is disabled — set GOCDNEXT_PUBLIC_BASE and GOCDNEXT_SECRET_KEY on the server")
	}

	jc := buildJobClaims(run, job)
	tokens := make(map[string]string, len(specs))
	for envName, spec := range specs {
		token, err := s.idTokens.Mint(ctx, jc, spec.Aud)
		if err != nil {
			return nil, fmt.Errorf("mint %s: %w", envName, err)
		}
		tokens[envName] = token
	}
	return tokens, nil
}

// idTokenSpecsForJob extracts ONLY the id_tokens specs of one job
// from the persisted definition — a skinny decode that skips the
// rest of the pipeline shape. Synth notification jobs aren't in
// def.Jobs and therefore can never carry tokens (by construction).
func idTokenSpecsForJob(definition []byte, jobName string) map[string]domain.IDTokenSpec {
	var def struct {
		Jobs []struct {
			Name     string
			IDTokens map[string]domain.IDTokenSpec
		}
	}
	if json.Unmarshal(definition, &def) != nil {
		return nil
	}
	for _, j := range def.Jobs {
		if j.Name == jobName {
			return j.IDTokens
		}
	}
	return nil
}

// buildJobClaims projects dispatch-time context into the claim set.
// Reuses the same cause_detail / revisions decoders the CI_* env
// surface uses (civars.go) so the token and the env vars can never
// disagree about what triggered the run.
func buildJobClaims(run store.RunForDispatch, job store.DispatchableJob) oidcissuer.JobClaims {
	sha, branch := primaryRevision(run.Revisions)
	jc := oidcissuer.JobClaims{
		ProjectSlug: run.ProjectSlug,
		ProjectID:   run.ProjectID.String(),
		Pipeline:    pipelineNameFromDefinition(run.Definition),
		PipelineID:  run.PipelineID.String(),
		Job:         job.Name,
		MatrixKey:   job.MatrixKey,
		RunID:       run.ID.String(),
		RunCounter:  strconv.FormatInt(run.Counter, 10),
		SHA:         sha,
		Cause:       run.Cause,
	}
	switch run.Cause {
	case "tag":
		var t tagDetail
		if len(run.CauseDetail) > 0 {
			_ = json.Unmarshal(run.CauseDetail, &t)
		}
		if t.Name != "" {
			jc.RefType, jc.Ref = "tag", t.Name
		}
	case "pull_request":
		// The PR head ref is attacker-controlled; Subject() drops it
		// from the sub entirely. ref/ref_type claims still carry it
		// for diagnostics — policies must pin sub, not ref, which the
		// docs spell out.
		var pr pullRequestDetail
		if len(run.CauseDetail) > 0 {
			_ = json.Unmarshal(run.CauseDetail, &pr)
		}
		if pr.Number > 0 {
			jc.PRNumber = strconv.Itoa(pr.Number)
		}
		if branch != "" {
			jc.RefType, jc.Ref = "branch", branch
		}
	default:
		if branch != "" {
			jc.RefType, jc.Ref = "branch", branch
		}
	}
	return jc
}

// pipelineNameFromDefinition pulls just the Name field out of the
// persisted definition JSONB. The dispatch path already decodes the
// full definition elsewhere; this stays independent so claims can be
// built before/without that decode and a malformed blob degrades to
// an empty pipeline segment instead of failing the mint.
func pipelineNameFromDefinition(raw []byte) string {
	var def struct {
		Name string `json:"Name"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &def) != nil {
		return ""
	}
	return def.Name
}
