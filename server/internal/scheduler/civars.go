package scheduler

import (
	"encoding/json"
	"sort"
	"strconv"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// shortSHALen mirrors the prefix length git's `--short` and most CI
// platforms display (GitHub Actions, GitLab, Drone). Long enough to
// stay collision-free on any realistic project, short enough to fit
// into image tags and slack messages.
const shortSHALen = 8

// buildCIVars assembles the read-only `CI_*` (and friends) namespace
// gocdnext exposes for substitution into plugin settings + job env.
// Same shape every popular CI emits (CI_BRANCH, CI_COMMIT_SHA,
// CI_COMMIT_SHORT_SHA, CI_RUN_COUNTER, …) so operators reuse plugin
// recipes from Drone / GitLab / Woodpecker without translating
// variable names.
//
// Returned map is fresh; caller may mutate freely. Empty / missing
// fields stay UNSET (rather than empty-string entries) so the
// substitution layer leaves `${CI_COMMIT_SHORT_SHA}` LITERAL when the
// run carries no revision (manual-trigger of a pipeline with no git
// materials) — the operator then catches the misconfiguration at
// dispatch time instead of building images tagged `myapp:1.2.`.
func buildCIVars(run store.RunForDispatch, jobName string) map[string]string {
	out := map[string]string{
		"CI":             "true",
		"GOCDNEXT":       "true",
		"CI_RUN_ID":      run.ID.String(),
		"CI_RUN_COUNTER": strconv.FormatInt(run.Counter, 10),
		"CI_PIPELINE_ID": run.PipelineID.String(),
		"CI_PROJECT_ID":  run.ProjectID.String(),
		"CI_JOB_NAME":    jobName,
	}
	commit, branch := primaryRevision(run.Revisions)
	if commit != "" {
		out["CI_COMMIT_SHA"] = commit
		out["CI_COMMIT_SHORT_SHA"] = shortSHA(commit)
	}
	if branch != "" {
		out["CI_BRANCH"] = branch
	}
	if run.Cause != "" {
		out["CI_CAUSE"] = run.Cause
	}
	addPullRequestVars(out, run.Cause, run.CauseDetail)
	return out
}

// pullRequestDetail mirrors the JSONB the webhook handler stamps on
// `runs.cause_detail` for a `pull_request` cause — see
// server/internal/webhook/pull_request.go. Only the fields the env
// surface uses are decoded; unknown fields are ignored so adding new
// keys to the webhook handler doesn't require touching the scheduler.
type pullRequestDetail struct {
	Number  int    `json:"pr_number"`
	Title   string `json:"pr_title"`
	Author  string `json:"pr_author"`
	URL     string `json:"pr_url"`
	HeadRef string `json:"pr_head_ref"`
	BaseRef string `json:"pr_base_ref"`
}

// addPullRequestVars materialises CI_PULL_REQUEST_* into out IF AND
// ONLY IF the run was triggered by a pull_request AND cause_detail
// decodes cleanly. Other causes (push, manual, upstream, schedule,
// poll) and malformed JSON silently skip — keeps the substitution
// layer happy with literal `${CI_PULL_REQUEST_KEY}` on non-PR runs
// rather than emitting empty strings that would bake `myapp:pr-` style
// tags. Fields that decode as zero (PR with no title, e.g.) likewise
// stay unset; same rationale as primaryRevision.
//
// The PR number is INT in JSON but rendered decimal to match how every
// other CI emits it (1234, not 1234.0). Zero-valued PR numbers are
// treated as missing — no legitimate PR has number 0.
func addPullRequestVars(out map[string]string, cause string, detail []byte) {
	if cause != "pull_request" || len(detail) == 0 {
		return
	}
	var pr pullRequestDetail
	if err := json.Unmarshal(detail, &pr); err != nil {
		return
	}
	if pr.Number > 0 {
		out["CI_PULL_REQUEST_KEY"] = strconv.Itoa(pr.Number)
	}
	if pr.HeadRef != "" {
		out["CI_PULL_REQUEST_BRANCH"] = pr.HeadRef
	}
	if pr.BaseRef != "" {
		out["CI_PULL_REQUEST_BASE"] = pr.BaseRef
	}
	if pr.Title != "" {
		out["CI_PULL_REQUEST_TITLE"] = pr.Title
	}
	if pr.Author != "" {
		out["CI_PULL_REQUEST_AUTHOR"] = pr.Author
	}
	if pr.URL != "" {
		out["CI_PULL_REQUEST_URL"] = pr.URL
	}
}

// primaryRevision picks one (revision, branch) pair from the
// revisions JSONB the run carries. Today's runs only bind one git
// material so the choice is moot — but we sort keys (material UUIDs)
// before iterating so a future multi-material run produces the same
// answer across replays / reaper requeues / late audit reads.
func primaryRevision(raw []byte) (commit, branch string) {
	if len(raw) == 0 {
		return "", ""
	}
	var parsed map[string]struct {
		Revision string `json:"revision"`
		Branch   string `json:"branch"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", ""
	}
	if len(parsed) == 0 {
		return "", ""
	}
	keys := make([]string, 0, len(parsed))
	for k := range parsed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	r := parsed[keys[0]]
	return r.Revision, r.Branch
}

// shortSHA truncates a commit SHA to the conventional display width.
// Shorter inputs (already-truncated, non-git revisions like a tag
// name) come back as-is so the var stays usable.
func shortSHA(sha string) string {
	if len(sha) <= shortSHALen {
		return sha
	}
	return sha[:shortSHALen]
}
