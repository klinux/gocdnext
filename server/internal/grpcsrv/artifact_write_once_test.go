package grpcsrv_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// headerRecordingStore wraps the filesystem store to (a) record whether
// SignedPutURL was asked for create-only and (b) return an S3/GCS-style
// precondition header when it was — the filesystem store itself never
// returns headers, so this stands in for a real object-store backend.
type headerRecordingStore struct {
	*artifacts.FilesystemStore
	mu            sync.Mutex
	sawCreateOnly bool
}

func (s *headerRecordingStore) SignedPutURL(ctx context.Context, key string, ttl time.Duration, opts ...artifacts.PutOption) (artifacts.SignedURL, error) {
	req := artifacts.ResolvePutOptions(opts)
	s.mu.Lock()
	s.sawCreateOnly = s.sawCreateOnly || req.CreateOnly
	s.mu.Unlock()
	su, err := s.FilesystemStore.SignedPutURL(ctx, key, ttl, opts...)
	if err != nil {
		return su, err
	}
	if req.CreateOnly {
		su.Headers = map[string]string{"If-None-Match": "*"}
	}
	return su, err
}

func bootWithWriteOnce(t *testing.T, writeOnce bool) (*pgxpool.Pool, gocdnextv1.AgentServiceClient, *headerRecordingStore) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	signer, err := artifacts.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	fs, err := artifacts.NewFilesystemStore(t.TempDir(), "http://unit-test", signer)
	if err != nil {
		t.Fatalf("fs store: %v", err)
	}
	rec := &headerRecordingStore{FilesystemStore: fs}

	svc := grpcsrv.NewAgentService(s, grpcsrv.NewSessionStore(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), heartbeatSecs).
		WithArtifactStore(rec, 5*time.Minute, 5*time.Minute, 24*time.Hour).
		WithArtifactWriteOnce(writeOnce)

	lis := bufconn.Listen(1 << 20)
	grpcSrv := grpc.NewServer()
	gocdnextv1.RegisterAgentServiceServer(grpcSrv, svc)
	go func() { _ = grpcSrv.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		grpcSrv.GracefulStop()
		_ = lis.Close()
	})
	return pool, gocdnextv1.NewAgentServiceClient(conn), rec
}

func requestUpload(t *testing.T, client gocdnextv1.AgentServiceClient, pool *pgxpool.Pool, agent string) *gocdnextv1.RequestArtifactUploadResponse {
	t.Helper()
	runID, jobID, agentID := seedDispatchedJob(t, pool, agent)
	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{AgentId: agent, Token: "tok-art"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	flipJobRunning(t, pool, jobID, agentID)
	up, err := client.RequestArtifactUpload(context.Background(), &gocdnextv1.RequestArtifactUploadRequest{
		SessionId: resp.SessionId,
		RunId:     runID.String(),
		JobId:     jobID.String(),
		Paths:     []string{"bin/core", "coverage.out"},
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	return up
}

// TestRequestArtifactUpload_WriteOnceOn_SignsCreateOnlyAndPropagatesHeaders:
// with write-once enabled, every artifact ticket is signed create-only and
// carries the backend's precondition header for the agent to echo (#210).
func TestRequestArtifactUpload_WriteOnceOn_SignsCreateOnlyAndPropagatesHeaders(t *testing.T) {
	pool, client, rec := bootWithWriteOnce(t, true)
	up := requestUpload(t, client, pool, "runner-wo-on")

	if !rec.sawCreateOnly {
		t.Errorf("write-once ON must sign artifact PUTs create-only")
	}
	if len(up.Tickets) != 2 {
		t.Fatalf("tickets = %d, want 2", len(up.Tickets))
	}
	for i, tkt := range up.Tickets {
		if tkt.GetPutHeaders()["If-None-Match"] != "*" {
			t.Errorf("ticket[%d] put_headers = %v, want If-None-Match:*", i, tkt.GetPutHeaders())
		}
	}
}

// TestRequestArtifactUpload_WriteOnceOff_NoPrecondition: default (off) signs a
// plain PUT — no create-only, no headers — so behaviour is unchanged.
func TestRequestArtifactUpload_WriteOnceOff_NoPrecondition(t *testing.T) {
	pool, client, rec := bootWithWriteOnce(t, false)
	up := requestUpload(t, client, pool, "runner-wo-off")

	if rec.sawCreateOnly {
		t.Errorf("write-once OFF must NOT sign create-only")
	}
	for i, tkt := range up.Tickets {
		if len(tkt.GetPutHeaders()) != 0 {
			t.Errorf("ticket[%d] should carry no put_headers when write-once off, got %v", i, tkt.GetPutHeaders())
		}
	}
}
