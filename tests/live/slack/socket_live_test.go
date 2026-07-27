//go:build live

// The Slack SOCKET MODE live smoke (E19 T3). Written now, run unchanged the moment the owner supplies
// SLACK_APP_TOKEN — and this is the CHEAPEST live proof in the whole phase, which is the point of the task
// rather than a footnote:
//
//	Socket Mode needs NO PUBLIC URL (plan §0.1). No tunnel, no ngrok, no DNS, no inbound firewall hole. The
//	app-level token in .env.local is the entire setup. T1's and T2's live legs need Slack to be able to POST
//	to a public HTTPS Request URL; this one does not, so if the owner supplies exactly one credential, this
//	is the one that buys a real receipt.
//
// WHAT IT SETTLES THAT NO FIXTURE CAN, and it is the reason a live leg exists at all: our fake WSS peer was
// built from the published page, and a page can be wrong, stale, or incomplete. TestLiveSlackSocketProtocol
// checks the DOCUMENT against the SERVER — the response shape of apps.connections.open, the first frame
// really being `hello`, the envelope really carrying accepts_response_payload, `payload` really being the
// same event_callback body the HTTP transport delivers. Every one of those is a line our fake asserts, so a
// failure here is a finding about the fake, not a flake.
//
// Neither test fails when a credential is missing: it SKIPS, naming the env var and the §0 handover row that
// supplies it, so a partial handover reports partial-green rather than a red wall.
package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/tests/uat"
)

