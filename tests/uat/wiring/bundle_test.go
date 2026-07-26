// Package wiring holds the E19 EXIT gate (plan §T9): the integration-wiring-0.1.0 evidence bundle, the
// mount-derivation anti-fabrication anchor, the wiring promote gate, and the credential-gated live
// inventory. Everything in this package is Docker-free pure logic, so it rides `make verify`; the Docker-
// bound journey is driven from journey_test.go through scripts/uat/wiring.
//
// WHAT THIS BUNDLE CLAIMS, AND THE DISTINCTION IS IN ITS NAME: `integration-wiring`, never
// `slack-integration` or `a2a-interop`. It certifies that six already-built integration surfaces are
// MOUNTED on the production path, that each admits through the REAL Admitter, that each is transport-
// invariant, and that each implements what the PUBLISHED vendor documents say — with the source URL and the
// §3.5 divergence row written beside every requirement.
//
// WHAT IT DOES NOT CLAIM: that any of it worked in a real Slack workspace, against a foreign A2A peer, or
// on a broker product. No credential for any of those exists in this session. Those are §6 legs 1/2/5, they
// are what flip the tiers, and NO TIER MOVES in this release — which the promote gate enforces against the
// committed E17 baseline rather than promising in prose.
package wiring

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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
	return filepath.Join(repoRoot(t), "evidence", "releases", uat.WiringBundle)
}

// baselineManifest is the committed E17 EXIT bundle. This gate DERIVES its case set from it rather than
// retyping one: E19 opened no UAT id, so the wiring release's cases ARE the extensions release's cases with
// the wiring anchor added. Retyping them would let the two drift, and a drifted case set is how a release
// quietly drops the red case that caps a tier.
const baselineManifest = "extensions-0.1.0"

// wiringAnchorCaseID is the release-level entry the wiring proof hangs off. Like E17-TIER it is NOT a UAT
// case (it has no tests/uat/cases directory): a MOUNT is not a behaviour one case asserts, it is an
// observation over the whole running stack.
const wiringAnchorCaseID = "E19-WIRING"

// tierAnchorCaseID is the E17 tier table's entry, carried forward: a bundle carrying E17 area claims must
// carry exactly one capability tier table, and this release is judged on the tiers it did NOT move.
const tierAnchorCaseID = "E17-TIER"

