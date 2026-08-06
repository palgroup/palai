package coordinator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// purgeBatch bounds one retention sweep, mirroring reclaimBatch: a backlog of expired
// responses cannot take a table-wide lock or a runaway transaction.
const purgeBatch = 100

// PurgeExpiredStoreFalse reaps the content of store=false responses whose terminal
// state has aged past ttl, leaving a tombstone (spec §8.3, §20.9). It is the retention
// sibling of ReclaimExpired: a global maintenance sweep, bounded per call, that never
// crosses a tenant boundary — every purge join in the query carries the victim's own
// organization/project. It returns the number of responses purged this pass and the
// object keys of the artifacts it scrubbed, so the caller can delete those bytes from the
// object store after this transaction has committed (LP §7.2).
func (s *Store) PurgeExpiredStoreFalse(ctx context.Context, ttl time.Duration) (int, []string, error) {
	ctx = storage.WithSystemScope(ctx) // retention sweep: spans every tenant by construction
	if ttl < 0 {
		return 0, nil, errors.New("retention TTL must not be negative")
	}
	var (
		purged     int
		objectKeys []string
	)
	if err := s.pool.QueryRow(ctx, storage.Query("PurgeExpiredStoreFalse"), ttl.Milliseconds(), purgeBatch).
		Scan(&purged, &objectKeys); err != nil {
		return 0, nil, fmt.Errorf("purge expired store-false responses: %w", err)
	}
	return purged, objectKeys, nil
}

// ResponseView is a response's retrievable projection (spec §22.3). Found is false for
// an unknown or out-of-scope id; Purged is true once the content has been reaped,
// leaving only the tombstone (retrieval then reads as 410).
type ResponseView struct {
	Found     bool
	Purged    bool
	State     string
	Output    []byte
	CreatedAt time.Time
	// Metadata is the caller's `metadata` object as stored JSON ('{}' when the request named none, and
	// after a retention purge has scrubbed it). It comes from its own column rather than from Output,
	// so no projection rewrite can drop it — see the GetResponse query's own note.
	Metadata []byte
	// AgentRevisionID is the revision the run RESOLVED to, empty when no agent steered it. It is what
	// makes a run reproducible — response-create.json promises exactly that — and until 2026-08-07 the
	// promise had no read: the column was written, and every surface answered null.
	AgentRevisionID string
}

// GetResponse reads a response's terminal projection within the tenant scope. A missing
// or foreign row is Found=false (the handler renders 404, never leaking existence across
// tenants), and a reaped row is Purged=true (410).
func (s *Store) GetResponse(ctx context.Context, tenant Tenant, id string) (ResponseView, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	var (
		view     ResponseView
		output   []byte
		purgedAt *time.Time
	)
	var metadata []byte
	// NULLABLE, and scanned through a pointer for it: a response with no run — a queued one, or one whose
	// run row has not been written yet — has no revision to report, and that is a different fact from the
	// empty string a non-null column would give.
	var agentRevisionID *string
	err := s.pool.QueryRow(ctx, storage.Query("GetResponse"), id, tenant.Project).
		Scan(&view.State, &output, &purgedAt, &view.CreatedAt, &metadata, &agentRevisionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ResponseView{}, nil
	}
	if err != nil {
		return ResponseView{}, fmt.Errorf("read response %s: %w", id, err)
	}
	view.Found = true
	view.Output = output
	view.Metadata = metadata
	view.Purged = purgedAt != nil
	if agentRevisionID != nil {
		view.AgentRevisionID = *agentRevisionID
	}
	return view, nil
}
