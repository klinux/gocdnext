// Package bitbucket verifies Bitbucket Cloud push webhooks and
// parses them into a PushEvent mirroring github's shape.
//
// Signature scheme: HMAC-SHA256 of the raw body, hex-encoded,
// delivered in X-Hub-Signature header (Bitbucket copied GitHub's
// older header name; no "256" suffix here). Shared secret is
// configured per-webhook in the repo settings.
package bitbucket

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidSignature signals HMAC mismatch. Wrapped as 401 by
// the caller.
var ErrInvalidSignature = errors.New("bitbucket: invalid signature")

// VerifySignature computes HMAC-SHA256 over body with the shared
// secret and constant-time compares against the hex-encoded
// header value. Accepts headers with or without a "sha256="
// prefix since Bitbucket has been inconsistent historically.
func VerifySignature(secret string, body []byte, header string) error {
	if secret == "" {
		return fmt.Errorf("bitbucket: secret is empty")
	}
	if header == "" {
		return fmt.Errorf("bitbucket: signature header missing")
	}
	expected := strings.TrimPrefix(header, "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	got := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(got)) != 1 {
		return ErrInvalidSignature
	}
	return nil
}

// PushEvent is the common-shape output, matching github.PushEvent.
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

// ParsePushEvent decodes the Bitbucket Cloud repo:push payload.
// The shape is push.changes[] — one entry per branch/tag affected
// in the push. We pick the first branch change and surface its
// new.target as the head. Multi-branch pushes are rare enough
// that handling only the first is a reasonable MVP; webhook
// deduplicates on (material_id, revision) anyway.
func ParsePushEvent(body []byte) (*PushEvent, error) {
	var raw struct {
		Repository struct {
			Links struct {
				HTML struct {
					Href string `json:"href"`
				} `json:"html"`
				Clone []struct {
					Href string `json:"href"`
					Name string `json:"name"`
				} `json:"clone"`
			} `json:"links"`
			FullName string `json:"full_name"`
		} `json:"repository"`
		Push struct {
			Changes []struct {
				New *struct {
					Type string `json:"type"` // "branch" | "tag"
					Name string `json:"name"`
					Target struct {
						Hash    string `json:"hash"`
						Message string `json:"message"`
						Date    string `json:"date"`
						Author  struct {
							Raw  string `json:"raw"`
							User struct {
								DisplayName string `json:"display_name"`
							} `json:"user"`
						} `json:"author"`
					} `json:"target"`
				} `json:"new"`
				Old *struct {
					Target struct {
						Hash string `json:"hash"`
					} `json:"target"`
				} `json:"old"`
				Closed bool `json:"closed"`
			} `json:"changes"`
		} `json:"push"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("bitbucket: decode push: %w", err)
	}

	// Prefer the HTTPS clone URL if enumerated, else fall back to
	// html.href (bitbucket.org/ws/repo), which still matches what
	// the store normalises for lookups.
	cloneURL := raw.Repository.Links.HTML.Href
	for _, c := range raw.Repository.Links.Clone {
		if strings.EqualFold(c.Name, "https") {
			cloneURL = c.Href
			break
		}
	}
	if cloneURL == "" {
		return nil, fmt.Errorf("bitbucket: push event missing repository links")
	}

	if len(raw.Push.Changes) == 0 {
		return nil, fmt.Errorf("bitbucket: push event has no changes")
	}
	change := raw.Push.Changes[0]

	ev := &PushEvent{
		Repository: RepositoryRef{CloneURL: cloneURL},
	}
	if change.Old != nil {
		ev.Before = change.Old.Target.Hash
	}
	if change.Closed || change.New == nil {
		ev.Deleted = true
		return ev, nil
	}
	if change.New.Type != "branch" {
		return nil, fmt.Errorf("bitbucket: only branch pushes supported, got %q", change.New.Type)
	}
	ev.Branch = change.New.Name
	ev.Ref = "refs/heads/" + change.New.Name
	ev.After = change.New.Target.Hash

	if change.New.Target.Hash != "" {
		ts, _ := time.Parse(time.RFC3339, change.New.Target.Date)
		author := change.New.Target.Author.User.DisplayName
		if author == "" {
			author = change.New.Target.Author.Raw
		}
		ev.HeadCommit = &Commit{
			ID:        change.New.Target.Hash,
			Message:   change.New.Target.Message,
			Author:    author,
			Timestamp: ts,
		}
	}
	return ev, nil
}
