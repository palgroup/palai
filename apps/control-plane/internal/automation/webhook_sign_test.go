package automation

import (
	"fmt"
	"testing"
	"time"
)

// TestSignEmptySecretIsRetryableNotPanic pins F1: a resolved secret that is empty or whitespace-only
// (an empty/misconfigured secret file) must NOT reach NewSigner — which panics on an empty secret set —
// because a panic fires before the reschedule and would wedge the poison row at the head of the due
// queue, halting delivery for every tenant. sign must instead return a retryable error.
func TestSignEmptySecretIsRetryableNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("sign panicked on an empty secret (poison-row DoS): %v", r)
		}
	}()
	p := &WebhookPump{secrets: func(_ string, _ string) ([]byte, error) { return []byte("  \n\t"), nil }, now: time.Now}
	if _, err := p.sign(dueDelivery{Org: "org_a", EndpointID: "whe_1", SecretRef: "ref", Payload: []byte("{}")}, time.Now(), 1); err == nil {
		t.Fatal("sign with an empty/whitespace secret must return a retryable error, not a signed delivery")
	}
}

// TestSignTrimsSecretWhitespace pins F12: a trailing newline in a secret file is stripped so a secret
// stored as "whsec_x\n" is not silently a different (broken) key from "whsec_x".
func TestSignTrimsSecretWhitespace(t *testing.T) {
	trimmed := func(_ string, _ string) ([]byte, error) { return []byte("whsec_padded\n"), nil }
	p := &WebhookPump{secrets: trimmed, now: time.Now}
	sig, err := p.sign(dueDelivery{Org: "org_a", EndpointID: "whe_1", SecretRef: "ref", Payload: []byte("{}")}, time.Unix(1784203200, 0), 1)
	if err != nil {
		t.Fatalf("sign error = %v", err)
	}
	// The signature must match the TRIMMED secret, not the raw newline-padded bytes.
	unpaddedPump := &WebhookPump{secrets: func(_ string, _ string) ([]byte, error) { return []byte("whsec_padded"), nil }, now: time.Now}
	sig2, _ := unpaddedPump.sign(dueDelivery{Org: "org_a", EndpointID: "whe_1", SecretRef: "ref", Payload: []byte("{}")}, time.Unix(1784203200, 0), 1)
	if sig.dst.Headers["Webhook-Signature"] != sig2.dst.Headers["Webhook-Signature"] {
		t.Fatal("a trailing newline changed the signature — secret whitespace is not trimmed")
	}
}

// TestSignResolvesSecretScopedToEndpointOrg pins F2: SigningSecretRef is tenant input, so resolution
// MUST be scoped by the endpoint's org — tenant A naming a ref that only exists under tenant B's scope
// must FAIL, never sign with B's secret (an HMAC-forgery oracle to an attacker-chosen receiver).
func TestSignResolvesSecretScopedToEndpointOrg(t *testing.T) {
	// The resolver serves the ref "shared" only under org "org_b".
	resolver := func(org, ref string) ([]byte, error) {
		if org == "org_b" && ref == "shared" {
			return []byte("whsec_org_b_secret"), nil
		}
		return nil, fmt.Errorf("no secret bridge for %s/%s", org, ref)
	}
	p := &WebhookPump{secrets: resolver, now: time.Now}

	if _, err := p.sign(dueDelivery{Org: "org_a", EndpointID: "whe_a", SecretRef: "shared", Payload: []byte("{}")}, time.Now(), 1); err == nil {
		t.Fatal("org_a resolved a secret registered under org_b — cross-tenant signing oracle open")
	}
	if _, err := p.sign(dueDelivery{Org: "org_b", EndpointID: "whe_b", SecretRef: "shared", Payload: []byte("{}")}, time.Now(), 1); err != nil {
		t.Fatalf("org_b failed to resolve its OWN secret: %v", err)
	}
}
