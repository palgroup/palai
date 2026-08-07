package uat

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// FleetConsoleCaseIDs are the UAT ids E28 opens, and the `FLC-` prefix is a GATE decision rather than a naming
// preference — the CON-/BGT-/FLT-/HIL- reasoning, arrived at for the eighth time. `FLT-` (E24) and `CON-`
// (E25) are BOTH already inside extensionIDPrefixes and each has its own owner list and its own promote gate,
// so an `FLT-006` would match carriesE24FleetCase in PromoteGateFor and dispatch to FleetPromoteGate — a gate
// that knows nothing about this epic's pool-birth counter, its key-value scan, its policy-write equality, its
// alertdialog sweep or its ceiling ids, and would pass a bundle that dropped all five. That is the
// promote-gate-family-dispatch defect, reachable from a naming choice.
//
// AND ESCAPING THE SWEEP WAS OPEN IN THIS VERY EPIC, DEMONSTRATED RATHER THAN REASONED ABOUT — the third time
// on request. tests/uat/extensions is the ONLY place in the tree that walks the cases DIRECTORY. FLC-001,
// FLC-002 and FLC-003 shipped in T1, T2 and T3 while `FLC-` was in NO prefix list, so all three escaped proof
// resolution and `go test ./tests/uat/...` reported `ok` for every one of the twenty-two packages. Adding the
// prefix was shown RED first — three directories reported at once, before this list existed.
//
// FLC-004's DIRECTORY DID NOT EXIST UNTIL THIS GATE, and it is the direction a directory walk cannot find. T2
// and T3 shipped the alertdialog, the focus trap, the live lease read and the reversible/irreversible split
// across two pages and opened no case for the CROSS-PAGE claim: that EVERY irreversible action in this console
// is behind an alertdialog and every reversible one is not. A sweep that walks directories finds a dir nobody
// owns; only a canonical list finds an id with no dir. Two directions, two owners.
var FleetConsoleCaseIDs = []string{"FLC-001", "FLC-002", "FLC-003", "FLC-004"}

// ---------------------------------------------------------------------------------------------------------
// The E28 T4 EXIT-gate proof type (plan §T4) — the `fleet-console-0.1.0` sign-off.
//
// WHAT THIS BUNDLE CLAIMS, and every clause is narrower than the sentence a reader will want it to be:
//
//   - A FLEET HAS A BIRTH PATH. A pool can be created with a posture and with the waiting room switched on,
//     through the public API and from a screen — and before this epic none of the three could be done: there
//     was no `POST /v1/runner-pools`, no `UPDATE runner_pools` anywhere outside two test files, and the one
//     statement that wrote a pool row wrote 'default', 'sandboxed-linux' and false as LITERALS.
//   - A MACHINE WAITS WHERE SOMETHING CAN SEE IT. It enrols into a strict pool, lands `pending`, and is
//     ADMITTED FROM THE CONSOLE — which is the first time E24 T6's approve route has been driven from a
//     screen against a machine an operator's own pool produced.
//   - A SERVER-MINTED VALUE IS SHOWN ONCE AND SURVIVES NOWHERE, measured by scanning DECODED bytes in every
//     site it could land in rather than by declaring it.
//   - A POLICY FORM WRITES THE WHOLE DOCUMENT. `PATCH /v1/projects/{id}` is an ASSIGNMENT, so a request
//     naming one field erases the other four — and one of them is `approvers`, which `HIL-P11` measured to be
//     PERMISSIVE when empty. The counter is an EQUALITY rather than a zero: as many approver entries after
//     the write as before it.
//   - EVERY DECLARED ROUTE IS SCANNED, and the two new pages raise both sides of that equality together.
//   - AN IRREVERSIBLE ACTION GETS A DIFFERENT CONFIRMATION FROM A REVERSIBLE ONE, on a published criterion
//     (WCAG 2.2 SC 3.3.4's Reversible / Checked / Confirmed) rather than on taste — and the sweep is over
//     the SOURCE of every page, not over the actions a spec happened to name.
//   - THE SCREENS WRITE THEIR OWN CEILINGS, by gap id, and the largest of them says a Mac does not yet run
//     `xcodebuild`.
//
// AND THE HONEST CEILING, WHICH IS THIS BUNDLE'S MOST IMPORTANT SENTENCE. NO REAL MAC WAS RENTED, ENROLLED OR
// BUILT ANYTHING. Every machine in this release is a fake runner built from the shipped enrolment package,
// every browser leg drives Chromium against a deterministic fixture upstream or a compose stack on ONE box,
// and `FLT-P15` stands: a `lease.offer` carries the ENGINE, every TOOL still executes in the control plane's
// own process, so a Mac pool is creatable and still does not run `xcodebuild` on a Mac. The screens say so
// themselves, which is what (g) counts.
//
// NO TIER ADVANCES. `console` closes PREVIEW, and the reason specific to this epic is that with `FLT-P15`
// open a fleet screen IMPLIES MORE THAN IT PROVES — raising the tier would formalise the implication.
// ---------------------------------------------------------------------------------------------------------

// FleetConsoleBundle is the E28 EXIT bundle's release name.
const FleetConsoleBundle = "fleet-console-0.1.0"

// FleetConsolePeer is the ONE honest value a FleetConsoleProof may carry, and it is the literal "fake" for the
// admin-console / fleet / agent-surface reason: the counterparty in every browser leg is
// tests/fake-control-plane.mjs or a compose stack on this box, and the machines are fake runners. A bundle
// that cannot type any other value into this field cannot accidentally claim a rented Mac.
const FleetConsolePeer = "fake"

// --- (a) a pool's birth path -------------------------------------------------------------------------------

// fleetConsolePoolRow is one pool this release created: its posture, whether the waiting room was switched on
// with it, and the SURFACE it was created through.
//
// `CreatedVia` is carried rather than assumed because the whole point of T1 is that no surface could do this
// at all. A row claiming a pool exists without naming the public route that made it would be satisfied by the
// `InsertDefaultRunnerPool` seed, which writes its posture as a literal and is exactly what this epic was
// opened to get past.
type fleetConsolePoolRow struct {
	Pool       string `json:"pool"`
	Posture    string `json:"posture"`
	Strict     bool   `json:"strict_enrollment"`
	CreatedVia string `json:"created_via"`
	Test       string `json:"test"`
}

// FleetConsolePostures are the two values `runner_pools.posture` may hold (migration 000045's CHECK), and the
// SECOND is the one this release exists to make reachable: before E28 T1 no code path in the tree could write
// `unsandboxed-host`, so the column the rented Mac was added FOR had one attainable value.
var FleetConsolePostures = []string{"sandboxed-linux", "unsandboxed-host"}

// fleetConsoleCreationSurfaces are the surfaces a pool may be created through. `seed` is deliberately ABSENT:
// a pool that came from InsertDefaultRunnerPool proves nothing about a birth path, since that statement wrote
// its posture as a literal for the whole life of this repository before T1.
var fleetConsoleCreationSurfaces = []string{"public-api", "console", "cli"}

