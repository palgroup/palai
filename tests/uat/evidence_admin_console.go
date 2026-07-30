package uat

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// AdminConsoleCaseIDs are the UAT ids E25 opens, and the `CON-` prefix is a GATE decision rather than a
// naming preference. The plan measured why: `UI-` is already inside `extensionIDPrefixes`
// (tests/uat/extensions/catalog_test.go), and that map IS the shipped extensions-0.1.0 bundle's case list —
// the generator reads it, and uat.CapabilityClaims feeds a digest folded into that bundle's every checksum.
// So a `UI-005` would either force the regeneration of a committed historical release, rewriting the record
// of a run that happened, or fall through PromoteGateFor to a weaker family gate that knows none of E25's
// guards and would pass it. E20, E21 and E22 each paid for that once.
//
// OWNERSHIP LIVES HERE; ESCAPING THE SWEEP IS NOT ALLOWED. tests/uat/extensions is the ONLY place in the
// tree that walks the cases DIRECTORY, so `CON-` is added to its prefix list in the same change that adds
// this list — measured, not assumed: a CON-001 directory under an UNLISTED prefix was verified to leave
// every UAT gate green, which is precisely the hole E23 T2's HIL-004 fell into (a case directory nothing
// resolved proofs for, reported green by silence). The two directions are closed in two places:
// tests/uat/extensions refuses a CON- dir that no catalog owns, and tests/uat/admin-console refuses an id in
// this list that has no directory or whose declared proofs are not in the tree.
//
// THIS LIST GROWS ONE ID PER TASK, and only when the directory exists. T1 opens CON-001, T2 opens CON-002,
// T3 opens CON-003, T4 opens CON-004; the remaining ids the plan names (CON-005..007) join as their tasks
// land, and the E25 exit gate is where they meet a bundle.
//
// CON-003 IS THE FIRST NON-BROWSER CASE IN THIS LIST, and its proof_class says so: `component-real` rather
// than `e2e-deterministic`, because T3 is the Go half of R1 and touches no console file. Its proofs are Go
// test FUNCTIONS, so the catalog gate resolves them as `func Name(` rather than as `test("title"`.
//
// CON-004 IS ITS BROWSER HALF, and the pair is what R1 is: T3 proves an operator-authored value reaches a
// real shell command and enters no durable row; T4 proves the surface a HUMAN types it into returns it
// nowhere — in no DOM node, no response body and no source map. Neither claim is the other's, and neither
// is the ceiling both share: giving an agent a secret is the agent having that secret.
//
// CON-005 IS THE SMALLEST CASE IN THIS LIST AND ITS SMALLNESS IS THE MEASUREMENT (plan §3.6 D3): E23 T9 had
// already opened GET /v1/approvals, both decision routes, the `approve` capability and the mandatory request
// hash, so T5 is a PAGE. What it claims is bounded by one thing no console profile can produce — a PARKED
// gated tool call on a real stack (DIV-UI-006) — so the release of the parked run stays a component-tier
// claim and the browser's claim ends at the decision reaching the single throat with the row's own binding.
// CON-006 IS THE FIRST CASE IN THIS LIST WITH LEGS ON BOTH SIDES OF THE BROWSER, and the split is a
// measurement rather than a structure: T6's console can be shown creating a repository binding, an agent, a
// revision bound to an environment and a run pinned to it, but it can NEVER be shown the consequence — no
// console profile reaches a tool call at all (the fake upstream scripts its events; the compose adapter is
// hardcoded with no ToolCalls, DIV-UNX-001), and a browser cannot read a subprocess's environment anyway. So
// its last proof is a component-real Go test that drives the SAME public HTTP routes the screens call, with no
// SQL below the tenant seed, and then watches a real `printenv` receive the keys. That test is the one place
// in the tree where E25 T3's pipe and T6's console meet, and it is why neither task's claim is the other's.
// CON-007 IS THE ONLY CASE IN THIS LIST WHOSE FIRST MEASUREMENT WAS OF A SHIPPED DOCUMENT. T7's RED is not a
// missing feature, it is docs/operations/jira-mcp-connection.md §3 telling operators for three and a half days
// to publish a `$REV_ID` that no /v1 response has ever contained — and the step being unreachable is what made
// the E23 T5 decision hung on it (approval_required) unreachable too. Its component leg EXECUTES that runbook
// over the public API alone, which is the only way a document is verified: it was edited three times while it
// was wrong, so being read is not verification and being edited is not either. Its browser legs are the screen,
// and its untrusted-text leg is an ATTACK rather than an assertion — the discovered description carries a
// <script> and an <a>, and the spec then counts elements, anchors and navigations.
var AdminConsoleCaseIDs = []string{"CON-001", "CON-002", "CON-003", "CON-004", "CON-005", "CON-006", "CON-007"}

// ---------------------------------------------------------------------------------------------------------
// The E25 T9 EXIT-gate proof type (plan §T9) — the `admin-console-0.1.0` sign-off.
//
// WHAT THIS BUNDLE CLAIMS, and every clause is narrower than the sentence a reader will want it to be:
//
//   - no write reaches the control plane through this console without an operator SESSION, and a console with
//     no password hash serves nothing at all — the gate is inside the RELAY, not in a proxy.ts and not in a
//     layout (§3.5 N2/N3/N5, GHSA-f82v-jwr5-mffw at CVSS 9.1);
//   - every page the console declares is axe-scanned, in BOTH colour schemes, under the WCAG 2.0+2.1+2.2 tags;
//   - a secret VALUE written through the configuration surface comes back from no read path, and that is
//     measured in three layers that each fail on a DIFFERENT mistake: the SQL query NAMES touching
//     `ciphertext`, the projection field sets, and a byte scan of every response body and browser-served
//     source map;
//   - a stranger can build an environment, a repository binding, an agent and an MCP tool grant without
//     writing `curl`, and a Slack-less installation can decide a gated tool call from a screen.
//
// AND THE HONEST CEILING, WHICH IS THIS BUNDLE'S MOST IMPORTANT SENTENCE. NOBODY OPERATED A DEPLOYED CONSOLE.
// AdminConsolePeer is STRUCTURALLY the literal "fake": every browser proof in this release ran against a
// deterministic fixture upstream or against a compose stack on one box, and compose is not a deployment
// (`capabilities_mount_test.go`). There is no manual screen-reader pass, axe's own published figure is 57% of
// WCAG issues on average, and E25 added the surface axe is WEAKEST on — forms. §6 leg 1 is OPEN and BIGGER.
// NO TIER ADVANCES: `console` closes PREVIEW.
//
// WHAT IT ADDS THAT NO EARLIER BUNDLE CLAIMED: that five rows of this tree's own divergence LEDGER — the
// record the console's evidence rested on — were measured to be WRONG and repaired.
// ---------------------------------------------------------------------------------------------------------

// AdminConsoleBundle is the E25 EXIT bundle's release name.
const AdminConsoleBundle = "admin-console-0.1.0"

// AdminConsolePeer is the ONE honest naming an AdminConsoleProof may carry. Every browser leg in this bundle
// drove a Chromium against either tests/fake-control-plane.mjs or a four-service compose stack on one
// machine. Neither is a deployment and neither is an operator. A bundle that cannot type the word "real" into
// this field cannot overclaim a configured console by accident.
const AdminConsolePeer = "fake"

// --- (a) the relay's methods and the identity gate --------------------------------------------------------

// adminConsoleRelayRow is one exported HTTP method of one Route Handler under the console's app/api tree.
//
// `Relay` is what separates the DATA surface from the door. Everything under app/api/palai proxies the
// control plane and must be session-gated; app/api/console/login is the door itself and cannot pass its own
// gate. Carrying the door as a row with Relay=false — rather than filtering it out of the ledger — is what
// makes the exception COUNTABLE: Complete() requires exactly one, and requires it to be the login route.
type adminConsoleRelayRow struct {
	File   string `json:"file"`
	Method string `json:"method"`
	Relay  bool   `json:"relay"`
	// Gate is the shape measured as the method body's FIRST TWO statements. The relay's rows must carry
	// exactly adminConsoleGateShape; the door's row carries its own name.
	Gate string `json:"gate"`
}

// adminConsoleGateShape is the exact two-statement opening tests/relay-gate.spec.ts pins on the console side
// (`const refused = requireSession(request); if (refused !== null) return refused;`). A gate further down the
// body, a gate on a different value, or a gate whose result is not returned is a DIFFERENT string here and
// fails — which is the whole reason the shape is carried rather than a boolean.
const adminConsoleGateShape = "const refused = requireSession(request); if (refused !== null) return refused;"

// adminConsoleDoorGate is the ONE non-relay gate value the ledger may carry, and it belongs to exactly one
// file. A second row with this value would be a second unauthenticated write path.
const adminConsoleDoorGate = "is-the-door: verifyPassword + Origin, issues the session this gate checks"

