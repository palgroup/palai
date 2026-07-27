//go:build component

package store_test

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"

	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/storage"
)

// E20 T1 — the RUN FOLLOWER against REAL PostgreSQL, the REAL admission route and the REAL coordinator. A
// signed mention births a run through the shipped route; the follower tails the run's own journal through the
// SAME api.EventReader the SSE endpoint reads; the thread shows a status and then a message; the terminal
// transaction's reply CLOSES that message rather than posting beside it.
//
// The only fake is Slack, and its streaming methods answer with the envelopes the PUBLISHED references
// document (slackOKEnvelope, with the four URLs and the date they were checked).
//
// HONEST CEILING, and it is why several tests below have "fake engine" in their name: a REAL run is
// single-step (E08 exposes no tools to a real provider) and the journal carries no token deltas, so a real
// run has exactly ONE thing to stream. The multi-append behaviour proven here is driven by journal events
// this fixture writes directly — the shape a fake engine produces. Nothing here is evidence that a real model
// streams anything.

// withStreaming mounts the run follower on the fixture's PRODUCTION bridge. It is not in newSlackFixture on
// purpose: the tests that predate streaming assert call counts against a Slack that saw only their own
// traffic, and a follower would add status calls to all of them.
func (f *slackFixture) withStreaming(t *testing.T, maxConcurrent int) {
	t.Helper()
	// log.Printf, not t.Logf: the supervisor outlives the test function, and logging into a finished test
	// panics. It only ever logs a panic — the follower never returns an error to be restarted on.
	f.bridge.WithStreaming(f.repo, coordinator.NewSupervisor(log.Printf, time.Second), maxConcurrent)
}

func (f *slackFixture) tenant() coordinator.Tenant {
	return coordinator.Tenant{Organization: f.org, Project: f.project}
}

// commitStep journals a model_step.completed.v1 through the PRODUCTION commit — the same call the real
// dispatch makes, carrying the same payload ({run_id, model_request_id}), which is precisely why the
// follower can report that a step landed and cannot report what it said.
func (f *slackFixture) commitStep(t *testing.T, sessionID, responseID, runID string) {
	t.Helper()
	if _, err := f.spine.CommitModelResult(context.Background(), f.tenant(), sessionID, responseID, runID,
		newID("mr"), []byte(`{"output":"ok"}`), "model_step.completed.v1",
		[]byte(`{"run_id":"`+runID+`","model_request_id":"mr_x"}`), contracts.Usage{}); err != nil {
		t.Fatalf("commit model result: %v", err)
	}
}

// upsertTask journals a task.created/updated.v1 through the PRODUCTION registry. Task events are the only
// journal events carrying human-readable text of their own, so they are what a multi-step run actually
// streams.
func (f *slackFixture) upsertTask(t *testing.T, sessionID, responseID, runID, key, title, status string) {
	t.Helper()
	if _, err := f.spine.UpsertTask(context.Background(), f.tenant(), coordinator.TaskUpsert{
		SessionID: sessionID, RunID: runID, ResponseID: responseID, Key: key, NewRowID: newID("task"),
		Title: title, SetTitle: true, Status: status, SetStatus: true,
	}); err != nil {
		t.Fatalf("upsert task %q: %v", key, err)
	}
}

