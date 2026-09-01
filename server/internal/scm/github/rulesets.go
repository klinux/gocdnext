package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// RequiredChecksRulesetName is the name of the DEDICATED repository ruleset
// gocdnext owns and upserts to enforce "required pipelines for PR merge" for a
// given project. It is PER PROJECT (name carries the slug): two projects bound
// to the same repo each own a separate ruleset, so one never adopts/overwrites
// or deletes the other's. Rulesets stack, so a dedicated one never clobbers the
// operator's other branch rules (unlike classic branch protection, a replace-all
// object).
func RequiredChecksRulesetName(slug string) string {
	return "gocdnext-required-checks-" + slug
}

// ErrAppLacksAdmin is returned when a ruleset write is refused with 403 —
// almost always because the GitHub App is missing the `Administration: write`
// permission the rulesets API requires. Surfaced to the operator as an
// actionable "re-approve the App with admin permission" message rather than an
// opaque 500.
var ErrAppLacksAdmin = errors.New("github: app lacks Administration:write permission for rulesets")

// RulesetInput is the desired state of the required-checks ruleset for one repo.
// The ruleset targets the repo's DEFAULT branch (~DEFAULT_BRANCH), so a rename
// of the default branch doesn't strand it.
type RulesetInput struct {
	Owner    string
	Repo     string
	Name     string   // per-project ruleset name (see RequiredChecksRulesetName)
	Contexts []string // required status-check contexts (ci/gocdnext/<slug>/<pipeline>)
}

// UpsertRequiredChecksRuleset creates or updates the dedicated required-checks
// ruleset so the repo requires every context in Contexts to pass before a PR to
// the default branch can merge. existingID is the id we stored from a previous
// sync. When it's nil (fresh config, or a DB restore that lost the id) we first
// look for a ruleset already named RequiredChecksRulesetName and ADOPT it rather
// than creating a duplicate; only if none exists do we POST. If a stored/adopted
// ruleset was deleted out-of-band (PUT 404s) it falls back to create so config
// self-heals. Returns the ruleset id to persist.
func (c *AppClient) UpsertRequiredChecksRuleset(ctx context.Context, installationID int64, in RulesetInput, existingID *int64) (int64, error) {
	body, err := json.Marshal(rulesetPayload(in, c.appID))
	if err != nil {
		return 0, fmt.Errorf("github: marshal ruleset: %w", err)
	}

	target := existingID
	if target == nil {
		// Idempotent by name: adopt THIS project's ruleset (DB restore, manual
		// create) instead of stacking a duplicate. The name is project-scoped, so
		// a sibling project on the same repo is never adopted.
		found, ferr := c.findRulesetByName(ctx, installationID, in.Owner, in.Repo, in.Name)
		if ferr != nil {
			return 0, ferr
		}
		target = found
	}

	if target != nil {
		id, err := c.putRuleset(ctx, installationID, in.Owner, in.Repo, *target, body)
		if errors.Is(err, errRulesetNotFound) {
			return c.postRuleset(ctx, installationID, in.Owner, in.Repo, body)
		}
		return id, err
	}
	return c.postRuleset(ctx, installationID, in.Owner, in.Repo, body)
}

// findRulesetByName lists the repo's rulesets and returns the id of the first
// one named `name`, or nil when none matches.
func (c *AppClient) findRulesetByName(ctx context.Context, installationID int64, owner, repo, name string) (*int64, error) {
	req, err := http.NewRequest(http.MethodGet,
		c.apiBase+"/repos/"+owner+"/"+repo+"/rulesets?per_page=100", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.DoAsInstallation(ctx, installationID, req)
	if err != nil {
		return nil, fmt.Errorf("github: list rulesets: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusForbidden {
		return nil, adminError(resp)
	}
	if resp.StatusCode/100 != 2 {
		rb, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github: list rulesets returned %s: %s", resp.Status, rb)
	}
	var raw []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("github: decode rulesets: %w", err)
	}
	for _, rs := range raw {
		if rs.Name == name {
			id := rs.ID
			return &id, nil
		}
	}
	return nil, nil
}

// DeleteRuleset removes the required-checks ruleset (used when the operator
// clears every required pipeline). A 404 is treated as success — the desired
// end state (no ruleset) already holds.
func (c *AppClient) DeleteRuleset(ctx context.Context, installationID int64, owner, repo string, id int64) error {
	req, err := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("%s/repos/%s/%s/rulesets/%d", c.apiBase, owner, repo, id), nil)
	if err != nil {
		return err
	}
	resp, err := c.DoAsInstallation(ctx, installationID, req)
	if err != nil {
		return fmt.Errorf("github: delete ruleset: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode == http.StatusForbidden {
		return adminError(resp)
	}
	if resp.StatusCode/100 != 2 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github: delete ruleset returned %s: %s", resp.Status, rb)
	}
	return nil
}

var errRulesetNotFound = errors.New("github: ruleset not found")

func (c *AppClient) postRuleset(ctx context.Context, installationID int64, owner, repo string, body []byte) (int64, error) {
	req, err := http.NewRequest(http.MethodPost,
		c.apiBase+"/repos/"+owner+"/"+repo+"/rulesets", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.doRuleset(ctx, installationID, req, "create")
}

func (c *AppClient) putRuleset(ctx context.Context, installationID int64, owner, repo string, id int64, body []byte) (int64, error) {
	req, err := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/repos/%s/%s/rulesets/%d", c.apiBase, owner, repo, id), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.doRuleset(ctx, installationID, req, "update")
}

func (c *AppClient) doRuleset(ctx context.Context, installationID int64, req *http.Request, verb string) (int64, error) {
	resp, err := c.DoAsInstallation(ctx, installationID, req)
	if err != nil {
		return 0, fmt.Errorf("github: %s ruleset: %w", verb, err)
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusNotFound {
		return 0, errRulesetNotFound
	}
	if resp.StatusCode == http.StatusForbidden {
		return 0, adminError(resp)
	}
	if resp.StatusCode/100 != 2 {
		rb, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("github: %s ruleset returned %s: %s", verb, resp.Status, rb)
	}
	var raw struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return 0, fmt.Errorf("github: decode %s ruleset: %w", verb, err)
	}
	return raw.ID, nil
}

// rulesetPayload builds the repository-ruleset body: active enforcement, scoped
// to the repo's default branch (~DEFAULT_BRANCH, rename-proof), with one
// required_status_checks rule listing the contexts. Each check is pinned to the
// gocdnext App via integration_id so ONLY a status posted by gocdnext satisfies
// it — another actor with write access can't green the context to unblock the
// merge. strict policy is off (don't force the PR branch up-to-date) to keep
// merge friction low.
func rulesetPayload(in RulesetInput, integrationID int64) map[string]any {
	checks := make([]map[string]any, 0, len(in.Contexts))
	for _, c := range in.Contexts {
		check := map[string]any{"context": c}
		if integrationID > 0 {
			check["integration_id"] = integrationID
		}
		checks = append(checks, check)
	}
	return map[string]any{
		"name":        in.Name,
		"target":      "branch",
		"enforcement": "active",
		"conditions": map[string]any{
			"ref_name": map[string]any{
				"include": []string{"~DEFAULT_BRANCH"},
				"exclude": []string{},
			},
		},
		"rules": []map[string]any{{
			"type": "required_status_checks",
			"parameters": map[string]any{
				"required_status_checks":               checks,
				"strict_required_status_checks_policy": false,
			},
		}},
	}
}

func adminError(resp *http.Response) error {
	rb, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("%w (%s): %s", ErrAppLacksAdmin, resp.Status, rb)
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
