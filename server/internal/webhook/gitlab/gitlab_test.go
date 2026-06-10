package gitlab

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyToken(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		header string
		want   error
	}{
		{"match", "shh", "shh", nil},
		{"mismatch", "shh", "nope", ErrBadToken},
		{"empty secret", "", "shh", errSentinel("empty")},
		{"empty header", "shh", "", errSentinel("missing")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyToken(tt.secret, tt.header)
			switch {
			case tt.want == nil && err != nil:
				t.Errorf("expected nil, got %v", err)
			case tt.want == ErrBadToken && !errors.Is(err, ErrBadToken):
				t.Errorf("expected ErrBadToken, got %v", err)
			case tt.want != nil && tt.want != ErrBadToken && err == nil:
				t.Errorf("expected an error, got nil")
			}
		})
	}
}

// errSentinel is a local marker so the test table can express
// "we want any error" without leaking implementation details.
type errSentinel string

func (e errSentinel) Error() string { return string(e) }

func TestParsePushEvent(t *testing.T) {
	body := []byte(`{
		"object_kind": "push",
		"ref": "refs/heads/main",
		"before": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"after":  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"repository": {
			"git_http_url": "https://gitlab.com/org/repo.git"
		},
		"commits": [
			{"id": "aaaa", "message": "old"},
			{"id": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "message": "tip",
			 "timestamp": "2026-04-24T10:00:00Z",
			 "author": {"name": "Alice", "email": "alice@example.com"}}
		]
	}`)
	ev, err := ParsePushEvent(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Branch != "main" {
		t.Errorf("branch = %q, want main", ev.Branch)
	}
	if ev.After != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("after = %q", ev.After)
	}
	if ev.Repository.CloneURL != "https://gitlab.com/org/repo.git" {
		t.Errorf("cloneURL = %q", ev.Repository.CloneURL)
	}
	if ev.HeadCommit == nil || ev.HeadCommit.Author != "Alice" {
		t.Errorf("head commit = %+v", ev.HeadCommit)
	}
	if ev.Deleted {
		t.Errorf("should not be deleted")
	}
}

func TestParsePushEvent_BranchDelete(t *testing.T) {
	body := []byte(`{
		"object_kind": "push",
		"ref": "refs/heads/featbranch",
		"after": "0000000000000000000000000000000000000000",
		"repository": {"git_http_url": "https://gitlab.com/org/repo.git"}
	}`)
	ev, err := ParsePushEvent(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ev.Deleted {
		t.Errorf("zero-sha push should flag Deleted=true; got %+v", ev)
	}
}

// TestParseMergeRequestEvent_Fixture runs the parser against the
// canonical mr_opened.json fixture — same fixture shape used by the
// integration tests downstream — and asserts every field the
// webhook dispatch path consumes. Keeps drift between fixture and
// parser caught at unit-test time.
func TestParseMergeRequestEvent_Fixture(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "mr_opened.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ev, err := ParseMergeRequestEvent(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Action != MRActionOpen {
		t.Errorf("action = %q, want %q", ev.Action, MRActionOpen)
	}
	if ev.Number != 7 {
		t.Errorf("number = %d, want 7", ev.Number)
	}
	if ev.HTMLURL != "https://gitlab.example.com/group/demo/-/merge_requests/7" {
		t.Errorf("html_url = %q", ev.HTMLURL)
	}
	if ev.Title != "Add caching to scheduler" {
		t.Errorf("title = %q", ev.Title)
	}
	if ev.Author != "operator-user" {
		t.Errorf("author = %q, want operator-user (user.username)", ev.Author)
	}
	if ev.HeadSHA != "abc0123456789abc0123456789abc0123456789a" {
		t.Errorf("head_sha = %q", ev.HeadSHA)
	}
	if ev.HeadRef != "feat/cache-scheduler" {
		t.Errorf("head_ref = %q", ev.HeadRef)
	}
	if ev.BaseRef != "main" {
		t.Errorf("base_ref = %q", ev.BaseRef)
	}
	if ev.Repository.CloneURL != "https://gitlab.example.com/group/demo.git" {
		t.Errorf("clone_url = %q", ev.Repository.CloneURL)
	}
	if ev.ProjectPath != "group/demo" {
		t.Errorf("project_path = %q, want group/demo (used as log label)", ev.ProjectPath)
	}
	if ev.Merged {
		t.Errorf("state=opened should not flag Merged")
	}
	if !ev.IsTriggerableAction() {
		t.Errorf("open should be triggerable")
	}
	// Labels: ["hotfix", "Hotfix", "  needs-review  ", ""] →
	// ["hotfix", "needs-review"] (lowercased, deduped, trimmed,
	// empty dropped — same contract as the github side).
	wantLabels := []string{"hotfix", "needs-review"}
	if len(ev.Labels) != len(wantLabels) {
		t.Fatalf("labels = %v, want %v", ev.Labels, wantLabels)
	}
	for i, l := range wantLabels {
		if ev.Labels[i] != l {
			t.Errorf("labels[%d] = %q, want %q", i, ev.Labels[i], l)
		}
	}
	// UpdatedAt = "2026-06-10 12:00:00 UTC" → parsed via space-MST form.
	if ev.At.IsZero() {
		t.Errorf("At should parse a non-zero timestamp; got %v", ev.At)
	}
	if ev.At.Year() != 2026 || ev.At.Month() != 6 || ev.At.Day() != 10 {
		t.Errorf("At = %v, want 2026-06-10", ev.At)
	}
}

