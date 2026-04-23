// Package runs exposes the HTTP endpoints backing the /runs + /projects
// UI surface. Read endpoints (Detail, Artifacts) coexist with the action
// endpoints (Cancel, Rerun, pipeline ManualTrigger) in one handler so the
// server action layer in web/ can stay thin.
package runs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/configsync"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

const defaultLogsPerJob int32 = 200

// downloadTTL is how long the /artifacts response stamps each signed
// GET URL for. Short enough to limit sharing, long enough for the UI
// to render + the user to click through. The UI refetches on the
// same cadence as the run detail (client-side polling).
const downloadTTL = 5 * time.Minute

// CancelDispatcher is the narrow slice of grpcsrv.SessionStore the
// Cancel handler needs to push CancelJob messages down to the agent
// that's running a given job. Defined locally so this package
// doesn't need to import grpcsrv just for the type.
type CancelDispatcher interface {
	Dispatch(agentID uuid.UUID, msg *gocdnextv1.ServerMessage) error
}

type Handler struct {
	store         *store.Store
	artifactStore artifacts.Store
	fetcher       configsync.Fetcher
	dispatcher    CancelDispatcher
	log           *slog.Logger
}

func NewHandler(s *store.Store, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: s, log: log}
}

// WithCancelDispatcher wires the gRPC session registry. When present,
// cancelling a run pushes CancelJob messages to every agent running
// one of the run's jobs so the container actually stops. Without it
// the handler falls back to "DB-only" cancel — the pre-C6 behaviour.
func (h *Handler) WithCancelDispatcher(d CancelDispatcher) *Handler {
	h.dispatcher = d
	return h
}

// WithArtifactStore enables the /artifacts endpoint. Without it the
// endpoint returns 503 — endpoints that don't depend on the store
// (Detail) keep working.
func (h *Handler) WithArtifactStore(st artifacts.Store) *Handler {
	h.artifactStore = st
	return h
}

// WithConfigFetcher enables the trigger-seed fallback: when a user
// asks to trigger a pipeline that has no modification yet (never
// received a push), we resolve HEAD of the default branch via the
// fetcher, insert a modification row and retry the trigger. Leaving
// the fetcher nil preserves the legacy "422 — push to seed" UX.
func (h *Handler) WithConfigFetcher(f configsync.Fetcher) *Handler {
	h.fetcher = f
	return h
}

// Detail handles GET /api/v1/runs/{id}. Returns the run, its stages and jobs,
// plus a tail of log lines per job controlled by the `logs` query param
// (default 200, max 2000, 0 disables logs).
func (h *Handler) Detail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := chi.URLParam(r, "id")
	runID, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	logsPerJob := defaultLogsPerJob
	if raw := r.URL.Query().Get("logs"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 0 {
			http.Error(w, "invalid 'logs' query", http.StatusBadRequest)
			return
		}
		if parsed > 2000 {
			parsed = 2000
		}
		logsPerJob = int32(parsed)
	}

	detail, err := h.store.GetRunDetail(r.Context(), runID, logsPerJob)
	if err != nil {
		if errors.Is(err, store.ErrRunNotFound) {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		h.log.Error("get run detail", "run_id", runID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(detail)
}

// ArtifactResponse is the wire shape the UI consumes.
type ArtifactResponse struct {
	ID            string    `json:"id"`
	JobRunID      string    `json:"job_run_id"`
	JobName       string    `json:"job_name"`
	Path          string    `json:"path"`
	Status        string    `json:"status"`
	SizeBytes     int64     `json:"size_bytes"`
	ContentSHA256 string    `json:"content_sha256"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	// DownloadURL is a short-lived signed GET. Absent when the row is
	// still pending (agent hasn't uploaded yet) or when the backend
	// doesn't produce signed URLs for the key (unexpected, logged).
	DownloadURL          string     `json:"download_url,omitempty"`
	DownloadURLExpiresAt *time.Time `json:"download_url_expires_at,omitempty"`
}

// Artifacts handles GET /api/v1/runs/{id}/artifacts. Returns all
// non-deleted artefacts for a run, each with a signed GET URL so the
// UI can link directly to the blob (filesystem: our own handler;
// S3/GCS: the cloud).
func (h *Handler) Artifacts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.artifactStore == nil {
		http.Error(w, "artifact backend not configured", http.StatusServiceUnavailable)
		return
	}

	idStr := chi.URLParam(r, "id")
	runID, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	rows, err := h.store.ListArtifactsWithJobByRun(r.Context(), runID)
	if err != nil {
		h.log.Error("list artifacts", "run_id", runID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	out := make([]ArtifactResponse, 0, len(rows))
	for _, a := range rows {
		resp := ArtifactResponse{
			ID:            a.ID.String(),
			JobRunID:      a.JobRunID.String(),
			JobName:       a.JobName,
			Path:          a.Path,
			Status:        a.Status,
			SizeBytes:     a.SizeBytes,
			ContentSHA256: a.ContentSHA256,
			CreatedAt:     a.CreatedAt,
			ExpiresAt:     a.ExpiresAt,
		}
		if a.Status == "ready" {
			if url, expires := h.signDownload(r.Context(), a.StorageKey, a.Path); url != "" {
				resp.DownloadURL = url
				resp.DownloadURLExpiresAt = &expires
			}
		}
		out = append(out, resp)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// signDownload calls the store's pre-sign. Failures are swallowed so
// one bad row doesn't kill the whole response — the UI just shows the
// row without a download link.
//
// `artifactPath` is the YAML-declared path (e.g. "bin/gocdnext-server"
// or "web/.next/standalone/") and becomes the downloaded file's name
// via WithContentDisposition — filesystem encodes it as a query param,
// S3/GCS bake it into the pre-signed URL's ResponseContentDisposition
// so browsers save the blob with a useful name instead of the raw
// token. Without this, users had to gunzip+untar as two steps.
func (h *Handler) signDownload(ctx context.Context, storageKey, artifactPath string) (string, time.Time) {
	var opts []artifacts.GetOption
	if name := downloadFilename(artifactPath); name != "" {
		opts = append(opts, artifacts.WithContentDisposition(name))
	}
	signed, err := h.artifactStore.SignedGetURL(ctx, storageKey, downloadTTL, opts...)
	if err != nil {
		h.log.Warn("sign artifact get", "storage_key", storageKey, "err", err)
		return "", time.Time{}
	}
	return signed.URL, signed.ExpiresAt
}

// downloadFilename picks a sensible saved-as name from the artifact's
// source path. File artifacts land as "<basename>.tar.gz"; directory
// artifacts as "<dirname>.tar.gz". Empty input yields "artifact.tar.gz".
func downloadFilename(artifactPath string) string {
	p := strings.TrimRight(artifactPath, "/")
	if p == "" {
		return "artifact.tar.gz"
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	if p == "" {
		return "artifact.tar.gz"
	}
	return p + ".tar.gz"
}
