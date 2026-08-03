// Package store is the slack-bot's own durable state: the mapping from a Slack thread to a Palai
// session (2026-08-03 plan, Task 8). It is a separate database from the control plane's — the bot owns
// none of Palai's tables and therefore inherits none of storage/migrations/000035_slack.up.sql's four
// foreign keys (organizations, projects, slack_connections, sessions). To this package, session_id is
// a plain TEXT column, not a REFERENCES: nothing here can validate that a session id names something
// real, because this schema has no visibility into Palai's schema at all. Two consequences follow
// directly from that, and both are load-bearing:
//
//  1. An ORPHANED row is the expected steady state, not an error condition. A session closed or
//     removed on Palai's side leaves its thread_sessions row behind; SessionForThread returns whatever
//     is stored without checking that it still resolves, because it cannot check. The caller (Task 9)
//     is the one that learns a session is gone — a 404 from the SDK's Sessions.Get/Events — and it is
//     the caller's job to open a new session and call RebindThread, not this package's job to guess.
//  2. The DATABASE no longer enforces that a session id is real; this package's DISCIPLINE does. The
//     old table's `REFERENCES sessions(id)` made writing a wrong id impossible. Here the only thing
//     that makes it impossible is that every session id BindThread/RebindThread ever see came out of
//     Palai's own API response. This package must never invent one (no UUID/ULID generation for
//     session_id anywhere below) and must never accept one sourced from Slack input — a message body,
//     a user-supplied string. Both methods refuse an empty sessionID as the one check that IS
//     mechanical; the rest of the invariant is enforced by never writing a code path that could
//     produce a session id from anywhere but a Sessions.Create/Sessions.Get response.
//
// What IS kept from 000035_slack.up.sql's slack_thread_sessions: a second event in the same thread
// resolves the SAME session (BindThread), and a concurrent race collapses at the database rather than
// minting two — see migrations/0001_thread_sessions.sql for the constraint that guarantees it.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/slack-bot/migrations"
)

// Store owns the slack-bot's own connection pool and, so far, only the thread-session correlation
// (Task 8). Later tasks may add more durable state to it; nothing here anticipates what that would be.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects a Store against databaseURL (PALAI_BOT_DATABASE_URL) and verifies the connection with
// a ping, so a bad DSN is refused here rather than several calls later at the first query. It does NOT
// run migrations — call Migrate once connected. The two are separate calls, mirroring
// apps/control-plane/cmd/palai-control-plane/main.go's own `store.Open(ctx, url)` followed by a
// distinct `repo.Migrate(ctx)`, so a caller that only wants a connection — this package's own tests
// included — is not forced to also own schema evolution on every call.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: parse/connect PALAI_BOT_DATABASE_URL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// schemaMigrationsDDL creates this schema's OWN migration-tracking table if it does not already exist.
// It is bootstrap DDL Migrate owns directly, not a numbered migration file in the migrations package:
// migration version 1 cannot record that it ran in a table that does not exist yet to record it in.
const schemaMigrationsDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER NOT NULL PRIMARY KEY,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// Migrate applies every migration in the migrations package that has not already run against this
// database, each in its own transaction alongside the schema_migrations row that records it — so an
// interrupted run resumes from the last committed migration on restart. This is the boot-time-runner
// shape the control plane uses (packages/coordinator Store.Migrate), scaled down to what this schema
// actually needs: one owner (this process), no row-level-security roles to switch between, and no
// checksum-audit journal (schema_revisions, 000033) — only "did version N already run, and if not, run
// it and record it." A caller (main.go, in whichever later task first needs a live *Store) is expected
// to call Migrate once, right after Open, at process boot.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied := make(map[int]bool)
	rows, err := s.pool.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("store: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read schema_migrations: %w", err)
	}

	for _, m := range migrations.Ordered() {
		if applied[m.Version] {
			continue
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("store: begin migration %d_%s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: apply migration %d_%s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", m.Version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: record migration %d_%s: %w", m.Version, m.Name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("store: commit migration %d_%s: %w", m.Version, m.Name, err)
		}
	}
	return nil
}

