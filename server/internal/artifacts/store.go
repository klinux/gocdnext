// Package artifacts is the server-side storage abstraction for pipeline
// artefacts (binaries, reports, coverage files, etc.). Implementations
// live next to this file: `filesystem.go` is the default; `s3.go` and
// `gcs.go` arrive in E2b.
//
// All backends return opaque URL + verb pairs so the agent never learns
// which backend is configured. For filesystem this is an HTTP endpoint
// served by the gocdnext-server itself (see handler.go); for S3/GCS the
// SDKs return a native pre-signed URL that bypasses the server.
package artifacts

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned by Head/Get/Delete when a storage_key does not
// exist in the backend. Callers treat it as non-fatal when building a
// sweeper (stale row is the same as missing object).
var ErrNotFound = errors.New("artifacts: object not found")

// Store is what the rest of the server uses. Concrete backends implement
// it; the scheduler/handler/sweeper consume it through this interface only.
type Store interface {
	// SignedPutURL returns a URL + expiry that the agent can PUT bytes to
	// for the given storage_key. TTL is advisory; backends may enforce
	// their own minimum.
	SignedPutURL(ctx context.Context, key string, ttl time.Duration) (SignedURL, error)

	// SignedGetURL returns a URL + expiry that the agent can GET bytes
	// from for a previously uploaded storage_key.
	SignedGetURL(ctx context.Context, key string, ttl time.Duration) (SignedURL, error)

	// Head returns the object's size in bytes and an existence bool.
	// Returns ErrNotFound if the backend has no record.
	Head(ctx context.Context, key string) (size int64, err error)

	// Delete removes the object. Returns nil (not ErrNotFound) if the
	// object was already gone — sweeper retries need idempotent delete.
	Delete(ctx context.Context, key string) error

	// Put is a direct write path, used by tests and by the filesystem
	// HTTP handler. Production upload flows use SignedPutURL + agent PUT.
	Put(ctx context.Context, key string, r io.Reader) (size int64, err error)

	// Get returns a reader for the object. The caller must Close.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// SignedURL is what SignedPutURL / SignedGetURL return. The URL is what
// the agent hits (with the appropriate verb); ExpiresAt is when the URL
// stops working — servers echo it back to the agent so the agent can
// refresh if a job takes longer than the window.
type SignedURL struct {
	URL       string
	ExpiresAt time.Time
}
