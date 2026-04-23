package grpcsrv

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// RequestCacheGet resolves the session → project, looks up the
// ready cache row for (project, key), and mints a signed GET URL
// the agent can pull the tarball from. A miss (no row, or row
// still pending an upload) returns `found=false` instead of an
// error — first-run pipelines pre-populate nothing and that's
// fine, not a bug.
func (a *AgentService) RequestCacheGet(
	ctx context.Context, req *gocdnextv1.RequestCacheGetRequest,
) (*gocdnextv1.RequestCacheGetResponse, error) {
	sess, projectID, err := a.authCacheSession(ctx, req.GetSessionId(), req.GetRunId(), req.GetJobId())
	if err != nil {
		return nil, err
	}
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "cache key is required")
	}
	if a.artifactStore == nil {
		return nil, status.Error(codes.Unimplemented, "artifact backend not configured")
	}

	c, err := a.store.GetReadyCacheByKey(ctx, projectID, req.GetKey())
	if errors.Is(err, store.ErrCacheNotFound) {
		// Expected on cold starts and while another agent is
		// uploading. Signal miss, agent proceeds without a pre-
		// populated cache dir.
		return &gocdnextv1.RequestCacheGetResponse{Found: false}, nil
	}
	if err != nil {
		a.log.Error("cache get: lookup failed", "err", err, "session", sess.ID, "key", req.GetKey())
		return nil, status.Error(codes.Internal, "lookup cache")
	}

	signed, err := a.artifactStore.SignedGetURL(ctx, c.StorageKey, a.artifactGetURLTTL)
	if err != nil {
		a.log.Error("cache get: sign failed", "err", err, "storage_key", c.StorageKey)
		return nil, status.Error(codes.Internal, "sign url")
	}
	return &gocdnextv1.RequestCacheGetResponse{
		Found:         true,
		GetUrl:        signed.URL,
		SizeBytes:     c.SizeBytes,
		ContentSha256: c.ContentSHA256,
		ExpiresAt:     timestamppb.New(signed.ExpiresAt),
	}, nil
}

// RequestCachePut mints a signed PUT URL the agent uploads the
// cache tarball at. The row goes pending; the agent calls
// MarkCacheReady after the upload confirms, making the cache
// visible to subsequent GETs. Replacing an existing cache keeps
// the same storage_key — the blob backend handles overwrite in
// place, so we don't need a cleanup pass for the old object.
func (a *AgentService) RequestCachePut(
	ctx context.Context, req *gocdnextv1.RequestCachePutRequest,
) (*gocdnextv1.RequestCachePutResponse, error) {
	sess, projectID, err := a.authCacheSession(ctx, req.GetSessionId(), req.GetRunId(), req.GetJobId())
	if err != nil {
		return nil, err
	}
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "cache key is required")
	}
	if a.artifactStore == nil {
		return nil, status.Error(codes.Unimplemented, "artifact backend not configured")
	}

	c, err := a.store.UpsertPendingCache(ctx, projectID, req.GetKey())
	if err != nil {
		a.log.Error("cache put: upsert failed", "err", err, "session", sess.ID, "key", req.GetKey())
		return nil, status.Error(codes.Internal, "persist cache")
	}

	signed, err := a.artifactStore.SignedPutURL(ctx, c.StorageKey, a.artifactPutURLTTL)
	if err != nil {
		a.log.Error("cache put: sign failed", "err", err, "storage_key", c.StorageKey)
		return nil, status.Error(codes.Internal, "sign url")
	}
	a.log.Info("cache put ticket issued",
		"session", sess.ID, "project", projectID, "key", req.GetKey(),
		"cache_id", c.ID, "storage_key", c.StorageKey)
	return &gocdnextv1.RequestCachePutResponse{
		CacheId:    c.ID.String(),
		PutUrl:     signed.URL,
		StorageKey: c.StorageKey,
		ExpiresAt:  timestamppb.New(signed.ExpiresAt),
	}, nil
}

// MarkCacheReady finalises an upload. After this RPC lands a
// concurrent RequestCacheGet returns the signed URL instead of a
// miss. Idempotent: re-calling on a ready row is a cheap noop
// (the UPDATE simply refreshes the metadata).
func (a *AgentService) MarkCacheReady(
	ctx context.Context, req *gocdnextv1.MarkCacheReadyRequest,
) (*gocdnextv1.MarkCacheReadyResponse, error) {
	if req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}
	if _, ok := a.sessions.Lookup(req.GetSessionId()); !ok {
		return nil, status.Error(codes.Unauthenticated, "invalid session")
	}
	if req.GetCacheId() == "" {
		return nil, status.Error(codes.InvalidArgument, "cache_id is required")
	}
	cacheID, err := uuid.Parse(req.GetCacheId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed cache_id")
	}
	if err := a.store.MarkCacheReady(ctx, cacheID, req.GetSizeBytes(), req.GetContentSha256()); err != nil {
		if errors.Is(err, store.ErrCacheNotFound) {
			return nil, status.Error(codes.NotFound, "cache id not found")
		}
		a.log.Error("cache ready: mark failed", "err", err, "cache_id", cacheID)
		return nil, status.Error(codes.Internal, "mark ready")
	}
	return &gocdnextv1.MarkCacheReadyResponse{}, nil
}

// authCacheSession is the shared prelude the three cache RPCs use:
// verify the session exists, parse run_id + job_id, and resolve the
// owning project via JobRunParents so we don't trust client-supplied
// project ids. Reuses the ownership guard from the artifact upload
// path (a session can only act on jobs assigned to its agent).
func (a *AgentService) authCacheSession(
	ctx context.Context, sessionID, runIDStr, jobIDStr string,
) (*Session, uuid.UUID, error) {
	if sessionID == "" {
		return nil, uuid.Nil, status.Error(codes.InvalidArgument, "session_id is required")
	}
	if runIDStr == "" || jobIDStr == "" {
		return nil, uuid.Nil, status.Error(codes.InvalidArgument, "run_id and job_id are required")
	}
	sess, ok := a.sessions.Lookup(sessionID)
	if !ok {
		return nil, uuid.Nil, status.Error(codes.Unauthenticated, "invalid session")
	}
	runID, err := uuid.Parse(runIDStr)
	if err != nil {
		return nil, uuid.Nil, status.Error(codes.InvalidArgument, "malformed run_id")
	}
	jobRunID, err := uuid.Parse(jobIDStr)
	if err != nil {
		return nil, uuid.Nil, status.Error(codes.InvalidArgument, "malformed job_id")
	}
	_, projectID, ownerAgentID, err := a.store.JobRunParents(ctx, jobRunID, runID)
	if errors.Is(err, store.ErrArtifactNotFound) {
		return nil, uuid.Nil, status.Error(codes.NotFound, "job/run not found")
	}
	if err != nil {
		a.log.Error("cache auth: parents lookup failed", "err", err)
		return nil, uuid.Nil, status.Error(codes.Internal, "internal error")
	}
	if ownerAgentID == uuid.Nil || ownerAgentID != sess.AgentID {
		a.log.Warn("cache auth: session agent does not own job",
			"session_agent", sess.AgentID, "job_agent", ownerAgentID, "job_id", jobRunID)
		return nil, uuid.Nil, status.Error(codes.PermissionDenied, "job not owned by session")
	}
	return sess, projectID, nil
}
