//go:build component

package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// The Slack RETURN LEG against REAL PostgreSQL and the REAL coordinator: a signed mention births a run
// through the shipped route, the run reaches a terminal state through the SAME ApplyRunTransition every
// production path uses, and the answer arrives in the thread the question was asked in — once.
//
// Nothing on our side is stubbed: real router, real admission, real terminal transaction, real
// slack_reply_deliveries row, real production pump. The only fake is Slack itself (the fixture's local
// stand-in for slack.com/api), and the honest ceiling is E19 T1's, unchanged: this is evidence about our
// code against the published contract, never about a real workspace.

// terminate drives a run to a terminal state through the production coordinator — the choke point whose
// terminal branch enqueues the reply. It deliberately does NOT use the fixture's terminateRuns shortcut
// (a raw UPDATE), because a raw UPDATE never runs the transaction this whole feature hangs off.
func (f *slackFixture) terminate(t *testing.T, runID string, commands ...statemachines.RunCommand) {
	t.Helper()
	tenant := coordinator.Tenant{Project: f.project}
	for _, cmd := range commands {
		if _, err := f.spine.ApplyRunTransition(context.Background(), tenant, runID, cmd); err != nil {
			t.Fatalf("apply %s to run %s: %v", cmd, runID, err)
		}
	}
}

// runAndResponse reads the single run the fixture's tenant holds.
func (f *slackFixture) runAndResponse(t *testing.T) (runID, responseID, sessionID string) {
	t.Helper()
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT id, response_id, session_id FROM runs WHERE organization_id=$1 AND project_id=$2`,
		f.org, f.project).Scan(&runID, &responseID, &sessionID); err != nil {
		t.Fatalf("read the born run: %v", err)
	}
	return runID, responseID, sessionID
}

// finalizeWith writes the terminal response projection the poster renders from.
func (f *slackFixture) finalizeWith(t *testing.T, responseID, state string, projection map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(projection)
	if err := f.spine.FinalizeResponse(context.Background(),
		coordinator.Tenant{Project: f.project}, responseID, state, raw); err != nil {
		t.Fatalf("finalize response: %v", err)
	}
}

// postCalls returns only the chat.postMessage calls the fake Slack saw, so a fixture that also repaired a
// message (chat.update) does not read as a reply.
func (f *slackFixture) postCalls() []slackCall {
	var out []slackCall
	for _, c := range f.slackCalls() {
		if c.path == "/chat.postMessage" {
			out = append(out, c)
		}
	}
	return out
}

func (f *slackFixture) replyState(t *testing.T, runID string) (state string, attempts int) {
	t.Helper()
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state, attempt_count FROM slack_reply_deliveries WHERE run_id=$1`, runID).Scan(&state, &attempts); err != nil {
		t.Fatalf("read the reply delivery for run %s: %v", runID, err)
	}
	return state, attempts
}

