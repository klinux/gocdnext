package webhook

import (
	"context"
	"net/http"

	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// repoKey is the persisted repo identity: the canonical form of the SAME
// clone_url the HMAC signature was verified against (see extractCloneURL). We
// deliberately do NOT key on repository.full_name — that field is unauthenticated
// and a signed-but-inconsistent payload could otherwise write lifecycle rows
// under a different repo than the one whose secret validated the request.
func repoKey(repo github.Repository) string {
	return domain.NormalizeGitURL(repo.CloneURL)
}

// recordGitHubPR persists the PR lifecycle timestamps carried by a pull_request
// event (the opened side always; the merge side on a merged close) so DORA
// lead-time decomposition can later read them. Best-effort — a write failure
// logs but never fails the webhook, which still needs to trigger the build.
func (h *Handler) recordGitHubPR(ctx context.Context, ev github.PullRequestEvent) {
	repo := repoKey(ev.Repository)
	if repo == "" || ev.Number <= 0 {
		return
	}
	if err := h.store.RecordPullRequestOpened(ctx, "github", repo, ev.Number,
		ev.CreatedAt, ev.Title, ev.Author, ev.HeadRef, ev.BaseRef, ev.HeadSHA); err != nil {
		h.log.Warn("github webhook: record PR opened failed", "number", ev.Number, "err", err)
	}
	if ev.Merged && !ev.MergedAt.IsZero() {
		if err := h.store.RecordPullRequestMerged(ctx, "github", repo, ev.Number,
			ev.MergedAt, ev.MergeSHA); err != nil {
			h.log.Warn("github webhook: record PR merged failed", "number", ev.Number, "err", err)
		}
	}
}

// handleReview reacts to GitHub's pull_request_review webhook: an approving
// review stamps the PR's approved_at (the Review-stage boundary). Non-approval
// reviews are acknowledged and ignored.
func (h *Handler) handleReview(w http.ResponseWriter, r *http.Request, body []byte, delivery string, rec *deliveryRec) {
	ev, err := github.ParseReviewEvent(body)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "parse review: " + err.Error()
		h.log.Warn("github webhook: review parse failed", "delivery", delivery, "err", err)
		http.Error(w, "invalid pull_request_review payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !ev.IsApproval() {
		rec.status = store.WebhookStatusIgnored
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Strict: an approval we'd persist must carry a usable PR number + time.
	if ev.Number <= 0 || ev.SubmittedAt.IsZero() {
		rec.status = store.WebhookStatusError
		rec.errText = "review missing number/submitted_at"
		http.Error(w, "invalid pull_request_review payload", http.StatusBadRequest)
		return
	}
	repo := repoKey(ev.Repository)
	if err := h.store.RecordPullRequestApproved(r.Context(), "github", repo, ev.Number, ev.SubmittedAt); err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "record approval: " + err.Error()
		h.log.Warn("github webhook: record PR approval failed", "number", ev.Number, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rec.status = store.WebhookStatusAccepted
	h.log.Info("github webhook: PR approval recorded", "delivery", delivery, "number", ev.Number)
	w.WriteHeader(http.StatusNoContent)
}