// adminConsoleLoginRoute is the door's file. Complete() pins the exception to it by NAME, so "the door" cannot
// migrate onto a data route.
const adminConsoleLoginRoute = "apps/web-console/app/api/console/login/route.ts"

// SweepAdminConsoleRelay decodes the relay ledger and returns the relay methods, the relay methods carrying
// the exact gate shape, and the non-relay (door) rows — group (a), RE-DERIVED from the carried rows rather
// than read off a declared count (the SweepActionableElements pattern).
func SweepAdminConsoleRelay(ledger json.RawMessage) (relay, gated int, doors []string, err error) {
	if len(ledger) == 0 {
		return 0, 0, nil, fmt.Errorf("no relay ledger to sweep: \"every exported method is behind the gate\" over nothing is vacuous")
	}
	var rows []adminConsoleRelayRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return 0, 0, nil, fmt.Errorf("the carried relay ledger is not JSON, so the gate count is unverifiable: %w", err)
	}
	if len(rows) == 0 {
		return 0, 0, nil, fmt.Errorf("the relay ledger is EMPTY: a console with no exported method proves nothing about gating one")
	}
	for _, row := range rows {
		if row.File == "" || row.Method == "" {
			return 0, 0, nil, fmt.Errorf("a relay row names no file or no method: %+v", row)
		}
		if !row.Relay {
			doors = append(doors, row.File+" "+row.Method)
			if row.File != adminConsoleLoginRoute || row.Gate != adminConsoleDoorGate {
				return 0, 0, nil, fmt.Errorf("%s %s is carried as a NON-relay export but is not the login door — an ungated export outside the door is exactly what this group counts", row.File, row.Method)
			}
			continue
		}
		relay++
		if row.Gate == adminConsoleGateShape {
			gated++
		}
	}
	return relay, gated, doors, nil
}

// --- (b) the declared routes and the axe sweep ------------------------------------------------------------

// adminConsoleRouteRow is one row of apps/web-console/lib/routes.ts beside the colour schemes axe actually
// scanned it in. `Lead` is carried because the design pass made it REQUIRED: a page with no first sentence is
// a page that never says what it is, and a11y.spec.ts fails on a blank one.
type adminConsoleRouteRow struct {
	Path         string   `json:"path"`
	ReadyTestID  string   `json:"ready_test_id"`
	Lead         string   `json:"lead"`
	AxeScannedIn []string `json:"axe_scanned_in"`
}

// AdminConsoleColourSchemes are the two Playwright projects the console suite runs. The dark one is E25's own
// finding: playwright-core@1.51.1 defaults `colorScheme` to "light" and writes that down in its types, so
// every axe scan this suite had ever run looked at the light palette while app/globals.css carried a whole
// dark block. A scan in one scheme is not a scan of the console.
var AdminConsoleColourSchemes = []string{"light", "dark"}

// SweepAdminConsoleRoutes decodes the route ledger and returns the declared routes and the routes scanned in
// EVERY colour scheme — group (b). A route scanned in one scheme only does not count, which is the point: it
// is the half that was invisible until this epic added the second project.
func SweepAdminConsoleRoutes(ledger json.RawMessage) (declared, scannedInEveryScheme int, unscanned []string, err error) {
	if len(ledger) == 0 {
		return 0, 0, nil, fmt.Errorf("no route ledger to sweep: \"every page is scanned\" over no pages is vacuous")
	}
	var rows []adminConsoleRouteRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return 0, 0, nil, fmt.Errorf("the carried route ledger is not JSON, so the axe coverage count is unverifiable: %w", err)
	}
	if len(rows) == 0 {
		return 0, 0, nil, fmt.Errorf("the route ledger is EMPTY: a console declaring no route cannot certify that every route was scanned")
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Path == "" || row.ReadyTestID == "" || strings.TrimSpace(row.Lead) == "" {
			return 0, 0, nil, fmt.Errorf("route %q carries no path, no readiness signal or no lead — all three are REQUIRED by lib/routes.ts, and a blank lead is a page that never says what it is", row.Path)
		}
		if seen[row.Path] {
			return 0, 0, nil, fmt.Errorf("route %q appears twice in the ledger — a duplicated row inflates the declared count without adding a page", row.Path)
		}
		seen[row.Path] = true
		declared++
		complete := true
		for _, scheme := range AdminConsoleColourSchemes {
			if !slices.Contains(row.AxeScannedIn, scheme) {
				complete = false
			}
		}
		if complete {
			scannedInEveryScheme++
		} else {
			unscanned = append(unscanned, row.Path)
		}
	}
	return declared, scannedInEveryScheme, unscanned, nil
}

// --- (c) the byte scan: response bodies and source maps ---------------------------------------------------

// adminConsoleByteScanRow is one surface a written secret value was searched in. `Kind` is the layer, `Hits`
// is how many times the sentinel was found, and `Probe`/`ProbeFound` are the layer's own NON-VACUITY control:
// a harmless token the same scan DID find in the same bytes.
//
// THE PROBE IS WHY THE ZERO MEANS ANYTHING. A scan over zero bytes reports zero hits, and so does a scan whose
// haystack was never actually read — which is not hypothetical: arm 2 and arm 3 of the shipped spec used to
// prove only that they had SOMETHING to scan (`scannedBodies > 0`, `assets.length > 0`), never that the scan
// could see a string that was genuinely in there. So each layer now names a token it found, and this group
// refuses a layer that found nothing.
type adminConsoleByteScanRow struct {
	Kind       string `json:"kind"`
	Subject    string `json:"subject"`
	Bytes      int    `json:"bytes_scanned"`
	Hits       int    `json:"sentinel_hits"`
	Probe      string `json:"probe"`
	ProbeFound bool   `json:"probe_found"`
}

// adminConsoleByteScanKinds are the three layers the T4 proof runs, and all three must be present. The third
// is the one that earned its place: a sentinel planted in a source COMMENT is stripped from the minified
// chunk, carried verbatim in the map's `sourcesContent` and never fetched by a browser — so only the
// source-map arm can name it, which is measured rather than assumed.
var adminConsoleByteScanKinds = []string{"dom", "response-body", "source-map"}

// SweepAdminConsoleByteScan decodes the byte-scan ledger and returns the sentinel hits, the layers whose probe
// was FOUND, and the layers covered — group (c).
func SweepAdminConsoleByteScan(ledger json.RawMessage) (hits, probed int, kinds []string, err error) {
	if len(ledger) == 0 {
		return 0, 0, nil, fmt.Errorf("no byte-scan ledger to sweep: \"no response body carries a secret\" over no bodies is vacuous")
	}
	var rows []adminConsoleByteScanRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return 0, 0, nil, fmt.Errorf("the carried byte-scan ledger is not JSON, so the leak count is unverifiable: %w", err)
	}
	byKind := map[string]bool{}
	for _, row := range rows {
		if !slices.Contains(adminConsoleByteScanKinds, row.Kind) {
			return 0, 0, nil, fmt.Errorf("byte-scan row %q declares kind %q, which is not one of %v", row.Subject, row.Kind, adminConsoleByteScanKinds)
		}
		if row.Bytes <= 0 {
			return 0, 0, nil, fmt.Errorf("byte-scan row %q scanned %d bytes — a scan over nothing reports zero hits and proves nothing", row.Subject, row.Bytes)
		}
		if strings.TrimSpace(row.Probe) == "" || !row.ProbeFound {
			return 0, 0, nil, fmt.Errorf("byte-scan layer %q names no probe it actually found — a haystack nothing was ever located in is a haystack nobody has shown was read", row.Kind)
		}
		if strings.Contains(strings.ToUpper(row.Probe), "SENTINEL") {
			return 0, 0, nil, fmt.Errorf("byte-scan layer %q uses the SENTINEL itself as its probe — the control has to be a token that is ALLOWED to be there, or finding it is the failure", row.Kind)
		}
		if byKind[row.Kind] {
			return 0, 0, nil, fmt.Errorf("two rows for layer %q — one row per layer, or a duplicated probe stands in for a missing one", row.Kind)
		}
		byKind[row.Kind] = true
		probed++
		hits += row.Hits
	}
	for _, kind := range adminConsoleByteScanKinds {
		if !byKind[kind] {
			return 0, 0, nil, fmt.Errorf("no row for the %q layer — without it, that layer's zero is indistinguishable from a layer that never ran (the source-map arm is exactly the layer the other two pass over)", kind)
		}
		kinds = append(kinds, kind)
	}
	return hits, probed, kinds, nil
}

// --- (d) the SQL queries that touch `ciphertext` ----------------------------------------------------------

