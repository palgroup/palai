package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// openTestStore opens a Store against PALAI_BOT_TEST_DATABASE_URL and migrates it, skipping the test
// entirely when that variable is unset so `go test ./apps/slack-bot/...` stays green with no Postgres
// reachable — the shape apps/control-plane/internal/execution's own component tests use
// (PALAI_COMPONENT_POSTGRES_URL). Point PALAI_BOT_TEST_DATABASE_URL at a database dedicated to this
// bot's tests, never at Palai's own: this file creates thread_sessions in whatever database the DSN
// names, and Palai's schema is not this package's to write into.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("PALAI_BOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PALAI_BOT_TEST_DATABASE_URL is required to run store tests against a real Postgres")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open(%q): %v", dsn, err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

// TestASecondEventInTheSameThreadReusesTheSession is SLK-003's own test, unchanged from the plan: a
// second event in the same thread must resolve the SAME session, not open a new one.
func TestASecondEventInTheSameThreadReusesTheSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	first, err := s.BindThread(ctx, "bot_1", "T1", "C1", "111.1", "sess_1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.BindThread(ctx, "bot_1", "T1", "C1", "111.1", "sess_2")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("thread bound twice: %q then %q", first, second)
	}
}

// TestTwoBotsInOneThreadDoNotShareASession fences this plan's multi-bot requirement: the old
// slack_thread_sessions table (000035_slack.up.sql) had no bot_id column, so two bots in the same
// Slack channel and thread could never have been told apart there.
func TestTwoBotsInOneThreadDoNotShareASession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a, err := s.BindThread(ctx, "bot_ios", "T1", "C1", "111.1", "sess_a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.BindThread(ctx, "bot_android", "T1", "C1", "111.1", "sess_b")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two bots collapsed onto one session; bot_id is part of the key")
	}
}

// TestSessionForThreadNotFoundForAnUnboundThread checks the not-found path directly: a thread key no
// test in this file ever binds must come back found=false, not a zero-value session id mistaken for a
// real one.
func TestSessionForThreadNotFoundForAnUnboundThread(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	sessionID, found, err := s.SessionForThread(ctx, "bot_never_bound", "T404", "C404", "999.9")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatalf("SessionForThread found a binding that was never made: %q", sessionID)
	}
}

// TestBindThreadRejectsAnEmptySessionID and TestRebindThreadRejectsAnEmptySessionID lock the one
// mechanical guard against inventing a session id (see threads.go's package doc): an empty string is
// refused before either method ever touches the database.
func TestBindThreadRejectsAnEmptySessionID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.BindThread(ctx, "bot_1", "T1", "C1", "222.2", ""); err == nil {
		t.Fatal("BindThread accepted an empty session id")
	}
}

func TestRebindThreadRejectsAnEmptySessionID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.RebindThread(ctx, "bot_1", "T1", "C1", "222.2", ""); err == nil {
		t.Fatal("RebindThread accepted an empty session id")
	}
}

// TestRebindThreadReplacesAnOrphanedSession is the orphan-recovery test the plan requires: a thread's
// previously bound session no longer exists on Palai's side (a 404 from Sessions), so the caller opens
// a new one and the mapping must move to it — not accumulate a second row (the PRIMARY KEY forbids
// that) and not keep resolving to the dead session id. The thread_ts carries a fresh timestamp suffix
// so re-running this test against a persistent database never collides with its own previous run's
// already-rebound row (unlike the two tests above, this one is NOT idempotent across runs — it
// mutates an existing binding rather than only reading or first-writing one).
func TestRebindThreadReplacesAnOrphanedSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	threadTS := fmt.Sprintf("333.%d", time.Now().UnixNano())

	bound, err := s.BindThread(ctx, "bot_1", "T1", "C1", threadTS, "sess_dead")
	if err != nil {
		t.Fatal(err)
	}
	if bound != "sess_dead" {
		t.Fatalf("BindThread on a fresh thread returned %q, want the session it was called with", bound)
	}

	// The real caller (Task 9) learns sess_dead is gone from a 404 calling the SDK's Sessions API,
	// opens sess_replacement in its place, and calls RebindThread with the new id. That call is
	// reproduced directly here.
	if err := s.RebindThread(ctx, "bot_1", "T1", "C1", threadTS, "sess_replacement"); err != nil {
		t.Fatal(err)
	}

	got, found, err := s.SessionForThread(ctx, "bot_1", "T1", "C1", threadTS)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("SessionForThread lost the binding after RebindThread")
	}
	if got != "sess_replacement" {
		t.Fatalf("SessionForThread = %q after RebindThread, want %q", got, "sess_replacement")
	}

	// A later BindThread must not resurrect the dead session: RebindThread's write is now the sticky
	// value, exactly like any binding BindThread itself established.
	again, err := s.BindThread(ctx, "bot_1", "T1", "C1", threadTS, "sess_dead")
	if err != nil {
		t.Fatal(err)
	}
	if again != "sess_replacement" {
		t.Fatalf("BindThread after a rebind returned %q, want the rebound %q", again, "sess_replacement")
	}
}
