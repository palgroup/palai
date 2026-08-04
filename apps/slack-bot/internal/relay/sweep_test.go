package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	palai "github.com/palgroup/palai/sdks/go"
)

// sweepFixture wires SweepApprovals to a store that already knows one thread, so a test can say "this
// session belongs to C1/1.1" and "this one belongs to nobody" without restating the plumbing.
type sweepFixture struct {
	deps  SweepApprovalDeps
	palai *fakeApprovalsPalai
	slack *fakeApprovalSlack
	store *fakeStore

	mu   sync.Mutex
	logs []string
}

func newSweepFixture(t *testing.T, approvals ...palai.Approval) *sweepFixture {
	t.Helper()
	f := &sweepFixture{
		palai: &fakeApprovalsPalai{pages: []palai.Page[palai.Approval]{{Data: approvals}}},
		slack: &fakeApprovalSlack{},
		store: newFakeStore(),
	}
	if _, err := f.store.BindThread(context.Background(), "bot_1", "T1", "C1", "1.1", "sess_ours"); err != nil {
		t.Fatalf("seed BindThread: %v", err)
	}
	f.deps = SweepApprovalDeps{
		Approvals: ApprovalDeps{Palai: f.palai, Slack: f.slack, Posts: f.store, BotID: "bot_1"},
		Threads:   f.store,
		Logf: func(format string, args ...any) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.logs = append(f.logs, fmt.Sprintf(format, args...))
		},
	}
	return f
}

func (f *sweepFixture) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.logs, "\n")
}

// ours builds an approval row on the thread the fixture bound — the shape GET /v1/approvals answers with
// for a gated tool call (a tool row's Identity/OperatorLabel/Arguments are derived server-side).
func ours(id string) palai.Approval {
	return palai.Approval{ID: id, SessionID: "sess_ours", RequestHash: "rh_" + id, Kind: "tool",
		Identity: "palai.workspace.shell", OperatorLabel: "Run a command", Arguments: `{"command":"ls"}`}
}

// THE HOLE THIS CLOSES, in one test. A run parked on a human while this process was down journalled its
// approval.requested.v1 into a stream nobody was reading, so the live arm never fired: nobody was ever
// asked, nothing timed out visibly, and the thread simply stopped. The sweep is the only thing that can
// discover it after the fact, because the approval is still OPEN and GET /v1/approvals still lists it.
func TestTheSweepAsksAboutAParkedRunNobodyWasAskedAbout(t *testing.T) {
	f := newSweepFixture(t, ours("appr_1"))
	if err := SweepApprovals(context.Background(), f.deps); err != nil {
		t.Fatalf("SweepApprovals: %v", err)
	}
	if len(f.slack.posted) != 1 {
		t.Fatalf("%d approval message(s) posted, want 1 — the run is parked and nobody can see the buttons", len(f.slack.posted))
	}
	if !strings.Contains(string(f.slack.posted[0]), "C1") {
		t.Fatalf("the question was posted to the wrong place: %s", f.slack.posted[0])
	}
}

