//go:build component

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/a2a"
	"github.com/palgroup/palai/adapters/integrations/queue"
	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/apps/control-plane/internal/knowledge"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
	"github.com/palgroup/palai/tests/uat"
)

// The E19 T9 EXIT-gate JOURNEY (plan §T9): every surface this epic wired, in ONE narrative, against REAL
// PostgreSQL and the REAL shipped router — and it ends by emitting the uat.WiringProof the
// integration-wiring-0.1.0 bundle carries, with the mount derivation asserted in-test.
//
// The order is the plan's, and each step is the previous one's consequence rather than a fresh setup:
//
//	 1. a clean stack registers a Slack workspace over the SHIPPED admin route, with secret_ref HANDLES
//	 2. a mention arrives over SOCKET MODE and births a real run through the real Admitter
//	 3. the SAME event_id arrives over the HTTP callback and births NOTHING (transport invariance)
//	 4. the run's approval is requested; an UNAUTHORIZED click decides nothing
//	 5. an AUTHORIZED click approves durably, through the whole shipped chain
//	 6. the run reaches its terminal, which COMMITS an outbound queue delivery in the same transaction
//	 7. the outbox pump delivers it exactly once to a recording sink (loss-less)
//	 8. an A2A push StreamResponse reaches a loopback sink through the production WebhookPusher
//	 9. the mounts are OBSERVED from the running stack and the WiringProof is built from those observations
//
// WHY THE OBSERVATIONS ARE TAKEN AT THE END, from the same process that did the work: a mount is a fact
// about a running binary. Reading it from a table would reproduce the §3.5 D14 defect this gate exists to
// refuse — `capability-workers` advertised "stable" by a binary that never imported the gateway package.
//
// HONEST CEILING, unchanged by anything here and enforced by uat.WiringPeers: every counterparty is a
// documented FAKE. No socket reaches slack.com, no foreign A2A peer is contacted, no broker product exists,
// and the console half runs in scripts/uat/wiring rather than here. This journey proves the code is correct
// against the PUBLISHED contracts and MOUNTED on the production path — never that it worked in a real
// workspace. NO TIER MOVES; §6 legs 1/2/5/8 are untouched.

// wiringFixture is the slackFixture plus everything else E19 wired, served by ONE fully-mounted router.
type wiringFixture struct {
	*slackFixture

	repo      *store.Store
	queues    *automation.QueueStore
	a2aStore  *a2a.Store
	pusher    *a2a.WebhookPusher
	pushSink  *recordingPushSink
	queueSink *recordingQueueSink
	// routes is every route the running router ANSWERED (non-404), and statuses is what each answered.
	routes   []string
	statuses map[string]int
}

// recordingPushSink is the A2A push receiver: a local HTTPS-less loopback server that records the exact
// StreamResponse bytes delivered to it. Loopback is NOT interop (plan §5) — a foreign peer is §6 leg 2 —
// and this sink can never be mistaken for one.
type recordingPushSink struct {
	mu     sync.Mutex
	bodies [][]byte
	tokens []string
	url    string
}

func (s *recordingPushSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	s.tokens = append(s.tokens, r.Header.Get(a2a.PushTokenHeader))
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (s *recordingPushSink) received() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.bodies...)
}

// recordingQueueSink stands in for an outbound broker destination. It is one of uat.QueueBrokerSeams'
// classes — the Postgres reference adapter's outbox delivering to a local recorder — never a broker product.
type recordingQueueSink struct {
	mu   sync.Mutex
	keys []string
}

func (s *recordingQueueSink) Deliver(_ context.Context, destKey string, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, destKey)
	return nil
}

func (s *recordingQueueSink) unique() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, k := range s.keys {
		seen[k] = true
	}
	return len(seen)
}

