//go:build component

package store_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/storage"
)

// slack_connections.allowed_channels, ENFORCED — E19 T2 follow-up. Until this file existed the column was
// written by the registration API, stored as JSONB and parsed back into SlackAuthorizationPolicy, and then
// read by NOBODY: ApproverAuthorized consulted AllowedUsers alone. An operator who scoped their bot to one
// channel got no scoping at all, and nothing anywhere said so.
//
// That is the shape of failure this repo has now shipped four times in one epic (T4's push CRUD, T5's remote
// delegation, the APV-001 approve branch), so the fix is not "add the check" — it is "add the check AND the
// test that fails without it", in both directions, on both paths a channel reaches.

// scopeToChannels narrows the fixture connection's allow-list, the way an operator narrowing an ALREADY
// REGISTERED binding does. Written as an UPDATE on purpose: the registration write-path is proven elsewhere,
// and what is under test here is ENFORCEMENT — including enforcement against a thread that was correlated
// while the channel was still in scope (see the click test below).
func (f *slackFixture) scopeToChannels(t *testing.T, channels ...string) {
	t.Helper()
	list := "[]"
	if len(channels) > 0 {
		list = `["` + channels[0] + `"`
		for _, c := range channels[1:] {
			list += `,"` + c + `"`
		}
		list += `]`
	}
	exec(t, f.pool, `UPDATE slack_connections SET allowed_channels=$1::jsonb
	                  WHERE organization_id=$2 AND project_id=$3`, list, f.org, f.project)
}

