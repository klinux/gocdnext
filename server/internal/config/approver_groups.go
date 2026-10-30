package config

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Bounds for GOCDNEXT_APPROVER_GROUPS_TIMEOUT. Every vote that needs the external
// source waits up to this long, so the ceiling stays well under an HTTP request's
// patience; the floor keeps a typo like "1ms" from refusing every vote.
const (
	approverGroupsTimeoutMin = 100 * time.Millisecond
	approverGroupsTimeoutMax = 30 * time.Second
	approverGroupsTimeoutDef = 2 * time.Second
)

// ApproverGroups configures the optional external approver-group source (ADR-0002):
// group membership resolved against the platform at vote time. URL empty = off, and
// the local group table stays the only source.
type ApproverGroups struct {
	URL     string
	Token   string
	Timeout time.Duration
}

// loadApproverGroups reads GOCDNEXT_APPROVER_GROUPS_*. A misconfiguration aborts boot
// rather than refusing every vote that needs the source later. URL shape is checked
// by groupsource.New at wiring time. Errors name the variable, never the token.
func loadApproverGroups() (ApproverGroups, error) {
	g := ApproverGroups{
		URL:     strings.TrimSpace(env("GOCDNEXT_APPROVER_GROUPS_URL", "")),
		Token:   env("GOCDNEXT_APPROVER_GROUPS_TOKEN", ""),
		Timeout: approverGroupsTimeoutDef,
	}
	if raw := strings.TrimSpace(env("GOCDNEXT_APPROVER_GROUPS_TIMEOUT", "")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return ApproverGroups{}, fmt.Errorf("GOCDNEXT_APPROVER_GROUPS_TIMEOUT: %w", err)
		}
		if d < approverGroupsTimeoutMin || d > approverGroupsTimeoutMax {
			return ApproverGroups{}, fmt.Errorf("GOCDNEXT_APPROVER_GROUPS_TIMEOUT must be within [%s, %s], got %s",
				approverGroupsTimeoutMin, approverGroupsTimeoutMax, d)
		}
		g.Timeout = d
	}
	if g.URL == "" {
		if g.Token != "" {
			slog.Warn("config: GOCDNEXT_APPROVER_GROUPS_TOKEN is set but GOCDNEXT_APPROVER_GROUPS_URL is empty; external approver groups stay off")
		}
		return g, nil
	}
	if g.Token == "" {
		return ApproverGroups{}, fmt.Errorf("GOCDNEXT_APPROVER_GROUPS_URL is set but GOCDNEXT_APPROVER_GROUPS_TOKEN is empty")
	}
	return g, nil
}
