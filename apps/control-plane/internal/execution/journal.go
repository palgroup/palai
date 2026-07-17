// Package execution reads the durable session journal for the control-plane's
// live event stream. Every read is keyed by the verified tenant scope (spec
// §39.2): a session_id from another tenant matches no row, so the journal cannot
// leak one tenant's events to another. The journal is read-only — streaming a
// session never writes state, so a disconnecting client cannot affect a run.
package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/storage"
)

// Journal reads the per-session event log as CloudEvents envelopes.
type Journal struct {
	pool *pgxpool.Pool
}

// NewJournal wraps a connection pool. The pool is shared with the durable spine.
func NewJournal(pool *pgxpool.Pool) *Journal { return &Journal{pool: pool} }

// SessionExists reports whether the session is visible in the given tenant scope.
// A false result is the 404 gate for a foreign or unknown session id: without it
// a foreign session would be indistinguishable from an empty one.
func (j *Journal) SessionExists(ctx context.Context, org, project, sessionID string) (bool, error) {
	var exists bool
	err := j.pool.QueryRow(ctx, storage.Query("SessionExistsInScope"), sessionID, org, project).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check session scope: %w", err)
	}
	return exists, nil
}

// ResolveCursor maps a Last-Event-ID (an evt_* id) to its per-session sequence so
// the stream can replay from the next sequence (asyncapi x-sse-binding). The
// second result is false when the id is unknown in this session/scope, which the
// caller treats as "resume from the beginning".
func (j *Journal) ResolveCursor(ctx context.Context, org, project, sessionID, eventID string) (int64, bool, error) {
	var seq int64
	err := j.pool.QueryRow(ctx, storage.Query("EventSequenceInScope"), eventID, sessionID, org, project).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("resolve event cursor: %w", err)
	}
	return seq, true, nil
}

// After returns up to limit events with sequence greater than afterSeq, in
// ascending sequence order, as CloudEvents envelopes. Passing the last delivered
// sequence tails the journal without gaps or duplicates.
func (j *Journal) After(ctx context.Context, org, project, sessionID string, afterSeq int64, limit int) ([]contracts.Event, error) {
	rows, err := j.pool.Query(ctx, storage.Query("ReadEventsAfter"), sessionID, org, project, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()

	source := "/v1/sessions/" + sessionID
	var events []contracts.Event
	for rows.Next() {
		var (
			id, typ   string
			seq       int64
			payload   []byte
			createdAt time.Time
		)
		if err := rows.Scan(&id, &seq, &typ, &payload, &createdAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		data := map[string]any{}
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &data); err != nil {
				return nil, fmt.Errorf("decode event payload: %w", err)
			}
		}
		events = append(events, contracts.Event{
			Specversion: "1.0",
			ID:          contracts.EventID(id),
			Source:      source,
			Type:        typ,
			Time:        createdAt.UTC().Format(time.RFC3339Nano),
			Sequence:    int(seq),
			Data:        data,
			SessionID:   contracts.SessionID(sessionID),
			ProjectID:   contracts.ProjectID(project),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return events, nil
}
