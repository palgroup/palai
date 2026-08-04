//go:build component

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// These are the durable-delivery half of this schema (migrations/0002_delivery_state.sql) against a REAL
// Postgres. Every one of them is here because the property it asserts lives in SQL rather than in Go — a
// GREATEST, a RETURNING, an ON CONFLICT, a LIMIT 2 — and an in-memory double reproducing that SQL proves
// only that the double and the test agree with each other.
//
// They run under `TEST=slack-bot scripts/test/component`, which builds this package with the `component`
// tag and points PALAI_COMPONENT_POSTGRES_URL at a throwaway container. That suite takes the whole package
// with no -run allow-list, so these run without the script being edited to name them.

// seed binds a thread and returns the four ids that address it, so each test below reads as the property
// it is about rather than as four repeated id strings.
//
// THE BOT ID IS THE TEST'S OWN NAME, and that is not cosmetic. PendingDeliveries and ThreadForSession are
// both scoped to a bot, and this suite shares ONE database across every test in the package — so a test
// that asserted "no thread is pending" under a shared bot id would be asserting something about its
// neighbours' leftovers rather than about itself. It fails that way too: the first draft of this file did
// exactly that and went red for a reason that had nothing to do with what it claimed. Per-test bot ids make
// each scan genuinely about the rows the test wrote, which is also how production scans — one bot, its own
// threads.
func seed(t *testing.T, s *Store, thread, session string) (bot, team, channel, threadTS string) {
	t.Helper()
	if _, err := s.BindThread(context.Background(), t.Name(), "T1", "C1", thread, session); err != nil {
		t.Fatalf("BindThread: %v", err)
	}
	return t.Name(), "T1", "C1", thread
}

// A THREAD IS NOT PENDING UNTIL A RUN IS ACCEPTED FOR IT, and a fresh bind is not a run. If a bind alone
// marked a thread pending, every thread this bot has ever seen would be re-attached on every boot.
func TestABoundThreadIsNotPendingUntilARunBegins(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bot, _, _, _ := seed(t, s, "d.1", "sess_d1")
	pending, err := s.PendingDeliveries(ctx, bot)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("a freshly bound thread is already pending: %v", pending)
	}
}

// THE FULL LIFECYCLE, in the order a run walks it, ending where a boot scan finds nothing left to do.
func TestARunThatBeginsIsPendingUntilItEnds(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bot, team, ch, thread := seed(t, s, "d.2", "sess_d2")

	if _, err := s.BeginDelivery(ctx, bot, team, ch, thread, "U_HUMAN"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordStreamTS(ctx, bot, team, ch, thread, "1785000000.000900"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDelivered(ctx, bot, team, ch, thread, 12); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingDeliveries(ctx, bot)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d pending row(s), want 1", len(pending))
	}
	got := pending[0]
	// Every field a recovery needs comes back, because a recovery that had to guess any of them could not
	// happen: an empty recipient makes chat.startStream refuse, and a wrong ts closes the wrong message.
	if got.SessionID != "sess_d2" || got.LastSequence != 12 || got.StreamTS != "1785000000.000900" || got.RecipientUserID != "U_HUMAN" {
		t.Fatalf("the pending row is %+v, want session sess_d2, sequence 12, ts 1785000000.000900, recipient U_HUMAN", got)
	}

	if err := s.EndDelivery(ctx, bot, team, ch, thread); err != nil {
		t.Fatal(err)
	}
	pending, err = s.PendingDeliveries(ctx, bot)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("a finished run is still pending: %v", pending)
	}
}

// THE CURSOR NEVER WALKS BACKWARD. This is the GREATEST in the UPDATE, and it is the direction that costs
// a reader something: a cursor moved back re-sends text they have already read.
func TestTheDeliveredCursorOnlyMovesForward(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bot, team, ch, thread := seed(t, s, "d.3", "sess_d3")
	if _, err := s.BeginDelivery(ctx, bot, team, ch, thread, "U_HUMAN"); err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int64{5, 9, 3, 7} {
		if err := s.RecordDelivered(ctx, bot, team, ch, thread, seq); err != nil {
			t.Fatal(err)
		}
	}
	resumeFrom, err := s.BeginDelivery(ctx, bot, team, ch, thread, "U_HUMAN")
	if err != nil {
		t.Fatal(err)
	}
	if resumeFrom != 9 {
		t.Fatalf("the cursor is %d after recording 5,9,3,7 — want 9", resumeFrom)
	}
}

