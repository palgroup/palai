package slack

import (
	"encoding/json"
	"errors"
	"testing"
)

// Every literal in this file is copied from the published Socket Mode page
// (https://docs.slack.dev/apis/events-api/using-socket-mode/, checked 2026-07-26), NOT from our own encoder —
// the E17 T10 lesson (a fixture built from our own writer can only ever confirm our own writer).

// TestSocketFrameCarriesAcceptsResponsePayload is plan §3.5 row D6: the documented envelope is
//
//	{"payload":…, "envelope_id":…, "type":…, "accepts_response_payload": <bool>}
//
// and the tree decoded only the first three. Whether the envelope ACCEPTS a response payload is not
// cosmetic: attaching one to an envelope that does not accept it is a protocol error, so a decoder that
// cannot see the field cannot obey the rule.
func TestSocketFrameCarriesAcceptsResponsePayload(t *testing.T) {
	accepting := []byte(`{"payload":{"type":"event_callback","team_id":"T1","event_id":"Ev1","event":{"type":"app_mention"}},"envelope_id":"env-1","type":"events_api","accepts_response_payload":true}`)
	f, err := UnwrapSocketFrame(accepting)
	if err != nil {
		t.Fatalf("UnwrapSocketFrame error = %v", err)
	}
	if !f.AcceptsResponsePayload {
		t.Fatalf("accepts_response_payload decoded as false for %s — the documented field is not being read", accepting)
	}

	refusing := []byte(`{"payload":{},"envelope_id":"env-2","type":"events_api","accepts_response_payload":false}`)
	f, err = UnwrapSocketFrame(refusing)
	if err != nil {
		t.Fatalf("UnwrapSocketFrame error = %v", err)
	}
	if f.AcceptsResponsePayload {
		t.Fatalf("accepts_response_payload decoded as true for an envelope that declares false")
	}
}

// TestSocketDisconnectReasonIsDecoded pins the disconnect frame, verbatim from the page:
//
//	{"type":"disconnect","reason":"link_disabled","debug_info":{"host":"wss-111.slack.com"}}
//
// The reason is the whole of the reconnect decision — `warning` is a ten-second notice to overlap a
// successor, `link_disabled` is permanent — so a decoder that drops it leaves the loop unable to tell a
// scheduled refresh from an app that has been switched off.
func TestSocketDisconnectReasonIsDecoded(t *testing.T) {
	for _, want := range []string{DisconnectWarning, DisconnectRefreshRequested, DisconnectLinkDisabled} {
		frame := []byte(`{"type":"disconnect","reason":"` + want + `","debug_info":{"host":"wss-111.slack.com"}}`)
		f, err := UnwrapSocketFrame(frame)
		if err != nil {
			t.Fatalf("UnwrapSocketFrame(%s) error = %v", frame, err)
		}
		if f.Type != SocketDisconnect {
			t.Fatalf("frame type = %q, want %q", f.Type, SocketDisconnect)
		}
		if f.Reason != want {
			t.Fatalf("disconnect reason = %q, want %q", f.Reason, want)
		}
	}
}

// TestSocketAckShape pins the acknowledgement, verbatim from the page:
//
//	{"envelope_id": "<$unique_identifier_string>", "payload": "<$payload_shape>"}   (payload optional)
//
// and the D6 rule the shape implies: a response payload may ride the ack ONLY when the envelope said it
// accepts one. Sending one otherwise is a protocol error, so this refuses to build it rather than silently
// dropping the caller's payload — a drop looks like success at the call site.
func TestSocketAckShape(t *testing.T) {
	ack, err := SocketAck("env-1", false, nil)
	if err != nil {
		t.Fatalf("SocketAck error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(ack, &decoded); err != nil {
		t.Fatalf("ack is not JSON: %v (%s)", err, ack)
	}
	if decoded["envelope_id"] != "env-1" {
		t.Fatalf("ack = %s, want envelope_id env-1", ack)
	}
	if _, present := decoded["payload"]; present {
		t.Fatalf("ack = %s carries a payload key; the payload is optional and none was asked for", ack)
	}

	ack, err = SocketAck("env-2", true, []byte(`{"text":"ok"}`))
	if err != nil {
		t.Fatalf("SocketAck with an accepting envelope error = %v", err)
	}
	if err := json.Unmarshal(ack, &decoded); err != nil {
		t.Fatalf("ack is not JSON: %v (%s)", err, ack)
	}
	if _, present := decoded["payload"]; !present {
		t.Fatalf("ack = %s dropped the payload an accepting envelope asked for", ack)
	}

	if _, err := SocketAck("env-3", false, []byte(`{"text":"nope"}`)); !errors.Is(err, ErrSocketNoResponsePayload) {
		t.Fatalf("SocketAck(payload, accepts=false) err = %v, want ErrSocketNoResponsePayload — attaching a payload to an envelope that does not accept one is a protocol error", err)
	}
	if _, err := SocketAck("", false, nil); err == nil {
		t.Fatalf("SocketAck with no envelope_id succeeded; an ack Slack cannot correlate is not an ack")
	}
}

// TestSocketHelloAndTypesDecode walks the documented `type` set. hello and disconnect carry NO envelope_id
// (they are not acknowledged); events_api / interactive / slash_commands do.
func TestSocketHelloAndTypesDecode(t *testing.T) {
	hello := []byte(`{"type":"hello","connection_info":{"app_id":"A1234"},"num_connections":1,"debug_info":{"host":"applink-1","started":"2020-10-11 12:12:12.120","build_number":54,"approximate_connection_time":3600}}`)
	f, err := UnwrapSocketFrame(hello)
	if err != nil {
		t.Fatalf("UnwrapSocketFrame(hello) error = %v", err)
	}
	if f.Type != SocketHello || f.EnvelopeID != "" {
		t.Fatalf("hello = %q/%q, want %q and no envelope id", f.Type, f.EnvelopeID, SocketHello)
	}
	if f.NumConnections != 1 {
		t.Fatalf("hello num_connections = %d, want 1 — the loop reports it so an operator can see the overlap against Slack's cap of %d",
			f.NumConnections, SocketMaxConnections)
	}
	for _, typ := range []string{SocketEventsAPI, SocketInteractive, SocketSlashCommands} {
		frame := []byte(`{"type":"` + typ + `","envelope_id":"e","payload":{},"accepts_response_payload":false}`)
		f, err := UnwrapSocketFrame(frame)
		if err != nil || f.Type != typ || f.EnvelopeID != "e" {
			t.Fatalf("UnwrapSocketFrame(%s) = %+v, %v", frame, f, err)
		}
	}
}