// SweepFleetConsolePools decodes the pool ledger and returns the pools created, the SORTED set of postures
// they were created with, and the creation surfaces covered — group (a), RE-DERIVED from the carried rows
// rather than read off a declared count.
func SweepFleetConsolePools(ledger json.RawMessage) (created int, postures, surfaces []string, err error) {
	if len(ledger) == 0 {
		return 0, nil, nil, fmt.Errorf("no pool ledger to sweep: \"a fleet has a birth path\" over no pools is vacuous")
	}
	var rows []fleetConsolePoolRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return 0, nil, nil, fmt.Errorf("the carried pool ledger is not JSON, so the birth-path count is unverifiable: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil, nil, fmt.Errorf("the pool ledger is EMPTY: a release that created no pool cannot certify that one can be created")
	}
	seen := map[string]bool{}
	seenPosture := map[string]bool{}
	seenSurface := map[string]bool{}
	for _, row := range rows {
		if strings.TrimSpace(row.Pool) == "" || row.Test == "" {
			return 0, nil, nil, fmt.Errorf("a pool row names no pool or no test: %+v", row)
		}
		if seen[row.Pool] {
			return 0, nil, nil, fmt.Errorf("pool %q appears twice — a duplicated row inflates the created count without creating a pool", row.Pool)
		}
		seen[row.Pool] = true
		if !slices.Contains(FleetConsolePostures, row.Posture) {
			return 0, nil, nil, fmt.Errorf("pool %q declares posture %q, want one of %v — migration 000045's CHECK names exactly these two, and a third would be a value the database refuses", row.Pool, row.Posture, FleetConsolePostures)
		}
		if !slices.Contains(fleetConsoleCreationSurfaces, row.CreatedVia) {
			return 0, nil, nil, fmt.Errorf("pool %q declares created_via %q, want one of %v — `seed` is not on that list on purpose: InsertDefaultRunnerPool wrote its posture as a LITERAL, so a seeded pool is evidence of the hole rather than of the fix", row.Pool, row.CreatedVia, fleetConsoleCreationSurfaces)
		}
		if !seenPosture[row.Posture] {
			seenPosture[row.Posture] = true
			postures = append(postures, row.Posture)
		}
		if !seenSurface[row.CreatedVia] {
			seenSurface[row.CreatedVia] = true
			surfaces = append(surfaces, row.CreatedVia)
		}
		created++
	}
	slices.Sort(postures)
	slices.Sort(surfaces)
	return created, postures, surfaces, nil
}

// --- (b) the waiting room ----------------------------------------------------------------------------------

// fleetConsoleWaitingRow is one machine that enrolled into a STRICT pool: whether it actually reached
// `pending`, and the surface it was admitted from.
//
// `AdmittedFrom` is the field that makes this group about E28 rather than about E24. E24 T6 shipped the
// approve route and proved it from a component test; what had never happened is an operator ADMITTING a
// machine from a screen, into a pool that same operator's console created.
type fleetConsoleWaitingRow struct {
	Machine        string `json:"machine"`
	Pool           string `json:"pool"`
	ReachedPending bool   `json:"reached_pending"`
	AdmittedFrom   string `json:"admitted_from"`
	Test           string `json:"test"`
}

// fleetConsoleAdmissionSurfaces are the surfaces an admission may come from. The gate requires at least one
// `console` row: an epic whose crown claim is a screen cannot certify that claim from a CLI transcript.
var fleetConsoleAdmissionSurfaces = []string{"console", "public-api", "cli"}

// SweepFleetConsoleWaitingRoom decodes the waiting-room ledger and returns the machines that reached
// `pending`, the number admitted FROM THE CONSOLE, and any machine that never actually waited — group (b).
func SweepFleetConsoleWaitingRoom(ledger json.RawMessage) (pending, admittedFromTheConsole int, neverWaited []string, err error) {
	if len(ledger) == 0 {
		return 0, 0, nil, fmt.Errorf("no waiting-room ledger to sweep: \"a machine waits where something can see it\" over no machines is vacuous")
	}
	var rows []fleetConsoleWaitingRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return 0, 0, nil, fmt.Errorf("the carried waiting-room ledger is not JSON, so the admission count is unverifiable: %w", err)
	}
	if len(rows) == 0 {
		return 0, 0, nil, fmt.Errorf("the waiting-room ledger is EMPTY: a release that admitted no machine cannot certify that the waiting room is reachable")
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if strings.TrimSpace(row.Machine) == "" || strings.TrimSpace(row.Pool) == "" || row.Test == "" {
			return 0, 0, nil, fmt.Errorf("a waiting-room row names no machine, no pool or no test: %+v", row)
		}
		if seen[row.Machine] {
			return 0, 0, nil, fmt.Errorf("machine %q appears twice — a duplicated row inflates the admission count without admitting a machine", row.Machine)
		}
		seen[row.Machine] = true
		if !slices.Contains(fleetConsoleAdmissionSurfaces, row.AdmittedFrom) {
			return 0, 0, nil, fmt.Errorf("machine %q declares admitted_from %q, want one of %v", row.Machine, row.AdmittedFrom, fleetConsoleAdmissionSurfaces)
		}
		if !row.ReachedPending {
			neverWaited = append(neverWaited, row.Machine)
			continue
		}
		pending++
		if row.AdmittedFrom == "console" {
			admittedFromTheConsole++
		}
	}
	return pending, admittedFromTheConsole, neverWaited, nil
}

// --- (c) the key value, and where it did not land ----------------------------------------------------------

// fleetConsoleKeyScanRow is one site a server-minted key VALUE was searched in. `Probe`/`ProbeFound` are the
// site's own NON-VACUITY control: a harmless token the same scan DID find in the same bytes.
//
// THE PROBE IS WHY THE ZERO MEANS ANYTHING, and this tree has paid for its absence twice — a raw-byte sweep
// over compressed output in E14 T7 and a JSON-escaped block assertion in E20 T4, both of which could never
// fail. `Decoded` is carried for the same reason and refused when false: a scan of a response body that was
// never decoded is a scan of a transport encoding.
type fleetConsoleKeyScanRow struct {
	Site       string `json:"site"`
	Subject    string `json:"subject"`
	Bytes      int    `json:"bytes_scanned"`
	Decoded    bool   `json:"decoded_before_scanning"`
	Hits       int    `json:"key_value_hits"`
	Probe      string `json:"probe"`
	ProbeFound bool   `json:"probe_found"`
}

// FleetConsoleKeyScanSites are the five places a minted value could survive, and all five must be present. The
// last two are the ones a tidy build would drop: a value can be gone from the DOM and still be in the URL a
// reader bookmarks, and it can be absent from the first response and served back by a LATER list call — which
// is the server behaviour `poolKeyView(item, false)` exists to prevent and which the browser half must mirror.
var FleetConsoleKeyScanSites = []string{"response-body", "dom", "web-storage", "url", "later-response"}

