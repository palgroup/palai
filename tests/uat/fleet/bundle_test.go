// Package fleet holds the E24 EXIT gate (plan §T8): the runner-fleet-0.1.0 evidence bundle, the refusal
// matrix, the fleet promote gate, the five UAT cases this epic OPENED, the §3.6 D4 belief sweep, the
// production-caller sweep over E24's new exported surface, and the operator entry point collapsed to one
// command. Everything here is Docker-free pure logic, so it rides `make verify`; the Docker-bound journey is
// driven from journey_test.go through scripts/uat/fleet.
//
// WHAT THIS BUNDLE CLAIMS, and it is deliberately narrower than "there is a cloud fleet now": that several
// machines can exist as an IDENTIFIED, tenant-scoped, pooled and revocable inventory whose ids the SERVER
// mints; that which pool a run executes in is a configuration and placement into one is a REFUSAL rather
// than a preference; that a run with no capacity PARKS instead of dying and wakes when a machine joins; that
// revoking a pool KEY leaves the machines that came in on it RUNNING; and that revoking a MACHINE outlives
// the control-plane process that decided it.
//
// AND THE HONEST CEILING, WHICH IS THE MOST IMPORTANT SENTENCE IN THIS PACKAGE. E24 T7 — the execution relay
// — WAS DEFERRED BY THE OWNER. Every tool still executes in the control plane's own process, so a Mac pool
// routes a run's ENGINE to a Mac while `xcodebuild` runs wherever the control plane is: A MAC IS ONLY A MAC
// WHEN THE CONTROL PLANE IS ON IT. The fleet is real; the remote EXECUTION is not. That is why there are five
// cases and not six, why the proof carries no credential-bytes counter, and why the journey has no relay leg.
//
// uat.FleetPeer is structurally the literal "fake". NO TIER ADVANCES.
package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// repoRoot resolves the repository root from THIS source file, so the gate finds the committed corpus no
// matter the process working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve this file's path")
	}
	return filepath.Join(filepath.Dir(self), "..", "..", "..")
}

func bundleDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "evidence", "releases", uat.FleetBundle)
}

// baselineManifest is the committed E23 EXIT bundle. This gate DERIVES its inherited case set from it rather
// than retyping one: the fleet E24 built distributes the human-gated line E23 built, so this release's cases
// ARE that release's cases plus the five ids E24 opened plus the fleet anchor. Retyping them would let the
// two drift, and a drifted case set is how a release quietly drops the red case that caps a tier.
const baselineManifest = uat.ToolApprovalBundle

// fleetAnchorCaseID is the release-level entry the fleet proof hangs off. Like E23-TOOL-APPROVAL,
// E22-CODE-AND-SHIP, E21-TOOLS-MEMORY, E20-SURFACE, E19-WIRING and E17-TIER it is NOT a UAT case (it has no
// tests/uat/cases directory): "no attempt was offered the wrong machine" is not a behaviour one case asserts,
// it is an observation over everything a whole journey did and did not do.
const fleetAnchorCaseID = "E24-FLEET"

// The anchors carried forward from the releases underneath. A bundle carrying E23 cases must carry the
// tool-approval anchor, one carrying E22 cases the code-and-ship anchor, and so down — so all five ride along
// unchanged, and this release is judged on the tier it did NOT move.
const (
	approvalAnchorCaseID = "E23-TOOL-APPROVAL"
	shipAnchorCaseID     = "E22-CODE-AND-SHIP"
	memoryAnchorCaseID   = "E21-TOOLS-MEMORY"
	surfaceAnchorCaseID  = "E20-SURFACE"
	tierAnchorCaseID     = "E17-TIER"
)

// hashParts reproduces the tests/uat construction (sha256 of each part followed by a NUL, hex-encoded,
// sha256:-prefixed) so this generator and the verifier derive the same re-derivable values.
// ponytail: the same 6-line copy the tool-approval / code-and-ship / tools-memory / agent-surface / wiring /
// extensions gates keep. A drift between this copy and the verifier's shows up immediately as a bundle whose
// checksums do not recompute.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// syntheticImageDigest is an obviously-unservable digest, the tool-approval / code-and-ship precedent: this
// journey is COMPONENT-tier (real PostgreSQL, real mTLS, no engine container), so there is no real engine
// image to name and the shape verifier's required field carries a value no registry could ever serve.
var syntheticImageDigest = "sha256:" + strings.Repeat("e24", 21) + "e"

