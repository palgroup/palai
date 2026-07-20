// Package artifacts is the control-plane's boundary over the S3-compatible object store
// (SeaweedFS locally). It is the only place the S3 credential lives: per spec §24 the
// engine and runner never receive it, which is why this package is internal to the
// control plane. Store carries the byte-level PUT/GET/DELETE; Writer ties a written
// object to a durable, tenant-scoped artifacts row (spec §22.6).
package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// defaultRegion is a placeholder region: a path-style S3-compatible endpoint ignores it,
// but the signer requires a non-empty value.
const defaultRegion = "us-east-1"

// maxReadBytes bounds a single object read so a malformed size cannot exhaust memory.
// Artifacts written today (a run's terminal output, later patch/test logs) are small;
// ponytail: streaming retrieval for large artifacts is future work, not this path.
const maxReadBytes = 64 * 1024 * 1024

// Config points the control-plane at its object store. The credential is control-plane
// only (spec §24); it is read here and never logged, put on a request, or handed to the
// engine. For the local SeaweedFS the identity is immaterial (it accepts anonymous S3),
// but the values still ride env, never source, so a real backend swaps in unchanged.
type Config struct {
	Endpoint  string // e.g. http://object-store:8333 (compose) or http://127.0.0.1:<port> (tests)
	Bucket    string
	Region    string // empty -> defaultRegion
	AccessKey string
	SecretKey string
}

// Store is the S3-compatible object boundary bound to one bucket.
type Store struct {
	client *s3.Client
	bucket string
}

// NewStore builds the object-store client from cfg. The HTTP client is bounded (dial,
// TLS, and overall timeouts) so a wedged object store fails fast rather than hanging a
// retention sweep or a write.
func NewStore(cfg Config) (*Store, error) {
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, fmt.Errorf("artifacts: invalid object-store endpoint %q", cfg.Endpoint)
	}
	if cfg.Bucket == "" {
		return nil, errors.New("artifacts: object-store bucket is required")
	}
	region := cfg.Region
	if region == "" {
		region = defaultRegion
	}
	provider := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			Source:          "palai-control-plane-artifacts",
		}, nil
	})
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			ForceAttemptHTTP2:   false,
			MaxIdleConns:        8,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 3 * time.Second,
		},
	}
	client := s3.NewFromConfig(aws.Config{
		Region:      region,
		Credentials: provider,
		HTTPClient:  httpClient,
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = true
		o.RetryMaxAttempts = 3
	})
	return &Store{client: client, bucket: cfg.Bucket}, nil
}

// EnsureBucket makes the configured bucket exist, tolerating a concurrent or prior
// create. It is called once at startup; a fresh local object store has no buckets.
func (s *Store) EnsureBucket(ctx context.Context) error {
	if _, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		// The bucket may already exist (a restart, or a peer control-plane). Confirm with a
		// HEAD rather than string-matching a provider-specific "already owned" code.
		if _, headErr := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); headErr != nil {
			return fmt.Errorf("ensure bucket %q: %w", s.bucket, err)
		}
	}
	return nil
}

// Put writes body under key with an end-to-end SHA-256 integrity check (the store
// verifies the digest on receipt) and returns the content checksum as "sha256:<hex>"
// plus the byte size, so the caller records them on the artifacts row.
func (s *Store) Put(ctx context.Context, key string, body []byte) (checksum string, size int64, err error) {
	sum := sha256.Sum256(body)
	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:            aws.String(s.bucket),
		Key:               aws.String(key),
		Body:              bytes.NewReader(body),
		ContentLength:     aws.Int64(int64(len(body))),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
		ChecksumSHA256:    aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	}); err != nil {
		return "", 0, fmt.Errorf("put object %q: %w", key, err)
	}
	return "sha256:" + hex.EncodeToString(sum[:]), int64(len(body)), nil
}

// Get reads the object at key. found is false (with a nil error) when the object is
// absent, so a caller distinguishes a miss from a transport failure.
func (s *Store) Get(ctx context.Context, key string) (body []byte, found bool, err error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if hasHTTPStatus(err, http.StatusNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get object %q: %w", key, err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(io.LimitReader(out.Body, maxReadBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read object %q: %w", key, err)
	}
	if len(data) > maxReadBytes {
		return nil, false, fmt.Errorf("object %q exceeds %d-byte read bound", key, maxReadBytes)
	}
	return data, true, nil
}

// ObjectInfo is a stored object's key and server-recorded last-modified time — the two
// facts the orphan reconcile needs: the key to join against the artifacts index, and the
// modified time to spare an object still inside the write grace window.
type ObjectInfo struct {
	Key          string
	LastModified time.Time
}

// List returns every object in the bucket, following pagination to completion. The bucket
// holds only this control-plane's artifacts, so the full listing is the left side of the
// orphan-GC reconcile (E10 Task 3). It stays control-plane-internal (spec §24): the S3
// credential never leaves, and only keys/timestamps — no bytes, no tenant data — are read.
func (s *Store) List(ctx context.Context) ([]ObjectInfo, error) {
	var objects []ObjectInfo
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket)})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects in %q: %w", s.bucket, err)
		}
		for _, obj := range page.Contents {
			info := ObjectInfo{Key: aws.ToString(obj.Key)}
			if obj.LastModified != nil {
				info.LastModified = *obj.LastModified
			}
			objects = append(objects, info)
		}
	}
	return objects, nil
}

// Delete removes the object at key. S3 delete is idempotent — deleting an absent key is
// not an error — so a retried retention sweep can re-issue it safely.
func (s *Store) Delete(ctx context.Context, key string) error {
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("delete object %q: %w", key, err)
	}
	return nil
}

// hasHTTPStatus reports whether err carries the given HTTP status from the S3 response.
func hasHTTPStatus(err error, status int) bool {
	var responseError *smithyhttp.ResponseError
	return errors.As(err, &responseError) && responseError.HTTPStatusCode() == status
}
