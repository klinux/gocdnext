package grpcsrv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/sarif"
)

const (
	maxSarifBytes       = 32 << 20 // 32 MiB per file (matches coverage's cap)
	maxSarifFilesPerJob = 20
	maxFindingsPerJob   = 10000
	sarifIngestTimeout  = 60 * time.Second
)

// sarifSem bounds concurrent SARIF parsing across the process so a burst of
// completed scan jobs with big reports can't pressure memory/DB.
var sarifSem = make(chan struct{}, 4)

// ingestSecurityFindings fires off (async, best-effort) the reconciliation of a
// completed job's SARIF artifacts into the security_findings table. Called from
// handleJobResult after CompleteJob. Never affects the job result.
func (a *AgentService) ingestSecurityFindings(jobRunID uuid.UUID, attempt int32) {
	if a.artifactStore == nil {
		return
	}
	go a.reconcileSecurityFindings(jobRunID, attempt)
}

// reconcileSecurityFindings parses the job's ready *.sarif artifacts and
// replaces the findings for the job_run — but only on a clean reconcile:
//   - no SARIF ready  → clear (handles a rerun that dropped its SARIF); skipped
//     entirely on first attempts (nothing to clear).
//   - all parse OK    → replace with the union.
//   - any read/parse error → keep the previous findings (a transient failure
//     must not silently hide a vulnerability).
func (a *AgentService) reconcileSecurityFindings(jobRunID uuid.UUID, attempt int32) {
	ctx, cancel := context.WithTimeout(context.Background(), sarifIngestTimeout)
	defer cancel()

	arts, err := a.store.ListArtifactsByJobRun(ctx, jobRunID)
	if err != nil {
		a.log.Warn("security: list artifacts", "job_id", jobRunID, "err", err)
		return
	}
	var sarifs []store.Artifact
	for _, art := range arts {
		if art.Status == "ready" && strings.HasSuffix(strings.ToLower(art.Path), ".sarif") {
			sarifs = append(sarifs, art)
		}
	}

	if len(sarifs) == 0 {
		if attempt == 0 {
			return // first attempt, no SARIF → nothing to reconcile
		}
		a.replaceFindings(ctx, jobRunID, attempt, nil)
		return
	}

	if len(sarifs) > maxSarifFilesPerJob {
		a.log.Warn("security: too many sarif files; capping", "job_id", jobRunID, "count", len(sarifs))
		sarifs = sarifs[:maxSarifFilesPerJob]
	}

	all := make([]store.FindingIn, 0, 64)
	for _, art := range sarifs {
		fs, perr := a.parseSarifArtifact(ctx, art)
		if perr != nil {
			// Keep prior findings — do NOT partial-replace on error.
			a.log.Warn("security: sarif parse failed; keeping previous findings",
				"job_id", jobRunID, "artifact", art.Path, "err", perr)
			return
		}
		all = append(all, fs...)
		if len(all) >= maxFindingsPerJob {
			a.log.Warn("security: per-job findings cap reached", "job_id", jobRunID)
			all = all[:maxFindingsPerJob]
			break
		}
	}

	a.replaceFindings(ctx, jobRunID, attempt, all)
	a.log.Info("security findings reconciled",
		"job_id", jobRunID, "sarif_files", len(sarifs), "findings", len(all))
}

// replaceFindings writes the reconciled set, treating a stale attempt (the job
// was reclaimed/rerun while we parsed) as a benign no-op rather than an error.
func (a *AgentService) replaceFindings(ctx context.Context, jobRunID uuid.UUID, attempt int32, findings []store.FindingIn) {
	err := a.store.ReplaceSecurityFindings(ctx, jobRunID, attempt, findings)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrSnapshotStale):
		a.log.Debug("security: skip stale findings write (job reclaimed/rerun)", "job_id", jobRunID)
	default:
		a.log.Warn("security: write findings", "job_id", jobRunID, "err", err)
	}
}

func (a *AgentService) parseSarifArtifact(ctx context.Context, art store.Artifact) ([]store.FindingIn, error) {
	// Respect the ingest timeout while waiting for a parse slot — don't let
	// goroutines pile up past their context on a big-SARIF burst.
	select {
	case sarifSem <- struct{}{}:
		defer func() { <-sarifSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	rc, err := a.artifactStore.Get(ctx, art.StorageKey)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", art.Path, err)
	}
	defer rc.Close()

	parsed, truncated, err := sarif.Parse(io.LimitReader(rc, maxSarifBytes))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", art.Path, err)
	}
	if truncated {
		a.log.Warn("security: sarif findings truncated at cap", "artifact", art.Path)
	}

	out := make([]store.FindingIn, 0, len(parsed))
	for _, f := range parsed {
		out = append(out, store.FindingIn{
			ArtifactID:   art.ID,
			ArtifactPath: art.Path,
			Tool:         f.Tool,
			RuleID:       f.RuleID,
			Severity:     f.Severity,
			Level:        f.Level,
			Message:      f.Message,
			LocationPath: f.LocationPath,
			LocationLine: f.LocationLine,
			LocationURL:  f.LocationURL,
			Fingerprint:  f.Fingerprint,
		})
	}
	return out, nil
}
