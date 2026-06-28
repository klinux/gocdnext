package github_test

import (
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
)

func TestParsePullRequestEvent_LifecycleTimestamps(t *testing.T) {
	body := []byte(`{
      "action": "opened",
      "number": 42,
      "pull_request": {
        "html_url": "https://github.com/acme/web/pull/42",
        "title": "Add feature",
        "merged": false,
        "created_at": "2026-06-01T10:00:00Z",
        "updated_at": "2026-06-01T10:05:00Z",
        "merge_commit_sha": "",
        "user": {"login": "dev"},
        "head": {"ref": "feat", "sha": "headsha"},
        "base": {"ref": "main", "sha": "basesha"}
      },
      "repository": {"full_name": "acme/web", "clone_url": "https://github.com/acme/web.git"}
    }`)
	ev, err := github.ParsePullRequestEvent(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ev.CreatedAt.Equal(time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("created_at = %v", ev.CreatedAt)
	}
	if ev.Repository.FullName != "acme/web" {
		t.Errorf("repo = %q", ev.Repository.FullName)
	}
	if !ev.MergedAt.IsZero() || ev.MergeSHA != "" {
		t.Errorf("not merged yet: mergedAt=%v sha=%q", ev.MergedAt, ev.MergeSHA)
	}
}

func TestParsePullRequestEvent_Merged(t *testing.T) {
	body := []byte(`{
      "action": "closed",
      "number": 42,
      "pull_request": {
        "html_url": "u", "title": "t", "merged": true,
        "created_at": "2026-06-01T10:00:00Z",
        "updated_at": "2026-06-02T09:00:00Z",
        "merged_at": "2026-06-02T09:00:00Z",
        "merge_commit_sha": "mergesha123",
        "user": {"login": "dev"},
        "head": {"ref": "feat", "sha": "headsha"},
        "base": {"ref": "main", "sha": "basesha"}
      },
      "repository": {"full_name": "acme/web", "clone_url": "https://github.com/acme/web.git"}
    }`)
	ev, err := github.ParsePullRequestEvent(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ev.Merged {
		t.Fatal("want merged")
	}
	if ev.MergeSHA != "mergesha123" {
		t.Errorf("merge sha = %q", ev.MergeSHA)
	}
	if !ev.MergedAt.Equal(time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("merged_at = %v", ev.MergedAt)
	}
}

func TestParseReviewEvent_Approved(t *testing.T) {
	body := []byte(`{
      "action": "submitted",
      "review": {"state": "approved", "submitted_at": "2026-06-01T15:30:00Z", "user": {"login": "lead"}},
      "pull_request": {"number": 42},
      "repository": {"full_name": "acme/web", "clone_url": "https://github.com/acme/web.git"}
    }`)
	ev, err := github.ParseReviewEvent(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ev.IsApproval() {
		t.Fatal("want approval")
	}
	if ev.Number != 42 || ev.Reviewer != "lead" {
		t.Errorf("ev = %+v", ev)
	}
	if !ev.SubmittedAt.Equal(time.Date(2026, 6, 1, 15, 30, 0, 0, time.UTC)) {
		t.Errorf("submitted_at = %v", ev.SubmittedAt)
	}
}

func TestReviewEvent_IsApproval(t *testing.T) {
	cases := []struct {
		action, state string
		want          bool
	}{
		{"submitted", "approved", true},
		{"submitted", "APPROVED", true},
		{"submitted", "changes_requested", false},
		{"submitted", "commented", false},
		{"dismissed", "approved", false},
	}
	for _, c := range cases {
		got := github.ReviewEvent{Action: c.action, State: c.state}.IsApproval()
		if got != c.want {
			t.Errorf("IsApproval(%s,%s) = %v, want %v", c.action, c.state, got, c.want)
		}
	}
}
