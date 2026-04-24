// Package gitlab verifies GitLab push webhooks and turns them
// into the same PushEvent shape the rest of the webhook pipeline
// consumes. Signature scheme is a shared-secret token compare
// (GitLab passes it raw in X-Gitlab-Token) — simpler than
// GitHub's HMAC, but still constant-time-compared so a cache
// timing attack can't leak the secret.
package gitlab

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrBadToken signals the X-Gitlab-Token header missed the
// secret registered for this scm_source. Wrapped at the caller
// as 401 Unauthorized.
var ErrBadToken = errors.New("gitlab: invalid webhook token")

// VerifyToken constant-time compares the X-Gitlab-Token header
// against the stored plaintext secret. GitLab's signature story
// is: operator pastes the same string into the "Secret token"
// field on the repo webhook config; on delivery GitLab sends it
// back verbatim as the header. No HMAC, no body signing.
func VerifyToken(secret, header string) error {
	if secret == "" {
		return fmt.Errorf("gitlab: secret is empty")
	}
	if header == "" {
		return fmt.Errorf("gitlab: token header missing")
	}
	if subtle.ConstantTimeCompare([]byte(secret), []byte(header)) != 1 {
		return ErrBadToken
	}
	return nil
}

// PushEvent is the shape the webhook handler consumes — matches
// github.PushEvent so the downstream path doesn't branch on
// provider. Only the fields we actually use are surfaced.
type PushEvent struct {
	Ref        string
	Branch     string
	After      string
	Before     string
	Deleted    bool
	Repository RepositoryRef
	HeadCommit *Commit
}

type RepositoryRef struct {
	CloneURL string
}

type Commit struct {
	ID        string
	Message   string
	Author    string
	Timestamp time.Time
}

// ParsePushEvent decodes GitLab's "Push Hook" payload. Rejects
// non-push events at the router level (caller filters by
// X-Gitlab-Event). The returned event uses the same field names
// as the github equivalent so callers treat them uniformly.
func ParsePushEvent(body []byte) (*PushEvent, error) {
	var raw struct {
		ObjectKind string `json:"object_kind"`
		Ref        string `json:"ref"`
		Before     string `json:"before"`
		After      string `json:"after"`
		Repository struct {
			GitHTTPURL string `json:"git_http_url"`
			URL        string `json:"url"`
		} `json:"repository"`
		Commits []struct {
			ID        string `json:"id"`
			Message   string `json:"message"`
			Timestamp string `json:"timestamp"`
			Author    struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"author"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("gitlab: decode push: %w", err)
	}
	if raw.ObjectKind != "" && raw.ObjectKind != "push" {
		return nil, fmt.Errorf("gitlab: expected object_kind=push, got %q", raw.ObjectKind)
	}
	if raw.Ref == "" {
		return nil, fmt.Errorf("gitlab: push event missing ref")
	}
	cloneURL := raw.Repository.GitHTTPURL
	if cloneURL == "" {
		cloneURL = raw.Repository.URL
	}
	if cloneURL == "" {
		return nil, fmt.Errorf("gitlab: push event missing repository url")
	}

	ev := &PushEvent{
		Ref:        raw.Ref,
		Before:     raw.Before,
		After:      raw.After,
		Repository: RepositoryRef{CloneURL: cloneURL},
	}
	// GitLab signals branch delete with after=000...0.
	if ev.After == "" || strings.Trim(ev.After, "0") == "" {
		ev.Deleted = true
	}
	if strings.HasPrefix(ev.Ref, "refs/heads/") {
		ev.Branch = strings.TrimPrefix(ev.Ref, "refs/heads/")
	}

	// Head commit = the last entry in commits[], matching GitLab's
	// ordering convention. Missing commits array is fine for
	// delete events.
	if len(raw.Commits) > 0 {
		tip := raw.Commits[len(raw.Commits)-1]
		ts, _ := time.Parse(time.RFC3339, tip.Timestamp)
		author := tip.Author.Name
		if author == "" {
			author = tip.Author.Email
		}
		ev.HeadCommit = &Commit{
			ID:        tip.ID,
			Message:   tip.Message,
			Author:    author,
			Timestamp: ts,
		}
	}
	return ev, nil
}
