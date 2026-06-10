// Package bitbucket verifies Bitbucket Cloud webhooks (push +
// pull request) and parses them into PushEvent / PullRequestEvent
// shapes mirroring the github/gitlab equivalents so downstream
// dispatch stays provider-uniform.
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
					Type   string `json:"type"` // "branch" | "tag"
					Name   string `json:"name"`
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

// Bitbucket Cloud Pull Request webhook (issue #12) ===============

// PR action values we care about. Bitbucket Cloud emits the verbs
// as the X-Event-Key tail ("pullrequest:created" etc.); the
// parser surfaces them stripped to the verb. The triggerable
// subset mirrors github.PullRequestEvent / gitlab.MergeRequestEvent
// — created/updated only. Fulfilled (merged) / rejected (declined)
// don't trigger, mirroring the close/merge contract on the other
// providers: the merge emits a push to the destination branch that
// handles itself.
const (
	PRActionCreated   = "created"
	PRActionUpdated   = "updated"
	PRActionFulfilled = "fulfilled" // merged
	PRActionRejected  = "rejected"  // declined / closed
)

// PullRequestEvent is the provider-uniform projection of
// Bitbucket Cloud's pullrequest:* payloads. Field names match
// github.PullRequestEvent + gitlab.MergeRequestEvent so the
// dispatch path doesn't branch on provider — same cause_detail
// shape, same CI_PULL_REQUEST_* env vars.
//
// Bitbucket Cloud has no native PR label primitive (only
// reviewers), so Labels is always nil here — operators using
// quorum_by_label on a Bitbucket repo will never satisfy the
// override path, which is the correct behaviour (no labels
// declared ⇒ default quorum applies).
type PullRequestEvent struct {
	Action     string // created / updated / fulfilled / rejected
	Number     int    // pullrequest.id
	HTMLURL    string // pullrequest.links.html.href
	Title      string // pullrequest.title
	Author     string // pullrequest.author.nickname (falls back to display_name)
	HeadSHA    string // pullrequest.source.commit.hash
	HeadRef    string // pullrequest.source.branch.name
	BaseRef    string // pullrequest.destination.branch.name
	Merged     bool   // state == "MERGED"
	Repository RepositoryRef
	At         time.Time // pullrequest.updated_on

	// RepoSlug is destination.repository.full_name ("ws/repo"),
	// preferred as the diagnostic log label over the raw clone
	// URL. Bitbucket Cloud clone URLs include the workspace name
	// only — no credentials in the public payload shape — but
	// the slug is shorter and stable.
	RepoSlug string

	// Labels is always nil on Bitbucket Cloud (no native PR label
	// primitive). Kept as a field so the projection matches
	// github/gitlab and downstream cause_detail JSON serialisation
	// is uniform.
	Labels []string
}

// prPayload mirrors Bitbucket Cloud's pullrequest webhook
// shape. Only the fields we surface are decoded. The signed
// `repository` block at the top level is used for clone URL
// fallback; the `pullrequest.destination.repository` block
// carries the slug used for log labels.
type prPayload struct {
	PullRequest *prObject  `json:"pullrequest"`
	Repository  *prRepoTop `json:"repository"`
}

type prObject struct {
	ID          int             `json:"id"`
	Title       string          `json:"title"`
	State       string          `json:"state"`
	Source      *prEndpoint     `json:"source"`
	Destination *prEndpoint     `json:"destination"`
	Links       *prLinks        `json:"links"`
	Author      *prAuthor       `json:"author"`
	UpdatedOn   string          `json:"updated_on"`
	Reviewers   []prAuthor      `json:"reviewers"` // reserved, currently unused
	Description string          `json:"description"`
	Type        string          `json:"type"`
	Reason      string          `json:"reason"`
	Rendered    json.RawMessage `json:"rendered"`
}

type prEndpoint struct {
	Branch     *prBranch `json:"branch"`
	Commit     *prCommit `json:"commit"`
	Repository *prRepo   `json:"repository"`
}

type prBranch struct {
	Name string `json:"name"`
}

type prCommit struct {
	Hash string `json:"hash"`
}

type prRepo struct {
	FullName string  `json:"full_name"`
	Links    prLinks `json:"links"`
}

type prLinks struct {
	HTML  prHref   `json:"html"`
	Clone []prHref `json:"clone"`
}

type prHref struct {
	Href string `json:"href"`
	Name string `json:"name,omitempty"`
}

type prAuthor struct {
	Nickname    string `json:"nickname"`
	DisplayName string `json:"display_name"`
	UUID        string `json:"uuid"`
}

// prRepoTop is the top-level `repository` envelope Bitbucket
// includes on every webhook delivery. We fall back to its
// clone links if the PR object's destination.repository is
// empty (rare but documented in some self-hosted Bitbucket
// Server fixtures).
type prRepoTop struct {
	FullName string  `json:"full_name"`
	Links    prLinks `json:"links"`
}

// EventKeyToAction strips the "pullrequest:" prefix from
// Bitbucket's X-Event-Key. Returns "" for non-PR events so the
// router can fall through to its existing push path.
func EventKeyToAction(eventKey string) string {
	const prefix = "pullrequest:"
	if !strings.HasPrefix(eventKey, prefix) {
		return ""
	}
	return strings.TrimPrefix(eventKey, prefix)
}

// IsPullRequestEvent reports whether the X-Event-Key header
// belongs to the pullrequest:* family. Used by the multi
// router BEFORE parsing.
func IsPullRequestEvent(eventKey string) bool {
	return strings.HasPrefix(eventKey, "pullrequest:")
}

