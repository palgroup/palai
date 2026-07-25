//go:build component

// The E17 T11 EXIT-gate SLACK JOURNEY — spec §63.3, all ten steps, ENTIRELY on a FAKE Slack peer.
//
// It lives here (package extensions) because this is the Slack connection's home and the journey needs three
// real seams at once against real Postgres: the T1 slack_connections / slack_thread_sessions store, the
// canonical automation delivery pipeline (durable insert → source-dedupe → named-session send_message — NO
// Slack-specific dedupe is invented, §34.1), and the coordinator's one-shot approval primitive. The pure
// adapter (adapters/integrations/slack) supplies v0 signature verification, event normalization, Socket Mode
// unwrapping, the interactive-approval mapping and the bounded 429 repair.
//
// §63.3's pass sentence is asserted VERBATIM and mechanically:
//   - ONE canonical session (the thread correlates once; a second event offering another session reuses it);
//   - ONE effect per source event (a redelivery collapses to `duplicate` with original linkage, no 2nd command);
//   - correct actor authorization (only a minted button carrying the exact request_hash from a MAPPED user
//     approves; an unmapped clicker and a plain "yes" message authorize nothing);
//   - the canonical result stays recoverable when the Slack output update FAILS.
//
// HONEST CEILING, stated loudly: the peer is FAKE. Every chat.postMessage receipt, every 429, and every
// Socket Mode frame is fixture data produced in this process. NOTHING here is evidence about a real Slack
// workspace — that external receipt is §6 leg 1 and is the operator work that would flip `slack` from
// preview to stable. The emitted uat.SlackMappingProof is structurally incapable of claiming otherwise
// (its Peer field must be the literal "fake").
package extensions

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
	"github.com/palgroup/palai/tests/uat"
)

// fakeSlackPeer is the whole of "Slack" for this journey: a slack.Doer that records every outbound call and
// replays a scripted status sequence. It is the honest ceiling made concrete — there is no socket to
// slack.com anywhere in this file.
type fakeSlackPeer struct {
	statuses []int    // scripted per-call HTTP statuses; the last one repeats
	call     int      // calls made
	posts    []string // the message ts of every ACCEPTED post/update ("receipts")
	bodies   []string // the raw body of every attempted call, so a repair can be shown to replay the SAME body
	retryAfter string
}

func (p *fakeSlackPeer) Do(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	p.bodies = append(p.bodies, string(body))
	status := 200
	if p.call < len(p.statuses) {
		status = p.statuses[p.call]
	} else if len(p.statuses) > 0 {
		status = p.statuses[len(p.statuses)-1]
	}
	p.call++

	header := http.Header{"Content-Type": []string{"application/json"}}
	if status == http.StatusTooManyRequests {
		after := p.retryAfter
		if after == "" {
			after = "1"
		}
		header.Set("Retry-After", after)
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error":"ratelimited"}`))}, nil
	}
	ts := fmt.Sprintf("9%02d.000100", p.call)
	p.posts = append(p.posts, ts)
	return &http.Response{StatusCode: status, Header: header,
		Body: io.NopCloser(strings.NewReader(`{"ok":true,"ts":"` + ts + `"}`))}, nil
}

// noWait replaces the real Retry-After sleep so the bounded repair is deterministic and instant. The BUDGET
// is what the journey proves (exactly one repair), not the wall-clock pause.
func noWait(context.Context, time.Duration) error { return nil }

// signedSlackRequest returns the two v0 headers for a body — the fake peer's half of Slack's signing scheme,
// built with the SAME construction slack.VerifySignature checks ("v0:{ts}:{body}", HMAC-SHA256).
func signedSlackRequest(secret, body []byte, now time.Time) (timestamp, signature string) {
	timestamp = strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	return timestamp, "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// slackEventBody builds an Events API event_callback envelope.
func slackEventBody(team, eventID, kind, user, channel, ts, threadTS string) []byte {
	inner := map[string]any{"type": kind, "user": user, "channel": channel, "ts": ts}
	if threadTS != "" {
		inner["thread_ts"] = threadTS
	}
	raw, _ := json.Marshal(map[string]any{
		"type": "event_callback", "team_id": team, "event_id": eventID, "event": inner,
	})
	return raw
}

// interactiveApprovalBody builds a Slack block_actions payload for one of the two minted buttons, carrying
// the one-shot request_hash in the button value.
func interactiveApprovalBody(team, user, actionID, requestHash string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"user": map[string]any{"id": user, "team_id": team},
		"team": map[string]any{"id": team},
		"actions": []any{map[string]any{
			"action_id": actionID, "type": "button", "value": requestHash,
		}},
	})
	return raw
}

