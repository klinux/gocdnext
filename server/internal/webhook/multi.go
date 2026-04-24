package webhook

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/store"
	bitbucketpkg "github.com/gocdnext/gocdnext/server/internal/webhook/bitbucket"
	gitlabpkg "github.com/gocdnext/gocdnext/server/internal/webhook/gitlab"
)

// normalizedPush is the provider-agnostic push shape the shared
// persistPush helper consumes. Provider-specific handlers build
// one after verifying + parsing their own payload; the helper
// does the drift-apply + material match + modification +
// run-create + delivery-recording flow once.
type normalizedPush struct {
	Provider    string
	Delivery    string
	CloneURL    string
	Branch      string
	After       string
	Body        []byte // raw webhook body for modification payload storage
	AuthorName  string
	CommitMsg   string
	CommittedAt time.Time
}

// persistPush runs the common tail after a webhook has passed
// signature verification + push parsing. Writes the HTTP
// response itself — caller returns right after.
func (h *Handler) persistPush(
	w http.ResponseWriter, r *http.Request,
	rec *deliveryRec, np normalizedPush,
) {
	// Config drift: same DB flow GitHub handler uses. Re-reads
	// `.gocdnext/` via configsync.MultiFetcher which dispatches
	// by provider internally, so self-hosted GitLab / Bitbucket
	// Cloud both work without new code here.
	var driftOutcome DriftOutcome
	if scm, ok := h.driftLookup(r.Context(), np.CloneURL); ok {
		driftOutcome = h.applyDrift(r.Context(), scm, np.Branch, np.After)
		if driftOutcome.Applied {
			h.log.Info(np.Provider+" webhook: config drift re-applied",
				"delivery", np.Delivery, "scm_source_id", scm.ID, "revision", np.After)
		} else if driftOutcome.Error != "" {
			h.log.Warn(np.Provider+" webhook: config drift failed",
				"delivery", np.Delivery, "scm_source_id", scm.ID, "err", driftOutcome.Error)
		}
	}

	fp := store.FingerprintFor(np.CloneURL, np.Branch)
	material, err := h.store.FindMaterialByFingerprint(r.Context(), fp)
	if errors.Is(err, store.ErrMaterialNotFound) {
		h.log.Info(np.Provider+" webhook: no matching material",
			"delivery", np.Delivery, "clone_url", np.CloneURL, "branch", np.Branch,
			"fingerprint", fp, "drift_applied", driftOutcome.Applied)
		if driftOutcome.Attempted {
			rec.status = store.WebhookStatusAccepted
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"drift": map[string]any{
					"applied":  driftOutcome.Applied,
					"error":    driftOutcome.Error,
					"revision": driftOutcome.Revision,
				},
			})
			return
		}
		rec.status = store.WebhookStatusIgnored
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "material lookup: " + err.Error()
		h.log.Error(np.Provider+" webhook: material lookup failed",
			"delivery", np.Delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rec.materialID = material.ID

	mod := store.Modification{
		MaterialID:  material.ID,
		Revision:    np.After,
		Branch:      np.Branch,
		Author:      np.AuthorName,
		Message:     np.CommitMsg,
		Payload:     json.RawMessage(np.Body),
		CommittedAt: np.CommittedAt,
	}
	res, err := h.store.InsertModification(r.Context(), mod)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "insert modification: " + err.Error()
		h.log.Error(np.Provider+" webhook: insert modification failed",
			"delivery", np.Delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := map[string]any{
		"modification_id": res.ID,
		"created":         res.Created,
		"material_id":     material.ID.String(),
	}
	if driftOutcome.Attempted {
		resp["drift"] = map[string]any{
			"applied":  driftOutcome.Applied,
			"error":    driftOutcome.Error,
			"revision": driftOutcome.Revision,
		}
	}

	if res.Created {
		runRes, err := h.store.CreateRunFromModification(r.Context(), store.CreateRunFromModificationInput{
			PipelineID:     material.PipelineID,
			MaterialID:     material.ID,
			ModificationID: res.ID,
			Revision:       np.After,
			Branch:         np.Branch,
			Provider:       np.Provider,
			Delivery:       np.Delivery,
			TriggeredBy:    "system:webhook",
		})
		if err != nil {
			rec.status = store.WebhookStatusError
			rec.errText = "create run: " + err.Error()
			h.log.Error(np.Provider+" webhook: create run failed",
				"delivery", np.Delivery, "modification_id", res.ID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		resp["run_id"] = runRes.RunID.String()
		resp["run_counter"] = runRes.Counter
		h.log.Info(np.Provider+" webhook: run queued",
			"delivery", np.Delivery, "pipeline_id", material.PipelineID,
			"run_id", runRes.RunID, "counter", runRes.Counter)
	} else {
		h.log.Info(np.Provider+" webhook: modification already present, no run queued",
			"delivery", np.Delivery, "modification_id", res.ID)
	}

	rec.status = store.WebhookStatusAccepted
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
}

// HandleGitLab verifies the X-Gitlab-Token header against the
// scm_source's registered secret and persists a modification on
// a push event. Non-push events (merge request, issue, tag) get
// 204 so GitLab doesn't retry.
//
// Route: POST /api/webhooks/gitlab — register outside auth
// middleware since HMAC verification is per-repo.
func (h *Handler) HandleGitLab(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-Gitlab-Event")
	if event == "" {
		http.Error(w, "missing X-Gitlab-Event header", http.StatusBadRequest)
		return
	}
	token := r.Header.Get("X-Gitlab-Token")

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	sc := &statusCapture{ResponseWriter: w}
	rec := &deliveryRec{
		provider: "gitlab",
		event:    event,
		headers:  headersJSON(r.Header),
		payload:  json.RawMessage(body),
		writer:   sc,
	}
	defer h.recordDelivery(r.Context(), rec)
	w = sc

	// GitLab puts the repo url inside project.git_http_url OR
	// repository.git_http_url depending on the hook kind. Peek
	// once for lookup.
	cloneURL := extractGitLabCloneURL(body)
	if cloneURL == "" {
		rec.status = store.WebhookStatusRejected
		rec.errText = "missing repository.git_http_url"
		http.Error(w, "invalid payload: repository.git_http_url required", http.StatusBadRequest)
		return
	}
	auth, err := h.store.FindSCMSourceWebhookSecret(r.Context(), cloneURL)
	if errors.Is(err, store.ErrSCMSourceNotFound) {
		rec.status = store.WebhookStatusRejected
		rec.errText = "no scm_source registered for this repo"
		http.Error(w, "no scm_source registered for this repo", http.StatusUnauthorized)
		return
	}
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "scm_source lookup: " + err.Error()
		h.log.Error("gitlab webhook: scm_source lookup", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if auth.Secret == "" {
		rec.status = store.WebhookStatusRejected
		rec.errText = "no webhook secret registered for this scm_source"
		http.Error(w, "no webhook secret registered for this repo", http.StatusUnauthorized)
		return
	}
	if err := gitlabpkg.VerifyToken(auth.Secret, token); err != nil {
		rec.status = store.WebhookStatusRejected
		rec.errText = "invalid token: " + err.Error()
		h.log.Warn("gitlab webhook: token rejected",
			"event", event, "scm_source_id", auth.ID, "err", err)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	if event != "Push Hook" {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("gitlab webhook: ignored event", "event", event)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ev, err := gitlabpkg.ParsePushEvent(body)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "parse push: " + err.Error()
		http.Error(w, "invalid push payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if ev.Deleted || ev.HeadCommit == nil {
		rec.status = store.WebhookStatusIgnored
		w.WriteHeader(http.StatusNoContent)
		return
	}

	delivery := r.Header.Get("X-Gitlab-Event-UUID")
	h.persistPush(w, r, rec, normalizedPush{
		Provider:    "gitlab",
		Delivery:    delivery,
		CloneURL:    ev.Repository.CloneURL,
		Branch:      ev.Branch,
		After:       ev.After,
		Body:        body,
		AuthorName:  ev.HeadCommit.Author,
		CommitMsg:   ev.HeadCommit.Message,
		CommittedAt: ev.HeadCommit.Timestamp,
	})
}

// HandleBitbucket verifies HMAC-SHA256 (X-Hub-Signature) against
// the scm_source's secret and persists a modification on a
// repo:push event. Other events get 204 so Bitbucket doesn't
// retry.
func (h *Handler) HandleBitbucket(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-Event-Key")
	if event == "" {
		http.Error(w, "missing X-Event-Key header", http.StatusBadRequest)
		return
	}
	signature := r.Header.Get("X-Hub-Signature")

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	sc := &statusCapture{ResponseWriter: w}
	rec := &deliveryRec{
		provider: "bitbucket",
		event:    event,
		headers:  headersJSON(r.Header),
		payload:  json.RawMessage(body),
		writer:   sc,
	}
	defer h.recordDelivery(r.Context(), rec)
	w = sc

	cloneURL := extractBitbucketCloneURL(body)
	if cloneURL == "" {
		rec.status = store.WebhookStatusRejected
		rec.errText = "missing repository.links"
		http.Error(w, "invalid payload: repository.links.html.href required", http.StatusBadRequest)
		return
	}
	auth, err := h.store.FindSCMSourceWebhookSecret(r.Context(), cloneURL)
	if errors.Is(err, store.ErrSCMSourceNotFound) {
		rec.status = store.WebhookStatusRejected
		rec.errText = "no scm_source registered for this repo"
		http.Error(w, "no scm_source registered for this repo", http.StatusUnauthorized)
		return
	}
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "scm_source lookup: " + err.Error()
		h.log.Error("bitbucket webhook: scm_source lookup", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if auth.Secret == "" {
		rec.status = store.WebhookStatusRejected
		rec.errText = "no webhook secret registered"
		http.Error(w, "no webhook secret registered for this repo", http.StatusUnauthorized)
		return
	}
	if err := bitbucketpkg.VerifySignature(auth.Secret, body, signature); err != nil {
		rec.status = store.WebhookStatusRejected
		rec.errText = "invalid signature: " + err.Error()
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	if event != "repo:push" {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("bitbucket webhook: ignored event", "event", event)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ev, err := bitbucketpkg.ParsePushEvent(body)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "parse push: " + err.Error()
		http.Error(w, "invalid push payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if ev.Deleted || ev.HeadCommit == nil {
		rec.status = store.WebhookStatusIgnored
		w.WriteHeader(http.StatusNoContent)
		return
	}

	delivery := r.Header.Get("X-Request-UUID")
	h.persistPush(w, r, rec, normalizedPush{
		Provider:    "bitbucket",
		Delivery:    delivery,
		CloneURL:    ev.Repository.CloneURL,
		Branch:      ev.Branch,
		After:       ev.After,
		Body:        body,
		AuthorName:  ev.HeadCommit.Author,
		CommitMsg:   ev.HeadCommit.Message,
		CommittedAt: ev.HeadCommit.Timestamp,
	})
}

// --- helpers: fish the clone url out of each provider's payload
//     shape BEFORE verifying signature. Same strategy as
//     extractCloneURL: attacker picks the URL, but they still
//     have to produce a valid signature for whichever scm_source
//     that lookup resolves to.

func extractGitLabCloneURL(body []byte) string {
	var env struct {
		Repository struct {
			GitHTTPURL string `json:"git_http_url"`
			URL        string `json:"url"`
		} `json:"repository"`
		Project struct {
			GitHTTPURL string `json:"git_http_url"`
		} `json:"project"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	if env.Repository.GitHTTPURL != "" {
		return env.Repository.GitHTTPURL
	}
	if env.Project.GitHTTPURL != "" {
		return env.Project.GitHTTPURL
	}
	return env.Repository.URL
}

func extractBitbucketCloneURL(body []byte) string {
	var env struct {
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
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	for _, c := range env.Repository.Links.Clone {
		if c.Name == "https" {
			return c.Href
		}
	}
	return env.Repository.Links.HTML.Href
}