// AdminConsoleCiphertextQueries is the CANONICAL pair, and it is exactly two. InsertSecretRef writes a sealed
// blob; ResolveSecretRef reads one back for internal decryption and its single caller (SecretStore.Resolve) is
// reachable from no /v1 route. Everything else that speaks about a secret speaks about its NAME, its VERSION
// and when it was written.
//
// storage/ciphertext_sweep_test.go pins the same pair against the real storage/queries corpus on every
// `make verify`. This copy is what a BUNDLE is judged against, and tests/uat/admin-console re-derives one from
// the other so the two cannot drift.
var AdminConsoleCiphertextQueries = []string{"InsertSecretRef", "ResolveSecretRef"}

// adminConsoleQueryRow is one FILE of storage/queries: how many named statements it holds, and which of
// them mention the ciphertext column in their BODY (never in their comments — the sweep that produced this
// proved that by searching for a term that appears only in prose and matching nothing).
//
// IT IS PER FILE RATHER THAN PER STATEMENT, and the reason is the corpus floor rather than brevity: the
// anti-vacuity check is that the PARSER still finds statements at all, which is a count, while the property
// being pinned is a two-element NAME set. Carrying four hundred and seventy-odd single-boolean rows would
// buy neither of them anything the co-run does not already give — tests/uat/admin-console re-derives this
// whole table from the real storage/queries corpus and fails on any drift.
type adminConsoleQueryRow struct {
	File       string   `json:"file"`
	Statements int      `json:"statements"`
	Ciphertext []string `json:"ciphertext_statements"`
}

// adminConsoleQueryCorpusFloor is the anti-vacuity floor storage/ciphertext_sweep_test.go uses: a parser that
// stopped finding `-- name:` markers returns an empty list for `ciphertext` AND for everything else, and an
// empty list would satisfy a `want` somebody had reduced to empty. Counting the whole corpus catches the
// parser independently of the term.
const adminConsoleQueryCorpusFloor = 300

// SweepAdminConsoleCiphertextQueries decodes the query ledger and returns the SORTED names whose statement
// touches `ciphertext` plus the corpus size — group (d), re-derived from the carried rows.
func SweepAdminConsoleCiphertextQueries(ledger json.RawMessage) (touching []string, corpus int, err error) {
	if len(ledger) == 0 {
		return nil, 0, fmt.Errorf("no query ledger to sweep: \"exactly two queries touch the ciphertext column\" over no queries is vacuous")
	}
	var rows []adminConsoleQueryRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return nil, 0, fmt.Errorf("the carried query ledger is not JSON, so the ciphertext pin is unverifiable: %w", err)
	}
	seenFile := map[string]bool{}
	seenName := map[string]bool{}
	for _, row := range rows {
		if row.File == "" || row.Statements <= 0 {
			return nil, 0, fmt.Errorf("query-ledger row %q declares %d statement(s) — a file with none is a file the parser did not read", row.File, row.Statements)
		}
		if seenFile[row.File] {
			return nil, 0, fmt.Errorf("query file %q appears twice — a duplicated row inflates the corpus without adding a statement", row.File)
		}
		seenFile[row.File] = true
		corpus += row.Statements
		if len(row.Ciphertext) > row.Statements {
			return nil, 0, fmt.Errorf("query file %q claims %d ciphertext statement(s) out of %d — more than it holds", row.File, len(row.Ciphertext), row.Statements)
		}
		for _, name := range row.Ciphertext {
			if seenName[name] {
				return nil, 0, fmt.Errorf("statement %q is claimed by two files", name)
			}
			seenName[name] = true
			touching = append(touching, name)
		}
	}
	slices.Sort(touching)
	return touching, corpus, nil
}

// --- (e) the conformance sweep's item-shape floor ---------------------------------------------------------

// AdminConsoleSweepFloorBeforeE25 is what tests/conformance.test.mjs compared before this epic: the THREE
// collections a bootstrap stack seeds (organizations, projects, api-keys). Everything else was envelope-only,
// which is how the fixture's knowledge-base row carried `display_name` for months against a real projection
// whose field is `name`.
const AdminConsoleSweepFloorBeforeE25 = 3

// --- (f) the divergence rows E25 measured to be WRONG ------------------------------------------------------

// adminConsoleDivergenceRepairRow is one row of tests/divergences.mjs the console's evidence rested on, the
// sentence that was false, and how the repair was RE-OBSERVED. `Verdict` is "closed" (the row is deleted
// because the difference no longer exists) or "repaired" (the row stays, narrowed to what is still true).
type adminConsoleDivergenceRepairRow struct {
	ID         string `json:"id"`
	Was        string `json:"was"`
	Verdict    string `json:"verdict"`
	Reobserved string `json:"reobserved"`
}

// adminConsoleDivergenceVerdicts are the only two honest outcomes for a measured-wrong ledger row. A third
// value would be a row somebody neither closed nor narrowed.
var adminConsoleDivergenceVerdicts = []string{"closed", "repaired"}

// SweepAdminConsoleDivergenceRepairs decodes the repair ledger and returns the row ids and how many were
// re-observed — group (f). A repair with no re-observation is a source read, which is exactly the move that
// put the false sentences there in the first place.
func SweepAdminConsoleDivergenceRepairs(ledger json.RawMessage) (ids []string, reobserved int, err error) {
	if len(ledger) == 0 {
		return nil, 0, fmt.Errorf("no divergence-repair ledger to sweep: this bundle's added claim is that five ledger rows were wrong, and a claim over no rows is vacuous")
	}
	var rows []adminConsoleDivergenceRepairRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return nil, 0, fmt.Errorf("the carried divergence-repair ledger is not JSON: %w", err)
	}
	for _, row := range rows {
		if row.ID == "" || strings.TrimSpace(row.Was) == "" {
			return nil, 0, fmt.Errorf("a repair row names no id or does not say what was false: %+v", row)
		}
		if !slices.Contains(adminConsoleDivergenceVerdicts, row.Verdict) {
			return nil, 0, fmt.Errorf("%s carries verdict %q, want one of %v — a row is either closed or narrowed, never something else", row.ID, row.Verdict, adminConsoleDivergenceVerdicts)
		}
		ids = append(ids, row.ID)
		if strings.TrimSpace(row.Reobserved) != "" {
			reobserved++
		}
	}
	slices.Sort(ids)
	return ids, reobserved, nil
}

// --- (g) the runbook, executed on the public API alone ------------------------------------------------------

// adminConsoleRunbookStep is one call of docs/operations/jira-mcp-connection.md §3. `PublicAPI` is false for a
// step that needed anything below /v1 — an SQL insert, a store method, a fixture hook. The whole point of T7
// is that this list has none.
type adminConsoleRunbookStep struct {
	Step      string `json:"step"`
	Call      string `json:"call"`
	PublicAPI bool   `json:"public_api"`
}

// SweepAdminConsoleRunbook decodes the runbook ledger and returns the steps completed on the public API alone
// plus the steps that were NOT — group (g).
func SweepAdminConsoleRunbook(ledger json.RawMessage) (onPublicAPI int, below []string, err error) {
	if len(ledger) == 0 {
		return 0, nil, fmt.Errorf("no runbook ledger to sweep: \"a shipped runbook runs on the public API alone\" over no steps is vacuous")
	}
	var steps []adminConsoleRunbookStep
	if err := json.Unmarshal(ledger, &steps); err != nil {
		return 0, nil, fmt.Errorf("the carried runbook ledger is not JSON: %w", err)
	}
	for _, step := range steps {
		if step.Step == "" || !strings.HasPrefix(step.Call, "GET /v1") && !strings.HasPrefix(step.Call, "POST /v1") {
			return 0, nil, fmt.Errorf("runbook step %q names the call %q, which is not a /v1 request — a step whose call is not addressable cannot be evidence that the runbook is executable", step.Step, step.Call)
		}
		if step.PublicAPI {
			onPublicAPI++
		} else {
			below = append(below, step.Step)
		}
	}
	return onPublicAPI, below, nil
}

// --- (h) the approvals decided from the console ------------------------------------------------------------

// adminConsoleApprovalRow is one decision the console's approval queue sent. `HashMatched` is whether the
// request hash the row carried was still the row's own when the server checked it; `Applied` is whether the
// decision took effect.
type adminConsoleApprovalRow struct {
	ApprovalID  string `json:"approval_id"`
	Decision    string `json:"decision"`
	HashMatched bool   `json:"request_hash_matched"`
	Applied     bool   `json:"applied"`
	Outcome     string `json:"outcome"`
}

