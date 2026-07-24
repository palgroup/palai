package palai

import (
	"testing"
	"time"
)

// The webhook verify mirrors the server's signer exactly (byte-identical HMAC) and is pinned
// cross-language by the corpus's signature-verify category. This unit test covers the rotation
// window, tampering, wrong secret, and the replay tolerance around now (API-014).
func TestVerifyWebhook(t *testing.T) {
	secret := []byte("secret-key-alpha")
	id := "tcall_conf_001"
	ts := time.Unix(1700000000, 0)
	body := []byte(`{"operation_id":"rop_conf_001","protocol":"tool-http.v1","result":{"answer":"sunny"},"tool_call_id":"tcall_conf_001"}`)
	sig := "v1=" + SignWebhook(secret, id, ts, body)
	tol := 300 * time.Second

	if !VerifyWebhook(secret, id, ts, body, sig, ts, tol) {
		t.Fatal("exact-time valid signature must verify")
	}
	if !VerifyWebhook(secret, id, ts, body, sig, time.Unix(1700000200, 0), tol) {
		t.Fatal("within-window signature must verify")
	}
	if VerifyWebhook(secret, id, ts, body, sig, time.Unix(1700000600, 0), tol) {
		t.Fatal("a stale (expired) timestamp must be rejected")
	}
	if VerifyWebhook(secret, id, ts, body, sig, time.Unix(1699999400, 0), tol) {
		t.Fatal("a future-skew timestamp must be rejected")
	}
	tampered := []byte(`{"operation_id":"rop_conf_001","protocol":"tool-http.v1","result":{"answer":"rainy"},"tool_call_id":"tcall_conf_001"}`)
	if VerifyWebhook(secret, id, ts, tampered, sig, ts, tol) {
		t.Fatal("a tampered body must be rejected")
	}
	if VerifyWebhook([]byte("secret-key-bravo"), id, ts, body, sig, ts, tol) {
		t.Fatal("the wrong secret must be rejected")
	}
	if VerifyWebhook(secret, id, ts, body, "garbage", ts, tol) {
		t.Fatal("a malformed (no v1=) signature must be rejected")
	}
}

// A rotation-overlap header carries several space-separated v1= values; a receiver on either the
// old OR the new secret verifies its own.
func TestVerifyWebhookRotationOverlap(t *testing.T) {
	id := "tcall_conf_001"
	ts := time.Unix(1700000000, 0)
	body := []byte(`{"answer":"sunny"}`)
	tol := 300 * time.Second
	alpha := SignWebhook([]byte("secret-key-alpha"), id, ts, body)
	bravo := SignWebhook([]byte("secret-key-bravo"), id, ts, body)
	header := "v1=" + alpha + " v1=" + bravo

	if !VerifyWebhook([]byte("secret-key-alpha"), id, ts, body, header, ts, tol) {
		t.Fatal("old-secret receiver must verify the rotation header")
	}
	if !VerifyWebhook([]byte("secret-key-bravo"), id, ts, body, header, ts, tol) {
		t.Fatal("new-secret receiver must verify the rotation header")
	}
}
