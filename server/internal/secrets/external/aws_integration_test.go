package external_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"

	"github.com/gocdnext/gocdnext/server/internal/secrets/external"
)

func startSecretsManager(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctr, err := localstack.Run(ctx, "localstack/localstack:3.8",
		testcontainers.WithEnv(map[string]string{"SERVICES": "secretsmanager"}))
	if err != nil {
		t.Skipf("localstack unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	host, _ := ctr.Host(ctx)
	port, _ := ctr.MappedPort(ctx, "4566/tcp")
	return "http://" + host + ":" + port.Port()
}

func TestAWSBackend(t *testing.T) {
	endpoint := startSecretsManager(t)
	ctx := context.Background()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatalf("aws cfg: %v", err)
	}
	sm := secretsmanager.NewFromConfig(cfg, func(o *secretsmanager.Options) { o.BaseEndpoint = aws.String(endpoint) })
	create := func(name, val string) {
		if _, err := sm.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
			Name: aws.String(name), SecretString: aws.String(val),
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	create("plain", "raw-value")
	create("jsonish", `{"DB_PASS":"pw123","USER":"svc"}`)
	// A binary secret EXISTS but has no SecretString — must fail loud, not
	// masquerade as not-found.
	if _, err := sm.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: aws.String("binblob"), SecretBinary: []byte{0x00, 0x01, 0x02},
	}); err != nil {
		t.Fatalf("create binary: %v", err)
	}

	b, err := external.NewAWSBackend(ctx, external.AWSConfig{Region: "us-east-1", Endpoint: endpoint})
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	if b.Name() != "aws" {
		t.Fatalf("name = %q", b.Name())
	}

	// whole secret (empty key)
	if v, err := b.Fetch(ctx, "plain", ""); err != nil || v != "raw-value" {
		t.Fatalf("whole-secret = %q, %v", v, err)
	}
	// JSON field
	if v, err := b.Fetch(ctx, "jsonish", "DB_PASS"); err != nil || v != "pw123" {
		t.Fatalf("json-field = %q, %v", v, err)
	}
	// missing field → not found (silent omit upstream)
	if _, err := b.Fetch(ctx, "jsonish", "NOPE"); !errors.Is(err, external.ErrSecretNotFound) {
		t.Fatalf("missing field err = %v, want ErrSecretNotFound", err)
	}
	// non-JSON secret + a key → loud error (never inject a blank env var)
	if _, err := b.Fetch(ctx, "plain", "DB_PASS"); err == nil {
		t.Fatal("non-JSON secret with a key request should error")
	}
	// missing secret → not found
	if _, err := b.Fetch(ctx, "absent", ""); !errors.Is(err, external.ErrSecretNotFound) {
		t.Fatalf("missing secret err = %v, want ErrSecretNotFound", err)
	}
	// binary secret → loud error (exists, but unusable as an env value), not
	// ErrSecretNotFound; cites the path.
	_, err = b.Fetch(ctx, "binblob", "")
	if err == nil || errors.Is(err, external.ErrSecretNotFound) {
		t.Fatalf("binary secret err = %v, want a loud (non-not-found) error", err)
	}
	if !errContains(err, "binblob") || !errContains(err, "binary") {
		t.Fatalf("binary error %q should name the path and say it's binary", err)
	}
}

func errContains(err error, sub string) bool {
	return err != nil && strings.Contains(err.Error(), sub)
}

// TestAWSBackend_NoRegionFailsClosed: enabling AWS without a resolvable region
// (no GOCDNEXT_SECRET_AWS_REGION, no AWS_REGION, no shared config) must fail at
// boot, not enter configured_sources and break on the first lookup. No
// container needed.
func TestAWSBackend_NoRegionFailsClosed(t *testing.T) {
	// Strip every region/config source so LoadDefaultConfig resolves none.
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	_, err := external.NewAWSBackend(context.Background(), external.AWSConfig{})
	if err == nil || !errContains(err, "region") {
		t.Fatalf("err = %v, want a clear no-region boot error", err)
	}
}
