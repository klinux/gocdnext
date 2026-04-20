package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// CheckRunStatus is the in-progress / queued / completed enum GitHub
// expects. Mirrored as constants to avoid typos at call sites.
type CheckRunStatus string

const (
	CheckStatusQueued     CheckRunStatus = "queued"
	CheckStatusInProgress CheckRunStatus = "in_progress"
	CheckStatusCompleted  CheckRunStatus = "completed"
)

// CheckRunConclusion is the terminal verdict. Only set when
// status=completed.
type CheckRunConclusion string

const (
	CheckConclusionSuccess        CheckRunConclusion = "success"
	CheckConclusionFailure        CheckRunConclusion = "failure"
	CheckConclusionCancelled      CheckRunConclusion = "cancelled"
	CheckConclusionTimedOut       CheckRunConclusion = "timed_out"
	CheckConclusionActionRequired CheckRunConclusion = "action_required"
	CheckConclusionNeutral        CheckRunConclusion = "neutral"
)

// CreateCheckRunInput is the write side: name + head_sha are
// required; DetailsURL is where clicking the check takes users (the
// run page on our UI).
type CreateCheckRunInput struct {
	Owner      string
	Repo       string
	Name       string
	HeadSHA    string
	Status     CheckRunStatus     // empty = queued
	DetailsURL string             // run page on gocdnext UI
	ExternalID string             // our run id, returned verbatim on future queries
	Output     *CheckRunOutput    // optional summary
}

// UpdateCheckRunInput is the patch side. Any zero field is omitted
// from the wire payload.
type UpdateCheckRunInput struct {
	Owner      string
	Repo      string
	CheckRunID int64
	Status     CheckRunStatus
	Conclusion CheckRunConclusion
	Output     *CheckRunOutput
}

// CheckRunOutput is the GitHub "output" card shown on the PR.
// Title + Summary required by GitHub when Output is present.
type CheckRunOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Text    string `json:"text,omitempty"`
}

// CheckRun is the minimum we care about from a create/patch response.
type CheckRun struct {
	ID     int64
	Status CheckRunStatus
	HTMLURL string
}

// CreateCheckRun creates a check on (owner/repo)@head_sha.
func (c *AppClient) CreateCheckRun(ctx context.Context, installationID int64, in CreateCheckRunInput) (CheckRun, error) {
	if in.Name == "" || in.HeadSHA == "" {
		return CheckRun{}, fmt.Errorf("github: CreateCheckRun requires Name and HeadSHA")
	}
	payload := map[string]any{
		"name":     in.Name,
		"head_sha": in.HeadSHA,
	}
	if in.Status != "" {
		payload["status"] = in.Status
	}
	if in.DetailsURL != "" {
		payload["details_url"] = in.DetailsURL
	}
	if in.ExternalID != "" {
		payload["external_id"] = in.ExternalID
	}
	if in.Output != nil {
		payload["output"] = in.Output
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost,
		c.apiBase+"/repos/"+in.Owner+"/"+in.Repo+"/check-runs", bytes.NewReader(body))
	if err != nil {
		return CheckRun{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.DoAsInstallation(ctx, installationID, req)
	if err != nil {
		return CheckRun{}, fmt.Errorf("github: create check run: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return CheckRun{}, fmt.Errorf("github: create check run returned %s: %s", resp.Status, b)
	}
	var raw struct {
		ID      int64          `json:"id"`
		Status  CheckRunStatus `json:"status"`
		HTMLURL string         `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return CheckRun{}, fmt.Errorf("github: decode check run: %w", err)
	}
	return CheckRun{ID: raw.ID, Status: raw.Status, HTMLURL: raw.HTMLURL}, nil
}

// UpdateCheckRun patches an existing check run. Use this when a run
// transitions from queued/in_progress → completed.
func (c *AppClient) UpdateCheckRun(ctx context.Context, installationID int64, in UpdateCheckRunInput) error {
	if in.CheckRunID == 0 {
		return fmt.Errorf("github: UpdateCheckRun requires CheckRunID")
	}
	payload := map[string]any{}
	if in.Status != "" {
		payload["status"] = in.Status
	}
	if in.Conclusion != "" {
		payload["conclusion"] = in.Conclusion
	}
	if in.Output != nil {
		payload["output"] = in.Output
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/repos/%s/%s/check-runs/%d", c.apiBase, in.Owner, in.Repo, in.CheckRunID)
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.DoAsInstallation(ctx, installationID, req)
	if err != nil {
		return fmt.Errorf("github: update check run: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github: update check run returned %s: %s", resp.Status, b)
	}
	return nil
}