func (f *slackFixture) runState(t *testing.T, runID string) string {
	t.Helper()
	var state string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id=$1`, runID).Scan(&state); err != nil {
		t.Fatalf("read run %s state: %v", runID, err)
	}
	return state
}

func decodeSlackCall(t *testing.T, c slackCall) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(c.body), &got); err != nil {
		t.Fatalf("decode %s body %q: %v", c.path, c.body, err)
	}
	return got
}

// THE WHOLE OF T1 IN ONE RUN: status while it works, a message that appears when the first step lands, ordered
// appends as the (fake-engine) run makes progress, and a terminal that CLOSES that message with the answer.
//
// The claim it pins hardest is the one streaming could most easily have broken: ONE run still produces ONE
// visible message. Before this task the reply pump posted it; now the pump closes the message the follower
// opened, and chat.postMessage is never called at all.
func TestSlackStreamShowsTheRunWorkingThenClosesWithTheAnswer(t *testing.T) {
	f := newSlackFixture(t)
	f.withStreaming(t, 4)

	f.deliver(t, f.eventText(t, "EvS1", "app_mention", "Umapped", "C80", "1700000080.000100", "", "<@"+f.botUser+"> ship it"),
		time.Now(), "", "").Body.Close()
	runID, responseID, sessionID := f.runAndResponse(t)

	// 1. THE STATUS. This is the part that needs no new scope and no panel (chat:write), so it is the first
	//    thing a real workspace sees — and for a single-step run it is most of the win.
	status := decodeSlackCall(t, f.awaitCalls(t, "/assistant.threads.setStatus", 1)[0])
	if status["channel_id"] != "C80" || status["thread_ts"] != "1700000080.000100" || status["status"] == "" {
		t.Fatalf("the working status was set as %v, want a non-empty status on the asking thread", status)
	}

	f.terminate(t, runID, statemachines.RunCmdProvision, statemachines.RunCmdStart)

	// 2. THE FIRST STEP OPENS THE MESSAGE. It lands in the thread now — not after the terminal transaction.
	f.commitStep(t, sessionID, responseID, runID)
	start := decodeSlackCall(t, f.awaitCalls(t, "/chat.startStream", 1)[0])
	// S9: both recipient ids are required when streaming to a channel, and both come off the event.
	if start["recipient_user_id"] != "Umapped" || start["recipient_team_id"] != f.team {
		t.Fatalf("startStream body = %v, want the recipient ids the event carried", start)
	}
	if start["channel"] != "C80" || start["thread_ts"] != "1700000080.000100" {
		t.Fatalf("startStream opened on %v/%v, want the thread the question was asked in", start["channel"], start["thread_ts"])
	}
	if _, forbidden := start["blocks"]; forbidden {
		t.Fatalf("startStream carried blocks; the conservative reading of S12 puts them on stopStream only: %v", start)
	}

	// 3. PROGRESS APPENDS — FAKE-ENGINE-DRIVEN. A real run never gets here: it is single-step, so its stream
	//    has exactly the opener above. These are task events, the only journal events carrying readable text.
	f.upsertTask(t, sessionID, responseID, runID, "t1", "Write the migration", "in_progress")
	f.upsertTask(t, sessionID, responseID, runID, "t1", "Write the migration", "complete")
	appends := f.awaitCalls(t, "/chat.appendStream", 2)
	streamTS, _ := decodeSlackCall(t, f.awaitCalls(t, "/chat.startStream", 1)[0])["ts"].(string)
	_ = streamTS
	first, second := decodeSlackCall(t, appends[0]), decodeSlackCall(t, appends[1])
	if !strings.Contains(first["markdown_text"].(string), "in_progress") ||
		!strings.Contains(second["markdown_text"].(string), "complete") {
		t.Fatalf("appends arrived out of order or lost their text: %q then %q", appends[0].body, appends[1].body)
	}
	openedTS := start["ts"]
	if openedTS == nil {
		// startStream's REQUEST has no ts; the ts comes back in the response. Read it off the append instead,
		// which is where the client had to put it.
		openedTS = first["ts"]
	}
	if first["ts"] != second["ts"] {
		t.Fatalf("two appends addressed different messages (%v, %v); one run opens ONE stream", first["ts"], second["ts"])
	}

	// 4. THE TERMINAL CLOSES THE MESSAGE IT OPENED.
	f.finalizeWith(t, responseID, "completed", map[string]any{
		"output": []any{map[string]any{"type": "message", "content": "Shipped. The migration is applied."}},
	})
	f.terminate(t, runID, statemachines.RunCmdComplete)

	// The follower exits on the terminal event and clears the status on its way out — a thread left saying
	// "is thinking…" after the answer landed is a claim nobody can check.
	cleared := decodeSlackCall(t, f.awaitCalls(t, "/assistant.threads.setStatus", 2)[1])
	if cleared["status"] != "" {
		t.Fatalf("the working status was left as %v after the run ended", cleared["status"])
	}

	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(context.Background()); err != nil || posted != 1 {
		t.Fatalf("the pump delivered %d answers (err %v), want 1", posted, err)
	}
	stops := f.callsTo("/chat.stopStream")
	if len(stops) != 1 {
		t.Fatalf("fake Slack saw %d chat.stopStream call(s), want exactly 1", len(stops))
	}
	stop := decodeSlackCall(t, stops[0])
	if stop["markdown_text"] != "Shipped. The migration is applied." {
		t.Fatalf("the stream closed with %q, want the run's own answer", stop["markdown_text"])
	}
	if stop["ts"] != first["ts"] {
		t.Fatalf("the answer closed message %v but the stream was %v — it must close the message it opened", stop["ts"], first["ts"])
	}

	// ONE RUN, ONE VISIBLE MESSAGE. The pump did not post beside the stream.
	if n := len(f.postCalls()); n != 0 {
		t.Fatalf("fake Slack saw %d chat.postMessage call(s) for a streamed run, want 0 — a second message is a second answer", n)
	}
	if state, _ := f.replyState(t, runID); state != "delivered" {
		t.Fatalf("delivery state = %q after closing the stream, want delivered", state)
	}
}

// EXACTLY ONCE, and note WHERE it comes from: the follower does not de-duplicate anything. Admit starts it
// only when the idempotency reservation reported Replayed == false, so Slack's redelivery — three attempts of
// the same event_id — reaches it zero times.
func TestSlackStreamRedeliveryOpensNoSecondStream(t *testing.T) {
	f := newSlackFixture(t)
	f.withStreaming(t, 4)

	body := f.eventText(t, "EvS2", "app_mention", "Umapped", "C81", "1700000081.000100", "", "<@"+f.botUser+"> hello")
	f.deliver(t, body, time.Now(), "", "").Body.Close()
	runID, responseID, sessionID := f.runAndResponse(t)
	f.terminate(t, runID, statemachines.RunCmdProvision, statemachines.RunCmdStart)
	f.commitStep(t, sessionID, responseID, runID)
	f.awaitCalls(t, "/chat.startStream", 1)

	// The same event, redelivered the way the Events API documents it (attempt 2 and 3).
	f.deliver(t, body, time.Now(), "2", "http_timeout").Body.Close()
	f.deliver(t, body, time.Now(), "3", "http_timeout").Body.Close()
	// Give a second follower every chance to exist before asserting it does not.
	time.Sleep(2 * time.Second)

	if n := f.runCount(t); n != 1 {
		t.Fatalf("the retry storm birthed %d runs, want 1", n)
	}
	if n := len(f.callsTo("/chat.startStream")); n != 1 {
		t.Fatalf("fake Slack saw %d chat.startStream call(s) after two redeliveries, want exactly 1", n)
	}
}

// OVER THE CAP the stream is SKIPPED — never dropped, never corrupted, just not decorated. A workspace under
// load must not lose answers because a Tier-2 method (startStream, 20+/min) is the scarce one.
func TestSlackStreamOverTheConcurrencyCapStillAnswers(t *testing.T) {
	f := newSlackFixture(t)
	f.withStreaming(t, 1) // one slot, and the first run takes it

	f.deliver(t, f.eventText(t, "EvS3a", "app_mention", "Umapped", "C82", "1700000082.000100", "", "<@"+f.botUser+"> first"),
		time.Now(), "", "").Body.Close()
	// The first follower is holding the only slot: it has set a status and is tailing a run that has not
	// finished. (follow() takes the semaphore synchronously, before the route answers.)
	f.awaitCalls(t, "/assistant.threads.setStatus", 1)

	f.deliver(t, f.eventText(t, "EvS3b", "app_mention", "Umapped", "C83", "1700000083.000100", "", "<@"+f.botUser+"> second"),
		time.Now(), "", "").Body.Close()
	var second string
	rows, err := f.pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT id, response_id, session_id FROM runs WHERE organization_id=$1 AND project_id=$2 ORDER BY created_at`, f.org, f.project)
	if err != nil {
		t.Fatalf("read runs: %v", err)
	}
	var responses, sessions []string
	var ids []string
	for rows.Next() {
		var id, resp, sess string
		if err := rows.Scan(&id, &resp, &sess); err != nil {
			t.Fatalf("scan run: %v", err)
		}
		ids, responses, sessions = append(ids, id), append(responses, resp), append(sessions, sess)
	}
	rows.Close()
	if len(ids) != 2 {
		t.Fatalf("%d runs were born, want 2", len(ids))
	}
	second = ids[1]

	// The second run produces output and finishes. No stream was opened for it, so its answer posts plainly.
	f.terminate(t, second, statemachines.RunCmdProvision, statemachines.RunCmdStart)
	f.commitStep(t, sessions[1], responses[1], second)
	f.finalizeWith(t, responses[1], "completed", map[string]any{
		"output": []any{map[string]any{"type": "message", "content": "the second answer"}},
	})
	f.terminate(t, second, statemachines.RunCmdComplete)

	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(context.Background()); err != nil || posted != 1 {
		t.Fatalf("the capped-out run delivered %d answers (err %v), want 1 — the cap must never lose an answer", posted, err)
	}
	posts := f.postCalls()
	if len(posts) != 1 || !strings.Contains(posts[0].body, "the second answer") {
		t.Fatalf("the undecorated run's answer posted as %v, want one plain chat.postMessage carrying it", posts)
	}
	if n := len(f.callsTo("/chat.startStream")); n != 0 {
		t.Fatalf("fake Slack saw %d chat.startStream call(s) with the cap full, want 0", n)
	}
}

