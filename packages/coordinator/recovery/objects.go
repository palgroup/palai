// Package recovery persists the durable recovery objects (spec §26.1-26.2): the engine checkpoint
// metadata and its shared transcript boundary, written as SEPARATE immutable rows. It is the DB
// half only — it never touches the object store (the checkpoint BYTES live in the artifact store,
// whose credential is control-plane-only, spec §24); the control plane PUTs the bytes and passes
// this the resulting object key + checksum + size. The checkpoint content stays opaque here (§26.2):
// this records where the bytes live and how to verify them, never what they mean.
package recovery

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// MaxCheckpointBytes bounds a persisted checkpoint (spec §26.2 size-bound). The engine's offer rides
// a <=1 MiB engine frame, so a real checkpoint is already frame-capped; this bound guards the direct
// persist path so an oversized checkpoint is rejected BEFORE the object-store PUT, leaving no orphan.
const MaxCheckpointBytes = 1 << 20

// ErrCheckpointExists reports that the checkpoint id is already written. A checkpoint is immutable
// (spec §26.1): a second write to the same id is rejected, never an overwrite.
var ErrCheckpointExists = errors.New("checkpoint_exists")

// ErrCheckpointTooLarge reports a checkpoint over MaxCheckpointBytes (spec §26.2 size-bound).
var ErrCheckpointTooLarge = errors.New("checkpoint_too_large")

// ErrChecksumRequired reports a persist with no content checksum: a checkpoint without integrity
// coverage is not durable (spec §26.2), so it is refused rather than stored unverifiable.
var ErrChecksumRequired = errors.New("checkpoint_checksum_required")

// Objects persists checkpoints + their transcript boundaries against the durable spine.
type Objects struct {
	pool *pgxpool.Pool
}

// New binds the persistence layer to the durable pool.
func New(pool *pgxpool.Pool) *Objects {
	return &Objects{pool: pool}
}

// PersistInput is one checkpoint to persist with its §26.2 metadata. The control plane mints the ids,
// resolves config/transcript/engine provenance, and PUTs the bytes; this records the immutable rows.
// WorkspaceSnapshotID is empty when the checkpoint declares no workspace dependency (§26.4) — stored
// as SQL NULL, so no snapshot is implied.
type PersistInput struct {
	CheckpointID        string
	BoundaryID          string
	Organization        string
	Project             string
	RunID               string
	AttemptID           string
	EngineDigest        string
	EngineVersion       string
	ProtocolVersion     string
	Format              string
	FormatVersion       int
	ConfigSnapshotHash  string
	TranscriptSequence  int64
	WorkspaceSnapshotID string
	ContentChecksum     string
	ObjectKey           string
	SizeBytes           int64
	// PendingOperations is the run's unresolved (uncertain/manual_resolution) tool operations at the
	// boundary as a JSON array (spec §26.2, §26.4, E10 T7). Nil/empty is normalised to '[]' so the column
	// is always a well-formed array a RESTORE can read back.
	PendingOperations []byte
}

// Persist records the transcript boundary and the checkpoint as two rows in one transaction, so a
// rejected checkpoint (duplicate id) leaves no orphan boundary behind. It enforces the §26.2
// invariants the control plane relies on: a checksum is present, the size is bounded, and the id is
// written at most once (immutability).
func (o *Objects) Persist(ctx context.Context, in PersistInput) error {
	if in.ContentChecksum == "" {
		return ErrChecksumRequired
	}
	if in.SizeBytes < 0 || in.SizeBytes > MaxCheckpointBytes {
		return ErrCheckpointTooLarge
	}

	tx, err := o.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin checkpoint tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, storage.Query("InsertTranscriptBoundary"),
		in.BoundaryID, in.RunID, in.AttemptID, in.Organization, in.Project, in.TranscriptSequence); err != nil {
		return fmt.Errorf("insert transcript boundary: %w", err)
	}

	pendingOps := in.PendingOperations
	if len(pendingOps) == 0 {
		pendingOps = []byte("[]") // never null: a RESTORE reads a well-formed array
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertCheckpoint"),
		in.CheckpointID, in.RunID, in.AttemptID, in.BoundaryID, in.Organization, in.Project,
		in.EngineDigest, in.EngineVersion, in.ProtocolVersion, in.Format, in.FormatVersion,
		in.ConfigSnapshotHash, in.TranscriptSequence, nullableText(in.WorkspaceSnapshotID),
		in.ContentChecksum, in.ObjectKey, in.SizeBytes, pendingOps); err != nil {
		if isUniqueViolation(err) {
			return ErrCheckpointExists
		}
		return fmt.Errorf("insert checkpoint: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit checkpoint tx: %w", err)
	}
	return nil
}

// nullableText maps "" to SQL NULL so an absent optional reference (e.g. a checkpoint with no
// workspace snapshot) stores as NULL, not an empty string that a FK would reject.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// isUniqueViolation reports whether err is a Postgres unique_violation (23505) — the id PK rejecting
// a second write to an immutable checkpoint.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
