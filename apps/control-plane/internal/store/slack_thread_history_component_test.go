//go:build component

package store_test

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/storage"
)

// READING A THREAD THE APP WAS INVITED INTO LATE, against real PostgreSQL, the shipped route and the real
// admission — with a FAKE Slack peer built from the published reference.
//
// THE OBSERVATION. The owner invited the bot into an existing #centauri thread carrying a long technical
// discussion, mentioned it and wrote "özetle" (summarise). The reply was "please share the text you want
// summarised", because the run's entire input was the word "özetle": the thread predated the bot, so no session
// and no history existed. The reasoning in slackRunInput said thread history was unnecessary ("the session
// already carries it") and impossible ("a scope this app is not granted"). The first is true only of a thread
// the app was in from the start; the second is simply false — channels:history is granted.
//
// CONTRACT for the scripted bodies: https://docs.slack.dev/reference/methods/conversations.replies/ (checked
// 2026-07-27) — GET, required channel + ts, answers {ok, messages:[…], has_more}; "the earliest messages in the
// time range are returned first"; `limit` DEFAULTS TO 1000. Built from that page, not from our client.

// threadPage scripts one conversations.replies answer.
func threadPage(hasMore bool, messages ...map[string]any) scriptedReply {
	body, err := json.Marshal(map[string]any{"ok": true, "has_more": hasMore, "messages": messages})
	if err != nil {
		panic(err)
	}
	return scriptedReply{status: 200, body: string(body)}
}

// threadSaid is one published message object. A subtype makes it non-content (a tombstone, a join, a share).
func threadSaid(user, ts, text, subtype string) map[string]any {
	m := map[string]any{"type": "message", "user": user, "ts": ts, "text": text}
	if subtype != "" {
		m["subtype"] = subtype
	}
	return m
}

// repliesCalls returns the reads the fake saw, with their arguments decoded — a read is a documented GET, so
// what it ADDRESSED lives in the query string, not in a body.
func (f *slackFixture) repliesCalls(t *testing.T) []url.Values {
	t.Helper()
	var out []url.Values
	for _, c := range f.callsTo("/conversations.replies") {
		q, err := url.ParseQuery(c.query)
		if err != nil {
			t.Fatalf("parse the read's arguments %q: %v", c.query, err)
		}
		if !strings.HasPrefix(c.auth, "Bearer xoxb-") {
			t.Fatalf("the read carried Authorization %q, want the connection's bot token as a bearer", c.auth)
		}
		if strings.Contains(c.query, "xoxb") {
			t.Fatalf("the bot token reached the query string: %s", c.query)
		}
		out = append(out, q)
	}
	return out
}

