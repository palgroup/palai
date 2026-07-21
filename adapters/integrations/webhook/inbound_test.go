package webhook

import (
	"errors"
	"testing"
	"time"
)

// signedEvent signs a raw inbound body with the given secrets under a deterministic delivery id, so
// sign (Signer.Headers) and verify (ParseInbound → Verify) share ONE vector.
func signedEvent(t *testing.T, secrets [][]byte, id string, ts time.Time, body []byte) map[string]string {
	t.Helper()
	return NewSigner(secrets...).Headers(id, ts, 1, body)
}

// TestInboundVerifyTamperStaleAndRotation pins the receiver side: a known-vector body verifies; a single
// flipped byte is ErrBadSignature; an out-of-tolerance timestamp is ErrStaleTimestamp; and an event signed
// under the SECOND of two active secrets still verifies (rotation overlap) — all through the T4 Verify, no
// new MAC code.
func TestInboundVerifyTamperStaleAndRotation(t *testing.T) {
	secret := []byte("whsec_inbound_primary")
	ts := time.Unix(1784203200, 0)
	now := ts.Add(time.Minute)
	body := []byte(`{"source":"harness","data":{"order":"o-1"}}`)
	headers := signedEvent(t, [][]byte{secret}, "evt_1", ts, body)

	ev, err := ParseInbound(headers, body, [][]byte{secret}, now, 5*time.Minute)
	if err != nil {
		t.Fatalf("known vector rejected: %v", err)
	}
	if ev.Source != "harness" || ev.SourceEventID != "evt_1" {
		t.Fatalf("normalized event = %+v, want source=harness source_event_id=evt_1 (from the signed Webhook-Id)", ev)
	}

	// A single flipped body byte no longer matches the MAC → ErrBadSignature (not stale, not malformed).
	tampered := append([]byte{}, body...)
	tampered[len(tampered)-2] ^= 0x01
	if _, err := ParseInbound(headers, tampered, [][]byte{secret}, now, 5*time.Minute); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered body err = %v, want ErrBadSignature", err)
	}

	// A timestamp outside tolerance is a replay-window reject → ErrStaleTimestamp (distinct from a bad MAC).
	if _, err := ParseInbound(headers, body, [][]byte{secret}, ts.Add(6*time.Minute), 5*time.Minute); !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("stale timestamp err = %v, want ErrStaleTimestamp", err)
	}

	// Rotation: an event signed under only the NEW secret verifies when both refs are active.
	oldS, newS := []byte("whsec_old"), []byte("whsec_new")
	rot := signedEvent(t, [][]byte{newS}, "evt_2", ts, body)
	if _, err := ParseInbound(rot, body, [][]byte{oldS, newS}, now, 5*time.Minute); err != nil {
		t.Fatalf("rotation: event signed under the new secret rejected: %v", err)
	}
	// After cutover (only the old secret is left) the new-secret signature fails.
	if _, err := ParseInbound(rot, body, [][]byte{oldS}, now, 5*time.Minute); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("post-cutover err = %v, want ErrBadSignature", err)
	}
}

// TestInboundEventNormalizedStrict pins §21.7 normalization: source is required; the SIGNED Webhook-Id is
// authoritative for source_event_id (a mismatching body value is rejected — the MAC binds the id); the
// envelope decodes STRICTLY (an unknown top-level field is rejected) while the opaque data payload passes
// through untouched.
func TestInboundEventNormalizedStrict(t *testing.T) {
	secret := []byte("whsec_norm")
	ts := time.Unix(1784203200, 0)
	now := ts

	parse := func(body []byte, id string) (InboundEvent, error) {
		return ParseInbound(signedEvent(t, [][]byte{secret}, id, ts, body), body, [][]byte{secret}, now, 5*time.Minute)
	}

	// The signed Webhook-Id is authoritative; source_tenant + opaque data pass through.
	ev, err := parse([]byte(`{"source":"harness","source_tenant":"acme","data":{"n":1}}`), "wid_1")
	if err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
	if ev.SourceEventID != "wid_1" || ev.SourceTenant != "acme" || string(ev.Data) != `{"n":1}` {
		t.Fatalf("normalized = %+v, want id=wid_1 tenant=acme data={\"n\":1}", ev)
	}

	// A missing source cannot be routed.
	if _, err := parse([]byte(`{"data":{}}`), "wid_2"); !errors.Is(err, ErrMalformedInbound) {
		t.Fatalf("missing source err = %v, want ErrMalformedInbound", err)
	}

	// An unknown top-level field is a strict-decode reject (the envelope, not the opaque data).
	if _, err := parse([]byte(`{"source":"harness","surprise":true}`), "wid_3"); !errors.Is(err, ErrMalformedInbound) {
		t.Fatalf("unknown field err = %v, want ErrMalformedInbound", err)
	}

	// A body source_event_id that disagrees with the signed Webhook-Id is a reject (the MAC binds the id).
	if _, err := parse([]byte(`{"source":"harness","source_event_id":"forged"}`), "wid_4"); !errors.Is(err, ErrMalformedInbound) {
		t.Fatalf("mismatched body id err = %v, want ErrMalformedInbound", err)
	}

	// Non-JSON is a malformed envelope (this event was signed — the MAC covers whatever bytes arrived).
	if _, err := parse([]byte(`not json`), "wid_5"); !errors.Is(err, ErrMalformedInbound) {
		t.Fatalf("non-JSON err = %v, want ErrMalformedInbound", err)
	}
}