// SweepAdminConsoleApprovals decodes the approval ledger and returns the decisions that APPLIED, the ones
// refused on a request-hash mismatch, and any decision that applied while its hash did NOT match — group (h).
//
// The third return is the one that must be empty: an approval whose arguments changed authorizing anything is
// the failure this whole surface exists to prevent, and it is re-derived here rather than believed.
func SweepAdminConsoleApprovals(ledger json.RawMessage) (applied, refusedOnHash int, appliedOnMismatch []string, err error) {
	if len(ledger) == 0 {
		return 0, 0, nil, fmt.Errorf("no approval ledger to sweep: \"a gated call was decided from a screen\" over no decisions is vacuous")
	}
	var rows []adminConsoleApprovalRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return 0, 0, nil, fmt.Errorf("the carried approval ledger is not JSON: %w", err)
	}
	for _, row := range rows {
		if row.ApprovalID == "" || row.Decision == "" || strings.TrimSpace(row.Outcome) == "" {
			return 0, 0, nil, fmt.Errorf("an approval row is missing its id, decision or outcome: %+v", row)
		}
		if row.Decision != "approve" && row.Decision != "deny" {
			return 0, 0, nil, fmt.Errorf("approval %s carries decision %q, want approve or deny", row.ApprovalID, row.Decision)
		}
		switch {
		case row.Applied && !row.HashMatched:
			appliedOnMismatch = append(appliedOnMismatch, row.ApprovalID)
		case row.Applied:
			applied++
		case !row.HashMatched:
			refusedOnHash++
		}
	}
	return applied, refusedOnHash, appliedOnMismatch, nil
}

// --- (i) the canonical vendor ledger -------------------------------------------------------------------

// AdminConsoleContracts is the CANONICAL ledger of the published vendor requirements and on-machine
// measurements E25 ACTED ON — the FleetContracts / ToolApprovalContracts / CodeAndShipContracts discipline. A
// proof's contracts must EQUAL this table, so a bundle cannot build a surface while quietly dropping the row
// that named its cost.
//
// IT CARRIES THE ROWS E25 ACTED ON, NOT EVERY ROW E25 INHERITED. §3.5 inherits N1-N16 from
// phase-23-admin-console.md with their original fetch dates, and a ledger that copied all sixteen would be a
// bibliography rather than a record of decisions. What is here is the subset a reader can hold this release to:
// each row changed a file, and the file is named.
//
// N19 IS DELIBERATELY ABSENT AND ITS ABSENCE IS THE HONEST ANSWER. §3.5 marks it UNCONFIRMED (1/1): whether a
// browser offers to SAVE a lone `type="password"` field outside a login form, and whether `new-password`
// suppresses that offer, could not be read on any published page — MDN documents the token's effect on
// FILLING and its `off` sentence is scoped, verbatim, to "a site's login form". A ledger of "published
// requirements we implement" is the wrong home for something nobody could read, and it entered no test, no doc
// sentence and no bundle field. It lives in docs/operations/known-gaps-1.0.md as CON-P5 and is measured by a
// §6 manual-observation leg. TestTheAdminConsoleLedgerRefusesTheUnconfirmedRow pins it.
var AdminConsoleContracts = []ContractRequirement{
	{
		Divergence: "N2",
		SourceURL:  "https://github.com/advisories/GHSA-f82v-jwr5-mffw (inherited from phase-23-admin-console.md §3.5, fetched 2026-07-28)",
		Requirement: "⭐⭐ THE DEFINING DECISION OF THIS EPIC, TAKEN FROM THE VENDOR'S OWN CVSS 9.1 INCIDENT: an attacker " +
			"could \"bypass authorization checks … if the authorization check occurs in middleware\". So E25's " +
			"identity gate is INSIDE THE RELAY — `requireSession` is the first two statements of every exported " +
			"method of every Route Handler under app/api/palai — and not in a `proxy.ts`. Three independent " +
			"reasons point at the same file: the vendor's own advisory, Partial Rendering (N5), and the fact " +
			"that every data path in this console already funnels through one function family",
	},
	{
		Divergence: "N5",
		SourceURL:  "https://nextjs.org/docs (Partial Rendering; inherited from phase-23-admin-console.md §3.5, fetched 2026-07-28)",
		Requirement: "A LAYOUT DOES NOT RE-RUN ON EVERY NAVIGATION. Partial Rendering re-uses the layout subtree across " +
			"a client-side navigation, so an authorization check in `app/layout.tsx` runs on the first request " +
			"and can be silently skipped afterwards. This is the second reason the gate is in the relay, and it " +
			"is why the un-configured console on UNCONFIGURED_PORT is probed on /login rather than on / — the " +
			"static shell still RENDERS, and \"does not serve\" is a statement about data, not about markup",
	},
	{
		Divergence: "N7",
		SourceURL:  "https://nextjs.org/docs (cookie examples; inherited from phase-23-admin-console.md §3.5, fetched 2026-07-28)",
		Requirement: "A DELIBERATE DIVERGENCE, NAMED RATHER THAN DRIFTED INTO: the vendor's own session-cookie examples " +
			"use `SameSite=Lax`; this console uses `Strict`. The cost is real and is paid by the operator — a " +
			"link from another site lands on a console that looks signed out and one more click is needed — and " +
			"it is taken because the thing behind this origin is a write proxy holding an UNLIMITED credential",
	},
	{
		Divergence: "N8",
		SourceURL:  "https://nextjs.org/docs (Server Actions security; inherited from phase-23-admin-console.md §3.5, fetched 2026-07-28)",
		Requirement: "SERVER ACTIONS GET CSRF PROTECTION FOR FREE; ROUTE HANDLERS DO NOT. This console uses no Server " +
			"Actions, which IS a conscious divergence from the vendor's recommendation for mutations — they " +
			"would open a SECOND write path that the public-API-only proof cannot see. The protection given up " +
			"is written by hand instead: an explicit Origin-vs-Host comparison on every non-GET, refusing a " +
			"missing Origin outright, as the third refusal inside `requireSession`",
	},
	{
		Divergence: "N10",
		SourceURL:  "https://github.com/dequelabs/axe-core/blob/develop/doc/API.md#tags (inherited from phase-23-admin-console.md §3.5, fetched 2026-07-28)",
		Requirement: "`wcag2a`/`wcag2aa` SELECT WCAG 2.0 RULES ONLY. `wcag21a`, `wcag21aa` and `wcag22aa` are separate " +
			"tags and were entirely outside this console's scan, so 2.4.11 Focus Not Obscured, 3.3.7 Redundant " +
			"Entry and 3.3.8 Accessible Authentication — the three that are a FORM's criteria — were unmeasured " +
			"on a surface about to gain nine forms. E25 widened the tag set and then MEASURED what the widening " +
			"buys, because an unknown tag selects no rules and reports clean: exactly three rules " +
			"(autocomplete-valid, avoid-inline-spacing, target-size), and `wcag21a` adds NONE",
	},
	{
		Divergence: "N13",
		SourceURL:  "https://developer.mozilla.org/en-US/docs/Web/HTML/Reference/Attributes/autocomplete (inherited, fetched 2026-07-28)",
		Requirement: "BROWSERS DO NOT HONOUR `autocomplete=\"off\"` ON PASSWORD FIELDS, verbatim: \"In most modern " +
			"browsers, setting autocomplete to \\\"off\\\" will not prevent a password manager from asking the user " +
			"if they would like to save username and password information, or from automatically filling in " +
			"those values in a site's login form.\" This is the measurement the ANCESTOR plan made correctly and " +
			"then over-concluded from — see N17",
	},
	{
		Divergence: "N17",
		SourceURL:  "https://developer.mozilla.org/en-US/docs/Web/HTML/Reference/Attributes/autocomplete (fetched 2026-07-29)",
		Requirement: "⭐ THE TOKEN FOR A NEW SECRET FIELD IS `new-password`, NOT `off`, verbatim: \"A new password. When " +
			"creating a new account or changing passwords, this should be used for an 'Enter your new password' " +
			"or 'Confirm new password' field… This may be used by the browser both to avoid accidentally filling " +
			"in an existing password and to offer assistance in creating a secure password.\" An environment " +
			"value is EXACTLY the target of the accidental-fill risk (the operator's own console password " +
			"dropped into a box whose contents an agent will then use as a credential), and the same page " +
			"restricts `off` to \"CAPTCHA or one-time token fields\". THIS ROW IS WHY THE ANCESTOR PLAN'S REFUSAL " +
			"WAS LIFTED: three of the four exposures it counted are named by this token, and the fourth — an " +
			"origin with no identity — is what T1 closed. Paste is NOT blocked (WCAG 2.2 SC 3.3.8)",
	},
	{
		Divergence: "N18",
		SourceURL:  "https://cheatsheetseries.owasp.org/cheatsheets/Secrets_Management_Cheat_Sheet.html (fetched 2026-07-29)",
		Requirement: "THREE OWASP LINES, AND E25 CLOSES ONE, BOUNDS ONE AND FILES ONE. §2.7.2 Rotation — \"You should " +
			"regularly rotate secrets so that any stolen credentials will only work for a short time\" — is " +
			"CLOSED: the rotate route already existed and T4 made it a button, on the same wire as create. §2.3 " +
			"Access Control — \"Engineers should not have access to all secrets… the Least Privilege principle " +
			"should be applied\" — is BOUNDED: T1 built the door, but the key behind it is still unlimited " +
			"(`Scope.HasScope` returns true for every capability on an empty scope set), so the narrow-key " +
			"recipe is documented and MEASURING which pages work with it is §6 leg 4. §2.6 Auditing — \"When the " +
			"secret was used and by whom/what\" — is FILED as CON-P3 and not fixed: `secret_refs` keeps no read " +
			"audit and no /v1 route exposes one",
	},
	{
		Divergence: "N20",
		SourceURL:  "MEASURED against the repo's PINNED playwright-core@1.51.1 type declarations (node_modules/playwright-core/types/types.d.ts, `colorScheme`), read 2026-07-30",
		Requirement: "⭐ THE VENDOR WRITES ITS OWN DEFAULT DOWN, verbatim: \"Passing `null` resets emulation to system " +
			"defaults. Defaults to 'light'.\" `playwright.config.ts` defined ONE project and named no " +
			"`colorScheme`, so EVERY axe scan this suite had ever run looked at the light palette — while " +
			"app/globals.css carries a whole `@media (prefers-color-scheme: dark)` block redefining text, border " +
			"and accent. The dark palette was outside automated accessibility coverage entirely, which is how a " +
			"2.63:1 skip link — white text on the lightened dark accent, the FIRST Tab stop on every page — " +
			"stayed green. E25 adds a second project running the WHOLE spec set rather than a named list of the " +
			"files that call AxeBuilder today, because a hand-maintained list is the same shape as the " +
			"hand-written route list lib/routes.ts exists to abolish",
	},
	{
		Divergence: "N21",
		SourceURL:  "MEASURED with the WCAG 2.x contrast formula against the rendered console on both colour schemes (apps/web-console/tests/contrast.spec.ts), 2026-07-30",
		Requirement: "⭐ AXE CANNOT SEE SC 1.4.11, AND EVERY CONTROL IN THIS CONSOLE FAILED IT. axe-core has NO rule for " +
			"a control's own BOUNDARY — `color-contrast` compares text to background and nothing else — and " +
			"inputs and buttons here take their background from the page, so the border is the only thing " +
			"marking where a control is. Measured before the fix: 53/53 controls under 3:1 in light (weakest " +
			"1.36:1) and 56/56 in dark (weakest 1.42:1). The repair is also a divergence: Radix Colors " +
			"guarantees its steps in APCA (Lc), not WCAG 2.x, so its published role for step 7 (\"UI element " +
			"border and focus rings\") re-measures at 1.53:1 and step 8 at 1.86:1 — copying the vendor's own " +
			"role table would have reproduced the exact defect. Step 10 is the first usable neutral. After: " +
			"0/55 and 0/57 failing, weakest 3.69:1 and 4.45:1",
	},
}

