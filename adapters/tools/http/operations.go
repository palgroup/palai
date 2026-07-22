// Package remotehttp is the remote-HTTP tool transport (spec §28.24-28.25, E12 Task 4): the broker's
// remote_http executor signs a tool-http.v1 invoke, POSTs it egress-safe to a customer tool server (a NEW
// trust boundary), and either returns the 200 result inline or opens a durable async operation a signed
// 202 callback later resolves. It reuses the webhook signer/verify (no new MAC) and the shared egress
// SSRF layer (no new pinning). The package name is remotehttp (not http) so it can import net/http.
package remotehttp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// OpenOperation is the durable pending row the executor opens BEFORE an invoke POST, so a callback can
// never race ahead of a row to persist into (spec §28.24). The raw callback token is NOT stored — only
// its sha256 hash (TokenHash); the raw lives only in the signed invoke envelope. SecretRef is the
// tool_revision handle the callback endpoint re-resolves to verify the callback signature.
type OpenOperation struct {
	OperationID string
	Org         string
	Project     string
	ToolCallID  string
	SecretRef   string
	TokenHash   string
	Deadline    time.Time
	Fence       uint64
}

// Operations is the pgx-backed remote_tool_operations ledger (migration 000025). It is the single
// concrete store the executor, the callback endpoint, and the reconcile prober share; each consumer
// depends on its OWN narrow interface, so this type simply implements all of them.
type Operations struct {
	pool *pgxpool.Pool
}

// NewOperations wraps a durable pool as the remote-tool operation ledger.
func NewOperations(pool *pgxpool.Pool) *Operations { return &Operations{pool: pool} }

// Open sweeps any EXPIRED pending operation for this tool_call to timed_out, then inserts the fresh
// pending row. The partial-unique(tool_call_id WHERE pending) makes a duplicate LIVE (non-expired) invoke
// a 0-row no-op (opened=false), so the executor polls the existing row instead of re-POSTing; a
// deadline-passed pending is swept first, so a stuck operation never blocks the next drive forever.
func (o *Operations) Open(ctx context.Context, in OpenOperation) (opened bool, err error) {
	if _, err := o.pool.Exec(ctx, storage.Query("SweepExpiredRemoteOperation"), in.ToolCallID); err != nil {
		return false, fmt.Errorf("sweep expired remote operation: %w", err)
	}
	tag, err := o.pool.Exec(ctx, storage.Query("OpenRemoteOperation"),
		in.OperationID, in.Org, in.Project, in.ToolCallID, in.SecretRef, in.TokenHash, in.Deadline, int64(in.Fence))
	if err != nil {
		return false, fmt.Errorf("open remote operation: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// Fail closes a pending operation the invoke got a DEFINITE negative answer for, so a retry re-POSTs
// (MF1). A transient network error before the deadline never calls this (the POST may have landed).
func (o *Operations) Fail(ctx context.Context, operationID string) error {
	if _, err := o.pool.Exec(ctx, storage.Query("FailRemoteOperation"), operationID); err != nil {
		return fmt.Errorf("fail remote operation: %w", err)
	}
	return nil
}

// Poll reads an operation's state + result by id (the executor polls the row it opened). A NULL result
// scans to nil.
func (o *Operations) Poll(ctx context.Context, operationID string) (state string, result []byte, err error) {
	if err := o.pool.QueryRow(ctx, storage.Query("PollRemoteOperation"), operationID).Scan(&state, &result); err != nil {
		return "", nil, fmt.Errorf("poll remote operation: %w", err)
	}
	return state, result, nil
}

// CompleteSync closes the pending row a 200 synchronous result answered (result already in hand).
func (o *Operations) CompleteSync(ctx context.Context, operationID string, result []byte, resultHash string) error {
	if _, err := o.pool.Exec(ctx, storage.Query("CompleteSyncRemoteOperation"), operationID, result, resultHash); err != nil {
		return fmt.Errorf("complete sync remote operation: %w", err)
	}
	return nil
}

// Timeout flips a still-pending operation to timed_out when the executor's deadline fires. A callback
// that already completed it is left untouched (0 rows), so the executor re-polls and returns that result.
func (o *Operations) Timeout(ctx context.Context, operationID string) error {
	if _, err := o.pool.Exec(ctx, storage.Query("TimeoutRemoteOperation"), operationID); err != nil {
		return fmt.Errorf("timeout remote operation: %w", err)
	}
	return nil
}

// CallbackRow is the verify-before-persist inputs the callback endpoint reads for an operation id.
type CallbackRow struct {
	Org        string
	SecretRef  string
	TokenHash  string
	State      string
	ResultHash string
}

// ForCallback reads the callback verification inputs by operation id. found=false is a generic 404 (no
// config oracle).
func (o *Operations) ForCallback(ctx context.Context, operationID string) (CallbackRow, bool, error) {
	var row CallbackRow
	err := o.pool.QueryRow(ctx, storage.Query("RemoteOperationForCallback"), operationID).
		Scan(&row.Org, &row.SecretRef, &row.TokenHash, &row.State, &row.ResultHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return CallbackRow{}, false, nil
	}
	if err != nil {
		return CallbackRow{}, false, fmt.Errorf("read remote operation for callback: %w", err)
	}
	return row, true, nil
}

// Consume is the atomic one-use token consume: it flips a pending/timed_out row to completed (a callback
// within the deadline) or late_result (after it), records the result + hash, and returns the new state.
// consumed=false means the token was already spent (or the row terminal) — the caller then compares
// result_hash for idempotent-200 vs 409. The token itself is verified constant-time BEFORE this runs.
func (o *Operations) Consume(ctx context.Context, operationID string, result []byte, resultHash string) (newState string, consumed bool, err error) {
	err = o.pool.QueryRow(ctx, storage.Query("ConsumeRemoteCallback"), operationID, result, resultHash).Scan(&newState)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("consume remote callback: %w", err)
	}
	return newState, true, nil
}

// ProberRead reads the resolved result for an uncertain tool_call (the RemoteToolProber destination
// read, spec §26.7): the newest row carrying a result. found=false -> the operation never resolved, so
// the prober escalates to manual_resolution.
func (o *Operations) ProberRead(ctx context.Context, toolCallID string) (state string, result []byte, found bool, err error) {
	err = o.pool.QueryRow(ctx, storage.Query("ProberReadRemoteOperation"), toolCallID).Scan(&state, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("prober read remote operation: %w", err)
	}
	return state, result, true, nil
}
