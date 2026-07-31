// The E28 EXIT gate's bundle half (plan §T4): the fleet-console-0.1.0 evidence bundle, its generator, and the
// shrink guards in both directions. Everything here is Docker-free pure logic, so it rides `make verify`; the
// browser-bound and Docker-bound backing legs are driven from journey_test.go through scripts/uat/fleet-console.
//
// WHAT THIS BUNDLE CLAIMS, and it is deliberately narrower than "the fleet is manageable": that a POOL CAN BE
// BORN — with a posture and with the waiting room switched on, through the public API, the CLI and a screen,
// where before this epic no code path could write either — that a machine enrols into a strict pool, lands
// `pending` and is ADMITTED FROM THE CONSOLE; that a server-minted value is shown once and is found in none of
// five sites scanned after decoding; that a policy form writes the WHOLE document so an approver list survives
// a write that named another field; that every declared route is axe-scanned in both colour schemes; that
// every irreversible action is behind an alertdialog and every reversible one is not; and that the screens
// state their own ceilings by gap id.
//
// AND THE HONEST CEILING, WHICH IS THE MOST IMPORTANT SENTENCE IN THIS PACKAGE: NO REAL MAC WAS RENTED,
// ENROLLED OR RAN A BUILD. Every machine here is a fake runner built from the shipped enrolment package, every
// browser leg drives Chromium against a fixture upstream or a compose stack on one box, and `FLT-P15` stands —
// a Mac pool is now creatable and STILL DOES NOT RUN `xcodebuild` ON A MAC. uat.FleetConsolePeer is
// structurally the literal "fake". NO TIER ADVANCES: `console` closes PREVIEW.
package fleetconsole

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"crypto/sha256"
	"encoding/hex"

	"github.com/palgroup/palai/tests/uat"
)

func bundleDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "evidence", "releases", uat.FleetConsoleBundle)
}

// baselineManifest is the committed E26 EXIT bundle. E28 forked from `main` after background-execution-0.1.0
// merged, so the inherited case set is derived from it rather than retyped — and that is also why an E28
// bundle owes the background proof, which PromoteGateFor's composition then judges.
const baselineManifest = uat.BackgroundBundle

// fleetConsoleAnchorCaseID is the release-level entry the fleet-console proof hangs off. Like E26-BACKGROUND,
// E25-ADMIN-CONSOLE, E23-TOOL-APPROVAL and the rest it is NOT a UAT case (it has no tests/uat/cases
// directory): "every irreversible action in this console is behind an alertdialog" is not a behaviour one case
// asserts, it is an observation over a whole surface.
const fleetConsoleAnchorCaseID = "E28-FLEET-CONSOLE"

// The anchors carried forward from the releases underneath. A bundle carrying E26 cases must carry the
// background anchor, one carrying E25 cases the admin-console anchor, and so down — so all seven ride along
// unchanged, and this release is judged on the tiers it did NOT move.
const (
	backgroundAnchorCaseID = "E26-BACKGROUND"
	consoleAnchorCaseID    = "E25-ADMIN-CONSOLE"
	approvalAnchorCaseID   = "E23-TOOL-APPROVAL"
	shipAnchorCaseID       = "E22-CODE-AND-SHIP"
	memoryAnchorCaseID     = "E21-TOOLS-MEMORY"
	surfaceAnchorCaseID    = "E20-SURFACE"
	wiringAnchorCaseID     = "E19-WIRING"
	tierAnchorCaseID       = "E17-TIER"
)

// hashParts reproduces the tests/uat construction (sha256 of each part followed by a NUL, hex-encoded,
// sha256:-prefixed) so this generator and the verifier derive the same re-derivable values.
// ponytail: the same 6-line copy the background / admin-console / fleet / tool-approval gates keep. A drift
// between this copy and the verifier's shows up immediately as a bundle whose checksums do not recompute.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// syntheticImageDigest is an obviously-unservable digest, the background / admin-console / fleet precedent:
// E28's legs are browser-tier against a fixture upstream and component-tier against real PostgreSQL, so there
// is no engine image to name and the shape verifier's required field carries a value no registry could serve.
var syntheticImageDigest = "sha256:" + strings.Repeat("e28", 21) + "e"

