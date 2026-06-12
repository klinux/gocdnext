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
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
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
		WithArtifactStore(fs, 5*time.Minute, 5*time.Minute, 24*time.Hour)

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

// seedDispatchedJob: seeds a pipeline + run + one job + an agent row
// for the given name. The job is left in 'queued' state and ONLY gets
// flipped to 'running' + bound to the agent AFTER the caller registers
// (see flipJobRunning below) — matching the real-world order
// (Register → scheduler dispatch → RPC) and avoiding the register-fence
// reclaiming a pre-seeded "running" row that no real agent process ever
// owned. Returns the run_id, the queued job_run_id, and the agent's
// UUID (caller registers and then calls flipJobRunning to dispatch).
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

	// NOTE: NO dispatch here. The job stays 'queued' until the caller
	// has registered the agent and calls flipJobRunning. Pre-fence,
	// this helper did the dispatch in one shot; the fence rightly
	// reclaims any 'running' row attributed to an agent at Register
	// time (it can only have come from the prior process), so the
	// dispatch MUST happen after Register or the fence undoes it.
	return
}

// flipJobRunning is the post-Register dispatch the new seedDispatchedJob
// flow leaves to the caller. Sets status='running', agent_id, started_at
// — the same UPDATE the real scheduler issues via AssignJob (in
// scheduler.sql), just bypassing the LISTEN/NOTIFY path tests don't need.
func flipJobRunning(t *testing.T, pool *pgxpool.Pool, jobID, agentID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='running', agent_id=$1, started_at=NOW() WHERE id=$2`,
		agentID, jobID,
	); err != nil {
		t.Fatalf("flipJobRunning: %v", err)
	}
}

func TestRequestArtifactUpload_HappyPath(t *testing.T) {
	pool, client, fs := bootServerWithArtifacts(t)
	_ = fs

	runID, jobID, agentID := seedDispatchedJob(t, pool, "runner-art-1")

	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "runner-art-1", Token: "tok-art",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Dispatch AFTER Register so the register-fence sees no
	// orphaned-running rows to reclaim. Matches the production
	// order: agent registers → scheduler dispatches.
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

// TestRequestArtifactUpload_DedupesPaths — the same path appearing
// twice in `paths[]` (in canonical or trailing-slash form) must
// produce ONE ticket, not two. Without the dedupe, the second
// InsertPendingArtifact would hit the partial unique index from
// migration 00035 and abort the batch — leaving the agent with no
// way to upload even the unique paths.
func TestRequestArtifactUpload_DedupesPaths(t *testing.T) {
	pool, client, _ := bootServerWithArtifacts(t)
	runID, jobID, agentID := seedDispatchedJob(t, pool, "runner-art-dedupe")

	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "runner-art-dedupe", Token: "tok-art",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	flipJobRunning(t, pool, jobID, agentID)

	// `dist`, `dist/`, and `dist` again all canonicalize to "dist".
	// `coverage.out` is the unique companion.
	up, err := client.RequestArtifactUpload(context.Background(), &gocdnextv1.RequestArtifactUploadRequest{
		SessionId: resp.SessionId,
		RunId:     runID.String(),
		JobId:     jobID.String(),
		Paths:     []string{"dist", "dist/", "dist", "coverage.out"},
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got := len(up.Tickets); got != 2 {
		t.Fatalf("tickets = %d, want 2 (dedupe should collapse dist/dist//dist)", got)
	}

	// First occurrence's shape ("dist") wins for the ticket round-trip.
	if up.Tickets[0].Path != "dist" {
		t.Errorf("ticket[0].Path = %q, want %q (first-occurrence shape)", up.Tickets[0].Path, "dist")
	}
	if up.Tickets[1].Path != "coverage.out" {
		t.Errorf("ticket[1].Path = %q, want %q", up.Tickets[1].Path, "coverage.out")
	}

	// Exactly one row landed for `dist` (stored as canonical, no trailing slash).
	s := store.New(pool)
	rows, err := s.ListArtifactsByJobRun(context.Background(), jobID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	distCount := 0
	for _, r := range rows {
		if r.Path == "dist" {
			distCount++
		}
	}
	if distCount != 1 {
		t.Fatalf("rows with path=dist = %d, want 1", distCount)
	}
}

func TestRequestArtifactUpload_RejectsBadSession(t *testing.T) {
	pool, client, _ := bootServerWithArtifacts(t)
	// Bad-session test: the session check fails before any ownership
	// lookup, so the job_run can stay 'queued' — we just need the
	// run/job ids to feed into the RPC. No flipJobRunning needed.
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
	runID, jobID, ownerID := seedDispatchedJob(t, pool, "owner-agent")
	// Owner never registers, so its agent_id never goes through the
	// fence. Flip the job to running for the owner BEFORE the foreign
	// register — the test needs the job to be owned (non-NULL
	// agent_id) so the ownership mismatch is what surfaces.
	flipJobRunning(t, pool, jobID, ownerID)

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
	runID, jobID, agentID := seedDispatchedJob(t, pool, "agent-no-art")

	// Register so we have a session; the Unimplemented check fires
	// AFTER the ownership check, so dispatch the job to the agent
	// post-register (post-fence) for the ownership check to pass
	// before the Unimplemented branch is reached.
	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "agent-no-art", Token: "tok-art",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	flipJobRunning(t, pool, jobID, agentID)

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
