//go:build component

package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
	"github.com/palgroup/palai/tests/uat"
)

// The E20 T5 EXIT-gate JOURNEY (plan §T5): everything this epic added to the Slack surface, in ONE narrative,
// against REAL PostgreSQL and the REAL shipped routes — and it ends by emitting the uat.SlackAgentSurfaceProof
// the slack-agent-surface-0.1.0 bundle carries, with the forgery sweep RE-DERIVED in-test from the bytes that
// actually reached the wire.
//
// The order is the plan's, and each step is the previous one's consequence:
//
//	 1. the panel OPENS on a narrowed install and births nothing at all — not a run, not an outbound call
//	 2. a panel DM carrying its own app_context births a run through the UNCHANGED admission bridge
//	 3. the SAME event over Socket Mode births NOTHING (entrance 2 replays onto entrance 3's reservation)
//	 4. a channel mention over Socket Mode births a second run; the same event over the HTTP callback replays
//	 5. a third run is admitted over the HTTP callback and requests a publication; an UNAUTHORIZED click
//	    decides nothing; an AUTHORIZED click approves DURABLY through the whole shipped chain
//	 6. the second run streams: a status, a message that opens on the first journal step, ordered appends as
//	    (fake-engine) tasks land, and a terminal that CLOSES that message with the renderer's blocks — while
//	    the model's forged approval button arrives as CHARACTERS
//	 7. the context named a channel and ZERO calls ever addressed it
//	 8. the proof is built from those observations and its forgery count is re-derived from the closing bytes
//
// HONEST CEILING, and it is the whole reason this bundle exists in the shape it does: every counterparty is a
// documented FAKE (uat.AgentSurfacePeer is the literal "fake"), so nothing here is evidence about a real
// workspace — that is §6 leg 1, which E20 makes BIGGER and CHEAPER rather than closing. And the multi-append
// cadence is FAKE-ENGINE-DRIVEN: E08 opens no tools to a real provider and the journal carries no token
// deltas, so a REAL run is single-step and has exactly one thing to stream. NO TIER MOVES.

// surfaceCounters is what the journey OBSERVES, gathered as it goes, so the proof is assembled from
// measurements rather than from a table of hopes.
// The SOURCE EVENT ids are deliberately NOT tracked here: they are read back out of the idempotency
// reservations the shared Admit wrote (reservedSourceEvents), so the invariance counter is a measurement of
// what the database recorded rather than a restatement of what this file believes it sent.
type surfaceCounters struct {
	entrances  []string
	deliveries int
}