// SweepFleetConsoleKeyScan decodes the key-scan ledger and returns the value hits, the sites whose probe was
// FOUND, and the sites covered — group (c).
func SweepFleetConsoleKeyScan(ledger json.RawMessage) (hits, probed int, sites []string, err error) {
	if len(ledger) == 0 {
		return 0, 0, nil, fmt.Errorf("no key-scan ledger to sweep: \"a minted value survives nowhere\" over no sites is vacuous")
	}
	var rows []fleetConsoleKeyScanRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return 0, 0, nil, fmt.Errorf("the carried key-scan ledger is not JSON, so the leak count is unverifiable: %w", err)
	}
	bySite := map[string]bool{}
	for _, row := range rows {
		if !slices.Contains(FleetConsoleKeyScanSites, row.Site) {
			return 0, 0, nil, fmt.Errorf("key-scan row %q declares site %q, which is not one of %v", row.Subject, row.Site, FleetConsoleKeyScanSites)
		}
		if bySite[row.Site] {
			return 0, 0, nil, fmt.Errorf("two rows for site %q — one row per site, or a duplicated probe stands in for a missing one", row.Site)
		}
		if row.Bytes <= 0 {
			return 0, 0, nil, fmt.Errorf("key-scan row %q scanned %d bytes — a scan over nothing reports zero hits and proves nothing", row.Subject, row.Bytes)
		}
		if !row.Decoded {
			return 0, 0, nil, fmt.Errorf("key-scan site %q was scanned WITHOUT decoding first — a raw-byte sweep over encoded output can never fail, which this tree measured in E14 T7 and paid for again in E20 T4", row.Site)
		}
		if strings.TrimSpace(row.Probe) == "" || !row.ProbeFound {
			return 0, 0, nil, fmt.Errorf("key-scan site %q names no probe it actually found — a haystack nothing was ever located in is a haystack nobody has shown was read", row.Site)
		}
		if strings.Contains(strings.ToUpper(row.Probe), "SENTINEL") || strings.Contains(strings.ToUpper(row.Probe), "RPK_") {
			return 0, 0, nil, fmt.Errorf("key-scan site %q uses the minted value's own shape as its probe — the control has to be a token that is ALLOWED to be there, or finding it is the failure", row.Site)
		}
		bySite[row.Site] = true
		probed++
		hits += row.Hits
	}
	for _, site := range FleetConsoleKeyScanSites {
		if !bySite[site] {
			return 0, 0, nil, fmt.Errorf("no row for the %q site — without it, that site's zero is indistinguishable from a site that never ran, and `later-response` is exactly the site the other four pass over", site)
		}
		sites = append(sites, site)
	}
	return hits, probed, sites, nil
}

// --- (d) the policy document, written whole ---------------------------------------------------------------

// fleetConsolePolicyRow is one write through the console's policy form: which field the operator changed, how
// many approver entries the project held BEFORE and AFTER, and how many fields the REQUEST carried.
//
// BOTH HALVES ARE LOAD-BEARING AND THE SECOND IS THE ONE A SERVER COULD FAKE. Asserting only the stored
// outcome would pass on a server that MERGED, over a form still sending one field — the exact build this epic
// exists to refuse. So the request's field count is carried beside the outcome.
type fleetConsolePolicyRow struct {
	FieldChanged    string `json:"field_changed"`
	ApproversBefore int    `json:"approvers_before"`
	ApproversAfter  int    `json:"approvers_after"`
	FieldsInRequest int    `json:"fields_in_request"`
	Test            string `json:"test"`
}

// FleetConsolePolicyFields is `configPolicyInput`'s full set (identity/store.go): a write naming fewer than
// five fields erases the ones it omitted, because UpdateProjectPolicy marshals the struct and hands the bytes
// over with no merge.
const FleetConsolePolicyFields = 5

// SweepFleetConsolePolicyWrites decodes the policy ledger and returns the approver entries before and after
// the writes and any write that shrank the list or sent a partial document — group (d).
func SweepFleetConsolePolicyWrites(ledger json.RawMessage) (before, after int, partial []string, err error) {
	if len(ledger) == 0 {
		return 0, 0, nil, fmt.Errorf("no policy ledger to sweep: \"a policy form writes the whole document\" over no writes is vacuous")
	}
	var rows []fleetConsolePolicyRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return 0, 0, nil, fmt.Errorf("the carried policy ledger is not JSON, so the approver equality is unverifiable: %w", err)
	}
	if len(rows) == 0 {
		return 0, 0, nil, fmt.Errorf("the policy ledger is EMPTY: a release that wrote no policy cannot certify that a write preserves what it did not touch")
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if strings.TrimSpace(row.FieldChanged) == "" || row.Test == "" {
			return 0, 0, nil, fmt.Errorf("a policy row names no changed field or no test: %+v", row)
		}
		if seen[row.FieldChanged] {
			return 0, 0, nil, fmt.Errorf("policy field %q appears twice — a duplicated row inflates the surviving count without performing a write", row.FieldChanged)
		}
		seen[row.FieldChanged] = true
		if row.ApproversBefore <= 0 {
			return 0, 0, nil, fmt.Errorf("policy write %q started from %d approver entries — a write measured against an ALREADY EMPTY list cannot show that anything survived, which is this group's whole content", row.FieldChanged, row.ApproversBefore)
		}
		if row.FieldsInRequest != FleetConsolePolicyFields {
			partial = append(partial, row.FieldChanged)
		}
		before += row.ApproversBefore
		after += row.ApproversAfter
	}
	return before, after, partial, nil
}

// --- (e) the routes, and the axe scan -----------------------------------------------------------------------

// fleetConsoleRouteRow is one row of apps/web-console/lib/routes.ts beside the colour schemes axe scanned it
// in. It mirrors the E25 shape deliberately: this epic ADDS two rows to that file and the equality it has to
// keep is the same one, so a second definition of "declared" would be a second thing to keep in step.
type fleetConsoleRouteRow struct {
	Path         string   `json:"path"`
	ReadyTestID  string   `json:"ready_test_id"`
	AxeScannedIn []string `json:"axe_scanned_in"`
}

// FleetConsoleNewRoutes are the two pages this epic opened. They are named rather than counted because the
// interesting assertion is not "the number rose" but "these two are among the scanned".
var FleetConsoleNewRoutes = []string{"/fleet", "/policy"}