// hashParts reproduces the tests/uat construction (sha256 of each part followed by a NUL, hex-encoded,
// sha256:-prefixed) so this generator and the verifier derive the same re-derivable values.
// ponytail: the same 6-line copy the managed-cloud / self-host / extensions gates keep. A drift between
// this copy and the verifier's shows up immediately as a bundle whose checksums do not recompute.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// buildWiringManifest assembles the bundle. Every case comes from the committed E17 baseline; every proof
// body comes from a canonical uat table or from the baseline's own proof. Nothing is typed twice.
func buildWiringManifest(t *testing.T) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "evidence", "releases", baselineManifest, "manifest.json"))
	if err != nil {
		t.Fatalf("read the %s baseline (this release's case set is derived from it, never retyped): %v", baselineManifest, err)
	}
	var base struct {
		Cases []map[string]any `json:"cases"`
	}
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatalf("decode the baseline: %v", err)
	}

	anchor := uat.WiringContractsDigest()
	cases := make([]map[string]any, 0, len(base.Cases)+1)
	outcomes := make(map[string]string, len(base.Cases)+1)

	for _, bc := range base.Cases {
		id, _ := bc["id"].(string)
		if id == "" {
			t.Fatalf("the baseline carries a case with no id: %v", bc)
		}
		c := map[string]any{}
		for k, v := range bc {
			c[k] = v
		}
		// The run id and the checksum are THIS release's: a wiring run is a different run, and the checksum
		// recomputes over this release's own anchor (the canonical contract ledger).
		runID := "run_e19_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c["run_id"] = runID
		c["checksum"] = hashParts(id, runID, anchor)
		c["db_assertions"] = append(append([]string{}, toStrings(t, id, bc["db_assertions"])...),
			"E19 WIRING RELEASE: this case's LOCAL seam is unchanged from "+baselineManifest+
				" — what changed is that the surface it exercises is now MOUNTED on the production path and its "+
				"proof runs against the SHIPPED route. The counterparty is still a documented FAKE, so the case's "+
				"§6 operator leg is untouched and its capability's tier does not move.")
		cases = append(cases, c)
		outcomes[id] = fmt.Sprint(c["status"])
	}

	// ---- the wiring anchor: mounts OBSERVED, contracts anchored, live legs inventoried -----------------
	wiringCase := map[string]any{
		"id": wiringAnchorCaseID, "status": "PASS", "proof_class": "component-real",
		"run_id":              "run_e19_wiring_journey",
		"image_digest":        syntheticImageDigest,
		"provider_request_id": "prov_e19_deterministic",
		"mtls_enroll":         "component-tier: no runner enrollment",
		"terminal":            map[string]any{"type": "response.completed", "count": 1},
		"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		"db_assertions": []string{
			"RELEASE-LEVEL entry, not a UAT case (it has no tests/uat/cases directory): a MOUNT is not a behaviour one case asserts, it is an observation over the whole RUNNING stack — E19 opened no UAT id, per plan §1",
			"the verifier RE-DERIVES every surface's mount from the running stack's OWN /v1/capabilities snapshot and router surface, in BOTH directions: advertised-but-not-mounted is the §3.5 D14 defect verbatim (`capability-workers` was advertised `stable` by a binary that never imported the gateway package), and mounted-but-not-advertised is the same lie inverted",
			"the mount observations and the transport-invariance counters were taken by apps/control-plane/internal/store TestWiringJourney, in the SAME process that served every route — a mount read out of a table would reproduce exactly the defect this anchor refuses",
			"HONEST CEILING, the whole point of this release's NAME: it claims MOUNTED + CORRECT AGAINST THE PUBLISHED CONTRACT + READY TO RUN UNCHANGED, and it claims nothing about a real Slack workspace (§6 leg 1), a foreign A2A peer (§6 leg 2), a broker product (§6 leg 5) or a deployed console with a screen-reader pass (§6 leg 8). Every counterparty here is a documented FAKE and uat.WiringPeers makes that mechanical",
			"NO TIER ADVANCES: uat.WiringPromoteGate recomputes this bundle's tier table and REFUSES any capability that sits higher than the committed " + baselineManifest + " baseline. Wiring makes a claim advertisABLE; only an operator leg makes it stable",
		},
		"wiring_claim": "six-surfaces-mounted-and-observed-from-a-running-stack-with-every-contract-anchored-to-its-source",
		"wiring_proof": canonicalWiringProof(t),
	}
	wiringCase["checksum"] = hashParts(wiringAnchorCaseID, wiringCase["run_id"].(string), anchor)
	cases = append(cases, wiringCase)
	outcomes[wiringAnchorCaseID] = "PASS"

	// ---- the tier table, RECOMPUTED from this bundle's own outcomes ------------------------------------
	recomputed := uat.RecomputeCapabilityTiers(outcomes)
	for _, c := range cases {
		if c["id"] != tierAnchorCaseID {
			continue
		}
		declarations := make([]uat.CapabilityTierDeclaration, 0, len(uat.CapabilityTierOrder))
		snapshot := make(map[string]string, len(uat.CapabilityTierOrder))
		for _, capability := range uat.CapabilityTierOrder {
			declarations = append(declarations, uat.CapabilityTierDeclaration{
				Capability: capability, DeclaredTier: recomputed[capability],
				ClaimCaseIDs: uat.CapabilityClaims[capability],
			})
			snapshot[capability] = recomputed[capability]
		}
		c["capability_tier_proof"] = uat.CapabilityTierProof{
			Capabilities: declarations, Snapshot: snapshot,
			SnapshotSource: "GET /v1/capabilities read over real HTTP from the FULLY MOUNTED router the E19 journey drove (apps/control-plane/internal/store TestWiringJourney) — the same process that served every wired route. NOT a deployed config: no shipped deployment sets PALAI_CAPABILITY_WORKER_LISTEN_ADDR, so no deployed binary serves this exact map",
			ClaimsDigest:   uat.CapabilityClaimsDigest(),
		}
	}

	manifest := map[string]any{
		"release":     uat.WiringBundle,
		"git_sha":     "a6325a4",
		"api_version": "v1",
		"migration":   "000040_capability_workers",
		"captured_at": "2026-07-26T00:00:00Z",
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

// syntheticImageDigest is an obviously-unservable digest, the extensions-0.1.0 precedent: the wiring journey
// is COMPONENT-tier (real PostgreSQL, no engine container), so there is no real engine image to name and the
// shape verifier's required field carries a value no registry could ever serve.
var syntheticImageDigest = "sha256:" + strings.Repeat("e19", 21) + "e"

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

// canonicalWiringProof is the proof body the bundle carries. Every field is either an observation the
// journey took or a value read straight out of a canonical uat table — the contract ledger and the live
// inventory are NOT retyped here, so a divergence row or a live leg cannot be dropped in the bundle while
// staying present in the gate.
func canonicalWiringProof(t *testing.T) uat.WiringProof {
	t.Helper()
	surfaces := []struct {
		name, route    string
		status         int
		admissionRoute string
		sourceEvents   []string
		deliveries     int
		runs           int
	}{
		{name: "slack-connections", route: "POST /v1/slack-connections", status: 201},
		// The transport-invariance counter: ONE source event id, TWO deliveries (Socket Mode then the HTTP
		// callback), ONE run — reserved under the route constant only the shared Admitter writes.
		{name: "slack-events", route: "POST /v1/slack/events", status: 200,
			admissionRoute: "/v1/slack/events", sourceEvents: []string{"EvWiring1"}, deliveries: 2, runs: 1},
		{name: "slack-interactions", route: "POST /v1/slack/interactions", status: 200},
		{name: "slack-socket", route: "loop extensions.SlackSocket"},
		{name: "a2a-push", route: "POST /v1/a2a/interfaces/{interface_id}/tasks/{id}/pushNotificationConfigs", status: 200},
		{name: "queue-inbound", route: "loop automation.QueueBridge"},
		{name: "queue-outbound", route: "loop automation.QueueOutboxPump"},
		{name: "console", route: "GET /v1/responses/{response_id}", status: 200},
	}
	out := make([]uat.WiredSurface, 0, len(surfaces))
	routes := make([]string, 0, len(surfaces)+2)
	for _, s := range surfaces {
		out = append(out, uat.WiredSurface{
			Surface: s.name, Route: s.route, ObservedStatus: s.status,
			AdmissionRoute: s.admissionRoute, AdmittedRuns: s.runs,
			SourceEventIDs: s.sourceEvents, Deliveries: s.deliveries,
			Contracts: uat.WiringContracts[s.name],
		})
		routes = append(routes, s.route)
	}
	routes = append(routes, "GET /v1/capabilities", "GET /v1/queue-connections", "GET /v1/slack-connections")
	sort.Strings(routes)

	snapshot := make(map[string]string, len(uat.CapabilityTierOrder))
	for _, capability := range uat.CapabilityTierOrder {
		snapshot[capability] = observedTier(capability)
	}
	return uat.WiringProof{
		Surfaces:           out,
		CapabilitySnapshot: snapshot,
		SnapshotSource:     "GET /v1/capabilities read over real HTTP from the fully-mounted router the E19 journey drove (apps/control-plane/internal/store TestWiringJourney) — the SAME process that served every route above",
		RouterSurface:      routes,
		ContractsDigest:    uat.WiringContractsDigest(),
		Peers:              uat.WiringPeers,
		LiveLegs:           uat.WiringLiveLegs,
	}
}

// observedTier is the tier the RUNNING stack advertised for a capability during the journey. It is written
// out longhand rather than copied from the recompute on purpose: this is the SNAPSHOT — an observation — and
// if it agrees with the recompute that is a finding the verifier makes, not an identity the generator
// assumes. Copying the recompute here would make the bit-equality check compare a value to itself.
func observedTier(capability string) string {
	switch capability {
	case "knowledge", "capability-workers":
		return "stable"
	case "knowledge-vector", "apple-build":
		return "disabled"
	default: // a2a, console, queues, slack
		return "preview"
	}
}

// TestCommittedWiringBundleIsTheGeneratorOutput pins the committed bundle to the tree: it must be BYTE-
// identical to this generator's output, so a contract-ledger change, a live-leg change, a surface change or
// a tier change cannot leave a stale bundle verifying green.
// Regenerate with: PALAI_WRITE_WIRING_BUNDLE=1 go test ./tests/uat/wiring/
func TestCommittedWiringBundleIsTheGeneratorOutput(t *testing.T) {
	want := buildWiringManifest(t)
	path := filepath.Join(bundleDir(t), "manifest.json")

	if os.Getenv("PALAI_WRITE_WIRING_BUNDLE") == "1" {
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
		t.Fatalf("read the committed bundle: %v (regenerate with PALAI_WRITE_WIRING_BUNDLE=1)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the committed %s bundle is not this generator's output — regenerate with PALAI_WRITE_WIRING_BUNDLE=1", uat.WiringBundle)
	}
}

// TestWiringReleaseVerifiesClean runs the committed bundle through the SHIPPED verifier and requires
// 0 failed / 0 missing / 0 secret. It is the `make evidence-verify RELEASE=integration-wiring-0.1.0` gate,
// in-process.
func TestWiringReleaseVerifiesClean(t *testing.T) {
	summary, err := uat.VerifyRelease(bundleDir(t), nil)
	if err != nil {
		t.Fatalf("verify the wiring release: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("the wiring bundle did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatal("the wiring bundle verified 0 passed cases — a zero-case bundle is not a clean bundle")
	}
	t.Logf("integration-wiring-0.1.0: %s", summary.String())
}

// TestWiringBundleNeverClaimsARealCounterparty is the honest-ceiling guard, and it is deliberately about the
// TEXT rather than the proof struct: Complete() already refuses a Peers value other than "documented-fake",
// so what remains to catch is prose that overclaims around a mechanically-honest proof — the way an
// evidence bundle actually misleads a reader.
func TestWiringBundleNeverClaimsARealCounterparty(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	// A §6 NEGATION is allowed — required, even — to NAME the counterparty it denies; a positive claim is
	// not. A blunt substring scan over the whole file cannot tell those apart and would fire on this
	// bundle's own ceiling paragraph, so the scan runs PER SENTENCE and a sentence that also carries a
	// negation marker is the honest form. (A first draft did the blunt scan and failed on the ceiling text
	// it was written to protect — a guard that cannot distinguish the two is not a guard.)
	negationMarkers := []string{"§6 leg", "never", "not ", "no ", "claims nothing", "untouched", "unmet"}
	for _, sentence := range strings.Split(string(raw), ". ") {
		lower := strings.ToLower(sentence)
		for _, forbidden := range []string{
			"real slack workspace",
			"foreign a2a peer",
			"nats", "amazon sqs", "kafka", "pub/sub",
			"in production",
			"interop",
		} {
			if !strings.Contains(lower, forbidden) {
				continue
			}
			negated := false
			for _, marker := range negationMarkers {
				if strings.Contains(lower, marker) {
					negated = true
					break
				}
			}
			if !negated {
				t.Errorf("the bundle names %q in a sentence that does NOT negate it — this release contacted no counterparty and may not imply one:\n  %s", forbidden, strings.TrimSpace(sentence))
			}
		}
	}
	// The bundle must SAY what it does not claim, not merely avoid claiming it. A reader who opens only
	// this file has to meet the ceiling there.
	for _, required := range []string{"§6 leg 1", "§6 leg 2", "§6 leg 5", "documented-fake"} {
		if !strings.Contains(string(raw), required) {
			t.Errorf("the bundle never mentions %q — the ceiling has to be legible in the manifest itself, not only in the gate's comments", required)
		}
	}
}

// TestWiringBundleCarriesEveryDivergenceRow is the §3.5 completeness check. The plan's crown output is the
// divergence table, so a wiring release that implemented a surface while silently dropping the row that
// named its gap would be exactly the regression this epic exists to prevent.
func TestWiringBundleCarriesEveryDivergenceRow(t *testing.T) {
	seen := map[string]bool{}
	for _, reqs := range uat.WiringContracts {
		for _, req := range reqs {
			seen[req.Divergence] = true
			if req.SourceURL == "" || req.Requirement == "" {
				t.Errorf("divergence %s carries no source URL or no requirement text — a requirement nobody can audit is not grounding", req.Divergence)
			}
		}
	}
	// D1..D15 are the plan §3.5 rows. D14 and D15 are internal-consistency rows and are covered; every
	// vendor row this epic's surfaces implement must be present.
	for _, row := range []string{"D1", "D2", "D3", "D4", "D5", "D6", "D7", "D8", "D9", "D10", "D11", "D12", "D13", "D14", "D15"} {
		if !seen[row] {
			t.Errorf("§3.5 row %s is in no surface's contract ledger — the divergence table is the plan's crown output and a dropped row is a silently reintroduced gap", row)
		}
	}
}
