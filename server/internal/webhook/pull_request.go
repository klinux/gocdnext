package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
	bitbucketpkg "github.com/gocdnext/gocdnext/server/internal/webhook/bitbucket"
	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
	gitlabpkg "github.com/gocdnext/gocdnext/server/internal/webhook/gitlab"
)

// pullRequestEvent is the provider-uniform projection both
// GitHub's pull_request and GitLab's merge_request paths fold
// into BEFORE the fan-out. Keeps dispatchPullRequest agnostic of
// which adapter parsed the body — the only thing that varies is
// the field labels in errors/logs, threaded through via
// pullRequestEvent.provider.
//
// All fields mirror github.PullRequestEvent + the GitLab
// equivalent so cause_detail and CI_PULL_REQUEST_* env vars
// stay identical across providers.
type pullRequestEvent struct {
	Provider    string // "github" | "gitlab"
	Action      string // provider-specific action (opened/synchronize/open/update/...)
	Number      int
	Title       string
	Author      string
	HTMLURL     string
	HeadSHA     string
	HeadRef     string
	BaseRef     string
	CloneURL    string
	RepoLabel   string // for log diagnostics — github.FullName OR gitlab project path
	At          time.Time
	Labels      []string
}

// handlePullRequest reacts to GitHub's pull_request webhook. Matching
// rule: the material that would fire on a push to the PR's BASE ref
// also fires on the PR (when its events list includes "pull_request"
// in the YAML). Rationale — pipelines are typically defined as
// `branch: main`, and people intend "lint these PRs that target main"
// without having to mirror the material to every feature branch.
//
// Run metadata:
//
//	revision    = PR head SHA
//	branch      = PR head ref (so git checkout of the run pulls the
//	              PR code, not main)
//	cause       = "pull_request"
//	cause_detail = { pr_number, pr_title, pr_author, pr_url,
//	                 pr_head_ref, pr_head_sha, pr_base_ref,
//	                 pr_action }
//
// Only opened / synchronize / reopened trigger runs. Close/merge are
// ack'd with 204 — the subsequent push to base handles itself.
func (h *Handler) handlePullRequest(w http.ResponseWriter, r *http.Request, body []byte, delivery string, rec *deliveryRec) {
	ev, err := github.ParsePullRequestEvent(body)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "parse pull_request: " + err.Error()
		h.log.Warn("github webhook: PR parse failed", "delivery", delivery, "err", err)
		http.Error(w, "invalid pull_request payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !ev.IsTriggerableAction() {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github webhook: PR action ignored",
			"delivery", delivery, "action", ev.Action, "number", ev.Number)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.dispatchPullRequest(w, r, body, delivery, rec, pullRequestEvent{
		Provider:  "github",
		Action:    ev.Action,
		Number:    ev.Number,
		Title:     ev.Title,
		Author:    ev.Author,
		HTMLURL:   ev.HTMLURL,
		HeadSHA:   ev.HeadSHA,
		HeadRef:   ev.HeadRef,
		BaseRef:   ev.BaseRef,
		CloneURL:  ev.Repository.CloneURL,
		RepoLabel: ev.Repository.FullName,
		At:        ev.At,
		Labels:    ev.Labels,
	})
}

