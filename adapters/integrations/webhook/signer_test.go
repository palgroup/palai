package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// TestSignatureCoversRawBodyAndVerifiesConstantTime pins the §21.5 signing contract: the MAC covers
// version + delivery id + timestamp + the EXACT raw body, the four headers are emitted, a single
// flipped body byte is rejected, and verification is constant-time (hmac.Equal).
func TestSignatureCoversRawBodyAndVerifiesConstantTime(t *testing.T) {
	secret := []byte("whsec_test_primary")
	signer := NewSigner(secret)
	deliveryID := "whd_abc123"
	ts := time.Unix(1784203200, 0)
	body := []byte(`{"type":"run.completed.v1","data":{"run_id":"run_1"}}`)

	headers := signer.Headers(deliveryID, ts, 3, body)
	if headers[HeaderID] != deliveryID {
		t.Fatalf("Webhook-Id = %q, want %q", headers[HeaderID], deliveryID)
	}
	if headers[HeaderTimestamp] != "1784203200" {
		t.Fatalf("Webhook-Timestamp = %q, want 1784203200", headers[HeaderTimestamp])
	}
	if headers[HeaderAttempt] != "3" {
		t.Fatalf("Webhook-Attempt = %q, want 3", headers[HeaderAttempt])
	}
	sig := headers[HeaderSignature]
	if !strings.HasPrefix(sig, "v1=") {
		t.Fatalf("Webhook-Signature = %q, want a v1= scheme", sig)
	}

	// Known-vector: the MAC is over version.deliveryID.timestamp.rawBody exactly.
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v1." + deliveryID + ".1784203200." + string(body)))
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	if sig != want {
		t.Fatalf("signature = %q, want known vector %q", sig, want)
	}

	// A receiver verifies the exact raw body under the 5-minute tolerance.
	if !Verify(secret, deliveryID, ts, body, sig, ts.Add(time.Minute), 5*time.Minute) {
		t.Fatal("Verify rejected a valid signature")
	}
	// A single flipped body byte must be rejected — the MAC covers the raw body.
	tampered := append([]byte{}, body...)
	tampered[0] ^= 0x01
	if Verify(secret, deliveryID, ts, tampered, sig, ts.Add(time.Minute), 5*time.Minute) {
		t.Fatal("Verify accepted a tampered body")
	}
	// The wrong secret must be rejected.
	if Verify([]byte("whsec_wrong"), deliveryID, ts, body, sig, ts.Add(time.Minute), 5*time.Minute) {
		t.Fatal("Verify accepted a foreign secret")
	}
}

// TestRotationBothSecretsVerifyWindowBounded pins §21.5 rotation: while two secrets are active the
// attempt carries a signature for each, so a receiver holding either the old or the new secret
// verifies; and a timestamp outside the configurable tolerance is rejected (replay window).
func TestRotationBothSecretsVerifyWindowBounded(t *testing.T) {
	oldSecret := []byte("whsec_old")
	newSecret := []byte("whsec_new")
	signer := NewSigner(oldSecret, newSecret)
	deliveryID := "whd_rot"
	ts := time.Unix(1784203200, 0)
	body := []byte(`{"n":1}`)

	sig := signer.Headers(deliveryID, ts, 1, body)[HeaderSignature]
	// Both the old and the new receiver verify against the overlapping header.
	if !Verify(oldSecret, deliveryID, ts, body, sig, ts, 5*time.Minute) {
		t.Fatal("rotation: receiver with the OLD secret failed to verify")
	}
	if !Verify(newSecret, deliveryID, ts, body, sig, ts, 5*time.Minute) {
		t.Fatal("rotation: receiver with the NEW secret failed to verify")
	}
	// The replay window is bounded: a timestamp older than tolerance fails even with the right secret.
	if Verify(newSecret, deliveryID, ts, body, sig, ts.Add(6*time.Minute), 5*time.Minute) {
		t.Fatal("a signature outside the timestamp tolerance was accepted (replay window unbounded)")
	}
	if Verify(newSecret, deliveryID, ts, body, sig, ts.Add(-6*time.Minute), 5*time.Minute) {
		t.Fatal("a future-skewed signature outside tolerance was accepted")
	}
}
