// Package gitlab verifies GitLab push and merge-request webhooks
// and turns them into the same PushEvent / MergeRequestEvent
// shapes the rest of the webhook pipeline consumes. Signature
// scheme is a shared-secret token compare (GitLab passes it raw
// in X-Gitlab-Token) — simpler than GitHub's HMAC, but still
// constant-time-compared so a cache timing attack can't leak the
// secret.
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

// GitLab Merge Request Hook (issue #11) ===========================

// MR action values we care about. GitLab emits many more
// (approved/unapproved/label-added/...) but none of them change
// the head SHA; ignoring them keeps scheduler noise down. The
// triggerable subset mirrors GitHub's opened/synchronize/
// reopened so handlePullRequest can route both providers through
// the same fan-out logic.
const (
	MRActionOpen   = "open"
	MRActionUpdate = "update"
	MRActionReopen = "reopen"
	MRActionClose  = "close"
	MRActionMerge  = "merge"
)

// MergeRequestEvent is the projection of GitLab's Merge Request
// Hook payload used by the webhook handler. Field names match
// github.PullRequestEvent so the downstream dispatch path
// doesn't branch on provider — same cause_detail shape, same
// CI_PULL_REQUEST_* env vars.
type MergeRequestEvent struct {
	Action     string
	Number     int       // GitLab's `iid` (project-scoped MR number, what shows in /merge_requests/<N>)
	HTMLURL    string    // object_attributes.url
	Title      string    // object_attributes.title
	Author     string    // user.username
	HeadSHA    string    // object_attributes.last_commit.id
	HeadRef    string    // object_attributes.source_branch
	BaseRef    string    // object_attributes.target_branch
	Merged     bool      // state == "merged" (only when action=merge)
	Repository RepositoryRef
	At         time.Time // object_attributes.updated_at, best proxy for "when this action happened"

	// ProjectPath is project.path_with_namespace ("group/demo"),
	// used as the diagnostic label in logs in preference to the
	// raw clone URL — the URL CAN carry credentials in unusual
	// self-hosted setups, the path never does.
	ProjectPath string

	// Labels are MR labels, lowercased + deduped, nil when empty.
	// Same shape + normalisation rule as the GitHub side so the
	// PR-label-driven approval quorum (quorum_by_label) treats
	// both providers uniformly.
	Labels []string
}

// mrPayload mirrors GitLab's Merge Request Hook JSON structure.
// Only the fields we surface are decoded. project.git_http_url
// is the canonical clone URL — the same one push hooks use, so
// material matching by fingerprint hits the same row.
type mrPayload struct {
	ObjectKind       string `json:"object_kind"`
	User             *mrUser
	Project          *mrProject
	ObjectAttributes *mrAttrs   `json:"object_attributes"`
	Labels           []mrLabel  `json:"labels"`
	Repository       *mrRepoTop `json:"repository"`
}

type mrUser struct {
	Username string `json:"username"`
	Name     string `json:"name"`
}

type mrProject struct {
	GitHTTPURL        string `json:"git_http_url"`
	WebURL            string `json:"web_url"`
	PathWithNamespace string `json:"path_with_namespace"`
}

// mrRepoTop is the top-level `repository` block on the MR payload —
// GitLab populates it on push hooks but sometimes leaves it empty
// on MR hooks; we fall back to project.git_http_url in that case.
type mrRepoTop struct {
	GitHTTPURL string `json:"git_http_url"`
	URL        string `json:"url"`
}

type mrAttrs struct {
	IID          int       `json:"iid"`
	Action       string    `json:"action"`
	Title        string    `json:"title"`
	State        string    `json:"state"`
	URL          string    `json:"url"`
	SourceBranch string    `json:"source_branch"`
	TargetBranch string    `json:"target_branch"`
	UpdatedAt    string    `json:"updated_at"` // GitLab uses a non-RFC3339 form; parsed below
	LastCommit   *mrCommit `json:"last_commit"`
}

type mrCommit struct {
	ID string `json:"id"`
}

type mrLabel struct {
	Title string `json:"title"`
}