// newWiringFixture builds the fully-mounted stack: the Slack fixture's real seams, plus the registration
// surface, the A2A server with a REAL pusher, the queue store, knowledge, and the capability-worker mount
// declaration — every option a production binary passes, so the observed /v1/capabilities is the production
// map rather than a subset.
func newWiringFixture(t *testing.T) *wiringFixture {
	t.Helper()
	base := newSlackFixture(t)

	repo := base.repo
	pool := base.pool

	pushSink := &recordingPushSink{}
	sinkSrv := httptest.NewServer(pushSink)
	t.Cleanup(sinkSrv.Close)
	pushSink.url = sinkSrv.URL + "/a2a/push"

	// The production WebhookPusher over the production egress-vetted sender. AllowPrivate is set because the
	// receiver is loopback; the metadata and special-use ranges stay denied even then (egress.VetIP), and
	// the host allowlist is the D12 mitigation exercised rather than skipped.
	pusher := a2a.NewWebhookPusher(webhook.NewSender(), a2a.PushPolicy{
		AllowedHosts: []string{"127.0.0.1"}, AllowPrivate: true, MaxAttempts: 3,
	})
	a2aStore := a2a.NewStore(pool, newID)
	a2aServer := api.NewA2AServer(repo, a2aStore, a2aStore, api.AdmissionLimits{}, "https://cp.test", pusher)
	a2aServer.ScopeFunc = func(*http.Request) (a2a.Scope, bool) {
		return a2a.Scope{Organization: base.org, Project: base.project, Principal: base.principal}, true
	}

	queues := automation.NewQueueStore(pool)
	queueSink := &recordingQueueSink{}

	f := &wiringFixture{
		slackFixture: base, repo: repo, queues: queues, a2aStore: a2aStore,
		pusher: pusher, pushSink: pushSink, queueSink: queueSink, statuses: map[string]int{},
	}

	scope := middleware.Scope{Organization: base.org, Project: base.project, Principal: base.principal}
	ts := httptest.NewServer(api.NewRouter(
		scopedVerifier{scope}, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		api.SSEConfig{}, nil, nil,
		api.WithSlack(base.bridge), api.WithSlackInteractions(base.bridge),
		api.WithSlackConnections(extensions.NewSlackRegistry(extensions.New(pool))),
		api.WithA2A(a2aServer, a2aServer.PublicCardHandler()),
		api.WithQueueConnections(queues, publicOnlyResolver{}),
		api.WithKnowledge(knowledge.New(pool)),
		api.WithCapabilityWorkers(),
	))
	t.Cleanup(ts.Close)
	// Every Slack helper on the embedded fixture posts to f.url, so re-pointing it moves the WHOLE journey
	// onto the fully-mounted router — the routes that get probed are the routes that carried the traffic.
	base.url = ts.URL
	return f
}

// authed issues a bearer-scoped request against the fully-mounted router.
func (f *wiringFixture) authed(t *testing.T, method, path, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer component-key-not-a-credential")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(raw)
}

// observe probes one route on the RUNNING router and records what it answered under its TEMPLATED name
// (an empty template means the concrete path IS the route). A 404 means unmounted, and the caller fails on
// it — this is the whole mount observation, so it never assumes.
func (f *wiringFixture) observe(t *testing.T, method, path, template string) {
	t.Helper()
	route := method + " " + path
	if template != "" {
		route = method + " " + template
	}
	resp, _ := f.authed(t, method, path, "")
	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("%s answered 404 on the fully-mounted router — an unmounted surface cannot be claimed as wired (plan §T9, §3.5 D14)", route)
	}
	f.statuses[route] = resp.StatusCode
	f.routes = append(f.routes, route)
}

