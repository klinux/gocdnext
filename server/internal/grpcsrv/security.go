package grpcsrv

import (
	"context"
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
		if err := a.store.ReplaceSecurityFindings(ctx, jobRunID, nil); err != nil {
			a.log.Warn("security: clear findings", "job_id", jobRunID, "err", err)
		}
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

	if err := a.store.ReplaceSecurityFindings(ctx, jobRunID, all); err != nil {
		a.log.Warn("security: write findings", "job_id", jobRunID, "err", err)
		return
	}
	a.log.Info("security findings ingested",
		"job_id", jobRunID, "sarif_files", len(sarifs), "findings", len(all))
}

func (a *AgentService) parseSarifArtifact(ctx context.Context, art store.Artifact) ([]store.FindingIn, error) {
	sarifSem <- struct{}{}
	defer func() { <-sarifSem }()

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
