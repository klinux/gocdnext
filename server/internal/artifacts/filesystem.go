package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FilesystemStore writes blobs under `root`, one file per storage_key.
// Keys are stored verbatim as the relative path (sanitised against path
// traversal). Signed URLs are served by the gocdnext-server itself at
// `publicBase + "/artifacts/" + token` — see handler.go for the matching
// mux route.
type FilesystemStore struct {
	root       string
	publicBase string
	signer     *Signer
}

// NewFilesystemStore creates the root dir if missing. publicBase is the
// externally-visible URL of the server (e.g. "http://localhost:8153")
// and is prepended to signed URLs so agents know where to PUT/GET.
func NewFilesystemStore(root, publicBase string, signer *Signer) (*FilesystemStore, error) {
	if signer == nil {
		return nil, errors.New("artifacts: filesystem: signer is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("artifacts: filesystem: abs(%s): %w", root, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("artifacts: filesystem: mkdir %s: %w", abs, err)
	}
	return &FilesystemStore{
		root:       abs,
		publicBase: strings.TrimRight(publicBase, "/"),
		signer:     signer,
	}, nil
}

// resolve validates a storage_key + joins it with the root, refusing any
// attempt to escape the root directory via ".." or absolute paths.
func (f *FilesystemStore) resolve(key string) (string, error) {
	if key == "" {
		return "", errors.New("artifacts: filesystem: empty key")
	}
	if strings.HasPrefix(key, "/") || strings.HasPrefix(key, "\\") {
		return "", errors.New("artifacts: filesystem: absolute key rejected")
	}
	// Reject any segment equal to ".." outright — don't rely on Clean
	// to normalise, since "/" + "../etc" resolves to "/etc" which would
	// silently pass the rel-under-root check below.
	for _, seg := range strings.Split(filepath.ToSlash(key), "/") {
		if seg == ".." {
			return "", errors.New("artifacts: filesystem: key contains '..'")
		}
	}
	clean := filepath.Clean(key)
	if clean == "." || clean == "/" {
		return "", errors.New("artifacts: filesystem: invalid key")
	}
	full := filepath.Join(f.root, clean)
	rel, err := filepath.Rel(f.root, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", errors.New("artifacts: filesystem: key escapes root")
	}
	return full, nil
}

func (f *FilesystemStore) SignedPutURL(_ context.Context, key string, ttl time.Duration) (SignedURL, error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	exp := time.Now().Add(ttl)
	tok := f.signer.Sign(key, VerbPUT, exp)
	return SignedURL{
		URL:       f.publicBase + "/artifacts/" + url.PathEscape(tok),
		ExpiresAt: exp,
	}, nil
}

func (f *FilesystemStore) SignedGetURL(_ context.Context, key string, ttl time.Duration, opts ...GetOption) (SignedURL, error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	exp := time.Now().Add(ttl)
	tok := f.signer.Sign(key, VerbGET, exp)
	u := f.publicBase + "/artifacts/" + url.PathEscape(tok)
	// Filename hint is plain query concatenation — the signed token
	// sits in the PATH, so appending a query param doesn't invalidate
	// any signature (contrast S3/GCS which must bake this into the
	// presigned URL before signing).
	if req := ResolveGetOptions(opts); req.Filename != "" {
		u += "?filename=" + url.QueryEscape(req.Filename)
	}
	return SignedURL{URL: u, ExpiresAt: exp}, nil
}

func (f *FilesystemStore) Head(_ context.Context, key string) (int64, error) {
	full, err := f.resolve(key)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(full)
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("artifacts: filesystem: stat: %w", err)
	}
	return fi.Size(), nil
}

func (f *FilesystemStore) Delete(_ context.Context, key string) error {
	full, err := f.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("artifacts: filesystem: delete: %w", err)
	}
	return nil
}

func (f *FilesystemStore) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	full, err := f.resolve(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return 0, fmt.Errorf("artifacts: filesystem: mkdir: %w", err)
	}
	// Write to tmp, rename — avoids a half-written file becoming visible.
	tmp := full + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, fmt.Errorf("artifacts: filesystem: create: %w", err)
	}
	n, copyErr := io.Copy(out, r)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("artifacts: filesystem: write: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("artifacts: filesystem: close: %w", closeErr)
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("artifacts: filesystem: rename: %w", err)
	}
	return n, nil
}

func (f *FilesystemStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	full, err := f.resolve(key)
	if err != nil {
		return nil, err
	}
	in, err := os.Open(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("artifacts: filesystem: open: %w", err)
	}
	return in, nil
}

// Signer exposes the backend's signer so callers (e.g. the HTTP handler)
// can verify tokens. Only the filesystem backend needs this; S3/GCS
// verify their own URLs server-side of the cloud provider.
func (f *FilesystemStore) SignerRef() *Signer { return f.signer }
