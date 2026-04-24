package runs_test

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/api/runs"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/logstream"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func streamHandler(t *testing.T) (*runs.Handler, *pgxpool.Pool, *logstream.Broker) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	broker := logstream.New(32, nil)
	h := runs.NewHandler(store.New(pool), slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithLogBroker(broker)
	return h, pool, broker
}

func TestLogsStream_NotFound(t *testing.T) {
	h, _, _ := streamHandler(t)
	r := chi.NewRouter()
	r.Get("/api/v1/runs/{id}/logs/stream", h.LogsStream)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+uuid.NewString()+"/logs/stream", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestLogsStream_EmitsPublishedLine(t *testing.T) {
	h, pool, broker := streamHandler(t)
	runID := seedRun(t, pool)

	r := chi.NewRouter()
	r.Get("/api/v1/runs/{id}/logs/stream", h.LogsStream)

	srv := httptest.NewServer(r)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/runs/"+runID.String()+"/logs/stream", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d body=%s", res.StatusCode, string(body))
	}

	reader := bufio.NewReader(res.Body)

	// The handler writes ": ready\n\n" immediately after Subscribe
	// returns. Reading that line proves our subscription is live
	// before we Publish — no race between send and subscribe.
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read ready: %v", err)
	}
	if !strings.HasPrefix(line, ": ready") {
		t.Fatalf("want ready comment, got %q", line)
	}
	_, _ = reader.ReadString('\n') // blank line after comment

	broker.Publish(logstream.Event{
		RunID:    runID,
		JobRunID: uuid.New(),
		Seq:      42,
		Stream:   "stdout",
		At:       time.Now(),
		Text:     "hello from test",
	})

	var (
		sawEvent bool
		sawID    bool
		sawData  bool
	)
	// SSE frame is 3 non-empty lines + a blank terminator. Read a
	// few more in case the runtime interleaves a keepalive comment
	// (unlikely at 15s cadence but the ticker is live).
	for i := 0; i < 6; i++ {
		l, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read frame line %d: %v", i, err)
		}
		switch {
		case strings.HasPrefix(l, "event: log"):
			sawEvent = true
		case strings.HasPrefix(l, "id: 42"):
			sawID = true
		case strings.HasPrefix(l, "data: "):
			sawData = true
			if !strings.Contains(l, `"hello from test"`) {
				t.Errorf("data missing text: %q", l)
			}
		}
		if sawEvent && sawID && sawData {
			return
		}
	}
	t.Fatalf("incomplete frame: event=%v id=%v data=%v", sawEvent, sawID, sawData)
}

func TestLogsStream_ServiceUnavailableWithoutBroker(t *testing.T) {
	pool := dbtest.SetupPool(t)
	// Handler without WithLogBroker: SSE should 503, NOT 404 (run
	// existence isn't checked before the feature gate fires).
	h := runs.NewHandler(store.New(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	runID := seedRun(t, pool)

	r := chi.NewRouter()
	r.Get("/api/v1/runs/{id}/logs/stream", h.LogsStream)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID.String()+"/logs/stream", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}
