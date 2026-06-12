package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	refPrefixBranch = "refs/heads/"
	refPrefixTag    = "refs/tags/"
	zeroSHA         = "0000000000000000000000000000000000000000"
)

// Parse-related errors.
var (
	ErrEmptyPayload      = errors.New("github webhook: empty payload")
	ErrInvalidJSON       = errors.New("github webhook: invalid JSON")
	ErrMissingRef        = errors.New("github webhook: missing ref")
	ErrMissingRepository = errors.New("github webhook: missing repository")
	ErrUnsupportedRef    = errors.New("github webhook: unsupported ref type")
)

// PushEvent is a minimal projection of GitHub's push event. Only the fields
// we need to persist a modification and match it against a material are kept.
type PushEvent struct {
	Ref     string // "refs/heads/main" or "refs/tags/v1"
	Branch  string // "main" when ref is a branch; empty for tags
	Tag     string // "v1"   when ref is a tag;    empty for branches
	IsTag   bool
	Before  string
	After   string
	Deleted bool

	Repository Repository
	HeadCommit *Commit
	Commits    []Commit
	// Size is the payload's `size` field — the number of commits in
	// the push. GitHub caps the embedded `commits` array (20), so
	// Size > len(Commits) flags a truncated file list.
	Size int
}

// ChangedFiles unions added/modified/removed across the payload's
// commits. `known` is false when the set can't be trusted as
// complete: no commits embedded (force-push payloads, some mirror
// pushes) or the array was truncated against `size`. Callers MUST
// fail open on !known — path filtering with a partial set silently
// drops legitimate runs.
func (ev PushEvent) ChangedFiles() (files []string, known bool) {
	if len(ev.Commits) == 0 || ev.Size > len(ev.Commits) {
		return nil, false
	}
	seen := make(map[string]struct{})
	for _, c := range ev.Commits {
		for _, lists := range [][]string{c.Added, c.Modified, c.Removed} {
			for _, f := range lists {
				if _, dup := seen[f]; !dup {
					seen[f] = struct{}{}
					files = append(files, f)
				}
			}
		}
	}
	return files, true
}

// Repository is the subset of repo metadata used by the receiver.
type Repository struct {
	FullName      string `json:"full_name"`
	HTMLURL       string `json:"html_url"`
	CloneURL      string `json:"clone_url"`
	SSHURL        string `json:"ssh_url"`
	DefaultBranch string `json:"default_branch"`
}

// Commit is the subset of commit metadata used by the receiver.
type Commit struct {
	ID        string    `json:"id"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
	Author    Author    `json:"author"`
	// Per-commit changed files — drive `when.paths` filtering.
	Added    []string `json:"added"`
	Modified []string `json:"modified"`
	Removed  []string `json:"removed"`
}

// Author captures the name/email fields present in GitHub push payloads.
type Author struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// ParsePushEvent decodes a GitHub push webhook payload. Returns a typed error
// (check with errors.Is) for invalid/unsupported inputs so the HTTP handler
// can map them to the right status code.
func ParsePushEvent(body []byte) (PushEvent, error) {
	if len(body) == 0 {
		return PushEvent{}, ErrEmptyPayload
	}

	var raw struct {
		Ref        string     `json:"ref"`
		Before     string     `json:"before"`
		After      string     `json:"after"`
		Deleted    bool       `json:"deleted"`
		Repository Repository `json:"repository"`
		HeadCommit *Commit    `json:"head_commit"`
		Commits    []Commit   `json:"commits"`
		Size       int        `json:"size"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return PushEvent{}, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}

	if raw.Ref == "" {
		return PushEvent{}, ErrMissingRef
	}
	if raw.Repository.FullName == "" || raw.Repository.CloneURL == "" {
		return PushEvent{}, ErrMissingRepository
	}

	ev := PushEvent{
		Ref:        raw.Ref,
		Before:     raw.Before,
		After:      raw.After,
		Deleted:    raw.Deleted || raw.After == zeroSHA,
		Repository: raw.Repository,
		HeadCommit: raw.HeadCommit,
		Commits:    raw.Commits,
		Size:       raw.Size,
	}

	switch {
	case strings.HasPrefix(raw.Ref, refPrefixBranch):
		ev.Branch = strings.TrimPrefix(raw.Ref, refPrefixBranch)
	case strings.HasPrefix(raw.Ref, refPrefixTag):
		ev.IsTag = true
		ev.Tag = strings.TrimPrefix(raw.Ref, refPrefixTag)
	default:
		return PushEvent{}, fmt.Errorf("%w: %s", ErrUnsupportedRef, raw.Ref)
	}

	return ev, nil
}
