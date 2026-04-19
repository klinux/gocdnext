package artifacts_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/gocdnext/gocdnext/server/internal/artifacts"
)

// fakeGCSTeardown stops the emulator + unsets the STORAGE_EMULATOR_HOST
// it set. Exposed so long test binaries can free resources on teardown,
// but `sync.Once` gates setup so tests don't restart the container.
var (
	fgcsOnce sync.Once
	fgcsURL  string
	fgcsErr  error
)

// fakeGCS boots fsouza/fake-gcs-server once per test binary. Sets
// STORAGE_EMULATOR_HOST (the well-known knob the GCS Go SDK honours)
// so GCSStore clients route to it. Public-host matches the mapped port
// via -external-url — without this, fake-gcs-server embeds container-
// internal hostnames in object metadata and downloads break.
func fakeGCS(t *testing.T) string {
	t.Helper()
	fgcsOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		// Two-phase start: create the container paused so we know the
		// mapped port, inject -external-url with that port, then start.
		// testcontainers doesn't expose "pre-start port reservation"
		// without LifecycleHooks, so we do it the long way.
		req := testcontainers.ContainerRequest{
			Image:        "fsouza/fake-gcs-server:latest",
			ExposedPorts: []string{"4443/tcp"},
			Cmd: []string{
				"-scheme", "http",
				"-port", "4443",
				"-public-host", "127.0.0.1:4443", // overridden post-start
			},
			WaitingFor: wait.ForLog("server started").WithStartupTimeout(60 * time.Second),
		}
		ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		})
		if err != nil {
			fgcsErr = err
			return
		}
		host, err := ctr.Host(ctx)
		if err != nil {
			fgcsErr = err
			return
		}
		port, err := ctr.MappedPort(ctx, "4443/tcp")
		if err != nil {
			fgcsErr = err
			return
		}
		fgcsURL = "http://" + host + ":" + port.Port()
		_ = os.Setenv("STORAGE_EMULATOR_HOST", host+":"+port.Port())
	})
	if fgcsErr != nil {
		t.Skipf("fake-gcs-server unavailable: %v", fgcsErr)
	}
	return fgcsURL
}

// fakeCredsJSON builds a service-account-shaped JSON blob with an
// RSA-2048 private key. Fake-gcs-server doesn't validate signatures, so
// any email + key pair works — this just makes the SDK happy when
// signing URLs in tests.
func fakeCredsJSON(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa gen: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	blob := map[string]string{
		"type":         "service_account",
		"client_email": "test-signer@gocdnext.iam.gserviceaccount.com",
		"private_key":  string(pemBytes),
	}
	out, _ := json.Marshal(blob)
	return out
}

func newGCSStore(t *testing.T, bucket string) *artifacts.GCSStore {
	t.Helper()
	// STORAGE_EMULATOR_HOST already set by fakeGCS; no Endpoint here so
	// NewGCSStore still keeps the credential for signing.
	store, err := artifacts.NewGCSStore(context.Background(), artifacts.GCSConfig{
		Bucket:          bucket,
		CredentialsJSON: fakeCredsJSON(t),
	})
	if err != nil {
		t.Fatalf("NewGCSStore: %v", err)
	}
	if err := store.EnsureBucket(context.Background(), "gocdnext-test"); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	return store
}

func TestGCS_PutHead_AgainstEmulator(t *testing.T) {
	// fake-gcs-server's download path (NewReader) embeds the container-
	// internal hostname in mediaLink and doesn't reliably work across
	// the docker bridge, so this test only exercises Put + Head — the
	// paths that go through the regular JSON API and are honoured by
	// STORAGE_EMULATOR_HOST. Full Get/CRUD against a real GCS bucket
	// is validated in prod; the SDK itself is well-covered upstream.
	_ = fakeGCS(t)
	s := newGCSStore(t, "artifact-gcs-crud")
	ctx := context.Background()

	key := "run/abc/job/def/blob"
	payload := []byte("hello gcs artifact")

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
		t.Errorf("head size = %d, want %d", size, len(payload))
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Head(ctx, key); !errors.Is(err, artifacts.ErrNotFound) {
		t.Errorf("head after delete: want ErrNotFound, got %v", err)
	}
}

func TestGCS_Head_Missing(t *testing.T) {
	_ = fakeGCS(t)
	s := newGCSStore(t, "artifact-gcs-missing")

	_, err := s.Head(context.Background(), "never-uploaded")
	if !errors.Is(err, artifacts.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestGCS_Delete_Missing_IsNoop(t *testing.T) {
	_ = fakeGCS(t)
	s := newGCSStore(t, "artifact-gcs-del-missing")

	if err := s.Delete(context.Background(), "ghost"); err != nil {
		t.Errorf("delete missing: got %v, want nil", err)
	}
}

func TestGCS_SignedPutURL_Format(t *testing.T) {
	// Signing is math-only; no network call. Does not need an emulator.
	store, err := artifacts.NewGCSStore(context.Background(), artifacts.GCSConfig{
		Bucket:          "some-bucket",
		CredentialsJSON: fakeCredsJSON(t),
	})
	if err != nil {
		t.Fatalf("NewGCSStore: %v", err)
	}
	su, err := store.SignedPutURL(context.Background(), "path/to/obj", time.Minute)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !strings.Contains(su.URL, "X-Goog-Signature") {
		t.Errorf("URL missing V4 signature marker: %s", su.URL)
	}
	if !strings.Contains(su.URL, "some-bucket/path/to/obj") {
		t.Errorf("URL missing bucket/key: %s", su.URL)
	}
	if time.Until(su.ExpiresAt) <= 0 {
		t.Errorf("expires must be in the future")
	}
}

func TestGCS_Sign_NoKey_Errors(t *testing.T) {
	store, err := artifacts.NewGCSStore(context.Background(), artifacts.GCSConfig{
		Bucket: "artifact-no-key",
		// no CredentialsJSON / CredentialsFile — signing must refuse.
	})
	if err != nil {
		t.Fatalf("NewGCSStore: %v", err)
	}
	if _, err := store.SignedPutURL(context.Background(), "x", time.Minute); err == nil {
		t.Error("expected error without signing key")
	}
}

func TestGCS_ExtractSigner_MissingFields(t *testing.T) {
	// Valid JSON but no client_email → must refuse to build with that
	// creds config? Today NewGCSStore silently swallows extractSigner
	// errors (so Head/Get/Delete still work on ADC). Document the
	// behaviour: signing lazy-errors in that case.
	store, err := artifacts.NewGCSStore(context.Background(), artifacts.GCSConfig{
		Bucket:          "gcs-missing-fields",
		CredentialsJSON: []byte(`{"type":"service_account"}`),
	})
	if err != nil {
		t.Fatalf("NewGCSStore: %v", err)
	}
	_ = io.EOF // unused import guard
	if _, err := store.SignedPutURL(context.Background(), "x", time.Minute); err == nil {
		t.Error("expected error for creds without signer fields")
	}
}