// SweepFleetConsoleRoutes decodes the route ledger and returns the declared routes, the routes scanned in
// EVERY colour scheme, and the ones that were not — group (e).
func SweepFleetConsoleRoutes(ledger json.RawMessage) (declared, scannedInEveryScheme int, unscanned []string, err error) {
	if len(ledger) == 0 {
		return 0, 0, nil, fmt.Errorf("no route ledger to sweep: \"every page is scanned\" over no pages is vacuous")
	}
	var rows []fleetConsoleRouteRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return 0, 0, nil, fmt.Errorf("the carried route ledger is not JSON, so the axe coverage count is unverifiable: %w", err)
	}
	if len(rows) == 0 {
		return 0, 0, nil, fmt.Errorf("the route ledger is EMPTY: a console declaring no route cannot certify that every route was scanned")
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Path == "" || row.ReadyTestID == "" {
			return 0, 0, nil, fmt.Errorf("route %q carries no path or no readiness signal — axe on a page still showing a spinner reports a clean bill of health for markup it never saw", row.Path)
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
	for _, want := range FleetConsoleNewRoutes {
		if !seen[want] {
			return 0, 0, nil, fmt.Errorf("the route ledger does not carry %q — this epic OPENED that page, and a ledger that omits it certifies the console it replaced", want)
		}
	}
	return declared, scannedInEveryScheme, unscanned, nil
}

// --- (f) the confirmation an irreversible action gets -------------------------------------------------------

// fleetConsoleActionRow is one destructive action on a console screen: whether the server can undo it, and
// which confirmation it goes through.
//
// THE REVERSIBLE ROWS ARE NOT PADDING. WCAG 2.2 SC 3.3.4 offers THREE legs and asks for one of them; a
// reversible action satisfies leg 1 (Reversible) and owes no dialog, so a build that put every action behind
// an alertdialog would not be more compliant, it would be a build that stopped distinguishing. The ledger
// therefore refuses BOTH directions: an irreversible action outside an alertdialog, and a reversible one
// inside it.
type fleetConsoleActionRow struct {
	Action       string `json:"action"`
	Reversible   bool   `json:"reversible"`
	Confirmation string `json:"confirmation"`
	SourceFile   string `json:"source_file"`
	// ReviewsLiveState is the "reviewing" word in SC 3.3.4 leg 3, which is a DATA CALL rather than a text:
	// the runner revoke dialog cannot open without GET /v1/runners/{id}, because ActiveLeases exists only on
	// the single read. It is required on the irreversible rows that name a live counterpart and stated as
	// false where there is none, so a later reader can see which is which.
	ReviewsLiveState bool   `json:"reviews_live_state"`
	Test             string `json:"test"`
}

// FleetConsoleConfirmations are the two confirmations this console has, and the split between them is the
// published criterion rather than taste: `window.confirm` is keyboard-operable, screen-reader-announced and
// focus-trapped by the browser for free, and a hand-rolled dialog has to re-earn all three.
const (
	FleetConsoleAlertDialog   = "alertdialog"
	FleetConsoleNativeConfirm = "window.confirm"
)

// SweepFleetConsoleActions decodes the action ledger and returns the irreversible actions behind an
// alertdialog, the reversible ones left on the native confirmation, and any row on the wrong side — group (f).
func SweepFleetConsoleActions(ledger json.RawMessage) (irreversibleGuarded, reversibleNative int, misplaced []string, err error) {
	if len(ledger) == 0 {
		return 0, 0, nil, fmt.Errorf("no action ledger to sweep: \"an irreversible action gets a different confirmation\" over no actions is vacuous")
	}
	var rows []fleetConsoleActionRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return 0, 0, nil, fmt.Errorf("the carried action ledger is not JSON, so the confirmation split is unverifiable: %w", err)
	}
	seen := map[string]bool{}
	sawIrreversible, sawReversible := false, false
	for _, row := range rows {
		if strings.TrimSpace(row.Action) == "" || row.SourceFile == "" || row.Test == "" {
			return 0, 0, nil, fmt.Errorf("an action row names no action, no source file or no test: %+v", row)
		}
		if seen[row.Action] {
			return 0, 0, nil, fmt.Errorf("action %q appears twice — a duplicated row inflates the guarded count without guarding an action", row.Action)
		}
		seen[row.Action] = true
		switch row.Confirmation {
		case FleetConsoleAlertDialog, FleetConsoleNativeConfirm:
		default:
			return 0, 0, nil, fmt.Errorf("action %q declares confirmation %q, want %q or %q — a third value would be a confirmation this console does not have", row.Action, row.Confirmation, FleetConsoleAlertDialog, FleetConsoleNativeConfirm)
		}
		if row.Reversible {
			sawReversible = true
			if row.Confirmation != FleetConsoleNativeConfirm {
				misplaced = append(misplaced, row.Action+" (reversible, behind an "+row.Confirmation+")")
				continue
			}
			reversibleNative++
			continue
		}
		sawIrreversible = true
		if row.Confirmation != FleetConsoleAlertDialog {
			misplaced = append(misplaced, row.Action+" (IRREVERSIBLE, only "+row.Confirmation+")")
			continue
		}
		irreversibleGuarded++
	}
	if !sawIrreversible || !sawReversible {
		return 0, 0, nil, fmt.Errorf("the action ledger carries only one side of the split (irreversible rows: %t, reversible rows: %t) — the claim is a DIFFERENCE, and a ledger with one kind in it cannot show one", sawIrreversible, sawReversible)
	}
	return irreversibleGuarded, reversibleNative, misplaced, nil
}

// --- (g) the ceilings the screens write themselves ---------------------------------------------------------

// fleetConsoleCeilingRow is one ceiling a page states, by GAP ID: which page, which test id renders it, and
// which spec asserts it.
//
// THE ID IS THE FIELD THAT MAKES THIS COUNTABLE. "The page warns about remote execution" is a sentence a
// reviewer judges; "the page renders `FLT-P15`" is a string a source scan finds and a deletion reddens.
type fleetConsoleCeilingRow struct {
	GapID      string `json:"gap_id"`
	Page       string `json:"page"`
	TestID     string `json:"test_id"`
	SourceFile string `json:"source_file"`
	Test       string `json:"test"`
}

// FleetConsoleCeilings are the four gap rows this epic's two pages must state, and the FIRST is the one
// known-gaps itself says to read before any other `FLT-P*` because it bounds what all of them are worth.
var FleetConsoleCeilings = []string{"CON-P2", "FLT-P12", "FLT-P15", "HIL-P11"}

// SweepFleetConsoleCeilings decodes the ceiling ledger and returns the SORTED gap ids found on screen and any
// ceiling this epic owes that no page states — group (g).
func SweepFleetConsoleCeilings(ledger json.RawMessage) (onScreen []string, missing []string, err error) {
	if len(ledger) == 0 {
		return nil, nil, fmt.Errorf("no ceiling ledger to sweep: \"a screen says what it does not show\" over no screens is vacuous")
	}
	var rows []fleetConsoleCeilingRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return nil, nil, fmt.Errorf("the carried ceiling ledger is not JSON, so the ceiling count is unverifiable: %w", err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.GapID == "" || row.Page == "" || row.TestID == "" || row.SourceFile == "" || row.Test == "" {
			return nil, nil, fmt.Errorf("a ceiling row is incomplete: %+v — a ceiling with no page, no test id or no spec is a sentence somebody can delete", row)
		}
		if seen[row.GapID] {
			return nil, nil, fmt.Errorf("ceiling %q appears twice — a duplicated row stands in for a ceiling no page states", row.GapID)
		}
		if !slices.Contains(FleetConsoleCeilings, row.GapID) {
			return nil, nil, fmt.Errorf("ceiling row names %q, which is not one of %v — a ceiling this epic did not owe is not evidence that it paid the ones it did", row.GapID, FleetConsoleCeilings)
		}
		seen[row.GapID] = true
		onScreen = append(onScreen, row.GapID)
	}
	for _, want := range FleetConsoleCeilings {
		if !seen[want] {
			missing = append(missing, want)
		}
	}
	slices.Sort(onScreen)
	return onScreen, missing, nil
}

// --- (h) the conformance sweep's floor ----------------------------------------------------------------------

// FleetConsoleSweepFloorBeforeE28 is what tests/conformance.test.mjs asserted before this epic — and plan
// §T4 (h) is where a number had to be re-measured rather than carried. The floor's history is written into
// that file's own comment: 3 (the collections a bootstrap seeds), then 4, then 6, then 8, then 11
// (aspirational and never actually reached), then 13 at E25 T8. THIRTEEN is the pre-E28 value; E28 T3 raised
// the assertion to sixteen, so reading "16" off today's tree and calling it the baseline would have compared
// this epic against itself.
//
// AND T3'S OWN MEASUREMENT CORRECTED TWO GUESSES, which is why the ledger carries SUBJECTS and not a count
// (E25 T6's rule): `runner-pools` had been comparable all along because every tenant is seeded a pool at
// birth, so the thirteen the earlier comment enumerated was stale by one; and a first draft of DIV-UI-009
// reasoned that a compose stack enrols no machines, while deploy/compose starts a runner SERVICE and
// `palai local up` mints it a token — the real side answers one. A bare number could not have said either.
const FleetConsoleSweepFloorBeforeE28 = 13

