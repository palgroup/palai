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
// ORG and one endpoint naming another tenant's ref failed. A.2 Task 6 removed the organization, leaving
// secret_refs reachable by every scope in the installation, so the resolver took a ref and nothing else.
//
// THE DATABASE HAS SINCE MOVED AND THIS TEST DID NOT NOTICE, which is worth stating plainly because the
// comment above used to end "the day that stops being acceptable the assertion is here to break" — and
// that day came without the assertion breaking. secret_refs now carries project_id under
// `tenant_isolation` keyed on it (secret_refs_name_version_key UNIQUE (project_id, name, version)), so a
// ref named by two projects no longer resolves to one row for rows written under that constraint.
//
// The assertion below did not break because it never reached the database: `resolver` here is a literal
// func, so what this test pins is the SEAM's shape — signing resolves by ref alone, with no tenant
// discriminator available to it — not the store's answer. That is still the honest claim for this file,
// and the DB-level boundary is proven where the DB is real (identity's secret component tests).
//
// TWO BRANCHES, because one sentence cannot cover both: a ref written since the project boundary
// resolves within its project, while a legacy row whose project_id is the empty string stays readable to every
// scope by 000002's contract — so on an installation with pre-existing secrets, two endpoints naming
// "shared" can still reach the same bytes.
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
		t.Fatal("two endpoints naming the same ref signed differently — this seam resolves by ref alone, with no tenant discriminator reaching it")
	}
	if _, err := p.sign(dueDelivery{EndpointID: "whe_a", SecretRef: "absent", Payload: []byte("{}")}, time.Now(), 1); err == nil {
		t.Fatal("an unprovisioned ref must fail the attempt, never sign with something else")
	}
}