// S11 — the human presses stop in Slack's UI. `stopped_by_user` carries no authenticated actor, so it stops
// the STREAM and nothing else: the run keeps going, the thread is told so plainly, and the answer arrives as
// an ordinary message when it is ready.
//
// The guarantee this pins is a NEGATIVE one: no run-control path exists here at all. Its structural half is
// TestSlackStreamNeverControlsTheRun in the extensions package, which fails if the follower ever so much as
// names AcceptCommand.
func TestSlackStreamStoppedByUserLeavesTheRunRunning(t *testing.T) {
	f := newSlackFixture(t)
	f.withStreaming(t, 4)
	f.slackRefuse("/chat.appendStream", "stopped_by_user")

	f.deliver(t, f.eventText(t, "EvS4", "app_mention", "Umapped", "C84", "1700000084.000100", "", "<@"+f.botUser+"> long job"),
		time.Now(), "", "").Body.Close()
	runID, responseID, sessionID := f.runAndResponse(t)
	f.terminate(t, runID, statemachines.RunCmdProvision, statemachines.RunCmdStart)

	f.commitStep(t, sessionID, responseID, runID)
	f.awaitCalls(t, "/chat.startStream", 1)
	f.upsertTask(t, sessionID, responseID, runID, "t1", "Building", "in_progress")

	// The thread is told the truth: the live view stopped, the work did not.
	notice := f.awaitCalls(t, "/chat.postMessage", 1)[0]
	if !strings.Contains(notice.body, "still going") {
		t.Fatalf("the stop notice said %q, want a plain statement that the run continues", notice.body)
	}

	// THE RUN IS UNTOUCHED. Nothing about a Slack UI affordance is run control.
	if state := f.runState(t, runID); state != string(statemachines.RunRunning) {
		t.Fatalf("run state after the user stopped the stream = %q, want it still running", state)
	}
	// And no further appends are attempted against a stream Slack will refuse forever.
	f.upsertTask(t, sessionID, responseID, runID, "t2", "Still building", "in_progress")
	time.Sleep(2 * time.Second)
	if n := len(f.callsTo("/chat.appendStream")); n != 1 {
		t.Fatalf("fake Slack saw %d append(s) after stopped_by_user, want exactly the one that was refused", n)
	}

	// The answer still arrives — as a message, because the stream is gone.
	f.finalizeWith(t, responseID, "completed", map[string]any{
		"output": []any{map[string]any{"type": "message", "content": "done anyway"}},
	})
	f.terminate(t, runID, statemachines.RunCmdComplete)
	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(context.Background()); err != nil || posted != 1 {
		t.Fatalf("the answer of a run whose stream was stopped delivered %d times (err %v), want 1", posted, err)
	}
	if n := len(f.callsTo("/chat.stopStream")); n != 0 {
		t.Fatalf("fake Slack saw %d stopStream call(s) on a user-stopped stream, want 0 — it is refused forever", n)
	}
	posts := f.postCalls()
	if len(posts) != 2 || !strings.Contains(posts[1].body, "done anyway") {
		t.Fatalf("the answer posted as %v, want the stop notice then the answer", posts)
	}
}

