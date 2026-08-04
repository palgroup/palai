package audit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// Querier is the read surface the chain needs: one query over the journal. *pgx.Conn and *pgxpool.Pool
// both satisfy it, so the CLI can use a plain connection and a component test its pool.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ReadRows loads the whole journal in canonical chain order. The chain is recomputed FROM THESE ROWS
// every time — nothing is read back out of a stored hash column, because a stored hash lives in the
// same mutable table it would vouch for.
//
// ponytail: the journal is read in one pass with no keyset paging. A journal that outgrows memory
// wants a streaming fold (the chain is already a fold — Head would take an iterator); it is bounded
// today by the same single-node scale every other operator command assumes.
func ReadRows(ctx context.Context, q Querier) ([]Row, error) {
	if err := assertWholeJournalVisible(ctx, q); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, storage.Query("AuditChainRows"))
	if err != nil {
		return nil, fmt.Errorf("audit: read journal: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var responseID *string
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.SessionID, &responseID,
			&r.Seq, &r.JournalID, &r.Type, &r.Payload, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("audit: scan journal row: %w", err)
		}
		if responseID != nil {
			r.ResponseID, r.HasResponseID = *responseID, true
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: iterate journal: %w", err)
	}
	return out, nil
}

// assertWholeJournalVisible refuses a read that row-level security would silently narrow.
//
// The commands SAY they connect as the stack's Postgres superuser, and for the default .palai path
// they do — but PALAI_AUDIT_POSTGRES_URL is a documented second path with no such guarantee, and
// every tenant table is FORCE ROW LEVEL SECURITY (000029): a connection without palai.org_id returns
// ZERO ROWS AND NO ERROR. An integrity check that cannot see a row cannot notice its deletion, and a
// checkpoint cut from that read anchors nothing while looking exactly like one that anchored
// everything. So the connection is PROBED before a single row is trusted, and the honest failure is
// an error rather than a smaller number nobody questions.
//
// row_security_active() is the direct question — is RLS in force for THIS connection on THIS table —
// so it needs no guessing about superuser vs BYPASSRLS vs table ownership. The control plane's own
// system-scope escape hatch (palai.system=on, storage.WithSystemScope) satisfies every tenant policy,
// so a pooled system-scoped read is a complete read and is admitted.
func assertWholeJournalVisible(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, `SELECT row_security_active('events'),
	                                  coalesce(current_setting('palai.system', true), '') = 'on',
	                                  current_user`)
	if err != nil {
		return fmt.Errorf("audit: probe journal visibility: %w", err)
	}
	defer rows.Close()
	var scoped, system bool
	var user string
	if rows.Next() {
		if err := rows.Scan(&scoped, &system, &user); err != nil {
			return fmt.Errorf("audit: probe journal visibility: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("audit: probe journal visibility: %w", err)
	}
	if scoped && !system {
		return fmt.Errorf("audit: REFUSING to read the journal as %q — row-level security is ACTIVE on `events` for this "+
			"connection, so it sees one tenant's view at most and returns no rows at all without palai.org_id. A chain "+
			"recomputed from that read would anchor or verify a fraction of the journal while reporting like it saw all "+
			"of it. Connect as the stack's Postgres superuser (the default .palai path does; check PALAI_AUDIT_POSTGRES_URL)", user)
	}
	return nil
}
