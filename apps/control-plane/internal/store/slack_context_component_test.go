//go:build component

package store_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// E20 T3 — THE AUTHORITY BOUNDARY, against real PostgreSQL, through the shipped route.
//
// The context tells us what the HUMAN is looking at. The run executes with the CONNECTION PRINCIPAL's
// authority. Those are different parties, and that gap is the whole risk — so all four boundaries get their
// own test, and none of them is a comment:
//
//	(1) no entity selects a TENANT          — TestSlackContextCannotSelectATenant
//	(2) no entity selects a RUN TARGET      — TestSlackContextCannotSelectARunTarget
//	(3) no entity widens allowed_channels   — TestSlackContextCannotWidenTheChannelAllowList
//	(4) no entity becomes a FETCH TARGET    — TestSlackContextNeverBecomesAFetchTarget (+ the AST guard in
//	    extensions/slack_context_guard_test.go, which is the half a behavioural test cannot cover)
//
// CONTRACT for every payload below: https://docs.slack.dev/reference/events/app_context_changed/ (checked
// 2026-07-27) — the context object is {"entities":[{"type","value","team_id"}]}, ordered by relevance —
// plus https://docs.slack.dev/changelog/2026/07/02/app-context/ (checked 2026-07-27), which is what puts the
// SAME object inside message.im and app_home_opened. Built from those pages, not from our mapper.

// ctxEntity is one published context entity.
func ctxEntity(typ, value, teamID string) map[string]any {
	return map[string]any{"type": typ, "value": value, "team_id": teamID}
}

// withContext splices an app_context into an already-built Events API envelope, so the context legs run over
// the SAME event builders (event/eventText/dmEvent) the rest of the Slack suite uses rather than a parallel
// set that could drift.
//
// S19, and it is why this writes `app_context` while panelEvent's app_context_changed writes `context`: no
// Slack page agrees on the name. The 2026-07-02 changelog says `app_context` for message.im and
// app_home_opened; the developing-agents page says the latter calls it `context`; app_context_changed's own
// reference documents `context`; message.im's example payload shows neither. The adapter reads both.
func withContext(t *testing.T, body []byte, entities ...map[string]any) []byte {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode the envelope to splice a context into: %v", err)
	}
	inner, ok := env["event"].(map[string]any)
	if !ok {
		t.Fatalf("envelope carries no inner event: %s", body)
	}
	inner["app_context"] = map[string]any{"entities": entities}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("re-encode the envelope: %v", err)
	}
	return raw
}

// runInput reads the stored prompt of the fixture's single run.
func (f *slackFixture) runInput(t *testing.T) string {
	t.Helper()
	var input string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT input::text FROM responses WHERE organization_id=$1 AND project_id=$2`, f.org, f.project).Scan(&input); err != nil {
		t.Fatalf("read the stored run input: %v", err)
	}
	return input
}

// BOUNDARY 1 — A CONTEXT ENTITY CANNOT SELECT A TENANT.
//
// This is the payload-selectable-tenant proof (TestSlackTenantIsNeverPayloadSelectable) re-run against the
// ONE payload field E20 newly reads. The tenant is STRUCTURAL: Admit builds middleware.Scope from conn.Org
// and conn.Project, which came from the row whose secret signed this body. A context cannot participate
// because it is never consulted — and the entity naming another workspace does not even survive the mapper.
func TestSlackContextCannotSelectATenant(t *testing.T) {
	f := newSlackFixture(t)
	victim, victimProject := newID("org"), newID("prj")
	exec(t, f.pool, `INSERT INTO organizations (id) VALUES ($1)`, victim)
	exec(t, f.pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, victimProject, victim)

	body := withContext(t, f.eventText(t, "EvCtxTenant", "app_mention", "Umapped", "C80", "1700000080.000100", "", "<@"+f.botUser+"> ship it"),
		ctxEntity("slack#/types/channel_id", "C0VICTIM", "T0OTHERWORKSPACE"), // another workspace's channel
		ctxEntity("organization_id", victim, f.team),                         // an entity pretending to be a tenant selector
		ctxEntity("project_id", victimProject, f.team),
	)
	resp := f.deliver(t, body, time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("status = %d, want 2xx — a hostile context is IGNORED, not an error (it is data)", resp.StatusCode)
	}

	var stolen int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM runs WHERE organization_id=$1`, victim).Scan(&stolen); err != nil {
		t.Fatalf("count victim runs: %v", err)
	}
	if stolen != 0 {
		t.Fatalf("%d runs landed in the tenant a CONTEXT ENTITY named; org/project come only from the resolved connection", stolen)
	}
	if n := f.runCount(t); n != 1 {
		t.Fatalf("%d runs in the connection's own tenant, want 1", n)
	}
	// Neither the forged tenant ids nor the FOREIGN workspace's channel may reach the prompt: an entity
	// carries its own team_id, and another workspace's channel name has no business in our prompt.
	input := f.runInput(t)
	for _, leaked := range []string{victim, victimProject, "C0VICTIM", "T0OTHERWORKSPACE", "organization_id"} {
		if strings.Contains(input, leaked) {
			t.Fatalf("the prompt carries %q: %s", leaked, input)
		}
	}
}

