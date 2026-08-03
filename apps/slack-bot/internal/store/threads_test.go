//go:build component

package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// openTestStore opens a Store against PALAI_COMPONENT_POSTGRES_URL and migrates it. This file carries
// the `component` build tag, the SAME tag every other `_component_test.go` in this tree carries
// (apps/control-plane/internal/execution/runner_placement_component_test.go and siblings), and reads
// the SAME conventional variable name those packages do — so it now matches BOTH halves that make a
// component test actually run somewhere: the tag `scripts/test/component` builds with
// (`go test -tags=component`), and the env var its `slack-bot` suite (added alongside this fix) sets
// after starting its own throwaway Postgres container, mirroring run_queue's shape (one container, one
// package, no other suite shares it, so no -run allow-list is needed here). Run it with
// `TEST=slack-bot scripts/test/component` — see that script for the exact container wiring.
//
// What this does NOT yet match: CI. This repository's CI runs `make verify` only and has no
// component tier at all (no workflow step ever sets TEST= or calls scripts/test/component), so tagging
// and naming this correctly makes the suite RUNNABLE AND NAMED, not CI-green — that gap is a property
// of CI, not of this file, and stays open after this change.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if dsn == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required to run store tests against a real Postgres")
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

// TestBindThreadRejectsAnEmptySessionID and the two Rebind variants below lock the one mechanical guard
// against inventing a session id (see threads.go's package doc): an empty string is refused before any
// of the three ever touch the database.
func TestBindThreadRejectsAnEmptySessionID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.BindThread(ctx, "bot_1", "T1", "C1", "222.2", ""); err == nil {
		t.Fatal("BindThread accepted an empty session id")
	}
}

func TestRebindThreadRejectsAnEmptyOldSessionID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.RebindThread(ctx, "bot_1", "T1", "C1", "222.2", "", "sess_new"); err == nil {
		t.Fatal("RebindThread accepted an empty old session id")
	}
}

func TestRebindThreadRejectsAnEmptyNewSessionID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.RebindThread(ctx, "bot_1", "T1", "C1", "222.2", "sess_old", ""); err == nil {
		t.Fatal("RebindThread accepted an empty new session id")
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
	// opens sess_replacement in its place, and calls RebindThread naming the id it read (sess_dead) as
	// the compare value. That call is reproduced directly here.
	won, err := s.RebindThread(ctx, "bot_1", "T1", "C1", threadTS, "sess_dead", "sess_replacement")
	if err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Fatal("RebindThread lost a CAS against the session id it had just read; nothing else touched this thread")
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

// TestRebindThreadConcurrentCallersOnlyOneWins is the concrete failure the review found: two Slack
// events land on the same orphaned thread near-simultaneously, both read the same stale session id
// (SessionForThread returning before either writes), both get a 404 from Palai, both open a brand-new
// session, and both call RebindThread naming the SAME old id. A blind `ON CONFLICT ... DO UPDATE`
// would let the second silently clobber the first, orphaning the first goroutine's freshly-opened
// session with no row anywhere pointing at it. The CAS must instead let exactly one caller win — the
// other's job (not this store's) is to close the session it opened, using won=false as the signal.
func TestRebindThreadConcurrentCallersOnlyOneWins(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	threadTS := fmt.Sprintf("444.%d", time.Now().UnixNano())

	if _, err := s.BindThread(ctx, "bot_1", "T1", "C1", threadTS, "sess_dead"); err != nil {
		t.Fatal(err)
	}

	const n = 10
	newIDs := make([]string, n)
	won := make([]bool, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		newIDs[i] = fmt.Sprintf("sess_new_%d", i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			won[i], errs[i] = s.RebindThread(ctx, "bot_1", "T1", "C1", threadTS, "sess_dead", newIDs[i])
		}(i)
	}
	wg.Wait()

	winners := 0
	winningID := ""
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if won[i] {
			winners++
			winningID = newIDs[i]
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly 1 winning RebindThread call among %d concurrent CAS attempts, got %d", n, winners)
	}

	got, found, err := s.SessionForThread(ctx, "bot_1", "T1", "C1", threadTS)
	if err != nil {
		t.Fatal(err)
	}
	if !found || got != winningID {
		t.Fatalf("SessionForThread = (%q, found=%v) after the race, want the sole winner %q", got, found, winningID)
	}
}
