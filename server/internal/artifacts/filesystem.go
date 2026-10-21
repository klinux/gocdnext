package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PruneByPrefixAge deletes files under <root>/<prefix> whose modtime is older
// than olderThan, returning the count + bytes freed. The embedded build cache
// (gradle-cache/) stores immutable objects that no DB row tracks, so the
// retention sweeper calls this to bound growth on the FILESYSTEM backend (S3
// and GCS use native object-lifecycle rules instead). It walks the subtree each
// call — acceptable for the filesystem backend's scale (dev / small self-
// hosted). A missing prefix (nothing cached yet) is not an error.
func (f *FilesystemStore) PruneByPrefixAge(ctx context.Context, prefix string, olderThan time.Duration) (int, int64, error) {
	base := filepath.Join(f.root, filepath.Clean(prefix))
	cutoff := time.Now().Add(-olderThan)
	var count, failures int
	var freed int64
	var firstErr error
	note := func(err error) {
		failures++
		if firstErr == nil {
			firstErr = err
		}
	}
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			if !os.IsNotExist(infoErr) { // a racing delete is fine; anything else isn't
				note(infoErr)
			}
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if rmErr := os.Remove(path); rmErr != nil {
				if !os.IsNotExist(rmErr) {
					note(rmErr) // e.g. a permission problem would silently stall eviction
				}
				return nil
			}
			count++
			freed += info.Size()
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return count, freed, fmt.Errorf("artifacts: filesystem: prune %s: %w", prefix, err)
	}
	if firstErr != nil {
		return count, freed, fmt.Errorf("artifacts: filesystem: prune %s: %d of its entries failed (first: %w)", prefix, failures, firstErr)
	}
	return count, freed, nil
}

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

func (f *FilesystemStore) SignedPutURL(_ context.Context, key string, ttl time.Duration, _ ...PutOption) (SignedURL, error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	exp := time.Now().Add(ttl)
	tok := f.signer.Sign(key, VerbPUT, exp)
	// WithCreateOnly is a no-op here: the HTTP handler already routes
	// non-`cache/` keys through PutCreateOnly (handler.go), so filesystem
	// artifacts are write-once regardless of the option. No extra header is
	// needed — the create-only enforcement lives server-side, not in the URL.
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
	// Write to a UNIQUE temp, then atomic rename — a per-call temp name is
	// required because the build-cache proxy can PUT the same key concurrently
	// (two builds computing the same entry); a shared "<key>.part" would let
	// those interleave into a corrupt file. The rename is atomic, so last writer
	// wins with intact bytes (fine for content-addressed entries).
	out, err := os.CreateTemp(filepath.Dir(full), "."+filepath.Base(full)+".*.part")
	if err != nil {
		return 0, fmt.Errorf("artifacts: filesystem: create: %w", err)
	}
	tmp := out.Name()
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

// PutCreateOnly stores immutable artifacts without replacing an existing
// object. A retry with identical bytes is accepted as success; a retry or
// replay with different bytes is rejected so a signed PUT URL cannot swap
// a ready artifact on the filesystem backend.
func (f *FilesystemStore) PutCreateOnly(_ context.Context, key string, r io.Reader) (int64, error) {
	full, err := f.resolve(key)
	if err != nil {
		return 0, err
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("artifacts: filesystem: mkdir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(full)+".*.part")
	if err != nil {
		return 0, fmt.Errorf("artifacts: filesystem: create tmp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	n, copyErr := io.Copy(tmp, r)
	closeErr := tmp.Close()
	if copyErr != nil {
		return 0, fmt.Errorf("artifacts: filesystem: write: %w", copyErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("artifacts: filesystem: close: %w", closeErr)
	}

	if err := os.Link(tmpName, full); err != nil {
		if errors.Is(err, os.ErrExist) {
			same, cmpErr := sameFileContent(tmpName, full)
			if cmpErr != nil {
				return 0, cmpErr
			}
			if same {
				return n, nil
			}
			return 0, ErrAlreadyExists
		}
		return 0, fmt.Errorf("artifacts: filesystem: link: %w", err)
	}
	return n, nil
}

func sameFileContent(a, b string) (bool, error) {
	ainfo, err := fileInfo(a)
	if err != nil {
		return false, err
	}
	binfo, err := fileInfo(b)
	if err != nil {
		return false, err
	}
	return ainfo == binfo, nil
}

func fileInfo(path string) (ObjectInfo, error) {
	in, err := os.Open(path)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("artifacts: filesystem: open compare: %w", err)
	}
	defer func() { _ = in.Close() }()

	h := sha256.New()
	n, err := io.Copy(h, in)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("artifacts: filesystem: read compare: %w", err)
	}
	return ObjectInfo{Size: n, ContentSHA256: hex.EncodeToString(h.Sum(nil))}, nil
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