// SessionForThread resolves an existing binding for (botID, teamID, channelID, threadTS), or
// found=false if this exact thread has never been bound by this bot. The WHERE clause pins the full
// primary key, so at most one row can ever match — there is no LIMIT here and none is needed, and
// therefore no ORDER BY ambiguity of the kind that has decided outcomes elsewhere in this tree.
//
// It returns whatever session id is stored without checking that it still resolves on Palai's side —
// it cannot check (see the package doc). A found=true result names an ORPHAN candidate, not a
// guarantee of liveness.
func (s *Store) SessionForThread(ctx context.Context, botID, teamID, channelID, threadTS string) (string, bool, error) {
	var sessionID string
	err := s.pool.QueryRow(ctx,
		`SELECT session_id FROM thread_sessions
		 WHERE bot_id = $1 AND team_id = $2 AND channel_id = $3 AND thread_ts = $4`,
		botID, teamID, channelID, threadTS,
	).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: resolve thread session: %w", err)
	}
	return sessionID, true, nil
}

// BindThread binds threadTS to sessionID the first time this exact (botID, teamID, channelID,
// threadTS) key is seen, and returns the WINNING session id: sessionID itself if this call inserted
// the row, or — if a concurrent call (or an earlier event) already bound this thread — whichever
// session id got there first. It never overwrites an existing binding; RebindThread is the
// orphan-recovery path that does.
//
// The insert races safely: ON CONFLICT DO NOTHING means a second, concurrent bind for the same key
// never sees a unique-violation error, it simply inserts nothing and this call falls through to
// reading back whatever the winner wrote.
//
// sessionID must be Palai's own answer to a Sessions.Create call — never Slack input (a message body,
// a user-supplied string), and never invented here. The empty-string check is this method's one
// mechanical guard against that; the rest of the invariant holds because nothing in this file
// generates a session id itself (see the package doc).
func (s *Store) BindThread(ctx context.Context, botID, teamID, channelID, threadTS, sessionID string) (string, error) {
	if sessionID == "" {
		return "", errors.New("store: BindThread: sessionID is required")
	}
	var bound string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO thread_sessions (bot_id, team_id, channel_id, thread_ts, session_id)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (bot_id, team_id, channel_id, thread_ts) DO NOTHING
		 RETURNING session_id`,
		botID, teamID, channelID, threadTS, sessionID,
	).Scan(&bound)
	if errors.Is(err, pgx.ErrNoRows) {
		// The insert did nothing: this key already had a row (our own earlier bind, or a concurrent
		// caller that won the race). Read back whichever session id is actually stored — not
		// sessionID, which may have lost the race.
		existing, found, selErr := s.SessionForThread(ctx, botID, teamID, channelID, threadTS)
		if selErr != nil {
			return "", selErr
		}
		if !found {
			return "", fmt.Errorf("store: BindThread: conflicted on %s/%s/%s/%s but no row was found", botID, teamID, channelID, threadTS)
		}
		return existing, nil
	}
	if err != nil {
		return "", fmt.Errorf("store: bind thread: %w", err)
	}
	return bound, nil
}

// RebindThread replaces an existing thread's session id — the orphan-recovery path the package doc
// describes: a thread's previously bound session no longer exists on Palai's side (a 404 the caller
// got back from Sessions), the caller opened a NEW one, and the stale mapping must move to it rather
// than accumulate a second row (the PRIMARY KEY forbids that anyway) or keep pointing callers at a
// session that no longer answers. Unlike BindThread, this DOES overwrite; it also creates the row if
// none exists yet, which is harmless — RebindThread's real caller already resolved the thread via
// SessionForThread first, so the row is expected to exist, but there is no reason to refuse creating
// it if it somehow does not.
//
// sessionID carries the exact same restriction as BindThread's: it must be Palai's own answer, never
// Slack input, never invented here.
func (s *Store) RebindThread(ctx context.Context, botID, teamID, channelID, threadTS, sessionID string) error {
	if sessionID == "" {
		return errors.New("store: RebindThread: sessionID is required")
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO thread_sessions (bot_id, team_id, channel_id, thread_ts, session_id)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (bot_id, team_id, channel_id, thread_ts)
		 DO UPDATE SET session_id = EXCLUDED.session_id`,
		botID, teamID, channelID, threadTS, sessionID,
	)
	if err != nil {
		return fmt.Errorf("store: rebind thread: %w", err)
	}
	return nil
}