// adminConsoleContractParts flattens the canonical ledger into hashParts input, so the digest is re-derivable
// from the CODE table alone and a bundle cannot present a self-consistent digest over an edited ledger.
func adminConsoleContractParts() []string {
	parts := make([]string, 0, 3*len(AdminConsoleContracts))
	for _, req := range AdminConsoleContracts {
		parts = append(parts, req.Divergence, req.SourceURL, req.Requirement)
	}
	return parts
}

// AdminConsoleContractsDigest is hashParts over the CANONICAL contract ledger — the E25 bundle's checksum
// anchor. A dropped or reworded §3.5 row moves every checksum in the release.
func AdminConsoleContractsDigest() string { return hashParts(adminConsoleContractParts()...) }

// --- the proof ---------------------------------------------------------------------------------------------

// AdminConsoleProof is the evidence an admin_console_claim requires (plan §T9 — the E25 EXIT anchor). Its
// groups are the plan's (a)..(h) plus the contract ledger:
//
//	(a) RelayLedger / RelayExportedMethods / RelayMethodsBehindTheIdentityGate — MUST BE EQUAL, RE-DERIVED,
//	    with the ONE non-relay export (the login door) carried as a countable row rather than filtered out;
//	(b) RouteLedger / DeclaredRoutes / AxeScannedRoutes — MUST BE EQUAL, RE-DERIVED, and "scanned" means
//	    scanned in EVERY colour scheme: a light-only scan is what this epic found had been happening;
//	(c) ByteScanLedger / SecretValuesInResponsesAndSourceMaps — MUST BE ZERO, RE-DERIVED, over a ledger that
//	    carries a PLANTED leak per layer which the scan actually caught;
//	(d) QueryLedger / CiphertextQueryNames — MUST BE EXACTLY TWO and exactly the canonical pair, RE-DERIVED
//	    from the carried rows over a corpus big enough to prove the parser still parses;
//	(e) SweepItemsCompared / SweepItemsBeforeE25 — MUST HAVE RISEN, with the compared subjects carried so a
//	    number nobody can attribute is impossible;
//	(f) DivergenceRepairLedger / RepairedDivergenceRows — every row RE-OBSERVED, never repaired on a source
//	    read, which is the move that put the false sentences there;
//	(g) RunbookLedger / RunbookStepsOnThePublicAPI — every step of a SHIPPED runbook, on /v1 alone;
//	(h) ApprovalLedger / ApprovalsDecidedFromTheConsole / ApprovalsRefusedOnARequestHashMismatch — both
//	    RE-DERIVED, and an approval that APPLIED on a mismatched hash fails the proof outright.
//
// HONEST CEILING, MECHANICALLY ENFORCED: Peer must be the literal "fake". This bundle is STRUCTURALLY
// incapable of claiming that a real operator configured a DEPLOYED console — see this section's header.
type AdminConsoleProof struct {
	Peer string `json:"peer"`

	// (a) The gate is inside the relay, and every exported method goes through it.
	RelayLedger                       json.RawMessage `json:"relay_ledger"`
	RelayExportedMethods              int             `json:"relay_exported_methods"`
	RelayMethodsBehindTheIdentityGate int             `json:"relay_methods_behind_the_identity_gate"`

	// (b) Every declared page is scanned, in both colour schemes.
	RouteLedger      json.RawMessage `json:"route_ledger"`
	DeclaredRoutes   int             `json:"declared_routes"`
	AxeScannedRoutes int             `json:"axe_scanned_routes"`

	// (c) No written value comes back in any byte a browser received.
	ByteScanLedger                       json.RawMessage `json:"byte_scan_ledger"`
	SecretValuesInResponsesAndSourceMaps int             `json:"secret_values_in_responses_and_source_maps"`

	// (d) Exactly two SQL statements in the tree touch the ciphertext column.
	QueryLedger          json.RawMessage `json:"query_ledger"`
	CiphertextQueryNames []string        `json:"ciphertext_query_names"`

	// (e) The fake-vs-real conformance sweep compares more item shapes than it did before this epic.
	SweepSubjects       []string `json:"sweep_subjects"`
	SweepItemsCompared  int      `json:"sweep_items_compared"`
	SweepItemsBeforeE25 int      `json:"sweep_items_before_e25"`

	// (f) The divergence-ledger rows this epic measured to be WRONG, each re-observed.
	DivergenceRepairLedger json.RawMessage `json:"divergence_repair_ledger"`
	RepairedDivergenceRows []string        `json:"repaired_divergence_rows"`

	// (g) A shipped runbook, executed on the public API alone.
	RunbookLedger              json.RawMessage `json:"runbook_ledger"`
	RunbookStepsOnThePublicAPI int             `json:"runbook_steps_on_the_public_api"`

	// (h) Gated tool calls decided from a screen, and the ones a changed request refused.
	ApprovalLedger                         json.RawMessage `json:"approval_ledger"`
	ApprovalsDecidedFromTheConsole         int             `json:"approvals_decided_from_the_console"`
	ApprovalsRefusedOnARequestHashMismatch int             `json:"approvals_refused_on_a_request_hash_mismatch"`

	// (i) The published contracts and measurements, anchored to the code table.
	Contracts       []ContractRequirement `json:"contracts"`
	ContractsDigest string                `json:"contracts_digest"`
}