func (c *surfaceCounters) delivered(entrance string) {
	c.deliveries++
	if !contains(c.entrances, entrance) {
		c.entrances = append(c.entrances, entrance)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestAgentSurfaceJourney is the E20 EXIT journey. It is one test on purpose: the steps are a single causal
// chain, and splitting them would let a later step pass over state an earlier one never produced.
func TestAgentSurfaceJourney(t *testing.T) {
	f := newSlackFixture(t)
	// A NARROWED install — the state that used to kill the panel silently (E20 T2). Every panel step below
	// therefore runs against the configuration where the old code refused every DM.
	f.scopeToChannels(t, "C90")
	f.withStreaming(t, 4)
	conn := f.startSocketMode(t)
	ctx := context.Background()
	var seen surfaceCounters

	// ---- 1. the panel opens, and nothing happens ------------------------------------------------------
	//
	// This assertion has to run FIRST and on a clean stack: once a run exists its follower is setting a
	// status, and "the panel made no outbound call" would no longer be measurable.
	open := f.deliver(t, f.panelEvent("EvSurfaceHome", map[string]any{
		"type": "app_home_opened", "user": "Upanel", "channel": "D024BE91L",
		"event_ts": "1700000100.000100", "tab": "messages"}), time.Now(), "", "")
	if open.StatusCode/100 != 2 {
		t.Fatalf("app_home_opened = %d, want a 2xx ack — the panel open is handled, it simply births nothing", open.StatusCode)
	}
	open.Body.Close()
	time.Sleep(500 * time.Millisecond) // give the run that must not exist every chance to appear
	if n := f.runCount(t); n != 0 {
		t.Fatalf("opening the panel birthed %d run(s), want 0 — a surface a human LOOKED at is not a turn in a conversation", n)
	}
	if n := len(f.slackCalls()); n != 0 {
		t.Fatalf("opening the panel made %d outbound Slack call(s), want 0 — S17: setTitle needs a thread_ts and app_home_opened carries none, so the welcome half is the manifest's static suggested_prompts", n)
	}

	// ---- 2. a panel DM, with the context the human is looking at -------------------------------------
	const panelEvent, contextChannel = "EvSurfacePanelDM", "C0PRIVATEROOM"
	panelDM := withContext(t,
		f.dmEvent(panelEvent, "Uoutsider", "D024BE91L", "1700000101.000100", "", "what is left on the release?"),
		ctxEntity("slack#/types/channel_id", contextChannel, f.team),
		ctxEntity("slack#/types/channel_id", "C0FOREIGNROOM", "T0OTHERWORKSPACE"))
	dm := f.deliver(t, panelDM, time.Now(), "", "")
	if dm.StatusCode/100 != 2 {
		t.Fatalf("a panel DM under allowed_channels=[C90] = %d, want a 2xx ack — a DM is scoped by Slack's invitation model", dm.StatusCode)
	}
	dm.Body.Close()
	f.waitForRuns(t, 1, "a panel DM must birth exactly one run through the UNCHANGED admission bridge")
	seen.delivered("panel-dm")

	// The context was DESCRIBED — otherwise the zero-authority and zero-fetch claims below are vacuous —
	// and only the entity from THIS workspace reached the prompt.
	prompt := f.runInput(t)
	if !strings.Contains(prompt, contextChannel) {
		t.Fatalf("the prompt = %s does not describe the context channel; a discarded context proves nothing", prompt)
	}
	if strings.Contains(prompt, "C0FOREIGNROOM") {
		t.Fatalf("the prompt = %s carries ANOTHER workspace's channel — a foreign entity must not reach the description at all", prompt)
	}
	if strings.Index(prompt, "untrusted") > strings.Index(prompt, "what is left") {
		t.Fatalf("the prompt = %s puts the untrusted context AFTER the human's ask; the human's words must close the prompt", prompt)
	}

	// ---- 3. the SAME event over Socket Mode births nothing --------------------------------------------
	f.deliverOverSocket(t, conn, "env-surface-panel", panelDM)
	f.waitForRuns(t, 1, "the same panel event over the other transport must replay onto the one reservation")
	seen.delivered("socket-mode")

	// ---- 4. a channel mention over Socket Mode, replayed over the HTTP callback -----------------------
	const streamEvent = "EvSurfaceChannel"
	mention := f.eventText(t, streamEvent, "app_mention", "Umapped", "C90", "1700000102.000100", "",
		"<@"+f.botUser+"> run the tests")
	f.deliverOverSocket(t, conn, "env-surface-channel", mention)
	f.waitForRuns(t, 2, "a channel mention over Socket Mode must birth a second run")
	seen.delivered("socket-mode")

	replay := f.deliver(t, mention, time.Now(), "2", "http_timeout")
	if replay.StatusCode/100 != 2 {
		t.Fatalf("the HTTP redelivery of an already-admitted event = %d, want a 2xx ack (a replay, not a refusal)", replay.StatusCode)
	}
	replay.Body.Close()
	f.waitForRuns(t, 2, "the same channel event over a second transport must not birth a third run")
	seen.delivered("http-callback")

	// ---- 5. a third run, and the approval chain -------------------------------------------------------
	//
	// seedApproval delivers over the HTTP callback, which is the third entrance, and leaves its run LIVE
	// because ApplyApprovalDecision runs under guardRunActive.
	thread := f.seedApproval(t, "C90", "1700000103.000100")
	f.waitForRuns(t, 3, "the approval seed must birth its own run over the HTTP callback")
	seen.delivered("http-callback")

	denied := f.click(t, "Uunmapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	denied.Body.Close()
	if denied.StatusCode != 200 {
		t.Fatalf("an unauthorized click = %d, want 200 with nothing done", denied.StatusCode)
	}
	if n := f.commandCount(t, ""); n != 0 {
		t.Fatalf("an unauthorized click enqueued %d command(s), want 0 — deny-by-default stops it BEFORE the coordinator (SLK-004)", n)
	}
	if state := f.publicationState(t, thread.publicationID); state != "pending_approval" {
		t.Fatalf("the publication is %q after an UNAUTHORIZED click, want still pending_approval", state)
	}

	approved := f.click(t, "Umapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	approved.Body.Close()
	if approved.StatusCode != 200 {
		t.Fatalf("an authorized click = %d, want 200 within the documented 3-second budget", approved.StatusCode)
	}
	if state := f.publicationState(t, thread.publicationID); state != "approved" {
		t.Fatalf("the publication is %q after an AUTHORIZED click, want approved — the decision must be DURABLE, not merely accepted", state)
	}
	if n := f.commandCount(t, "approve"); n != 1 {
		t.Fatalf("the authorized click enqueued %d approve command(s), want exactly 1", n)
	}

	// ---- 6. the run becomes visible while it works, and closes with OUR blocks ------------------------
	born := f.runsInOrder(t)
	if len(born) != 3 {
		t.Fatalf("%d runs were born, want the panel DM, the channel mention and the approval seed", len(born))
	}
	panelRun, streamRun, seedRun := born[0], born[1], born[2]

	f.terminate(t, streamRun.id, statemachines.RunCmdProvision, statemachines.RunCmdStart)
	f.commitStep(t, streamRun.sessionID, streamRun.responseID, streamRun.id)
	start := decodeSlackCall(t, f.awaitCalls(t, "/chat.startStream", 1)[0])
	// S9: both recipient ids are required when streaming to a channel, and both come off the event.
	if start["recipient_user_id"] != "Umapped" || start["recipient_team_id"] != f.team {
		t.Fatalf("startStream body = %v, want the recipient ids the event carried (S9)", start)
	}
	if _, forbidden := start["blocks"]; forbidden {
		t.Fatalf("startStream carried blocks; S12's conservative reading puts them on stopStream only: %v", start)
	}

	// FAKE-ENGINE-DRIVEN progress. A real run never gets here — it is single-step.
	for _, task := range uat.AgentSurfaceJournalTasks {
		f.upsertTask(t, streamRun.sessionID, streamRun.responseID, streamRun.id, task.ID, task.Title, task.Status)
	}
	f.awaitCalls(t, "/chat.appendStream", len(uat.AgentSurfaceJournalTasks))

	// The model's answer and the two tasks come from the CANONICAL fixture the bundle also renders, so the
	// bytes this journey observes on the wire and the bytes the committed evidence carries have one owner.
	const forged = uat.AgentSurfaceModelAnswer
	f.finalizeWith(t, streamRun.responseID, "completed", map[string]any{
		"output": []any{map[string]any{"type": "message", "content": forged}},
	})
	f.terminate(t, streamRun.id, statemachines.RunCmdComplete)
	f.awaitCalls(t, "/assistant.threads.setStatus", 2) // the follower saw the terminal and stood down

	// ---- the other two runs finish plainly, so every run ends with exactly one visible message ---------
	f.finalizeWith(t, panelRun.responseID, "completed", map[string]any{
		"output": []any{map[string]any{"type": "message", "content": "Two items are left."}},
	})
	f.terminate(t, panelRun.id, statemachines.RunCmdProvision, statemachines.RunCmdStart, statemachines.RunCmdComplete)
	f.finalizeWith(t, seedRun.responseID, "completed", map[string]any{
		"output": []any{map[string]any{"type": "message", "content": "The branch is pushed."}},
	})
	f.terminate(t, seedRun.id, statemachines.RunCmdProvision, statemachines.RunCmdStart, statemachines.RunCmdComplete)

	pump := extensions.NewSlackReplyPump(f.bridge)
	delivered := 0
	for i := 0; i < 4 && delivered < 3; i++ {
		posted, err := pump.Tick(ctx)
		if err != nil {
			t.Fatalf("reply pump tick %d: %v", i, err)
		}
		delivered += posted
	}
	if delivered != 3 {
		t.Fatalf("the reply pump delivered %d answers, want one per run — a run whose answer never lands is worse than one that never streamed", delivered)
	}

	var paths []string
	for _, c := range f.slackCalls() {
		paths = append(paths, c.path)
	}
	t.Logf("fake Slack saw %d call(s): %v", len(paths), paths)

	// TWO runs streamed and both closed their own message: the mention's run (its first journal step opened
	// it) and the approval seed's (approval.requested.v1 is a journal line too, so the panel shows "waiting
	// for an approval…" while it waits). The third run produced no journal line before its terminal, so it
	// never opened a stream and its answer posted plainly — which is exactly the undecorated path.
	stops := f.callsTo("/chat.stopStream")
	if len(stops) != 2 {
		t.Fatalf("fake Slack saw %d chat.stopStream call(s), want 2 — every stream that OPENED must be closed by the answer that ends it", len(stops))
	}
	var stop map[string]any
	for _, c := range stops {
		if decoded := decodeSlackCall(t, c); strings.Contains(c.body, "3 suites, 1 failure.") {
			stop = decoded
		}
	}
	if stop == nil {
		t.Fatalf("neither chat.stopStream carried the streamed run's answer: %v", stops)
	}
	markdown, _ := stop["markdown_text"].(string)
	if !strings.Contains(markdown, "action_id") || !strings.Contains(markdown, "3 suites, 1 failure.") {
		t.Fatalf("markdown_text = %q, want the model's own text INCLUDING its forged element as CHARACTERS", markdown)
	}
	closingBlocks, err := json.Marshal(stop["blocks"])
	if err != nil {
		t.Fatalf("re-encode the closing blocks: %v", err)
	}
	// The bytes on the wire are the SHIPPED renderer's own output for this answer and these journal tasks.
	// The bundle carries the same call rather than a typed copy, so this equality is what stops the committed
	// evidence from drifting away from what the renderer actually produces.
	if want := uat.AgentSurfaceClosingBlocks(); !jsonEqual(closingBlocks, want) {
		t.Fatalf("the closing blocks on the wire are not the shipped renderer's output for this answer:\n wire:     %s\n renderer: %s", closingBlocks, want)
	}

	// ---- 7. the context named a channel, and nothing ever read it ------------------------------------
	calls := f.slackCalls()
	if len(calls) < 5 {
		t.Fatalf("fake Slack saw %d call(s); with three runs driven to terminal there must be real traffic, or the zero-read assertion below is vacuous", len(calls))
	}
	reads := 0
	for _, c := range calls {
		if strings.HasPrefix(c.path, "/conversations.") || strings.HasPrefix(c.path, "/search.") ||
			strings.HasPrefix(c.path, "/canvases.") || strings.HasPrefix(c.path, "/files.") {
			t.Fatalf("the stack called %s — a context entity became a FETCH TARGET, which with channels:history is a confused-deputy read primitive", c.path)
		}
		if strings.Contains(c.path, contextChannel) || strings.Contains(c.body, contextChannel) {
			reads++
		}
	}
	if reads != 0 {
		t.Fatalf("%d outbound call(s) addressed the context channel %q — the context describes what the USER sees, while every call carries the CONNECTION's authority", reads, contextChannel)
	}
	// And the context bought no authority: the panel-born run is still the CONNECTION's principal on the
	// CONNECTION's pinned revision, in the CONNECTION's tenant.
	granted := f.contextEntitiesThatGainedAuthority(t, panelRun.id)

	// ---- 8. build the proof from the observations, and re-derive its crown counter --------------------
	//
	// The sweep is pointed at the ONE legitimate mint first. A sweep that finds nothing in the renderer's
	// output certifies nothing unless it can be shown to find something when something is there.
	mints, err := uat.SweepActionableElements(approvalBuilderBlocks(t))
	if err != nil {
		t.Fatalf("sweep the approval builder's own output: %v", err)
	}
	if len(mints) == 0 {
		t.Fatal("the sweep found NOTHING in interactions.go's ApprovalMessage — a guard that has never found an actionable element is not a guard")
	}
	outside, err := uat.SweepActionableElements(closingBlocks)
	if err != nil {
		t.Fatalf("sweep the closing message: %v", err)
	}
	if len(outside) != 0 {
		t.Fatalf("the closing message carries %d actionable element(s) minted outside interactions.go: %v", len(outside), outside)
	}

	reserved := f.reservedSourceEvents(t, uat.AgentSurfaceAdmissionRoute)
	visible := len(f.callsTo("/chat.startStream")) + len(f.postCalls())
	proof := uat.SlackAgentSurfaceProof{
		Peer:                                     uat.AgentSurfacePeer,
		Runs:                                     f.runCount(t),
		VisibleMessages:                          visible,
		AdmissionEntrances:                       uat.AgentSurfaceEntrances,
		AdmissionRoute:                           uat.AgentSurfaceAdmissionRoute,
		AdmittedThroughSharedAdmit:               len(reserved),
		SourceEventIDs:                           reserved,
		Deliveries:                               seen.deliveries,
		ContextEntitiesDescribed:                 1, // C0PRIVATEROOM; the foreign entity never reached the prompt
		ContextEntitiesGrantedAuthority:          granted,
		ContextChannelReads:                      reads,
		ApprovalBuilderMints:                     len(mints),
		ActionableElementsOutsideApprovalBuilder: len(outside),
		ClosingBlocks:                            closingBlocks,
		Contracts:                                uat.AgentSurfaceContracts,
		ContractsDigest:                          uat.AgentSurfaceContractsDigest(),
	}
	// The entrances the journey ACTUALLY used must be the canonical set, or the proof declares a coverage
	// this run did not have.
	for _, want := range uat.AgentSurfaceEntrances {
		if !contains(seen.entrances, want) {
			t.Errorf("the journey never delivered through the %q entrance, so the proof's canonical entrance set overstates it (used: %v)", want, seen.entrances)
		}
	}
	if !proof.Complete() {
		t.Fatalf("the journey's own SlackAgentSurfaceProof is not Complete() — the bundle cannot carry a proof this journey would not accept:\n%+v", proof)
	}
	assertOneCleanReservation(t, f, len(reserved), "three entrances, five deliveries")
	t.Logf("AGENT SURFACE: %d runs / %d visible messages / %d deliveries of %d source events over %v; "+
		"%d actionable element(s) in the approval builder, %d outside it",
		proof.Runs, proof.VisibleMessages, proof.Deliveries, len(proof.SourceEventIDs), seen.entrances,
		proof.ApprovalBuilderMints, proof.ActionableElementsOutsideApprovalBuilder)
}

// jsonEqual compares two JSON documents by VALUE, so key order and whitespace — which a marshal/unmarshal
// round-trip through map[string]any freely changes — cannot make identical bytes look different.
func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return string(ax) == string(by)
}

// approvalBuilderBlocks is interactions.go's ApprovalMessage output, reduced to its blocks — the ONE mint of
// an actionable element in this tree. The journey sweeps it to prove the sweep DISCRIMINATES.
//
// It is called rather than posted, and that is an honest statement about the tree: ApprovalMessage has no
// production caller today (the approval message is posted by the operator's live leg, and slack_decision.go
// only REPAIRS a message that already exists). The singularity being asserted is therefore about the MINT,
// not about a worker.
func approvalBuilderBlocks(t *testing.T) json.RawMessage {
	t.Helper()
	var body struct {
		Blocks json.RawMessage `json:"blocks"`
	}
	if err := json.Unmarshal(slack.ApprovalMessage("C90", "1700000103.000100", "push agent/journey to main", "req_hash_journey"), &body); err != nil {
		t.Fatalf("decode the approval builder's message: %v", err)
	}
	return body.Blocks
}

// reservedSourceEvents reads the SOURCE EVENT ids the tenant reserved under one route constant, in birth
// order, with the team prefix stripped. Only the SHARED Admit writes an idempotency_records row, so this
// list IS the evidence that every entrance went through it rather than through a parallel path a new
// surface quietly opened — and reading it from the DATABASE rather than from the journey's own bookkeeping
// is what makes the counter a measurement instead of a restatement.
func (f *slackFixture) reservedSourceEvents(t *testing.T, route string) []string {
	t.Helper()
	rows, err := f.pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT idempotency_key FROM idempotency_records
		  WHERE organization_id=$1 AND project_id=$2 AND route=$3 ORDER BY id`, f.org, f.project, route)
	if err != nil {
		t.Fatalf("read reservations under %s: %v", route, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan reservation: %v", err)
		}
		ids = append(ids, strings.TrimPrefix(key, f.team+":"))
	}
	return ids
}

// contextEntitiesThatGainedAuthority is the (d) counter, DERIVED rather than declared: a context entity
// "gained authority" if the run it rode in on ended up in a tenant or on a revision other than the resolved
// connection's, or if ANY reservation ran as a principal other than the connection's. Those are the things
// authority could mean here, and all of them come from the connection row by construction — so the honest
// answer is zero, computed rather than asserted.
func (f *slackFixture) contextEntitiesThatGainedAuthority(t *testing.T, runID string) int {
	t.Helper()
	var org, project, revision string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT organization_id, project_id, COALESCE(agent_revision_id,'') FROM runs WHERE id=$1`, runID).
		Scan(&org, &project, &revision); err != nil {
		t.Fatalf("read the context-carrying run's identity: %v", err)
	}
	var foreignPrincipals int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM idempotency_records
		  WHERE organization_id=$1 AND project_id=$2 AND route=$3 AND principal_id <> $4`,
		f.org, f.project, uat.AgentSurfaceAdmissionRoute, f.principal).Scan(&foreignPrincipals); err != nil {
		t.Fatalf("count reservations taken under a foreign principal: %v", err)
	}
	gained := foreignPrincipals
	for _, mismatch := range []bool{org != f.org, project != f.project, revision != f.revision} {
		if mismatch {
			gained++
		}
	}
	return gained
}
