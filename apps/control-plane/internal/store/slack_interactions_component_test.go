//go:build component

package store_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// The E19 T2 interactivity decision path, end to end against REAL PostgreSQL and the REAL coordinator — and
// it is what EARNS deleting E17 T11's "UNWIRED DECISION PATH" note. That note said, correctly, that
// Store.SlackAuthorizationPolicyFor + SlackAuthorizationPolicy.ApproverAuthorized had no non-test caller and
// that no Slack HTTP route existed, so the journey hand-composed the chain a shipped handler would compose.
//
// These tests do not hand-compose anything. A signed form body enters the SHIPPED route and every link is the
// production one:
//
//	POST /v1/slack/interactions → VerifySignature(raw form body) → slack.MapInteractiveApproval →
//	Store.SlackAuthorizationPolicyFor → SlackAuthorizationPolicy.ApproverAuthorized →
//	coordinator.AcceptCommand → coordinator.ApplyApprovalDecision → chat.update repair
//
// The only fake is Slack itself (a local HTTP server standing in for slack.com/api), and the honest ceiling
// stays exactly where T1 left it: this is evidence about our code against the published contract, never
// evidence about a real workspace. `slack` stays PREVIEW.

// TestSlackAuthorizedClickApprovesThroughTheWholeChain is the positive: a MAPPED approver's click on a minted
// button carrying the exact request hash produces exactly ONE approve command and an APPROVED publication.
func TestSlackAuthorizedClickApprovesThroughTheWholeChain(t *testing.T) {
	f := newSlackFixture(t)
	thread := f.seedApproval(t, "C30", "1700000030.000100")

	resp := f.click(t, "Umapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an authorized click = %d, want 200 within 3 seconds", resp.StatusCode)
	}

	if n := f.commandCount(t, "approve"); n != 1 {
		t.Fatalf("the click enqueued %d approve commands, want exactly 1", n)
	}
	if state := f.publicationState(t, thread.publicationID); state != "approved" {
		t.Fatalf("the publication is %q after an AUTHORIZED click, want approved — the decision must be durable, not just accepted", state)
	}
	// The command was SETTLED by the same call that transitioned the publication (they commit together), so a
	// boundary pump re-draining later finds nothing to re-apply.
	if state := f.commandState(t, "approve"); state != "applied" {
		t.Fatalf("the approve command is %q, want applied — ApplyApprovalDecision settles the command it consumed", state)
	}
	// And the decision is recorded against the approval row (who decided), which is the audit half of SLK-007.
	var decidedBy string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT COALESCE(a.decided_by,'') FROM approvals a WHERE a.publication_id=$1`, thread.publicationID).Scan(&decidedBy); err != nil {
		t.Fatalf("read the approval decision: %v", err)
	}
	if decidedBy == "" {
		t.Fatal("the approval records no decider; a decision nobody can attribute is not an audit trail")
	}
}

// TestSlackUnauthorizedClickEnqueuesNothing is the negative that matters most, and the reason ApproverAuthorized
// exists at all (SLK-004): an UNMAPPED Slack user clicks the very same minted button carrying the very same
// request hash. Deny-by-default must stop it BEFORE any command is enqueued — not at the boundary, not by the
// coordinator, but here.
func TestSlackUnauthorizedClickEnqueuesNothing(t *testing.T) {
	f := newSlackFixture(t)
	thread := f.seedApproval(t, "C31", "1700000031.000100")

	resp := f.click(t, "Uunmapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	defer resp.Body.Close()
	// 200: the delivery succeeded and the refusal is recorded control-plane-side. A non-200 would turn "you
	// are not an approver" into a signal an unmapped user can read off the Slack UI.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an unauthorized click = %d, want 200 with nothing done", resp.StatusCode)
	}
	if n := f.commandCount(t, ""); n != 0 {
		t.Fatalf("an unauthorized click enqueued %d commands, want 0 — an unmapped clicker is a CONSTRAINED actor whose click authorizes nothing (SLK-004)", n)
	}
	if state := f.publicationState(t, thread.publicationID); state != "pending_approval" {
		t.Fatalf("the publication is %q after an UNAUTHORIZED click, want still pending_approval", state)
	}
	// The Slack message was NOT edited either: nothing about this click reached the outbound path.
	if n := len(f.slackCalls()); n != 0 {
		t.Fatalf("an unauthorized click made %d outbound Slack calls, want 0", n)
	}

	// The same button, clicked by the MAPPED user, still works — so the refusal above was about the CLICKER
	// and not about a broken fixture.
	ok := f.click(t, "Umapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	defer ok.Body.Close()
	if state := f.publicationState(t, thread.publicationID); state != "approved" {
		t.Fatalf("the mapped approver's click left the publication %q, want approved — the allow-list must discriminate, not refuse everyone", state)
	}
}

// TestSlackStaleHashAndForeignThreadDecideNothing covers the two bindings a decision carries beyond the
// clicker: the exact operation (the one-shot request hash) and the conversation (the thread the button was
// clicked in). Either one being wrong authorizes nothing and enqueues nothing.
func TestSlackStaleHashAndForeignThreadDecideNothing(t *testing.T) {
	f := newSlackFixture(t)
	thread := f.seedApproval(t, "C32", "1700000032.000100")

	// A hash that does not match the session's pending approval — a moved head, an edited request, or a
	// button replayed from a different operation.
	stale := f.click(t, "Umapped", thread.channel, thread.root, slack.ActionApprove, "req_hash_from_another_operation", time.Now())
	stale.Body.Close()
	if n := f.commandCount(t, ""); n != 0 {
		t.Fatalf("a STALE request hash enqueued %d commands, want 0 — the one-shot binding is what makes a Slack approval exact (SLK-007)", n)
	}
	if state := f.publicationState(t, thread.publicationID); state != "pending_approval" {
		t.Fatalf("a stale hash left the publication %q, want still pending_approval", state)
	}

	// The right hash, clicked in a thread that owns no session at all. The click cannot reach across
	// conversations to decide one it does not belong to.
	foreign := f.click(t, "Umapped", "C99", "1700009999.000100", slack.ActionApprove, thread.requestHash, time.Now())
	foreign.Body.Close()
	if n := f.commandCount(t, ""); n != 0 {
		t.Fatalf("a click from an UNCORRELATED thread enqueued %d commands, want 0 — a decision is bound to the conversation it was clicked in", n)
	}
	if state := f.publicationState(t, thread.publicationID); state != "pending_approval" {
		t.Fatalf("a foreign-thread click left the publication %q, want still pending_approval", state)
	}
}

// TestSlackDenyClickDeniesThePublication: the deny button is the same chain with the other verdict, and it is
// tested separately because "approve works" says nothing about whether deny transitions the row at all.
func TestSlackDenyClickDeniesThePublication(t *testing.T) {
	f := newSlackFixture(t)
	thread := f.seedApproval(t, "C33", "1700000033.000100")

	resp := f.click(t, "Umapped", thread.channel, thread.root, slack.ActionDeny, thread.requestHash, time.Now())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a deny click = %d, want 200", resp.StatusCode)
	}
	if state := f.publicationState(t, thread.publicationID); state != "denied" {
		t.Fatalf("the publication is %q after a deny click, want denied", state)
	}
	if n := f.commandCount(t, "deny"); n != 1 {
		t.Fatalf("%d deny commands, want exactly 1", n)
	}
}

// TestSlackDecisionRepairsTheVisibleMessageExactlyOnceUnderA429 is the outbound half (SLK-006 + plan §3.5
// D10): the decision edits the message the human is looking at, with the bot token resolved from
// bot_token_ref, honouring Retry-After exactly once — and a rate-limited repair NEVER erases the canonical
// decision.
//
// CONTRACT: https://docs.slack.dev/apis/web-api/rate-limits/ (checked 2026-07-26) — a rate-limited call
// answers "HTTP 1.1 429 Too Many Requests" with "Retry-After: [number]" seconds.
//
// It drives the bridge's Decide directly rather than the HTTP route: the route's ack budget (2s) is not what
// this test is about, and making a real Retry-After sleep race it would be a flake, not a proof. The route's
// own budget is pinned in apps/control-plane/api.
func TestSlackDecisionRepairsTheVisibleMessageExactlyOnceUnderA429(t *testing.T) {
	f := newSlackFixture(t)
	thread := f.seedApproval(t, "C34", "1700000034.000100")
	f.slackStatuses(http.StatusTooManyRequests, http.StatusOK)
	f.slackRetryAfter("1")

	start := time.Now()
	out, err := f.bridge.Decide(context.Background(), f.connRef(t), slack.ApprovalIntent{
		TeamID: f.team, UserID: "Umapped", RequestHash: thread.requestHash, Decision: "approve",
		ActionID: slack.ActionApprove, ChannelID: thread.channel, ThreadTS: thread.root,
		MessageTS: thread.botMessageTS,
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if out.Rejected != "" {
		t.Fatalf("Decide rejected an authorized click: %q", out.Rejected)
	}
	if !out.Repaired {
		t.Fatal("the visible message was not repaired after the 429")
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("the repair took %v — Retry-After: 1 was not honoured, it was retried immediately", elapsed)
	}

	calls := f.slackCalls()
	if len(calls) != 2 {
		t.Fatalf("%d outbound calls, want 2 (the refused one and its single repair)", len(calls))
	}
	if calls[0].body != calls[1].body {
		t.Fatalf("the repair replayed a DIFFERENT body:\n%s\n%s", calls[0].body, calls[1].body)
	}
	for i, c := range calls {
		if !strings.HasSuffix(c.path, "/chat.update") {
			t.Fatalf("call %d went to %q, want chat.update — the visible message is EDITED, not duplicated with a second post (SLK-006)", i, c.path)
		}
		if c.auth != "Bearer "+string(f.botToken) {
			t.Fatalf("call %d carried Authorization %q, want the token resolved from bot_token_ref", i, c.auth)
		}
		if strings.Contains(c.body, string(f.botToken)) {
			t.Fatalf("call %d put the bot token in the BODY; it rides the Authorization header only", i)
		}
	}
	// The repair targeted the visible bot message and RECORDED it as the thread's repair handle, so any later
	// coalesced update edits that same one message rather than posting a second (SLK-006).
	if got := f.threadMessageTS(t, thread.channel, thread.root); got != thread.botMessageTS {
		t.Fatalf("last_bot_message_ts = %q, want the repaired message %q", got, thread.botMessageTS)
	}
	// And the canonical decision survived the rate limit entirely.
	if state := f.publicationState(t, thread.publicationID); state != "approved" {
		t.Fatalf("the publication is %q after a rate-limited repair, want approved — a Slack delivery problem never erases canonical state", state)
	}
}

// TestSlackDecisionSurvivesAPermanentlyRateLimitedSlack is the same invariant at its limit: Slack refuses
// every attempt, the repair budget is spent, and the DECISION still stands. A failure to draw the outcome is
// not a failure to reach it.
func TestSlackDecisionSurvivesAPermanentlyRateLimitedSlack(t *testing.T) {
	f := newSlackFixture(t)
	thread := f.seedApproval(t, "C35", "1700000035.000100")
	f.slackStatuses(http.StatusTooManyRequests)
	f.slackRetryAfter("0") // the budget, not the wall clock, is what this test is about

	out, err := f.bridge.Decide(context.Background(), f.connRef(t), slack.ApprovalIntent{
		TeamID: f.team, UserID: "Umapped", RequestHash: thread.requestHash, Decision: "approve",
		ActionID: slack.ActionApprove, ChannelID: thread.channel, ThreadTS: thread.root,
		MessageTS: thread.botMessageTS,
	})
	if err != nil {
		t.Fatalf("a permanently rate-limited repair FAILED the decision: %v — the canonical result must survive a Slack outage", err)
	}
	if out.Rejected != "" {
		t.Fatalf("Decide rejected the click because Slack was unreachable: %q", out.Rejected)
	}
	if out.Repaired {
		t.Fatal("Repaired=true while every attempt was refused; the outcome must report the message as stale, not claim a repair that never landed")
	}
	if state := f.publicationState(t, thread.publicationID); state != "approved" {
		t.Fatalf("the publication is %q, want approved", state)
	}
	if n := len(f.slackCalls()); n != 2 {
		t.Fatalf("%d outbound attempts, want 2 (the call and its ONE bounded repair) — a persistent 429 must not become a retry storm", n)
	}
}

// TestSlackCoalescedUpdatesArePacedPerChannel closes D10's pacing half against the wired path: two decisions
// in the SAME channel are held to the documented Special-Tier rate, while a decision in a different channel
// is not made to wait behind them.
func TestSlackCoalescedUpdatesArePacedPerChannel(t *testing.T) {
	f := newSlackFixture(t)
	first := f.seedApproval(t, "C36", "1700000036.000100")
	second := f.seedApproval(t, "C36", "1700000037.000100")
	other := f.seedApproval(t, "C37", "1700000038.000100")

	decide := func(th seededApproval) time.Duration {
		t.Helper()
		start := time.Now()
		out, err := f.bridge.Decide(context.Background(), f.connRef(t), slack.ApprovalIntent{
			TeamID: f.team, UserID: "Umapped", RequestHash: th.requestHash, Decision: "approve",
			ActionID: slack.ActionApprove, ChannelID: th.channel, ThreadTS: th.root, MessageTS: th.botMessageTS,
		})
		if err != nil || out.Rejected != "" {
			t.Fatalf("Decide(%s) = (%+v,%v)", th.channel, out, err)
		}
		return time.Since(start)
	}

	decide(first)
	sameChannel := decide(second)
	if sameChannel < slack.SpecialTierPerChannel {
		t.Fatalf("a second update in the SAME channel took %v, want at least %v — chat.postMessage is the Special Tier (~1 message per second per channel), so unpaced coalesced updates are asking to be 429'd (plan §3.5 D10)",
			sameChannel, slack.SpecialTierPerChannel)
	}
	if differentChannel := decide(other); differentChannel >= slack.SpecialTierPerChannel {
		t.Fatalf("an update in a DIFFERENT channel waited %v — the documented limit is per channel; pacing globally throttles unrelated conversations", differentChannel)
	}
}

// ---- fixture ---------------------------------------------------------------------------------------

// seededApproval is one thread with a live run and a pending publication awaiting approval.
type seededApproval struct {
	channel       string
	root          string
	sessionID     string
	publicationID string
	requestHash   string
	botMessageTS  string // the ts of the (fake) approval message the buttons live on
}

// seedApproval drives a REAL Slack event through the shipped events route to create the thread↔session
// correlation and its run, then requests a publication on that session — so the approval the click decides
// hangs off the same session the thread owns, exactly as production would have it.
func (f *slackFixture) seedApproval(t *testing.T, channel, root string) seededApproval {
	t.Helper()
	ctx := context.Background()
	// No terminateRuns here, and it is load-bearing rather than an omission: ApplyApprovalDecision runs under
	// guardRunActive, so the run the approval hangs off must still be LIVE. Each seed uses its own thread and
	// therefore its own session, so the one-active-root rule is never in play.
	resp := f.deliver(t, f.event(newID("Ev"), "app_mention", "Umapped", channel, root, ""), time.Now(), "", "")
	if resp.StatusCode/100 != 2 {
		t.Fatalf("seed event = %d, want a 2xx ack", resp.StatusCode)
	}
	resp.Body.Close()

	var sessionID, runID, responseID string
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT t.session_id, r.id, COALESCE(r.response_id,'')
		   FROM slack_thread_sessions t JOIN runs r ON r.session_id = t.session_id
		  WHERE t.project_id=$1 AND t.channel_id=$2 AND t.thread_ts=$3`, f.project, channel, root).Scan(&sessionID, &runID, &responseID); err != nil {
		t.Fatalf("read the seeded thread's session/run: %v", err)
	}

	branch := "agent/" + newID("b")
	hash := repositories.RequestHash(f.project, runID, repositories.OpPushBranch, "git@fake:o/r", branch, "main", "c0ffee")
	pub, err := f.spine.RequestPublication(ctx, coordinator.Tenant{Project: f.project},
		coordinator.PublicationRequest{
			PublicationID: newID("pub"), ApprovalID: newID("apr"), SessionID: sessionID, RunID: runID,
			ResponseID: responseID, Operation: "push_branch", Remote: "git@fake:o/r", Branch: branch,
			Base: "main", HeadSHA: "c0ffee",
			IdempotencyKey: repositories.IdempotencyKey(f.project, runID, repositories.OpPushBranch, "git@fake:o/r", branch, "main", "c0ffee"),
			RequestHash:    hash,
			Display:        "push " + branch,
		})
	if err != nil {
		t.Fatalf("request the publication the click will decide: %v", err)
	}
	return seededApproval{
		channel: channel, root: root, sessionID: sessionID, publicationID: pub.ID, requestHash: pub.RequestHash,
		botMessageTS: root, // the approval message is the thread root in this fixture
	}
}