// TestSlackChannelAllowListRefusesAnEventOutsideIt is the guarantee the dead field claimed and did not have:
// with a NON-EMPTY allow-list, an event from a channel outside it births nothing.
//
// The refusal is TERMINAL (422 + the suppress header), and that classification is the point rather than a
// detail: no redelivery can move a channel into the connection's allow-list, so three more attempts would
// produce three more identical refusals. It is the same verdict slackAdmitRejection already gives a draft
// revision pin — configuration, not load.
func TestSlackChannelAllowListRefusesAnEventOutsideIt(t *testing.T) {
	f := newSlackFixture(t)
	f.scopeToChannels(t, "C40")

	// In scope: admits exactly as it always did.
	inScope := f.deliver(t, f.event("EvChan1", "app_mention", "Umapped", "C40", "1700000040.000100", ""), time.Now(), "", "")
	if inScope.StatusCode/100 != 2 {
		t.Fatalf("an event in an ALLOW-LISTED channel = %d, want a 2xx ack — the allow-list must scope, not disable", inScope.StatusCode)
	}
	inScope.Body.Close()
	if n := f.runCount(t); n != 1 {
		t.Fatalf("the in-scope event birthed %d runs, want 1", n)
	}

	// Out of scope: nothing. Not a run, not a session, not a reservation.
	out := f.deliver(t, f.event("EvChan2", "app_mention", "Umapped", "C41", "1700000041.000100", ""), time.Now(), "", "")
	defer out.Body.Close()
	if out.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("an event from a channel OUTSIDE allowed_channels = %d, want 422 — the scope an operator configured must actually hold", out.StatusCode)
	}
	if got := out.Header.Get("X-Slack-No-Retry"); got != "1" {
		t.Fatalf("the out-of-scope refusal answered with X-Slack-No-Retry=%q, want \"1\" — no redelivery can put a channel into the allow-list, so Slack must not pull it three more times", got)
	}
	if n := f.runCount(t); n != 1 {
		t.Fatalf("%d runs after an out-of-scope event, want still 1 — a channel outside the allow-list must birth NOTHING", n)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("%d thread↔session rows, want still 1 — a refused channel must not correlate a thread either", n)
	}
	var reservations int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM idempotency_records WHERE organization_id=$1 AND project_id=$2`, f.org, f.project).Scan(&reservations); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	if reservations != 1 {
		t.Fatalf("%d idempotency reservations, want 1 — the out-of-scope event must be refused BEFORE the reservation, not after", reservations)
	}
}

// TestSlackEmptyAllowListsMeanOppositeThingsOnPurpose pins the asymmetry in ONE place, because two allow-lists
// on one type whose emptiness means opposite things is a trap unless it is deliberate, justified and tested.
//
// EMPTY allowed_channels ⇒ EVERY channel. EMPTY allowed_users ⇒ NO user.
//
// WHY THAT IS NOT ARBITRARY: the two lists sit in front of different boundaries.
//
//   - allowed_channels NARROWS a gate that already exists. Slack only delivers events from conversations the
//     bot was invited to, so the unconfigured state is already scoped by the workspace admin who did the
//     inviting. Empty meaning "nowhere" would make every freshly registered connection inert — silently, which
//     is the exact failure this file exists to close.
//   - allowed_users has NOTHING behind it. It is the only thing standing between "any member of the workspace"
//     and authorizing a privileged operation, so its unconfigured state must be deny.
//
// The 000035 migration already committed to this reading in the column comment ("empty = no channel
// restriction"); until now no code honoured it either way.
func TestSlackEmptyAllowListsMeanOppositeThingsOnPurpose(t *testing.T) {
	f := newSlackFixture(t) // registered with NO allowed_channels and allowed_users:["Umapped"]
	f.scopeToChannels(t)    // explicitly empty, the fresh-registration state

	// Channels: empty ⇒ every channel. An arbitrary channel nobody enumerated is admitted.
	resp := f.deliver(t, f.event("EvChan3", "app_mention", "Umapped", "C_NEVER_ENUMERATED", "1700000042.000100", ""), time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("with an EMPTY allowed_channels an arbitrary channel = %d, want a 2xx ack — empty must mean 'no channel restriction' (000035), or every fresh connection is silently inert", resp.StatusCode)
	}
	if n := f.runCount(t); n != 1 {
		t.Fatalf("an empty channel allow-list birthed %d runs, want 1", n)
	}

	// Users: empty ⇒ nobody. The SAME connection, its approver list emptied, refuses the approver it just had.
	ext := extensions.New(f.pool)
	connID := f.connRef(t).ID
	policy, err := ext.SlackAuthorizationPolicyFor(context.Background(), f.org, f.project, connID)
	if err != nil {
		t.Fatalf("read the policy: %v", err)
	}
	if !policy.ApproverAuthorized("Umapped") {
		t.Fatalf("the seeded approver is not authorized; the fixture, not the asymmetry, is broken")
	}
	exec(t, f.pool, `UPDATE slack_connections SET allowed_users='[]'::jsonb WHERE organization_id=$1 AND project_id=$2`, f.org, f.project)
	emptied, err := ext.SlackAuthorizationPolicyFor(context.Background(), f.org, f.project, connID)
	if err != nil {
		t.Fatalf("re-read the policy: %v", err)
	}
	if emptied.ApproverAuthorized("Umapped") {
		t.Fatal("an EMPTY allowed_users authorized a user — deny-by-default is the only safe reading for the list with no gate behind it")
	}
	if !emptied.ChannelAllowed("C_ANY", false) {
		t.Fatal("an EMPTY allowed_channels refused a channel — the two lists must keep their DIFFERENT emptiness meanings, each justified by what sits behind it")
	}
}

// TestSlackChannelAllowListRefusesAClickOutsideIt is the second path a channel reaches, and the reason
// enforcing at admission alone is not enough.
//
// The transitive argument is tempting and WRONG: "a click can only land in a thread we correlated, and we only
// correlate in allowed channels, so Decide is covered." It fails on the ordering that matters most — an
// operator NARROWING the allow-list to contain an incident. The thread was correlated while its channel was in
// scope; removing the channel has to take the in-flight threads with it, or the narrowing an operator performs
// during an incident quietly excludes exactly the conversations they were trying to cut off.
func TestSlackChannelAllowListRefusesAClickOutsideIt(t *testing.T) {
	f := newSlackFixture(t)
	thread := f.seedApproval(t, "C42", "1700000043.000100")

	// The operator narrows the scope AFTER the thread was correlated and the approval posted.
	f.scopeToChannels(t, "C43")

	resp := f.click(t, "Umapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an out-of-scope click = %d, want 200 with nothing done — the refusal is recorded control-plane-side, not read off the Slack UI", resp.StatusCode)
	}
	if n := f.commandCount(t, ""); n != 0 {
		t.Fatalf("a click in a channel OUTSIDE allowed_channels enqueued %d commands, want 0 — narrowing the allow-list must take the in-flight threads with it", n)
	}
	if state := f.publicationState(t, thread.publicationID); state != "pending_approval" {
		t.Fatalf("the publication is %q after an out-of-scope click, want still pending_approval", state)
	}
	if n := len(f.slackCalls()); n != 0 {
		t.Fatalf("an out-of-scope click made %d outbound Slack calls, want 0", n)
	}

	// Widen it again and the SAME click decides — so the refusal was about the CHANNEL, not a broken fixture.
	f.scopeToChannels(t, "C43", thread.channel)
	ok := f.click(t, "Umapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	defer ok.Body.Close()
	if state := f.publicationState(t, thread.publicationID); state != "approved" {
		t.Fatalf("with the channel back in scope the click left the publication %q, want approved — the allow-list must discriminate, not refuse everything", state)
	}
}

// ---- E20 T2: the agent panel's DM, and the surface events that are not conversation --------------------

// dmEvent builds a message.im envelope — the agent panel's conversation.
//
// CONTRACT: https://docs.slack.dev/reference/events/message.im/ (checked 2026-07-27). The example payload is
// {"type":"event_callback","team_id":…,"event_id":…,"event":{"type":"message","channel":"D024BE91L",
// "user":…,"text":…,"ts":…,"event_ts":…,"channel_type":"im"}} and the event requires the `im:history` scope.
// Built from THAT page rather than from our own mapper, so a drift in MapEvent cannot drag the fixture along.
func (f *slackFixture) dmEvent(eventID, user, channel, ts, text string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"type": "event_callback", "team_id": f.team, "api_app_id": "A0001",
		"event_id": eventID, "event_time": 1700000000,
		"event": map[string]any{
			"type": "message", "channel": channel, "user": user, "text": text,
			"ts": ts, "event_ts": ts, "channel_type": "im",
		},
	})
	return raw
}

// panelEvent builds one of the agent panel's SURFACE events.
//
// CONTRACT: https://docs.slack.dev/reference/events/app_home_opened/ (checked 2026-07-27) —
// {"type":"app_home_opened","user":…,"channel":"D…","event_ts":…,"tab":"home"|"messages"}, no scopes required.
// https://docs.slack.dev/reference/events/app_context_changed/ (checked 2026-07-27) —
// {"type":"app_context_changed","context":{"entities":[{type,value,team_id}]}}, and an empty context object
// when there are no entities. Neither carries a message.
func (f *slackFixture) panelEvent(eventID string, inner map[string]any) []byte {
	raw, _ := json.Marshal(map[string]any{
		"type": "event_callback", "team_id": f.team, "api_app_id": "A0001",
		"event_id": eventID, "event_time": 1700000000, "event": inner,
	})
	return raw
}

// TestSlackDMIsExemptFromTheConfiguredChannelAllowList is E20 T2's core security decision, proved end to end
// against real Postgres through the shipped route — and it is a WIDENING, so all three of its legs run in one
// test rather than in three that could drift apart.
//
// THE DECISION: allowed_channels governs CHANNEL conversations; a DM (Slack's own channel_type == "im") is
// EXEMPT, because a DM's scope is Slack's invitation model — whoever can DM the app is already a member of
// the workspace it was installed into. Before this, a non-empty allowed_channels refused every DM, so the
// agent panel died silently on any install that had ever narrowed its channel scope.
//
// THE THREE LEGS, and the third is the one that keeps the widening from becoming a privilege grant:
//
//	(1) a non-empty allowed_channels no longer refuses a DM;
//	(2) that SAME list still refuses a CHANNEL event — the exemption is about DMs, not about the list;
//	(3) the DM run's principal is still the CONNECTION's, never the human who opened the DM.
func TestSlackDMIsExemptFromTheConfiguredChannelAllowList(t *testing.T) {
	f := newSlackFixture(t)
	f.scopeToChannels(t, "C40") // an operator who has narrowed their scope: the state that used to kill the panel

	// (1) The panel's DM is admitted despite being nowhere in the list.
	const dmChannel, dmOpener = "D024BE91L", "Uoutsider"
	dm := f.deliver(t, f.dmEvent("EvDM1", dmOpener, dmChannel, "1700000050.000100", "ship the release notes"), time.Now(), "", "")
	if dm.StatusCode/100 != 2 {
		t.Fatalf("a panel DM under allowed_channels=[C40] = %d, want a 2xx ack — a DM is scoped by Slack's invitation model, not by the operator's channel list", dm.StatusCode)
	}
	dm.Body.Close()
	if n := f.runCount(t); n != 1 {
		t.Fatalf("the panel DM birthed %d runs, want 1", n)
	}

	// (2) The same list still refuses a CHANNEL event. The exemption must discriminate, or it has repealed
	// allowed_channels rather than carved a DM out of it.
	out := f.deliver(t, f.event("EvDM2", "app_mention", "Umapped", "C41", "1700000051.000100", ""), time.Now(), "", "")
	defer out.Body.Close()
	if out.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a CHANNEL event outside allowed_channels = %d, want 422 — the DM exemption must not widen the list itself", out.StatusCode)
	}
	if n := f.runCount(t); n != 1 {
		t.Fatalf("%d runs after the out-of-scope channel event, want still 1", n)
	}

	// (3) The DM run carries NO extra authority. Its principal and pinned revision are the CONNECTION's — the
	// same two the channel path gets — and the Slack user who opened the DM is nowhere near either.
	var revision, principal, input string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT COALESCE(r.agent_revision_id,''), COALESCE(i.principal_id,''), COALESCE(resp.input::text,'')
		   FROM runs r
		   JOIN idempotency_records i
		     ON i.organization_id = r.organization_id AND i.project_id = r.project_id
		   JOIN responses resp
		     ON resp.organization_id = r.organization_id AND resp.project_id = r.project_id
		  WHERE r.organization_id=$1 AND r.project_id=$2`, f.org, f.project).
		Scan(&revision, &principal, &input); err != nil {
		t.Fatalf("read the DM-born run: %v", err)
	}
	if principal != f.principal || revision != f.revision {
		t.Fatalf("the DM run ran as principal %q revision %q, want the CONNECTION's %q/%q — a DM must not choose who it runs as",
			principal, revision, f.principal, f.revision)
	}
	if principal == dmOpener || strings.Contains(input, dmOpener) {
		t.Fatalf("the DM opener %q reached the run's identity or its prompt (principal=%q input=%q) — a Slack user is data, never authority (SLK-004)",
			dmOpener, principal, input)
	}
	if !strings.Contains(input, "ship the release notes") {
		t.Fatalf("the DM run's input = %q, want the human's message", input)
	}
}