// BOUNDARY 2 — A CONTEXT ENTITY CANNOT SELECT A RUN TARGET.
//
// default_policy on the connection row is the only thing that says WHAT runs and AS WHOM (slackRunTarget).
// A context that names a revision and a principal must move neither — including when the names it invents
// would be valid-looking ids.
func TestSlackContextCannotSelectARunTarget(t *testing.T) {
	f := newSlackFixture(t)
	forgedRevision, forgedPrincipal := newID("arev"), newID("prin")

	body := withContext(t, f.eventText(t, "EvCtxTarget", "app_mention", "Umapped", "C80", "1700000080.000100", "", "<@"+f.botUser+"> ship it"),
		ctxEntity("agent_revision_id", forgedRevision, f.team),
		ctxEntity("principal_id", forgedPrincipal, f.team),
		ctxEntity("slack#/types/channel_id", "C0LOOKING", f.team),
	)
	f.deliver(t, body, time.Now(), "", "").Body.Close()

	var revision string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT COALESCE(agent_revision_id,'') FROM runs WHERE organization_id=$1 AND project_id=$2`,
		f.org, f.project).Scan(&revision); err != nil {
		t.Fatalf("read the run's pin: %v", err)
	}
	if revision != f.revision {
		t.Fatalf("run pinned %q, want the connection's %q — default_policy is the only run target", revision, f.revision)
	}
	// The admission's own principal, read off the reservation the run was born under.
	var principal string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT principal_id FROM idempotency_records WHERE organization_id=$1 AND project_id=$2`,
		f.org, f.project).Scan(&principal); err != nil {
		t.Fatalf("read the reservation's principal: %v", err)
	}
	if principal != f.principal {
		t.Fatalf("the run was admitted as %q, want the connection's principal %q", principal, f.principal)
	}
	input := f.runInput(t)
	for _, leaked := range []string{forgedRevision, forgedPrincipal, "agent_revision_id", "principal_id"} {
		if strings.Contains(input, leaked) {
			t.Fatalf("the prompt carries %q: %s — an entity type this app cannot describe contributes NOTHING", leaked, input)
		}
	}
	// The one DOCUMENTED entity beside them IS described: this test would pass vacuously if the context were
	// simply being thrown away, and "we ignore it entirely" is not the claim T3 makes.
	if !strings.Contains(input, "C0LOOKING") {
		t.Fatalf("the prompt = %s, want the documented channel entity described — the boundary is 'described, "+
			"never resolved', not 'discarded'", input)
	}
}

// BOUNDARY 3 — A CONTEXT ENTITY CANNOT WIDEN allowed_channels.
//
// Both directions, because a one-directional proof of a gate is half a proof:
//
//   - An event from OUTSIDE the allow-list whose context names an ALLOWED channel is still refused. This is
//     the attack: "I am looking at #approved" must not admit a message sent from #not-approved.
//   - An event from INSIDE the allow-list whose context names a channel outside it is still ADMITTED. The
//     context is not a gate in either direction — it neither opens nor closes one.
func TestSlackContextCannotWidenTheChannelAllowList(t *testing.T) {
	f := newSlackFixture(t)
	f.scopeToChannels(t, "C40")

	out := f.deliver(t, withContext(t,
		f.eventText(t, "EvCtxScope1", "app_mention", "Umapped", "C41", "1700000041.000100", "", "<@"+f.botUser+"> ship it"),
		ctxEntity("slack#/types/channel_id", "C40", f.team)), time.Now(), "", "")
	defer out.Body.Close()
	if out.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("an event from OUTSIDE allowed_channels whose context names an allowed channel = %d, want 422 "+
			"— a context describes a view; it is not a pass", out.StatusCode)
	}
	if n := f.runCount(t); n != 0 {
		t.Fatalf("%d runs, want 0 — the context must not have carried the event into scope", n)
	}

	in := f.deliver(t, withContext(t,
		f.eventText(t, "EvCtxScope2", "app_mention", "Umapped", "C40", "1700000040.000100", "", "<@"+f.botUser+"> ship it"),
		ctxEntity("slack#/types/channel_id", "C41", f.team)), time.Now(), "", "")
	defer in.Body.Close()
	if in.StatusCode/100 != 2 {
		t.Fatalf("an IN-SCOPE event whose context names an out-of-scope channel = %d, want 2xx — the context is "+
			"not a gate in that direction either", in.StatusCode)
	}
	if n := f.runCount(t); n != 1 {
		t.Fatalf("%d runs, want 1 — an in-scope event must still admit with any context on it", n)
	}
	if input := f.runInput(t); !strings.Contains(input, "C41") {
		t.Fatalf("the prompt = %s, want the out-of-scope channel DESCRIBED — describing where someone is "+
			"looking is exactly what allowed_channels does not govern", input)
	}
}

