package execution_test

// The E24 T1 runner-registry proofs. They exist because two files in this tree independently ordered
// the same missing table: runner_gateway.go:73 ("there is no hosts/runners table in this tier … that
// is the SaaS/post-SH-0 upgrade path") and local_credentials.go:122 ("Bounding concurrent identities
// per token needs a runner registry the single-runner SH-0 topology does not have"). Neither file
// mentions the other.
//
// Both proofs drive the REAL gateway over REAL mTLS against a REAL packages/runner enrollment — the
// same fixture the shipped gateway proofs use — because the claim is about what the WIRE does, not
// about what a store method returns when a test calls it directly.

import (
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/runner"
)

// runnerDNSFor mirrors the gateway's runnerDNS (runner_gateway.go) from OUTSIDE the package: the
// suffix is part of the wire contract the runner and the CA both depend on, so restating it here
// means the proof breaks if the gateway ever changes it silently.
func runnerDNSFor(id string) string { return id + ".runners.palai.internal" }

// TestTwoRunnersAreTwoRowsInTheRegistry is §3.6 D13 stated as a measurement: the gateway has always
// ACCEPTED n runners (handleConnect has no count guard and `available` is an unbuffered channel), but
// the only place it recorded one was `identity atomic.Pointer[RunnerIdentity]` — a single slot, last
// writer wins. With two runners up, `palai local doctor` read whichever presented a certificate most
// recently and knew nothing at all about the other.
//
// The assertion is two ROWS with two distinct ids, not "the registry was called twice": a registry
// that overwrites is exactly the failure the single slot already was.
func TestTwoRunnersAreTwoRowsInTheRegistry(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("gw-token-1", "gw-token-2"))
	registry := newFakeRegistry()
	f.gateway.SetRegistry(registry)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i, token := range []string{"gw-token-1", "gw-token-2"} {
		if _, err := runner.Enroll(ctx, f.bootstrap(token)); err != nil {
			t.Fatalf("runner %d enroll: %v", i+1, err)
		}
	}

	rows := registry.snapshot()
	if len(rows) != 2 {
		t.Fatalf("two runners enrolled but the registry carries %d row(s): the gateway records at most one identity", len(rows))
	}
	if rows[0].ID == rows[1].ID {
		t.Fatalf("both rows carry runner id %q: the second enrollment overwrote the first", rows[0].ID)
	}
	for _, row := range rows {
		// The row has to be addressable and attributable: an inventory whose entries carry no pool and
		// no certificate identity answers none of the questions it exists for.
		if row.DNS == "" || row.PoolID == "" {
			t.Fatalf("registry row %+v is missing its certificate identity or its pool", row)
		}
		// The client-supplied name survives as a LABEL — operator-facing, never an identity.
		if row.Label != gwRunnerID {
			t.Fatalf("registry row %q records label %q, want the enrolling machine's own name %q", row.ID, row.Label, gwRunnerID)
		}
	}
}

// TestTwoMachinesClaimingOneRunnerIDGetSeparateIdentities is the §2 invariant "RUNNER ID'Sİ SUNUCU
// MINTLER" stated as a measurement. Two machines enrol claiming the SAME runner_id — which is not a
// hypothetical: deploy/compose/runner-entrypoint.sh:10 exports PALAI_RUNNER_ID="runner-local"
// unconditionally, so `docker compose up --scale runner=3` IS three machines sending one name.
//
// Each must get its OWN identity, and neither may invalidate the other's certificate. Before E24 the
// gateway signed whatever name the enrolling party asked for (runnerDNS(request.RunnerID),
// runner_gateway.go:247, no verification), so both certificates carried
// `runner-local.runners.palai.internal` and the two machines were indistinguishable to every later
// decision — a revoke, a drain, a placement.
//
// The assertion is deliberately on the CERTIFICATE rather than on a registry row: a registry that
// records two rows while the CA still mints one name has proved nothing, because the certificate is
// what every subsequent request authenticates with.
func TestTwoMachinesClaimingOneRunnerIDGetSeparateIdentities(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("gw-token-1", "gw-token-2"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Both machines send the SAME runner_id — the compose default.
	first, err := runner.Enroll(ctx, f.bootstrap("gw-token-1"))
	if err != nil {
		t.Fatalf("first machine enroll: %v", err)
	}
	second, err := runner.Enroll(ctx, f.bootstrap("gw-token-2"))
	if err != nil {
		t.Fatalf("second machine enroll: %v", err)
	}

	firstDNS, secondDNS := certDNS(t, first), certDNS(t, second)
	if firstDNS == secondDNS {
		t.Fatalf("two machines that both claimed %q were issued the SAME identity %q: the client named itself and the CA signed it",
			gwRunnerID, firstDNS)
	}
	// The server minted them, so neither can be the name the client asked for.
	if firstDNS == runnerDNSFor(gwRunnerID) || secondDNS == runnerDNSFor(gwRunnerID) {
		t.Fatalf("an issued identity carries the CLIENT-supplied name (%q, %q): the runner_id is a label, not an identity",
			firstDNS, secondDNS)
	}
	// The enrol response carries the server's id back, and it is the one the certificate is for —
	// this is the backward-compatibility half: a runner that sent its own id still learns the real one.
	if first.RunnerID == second.RunnerID {
		t.Fatalf("both machines came away holding runner id %q", first.RunnerID)
	}
	if runnerDNSFor(first.RunnerID) != firstDNS || runnerDNSFor(second.RunnerID) != secondDNS {
		t.Fatalf("the returned runner id does not match the issued certificate: %q vs %q, %q vs %q",
			first.RunnerID, firstDNS, second.RunnerID, secondDNS)
	}

	// Neither certificate invalidated the other: both still renew over their OWN mTLS identity.
	for name, identity := range map[string]runner.Identity{"first": first, "second": second} {
		if _, err := runner.Renew(ctx, identity, f.renewConfig()); err != nil {
			t.Fatalf("%s machine could not renew its identity after the other enrolled: %v", name, err)
		}
	}
}

// certDNS is the DNS identity an issued runner certificate carries — what the gateway signed.
func certDNS(t *testing.T, identity runner.Identity) string {
	t.Helper()
	leaf := identity.Certificate.Leaf
	if leaf == nil {
		t.Fatal("issued identity carries no parsed leaf certificate")
	}
	if len(leaf.DNSNames) != 1 {
		t.Fatalf("issued certificate carries %d SANs, want exactly 1: %v", len(leaf.DNSNames), leaf.DNSNames)
	}
	return leaf.DNSNames[0]
}