// newCaseIDs is the four ids E28 opened, read from the CANONICAL table rather than retyped so this bundle and
// the orphan guards can never disagree about which cases exist.
func newCaseIDs() []string { return uat.FleetConsoleCaseIDs }

// buildFleetConsoleManifest assembles the bundle. Every inherited case comes from the committed E26 baseline;
// every proof body comes from a canonical uat table. Nothing is typed twice.
func buildFleetConsoleManifest(t *testing.T) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "evidence", "releases", baselineManifest, "manifest.json"))
	if err != nil {
		t.Fatalf("read the %s baseline (this release's inherited case set is derived from it, never retyped): %v", baselineManifest, err)
	}
	var base struct {
		Cases []map[string]any `json:"cases"`
	}
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatalf("decode the baseline: %v", err)
	}

	anchor := uat.FleetConsoleContractsDigest()
	cases := make([]map[string]any, 0, len(base.Cases)+len(newCaseIDs())+1)

	for _, bc := range base.Cases {
		id, _ := bc["id"].(string)
		if id == "" {
			t.Fatalf("the baseline carries a case with no id: %v", bc)
		}
		c := map[string]any{}
		for k, v := range bc {
			c[k] = v
		}
		runID := "run_e28_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c["run_id"] = runID
		c["checksum"] = hashParts(id, runID, anchor)
		c["db_assertions"] = append(append([]string{}, toStrings(t, id, bc["db_assertions"])...),
			"E28 FLEET-CONSOLE RELEASE: this case's LOCAL seam is unchanged from "+baselineManifest+
				" — what changed is that the FLEET this deployment can place a run on now has a BIRTH PATH and "+
				"a screen. Before this epic `POST /v1/runner-pools` was not in the route table, "+
				"`grep \"UPDATE runner_pools\" | grep -v _test` answered zero, and the one statement that wrote "+
				"a pool row wrote 'default', 'sandboxed-linux' and false as LITERALS — so a second pool could "+
				"not exist, an `unsandboxed-host` (rented Mac) pool could not exist, and `strict_enrollment` "+
				"could not be switched on outside two test files. E24 T6's approve route was therefore deciding "+
				"a state NO OPERATOR COULD PRODUCE. WHAT THIS CHANGES FOR THIS CASE: nothing about its own "+
				"seam. Every route it exercises is byte-unchanged, the relay took not one line, and no "+
				"capability was added — a member on CapabilityTierOrder would move CapabilityClaimsDigest and "+
				"redden every case checksum in every committed bundle, which is why E28 opened none. AND THE "+
				"CEILING THAT OUTRANKS ALL OF IT: NO REAL MAC WAS RENTED, ENROLLED OR RAN A BUILD. Every "+
				"machine in this release is a fake runner built from the shipped enrolment package, and "+
				"`FLT-P15` stands — a `lease.offer` carries the ENGINE while every TOOL still executes in the "+
				"control plane's own process, so a Mac pool is creatable and still does not run `xcodebuild` "+
				"on a Mac. Nor is there a DEPLOYED console (compose is not a deployment) or a manual "+
				"screen-reader pass, and E28 added exactly the two things a scanner is weakest at: a modal "+
				"dialog whose focus trap is a FEEL, and a one-time value announcement whose correctness is a "+
				"TIMING. So this case's §6 operator legs are untouched and its capability's tier does not move.")
		cases = append(cases, c)
	}

	// ---- the four ids E28 OPENED --------------------------------------------------------------------
	for _, id := range newCaseIDs() {
		runID := "run_e28_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c := map[string]any{
			"id": id, "status": "PASS", "proof_class": caseProofClass(t, id),
			"run_id":              runID,
			"image_digest":        syntheticImageDigest,
			"provider_request_id": "prov_e28_deterministic",
			"mtls_enroll":         "component-tier: a REAL mTLS enrolment against a real PostgreSQL and the shipped gateway, by a FAKE runner built from the enrolment package — no rented Mac is claimed anywhere in this release",
			"terminal":            map[string]any{"type": "response.completed", "count": 1},
			"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
			"db_assertions": []string{
				caseAssertion(t, id),
				"OPENED BY E28 UNDER THE `FLC-` PREFIX, and that prefix is a GATE decision rather than a naming " +
					"preference: `FLT-` (E24) and `CON-` (E25) are BOTH already inside extensionIDPrefixes with " +
					"their own owner lists and their own promote gates, so an `FLT-006` would match " +
					"carriesE24FleetCase in PromoteGateFor and dispatch to a WEAKER gate that knows nothing about " +
					"this epic's birth-path counter, its key-value scan, its approver-list equality, its " +
					"alertdialog sweep or its ceiling ids — and would pass a bundle that dropped all five. E20, " +
					"E21, E22, E25 and E26 each paid for that reasoning once. AND THE OTHER HALF WAS OPEN IN THIS " +
					"VERY EPIC: `FLC-` was in NO prefix list while FLC-001, FLC-002 and FLC-003 shipped in T1, T2 " +
					"and T3, so all three escaped tests/uat/extensions — the ONLY sweep in the tree that walks " +
					"tests/uat/cases — and every UAT package reported ok. Both directions are closed here: `FLC-` " +
					"is in extensionIDPrefixes, and tests/uat/fleet-console refuses an id in " +
					"uat.FleetConsoleCaseIDs with no directory or with a declared proof that is not in the tree. " +
					"THE SECOND DIRECTION IS THE ONE A DIRECTORY WALK COULD NOT FIND: FLC-004's directory did not " +
					"exist at all, because T2 and T3 built the two halves of the confirmation rule and neither " +
					"opened a case for the rule itself",
				"NOT in uat.CapabilityClaims and therefore NOT an input to any tier recompute — which is the " +
					"whole tier argument in one line: A MANAGEMENT SCREEN IS NOT EVIDENCE ABOUT THE PLANE IT " +
					"MANAGES. E22 did not advance a tier for DELETING a boundary, E23 for ADDING one, E24 for " +
					"adding a PLANE, E25 for adding a CONFIGURATION SURFACE, E26 for changing the SHAPE of a " +
					"call, and E28 does not for putting a fleet on a screen. `console` closes PREVIEW in the epic " +
					"that made it BIGGER — two pages, two components, three new routes — with §6 leg 1 (a real " +
					"rented Mac) and leg 3 (a deployed console with a manual assistive-technology pass) both " +
					"open, and leg 3 GREW here because a modal dialog and a one-time announcement are precisely " +
					"where axe is blind",
				"HONEST CEILING, AND THE FIRST PART IS THE ONE A READER OF A FLEET SCREEN WILL ASSUME THE " +
					"OPPOSITE OF. NO REAL MAC WAS RENTED, ENROLLED OR RAN A BUILD: every machine here is a fake " +
					"runner built from the shipped enrolment package, driven over a real mTLS wire against a real " +
					"PostgreSQL on ONE box. `FLT-P15` STANDS AND BOUNDS EVERYTHING ELSE — a `lease.offer` carries " +
					"the ENGINE, every TOOL still executes in the control plane's own process, so this release " +
					"makes a Mac pool CREATABLE and does not make a Mac run `xcodebuild`. The screens say so " +
					"themselves, in words, with the gap id, and a test counts that. `FLT-P2` stands: a declared " +
					"posture is COMPARED and never verified, so an `unsandboxed-host` pool does not stop a Linux " +
					"container calling itself one. `FLT-P13` stands: an approval admits an ENROLMENT rather than " +
					"a MACHINE, so a strict pool asks again after every reboot. And `FLC-P1`/`FLC-P2`/`FLC-P4` " +
					"record what this console does not do: the policy form is last-writer-wins, RevealOnce makes " +
					"no claim about browser MEMORY, and creating a pool proves nothing about a machine being in " +
					"it. §6 legs 1-6 are operator work and are NEVER claimed here",
			},
		}
		c["checksum"] = hashParts(id, runID, anchor)
		cases = append(cases, c)
	}

	// ---- the fleet-console anchor: the birth path, the waiting room, the value and the absences ---------
	fleetConsoleCase := map[string]any{
		"id": fleetConsoleAnchorCaseID, "status": "PASS", "proof_class": "e2e-deterministic",
		"run_id":              "run_e28_fleet_console_journey",
		"image_digest":        syntheticImageDigest,
		"provider_request_id": "prov_e28_deterministic",
		"mtls_enroll":         "component-tier: a REAL mTLS enrolment against a real PostgreSQL and the shipped gateway, by a FAKE runner built from the enrolment package — no rented Mac is claimed anywhere in this release",
		"terminal":            map[string]any{"type": "response.completed", "count": 1},
		"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		"db_assertions": []string{
			"RELEASE-LEVEL entry, not a UAT case (it has no tests/uat/cases directory): \"a fleet has a birth path\", \"a minted value survives in none of five sites\" and \"every irreversible action in this console is behind an alertdialog\" are not behaviours one case asserts, they are observations over a whole surface — the E26-BACKGROUND / E25-ADMIN-CONSOLE / E23-TOOL-APPROVAL / E22-CODE-AND-SHIP / E21-TOOLS-MEMORY / E20-SURFACE / E19-WIRING / E17-TIER precedent",
			"THE FIRST CROWN CLAIM IS THAT A FLEET NOW HAS A BIRTH PATH, AND THE MEASUREMENT MATTERS MORE THAN THE FEATURE. E24 shipped EIGHT TASKS over a fleet whose pools could not be created: `POST /v1/runner-pools` was not in the route table (`api/router.go` mounted four reads and four writes and no create), `grep -rn \"UPDATE runner_pools\" storage/ apps/ cmd/ packages/ | grep -v _test | wc -l` answered ZERO, and the ONE statement that wrote a pool row — `InsertDefaultRunnerPool` — wrote 'default', 'sandboxed-linux' and false as LITERALS. The consequence was three-layered: a second pool could not exist, an `unsandboxed-host` pool could not exist so the column the rented Mac was added FOR had one attainable value, and `strict_enrollment` could not be switched on — which left E24 T6's `POST /v1/runners/{runner_id}/approve`, correctly written and correctly gated on `approve` rather than `provision`, DECIDING A STATE NO OPERATOR COULD PRODUCE. Two code comments handed pool creation to \"T5/T6\" and both shipped without it; both are corrected and the new sentences name NO owner, because a comment can state what is true and cannot schedule work. The proof is not that a route exists: it is a pool created with posture `unsandboxed-host` and strict enrolment on, a machine enrolling into it over a real mTLS wire, landing `pending`, a Dial answering `ErrPoolHasNoRunner` so the waiting room is STRUCTURAL rather than a comparison a later change could soften, and that machine then admitted — the counter carries the posture SET rather than a pool count, because a count alone is satisfied by the seed every tenant gets at birth",
			"THE SECOND CROWN CLAIM IS THAT A MACHINE IS ADMITTED FROM A SCREEN, AND `admitted_from` IS CHECKED BY VALUE FOR THAT REASON. E24 T6 already proved the approve route from a component test; what had never happened in this tree is an operator admitting a machine FROM THE CONSOLE, into a pool the console itself created — an epic whose crown claim is a screen cannot certify it from a CLI transcript. The waiting room lives on /fleet rather than on /approvals for the SERVER's own reason, quoted in `api/runners.go`: A MACHINE ENROLMENT HAS NO REQUEST HASH TO BIND TO, and `/v1/approvals`'s decision route requires one. So the two screens name each other rather than one absorbing the other, and each says what it does NOT cover while the other's condition is actually present",
			"THE THIRD CROWN CLAIM IS AN EQUALITY RATHER THAN A ZERO, AND A SECURITY INCIDENT SITS BEHIND IT. `PATCH /v1/projects/{project_id}` is an ASSIGNMENT — `UpdateProjectPolicy` marshals the input struct and hands the bytes over with NO merge — and its list fields are nil-able Go slices, so a request naming only `pool` stores `approvers: null`. `HIL-P11` measured that an empty approver list is PERMISSIVE: it admits EVERY principal. The most innocent button an admin console can carry — put this project on the Mac pool — would therefore have opened the approval gate to everyone, and the operator's own action would have SUCCEEDED while it happened. Shown RED against the naive form before anything was fixed. The counter is the approver entries BEFORE the write and AFTER it, and the REQUEST's field count is carried beside the stored outcome — because a server that MERGED would let \"the approvers survived\" pass over a form still sending one field, which is the build this claim exists to refuse",
			"THE FOURTH CROWN CLAIM IS THAT A SERVER-MINTED VALUE SURVIVES NOWHERE, MEASURED IN FIVE SITES AFTER DECODING. This is E25 T4's `SecretField` pointed the OTHER WAY and the plan measured why they are not variants: `SecretField` takes what a HUMAN writes and reads the DOM node once; a pool key arrives in a FETCH BODY and is therefore already in JS memory before any component exists, so a password input, `new-password` and a take-once read are all inapplicable. What they share is one rule in opposite directions — the bytes exist in exactly ONE place. The sites are the response body, the DOM, both web storages, the URL and a LATER response, and the fifth is the one the other four pass over: a value can be gone from the document and still be served back by a list call, which is what `poolKeyView(item, false)` prevents server-side and what the browser half has to mirror rather than assume. EVERY SITE NAMES A PROBE THE SAME SCAN FOUND, because a scan over an empty haystack reports the same zero as a clean one — and the web-storage probe is SEEDED rather than found, which is itself a measurement: this console writes nothing to either storage, so there was no naturally-occurring token to point at",
			"THE FIFTH CLAIM IS THAT AN IRREVERSIBLE ACTION GETS A DIFFERENT CONFIRMATION FROM A REVERSIBLE ONE, ON A PUBLISHED CRITERION AND ACROSS THE WHOLE SET. WCAG 2.2 SC 3.3.4 asks for ONE of three legs; `cordon`/`resume` satisfies leg 1 VERBATIM (`Resume` clears it) so a `window.confirm` discharges it and a modal would be code with no obligation behind it, while `revoke` cannot reach leg 1 at all (\"a revoked runner identity is decommissioned, not paused\") and leg 3's word REVIEWING is a DATA CALL: the dialog cannot open without `GET /v1/runners/{id}`, because `ActiveLeases` exists only on the single read and `nil` and `0` are different answers. The sweep is over the SOURCE of every page rather than over the actions a spec named, and it refuses BOTH directions — a build that put every action behind an alertdialog would not be more compliant, it would have stopped distinguishing, and the next reader converting the remaining `window.confirm` calls for consistency is the change this refuses",
			"THE SIXTH CLAIM IS THAT THE SCREENS WRITE THEIR OWN CEILINGS BY GAP ID, and the id is what makes it countable: \"the page warns about remote execution\" is a sentence a reviewer judges, while \"the page renders `FLT-P15`\" is a string a source scan finds and a deletion reddens. Four ids are required — `FLT-P15`, `FLT-P12`, `HIL-P11`, `CON-P2` — and the first is the row known-gaps itself says to read before any other `FLT-P*` because it bounds what all of them are worth. A fleet page showing three active machines and saying nothing about where a tool EXECUTES is silent truncation on the fleet surface",
			"NO TIER ADVANCES, and the reason is a RULE plus one fact specific to this epic. THE RULE: a management screen is not evidence about the plane it manages — E22 did not advance a tier for deleting a boundary, E23 for adding one, E24 for adding a plane, E25 for adding a configuration surface, E26 for changing the shape of a call. THE FACT: with `FLT-P15` open, a fleet screen IMPLIES MORE THAN IT PROVES. A page listing pools, keys and machines reads as \"these machines do the work\"; they do not, and the screens say so in words. RAISING THE TIER WOULD FORMALISE THE IMPLICATION THE SENTENCES EXIST TO PREVENT. `slack` closes preview, `knowledge-vector` disabled, `apple-build` disabled, `console` PREVIEW — and `console` closes preview in the epic that made it bigger, which is the second consecutive time. WHAT WOULD HAVE HAD TO BE TRUE to move one: a deployed console behind a real origin with a manual assistive-technology pass, or the execution relay that makes a Mac pool actually run `xcodebuild` on a Mac. Neither exists",
		},
		"fleet_console_claim": "fleet console proven: a pool is created with a posture and with the waiting room switched on through the public API, the CLI and a screen — none of which any code path could do before this epic — a machine enrols into a strict pool over real mTLS, lands `pending` and is ADMITTED FROM THE CONSOLE, a server-minted value is shown once and found in none of five sites scanned after decoding, a policy write preserves as many approver entries as it started with over requests carrying all five fields, every route lib/routes.ts declares is axe-scanned in both colour schemes, every irreversible action is behind an alertdialog while every reversible one is not, and four ceiling gap ids are on screen — with NO rented Mac, NO deployed console and NO manual screen-reader pass claimed",
		"fleet_console_proof": canonicalFleetConsoleProof(),
	}
	fleetConsoleCase["checksum"] = hashParts(fleetConsoleAnchorCaseID, "run_e28_fleet_console_journey", anchor)
	cases = append(cases, fleetConsoleCase)

	manifest := map[string]any{
		"release":     uat.FleetConsoleBundle,
		"git_sha":     "5d41bc6",
		"api_version": "v1",
		// THE CHAIN HEAD, AND IT IS E26's — the field names the schema a bundle was CAPTURED AGAINST rather
		// than one the release added, which is why extensions-0.1.0, integration-wiring-0.1.0 and
		// slack-agent-surface-0.1.0 all carry 000040. E28 OPENED NO MIGRATION, which plan §0.4 called the
		// easiest sentence in the plan to falsify and which held: `runner_pools` already carried posture, os,
		// arch, strict_enrollment and its UNIQUE index from 000045, so what T1 was missing was the code that
		// wrote to them and not a column. An empty string here is refused by the shape verifier, and rightly:
		// "this release captured against no schema" is not a thing a manifest can honestly say.
		"migration":   "000047_background_tasks",
		"captured_at": "2026-07-31T12:00:00Z",
		"maturity":    "rc",
		"cases":       cases,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return buf.Bytes()
}

