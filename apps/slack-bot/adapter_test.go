// This file is Task 6 (2026-08-03 plan)'s: the seam that apps/slack-bot, the module, can reach
// adapters/integrations/slack from exactly where it stands — adapter and bot are in the
// SAME Go module (github.com/palgroup/palai), so no move is needed for the bot to use it. The import
// surface was measured empty of Palai dependencies (`grep -h 'palai/' adapters/integrations/slack/*.go
// | grep -v _test | grep '"'` → nothing) before this test was written; that emptiness is what makes
// the adapter portable, and it is what this test exercises, not just imports.
//
// WHAT USED TO STAND HERE signed a request with slack.VerifySignature — a real, HTTP-transport-only
// symbol that this SAME bot never called at runtime (Socket Mode is authenticated by the WebSocket
// connection itself, not a per-message MAC; see adapters/integrations/slack/socket.go's own doc on why
// VerifySignature "must never be reached from here"). It proved the module boundary using a symbol that
// was not actually part of the bot's own dependency surface, and it was deleted with the rest of the HTTP
// transport (2026-08-05 cleanup) — the seam still needs a proof.
//
// So this one drives slack.MapEvent instead: the ONE adapter call apps/slack-bot/internal/socket's dispatch
// loop makes on every inbound frame (see internal/socket/socket.go), over a genuine Socket Mode envelope
// payload shaped exactly as Slack documents it — proving the SAME reachability the deleted test proved,
// through a dependency this process actually has at runtime rather than one it does not.
package main

import (
	"testing"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
)

func TestBotCanReachTheAdapterThroughItsPublicSurface(t *testing.T) {
	body := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev01",` +
		`"event":{"type":"app_mention","user":"U1","channel":"C1","ts":"1700000000.000100","text":"hi"}}`)

	ev, err := slack.MapEvent(body, "U_BOT", false)
	if err != nil {
		t.Fatalf("MapEvent through the adapter's public surface: %v", err)
	}
	if ev.SourceEventID != "Ev01" || ev.ChannelID != "C1" || ev.TeamID != "T1" {
		t.Fatalf("mapped event = %+v, want SourceEventID=Ev01 ChannelID=C1 TeamID=T1", ev)
	}
	if ev.Type != "app_mention" || ev.Kind != slack.KindMessage {
		t.Fatalf("mapped event = %+v, want an app_mention classified as KindMessage", ev)
	}

	// The self-loop guard is the OTHER half of what this call is trusted to get right: an event whose user
	// IS the bot must map to ErrIgnored rather than becoming a turn.
	if _, err := slack.MapEvent(body, "U1", false); err != slack.ErrIgnored {
		t.Fatalf("MapEvent with the event's own author as botUserID: err = %v, want ErrIgnored", err)
	}
}
