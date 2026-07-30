package fleet_test

// THE FENCE FOR THE APPROVE PATH (E24 T6).
//
// This repository has shipped a fully-built, fully-tested, UNREACHABLE security surface FOUR times:
// `CreateSlackConnection` (E19 T9), `DecideToolApproval` (E23), the pool key's composition root (E24 T3) and
// `Revoke`/`Resume` (E24 T5, where SAN-011's hard stop was catalogued in the UAT corpus and callable by
// nobody). A waiting room whose door nothing opens is the same shape and worse: it would hold every machine
// in a strict pool forever, and the operator's only way out would be to turn strict mode off.
//
// NONE OF IT CAN BE CAUGHT BY THE PROOFS ABOVE. Every component test in this epic constructs the store, the
// gateway and the router itself, so all of them pass against a binary that mounts no route and a CLI that
// has no verb. So the sites are pinned BY NAME, matched on the identifier and on the route STRING rather
// than on a line — a rename breaks this and a reformat does not.
//
// WHY THESE THREE. The route is what makes the decision reachable over HTTP at all. The CLI verb is what
// makes it reachable by a human, which is the whole subject of strict mode — an operator with only `curl` is
// an operator who will turn the feature off. And `awaitApproval`'s call site inside the connect handler is
// the ENFORCEMENT: without it the row still says `pending` and the machine takes work anyway, which is the
// worst of the three failures because everything an operator can SEE would look correct.
//
// `WithLifecycle` in the composition root is the fourth site and is deliberately NOT re-asserted here: E24
// T5's fence already pins it by name, and a second copy of that assertion is a second thing to keep in step.

import "testing"

// TestTheApprovalRouteIsMountedOnThePublicAPI pins the HTTP door.
func TestTheApprovalRouteIsMountedOnThePublicAPI(t *testing.T) {
	const router = "../../api/router.go"
	if !mentionsIdent(t, router, "approveRunner") {
		t.Errorf("%s does not name approveRunner: a strict pool would hold every machine it admitted a certificate to, forever, with no surface to admit one — and the only operator remedy would be to turn strict enrolment off", router)
	}
	// The URL as well as the handler, because a handler mounted on a different path is a handler the
	// documented surface and the CLI both miss.
	if !mentionsString(t, router, "POST /v1/runners/{runner_id}/approve") {
		t.Errorf("%s mounts no POST /v1/runners/{runner_id}/approve: the CLI and docs/operations/runner-fleet.md both name that path", router)
	}
}

// TestTheApprovalVerbIsReachableFromTheAdminCLI pins the human's route to it. `palai admin runner approve`
// is what an operator actually runs, and the arity table is what makes the verb exist at all: a subcommand
// missing from it is refused by execute() before any case is reached.
func TestTheApprovalVerbIsReachableFromTheAdminCLI(t *testing.T) {
	const cli = "../../../../cmd/cli/internal/admin/admin.go"
	if !mentionsString(t, cli, "runner/approve") {
		t.Errorf("%s does not carry `runner/approve` in positionalArity: `palai admin runner approve` is refused before it reaches a case, so the waiting room has no operator surface", cli)
	}
	if !mentionsString(t, cli, "approve") {
		t.Errorf("%s dispatches no approve subcommand", cli)
	}
}

// TestTheWaitingRoomIsEnforcedInTheConnectHandler pins the enforcement. It is the one of the three whose
// absence would leave every operator-visible surface looking correct — the row would say `pending`, the CLI
// would approve, the docs would be true — while the machine took work the moment it connected.
func TestTheWaitingRoomIsEnforcedInTheConnectHandler(t *testing.T) {
	const gateway = "../execution/runner_gateway.go"
	if !callsMade(t, gateway, map[string]string{"awaitApproval": ""})["awaitApproval"] {
		t.Errorf("%s: nothing calls awaitApproval — a `pending` machine would join its pool's rendezvous and be offered the next lease, while every surface an operator can read still said it was waiting", gateway)
	}
	// The adoption, without which the enforcement only ever applies to the session that enrolled: a machine
	// that reboots comes back through handleConnect with nothing in memory, and the ROW is the only thing
	// that knows it was never admitted.
	if !callsMade(t, gateway, map[string]string{"setPending": ""})["setPending"] {
		t.Errorf("%s: nothing calls setPending — the waiting room would be forgotten at every reboot of the machine and at every restart of the control plane", gateway)
	}
}

// mentionsString reports whether a file carries `want` as a string literal. It is the companion to
// mentionsIdent for the things that are not identifiers — a route pattern, a map key — and it reads the
// AST rather than the bytes so a mention inside a COMMENT cannot satisfy it.
func mentionsString(t *testing.T, path, want string) bool {
	t.Helper()
	return literals(t, path)[want]
}
