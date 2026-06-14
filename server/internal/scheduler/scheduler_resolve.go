package scheduler

import (
	"context"
	"errors"
	"fmt"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
	"github.com/google/uuid"
)

// recordDeployRevision lazy-creates the target environment and writes
// an in_progress deployment_revision for a job about to dispatch
// (#39). Returns the new revision id (uuid.Nil on a tracking-write
// failure) so the caller can delete it if the dispatch then fails.
// Best-effort: a tracking-write failure is logged, never fatal — the
// deploy still happens. The job's terminal result finalises this
// revision (handleJobResult → FinalizeDeploymentRevision). deployed_by
// is left empty for now; the promoting actor lives on the upstream
// approval gate's decided_by and wiring it through is a later
// refinement.
func (s *Scheduler) recordDeployRevision(ctx context.Context, run store.RunForDispatch, job store.DispatchableJob, attempt int32, target *DeployTarget) uuid.UUID {
	envID, err := s.store.EnsureEnvironment(ctx, run.ProjectID, target.Environment)
	if err != nil {
		s.log.Warn("scheduler: deploy tracking — ensure environment",
			"run_id", run.ID, "job_id", job.ID, "environment", target.Environment, "err", err)
		return uuid.Nil
	}
	revID, err := s.store.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID,
		RunID:         run.ID,
		JobRunID:      job.ID,
		Attempt:       attempt,
		Version:       target.Version,
		// A rollback re-runs the deploy job of a past run; the
		// dispatch carries deploy_rollback so the new revision is
		// flagged (it ships the SAME version that run originally
		// deployed, re-resolved from its immutable outputs).
		IsRollback: job.DeployRollback,
	})
	if err != nil {
		s.log.Warn("scheduler: deploy tracking — create revision",
			"run_id", run.ID, "job_id", job.ID, "environment", target.Environment, "err", err)
		return uuid.Nil
	}
	return revID
}

// resolveProfile pulls the runner profile referenced by the job
// (if any) and returns the full ResolvedProfile — merged env, secret
// values for LogMasks, and the k8s scheduling hints (NodeSelector +
// Tolerations) that the agent engine pipes into the pod spec. A job
// without a profile returns an empty ResolvedProfile — the fast path
// stays free.
//
// Profile lookup is by name from the job definition; missing profile
// fails the dispatch with a clear error so the operator notices a
// rename/typo instead of silently shipping without the env.
func (s *Scheduler) resolveProfile(ctx context.Context, run store.RunForDispatch, jobName string) (store.ResolvedProfile, error) {
	jobDef, err := jobDefFromDefinition(run.Definition, jobName)
	if err != nil {
		return store.ResolvedProfile{}, err
	}
	// When the job declares no profile, fall back to `default` if
	// it exists. Mirrors the apply-time fallback in
	// store.ResolveProfiles (which fills resource bounds from the
	// `default` profile) so the runtime-resolved fields
	// (env / secret values / node_selector / tolerations) also pick
	// up the safety net rather than the inconsistent split where
	// bounds came from default but scheduling didn't.
	//
	// Missing `default` → empty resolved profile, same as before
	// this fallback existed. The strict path (declared profile not
	// found) still fails the dispatch.
	if jobDef.Profile == "" {
		resolved, err := s.store.ResolveProfileByName(ctx, s.cipher, store.DefaultRunnerProfileName)
		if err != nil {
			if errors.Is(err, store.ErrRunnerProfileNotFound) {
				return store.ResolvedProfile{}, nil
			}
			return store.ResolvedProfile{}, fmt.Errorf("profile %q (fallback): %w", store.DefaultRunnerProfileName, err)
		}
		return resolved, nil
	}
	resolved, err := s.store.ResolveProfileByName(ctx, s.cipher, jobDef.Profile)
	if err != nil {
		return store.ResolvedProfile{}, fmt.Errorf("profile %q: %w", jobDef.Profile, err)
	}
	return resolved, nil
}

// resolveJobSecrets reads the declared secret names off the pipeline
// definition snapshot, then asks the configured Resolver for their values.
// Returns an empty map when the job has no secrets. Fails when a job
// references secrets but no Resolver is configured, or when a declared name
// isn't present in the backend — both are user-visible pipeline mistakes.
func (s *Scheduler) resolveJobSecrets(ctx context.Context, run store.RunForDispatch, jobName string) (map[string]string, error) {
	names, err := JobSecretsFromDefinition(run.Definition, jobName)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return map[string]string{}, nil
	}
	if s.resolver == nil {
		return nil, fmt.Errorf("secret %q declared but no secrets backend is configured on this server", names[0])
	}
	resolved, err := s.resolver.Resolve(ctx, run.ProjectID, names)
	if err != nil {
		return nil, err
	}
	// Every declared name must be present; Resolver implementations silently
	// drop unknown names, so we diff here for a precise error.
	var missing []string
	for _, n := range names {
		if _, ok := resolved[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("secrets not set on project: %v", missing)
	}
	return resolved, nil
}