// TestLiveSlackSocketProtocol opens a REAL Socket Mode connection with a REAL app-level token and checks the
// published contract against the live server, one documented claim at a time.
//
// CONTRACT under test — https://docs.slack.dev/apis/events-api/using-socket-mode/ (checked 2026-07-26):
//
//	POST https://slack.com/api/apps.connections.open, Authorization: Bearer xapp-***
//	  ⇒ {"ok":true,"url":"wss://wss.slack.com/link/?ticket=…"}
//	first frame  ⇒ {"type":"hello","connection_info":{…},"num_connections":N,"debug_info":{…}}
//	envelope     ⇒ {"payload":…,"envelope_id":…,"type":…,"accepts_response_payload":<bool>}
//	ack          ⇒ {"envelope_id":…}
//
// SETUP the operator does once: enable Socket Mode in the app, generate an app-level token with
// `connections:write`, subscribe to app_mention, and invite the bot to a channel. Then @-mention it while
// this test waits.
//
// DO NOT run this while the control plane's own Socket Mode loop is connected to the same app: Slack
// distributes events across an app's open connections, so a second consumer can take the mention the other
// one was waiting for. Run this OR TestLiveSlackSocketMentionBirthsExactlyOneRun, not both at once.
func TestLiveSlackSocketProtocol(t *testing.T) {
	token := need(t, "SLACK_APP_TOKEN", "§0.1 — App → Basic Information → App-Level Tokens → Generate, scope connections:write")
	if !strings.HasPrefix(token, "xapp-") {
		t.Fatalf("SLACK_APP_TOKEN does not look like an app-level token (it must start with xapp-); a bot token will be refused with not_allowed_token_type")
	}
	base := "https://slack.com/api"
	if override := os.Getenv("PALAI_SLACK_API_BASE_URL"); override != "" {
		base = override
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveWindow(t)+time.Minute)
	defer cancel()

	// --- apps.connections.open ---
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/apps.connections.open", nil)
	if err != nil {
		t.Fatalf("build apps.connections.open: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("apps.connections.open: %v", err)
	}
	defer resp.Body.Close()
	var opened struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&opened); err != nil {
		t.Fatalf("apps.connections.open: decode: %v", err)
	}
	if !opened.OK {
		t.Fatalf("apps.connections.open answered ok=false error=%q — invalid_auth means the token is wrong, not_allowed_token_type means it is not an app-level token, and missing_scope means it lacks connections:write",
			opened.Error)
	}
	// The URL is a live credential (the ticket). Assert its SHAPE, never its value, and never log it.
	if !strings.HasPrefix(opened.URL, "wss://") {
		t.Fatalf("apps.connections.open returned a non-wss URL scheme; the documented answer is wss://… (value withheld — it carries a ticket)")
	}
	if !strings.Contains(opened.URL, "ticket=") {
		t.Errorf("the returned URL carries no ticket= parameter; the documented example does. This is a contract note, not a failure of our code — our fake models the documented shape.")
	}

	// --- the dial, and the first frame ---
	conn, _, err := websocket.Dial(ctx, opened.URL, nil)
	if err != nil {
		t.Fatal("the WebSocket dial failed (URL withheld: it carries a live ticket)")
	}
	defer conn.CloseNow()

	helloCtx, helloCancel := context.WithTimeout(ctx, 30*time.Second)
	defer helloCancel()
	_, first, err := conn.Read(helloCtx)
	if err != nil {
		t.Fatalf("no first frame within 30s of connecting: %v", err)
	}
	f, err := slack.UnwrapSocketFrame(first)
	if err != nil {
		t.Fatalf("OUR decoder could not read the first frame REAL Slack sent (%v) — that is a finding about adapters/integrations/slack, not a flake", err)
	}
	if f.Type != slack.SocketHello {
		t.Fatalf("the first frame was %q, want %q — the fake peer opens with hello because the page says Slack does", f.Type, slack.SocketHello)
	}
	// The hello's debug_info is what the page documents; read it longhand so a change in Slack's shape shows
	// up here rather than being smoothed over by our own decoder.
	var helloRaw struct {
		ConnectionInfo map[string]any `json:"connection_info"`
		NumConnections int            `json:"num_connections"`
		DebugInfo      struct {
			ApproximateConnectionTime int `json:"approximate_connection_time"`
		} `json:"debug_info"`
	}
	if err := json.Unmarshal(first, &helloRaw); err != nil {
		t.Fatalf("decode the real hello frame: %v", err)
	}
	t.Logf("hello: num_connections=%d approximate_connection_time=%ds — the connection will be refreshed around then, which is what the overlap on `warning` is for",
		helloRaw.NumConnections, helloRaw.DebugInfo.ApproximateConnectionTime)
	if helloRaw.NumConnections > slack.SocketMaxConnections {
		t.Errorf("Slack reports %d open connections, above the documented cap of %d that slack.SocketMaxConnections encodes",
			helloRaw.NumConnections, slack.SocketMaxConnections)
	}

	// --- a real envelope ---
	t.Logf("connected with no public URL, no tunnel and no DNS. @-mention the bot in a channel it has been invited to; waiting up to %s.", liveWindow(t))
	deadline := time.Now().Add(liveWindow(t))
	for {
		if time.Now().After(deadline) {
			t.Fatalf("no events_api envelope arrived in %s — check that the app is subscribed to app_mention, that Socket Mode is ON, and that the bot is in the channel", liveWindow(t))
		}
		readCtx, readCancel := context.WithDeadline(ctx, deadline)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			t.Fatalf("reading frames: %v", err)
		}
		frame, err := slack.UnwrapSocketFrame(data)
		if err != nil {
			t.Fatalf("OUR decoder could not read a frame REAL Slack sent: %v", err)
		}
		if frame.Type == slack.SocketDisconnect {
			// Worth recording precisely, because the overlap is built on this reason set.
			t.Logf("a real disconnect arrived: reason=%q (the loop overlaps on %q/%q and stops permanently on %q)",
				frame.Reason, slack.DisconnectWarning, slack.DisconnectRefreshRequested, slack.DisconnectLinkDisabled)
			switch frame.Reason {
			case slack.DisconnectWarning, slack.DisconnectRefreshRequested, slack.DisconnectLinkDisabled:
			default:
				t.Errorf("Slack sent an UNDOCUMENTED disconnect reason %q; the three the page lists are the only ones the connect loop knows. Add it to adapters/integrations/slack/socket.go.", frame.Reason)
			}
			continue
		}
		if frame.Type != slack.SocketEventsAPI {
			t.Logf("ignoring a %q frame while waiting for events_api", frame.Type)
			continue
		}

		// D6 against reality: the field the tree did not decode until this task must actually be present.
		var rawFrame map[string]json.RawMessage
		if err := json.Unmarshal(data, &rawFrame); err != nil {
			t.Fatalf("decode the raw envelope: %v", err)
		}
		if _, present := rawFrame["accepts_response_payload"]; !present {
			t.Errorf("a REAL events_api envelope carried no accepts_response_payload field, though the page documents one. Our fake always sends it — that is a fixture/contract divergence worth recording.")
		}
		if frame.EnvelopeID == "" {
			t.Fatal("a real envelope carried no envelope_id; there is nothing to acknowledge with")
		}

		// The whole transport-invariance claim, against a real payload: the envelope's `payload` is the SAME
		// event_callback body the HTTP transport delivers, so T1's mapping consumes it unchanged.
		ev, err := slack.MapEvent(frame.Payload, "", false)
		if err != nil {
			t.Fatalf("OUR MapEvent could not read the payload of a REAL Socket Mode envelope (%v) — the claim that both transports carry the same body would be false", err)
		}
		if ev.SourceEventID == "" || ev.TeamID == "" {
			t.Fatalf("a real Socket Mode payload produced identity (team=%q event=%q); both anchor the dedupe and must be present", ev.TeamID, ev.SourceEventID)
		}
		t.Logf("a REAL Socket Mode envelope mapped to the canonical identity team=%s event=%s kind=%s — the same identity the HTTP transport produces for the same event (SLK-001).",
			ev.TeamID, ev.SourceEventID, ev.Kind)

		// Acknowledge it LONGHAND, exactly as the page prints it, so this test does not confirm our own writer.
		ack := fmt.Sprintf(`{"envelope_id":%q}`, frame.EnvelopeID)
		if err := conn.Write(ctx, websocket.MessageText, []byte(ack)); err != nil {
			t.Fatalf("write the acknowledgement: %v", err)
		}
		_ = conn.Close(websocket.StatusNormalClosure, "live smoke complete")
		return
	}
}