// SweepFleetConsoleConformance validates the compared-collection ledger and returns the collections compared —
// group (h). The subjects are carried rather than a bare number for the E25 T6 reason: a count told nobody
// WHICH collections were compared, so a sweep that lost one and gained another looked identical.
func SweepFleetConsoleConformance(ledger json.RawMessage) (compared []string, err error) {
	if len(ledger) == 0 {
		return nil, fmt.Errorf("no conformance ledger to sweep: \"the fake mirrors the real one\" over no collections is vacuous")
	}
	var subjects []string
	if err := json.Unmarshal(ledger, &subjects); err != nil {
		return nil, fmt.Errorf("the carried conformance ledger is not JSON, so the compared count is unverifiable: %w", err)
	}
	seen := map[string]bool{}
	for _, s := range subjects {
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("the conformance ledger carries a blank subject — a blank name inflates the count without comparing a collection")
		}
		if seen[s] {
			return nil, fmt.Errorf("conformance subject %q appears twice — a duplicated subject inflates the count without comparing a second collection", s)
		}
		seen[s] = true
		compared = append(compared, s)
	}
	if len(compared) <= FleetConsoleSweepFloorBeforeE28 {
		return nil, fmt.Errorf("the conformance ledger compares %d collection(s), which is not ABOVE the pre-E28 floor of %d — this epic added a fleet surface to both sides of the sweep, so a floor that did not move means the new surface is not in it", len(compared), FleetConsoleSweepFloorBeforeE28)
	}
	return compared, nil
}

// --- the canonical §3.5 contract ledger ---------------------------------------------------------------------

// FleetConsoleContracts is the CANONICAL ledger of the published contracts E28's design rests on, and it is
// the bundle's checksum ANCHOR: a dropped or reworded row moves every checksum in the release.
//
// W5 IS PRESENT AS AN ABSENCE AND THAT IS THE POINT. A vendor silence is not a design freedom — this tree's
// fifth statement of the same rule — so the row that could NOT be confirmed is carried here saying so, and
// the code took the cheapest position available (do not change the behaviour) rather than inventing one.
var FleetConsoleContracts = []ContractRequirement{
	{
		Divergence: "W1",
		SourceURL:  "https://www.w3.org/WAI/WCAG22/Understanding/error-prevention-legal-financial-data.html (fetched 2026-07-31)",
		Requirement: "⭐ WHY A REVOKE'S CONFIRMATION DIFFERS FROM A CORDON'S IS A PUBLISHED CRITERION RATHER THAN " +
			"TASTE. SC 3.3.4 asks for ONE of three legs — \"1. Reversible … 2. Checked … 3. Confirmed — A " +
			"mechanism is available for reviewing, confirming, and correcting information before finalizing the " +
			"submission\" — for pages that \"modify or delete user-controllable data in data storage systems\". " +
			"Cordon/resume satisfies leg 1 verbatim (Resume clears it, runner_gateway.go:324) so a window.confirm " +
			"is enough and more would be noise; revoke CANNOT (runner_gateway.go:328, \"a revoked runner identity " +
			"is decommissioned, not paused\"), leg 2 is meaningless with no entered data, and leg 3's word " +
			"REVIEWING is a data call: the dialog shows the machine's id, its label and its ActiveLeases, which " +
			"exists only on the SINGLE read (api/runners.go:48-59), so the dialog cannot open without calling " +
			"GET /v1/runners/{id}",
	},
	{
		Divergence: "W2",
		SourceURL:  "https://www.w3.org/WAI/ARIA/apg/patterns/alertdialog/ (fetched 2026-07-31)",
		Requirement: "THE COST OF LEAVING window.confirm IS NAMED BY THE PATTERN ITSELF. APG requires role " +
			"`alertdialog`, `aria-modal=\"true\"`, a label via `aria-labelledby` or `aria-label`, an " +
			"`aria-describedby` pointing at the alert message, and the modal dialog pattern's keyboard " +
			"interaction — every property the browser gives window.confirm for free and a hand-rolled dialog " +
			"has to re-earn. So the threshold environments/page.tsx's own ponytail note recorded (\"more than one " +
			"sentence and a yes/no\") is crossed ONLY by the irreversible actions, and the reversible ones keep " +
			"the native confirmation rather than being converted for consistency",
	},
	{
		Divergence: "W3",
		SourceURL:  "https://developer.mozilla.org/en-US/docs/Web/API/Clipboard/writeText (fetched 2026-07-31)",
		Requirement: "A COPY BUTTON NEEDS A CARRYING FALLBACK RATHER THAN A COURTESY ONE: \"Secure context: This " +
			"feature is available only in secure contexts (HTTPS)\" and \"Writing to the clipboard can only be " +
			"done in a secure context\", with `NotAllowedError` \"thrown if writing to the clipboard is not " +
			"allowed\". An operator reaching http://<host>:3000 is a SUPPORTED posture in this tree (compose edge " +
			"TLS lives in the production overlay, not the base profile), so the value always renders in a " +
			"selectable <code> node, the copy button is ABSENT rather than broken where the API is missing, and " +
			"a refused write is SHOWN rather than swallowed",
	},
	{
		Divergence: "W4",
		SourceURL:  "https://www.w3.org/WAI/WCAG22/Understanding/accessible-authentication-minimum.html (fetched 2026-07-31)",
		Requirement: "AN EXTENSION, MARKED AS ONE RATHER THAN CLAIMED AS COMPLIANCE. SC 3.3.8's understanding text " +
			"says \"Copy and paste can be relied on to avoid transcription\" and that blocking paste \"would force " +
			"the user to transcribe information and therefore fail this criterion\" — about AUTHENTICATION fields, " +
			"which a pool key display is not. E25 T4 applied it on the INPUT side (SecretField's onPaste ban); " +
			"E28 carries the RATIONALE, not the normative text, to the OUTPUT side. RevealOnce claims NO " +
			"conformance with 3.3.8; what it does is remove the same cognitive cost for the same reason, so there " +
			"is no `user-select: none`, no copy-blocking handler, and a test counts that",
	},
	{
		Divergence: "W5",
		SourceURL:  "UNCONFIRMED — searched the ARIA APG alertdialog pattern and the modal dialog pattern's keyboard section (2026-07-31); no normative sentence about which button receives initial focus was found",
		Requirement: "⭐ A VENDOR SILENCE IS NOT A DESIGN FREEDOM, FOR THE FIFTH TIME IN THIS TREE (SLK-P3, TLM-P4, " +
			"HIL-P9, N19, and now this). APG describes a CHOICE of \"the element that will receive focus when the " +
			"dialog opens\" and no published criterion makes \"the least destructive action\" normative. So this " +
			"entered the code as NOTHING: the cancel button takes focus because window.confirm already behaves " +
			"that way and not changing a behaviour is the cheapest way to make no claim, and no test, document or " +
			"bundle says it is an accessibility requirement. Filed as FLC-P5",
	},
}

// fleetConsoleContractParts flattens the canonical ledger into hashParts input, so the digest is re-derivable
// from the CODE table alone and a bundle cannot present a self-consistent digest over an edited ledger.
func fleetConsoleContractParts() []string {
	parts := make([]string, 0, 3*len(FleetConsoleContracts))
	for _, req := range FleetConsoleContracts {
		parts = append(parts, req.Divergence, req.SourceURL, req.Requirement)
	}
	return parts
}

// FleetConsoleContractsDigest is hashParts over the CANONICAL contract ledger — the E28 bundle's checksum
// anchor.
func FleetConsoleContractsDigest() string { return hashParts(fleetConsoleContractParts()...) }