// canonicalFleetConsoleProof is the proof body the bundle carries. Every BYTE field is READ FROM A CANONICAL
// SOURCE in tests/uat rather than retyped here, and every declared COUNT is computed by the same sweep the
// gate runs — so a proof that declared a number the ledger does not support could not be written by this
// generator at all, let alone verify.
func canonicalFleetConsoleProof() uat.FleetConsoleProof {
	created, postures, _, err := uat.SweepFleetConsolePools(json.RawMessage(uat.FleetConsolePoolLedger))
	if err != nil {
		panic("the pool ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	pending, fromConsole, _, err := uat.SweepFleetConsoleWaitingRoom(json.RawMessage(uat.FleetConsoleWaitingRoomLedger))
	if err != nil {
		panic("the waiting-room ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	hits, _, _, err := uat.SweepFleetConsoleKeyScan(json.RawMessage(uat.FleetConsoleKeyScanLedger))
	if err != nil {
		panic("the key-scan ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	before, after, _, err := uat.SweepFleetConsolePolicyWrites(json.RawMessage(uat.FleetConsolePolicyLedger))
	if err != nil {
		panic("the policy ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	declared, scanned, _, err := uat.SweepFleetConsoleRoutes(json.RawMessage(uat.FleetConsoleRouteLedger))
	if err != nil {
		panic("the route ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	guarded, native, _, err := uat.SweepFleetConsoleActions(json.RawMessage(uat.FleetConsoleActionLedger))
	if err != nil {
		panic("the action ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	onScreen, _, err := uat.SweepFleetConsoleCeilings(json.RawMessage(uat.FleetConsoleCeilingLedger))
	if err != nil {
		panic("the ceiling ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	compared, err := uat.SweepFleetConsoleConformance(json.RawMessage(uat.FleetConsoleConformanceLedger))
	if err != nil {
		panic("the conformance ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	return uat.FleetConsoleProof{
		Peer: uat.FleetConsolePeer,

		// EVERY COUNT BELOW COMES FROM THE SWEEP, NEVER FROM A LITERAL. A literal here would be the one place
		// this proof could disagree with the bytes it carries, and half of these are EQUALITIES rather than
		// zeros — an equality written by hand is an equality nobody checked.
		PoolLedger:      json.RawMessage(uat.FleetConsolePoolLedger),
		PoolsCreated:    created,
		PosturesCreated: postures,

		WaitingRoomLedger:              json.RawMessage(uat.FleetConsoleWaitingRoomLedger),
		MachinesPending:                pending,
		MachinesAdmittedFromTheConsole: fromConsole,

		KeyScanLedger:  json.RawMessage(uat.FleetConsoleKeyScanLedger),
		KeyValuesFound: hits,

		PolicyLedger:          json.RawMessage(uat.FleetConsolePolicyLedger),
		ApproverEntriesBefore: before,
		ApproverEntriesAfter:  after,

		RouteLedger:      json.RawMessage(uat.FleetConsoleRouteLedger),
		RoutesDeclared:   declared,
		RoutesAxeScanned: scanned,

		ActionLedger:                           json.RawMessage(uat.FleetConsoleActionLedger),
		IrreversibleActionsBehindAnAlertDialog: guarded,
		ReversibleActionsOnTheNativeConfirm:    native,

		CeilingLedger:    json.RawMessage(uat.FleetConsoleCeilingLedger),
		CeilingsOnScreen: onScreen,

		ConformanceLedger:              json.RawMessage(uat.FleetConsoleConformanceLedger),
		ConformanceCollectionsCompared: len(compared),

		Contracts:       uat.FleetConsoleContracts,
		ContractsDigest: uat.FleetConsoleContractsDigest(),
	}
}

func toStrings(t *testing.T, id string, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: db_assertions is not a list: %T", id, v)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("%s: a db_assertion is not a string: %T", id, e)
		}
		out = append(out, s)
	}
	return out
}

// TestCommittedFleetConsoleBundleIsTheGeneratorOutput pins the committed bundle to the tree: it must be
// BYTE-identical to this generator's output, so a contract-ledger change, a ledger-row change, a case-set
// change or a baseline change cannot leave a stale bundle verifying green.
// Regenerate with: PALAI_WRITE_FLEET_CONSOLE_BUNDLE=1 go test ./tests/uat/fleet-console/
func TestCommittedFleetConsoleBundleIsTheGeneratorOutput(t *testing.T) {
	want := buildFleetConsoleManifest(t)
	path := filepath.Join(bundleDir(t), "manifest.json")

	if os.Getenv("PALAI_WRITE_FLEET_CONSOLE_BUNDLE") == "1" {
		if err := os.MkdirAll(bundleDir(t), 0o755); err != nil {
			t.Fatalf("create release dir: %v", err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatalf("write bundle: %v", err)
		}
		t.Logf("wrote %s", path)
		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the committed bundle: %v (regenerate with PALAI_WRITE_FLEET_CONSOLE_BUNDLE=1)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the committed %s bundle is not this generator's output — regenerate with PALAI_WRITE_FLEET_CONSOLE_BUNDLE=1", uat.FleetConsoleBundle)
	}
}

// TestFleetConsoleReleaseVerifiesClean runs the committed bundle through the SHIPPED verifier and requires
// 0 failed / 0 missing / 0 secret. It is the `make evidence-verify RELEASE=fleet-console-0.1.0` gate,
// in-process.
func TestFleetConsoleReleaseVerifiesClean(t *testing.T) {
	summary, err := uat.VerifyRelease(bundleDir(t), nil)
	if err != nil {
		t.Fatalf("verify the fleet-console release: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("the fleet-console bundle did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatal("the fleet-console bundle verified 0 passed cases — a zero-case bundle is not a clean bundle")
	}
	t.Logf("%s: %s", uat.FleetConsoleBundle, summary.String())
}

// TestTheFourNewCasesAreInTheBundle is the shrink guard the E18 T8 sweep taught: a release that OPENED four
// ids must carry all four, or the ids exist in the tree with no bundle certifying them.
func TestTheFourNewCasesAreInTheBundle(t *testing.T) {
	var m struct {
		Cases []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"cases"`
	}
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}
	carried := map[string]string{}
	for _, c := range m.Cases {
		carried[c.ID] = c.Status
	}
	for _, id := range uat.FleetConsoleCaseIDs {
		if status, ok := carried[id]; !ok {
			t.Errorf("%s is a case this epic OPENED but the bundle that certifies it does not carry it", id)
		} else if status != "PASS" {
			t.Errorf("%s is carried as %q; this release claims it green", id, status)
		}
	}
	for _, anchor := range []string{fleetConsoleAnchorCaseID, backgroundAnchorCaseID, consoleAnchorCaseID, approvalAnchorCaseID, shipAnchorCaseID, memoryAnchorCaseID, surfaceAnchorCaseID, wiringAnchorCaseID, tierAnchorCaseID} {
		if _, ok := carried[anchor]; !ok {
			t.Errorf("%s is not in the bundle — every anchor underneath this release rides along, so the release is judged on the tiers it did NOT move", anchor)
		}
	}
}

// TestTheBundleClaimsNoRentedMacAndNoDeployedConsole is this release's version of the absence guard, and it
// exists because both absences are things a reader of a FLEET screen will assume the opposite of. A Mac that
// ran a build and a console somebody deployed are the two features somebody will believe shipped here; neither
// did.
//
// STRUCTURAL, NOT A SUBSTRING SCAN — E24's own note on this shape: a byte scan for "Mac" would fail on this
// bundle's honest paragraphs explaining that there was not one.
func TestTheBundleClaimsNoRentedMacAndNoDeployedConsole(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	var m struct {
		Cases []struct {
			ID           string                 `json:"id"`
			Claim        string                 `json:"fleet_console_claim"`
			FleetConsole *uat.FleetConsoleProof `json:"fleet_console_proof"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}
	proofs := 0
	for _, c := range m.Cases {
		if c.FleetConsole == nil {
			continue
		}
		proofs++
		if c.FleetConsole.Peer != uat.FleetConsolePeer {
			t.Errorf("%s carries peer %q — every machine in this release is a FAKE runner built from the shipped enrolment package, and a field that could say \"real\" is a field that could claim a rented Mac", c.ID, c.FleetConsole.Peer)
		}
		// The pool ledger's vocabulary is CLOSED by the sweep (`seed` is not a creation surface, and only two
		// postures exist), so this asserts the closure still holds rather than searching prose.
		if _, _, _, err := uat.SweepFleetConsolePools(c.FleetConsole.PoolLedger); err != nil {
			t.Errorf("%s: the pool ledger no longer sweeps: %v", c.ID, err)
		}
		// AND THE CEILING IS IN THE PROOF ITSELF, by id: `FLT-P15` is the row that bounds what every other
		// fleet claim is worth, so a bundle whose screens stopped stating it is a bundle claiming more.
		if !slices.Contains(c.FleetConsole.CeilingsOnScreen, "FLT-P15") {
			t.Errorf("%s: the proof does not carry FLT-P15 among the ceilings on screen — that is the row known-gaps says to read before any other FLT-P*, because it bounds what all of them are worth", c.ID)
		}
	}
	if proofs != 1 {
		t.Errorf("the bundle carries %d fleet-console proof(s), want exactly 1 — a second could ride behind an honest one", proofs)
	}
}
