package external

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// AWSConfig is the connection config. Credentials come from the default
// chain (IRSA on EKS, env locally) — same posture as the S3 artifact
// backend. Endpoint overrides the resolved endpoint (LocalStack in tests).
type AWSConfig struct {
	Region   string
	Endpoint string
}

// AWSBackend reads from AWS Secrets Manager. ref_path is the secret id/ARN;
// an empty ref_key returns the whole SecretString, a non-empty key extracts
// that field from a JSON secret (the common "one secret, many fields" shape).
type AWSBackend struct {
	client *secretsmanager.Client
}

// NewAWSBackend builds the client (fail-fast at boot).
func NewAWSBackend(ctx context.Context, cfg AWSConfig) (*AWSBackend, error) {
	var optFns []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		optFns = append(optFns, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, optFns...)
	if err != nil {
		return nil, fmt.Errorf("aws: load config: %w", err)
	}
	// Fail fast at boot if no region resolved (neither GOCDNEXT_SECRET_AWS_REGION
	// nor AWS_REGION / shared config). Otherwise the backend would enter
	// configured_sources and only break on the first GetSecretValue.
	if awsCfg.Region == "" {
		return nil, errors.New("aws: no region resolved — set GOCDNEXT_SECRET_AWS_REGION (or AWS_REGION)")
	}
	var smOpts []func(*secretsmanager.Options)
	if cfg.Endpoint != "" {
		smOpts = append(smOpts, func(o *secretsmanager.Options) { o.BaseEndpoint = aws.String(cfg.Endpoint) })
	}
	return &AWSBackend{client: secretsmanager.NewFromConfig(awsCfg, smOpts...)}, nil
}

func (b *AWSBackend) Name() string { return "aws" }

// Fetch gets the secret by id (path). Empty key → whole string; non-empty
// key → JSON field. A non-JSON secret with a key requested fails loud (so a
// blank env var is never silently injected); a missing field is not-found.
func (b *AWSBackend) Fetch(ctx context.Context, path, key string) (string, error) {
	out, err := b.client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(path),
	})
	if err != nil {
		var nf *smtypes.ResourceNotFoundException
		if errors.As(err, &nf) {
			return "", ErrSecretNotFound
		}
		return "", fmt.Errorf("aws: get secret %q: %w", path, err)
	}
	if out.SecretString == nil {
		// A binary secret EXISTS — it's just not a usable env-var value.
		// Returning not-found here would be a lie (and surface as the
		// misleading "secrets not set"); fail loud instead, citing the path.
		if out.SecretBinary != nil {
			return "", fmt.Errorf("aws: secret %q is binary (SecretBinary) — unsupported as an env value; store it as a string", path)
		}
		return "", ErrSecretNotFound
	}
	value := *out.SecretString
	if key == "" {
		return value, nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(value), &obj); err != nil {
		return "", fmt.Errorf("aws: secret %q is not JSON but key %q was requested", path, key)
	}
	v, ok := obj[key]
	if !ok {
		return "", ErrSecretNotFound
	}
	if s, isStr := v.(string); isStr {
		return s, nil
	}
	return fmt.Sprintf("%v", v), nil
}
