package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config holds everything NewS3Store needs. Endpoint is optional:
// empty = talk to AWS; set = talk to a compatible service (R2, Tigris,
// LocalStack, Backblaze B2 S3-compat endpoint).
//
// AccessKey/SecretKey are also optional — when both empty we fall back
// to the AWS default credentials chain (env, IMDS, IRSA, profile).
// That's how prod deployments on EC2/EKS get their role-based creds
// without shipping static keys into the container.
type S3Config struct {
	Bucket    string
	Region    string
	Endpoint  string
	AccessKey string
	SecretKey string
	// UsePathStyle forces `http://host/bucket/key` URLs instead of
	// `http://bucket.host/key`. Required by LocalStack and MinIO-like
	// services; AWS itself is fine with either.
	UsePathStyle bool
}

// S3Store implements Store against an S3-compatible endpoint via the
// AWS SDK v2. Signed URLs come from the SDK's PresignClient and bypass
// the gocdnext-server entirely — the agent PUTs/GETs straight at S3.
type S3Store struct {
	client    *s3.Client
	presigner *s3.PresignClient
	bucket    string
}

// NewS3Store wires the SDK client. Does not create the bucket — that's
// an operator concern (and AWS IAM usually doesn't grant CreateBucket
// to the role that writes objects).
func NewS3Store(ctx context.Context, cfg S3Config) (*S3Store, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("artifacts: s3: bucket is required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
	}
	if cfg.AccessKey != "" || cfg.SecretKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("artifacts: s3: load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		if cfg.UsePathStyle {
			o.UsePathStyle = true
		}
	})
	return &S3Store{
		client:    client,
		presigner: s3.NewPresignClient(client),
		bucket:    cfg.Bucket,
	}, nil
}

// Bucket exposes the configured bucket (for logging/tests).
func (s *S3Store) Bucket() string { return s.bucket }

// Client exposes the underlying S3 client for tests that need to verify
// object state directly. Do not use from production code paths.
func (s *S3Store) Client() *s3.Client { return s.client }

func (s *S3Store) SignedPutURL(ctx context.Context, key string, ttl time.Duration) (SignedURL, error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	req, err := s.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return SignedURL{}, fmt.Errorf("artifacts: s3: presign put: %w", err)
	}
	return SignedURL{URL: req.URL, ExpiresAt: time.Now().Add(ttl)}, nil
}

func (s *S3Store) SignedGetURL(ctx context.Context, key string, ttl time.Duration) (SignedURL, error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	req, err := s.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return SignedURL{}, fmt.Errorf("artifacts: s3: presign get: %w", err)
	}
	return SignedURL{URL: req.URL, ExpiresAt: time.Now().Add(ttl)}, nil
}

func (s *S3Store) Head(ctx context.Context, key string) (int64, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("artifacts: s3: head: %w", err)
	}
	if out.ContentLength == nil {
		return 0, nil
	}
	return *out.ContentLength, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	// DeleteObject is idempotent on S3 — a delete of a non-existent key
	// returns 204, so there's no branch for "not found" here.
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("artifacts: s3: delete: %w", err)
	}
	return nil
}

func (s *S3Store) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	// The SDK reads r fully; for streaming uploads the agent uses the
	// pre-signed URL path and never hits this method. Kept for tests
	// and the handler's direct-write fallback.
	body, ok := r.(io.ReadSeeker)
	if !ok {
		// Buffer so SDK can compute Content-Length; accept the memory
		// cost, it's only used by tests/tooling.
		b, err := io.ReadAll(r)
		if err != nil {
			return 0, fmt.Errorf("artifacts: s3: read body: %w", err)
		}
		_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(key),
			Body:          &byteSliceReader{buf: b},
			ContentLength: aws.Int64(int64(len(b))),
		})
		if err != nil {
			return 0, fmt.Errorf("artifacts: s3: put: %w", err)
		}
		return int64(len(b)), nil
	}
	size, err := seekSize(body)
	if err != nil {
		return 0, err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		return 0, fmt.Errorf("artifacts: s3: put: %w", err)
	}
	return size, nil
}

func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("artifacts: s3: get: %w", err)
	}
	return out.Body, nil
}

// EnsureBucket creates the bucket if it doesn't exist. Opt-in — call
// from the server bootstrap when using LocalStack/MinIO-like
// environments where the operator expects auto-setup. In production AWS
// this is usually not wanted.
func (s *S3Store) EnsureBucket(ctx context.Context, region string) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err == nil {
		return nil
	}
	input := &s3.CreateBucketInput{Bucket: aws.String(s.bucket)}
	if region != "" && region != "us-east-1" {
		input.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(region),
		}
	}
	if _, err := s.client.CreateBucket(ctx, input); err != nil {
		return fmt.Errorf("artifacts: s3: create bucket %q: %w", s.bucket, err)
	}
	return nil
}

// isS3NotFound covers both the typed NotFound error from HeadObject and
// the generic smithy APIError for "NoSuchKey" from GetObject.
func isS3NotFound(err error) bool {
	var nf *s3types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return true
		}
	}
	return false
}

// seekSize figures out the body length by seeking to the end. Callers
// are responsible for the ReadSeeker state; we restore position.
func seekSize(rs io.ReadSeeker) (int64, error) {
	cur, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, fmt.Errorf("artifacts: s3: seek current: %w", err)
	}
	end, err := rs.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("artifacts: s3: seek end: %w", err)
	}
	if _, err := rs.Seek(cur, io.SeekStart); err != nil {
		return 0, fmt.Errorf("artifacts: s3: seek restore: %w", err)
	}
	return end - cur, nil
}

// byteSliceReader is a minimal ReadSeeker over a byte slice — avoids
// pulling bytes.Reader just to satisfy the SDK's body type.
type byteSliceReader struct {
	buf []byte
	pos int64
}

func (b *byteSliceReader) Read(p []byte) (int, error) {
	if b.pos >= int64(len(b.buf)) {
		return 0, io.EOF
	}
	n := copy(p, b.buf[b.pos:])
	b.pos += int64(n)
	return n, nil
}

func (b *byteSliceReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = b.pos + offset
	case io.SeekEnd:
		abs = int64(len(b.buf)) + offset
	default:
		return 0, errors.New("byteSliceReader: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("byteSliceReader: negative position")
	}
	b.pos = abs
	return abs, nil
}
