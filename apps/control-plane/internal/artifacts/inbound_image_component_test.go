//go:build component

package artifacts

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/storage"
)

// pngHeader is a real 1x1 PNG prefix — enough that a reader sniffing these bytes agrees they are an image.
var pngHeader = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89,
}

// TestArtifactInboundImageWriteIsIdempotentAtACallerChosenID proves the three properties the Slack image
// leg's ORDERING rests on, against real Postgres and a real object store:
//
//  1. The caller chooses the id and the write lands under it. This is what keeps a Slack redelivery a
//     REPLAY: the id enters the run input, the input is hashed into the idempotency reservation, and a
//     minted id would hash differently the second time.
//  2. Writing the SAME id twice is a no-op, not an error — which is exactly what a redelivery does.
//  3. run_id is NULL until it is attached, so the row can exist BEFORE the run does (artifacts.run_id
//     references runs(id), and the admission that creates the run also commits its dispatch outbox, so a
//     row written after the admission races the model step that reads it).
func TestArtifactInboundImageWriteIsIdempotentAtACallerChosenID(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, runID := h.seedRun(t)
	const artifactID = "art_deadbeefdeadbeefdeadbeefdeadbeef"

	if err := h.writer.WriteInboundArtifact(ctx, org, project, artifactID, pngHeader, "image/png",
		map[string]any{"source": "slack", "slack_file_id": "F1"}); err != nil {
		t.Fatalf("WriteInboundArtifact() error = %v", err)
	}

	// (1) + (3): the row is there, under the caller's id, with NO run attached yet.
	var storedRun *string
	var mediaType, logicalType string
	if err := h.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT run_id, media_type, logical_type FROM artifacts WHERE id = $1 AND organization_id = $2 AND project_id = $3`,
		artifactID, org, project).Scan(&storedRun, &mediaType, &logicalType); err != nil {
		t.Fatalf("read inbound artifact row error = %v", err)
	}
	if storedRun != nil {
		t.Fatalf("run_id = %q, want NULL before the run is attached", *storedRun)
	}
	if mediaType != "image/png" || logicalType != InboundImageLogicalType {
		t.Fatalf("classification = %s/%s, want image/png and %s", mediaType, logicalType, InboundImageLogicalType)
	}

	// (2) a redelivery re-derives the same id and must not fail.
	if err := h.writer.WriteInboundArtifact(ctx, org, project, artifactID, pngHeader, "image/png", nil); err != nil {
		t.Fatalf("second WriteInboundArtifact() (a redelivery) error = %v, want a silent no-op", err)
	}
	var rows int
	if err := h.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM artifacts WHERE id = $1`, artifactID).Scan(&rows); err != nil {
		t.Fatalf("count rows error = %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want exactly 1 after a redelivery", rows)
	}

	// The attach binds it to the run and is itself idempotent.
	if err := h.writer.AttachArtifactRun(ctx, org, project, artifactID, runID); err != nil {
		t.Fatalf("AttachArtifactRun() error = %v", err)
	}
	if err := h.writer.AttachArtifactRun(ctx, org, project, artifactID, runID); err != nil {
		t.Fatalf("second AttachArtifactRun() error = %v, want idempotent", err)
	}
	if err := h.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT run_id FROM artifacts WHERE id = $1`, artifactID).Scan(&storedRun); err != nil {
		t.Fatalf("re-read row error = %v", err)
	}
	if storedRun == nil || *storedRun != runID {
		t.Fatalf("run_id = %v, want %q after the attach", storedRun, runID)
	}
}

// TestArtifactAttachCannotRepointAnArtifactAtAnotherRun proves the attach only ever WIDENS NULL. Without the
// `run_id IS NULL` guard, a second attach would move an image between runs — and since the run is what
// retention purges through, that would move somebody's screenshot out from under its own response's purge.
func TestArtifactAttachCannotRepointAnArtifactAtAnotherRun(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, runID := h.seedRun(t)
	_, _, otherRun := h.seedRun(t)
	const artifactID = "art_00000000000000000000000000000001"

	if err := h.writer.WriteInboundArtifact(ctx, org, project, artifactID, pngHeader, "image/png", nil); err != nil {
		t.Fatalf("WriteInboundArtifact() error = %v", err)
	}
	if err := h.writer.AttachArtifactRun(ctx, org, project, artifactID, runID); err != nil {
		t.Fatalf("AttachArtifactRun() error = %v", err)
	}
	if err := h.writer.AttachArtifactRun(ctx, org, project, artifactID, otherRun); err != nil {
		t.Fatalf("re-attach error = %v, want a silent no-op", err)
	}
	var storedRun string
	if err := h.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT run_id FROM artifacts WHERE id = $1`, artifactID).Scan(&storedRun); err != nil {
		t.Fatalf("read row error = %v", err)
	}
	if storedRun != runID {
		t.Fatalf("run_id = %q, want it pinned to the first run %q", storedRun, runID)
	}
}

