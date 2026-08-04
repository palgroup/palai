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
//     Two events on the SAME orphaned thread can make that discovery at once, both open a new Palai
//     session, and both call RebindThread — so RebindThread is a compare-and-swap on the session id the
//     caller read (not a blind overwrite): exactly one of them wins the row, and the loser's return
//     value says so, so the loser can close the session it just opened instead of leaking it.
//  2. The DATABASE no longer enforces that a session id is real; this package's DISCIPLINE does. The
//     old table's `REFERENCES sessions(id)` made writing a wrong id impossible. Here the only thing
//     that makes it impossible is that every session id BindThread/RebindThread ever see came out of
//     Palai's own API response. This package must never invent one (no UUID/ULID generation for
//     session_id anywhere below) and must never accept one sourced from Slack input — a message body,
//     a user-supplied string. Both methods refuse an empty session id as the one check that IS
//     mechanical (BindThread on its one, RebindThread on both the old and the new); the rest of the
//     invariant is enforced by never writing a code path that could produce a session id from anywhere
//     but a Sessions.Create/Sessions.Get response.
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
// got back from Sessions), so the caller opened a NEW one and wants the stale mapping to move to it.
//
// It is a compare-and-swap, not a blind overwrite: the write only lands if the row STILL carries
// oldSessionID — the value the caller actually read before deciding it was dead. won=false means it
// did not (something else changed the row first: another event on the same orphaned thread that ran
// this exact same recovery concurrently, or a third call this process never sees). Two events landing
// on one orphaned thread at once will BOTH open a new Palai session and both call RebindThread with the
// same oldSessionID; without the WHERE session_id = oldSessionID guard the second write would silently
// clobber the first, and the session the first write pointed at would be abandoned — live on Palai's
// side, referenced nowhere. With it, exactly one call wins (won=true, the table now names its
// newSessionID) and the other loses (won=false, the table is unchanged by this call) — the loser's job
// is to close the session IT just opened rather than leave it orphaned, not to retry this write.
//
// It never creates a row: the WHERE clause requires an existing session_id to match against, so a
// thread that was never bound cannot be "rebound" — RebindThread's caller already resolved the thread
// via SessionForThread and is replacing what that call read, not guessing at a fresh key.
//
// Both session ids carry BindThread's restriction: they must be Palai's own answers, never Slack
// input, never invented here.
func (s *Store) RebindThread(ctx context.Context, botID, teamID, channelID, threadTS, oldSessionID, newSessionID string) (bool, error) {
	if oldSessionID == "" {
		return false, errors.New("store: RebindThread: oldSessionID is required")
	}
	if newSessionID == "" {
		return false, errors.New("store: RebindThread: newSessionID is required")
	}
	// THE DELIVERY STATE IS RESET IN THE SAME STATEMENT, not in a second call, and that is the property
	// this write owes the recovery scan rather than a tidiness. last_sequence is a cursor into ONE
	// session's journal (migrations/0002_delivery_state.sql), so carrying the dead session's number into
	// the new one would make the next stream resume past events it never delivered. run_pending/stream_ts
	// go with it: the run those two named was on the session that no longer exists, so a recovery that
	// found them would re-attach to a 404. Doing it here means no caller can rebind and forget — and the
	// CAS's own guarantee extends to it, since the loser of a rebind race changes nothing at all.
	tag, err := s.pool.Exec(ctx,
		`UPDATE thread_sessions
		 SET session_id = $6, last_sequence = 0, run_pending = false, stream_ts = ''
		 WHERE bot_id = $1 AND team_id = $2 AND channel_id = $3 AND thread_ts = $4 AND session_id = $5`,
		botID, teamID, channelID, threadTS, oldSessionID, newSessionID,
	)
	if err != nil {
		return false, fmt.Errorf("store: rebind thread: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// PendingRun is one thread that was being rendered into when this process last stopped — everything the
// recovery scan (relay.RecoverPendingRuns) needs to finish the job, and nothing else.
//
// StreamTS may be EMPTY and that is a different recovery, not a degraded one: it means the run was accepted
// by the control plane but the process died before chat.startStream returned, so there is no message to
// resume and recovery opens one. RecipientUserID is what makes that second case possible at all (see the
// column's own note in migrations/0002_delivery_state.sql).
type PendingRun struct {
	TeamID          string
	ChannelID       string
	ThreadTS        string
	SessionID       string
	LastSequence    int64
	StreamTS        string
	RecipientUserID string
}

// BeginDelivery records that a run has been accepted for this thread and has delivered nothing yet.
//
// IT IS CALLED AFTER POST /v1/responses RETURNS AND BEFORE chat.startStream, which is the whole point of
// its existence as a separate call from RecordStreamTS: the window between those two is where a killed
// process leaves a live run with no Slack message, and a design that only wrote the stream ts would have
// no record of it. From this write until EndDelivery, this thread is one the recovery scan will pick up.
//
// It does NOT touch last_sequence — it RETURNS it, and that return value fixes something older than this
// column. A second run in the same session continues the same journal, so the new stream must be opened
// after the previous run's terminal or it reads that terminal first and closes on it, rendering the
// PREVIOUS answer's ending instead of this one's. That cursor used to live only in memory
// (relay/inbound.go's inboundState), so it was zero for the first message in every thread after every
// restart — the exact case a long-lived thread is in all day. Reading it back here makes the resume point
// survive the process, which is the same property the rest of this file is about.
//
// The only things that reset it are a rebind to a different session (RebindThread) and a first bind (the
// column's default), because a sequence number means nothing outside the journal it came from.
func (s *Store) BeginDelivery(ctx context.Context, botID, teamID, channelID, threadTS, recipientUserID string) (int64, error) {
	var lastSequence int64
	err := s.pool.QueryRow(ctx,
		`UPDATE thread_sessions
		 SET run_pending = true, stream_ts = '', recipient_user_id = $5
		 WHERE bot_id = $1 AND team_id = $2 AND channel_id = $3 AND thread_ts = $4
		 RETURNING last_sequence`,
		botID, teamID, channelID, threadTS, recipientUserID,
	).Scan(&lastSequence)
	if errors.Is(err, pgx.ErrNoRows) {
		// The thread has no row at all. Every caller binds before it starts a run (relay/inbound.go
		// openNewSession), so this is not a state this bot reaches on its own — it is somebody deleting the
		// row underneath a live turn. Refuse rather than start a run nothing will be able to recover.
		return 0, fmt.Errorf("store: BeginDelivery: %s/%s/%s/%s has no binding to record a run against", botID, teamID, channelID, threadTS)
	}
	if err != nil {
		return 0, fmt.Errorf("store: begin delivery: %w", err)
	}
	return lastSequence, nil
}

// RecordStreamTS names the Slack message this thread's pending run is being written into, so a later
// process can finish THAT message instead of posting a second one.
func (s *Store) RecordStreamTS(ctx context.Context, botID, teamID, channelID, threadTS, streamTS string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE thread_sessions SET stream_ts = $5
		 WHERE bot_id = $1 AND team_id = $2 AND channel_id = $3 AND thread_ts = $4`,
		botID, teamID, channelID, threadTS, streamTS,
	)
	if err != nil {
		return fmt.Errorf("store: record stream ts: %w", err)
	}
	return nil
}

// RecordDelivered advances the thread's delivered-through cursor.
//
// GREATEST rather than a plain assignment: the caller is a single goroutine per thread today, but the
// column's meaning is "everything up to here has reached Slack" and a monotonic write is the only shape
// that cannot be walked BACKWARD by an out-of-order call — and walking it backward is the one direction
// that costs a reader something, since it re-sends text they have already read.
func (s *Store) RecordDelivered(ctx context.Context, botID, teamID, channelID, threadTS string, sequence int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE thread_sessions SET last_sequence = GREATEST(last_sequence, $5)
		 WHERE bot_id = $1 AND team_id = $2 AND channel_id = $3 AND thread_ts = $4`,
		botID, teamID, channelID, threadTS, sequence,
	)
	if err != nil {
		return fmt.Errorf("store: record delivered sequence: %w", err)
	}
	return nil
}

// EndDelivery records that this thread's run reached its terminal and its Slack message is closed, so
// recovery has nothing left to do for it.
//
// IT IS CALLED ONLY ON A REAL TERMINAL. A relay that stops for any other reason — a read error, a
// shutdown, a panic — deliberately leaves the row pending, because "we stopped reading" is precisely the
// state a restart must pick up. The cursor is left where it is rather than cleared: it is what the next
// run in the same session resumes from.
func (s *Store) EndDelivery(ctx context.Context, botID, teamID, channelID, threadTS string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE thread_sessions SET run_pending = false, stream_ts = ''
		 WHERE bot_id = $1 AND team_id = $2 AND channel_id = $3 AND thread_ts = $4`,
		botID, teamID, channelID, threadTS,
	)
	if err != nil {
		return fmt.Errorf("store: end delivery: %w", err)
	}
	return nil
}

// PendingDeliveries lists every thread of botID's that owes somebody an answer — the boot scan's whole
// input. Ordered by thread so a run of the scan is reproducible and a log of it can be compared with the
// next one; the set is normally empty, since a run that finishes clears its own row.
func (s *Store) PendingDeliveries(ctx context.Context, botID string) ([]PendingRun, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT team_id, channel_id, thread_ts, session_id, last_sequence, stream_ts, recipient_user_id
		 FROM thread_sessions
		 WHERE bot_id = $1 AND run_pending
		 ORDER BY team_id, channel_id, thread_ts`,
		botID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list pending deliveries: %w", err)
	}
	defer rows.Close()
	var out []PendingRun
	for rows.Next() {
		var p PendingRun
		if err := rows.Scan(&p.TeamID, &p.ChannelID, &p.ThreadTS, &p.SessionID, &p.LastSequence, &p.StreamTS, &p.RecipientUserID); err != nil {
			return nil, fmt.Errorf("store: scan pending delivery: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list pending deliveries: %w", err)
	}
	return out, nil
}

// ErrAmbiguousSession is ThreadForSession finding a session id bound to more than one thread.
//
// It cannot happen through this package's own writes — BindThread mints a session per thread and
// RebindThread only ever moves one row — so it is a refusal rather than a pick. The alternative is the
// shape this tree has already been bitten by twice: a LIMIT 1 with nothing deciding WHICH row, answering
// confidently with one of two possible threads. An approval question posted into the wrong thread is a
// gated action shown to the wrong audience, so guessing is the one thing this lookup must not do.
var ErrAmbiguousSession = errors.New("store: this session id is bound to more than one thread")

// ThreadForSession is the ONE reverse lookup in this schema: an approval row names a session and nothing
// else (GET /v1/approvals carries no Slack anything), so the sweep has to find the thread to ask in.
//
// ORDER BY + LIMIT 2 is the ambiguity check, not pagination: reading a second row is how this call learns
// that the answer is not unique, which a LIMIT 1 could never tell it. See ErrAmbiguousSession.
func (s *Store) ThreadForSession(ctx context.Context, botID, sessionID string) (PendingRun, bool, error) {
	if sessionID == "" {
		return PendingRun{}, false, errors.New("store: ThreadForSession: sessionID is required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT team_id, channel_id, thread_ts, session_id, last_sequence, stream_ts, recipient_user_id
		 FROM thread_sessions
		 WHERE bot_id = $1 AND session_id = $2
		 ORDER BY team_id, channel_id, thread_ts
		 LIMIT 2`,
		botID, sessionID,
	)
	if err != nil {
		return PendingRun{}, false, fmt.Errorf("store: resolve thread for session: %w", err)
	}
	defer rows.Close()
	var found []PendingRun
	for rows.Next() {
		var p PendingRun
		if err := rows.Scan(&p.TeamID, &p.ChannelID, &p.ThreadTS, &p.SessionID, &p.LastSequence, &p.StreamTS, &p.RecipientUserID); err != nil {
			return PendingRun{}, false, fmt.Errorf("store: scan thread for session: %w", err)
		}
		found = append(found, p)
	}
	if err := rows.Err(); err != nil {
		return PendingRun{}, false, fmt.Errorf("store: resolve thread for session: %w", err)
	}
	switch len(found) {
	case 0:
		return PendingRun{}, false, nil
	case 1:
		return found[0], true, nil
	default:
		return PendingRun{}, false, fmt.Errorf("%w: %s", ErrAmbiguousSession, sessionID)
	}
}

// ClaimApprovalPost reserves the right to put approvalID's question into Slack, exactly once across every
// producer this bot has (the live event arm and the sweep — see approval_posts's own note).
//
// won=false means somebody already claimed it and a human can already see the buttons, so the caller posts
// NOTHING. The claim is taken BEFORE the post rather than recorded after it, because the hazard being
// closed is two posts of one question, and a record written afterwards leaves both posters inside the
// window. The other direction — a claim whose post then fails — is closed by ReleaseApprovalPost, so the
// row that survives means "a human can see this", not "we tried".
func (s *Store) ClaimApprovalPost(ctx context.Context, botID, approvalID, channelID, threadTS string) (bool, error) {
	if approvalID == "" {
		return false, errors.New("store: ClaimApprovalPost: approvalID is required")
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO approval_posts (bot_id, approval_id, channel_id, thread_ts)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (bot_id, approval_id) DO NOTHING`,
		botID, approvalID, channelID, threadTS,
	)
	if err != nil {
		return false, fmt.Errorf("store: claim approval post: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseApprovalPost gives back a claim whose post did not land, so the next sweep asks again. Without it
// a single Slack hiccup would mark a question as asked forever and the run would park on a human who was
// never shown anything — the exact failure this whole file exists to make impossible.
func (s *Store) ReleaseApprovalPost(ctx context.Context, botID, approvalID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM approval_posts WHERE bot_id = $1 AND approval_id = $2`,
		botID, approvalID,
	)
	if err != nil {
		return fmt.Errorf("store: release approval post: %w", err)
	}
	return nil
}
