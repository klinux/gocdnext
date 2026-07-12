package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Field validation for registering a deploy target. Mirrors the DB CHECKs but runs
// in Go too, so a bad input fails early with a clear message rather than a raw
// constraint error (defence in depth).

// ValidateProvider accepts only the providers this slice implements.
func ValidateProvider(p string) error {
	if p != "argocd" {
		return fmt.Errorf("deploy: unknown provider %q (want \"argocd\")", p)
	}
	return nil
}

// ValidateSyncMode accepts trigger | observe.
func ValidateSyncMode(m string) error {
	switch SyncMode(m) {
	case SyncModeTrigger, SyncModeObserve:
		return nil
	default:
		return fmt.Errorf("deploy: unknown sync_mode %q (want \"trigger\" or \"observe\")", m)
	}
}

// ValidateTargetFields checks a registration's scalar fields (provider/sync_mode
// enums + non-empty cluster/application).
func ValidateTargetFields(provider, cluster, application, syncMode string) error {
	if err := ValidateProvider(provider); err != nil {
		return err
	}
	if err := ValidateSyncMode(syncMode); err != nil {
		return err
	}
	if strings.TrimSpace(cluster) == "" {
		return errors.New("deploy: cluster is required")
	}
	if strings.TrimSpace(application) == "" {
		return errors.New("deploy: application is required")
	}
	return nil
}

// NormalizeNamespace trims and defaults an empty namespace to argocd. The DB
// DEFAULT doesn't apply because the upsert always sends the column, and an empty
// value would violate the non-empty CHECK — so the default lives here, at the
// write boundary.
func NormalizeNamespace(ns string) string {
	if s := strings.TrimSpace(ns); s != "" {
		return s
	}
	return defaultAppNamespace
}

// ErrMultiSource marks a rejection of a multi-source Application, so the caller can
// map it to a distinct HTTP status without string-matching the message.
var ErrMultiSource = errors.New("deploy: application is multi-source")

// applicationIsMultiSource reports whether an ArgoCD Application uses multiple
// sources (`spec.sources`, a list). Multi-source is out of scope for this slice —
// the single-source `.status.sync.revision` the watch relies on is empty for
// these — so registration rejects it up front.
func applicationIsMultiSource(raw []byte) (bool, error) {
	var app struct {
		Spec struct {
			Sources []json.RawMessage `json:"sources"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &app); err != nil {
		return false, fmt.Errorf("deploy: decode application spec: %w", err)
	}
	return len(app.Spec.Sources) > 0, nil
}

// ValidateSingleSource fetches the target's Application and rejects it if it is
// multi-source. The fetch doubles as an existence + reachability + cluster->project
// authorization check (the transport resolves the cluster with the project's
// access), so a registration fails early for a missing/unreachable/unauthorized
// Application, not only at watch time.
func (a *ArgoProvider) ValidateSingleSource(ctx context.Context, target DeploymentTarget) error {
	raw, err := a.fetch.fetchApplication(ctx, target)
	if err != nil {
		return fmt.Errorf("deploy: validate application %s/%s: %w", target.Namespace, target.Application, err)
	}
	multi, err := applicationIsMultiSource(raw)
	if err != nil {
		return err
	}
	if multi {
		return fmt.Errorf("%w: %q (spec.sources) — register a single-source Application", ErrMultiSource, target.Application)
	}
	return nil
}