// TestSlackTerminalRunAnswersInItsThreadExactlyOnce is the whole return leg. It also pins EXACTLY-ONCE the
// hard way: the terminal transaction is driven a SECOND time (what a reconciled cancel or a re-driven
// transition looks like) and the pump is ticked repeatedly — the human still sees one message.
func TestSlackTerminalRunAnswersInItsThreadExactlyOnce(t *testing.T) {
	f := newSlackFixture(t)
	ctx := context.Background()

	f.deliver(t, f.eventText(t, "EvR1", "app_mention", "Umapped", "C90", "1700000090.000100", "", "<@"+f.botUser+"> merhaba"),
		time.Now(), "", "").Body.Close()
	runID, responseID, _ := f.runAndResponse(t)

	// The run finishes the way a real one does: the projection is written and the transition is applied
	// through the production coordinator.
	f.finalizeWith(t, responseID, "completed", map[string]any{
		"output": []any{map[string]any{"type": "message", "content": "Merhaba! Size nasıl yardımcı olabilirim?"}},
	})
	f.terminate(t, runID, statemachines.RunCmdProvision, statemachines.RunCmdStart, statemachines.RunCmdComplete)

	// The order to post committed INSIDE that transaction — before any pump ran, and independent of one.
	if state, _ := f.replyState(t, runID); state != "pending" {
		t.Fatalf("delivery state after the terminal transaction = %q, want pending — the order must commit WITH the run's terminality", state)
	}

	pump := extensions.NewSlackReplyPump(f.bridge)
	posted, err := pump.Tick(ctx)
	if err != nil {
		t.Fatalf("pump tick: %v", err)
	}
	if posted != 1 {
		t.Fatalf("the pump posted %d replies, want 1", posted)
	}

	calls := f.postCalls()
	if len(calls) != 1 {
		t.Fatalf("fake Slack saw %d chat.postMessage call(s), want exactly 1", len(calls))
	}
	var body struct {
		Channel  string `json:"channel"`
		ThreadTS string `json:"thread_ts"`
		Text     string `json:"text"`
	}
	if err := json.Unmarshal([]byte(calls[0].body), &body); err != nil {
		t.Fatalf("decode the posted body: %v", err)
	}
	if body.Channel != "C90" || body.ThreadTS != "1700000090.000100" {
		t.Fatalf("posted to channel %q thread %q, want C90/1700000090.000100 — the answer belongs in the thread the question was asked in",
			body.Channel, body.ThreadTS)
	}
	if body.Text != "Merhaba! Size nasıl yardımcı olabilirim?" {
		t.Fatalf("posted text = %q, want the run's own output", body.Text)
	}
	// The bot token was redeemed from bot_token_ref and rode the Authorization header — never argv, never a log.
	if calls[0].auth != "Bearer "+string(f.botToken) {
		t.Fatalf("posted with auth %q, want the redeemed bot token", calls[0].auth)
	}

	// EXACTLY ONCE, three ways.
	if state, _ := f.replyState(t, runID); state != "delivered" {
		t.Fatalf("delivery state after the post = %q, want delivered", state)
	}
	for i := 0; i < 3; i++ {
		if posted, err := pump.Tick(ctx); err != nil || posted != 0 {
			t.Fatalf("tick %d after delivery posted %d (err %v), want 0 — a delivered reply is never re-posted", i, posted, err)
		}
	}
	// A re-driven terminal transition (a reconciled cancel arriving on an already-terminal run) must not
	// enqueue a second order. ErrRunTerminal is the expected refusal; what matters is the row count.
	_, _ = f.spine.ApplyRunTransition(ctx, coordinator.Tenant{Project: f.project},
		runID, statemachines.RunCmdCancel)
	if _, err := pump.Tick(ctx); err != nil {
		t.Fatalf("tick after a re-driven terminal: %v", err)
	}
	var deliveries int
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM slack_reply_deliveries WHERE run_id=$1`, runID).Scan(&deliveries); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if deliveries != 1 {
		t.Fatalf("%d delivery rows for one run, want 1 — UNIQUE(run_id) is the exactly-once claim", deliveries)
	}
	if n := len(f.postCalls()); n != 1 {
		t.Fatalf("fake Slack saw %d posts in total, want exactly 1", n)
	}
}

// SILENCE ON FAILURE IS ITS OWN BUG: a human who asked a question in a thread and gets nothing cannot tell a
// failed run from a broken integration — which is exactly the state the return leg exists to end. Every
// terminal state answers, and a failed one says so without pasting an internal error surface into a channel.
func TestSlackFailedRunStillAnswersInTheThread(t *testing.T) {
	f := newSlackFixture(t)

	f.deliver(t, f.eventText(t, "EvR2", "app_mention", "Umapped", "C91", "1700000091.000100", "", "<@"+f.botUser+"> build it"),
		time.Now(), "", "").Body.Close()
	runID, responseID, _ := f.runAndResponse(t)

	f.finalizeWith(t, responseID, "failed", map[string]any{
		"output": []any{},
		"error":  map[string]any{"code": "engine_error", "title": "The run could not be completed.", "status": 500},
	})
	f.terminate(t, runID, statemachines.RunCmdFail)

	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(context.Background()); err != nil || posted != 1 {
		t.Fatalf("failed run posted %d replies (err %v), want 1 — silence leaves the asker staring at nothing", posted, err)
	}
	calls := f.postCalls()
	if len(calls) != 1 {
		t.Fatalf("fake Slack saw %d posts, want 1", len(calls))
	}
	if !strings.Contains(calls[0].body, "The run failed") {
		t.Fatalf("the failure reply said %q, want a plain statement that the run failed", calls[0].body)
	}
	if strings.Contains(calls[0].body, "engine_error") {
		t.Fatalf("the failure reply leaked an internal error code into a workspace channel: %q", calls[0].body)
	}
}

// SLK-006 over the return leg: a Slack that refuses the message must not be able to touch the run. The
// canonical result stands, and the delivery retries on its own schedule rather than corrupting anything.
func TestSlackReplyFailureNeverCorruptsTheRun(t *testing.T) {
	f := newSlackFixture(t)
	ctx := context.Background()

	f.deliver(t, f.eventText(t, "EvR3", "app_mention", "Umapped", "C92", "1700000092.000100", "", "<@"+f.botUser+"> hello"),
		time.Now(), "", "").Body.Close()
	runID, responseID, _ := f.runAndResponse(t)
	f.finalizeWith(t, responseID, "completed", map[string]any{
		"output": []any{map[string]any{"type": "message", "content": "the answer"}},
	})
	f.terminate(t, runID, statemachines.RunCmdProvision, statemachines.RunCmdStart, statemachines.RunCmdComplete)

	// Slack is permanently rate limited: PostMessage spends its bounded repair budget and gives up.
	f.slackStatuses(429)
	f.slackRetryAfter("0")
	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(ctx); err != nil || posted != 0 {
		t.Fatalf("a refused post reported %d posted (err %v), want 0 and no error — a delivery failure is not a pump failure", posted, err)
	}

	// The run and its answer are exactly as they were.
	var state, projection string
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT r.state, resp.output::text FROM runs r JOIN responses resp ON resp.id = r.response_id WHERE r.id=$1`,
		runID).Scan(&state, &projection); err != nil {
		t.Fatalf("read the run after a failed delivery: %v", err)
	}
	if state != "completed" || !strings.Contains(projection, "the answer") {
		t.Fatalf("run = %q with projection %q; a Slack failure must never erase the canonical result", state, projection)
	}
	// The delivery is still pending with its attempt counted — it retries later, it is not lost and not dead.
	if s, attempts := f.replyState(t, runID); s != "pending" || attempts != 1 {
		t.Fatalf("delivery = %q after %d attempt(s), want pending after 1", s, attempts)
	}
}