// journeySpine opens the migrated spine and returns the coordinator store, the extensions store, the
// automation trigger store wired to the real admitter, and the shared pool.
func journeySpine(t *testing.T) (*coordinator.Store, *Store, *automation.TriggerStore, *pgxpool.Pool) {
	t.Helper()
	s, _, _ := openStore(t) // skips without PALAI_COMPONENT_POSTGRES_URL; migrates the spine
	pool := s.pool
	cs, err := coordinator.Open(context.Background(), poolURL(t))
	if err != nil {
		t.Fatalf("coordinator.Open for the journey admitter: %v", err)
	}
	t.Cleanup(cs.Close)
	return cs, New(pool, "file"), automation.NewTriggerStore(pool).WithAdmitter(cs), pool
}

// poolURL re-reads the component Postgres URL (openStore already skipped if it is unset).
func poolURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	return url
}

// TestSlackJourneyOnFakePeer is the §63.3 journey. Every step is numbered to the spec's list.
func TestSlackJourneyOnFakePeer(t *testing.T) {
	cs, store, triggers, pool := journeySpine(t)
	ctx := context.Background()

	const (
		team     = "T63300"
		channel  = "C63300"
		threadTS = "1700000000.000100"
		botUser  = "Ubot63300"
		mapped   = "Umapped63300"   // in slack_connections.allowed_users
		unmapped = "Uunmapped63300" // NOT in the allow-list — a constrained integration actor
	)
	signingSecret := []byte("journey-signing-secret-not-a-credential")

	// The tenant + the canonical session and its ACTIVE root run. The run's birth is not what this journey
	// proves (the Slack mapping is), so it is seeded directly — the seedRun idiom.
	org, project := seedOrgProject(t, store)
	tenant := coordinator.Tenant{Organization: org, Project: project}
	sessionID, otherSessionID := seedSession(t, store, org, project), seedSession(t, store, org, project)
	respID, runID := testID("resp"), testID("run")
	mustSystemExec(t, pool, `UPDATE sessions SET state='active' WHERE id=$1`, sessionID)
	mustSystemExec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'in_progress')`,
		respID, org, project, sessionID)
	mustSystemExec(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,$5,'running')`,
		runID, org, project, sessionID, respID)
	principal := testID("prin")
	mustSystemExec(t, pool, `INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1,$2,$3,'service')`, principal, org, project)

	// ---- 1. install the integration in a (fake) workspace -------------------------------------------
	// Credentials are secret_ref HANDLES; an inline value is refused by the store (SLK-004's write half).
	conn, err := store.CreateSlackConnection(ctx, org, project, []byte(`{
		"team_id":"`+team+`","bot_user_id":"`+botUser+`",
		"signing_secret_ref":"slack/journey/signing","bot_token_ref":"slack/journey/bot",
		"scopes":"app_mentions:read chat:write","allowed_users":["`+mapped+`"]}`))
	if err != nil {
		t.Fatalf("step 1 install: %v", err)
	}
	resolved, found, err := store.ResolveSlackConnectionByTeam(ctx, team, "")
	if err != nil || !found || resolved.ID != conn.ID {
		t.Fatalf("step 1: installed workspace does not resolve by team: (%+v,%v,%v)", resolved, found, err)
	}
	// The AUTHORIZATION policy the step-7 decision is checked against, read through the production seam
	// (SLK-004 promised this enforcement wiring to T11): the allow-list was written at install and is now
	// READABLE, so a decision can be refused before any command is enqueued.
	policy, err := store.SlackAuthorizationPolicyFor(ctx, resolved.Org, resolved.Project, conn.ID)
	if err != nil {
		t.Fatalf("step 1: read authorization policy: %v", err)
	}
	if !policy.ApproverAuthorized(mapped) || policy.ApproverAuthorized(unmapped) {
		t.Fatalf("step 1: allow-list %v must admit %q and refuse %q", policy.AllowedUsers, mapped, unmapped)
	}

	// The trigger the Slack pipeline delivers through: dedupe on the SLACK EVENT ID (the canonical
	// source-dedupe, not a parallel mechanism) and named_session correlation onto the thread's session, so a
	// message arriving while the run is live becomes a QUEUED send_message — never a second run (step 6).
	triggerID, err := triggers.CreateTrigger(ctx, org, project, principal, "slack-journey", "manual_api")
	if err != nil {
		t.Fatalf("create slack trigger: %v", err)
	}
	if _, err := triggers.ReviseTrigger(ctx, org, project, triggerID, automation.TriggerRevisionInput{
		DedupeKeyExpr:      `{"select":"slack.event_id"}`,
		CorrelationMode:    "named_session",
		CorrelationKeyExpr: `{"select":"slack.session_id"}`,
	}); err != nil {
		t.Fatalf("revise slack trigger: %v", err)
	}

	// deliver runs the FULL inbound leg for one fake-peer callback: verify the v0 signature over the RAW
	// body STRICTLY before decoding, normalize, correlate the thread to its canonical session, then hand the
	// canonical identity to the durable delivery pipeline.
	deliver := func(t *testing.T, body []byte, retry bool, transport string) (slack.Event, automation.DeliveryResult) {
		t.Helper()
		if transport == "events-api" { // Socket Mode frames are unsigned (the WS token is the auth)
			ts, sig := signedSlackRequest(signingSecret, body, time.Now())
			if err := slack.VerifySignature(signingSecret, ts, sig, body, time.Now(), slack.DefaultTolerance); err != nil {
				t.Fatalf("%s: signature must verify before any decode: %v", transport, err)
			}
		}
		ev, err := slack.MapEvent(body, botUser, retry)
		if err != nil {
			t.Fatalf("%s: MapEvent: %v", transport, err)
		}
		session, _, err := store.CorrelateThreadSession(ctx, org, project, conn.ID, ev.TeamID, ev.ChannelID, ev.ThreadTS, sessionID)
		if err != nil {
			t.Fatalf("%s: correlate thread session: %v", transport, err)
		}
		payload, _ := json.Marshal(map[string]any{"slack": map[string]any{
			"event_id": ev.SourceEventID, "session_id": session, "team_id": ev.TeamID,
			"channel": ev.ChannelID, "thread_ts": ev.ThreadTS, "user": ev.UserID, "kind": string(ev.Kind),
		}})
		res, err := triggers.CreateDelivery(ctx, org, project, principal, triggerID, payload)
		if err != nil {
			t.Fatalf("%s: CreateDelivery: %v", transport, err)
		}
		return ev, res
	}

	// ---- 2. mention / start the assistant thread ---------------------------------------------------
	mentionBody := slackEventBody(team, "Ev63301", "app_mention", mapped, channel, threadTS, "")
	mention, first := deliver(t, mentionBody, false, "events-api")
	if mention.ThreadTS != threadTS || mention.SourceEventID != "Ev63301" {
		t.Fatalf("step 2: normalized identity = %+v, want thread %s / event Ev63301", mention, threadTS)
	}
	if first.State != "run_created" && first.State != "message_queued" {
		t.Fatalf("step 2: delivery state = %q (%s), want the canonical effect", first.State, first.Reason)
	}
	if first.SessionID != sessionID {
		t.Fatalf("step 2: delivery landed in session %q, want the thread's canonical session %q", first.SessionID, sessionID)
	}

	// ---- 3. the SAME source event is redelivered (Slack did not observe the 3s ack) ------------------
	_, duplicate := deliver(t, mentionBody, true, "events-api")
	if duplicate.State != "duplicate" {
		t.Fatalf("step 3: redelivery state = %q, want duplicate — one effect per source event (§63.3)", duplicate.State)
	}
	if duplicate.DuplicateOf != first.ID {
		t.Fatalf("step 3: duplicate links %q, want the canonical original %q (original linkage)", duplicate.DuplicateOf, first.ID)
	}

	// ---- 4. the session started ONCE and streams visible progress to the (fake) peer ---------------
	peer := &fakeSlackPeer{statuses: []int{200}}
	progress, err := slack.PostMessage(ctx, peer, slack.PostRequest{
		MethodURL: "https://slack.invalid/api/chat.postMessage",
		Token:     []byte("xoxb-fake-journey-token"),
		Body:      []byte(`{"channel":"` + channel + `","thread_ts":"` + threadTS + `","text":"working…"}`),
	}, slack.PostOptions{Wait: noWait})
	if err != nil || progress.MessageTS == "" {
		t.Fatalf("step 4: fake-peer progress post = (%+v,%v), want a recorded message ts", progress, err)
	}

	// ---- 5. the web console attaches to the SAME session --------------------------------------------
	// An attach OFFERS its own session; the thread's canonical one wins (SLK-003).
	attached, created, err := store.CorrelateThreadSession(ctx, org, project, conn.ID, team, channel, threadTS, otherSessionID)
	if err != nil || created || attached != sessionID {
		t.Fatalf("step 5: web attach resolved (%q,created=%v,%v), want the SAME canonical session %q", attached, created, err, sessionID)
	}
	if n := threadSessionRows(t, pool, conn.ID, team, channel, threadTS); n != 1 {
		t.Fatalf("step 5: %d thread↔session rows, want exactly 1 canonical session per thread", n)
	}

	// ---- 6. a Slack message arrives while the run is live: QUEUED, not a second run -----------------
	// It arrives over SOCKET MODE — the transport switch must not change correlation identity.
	frame, _ := json.Marshal(map[string]any{
		"type": "events_api", "envelope_id": "env-63306",
		"payload": json.RawMessage(slackEventBody(team, "Ev63306", "message", mapped, channel, "1700000009.000100", threadTS)),
	})
	unwrapped, err := slack.UnwrapSocketFrame(frame)
	if err != nil || unwrapped.Type != "events_api" {
		t.Fatalf("step 6: unwrap socket frame = (%+v,%v)", unwrapped, err)
	}
	queuedEvent, queued := deliver(t, unwrapped.Payload, false, "socket-mode")
	if queuedEvent.ThreadTS != threadTS {
		t.Fatalf("step 6: the socket-mode transport changed the correlation root (%q != %q)", queuedEvent.ThreadTS, threadTS)
	}
	if queued.SessionID != sessionID || queued.RunID != runID {
		t.Fatalf("step 6: queued message landed in (%q,%q), want the live (%q,%q) — no second run", queued.SessionID, queued.RunID, sessionID, runID)
	}
	if runCount := rootRunCount(t, pool, sessionID); runCount != 1 {
		t.Fatalf("step 6: session holds %d runs, want 1 — a queued Slack message never starts a second run", runCount)
	}

	// ---- 7. an interactive EXACT approval is completed by an AUTHORIZED user ------------------------
	pub, err := cs.RequestPublication(ctx, tenant, coordinator.PublicationRequest{
		PublicationID: testID("pub"), ApprovalID: testID("apr"), SessionID: sessionID, RunID: runID,
		Operation: "push_branch", Remote: "git@fake:o/r", Branch: "agent/journey", Base: "main", HeadSHA: "c0ffee",
		IdempotencyKey: repositories.IdempotencyKey(org, project, runID, repositories.OpPushBranch, "git@fake:o/r", "agent/journey", "main", "c0ffee"),
		RequestHash:    repositories.RequestHash(org, project, runID, repositories.OpPushBranch, "git@fake:o/r", "agent/journey", "main", "c0ffee"),
		Display:        "push agent/journey",
	})
	if err != nil {
		t.Fatalf("step 7: request publication: %v", err)
	}

	// 7a. NEGATIVE — a plain message saying "yes" is an EVENT, never an interaction: it authorizes nothing.
	if _, err := slack.MapInteractiveApproval(slackEventBody(team, "Ev63307", "message", mapped, channel, "1700000010.000100", threadTS)); !errors.Is(err, slack.ErrNotApproval) {
		t.Fatalf("step 7a: a chat message must never map to an approval, got err = %v", err)
	}
	// 7b. NEGATIVE — the UNMAPPED user clicks the very same minted button. The mapping carries who clicked;
	// the allow-list read from slack_connections REJECTS them before any command is enqueued (SLK-004).
	unmappedIntent, err := slack.MapInteractiveApproval(interactiveApprovalBody(team, unmapped, slack.ActionApprove, pub.RequestHash))
	if err != nil {
		t.Fatalf("step 7b: map unmapped click: %v", err)
	}
	if policy.ApproverAuthorized(unmappedIntent.UserID) {
		t.Fatalf("step 7b: unmapped user %q passed the allow-list %v — a constrained actor cannot approve", unmapped, policy.AllowedUsers)
	}
	if state := publicationState(t, pool, pub.ID); state != "pending_approval" {
		t.Fatalf("step 7b: publication is %q after the unauthorized click, want still pending_approval", state)
	}

	// 7c. POSITIVE — the MAPPED user's minted button carrying the EXACT request_hash approves.
	intent, err := slack.MapInteractiveApproval(interactiveApprovalBody(team, mapped, slack.ActionApprove, pub.RequestHash))
	if err != nil {
		t.Fatalf("step 7c: map mapped click: %v", err)
	}
	if !policy.ApproverAuthorized(intent.UserID) {
		t.Fatalf("step 7c: mapped user %q failed the allow-list %v", mapped, policy.AllowedUsers)
	}
	if intent.RequestHash != pub.RequestHash || intent.Decision != "approve" {
		t.Fatalf("step 7c: intent = %+v, want the exact request hash + approve", intent)
	}
	approveCmd := coordinator.CommandInput{CommandID: testID("cmd"), Kind: "approve", Payload: []byte(`{"request_hash":"` + intent.RequestHash + `"}`)}
	acc, err := cs.AcceptCommand(ctx, tenant, sessionID, approveCmd)
	if err != nil || acc.State != "queued" {
		t.Fatalf("step 7c: accept approve = (%q,%v), want queued", acc.State, err)
	}
	if _, err := cs.ApplyApprovalDecision(ctx, tenant, sessionID, respID, runID, approveCmd.CommandID, "approve", intent.RequestHash); err != nil {
		t.Fatalf("step 7c: apply approval: %v", err)
	}
	approved, err := cs.ApprovedPublicationsForRun(ctx, tenant, runID)
	if err != nil || len(approved) != 1 || approved[0].ID != pub.ID {
		t.Fatalf("step 7c: approved publications = %+v (%v), want exactly %s", approved, err, pub.ID)
	}

	// ---- 8. the model route changes ----------------------------------------------------------------
	//
	// CEILING, STATED: this is a command-ACCEPTANCE step, and the assertion says exactly that — the command is
	// read BACK off the session's durable command rows (state + payload + the session it landed on), so an
	// accept that dropped the change is caught. It deliberately does NOT assert APPLICATION: a change_config
	// applies at a model-step boundary, which needs a running engine (execution/command_pump). This journey
	// starts no engine, so that is an E08 execution-tier proof living beside that code.
	routeCmd := coordinator.CommandInput{CommandID: testID("cmd"), Kind: "change_config",
		Payload: []byte(`{"model":"journey-model-b"}`)}
	route, err := cs.AcceptCommand(ctx, tenant, sessionID, routeCmd)
	if err != nil {
		t.Fatalf("step 8: accept change_config: %v", err)
	}
	if route.State != "queued" {
		t.Fatalf("step 8: change_config state = %q, want queued", route.State)
	}
	gotSession, gotState, gotPayload := commandRow(t, pool, routeCmd.CommandID)
	if gotSession != sessionID || gotState != "queued" || !strings.Contains(gotPayload, "journey-model-b") {
		t.Fatalf("step 8: the change_config did not read back on the session: (session %q, state %q, payload %s), want (%q, queued, carrying journey-model-b) — an accepted-but-dropped route change is a no-op",
			gotSession, gotState, gotPayload, sessionID)
	}

	// ---- 9. cancellation and follow-up work --------------------------------------------------------
	//
	// A DEFECT THIS ASSERTION CAUGHT: the interrupt was posted as Kind:"interrupt", which is not a command
	// kind at all (§9.2's interrupt is a send_message DELIVERY mode). The coordinator durably REJECTED it with
	// `unsupported_command` on every run, and the old step-9 assertion — "AcceptCommand returned no error" —
	// passed over that rejection. So the interrupt is now the real §9.2 one, and it is read back QUEUED for
	// the pump; a rejection fails here.
	interruptCmd := coordinator.CommandInput{CommandID: testID("cmd"), Kind: "send_message",
		Delivery: "interrupt", Payload: []byte(`{"message":"stop and summarize"}`)}
	if _, err := cs.AcceptCommand(ctx, tenant, sessionID, interruptCmd); err != nil {
		t.Fatalf("step 9: accept the §9.2 interrupt-delivery message: %v", err)
	}
	if gotSession, gotState, _ := commandRow(t, pool, interruptCmd.CommandID); gotSession != sessionID || gotState != "queued" {
		t.Fatalf("step 9: the interrupt did not read back QUEUED on the session: (session %q, state %q), want (%q, queued) — a rejected or dropped interrupt could never reach the run",
			gotSession, gotState, sessionID)
	}

	// The cancellation itself is NOT acceptance-only: CancelRunReconciled is the single production cancel path
	// (CancelResponse routes here, spec §26.10/SES-010) and it drives the run to a terminal without an engine.
	// The journey asserts the run genuinely REACHES `canceled` — a cancel that left the run running would pass
	// an "err == nil" check and fail this one.
	terminal, err := cs.CancelRunReconciled(ctx, tenant, respID, runID, canceledProjectionForJourney(t), canceledProjectionForJourney(t))
	if err != nil {
		t.Fatalf("step 9: cancel the run: %v", err)
	}
	if terminal != "canceled" {
		t.Fatalf("step 9: the run's terminal = %q, want canceled", terminal)
	}
	if state := runState(t, pool, runID); state != "canceled" {
		t.Fatalf("step 9: the run row is %q after the cancel, want canceled — an interrupt/cancel that does not reach a terminal is not cancellation", state)
	}

	// ---- 10. a Slack rate limit / network interruption occurs ---------------------------------------
	// 10a. A 429 repairs the VISIBLE message exactly ONCE, replaying the SAME body (not a duplicate post).
	repairPeer := &fakeSlackPeer{statuses: []int{http.StatusTooManyRequests, 200}, retryAfter: "1"}
	repairBody := []byte(`{"channel":"` + channel + `","ts":"` + progress.MessageTS + `","text":"done"}`)
	repaired, err := slack.PostMessage(ctx, repairPeer, slack.PostRequest{
		MethodURL: "https://slack.invalid/api/chat.update", Token: []byte("xoxb-fake-journey-token"), Body: repairBody,
	}, slack.PostOptions{Wait: noWait})
	if err != nil || !repaired.Repaired || repaired.Attempts != 2 {
		t.Fatalf("step 10a: repair = (%+v,%v), want one repaired post in 2 attempts", repaired, err)
	}
	if len(repairPeer.posts) != 1 {
		t.Fatalf("step 10a: the fake peer accepted %d posts, want 1 — the visible message is REPAIRED, not duplicated", len(repairPeer.posts))
	}
	if repairPeer.bodies[0] != repairPeer.bodies[1] {
		t.Fatal("step 10a: the repair did not replay the SAME canonical body")
	}

	// 10b. A PERSISTENT 429 past the budget is a DELIVERY failure — and the canonical result survives it.
	// This is §63.3's fourth pass criterion: Slack output failing never erases canonical state.
	deadPeer := &fakeSlackPeer{statuses: []int{http.StatusTooManyRequests}, retryAfter: "1"}
	if _, err := slack.PostMessage(ctx, deadPeer, slack.PostRequest{
		MethodURL: "https://slack.invalid/api/chat.update", Token: []byte("xoxb-fake-journey-token"), Body: repairBody,
	}, slack.PostOptions{Wait: noWait}); !errors.Is(err, slack.ErrRateLimited) {
		t.Fatalf("step 10b: persistent 429 err = %v, want ErrRateLimited", err)
	}
	if state := publicationState(t, pool, pub.ID); state != "approved" {
		t.Fatalf("step 10b: the approved publication is %q after the Slack delivery failed — the canonical result must survive", state)
	}
	if got := canonicalDeliveryCount(t, pool, triggerID); got != 2 {
		t.Fatalf("step 10b: %d canonical deliveries, want 2 (the mention + the queued message; the redelivery is a duplicate)", got)
	}

	// ---- the EXIT-gate proof ------------------------------------------------------------------------
	// TerminalSummaryPosts counts what actually happened: the terminal SURFACE was posted exactly once (the
	// repaired chat.update above), never duplicated. It is deliberately NOT called "per delivery" — there are
	// TWO canonical deliveries and one terminal surface, and there is no shipped Slack outbound worker to fan a
	// summary out per delivery id, so the §63.3 fan-out form is named as unproven rather than divided into 1.
	proof := uat.SlackMappingProof{
		Peer:                         "fake",
		TeamID:                       team,
		SessionID:                    sessionID,
		CanonicalSessions:            threadSessionRows(t, pool, conn.ID, team, channel, threadTS),
		SourceEventIDs:               []string{"Ev63301", "Ev63306"},
		DeliveredEvents:              3, // the mention, its redelivery, the socket-mode message
		CanonicalEffects:             canonicalDeliveryCount(t, pool, triggerID),
		PostReceipts:                 append(append([]string{}, peer.posts...), repairPeer.posts...),
		TerminalSummaryPosts:         len(repairPeer.posts),
		RateLimitRepairs:             1,
		UnauthorizedApprovalRejected: true,
		CanonicalResultIntact:        true,
	}
	if !proof.Complete() {
		t.Fatalf("the journey's SlackMappingProof is not COMPLETE: %+v", proof)
	}
	t.Logf("§63.3 journey PASS on a FAKE peer: one canonical session (%d), one effect per source event (%d effects / %d deliveries), unauthorized approval rejected, canonical result intact through a Slack delivery failure. A real workspace receipt is §6 leg 1 — NOT claimed.",
		proof.CanonicalSessions, proof.CanonicalEffects, proof.DeliveredEvents)
	t.Log("§63.3 journey CEILINGS, so no reader has to infer them: (1) the SLK-004 approver allow-list is enforced at the POLICY-PRIMITIVE level — SlackAuthorizationPolicyFor/ApproverAuthorized still have no non-test caller (E19 T2 wires the interactivity route that becomes the first one), so this journey hand-composes the DECISION leg a shipped handler would. Its inbound leg is likewise hand-composed onto the trigger pipeline, which is now a SECOND path rather than the only one: E19 T1 shipped POST /v1/slack/events, admitting through the real Admitter, and its own component-real proofs live in apps/control-plane/internal/store/slack_events_component_test.go; (2) step 8 is command-ACCEPTANCE (read back durable+queued with its payload), NOT application — a boundary config change needs a running engine (E08 execution tier); step 9's cancellation IS asserted to a real terminal through the production CancelRunReconciled path; (3) terminal_summary_posts counts the single non-duplicated terminal surface post, not a per-delivery-id fan-out.")
}

