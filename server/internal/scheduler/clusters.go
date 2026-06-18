package scheduler

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// resolveClusterKubeconfig resolves a job's `cluster:` reference into a
// kubeconfig string to inject as PLUGIN_KUBECONFIG plus the log masks
// that credential demands. Returns ("", nil, nil) when the job names no
// cluster OR the cluster is in_cluster (the job pod's mounted SA is used
// — nothing to inject, nothing to mask). A declared-but-unresolvable
// cluster (deleted, or the project not in allowed_projects) returns an
// error the caller turns into failJobWithError — never a silent
// dispatch without the credential the pipeline asked for, same contract
// as secrets / id_tokens.
func (s *Scheduler) resolveClusterKubeconfig(ctx context.Context, run store.RunForDispatch, job store.DispatchableJob) (string, []string, error) {
	// Cheap gate: pipelines with no `cluster:` (the majority) skip the
	// decode. `"Cluster"` only appears in the definition JSONB when the
	// field is non-empty (omitempty). A false positive (the literal in
	// a script string) just pays the decode and finds no name.
	if !bytes.Contains(run.Definition, []byte(`"Cluster"`)) {
		return "", nil, nil
	}
	name := clusterNameForJob(run.Definition, job.Name)
	if name == "" {
		return "", nil, nil
	}
	// in_cluster yields ("", true, nil, nil) — authorization still
	// enforced, nothing injected, nothing to mask.
	kubeconfig, _, masks, err := s.store.ResolveClusterForDispatch(ctx, run.ProjectID, name)
	if err != nil {
		return "", nil, err
	}
	return kubeconfig, masks, nil
}

// clusterNameForJob extracts ONLY the Cluster of one job from the
// persisted definition — a skinny, tolerant decode (mirrors
// idTokenSpecsForJob). Synth notification jobs aren't in def.Jobs and
// so carry no cluster by construction.
func clusterNameForJob(definition []byte, jobName string) string {
	var def struct {
		Jobs []struct {
			Name    string
			Cluster string
		}
	}
	if json.Unmarshal(definition, &def) != nil {
		return ""
	}
	for _, j := range def.Jobs {
		if j.Name == jobName {
			return j.Cluster
		}
	}
	return ""
}