// TestSlackPanelSurfaceEventsBirthNoRun is the RED-first half of the panel: app_home_opened and
// app_context_changed are things a human LOOKED AT, not things they said. Before E20 T2 they classified as
// KindOther and mapped cleanly, so subscribing to them — which the agent panel REQUIRES — would have birthed
// a run with an empty prompt every time somebody opened the panel.
//
// Both tabs are asserted: tab=="home" is App Home's own surface, tab=="messages" is the agent panel, and
// NEITHER is conversation. The delivery is still acked (2xx) — an unacknowledged event is one Slack redelivers
// forever.
func TestSlackPanelSurfaceEventsBirthNoRun(t *testing.T) {
	f := newSlackFixture(t)
	for _, tc := range []struct {
		name  string
		inner map[string]any
	}{
		{"app_home_opened messages tab", map[string]any{
			"type": "app_home_opened", "user": "U9", "channel": "D42", "event_ts": "1515449522.000016", "tab": "messages"}},
		{"app_home_opened home tab", map[string]any{
			"type": "app_home_opened", "user": "U9", "channel": "D42", "event_ts": "1515449522.000016", "tab": "home",
			"view": map[string]any{"id": "V1", "type": "home"}}},
		{"app_context_changed", map[string]any{
			"type": "app_context_changed", "context": map[string]any{"entities": []any{
				map[string]any{"type": "slack#/types/channel_id", "value": "C01234ABDCE", "team_id": "T0ABCDE6543"}}}}},
		{"app_context_changed with an empty context", map[string]any{
			"type": "app_context_changed", "context": map[string]any{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.deliver(t, f.panelEvent("EvPanel-"+tc.name, tc.inner), time.Now(), "", "")
			defer resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				t.Fatalf("a panel surface event = %d, want a 2xx ack — it was delivered, it is simply not a turn in a conversation", resp.StatusCode)
			}
		})
	}
	if n := f.runCount(t); n != 0 {
		t.Fatalf("the panel surface events birthed %d runs, want 0 — opening a panel is not asking for anything", n)
	}
	if n := f.sessionCount(t); n != 0 {
		t.Fatalf("the panel surface events correlated %d threads, want 0", n)
	}
	var reservations int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM idempotency_records WHERE organization_id=$1 AND project_id=$2`, f.org, f.project).Scan(&reservations); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	if reservations != 0 {
		t.Fatalf("the panel surface events took %d idempotency reservations, want 0 — they are refused BEFORE admission, in the mapper", reservations)
	}
}
