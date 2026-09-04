package github

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MergeGroupEvent is the GitHub App-delivered `merge_group` webhook,
// trimmed to the fields gocdnext needs to validate a merge queue entry.
//
// GitHub asks third-party CI to report the required check against
// MergeGroup.HeadSHA, not the PR head SHA. HeadRef is the ephemeral
// gh-readonly-queue ref the agent checks out; BaseRef identifies which
// default/release branch material should fire.
type MergeGroupEvent struct {
	Action string

	AppID          int64
	InstallationID int64
	Repository     Repository

	HeadSHA string
	HeadRef string
	BaseSHA string
	BaseRef string
	Commit  MergeGroupCommit
	Reason  string
}

type MergeGroupCommit struct {
	ID        string
	Message   string
	Timestamp time.Time
}

// ParseMergeGroupEvent decodes a `merge_group` webhook body. Required
// merge_group fields are validated explicitly so a schema drift or malformed
// delivery cannot silently create a zero-SHA run.
func ParseMergeGroupEvent(body []byte) (MergeGroupEvent, error) {
	var raw struct {
		Action string `json:"action"`
		App    struct {
			ID int64 `json:"id"`
		} `json:"app"`
		Reason       string `json:"reason"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Repository Repository `json:"repository"`
		MergeGroup struct {
			HeadSHA    string `json:"head_sha"`
			HeadRef    string `json:"head_ref"`
			BaseSHA    string `json:"base_sha"`
			BaseRef    string `json:"base_ref"`
			HeadCommit struct {
				ID        string    `json:"id"`
				Message   string    `json:"message"`
				Timestamp time.Time `json:"timestamp"`
			} `json:"head_commit"`
		} `json:"merge_group"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return MergeGroupEvent{}, fmt.Errorf("parse merge_group event: %w", err)
	}
	switch raw.Action {
	case "checks_requested", "destroyed":
	default:
		return MergeGroupEvent{}, fmt.Errorf("unsupported merge_group action %q (accepted: checks_requested, destroyed)", raw.Action)
	}

	required := []struct {
		name  string
		value string
	}{
		{"merge_group.head_sha", raw.MergeGroup.HeadSHA},
		{"merge_group.head_ref", raw.MergeGroup.HeadRef},
		{"merge_group.base_sha", raw.MergeGroup.BaseSHA},
		{"merge_group.base_ref", raw.MergeGroup.BaseRef},
	}
	for _, f := range required {
		if strings.TrimSpace(f.value) == "" {
			return MergeGroupEvent{}, fmt.Errorf("merge_group payload missing %s", f.name)
		}
	}
	if raw.Repository.FullName == "" || raw.Repository.CloneURL == "" {
		return MergeGroupEvent{}, ErrMissingRepository
	}

	return MergeGroupEvent{
		Action:         raw.Action,
		AppID:          raw.App.ID,
		InstallationID: raw.Installation.ID,
		Repository:     raw.Repository,
		HeadSHA:        raw.MergeGroup.HeadSHA,
		HeadRef:        raw.MergeGroup.HeadRef,
		BaseSHA:        raw.MergeGroup.BaseSHA,
		BaseRef:        raw.MergeGroup.BaseRef,
		Commit: MergeGroupCommit{
			ID:        raw.MergeGroup.HeadCommit.ID,
			Message:   raw.MergeGroup.HeadCommit.Message,
			Timestamp: raw.MergeGroup.HeadCommit.Timestamp,
		},
		Reason: raw.Reason,
	}, nil
}