// newCaseIDs is the five ids E24 opened, read from the CANONICAL table rather than retyped so this bundle and
// the orphan guards can never disagree about which cases exist.
func newCaseIDs() []string { return uat.FleetCaseIDs }

// buildFleetManifest assembles the bundle. Every inherited case comes from the committed E23 baseline; every
// proof body comes from a canonical uat table. Nothing is typed twice.
func buildFleetManifest(t *testing.T) []byte {
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

	anchor := uat.FleetContractsDigest()
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
		runID := "run_e24_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c["run_id"] = runID
		c["checksum"] = hashParts(id, runID, anchor)
		c["db_assertions"] = append(append([]string{}, toStrings(t, id, bc["db_assertions"])...),
			"E24 RUNNER-FLEET RELEASE: this case's LOCAL seam is unchanged from "+baselineManifest+
				" — what changed is that WHERE its run executes is now a choice. A machine is a row with a "+
				"SERVER-minted identity, a pool is a queue and a label and a posture, placement into one is a "+
				"REFUSAL rather than a preference, the rendezvous is keyed by (tenant, pool) where the runner "+
				"plane previously had no tenant on it AT ALL, a run whose pool is empty PARKS through E23 T1's "+
				"choreography instead of dead-lettering in about two and a half minutes, revoking a pool KEY "+
				"leaves the machines that came in on it renewing and relaying, and revoking a MACHINE survives "+
				"the control-plane restart that used to erase it. AND THE CEILING THAT MATTERS MORE THAN ANY OF "+
				"IT: E24 T7, the execution relay, WAS DEFERRED, so every tool still runs in the control plane's "+
				"own process — a Mac is only a Mac when the control plane is ON it, and this bundle claims a "+
				"fleet SPINE rather than remote execution. The counterparty is a documented FAKE on one box, so "+
				"this case's §6 operator leg is untouched and its capability's tier does not move. E24 makes §6 "+
				"leg 1 BIGGER again — it now covers a real remote enrolment, a real pool-key revocation and a "+
				"real machine revocation across a restart — and it makes §6 leg 2 (`linux/amd64`) MORE pressing, "+
				"because a fleet is heterogeneous by definition and `runner_pools.arch` can already name an "+
				"architecture nothing here has ever run on.")
		cases = append(cases, c)
	}

	// ---- the five ids E24 OPENED ----------------------------------------------------------------------
	for _, id := range newCaseIDs() {
		runID := "run_e24_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c := map[string]any{
			"id": id, "status": "PASS", "proof_class": "component-real",
			"run_id":              runID,
			"image_digest":        syntheticImageDigest,
			"provider_request_id": "prov_e24_deterministic",
			"mtls_enroll":         "component-tier: a REAL mTLS enrolment against the shipped gateway, but on this box",
			"terminal":            map[string]any{"type": "response.completed", "count": 1},
			"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
			"db_assertions": []string{
				caseAssertion(t, id),
				"OPENED BY E24 UNDER THE `FLT-` PREFIX, and that prefix is a GATE decision rather than a naming " +
					"preference: a `WRK-`/`SAN-`/`OPS-` id added to expectedExtensionsCatalog regenerates the " +
					"shipped extensions-0.1.0 bundle (that map IS its case list and CapabilityClaims feeds a digest " +
					"folded into its every checksum), and an id already inside AgentSurfaceCaseIDs, " +
					"ToolsMemoryCaseIDs, CodeAndShipCaseIDs or ToolApprovalCaseIDs would have matched an EARLIER " +
					"family marker in PromoteGateFor and dispatched this release to a WEAKER gate that knows " +
					"nothing about the wrong-pool sweep, the cross-tenant sweep, the capacity-death count or the " +
					"key-revocation fence. A case belongs to the bundle that certifies it, and the prefix is what " +
					"keeps dispatch unambiguous",
				"AND THE PREFIX HAD THE SAME HOLE E23's DID, MEASURED HERE: FLT-002..005 shipped in T2-T6 while " +
					"`FLT-` was in NO prefix list, so all four directories escaped tests/uat/extensions' orphan " +
					"sweep — the ONLY sweep in the tree that walks tests/uat/cases — and nothing resolved their " +
					"proofs. It was shown RED (five directories reported at once) before the ownership half was " +
					"added: the prefix is in extensionIDPrefixes now, the sweep defers to uat.FleetCaseIDs, and " +
					"TestTheFLTPrefixIsGuardedBySweep fails if either half is removed",
				"NOT in uat.CapabilityClaims and therefore NOT an input to any tier recompute — which changes " +
					"nothing about the outcome: `workspaces` is capped by CapabilityOperatorLegs (§6 leg 1) " +
					"whatever its claims do, `knowledge-vector` and `apple-build` are `disabled` by construction, " +
					"and NO new capability was opened (adding a `fleet` member to CapabilityTierOrder would shift " +
					"CapabilityClaimsDigest and redden every case checksum in every committed bundle)",
				"HONEST CEILING: every machine is a FAKE runner built from the SHIPPED enrollment package against " +
					"the SHIPPED gateway over a real mTLS wire and a real PostgreSQL — on ONE box, in ONE process " +
					"tree, on darwin/arm64. §6 leg 1 (a CAPTURED, re-derivable receipt from TWO PHYSICAL MACHINES) " +
					"and §6 leg 2 (`linux/amd64` verification) are operator work and are NEVER claimed here. " +
					"NEITHER IS REMOTE EXECUTION: T7 was deferred, so no tool call in this bundle ran anywhere but " +
					"the control plane's process",
			},
		}
		c["checksum"] = hashParts(id, runID, anchor)
		cases = append(cases, c)
	}

	// ---- the fleet anchor: the inventory, the placement, the park and the two revocations --------------
	fleetCase := map[string]any{
		"id": fleetAnchorCaseID, "status": "PASS", "proof_class": "component-real",
		"run_id":              "run_e24_fleet_journey",
		"image_digest":        syntheticImageDigest,
		"provider_request_id": "prov_e24_deterministic",
		"mtls_enroll":         "component-tier: a REAL mTLS enrolment against the shipped gateway, but on this box",
		"terminal":            map[string]any{"type": "response.completed", "count": 1},
		"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		"db_assertions": []string{
			"RELEASE-LEVEL entry, not a UAT case (it has no tests/uat/cases directory): \"no attempt was offered the wrong machine\" is not a behaviour one case asserts, it is an observation over everything a whole journey did and did not do — the E23-TOOL-APPROVAL / E22-CODE-AND-SHIP / E21-TOOLS-MEMORY / E20-SURFACE / E19-WIRING / E17-TIER precedent",
			"THE CROWN CLAIM IS THAT PLACEMENT IS A REFUSAL AND NOT A PREFERENCE, AND IT IS RE-DERIVED FROM THE OFFER LEDGER RATHER THAN DECLARED. Both zeros — an attempt offered a machine in another POOL, and an attempt offered another TENANT's machine — are recomputed here, and the ledger must itself contain a machine that was PASSED OVER for its pool and one passed over for its tenant, because a corpus with no wrong machine in it satisfies both zeros trivially and that is precisely the shape this plane had before E24: ONE unbuffered `available` channel that every parked machine sent itself down and every Dial received from, with no pool on either side and no tenant anywhere. The tenant half is the one to read twice — enrolment carried no org or project, `AttemptDescriptor` carried none, `leaseOffer` carried none and `Dial` checked none, so ANY enrolled runner could take ANY tenant's attempt. In a single-runner topology that is a definition; the moment two customers have Macs it is a hole",
			"THE SECOND CROWN CLAIM IS THAT A RUN WITH NO MACHINE PARKS INSTEAD OF DYING, AND ITS EXISTENCE-REASON IS A VENDOR MEASUREMENT. `Dial` was bounded by a 20-second handshake deadline and the retry ladder tried five times, so a run placed in an empty pool dead-lettered in about two and a half minutes — while AWS's own documentation says a Mac instance's \"launch time can range from approximately 6 minutes to 20 minutes\" (§3.5 P10). So \"bring a Mac up when load arrives\" was not an expensive choice but an UNREACHABLE one: the run died four times before the machine booted. The repair is not a bigger timeout, it is E23 T1's park-and-wake choreography used a SECOND time rather than a second mechanism, because two parking paths mean two waking bugs. The wake lives inside handleConnect in one transaction and does NOT reserve a machine — the woken run dials, another run may take that machine first, and the loser parks again; that race is benign and a test says so",
			"THE THIRD CROWN CLAIM IS THIS EPIC'S CHEAPEST SECURITY TEST AND IT IS A COUNTER RATHER THAN A ZERO. Revoking a pool KEY must leave the machines that came in on it RUNNING, so the proof counts the renewals that happened AFTER the revocation instant and re-derives that EVERY ONE OF THEM SUCCEEDED — plus that the revocation still refused a new enrolment, and that an in-flight lease was still relaying. The structural reason it holds: `handleRenew` authenticates with the certificate the machine already holds and the credential chain is not on that path at all, so a key revocation cannot become a back door into SAN-011's machine-level hard stop. WHEN THE NEXT READER SAYS \"if we revoked the key we should cut the connection too\", THIS IS THE ANSWER: cutting would delete the exact property that makes easy enrolment safe. A pool key is designed to be pasted onto ten machines, and that is only tolerable because revoking it costs an operator NOTHING — make a revoke also an outage and nobody revokes, so the leaked key stays live",
			"THE FOURTH CROWN CLAIM IS THAT REVOKING A MACHINE OUTLIVES THE PROCESS THAT DECIDED IT, re-derived across two gateway GENERATIONS on the same CA and the same database. `revoked` was a process-local atomic.Bool, so a restart erased the decision and with `restart: always` a VM reboot did that on a schedule. The discriminating half is carried too: the SAME restarted process must have SERVED an unrevoked machine, because a gateway that refuses everybody is indistinguishable from one that refuses the right machine — which is the difference between a revocation and an outage. AND THE FACT THAT MADE T5 EXIST: `Revoke()` and `Resume()` had NO PRODUCTION CALLER ANYWHERE and `Cordon()`'s only caller was `Drain`'s own first line, whose only caller was SIGTERM. SAN-011's hard stop, written for a compromised runner, was implemented, tested, catalogued and reachable by nobody — the FOURTH time this repository shipped that exact shape, after CreateSlackConnection, DecideToolApproval and the pool key's own composition root",
			"THE FIFTH CLAIM IS THAT THE SERVER NAMES THE MACHINES, and the strong form is a COLLISION rather than a count. Two machines with two labels getting two rows was already true; what was not is that two machines asking for the SAME name hold two identities — `deploy/compose/runner-entrypoint.sh` hardcodes `PALAI_RUNNER_ID=runner-local`, so `--scale runner=3` was three machines holding ONE certificate identity while `identity atomic.Pointer` kept a single slot the last writer won and `palai local doctor` read whichever had presented a certificate most recently. The proof re-derives that every id carries the server's `rnr_` mint prefix, that every certificate's DNS is DERIVED FROM THE ROW'S ID — the T1 verification-pass defect, where two id minters that never met left `last_seen_at` unable to advance for anybody — and that at least one requested label was shared by two distinct identities",
			"AND THE CEILING THAT OUTRANKS EVERY CLAIM ABOVE, STATED HERE BECAUSE A SUMMARY WOULD DROP IT: E24 T7, THE EXECUTION RELAY, WAS DEFERRED BY THE OWNER AND IS NOT IN THIS EPIC. A `lease.offer` carries an `image_digest` — that is the ENGINE, the model loop. Every TOOL still runs in the control plane's process: the shell through `orch.SetShellRunner(shellRunnerFromEnv())` and the file tool through `workspace.NewWorkspaceFS(env.WorkspaceRoot)`, both against the allocation the control plane holds, and the tree wrote this down itself before the epic began. SO A MAC IS ONLY A MAC WHEN THE CONTROL PLANE IS ON IT — which is exactly what today's shipped Mac deployment does (`--native` selects where the CONTROL PLANE runs, nothing else). The fleet is real; the remote EXECUTION is not. There is therefore no FLT-006, no credential-bytes counter (§3.5 P4 names the requirement the relay would have been measured against and says why the number is absent rather than zero), and no relay leg in the journey. A number for a leg that never ran would be a fabrication",
			"NO TIER ADVANCES, and the reason is a RULE rather than an omission: adding a PLANE is not evidence that the plane works in a real fleet. E22 did not advance a tier for DELETING a boundary, E23 did not advance one for ADDING a boundary, and E24 does not advance one for adding a plane — all three rest on the same fake peer. §6 leg 1 grew AGAIN (it now covers a real remote enrolment, a real pool-key revocation and a real machine revocation across a restart) and with T7 deferred the fleet has no remote execution at all; `linux/amd64` is still unverified and E24 makes that gap MORE pressing, because `runner_pools.arch` can now name an architecture nothing here has ever run on; and T2's posture ceiling is open by construction — a machine STATES its posture and the registry COMPARES it with the pool's, but nothing VERIFIES the statement and nothing can on this wire, so a machine that LIES enters the pool it claims. WHAT WOULD HAVE HAD TO BE TRUE to move `workspaces` to `stable`: a captured, re-derivable receipt from TWO PHYSICAL MACHINES; the removal of FleetPeer's structural \"fake\"; and `linux/amd64` verified. None of the three exists",
		},
		"fleet_claim": "fleet spine proven: placement refuses the wrong pool and the wrong tenant, capacity absence parks rather than kills, a key revocation stops new machines and stops nobody who is already in, a machine revocation outlives the process, and the server mints every identity",
		"fleet_proof": canonicalFleetProof(),
	}
	fleetCase["checksum"] = hashParts(fleetAnchorCaseID, "run_e24_fleet_journey", anchor)
	cases = append(cases, fleetCase)

	manifest := map[string]any{
		"release":     uat.FleetBundle,
		"git_sha":     "eef00bb",
		"api_version": "v1",
		"migration":   "000045_runner_fleet",
		"captured_at": "2026-07-30T00:00:00Z",
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

// canonicalFleetProof is the proof body the bundle carries. Every BYTE field is READ FROM A CANONICAL SOURCE
// in tests/uat rather than retyped here, and every declared COUNT is computed by the same sweep the gate
// runs — so a proof that declared a number the ledger does not support could not be written by this
// generator at all, let alone verify.
func canonicalFleetProof() uat.FleetProof {
	_, _, _, _, _, err := uat.SweepFleetOffers(json.RawMessage(uat.FleetOfferLedger))
	if err != nil {
		panic("the fleet offer ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	_, _, renewals, _, _, err := uat.SweepFleetKeyRevocation(json.RawMessage(uat.FleetCredentialLedger))
	if err != nil {
		panic("the fleet credential ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	_, distinct, _, err := uat.SweepFleetRegistry(json.RawMessage(uat.FleetRegistryLedger))
	if err != nil {
		panic("the fleet registry ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	return uat.FleetProof{
		Peer: uat.FleetPeer,

		OfferLedger:          json.RawMessage(uat.FleetOfferLedger),
		OffersToTheWrongPool: 0,
		OffersAcrossTenants:  0,

		RunLedger:                         json.RawMessage(uat.FleetRunLedger),
		RunsDeadLetteredForAbsentCapacity: 0,

		CredentialLedger:                       json.RawMessage(uat.FleetCredentialLedger),
		EnrolledRunnersDroppedByAKeyRevocation: 0,
		// THE COUNT COMES FROM THE SWEEP, NEVER FROM A LITERAL. This is the (d) fence's own number: how many
		// renewals happened after the revocation instant, every one of which the gate requires to have
		// succeeded. A literal here would be the one place this proof could lie about its cheapest test.
		RenewalsAfterTheRevocation: renewals,

		LifecycleLedger:            json.RawMessage(uat.FleetLifecycleLedger),
		RevokedRunnersThatCameBack: 0,

		RegistryLedger: json.RawMessage(uat.FleetRegistryLedger),
		// Also from the sweep: the inventory's size is what the rows say it is.
		DistinctRunners: distinct,

		Contracts:       uat.FleetContracts,
		ContractsDigest: uat.FleetContractsDigest(),
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

// TestCommittedFleetBundleIsTheGeneratorOutput pins the committed bundle to the tree: it must be
// BYTE-identical to this generator's output, so a contract-ledger change, a ledger-row change, a case-set
// change or a baseline change cannot leave a stale bundle verifying green.
// Regenerate with: PALAI_WRITE_FLEET_BUNDLE=1 go test ./tests/uat/fleet/
func TestCommittedFleetBundleIsTheGeneratorOutput(t *testing.T) {
	want := buildFleetManifest(t)
	path := filepath.Join(bundleDir(t), "manifest.json")

	if os.Getenv("PALAI_WRITE_FLEET_BUNDLE") == "1" {
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
		t.Fatalf("read the committed bundle: %v (regenerate with PALAI_WRITE_FLEET_BUNDLE=1)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the committed %s bundle is not this generator's output — regenerate with PALAI_WRITE_FLEET_BUNDLE=1", uat.FleetBundle)
	}
}

// TestFleetReleaseVerifiesClean runs the committed bundle through the SHIPPED verifier and requires
// 0 failed / 0 missing / 0 secret. It is the `make evidence-verify RELEASE=runner-fleet-0.1.0` gate,
// in-process.
func TestFleetReleaseVerifiesClean(t *testing.T) {
	summary, err := uat.VerifyRelease(bundleDir(t), nil)
	if err != nil {
		t.Fatalf("verify the fleet release: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("the fleet bundle did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatal("the fleet bundle verified 0 passed cases — a zero-case bundle is not a clean bundle")
	}
	t.Logf("%s: %s", uat.FleetBundle, summary.String())
}

// TestTheFiveNewCasesAreInTheBundle is the shrink guard the E18 T8 sweep taught: a release that OPENED five
// ids must carry all five, or the ids exist in the tree with no bundle certifying them.
//
// AND IT IS FIVE ON PURPOSE. The plan named six; FLT-006 would have claimed that a tool call ran on the
// runner's machine, which is T7 and was deferred. This test would go RED if somebody added a sixth id to
// uat.FleetCaseIDs without a bundle entry — and it should, because the id and the claim have to arrive
// together.
func TestTheFiveNewCasesAreInTheBundle(t *testing.T) {
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
	for _, id := range uat.FleetCaseIDs {
		if status, ok := carried[id]; !ok {
			t.Errorf("%s is a case this epic OPENED but the bundle that certifies it does not carry it", id)
		} else if status != "PASS" {
			t.Errorf("%s is carried as %q; this release claims it green", id, status)
		}
	}
	for _, anchor := range []string{fleetAnchorCaseID, approvalAnchorCaseID, shipAnchorCaseID, memoryAnchorCaseID, surfaceAnchorCaseID, tierAnchorCaseID} {
		if _, ok := carried[anchor]; !ok {
			t.Errorf("%s is not in the bundle — every anchor underneath this release rides along, so the release is judged on the tier it did NOT move", anchor)
		}
	}
}

// TestTheBundleCarriesNoSixthCase is the other direction, and it exists because a plan that named six ids is
// exactly the kind of thing a later reader completes out of tidiness. FLT-006 was going to certify that a
// tool call runs on the runner's machine and that no credential crosses that boundary. T7 was DEFERRED, so
// nothing in this tree can produce that evidence — an id for it would be a green row for a claim nobody
// proved.
func TestTheBundleCarriesNoSixthCase(t *testing.T) {
	if len(uat.FleetCaseIDs) != 5 {
		t.Fatalf("uat.FleetCaseIDs holds %d ids, want 5: the plan named six and the sixth (a tool call executing on the runner's machine) is E24 T7, which was deferred — if a sixth id is being added, the relay has to arrive with it", len(uat.FleetCaseIDs))
	}
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	// THE CHECK IS STRUCTURAL AND NOT A SUBSTRING SCAN, and that is not a stylistic preference — the first
	// draft of this test WAS a `bytes.Contains(raw, "FLT-006")` and it went red on the bundle's own honest
	// paragraph explaining why FLT-006 does not exist. A byte scan cannot tell a case id from prose about a
	// case id, which is the same defect in the opposite direction from the vacuous-substring one this
	// repository keeps finding: over-broad rather than unfireable, and it fails on the truth.
	var m struct {
		Cases []struct {
			ID    string          `json:"id"`
			Fleet json.RawMessage `json:"fleet_proof"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}
	for _, c := range m.Cases {
		if c.ID == "FLT-006" {
			t.Error("the bundle carries a case FLT-006 — that case would claim a tool call ran on the runner's machine, which E24 does not do: every tool still executes in the control plane's process, so a Mac is only a Mac when the control plane is on it")
		}
	}

	// And the credential-bytes counter is absent for the same reason: there is no relay to sweep. Checked on
	// the proof's own decoded KEYS, so a field added under any name that means it fails here rather than
	// passing because the struct happens not to have one today.
	for _, c := range m.Cases {
		if len(c.Fleet) == 0 {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(c.Fleet, &fields); err != nil {
			t.Fatalf("%s: decode the fleet proof: %v", c.ID, err)
		}
		for key := range fields {
			// `credential_bytes` and not `credential` — the proof DOES carry a `credential_ledger`, which
			// records enrolment and renewal OUTCOMES and no credential value at all. Banning the word would
			// have made this guard fail on the field group (d) is built from.
			for _, forbidden := range []string{"credential_bytes", "relay", "frame"} {
				if strings.Contains(key, forbidden) {
					t.Errorf("%s: the fleet proof carries the field %q — plan §T8's counter (f) measured credential bytes in the execution RELAY's frames, and with T7 deferred there is no relay and no frames, so any number here is measuring a leg that never ran (§3.5 P4)", c.ID, key)
				}
			}
		}
	}
}