// --- the proof ---------------------------------------------------------------------------------------------

// FleetConsoleProof is the evidence a fleet_console_claim requires (plan §T4 — the E28 EXIT anchor). Its
// groups are the plan's (a)..(h) plus the contract ledger, and EVERY counter is RE-DERIVED from the carried
// bytes: not one of them is trusted as declared.
//
//	(a) PoolLedger / PoolsCreated / PosturesCreated — created through a PUBLIC surface, and the posture set
//	    MUST contain `unsandboxed-host`, which no code path in this tree could write before T1;
//	(b) WaitingRoomLedger / MachinesPending / MachinesAdmittedFromTheConsole — the second MUST be ≥ 1, or an
//	    epic whose crown claim is a screen certified it from a CLI transcript;
//	(c) KeyScanLedger / KeyValuesFound — MUST BE ZERO, over five sites each DECODED before it was scanned and
//	    each naming a harmless token the same scan DID find;
//	(d) PolicyLedger / ApproverEntriesBefore / ApproverEntriesAfter — an EQUALITY rather than a zero, over
//	    writes that each carried all five policy fields;
//	(e) RouteLedger / RoutesDeclared / RoutesAxeScanned — EQUAL, in both colour schemes, and carrying the two
//	    pages this epic opened;
//	(f) ActionLedger / IrreversibleActionsBehindAnAlertDialog / ReversibleActionsOnTheNativeConfirm — refused
//	    in BOTH directions, because the claim is a DIFFERENCE;
//	(g) CeilingLedger / CeilingsOnScreen — all four gap ids, by id;
//	(h) ConformanceLedger / ConformanceCollectionsCompared — ABOVE the pre-E28 floor.
//
// HONEST CEILING, MECHANICALLY ENFORCED: Peer must be the literal "fake". No Mac was rented.
type FleetConsoleProof struct {
	Peer string `json:"peer"`

	// (a) A pool can be born, with a posture, through a public surface.
	PoolLedger      json.RawMessage `json:"pool_ledger"`
	PoolsCreated    int             `json:"pools_created"`
	PosturesCreated []string        `json:"postures_created"`

	// (b) A machine waits in a strict pool and is admitted from a screen.
	WaitingRoomLedger              json.RawMessage `json:"waiting_room_ledger"`
	MachinesPending                int             `json:"machines_pending"`
	MachinesAdmittedFromTheConsole int             `json:"machines_admitted_from_the_console"`

	// (c) The minted value lands nowhere. Zero, over decoded bytes, with a probe in each site.
	KeyScanLedger  json.RawMessage `json:"key_scan_ledger"`
	KeyValuesFound int             `json:"key_values_found"`

	// (d) The policy write preserves what it did not touch. An equality, not a zero.
	PolicyLedger          json.RawMessage `json:"policy_ledger"`
	ApproverEntriesBefore int             `json:"approver_entries_before"`
	ApproverEntriesAfter  int             `json:"approver_entries_after"`

	// (e) Every declared route is scanned, in every colour scheme.
	RouteLedger      json.RawMessage `json:"route_ledger"`
	RoutesDeclared   int             `json:"routes_declared"`
	RoutesAxeScanned int             `json:"routes_axe_scanned"`

	// (f) The irreversible actions are behind an alertdialog and the reversible ones are not.
	ActionLedger                           json.RawMessage `json:"action_ledger"`
	IrreversibleActionsBehindAnAlertDialog int             `json:"irreversible_actions_behind_an_alertdialog"`
	ReversibleActionsOnTheNativeConfirm    int             `json:"reversible_actions_on_the_native_confirm"`

	// (g) The screens write their own ceilings, by gap id.
	CeilingLedger    json.RawMessage `json:"ceiling_ledger"`
	CeilingsOnScreen []string        `json:"ceilings_on_screen"`

	// (h) The fake-vs-real sweep compares more collections than it did before this epic.
	ConformanceLedger              json.RawMessage `json:"conformance_ledger"`
	ConformanceCollectionsCompared int             `json:"conformance_collections_compared"`

	// The published contracts, anchored to the code table.
	Contracts       []ContractRequirement `json:"contracts"`
	ContractsDigest string                `json:"contracts_digest"`
}

// Complete reports the groups hold against a FAKE counterparty AND re-derives (a) through (h) from the bytes
// the proof carries. A proof declaring a posture set the ledger does not support, a zero over a key scan
// containing a hit, an approver equality the rows contradict, a route count above what was scanned, an
// irreversible action outside an alertdialog or a ceiling no page states fails HERE — in the shape verifier —
// rather than in a dedicated test somebody could forget to run.
func (p FleetConsoleProof) Complete() bool {
	if p.Peer != FleetConsolePeer || p.ContractsDigest != FleetConsoleContractsDigest() ||
		!slices.Equal(p.Contracts, FleetConsoleContracts) {
		return false
	}
	// (a) Pools created through a public surface, and the posture that could not be written before T1.
	created, postures, surfaces, err := SweepFleetConsolePools(p.PoolLedger)
	if err != nil || created != p.PoolsCreated || !slices.Equal(postures, p.PosturesCreated) {
		return false
	}
	if !slices.Contains(postures, "unsandboxed-host") || len(surfaces) == 0 {
		return false // the rented-Mac posture is the one the birth path exists for
	}
	// (b) A machine waited, and at least one was admitted FROM THE CONSOLE.
	pending, fromConsole, neverWaited, err := SweepFleetConsoleWaitingRoom(p.WaitingRoomLedger)
	if err != nil || len(neverWaited) != 0 || pending != p.MachinesPending ||
		fromConsole != p.MachinesAdmittedFromTheConsole || fromConsole < 1 || pending < 1 {
		return false
	}
	// (c) No key value anywhere, over five decoded sites each carrying a probe it found.
	hits, probed, sites, err := SweepFleetConsoleKeyScan(p.KeyScanLedger)
	if err != nil || hits != 0 || p.KeyValuesFound != 0 ||
		probed != len(FleetConsoleKeyScanSites) || len(sites) != len(FleetConsoleKeyScanSites) {
		return false
	}
	// (d) As many approver entries after the write as before it, over complete documents.
	before, after, partial, err := SweepFleetConsolePolicyWrites(p.PolicyLedger)
	if err != nil || len(partial) != 0 || before != after ||
		p.ApproverEntriesBefore != before || p.ApproverEntriesAfter != after {
		return false
	}
	// (e) Declared == scanned, in every colour scheme.
	declared, scanned, unscanned, err := SweepFleetConsoleRoutes(p.RouteLedger)
	if err != nil || len(unscanned) != 0 || declared != scanned ||
		p.RoutesDeclared != declared || p.RoutesAxeScanned != scanned {
		return false
	}
	// (f) Both directions of the confirmation split.
	guarded, native, misplaced, err := SweepFleetConsoleActions(p.ActionLedger)
	if err != nil || len(misplaced) != 0 ||
		p.IrreversibleActionsBehindAnAlertDialog != guarded || p.ReversibleActionsOnTheNativeConfirm != native {
		return false
	}
	// (g) All four ceilings, by gap id.
	onScreen, missingCeilings, err := SweepFleetConsoleCeilings(p.CeilingLedger)
	if err != nil || len(missingCeilings) != 0 || !slices.Equal(onScreen, p.CeilingsOnScreen) {
		return false
	}
	// (h) The sweep compares more than it did before this epic.
	compared, err := SweepFleetConsoleConformance(p.ConformanceLedger)
	if err != nil || len(compared) != p.ConformanceCollectionsCompared {
		return false
	}
	return true
}

