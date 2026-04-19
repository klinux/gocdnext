package artifacts_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"

	"github.com/gocdnext/gocdnext/server/internal/artifacts"
)

// Shared LocalStack container per test binary — start is ~8s, so we
// don't pay it per-test.
var (
	lsOnce sync.Once
	lsCtr  *localstack.LocalStackContainer
	lsURL  string
	lsErr  error
)

func localStack(t *testing.T) string {
	t.Helper()
	lsOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		ctr, err := localstack.Run(ctx, "localstack/localstack:3.8",
			testcontainers.WithEnv(map[string]string{"SERVICES": "s3"}),
		)
		if err != nil {
			lsErr = err
			return
		}
		// Port 4566 is the unified LocalStack edge.
		host, err := ctr.Host(ctx)
		if err != nil {
			lsErr = err
			return
		}
		port, err := ctr.MappedPort(ctx, "4566/tcp")
		if err != nil {
			lsErr = err
			return
		}
		lsCtr = ctr
		lsURL = "http://" + host + ":" + port.Port()
	})
	if lsErr != nil {
		t.Skipf("localstack unavailable: %v", lsErr)
	}
	return lsURL
}

func newS3Store(t *testing.T, endpoint, bucket string) *artifacts.S3Store {
	t.Helper()
	store, err := artifacts.NewS3Store(context.Background(), artifacts.S3Config{
		Bucket:       bucket,
		Region:       "us-east-1",
		Endpoint:     endpoint,
		AccessKey:    "test",
		SecretKey:    "test",
		UsePathStyle: true, // LocalStack needs this
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	if err := store.EnsureBucket(context.Background(), "us-east-1"); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	return store
}

func TestS3_PutHeadGetDelete(t *testing.T) {
	endpoint := localStack(t)
	s := newS3Store(t, endpoint, "artifact-crud")
	ctx := context.Background()

	key := "run/abc/job/def/blob"
	payload := []byte("hello s3 artifact")

	n, err := s.Put(ctx, key, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("put size = %d", n)
	}

	size, err := s.Head(ctx, key)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if size != int64(len(payload)) {
		t.Errorf("head size = %d", size)
	}

	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Errorf("get mismatch")
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Head(ctx, key); !errors.Is(err, artifacts.ErrNotFound) {
		t.Errorf("head after delete: want ErrNotFound, got %v", err)
	}
}

func TestS3_Head_Missing(t *testing.T) {
	endpoint := localStack(t)
	s := newS3Store(t, endpoint, "artifact-missing")

	_, err := s.Head(context.Background(), "never-uploaded")
	if !errors.Is(err, artifacts.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestS3_Delete_Missing_IsNoop(t *testing.T) {
	endpoint := localStack(t)
	s := newS3Store(t, endpoint, "artifact-del-missing")

	if err := s.Delete(context.Background(), "ghost"); err != nil {
		t.Errorf("delete of missing: got %v, want nil", err)
	}
}

func TestS3_SignedPut_Works(t *testing.T) {
	endpoint := localStack(t)
	s := newS3Store(t, endpoint, "artifact-signed-put")
	ctx := context.Background()

	key := "signed/one"
	su, err := s.SignedPutURL(ctx, key, time.Minute)
	if err != nil {
		t.Fatalf("sign put: %v", err)
	}
	if !strings.Contains(su.URL, "X-Amz-Signature") {
		t.Errorf("url missing signature: %s", su.URL)
	}

	body := []byte("signed put payload")
	req, _ := http.NewRequest(http.MethodPut, su.URL, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put via signed url: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("PUT status = %d", resp.StatusCode)
	}

	size, err := s.Head(ctx, key)
	if err != nil {
		t.Fatalf("head after signed put: %v", err)
	}
	if size != int64(len(body)) {
		t.Errorf("size = %d, want %d", size, len(body))
	}
}

func TestS3_SignedGet_Works(t *testing.T) {
	endpoint := localStack(t)
	s := newS3Store(t, endpoint, "artifact-signed-get")
	ctx := context.Background()

	key := "signed/get"
	body := []byte("signed get payload")
	if _, err := s.Put(ctx, key, bytes.NewReader(body)); err != nil {
		t.Fatalf("put: %v", err)
	}

	su, err := s.SignedGetURL(ctx, key, time.Minute)
	if err != nil {
		t.Fatalf("sign get: %v", err)
	}
	resp, err := http.Get(su.URL)
	if err != nil {
		t.Fatalf("get via signed url: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("body mismatch")
	}
}