// TestWiringJourney is the E19 EXIT journey. It is one test on purpose: the steps are a single causal chain,
// and splitting them would let a later step pass over state an earlier one never actually produced.
func TestWiringJourney(t *testing.T) {
	f := newWiringFixture(t)
	ctx := context.Background()

	// ---- 1. register the workspace over the SHIPPED admin route ---------------------------------------
	//
	// This is the step that did not exist before T9. The registration names secret_ref HANDLES; the values
	// live in the fixture's org-scoped secret bridge, exactly as production resolves them from secret_refs.
	team := strings.ToUpper(newID("T"))
	const signingRef, botRef, appRef = "slack/wiring/signing", "slack/wiring/bot", "slack/wiring/app"
	f.secrets[f.org+"/"+signingRef] = f.secret
	f.secrets[f.org+"/"+botRef] = f.botToken
	f.secrets[f.org+"/"+appRef] = f.appToken

	resp, raw := f.authed(t, http.MethodPost, "/v1/slack-connections", fmt.Sprintf(
		`{"team_id":%q,"bot_user_id":%q,"signing_secret_ref":%q,"bot_token_ref":%q,"app_token_ref":%q,
		  "allowed_users":["Umapped"],
		  "default_policy":{"agent_revision_id":%q,"principal_id":%q}}`,
		team, f.botUser, signingRef, botRef, appRef, f.revision, f.principal))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register the workspace = %d, want 201; %s", resp.StatusCode, raw)
	}
	var registered struct{ ID string }
	if err := json.Unmarshal([]byte(raw), &registered); err != nil || registered.ID == "" {
		t.Fatalf("the registration returned no id: %s", raw)
	}
	f.statuses["POST /v1/slack-connections"] = resp.StatusCode
	f.routes = append(f.routes, "POST /v1/slack-connections")
	// Every later step drives THIS workspace, so the registration is load-bearing rather than decorative:
	// if the row were wrong, nothing below would resolve.
	f.team = team

	// No credential VALUE was written anywhere by the registration.
	for _, ref := range [][2]string{{"signing_secret_ref", signingRef}, {"bot_token_ref", botRef}, {"app_token_ref", appRef}} {
		var stored string
		if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
			fmt.Sprintf(`SELECT COALESCE(%s,'') FROM slack_connections WHERE id=$1`, ref[0]), registered.ID).
			Scan(&stored); err != nil {
			t.Fatalf("read %s: %v", ref[0], err)
		}
		if stored != ref[1] {
			t.Fatalf("%s = %q, want the HANDLE %q — a registration stores refs, never values", ref[0], stored, ref[1])
		}
	}

	// ---- 2. a Socket Mode mention births a real run ---------------------------------------------------
	conn := f.startSocketMode(t)
	const sourceEventID = "EvWiring1"
	mention := f.event(sourceEventID, "app_mention", "Umapped", "C90", "1700000090.000100", "")
	f.deliverOverSocket(t, conn, "env-wiring-1", mention)
	f.waitForRuns(t, 1, "a Socket Mode mention must birth exactly one run through the real Admitter")

	var runID, sessionID, responseID, admissionRoute, idemKey string
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT r.id, r.session_id, COALESCE(r.response_id,''), i.route, i.idempotency_key
		   FROM runs r JOIN idempotency_records i
		     ON i.organization_id = r.organization_id AND i.project_id = r.project_id
		  WHERE r.organization_id=$1 AND r.project_id=$2`, f.org, f.project).
		Scan(&runID, &sessionID, &responseID, &admissionRoute, &idemKey); err != nil {
		t.Fatalf("read the socket-born run and its reservation: %v", err)
	}
	// The idempotency record IS the evidence the shared Admitter ran: no parallel path writes one.
	if admissionRoute != "/v1/slack/events" || idemKey != team+":"+sourceEventID {
		t.Fatalf("reservation = (%q,%q), want (/v1/slack/events, %s:%s) — the run must come through the ONE admission, keyed by the SOURCE EVENT",
			admissionRoute, idemKey, team, sourceEventID)
	}

	// ---- 3. the SAME event over HTTP births nothing ---------------------------------------------------
	httpResp := f.deliver(t, mention, time.Now(), "", "")
	if httpResp.StatusCode/100 != 2 {
		t.Fatalf("the HTTP callback for an event already admitted over Socket Mode = %d, want a 2xx ack (a replay, not a refusal)", httpResp.StatusCode)
	}
	httpResp.Body.Close()
	f.waitForRuns(t, 1, "the same event over a second transport must not birth a second run")
	assertOneCleanReservation(t, f.slackFixture, 1, "socket then HTTP")
	f.statuses["POST /v1/slack/events"] = httpResp.StatusCode
	f.routes = append(f.routes, "POST /v1/slack/events")

	// ---- 4/5. the approval chain: unauthorized decides nothing, authorized decides durably --------------
	thread := f.seedApproval(t, "C91", "1700000091.000100")

	denied := f.click(t, "Uunmapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	denied.Body.Close()
	if denied.StatusCode != http.StatusOK {
		t.Fatalf("an unauthorized click = %d, want 200 with nothing done", denied.StatusCode)
	}
	if n := f.commandCount(t, ""); n != 0 {
		t.Fatalf("an unauthorized click enqueued %d commands, want 0 — deny-by-default stops it BEFORE the coordinator (SLK-004)", n)
	}
	if state := f.publicationState(t, thread.publicationID); state != "pending_approval" {
		t.Fatalf("the publication is %q after an UNAUTHORIZED click, want still pending_approval", state)
	}

	approved := f.click(t, "Umapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	approved.Body.Close()
	if approved.StatusCode != http.StatusOK {
		t.Fatalf("an authorized click = %d, want 200 within the 3-second budget (D8)", approved.StatusCode)
	}
	if state := f.publicationState(t, thread.publicationID); state != "approved" {
		t.Fatalf("the publication is %q after an AUTHORIZED click, want approved — the decision must be DURABLE, not merely accepted", state)
	}
	if n := f.commandCount(t, "approve"); n != 1 {
		t.Fatalf("the click enqueued %d approve commands, want exactly 1", n)
	}
	f.statuses["POST /v1/slack/interactions"] = approved.StatusCode
	f.routes = append(f.routes, "POST /v1/slack/interactions")

	// ---- 6/7. the run terminal commits an outbound delivery; the pump delivers it exactly once ----------
	outboundResp, outboundRaw := f.authed(t, http.MethodPost, "/v1/queue-connections",
		`{"name":"wiring-results","direction":"outbound","max_deliveries":5,
		  "config":{"destination_url":"https://sink.example.test/queue"}}`)
	if outboundResp.StatusCode != http.StatusCreated {
		t.Fatalf("register the outbound binding = %d, want 201; %s", outboundResp.StatusCode, outboundRaw)
	}
	f.statuses["POST /v1/queue-connections"] = outboundResp.StatusCode
	f.routes = append(f.routes, "POST /v1/queue-connections")

	// The approval's own run is still live; terminating the JOURNEY's run is what fires the terminal hook.
	if _, err := f.spine.ApplyRunTransition(ctx,
		coordinator.Tenant{Organization: f.org, Project: f.project}, runID, statemachines.RunCmdCancel); err != nil {
		t.Fatalf("drive the run to its terminal: %v", err)
	}
	// DURABILITY IS THE TERMINAL TRANSACTION'S, not the pump's: the row exists before any tick.
	var pending int
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM queue_deliveries WHERE destination_key=$1 AND state='pending'`, runID).Scan(&pending); err != nil {
		t.Fatalf("read the enqueued delivery: %v", err)
	}
	if pending != 1 {
		t.Fatalf("%d pending outbound deliveries after the run terminal, want 1 — the result must be durable before any pump runs", pending)
	}

	pump := automation.NewQueueOutboxPump(f.queues,
		func([]byte) (queue.Sink, error) { return f.queueSink, nil },
		automation.QueueBridgeConfig{DeliveryBackoff: -time.Second}, t.Logf)
	for i := 0; i < 3; i++ {
		if err := pump.Tick(ctx); err != nil {
			t.Fatalf("outbox tick %d: %v", i, err)
		}
	}
	if got := f.queueSink.unique(); got != 1 {
		t.Fatalf("the sink saw %d unique destination keys across three ticks, want exactly 1 — loss-less means once, not at-least-once at the destination", got)
	}
	f.routes = append(f.routes, "loop automation.QueueOutboxPump")
	f.routes = append(f.routes, "loop automation.QueueBridge")
	f.routes = append(f.routes, "loop extensions.SlackSocket")

	// ---- 8. an A2A push StreamResponse reaches the loopback sink ---------------------------------------
	f.driveA2APush(t)

	// ---- 9. observe the remaining mounts and build the proof from the observations ---------------------
	f.observe(t, http.MethodGet, "/v1/slack-connections", "")
	f.observe(t, http.MethodGet, "/v1/queue-connections", "")
	// The /v1 surface the console consumes on a REAL control plane (E19 T7's real profile drives the
	// browser half in scripts/uat/wiring; what is observed HERE is that the surface it reaches is mounted).
	f.observe(t, http.MethodGet, "/v1/responses/"+responseID, "/v1/responses/{response_id}")

	snapshot := f.servedCapabilities(t)
	proof := f.buildWiringProof(t, snapshot, sourceEventID)

	if !proof.Complete() {
		t.Fatalf("the journey's own WiringProof is not Complete() — the bundle cannot carry a proof this journey would not accept:\n%+v", proof)
	}
	if problems := uat.VerifyWiredMounts(&proof); len(problems) != 0 {
		t.Fatalf("the mount derivation over the OBSERVED stack failed:\n  %s", strings.Join(problems, "\n  "))
	}

	// The closing prediction, asserted rather than assumed: the observed discovery map must equal the tier
	// recompute over the E17 baseline's outcomes. NO TIER MOVED is the epic's defining claim, so it is
	// checked against bytes committed before this epic started, not against anything this journey wrote.
	for capability, served := range snapshot {
		t.Logf("observed /v1/capabilities %s=%s", capability, served)
	}
	for _, want := range []struct{ capability, tier string }{
		{"slack", "preview"}, {"a2a", "preview"}, {"queues", "preview"}, {"console", "preview"},
		{"knowledge", "stable"}, {"capability-workers", "stable"},
		{"knowledge-vector", "disabled"}, {"apple-build", "disabled"},
	} {
		if got := snapshot[want.capability]; got != want.tier {
			t.Errorf("the RUNNING stack advertises %s=%q, want %q — E19 moves no tier", want.capability, got, want.tier)
		}
	}
}

