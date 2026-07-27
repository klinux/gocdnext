package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// CreateStatusInput is a legacy Commit Status (distinct from a Check Run).
// gocdnext posts BOTH: the check run is the rich, GitHub-hosted view; the
// commit status is the plain entry whose row links STRAIGHT to the run via
// TargetURL — the UX teams migrating from Woodpecker/GoCD expect.
//
// Requires the App's "Commit statuses: write" permission — SEPARATE from
// "Checks: write". Callers treat a 4xx (e.g. the permission not yet granted)
// as best-effort: log and continue, never fail the run.
type CreateStatusInput struct {
	Owner, Repo, SHA string
	State            string // pending | success | failure | error (GitHub enum)
	Context          string // the check identifier, e.g. "ci/gocdnext/pr"
	TargetURL        string // where the row links — the gocdnext run page
	Description      string // short inline text; GitHub caps it at 140 chars
}

// CreateStatus posts a commit status on (owner/repo)@sha. Statuses have no
// "update" verb: re-POSTing the same Context supersedes the previous state,
// so the same call drives pending → success/failure across the run lifecycle.
func (c *AppClient) CreateStatus(ctx context.Context, installationID int64, in CreateStatusInput) error {
	if in.SHA == "" || in.Context == "" || in.State == "" {
		return fmt.Errorf("github: CreateStatus requires SHA, Context, and State")
	}
	payload := map[string]any{"state": in.State, "context": in.Context}
	if in.TargetURL != "" {
		payload["target_url"] = in.TargetURL
	}
	if in.Description != "" {
		payload["description"] = truncateStatusDescription(in.Description)
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost,
		c.apiBase+"/repos/"+in.Owner+"/"+in.Repo+"/statuses/"+in.SHA, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.DoAsInstallation(ctx, installationID, req)
	if err != nil {
		return fmt.Errorf("github: create status: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github: create status returned %s: %s", resp.Status, b)
	}
	return nil
}

// truncateStatusDescription caps at GitHub's 140-char limit, rune-safe so a
// multibyte character is never split at the boundary.
func truncateStatusDescription(s string) string {
	const max = 140
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
