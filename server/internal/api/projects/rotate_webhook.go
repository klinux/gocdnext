package projects

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// RotateWebhookSecret handles
// POST /api/v1/projects/{slug}/scm-sources/rotate-webhook-secret.
//
// Generates a fresh 32-byte secret for the project's bound
// scm_source, seals it via the store cipher, replaces the stored
// ciphertext, and returns the plaintext exactly once. Any existing
// webhook still signed with the old secret starts 401-ing after
// this call — the operator has to update the provider's webhook
// config with the new value.
//
// 404 when the project has no scm_source bound.
// 503 when the server has no cipher configured.
func (h *Handler) RotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "missing slug", http.StatusBadRequest)
		return
	}

	scm, err := h.store.FindSCMSourceByProjectSlug(r.Context(), slug)
	switch {
	case errors.Is(err, store.ErrSCMSourceNotFound):
		http.Error(w, "project has no scm_source bound", http.StatusNotFound)
		return
	case err != nil:
		h.log.Error("rotate webhook secret: lookup", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	plain, err := h.store.RotateSCMSourceWebhookSecret(r.Context(), scm.ID)
	if errors.Is(err, store.ErrAuthProviderCipherUnset) {
		http.Error(w, "GOCDNEXT_SECRET_KEY must be set to rotate webhook secrets", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		h.log.Error("rotate webhook secret", "scm_source_id", scm.ID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Re-run the webhook reconcile with the new plaintext. Two
	// cases matter here:
	//   1) The hook already exists → UpdateRepoHook PATCHes its
	//      config.secret so GitHub signs future pushes with the
	//      new value (otherwise the rotation would silently break
	//      validation on the next push).
	//   2) The hook never registered (e.g. initial install
	//      failed on 422 "unreachable URL") → CreateRepoHook
	//      retries now that secrets / URL may be fixed.
	// A nil outcome happens on projects without a provider hook
	// affordance (manual, non-github) — stay silent there.
	applied := &store.SCMSourceApplied{
		ID:            scm.ID,
		Provider:      scm.Provider,
		URL:           scm.URL,
		DefaultBranch: scm.DefaultBranch,
	}
	resp := map[string]any{
		"scm_source_id":            scm.ID.String(),
		"generated_webhook_secret": plain,
	}
	if hr := h.reconcileSCMSourceWebhook(r.Context(), applied, plain); hr != nil {
		resp["webhook"] = hr
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