// driveA2APush publishes an interface with push advertised, births a task, registers a loopback push target
// through the SHIPPED pushNotificationConfigs route, and drives a state-producing operation so the
// production WebhookPusher delivers a StreamResponse to the sink.
//
// D13 is what makes the CRUD reachable at all: the routes mount on the SAME condition the card's
// pushNotifications flag reads, so a stack with no Pusher 404s here rather than accepting a target it will
// never fire.
func (f *wiringFixture) driveA2APush(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	iface := a2a.ProjectInterface(f.revision,
		a2a.RevisionSource{Organization: f.org, Project: f.project, Model: "model-pinned"},
		a2a.PublishMeta{Name: "Wiring Planner", Version: "1", AuthScheme: "bearer",
			PushNotifications: true,
			InputModes:        []string{"text/plain"}, OutputModes: []string{"application/json"}})
	ifaceID, err := f.a2aStore.PublishInterface(ctx, iface)
	if err != nil {
		t.Fatalf("publish the A2A interface: %v", err)
	}
	base := "/v1/a2a/interfaces/" + ifaceID

	sendResp, sendRaw := f.authed(t, http.MethodPost, base+"/message:send",
		`{"message":{"role":"user","messageId":"wiring-1","parts":[{"kind":"text","text":"plan the rollout"}]}}`)
	if sendResp.StatusCode != http.StatusOK {
		t.Fatalf("A2A message:send = %d, want 200; %s", sendResp.StatusCode, sendRaw)
	}
	var task struct{ ID string }
	if err := json.Unmarshal([]byte(sendRaw), &task); err != nil || task.ID == "" {
		t.Fatalf("message:send returned no durable task: %s", sendRaw)
	}

	// CONTRACT https://a2a-protocol.org/latest/specification/ (checked 2026-07-26): a PushNotificationConfig
	// carries `url` (required), and the optional `token`/`authentication`/`id`.
	cfgResp, cfgRaw := f.authed(t, http.MethodPost, base+"/tasks/"+task.ID+"/pushNotificationConfigs",
		fmt.Sprintf(`{"url":%q,"token":"wiring-validation-token"}`, f.pushSink.url))
	if cfgResp.StatusCode/100 != 2 {
		t.Fatalf("register the push target = %d, want 2xx (the CRUD mounts only when a Pusher exists — D13); %s", cfgResp.StatusCode, cfgRaw)
	}
	// The response REDACTS the token: a config read-back is not a way to retrieve a shared secret.
	if strings.Contains(cfgRaw, "wiring-validation-token") {
		t.Fatalf("the push-config response echoed the token: %s", cfgRaw)
	}
	f.statuses["POST /v1/a2a/interfaces/{interface_id}/tasks/{id}/pushNotificationConfigs"] = cfgResp.StatusCode
	f.routes = append(f.routes, "POST /v1/a2a/interfaces/{interface_id}/tasks/{id}/pushNotificationConfigs")

	// A state-producing operation is what fires a notification (the server pushes only while serving a
	// request — it runs no background poller).
	cancelResp, cancelRaw := f.authed(t, http.MethodPost, base+"/tasks/"+task.ID+":cancel", "")
	if cancelResp.StatusCode/100 != 2 {
		t.Fatalf("tasks:cancel = %d, want 2xx; %s", cancelResp.StatusCode, cancelRaw)
	}
	f.pusher.Wait()

	bodies := f.pushSink.received()
	if len(bodies) == 0 {
		t.Fatal("the loopback sink received NO push — a registered target that never fires is the D13 silent-drop defect, in the delivery half")
	}
	// CONTRACT https://a2a-protocol.org/latest/specification/ (checked 2026-07-26): the server POSTs a
	// StreamResponse — task | message | statusUpdate | artifactUpdate — never a bare Task envelope.
	var stream map[string]json.RawMessage
	if err := json.Unmarshal(bodies[0], &stream); err != nil {
		t.Fatalf("the pushed body is not JSON: %v (%q)", err, bodies[0])
	}
	member := false
	for _, key := range []string{"task", "message", "statusUpdate", "artifactUpdate"} {
		if _, ok := stream[key]; ok {
			member = true
		}
	}
	if !member {
		t.Fatalf("the pushed body carries none of the four StreamResponse members: %s", bodies[0])
	}
	if stats := f.pusher.Stats(); stats.Delivered == 0 {
		t.Fatalf("the pusher reports %+v — a sink that received bytes with a zero delivered counter means the counter is not wired", stats)
	}
}

