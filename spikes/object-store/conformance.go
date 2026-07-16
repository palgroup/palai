package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const (
	region            = "us-east-1"
	minimumPartSize   = 5 * 1024 * 1024
	maxSingleReadSize = 16 * 1024 * 1024
)

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{7,62}$`)

type LiveConfiguration struct {
	Endpoint    string
	AccessKey   string
	SecretKey   string
	RunID       string
	Iteration   int
	SummaryPath string
	GitCommit   string
	SourceTree  string
}

type RunResult struct {
	SchemaVersion int
	GitCommit     string
	SourceTree    string
	RunID         string
	Iteration     int
	Phase         string
	Cases         map[string]time.Duration
	BytesVerified int64
}

type stageError struct {
	stage string
	err   error
}

func (e *stageError) Error() string { return e.stage }
func (e *stageError) Unwrap() error { return e.err }

func LiveConfigurationFromEnvironment() (LiveConfiguration, bool) {
	configuration := LiveConfiguration{
		Endpoint:    os.Getenv("PALAI_SPIKE_OBJECT_STORE_ENDPOINT"),
		AccessKey:   os.Getenv("PALAI_SPIKE_OBJECT_STORE_ACCESS_KEY"),
		SecretKey:   os.Getenv("PALAI_SPIKE_OBJECT_STORE_SECRET_KEY"),
		RunID:       os.Getenv("PALAI_SPIKE_OBJECT_STORE_RUN_ID"),
		SummaryPath: os.Getenv("PALAI_SPIKE_OBJECT_STORE_SUMMARY"),
		GitCommit:   os.Getenv("PALAI_SPIKE_GIT_COMMIT"),
		SourceTree:  os.Getenv("PALAI_SPIKE_SOURCE_TREE"),
	}
	iteration, err := strconv.Atoi(os.Getenv("PALAI_SPIKE_OBJECT_STORE_ITERATION"))
	configuration.Iteration = iteration
	if err != nil || iteration < 1 || configuration.Endpoint == "" || configuration.AccessKey == "" ||
		configuration.SecretKey == "" || configuration.RunID == "" || configuration.SummaryPath == "" ||
		configuration.GitCommit == "" || configuration.SourceTree == "" {
		return LiveConfiguration{}, false
	}
	return configuration, true
}

func ProbeSignedReadiness(ctx context.Context, configuration LiveConfiguration) error {
	if err := validateLiveConfiguration(configuration); err != nil {
		return err
	}
	_, err := newS3Client(configuration, configuration.SecretKey).ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return fail("signed readiness", err)
	}
	return nil
}

func RunS3Conformance(ctx context.Context, configuration LiveConfiguration) (RunResult, error) {
	if err := validateLiveConfiguration(configuration); err != nil {
		return RunResult{}, err
	}
	client := newS3Client(configuration, configuration.SecretKey)
	wrongSecretClient := newS3Client(configuration, configuration.SecretKey+"-wrong")
	bucket := bucketName(configuration)
	keys := objectKeys()
	result := newRunResult(configuration, "conformance")
	keepBucket := false
	defer func() {
		if !keepBucket {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = cleanupBucket(cleanupCtx, client, bucket)
		}
	}()

	if err := observe(&result, "auth.wrong_secret_rejected", func() error {
		_, err := wrongSecretClient.ListBuckets(ctx, &s3.ListBucketsInput{})
		if err == nil || !hasHTTPStatus(err, http.StatusForbidden) {
			return errors.New("wrong secret was not rejected with HTTP 403")
		}
		return nil
	}); err != nil {
		return RunResult{}, err
	}

	if err := observe(&result, "bucket.create_and_head", func() error {
		if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
			return err
		}
		_, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
		return err
	}); err != nil {
		return RunResult{}, err
	}

	checksumPayload := deterministicPayload(configuration, "checksum", 8192)
	if err := observe(&result, "checksum.put_head_get", func() error {
		checksum := checksumBase64(checksumPayload)
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:            aws.String(bucket),
			Key:               aws.String(keys.checksum),
			Body:              bytes.NewReader(checksumPayload),
			ContentLength:     aws.Int64(int64(len(checksumPayload))),
			ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
			ChecksumSHA256:    aws.String(checksum),
		}); err != nil {
			return err
		}
		return verifyChecksummedObject(ctx, client, bucket, keys.checksum, checksumPayload)
	}); err != nil {
		return RunResult{}, err
	}
	result.BytesVerified += int64(len(checksumPayload))

	conditionalPayload := deterministicPayload(configuration, "conditional", 1024)
	if err := observe(&result, "conditional.if_none_match", func() error {
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(bucket),
			Key:           aws.String(keys.conditional),
			Body:          bytes.NewReader(conditionalPayload),
			ContentLength: aws.Int64(int64(len(conditionalPayload))),
			IfNoneMatch:   aws.String("*"),
		}); err != nil {
			return err
		}
		mutated := deterministicPayload(configuration, "conditional-mutated", 1024)
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(bucket),
			Key:           aws.String(keys.conditional),
			Body:          bytes.NewReader(mutated),
			ContentLength: aws.Int64(int64(len(mutated))),
			IfNoneMatch:   aws.String("*"),
		})
		if err == nil || !isPreconditionFailed(err) {
			return errors.New("repeat conditional PUT did not return PreconditionFailed")
		}
		return verifyObjectBytes(ctx, client, bucket, keys.conditional, conditionalPayload, false)
	}); err != nil {
		return RunResult{}, err
	}
	result.BytesVerified += int64(len(conditionalPayload))

	if err := observe(&result, "range.exact_bytes", func() error {
		output, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(keys.checksum),
			Range:  aws.String("bytes=7-18"),
		})
		if err != nil {
			return err
		}
		defer output.Body.Close()
		body, err := readBounded(output.Body, 12)
		if err != nil {
			return err
		}
		expectedRange := fmt.Sprintf("bytes 7-18/%d", len(checksumPayload))
		if !bytes.Equal(body, checksumPayload[7:19]) || aws.ToString(output.ContentRange) != expectedRange {
			return errors.New("ranged response bytes or Content-Range differed")
		}
		return nil
	}); err != nil {
		return RunResult{}, err
	}
	result.BytesVerified += 12

	partOne := deterministicPayload(configuration, "multipart-1", minimumPartSize)
	partTwo := deterministicPayload(configuration, "multipart-2", 1024*1024+17)
	if err := observe(&result, "multipart.complete", func() error {
		created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(keys.multipart),
		})
		if err != nil || created.UploadId == nil {
			return coalesceError(err, "multipart upload ID was absent")
		}
		completed := make([]types.CompletedPart, 0, 2)
		for index, payload := range [][]byte{partOne, partTwo} {
			partNumber := int32(index + 1)
			uploaded, uploadErr := client.UploadPart(ctx, &s3.UploadPartInput{
				Bucket:        aws.String(bucket),
				Key:           aws.String(keys.multipart),
				UploadId:      created.UploadId,
				PartNumber:    aws.Int32(partNumber),
				Body:          bytes.NewReader(payload),
				ContentLength: aws.Int64(int64(len(payload))),
			})
			if uploadErr != nil || uploaded.ETag == nil {
				return coalesceError(uploadErr, "multipart part ETag was absent")
			}
			completed = append(completed, types.CompletedPart{ETag: uploaded.ETag, PartNumber: aws.Int32(partNumber)})
		}
		if _, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket:   aws.String(bucket),
			Key:      aws.String(keys.multipart),
			UploadId: created.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{
				Parts: completed,
			},
		}); err != nil {
			return err
		}
		expected := append(append(make([]byte, 0, len(partOne)+len(partTwo)), partOne...), partTwo...)
		return verifyObjectBytes(ctx, client, bucket, keys.multipart, expected, false)
	}); err != nil {
		return RunResult{}, err
	}
	result.BytesVerified += int64(len(partOne) + len(partTwo))

	if err := observe(&result, "multipart.abort", func() error {
		created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(keys.abortedMultipart),
		})
		if err != nil || created.UploadId == nil {
			return coalesceError(err, "abort upload ID was absent")
		}
		uploaded, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(bucket),
			Key:           aws.String(keys.abortedMultipart),
			UploadId:      created.UploadId,
			PartNumber:    aws.Int32(1),
			Body:          bytes.NewReader(partOne),
			ContentLength: aws.Int64(int64(len(partOne))),
		})
		if err != nil || uploaded.ETag == nil {
			return coalesceError(err, "abort part ETag was absent")
		}
		if _, err := client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(bucket),
			Key:      aws.String(keys.abortedMultipart),
			UploadId: created.UploadId,
		}); err != nil {
			return err
		}
		listed, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
			Bucket: aws.String(bucket),
			Prefix: aws.String(keys.abortedMultipart),
		})
		if err != nil {
			return err
		}
		for _, upload := range listed.Uploads {
			if aws.ToString(upload.UploadId) == aws.ToString(created.UploadId) {
				return errors.New("aborted upload remained listable")
			}
		}
		_, err = client.ListParts(ctx, &s3.ListPartsInput{
			Bucket:   aws.String(bucket),
			Key:      aws.String(keys.abortedMultipart),
			UploadId: created.UploadId,
		})
		if err == nil || (!hasHTTPStatus(err, http.StatusNotFound) && apiErrorCode(err) != "NoSuchUpload") {
			return errors.New("ListParts did not reject the aborted upload ID")
		}
		return nil
	}); err != nil {
		return RunResult{}, err
	}

	if err := observe(&result, "object.delete_not_found", func() error {
		if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(keys.checksum),
		}); err != nil {
			return err
		}
		if _, err := client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(keys.checksum),
		}); err == nil || !hasHTTPStatus(err, http.StatusNotFound) {
			return errors.New("HEAD after delete was not not-found")
		}
		if output, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(keys.checksum),
		}); err == nil {
			output.Body.Close()
			return errors.New("GET after delete succeeded")
		} else if !hasHTTPStatus(err, http.StatusNotFound) {
			return errors.New("GET after delete was not not-found")
		}
		return nil
	}); err != nil {
		return RunResult{}, err
	}

	for _, key := range []string{keys.conditional, keys.multipart} {
		if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err != nil {
			return RunResult{}, fail("remove transient conformance object", err)
		}
	}
	persistencePayload := deterministicPayload(configuration, "persistence", 4096)
	if err := observe(&result, "persistence.seeded", func() error {
		checksum := checksumBase64(persistencePayload)
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:            aws.String(bucket),
			Key:               aws.String(keys.persistence),
			Body:              bytes.NewReader(persistencePayload),
			ContentLength:     aws.Int64(int64(len(persistencePayload))),
			ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
			ChecksumSHA256:    aws.String(checksum),
		})
		return err
	}); err != nil {
		return RunResult{}, err
	}
	keepBucket = true
	return result, nil
}

func VerifyRestartPersistence(ctx context.Context, configuration LiveConfiguration) (RunResult, error) {
	if err := validateLiveConfiguration(configuration); err != nil {
		return RunResult{}, err
	}
	client := newS3Client(configuration, configuration.SecretKey)
	bucket := bucketName(configuration)
	keys := objectKeys()
	result := newRunResult(configuration, "persistence")
	payload := deterministicPayload(configuration, "persistence", 4096)

	if err := observe(&result, "persistence.retained_bytes_checksum", func() error {
		return verifyChecksummedObject(ctx, client, bucket, keys.persistence, payload)
	}); err != nil {
		return RunResult{}, err
	}
	result.BytesVerified += int64(len(payload))
	if err := observe(&result, "persistence.cleanup", func() error {
		return cleanupBucket(ctx, client, bucket)
	}); err != nil {
		return RunResult{}, err
	}
	return result, nil
}

func PublicError(err error, _ LiveConfiguration) error {
	var staged *stageError
	if errors.As(err, &staged) {
		return fmt.Errorf("object-store live proof failed at %s", staged.stage)
	}
	return errors.New("object-store live proof failed")
}

func WriteRunSummary(path string, result RunResult) error {
	if path == "" || result.SchemaVersion != 1 || len(result.Cases) == 0 {
		return fail("validate run summary", errors.New("incomplete run summary"))
	}
	latencies := make(map[string]float64, len(result.Cases))
	for name, latency := range result.Cases {
		if name == "" || latency < 0 {
			return fail("validate run summary", errors.New("invalid case latency"))
		}
		latencies[name] = float64(latency) / float64(time.Millisecond)
	}
	value := struct {
		SchemaVersion int                `json:"schema_version"`
		GitCommit     string             `json:"git_commit"`
		SourceTree    string             `json:"source_tree"`
		RunID         string             `json:"run_id"`
		Iteration     int                `json:"iteration"`
		Phase         string             `json:"phase"`
		CaseLatencyMS map[string]float64 `json:"case_latency_ms"`
		BytesVerified int64              `json:"bytes_verified"`
	}{
		SchemaVersion: result.SchemaVersion,
		GitCommit:     result.GitCommit,
		SourceTree:    result.SourceTree,
		RunID:         result.RunID,
		Iteration:     result.Iteration,
		Phase:         result.Phase,
		CaseLatencyMS: latencies,
		BytesVerified: result.BytesVerified,
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fail("marshal run summary", err)
	}
	file, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fail("open run summary", err)
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return fail("restrict run summary permissions", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fail("write run summary", err)
	}
	return nil
}

func validateLiveConfiguration(configuration LiveConfiguration) error {
	endpoint, err := url.Parse(configuration.Endpoint)
	if err != nil || endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" || endpoint.User != nil || endpoint.Port() == "" {
		return fail("validate explicit loopback endpoint", errors.New("invalid endpoint"))
	}
	if configuration.AccessKey == "" || configuration.SecretKey == "" || !runIDPattern.MatchString(configuration.RunID) ||
		configuration.Iteration < 1 || !commitPattern.MatchString(configuration.GitCommit) ||
		!commitPattern.MatchString(configuration.SourceTree) || configuration.SummaryPath == "" {
		return fail("validate explicit static configuration", errors.New("invalid configuration"))
	}
	return nil
}

func newS3Client(configuration LiveConfiguration, secret string) *s3.Client {
	provider := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{
			AccessKeyID:     configuration.AccessKey,
			SecretAccessKey: secret,
			Source:          "palai-object-store-spike-static",
		}, nil
	})
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	httpClient := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			ForceAttemptHTTP2:   false,
			MaxIdleConns:        4,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 3 * time.Second,
		},
	}
	configurationValue := aws.Config{
		Region:      region,
		Credentials: provider,
		HTTPClient:  httpClient,
	}
	return s3.NewFromConfig(configurationValue, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(configuration.Endpoint)
		options.UsePathStyle = true
		options.RetryMaxAttempts = 2
	})
}

func verifyChecksummedObject(ctx context.Context, client *s3.Client, bucket, key string, expected []byte) error {
	checksum := checksumBase64(expected)
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:       aws.String(bucket),
		Key:          aws.String(key),
		ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		return err
	}
	if aws.ToInt64(head.ContentLength) != int64(len(expected)) || aws.ToString(head.ChecksumSHA256) != checksum {
		return errors.New("HEAD size or SHA-256 checksum differed")
	}
	return verifyObjectBytes(ctx, client, bucket, key, expected, true)
}

func verifyObjectBytes(ctx context.Context, client *s3.Client, bucket, key string, expected []byte, requireChecksum bool) error {
	input := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if requireChecksum {
		input.ChecksumMode = types.ChecksumModeEnabled
	}
	output, err := client.GetObject(ctx, input)
	if err != nil {
		return err
	}
	defer output.Body.Close()
	if len(expected) > maxSingleReadSize {
		return errors.New("expected object exceeds proof read bound")
	}
	body, err := readBounded(output.Body, len(expected))
	if err != nil {
		return err
	}
	if aws.ToInt64(output.ContentLength) != int64(len(expected)) || !bytes.Equal(body, expected) {
		return errors.New("GET bytes or size differed")
	}
	if requireChecksum && aws.ToString(output.ChecksumSHA256) != checksumBase64(expected) {
		return errors.New("GET SHA-256 checksum differed")
	}
	return nil
}

func readBounded(reader io.Reader, expected int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, int64(expected+1)))
	if err != nil {
		return nil, err
	}
	if len(data) != expected {
		return nil, errors.New("response body length differed")
	}
	return data, nil
}

func cleanupBucket(ctx context.Context, client *s3.Client, bucket string) error {
	uploads, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
	if err == nil {
		for _, upload := range uploads.Uploads {
			_, _ = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
				Bucket: aws.String(bucket), Key: upload.Key, UploadId: upload.UploadId,
			})
		}
	} else if !hasHTTPStatus(err, http.StatusNotFound) {
		return err
	}
	objects, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err == nil {
		for _, object := range objects.Contents {
			if _, deleteErr := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: object.Key}); deleteErr != nil {
				return deleteErr
			}
		}
	} else if !hasHTTPStatus(err, http.StatusNotFound) {
		return err
	}
	if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil && !hasHTTPStatus(err, http.StatusNotFound) {
		return err
	}
	return nil
}

func observe(result *RunResult, name string, operation func() error) error {
	started := time.Now()
	if err := operation(); err != nil {
		return fail(name, err)
	}
	result.Cases[name] = time.Since(started)
	return nil
}

func newRunResult(configuration LiveConfiguration, phase string) RunResult {
	return RunResult{
		SchemaVersion: 1,
		GitCommit:     configuration.GitCommit,
		SourceTree:    configuration.SourceTree,
		RunID:         configuration.RunID,
		Iteration:     configuration.Iteration,
		Phase:         phase,
		Cases:         make(map[string]time.Duration),
	}
}

func deterministicPayload(configuration LiveConfiguration, label string, size int) []byte {
	seed := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", label, configuration.RunID, configuration.Iteration)))
	payload := make([]byte, size)
	for offset := 0; offset < len(payload); offset += len(seed) {
		copy(payload[offset:], seed[:])
	}
	return payload
}

func checksumBase64(data []byte) string {
	sum := sha256.Sum256(data)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func bucketName(configuration LiveConfiguration) string {
	value := strings.ToLower(configuration.RunID)
	return "palai-spike-" + value[:min(len(value), 48)]
}

type keys struct {
	checksum         string
	conditional      string
	multipart        string
	abortedMultipart string
	persistence      string
}

func objectKeys() keys {
	return keys{
		checksum:         "proof/checksum.bin",
		conditional:      "proof/conditional.bin",
		multipart:        "proof/multipart.bin",
		abortedMultipart: "proof/aborted-multipart.bin",
		persistence:      "proof/persistence.bin",
	}
}

func hasHTTPStatus(err error, status int) bool {
	var responseError *smithyhttp.ResponseError
	return errors.As(err, &responseError) && responseError.HTTPStatusCode() == status
}

func apiErrorCode(err error) string {
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		return apiError.ErrorCode()
	}
	return ""
}

func isPreconditionFailed(err error) bool {
	return hasHTTPStatus(err, http.StatusPreconditionFailed) || apiErrorCode(err) == "PreconditionFailed"
}

func coalesceError(err error, message string) error {
	if err != nil {
		return err
	}
	return errors.New(message)
}

func fail(stage string, err error) error {
	return &stageError{stage: stage, err: err}
}