// resolveArtifactDeps turns the job's needs_artifacts entries into a
// list of signed-URL download tickets. Fails when: no artifact backend
// is configured but the job declares deps, an upstream job produced
// zero ready artefacts (matching the optional paths filter), or
// signing errors. Empty return for a job with no deps.
func (s *Scheduler) resolveArtifactDeps(ctx context.Context, run store.RunForDispatch, jobName string) ([]*gocdnextv1.ArtifactDownload, error) {
	deps, err := JobArtifactDepsFromDefinition(run.Definition, jobName)
	if err != nil {
		return nil, err
	}
	if len(deps) == 0 {
		return nil, nil
	}
	if s.artifactStore == nil {
		return nil, fmt.Errorf("needs_artifacts declared but no artifact backend is configured on this server")
	}

	out := make([]*gocdnextv1.ArtifactDownload, 0)
	for _, dep := range deps {
		sourceRunID, err := s.resolveDepRunID(ctx, run, dep)
		if err != nil {
			return nil, err
		}
		rows, err := s.store.ListReadyArtifactsByRunAndJob(ctx, sourceRunID, dep.FromJob, dep.Paths)
		if err != nil {
			return nil, fmt.Errorf("lookup artefacts from %q: %w", dep.FromJob, err)
		}
		if len(rows) == 0 {
			scope := "same run"
			if dep.FromPipeline != "" {
				scope = fmt.Sprintf("upstream run of pipeline %q", dep.FromPipeline)
			}
			if len(dep.Paths) == 0 {
				return nil, fmt.Errorf("no ready artefacts found from job %q (%s)", dep.FromJob, scope)
			}
			return nil, fmt.Errorf("no ready artefacts from job %q matching paths %v (%s)", dep.FromJob, dep.Paths, scope)
		}
		dest := dep.Dest
		if dest == "" {
			dest = "./"
		}
		for _, a := range rows {
			signed, err := s.artifactStore.SignedGetURL(ctx, a.StorageKey, s.artifactGetURLTTL)
			if err != nil {
				return nil, fmt.Errorf("sign get url for %q: %w", a.Path, err)
			}
			out = append(out, &gocdnextv1.ArtifactDownload{
				Path:          a.Path,
				StorageKey:    a.StorageKey,
				GetUrl:        signed.URL,
				Dest:          dest,
				ContentSha256: a.ContentSHA256,
				FromJob:       a.JobName,
			})
		}
	}
	return out, nil
}

// resolveDepRunID picks the run_id whose artefacts back this
// particular `needs_artifacts` entry. Empty FromPipeline = current
// run (intra). Set FromPipeline = upstream run that triggered this
// run (fanout). Validates that the upstream is indeed the named
// pipeline so a typo surfaces as a clear error instead of "no
// artefacts found".
func (s *Scheduler) resolveDepRunID(ctx context.Context, run store.RunForDispatch, dep domain.ArtifactDep) (uuid.UUID, error) {
	if dep.FromPipeline == "" {
		return run.ID, nil
	}
	upstream, err := s.store.GetRunUpstreamContext(ctx, run.ID)
	if err != nil {
		return uuid.Nil, err
	}
	if upstream.UpstreamRunID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("needs_artifacts references upstream pipeline %q but this run has no upstream (cause is webhook/manual)", dep.FromPipeline)
	}
	if upstream.UpstreamPipeline != dep.FromPipeline {
		return uuid.Nil, fmt.Errorf("needs_artifacts references upstream pipeline %q but this run was triggered by %q", dep.FromPipeline, upstream.UpstreamPipeline)
	}
	return upstream.UpstreamRunID, nil
}

// failJobWithError marks a still-queued job as failed (with cascade to
// stage/run via store.CompleteJob). Called when dispatch-time resolution
// fails — e.g. a declared secret isn't set on the project. CompleteJob's
// WHERE clause accepts both queued and running, so we don't need to flip
// to running first just to fail it.
//
// ExpectedAgentID is uuid.Nil here on purpose: a queued job has
// agent_id IS NULL, and CompleteJobRun's `IS NOT DISTINCT FROM`
// predicate matches NULL with NULL. If a scheduler tick raced the
// agent (somehow the row got AssignJob'd between our list and our
// fail), this NULL-expected guard makes our fail no-op via ErrNoRows
// — which is the correct outcome, since a job that's been picked up
// by a live agent should not be failed by the dispatch path.
func (s *Scheduler) failJobWithError(ctx context.Context, job store.DispatchableJob, errMsg string) {
	if _, _, err := s.store.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID:        job.ID,
		Status:          string(domain.StatusFailed),
		ExitCode:        -1,
		ErrorMsg:        errMsg,
		ExpectedAgentID: uuid.Nil,
		ExpectedAttempt: job.Attempt,
	}); err != nil {
		s.log.Warn("scheduler: fail job", "job_id", job.ID, "err", err)
		return
	}
	s.log.Warn("scheduler: job failed at dispatch",
		"run_id", job.RunID, "job_id", job.ID, "job_name", job.Name, "err", errMsg)
}
