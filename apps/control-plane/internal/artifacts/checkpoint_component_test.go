//go:build component

// Checkpoint create-path component tests (spec §26.1-26.2, E10 Task 1). They run in the artifacts
// package because the checkpoint bytes live in the SAME object store as artifacts (whose S3
// credential is control-plane-only, §24) — so the real SeaweedFS + real Postgres the artifacts
// suite already stands up are exactly what the checkpoint sink needs. They exercise the
// execution.CheckpointSink seam directly (the orchestrator's checkpoint.offer handler is thin glue
// over it), against real infrastructure.

package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/coordinator/recovery"
)

// seedRunWithAttempt creates org -> project -> session -> run -> attempt and returns the ids a
// checkpoint FKs (the checkpoint references the fenced attempt that authored it).
func (h *artifactsHarness) seedRunWithAttempt(t *testing.T) (org, project, session, runID, attemptID string) {
	t.Helper()
	org, project = newID("org"), newID("prj")
	session = newID("ses")
	runID = newID("run")
	attemptID = newID("att")
	h.exec(t, `INSERT INTO organizations (id) VALUES ($1)`, org)
	h.exec(t, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org)
	h.exec(t, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, session, org, project)
	h.exec(t, `INSERT INTO runs (id, organization_id, project_id, session_id) VALUES ($1, $2, $3, $4)`, runID, org, project, session)
	h.exec(t, `INSERT INTO attempts (id, organization_id, project_id, run_id, fence, state) VALUES ($1, $2, $3, $4, 1, 'assigned')`,
		attemptID, org, project, runID)
	return org, project, session, runID, attemptID
}

// offerFrameData builds the data map an engine checkpoint.offer carries: format + format_version +
// boundary_kind + the opaque bytes base64-encoded (spec §26.2). format_version rides as a JSON
// number (float64), matching a real frame that crossed the transport.
func offerFrameData(rawState []byte) map[string]any {
	return map[string]any{
		"format":         "reference-kernel",
		"format_version": float64(1),
		"boundary_kind":  "tool",
		"state":          base64.StdEncoding.EncodeToString(rawState),
	}
}

