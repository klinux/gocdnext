// Package compliance is the CLI-side HTTP client for the admin compliance
// read endpoints (/api/v1/admin/compliance/* and the per-project
// effective-pipeline preview). Read-only — listing frameworks/policies and
// previewing the merged pipeline. Split from cmd/ so the request shaping can be
// tested without booting cobra.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Framework mirrors the admin frameworkDTO. ID is surfaced so the user can feed
// it to `effective-pipeline --frameworks` (which keys off ids, not names).
type Framework struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedBy   string `json:"created_by"`
}

// Policy mirrors the admin policyDTO. The list endpoint returns metadata only
// (config_yaml / framework_ids are empty there — fetched per-policy on GET).
type Policy struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	Enabled        bool   `json:"enabled"`
	Mode           string `json:"mode"`
	Priority       int    `json:"priority"`
	AppliesToAll   bool   `json:"applies_to_all"`
	PositionBefore string `json:"position_before"`
	PositionAfter  string `json:"position_after"`
}

// PipelineDef is the subset of a pipeline definition the preview prints,
// matching the endpoint's explicit lower-case DTO.
type PipelineDef struct {
	Stages []string `json:"stages"`
	Jobs   []struct {
		Name  string `json:"name"`
		Stage string `json:"stage"`
	} `json:"jobs"`
}

// PipelineView is one pipeline's raw + effective definition. SystemManaged
// flags the server-owned synthetic `_compliance` pipeline.
type PipelineView struct {
	Name          string      `json:"name"`
	SystemManaged bool        `json:"system_managed"`
	Raw           PipelineDef `json:"raw"`
	Effective     PipelineDef `json:"effective"`
}

// ListFrameworks returns the compliance framework catalogue.
func ListFrameworks(ctx context.Context, client *http.Client, serverURL string) ([]Framework, error) {
	var out []Framework
	if err := getJSON(ctx, client, base(serverURL)+"/compliance/frameworks", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListPolicies returns the compliance policies (metadata only).
func ListPolicies(ctx context.Context, client *http.Client, serverURL string) ([]Policy, error) {
	var out []Policy
	if err := getJSON(ctx, client, base(serverURL)+"/compliance/policies", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// EffectivePipeline returns the per-pipeline raw + effective definition for a
// project. whatIf selects the mode: nil reads the stored effective definition
// (what runs today); a non-nil value is a what-if recompute for that
// comma-separated framework set (an empty string means "no frameworks").
func EffectivePipeline(ctx context.Context, client *http.Client, serverURL, slug string, whatIf *string) ([]PipelineView, error) {
	u := base(serverURL) + "/projects/" + url.PathEscape(slug) + "/effective-pipeline"
	if whatIf != nil {
		u += "?frameworks=" + url.QueryEscape(*whatIf)
	}
	var out []PipelineView
	if err := getJSON(ctx, client, u, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func base(serverURL string) string {
	return strings.TrimRight(serverURL, "/") + "/api/v1/admin"
}

// getJSON performs a GET and decodes a JSON body into v, mapping a non-2xx
// status to a loud error that carries the server's message.
func getJSON(ctx context.Context, client *http.Client, u string, v any) error {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decode %s: %w", u, err)
	}
	return nil
}
