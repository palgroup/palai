package artifacts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// Artifact is an immutable, versioned output persisted by the write-path (spec §22.6):
// the durable row's identity, its object key, and the size/checksum that let a reader
// verify integrity. The richer §22.6 fields (media type, logical type, malware-scan
// status, provenance links) arrive with their first producer in T5's changeset path;
// ponytail: no columns and no struct fields without a writer that fills them.
type Artifact struct {
	ID        string
	RunID     string
	ObjectKey string
	SizeBytes int64
	Checksum  string
}

// WriteRequest is one artifact to persist: the verified tenant scope, the run that
// produced it, and its bytes. Scope comes from the caller's identity, never a body
// field (spec §39.2), which is why it is passed explicitly.
type WriteRequest struct {
	Organization string
	Project      string
	RunID        string
	Content      []byte
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
// two leaves an orphan object (no row references it), which retention never reaches.
// ponytail: orphan objects from a mid-write crash are swept by the same list-vs-rows
// reconcile the retention path defers — not a correctness break, the row is the index.
func (w *Writer) Write(ctx context.Context, req WriteRequest) (Artifact, error) {
	if req.Organization == "" || req.Project == "" || req.RunID == "" {
		return Artifact{}, errors.New("artifacts: write requires organization, project, and run")
	}
	id := newArtifactID()
	key := objectKey(req.Organization, req.Project, req.RunID, id)
	checksum, size, err := w.store.Put(ctx, key, req.Content)
	if err != nil {
		return Artifact{}, err
	}
	if _, err := w.pool.Exec(ctx, storage.Query("InsertArtifact"),
		id, req.Organization, req.Project, req.RunID, key, size, checksum); err != nil {
		return Artifact{}, fmt.Errorf("record artifact row: %w", err)
	}
	return Artifact{ID: id, RunID: req.RunID, ObjectKey: key, SizeBytes: size, Checksum: checksum}, nil
}

// Read resolves an artifact within the tenant scope and returns its row and bytes. found
// is false for an unknown or foreign id (the tenant-scoped GetArtifact returns no row),
// so a caller renders the same miss whether the artifact is absent or owned by another
// tenant — no cross-tenant existence leaks (spec §22.6, the retrieval non-disclosure rule).
func (w *Writer) Read(ctx context.Context, org, project, artifactID string) (Artifact, []byte, bool, error) {
	art := Artifact{ID: artifactID}
	err := w.pool.QueryRow(ctx, storage.Query("GetArtifact"), artifactID, org, project).
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

// objectKey lays out the S3 key tenant-first so keys never collide across tenants and a
// bucket listing groups an org's objects together. The DB read is the authoritative
// tenant gate; this layout is defense in depth.
func objectKey(org, project, runID, artifactID string) string {
	return fmt.Sprintf("%s/%s/%s/%s", org, project, runID, artifactID)
}

// newArtifactID mints a random, unguessable artifact id. TEXT primary key, no format
// constraint; the "art_" prefix matches the resource-id shape used across the spine.
func newArtifactID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return "art_" + hex.EncodeToString(raw[:])
}