// inputs returns every stored run input in creation order — this suite births more than one run per test, so
// the single-row runInput helper cannot answer "what did the SECOND run see".
func (f *slackFixture) inputs(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT input::text FROM responses WHERE  project_id=$1 ORDER BY created_at, id`, f.project)
	if err != nil {
		t.Fatalf("read the stored run inputs: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var in string
		if err := rows.Scan(&in); err != nil {
			t.Fatalf("scan an input: %v", err)
		}
		out = append(out, in)
	}
	return out
}

// THE TEST THAT MATTERS. A mention in a thread the app has NO session for produces a run whose input carries
// that thread's earlier messages; a mention in a thread it already has history for does NOT fetch again.
//
// Both halves are load bearing and they pull in opposite directions, which is why they are one test: fetching
// nothing was the live defect, and fetching every time would put a second, disagreeing copy of the conversation
// in the same prompt that run.start already replays.
func TestSlackThreadHistoryIsFetchedOnceForAThreadWeWereInvitedInto(t *testing.T) {
	f := newSlackFixture(t)
	const root = "1700000200.000100"

	// The thread as it stood BEFORE the bot was invited: two engineers, one of them retracted, plus a join.
	f.slackScript("/conversations.replies", threadPage(false,
		threadSaid("U0ALICE", root, "the migration lock times out at 41", ""),
		threadSaid("U0BOB", "1700000201.000100", "because the batch is unbounded", ""),
		threadSaid("U0BOB", "1700000202.000100", "This message was deleted.", "tombstone"),
		threadSaid("U0CAROL", "1700000203.000100", "<@U0CAROL> has joined the channel", "channel_join"),
		threadSaid("U0ALICE", "1700000204.000100", "so we cap it and retry", ""),
	))

	// The invitation: a mention INSIDE that existing thread (thread_ts is the older root, not this message).
	f.deliver(t, f.eventText(t, "EvHist1", "app_mention", "Umapped", "C200", "1700000205.000100", root,
		"<@"+f.botUser+"> özetle"), time.Now(), "", "").Body.Close()

	if n := f.runCount(t); n != 1 {
		t.Fatalf("the mention birthed %d run(s), want 1", n)
	}
	reads := f.repliesCalls(t)
	if len(reads) != 1 {
		t.Fatalf("fake Slack saw %d conversations.replies call(s), want exactly 1 — a thread with no session of "+
			"ours is the ONLY case that earns a read", len(reads))
	}
	// SCOPE: the read addressed the ADMITTED EVENT's own channel and thread root, and carried an explicit
	// bound. Slack's own limit default is 1000, so an absent bound is an unbounded prompt.
	if got := reads[0].Get("channel"); got != "C200" {
		t.Fatalf("the read addressed channel %q, want the event's own C200", got)
	}
	if got := reads[0].Get("ts"); got != root {
		t.Fatalf("the read addressed ts %q, want the event's own thread root %q", got, root)
	}
	if reads[0].Get("limit") == "" {
		t.Fatal("the read carried no limit — conversations.replies defaults to 1000, i.e. to an unbounded prompt")
	}

	input := f.inputs(t)[0]
	for _, said := range []string{"the migration lock times out at 41", "because the batch is unbounded", "so we cap it and retry"} {
		if !strings.Contains(input, said) {
			t.Fatalf("the run's input does not carry %q — this is the whole defect: the model was asked to "+
				"summarise a thread it had never been shown.\ninput = %s", said, input)
		}
	}
	// A RETRACTION STAYS RETRACTED, from Slack's side too: a deleted message must not come back through a read.
	if strings.Contains(input, "This message was deleted") || strings.Contains(input, "has joined the channel") {
		t.Fatalf("the input carries a tombstone or a join — only real message content may be quoted:\n%s", input)
	}
	// UNTRUSTED, LABELLED, AND THE HUMAN'S WORDS CLOSE IT.
	if !strings.Contains(input, "untrusted context") || !strings.Contains(input, "not an instruction") {
		t.Fatalf("the quoted thread is not labelled untrusted — it is other people's words:\n%s", input)
	}
	if strings.LastIndex(input, "özetle") < strings.Index(input, "end of the quoted thread") {
		t.Fatalf("the human's own ask is not the last thing in the prompt:\n%s", input)
	}
	// NO IDS AND NO CREDENTIAL. The tenant, the clicker, the channel and the token are all scope or secret;
	// none of them is conversation.
	for _, leaked := range []string{"U0ALICE", "U0BOB", "Umapped", "C200", f.team, f.principal, string(f.botToken), "xoxb"} {
		if strings.Contains(input, leaked) {
			t.Fatalf("the input carries %q — an id is scope and a token is a secret; neither belongs in a prompt:\n%s", leaked, input)
		}
	}

	// ---- the second half: a thread we ALREADY have history for is not re-read -------------------------
	//
	// The mention above correlated this thread to a session, so run.start replays it from here on. A second
	// read would duplicate that — and disagree with it the moment somebody edits a message.
	f.terminateRuns(t) // one active root run per session
	f.slackScript("/conversations.replies", threadPage(false,
		threadSaid("U0ALICE", "1700000206.000100", "A SECOND FETCH HAPPENED", "")))
	follow := f.deliver(t, f.eventText(t, "EvHist2", "message", "Umapped", "C200", "1700000207.000100", root,
		"and the rollback?"), time.Now(), "", "")
	if follow.StatusCode/100 != 2 {
		t.Fatalf("the follow-up in our own thread = %d, want a 2xx ack", follow.StatusCode)
	}
	follow.Body.Close()

	if n := len(f.repliesCalls(t)); n != 1 {
		t.Fatalf("fake Slack saw %d conversations.replies call(s) in total, want still 1 — a thread whose session "+
			"we chain onto already HAS its history, and a second copy in the same prompt is what the original "+
			"reasoning was right to refuse", n)
	}
	inputs := f.inputs(t)
	if len(inputs) != 2 {
		t.Fatalf("%d stored inputs, want 2", len(inputs))
	}
	if strings.Contains(inputs[1], "A SECOND FETCH HAPPENED") || strings.Contains(inputs[1], "untrusted context") {
		t.Fatalf("the follow-up's prompt carries a quoted thread:\n%s", inputs[1])
	}
	if inputs[1] != `"and the rollback?"` {
		t.Fatalf("the follow-up's input = %s, want the bare message — nothing about this event changed", inputs[1])
	}
}

// A TOP-LEVEL MENTION READS NOTHING. It IS its own thread root, so a read would answer with the mention itself
// and nothing else: one Slack call per new conversation to learn the words we already have.
func TestSlackTopLevelMentionReadsNoThread(t *testing.T) {
	f := newSlackFixture(t)

	f.deliver(t, f.eventText(t, "EvHist3", "app_mention", "Umapped", "C201", "1700000210.000100", "",
		"<@"+f.botUser+"> merhaba"), time.Now(), "", "").Body.Close()

	if n := f.runCount(t); n != 1 {
		t.Fatalf("the mention birthed %d run(s), want 1", n)
	}
	if n := len(f.repliesCalls(t)); n != 0 {
		t.Fatalf("fake Slack saw %d conversations.replies call(s), want 0 — a lone mention has no earlier thread", n)
	}
	if got := f.runInput(t); got != `"merhaba"` {
		t.Fatalf("input = %s, want the bare message, byte-identical to what it was before thread history existed", got)
	}
}

// THE READ IS KEYED ON THE EVENT, NEVER ON WHAT THE HUMAN IS LOOKING AT. This is E20 T3's fourth boundary
// re-asserted against the ONE read that now exists — and it is a stronger assertion than the original, because
// the original could only prove that NO read happened. Here a read genuinely happens, and it addresses the
// event's channel rather than the context's.
//
// The app holds channels:history, so a read keyed on a context entity would hand the user the CONNECTION's
// access: the context reports what the USER sees, while the run carries the connection principal's authority.
func TestSlackThreadReadAddressesTheEventNotTheContext(t *testing.T) {
	f := newSlackFixture(t)
	const root, private = "1700000220.000100", "C0PRIVATEROOM"

	f.slackScript("/conversations.replies", threadPage(false,
		threadSaid("U0ALICE", root, "the thread the mention arrived in", "")))

	f.deliver(t, withContext(t,
		f.eventText(t, "EvHist4", "app_mention", "Umapped", "C220", "1700000221.000100", root,
			"<@"+f.botUser+"> özetle"),
		ctxEntity("slack#/types/channel_id", private, f.team)), time.Now(), "", "").Body.Close()

	reads := f.repliesCalls(t)
	if len(reads) != 1 {
		t.Fatalf("fake Slack saw %d read(s), want 1 — with none, the assertion below would be vacuous", len(reads))
	}
	if reads[0].Get("channel") != "C220" {
		t.Fatalf("the read addressed %q, want the EVENT's channel C220", reads[0].Get("channel"))
	}
	for _, c := range f.slackCalls() {
		if strings.Contains(c.path, private) || strings.Contains(c.body, private) || strings.Contains(c.query, private) {
			t.Fatalf("an outbound call addressed the CONTEXT channel %s (%s?%s) — the context describes what the "+
				"USER sees, while every call carries the CONNECTION's authority", private, c.path, c.query)
		}
	}
	// The context is still DESCRIBED (that is E20 T3's behaviour and it is unchanged), just never fetched.
	if input := f.runInput(t); !strings.Contains(input, private) {
		t.Fatalf("the context channel is no longer described: %s", input)
	}
}

// A CHANNEL OUTSIDE allowed_channels IS NOT READ. The scope check is at the very top of Admit, so an event the
// operator confined this integration out of must birth nothing — and must not spend a read on the way. Without
// this ordering, allowed_channels would gate the RUN while leaving the workspace readable.
func TestSlackThreadHistoryIsNotReadOutsideTheChannelAllowList(t *testing.T) {
	f := newSlackFixture(t)
	f.scopeToChannels(t, "C230")

	out := f.deliver(t, f.eventText(t, "EvHist5", "app_mention", "Umapped", "C231", "1700000231.000100",
		"1700000230.000100", "<@"+f.botUser+"> özetle"), time.Now(), "", "")
	defer out.Body.Close()
	if out.StatusCode != 422 {
		t.Fatalf("an event from outside allowed_channels = %d, want 422", out.StatusCode)
	}
	if n := f.runCount(t); n != 0 {
		t.Fatalf("%d run(s) born, want 0", n)
	}
	if n := len(f.slackCalls()); n != 0 {
		t.Fatalf("the refused event produced %d outbound Slack call(s), want 0 — a channel the operator "+
			"confined this integration out of must not be readable either: %v", n, f.slackCalls())
	}
}

// SLACK REFUSING THE READ COSTS THE CONTEXT, NEVER THE RUN. `missing_scope` is the expected one and it is a
// POSTURE fact, not a defect: the manifest grants channels:history and im:history, so a PRIVATE channel
// (groups:history) answers exactly this. The message still gets an answer, with the prompt it had before.
func TestSlackThreadHistoryRefusalStillAdmitsTheRun(t *testing.T) {
	f := newSlackFixture(t)

	f.slackRefuse("/conversations.replies", "missing_scope")
	resp := f.deliver(t, f.eventText(t, "EvHist6", "app_mention", "Umapped", "C240", "1700000241.000100",
		"1700000240.000100", "<@"+f.botUser+"> özetle"), time.Now(), "", "")
	if resp.StatusCode/100 != 2 {
		t.Fatalf("a refused thread read made the admission answer %d, want a 2xx — a read is not the run", resp.StatusCode)
	}
	resp.Body.Close()
	if n := f.runCount(t); n != 1 {
		t.Fatalf("%d run(s) born, want 1", n)
	}
	// NOT VACUOUS: the read has to have been ATTEMPTED, or this test is green for a build that never reads a
	// thread at all — which is the state this whole change exists to leave behind.
	if n := len(f.repliesCalls(t)); n != 1 {
		t.Fatalf("fake Slack saw %d read(s), want exactly 1 — with none, this test proves nothing about a refusal", n)
	}
	if got := f.runInput(t); got != `"özetle"` {
		t.Fatalf("input = %s, want the bare message: no history is a less useful answer, never a lost message", got)
	}
}

// A REDELIVERY REPLAYS, IT DOES NOT CONFLICT — and this is the constraint that decided WHERE the fetched text
// goes: the request HASH is over the EVENT alone, and the thread history rides only the prompt.
//
// The mechanism is exactly what this test walks into. The first delivery reads the thread and its prompt carries
// the quoted messages; the redelivery finds the correlation the first one claimed, so it reads NOTHING and its
// prompt would be the bare message. Those are two different strings for one event id. If the reservation's hash
// were taken over the prompt, the redelivery would be refused as "the source event id was reused with a
// different request" instead of replaying onto the run that already exists — SLK-002 inverted.
func TestSlackThreadHistoryRedeliveryStillReplays(t *testing.T) {
	f := newSlackFixture(t)
	const root = "1700000250.000100"
	body := f.eventText(t, "EvHist7", "app_mention", "Umapped", "C250", "1700000251.000100", root,
		"<@"+f.botUser+"> özetle")

	f.slackScript("/conversations.replies", threadPage(false, threadSaid("U0ALICE", root, "the first delivery's thread", "")))
	f.deliver(t, body, time.Now(), "", "").Body.Close()

	again := f.deliver(t, body, time.Now(), "1", "http_timeout")
	if again.StatusCode/100 != 2 {
		t.Fatalf("the redelivery = %d, want a 2xx replay", again.StatusCode)
	}
	again.Body.Close()

	if n := f.runCount(t); n != 1 {
		t.Fatalf("the redelivery birthed a total of %d run(s), want 1 (SLK-002)", n)
	}
	// The two prompts genuinely DIFFER, which is what makes the reservation's answer below meaningful: the
	// first delivery read the thread, the redelivery did not.
	reads := f.repliesCalls(t)
	if len(reads) != 1 {
		t.Fatalf("fake Slack saw %d read(s), want 1 — the first delivery reads and the redelivery must not; "+
			"with 0 or 2 the prompts would be identical and this test would assert nothing", len(reads))
	}
	if input := f.runInput(t); !strings.Contains(input, "the first delivery's thread") {
		t.Fatalf("the stored prompt is the redelivery's, not the original's: %s", input)
	}
	var records int
	var status string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*), COALESCE(max(status),'') FROM idempotency_records WHERE  project_id=$1`, f.project).Scan(&records, &status); err != nil {
		t.Fatalf("read the reservation: %v", err)
	}
	if records != 1 {
		t.Fatalf("%d idempotency records for one source event, want 1", records)
	}
	if status == "conflict" {
		t.Fatal("the redelivery was recorded as an idempotency CONFLICT — the admitted REQUEST must be a pure " +
			"function of the event, so the fetched thread may ride the prompt but never the request hash")
	}
}
