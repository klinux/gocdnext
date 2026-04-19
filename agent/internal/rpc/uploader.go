package rpc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/agent/internal/runner"
)

// ArtifactUploader implements runner.ArtifactUploader by calling
// RequestArtifactUpload on the gRPC client and then streaming the
// tar+gz of each path to the returned PUT URL over HTTP.
//
// Two-stage flow per path:
//  1. tar+gz to a temp file in order to know the Content-Length up
//     front (S3/GCS refuse chunked uploads); compute sha256 during
//     the write for free.
//  2. PUT the temp file with Content-Length set; delete on completion.
type ArtifactUploader struct {
	client    gocdnextv1.AgentServiceClient
	sessionID string
	http      *http.Client
}

// NewArtifactUploader wires the concrete uploader. http is optional;
// nil uses a default client with a 30-min timeout (big binaries take
// real time).
func NewArtifactUploader(client gocdnextv1.AgentServiceClient, sessionID string, httpClient *http.Client) *ArtifactUploader {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Minute}
	}
	return &ArtifactUploader{client: client, sessionID: sessionID, http: httpClient}
}

// Upload implements runner.ArtifactUploader.
func (u *ArtifactUploader) Upload(ctx context.Context, workDir, runID, jobID string, paths []string) ([]*gocdnextv1.ArtifactRef, error) {
	if len(paths) == 0 {
		return nil, nil
	}

	resp, err := u.client.RequestArtifactUpload(ctx, &gocdnextv1.RequestArtifactUploadRequest{
		SessionId: u.sessionID,
		RunId:     runID,
		JobId:     jobID,
		Paths:     paths,
	})
	if err != nil {
		return nil, fmt.Errorf("request upload: %w", err)
	}
	if got := len(resp.GetTickets()); got != len(paths) {
		return nil, fmt.Errorf("server returned %d tickets for %d paths", got, len(paths))
	}

	refs := make([]*gocdnextv1.ArtifactRef, 0, len(paths))
	for _, tkt := range resp.GetTickets() {
		ref, err := u.uploadOne(ctx, workDir, tkt)
		if err != nil {
			// Partial success returns what succeeded; caller decides
			// whether a missing ref is fatal. For now the runner logs
			// the error and continues (artifacts are best-effort).
			return refs, fmt.Errorf("upload %q: %w", tkt.GetPath(), err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func (u *ArtifactUploader) uploadOne(ctx context.Context, workDir string, tkt *gocdnextv1.ArtifactUploadTicket) (*gocdnextv1.ArtifactRef, error) {
	tmp, err := os.CreateTemp("", "gocdnext-artifact-*.tar.gz")
	if err != nil {
		return nil, fmt.Errorf("tempfile: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	sha, size, err := runner.TarGzPath(workDir, tkt.GetPath(), tmp)
	if cerr := tmp.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("tar: %w", err)
	}

	body, err := os.Open(tmpName)
	if err != nil {
		return nil, fmt.Errorf("open tar: %w", err)
	}
	defer func() { _ = body.Close() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, tkt.GetPutUrl(), body)
	if err != nil {
		return nil, fmt.Errorf("build PUT: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/gzip")

	resp, err := u.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http PUT: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("PUT returned %s", resp.Status)
	}
	return &gocdnextv1.ArtifactRef{
		Path:          tkt.GetPath(),
		StorageKey:    tkt.GetStorageKey(),
		Size:          size,
		ContentSha256: sha,
	}, nil
}