// handleGitLabMergeRequest is the GitLab-side entry point. GitLab
// emits "Merge Request Hook" events whose payload looks different
// from GitHub's pull_request but carries the same semantic
// information; ParseMergeRequestEvent normalises the shape, then
// we fold into the same dispatchPullRequest path so cause_detail,
// CI_PULL_REQUEST_* env vars, and label-driven approval quorum
// are provider-uniform downstream.
//
// Material matching uses `events: [pull_request]` (NOT a separate
// "merge_request" entry) so operators don't have to declare both
// for projects that mirror to GitHub + GitLab. The webhook handler
// is the provider boundary; everything past it is generic.
func (h *Handler) handleGitLabMergeRequest(w http.ResponseWriter, r *http.Request, body []byte, delivery string, rec *deliveryRec) {
	ev, err := gitlabpkg.ParseMergeRequestEvent(body)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "parse merge_request: " + err.Error()
		h.log.Warn("gitlab webhook: MR parse failed", "delivery", delivery, "err", err)
		http.Error(w, "invalid merge_request payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !ev.IsTriggerableAction() {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("gitlab webhook: MR action ignored",
			"delivery", delivery, "action", ev.Action, "number", ev.Number)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// RepoLabel prefers project.path_with_namespace ("group/demo")
	// over the raw clone URL — self-hosted GitLab installs CAN
	// emit URLs with embedded credentials in unusual setups.
	repoLabel := ev.ProjectPath
	if repoLabel == "" {
		repoLabel = ev.Repository.CloneURL
	}
	h.dispatchPullRequest(w, r, body, delivery, rec, pullRequestEvent{
		Provider:  "gitlab",
		Action:    ev.Action,
		Number:    ev.Number,
		Title:     ev.Title,
		Author:    ev.Author,
		HTMLURL:   ev.HTMLURL,
		HeadSHA:   ev.HeadSHA,
		HeadRef:   ev.HeadRef,
		BaseRef:   ev.BaseRef,
		CloneURL:  ev.Repository.CloneURL,
		RepoLabel: repoLabel,
		At:        ev.At,
		Labels:    ev.Labels,
	})
}

// handleBitbucketPullRequest is the Bitbucket Cloud entry point
// (issue #12). Bitbucket emits the action verb out-of-band on
// X-Event-Key (the body doesn't restate it), so the caller
// passes it in. ParsePullRequestEvent normalises the rest of the
// shape, then we fold into the same dispatchPullRequest path so
// cause_detail, CI_PULL_REQUEST_* env vars, and label-driven
// approval quorum stay provider-uniform downstream.
//
// Bitbucket Cloud has no native PR label primitive — Labels is
// nil for every event from this provider, which means
// quorum_by_label never satisfies an override on Bitbucket
// (correct: no labels declared ⇒ default quorum applies).
func (h *Handler) handleBitbucketPullRequest(w http.ResponseWriter, r *http.Request, body []byte, delivery, action string, rec *deliveryRec) {
	ev, err := bitbucketpkg.ParsePullRequestEvent(body, action)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "parse pullrequest: " + err.Error()
		h.log.Warn("bitbucket webhook: PR parse failed", "delivery", delivery, "err", err)
		http.Error(w, "invalid pullrequest payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !ev.IsTriggerableAction() {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("bitbucket webhook: PR action ignored",
			"delivery", delivery, "action", ev.Action, "number", ev.Number)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// RepoLabel prefers destination.repository.full_name
	// ("ws/repo") over the clone URL — same defensive rationale
	// as the gitlab side (URL CAN carry credentials in unusual
	// self-hosted setups).
	repoLabel := ev.RepoSlug
	if repoLabel == "" {
		repoLabel = ev.Repository.CloneURL
	}
	h.dispatchPullRequest(w, r, body, delivery, rec, pullRequestEvent{
		Provider:  "bitbucket",
		Action:    ev.Action,
		Number:    ev.Number,
		Title:     ev.Title,
		Author:    ev.Author,
		HTMLURL:   ev.HTMLURL,
		HeadSHA:   ev.HeadSHA,
		HeadRef:   ev.HeadRef,
		BaseRef:   ev.BaseRef,
		CloneURL:  ev.Repository.CloneURL,
		RepoLabel: repoLabel,
		At:        ev.At,
		Labels:    ev.Labels,
	})
}

