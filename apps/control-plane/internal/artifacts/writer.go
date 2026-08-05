package artifacts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// Artifact is an immutable, versioned output persisted by the write-path (spec §22.6):
// the durable row's identity, its object key, and the size/checksum that let a reader
// verify integrity, plus the §22.6 classification (media/logical type) its first producer
// — T5's changeset patch/test-log — now fills.
type Artifact struct {
	ID          string
	RunID       string
	ObjectKey   string
	SizeBytes   int64
	Checksum    string
	MediaType   string
	LogicalType string
}

// notScanned is the honest malware-scan status the write-path records: the §22.6 column
// exists, but no malware scanner is wired yet, so an artifact is marked not-scanned rather
// than claiming a clean result. ponytail: wiring a real scanner is a later concern; the
// column is here so a producer never has to backfill it.
const notScanned = "not_scanned"

// WriteRequest is one artifact to persist: the verified tenant scope, the run that
// produced it, its bytes, and the §22.6 classification. Scope comes from the caller's
// identity, never a body field (spec §39.2), which is why it is passed explicitly.
// MediaType/LogicalType/Provenance are optional — a caller with no classification leaves
// them zero and the row stores the empty defaults.
type WriteRequest struct {
	Project     string
	RunID       string
	Content     []byte
	MediaType   string         // e.g. text/x-diff, text/plain (§22.6)
	LogicalType string         // report/patch/diff/log/test-result (§22.6)
	Provenance  map[string]any // links back to the producing changeset/run/tool (§22.6)
}

// Writer persists artifacts: bytes to the object Store, then an index row in Postgres.
type Writer struct {
	store *Store
	pool  *pgxpool.Pool
}

// NewWriter binds the object store and the durable pool the write-path uses.
func NewWriter(store *Store, pool *pgxpool.Pool) *Writer {
	return &Writer{store: store, pool: pool}
}

// Write commits an artifact's bytes to the object store and then records its row. The
// object is written first so the row never points at absent bytes; a failure between the
// two leaves an orphan object (no row references it), which retention never reaches. That
// orphan is not a leak: the artifact orphan-GC (Collector) reconciles the bucket against the
// artifacts rows and reclaims any object no non-empty object_key row references — not a
// correctness break either way, the row is the index.
func (w *Writer) Write(ctx context.Context, req WriteRequest) (Artifact, error) {
	if req.Project == "" || req.RunID == "" {
		return Artifact{}, errors.New("artifacts: write requires a project and a run")
	}
	// Scope the INSERT to the row's own tenant so migration 000029's FORCE ROW LEVEL SECURITY admits it
	// (its WITH CHECK reads palai.project_id). ScopeToTenant defers to an already-scoped
	// context, so the production path (a request, or WriteArtifact, which scopes before calling Write) is
	// unchanged; a direct caller with an unscoped context — the write-path's own tests — is scoped to the
	// tenant it is writing for, exactly as Read and WriteArtifact already do.
	ctx = storage.ScopeToTenant(ctx, req.Project)
	id := newArtifactID()
	key := objectKey(req.Project, req.RunID, id)
	// ponytail: the object PUT lands before the RLS-checked INSERT, so an internal caller whose ctx scope (A)
	// differed from req's tenant (B) would write bytes under B's prefix that the INSERT then rejects — orphan
	// bytes, rowless (unreadable via the API, reclaimed by the orphan-GC), never a cross-tenant read. No
	// request-path caller can produce that mismatch (ScopeToTenant above derives the scope from req itself).
	// Upgrade path if an internal caller ever can: INSERT-before-PUT, or a compensating delete on INSERT
	// failure — deferred to E13-H.
	checksum, size, err := w.store.Put(ctx, key, req.Content)
	if err != nil {
		return Artifact{}, err
	}
	provenance, err := json.Marshal(provenanceOrEmpty(req.Provenance))
	if err != nil {
		return Artifact{}, fmt.Errorf("marshal artifact provenance: %w", err)
	}
	if _, err := w.pool.Exec(ctx, storage.Query("InsertArtifact"),
		id, req.Project, req.RunID, key, size, checksum,
		req.MediaType, req.LogicalType, notScanned, provenance); err != nil {
		return Artifact{}, fmt.Errorf("record artifact row: %w", err)
	}
	return Artifact{
		ID: id, RunID: req.RunID, ObjectKey: key, SizeBytes: size, Checksum: checksum,
		MediaType: req.MediaType, LogicalType: req.LogicalType,
	}, nil
}

// WriteArtifact is the primitive-arg write the changeset compiler drives through (its
// execution.ArtifactWriter seam): it persists content with its §22.6 classification and returns the
// artifact id. Keeping the params primitive lets execution depend on this without importing the
// artifacts package (the same decoupling retention's ArtifactDeleter uses).
func (w *Writer) WriteArtifact(ctx context.Context, project, runID string, content []byte, mediaType, logicalType string, provenance map[string]any) (string, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	art, err := w.Write(ctx, WriteRequest{
		Project: project, RunID: runID, Content: content,
		MediaType: mediaType, LogicalType: logicalType, Provenance: provenance,
	})
	if err != nil {
		return "", err
	}
	return art.ID, nil
}

