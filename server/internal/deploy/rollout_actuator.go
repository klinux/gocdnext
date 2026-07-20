package deploy

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Merge-patch bodies for the Rollout `/status` subresource. Verified against
// argoproj/argo-rollouts (cmd/promote, cmd/abort) and the 2a lab spike.
//
//   - promote a step-paused canary: clear `.status.pauseConditions` — the controller
//     then advances one step (re-pausing if the next step is another indefinite pause).
//     This is argo's `clearPauseConditionsPatch`; the null is the merge-patch "delete
//     this field". We deliberately do NOT also set currentStepIndex: the controller
//     owns the advance, and pinning an index would fight it under drift.
//   - abort: set `.status.abort` — the controller scales the stable ReplicaSet back up
//     (traffic → stable). This reverts TRAFFIC, never Git/desired (`.spec.template`
//     keeps the new version); the UI copy says so.
const (
	promoteStatusBody = `{"status":{"pauseConditions":null}}`
	abortStatusBody   = `{"status":{"abort":true}}`
)

// rolloutActuator drives an Argo Rollouts canary via merge-patches to its `/status`
// subresource. Behind the same test seam as the fetcher/syncer — a fake asserts the
// exact path + body without a cluster. The watcher is the ONLY caller (single
// actuator; it renews the lease right before invoking these).
type rolloutActuator interface {
	promoteRollout(ctx context.Context, target DeploymentTarget) error
	abortRollout(ctx context.Context, target DeploymentTarget) error
}

// k8sRolloutActuator patches the Rollout `/status` through the credentialed cluster
// transport (the same ClusterPatcher the Application sync uses). The Rollout lives on
// the workload's destination cluster; the target carries the RESOLVED/pinned identity
// (RolloutCluster/Namespace/Name) — this never re-discovers.
type k8sRolloutActuator struct {
	patch ClusterPatcher
}

func newK8sRolloutActuator(p ClusterPatcher) *k8sRolloutActuator {
	return &k8sRolloutActuator{patch: p}
}

func (a *k8sRolloutActuator) promoteRollout(ctx context.Context, target DeploymentTarget) error {
	return a.patchStatus(ctx, target, []byte(promoteStatusBody))
}

func (a *k8sRolloutActuator) abortRollout(ctx context.Context, target DeploymentTarget) error {
	return a.patchStatus(ctx, target, []byte(abortStatusBody))
}

// patchStatus resolves the rollout cluster (RolloutCluster, else the App's Cluster)
// and merge-patches its `/status`. It fails closed on an incomplete pin — an empty
// name/namespace/cluster would build a collection-level or half-formed path, so we
// never issue the write. The caller (watcher) guarantees a complete pinned identity;
// this is defence in depth.
func (a *k8sRolloutActuator) patchStatus(ctx context.Context, target DeploymentTarget, body []byte) error {
	cluster := target.RolloutCluster
	if cluster == "" {
		cluster = target.Cluster
	}
	if cluster == "" || target.RolloutNamespace == "" || target.RolloutName == "" || target.ProjectID == uuid.Nil {
		return errors.New("deploy: incomplete rollout target for actuation")
	}
	_, err := a.patch.ClusterAPIPatch(ctx, cluster, target.ProjectID, rolloutStatusPath(target.RolloutNamespace, target.RolloutName), body)
	return err
}
