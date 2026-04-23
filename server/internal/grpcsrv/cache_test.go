package grpcsrv_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// TestRequestCacheGet_MissReturnsNotFound covers the cold-start path
// (first run for a key): no row exists, so the RPC returns
// Found=false rather than an error — the agent treats that as
// "run without pre-populated cache", which is exactly what the
// first build after a pipeline lands should do.
func TestRequestCacheGet_MissReturnsNotFound(t *testing.T) {
	pool, client, _ := bootServerWithArtifacts(t)
	runID, jobID, _ := seedDispatchedJob(t, pool, "cache-agent-miss")

	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "cache-agent-miss", Token: "tok-art",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := client.RequestCacheGet(context.Background(), &gocdnextv1.RequestCacheGetRequest{
		SessionId: resp.SessionId,
		RunId:     runID.String(),
		JobId:     jobID.String(),
		Key:       "pnpm-store",
	})
	if err != nil {
		t.Fatalf("cache get: %v", err)
	}
	if got.Found {
		t.Errorf("Found = true, want false on cold-start miss")
	}
	if got.GetUrl != "" {
		t.Errorf("GetUrl = %q, want empty on miss", got.GetUrl)
	}
}

// TestCacheRoundTrip: Put → MarkReady → Get returns the blob.
// This is the happy path end-to-end: the agent asks for a PUT
// URL, flips the row to ready, and a subsequent Get hands back
// a signed URL against the same storage_key the PUT used.
func TestCacheRoundTrip(t *testing.T) {
	pool, client, _ := bootServerWithArtifacts(t)
	runID, jobID, _ := seedDispatchedJob(t, pool, "cache-agent-rt")

	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "cache-agent-rt", Token: "tok-art",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	ctx := context.Background()

	put, err := client.RequestCachePut(ctx, &gocdnextv1.RequestCachePutRequest{
		SessionId: resp.SessionId,
		RunId:     runID.String(),
		JobId:     jobID.String(),
		Key:       "pnpm-store",
	})
	if err != nil {
		t.Fatalf("cache put: %v", err)
	}
	if put.CacheId == "" || put.StorageKey == "" {
		t.Fatalf("put response missing ids: %+v", put)
	}
	if !strings.HasPrefix(put.PutUrl, "http://unit-test/artifacts/") {
		t.Errorf("put_url = %q", put.PutUrl)
	}

	// Before MarkReady, Get should still miss — we never expose
	// pending rows to avoid handing out torn data.
	miss, err := client.RequestCacheGet(ctx, &gocdnextv1.RequestCacheGetRequest{
		SessionId: resp.SessionId,
		RunId:     runID.String(),
		JobId:     jobID.String(),
		Key:       "pnpm-store",
	})
	if err != nil {
		t.Fatalf("cache get (pending): %v", err)
	}
	if miss.Found {
		t.Error("pending row leaked as ready to Get")
	}

	if _, err := client.MarkCacheReady(ctx, &gocdnextv1.MarkCacheReadyRequest{
		SessionId:     resp.SessionId,
		CacheId:       put.CacheId,
		SizeBytes:     8192,
		ContentSha256: "deadbeef",
	}); err != nil {
		t.Fatalf("mark ready: %v", err)
	}

	ready, err := client.RequestCacheGet(ctx, &gocdnextv1.RequestCacheGetRequest{
		SessionId: resp.SessionId,
		RunId:     runID.String(),
		JobId:     jobID.String(),
		Key:       "pnpm-store",
	})
	if err != nil {
		t.Fatalf("cache get (ready): %v", err)
	}
	if !ready.Found {
		t.Fatal("Found = false after MarkCacheReady")
	}
	if ready.SizeBytes != 8192 || ready.ContentSha256 != "deadbeef" {
		t.Errorf("metadata mismatch: size=%d sha=%q", ready.SizeBytes, ready.ContentSha256)
	}
	if !strings.HasPrefix(ready.GetUrl, "http://unit-test/artifacts/") {
		t.Errorf("get_url = %q", ready.GetUrl)
	}
}

