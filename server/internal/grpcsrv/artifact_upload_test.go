package grpcsrv_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// bootServerWithArtifacts returns a client + pool + live filesystem
// artifact store so tests can exercise the upload flow without a real
// S3/GCS. The filesystem root is a t.TempDir cleaned up at test end.
func bootServerWithArtifacts(t *testing.T) (*pgxpool.Pool, gocdnextv1.AgentServiceClient, *artifacts.FilesystemStore) {
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

	svc := grpcsrv.NewAgentService(s, grpcsrv.NewSessionStore(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), heartbeatSecs).
		WithArtifactStore(fs, 5*time.Minute, 24*time.Hour)

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
	return pool, gocdnextv1.NewAgentServiceClient(conn), fs
}

// seedDispatchedJob: runs a fresh pipeline, flips one job to running +
// bound to a given agent. Returns the run_id, the dispatched job_run_id,
// and the agent's UUID (so the test can register that agent and get a
// session whose AgentID matches).
func seedDispatchedJob(t *testing.T, pool *pgxpool.Pool, agentName string) (runID, jobID, agentID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	// Seed minimal pipeline via ApplyProject (git material), then trigger
	// a run on its modification.
	s := store.New(pool)
	fp := store.FingerprintFor("https://github.com/org/art", "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "proj-art-" + agentName, Name: "ArtifactTest",
		Pipelines: []*domain.Pipeline{{
			Name:   "p1",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/art", Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{
				Name: "one", Stage: "build", Image: "alpine",
				Tasks: []domain.Task{{Script: "true"}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply project: %v", err)
	}
	pipelineID := applied.Pipelines[0].PipelineID

	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material lookup: %v", err)
	}

	res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:     pipelineID,
		MaterialID:     materialID,
		ModificationID: 1,
		Revision:       "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1",
		Branch:         "main",
		Provider:       "github",
		Delivery:       "test",
		TriggeredBy:    "system:test",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	runID = res.RunID
	if len(res.JobRuns) == 0 {
		t.Fatal("no job_runs created")
	}
	jobID = res.JobRuns[0].ID

	_, err = pool.Exec(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ($1, $2) RETURNING id`,
		agentName, store.HashToken("tok-art"),
	)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	err = pool.QueryRow(ctx,
		`SELECT id FROM agents WHERE name=$1`, agentName,
	).Scan(&agentID)
	if err != nil {
		t.Fatalf("lookup agent: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='running', agent_id=$1, started_at=NOW() WHERE id=$2`,
		agentID, jobID,
	); err != nil {
		t.Fatalf("dispatch job: %v", err)
	}
	return
}

func TestRequestArtifactUpload_HappyPath(t *testing.T) {
	pool, client, fs := bootServerWithArtifacts(t)
	_ = fs

	runID, jobID, _ := seedDispatchedJob(t, pool, "runner-art-1")

	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "runner-art-1", Token: "tok-art",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	up, err := client.RequestArtifactUpload(context.Background(), &gocdnextv1.RequestArtifactUploadRequest{
		SessionId: resp.SessionId,
		RunId:     runID.String(),
		JobId:     jobID.String(),
		Paths:     []string{"bin/core", "coverage.out"},
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got := len(up.Tickets); got != 2 {
		t.Fatalf("tickets = %d, want 2", got)
	}
	for i, tkt := range up.Tickets {
		if tkt.StorageKey == "" {
			t.Errorf("ticket[%d] storage_key empty", i)
		}
		if !strings.HasPrefix(tkt.PutUrl, "http://unit-test/artifacts/") {
			t.Errorf("ticket[%d] put_url = %q", i, tkt.PutUrl)
		}
		if tkt.ExpiresAt == nil {
			t.Errorf("ticket[%d] no expires_at", i)
		}
	}

	// Every ticket's storage_key now exists as a pending DB row.
	s := store.New(pool)
	for _, tkt := range up.Tickets {
		row, err := s.GetArtifactByStorageKey(context.Background(), tkt.StorageKey)
		if err != nil {
			t.Fatalf("lookup %s: %v", tkt.StorageKey, err)
		}
		if row.Status != "pending" {
			t.Errorf("status = %q, want pending", row.Status)
		}
	}
}

func TestRequestArtifactUpload_RejectsBadSession(t *testing.T) {
	pool, client, _ := bootServerWithArtifacts(t)
	runID, jobID, _ := seedDispatchedJob(t, pool, "runner-art-2")

	_, err := client.RequestArtifactUpload(context.Background(), &gocdnextv1.RequestArtifactUploadRequest{
		SessionId: "not-a-session",
		RunId:     runID.String(),
		JobId:     jobID.String(),
		Paths:     []string{"x"},
	})
	if code := status.Code(err); code != codes.Unauthenticated {
		t.Errorf("code = %s, want Unauthenticated", code)
	}
}

func TestRequestArtifactUpload_RejectsForeignAgentSession(t *testing.T) {
	pool, client, _ := bootServerWithArtifacts(t)
	runID, jobID, _ := seedDispatchedJob(t, pool, "owner-agent")

	// Register a *different* agent and use its session.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (name, token_hash) VALUES ($1, $2)`,
		"foreign-agent", store.HashToken("tok-foreign"),
	); err != nil {
		t.Fatalf("seed foreign agent: %v", err)
	}
	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "foreign-agent", Token: "tok-foreign",
	})
	if err != nil {
		t.Fatalf("register foreign: %v", err)
	}

	_, err = client.RequestArtifactUpload(context.Background(), &gocdnextv1.RequestArtifactUploadRequest{
		SessionId: resp.SessionId,
		RunId:     runID.String(),
		JobId:     jobID.String(),
		Paths:     []string{"x"},
	})
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("code = %s, want PermissionDenied", code)
	}
}

func TestRequestArtifactUpload_UnconfiguredBackend(t *testing.T) {
	// Server without WithArtifactStore — default bootServer.
	pool, client := bootServer(t)
	runID, jobID, _ := seedDispatchedJob(t, pool, "agent-no-art")

	// Register so we have a session; the Unimplemented check fires
	// before any DB lookup.
	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "agent-no-art", Token: "tok-art",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	_, err = client.RequestArtifactUpload(context.Background(), &gocdnextv1.RequestArtifactUploadRequest{
		SessionId: resp.SessionId,
		RunId:     runID.String(),
		JobId:     jobID.String(),
		Paths:     []string{"x"},
	})
	if code := status.Code(err); code != codes.Unimplemented {
		t.Errorf("code = %s, want Unimplemented", code)
	}
}
