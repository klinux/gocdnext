package artifacts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func mustFS(t *testing.T) *FilesystemStore {
	t.Helper()
	fs, err := NewFilesystemStore(t.TempDir(), "http://localhost:8153", mustSigner(t))
	if err != nil {
		t.Fatalf("fs: %v", err)
	}
	return fs
}

func TestFilesystem_PutGetHeadDelete(t *testing.T) {
	fs := mustFS(t)
	ctx := context.Background()

	key := "run/abc/job/def/blob"
	payload := []byte("hello artifact world")

	n, err := fs.Put(ctx, key, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("put size = %d, want %d", n, len(payload))
	}

	size, err := fs.Head(ctx, key)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if size != int64(len(payload)) {
		t.Errorf("head size = %d, want %d", size, len(payload))
	}

	rc, err := fs.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Errorf("get payload mismatch")
	}

	if err := fs.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := fs.Head(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("head after delete: want ErrNotFound, got %v", err)
	}
}

func TestFilesystem_Delete_Missing_IsNoop(t *testing.T) {
	fs := mustFS(t)
	if err := fs.Delete(context.Background(), "never/existed"); err != nil {
		t.Errorf("delete missing: want nil, got %v", err)
	}
}

func TestFilesystem_Get_Missing(t *testing.T) {
	fs := mustFS(t)
	_, err := fs.Get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestFilesystem_SignedPutURL_Format(t *testing.T) {
	fs := mustFS(t)
	su, err := fs.SignedPutURL(context.Background(), "obj/1", time.Minute)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !strings.HasPrefix(su.URL, "http://localhost:8153/artifacts/") {
		t.Errorf("url prefix: got %q", su.URL)
	}
	if time.Until(su.ExpiresAt) <= 0 {
		t.Errorf("expires must be in the future: %v", su.ExpiresAt)
	}
}

func TestFilesystem_SignedGetURL_AppendsFilenameHint(t *testing.T) {
	// WithContentDisposition must survive the filesystem backend's
	// URL shape — the signer lives in the PATH so we can append a
	// plain `?filename=…` query without invalidating anything. The
	// handler reads this param and turns it into Content-Disposition
	// so the user's browser saves the blob with a useful name.
	fs := mustFS(t)
	ctx := context.Background()

	plain, err := fs.SignedGetURL(ctx, "obj/2", time.Minute)
	if err != nil {
		t.Fatalf("sign plain: %v", err)
	}
	if strings.Contains(plain.URL, "filename=") {
		t.Errorf("plain get URL shouldn't carry filename: %q", plain.URL)
	}

	hinted, err := fs.SignedGetURL(ctx, "obj/2", time.Minute,
		WithContentDisposition("gocdnext-server.tar.gz"))
	if err != nil {
		t.Fatalf("sign hinted: %v", err)
	}
	if !strings.Contains(hinted.URL, "?filename=gocdnext-server.tar.gz") {
		t.Errorf("hinted URL missing filename param: %q", hinted.URL)
	}
}

func TestFilesystem_PathTraversal_Rejected(t *testing.T) {
	fs := mustFS(t)
	ctx := context.Background()

	for _, bad := range []string{
		"../etc/passwd",
		"/etc/passwd",
		"",
		"..",
		"a/../../etc/passwd",
	} {
		_, err := fs.Put(ctx, bad, bytes.NewReader([]byte("x")))
		if err == nil {
			t.Errorf("put(%q): expected error", bad)
		}
	}
}

func TestFilesystem_AtomicPut(t *testing.T) {
	// Failed copy must not leave a visible partial file.
	fs := mustFS(t)
	ctx := context.Background()
	key := "atomic/one"

	_, err := fs.Put(ctx, key, iotest.ErrReader(errors.New("boom")))
	if err == nil {
		t.Fatal("expected error")
	}
	if _, err := fs.Head(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("partial file must not exist; head err = %v", err)
	}
}
