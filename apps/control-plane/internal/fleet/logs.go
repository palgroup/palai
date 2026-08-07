package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// RunnerLogLine is one line a machine wrote, as it crosses the runner plane and as it is read back.
type RunnerLogLine struct {
	ID         string
	ProjectID  string
	RunnerID   string
	At         time.Time
	ReceivedAt time.Time
	Level      string
	SessionID  string
	Message    string
}

// RunnerLogs stores and reads what the machines say about themselves.
//
// IT EXISTS BECAUSE THE RUNNER PLANE CARRIED NO SUCH LINE. Measured 2026-08-07: the four routes an
// agent speaks — connect, enroll, renew, settings — move leases, identity and config, and none of them
// moves a sentence the agent wrote. "What went wrong on that Mac" was answerable only by logging into
// it, which is the one thing a fleet of a hundred machines cannot do.
type RunnerLogs struct {
	pool  *pgxpool.Pool
	keep  int
	newID func() string
}

// DefaultRunnerLogRetention is how many lines one machine keeps in the installation.
//
// IT IS ENFORCED ON THE WRITE PATH, not by a sweeper. A hundred agents shipping their own logs is
// unbounded growth by construction, and a bound that depends on somebody remembering to wire a
// collector is the bound that is missing on the Sunday the disk fills.
const DefaultRunnerLogRetention = 5000

// MaxRunnerLogBatch bounds ONE shipment. An agent that reconnects after an outage has a backlog, and
// the answer to a backlog is several batches rather than one unbounded request — a single body large
// enough to hold an hour of logs is a single body large enough to exhaust the reader.
const MaxRunnerLogBatch = 500

func NewRunnerLogs(pool *pgxpool.Pool) *RunnerLogs {
	return &RunnerLogs{pool: pool, keep: DefaultRunnerLogRetention, newID: func() string {
		var b [12]byte
		_, _ = rand.Read(b[:])
		return "rlog_" + hex.EncodeToString(b[:])
	}}
}

// Append writes one batch and then trims the machine back to its retention.
//
// THE TRIM RUNS IN THE SAME TRANSACTION AS THE WRITE. A trim that ran separately would be a second call
// somebody could skip, disable or lose to a crash — and the state it leaves is a table that only grows.
func (l *RunnerLogs) Append(ctx context.Context, project, runnerID string, lines []RunnerLogLine) error {
	if strings.TrimSpace(runnerID) == "" {
		return fmt.Errorf("append runner log: no machine")
	}
	if len(lines) == 0 {
		return nil
	}
	if len(lines) > MaxRunnerLogBatch {
		return fmt.Errorf("append runner log: %d lines exceeds the %d-line batch bound", len(lines), MaxRunnerLogBatch)
	}
	ctx = storage.WithSystemScope(ctx)
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("append runner log: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, line := range lines {
		at := line.At
		if at.IsZero() {
			at = time.Now().UTC()
		}
		if _, err := tx.Exec(ctx, storage.Query("AppendRunnerLog"),
			l.newID(), project, runnerID, at, line.Level, line.SessionID, line.Message); err != nil {
			return fmt.Errorf("append runner log: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, storage.Query("TrimRunnerLog"), runnerID, l.keep); err != nil {
		return fmt.Errorf("trim runner log: %w", err)
	}
	return tx.Commit(ctx)
}

// Page reads one machine's lines, newest first. sessionID empty reads everything the machine said,
// including the lines between sessions — which is what an infrastructure question is about.
func (l *RunnerLogs) Page(ctx context.Context, runnerID, sessionID string, limit int) ([]RunnerLogLine, error) {
	if limit <= 0 || limit > MaxRunnerLogBatch {
		limit = 100
	}
	rows, err := l.pool.Query(storage.WithSystemScope(ctx), storage.Query("RunnerLogPage"), runnerID, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("read runner log: %w", err)
	}
	defer rows.Close()
	var out []RunnerLogLine
	for rows.Next() {
		var line RunnerLogLine
		if err := rows.Scan(&line.ID, &line.ProjectID, &line.RunnerID, &line.At, &line.ReceivedAt,
			&line.Level, &line.SessionID, &line.Message); err != nil {
			return nil, fmt.Errorf("read runner log: %w", err)
		}
		out = append(out, line)
	}
	return out, rows.Err()
}