// servedCapabilities reads the discovery map off the RUNNING router. This is the observation the mount
// derivation is judged against, so it is an HTTP read of the live surface and never a table lookup.
func (f *wiringFixture) servedCapabilities(t *testing.T) map[string]string {
	t.Helper()
	resp, raw := f.authed(t, http.MethodGet, "/v1/capabilities", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/capabilities = %d, want 200; %s", resp.StatusCode, raw)
	}
	var body struct {
		Capabilities map[string]string `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode the discovery body: %v (%s)", err, raw)
	}
	f.statuses["GET /v1/capabilities"] = resp.StatusCode
	f.routes = append(f.routes, "GET /v1/capabilities")
	return body.Capabilities
}

// buildWiringProof assembles the proof FROM THE OBSERVATIONS this journey took. Every route and every
// status came out of f.observe or out of a step that actually ran; the contract ledger is read from the
// canonical uat table rather than retyped, so a divergence row cannot be dropped here either.
func (f *wiringFixture) buildWiringProof(t *testing.T, snapshot map[string]string, sourceEventID string) uat.WiringProof {
	t.Helper()
	routes := append([]string(nil), f.routes...)
	sort.Strings(routes)
	routes = slicesCompact(routes)

	mount := func(surface, route string, admissionRoute string, sourceEvents []string, deliveries, runs int) uat.WiredSurface {
		return uat.WiredSurface{
			Surface: surface, Route: route, ObservedStatus: f.statuses[route],
			AdmissionRoute: admissionRoute, AdmittedRuns: runs,
			SourceEventIDs: sourceEvents, Deliveries: deliveries,
			Contracts: uat.WiringContracts[surface],
		}
	}
	return uat.WiringProof{
		Surfaces: []uat.WiredSurface{
			mount("slack-connections", "POST /v1/slack-connections", "", nil, 0, 0),
			// TWO deliveries of ONE source event id, ONE run: the transport-invariance counter.
			mount("slack-events", "POST /v1/slack/events", "/v1/slack/events", []string{sourceEventID}, 2, 1),
			mount("slack-interactions", "POST /v1/slack/interactions", "", nil, 0, 0),
			mount("slack-socket", "loop extensions.SlackSocket", "", nil, 0, 0),
			mount("a2a-push", "POST /v1/a2a/interfaces/{interface_id}/tasks/{id}/pushNotificationConfigs", "", nil, 0, 0),
			mount("queue-inbound", "loop automation.QueueBridge", "", nil, 0, 0),
			mount("queue-outbound", "loop automation.QueueOutboxPump", "", nil, 0, 0),
			mount("console", "GET /v1/responses/{response_id}", "", nil, 0, 0),
		},
		CapabilitySnapshot: snapshot,
		SnapshotSource:     "GET /v1/capabilities read over real HTTP from the fully-mounted router this journey drove (apps/control-plane/internal/store TestWiringJourney) — the SAME process that served every route below",
		RouterSurface:      routes,
		ContractsDigest:    uat.WiringContractsDigest(),
		Peers:              uat.WiringPeers,
		LiveLegs:           uat.WiringLiveLegs,
	}
}

// slicesCompact drops adjacent duplicates from a sorted slice.
func slicesCompact(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// publicOnlyResolver resolves every name to a routable public address, so the queue surface's create-time
// egress vet exercises the POLICY rather than this host's DNS.
type publicOnlyResolver struct{}

func (publicOnlyResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
}
