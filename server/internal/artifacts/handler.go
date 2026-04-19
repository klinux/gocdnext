package artifacts

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

// Handler serves signed PUT/GET on `/artifacts/{token}` for the
// FilesystemStore backend. S3 and GCS do their own signing and bypass the
// server entirely, so this handler is a no-op for them — don't mount it
// when the backend is not filesystem.
type Handler struct {
	store *FilesystemStore
	log   *slog.Logger
	// maxBody caps PUT body size. Protects disk against a runaway agent
	// or replayed token. Zero = no cap (not recommended).
	maxBody int64
}

// NewHandler binds a chi-compatible handler. maxBodyBytes of 0 means "no
// cap"; typical production default is 2 GiB.
func NewHandler(store *FilesystemStore, log *slog.Logger, maxBodyBytes int64) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: store, log: log, maxBody: maxBodyBytes}
}

// Mount registers PUT and GET on `/artifacts/{token}` against the given
// chi router.
func (h *Handler) Mount(r chi.Router) {
	r.Put("/artifacts/{token}", h.handlePut)
	r.Get("/artifacts/{token}", h.handleGet)
}

func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request) {
	key, ok := h.verify(w, r, VerbPUT)
	if !ok {
		return
	}

	body := r.Body
	if h.maxBody > 0 {
		body = http.MaxBytesReader(w, r.Body, h.maxBody)
	}
	defer func() { _ = body.Close() }()

	n, err := h.store.Put(r.Context(), key, body)
	if err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			http.Error(w, "artifact exceeds max size", http.StatusRequestEntityTooLarge)
			return
		}
		h.log.Error("artifact put failed", "key", key, "err", err)
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("X-Artifact-Size", strconv.FormatInt(n, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	key, ok := h.verify(w, r, VerbGET)
	if !ok {
		return
	}

	size, err := h.store.Head(r.Context(), key)
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.log.Error("artifact head failed", "key", key, "err", err)
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}

	rc, err := h.store.Get(r.Context(), key)
	if err != nil {
		h.log.Error("artifact get failed", "key", key, "err", err)
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if _, err := io.Copy(w, rc); err != nil {
		h.log.Warn("artifact stream aborted", "key", key, "err", err)
	}
}

// verify pulls the token from the chi path param, un-escapes it, and
// asks the Signer. On failure we 401 and log nothing — callers see a
// generic "bad token", never *why* it was bad.
func (h *Handler) verify(w http.ResponseWriter, r *http.Request, verb Verb) (string, bool) {
	raw := chi.URLParam(r, "token")
	tok, err := url.PathUnescape(raw)
	if err != nil {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return "", false
	}
	key, err := h.store.SignerRef().Verify(tok, verb, time.Now())
	if err != nil {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return "", false
	}
	return key, true
}
