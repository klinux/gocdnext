package webhook

import (
	"context"
	"net/http"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// PRCommitsFetcher resolves a pull request's first-commit timestamp (the start
// of the DORA Coding stage) from the provider API, given the scm_source that
// authenticated the delivery. ok=false on any miss — the Coding stage is then
// just unavailable for that PR.
type PRCommitsFetcher interface {
	PRFirstCommitAt(ctx context.Context, source store.SCMSource, number int) (time.Time, bool)
}

// WithPRCommitsFetcher opts the handler into recording PR first-commit times.
func (h *Handler) WithPRCommitsFetcher(f PRCommitsFetcher) *Handler {
	h.prCommits = f
	return h
}

// recordFirstCommit fetches + persists a PR's first-commit time, best-effort,
// on the opening event (GitHub only). Bounded so it never holds the webhook
// response for long; a miss leaves first_commit_at unset.
func (h *Handler) recordFirstCommit(ctx context.Context, cloneURL string, number int) {
	if h.prCommits == nil {
		return
	}
	scm, ok := h.driftLookup(ctx, cloneURL)
	if !ok {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	at, ok := h.prCommits.PRFirstCommitAt(fctx, scm, number)
	if !ok {
		return
	}
	if err := h.store.RecordPullRequestFirstCommit(ctx, "github", domain.NormalizeGitURL(cloneURL), number, at); err != nil {
		h.log.Warn("github webhook: record PR first commit failed", "number", number, "err", err)
	}
}

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