// A run that reaches a terminal state having produced NO output opens no stream at all — one honest message,
// not an empty streaming message that fills in later.
func TestSlackRunWithNoOutputOpensNoStream(t *testing.T) {
	f := newSlackFixture(t)
	f.withStreaming(t, 4)

	f.deliver(t, f.eventText(t, "EvS5", "app_mention", "Umapped", "C85", "1700000085.000100", "", "<@"+f.botUser+"> nope"),
		time.Now(), "", "").Body.Close()
	runID, responseID, _ := f.runAndResponse(t)
	f.awaitCalls(t, "/assistant.threads.setStatus", 1)

	f.finalizeWith(t, responseID, "failed", map[string]any{"output": []any{},
		"error": map[string]any{"code": "engine_error", "title": "The run could not be completed.", "status": 500}})
	f.terminate(t, runID, statemachines.RunCmdFail)
	f.awaitCalls(t, "/assistant.threads.setStatus", 2) // the follower saw the terminal and stood down

	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(context.Background()); err != nil || posted != 1 {
		t.Fatalf("a failed run delivered %d answers (err %v), want 1 — silence on failure is its own bug", posted, err)
	}
	if n := len(f.callsTo("/chat.startStream")); n != 0 {
		t.Fatalf("a run with no output opened %d stream(s), want 0", n)
	}
	posts := f.postCalls()
	if len(posts) != 1 || !strings.Contains(posts[0].body, "The run failed") {
		t.Fatalf("the failure answer posted as %v, want one plain message saying the run failed", posts)
	}
}

