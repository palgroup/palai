//go:build component

// Package artifacts' component tests exercise the real S3 write-path against a throwaway
// SeaweedFS (and a throwaway Postgres for the index rows). They run only under
// `make test-component TEST=artifacts`, which starts both containers and exports
// PALAI_COMPONENT_POSTGRES_URL + PALAI_S3_ENDPOINT. The white-box package + build tag
// keep them out of the credential-free, Docker-free unit tier, and — because the object
// store is internal to the control plane (spec §24) — inside apps/control-plane, where Go
// permits importing internal packages.
package artifacts

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
)

// artifactsHarness is a migrated durable spine plus a bucket-ensured object store and a
// Writer bound to both — the real infrastructure the write-path runs against.
type artifactsHarness struct {
	repo   *store.Store
	pool   *pgxpool.Pool
	s3     *Store
	writer *Writer
}

func openArtifactsHarness(t *testing.T) *artifactsHarness {
	t.Helper()
	pgURL := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	endpoint := os.Getenv("PALAI_S3_ENDPOINT")
	if pgURL == "" || endpoint == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL and PALAI_S3_ENDPOINT are required; run make test-component TEST=artifacts")
	}
	ctx := context.Background()
	repo, err := store.Open(ctx, pgURL)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	s3, err := NewStore(Config{
		Endpoint:  endpoint,
		Bucket:    envOr("PALAI_S3_BUCKET", "palai-artifacts-component"),
		Region:    os.Getenv("PALAI_S3_REGION"),
		AccessKey: os.Getenv("PALAI_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("PALAI_S3_SECRET_KEY"),
	})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if err := s3.EnsureBucket(ctx); err != nil {
		t.Fatalf("EnsureBucket() error = %v", err)
	}
	pool := repo.Spine().Pool()
	return &artifactsHarness{repo: repo, pool: pool, s3: s3, writer: NewWriter(s3, pool)}
}

// seedRun creates org -> project -> session -> run and returns the tenant scope and run
// id an artifact must reference (the artifacts row FKs projects and runs).
func (h *artifactsHarness) seedRun(t *testing.T) (org, project, runID string) {
	t.Helper()
	org, project = newID("org"), newID("prj")
	session := newID("ses")
	runID = newID("run")
	h.exec(t, `INSERT INTO organizations (id) VALUES ($1)`, org)
	h.exec(t, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org)
	h.exec(t, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, session, org, project)
	h.exec(t, `INSERT INTO runs (id, organization_id, project_id, session_id) VALUES ($1, $2, $3, $4)`, runID, org, project, session)
	return org, project, runID
}

func (h *artifactsHarness) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q error = %v", sql, err)
	}
}

// TestArtifactPutRecordsRowAndBytes proves the write-path commits both halves and keeps
// them consistent: an artifacts row keyed to the run, an S3 object holding the exact
// bytes, and a checksum that is the SHA-256 of those bytes (spec §22.6, LP §7.2).
func TestArtifactPutRecordsRowAndBytes(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, runID := h.seedRun(t)

	content := []byte("terminal output: build passed in 3.2s\n")
	art, err := h.writer.Write(ctx, WriteRequest{Organization: org, Project: project, RunID: runID, Content: content})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	// The returned checksum and size are the SHA-256 and length of the exact bytes.
	sum := sha256.Sum256(content)
	wantChecksum := "sha256:" + hex.EncodeToString(sum[:])
	if art.Checksum != wantChecksum {
		t.Fatalf("checksum = %q, want %q (SHA-256 of the written bytes)", art.Checksum, wantChecksum)
	}
	if art.SizeBytes != int64(len(content)) {
		t.Fatalf("size = %d, want %d", art.SizeBytes, len(content))
	}

	// The artifacts row is present, tenant-scoped, and carries the same object key,
	// size, and checksum the write returned.
	var (
		gotRun    string
		objectKey string
		size      int64
		checksum  string
	)
	if err := h.pool.QueryRow(ctx,
		`SELECT run_id, object_key, size_bytes, checksum FROM artifacts WHERE id = $1 AND organization_id = $2 AND project_id = $3`,
		art.ID, org, project).Scan(&gotRun, &objectKey, &size, &checksum); err != nil {
		t.Fatalf("read artifacts row error = %v", err)
	}
	if gotRun != runID || objectKey != art.ObjectKey || size != art.SizeBytes || checksum != art.Checksum {
		t.Fatalf("row = {run:%s key:%s size:%d checksum:%s}, want {run:%s key:%s size:%d checksum:%s}",
			gotRun, objectKey, size, checksum, runID, art.ObjectKey, art.SizeBytes, art.Checksum)
	}

	// The S3 object holds exactly those bytes, and the recorded checksum is their digest.
	body, found, err := h.s3.Get(ctx, art.ObjectKey)
	if err != nil {
		t.Fatalf("Get(%q) error = %v", art.ObjectKey, err)
	}
	if !found {
		t.Fatalf("S3 object %q is absent after a successful write", art.ObjectKey)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("S3 bytes = %q, want %q", body, content)
	}
	storedSum := sha256.Sum256(body)
	if checksum != "sha256:"+hex.EncodeToString(storedSum[:]) {
		t.Fatalf("recorded checksum %q does not match the SHA-256 of the stored bytes", checksum)
	}
}