// BeginDelivery HANDS BACK THE RESUME POINT, which is the return value that fixes the older, restart-shaped
// defect: a new process would otherwise open a thread's next run at sequence 0, read the PREVIOUS run's
// terminal first and close on it.
func TestBeginDeliveryReturnsTheCursorTheNextRunResumesFrom(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bot, team, ch, thread := seed(t, s, "d.4", "sess_d4")
	if _, err := s.BeginDelivery(ctx, bot, team, ch, thread, "U_HUMAN"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDelivered(ctx, bot, team, ch, thread, 28); err != nil {
		t.Fatal(err)
	}
	if err := s.EndDelivery(ctx, bot, team, ch, thread); err != nil {
		t.Fatal(err)
	}
	// A BRAND NEW Store over the SAME database is what a restarted process has.
	fresh := openTestStore(t)
	resumeFrom, err := fresh.BeginDelivery(ctx, bot, team, ch, thread, "U_HUMAN")
	if err != nil {
		t.Fatal(err)
	}
	if resumeFrom != 28 {
		t.Fatalf("a new process resumes this thread at %d, want 28 — at 0 it would render the previous answer's ending", resumeFrom)
	}
	// And the new run starts with no stream of its own, whatever the last one left behind.
	pending, err := fresh.PendingDeliveries(ctx, bot)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].StreamTS != "" {
		t.Fatalf("the new run inherited a stream ts: %+v", pending)
	}
}

// A RUN CANNOT BE RECORDED AGAINST A THREAD THAT IS NOT BOUND. The UPDATE matches nothing, and the RETURNING
// yields no row — which must surface as a refusal rather than as a silent zero, because a run nothing can
// name is a run nothing can recover.
func TestBeginDeliveryRefusesAnUnboundThread(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.BeginDelivery(context.Background(), t.Name(), "T1", "C1", "never.bound", "U_HUMAN"); err == nil {
		t.Fatal("a run was recorded against a thread with no binding")
	}
}

// A REBIND CLEARS THE WHOLE DELIVERY ROW IN THE SAME STATEMENT. A cursor is meaningful only inside the
// journal it came from, and the pending run it named was on a session that no longer exists — a recovery
// that inherited either would re-attach to a 404 or resume a new journal at a stranger's sequence number.
func TestRebindingAThreadResetsItsDeliveryState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bot, team, ch, thread := seed(t, s, "d.5", "sess_old")
	if _, err := s.BeginDelivery(ctx, bot, team, ch, thread, "U_HUMAN"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDelivered(ctx, bot, team, ch, thread, 42); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordStreamTS(ctx, bot, team, ch, thread, "1785000000.001100"); err != nil {
		t.Fatal(err)
	}
	won, err := s.RebindThread(ctx, bot, team, ch, thread, "sess_old", "sess_new")
	if err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Fatal("the rebind did not land")
	}
	pending, err := s.PendingDeliveries(ctx, bot)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("the rebound thread is still pending on the dead session: %+v", pending)
	}
	resumeFrom, err := s.BeginDelivery(ctx, bot, team, ch, thread, "U_HUMAN")
	if err != nil {
		t.Fatal(err)
	}
	if resumeFrom != 0 {
		t.Fatalf("the new session resumes at %d, want 0 — that number came from a different journal", resumeFrom)
	}
}

