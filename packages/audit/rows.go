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
	rows, err := q.Query(ctx, storage.Query("AuditChainRows"))
	if err != nil {
		return nil, fmt.Errorf("audit: read journal: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var responseID *string
		if err := rows.Scan(&r.ID, &r.OrganizationID, &r.ProjectID, &r.SessionID, &responseID,
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