// ParseMergeRequestEvent decodes a GitLab Merge Request Hook
// payload. Returns a typed error for anything that would have
// been caught by GitLab's own validator; the caller decides
// whether to ACT on the event via IsTriggerableAction.
func ParseMergeRequestEvent(body []byte) (MergeRequestEvent, error) {
	if len(body) == 0 {
		return MergeRequestEvent{}, fmt.Errorf("gitlab: empty merge_request payload")
	}
	var p mrPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return MergeRequestEvent{}, fmt.Errorf("gitlab: decode merge_request: %w", err)
	}
	if p.ObjectKind != "" && p.ObjectKind != "merge_request" {
		return MergeRequestEvent{}, fmt.Errorf("gitlab: expected object_kind=merge_request, got %q", p.ObjectKind)
	}
	if p.ObjectAttributes == nil {
		return MergeRequestEvent{}, fmt.Errorf("gitlab: merge_request missing object_attributes")
	}
	if p.ObjectAttributes.Action == "" {
		return MergeRequestEvent{}, fmt.Errorf("gitlab: merge_request missing object_attributes.action")
	}
	cloneURL := ""
	if p.Project != nil {
		cloneURL = p.Project.GitHTTPURL
	}
	if cloneURL == "" && p.Repository != nil {
		cloneURL = p.Repository.GitHTTPURL
		if cloneURL == "" {
			cloneURL = p.Repository.URL
		}
	}
	if cloneURL == "" {
		return MergeRequestEvent{}, fmt.Errorf("gitlab: merge_request missing project.git_http_url")
	}
	if p.ObjectAttributes.LastCommit == nil || p.ObjectAttributes.LastCommit.ID == "" {
		return MergeRequestEvent{}, fmt.Errorf("gitlab: merge_request missing object_attributes.last_commit.id")
	}
	if p.ObjectAttributes.SourceBranch == "" || p.ObjectAttributes.TargetBranch == "" {
		return MergeRequestEvent{}, fmt.Errorf("gitlab: merge_request missing source_branch or target_branch")
	}

	ev := MergeRequestEvent{
		Action:     p.ObjectAttributes.Action,
		Number:     p.ObjectAttributes.IID,
		HTMLURL:    p.ObjectAttributes.URL,
		Title:      p.ObjectAttributes.Title,
		HeadSHA:    p.ObjectAttributes.LastCommit.ID,
		HeadRef:    p.ObjectAttributes.SourceBranch,
		BaseRef:    p.ObjectAttributes.TargetBranch,
		Merged:     p.ObjectAttributes.State == "merged",
		Repository: RepositoryRef{CloneURL: cloneURL},
	}
	if p.User != nil {
		ev.Author = p.User.Username
		if ev.Author == "" {
			ev.Author = p.User.Name
		}
	}
	if p.Project != nil {
		ev.ProjectPath = p.Project.PathWithNamespace
	}
	// GitLab uses a space-separated UTC form like
	// "2024-01-02 03:04:05 UTC" on object_attributes.updated_at;
	// try the standard RFC3339 first (newer payload versions),
	// then fall back to the space form.
	if ts := p.ObjectAttributes.UpdatedAt; ts != "" {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			ev.At = parsed
		} else if parsed, err := time.Parse("2006-01-02 15:04:05 MST", ts); err == nil {
			ev.At = parsed
		} else if parsed, err := time.Parse("2006-01-02 15:04:05 -0700", ts); err == nil {
			ev.At = parsed
		}
	}
	ev.Labels = normaliseMRLabels(p.Labels)
	return ev, nil
}

// normaliseMRLabels lowercases each label title (GitLab treats
// labels case-insensitively), trims whitespace, drops empties,
// and dedupes preserving first-seen order. Returns nil (not an
// empty slice) when nothing survives so callers' `len(...) > 0`
// checks and JSON `omitempty` both behave naturally. Mirrors the
// GitHub-side normaliseLabels contract so cause_detail.pr_labels
// is provider-uniform.
func normaliseMRLabels(in []mrLabel) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, l := range in {
		name := strings.ToLower(strings.TrimSpace(l.Title))
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// IsTriggerableAction returns true for the MR actions that
// SHOULD kick off a build (open + update + reopen). Close /
// merge don't — merging into target emits a push to the base
// branch that handles itself via the push event path.
//
// Defence-in-depth: if state=merged but action != merge (some
// self-hosted GitLab versions emit a trailing `update` after
// the merge), we still bail out — the push to target_branch
// will do the work.
func (ev MergeRequestEvent) IsTriggerableAction() bool {
	if ev.Merged {
		return false
	}
	switch ev.Action {
	case MRActionOpen, MRActionUpdate, MRActionReopen:
		return true
	}
	return false
}