// BOUNDARY 4 — A CONTEXT ENTITY NEVER BECOMES A FETCH TARGET. The heaviest one.
//
// The bot holds `channels:history`. If a context saying "the user is looking at C_PRIVATE" caused a read of
// C_PRIVATE, the user's VIEW would have granted them the CONNECTION's ACCESS — a confused-deputy read
// primitive, and one that is invisible from the outside because the read looks exactly like the app doing
// its job.
//
// NOT VACUOUS, and that is the whole design of this test: the follower is mounted and the run is driven to
// terminal, so the fake Slack sees REAL outbound traffic (setStatus, startStream, appendStream, stopStream).
// The assertion is not "no calls happened" — it is "calls happened, and not one of them so much as mentions
// the channel the context named". A test that admitted with no outbound seam mounted would pass while a
// resolver sat right next to it.
//
// Its structural half is TestSlackNoCodePathResolvesAContextEntity (extensions): this leg proves today's
// paths fetch nothing, that one proves a resolver cannot appear by accident tomorrow.
func TestSlackContextNeverBecomesAFetchTarget(t *testing.T) {
	f := newSlackFixture(t)
	f.withStreaming(t, 4)
	const private = "C0PRIVATEROOM"

	f.deliver(t, withContext(t,
		f.eventText(t, "EvCtxFetch", "app_mention", "Umapped", "C80", "1700000080.000100", "", "<@"+f.botUser+"> what is left?"),
		ctxEntity("slack#/types/channel_id", private, f.team)), time.Now(), "", "").Body.Close()
	runID, responseID, sessionID := f.runAndResponse(t)

	// The context WAS described — otherwise the zero-fetch claim below would be about a context nobody read.
	if input := f.runInput(t); !strings.Contains(input, private) {
		t.Fatalf("the prompt = %s, want the context channel described; a discarded context proves nothing", input)
	}

	// Drive the run all the way through the outbound seam, so there is traffic to inspect.
	f.terminate(t, runID, statemachines.RunCmdProvision, statemachines.RunCmdStart)
	f.commitStep(t, sessionID, responseID, runID)
	f.awaitCalls(t, "/chat.startStream", 1)
	f.finalizeWith(t, responseID, "completed", map[string]any{
		"output": []any{map[string]any{"type": "message", "content": "Two items are left."}},
	})
	f.terminate(t, runID, statemachines.RunCmdComplete)
	// The reply pump is what CLOSES the stream (E20 T1), so the terminal leg of the outbound seam only runs
	// when it ticks. Without it this test would await a call nobody makes.
	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(context.Background()); err != nil || posted != 1 {
		t.Fatalf("the reply pump delivered %d answers (err %v), want 1", posted, err)
	}
	f.awaitCalls(t, "/chat.stopStream", 1)

	calls := f.slackCalls()
	if len(calls) < 3 {
		t.Fatalf("fake Slack saw %d calls; with the follower mounted and a run driven to terminal there must be "+
			"real traffic, or the zero-fetch assertion below is vacuous: %v", len(calls), calls)
	}
	for _, c := range calls {
		// No read method, at all. The app's whole outbound vocabulary is chat.* and assistant.threads.*.
		if strings.HasPrefix(c.path, "/conversations.") || strings.HasPrefix(c.path, "/search.") ||
			strings.HasPrefix(c.path, "/canvases.") || strings.HasPrefix(c.path, "/files.") {
			t.Fatalf("the stack called %s — a context entity became a fetch target", c.path)
		}
		// And nothing addressed the context's channel: every call belongs to the thread the human wrote in.
		if strings.Contains(c.path, private) || strings.Contains(c.body, private) {
			t.Fatalf("a call to %s carries the context channel %q: %s — the context describes what the USER "+
				"sees, while this call carries the CONNECTION's authority", c.path, private, c.body)
		}
	}
}

// The panel's refresh signal stays a refresh signal. app_context_changed births no run (E20 T2's no-run
// path), and T3 does not give it one: a context arriving on its own is not a turn in a conversation, and a
// surface that could birth a run from "the user looked at something" would be a run nobody asked for.
func TestSlackContextChangedStillBirthsNoRun(t *testing.T) {
	f := newSlackFixture(t)

	resp := f.deliver(t, f.panelEvent("EvCtxChanged", map[string]any{
		"type": "app_context_changed",
		"context": map[string]any{"entities": []any{
			ctxEntity("slack#/types/channel_id", "C0LOOKING", f.team),
		}},
	}), time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("app_context_changed = %d, want a 2xx ack — it is handled, it simply births nothing", resp.StatusCode)
	}
	if n := f.runCount(t); n != 0 {
		t.Fatalf("app_context_changed birthed %d runs, want 0", n)
	}
	if n := len(f.slackCalls()); n != 0 {
		t.Fatalf("app_context_changed made %d outbound Slack calls, want 0", n)
	}
}