// dispatchPullRequest is the provider-agnostic fan-out path used
// by handlePullRequest (GitHub), handleGitLabMergeRequest
// (GitLab), and handleBitbucketPullRequest (Bitbucket). The
// pullRequestEvent argument is already provider-normalised —
// the only branching left is the `provider` field on the fan-out
// input (so persisted modifications + delivery records remember
// which adapter parsed the body).
//
// Material matching rule: the material that would fire on a push
// to the PR/MR's BASE ref also fires on the PR/MR when its events
// list includes "pull_request" in the YAML — operators don't
// have to declare both `pull_request` and a hypothetical
// `merge_request` event for projects that mirror to multiple
// providers. The fingerprint normalisation makes (url, base_ref)
// lookups hit the same row regardless of provider.
func (h *Handler) dispatchPullRequest(w http.ResponseWriter, r *http.Request, body []byte, delivery string, rec *deliveryRec, ev pullRequestEvent) {
	logKind := "PR"
	if ev.Provider == "gitlab" {
		logKind = "MR"
	}

	fp := store.FingerprintFor(ev.CloneURL, ev.BaseRef)
	allMaterials, err := h.store.FindMaterialsByFingerprint(r.Context(), fp)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "material lookup (" + logKind + "): " + err.Error()
		h.log.Error(ev.Provider+" webhook: material lookup failed ("+logKind+")",
			"delivery", delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(allMaterials) == 0 {
		rec.status = store.WebhookStatusIgnored
		h.log.Info(ev.Provider+" webhook: no material for "+logKind+" base",
			"delivery", delivery, "repo", ev.RepoLabel,
			"base_ref", ev.BaseRef, "fingerprint", fp)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Per-material event filter: only materials that declare
	// `pull_request` in their events list are eligible (GitLab MRs
	// reuse the same event name — webhook is the provider
	// boundary). A pipeline that hasn't opted in (push-only) is
	// silently skipped; other pipelines still fire.
	materials := make([]store.Material, 0, len(allMaterials))
	for _, m := range allMaterials {
		var cfg domain.GitMaterial
		if err := json.Unmarshal(m.Config, &cfg); err != nil {
			h.log.Warn(ev.Provider+" webhook: decode material config ("+logKind+")",
				"delivery", delivery, "material_id", m.ID, "err", err)
			continue
		}
		if !slices.Contains(cfg.Events, "pull_request") {
			continue
		}
		materials = append(materials, m)
	}
	if len(materials) == 0 {
		rec.status = store.WebhookStatusIgnored
		h.log.Info(ev.Provider+" webhook: no "+logKind+"-listening material for this base ref",
			"delivery", delivery, "material_candidates", len(allMaterials))
		w.WriteHeader(http.StatusNoContent)
		return
	}

	detail := map[string]any{
		"pr_number":   ev.Number,
		"pr_title":    ev.Title,
		"pr_author":   ev.Author,
		"pr_url":      ev.HTMLURL,
		"pr_head_ref": ev.HeadRef,
		"pr_head_sha": ev.HeadSHA,
		"pr_base_ref": ev.BaseRef,
		"pr_action":   ev.Action,
	}
	// pr_labels: only stamped when the PR/MR actually has labels.
	// Same shape across providers — both adapters lowercase + dedupe
	// before this point.
	if len(ev.Labels) > 0 {
		detail["pr_labels"] = ev.Labels
	}
	causeDetail, _ := json.Marshal(detail)
	outcomes := fanOutMaterials(r.Context(), h.log, h.store, fanOutInput{
		Materials:   materials,
		Revision:    ev.HeadSHA,
		Branch:      ev.HeadRef,
		Author:      ev.Author,
		Message:     ev.Title,
		Payload:     json.RawMessage(body),
		CommittedAt: ev.At,
		Provider:    ev.Provider,
		Delivery:    delivery,
		TriggeredBy: "system:webhook",
		Cause:       string(domain.CausePullRequest),
		CauseDetail: causeDetail,
	})
	rec.materialID = firstCreatedRunMaterialID(outcomes)

	runs := runsPayload(outcomes)
	allErrored := len(outcomes) > 0
	for _, oc := range outcomes {
		if oc.Err == nil {
			allErrored = false
			if oc.RunID != uuid.Nil {
				h.log.Info(ev.Provider+" webhook: "+logKind+" run queued",
					"delivery", delivery, "pipeline_id", oc.PipelineID,
					"run_id", oc.RunID, "counter", oc.RunCounter,
					"pr_number", ev.Number, "head_sha", ev.HeadSHA, "head_ref", ev.HeadRef)
				h.reporter.ReportRunCreated(r.Context(), oc.RunID)
			}
		}
	}
	if allErrored {
		rec.status = store.WebhookStatusError
		rec.errText = logKind + " fan-out: every pipeline errored"
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rec.status = store.WebhookStatusAccepted
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	resp := map[string]any{
		"runs":      runs,
		"materials": len(materials),
		"pr_number": ev.Number,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.log.Warn(ev.Provider+" webhook: encode response failed", "err", fmt.Sprint(err))
	}
}