// Complete reports the groups hold on a FAKE peer AND re-derives (a) through (h) from the bytes the proof
// carries. A proof that declares four gated methods over a ledger holding five, a route list longer than the
// scan list, a zero over a byte scan containing a hit, or a "two" over three ciphertext queries fails HERE —
// in the shape verifier — rather than in a dedicated test somebody could forget to run.
func (p AdminConsoleProof) Complete() bool {
	if p.Peer != AdminConsolePeer || p.ContractsDigest != AdminConsoleContractsDigest() ||
		!slices.Equal(p.Contracts, AdminConsoleContracts) {
		return false
	}
	// (a) Every relay method is behind the gate, and the ONE exception is the door.
	relay, gated, doors, err := SweepAdminConsoleRelay(p.RelayLedger)
	if err != nil || relay != gated || p.RelayExportedMethods != relay ||
		p.RelayMethodsBehindTheIdentityGate != gated {
		return false
	}
	if relay < 2 || len(doors) != 1 {
		return false // one method proves nothing about "every"; two doors is two unauthenticated writes
	}
	// (b) Every declared route was scanned, in every colour scheme.
	declared, scanned, unscanned, err := SweepAdminConsoleRoutes(p.RouteLedger)
	if err != nil || declared != scanned || len(unscanned) != 0 ||
		p.DeclaredRoutes != declared || p.AxeScannedRoutes != scanned {
		return false
	}
	if declared < 2 {
		return false
	}
	// (c) No secret value in any byte a browser received, over a scan shown to bite in all three layers.
	hits, probed, kinds, err := SweepAdminConsoleByteScan(p.ByteScanLedger)
	if err != nil || hits != 0 || p.SecretValuesInResponsesAndSourceMaps != 0 ||
		probed != len(adminConsoleByteScanKinds) || len(kinds) != len(adminConsoleByteScanKinds) {
		return false
	}
	// (d) Exactly the canonical two queries touch `ciphertext`, over a corpus the parser still reads.
	touching, corpus, err := SweepAdminConsoleCiphertextQueries(p.QueryLedger)
	if err != nil || !slices.Equal(touching, AdminConsoleCiphertextQueries) ||
		!slices.Equal(p.CiphertextQueryNames, AdminConsoleCiphertextQueries) ||
		corpus < adminConsoleQueryCorpusFloor {
		return false
	}
	// (e) The sweep compares more than it did, and the subjects are named rather than counted.
	if p.SweepItemsBeforeE25 != AdminConsoleSweepFloorBeforeE25 ||
		p.SweepItemsCompared <= p.SweepItemsBeforeE25 ||
		len(p.SweepSubjects) != p.SweepItemsCompared {
		return false
	}
	// (f) Every repaired ledger row was RE-OBSERVED.
	ids, reobserved, err := SweepAdminConsoleDivergenceRepairs(p.DivergenceRepairLedger)
	if err != nil || reobserved != len(ids) || !slices.Equal(p.RepairedDivergenceRows, ids) {
		return false
	}
	if len(ids) < 1 {
		return false
	}
	// (g) Every runbook step ran on /v1 alone.
	onPublicAPI, below, err := SweepAdminConsoleRunbook(p.RunbookLedger)
	if err != nil || len(below) != 0 || p.RunbookStepsOnThePublicAPI != onPublicAPI || onPublicAPI < 2 {
		return false
	}
	// (h) Decisions applied, a changed request refused, and NOTHING applied on a mismatch.
	applied, refusedOnHash, appliedOnMismatch, err := SweepAdminConsoleApprovals(p.ApprovalLedger)
	if err != nil || len(appliedOnMismatch) != 0 ||
		p.ApprovalsDecidedFromTheConsole != applied ||
		p.ApprovalsRefusedOnARequestHashMismatch != refusedOnHash {
		return false
	}
	if applied < 1 || refusedOnHash < 1 {
		return false // a queue that never decided anything, or never refused a stale binding
	}
	return true
}

// carriesE25AdminConsoleCase reports whether a case is one of the seven ids E25 OPENED — the FAMILY marker,
// shared by the manifest verifier and PromoteGateFor so the two can never disagree about what an E25 release
// is.
//
// THE FAMILY IS RECOGNIZED BY THE CASE IDS, NEVER BY THE admin_console_claim THE GATE ENFORCES. Dispatching on
// the claim marker is precisely how a release DROPS it, reroutes to a weaker family and passes — the
// promote-gate-family-dispatch defect this repository has shipped once already.
func carriesE25AdminConsoleCase(c evidenceCase) bool {
	return slices.Contains(AdminConsoleCaseIDs, c.ID)
}

// verifyE25AdminConsolePresence stops the re-derivations from being OPTIONAL: a manifest carrying ANY of the
// seven E25 cases MUST carry EXACTLY ONE admin_console_claim with its proof. "Exactly one" because
// AdminConsolePromoteGate judges the first while this verifier checks all of them, so a second fabricated
// proof could ride behind an honest one.
func verifyE25AdminConsolePresence(m evidenceManifest) []Finding {
	family, claims, withProof := false, 0, 0
	for _, c := range m.Cases {
		if carriesE25AdminConsoleCase(c) {
			family = true
		}
		if c.AdminConsoleClaim != "" {
			claims++
			if c.AdminConsoleProof != nil {
				withProof++
			}
		}
	}
	if !family {
		return nil
	}
	switch {
	case claims == 0:
		return []Finding{{Kind: "missing", Detail: fmt.Sprintf(
			"admin_console_claim (this manifest carries E25 case(s) from %v, so it must carry the anchor that judges them — without it \"no write passes without a session\", \"every page is scanned\" and \"no written secret value comes back\" ship unverified behind seven green rows; plan §T9)",
			AdminConsoleCaseIDs)}}
	case claims > 1:
		return []Finding{{Kind: "invalid", Detail: fmt.Sprintf(
			"%d admin_console_claims in one manifest (want exactly 1): AdminConsolePromoteGate judges the FIRST admin-console proof, so a second could ride behind an honest one — one release, one re-derivation (plan §T9)", claims)}}
	case withProof != claims:
		return []Finding{{Kind: "missing", Detail: "admin_console_proof (the admin-console claim carries no proof body)"}}
	}
	return nil
}

// --- the canonical bytes the proof carries ---------------------------------------------------------------
//
// THE LEDGERS ARE AUTHORED, AND WHAT MAKES THEM HONEST IS THE CO-RUN RATHER THAN THIS FILE — the E24
// precedent, applied to a surface whose seams are mostly in TypeScript. Each row below records an outcome one
// of the SHIPPED suites produces: the console spec set on both colour schemes, the storage query sweep, the
// conformance sweep, and the component-real Go legs — all of which `scripts/uat/admin-console` runs in the
// SAME invocation that verifies this bundle. The gate's job is not to believe the rows: it is to RE-DERIVE
// every counter from them, and tests/uat/admin-console additionally re-derives the ROWS THEMSELVES from the
// tree (lib/routes.ts, the route handlers' own source, storage/queries) so a ledger edited to flatter the
// release fails twice.

// AdminConsoleRelayLedger is group (a): every exported HTTP method of every Route Handler under
// apps/web-console/app/api, beside the gate its body opens with. Produced by tests/relay-gate.spec.ts, which
// tokenizes each file with TypeScript's OWN scanner (a commented-out export and a string holding the gate's
// spelling are both invisible to it) and prints:
//
//	RELAY GATE AUDIT — 3 route file(s), 6 exported HTTP method(s), 5 gated
//
// The SIXTH is the login door, and it is carried here rather than filtered out so that the exception is
// COUNTABLE: the sweep refuses a second one, and refuses one that is not the login route.
const AdminConsoleRelayLedger = `[
  {"file": "apps/web-console/app/api/palai/v1/[...path]/route.ts", "method": "GET", "relay": true, "gate": "const refused = requireSession(request); if (refused !== null) return refused;"},
  {"file": "apps/web-console/app/api/palai/v1/[...path]/route.ts", "method": "POST", "relay": true, "gate": "const refused = requireSession(request); if (refused !== null) return refused;"},
  {"file": "apps/web-console/app/api/palai/v1/[...path]/route.ts", "method": "PATCH", "relay": true, "gate": "const refused = requireSession(request); if (refused !== null) return refused;"},
  {"file": "apps/web-console/app/api/palai/v1/[...path]/route.ts", "method": "DELETE", "relay": true, "gate": "const refused = requireSession(request); if (refused !== null) return refused;"},
  {"file": "apps/web-console/app/api/palai/stream/route.ts", "method": "POST", "relay": true, "gate": "const refused = requireSession(request); if (refused !== null) return refused;"},
  {"file": "apps/web-console/app/api/console/login/route.ts", "method": "POST", "relay": false, "gate": "is-the-door: verifyPassword + Origin, issues the session this gate checks"}
]`

