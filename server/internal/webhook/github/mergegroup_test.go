package github

import (
	"strings"
	"testing"
)

func TestParseMergeGroupEvent(t *testing.T) {
	body := []byte(`{
		"action": "checks_requested",
		"app": {"id": 424242},
		"installation": {"id": 100},
		"repository": {
			"id": 1,
			"full_name": "org/repo",
			"clone_url": "https://github.com/org/repo.git"
		},
		"merge_group": {
			"head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"head_ref": "refs/heads/gh-readonly-queue/main/pr-1-aaaa",
			"base_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"base_ref": "refs/heads/main",
			"head_commit": {
				"id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"message": "Merge queue",
				"timestamp": "2026-08-30T10:00:00Z",
				"author": null,
				"committer": null
			}
		}
	}`)
	ev, err := ParseMergeGroupEvent(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Action != "checks_requested" || ev.AppID != 424242 || ev.InstallationID != 100 {
		t.Fatalf("metadata = %+v", ev)
	}
	if ev.HeadSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		ev.HeadRef != "refs/heads/gh-readonly-queue/main/pr-1-aaaa" ||
		ev.BaseSHA != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" ||
		ev.BaseRef != "refs/heads/main" {
		t.Fatalf("merge_group fields = %+v", ev)
	}
	if ev.Commit.Message != "Merge queue" || ev.Commit.Timestamp.IsZero() {
		t.Fatalf("commit = %+v", ev.Commit)
	}
}

func TestParseMergeGroupEvent_DestroyedReasonOptional(t *testing.T) {
	body := []byte(`{
		"action": "destroyed",
		"installation": {"id": 100},
		"repository": {"full_name": "org/repo", "clone_url": "https://github.com/org/repo.git"},
		"merge_group": {
			"head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"head_ref": "refs/heads/gh-readonly-queue/main/pr-1-aaaa",
			"base_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"base_ref": "refs/heads/main"
		}
	}`)
	ev, err := ParseMergeGroupEvent(body)
	if err != nil {
		t.Fatalf("parse destroyed without reason: %v", err)
	}
	if ev.Action != "destroyed" || ev.Reason != "" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestParseMergeGroupEvent_DestroyedReasonTopLevel(t *testing.T) {
	body := []byte(`{
		"action": "destroyed",
		"reason": "invalidated",
		"installation": {"id": 100},
		"repository": {"full_name": "org/repo", "clone_url": "https://github.com/org/repo.git"},
		"merge_group": {
			"head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"head_ref": "refs/heads/gh-readonly-queue/main/pr-1-aaaa",
			"base_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"base_ref": "refs/heads/main"
		}
	}`)
	ev, err := ParseMergeGroupEvent(body)
	if err != nil {
		t.Fatalf("parse destroyed with reason: %v", err)
	}
	if ev.Reason != "invalidated" {
		t.Fatalf("reason = %q, want invalidated", ev.Reason)
	}
}

func TestParseMergeGroupEvent_MissingRequiredField(t *testing.T) {
	body := []byte(`{
		"action": "checks_requested",
		"repository": {"full_name": "org/repo", "clone_url": "https://github.com/org/repo.git"},
		"merge_group": {
			"head_ref": "refs/heads/gh-readonly-queue/main/pr-1-aaaa",
			"base_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"base_ref": "refs/heads/main",
			"head_commit": {"id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
		}
	}`)
	_, err := ParseMergeGroupEvent(body)
	if err == nil {
		t.Fatal("expected missing head_sha to fail")
	}
	if !strings.Contains(err.Error(), "merge_group.head_sha") {
		t.Fatalf("err = %v, want field name", err)
	}
}

func TestParseMergeGroupEvent_UnknownAction(t *testing.T) {
	body := []byte(`{
		"action": "queued",
		"repository": {"full_name": "org/repo", "clone_url": "https://github.com/org/repo.git"},
		"merge_group": {
			"head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"head_ref": "refs/heads/gh-readonly-queue/main/pr-1-aaaa",
			"base_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"base_ref": "refs/heads/main"
		}
	}`)
	_, err := ParseMergeGroupEvent(body)
	if err == nil {
		t.Fatal("expected unknown action to fail")
	}
	if !strings.Contains(err.Error(), "accepted: checks_requested, destroyed") {
		t.Fatalf("err = %v, want accepted set", err)
	}
}