// ParsePullRequestEvent decodes a Bitbucket Cloud pullrequest:*
// webhook payload. The action verb is passed in by the caller
// (already stripped from X-Event-Key) since Bitbucket doesn't
// re-state it inside the body. Returns a typed error for every
// shape that would have been caught by Bitbucket's own webhook
// validator; the caller decides whether to ACT on the event via
// IsTriggerableAction.
func ParsePullRequestEvent(body []byte, action string) (PullRequestEvent, error) {
	if len(body) == 0 {
		return PullRequestEvent{}, fmt.Errorf("bitbucket: empty pullrequest payload")
	}
	if action == "" {
		return PullRequestEvent{}, fmt.Errorf("bitbucket: pullrequest event missing action verb")
	}
	var p prPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return PullRequestEvent{}, fmt.Errorf("bitbucket: decode pullrequest: %w", err)
	}
	if p.PullRequest == nil {
		return PullRequestEvent{}, fmt.Errorf("bitbucket: pullrequest payload missing `pullrequest` block")
	}
	if p.PullRequest.Source == nil || p.PullRequest.Source.Branch == nil || p.PullRequest.Source.Branch.Name == "" {
		return PullRequestEvent{}, fmt.Errorf("bitbucket: pullrequest missing source.branch.name")
	}
	if p.PullRequest.Source.Commit == nil || p.PullRequest.Source.Commit.Hash == "" {
		return PullRequestEvent{}, fmt.Errorf("bitbucket: pullrequest missing source.commit.hash")
	}
	if p.PullRequest.Destination == nil || p.PullRequest.Destination.Branch == nil || p.PullRequest.Destination.Branch.Name == "" {
		return PullRequestEvent{}, fmt.Errorf("bitbucket: pullrequest missing destination.branch.name")
	}

	// Clone URL: prefer destination.repository's https clone link
	// (this is the repo we'd push the merge to), fall back to the
	// top-level repository envelope. Same selection rule as the
	// push path so the (url, base_ref) fingerprint hits the same
	// material row regardless of which webhook flavour arrived.
	cloneURL := pickHTTPSClone(p.PullRequest.Destination.Repository)
	if cloneURL == "" {
		cloneURL = pickHTTPSCloneTop(p.Repository)
	}
	if cloneURL == "" {
		return PullRequestEvent{}, fmt.Errorf("bitbucket: pullrequest missing destination clone url")
	}

	repoSlug := ""
	if p.PullRequest.Destination.Repository != nil {
		repoSlug = p.PullRequest.Destination.Repository.FullName
	}
	if repoSlug == "" && p.Repository != nil {
		repoSlug = p.Repository.FullName
	}

	ev := PullRequestEvent{
		Action:     action,
		Number:     p.PullRequest.ID,
		Title:      p.PullRequest.Title,
		HeadSHA:    p.PullRequest.Source.Commit.Hash,
		HeadRef:    p.PullRequest.Source.Branch.Name,
		BaseRef:    p.PullRequest.Destination.Branch.Name,
		Merged:     strings.EqualFold(p.PullRequest.State, "MERGED"),
		Repository: RepositoryRef{CloneURL: cloneURL},
		RepoSlug:   repoSlug,
	}
	if p.PullRequest.Links != nil {
		ev.HTMLURL = p.PullRequest.Links.HTML.Href
	}
	if p.PullRequest.Author != nil {
		ev.Author = p.PullRequest.Author.Nickname
		if ev.Author == "" {
			ev.Author = p.PullRequest.Author.DisplayName
		}
	}
	// Bitbucket Cloud sends updated_on with microsecond precision
	// and an explicit timezone offset, e.g.
	// "2026-06-10T12:00:00.123456+00:00". time.RFC3339Nano handles
	// up to 9 fractional digits; fall back to plain RFC3339 if
	// the precision varies.
	if ts := p.PullRequest.UpdatedOn; ts != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			ev.At = parsed
		} else if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			ev.At = parsed
		}
	}
	return ev, nil
}

// pickHTTPSClone walks a prRepo.Links.Clone list for the HTTPS
// entry; falls back to the html href since that's also a valid
// clonable URL on Bitbucket Cloud.
func pickHTTPSClone(r *prRepo) string {
	if r == nil {
		return ""
	}
	for _, c := range r.Links.Clone {
		if strings.EqualFold(c.Name, "https") && c.Href != "" {
			return c.Href
		}
	}
	return r.Links.HTML.Href
}

// pickHTTPSCloneTop mirrors pickHTTPSClone for the top-level
// repository envelope.
func pickHTTPSCloneTop(r *prRepoTop) string {
	if r == nil {
		return ""
	}
	for _, c := range r.Links.Clone {
		if strings.EqualFold(c.Name, "https") && c.Href != "" {
			return c.Href
		}
	}
	return r.Links.HTML.Href
}

// IsTriggerableAction returns true for PR actions that SHOULD
// fan out (created + updated). Fulfilled (merged) and rejected
// (declined) don't — same contract as github close/merge and
// gitlab close/merge: the destination-branch push handles the
// merged case.
//
// Defence-in-depth: state == "MERGED" short-circuits even when
// action is a normally-triggerable verb. Bitbucket has been
// observed sending a trailing `updated` after the merge in some
// race conditions.
func (ev PullRequestEvent) IsTriggerableAction() bool {
	if ev.Merged {
		return false
	}
	switch ev.Action {
	case PRActionCreated, PRActionUpdated:
		return true
	}
	return false
}