// provenanceOrEmpty returns a non-nil provenance map so the JSONB column is a `{}` object
// rather than SQL null when a caller records no links.
func provenanceOrEmpty(p map[string]any) map[string]any {
	if p == nil {
		return map[string]any{}
	}
	return p
}

// Read resolves an artifact within the tenant scope and returns its row and bytes. found
// is false for an unknown or foreign id (the tenant-scoped GetArtifact returns no row),
// so a caller renders the same miss whether the artifact is absent or owned by another
// tenant — no cross-tenant existence leaks (spec §22.6, the retrieval non-disclosure rule).
func (w *Writer) Read(ctx context.Context, project, artifactID string) (Artifact, []byte, bool, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	art := Artifact{ID: artifactID}
	err := w.pool.QueryRow(ctx, storage.Query("GetArtifact"), artifactID, project).
		Scan(&art.RunID, &art.ObjectKey, &art.SizeBytes, &art.Checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		return Artifact{}, nil, false, nil
	}
	if err != nil {
		return Artifact{}, nil, false, fmt.Errorf("read artifact row: %w", err)
	}
	body, found, err := w.store.Get(ctx, art.ObjectKey)
	if err != nil {
		return Artifact{}, nil, false, err
	}
	if !found {
		// The row indexes an object the store no longer holds (e.g. a retention delete that
		// raced the read). Surface it as a miss, not a half-read.
		return Artifact{}, nil, false, nil
	}
	return art, body, true, nil
}

// InboundImageLogicalType is the §22.6 logical type of an image an integration fetched from the
// outside world on a user's behalf. It is its own type, not "report"/"patch"/"log": these bytes are
// UNTRUSTED THIRD-PARTY INPUT rather than something a run produced, and an operator reading the
// artifacts table has to be able to tell those apart.
const InboundImageLogicalType = "inbound-image"

// inboundObjectKeyPrefix is where an inbound artifact's bytes live, in place of the run id an
// artifact produced by a run is keyed under. It has to be run-less for the same reason the row is
// (see WriteInboundArtifact): the run does not exist when the bytes are written.
const inboundObjectKeyPrefix = "inbound"

// WriteInboundArtifact persists bytes an integration fetched on a user's behalf under an id the
// CALLER derived, idempotently, with no run attached yet. AttachArtifactRun binds it once the run is
// real; the SQL comments on InsertInboundArtifact carry the full ordering argument.
//
// It is deliberately NOT Write: Write mints a random id and demands a run, and both would break the
// admission's idempotency (a redelivery must re-derive the same id) and its ordering (the row must
// exist before the run's dispatch is committed).
//
// The bytes are PUT before the row is inserted, matching Write: a failure between them leaves an
// object no row references, which the orphan GC reclaims. mediaType must be the caller's SNIFFED
// type — a filename or a sender-declared mimetype is not evidence of what bytes are.
//
// provenance MUST carry no credential. It is stored, read back by operators, and served over the
// retrieval API; a fetch token that reached it would be a token in the database.
func (w *Writer) WriteInboundArtifact(ctx context.Context, project, artifactID string, content []byte, mediaType string, provenance map[string]any) error {
	if project == "" || artifactID == "" {
		return errors.New("artifacts: an inbound artifact write requires a project and an id")
	}
	ctx = storage.ScopeToTenant(ctx, project)
	key := objectKey(project, inboundObjectKeyPrefix, artifactID)
	checksum, size, err := w.store.Put(ctx, key, content)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(provenanceOrEmpty(provenance))
	if err != nil {
		return fmt.Errorf("marshal artifact provenance: %w", err)
	}
	if _, err := w.pool.Exec(ctx, storage.Query("InsertInboundArtifact"),
		artifactID, project, key, size, checksum,
		mediaType, InboundImageLogicalType, notScanned, encoded); err != nil {
		return fmt.Errorf("record inbound artifact row: %w", err)
	}
	return nil
}

// AttachArtifactRun binds an inbound artifact to the run admitted for it, which is what brings the
// row inside retention's reach (§22.2 purges an artifact through its run). Idempotent and one-way:
// only a NULL run_id is filled, so a redelivery is a no-op and no artifact can be re-pointed at a
// different run.
func (w *Writer) AttachArtifactRun(ctx context.Context, project, artifactID, runID string) error {
	ctx = storage.ScopeToTenant(ctx, project)
	if _, err := w.pool.Exec(ctx, storage.Query("AttachArtifactRun"), artifactID, project, runID); err != nil {
		return fmt.Errorf("attach artifact %s to run: %w", artifactID, err)
	}
	return nil
}