// AdminConsoleRouteLedger is group (b): every row of apps/web-console/lib/routes.ts beside the colour schemes
// axe scanned it in. Produced by tests/a11y.spec.ts, which GENERATES one scan per row and prints:
//
//	AXE ROUTE COVERAGE — 10/10 declared route(s) scanned: /, /agents, /approvals, /capabilities,
//	  /environments, /history, /repositories, /runs, /tools, /usage
//
// once per Playwright project. `lead` is carried verbatim because the design pass made it REQUIRED — a page
// with no first sentence never says what it is, and eleven screens used to open into a panel under an <h1>
// that read "Palai Console" on all of them.
//
// /login IS NOT HERE, and that is a measurement rather than an omission: it sits OUTSIDE the session gate, so
// it is outside the generated loop that signs in first. It is scanned by its own leg in tests/auth.spec.ts
// ("the login page is axe-clean and operable with the keyboard alone") and measured by tests/contrast.spec.ts,
// which walks eleven routes — these ten plus /login.
const AdminConsoleRouteLedger = `[
  {"path": "/", "ready_test_id": "panel-organizations", "lead": "The configuration this deployment is actually running, read straight from the public API: the organizations and projects, the keys that reach them, the model wiring behind them, and the agents they run.", "axe_scanned_in": ["light", "dark"]},
  {"path": "/runs", "ready_test_id": "run-button", "lead": "Start a run and watch it happen \u2014 the canonical event stream as it arrives, the approval it parks on, and whatever it leaves behind.", "axe_scanned_in": ["light", "dark"]},
  {"path": "/environments", "ready_test_id": "panel-environments", "lead": "The named KEY=value sets an agent's shell commands run against. A value is written here and never read back.", "axe_scanned_in": ["light", "dark"]},
  {"path": "/approvals", "ready_test_id": "panel-approvals", "lead": "Gated tool calls waiting for an answer. Read the arguments, then decide: the decision binds to the request hash the row carries and to nothing else.", "axe_scanned_in": ["light", "dark"]},
  {"path": "/repositories", "ready_test_id": "panel-repository-bindings", "lead": "The repositories a coding run can attach a workspace to. Registering a binding checks nothing \u2014 the first thing that exercises one is a run.", "axe_scanned_in": ["light", "dark"]},
  {"path": "/agents", "ready_test_id": "panel-agent-profiles", "lead": "An agent is a name with a lineage of immutable revisions. A revision is drafted here and published once; publishing is what makes a run pinned to it reproducible, and it cannot be undone.", "axe_scanned_in": ["light", "dark"]},
  {"path": "/tools", "ready_test_id": "panel-mcp-connections", "lead": "What an upstream MCP server offers, and what this deployment lets a model actually call. Discovery leaves drafts; approving one is a publish, and pinning it into a set is what puts it in front of a model.", "axe_scanned_in": ["light", "dark"]},
  {"path": "/history", "ready_test_id": "panel-runs", "lead": "Every run this project has started and what each one wrote \u2014 the event journal replayed from the record rather than remembered, and the files it produced.", "axe_scanned_in": ["light", "dark"]},
  {"path": "/usage", "ready_test_id": "panel-usage-meters", "lead": "What has been spent in this scope and what caps it. This is a read surface: nothing on it sets, raises or removes a limit.", "axe_scanned_in": ["light", "dark"]},
  {"path": "/capabilities", "ready_test_id": "panel-capabilities", "lead": "What this deployment advertises to every client and at which tier \u2014 exactly as GET /v1/capabilities answers it, with no console-side opinion about what a tier ought to mean.", "axe_scanned_in": ["light", "dark"]}
]`

// AdminConsoleByteScanLedger is group (c): the three layers a written environment value was searched in, with
// the bytes each covered and the harmless token each one FOUND. Produced by tests/secret-never-returns.spec.ts:
//
//	SENTINEL SWEEP — dom=15159 probe=SWEEP_TOKEN found; response-body=564692 bodies=17 probe=SWEEP_TOKEN
//	  hits=2; source-map=3430410 maps=19 assets=43 probe=panel-environment-keys hits=1; sentinel found in 0
//
// The probes are E25 T9's addition and they are the difference between an absence and an unread haystack:
// arms 2 and 3 previously proved only that they had SOMETHING to scan. Each probe is a token that is ALLOWED
// to be there — the key NAME comes back from the API by design, which is exactly the distinction this screen
// is about.
//
// THE BYTE COUNTS ARE A MEASUREMENT OF ONE BUILD, NOT A PIN, and the difference is stated because a reader
// will otherwise assume the stronger thing. They are the LIGHT project's numbers from the run that produced
// this bundle; the dark project scans a slightly different DOM (14307 chars against 15163) because the
// rendered markup differs by scheme. What the exit gate DIFFS against a run is the ZERO and the three probe
// names — never these totals, which would turn a stylesheet edit into a red bundle and teach the next reader
// to regenerate rather than to read.
const AdminConsoleByteScanLedger = `[
  {"kind": "dom", "subject": "/environments after a create + a value write: outerHTML plus every input's live .value", "bytes_scanned": 15163, "sentinel_hits": 0, "probe": "SWEEP_TOKEN", "probe_found": true},
  {"kind": "response-body", "subject": "every response the browser retained during the whole journey (17 bodies)", "bytes_scanned": 564698, "sentinel_hits": 0, "probe": "SWEEP_TOKEN", "probe_found": true},
  {"kind": "source-map", "subject": "every file under .next/static, walked recursively (43 assets, 19 .js.map)", "bytes_scanned": 3430410, "sentinel_hits": 0, "probe": "panel-environment-keys", "probe_found": true}
]`

// AdminConsoleQueryLedger is group (d): storage/queries, file by file, with the named statements each holds
// and the ones whose BODY mentions the ciphertext column. Produced by the same parse
// storage/ciphertext_sweep_test.go runs on every `make verify`, which also proves it reads STATEMENTS and not
// prose by searching for a term that appears only in a comment and matching nothing.
const AdminConsoleQueryLedger = `[
  {"file": "storage/queries/a2a.sql", "statements": 12, "ciphertext_statements": []},
  {"file": "storage/queries/agents.sql", "statements": 16, "ciphertext_statements": []},
  {"file": "storage/queries/artifacts.sql", "statements": 7, "ciphertext_statements": []},
  {"file": "storage/queries/audit.sql", "statements": 2, "ciphertext_statements": []},
  {"file": "storage/queries/changesets.sql", "statements": 4, "ciphertext_statements": []},
  {"file": "storage/queries/commands.sql", "statements": 21, "ciphertext_statements": []},
  {"file": "storage/queries/config.sql", "statements": 3, "ciphertext_statements": []},
  {"file": "storage/queries/environments.sql", "statements": 8, "ciphertext_statements": []},
  {"file": "storage/queries/events.sql", "statements": 8, "ciphertext_statements": []},
  {"file": "storage/queries/hooks.sql", "statements": 5, "ciphertext_statements": []},
  {"file": "storage/queries/identity.sql", "statements": 1, "ciphertext_statements": []},
  {"file": "storage/queries/jobs.sql", "statements": 14, "ciphertext_statements": []},
  {"file": "storage/queries/knowledge.sql", "statements": 25, "ciphertext_statements": []},
  {"file": "storage/queries/mcp.sql", "statements": 8, "ciphertext_statements": []},
  {"file": "storage/queries/merge_records.sql", "statements": 1, "ciphertext_statements": []},
  {"file": "storage/queries/metrics.sql", "statements": 6, "ciphertext_statements": []},
  {"file": "storage/queries/model_routes.sql", "statements": 16, "ciphertext_statements": []},
  {"file": "storage/queries/provisioning.sql", "statements": 13, "ciphertext_statements": []},
  {"file": "storage/queries/publications.sql", "statements": 25, "ciphertext_statements": []},
  {"file": "storage/queries/queues.sql", "statements": 21, "ciphertext_statements": []},
  {"file": "storage/queries/recovery.sql", "statements": 4, "ciphertext_statements": []},
  {"file": "storage/queries/remote_tools.sql", "statements": 9, "ciphertext_statements": []},
  {"file": "storage/queries/repository_bindings.sql", "statements": 7, "ciphertext_statements": []},
  {"file": "storage/queries/responses.sql", "statements": 37, "ciphertext_statements": []},
  {"file": "storage/queries/runners.sql", "statements": 22, "ciphertext_statements": []},
  {"file": "storage/queries/schedules.sql", "statements": 15, "ciphertext_statements": []},
  {"file": "storage/queries/secrets.sql", "statements": 5, "ciphertext_statements": ["InsertSecretRef", "ResolveSecretRef"]},
  {"file": "storage/queries/sessions.sql", "statements": 7, "ciphertext_statements": []},
  {"file": "storage/queries/skills.sql", "statements": 8, "ciphertext_statements": []},
  {"file": "storage/queries/slack.sql", "statements": 26, "ciphertext_statements": []},
  {"file": "storage/queries/tasks.sql", "statements": 4, "ciphertext_statements": []},
  {"file": "storage/queries/tools.sql", "statements": 17, "ciphertext_statements": []},
  {"file": "storage/queries/triggers.sql", "statements": 42, "ciphertext_statements": []},
  {"file": "storage/queries/usage.sql", "statements": 9, "ciphertext_statements": []},
  {"file": "storage/queries/webhooks.sql", "statements": 15, "ciphertext_statements": []},
  {"file": "storage/queries/workers.sql", "statements": 11, "ciphertext_statements": []},
  {"file": "storage/queries/workspaces.sql", "statements": 18, "ciphertext_statements": []}
]`