// TestArtifactReadIsTenantScoped proves a read is gated by the verified tenant scope: the
// owner reads its artifact back, but a different tenant asking for the same artifact id
// gets the identical miss a truly-absent id returns — no bytes, no error, no way to tell a
// real artifact from a missing one (spec §22.6 existence non-disclosure; the LP retrieval
// pattern). The foreign read never reaches the object store.
func TestArtifactReadIsTenantScoped(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, runID := h.seedRun(t)

	content := []byte("tenant A private artifact bytes")
	art, err := h.writer.Write(ctx, WriteRequest{Organization: org, Project: project, RunID: runID, Content: content})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	// The owner reads it back: found, with the exact bytes and object key.
	gotArt, body, found, err := h.writer.Read(ctx, org, project, art.ID)
	if err != nil {
		t.Fatalf("owner Read() error = %v", err)
	}
	if !found {
		t.Fatalf("owner Read() found = false, want the owner's own artifact")
	}
	if gotArt.ObjectKey != art.ObjectKey || !bytes.Equal(body, content) {
		t.Fatalf("owner Read() = {key:%s bytes:%q}, want {key:%s bytes:%q}", gotArt.ObjectKey, body, art.ObjectKey, content)
	}

	// A second tenant asking for the SAME artifact id gets a miss: no bytes, no error.
	otherOrg, otherProject := newID("org"), newID("prj")
	_, foreignBody, foreignFound, err := h.writer.Read(ctx, otherOrg, otherProject, art.ID)
	if err != nil {
		t.Fatalf("foreign Read() error = %v, want a clean miss", err)
	}
	if foreignFound || foreignBody != nil {
		t.Fatalf("foreign Read() found = %v (bytes=%q), want a miss with no existence disclosure", foreignFound, foreignBody)
	}

	// A truly-missing id for the owner returns the identical miss shape.
	_, _, missingFound, err := h.writer.Read(ctx, org, project, "art_does_not_exist")
	if err != nil {
		t.Fatalf("missing Read() error = %v, want a clean miss", err)
	}
	if missingFound {
		t.Fatalf("missing Read() found = true, want a miss")
	}
}

// seedExpiredStoreFalseRun creates org -> project -> session -> store:false terminal
// response (aged an hour, so any sub-hour TTL reaps it) -> run keyed to that response,
// and returns the scope and run id an artifact produced by the run must reference.
func (h *artifactsHarness) seedExpiredStoreFalseRun(t *testing.T) (org, project, runID string) {
	t.Helper()
	org, project = newID("org"), newID("prj")
	session := newID("ses")
	respID := newID("resp")
	runID = newID("run")
	h.exec(t, `INSERT INTO organizations (id) VALUES ($1)`, org)
	h.exec(t, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org)
	h.exec(t, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, session, org, project)
	h.exec(t, `INSERT INTO responses (id, organization_id, project_id, session_id, state, input, store, updated_at)
		VALUES ($1, $2, $3, $4, 'completed', '{}', false, clock_timestamp() - interval '1 hour')`,
		respID, org, project, session)
	h.exec(t, `INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state)
		VALUES ($1, $2, $3, $4, $5, 'completed')`, runID, org, project, session, respID)
	return org, project, runID
}

// TestStoreFalsePurgeDeletesArtifactBytes proves the retention sweep genuinely erases the
// object bytes of an expired store:false run, closing the step that was vacuous before this
// task — the DB scrub cleared the row's object_key but the S3 object lingered. The reaper
// wired with the real object store deletes the bytes and tombstones the row (spec §8.3, §20.9,
// LP §7.2), the artifact parallel of the response-content purge.
func TestStoreFalsePurgeDeletesArtifactBytes(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, runID := h.seedExpiredStoreFalseRun(t)

	content := []byte("store:false run terminal output — must not survive retention")
	art, err := h.writer.Write(ctx, WriteRequest{Organization: org, Project: project, RunID: runID, Content: content})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	// Precondition: the bytes are really in the object store before the sweep.
	if _, found, err := h.s3.Get(ctx, art.ObjectKey); err != nil || !found {
		t.Fatalf("precondition: artifact bytes absent before purge (found=%v, err=%v)", found, err)
	}

	// One sweep reaps the hour-old store:false response and deletes its artifact bytes.
	reaper := execution.NewReaper(h.repo, time.Minute).WithArtifactStore(h.s3)
	purged, err := reaper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if purged == 0 {
		t.Fatalf("reaper purged 0 responses, want the expired store:false response reaped")
	}

	// The S3 object is gone — the byte-delete actually happened, not just the row scrub.
	if _, found, err := h.s3.Get(ctx, art.ObjectKey); err != nil || found {
		t.Fatalf("artifact bytes survived the store:false purge (found=%v, err=%v)", found, err)
	}
	// And the row is tombstoned: object_key cleared, size zeroed.
	var (
		objectKey string
		size      int64
	)
	if err := h.pool.QueryRow(ctx,
		`SELECT object_key, size_bytes FROM artifacts WHERE id = $1 AND organization_id = $2 AND project_id = $3`,
		art.ID, org, project).Scan(&objectKey, &size); err != nil {
		t.Fatalf("read artifact row error = %v", err)
	}
	if objectKey != "" || size != 0 {
		t.Fatalf("artifact row not scrubbed after purge: object_key=%q size=%d", objectKey, size)
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}