// commandRow reads a durable command back off the session: the session it landed on, its state and its raw
// payload. Steps 8-9 assert the accept was DURABLE rather than trusting the returned state alone — a no-op
// accept that dropped the payload, or a command the coordinator durably REJECTED, fails here.
func commandRow(t *testing.T, pool *pgxpool.Pool, commandID string) (session, state, payload string) {
	t.Helper()
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT session_id, state, payload::text FROM commands WHERE id=$1`, commandID).
		Scan(&session, &state, &payload); err != nil {
		t.Fatalf("read command %s: %v", commandID, err)
	}
	return session, state, payload
}

// runState reads a run's durable state, so step 9's cancel is asserted on the row rather than on a return value.
func runState(t *testing.T, pool *pgxpool.Pool, runID string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id=$1`, runID).Scan(&state); err != nil {
		t.Fatalf("read run %s: %v", runID, err)
	}
	return state
}

// canceledProjectionForJourney is the terminal Response projection the production cancel path hands
// CancelRunReconciled (store.canceledProjection's shape). The journey builds it here rather than importing the
// store package, which would be an import cycle for a component test in this package.
func canceledProjectionForJourney(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"output": []any{}, "usage": map[string]any{}, "model": "",
		"error": map[string]any{"type": "canceled", "title": "run canceled"},
	})
	if err != nil {
		t.Fatalf("marshal canceled projection: %v", err)
	}
	return raw
}

func threadSessionRows(t *testing.T, pool *pgxpool.Pool, connID, team, channel, thread string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM slack_thread_sessions WHERE connection_id=$1 AND team_id=$2 AND channel_id=$3 AND thread_ts=$4`,
		connID, team, channel, thread).Scan(&n); err != nil {
		t.Fatalf("count thread sessions: %v", err)
	}
	return n
}

func rootRunCount(t *testing.T, pool *pgxpool.Pool, sessionID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM runs WHERE session_id=$1`, sessionID).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	return n
}

func canonicalDeliveryCount(t *testing.T, pool *pgxpool.Pool, triggerID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1 AND duplicate_of IS NULL`, triggerID).Scan(&n); err != nil {
		t.Fatalf("count canonical deliveries: %v", err)
	}
	return n
}

func publicationState(t *testing.T, pool *pgxpool.Pool, pubID string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM publications WHERE id=$1`, pubID).Scan(&state); err != nil {
		t.Fatalf("read publication state: %v", err)
	}
	return state
}

func mustSystemExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(storage.WithSystemScope(context.Background()), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
