package github

import (
	"encoding/json"
	"fmt"
)

// CheckRunEvent is the inbound `check_run` (and `check_suite`) webhook payload,
// trimmed to what the GitHub App re-run path needs. GitHub delivers the
// `rerequested` action (the PR "Re-run" button) ONLY to the GitHub App that
// created the check run, signed with the App's webhook secret.
//
//   - ExternalID is what gocdnext stamped on the check run at creation: the run
//     UUID. That's the direct correlation to the run to re-run.
//   - AppID (check_run.app.id) selects the candidate App integration whose
//     webhook secret verifies the HMAC — trusted for nothing until it does.
//   - CheckRunID / InstallationID / Owner / Repo are cross-checked against the
//     persisted github_check_runs identity before acting.
type CheckRunEvent struct {
	Action         string
	CheckRunID     int64
	ExternalID     string
	AppID          int64
	InstallationID int64
	Owner          string
	Repo           string
}

// ParseCheckRunEvent decodes a `check_run`/`check_suite` webhook body.
func ParseCheckRunEvent(body []byte) (CheckRunEvent, error) {
	var raw struct {
		Action   string `json:"action"`
		CheckRun struct {
			ID         int64  `json:"id"`
			ExternalID string `json:"external_id"`
			App        struct {
				ID int64 `json:"id"`
			} `json:"app"`
		} `json:"check_run"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Repository struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return CheckRunEvent{}, fmt.Errorf("parse check_run event: %w", err)
	}
	return CheckRunEvent{
		Action:         raw.Action,
		CheckRunID:     raw.CheckRun.ID,
		ExternalID:     raw.CheckRun.ExternalID,
		AppID:          raw.CheckRun.App.ID,
		InstallationID: raw.Installation.ID,
		Owner:          raw.Repository.Owner.Login,
		Repo:           raw.Repository.Name,
	}, nil
}

// PingEvent is the App-webhook setup `ping`. hook.app_id identifies the App the
// webhook belongs to — used to select the candidate secret (headers carry no
// App id). Zero when absent, and the caller falls back to the sole configured
// App integration.
type PingEvent struct {
	HookAppID int64
}

// ParsePingEvent decodes a `ping` webhook body.
func ParsePingEvent(body []byte) (PingEvent, error) {
	var raw struct {
		Hook struct {
			AppID int64 `json:"app_id"`
		} `json:"hook"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return PingEvent{}, fmt.Errorf("parse ping event: %w", err)
	}
	return PingEvent{HookAppID: raw.Hook.AppID}, nil
}
