// The E25 EXIT gate's bundle half (plan §T9): the admin-console-0.1.0 evidence bundle, its generator, and the
// shrink guards in both directions. Everything here is Docker-free pure logic, so it rides `make verify`; the
// Docker-bound backing legs and the browser suite are driven from journey_test.go through
// scripts/uat/admin-console.
//
// WHAT THIS BUNDLE CLAIMS, and it is deliberately narrower than "the console is done": that no write reaches
// the control plane through this origin without an operator SESSION and that a console with no password hash
// serves nothing at all; that every page the console DECLARES is axe-scanned, in both colour schemes, under
// the WCAG 2.0+2.1+2.2 tags; that a secret VALUE written through the configuration surface comes back from no
// read path, measured at the SQL query names, at the projection field sets and at the bytes a browser
// received; that a stranger can build an environment, a repository binding, an agent and an MCP tool grant
// without writing `curl`; and that a Slack-less installation can decide a gated tool call from a screen.
//
// AND THE HONEST CEILING, WHICH IS THE MOST IMPORTANT SENTENCE IN THIS PACKAGE. NOBODY OPERATED A DEPLOYED
// CONSOLE. Every browser leg drove a Chromium against a deterministic fixture upstream or a compose stack on
// one box, and compose is not a deployment — `capabilities_mount_test.go` makes the console being a separate
// deployable the basis of this tier's derivation. There is no manual screen-reader pass, and E25 added the
// surface axe is weakest on: forms.
//
// uat.AdminConsolePeer is structurally the literal "fake". NO TIER ADVANCES.
package adminconsole

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

func bundleDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "evidence", "releases", uat.AdminConsoleBundle)
}

// baselineManifest is the committed E23 EXIT bundle, and choosing it over E24's is a MEASUREMENT rather than
// an ordering accident. E25 ran in PARALLEL with E24 (plan §7): it built nothing on the runner fleet, carries
// no `FLT-` case and owes no fleet proof. Deriving the inherited case set from `runner-fleet-0.1.0` would
// make an admin-console release carry a claim about machines it never placed a run on.
const baselineManifest = uat.ToolApprovalBundle

// adminConsoleAnchorCaseID is the release-level entry the admin-console proof hangs off. Like
// E23-TOOL-APPROVAL, E22-CODE-AND-SHIP, E21-TOOLS-MEMORY, E20-SURFACE, E19-WIRING and E17-TIER it is NOT a
// UAT case (it has no tests/uat/cases directory): "every exported method of the relay is behind the gate" is
// not a behaviour one case asserts, it is an observation over a whole surface.
const adminConsoleAnchorCaseID = "E25-ADMIN-CONSOLE"

// The anchors carried forward from the releases underneath. A bundle carrying E23 cases must carry the
// tool-approval anchor, one carrying E22 cases the code-and-ship anchor, and so down — so all five ride along
// unchanged, and this release is judged on the tier it did NOT move.
const (
	approvalAnchorCaseID = "E23-TOOL-APPROVAL"
	shipAnchorCaseID     = "E22-CODE-AND-SHIP"
	memoryAnchorCaseID   = "E21-TOOLS-MEMORY"
	surfaceAnchorCaseID  = "E20-SURFACE"
	wiringAnchorCaseID   = "E19-WIRING"
	tierAnchorCaseID     = "E17-TIER"
)

// hashParts reproduces the tests/uat construction (sha256 of each part followed by a NUL, hex-encoded,
// sha256:-prefixed) so this generator and the verifier derive the same re-derivable values.
// ponytail: the same 6-line copy the fleet / tool-approval / code-and-ship / tools-memory / agent-surface /
// wiring / extensions gates keep. A drift between this copy and the verifier's shows up immediately as a
// bundle whose checksums do not recompute.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// syntheticImageDigest is an obviously-unservable digest, the fleet / tool-approval precedent: E25's legs are
// BROWSER-tier and component-tier, so there is no real engine image to name and the shape verifier's required
// field carries a value no registry could ever serve.
var syntheticImageDigest = "sha256:" + strings.Repeat("e25", 21) + "e"

// newCaseIDs is the seven ids E25 opened, read from the CANONICAL table rather than retyped so this bundle
// and the orphan guards can never disagree about which cases exist.
func newCaseIDs() []string { return uat.AdminConsoleCaseIDs }