// AND IT ASKS ONLY ONCE, however many times it runs. This is the hazard the sweep introduces and the
// reason the claim exists: an approval stays open until somebody CLICKS it, so every tick sees the same
// row again. Without the claim a reader would collect one identical question with working buttons per
// interval, and would have no way to tell which pair was real.
//
// THE LOG IS ASSERTED ALONGSIDE THE POSTS, and that half is here because the first version of this file
// asserted only the posts and shipped a defect the LIVE proof caught instead: nothing was double-posted,
// but the sweep counted every already-claimed row as an asking, so a single parked approval made it report
// "asked about 1 parked run(s)" every fifteen seconds forever. A test that reads only the Slack side
// cannot see that — the Slack side was correct. What was wrong was the sentence describing it.
func TestTheSweepAsksAboutTheSameApprovalExactlyOnceAndSaysSoOnlyOnce(t *testing.T) {
	f := newSweepFixture(t, ours("appr_1"))
	for i := 0; i < 4; i++ {
		if err := SweepApprovals(context.Background(), f.deps); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if len(f.slack.posted) != 1 {
		t.Fatalf("four sweeps posted %d message(s), want 1 — an open approval is listed on every tick", len(f.slack.posted))
	}
	if n := strings.Count(f.log(), "approval sweep asked about"); n != 1 {
		t.Fatalf("four sweeps reported asking %d time(s), want 1 — a line claiming a human was just asked, about a question asked once, is what an operator would chase:\n%s", n, f.log())
	}
}

// THE TWO PRODUCERS DO NOT DOUBLE UP EITHER, which is the case that actually happens in production: the
// live event arm asks the instant the run parks, and the sweep runs seconds later while the approval is
// still open. They share one claim, so the second one posts nothing.
func TestTheLiveArmAndTheSweepDoNotBothAskTheSameQuestion(t *testing.T) {
	f := newSweepFixture(t, ours("appr_1"))
	live := palai.Event{Type: ApprovalRequestedEventType, SessionID: "sess_ours",
		Data: map[string]any{"request_hash": "rh_appr_1"}}
	if err := OnApprovalRequested(context.Background(), f.deps.Approvals, "C1", "1.1", live); err != nil {
		t.Fatalf("OnApprovalRequested: %v", err)
	}
	if err := SweepApprovals(context.Background(), f.deps); err != nil {
		t.Fatalf("SweepApprovals: %v", err)
	}
	if len(f.slack.posted) != 1 {
		t.Fatalf("the live arm and the sweep posted %d message(s) between them, want 1", len(f.slack.posted))
	}
}

// A FAILED POST GIVES THE CLAIM BACK. Without this the de-duplication would become a NEW silent loss: one
// Slack hiccup would mark the question as asked forever, and the run would park on a human who was shown
// nothing — the exact failure this whole change exists to prevent, reintroduced by its own fix.
func TestAFailedPostIsRetriedByTheNextSweep(t *testing.T) {
	f := newSweepFixture(t, ours("appr_1"))
	f.slack.postErr = errors.New("slack is down")
	if err := SweepApprovals(context.Background(), f.deps); err != nil {
		t.Fatalf("SweepApprovals: %v", err)
	}
	if !strings.Contains(f.log(), "NOBODY HAS BEEN ASKED") {
		t.Fatalf("a failed post was not reported; the log said:\n%s", f.log())
	}
	f.slack.postErr = nil
	if err := SweepApprovals(context.Background(), f.deps); err != nil {
		t.Fatalf("second SweepApprovals: %v", err)
	}
	// posted counts ATTEMPTS in this double, so the retry is the second entry: what matters is that the
	// second sweep tried at all rather than treating the failed claim as an asked question.
	if len(f.slack.posted) != 2 {
		t.Fatalf("the retry made %d post attempt(s), want 2 — a claim whose post failed must not count as asked", len(f.slack.posted))
	}
}

// AN APPROVAL FROM SOMEWHERE ELSE IS NOT PUT IN FRONT OF A SLACK CHANNEL. GET /v1/approvals is
// PROJECT-scoped: it lists every gated call in the tenant, including runs started from the console, the
// CLI or another bot. A sweep that posted those would take a decision meant for somebody else and hand a
// Slack channel working buttons for it.
func TestTheSweepIgnoresApprovalsThatBelongToNoThreadOfThisBots(t *testing.T) {
	foreign := palai.Approval{ID: "appr_console", SessionID: "sess_console", RequestHash: "rh_c", Kind: "tool"}
	f := newSweepFixture(t, foreign)
	if err := SweepApprovals(context.Background(), f.deps); err != nil {
		t.Fatalf("SweepApprovals: %v", err)
	}
	if len(f.slack.posted) != 0 {
		t.Fatalf("%d message(s) posted for an approval belonging to another surface: %s", len(f.slack.posted), f.slack.posted[0])
	}
}

// ONE BROKEN ROW DOES NOT STARVE THE REST. A sweep that returned at the first failure would let a single
// unresolvable session block every other parked run in the workspace from ever being asked about.
func TestOneUnpostableApprovalDoesNotStopTheSweep(t *testing.T) {
	// Two threads, two sessions; one of them is bound TWICE, which is what makes ThreadForSession refuse
	// rather than guess (store.ErrAmbiguousSession).
	f := newSweepFixture(t, palai.Approval{ID: "appr_bad", SessionID: "sess_dup", RequestHash: "rh_b", Kind: "tool"}, ours("appr_good"))
	for _, thread := range []string{"9.1", "9.2"} {
		if _, err := f.store.BindThread(context.Background(), "bot_1", "T1", "C9", thread, "sess_dup"); err != nil {
			t.Fatalf("seed BindThread: %v", err)
		}
	}
	if err := SweepApprovals(context.Background(), f.deps); err != nil {
		t.Fatalf("SweepApprovals: %v", err)
	}
	if len(f.slack.posted) != 1 {
		t.Fatalf("%d message(s) posted, want 1 — the good row must still be asked about", len(f.slack.posted))
	}
	if !strings.Contains(f.log(), "could not resolve the thread") {
		t.Fatalf("the ambiguous row was not reported; the log said:\n%s", f.log())
	}
}

// AN EMPTY SWEEP SAYS NOTHING. It runs every few seconds for the whole life of the process, so a line per
// tick would bury the output an operator actually reads.
func TestAnEmptySweepIsSilent(t *testing.T) {
	f := newSweepFixture(t)
	if err := SweepApprovals(context.Background(), f.deps); err != nil {
		t.Fatalf("SweepApprovals: %v", err)
	}
	if f.log() != "" {
		t.Fatalf("an empty sweep logged %q, want nothing", f.log())
	}
}

// THE SWEEP REFUSES TO RUN WITHOUT THE CLAIM STORE, rather than degrading into the double-posting version
// of itself. A sweep with no de-duplication is worse than no sweep: it turns a missed question into a
// stream of duplicate ones.
func TestTheSweepRefusesWithoutItsClaimStore(t *testing.T) {
	f := newSweepFixture(t, ours("appr_1"))
	f.deps.Approvals.Posts = nil
	if err := SweepApprovals(context.Background(), f.deps); err == nil {
		t.Fatal("the sweep ran with no claim store — every tick would repeat every open question")
	}
	f = newSweepFixture(t, ours("appr_1"))
	f.deps.Threads = nil
	if err := SweepApprovals(context.Background(), f.deps); err == nil {
		t.Fatal("the sweep ran with no thread lookup — it cannot know where any question belongs")
	}
	if len(f.slack.posted) != 0 {
		t.Fatalf("%d message(s) were posted by a sweep that should have refused", len(f.slack.posted))
	}
}