// TestCheckpointOfferPersistsImmutableRowAndBytes proves a checkpoint.offer becomes a durable row +
// an S3 object whose checksum matches, and that a retransmitted offer (same run/attempt/sequence) is
// rejected — the checkpoint is written at most once (spec §26.1-26.2).
func TestCheckpointOfferPersistsImmutableRowAndBytes(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, _, runID, attemptID := h.seedRunWithAttempt(t)
	sink := execution.NewCheckpointSink(h.s3, recovery.New(h.pool))

	rawState := []byte(`{"state":"awaiting_model","step":2,"pending_tools":[]}`)
	meta := execution.CheckpointMeta{
		Organization: org, Project: project, RunID: runID, AttemptID: attemptID, OfferSequence: 12,
		EngineVersion: "0.1.0", ProtocolVersion: "engine.v1",
	}

	if err := sink.Persist(ctx, meta, offerFrameData(rawState)); err != nil {
		t.Fatalf("Persist() error = %v", err)
	}

	// The row is present and points at the object; the recorded checksum is the SHA-256 of the bytes.
	var objectKey, checksum string
	var size int64
	if err := h.pool.QueryRow(ctx,
		`SELECT object_key, content_checksum, size_bytes FROM checkpoints WHERE run_id=$1 AND organization_id=$2 AND project_id=$3`,
		runID, org, project).Scan(&objectKey, &checksum, &size); err != nil {
		t.Fatalf("read checkpoint row: %v", err)
	}
	wantSum := sha256.Sum256(rawState)
	if checksum != "sha256:"+hex.EncodeToString(wantSum[:]) {
		t.Fatalf("checkpoint checksum %q is not the SHA-256 of the opaque bytes", checksum)
	}
	if size != int64(len(rawState)) {
		t.Fatalf("size = %d, want %d", size, len(rawState))
	}

	// The S3 object holds EXACTLY the opaque bytes the engine offered.
	body, found, err := h.s3.Get(ctx, objectKey)
	if err != nil || !found {
		t.Fatalf("Get(%q) found=%v err=%v", objectKey, found, err)
	}
	if string(body) != string(rawState) {
		t.Fatalf("stored bytes = %q, want the offered %q", body, rawState)
	}

	// Immutable: the SAME offer re-derives the same id and is rejected, not written twice.
	if err := sink.Persist(ctx, meta, offerFrameData(rawState)); err != recovery.ErrCheckpointExists {
		t.Fatalf("second Persist() = %v, want ErrCheckpointExists", err)
	}
	var rows int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM checkpoints WHERE run_id=$1`, runID).Scan(&rows); err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	if rows != 1 {
		t.Fatalf("checkpoint rows = %d, want 1 (immutable, no duplicate on retransmit)", rows)
	}
}

// TestCheckpointMigrationPreservesOriginalWithProvenance proves the ENG-011 migration mechanism
// (spec §26.2): migrating a v1 checkpoint to v2 persists a NEW immutable row alongside the original —
// the original row + bytes stay byte-for-byte UNTOUCHED (immutability, and the rollback: the original
// id remains integrity-valid and restore-selectable). The new row carries a new content_checksum +
// format_version=2, and a provenance journal event checkpoint.migrated.v1 {from_id,to_id,from_format,
// to_format} links them.
//
// Honest ceiling: the mechanism is proven with the reference-kernel v2 SHAPE; the transform itself is
// engine-owned (checkpoint.migrate, proven in engine pytest) and the control plane treats the bytes
// opaquely (§26.2). The PRODUCTION engine stays v1 — engine.ready.checkpoint_formats is still
// ["reference-kernel/1"] (schema-pin unchanged); no live run migrates, this is the reversible-migration seam.
func TestCheckpointMigrationPreservesOriginalWithProvenance(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, session, runID, attemptID := h.seedRunWithAttempt(t)
	sink := execution.NewCheckpointSink(h.s3, recovery.New(h.pool))
	tenant := coordinator.Tenant{Organization: org, Project: project}

	// Persist the v1 checkpoint.
	v1Bytes := []byte(`{"pending_tools":["tcall_x"],"state":"awaiting_tools","step":1}`)
	metaV1 := execution.CheckpointMeta{
		Organization: org, Project: project, RunID: runID, AttemptID: attemptID, OfferSequence: 10,
		EngineVersion: "0.1.0", ProtocolVersion: "engine.v1",
	}
	if err := sink.Persist(ctx, metaV1, offerFrameData(v1Bytes)); err != nil {
		t.Fatalf("Persist(v1) error = %v", err)
	}
	var fromID, v1Key, v1Checksum string
	var v1Version int
	if err := h.pool.QueryRow(ctx,
		`SELECT id, object_key, content_checksum, format_version FROM checkpoints WHERE run_id=$1`,
		runID).Scan(&fromID, &v1Key, &v1Checksum, &v1Version); err != nil {
		t.Fatalf("read v1 checkpoint: %v", err)
	}

	// The engine-owned transform yields a v2 SHAPE (opaque to the control plane) — mirroring
	// checkpoint.migrate's output: the v1 state plus the explicit state_version marker v1 lacked.
	v2Bytes := []byte(`{"pending_tools":["tcall_x"],"state":"awaiting_tools","state_version":2,"step":1}`)
	v2Offer := map[string]any{
		"format": "reference-kernel", "format_version": float64(2), "boundary_kind": "request",
		"state": base64.StdEncoding.EncodeToString(v2Bytes),
	}
	metaV2 := metaV1
	metaV2.OfferSequence = 11 // a distinct offer sequence -> a distinct immutable id

	toID, err := execution.MigrateCheckpoint(ctx, sink, h.repo.Spine(), tenant, session, "", fromID, "reference-kernel/1", metaV2, v2Offer)
	if err != nil {
		t.Fatalf("MigrateCheckpoint() error = %v", err)
	}
	if toID == fromID {
		t.Fatal("migrated checkpoint id must differ from the original")
	}

	// TWO separate immutable rows.
	var rows int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM checkpoints WHERE run_id=$1`, runID).Scan(&rows); err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	if rows != 2 {
		t.Fatalf("checkpoint rows = %d, want 2 (original + migrated, both immutable)", rows)
	}

	// The ORIGINAL row is byte-for-byte untouched: same version/key/checksum, and the S3 bytes intact.
	var origVersion int
	var origKey, origChecksum string
	if err := h.pool.QueryRow(ctx,
		`SELECT format_version, object_key, content_checksum FROM checkpoints WHERE id=$1`, fromID).
		Scan(&origVersion, &origKey, &origChecksum); err != nil {
		t.Fatalf("read original checkpoint: %v", err)
	}
	if origVersion != 1 || origKey != v1Key || origChecksum != v1Checksum {
		t.Fatalf("original checkpoint mutated: version=%d key=%q checksum=%q", origVersion, origKey, origChecksum)
	}
	origBytes, found, err := h.s3.Get(ctx, v1Key)
	if err != nil || !found || string(origBytes) != string(v1Bytes) {
		t.Fatalf("original bytes changed: found=%v err=%v", found, err)
	}

	// The NEW row: format_version 2, a NEW content_checksum.
	var newVersion int
	var newChecksum string
	if err := h.pool.QueryRow(ctx,
		`SELECT format_version, content_checksum FROM checkpoints WHERE id=$1`, toID).
		Scan(&newVersion, &newChecksum); err != nil {
		t.Fatalf("read migrated checkpoint: %v", err)
	}
	if newVersion != 2 {
		t.Fatalf("migrated format_version = %d, want 2", newVersion)
	}
	if newChecksum == v1Checksum {
		t.Fatal("migrated checkpoint must carry a NEW content_checksum (distinct bytes)")
	}

	// Provenance journal event linking the two.
	var payload []byte
	if err := h.pool.QueryRow(ctx,
		`SELECT payload FROM events WHERE session_id=$1 AND type='checkpoint.migrated.v1'`, session).Scan(&payload); err != nil {
		t.Fatalf("read provenance event: %v", err)
	}
	var prov struct {
		FromID     string `json:"from_id"`
		ToID       string `json:"to_id"`
		FromFormat string `json:"from_format"`
		ToFormat   string `json:"to_format"`
	}
	if err := json.Unmarshal(payload, &prov); err != nil {
		t.Fatalf("decode provenance: %v", err)
	}
	if prov.FromID != fromID || prov.ToID != toID || prov.FromFormat != "reference-kernel/1" || prov.ToFormat != "reference-kernel/2" {
		t.Fatalf("provenance = %+v, want from=%s to=%s formats reference-kernel/1->2", prov, fromID, toID)
	}

	// Rollback basis: the original row + bytes remain intact and integrity-valid — the durable basis a
	// rollback selects. (The ladder's LatestRunCheckpoint is newest-ONLY, so actually SELECTING the
	// older row on a rejected-newest is a follow-up when a real format bump lands — named as a ceiling
	// on LatestRunCheckpoint; here we prove the v1 object survives migration byte-for-byte.)
	sum := sha256.Sum256(origBytes)
	if "sha256:"+hex.EncodeToString(sum[:]) != v1Checksum {
		t.Fatal("original checkpoint is no longer integrity-valid — a rollback basis is impossible")
	}
}

