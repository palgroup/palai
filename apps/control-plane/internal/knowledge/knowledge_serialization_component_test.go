//go:build component

// Serialization proof for the E17 Task 4 knowledge spine (SF-1). Concurrent same-KB ingests must not
// interleave a stale membership snapshot with another build's commit. The fix is one per-KB row lock taken
// as the FIRST statement of the build transaction; this test asserts that ordering deterministically (a
// timing-based concurrency race would flake). Runs under the same real-PostgreSQL component tier.
package knowledge_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/internal/knowledge"
	"github.com/palgroup/palai/storage"
)

// queryRecorder is a pgx QueryTracer that records, in order, the SQL of every query run on its pool.
type queryRecorder struct {
	mu   sync.Mutex
	sqls []string
}

func (r *queryRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	r.sqls = append(r.sqls, strings.TrimSpace(data.SQL))
	r.mu.Unlock()
	return ctx
}

func (r *queryRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// indexOf returns the position of the first recorded query whose SQL equals want, or -1.
func (r *queryRecorder) indexOf(want string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	want = strings.TrimSpace(want)
	for i, s := range r.sqls {
		if s == want {
			return i
		}
	}
	return -1
}

// openTracedKnowledgeStore opens a knowledge store over a pool whose queries are recorded, against the same
// (already-migrated) component database.
func openTracedKnowledgeStore(t *testing.T, rec *queryRecorder) *knowledge.Store {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(envURL(t))
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.ConnConfig.Tracer = rec
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return knowledge.New(pool)
}

// TestBuildTakesPerKBLockBeforeReadingMembership proves SF-1's serialization fix: a build transaction takes
// the per-KB row lock (SELECT ... FOR UPDATE) as its FIRST statement — BEFORE it reads the next document
// version, snapshots membership (ActiveDocumentRevisions), or reads the next index version. That ordering is
// what serializes concurrent same-KB ingests: job B cannot snapshot a stale member set or grab a colliding
// UNIQUE(kb, version) while job A's build is mid-flight, so no build activates an index that omits a
// just-committed doc. Asserting query ORDER is deterministic where a timing-based concurrency test flakes.
func TestBuildTakesPerKBLockBeforeReadingMembership(t *testing.T) {
	cs, _ := openStore(t) // migrates the schema + gives a store for tenant provisioning
	scope := provisionTenant(t, cs, "kno-serialize")

	var rec queryRecorder
	ks := openTracedKnowledgeStore(t, &rec)
	kb := createKB(t, ks, scope, "kb")
	src := createSource(t, ks, scope, kb, "")

	if o := ingest(t, ks, scope, kb, src, "Serialized builds keep the active index consistent."); o.State != "succeeded" {
		t.Fatalf("ingest = %+v, want succeeded", o)
	}

	lock := rec.indexOf(storage.Query("LockKnowledgeBaseForBuild"))
	nextDoc := rec.indexOf(storage.Query("NextDocumentVersion"))
	membership := rec.indexOf(storage.Query("ActiveDocumentRevisions"))
	nextVersion := rec.indexOf(storage.Query("NextIndexVersion"))
	if lock < 0 {
		t.Fatal("build did not take a per-KB FOR UPDATE lock (SF-1: concurrent same-KB ingest can activate a stale index)")
	}
	if lock > nextDoc || lock > membership || lock > nextVersion {
		t.Fatalf("FOR UPDATE lock is not the first build statement: lock@%d nextDoc@%d membership@%d nextVersion@%d",
			lock, nextDoc, membership, nextVersion)
	}
}
