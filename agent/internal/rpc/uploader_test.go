package rpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// stubAgentClient implements just enough of gocdnextv1.AgentServiceClient
// for the uploader's RequestArtifactUpload + PUT path to round-trip.
// Records the last request so the test can assert what hit the wire.
type stubAgentClient struct {
	gocdnextv1.AgentServiceClient // embed so methods we don't implement compile-error if called
	lastRequest                   *gocdnextv1.RequestArtifactUploadRequest
	tickets                       []*gocdnextv1.ArtifactUploadTicket
}

func (s *stubAgentClient) RequestArtifactUpload(
	_ context.Context,
	in *gocdnextv1.RequestArtifactUploadRequest,
	_ ...grpc.CallOption,
) (*gocdnextv1.RequestArtifactUploadResponse, error) {
	s.lastRequest = in
	return &gocdnextv1.RequestArtifactUploadResponse{Tickets: s.tickets}, nil
}

// TestUpload_DedupesPathsBeforeRPC — round-2 fix for the
// server/agent contract mismatch: the server now dedupes paths
// before issuing tickets (migration 00035 partial unique index
// requires it), so the agent MUST send canonical-deduped paths
// or the `len(tickets) != len(paths)` validation rejects the
// server's perfectly-valid response.
//
// Asserts:
//   (1) The RPC carries the deduped path list, not the raw one.
//   (2) First-occurrence shape survives (the operator wrote `dist`
//       first, so `dist` reaches the server even though `dist/`
//       came right after).
//   (3) The length check uses the deduped count, so a server that
//       returns N tickets for N unique paths is accepted.
func TestUpload_DedupesPathsBeforeRPC(t *testing.T) {
	// Prep a workspace with the files the tar step needs.
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "dist"), 0o755); err != nil {
		t.Fatalf("mkdir dist: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "dist", "out"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write dist/out: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "coverage.out"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write coverage: %v", err)
	}

	// Stub PUT endpoint — accepts any body, returns 200.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = r.Body.Read(make([]byte, 1024))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stub := &stubAgentClient{
		tickets: []*gocdnextv1.ArtifactUploadTicket{
			{Path: "dist", StorageKey: "k1", PutUrl: srv.URL + "/dist", ExpiresAt: timestamppb.Now()},
			{Path: "coverage.out", StorageKey: "k2", PutUrl: srv.URL + "/cov", ExpiresAt: timestamppb.Now()},
		},
	}
	u := NewArtifactUploader(stub, "sess", srv.Client())

	// Duplicate `dist` in three different shapes; coverage.out unique.
	refs, err := u.Upload(context.Background(), workDir, "run-1", "job-1",
		[]string{"dist", "dist/", "dist", "coverage.out"})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %d, want 2 (dedupe should collapse the dist variants)", len(refs))
	}

	if stub.lastRequest == nil {
		t.Fatal("stub never received the RPC")
	}
	got := stub.lastRequest.GetPaths()
	if len(got) != 2 {
		t.Fatalf("RPC paths = %d, want 2 (deduped before RPC): %v", len(got), got)
	}
	if got[0] != "dist" {
		t.Errorf("RPC paths[0] = %q, want %q (first-occurrence shape)", got[0], "dist")
	}
	if got[1] != "coverage.out" {
		t.Errorf("RPC paths[1] = %q, want %q", got[1], "coverage.out")
	}
}

// TestUpload_TicketCountMismatchStillSurfacesAsError — sanity check
// that a misbehaving server returning a wrong count (NOT just a
// deduped count) still fails the upload loudly. The validation
// uses len(unique), so a server returning 1 ticket for 2 unique
// paths must be caught.
func TestUpload_TicketCountMismatchStillSurfacesAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stub := &stubAgentClient{
		tickets: []*gocdnextv1.ArtifactUploadTicket{
			{Path: "dist", StorageKey: "k1", PutUrl: srv.URL, ExpiresAt: timestamppb.Now()},
			// Missing ticket for `other` — server bug.
		},
	}
	u := NewArtifactUploader(stub, "sess", srv.Client())
	_, err := u.Upload(context.Background(), t.TempDir(), "run-1", "job-1",
		[]string{"dist", "other"})
	if err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "1 tickets for 2 paths") {
		t.Fatalf("err = %v, want '1 tickets for 2 paths'", err)
	}
}
