package runs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/logstream"
)

// heartbeatInterval is how often the SSE handler emits a comment
// frame when no real event has flowed. Without it, proxies /
// corporate firewalls that buffer idle TCP streams decide the
// connection is dead and kill it mid-run. 15s is a safe floor
// well under the 30-60s most middleboxes allow.
const heartbeatInterval = 15 * time.Second

// WithLogBroker wires the in-process fan-out used by the SSE log
// endpoint. nil is allowed: the endpoint still works, it just
// never delivers live events (clients fall back to poll via
// Detail). Kept behind a With* to match the rest of the handler.
func (h *Handler) WithLogBroker(b *logstream.Broker) *Handler {
	h.logBroker = b
	return h
}

// LogsStream handles GET /api/v1/runs/{id}/logs/stream. Open a
// Server-Sent Events connection that emits each new log line of
// the run as an `event: log` frame. Clients are expected to
// bootstrap the backlog via Detail, then open this stream; the
// broker only fans out lines that arrive AFTER the subscription
// is registered, so the race window is "line persisted while we
// were still handshaking", which the client masks by treating
// an already-seen (job_id, seq) tuple as a no-op.
//
// The endpoint does NOT replay history. If a user reloads the
// page they hit Detail first (gets backlog) then this (gets
// future lines). The broker is in-process only — see
// logstream.Broker docstring for the HA story.
func (h *Handler) LogsStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if h.logBroker == nil {
		http.Error(w, "log streaming not enabled", http.StatusServiceUnavailable)
		return
	}

	// Validate the run exists. Authz is already enforced by the
	// outer middleware group (same RequireAuth slot as Detail);
	// this check catches a typo'd URL so the browser doesn't
	// silently sit on a keepalive stream that never fires.
	if exists, err := h.store.RunExists(r.Context(), runID); err != nil {
		http.Error(w, "run lookup failed", http.StatusInternalServerError)
		return
	} else if !exists {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Server setups without a flushable ResponseWriter
		// (old proxies under test) can't do SSE — say so
		// loudly instead of buffering the whole stream forever.
		http.Error(w, "server does not support streaming", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// X-Accel-Buffering disables nginx response buffering for this
	// request specifically. Without it, a nginx sitting between the
	// browser and gocdnext holds frames until it sees a byte
	// threshold, defeating the "logs appear live" point.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub := h.logBroker.Subscribe(runID)
	defer sub.Close()

	// Immediate "ready" comment so the browser's EventSource
	// resolves the open promise before any real data flows. Also
	// doubles as a sanity check that the pipeline is alive.
	if _, err := fmt.Fprint(w, ": ready\n\n"); err != nil {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				return
			}
			if err := writeLogEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeLogEvent(w http.ResponseWriter, ev logstream.Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	// SSE frame shape: "event: <name>\ndata: <body>\n\n". The body
	// is single-line JSON — escaping newlines inside `text` is
	// already handled by json.Marshal, so we never have to worry
	// about a stray \n breaking the frame.
	var buf strings.Builder
	buf.WriteString("event: log\n")
	buf.WriteString("id: ")
	buf.WriteString(strconv.FormatInt(ev.Seq, 10))
	buf.WriteString("\n")
	buf.WriteString("data: ")
	buf.Write(payload)
	buf.WriteString("\n\n")
	_, err = w.Write([]byte(buf.String()))
	return err
}
