package automation

import (
	"fmt"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
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
	p := &WebhookPump{secrets: func(_ coordinator.Tenant, _ string) ([]byte, error) { return []byte("  \n\t"), nil }, now: time.Now}
	if _, err := p.sign(dueDelivery{EndpointID: "whe_1", SecretRef: "ref", Payload: []byte("{}")}, time.Now(), 1); err == nil {
		t.Fatal("sign with an empty/whitespace secret must return a retryable error, not a signed delivery")
	}
}

// TestSignTrimsSecretWhitespace pins F12: a trailing newline in a secret file is stripped so a secret
// stored as "whsec_x\n" is not silently a different (broken) key from "whsec_x".
func TestSignTrimsSecretWhitespace(t *testing.T) {
	trimmed := func(_ coordinator.Tenant, _ string) ([]byte, error) { return []byte("whsec_padded\n"), nil }
	p := &WebhookPump{secrets: trimmed, now: time.Now}
	sig, err := p.sign(dueDelivery{EndpointID: "whe_1", SecretRef: "ref", Payload: []byte("{}")}, time.Unix(1784203200, 0), 1)
	if err != nil {
		t.Fatalf("sign error = %v", err)
	}
	// The signature must match the TRIMMED secret, not the raw newline-padded bytes.
	unpaddedPump := &WebhookPump{secrets: func(_ coordinator.Tenant, _ string) ([]byte, error) { return []byte("whsec_padded"), nil }, now: time.Now}
	sig2, _ := unpaddedPump.sign(dueDelivery{EndpointID: "whe_1", SecretRef: "ref", Payload: []byte("{}")}, time.Unix(1784203200, 0), 1)
	if sig.dst.Headers["Webhook-Signature"] != sig2.dst.Headers["Webhook-Signature"] {
		t.Fatal("a trailing newline changed the signature — secret whitespace is not trimmed")
	}
}

// TestSignResolvesSecretByRefAlone REPLACES TestSignResolvesSecretScopedToEndpointOrg, and the direction
// is REVERSED rather than the test deleted, because deleting it would leave the tree silent about a
// boundary that used to be there.
//
// The old test pinned F2: SigningSecretRef is tenant input, so resolution was scoped by the endpoint's
// ORG and one endpoint naming another tenant's ref failed. A.2 Task 6 removed the organization, and
// migration 000066 keys secret_refs on the INSTALLATION (the table carries no tenant column), so the
// resolver takes a ref and nothing else. Every endpoint in this installation that names "shared"
// therefore signs with the SAME secret — which is what this asserts, so the day that stops being
// acceptable the assertion is here to break.
func TestSignResolvesSecretByRefAlone(t *testing.T) {
	resolver := func(_ coordinator.Tenant, ref string) ([]byte, error) {
		if ref == "shared" {
			return []byte("whsec_shared_secret"), nil
		}
		return nil, fmt.Errorf("no secret bridge for %s", ref)
	}
	p := &WebhookPump{secrets: resolver, now: time.Now}

	a, err := p.sign(dueDelivery{EndpointID: "whe_a", SecretRef: "shared", Payload: []byte("{}")}, time.Unix(1784203200, 0), 1)
	if err != nil {
		t.Fatalf("endpoint A failed to resolve the installation's secret: %v", err)
	}
	b, err := p.sign(dueDelivery{EndpointID: "whe_b", SecretRef: "shared", Payload: []byte("{}")}, time.Unix(1784203200, 0), 1)
	if err != nil {
		t.Fatalf("endpoint B failed to resolve the installation's secret: %v", err)
	}
	if a.dst.Headers["Webhook-Signature"] != b.dst.Headers["Webhook-Signature"] {
		t.Fatal("two endpoints naming the same ref signed differently — the ref is installation-wide since 000066")
	}
	if _, err := p.sign(dueDelivery{EndpointID: "whe_a", SecretRef: "absent", Payload: []byte("{}")}, time.Now(), 1); err == nil {
		t.Fatal("an unprovisioned ref must fail the attempt, never sign with something else")
	}
}
