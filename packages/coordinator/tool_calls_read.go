package coordinator

import (
	"context"
	"fmt"
	"time"

	"github.com/palgroup/palai/storage"
)

// THE TOOL-CALL READ (E30 T2).
//
// It exists because a chat that watches an agent build an iOS app has to render what `xcodebuild` said,
// and until now nothing could: the journal's tool frames carried a call id and a replay class, and the
// name, arguments and result lived only on the ledger with no route to them. The demo's own adapter
// wrote `nameUnavailable: true` rather than invent a field, which was the honest thing to do and also a
// permanent hole in every rendering of a tool call.
//
// THE SPLIT THIS COMPLETES. The NAME now rides the journal frame — short, and drawn from the closed set
// of tools the deployment registered. The ARGUMENTS and RESULT are read here instead, because an event
// payload does not stop at the SSE stream: it is POSTed to every registered webhook endpoint and stored
// immutably for redelivery, and hashed into the audit chain. A trivial `xcodebuild` build measures
// 51,422 bytes; nothing bounds an event payload. So the small bounded label goes on the stream that fans
// out, and the unbounded model-authored bytes stay behind a read somebody has to ask for.

// ToolCallView is one ledger row as a renderer reads it. Arguments and Result are the raw JSON bytes the
// ledger holds — not re-encoded, not summarised — because a renderer that wants to say "this build
// failed at Foo.swift:42" needs the bytes the tool actually returned.
//
// Result is nil for a call that has not resolved (a parked approval, an in-flight execution) and for an
// UNRETAINED tool, whose row deliberately carries a marker instead of its output. Both are honest
// absences and a renderer must not draw them as an empty result.
type ToolCallView struct {
	ID          string
	Name        string
	State       string
	Arguments   []byte
	Result      []byte
	ReplayClass string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ToolCallsForResponse returns a response's tool calls in commit order, within the tenant scope.
//
// A response id from another tenant matches no rows and reads as a response with no tool calls, which is
// the same answer an id that never existed gives — no cross-tenant existence disclosure (§39.2).
func (s *Store) ToolCallsForResponse(ctx context.Context, tenant Tenant, responseID string) ([]ToolCallView, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("ToolCallsForResponse"), responseID, tenant.Organization, tenant.Project)
	if err != nil {
		return nil, fmt.Errorf("read tool calls for response %s: %w", responseID, err)
	}
	defer rows.Close()

	var out []ToolCallView
	for rows.Next() {
		var v ToolCallView
		if err := rows.Scan(&v.ID, &v.Name, &v.State, &v.Arguments, &v.Result, &v.ReplayClass,
			&v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan tool call row: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tool call rows: %w", err)
	}
	return out, nil
}