// --- the canonical ledgers ----------------------------------------------------------------------------------
//
// EVERY ROW BELOW WAS PRODUCED BY A RUN OR PARSED OUT OF THE TREE, AND WHICH ONE IS STATED PER GROUP. The
// bundle generator reads these constants and the sweeps above compute its counters from them, so a number in
// the manifest is never typed by a human. What a human still types is the ROWS — which is why
// tests/uat/fleet-console re-derives (e), (f) and (g) from the console's own files and diffs (a), (b), (c),
// (d) and (h) against the lines a run printed.
//
// BYTE COUNTS ARE RECORDED AND NOT DIFFED, deliberately (the E25 T4 precedent). A DOM's size is a property of
// the build; pinning it would make this bundle a version pin rather than a measurement, and the next
// dependency bump would redden a security gate for a reason that is not a security fact. What IS diffed is
// the zero and the probe.

// FleetConsolePoolLedger is group (a): the pools this release created, each through a surface that did not
// exist before T1. Rows 2 and 3 come from Go component tests against real PostgreSQL; row 1 is the console
// leg, printed by tests/fleet.spec.ts as `POOL CREATED — …` (measured 2026-07-31).
const FleetConsolePoolLedger = `[
  {
    "pool": "console-created mac pool",
    "posture": "unsandboxed-host",
    "strict_enrollment": true,
    "created_via": "console",
    "test": "apps/web-console/tests/fleet.spec.ts:a pool is created from the console with the posture it was given"
  },
  {
    "pool": "component-tier mac pool",
    "posture": "unsandboxed-host",
    "strict_enrollment": true,
    "created_via": "public-api",
    "test": "TestPoolBirthReachesTheWaitingRoomAndTheApproveRoute"
  }
]`

// FleetConsoleWaitingRoomLedger is group (b). The console row is printed by tests/fleet.spec.ts as
// `WAITING ROOM — …`; the public-API row is T1's component leg, which drives one machine's whole life —
// enrolment, `pending`, a Dial that answers ErrPoolHasNoRunner, admission — against a real gateway.
const FleetConsoleWaitingRoomLedger = `[
  {
    "machine": "fixture machine in a strict pool",
    "pool": "pool_mac",
    "reached_pending": true,
    "admitted_from": "console",
    "test": "apps/web-console/tests/fleet.spec.ts:a machine waiting in a strict pool is on this page and is admitted from it"
  },
  {
    "machine": "component-tier runner enrolled over real mTLS",
    "pool": "component-tier mac pool",
    "reached_pending": true,
    "admitted_from": "public-api",
    "test": "TestPoolBirthReachesTheWaitingRoomAndTheApproveRoute"
  }
]`

// FleetConsoleKeyScanLedger is group (c), printed by tests/reveal-once.spec.ts as `KEY VALUE SCAN — …`. The
// byte counts are the ones the 2026-07-31 fake-profile run reported and are RECORDED rather than diffed; the
// hits and the probes are what the gate re-derives.
//
// THE web-storage PROBE IS SEEDED RATHER THAN FOUND, AND THAT IS A MEASUREMENT ABOUT THIS CONSOLE: it writes
// NOTHING to either storage (`grep -rn "sessionStorage\|localStorage" app/ lib/ components/` finds one
// COMMENT and no call), so the dump is empty and an empty haystack reports zero hits exactly like a clean
// one. There is no naturally-occurring token to point at, so the spec writes one, proves the sweep sees it,
// and removes it. A probe that could not exist is the case where "find a token that is allowed to be there"
// has to become "put one there".
const FleetConsoleKeyScanLedger = `[
  {
    "site": "dom",
    "subject": "the whole document plus every input's live value property, after the region was dismissed and the page reloaded",
    "bytes_scanned": 15778,
    "decoded_before_scanning": true,
    "key_value_hits": 0,
    "probe": "the minted key's own id",
    "probe_found": true
  },
  {
    "site": "web-storage",
    "subject": "every key and value of sessionStorage and localStorage",
    "bytes_scanned": 155,
    "decoded_before_scanning": true,
    "key_value_hits": 0,
    "probe": "palai-console-scan-control (SEEDED: this console writes nothing to either storage, so there is no found token to use)",
    "probe_found": true
  },
  {
    "site": "url",
    "subject": "window.location.href, so a query parameter or a fragment is covered",
    "bytes_scanned": 28,
    "decoded_before_scanning": true,
    "key_value_hits": 0,
    "probe": "the page path",
    "probe_found": true
  },
  {
    "site": "response-body",
    "subject": "every response body the reload produced, including the console's own GET /v1/api-keys",
    "bytes_scanned": 575910,
    "decoded_before_scanning": true,
    "key_value_hits": 0,
    "probe": "the minted key's own id",
    "probe_found": true
  },
  {
    "site": "later-response",
    "subject": "a fresh GET /v1/api-keys made AFTER the value was shown — the site the other four pass over",
    "bytes_scanned": 497,
    "decoded_before_scanning": true,
    "key_value_hits": 0,
    "probe": "the minted key's own id",
    "probe_found": true
  }
]`

// FleetConsolePolicyLedger is group (d), printed by tests/policy.spec.ts as `POLICY WRITE — …`. The two rows
// are the two halves of one claim and neither is sufficient: the first reads the STORED document back over
// the public API, the second reads the bytes the BROWSER SENT.
const FleetConsolePolicyLedger = `[
  {
    "field_changed": "pool",
    "approvers_before": 1,
    "approvers_after": 1,
    "fields_in_request": 5,
    "test": "apps/web-console/tests/policy.spec.ts:setting only the pool from the console leaves the approver list intact"
  },
  {
    "field_changed": "pool (the request, not the outcome)",
    "approvers_before": 1,
    "approvers_after": 1,
    "fields_in_request": 5,
    "test": "apps/web-console/tests/policy.spec.ts:the request body carries all five policy fields, not the one that changed"
  }
]`

