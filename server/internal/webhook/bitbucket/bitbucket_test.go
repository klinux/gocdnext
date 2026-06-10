package bitbucket

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifySignature(t *testing.T) {
	secret := "shh"
	body := []byte(`{"push":{}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	good := hex.EncodeToString(mac.Sum(nil))

	tests := []struct {
		name      string
		secret    string
		header    string
		wantError error
	}{
		{"match raw", secret, good, nil},
		{"match with prefix", secret, "sha256=" + good, nil},
		{"mismatch", secret, "0000deadbeef", ErrInvalidSignature},
		{"empty header", secret, "", errSentinel("missing")},
		{"empty secret", "", good, errSentinel("empty")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifySignature(tt.secret, body, tt.header)
			switch {
			case tt.wantError == nil && err != nil:
				t.Errorf("want nil, got %v", err)
			case tt.wantError == ErrInvalidSignature && !errors.Is(err, ErrInvalidSignature):
				t.Errorf("want ErrInvalidSignature, got %v", err)
			case tt.wantError != nil && tt.wantError != ErrInvalidSignature && err == nil:
				t.Errorf("want error, got nil")
			}
		})
	}
}

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

func TestParsePushEvent(t *testing.T) {
	body := []byte(`{
		"repository": {
			"links": {
				"html": {"href": "https://bitbucket.org/acme/svc"},
				"clone": [
					{"name": "https", "href": "https://bitbucket.org/acme/svc.git"},
					{"name": "ssh", "href": "git@bitbucket.org:acme/svc.git"}
				]
			},
			"full_name": "acme/svc"
		},
		"push": {
			"changes": [{
				"new": {
					"type": "branch",
					"name": "main",
					"target": {
						"hash": "abc123",
						"message": "ship it",
						"date": "2026-04-24T10:00:00Z",
						"author": {"raw": "Alice <alice@x>", "user": {"display_name": "Alice"}}
					}
				},
				"old": {"target": {"hash": "prev456"}}
			}]
		}
	}`)
	ev, err := ParsePushEvent(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Branch != "main" {
		t.Errorf("branch = %q", ev.Branch)
	}
	if ev.After != "abc123" {
		t.Errorf("after = %q", ev.After)
	}
	if ev.Before != "prev456" {
		t.Errorf("before = %q", ev.Before)
	}
	if ev.Repository.CloneURL != "https://bitbucket.org/acme/svc.git" {
		t.Errorf("prefer https clone; got %q", ev.Repository.CloneURL)
	}
	if ev.HeadCommit == nil || ev.HeadCommit.Author != "Alice" {
		t.Errorf("head commit = %+v", ev.HeadCommit)
	}
}

func TestParsePushEvent_BranchDelete(t *testing.T) {
	body := []byte(`{
		"repository": {"links": {"html": {"href": "https://bitbucket.org/acme/svc"}}},
		"push": {"changes": [{"closed": true, "old": {"target": {"hash": "abc"}}}]}
	}`)
	ev, err := ParsePushEvent(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ev.Deleted {
		t.Errorf("closed change should flag Deleted=true; got %+v", ev)
	}
}

// TestEventKeyToAction covers the X-Event-Key strip. The router
// calls this BEFORE the body parser; everything outside the
// pullrequest:* family returns "" so the push path keeps owning
// repo:push (and unknown keys 204 with no work).
func TestEventKeyToAction(t *testing.T) {
	tests := map[string]string{
		"pullrequest:created":         "created",
		"pullrequest:updated":         "updated",
		"pullrequest:fulfilled":       "fulfilled",
		"pullrequest:rejected":        "rejected",
		"pullrequest:approved":        "approved",
		"pullrequest:comment_created": "comment_created",
		"repo:push":                   "",
		"repo:fork":                   "",
		"":                            "",
	}
	for key, want := range tests {
		t.Run(key, func(t *testing.T) {
			if got := EventKeyToAction(key); got != want {
				t.Errorf("EventKeyToAction(%q) = %q, want %q", key, got, want)
			}
			if want != "" && !IsPullRequestEvent(key) {
				t.Errorf("IsPullRequestEvent(%q) = false, want true", key)
			}
			if want == "" && IsPullRequestEvent(key) {
				t.Errorf("IsPullRequestEvent(%q) = true, want false", key)
			}
		})
	}
}

// TestParsePullRequestEvent_Fixture asserts every field the
// webhook dispatch path consumes from the canonical pr_created
// fixture — keeps drift between fixture and parser caught at
// unit-test time, same pattern as the gitlab side.
func TestParsePullRequestEvent_Fixture(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "pr_created.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ev, err := ParsePullRequestEvent(body, PRActionCreated)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Action != PRActionCreated {
		t.Errorf("action = %q, want %q", ev.Action, PRActionCreated)
	}
	if ev.Number != 5 {
		t.Errorf("number = %d, want 5", ev.Number)
	}
	if ev.HTMLURL != "https://bitbucket.org/acme/demo/pull-requests/5" {
		t.Errorf("html_url = %q", ev.HTMLURL)
	}
	if ev.Title != "Add caching to scheduler" {
		t.Errorf("title = %q", ev.Title)
	}
	if ev.Author != "operator-user" {
		t.Errorf("author = %q, want operator-user (nickname preferred)", ev.Author)
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
	if ev.Repository.CloneURL != "https://bitbucket.org/acme/demo.git" {
		t.Errorf("clone_url = %q, want https clone link preferred", ev.Repository.CloneURL)
	}
	if ev.RepoSlug != "acme/demo" {
		t.Errorf("repo_slug = %q, want acme/demo (used as log label)", ev.RepoSlug)
	}
	if ev.Merged {
		t.Errorf("state=OPEN should not flag Merged")
	}
	if !ev.IsTriggerableAction() {
		t.Errorf("created should be triggerable")
	}
	// Bitbucket Cloud has no PR label primitive — must stay nil
	// so cause_detail's pr_labels (omitempty) doesn't leak an
	// empty array.
	if ev.Labels != nil {
		t.Errorf("labels = %v, want nil (bitbucket has no PR labels)", ev.Labels)
	}
	// updated_on = "2026-06-10T12:00:00.123456+00:00" — must parse
	// via RFC3339Nano (microseconds + explicit offset).
	if ev.At.IsZero() {
		t.Errorf("At should parse a non-zero timestamp; got %v", ev.At)
	}
	if ev.At.Year() != 2026 || ev.At.Month() != 6 || ev.At.Day() != 10 {
		t.Errorf("At = %v, want 2026-06-10", ev.At)
	}
}

// TestParsePullRequestEvent_Errors covers the validation
// boundary — each shape the dispatch path treats as 400. Mirror
// of the gitlab error test so the contract stays uniform.
func TestParsePullRequestEvent_Errors(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		action string
	}{
		{"empty body", "", PRActionCreated},
		{"bad json", `{not json`, PRActionCreated},
		{"missing action verb", `{"pullrequest": {"id": 1}}`, ""},
		{"missing pullrequest block", `{"repository": {"full_name": "x/y"}}`, PRActionCreated},
		{"missing source.branch", `{"pullrequest": {
			"id": 1, "state": "OPEN",
			"source": {"commit": {"hash": "x"}},
			"destination": {"branch": {"name": "main"}, "repository": {"links": {"html": {"href": "https://x/y"}}}}
		}}`, PRActionCreated},
		{"missing source.commit", `{"pullrequest": {
			"id": 1, "state": "OPEN",
			"source": {"branch": {"name": "feat"}},
			"destination": {"branch": {"name": "main"}, "repository": {"links": {"html": {"href": "https://x/y"}}}}
		}}`, PRActionCreated},
		{"missing destination.branch", `{"pullrequest": {
			"id": 1, "state": "OPEN",
			"source": {"branch": {"name": "feat"}, "commit": {"hash": "x"}},
			"destination": {"repository": {"links": {"html": {"href": "https://x/y"}}}}
		}}`, PRActionCreated},
		{"missing clone url", `{"pullrequest": {
			"id": 1, "state": "OPEN",
			"source": {"branch": {"name": "feat"}, "commit": {"hash": "x"}},
			"destination": {"branch": {"name": "main"}}
		}}`, PRActionCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParsePullRequestEvent([]byte(tt.body), tt.action); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

// TestParsePullRequestEvent_RepositoryFallback covers the case
// where pullrequest.destination.repository is empty but the
// top-level `repository` envelope carries the clone link. Some
// older Bitbucket Server fixtures send the PR shape this way.
func TestParsePullRequestEvent_RepositoryFallback(t *testing.T) {
	body := []byte(`{
		"pullrequest": {
			"id": 3, "state": "OPEN",
			"source": {"branch": {"name": "feat"}, "commit": {"hash": "cafebabe"}},
			"destination": {"branch": {"name": "main"}}
		},
		"repository": {
			"full_name": "acme/demo",
			"links": {
				"html": {"href": "https://bitbucket.org/acme/demo"},
				"clone": [{"name": "https", "href": "https://bitbucket.org/acme/demo.git"}]
			}
		}
	}`)
	ev, err := ParsePullRequestEvent(body, PRActionUpdated)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Repository.CloneURL != "https://bitbucket.org/acme/demo.git" {
		t.Errorf("clone_url = %q, want repository fallback", ev.Repository.CloneURL)
	}
	if ev.RepoSlug != "acme/demo" {
		t.Errorf("repo_slug = %q, want fallback to top-level repository.full_name", ev.RepoSlug)
	}
	if ev.Action != PRActionUpdated || !ev.IsTriggerableAction() {
		t.Errorf("updated should be triggerable; got %q", ev.Action)
	}
}

// TestPullRequestEvent_IsTriggerableAction asserts the
// created/updated subset — fulfilled (merged) and rejected
// (declined) intentionally fall through, mirroring the
// github close + gitlab close/merge contract.
func TestPullRequestEvent_IsTriggerableAction(t *testing.T) {
	tests := []struct {
		action string
		want   bool
	}{
		{PRActionCreated, true},
		{PRActionUpdated, true},
		{PRActionFulfilled, false},
		{PRActionRejected, false},
		{"approved", false},
		{"comment_created", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			got := PullRequestEvent{Action: tt.action}.IsTriggerableAction()
			if got != tt.want {
				t.Errorf("action=%q triggerable=%v, want %v", tt.action, got, tt.want)
			}
		})
	}

	// Defence-in-depth: state=MERGED short-circuits even when
	// action is a normally-triggerable verb. Bitbucket has been
	// observed sending a trailing `updated` after merge in some
	// race conditions — firing a build for it would race the
	// push-to-destination path.
	t.Run("merged short-circuits updated", func(t *testing.T) {
		ev := PullRequestEvent{Action: PRActionUpdated, Merged: true}
		if ev.IsTriggerableAction() {
			t.Errorf("merged=true must not be triggerable regardless of action")
		}
	})
}