// TestLiveSlackSocketMentionBirthsExactlyOneRun is the wiring half: with a RUNNING control plane whose
// PALAI_SLACK_SOCKET_TEAM_ID names this workspace, one real @-mention must produce exactly one run — over a
// transport that required no public URL.
//
// It observes the running stack's DATABASE rather than opening a socket of its own, for two reasons: the
// thing under test is the DEPLOYED loop, and a second consumer would compete for the same events (Slack
// distributes an app's events across its open connections).
//
// This is the Socket Mode half of §6 leg 1. It does NOT flip `slack` out of preview by itself — the tier is
// recomputed from claim outcomes by E17 T11 / E18 T10, and the operator legs are what close it.
func TestLiveSlackSocketMentionBirthsExactlyOneRun(t *testing.T) {
	need(t, "SLACK_APP_TOKEN", "§0.1 — App → Basic Information → App-Level Tokens → Generate, scope connections:write")
	dbURL := need(t, "PALAI_SLACK_LIVE_POSTGRES_URL", "the RUNNING control plane's Postgres URL (make compose-up prints it)")
	team := need(t, "SLACK_TEAM_ID", "§0.1 — workspace admin → about, or any event payload's team_id")

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to the running stack: %v", err)
	}
	defer pool.Close()

	// Keyed on the admission reservation; uat.SlackBornRunsByTeam records why the input projection stopped working.
	const bornSince = uat.SlackBornRunsByTeam
	start := time.Now().UTC()
	t.Logf("watching for a Socket-Mode-born run in workspace %s. The control plane must be running with PALAI_SLACK_SOCKET_TEAM_ID=%s and the workspace registered with a resolvable app_token_ref. @-mention the bot now.",
		team, team)

	deadline := time.Now().Add(liveWindow(t))
	byEvent := map[string][]string{}
	for time.Now().Before(deadline) {
		byEvent = pollSlackBornRuns(t, ctx, pool, bornSince, team, start)
		if len(byEvent) > 0 {
			// Socket Mode has no documented redelivery, but the control plane may hold overlapping connections
			// during a refresh — so settle briefly and re-read once before concluding.
			time.Sleep(10 * time.Second)
			byEvent = pollSlackBornRuns(t, ctx, pool, bornSince, team, start)
			break
		}
		time.Sleep(2 * time.Second)
	}
	if len(byEvent) == 0 {
		t.Fatalf("no Slack-born run appeared in %s — check that Socket Mode is ON in the app, that PALAI_SLACK_SOCKET_TEAM_ID is set on the running control plane, that its log carries \"Slack Socket Mode enabled\", and that the connection's app_token_ref resolves",
			liveWindow(t))
	}
	for eventID, responses := range byEvent {
		if len(responses) != 1 {
			t.Fatalf("source event %s produced %d responses (%v), want exactly 1 — one effect per source event holds across transports, and an overlapping reconnect is exactly when a second one would appear",
				eventID, len(responses), responses)
		}
	}
	t.Logf("%d real Slack event(s) each produced exactly one run, over Socket Mode, with NO public URL. §6 leg 1's Socket Mode half has a receipt.", len(byEvent))
}
