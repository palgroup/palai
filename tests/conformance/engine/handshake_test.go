package engine_test

import (
	"context"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
)

func conformanceNow() time.Time { return time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC) }

// TestEnrollmentTokenIsSingleUse proves an enrollment token exchanges for an identity
// exactly once: the second presentation of the same token is rejected, so a stolen or
// replayed bootstrap token cannot mint a second runner identity (spec §28 enrollment).
func TestEnrollmentTokenIsSingleUse(t *testing.T) {
	stub := newStubControlPlane(t, []string{"enroll-token-1"}, stubOptions{now: conformanceNow()})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	identity, err := runner.Enroll(ctx, stub.bootstrap("enroll-token-1"))
	if err != nil {
		t.Fatalf("first enrollment: %v", err)
	}
	if identity.RunnerID != runnerID || identity.Certificate.Leaf == nil {
		t.Fatalf("enrollment returned no usable identity: %+v", identity)
	}
	if !identity.NotAfter.After(conformanceNow()) {
		t.Fatalf("issued identity is already expired: NotAfter=%s", identity.NotAfter)
	}

	if _, err := runner.Enroll(ctx, stub.bootstrap("enroll-token-1")); err == nil {
		t.Fatal("control plane accepted a reused one-use enrollment token")
	}
}

// TestEnrolledRunnerReconnectsWithShortLivedIdentity proves the runner opens the
// outbound session with the short-lived certificate it obtained at enrollment — never
// the bootstrap token — and can reconnect on the same identity to receive a lease.
func TestEnrolledRunnerReconnectsWithShortLivedIdentity(t *testing.T) {
	stub := newStubControlPlane(t, []string{"enroll-token-1"}, stubOptions{now: conformanceNow()})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	identity, err := runner.Enroll(ctx, stub.bootstrap("enroll-token-1"))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	session := stub.session(identity)

	for attempt := 1; attempt <= 2; attempt++ { // a second call is a reconnect on the same identity
		lease, err := session.ReceiveLease(ctx)
		if err != nil {
			t.Fatalf("receive lease on attempt %d: %v", attempt, err)
		}
		if lease.Fence != 7 || lease.LeaseID == "" || lease.RunID != contracts.RunID("run_conformance1") {
			t.Fatalf("attempt %d received an incomplete lease: %+v", attempt, lease)
		}
		if !strings.HasPrefix(lease.ImageDigest, "sha256:") || lease.Limits.WallTimeMS <= 0 {
			t.Fatalf("attempt %d lease omitted digest or bounds: %+v", attempt, lease)
		}
	}
}

// TestSessionFailsOnHandshakeTimeout proves that when the control plane never completes
// the lease handshake the runner surfaces an error and yields no lease — nothing
// downstream (no engine, no secret) can proceed past an incomplete handshake (ENG-001).
func TestSessionFailsOnHandshakeTimeout(t *testing.T) {
	stub := newStubControlPlane(t, []string{"enroll-token-1"}, stubOptions{now: conformanceNow(), silentHandshake: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	identity, err := runner.Enroll(ctx, stub.bootstrap("enroll-token-1"))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	handshakeCtx, handshakeCancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer handshakeCancel()
	lease, err := stub.session(identity).ReceiveLease(handshakeCtx)
	if err == nil {
		t.Fatalf("runner accepted an incomplete handshake and returned lease %+v", lease)
	}
	if lease.LeaseID != "" {
		t.Fatalf("runner returned a lease despite the handshake error: %+v", lease)
	}
}

// TestSessionRejectsProtocolMajorMismatch proves the runner refuses a lease offered on
// a different protocol major, before any lease work begins (ENG-001 version policy).
func TestSessionRejectsProtocolMajorMismatch(t *testing.T) {
	stub := newStubControlPlane(t, []string{"enroll-token-1"}, stubOptions{now: conformanceNow(), offerProtocol: "runner.v2"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	identity, err := runner.Enroll(ctx, stub.bootstrap("enroll-token-1"))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := stub.session(identity).ReceiveLease(ctx); err == nil {
		t.Fatal("runner accepted a lease offered on an unsupported protocol major")
	}
}

// TestFrameLedgerDedupesIdenticalRetransmit proves a duplicate frame id carrying the
// same payload hash is deduplicated (idempotent retransmit), while the same id with a
// changed payload is a protocol violation (ENG-002 stable request-id discipline).
func TestFrameLedgerDedupesIdenticalRetransmit(t *testing.T) {
	ledger := runner.NewFrameLedger()
	frame := contracts.EngineFrame{
		Protocol:  "engine.v1",
		ID:        contracts.FrameID("frm_req1"),
		Type:      "model.request",
		Sequence:  1,
		Time:      "2026-07-16T12:00:00Z",
		RunID:     contracts.RunID("run_conformance1"),
		AttemptID: contracts.AttemptID("att_conformance1"),
		Data:      map[string]any{"model_request_id": "mreq_abc", "prompt": "hello"},
	}

	duplicate, err := ledger.Admit(frame)
	if err != nil || duplicate {
		t.Fatalf("first admit: duplicate=%v err=%v, want false/nil", duplicate, err)
	}

	// A byte-identical retransmit (same id, same payload) is deduped, not rejected.
	retransmit := frame
	retransmit.Sequence = 2 // transport framing may differ; the semantic payload does not
	retransmit.Time = "2026-07-16T12:00:05Z"
	duplicate, err = ledger.Admit(retransmit)
	if err != nil || !duplicate {
		t.Fatalf("identical retransmit: duplicate=%v err=%v, want true/nil", duplicate, err)
	}

	// The same id with a changed payload is a protocol violation.
	conflicting := frame
	conflicting.Data = map[string]any{"model_request_id": "mreq_abc", "prompt": "tampered"}
	if _, err := ledger.Admit(conflicting); err == nil {
		t.Fatal("ledger accepted a reused frame id with a different payload hash")
	}
}

// TestSessionHasNoInboundListener proves the runner session is outbound-only: it
// exposes no listener configuration, so the private execution host never opens an
// inbound port (spec §28; ADR-0003 outbound-only transport).
func TestSessionHasNoInboundListener(t *testing.T) {
	sessionType := reflect.TypeOf(runner.Session{})
	listenerType := reflect.TypeOf((*net.Listener)(nil)).Elem()
	for i := 0; i < sessionType.NumField(); i++ {
		field := sessionType.Field(i)
		if strings.Contains(strings.ToLower(field.Name), "listen") || field.Type.Implements(listenerType) {
			t.Fatalf("Session exposes an inbound listener field %q", field.Name)
		}
	}
}

// TestIdentityRetainsNoBootstrapToken proves the one-use token never becomes part of
// the runner's persisted identity: it is consumed in the enrollment exchange and
// discarded, not carried on any Identity field (spec §28 — token is not a retained
// credential).
func TestIdentityRetainsNoBootstrapToken(t *testing.T) {
	identityType := reflect.TypeOf(runner.Identity{})
	for i := 0; i < identityType.NumField(); i++ {
		name := strings.ToLower(identityType.Field(i).Name)
		if strings.Contains(name, "token") {
			t.Fatalf("Identity exposes a token-bearing field %q", identityType.Field(i).Name)
		}
	}
}