// buildAdminConsoleManifest assembles the bundle. Every inherited case comes from the committed E23 baseline;
// every proof body comes from a canonical uat table. Nothing is typed twice.
func buildAdminConsoleManifest(t *testing.T) []byte {
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

	anchor := uat.AdminConsoleContractsDigest()
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
		runID := "run_e25_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c["run_id"] = runID
		c["checksum"] = hashParts(id, runID, anchor)
		c["db_assertions"] = append(append([]string{}, toStrings(t, id, bc["db_assertions"])...),
			"E25 ADMIN-CONSOLE RELEASE: this case's LOCAL seam is unchanged from "+baselineManifest+
				" — what changed is that the surface an operator CONFIGURES all of it from now asks who they "+
				"are. The gate is inside the RELAY (every exported method of every Route Handler under "+
				"app/api/palai opens with requireSession) rather than in a proxy.ts, and that is three "+
				"independent reasons pointing at one file: the vendor's own CVSS 9.1 advisory about "+
				"authorization in middleware, Partial Rendering making a layout check unreliable, and every "+
				"data path already funnelling through one function family. Before this epic the console had "+
				"ZERO authentication of any kind while the relay exported POST/PATCH/DELETE, and the key "+
				"behind it holds every capability implicitly — so anyone who reached the origin could decide "+
				"every tool approval E23 T9 had just opened. AND THE CEILING THAT OUTRANKS IT: NOBODY "+
				"OPERATED A DEPLOYED CONSOLE. Every browser proof ran against a deterministic fixture "+
				"upstream or a compose stack on ONE box; compose is not a deployment, there is no manual "+
				"screen-reader pass, and axe's own published figure is 57% of WCAG issues on average while "+
				"E25 added nine pages of FORMS — the surface axe is weakest on and where an error message's "+
				"MEANING, which no scanner reads, matters most. So this case's §6 operator leg is untouched "+
				"and its capability's tier does not move. E25 makes §6 leg 8 BIGGER: it now covers a login "+
				"form, a SECRET-WRITING form, an approval decision surface, six configuration forms and a "+
				"pagination control.")
		cases = append(cases, c)
	}

	// ---- the seven ids E25 OPENED --------------------------------------------------------------------
	for _, id := range newCaseIDs() {
		runID := "run_e25_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c := map[string]any{
			"id": id, "status": "PASS", "proof_class": caseProofClass(t, id),
			"run_id":              runID,
			"image_digest":        syntheticImageDigest,
			"provider_request_id": "prov_e25_deterministic",
			"mtls_enroll":         "browser-tier: a real Chromium against the console's own relay, on this box",
			"terminal":            map[string]any{"type": "response.completed", "count": 1},
			"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
			"db_assertions": []string{
				caseAssertion(t, id),
				"OPENED BY E25 UNDER THE `CON-` PREFIX, and that prefix is a GATE decision rather than a naming " +
					"preference: a `UI-` id is already inside extensionIDPrefixes, and that map IS the shipped " +
					"extensions-0.1.0 bundle's case list (its generator reads it, and uat.CapabilityClaims feeds " +
					"a digest folded into its every checksum), so a UI-005 would either force the regeneration of " +
					"a committed historical release — rewriting the record of a run that happened — or fall " +
					"through PromoteGateFor to a WEAKER family gate that knows nothing about the relay-gate " +
					"totality, the axe coverage equality, the ciphertext query pin, the byte scan, the sweep " +
					"floor or the ledger repairs, and would pass it. E20, E21 and E22 each paid for that once, " +
					"and E23's HIL-004 paid for the other half of it: a case directory under a prefix NO sweep " +
					"walks, reported green by silence. Both directions are closed here — `CON-` is in " +
					"extensionIDPrefixes, and tests/uat/admin-console refuses an id in uat.AdminConsoleCaseIDs " +
					"with no directory or with a declared proof that is not in the tree",
				"NOT in uat.CapabilityClaims and therefore NOT an input to any tier recompute — which changes " +
					"nothing about the outcome: `console` is capped by CapabilityOperatorLegs (§6 leg 8) whatever " +
					"its claims do, and NO new capability was opened. Adding an `environments` or `mcp` member to " +
					"CapabilityTierOrder would shift CapabilityClaimsDigest and redden every case checksum in " +
					"every committed bundle, which is why E25 opened neither",
				"HONEST CEILING: every browser leg is a REAL Chromium driving the SHIPPED relay and the SHIPPED " +
					"pages, in two colour schemes, against either tests/fake-control-plane.mjs or a four-service " +
					"compose stack — on ONE box, on darwin/arm64, with no operator and no deployment. §6 leg 8 (a " +
					"DEPLOYED console plus a manual screen-reader pass) is operator work and is NEVER claimed " +
					"here, and it GREW in this epic. Neither is the narrow-key measurement (§6 leg 4): T1 built " +
					"the door, but the key behind it still holds every capability implicitly, and WHICH PAGES " +
					"WORK with `palai apikey create --scope provision --scope approve` has not been measured",
			},
		}
		c["checksum"] = hashParts(id, runID, anchor)
		cases = append(cases, c)
	}

	// ---- the admin-console anchor: the gate, the sweep, the absence and the ledger repairs -------------
	consoleCase := map[string]any{
		"id": adminConsoleAnchorCaseID, "status": "PASS", "proof_class": "e2e-deterministic",
		"run_id":              "run_e25_admin_console_journey",
		"image_digest":        syntheticImageDigest,
		"provider_request_id": "prov_e25_deterministic",
		"mtls_enroll":         "browser-tier: a real Chromium against the console's own relay, on this box",
		"terminal":            map[string]any{"type": "response.completed", "count": 1},
		"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		"db_assertions": []string{
			"RELEASE-LEVEL entry, not a UAT case (it has no tests/uat/cases directory): \"every exported method of the relay is behind the gate\" and \"every page this console declares was scanned\" are not behaviours one case asserts, they are observations over a whole surface — the E23-TOOL-APPROVAL / E22-CODE-AND-SHIP / E21-TOOLS-MEMORY / E20-SURFACE / E19-WIRING / E17-TIER precedent",
			"THE FIRST CROWN CLAIM IS THAT THE IDENTITY GATE IS TOTAL, AND IT IS RE-DERIVED FROM THE RELAY'S OWN SOURCE RATHER THAN DECLARED. The exported HTTP method count and the count opening with `requireSession` must be EQUAL — not most, not the ones a list happened to name — and the ONE non-relay export is pinned to the login door BY FILE NAME, so the exception cannot multiply and cannot migrate onto a data route. The measurement that makes it urgent rather than tidy: before E25 the console had NO authentication of any kind (`app/`, `lib/` and `components/` held zero hits) while the relay had exported POST/PATCH/DELETE since E17 T10, the server-side key holds every capability implicitly (`Scope.HasScope` returns true for every scope on an empty set, and api/approvals.go writes that down verbatim), and E23 T9 had just added a DECISION surface behind the same origin — so anyone who could reach it could approve every gated tool call in the deployment. The gate's home is the relay and NOT a proxy.ts, for three independent reasons that point at the same file: the vendor's own GHSA-f82v-jwr5-mffw at CVSS 9.1 (\"bypass authorization checks … if the authorization check occurs in middleware\"), Partial Rendering not re-running a layout on every navigation, and every data path in this console already funnelling through one function family",
			"THE SECOND CROWN CLAIM IS THAT NO WRITTEN SECRET VALUE COMES BACK, AND IT IS MEASURED IN THREE LAYERS THAT EACH FAIL ON A DIFFERENT MISTAKE. At the SQL level the statements touching `ciphertext` are pinned to exactly {InsertSecretRef, ResolveSecretRef} over a corpus of 472 named statements in 37 files — this is the layer that fails when someone WRITES a new query, before it has a caller, a view or a route, and the shortest path to a \"show me the value\" screen is exactly one new statement in storage/queries. At the PROJECTION level three view field sets are pinned by EXACT SET rather than by absence-of-the-word-value, because an absence check passes for Plaintext, Cleartext, Bytes, Decrypted and V. At the BYTE level the sentinel is searched in the DOM (including every input's live `.value`, which outerHTML does not carry), in every response body the browser retained, and in every browser-served source map — and each of those three layers now names a harmless token it actually FOUND, because arms 2 and 3 previously proved only that they had something to scan. The source-map layer is the one that earns its place: a sentinel planted in a source COMMENT is stripped from the minified chunk, survives verbatim in the map's sourcesContent, is never fetched by a browser, and only that arm names it",
			"THE THIRD CROWN CLAIM IS THAT AN UNSCANNED PAGE IS STRUCTURALLY IMPOSSIBLE, AND THAT IT IS TRUE IN BOTH COLOUR SCHEMES. lib/routes.ts is the single source of the navigation and of the sweep, so the declared count and the scanned count are the same list by construction and a route added without a scan is a red test rather than a review question. The colour-scheme half is E25's own finding and it was invisible for the console's whole life: playwright.config.ts defined ONE project and named no colorScheme, while the installed playwright-core@1.51.1 writes its own default down in its types (\"Defaults to 'light'\"), so every axe scan this suite had ever run looked at the light palette — and app/globals.css carries a whole prefers-color-scheme: dark block redefining text, border and accent. That is how a 2.63:1 skip link, the FIRST Tab stop on every page, stayed green. AND WHAT THE SECOND PROJECT DID NOT CATCH IS THE FINDING WORTH RECORDING: nothing. Both real violations are invisible to axe by construction — SC 1.4.11 has NO axe rule at all (color-contrast compares text to background and nothing else), and the skip link is off-screen where axe skips hidden elements — so they were measured with WCAG 2.2's own formula against the rendered page, 53/53 and 56/56 controls failing before the repair, 0/55 and 0/57 after",
			"THE FOURTH CLAIM IS THE CHEAPEST ONE AND IT IS ABOUT A DOCUMENT: a shipped runbook now RUNS. docs/operations/jira-mcp-connection.md §3 told operators for three and a half days to publish a `$REV_ID` that no /v1 response had ever carried, and three of its ten calls rested on that one. The component leg executes the whole chain over the public API alone and it was RED at step (c2) with a 405 rather than a 404 — the tree served POST on exactly the path it refused to serve GET on. Being read is not verification and being edited is not either: that document was edited three times while it was wrong",
			"THE FIFTH CLAIM IS THAT A SLACK-LESS INSTALLATION CAN ANSWER A GATE. A gated tool call is decided from a screen, the binding comes out of the ROW's own displayed data (asserted on the WIRE: the POST body's request_hash equals the hash the row showed, an approve body carries that and nothing else, and no hidden input exists anywhere on the page), and an approval whose ARGUMENTS CHANGED authorizes nothing — re-derived here as zero decisions applied on a mismatched hash, over a ledger that contains real mismatches. The screen also says what it does NOT hold: GET /v1/approvals carries TOOL approvals only, publication approvals (push/PR/merge) are not in it and have no list route at all, and \"nothing is waiting\" would have been a lie for half the queue",
			"AND THE SIXTH IS WHAT NO EARLIER BUNDLE CLAIMED: FIVE ROWS OF THIS TREE'S OWN DIVERGENCE LEDGER — the record the console's evidence rested on — WERE MEASURED WRONG AND REPAIRED. Three said routes were unmounted on a compose stack that passes their configuration in its own compose.yaml, and OPTIONS against a running four-service stack answered 405 with an Allow header for all three; two recorded an envelope difference too strongly, and one of those was actively HIDING a second divergence, because an envelope difference SUBSUMES the item comparison in the sweep's arm 3. THE REASON THEY ROTTED IS ALSO REPAIRED AND IT IS THE MORE IMPORTANT HALF: the sweep ran in no routine gate at all, because scripts/uat/wiring's console tier echoed and CONTINUED when the toolchain was absent while scripts/uat/extensions exited 2 on the same condition. Measured before it was changed: with no node_modules the wiring gate printed uat-wiring=PASS and exited 0. A ledger is only as valid as the gate that runs it",
			"NO TIER ADVANCES, and the reason is a RULE rather than an omission: a configuration surface does not advance a tier, and above all not in the epic that widened what the tier covers. E22 did not advance one for DELETING a boundary, E23 did not for ADDING one, E24 did not for adding a PLANE, and E25 does not for adding a CONFIGURATION SURFACE — all four rest on the same fake peer. §6 leg 8 grew with this epic (a login form, a secret-writing form, an approval decision surface, six configuration forms and a pagination control), there is no DEPLOYED console because compose is not a deployment, and there is no manual screen-reader pass over exactly the surface axe is weakest on. WHAT WOULD HAVE HAD TO BE TRUE to move `console` to `stable`: a deployed console reachable over TLS at an address an operator uses, with a captured transcript; a manual VoiceOver pass over the forms; and the removal of Peer's structural \"fake\". None of the three exists",
		},
		"admin_console_claim": "configuration surface proven: no write passes without an operator session, the console does not open without a password, every declared page is axe-scanned in both colour schemes, no written secret value comes back from any read path, a shipped runbook runs on the public API alone, a gated tool call is decided from a screen, and five rows of the divergence ledger the surface rested on were measured wrong and repaired",
		"admin_console_proof": canonicalAdminConsoleProof(),
	}
	consoleCase["checksum"] = hashParts(adminConsoleAnchorCaseID, "run_e25_admin_console_journey", anchor)
	cases = append(cases, consoleCase)

	manifest := map[string]any{
		"release":     uat.AdminConsoleBundle,
		"git_sha":     "6ddb85a",
		"api_version": "v1",
		"migration":   "000046_environments",
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

// canonicalAdminConsoleProof is the proof body the bundle carries. Every BYTE field is READ FROM A CANONICAL
// SOURCE in tests/uat rather than retyped here, and every declared COUNT is computed by the same sweep the
// gate runs — so a proof that declared a number the ledger does not support could not be written by this
// generator at all, let alone verify.
func canonicalAdminConsoleProof() uat.AdminConsoleProof {
	relay, gated, _, err := uat.SweepAdminConsoleRelay(json.RawMessage(uat.AdminConsoleRelayLedger))
	if err != nil {
		panic("the relay ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	declared, scanned, _, err := uat.SweepAdminConsoleRoutes(json.RawMessage(uat.AdminConsoleRouteLedger))
	if err != nil {
		panic("the route ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	hits, _, _, err := uat.SweepAdminConsoleByteScan(json.RawMessage(uat.AdminConsoleByteScanLedger))
	if err != nil {
		panic("the byte-scan ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	touching, _, err := uat.SweepAdminConsoleCiphertextQueries(json.RawMessage(uat.AdminConsoleQueryLedger))
	if err != nil {
		panic("the query ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	ids, _, err := uat.SweepAdminConsoleDivergenceRepairs(json.RawMessage(uat.AdminConsoleDivergenceRepairLedger))
	if err != nil {
		panic("the divergence-repair ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	steps, _, err := uat.SweepAdminConsoleRunbook(json.RawMessage(uat.AdminConsoleRunbookLedger))
	if err != nil {
		panic("the runbook ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	applied, refusedOnHash, _, err := uat.SweepAdminConsoleApprovals(json.RawMessage(uat.AdminConsoleApprovalLedger))
	if err != nil {
		panic("the approval ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	return uat.AdminConsoleProof{
		Peer: uat.AdminConsolePeer,

		// EVERY COUNT BELOW COMES FROM THE SWEEP, NEVER FROM A LITERAL. A literal here would be the one place
		// this proof could disagree with the bytes it carries, and (a), (b) and (d) are equalities rather than
		// zeros — an equality written by hand is an equality nobody checked.
		RelayLedger:                       json.RawMessage(uat.AdminConsoleRelayLedger),
		RelayExportedMethods:              relay,
		RelayMethodsBehindTheIdentityGate: gated,

		RouteLedger:      json.RawMessage(uat.AdminConsoleRouteLedger),
		DeclaredRoutes:   declared,
		AxeScannedRoutes: scanned,

		ByteScanLedger:                       json.RawMessage(uat.AdminConsoleByteScanLedger),
		SecretValuesInResponsesAndSourceMaps: hits,

		QueryLedger:          json.RawMessage(uat.AdminConsoleQueryLedger),
		CiphertextQueryNames: touching,

		SweepSubjects:       uat.AdminConsoleSweepSubjects,
		SweepItemsCompared:  len(uat.AdminConsoleSweepSubjects),
		SweepItemsBeforeE25: uat.AdminConsoleSweepFloorBeforeE25,

		DivergenceRepairLedger: json.RawMessage(uat.AdminConsoleDivergenceRepairLedger),
		RepairedDivergenceRows: ids,

		RunbookLedger:              json.RawMessage(uat.AdminConsoleRunbookLedger),
		RunbookStepsOnThePublicAPI: steps,

		ApprovalLedger:                         json.RawMessage(uat.AdminConsoleApprovalLedger),
		ApprovalsDecidedFromTheConsole:         applied,
		ApprovalsRefusedOnARequestHashMismatch: refusedOnHash,

		Contracts:       uat.AdminConsoleContracts,
		ContractsDigest: uat.AdminConsoleContractsDigest(),
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

// TestCommittedAdminConsoleBundleIsTheGeneratorOutput pins the committed bundle to the tree: it must be
// BYTE-identical to this generator's output, so a contract-ledger change, a ledger-row change, a case-set
// change or a baseline change cannot leave a stale bundle verifying green.
// Regenerate with: PALAI_WRITE_ADMIN_CONSOLE_BUNDLE=1 go test ./tests/uat/admin-console/
func TestCommittedAdminConsoleBundleIsTheGeneratorOutput(t *testing.T) {
	want := buildAdminConsoleManifest(t)
	path := filepath.Join(bundleDir(t), "manifest.json")

	if os.Getenv("PALAI_WRITE_ADMIN_CONSOLE_BUNDLE") == "1" {
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
		t.Fatalf("read the committed bundle: %v (regenerate with PALAI_WRITE_ADMIN_CONSOLE_BUNDLE=1)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the committed %s bundle is not this generator's output — regenerate with PALAI_WRITE_ADMIN_CONSOLE_BUNDLE=1", uat.AdminConsoleBundle)
	}
}

// TestAdminConsoleReleaseVerifiesClean runs the committed bundle through the SHIPPED verifier and requires
// 0 failed / 0 missing / 0 secret. It is the `make evidence-verify RELEASE=admin-console-0.1.0` gate,
// in-process.
func TestAdminConsoleReleaseVerifiesClean(t *testing.T) {
	summary, err := uat.VerifyRelease(bundleDir(t), nil)
	if err != nil {
		t.Fatalf("verify the admin-console release: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("the admin-console bundle did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatal("the admin-console bundle verified 0 passed cases — a zero-case bundle is not a clean bundle")
	}
	t.Logf("%s: %s", uat.AdminConsoleBundle, summary.String())
}

// TestTheSevenNewCasesAreInTheBundle is the shrink guard the E18 T8 sweep taught: a release that OPENED seven
// ids must carry all seven, or the ids exist in the tree with no bundle certifying them.
func TestTheSevenNewCasesAreInTheBundle(t *testing.T) {
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
	for _, id := range uat.AdminConsoleCaseIDs {
		if status, ok := carried[id]; !ok {
			t.Errorf("%s is a case this epic OPENED but the bundle that certifies it does not carry it", id)
		} else if status != "PASS" {
			t.Errorf("%s is carried as %q; this release claims it green", id, status)
		}
	}
	for _, anchor := range []string{adminConsoleAnchorCaseID, approvalAnchorCaseID, shipAnchorCaseID, memoryAnchorCaseID, surfaceAnchorCaseID, wiringAnchorCaseID, tierAnchorCaseID} {
		if _, ok := carried[anchor]; !ok {
			t.Errorf("%s is not in the bundle — every anchor underneath this release rides along, so the release is judged on the tier it did NOT move", anchor)
		}
	}
}

// TestTheBundleCarriesNoFleetClaim is the other direction, and it is this release's version of the guard E24
// wrote for its own absent sixth case. E25 ran in PARALLEL with E24 and inherits from tool-approval-0.1.0, so
// it carries no `FLT-` case and owes no fleet proof — and a later reader completing the inheritance chain out
// of tidiness would make an admin-console release claim something about machines it never placed a run on,
// AND would change which gate PromoteGateFor dispatches to.
func TestTheBundleCarriesNoFleetClaim(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	// STRUCTURAL, NOT A SUBSTRING SCAN — E24's own note on this shape, arrived at from the other side: a byte
	// scan for "FLT-" would fail on this bundle's honest paragraph explaining why it carries none.
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
		if slices.Contains(uat.FleetCaseIDs, c.ID) {
			t.Errorf("the bundle carries the E24 case %s — E25 ran in PARALLEL with the fleet epic and inherits from %s, so a FLT- id here means the baseline moved and this release now owes a fleet proof it cannot produce", c.ID, baselineManifest)
		}
		if len(c.Fleet) != 0 {
			t.Errorf("%s carries a fleet_proof — E25 placed no run on any machine", c.ID)
		}
	}
}