// A DISABLED connection stops writing to the workspace, including for runs already in flight. Turning an
// integration off is what containing an incident looks like, and it has to take the pending replies with it.
func TestSlackDisabledConnectionPostsNothing(t *testing.T) {
	f := newSlackFixture(t)

	exec(t, f.pool, `UPDATE slack_connections SET disabled = true WHERE organization_id=$1 AND project_id=$2`, f.org, f.project)
	// The event path refuses a disabled connection outright, so the run is created directly against the
	// correlated thread instead — the point under test is the POSTER, not the receiver.
	sessionID := newID("ses")
	exec(t, f.pool, `INSERT INTO sessions (id, organization_id, project_id, state) VALUES ($1,$2,$3,'active')`,
		sessionID, f.org, f.project)
	var connID string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT id FROM slack_connections WHERE organization_id=$1 AND project_id=$2`, f.org, f.project).Scan(&connID); err != nil {
		t.Fatalf("read the connection: %v", err)
	}
	exec(t, f.pool, `INSERT INTO slack_thread_sessions (id, organization_id, project_id, connection_id, team_id, channel_id, thread_ts, session_id)
	                 VALUES ($1,$2,$3,$4,$5,'C93','1700000093.000100',$6)`,
		newID("sthr"), f.org, f.project, connID, f.team, sessionID)

	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(context.Background()); err != nil || posted != 0 {
		t.Fatalf("a disabled connection posted %d replies (err %v), want 0", posted, err)
	}
	if n := len(f.postCalls()); n != 0 {
		t.Fatalf("fake Slack saw %d posts from a disabled connection, want 0", n)
	}
}