// TestRequestCachePut_RejectsForeignAgentSession: the session
// authz guard (session.AgentID must own job_runs.agent_id) is
// the same invariant RequestArtifactUpload enforces — repeated
// here because cache RPCs have their own handler path and a
// regression would surface here first.
func TestRequestCachePut_RejectsForeignAgentSession(t *testing.T) {
	pool, client, _ := bootServerWithArtifacts(t)
	runID, jobID, _ := seedDispatchedJob(t, pool, "cache-owner")

	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (name, token_hash) VALUES ($1, $2)`,
		"cache-foreign", store.HashToken("tok-foreign"),
	); err != nil {
		t.Fatalf("seed foreign: %v", err)
	}
	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "cache-foreign", Token: "tok-foreign",
	})
	if err != nil {
		t.Fatalf("register foreign: %v", err)
	}

	_, err = client.RequestCachePut(context.Background(), &gocdnextv1.RequestCachePutRequest{
		SessionId: resp.SessionId,
		RunId:     runID.String(),
		JobId:     jobID.String(),
		Key:       "pnpm-store",
	})
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("code = %s, want PermissionDenied", code)
	}
}

// TestRequestCacheGet_RejectsMissingKey: the agent path passes
// the YAML-declared key straight through — empty means the wire
// got corrupted or the parser let a bad manifest through. Either
// way, refuse rather than masking the bug with a miss.
func TestRequestCacheGet_RejectsMissingKey(t *testing.T) {
	pool, client, _ := bootServerWithArtifacts(t)
	runID, jobID, _ := seedDispatchedJob(t, pool, "cache-badkey")

	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "cache-badkey", Token: "tok-art",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	_, err = client.RequestCacheGet(context.Background(), &gocdnextv1.RequestCacheGetRequest{
		SessionId: resp.SessionId,
		RunId:     runID.String(),
		JobId:     jobID.String(),
		Key:       "",
	})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", code)
	}
}

// TestMarkCacheReady_UnknownCacheID: if the eviction sweeper
// deletes a pending row between Put and MarkReady, the agent's
// saved cache_id is stale — surface NotFound instead of Internal
// so the runner can log-and-skip rather than fail the job.
func TestMarkCacheReady_UnknownCacheID(t *testing.T) {
	pool, client, _ := bootServerWithArtifacts(t)
	_, _, _ = seedDispatchedJob(t, pool, "cache-ghost")

	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "cache-ghost", Token: "tok-art",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	_, err = client.MarkCacheReady(context.Background(), &gocdnextv1.MarkCacheReadyRequest{
		SessionId:     resp.SessionId,
		CacheId:       "11111111-1111-1111-1111-111111111111",
		SizeBytes:     1,
		ContentSha256: "x",
	})
	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("code = %s, want NotFound", code)
	}
}

// TestCacheRPCs_UnconfiguredBackend: every cache RPC must refuse
// with Unimplemented when the server was started without a
// WithArtifactStore call — the same flag the upload path checks.
func TestCacheRPCs_UnconfiguredBackend(t *testing.T) {
	pool, client := bootServer(t)
	runID, jobID, _ := seedDispatchedJob(t, pool, "cache-noart")

	resp, err := client.Register(context.Background(), &gocdnextv1.RegisterRequest{
		AgentId: "cache-noart", Token: "tok-art",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	_, err = client.RequestCacheGet(context.Background(), &gocdnextv1.RequestCacheGetRequest{
		SessionId: resp.SessionId, RunId: runID.String(), JobId: jobID.String(), Key: "k",
	})
	if code := status.Code(err); code != codes.Unimplemented {
		t.Errorf("get code = %s, want Unimplemented", code)
	}
	_, err = client.RequestCachePut(context.Background(), &gocdnextv1.RequestCachePutRequest{
		SessionId: resp.SessionId, RunId: runID.String(), JobId: jobID.String(), Key: "k",
	})
	if code := status.Code(err); code != codes.Unimplemented {
		t.Errorf("put code = %s, want Unimplemented", code)
	}
}
