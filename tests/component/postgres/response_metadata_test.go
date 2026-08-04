//go:build component

package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/palgroup/palai/storage"
)

// TestResponseMetadataSurvivesAdmissionAndRetrieval is the durable half of the fix migration 000004
// makes: the `metadata` object a caller submits is STORED and comes back out of GetResponse.
//
// It is written against a real Postgres rather than a fake because the defect it pins was invisible to
// every fake in this tree — nothing dropped the field in Go, the field simply had no column, so a fake
// store carrying a map in memory would have "passed" while the server returned null. The thing under
// test is the round trip through the durable schema, so the schema has to be real.
//
// THE READ IS DELIBERATELY NOT THE PROJECTION BLOB. GetResponse's SQL selects metadata from its own
// column, so this stays green across every terminal rewrite (FinalizeResponse, TimeoutQueuedIfExpired,
// timeOutOneCapacityPark, cancel) — none of which carries metadata, and none of which has to.
func TestResponseMetadataSurvivesAdmissionAndRetrieval(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, principalID := seedTenantWithKey(t, cs.Pool(), "metadata-token")

	in := admissionInput(principalID, "metadata-key-1", "hash-M", `{"id":"resp_ignored"}`)
	in.Metadata = []byte(`{"bot_id":"B123","workflow_id":"wf_abc"}`)
	created, err := cs.AdmitResponse(ctx, tenant, in)
	if err != nil {
		t.Fatalf("AdmitResponse() error = %v", err)
	}
	if created.Replayed || created.Conflict {
		t.Fatalf("AdmitResponse() = %+v, want a fresh admission", created)
	}

	view, err := cs.GetResponse(ctx, tenant, in.ResponseID)
	if err != nil {
		t.Fatalf("GetResponse() error = %v", err)
	}
	if !view.Found {
		t.Fatalf("GetResponse() Found = false, want the admitted response")
	}
	// Compared as a decoded object, not as bytes: Postgres reserializes jsonb with its own key order and
	// spacing, so a byte comparison here would assert the driver's formatting and not the round trip.
	got := decodeObject(t, view.Metadata)
	if got["bot_id"] != "B123" || got["workflow_id"] != "wf_abc" {
		t.Fatalf("GetResponse() Metadata = %v, want the submitted bot_id and workflow_id", got)
	}
}

// TestResponseMetadataDefaultsToEmptyObject pins the OTHER half of the column's contract: a response
// admitted with no metadata reads back an empty object and never a NULL. The API renders `metadata`
// with omitempty, so an empty map and an absent field are the same wire shape — but only if what comes
// out of the database is decodable at all. A NULL here would reach json.Unmarshal as `null`, and the
// distinction is worth a test because the INSERT names the column and so opts OUT of its default; the
// COALESCE in the query is the only thing supplying one.
func TestResponseMetadataDefaultsToEmptyObject(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, principalID := seedTenantWithKey(t, cs.Pool(), "metadata-absent-token")

	in := admissionInput(principalID, "metadata-key-2", "hash-N", `{"id":"resp_ignored"}`)
	if _, err := cs.AdmitResponse(ctx, tenant, in); err != nil {
		t.Fatalf("AdmitResponse() error = %v", err)
	}

	view, err := cs.GetResponse(ctx, tenant, in.ResponseID)
	if err != nil {
		t.Fatalf("GetResponse() error = %v", err)
	}
	if len(decodeObject(t, view.Metadata)) != 0 {
		t.Fatalf("GetResponse() Metadata = %s, want an empty object", view.Metadata)
	}
}

// TestRetentionPurgeScrubsResponseMetadata proves the retention sweep reaps metadata with the rest of a
// store:false response's content. It is a real assertion and not a formality: PurgeExpiredStoreFalse
// scrubbed `input` and `output` and would have left this column untouched, so a caller's correlation
// would have outlived the content it correlated — in a row whose whole point is that it is transient.
func TestRetentionPurgeScrubsResponseMetadata(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, principalID := seedTenantWithKey(t, cs.Pool(), "metadata-purge-token")

	in := admissionInput(principalID, "metadata-key-3", "hash-P", `{"id":"resp_ignored"}`)
	in.Store = false
	in.Metadata = []byte(`{"bot_id":"B999"}`)
	if _, err := cs.AdmitResponse(ctx, tenant, in); err != nil {
		t.Fatalf("AdmitResponse() error = %v", err)
	}
	// The purge takes only TERMINAL responses, so drive the row there directly. Going through the
	// orchestrator would drag a whole run into a test about one column.
	exec(t, cs.Pool(), `UPDATE responses SET state = 'completed', updated_at = clock_timestamp() - interval '1 hour' WHERE id = $1`, in.ResponseID)

	purged, _, err := cs.PurgeExpiredStoreFalse(ctx, time.Minute)
	if err != nil {
		t.Fatalf("PurgeExpiredStoreFalse() error = %v", err)
	}
	if purged == 0 {
		t.Fatalf("PurgeExpiredStoreFalse() purged 0, want the aged store:false response")
	}

	var metadata []byte
	row := cs.Pool().QueryRow(storage.WithSystemScope(ctx), `SELECT metadata FROM responses WHERE id = $1`, in.ResponseID)
	if err := row.Scan(&metadata); err != nil {
		t.Fatalf("read purged metadata: %v", err)
	}
	if len(decodeObject(t, metadata)) != 0 {
		t.Fatalf("metadata after purge = %s, want it scrubbed to an empty object", metadata)
	}
}

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}