// ReadImageArtifact resolves an artifact to its media type and bytes within the tenant scope — the
// execution.ImageReader seam a run's `image_ref` content item resolves through, so the control plane
// can join an image to a provider request without the engine ever seeing the bytes (spec §24).
//
// found is false for an unknown id, a FOREIGN id, and a row whose object the store no longer holds
// (retention reaped it). All three read the same way on purpose: an id out of a run's input is
// untrusted content, and a caller must not be able to tell a foreign artifact from a missing one
// (§22.6 non-disclosure). The caller renders the miss as a marker in the conversation.
//
// The media type is returned as RECORDED, and its caller is what enforces that it is an image — the
// row is the only claim about these bytes that a producer, not a sender, made.
func (w *Writer) ReadImageArtifact(ctx context.Context, project, artifactID string) (string, []byte, bool, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	var runID *string
	var objectKey, checksum, mediaType, logicalType, scanStatus string
	var size int64
	var createdAt time.Time
	err := w.pool.QueryRow(ctx, storage.Query("ArtifactByID"), artifactID, project).
		Scan(&runID, &objectKey, &size, &checksum, &mediaType, &logicalType, &scanStatus, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("read artifact row: %w", err)
	}
	if objectKey == "" {
		return "", nil, false, nil // retention scrubbed the row's index of its bytes
	}
	content, found, err := w.store.Get(ctx, objectKey)
	if err != nil {
		return "", nil, false, err
	}
	if !found {
		return "", nil, false, nil
	}
	return mediaType, content, true, nil
}

// ReadRunArtifact resolves an artifact THE NAMED RUN PRODUCED, within the tenant scope, refusing to read the
// bytes of anything over maxBytes.
//
// It exists as its own method rather than as Read plus two checks at the call site, and both halves of that
// are deliberate:
//
//   - THE RUN IS PART OF THE KEY. Its caller is handed an artifact id that a MODEL wrote into its answer, and
//     tenant scoping alone would let one run publish another run's artifact — same tenant, different
//     conversation, and a screenshot from somebody else's thread posted into this one. The run id comes from
//     the delivery row, which the model never touched.
//   - THE SIZE IS CHECKED BEFORE THE BYTES ARE READ. The row already knows how big the object is, so a 2 GB
//     build log is refused for the cost of one SELECT instead of being pulled into the control plane's heap
//     and then thrown away. That is the difference between a ceiling and a formality.
//
// THE THREE ANSWERS, and they are distinguished by the RETURN VALUES rather than by a sentinel error, because
// this package cannot export one to its caller: internal/extensions consumes this seam and internal/execution
// imports extensions, so an extensions -> artifacts import closes a cycle. The contract is therefore explicit:
//
//   - found=false, no bytes: unknown id, another tenant's id, another run's id, or bytes retention already
//     reclaimed. ALL FOUR LOOK IDENTICAL, which is the §22.6 non-disclosure rule applied to a lookup key an
//     outsider chose.
//   - found=true, size>maxBytes, NO BYTES: it exists and it is too big. The caller says so out loud (E22 T5 —
//     an artifact too big to publish earns an honest sentence, never a silent drop), which is why size comes
//     back rather than being swallowed.
//   - found=true, size<=maxBytes: the bytes.
func (w *Writer) ReadRunArtifact(ctx context.Context, project, runID, artifactID string, maxBytes int64) ([]byte, int64, bool, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	// run_id is NULLABLE — an inbound artifact is written before the run it belongs to exists
	// (WriteInboundArtifact) — so it is scanned as a pointer. A NULL owner matches no run, which is the
	// fail-closed answer: an artifact attached to nothing is not this run's to publish.
	var owner *string
	var key, checksum string
	var size int64
	err := w.pool.QueryRow(ctx, storage.Query("GetArtifact"), artifactID, project).
		Scan(&owner, &key, &size, &checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("read artifact row: %w", err)
	}
	if owner == nil || *owner != runID || key == "" {
		// A foreign run's artifact, or a row retention has scrubbed of its bytes. Both are a miss.
		return nil, 0, false, nil
	}
	if size > maxBytes {
		return nil, size, true, nil
	}
	body, found, err := w.store.Get(ctx, key)
	if err != nil {
		return nil, size, false, err
	}
	if !found {
		return nil, 0, false, nil
	}
	return body, size, true, nil
}

// objectKey lays out the S3 key tenant-first so keys never collide across tenants and a
// bucket listing groups a project's objects together. The DB read is the authoritative
// tenant gate; this layout is defense in depth. A.2 Task 6 dropped the organization
// segment; every read goes through the key the row already stores, never a re-derived one.
func objectKey(project, runID, artifactID string) string {
	return fmt.Sprintf("%s/%s/%s", project, runID, artifactID)
}

// newArtifactID mints a random, unguessable artifact id. TEXT primary key, no format
// constraint; the "art_" prefix matches the resource-id shape used across the spine.
func newArtifactID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return "art_" + hex.EncodeToString(raw[:])
}