// TestReadImageArtifactIsTenantScopedAndSurvivesRetention proves the READ side a run's `image_ref` resolves
// through: the owner gets the media type and the exact bytes, a FOREIGN tenant asking for the same id gets
// the identical miss a missing id gives, and a row retention has scrubbed reads as a miss rather than as a
// half-image.
//
// The three cases reading the same way is the §22.6 non-disclosure rule, and it matters more here than on the
// retrieval API: the artifact id arrives inside a run's INPUT, which is untrusted content.
func TestReadImageArtifactIsTenantScopedAndSurvivesRetention(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, runID := h.seedRun(t)
	const artifactID = "art_00000000000000000000000000000002"

	if err := h.writer.WriteInboundArtifact(ctx, org, project, artifactID, pngHeader, "image/png", nil); err != nil {
		t.Fatalf("WriteInboundArtifact() error = %v", err)
	}
	if err := h.writer.AttachArtifactRun(ctx, org, project, artifactID, runID); err != nil {
		t.Fatalf("AttachArtifactRun() error = %v", err)
	}

	mediaType, content, found, err := h.writer.ReadImageArtifact(ctx, org, project, artifactID)
	if err != nil || !found {
		t.Fatalf("owner ReadImageArtifact() = found %v err %v, want the owner's own image", found, err)
	}
	if mediaType != "image/png" || !bytes.Equal(content, pngHeader) {
		t.Fatalf("read = %s/%d bytes, want image/png and the exact bytes written", mediaType, len(content))
	}

	// A foreign tenant, and a missing id, are the same miss.
	for _, tc := range []struct {
		name             string
		org, project, id string
	}{
		{"a foreign tenant asking for the same id", newID("org"), newID("prj"), artifactID},
		{"the owner asking for an id that does not exist", org, project, "art_does_not_exist"},
	} {
		_, body, gotFound, err := h.writer.ReadImageArtifact(ctx, tc.org, tc.project, tc.id)
		if err != nil {
			t.Fatalf("%s: err = %v, want a clean miss", tc.name, err)
		}
		if gotFound || body != nil {
			t.Fatalf("%s: found = %v bytes = %d, want a miss with no existence disclosure", tc.name, gotFound, len(body))
		}
	}

	// A retention-scrubbed row (object_key cleared) is a miss, not a read of absent bytes.
	if _, err := h.pool.Exec(storage.WithSystemScope(ctx),
		`UPDATE artifacts SET object_key = '', size_bytes = 0, checksum = '' WHERE id = $1`, artifactID); err != nil {
		t.Fatalf("scrub row error = %v", err)
	}
	if _, _, gotFound, err := h.writer.ReadImageArtifact(ctx, org, project, artifactID); err != nil || gotFound {
		t.Fatalf("scrubbed ReadImageArtifact() = found %v err %v, want a miss", gotFound, err)
	}
}

// TestInboundImageIsReachedByRetentionOnlyOnceAttached is the honest statement of the ceiling
// slack_vision.go names: retention reaches an artifact through `artifacts JOIN runs`, so an UNATTACHED row's
// bytes outlive its response's purge. It asserts both halves — the attached artifact IS purged, the
// unattached one is NOT — so the gap is a measured, documented fact rather than an assumption, and the day
// someone closes it this test says so by failing.
func TestInboundImageIsReachedByRetentionOnlyOnceAttached(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, runID := h.seedExpiredStoreFalseRun(t)
	const attachedID = "art_00000000000000000000000000000003"
	const orphanID = "art_00000000000000000000000000000004"

	for _, id := range []string{attachedID, orphanID} {
		if err := h.writer.WriteInboundArtifact(ctx, org, project, id, pngHeader, "image/png", nil); err != nil {
			t.Fatalf("WriteInboundArtifact(%s) error = %v", id, err)
		}
	}
	if err := h.writer.AttachArtifactRun(ctx, org, project, attachedID, runID); err != nil {
		t.Fatalf("AttachArtifactRun() error = %v", err)
	}

	// Precondition: both sets of bytes are really in the object store before the sweep.
	attachedObject := objectKey(org, project, inboundObjectKeyPrefix, attachedID)
	orphanObject := objectKey(org, project, inboundObjectKeyPrefix, orphanID)
	for _, key := range []string{attachedObject, orphanObject} {
		if _, found, err := h.s3.Get(ctx, key); err != nil || !found {
			t.Fatalf("precondition: %q absent before the purge (found=%v err=%v)", key, found, err)
		}
	}

	// The REAL retention sweep, wired the way main.go wires it — not a hand-written DELETE.
	reaper := execution.NewReaper(h.repo, time.Minute).WithArtifactStore(h.s3)
	if purged, err := reaper.Sweep(ctx); err != nil || purged == 0 {
		t.Fatalf("Sweep() = %d purged, err %v; want the expired store:false response reaped", purged, err)
	}

	if _, found, err := h.s3.Get(ctx, attachedObject); err != nil || found {
		t.Fatalf("the ATTACHED image's bytes survived the purge (found=%v err=%v)", found, err)
	}
	if _, found, err := h.s3.Get(ctx, orphanObject); err != nil || !found {
		t.Fatalf("the UNATTACHED image's bytes were reaped (found=%v err=%v) — retention has started reaching an unattached row, so the ceiling documented in slack_vision.go is closed and its comment (and this test) must be updated", found, err)
	}

	var attachedKey, orphanKey string
	if err := h.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT object_key FROM artifacts WHERE id = $1`, attachedID).Scan(&attachedKey); err != nil {
		t.Fatalf("read attached row error = %v", err)
	}
	if err := h.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT object_key FROM artifacts WHERE id = $1`, orphanID).Scan(&orphanKey); err != nil {
		t.Fatalf("read orphan row error = %v", err)
	}
	if attachedKey != "" {
		t.Fatalf("attached artifact still points at %q after the purge — an image joined to a purged response must be scrubbed", attachedKey)
	}
	if orphanKey == "" {
		t.Fatalf("the UNATTACHED artifact was scrubbed — retention has started reaching it, so the ceiling documented in slack_vision.go is closed and its comment (and this test) must be updated")
	}
}

