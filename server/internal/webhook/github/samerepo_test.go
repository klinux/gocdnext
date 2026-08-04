package github

import (
	"encoding/json"
	"testing"
)

// prOpts builds a minimal-but-valid pull_request payload, letting each
// test control the three repo ids and whether head.repo / base.repo are
// present at all (GitHub omits them for some fork/deleted states).
type prOpts struct {
	headRepoPresent bool
	headRepoID      int64
	headCloneURL    string
	baseRepoPresent bool
	baseRepoID      int64
	baseCloneURL    string
	topRepoID       int64
}

func buildPRBody(t *testing.T, o prOpts) []byte {
	t.Helper()
	head := map[string]any{"ref": "feature", "sha": "aaaaaaaa"}
	if o.headRepoPresent {
		head["repo"] = map[string]any{"id": o.headRepoID, "clone_url": o.headCloneURL}
	}
	base := map[string]any{"ref": "main", "sha": "bbbbbbbb"}
	if o.baseRepoPresent {
		base["repo"] = map[string]any{"id": o.baseRepoID, "clone_url": o.baseCloneURL}
	}
	body := map[string]any{
		"action": "opened",
		"number": 1,
		"pull_request": map[string]any{
			"html_url": "https://github.com/acme/app/pull/1",
			"head":     head,
			"base":     base,
		},
		"repository": map[string]any{
			"id":        o.topRepoID,
			"clone_url": "https://github.com/acme/app.git",
			"full_name": "acme/app",
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// TestParsePullRequestEvent_SameRepo covers the fail-closed identity
// predicate for #223: same-repo is decided by immutable repo id only, and
// anything ambiguous (fork, missing id, inconsistent payload, or a
// deceptively-equal clone url) must resolve to SameRepo=false.
func TestParsePullRequestEvent_SameRepo(t *testing.T) {
	const baseURL = "https://github.com/acme/app.git"
	tests := []struct {
		name       string
		opts       prOpts
		wantSame   bool
		wantHeadID int64
		wantBaseID int64
	}{
		{
			name:       "same-repo branch PR",
			opts:       prOpts{true, 100, baseURL, true, 100, baseURL, 100},
			wantSame:   true,
			wantHeadID: 100,
			wantBaseID: 100,
		},
		{
			name:       "fork PR (different head id)",
			opts:       prOpts{true, 200, "https://github.com/mallory/app.git", true, 100, baseURL, 100},
			wantSame:   false,
			wantHeadID: 200,
			wantBaseID: 100,
		},
		{
			name:       "fork with deceptively-equal clone_url still not same",
			opts:       prOpts{true, 200, baseURL, true, 100, baseURL, 100},
			wantSame:   false,
			wantHeadID: 200,
			wantBaseID: 100,
		},
		{
			name:       "head.repo omitted -> fail closed",
			opts:       prOpts{false, 0, "", true, 100, baseURL, 100},
			wantSame:   false,
			wantHeadID: 0,
			wantBaseID: 100,
		},
		{
			name:       "base.repo omitted -> fail closed",
			opts:       prOpts{true, 100, baseURL, false, 0, "", 100},
			wantSame:   false,
			wantHeadID: 100,
			wantBaseID: 0,
		},
		{
			name:       "zero head id -> fail closed",
			opts:       prOpts{true, 0, baseURL, true, 100, baseURL, 100},
			wantSame:   false,
			wantHeadID: 0,
			wantBaseID: 100,
		},
		{
			name:       "base id inconsistent with top-level repository.id -> fail closed",
			opts:       prOpts{true, 100, baseURL, true, 100, baseURL, 999},
			wantSame:   false,
			wantHeadID: 100,
			wantBaseID: 100,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, err := ParsePullRequestEvent(buildPRBody(t, tt.opts))
			if err != nil {
				t.Fatalf("ParsePullRequestEvent: %v", err)
			}
			if ev.SameRepo != tt.wantSame {
				t.Errorf("SameRepo = %v, want %v", ev.SameRepo, tt.wantSame)
			}
			if ev.HeadRepoID != tt.wantHeadID {
				t.Errorf("HeadRepoID = %d, want %d", ev.HeadRepoID, tt.wantHeadID)
			}
			if ev.BaseRepoID != tt.wantBaseID {
				t.Errorf("BaseRepoID = %d, want %d", ev.BaseRepoID, tt.wantBaseID)
			}
		})
	}
}

// TestSameRepoGitHub_Unit exercises the pure predicate directly, so the
// fail-closed branches are pinned independent of JSON plumbing.
func TestSameRepoGitHub_Unit(t *testing.T) {
	tests := []struct {
		name            string
		head, base, top int64
		want            bool
	}{
		{"all equal", 1, 1, 1, true},
		{"head differs (fork)", 2, 1, 1, false},
		{"head zero", 0, 1, 1, false},
		{"base zero", 1, 0, 1, false},
		{"top zero", 1, 1, 0, false},
		{"base != top (inconsistent)", 1, 1, 2, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameRepoGitHub(tt.head, tt.base, tt.top); got != tt.want {
				t.Errorf("sameRepoGitHub(%d,%d,%d) = %v, want %v", tt.head, tt.base, tt.top, got, tt.want)
			}
		})
	}
}