// FleetConsoleRouteLedger is group (e), RE-DERIVED from apps/web-console/lib/routes.ts by
// tests/uat/fleet-console/source_test.go and diffed against `AXE ROUTE COVERAGE — …` by journey_test.go.
// `/login` is deliberately absent: it is not in CONSOLE_ROUTES and must not be, since the nav renders from
// that list and a link to the login page from inside a session is a link to nowhere.
//
// TWO ROWS MOVED 2026-08-05, and the re-derivation guard is what found both — which is the whole reason it
// re-derives instead of being maintained by hand:
//
//   - `/` swapped its readiness signal. A.2 unmounted the organizations route and the overview panel became
//     `panel-projects`; a ledger still naming the old testid would have had the axe sweep waiting for a
//     signal the page never emits, which is the failure this field exists to prevent, arriving through the
//     ledger rather than through the page.
//   - `/bots` is a page the console gained and the ledger did not. Every row here is one axe test — the spec
//     generates one scan per CONSOLE_ROUTES row and playwright.config.ts runs every spec under a light and a
//     dark project — so the page WAS being scanned all along; what was wrong is that the bundle certified
//     "every declared route was scanned" over a console with one more page than it had counted.
const FleetConsoleRouteLedger = `[
  {"path": "/", "ready_test_id": "panel-projects", "axe_scanned_in": ["light", "dark"]},
  {"path": "/sessions", "ready_test_id": "panel-sessions", "axe_scanned_in": ["light", "dark"]},
  {"path": "/runs", "ready_test_id": "run-button", "axe_scanned_in": ["light", "dark"]},
  {"path": "/environments", "ready_test_id": "panel-environments", "axe_scanned_in": ["light", "dark"]},
  {"path": "/mcp", "ready_test_id": "panel-mcp-connections", "axe_scanned_in": ["light", "dark"]},
  {"path": "/bots", "ready_test_id": "panel-bots", "axe_scanned_in": ["light", "dark"]},
  {"path": "/approvals", "ready_test_id": "panel-approvals", "axe_scanned_in": ["light", "dark"]},
  {"path": "/repositories", "ready_test_id": "panel-repository-bindings", "axe_scanned_in": ["light", "dark"]},
  {"path": "/agents", "ready_test_id": "panel-agent-profiles", "axe_scanned_in": ["light", "dark"]},
  {"path": "/tools", "ready_test_id": "panel-mcp-connections", "axe_scanned_in": ["light", "dark"]},
  {"path": "/history", "ready_test_id": "panel-runs", "axe_scanned_in": ["light", "dark"]},
  {"path": "/usage", "ready_test_id": "panel-usage-meters", "axe_scanned_in": ["light", "dark"]},
  {"path": "/capabilities", "ready_test_id": "panel-capabilities", "axe_scanned_in": ["light", "dark"]},
  {"path": "/deployment", "ready_test_id": "panel-deployment-settings", "axe_scanned_in": ["light", "dark"]},
  {"path": "/policy", "ready_test_id": "panel-api-keys", "axe_scanned_in": ["light", "dark"]},
  {"path": "/fleet", "ready_test_id": "panel-runner-pools", "axe_scanned_in": ["light", "dark"]},
  {"path": "/registry", "ready_test_id": "panel-model-connections", "axe_scanned_in": ["light", "dark"]}
]`

// FleetConsoleActionLedger is group (f), RE-DERIVED from the console's page sources by
// tests/uat/fleet-console/source_test.go — every ConfirmDestructive mount by its testId, every window.confirm
// CALL by its file.
//
// THE REVERSIBLE ROWS ARE KEYED BY FILE AND THE IRREVERSIBLE ONES BY testId, AND THAT IS NOT A COSMETIC
// CHOICE. A window.confirm has no handle a sweep can name, and app/fleet/page.tsx's single call serves BOTH
// cordon and resume — so splitting it into two rows would make a one-to-one source sweep impossible to write
// honestly. One row per confirmation SITE keeps the sweep total in both directions.
const FleetConsoleActionLedger = `[
  {
    "action": "runner-revoke-dialog",
    "reversible": false,
    "confirmation": "alertdialog",
    "source_file": "app/fleet/page.tsx",
    "reviews_live_state": true,
    "test": "apps/web-console/tests/fleet.spec.ts:the revoke dialog cannot open without the single read that carries the lease count"
  },
  {
    "action": "poolkey-revoke-dialog",
    "reversible": false,
    "confirmation": "alertdialog",
    "source_file": "app/fleet/page.tsx",
    "reviews_live_state": false,
    "test": "apps/web-console/tests/fleet.spec.ts:revoking a pool key shows the machines it already admitted and does not stop"
  },
  {
    "action": "key-revoke-dialog",
    "reversible": false,
    "confirmation": "alertdialog",
    "source_file": "app/policy/page.tsx",
    "reviews_live_state": false,
    "test": "apps/web-console/tests/policy.spec.ts:the revoke dialog is an alertdialog that reviews what is about to die"
  },
  {
    "action": "app/fleet/page.tsx",
    "reversible": true,
    "confirmation": "window.confirm",
    "source_file": "app/fleet/page.tsx",
    "reviews_live_state": false,
    "test": "apps/web-console/tests/fleet.spec.ts:cordon goes through the native confirmation and revoke does not"
  },
  {
    "action": "app/environments/page.tsx",
    "reversible": true,
    "confirmation": "window.confirm",
    "source_file": "app/environments/page.tsx",
    "reviews_live_state": false,
    "test": "apps/web-console/tests/policy.spec.ts:window.confirm is still the confirmation for the REVERSIBLE actions"
  }
]`

// FleetConsoleCeilingLedger is group (g), RE-DERIVED from the page sources by
// tests/uat/fleet-console/source_test.go: each row's gap id must appear as a `<code>` element and its test id
// as a `data-testid` in the named file, exactly once, so a deleted sentence reddens the gate.
const FleetConsoleCeilingLedger = `[
  {
    "gap_id": "FLT-P15",
    "page": "/fleet",
    "test_id": "fleet-remote-execution-note",
    "source_file": "app/fleet/page.tsx",
    "test": "apps/web-console/tests/fleet.spec.ts:the page writes the three things a fleet screen must not let an operator assume"
  },
  {
    "gap_id": "FLT-P12",
    "page": "/fleet",
    "test_id": "fleet-strict-note",
    "source_file": "app/fleet/page.tsx",
    "test": "apps/web-console/tests/fleet.spec.ts:the page writes the three things a fleet screen must not let an operator assume"
  },
  {
    "gap_id": "HIL-P11",
    "page": "/policy",
    "test_id": "policy-approvers-permissive",
    "source_file": "app/policy/page.tsx",
    "test": "apps/web-console/tests/policy.spec.ts:an empty approver list is shown as permissive, in words, before it is saved"
  },
  {
    "gap_id": "CON-P2",
    "page": "/policy",
    "test_id": "policy-console-key-note",
    "source_file": "app/policy/page.tsx",
    "test": "apps/web-console/tests/policy.spec.ts:the revoke dialog is an alertdialog that reviews what is about to die"
  }
]`

// FleetConsoleConformanceLedger is group (h): the collections tests/conformance.test.mjs compared on both
// sides, as the run prints them. SUBJECTS rather than a count, which is E25 T6's rule and which E28 T3's own
// run vindicated twice — reading the membership is what found that `runner-pools` had been comparable all
// along and that a compose stack does enrol a machine.
const FleetConsoleConformanceLedger = `[
  "GET /v1/organizations",
  "GET /v1/projects",
  "GET /v1/api-keys",
  "GET /v1/runner-pools",
  "GET /v1/runner-pools/{pool_id}/keys",
  "GET /v1/runners",
  "GET /v1/secret-refs",
  "GET /v1/knowledge-bases",
  "GET /v1/agents/{agent_id}/revisions",
  "GET /v1/tools",
  "GET /v1/tools/{tool_id}/revisions",
  "GET /v1/tool-sets",
  "GET /v1/repository-bindings",
  "GET /v1/environments",
  "GET /v1/responses",
  "GET /v1/usage/ledger"
]`

// carriesE28FleetConsoleCase reports whether a case is one of the four ids E28 OPENED — the FAMILY marker,
// shared by the manifest verifier and PromoteGateFor so the two can never disagree about what an E28 release
// is.
//
// THE FAMILY IS RECOGNIZED BY THE CASE IDS, NEVER BY THE fleet_console_claim THE GATE ENFORCES. Dispatching on
// the claim marker is precisely how a release DROPS it, reroutes to a weaker family and passes — the
// promote-gate-family-dispatch defect this repository has shipped once already.
func carriesE28FleetConsoleCase(c evidenceCase) bool {
	return slices.Contains(FleetConsoleCaseIDs, c.ID)
}