// THE UPLOAD-SIDE GUARD (E22 T5) against a REAL row and a REAL object store. The Slack answer that reaches
// this function names an artifact id a MODEL wrote, so the three things it refuses are the three ways that id
// could name something it should not: another tenant's artifact, another RUN's artifact, and one too big to
// put in front of a human.
//
// The run in the key is the one that is easy to miss and the one that matters most here: the tenant boundary
// is real but it is not the boundary in question — a screenshot taken for somebody else's thread is in the
// SAME org and project, and publishing it into this one is a leak the tenant check cannot see.
func TestReadRunArtifactRefusesForeignRunsForeignTenantsAndOversize(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	org, project, runID := h.seedRun(t)
	_, _, otherRun := h.seedRun(t)
	otherOrg, otherProject, _ := h.seedRun(t)

	content := []byte("** BUILD SUCCEEDED **\n")
	mine, err := h.writer.Write(ctx, WriteRequest{Organization: org, Project: project, RunID: runID, Content: content})
	if err != nil {
		t.Fatalf("write this run's artifact: %v", err)
	}
	theirs, err := h.writer.Write(ctx, WriteRequest{Organization: org, Project: project, RunID: otherRun, Content: content})
	if err != nil {
		t.Fatalf("write the other run's artifact: %v", err)
	}

	// The run's OWN artifact comes back whole.
	body, size, found, err := h.writer.ReadRunArtifact(ctx, org, project, runID, mine.ID, 8<<20)
	if err != nil || !found {
		t.Fatalf("ReadRunArtifact(own) = (found %v, err %v), want the bytes", found, err)
	}
	if !bytes.Equal(body, content) || size != int64(len(content)) {
		t.Fatalf("ReadRunArtifact(own) returned %d byte(s)/size %d, want %d verbatim", len(body), size, len(content))
	}

	// EVERY REFUSAL IS THE SAME MISS. A caller cannot tell "another run's" from "does not exist", which is the
	// §22.6 non-disclosure rule applied to a lookup key an outsider chose.
	for _, tc := range []struct {
		name                        string
		org, project, run, artefact string
	}{
		{"another run's artifact", org, project, runID, theirs.ID},
		{"another tenant reading ours", otherOrg, otherProject, runID, mine.ID},
		{"an id that does not exist", org, project, runID, "art_00000000000000000000000000000000"},
	} {
		body, _, found, err := h.writer.ReadRunArtifact(ctx, tc.org, tc.project, tc.run, tc.artefact, 8<<20)
		if err != nil || found || body != nil {
			t.Fatalf("%s: ReadRunArtifact = (%d bytes, found %v, err %v), want one indistinguishable miss",
				tc.name, len(body), found, err)
		}
	}

	// THE CEILING IS CHECKED ON THE ROW. The refusal names the size — which the caller turns into an honest
	// sentence — and returns no bytes, so an artifact far larger than the control plane's memory is refused
	// for the cost of one SELECT.
	body, size, found, err = h.writer.ReadRunArtifact(ctx, org, project, runID, mine.ID, 4)
	if err != nil {
		t.Fatalf("an over-ceiling read errored (%v); it is a documented ANSWER, not a failure", err)
	}
	if body != nil {
		t.Fatalf("an over-ceiling read still returned %d byte(s)", len(body))
	}
	if !found || size != int64(len(content)) {
		t.Fatalf("an over-ceiling read reported (found %v, size %d); it must say the artifact EXISTS and how big it is", found, size)
	}
}