// ONE RETRY OWNER (plan §2). A throttled append is repaired by slack.PostMessage's bounded Retry-After
// discipline — ONCE — and no second retry layer exists in the follower to repair it again.
func TestSlackStreamAppendRepairsA429WithoutASecondRetryLayer(t *testing.T) {
	f := newSlackFixture(t)
	f.withStreaming(t, 4)
	f.slackScript("/chat.appendStream", scriptedReply{status: http.StatusTooManyRequests})

	f.deliver(t, f.eventText(t, "EvS6", "app_mention", "Umapped", "C86", "1700000086.000100", "", "<@"+f.botUser+"> paced"),
		time.Now(), "", "").Body.Close()
	runID, responseID, sessionID := f.runAndResponse(t)
	f.terminate(t, runID, statemachines.RunCmdProvision, statemachines.RunCmdStart)
	f.commitStep(t, sessionID, responseID, runID)
	f.awaitCalls(t, "/chat.startStream", 1)

	f.upsertTask(t, sessionID, responseID, runID, "t1", "Paced work", "complete")
	f.awaitCalls(t, "/chat.appendStream", 2) // the refusal, then the repair
	time.Sleep(2 * time.Second)
	if n := len(f.callsTo("/chat.appendStream")); n != 2 {
		t.Fatalf("one line cost %d append call(s), want exactly 2 (one 429 + one bounded repair, no second layer)", n)
	}
}