// TestCheckpointRejectsEmptyState proves an offer with no state is refused rather than stored as a
// 0-byte object + size-0 immutable row (nothing to restore).
func TestCheckpointRejectsEmptyState(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, _, runID, attemptID := h.seedRunWithAttempt(t)
	sink := execution.NewCheckpointSink(h.s3, recovery.New(h.pool))
	meta := execution.CheckpointMeta{
		Organization: org, Project: project, RunID: runID, AttemptID: attemptID, OfferSequence: 3,
	}

	// An offer with no "state" field has nothing to persist.
	err := sink.Persist(ctx, meta, map[string]any{"format": "reference-kernel", "format_version": float64(1)})
	if !errors.Is(err, execution.ErrEmptyCheckpoint) {
		t.Fatalf("Persist(empty state) = %v, want ErrEmptyCheckpoint", err)
	}
	var rows int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM checkpoints WHERE run_id=$1`, runID).Scan(&rows); err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	if rows != 0 {
		t.Fatal("an empty checkpoint must leave NO row")
	}
}

// TestCheckpointMetadataCarriesSpecFields proves the persisted checkpoint carries the §26.2 metadata
// set faithfully, with config_snapshot_hash sourced from a REAL ConfigSnapshot and transcript_sequence
// from the REAL journal — never fabricated. The workspace-less checkpoint declares no workspace
// dependency (NULL) and pending_operations defaults to the empty T7-fills-it array.
func TestCheckpointMetadataCarriesSpecFields(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, session, runID, attemptID := h.seedRunWithAttempt(t)
	sink := execution.NewCheckpointSink(h.s3, recovery.New(h.pool))

	// A real effective ConfigSnapshot — its content-addressed hash is what the boundary records.
	snap := execution.Resolve(execution.ResolveInput{DeploymentModel: "cheap-1", ProjectTools: []string{"search"}})

	// A real journal: append events, then take the boundary at the current max seq.
	for seq := 1; seq <= 3; seq++ {
		h.exec(t, `INSERT INTO events (id, organization_id, project_id, session_id, seq, type, payload)
		           VALUES ($1, $2, $3, $4, $5, 'output.item.v1', '{}')`,
			newID("evt"), org, project, session, seq)
	}
	var journalSeq int64
	if err := h.pool.QueryRow(ctx, `SELECT max(seq) FROM events WHERE session_id=$1`, session).Scan(&journalSeq); err != nil {
		t.Fatalf("read journal seq: %v", err)
	}

	meta := execution.CheckpointMeta{
		Organization: org, Project: project, RunID: runID, AttemptID: attemptID, OfferSequence: 5,
		EngineDigest: "sha256:enginedigest", EngineVersion: "0.1.0", ProtocolVersion: "engine.v1",
		ConfigSnapshotHash: snap.Hash, TranscriptSequence: journalSeq,
	}
	if err := sink.Persist(ctx, meta, offerFrameData([]byte(`{"step":1}`))); err != nil {
		t.Fatalf("Persist() error = %v", err)
	}

	var (
		format, engineDigest, engineVersion, protocolVersion, configHash string
		formatVersion                                                    int
		transcriptSeq                                                    int64
		pendingOps                                                       string
		workspaceSnapshot                                                *string
	)
	if err := h.pool.QueryRow(ctx,
		`SELECT format, format_version, engine_digest, engine_version, protocol_version,
		        config_snapshot_hash, transcript_sequence, pending_operations::text, workspace_snapshot_id
		 FROM checkpoints WHERE run_id=$1`, runID).Scan(
		&format, &formatVersion, &engineDigest, &engineVersion, &protocolVersion,
		&configHash, &transcriptSeq, &pendingOps, &workspaceSnapshot); err != nil {
		t.Fatalf("read checkpoint metadata: %v", err)
	}

	if format != "reference-kernel" || formatVersion != 1 {
		t.Fatalf("format = %q/%d, want reference-kernel/1", format, formatVersion)
	}
	if engineDigest != "sha256:enginedigest" || engineVersion != "0.1.0" || protocolVersion != "engine.v1" {
		t.Fatalf("engine provenance = %q/%q/%q, want the handshake identity", engineDigest, engineVersion, protocolVersion)
	}
	if configHash != snap.Hash {
		t.Fatalf("config_snapshot_hash = %q, want the real ConfigSnapshot hash %q", configHash, snap.Hash)
	}
	if transcriptSeq != journalSeq {
		t.Fatalf("transcript_sequence = %d, want the real journal boundary %d", transcriptSeq, journalSeq)
	}
	if pendingOps != "[]" {
		t.Fatalf("pending_operations = %q, want the empty [] (T7 fills it)", pendingOps)
	}
	if workspaceSnapshot != nil {
		t.Fatalf("workspace_snapshot_id = %q, want NULL (no workspace dependency)", *workspaceSnapshot)
	}
}