// TestParseMergeRequestEvent_Errors covers the validation boundary —
// each shape that GitLab's own webhook validator would have caught.
// The dispatch path treats any error as a 400 BadRequest, so the
// only thing the caller assumes is "errored or not".
func TestParseMergeRequestEvent_Errors(t *testing.T) {
	base := func() string {
		return `{
			"object_kind": "merge_request",
			"project": {"git_http_url": "https://gitlab.com/org/repo.git"},
			"object_attributes": {
				"iid": 1, "action": "open",
				"source_branch": "feat", "target_branch": "main",
				"last_commit": {"id": "deadbeef"}
			}
		}`
	}
	tests := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"bad json", `{not json`},
		{"wrong object_kind", `{"object_kind": "push"}`},
		{"missing object_attributes", `{"object_kind": "merge_request"}`},
		{"missing action", `{"object_kind": "merge_request",
			"project": {"git_http_url": "https://x/x.git"},
			"object_attributes": {"iid": 1, "source_branch": "a", "target_branch": "b",
				"last_commit": {"id": "x"}}}`},
		{"missing clone url", `{"object_kind": "merge_request",
			"object_attributes": {"iid": 1, "action": "open",
				"source_branch": "a", "target_branch": "b",
				"last_commit": {"id": "x"}}}`},
		{"missing last_commit", `{"object_kind": "merge_request",
			"project": {"git_http_url": "https://x/x.git"},
			"object_attributes": {"iid": 1, "action": "open",
				"source_branch": "a", "target_branch": "b"}}`},
		{"missing source_branch", `{"object_kind": "merge_request",
			"project": {"git_http_url": "https://x/x.git"},
			"object_attributes": {"iid": 1, "action": "open",
				"target_branch": "main", "last_commit": {"id": "x"}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = base // referenced by future expansion of valid cases
			if _, err := ParseMergeRequestEvent([]byte(tt.body)); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

// TestParseMergeRequestEvent_RepositoryFallback covers the case
// where project.git_http_url is missing but the top-level
// repository block carries one. Older GitLab versions and some
// self-hosted setups send the MR payload in this shape — we
// MUST NOT 400 on them.
func TestParseMergeRequestEvent_RepositoryFallback(t *testing.T) {
	body := []byte(`{
		"object_kind": "merge_request",
		"repository": {"git_http_url": "https://gitlab.example.com/org/repo.git"},
		"object_attributes": {
			"iid": 3, "action": "update",
			"source_branch": "feat", "target_branch": "main",
			"last_commit": {"id": "cafebabe"}
		}
	}`)
	ev, err := ParseMergeRequestEvent(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Repository.CloneURL != "https://gitlab.example.com/org/repo.git" {
		t.Errorf("clone_url = %q, want repository fallback", ev.Repository.CloneURL)
	}
	if ev.Action != MRActionUpdate || !ev.IsTriggerableAction() {
		t.Errorf("update should be triggerable; got %q", ev.Action)
	}
}

// TestMergeRequestEvent_IsTriggerableAction asserts the
// open/update/reopen subset — close/merge intentionally fall
// through (the subsequent push to target_branch handles those).
func TestMergeRequestEvent_IsTriggerableAction(t *testing.T) {
	tests := []struct {
		action string
		want   bool
	}{
		{MRActionOpen, true},
		{MRActionUpdate, true},
		{MRActionReopen, true},
		{MRActionClose, false},
		{MRActionMerge, false},
		{"approved", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			got := MergeRequestEvent{Action: tt.action}.IsTriggerableAction()
			if got != tt.want {
				t.Errorf("action=%q triggerable=%v, want %v", tt.action, got, tt.want)
			}
		})
	}

	// Defence-in-depth: state=merged short-circuits even when
	// action is a normally-triggerable verb. Some self-hosted
	// GitLab installs emit a trailing `update` after the merge —
	// firing a build for it would race the push-to-target path.
	t.Run("merged short-circuits update", func(t *testing.T) {
		ev := MergeRequestEvent{Action: MRActionUpdate, Merged: true}
		if ev.IsTriggerableAction() {
			t.Errorf("merged=true must not be triggerable regardless of action")
		}
	})
}

// TestNormaliseMRLabels covers the dedup/lowercase/trim contract.
// Same shape as the github normaliseLabels test — mirroring is
// load-bearing because quorum_by_label downstream is provider-
// agnostic on the labels list.
func TestNormaliseMRLabels(t *testing.T) {
	tests := []struct {
		name string
		in   []mrLabel
		want []string
	}{
		{"nil", nil, nil},
		{"empty", []mrLabel{}, nil},
		{"all-empty-titles", []mrLabel{{""}, {"  "}}, nil},
		{
			"lowercase + dedupe + preserve first-seen order",
			[]mrLabel{{"Hotfix"}, {"  needs-review  "}, {"hotfix"}, {"Needs-Review"}},
			[]string{"hotfix", "needs-review"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normaliseMRLabels(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
