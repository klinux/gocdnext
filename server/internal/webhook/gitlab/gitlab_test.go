package gitlab

import (
	"errors"
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