// AdminConsoleSweepSubjects is group (e): the collections tests/conformance.test.mjs compared ITEM shapes for
// on its last real-profile run. It prints them rather than counting them, and that is E25 T6's correction —
// a bare number told nobody which collection had dropped out, and T6 spent a whole sweep run discovering by
// elimination that GET /v1/agents is not on this list and structurally cannot be (DIV-SHP-004).
var AdminConsoleSweepSubjects = []string{
	"GET /v1/organizations",
	"GET /v1/projects",
	"GET /v1/api-keys",
	"GET /v1/knowledge-bases",
	"GET /v1/environments",
	"GET /v1/secret-refs",
	"GET /v1/repository-bindings",
	"GET /v1/agents/{agent_id}/revisions",
	"GET /v1/tools",
	"GET /v1/tools/{tool_id}/revisions",
	"GET /v1/tool-sets",
	"GET /v1/responses",
	"GET /v1/usage/ledger",
}

// AdminConsoleDivergenceRepairLedger is group (f) — THIS RELEASE'S ADDED CLAIM. Five rows of
// tests/divergences.mjs that the console's evidence rested on were measured WRONG. Three are CLOSED and
// deleted; two are REPAIRED and narrowed to what is still true. Every one carries how it was RE-OBSERVED,
// because a repair made on a source read is the same move that put the false sentence there.
const AdminConsoleDivergenceRepairLedger = `[
  {"id": "DIV-RTE-001", "was": "\"the artifact/secret-ref routes are mounted CONDITIONALLY and deploy/compose/compose.yaml configures none\" \u2014 while compose.yaml:116 passes PALAI_SECRET_MASTER_KEY_FILE", "verdict": "closed", "reobserved": "OPTIONS /v1/secret-refs against a running four-service compose stack answered 405 with Allow: GET, HEAD, POST \u2014 which is exactly how the sweep's route arm reads a registered pattern (an unregistered one answers a bare 404). The row is DELETED, and deleting it on the strength of READING compose.yaml would have been the same move that put the false sentence there."},
  {"id": "DIV-RTE-002", "was": "\"the artifact content route is not mounted on a compose stack\" \u2014 while compose.yaml:77-78 pass PALAI_S3_ENDPOINT and PALAI_S3_BUCKET", "verdict": "closed", "reobserved": "OPTIONS /v1/artifacts/{id}/content answered 405 with Allow: GET, HEAD on the same running stack."},
  {"id": "DIV-RTE-003", "was": "\"the run-artifacts list route is not mounted on a compose stack\" \u2014 same conditional-mount claim", "verdict": "closed", "reobserved": "OPTIONS /v1/responses/{id}/artifacts answered 405 with Allow: GET, HEAD on the same running stack, and the staleness arm then named those three rows and nothing else."},
  {"id": "DIV-SHP-004", "was": "\"the real paginated envelope is {data, has_more}\" \u2014 as though the API minted no forward cursor at all, while api/pagination.go:226-227 GENUINELY mints next_cursor whenever has_more is true", "verdict": "repaired", "reobserved": "narrowed to the only structurally absent half (previous_cursor: beginList refuses ?before= with a 400 and renderPage never populates it), and the fixture's own share was then CLOSED by E25 T6 \u2014 pageSlice had served both cursor keys as explicit nulls, offering a previous_cursor the API will never mint. What remains is irreducible and was re-derived from a real run of the sweep: the fixture's agents collection must exceed one page for truncation to be provable at all, so its first page carries a next_cursor a short real list omits."},
  {"id": "DIV-SHP-005", "was": "the same explicit-null cursor keys on GET /v1/agents/{agent_id}/revisions \u2014 and an ENVELOPE difference SUBSUMES the item comparison, so the revision row's SHAPE was never once compared against the real projection", "verdict": "closed", "reobserved": "the fixture now serves {data, has_more} there and the sweep's item arm compares the row on every run, which immediately named three infidelities the row had hidden: published:true where the real projection says status:\"published\", a missing agent_id/revision_number/mcp_connections/environment/instructions set, and [] where a revision naming neither tools nor mcp_connections has null on the real wire."}
]`

// AdminConsoleRunbookLedger is group (g): docs/operations/jira-mcp-connection.md §3, call by call, executed
// against the shipped router over the public API alone by
// apps/control-plane/internal/execution/jira_runbook_component_test.go. Step (c2) is why T7 existed — the
// document told operators for three and a half days to publish a `$REV_ID` no /v1 response had ever carried,
// and the component test stopped there with a 405, not a 404: the tree served POST on the exact path it
// refused to serve GET on.
const AdminConsoleRunbookLedger = `[
  {"step": "(a) register the connection with a credential HANDLE", "call": "POST /v1/mcp-connections", "public_api": true},
  {"step": "(b) discover its tools \u2014 each becomes a DRAFT revision, nothing is advertised yet", "call": "POST /v1/mcp-connections/{id}/discover", "public_api": true},
  {"step": "(c) find the tool LINEAGE ids", "call": "GET /v1/tools", "public_api": true},
  {"step": "(c2) find the REVISION ids and read what is being approved \u2014 the description and the input schema", "call": "GET /v1/tools/{tool_id}/revisions", "public_api": true},
  {"step": "(d) approve a READ tool by publishing its revision, bodyless", "call": "POST /v1/tools/{tool_id}/revisions/{revision_id}/publish", "public_api": true},
  {"step": "(d') approve a WRITE tool on the same call, declaring the gate and its operator label", "call": "POST /v1/tools/{tool_id}/revisions/{revision_id}/publish", "public_api": true},
  {"step": "(e) pin the approved revisions into a tool set", "call": "POST /v1/tool-sets/{set}/revisions", "public_api": true},
  {"step": "(e') publish the set revision", "call": "POST /v1/tool-sets/{set}/revisions/{revision_id}/publish", "public_api": true},
  {"step": "(e2) read the set's PINS back \u2014 the only place the API says WHICH tool revisions a set grants", "call": "GET /v1/tool-sets/{set}/revisions/{revision_id}", "public_api": true},
  {"step": "(f) grant them to an agent revision \u2014 tool_sets is the GRANT, mcp_connections the CEILING, and both are required", "call": "POST /v1/agents/{agent_id}/revisions", "public_api": true}
]`

// AdminConsoleApprovalLedger is group (h): every decision tests/approval-queue.spec.ts drives from the screen,
// with whether the request hash the row displayed was still the row's own when the server checked it. Each
// row is printed by the spec itself as
//
//	APPROVAL DECISION — id=<id> decision=<approve|deny> hash_matched=<bool> applied=<bool> outcome=<...>
//
// and tests/uat/admin-console/journey_test.go diffs those lines against this table, so a row nobody produced
// fails the gate.
const AdminConsoleApprovalLedger = `[
  {"approval_id": "apvl_console_approve", "decision": "approve", "request_hash_matched": true, "applied": true, "outcome": "the run was released; the row left the queue, which is the whole of what \"it was answered\" looks like on this projection"},
  {"approval_id": "apvl_console_deny", "decision": "deny", "request_hash_matched": true, "applied": true, "outcome": "the operator's reason reached the API verbatim \u2014 a colon, a newline and a quote all survived, because that text is what the MODEL is handed"},
  {"approval_id": "apvl_console_drift", "decision": "approve", "request_hash_matched": false, "applied": false, "outcome": "409 no-longer-decidable: the fixture's drift row rotates its arguments and its hash on every serve, so the console's carried binding is always the previous one and the refusal comes from the one-shot binding genuinely failing"},
  {"approval_id": "apvl_console_drift", "decision": "approve", "request_hash_matched": false, "applied": false, "outcome": "409 again, reached through the typed-refusal sweep, where it must produce a DIFFERENT sentence from the 404 and the 403 beside it"},
  {"approval_id": "apvl_console_gone", "decision": "approve", "request_hash_matched": true, "applied": false, "outcome": "404 unknown-or-foreign, indistinguishable on purpose \u2014 the screen must not imply the approval exists somewhere else"},
  {"approval_id": "apvl_console_locked", "decision": "approve", "request_hash_matched": true, "applied": false, "outcome": "403 not_an_approver: the project's approver list, which is an INDEPENDENT gate from the key's approve capability and a different fix for the operator"}
]`
