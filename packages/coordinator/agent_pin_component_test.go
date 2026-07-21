//go:build component

package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestVerifyPublishedRevisionPropagatesQueryError proves the pin-check does NOT swallow an unexpected
// DB error: on a real query failure (here a canceled context) it must RETURN the error, so AdmitResponse
// rolls back and the API answers 500 — never a fake 202 with an empty body over a rolled-back tx. A
// swallowed error (the old `return Admission{}, false` with no error) would leave err nil here.
func TestVerifyPublishedRevisionPropagatesQueryError(t *testing.T) {
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	cs, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	tx, err := cs.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// A canceled context makes the pin-state query fail with a real DB error (not pgx.ErrNoRows).
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	tenant := Tenant{Organization: pinTestID("org"), Project: pinTestID("prj")}
	_, ok, qErr := verifyPublishedRevision(canceled, tx, "AgentRevisionPublished", pinTestID("arev"), tenant)
	if qErr == nil {
		t.Fatal("verifyPublishedRevision swallowed a query error (returned nil); want it propagated so admission fails closed")
	}
	if ok {
		t.Fatal("verifyPublishedRevision reported ok on a failed query; want ok=false")
	}
}

func pinTestID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}
