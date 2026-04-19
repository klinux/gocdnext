package runner

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// ArtifactUploader is how the runner ships job outputs back to the
// server. Implementations handle both the gRPC dance
// (RequestArtifactUpload) and the HTTP PUT(s). The runner is ignorant
// of which backend is configured — it hands paths in, gets ArtifactRef
// entries out, and attaches them to JobResult.
type ArtifactUploader interface {
	Upload(ctx context.Context, workDir, runID, jobID string, paths []string) ([]*gocdnextv1.ArtifactRef, error)
}

// TarGzPath writes a gzip-compressed tar of `path` (file or dir,
// relative to workDir) into `dst`, streaming, while computing sha256
// and total bytes. Returns the sha (hex) and written size. Used by the
// concrete uploader but exported so tests / debug tooling can invoke it
// directly.
func TarGzPath(workDir, path string, dst io.Writer) (sha string, size int64, err error) {
	abs := filepath.Join(workDir, path)
	info, err := os.Stat(abs)
	if err != nil {
		return "", 0, fmt.Errorf("artifact: stat %q: %w", path, err)
	}

	hasher := sha256.New()
	counter := &countingWriter{}
	mw := io.MultiWriter(dst, hasher, counter)

	gz := gzip.NewWriter(mw)
	tw := tar.NewWriter(gz)

	addEntry := func(abs, rel string, fi os.FileInfo) error {
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return fmt.Errorf("artifact: header %q: %w", rel, err)
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("artifact: write header %q: %w", rel, err)
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(abs)
		if err != nil {
			return fmt.Errorf("artifact: open %q: %w", rel, err)
		}
		defer func() { _ = in.Close() }()
		if _, err := io.Copy(tw, in); err != nil {
			return fmt.Errorf("artifact: copy %q: %w", rel, err)
		}
		return nil
	}

	if info.Mode().IsRegular() {
		// Single file: store under its basename so untar drops it next
		// to whatever extracts it.
		if err := addEntry(abs, filepath.Base(path), info); err != nil {
			return "", 0, err
		}
	} else if info.IsDir() {
		root := abs
		err := filepath.Walk(root, func(p string, fi os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			// Skip the top-level "." entry (tar doesn't need it).
			if rel == "." {
				return nil
			}
			// Store paths relative to `path` so `artifacts: [bin/]`
			// unpacks cleanly. Convert to forward slashes for tar
			// portability.
			name := filepath.Join(filepath.Base(path), rel)
			return addEntry(p, strings.ReplaceAll(name, string(filepath.Separator), "/"), fi)
		})
		if err != nil {
			return "", 0, err
		}
	} else {
		return "", 0, fmt.Errorf("artifact: %q is neither file nor directory", path)
	}

	if err := tw.Close(); err != nil {
		return "", 0, fmt.Errorf("artifact: close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", 0, fmt.Errorf("artifact: close gzip: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), counter.n, nil
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
