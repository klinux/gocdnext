package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PullRequestAction values we care about. GitHub emits many more
// (labeled/assigned/review_requested/...) but none of them change the
// head SHA; ignoring them keeps scheduler noise down.
const (
	PRActionOpened      = "opened"
	PRActionSynchronize = "synchronize"
	PRActionReopened    = "reopened"
	PRActionClosed      = "closed"
)

// PullRequestEvent is a minimal projection of GitHub's pull_request
// webhook payload. We keep only what we need to persist a modification
// + trigger a run + annotate the run's cause_detail so the UI + future
// Checks API integration know which PR a run is for.
type PullRequestEvent struct {
	Action     string
	Number     int
	HTMLURL    string
	Title      string
	Author     string
	HeadSHA    string
	HeadRef    string
	BaseRef    string
	Merged     bool
	Repository Repository
	At         time.Time // PR.updated_at, best proxy for "when this action happened"
}

type prPayload struct {
	Action      string     `json:"action"`
	Number      int        `json:"number"`
	PullRequest *prDetails `json:"pull_request"`
	Repository  *Repository `json:"repository"`
}

type prDetails struct {
	HTMLURL   string    `json:"html_url"`
	Title     string    `json:"title"`
	Merged    bool      `json:"merged"`
	UpdatedAt time.Time `json:"updated_at"`
	User      *prUser   `json:"user"`
	Head      *prRef    `json:"head"`
	Base      *prRef    `json:"base"`
}

type prUser struct {
	Login string `json:"login"`
}

type prRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// ParsePullRequestEvent decodes a pull_request payload. Returns a
// typed error for anything that would have been caught by GitHub's
// own validator; we're intentionally strict so a subtly-wrong payload
// doesn't silently skip a run. Caller still decides whether to ACT on
// the event (action-filter is upstream).
func ParsePullRequestEvent(body []byte) (PullRequestEvent, error) {
	if len(body) == 0 {
		return PullRequestEvent{}, ErrEmptyPayload
	}
	var p prPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return PullRequestEvent{}, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if p.Action == "" {
		return PullRequestEvent{}, errors.New("github webhook: missing pull_request action")
	}
	if p.PullRequest == nil || p.PullRequest.Head == nil || p.PullRequest.Base == nil {
		return PullRequestEvent{}, errors.New("github webhook: missing pull_request fields")
	}
	if p.Repository == nil || p.Repository.CloneURL == "" {
		return PullRequestEvent{}, ErrMissingRepository
	}

	ev := PullRequestEvent{
		Action:     p.Action,
		Number:     p.Number,
		HTMLURL:    p.PullRequest.HTMLURL,
		Title:      p.PullRequest.Title,
		Merged:     p.PullRequest.Merged,
		HeadSHA:    p.PullRequest.Head.SHA,
		HeadRef:    p.PullRequest.Head.Ref,
		BaseRef:    p.PullRequest.Base.Ref,
		Repository: *p.Repository,
		At:         p.PullRequest.UpdatedAt,
	}
	if p.PullRequest.User != nil {
		ev.Author = p.PullRequest.User.Login
	}
	return ev, nil
}

// IsTriggerableAction returns true for the PR actions that SHOULD
// kick off a build (opened + synchronize + reopened). Closed/merged
// don't — the merge emits a push to the base branch that handles
// itself via the push event path.
func (ev PullRequestEvent) IsTriggerableAction() bool {
	switch ev.Action {
	case PRActionOpened, PRActionSynchronize, PRActionReopened:
		return true
	}
	return false
}
