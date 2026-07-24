package slack

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

// sign is the fixture signer — the fake Slack peer computes the v0 header exactly as the receiver does, so
// the tests exercise real MAC agreement rather than a hand-typed hex string.
func sign(secret []byte, ts time.Time, body []byte) (string, string) {
	stamp := strconv.FormatInt(ts.Unix(), 10)
	return stamp, signatureFor(secret, stamp, body)
}

func TestVerifySignatureAcceptsAGenuineRequest(t *testing.T) {
	secret := []byte("8f742231b10e8888abcd99yyyzzz85a5")
	body := []byte(`{"type":"event_callback","event_id":"Ev01"}`)
	now := time.Unix(1_700_000_000, 0)
	stamp, sig := sign(secret, now, body)

	if err := VerifySignature(secret, stamp, sig, body, now, DefaultTolerance); err != nil {
		t.Fatalf("genuine request rejected: %v", err)
	}
}

func TestVerifySignatureRejectsATamperedBody(t *testing.T) {
	secret := []byte("8f742231b10e8888abcd99yyyzzz85a5")
	body := []byte(`{"type":"event_callback","event_id":"Ev01"}`)
	now := time.Unix(1_700_000_000, 0)
	stamp, sig := sign(secret, now, body)

	// The signature was computed over the original body; a single flipped byte must break the MAC.
	tampered := []byte(`{"type":"event_callback","event_id":"Ev99"}`)
	if err := VerifySignature(secret, stamp, sig, tampered, now, DefaultTolerance); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered body: err = %v, want ErrBadSignature", err)
	}
}

func TestVerifySignatureRejectsAWrongSecret(t *testing.T) {
	body := []byte(`{"type":"event_callback"}`)
	now := time.Unix(1_700_000_000, 0)
	stamp, sig := sign([]byte("the-real-signing-secret"), now, body)

	if err := VerifySignature([]byte("a-different-secret"), stamp, sig, body, now, DefaultTolerance); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong secret: err = %v, want ErrBadSignature", err)
	}
}

func TestVerifySignatureRejectsAStaleTimestamp(t *testing.T) {
	secret := []byte("8f742231b10e8888abcd99yyyzzz85a5")
	body := []byte(`{"type":"event_callback"}`)
	signed := time.Unix(1_700_000_000, 0)
	stamp, sig := sign(secret, signed, body)

	// Six minutes later the (still cryptographically valid) signature is outside the replay window — a
	// captured-then-replayed request must be refused as stale, a DISTINCT reason from a bad MAC.
	now := signed.Add(6 * time.Minute)
	if err := VerifySignature(secret, stamp, sig, body, now, DefaultTolerance); !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("stale timestamp: err = %v, want ErrStaleTimestamp", err)
	}
}

func TestVerifySignatureRejectsAGarbledTimestampAndEmptySecret(t *testing.T) {
	secret := []byte("s")
	body := []byte(`{}`)
	now := time.Unix(1_700_000_000, 0)
	if err := VerifySignature(secret, "not-a-number", "v0=deadbeef", body, now, DefaultTolerance); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("garbled timestamp: err = %v, want ErrBadSignature", err)
	}
	// An empty secret must fail closed, never accept an unauthenticated body.
	stamp, sig := sign([]byte("real"), now, body)
	if err := VerifySignature(nil, stamp, sig, body, now, DefaultTolerance); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("empty secret: err = %v, want ErrBadSignature (fail closed)", err)
	}
}
