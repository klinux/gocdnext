package bitbucket

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