// THE REVERSE LOOKUP REFUSES RATHER THAN GUESSES. An approval row names only a session; if two threads
// somehow named it, picking one would post a gated decision's buttons in front of the wrong audience. This
// tree has shipped a LIMIT-1-with-no-ORDER-BY deciding a security outcome twice, and this is the same shape.
func TestThreadForSessionRefusesAnAmbiguousSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bot, _, _, _ := seed(t, s, "d.6a", "sess_shared")
	seed(t, s, "d.6b", "sess_shared")
	if _, _, err := s.ThreadForSession(ctx, bot, "sess_shared"); !errors.Is(err, ErrAmbiguousSession) {
		t.Fatalf("ThreadForSession answered %v for a session bound to two threads, want ErrAmbiguousSession", err)
	}
}

// ...and answers cleanly for the one and the none.
func TestThreadForSessionResolvesOneAndReportsNone(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bot, _, _, _ := seed(t, s, "d.7", "sess_d7")
	got, found, err := s.ThreadForSession(ctx, bot, "sess_d7")
	if err != nil || !found {
		t.Fatalf("ThreadForSession(sess_d7) = (%+v, %v, %v)", got, found, err)
	}
	if got.ChannelID != "C1" || got.ThreadTS != "d.7" {
		t.Fatalf("resolved %s/%s, want C1/d.7", got.ChannelID, got.ThreadTS)
	}
	// A session this bot never bound — a console run, another bot — is not an error and must not be.
	if _, found, err := s.ThreadForSession(ctx, bot, "sess_console"); err != nil || found {
		t.Fatalf("a foreign session resolved to (%v, %v), want (false, nil)", found, err)
	}
	// Nor is another bot's session visible through this bot's key.
	if _, found, err := s.ThreadForSession(ctx, "bot_other", "sess_d7"); err != nil || found {
		t.Fatalf("bot_other saw bot_d's session: (%v, %v)", found, err)
	}
}

// THE CLAIM IS WON EXACTLY ONCE. This is the ON CONFLICT, and it is what keeps the live arm and the sweep
// from asking a human the same question twice.
func TestAnApprovalPostIsClaimedExactlyOnce(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	first, err := s.ClaimApprovalPost(ctx, t.Name(), "appr_c1", "C1", "d.8")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimApprovalPost(ctx, t.Name(), "appr_c1", "C1", "d.8")
	if err != nil {
		t.Fatal(err)
	}
	if !first || second {
		t.Fatalf("the claim was won %v then %v, want true then false", first, second)
	}
	// Released, it can be claimed again — the retry path for a post that did not land.
	if err := s.ReleaseApprovalPost(ctx, t.Name(), "appr_c1"); err != nil {
		t.Fatal(err)
	}
	again, err := s.ClaimApprovalPost(ctx, t.Name(), "appr_c1", "C1", "d.8")
	if err != nil {
		t.Fatal(err)
	}
	if !again {
		t.Fatal("a released claim could not be retaken — the question would never be asked again")
	}
}

// TWO BOTS CLAIM INDEPENDENTLY. The claim is partitioned by bot for the same reason thread_sessions is: an
// iOS bot and an Android bot in one workspace each owe their own humans their own question.
func TestTwoBotsClaimTheSameApprovalIndependently(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, bot := range []string{"bot_p", "bot_q"} {
		won, err := s.ClaimApprovalPost(ctx, bot, "appr_shared", "C1", "d.9")
		if err != nil {
			t.Fatal(err)
		}
		if !won {
			t.Fatalf("%s could not claim an approval another bot holds", bot)
		}
	}
}

// PendingDeliveries IS SCOPED TO ONE BOT. A boot scan that picked up another bot's threads would render
// that bot's answers with this bot's token, into threads it was never invited to.
func TestPendingDeliveriesAreScopedToOneBot(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i, bot := range []string{"bot_x", "bot_y"} {
		thread := fmt.Sprintf("d.10.%d", i)
		if _, err := s.BindThread(ctx, bot, "T1", "C1", thread, fmt.Sprintf("sess_%s", bot)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BeginDelivery(ctx, bot, "T1", "C1", thread, "U_HUMAN"); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := s.PendingDeliveries(ctx, "bot_x")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].SessionID != "sess_bot_x" {
		t.Fatalf("bot_x's scan returned %+v, want only its own thread", pending)
	}
}