// click posts a real interactivity callback: the documented form body, signed over the RAW bytes.
//
// CONTRACT: https://docs.slack.dev/interactivity/handling-user-interaction/ (checked 2026-07-26) — an
// interactivity POST is application/x-www-form-urlencoded with a single `payload` parameter of JSON.
func (f *slackFixture) click(t *testing.T, user, channel, messageTS, actionID, value string, at time.Time) *http.Response {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"team": map[string]any{"id": f.team, "domain": "example"},
		"user": map[string]any{"id": user, "username": "clicker", "team_id": f.team},
		"container": map[string]any{
			"type": "message", "message_ts": messageTS, "channel_id": channel, "is_ephemeral": false,
		},
		"channel":      map[string]any{"id": channel, "name": "approvals"},
		"message":      map[string]any{"type": "message", "ts": messageTS, "text": "approve?"},
		"response_url": "https://hooks.slack.invalid/actions/T0/1/2",
		"actions": []any{map[string]any{
			"action_id": actionID, "block_id": "=qXel", "value": value, "type": "button",
			"action_ts": "1548426417.840180",
		}},
	})
	raw := []byte("payload=" + url.QueryEscape(string(payload)))
	timestamp, signature := signSlack(f.secret, raw, at)

	req, err := http.NewRequest(http.MethodPost, f.url+"/v1/slack/interactions", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("build the click: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", signature)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/slack/interactions: %v", err)
	}
	return resp
}

// commandCount counts the tenant's durable commands, optionally of one kind.
func (f *slackFixture) commandCount(t *testing.T, kind string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM commands WHERE  project_id=$1 AND ($2='' OR kind=$2)`, f.project, kind).Scan(&n); err != nil {
		t.Fatalf("count commands: %v", err)
	}
	return n
}

func (f *slackFixture) commandState(t *testing.T, kind string) string {
	t.Helper()
	var state string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM commands WHERE  project_id=$1 AND kind=$2`, f.project, kind).Scan(&state); err != nil {
		t.Fatalf("read command state: %v", err)
	}
	return state
}

func (f *slackFixture) publicationState(t *testing.T, id string) string {
	t.Helper()
	var state string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM publications WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatalf("read publication state: %v", err)
	}
	return state
}

func (f *slackFixture) threadMessageTS(t *testing.T, channel, root string) string {
	t.Helper()
	var ts string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT COALESCE(last_bot_message_ts,'') FROM slack_thread_sessions
		  WHERE project_id=$1 AND channel_id=$2 AND thread_ts=$3`, f.project, channel, root).Scan(&ts); err != nil {
		t.Fatalf("read last_bot_message_ts: %v", err)
	}
	return ts
}
