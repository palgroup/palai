package uat

// The E25 T9 promote gate (plan §T9). It lives beside promote.go rather than inside it for the same reason
// evidence_admin_console.go lives beside evidence.go, and it follows the family discipline exactly: the gate
// does its OWN re-derivation (never inheriting Complete()'s verdict), and it COMPOSES the epic underneath it
// rather than restating that epic's rules.

import (
	"encoding/json"
	"fmt"
	"slices"
)

// AdminConsolePromoteGate is the mechanical form of the E25 exit-gate sentence (plan §T9, §7): a release
// cannot be tagged while any of the console's guards is red. It has eight clauses:
//
//  1. the bundle must carry EXACTLY ONE complete AdminConsoleProof;
//
//  2. THE IDENTITY GATE IS TOTAL, RE-DERIVED. Every exported HTTP method of every Route Handler under the
//     console's app/api/palai tree must open with `requireSession`, and the two counts must be EQUAL — not
//     "most", not "the ones we listed". The ONE non-relay export is the login door, pinned by file name, so
//     the exception cannot multiply and cannot migrate onto a data route. Before E25 the console had zero
//     authentication of any kind while the relay exported POST/PATCH/DELETE, and the key behind it holds
//     every capability implicitly (`Scope.HasScope` on an empty scope set) — so anyone reaching the origin
//     could decide every tool approval E23 T9 had just opened;
//
//  3. EVERY DECLARED PAGE IS SCANNED, IN EVERY COLOUR SCHEME, RE-DERIVED. `lib/routes.ts` is the single
//     source of the nav and of the sweep, so "declared" and "scanned" must be the same number. And scanned
//     means scanned in BOTH projects: playwright-core@1.51.1 defaults `colorScheme` to "light" and says so
//     in its own types, so every scan this suite had ever run looked at one of the two palettes the
//     stylesheet ships;
//
//  4. NO WRITTEN SECRET VALUE COMES BACK, RE-DERIVED IN THREE LAYERS THAT EACH FAIL ON A DIFFERENT MISTAKE.
//     The SQL statement NAMES touching `ciphertext` are pinned to exactly two (this is the layer that fails
//     when someone writes a new query, before it has a caller, a view or a route); the byte scan covers the
//     DOM, every response body and every browser-served source map; and each layer must name a harmless
//     token it actually FOUND, because a haystack nobody has shown was read is not a haystack;
//
//  5. THE CONFORMANCE SWEEP COMPARES MORE THAN IT DID. Its item-shape arm could only ever compare the three
//     collections a bootstrap stack seeds, so everything else was envelope-only — which is how the fixture's
//     knowledge-base row carried `display_name` for months against a real projection whose field is `name`.
//     The floor is re-derived against its pre-E25 value and the compared subjects are carried, so a number
//     nobody can attribute is impossible;
//
//  6. THE REPAIRED LEDGER ROWS WERE RE-OBSERVED. This release's ADDED claim is that five rows of the
//     divergence ledger the console's evidence rested on were WRONG. A repair made on a source read is the
//     same move that put the false sentences there, so every row must carry how it was re-measured;
//
//  7. A SHIPPED RUNBOOK RUNS ON THE PUBLIC API ALONE, and a gated tool call is DECIDED from a screen with
//     nothing applying on a mismatched request hash;
//
//  8. NO TIER MAY ADVANCE — enforced by the composed E23 gate (which composes E22's, which composes E21's,
//     E20's, E19's mount derivation, E17's tier table and the eval numbers underneath it). Composing rather
//     than re-deriving keeps ONE owner of each rule.
//
// WHY IT COMPOSES E23 AND NOT E24, WHICH IS A MEASUREMENT RATHER THAN AN OVERSIGHT. E25 ran in PARALLEL with
// E24 (plan §7) and inherits its case set from `tool-approval-0.1.0`, not from `runner-fleet-0.1.0`: this
// epic built nothing on the fleet and carries no `FLT-` case, so composing the fleet gate would make an
// admin-console bundle owe a fleet proof it has no business carrying. The two families are siblings with
// disjoint id sets, which is why the relative order of their two clauses in PromoteGateFor is immaterial —
// and asserted to be, rather than assumed.
//
// THE TIER DECISION IS ARGUED, NOT ASSUMED, and this epic's counter-argument is a strong one:
//
//	"The console has an identity now. It can configure a deployment, write secrets, decide approvals, and
//	 every page of it is proven on two profiles and in two colour schemes. `console` should be `stable`."
//
// It is REFUSED for three reasons that belong in the code and not only in a commit message:
//
//	(1) §6 LEG 8 IS STILL OPEN AND E25 GREW IT. The leg is "a DEPLOYED console plus a manual screen-reader
//	    pass", and its scope now includes a login form, a SECRET-WRITING form, an approval decision surface,
//	    six configuration forms and a pagination control. There is no deployed console: compose is not a
//	    deployment, and `capabilities_mount_test.go` makes the console being a separate deployable the very
//	    basis of this tier's derivation. WHEN A LEG GROWS YOU ARE FURTHER FROM THE FLIP, NOT NEARER.
//	(2) THERE IS NO MANUAL SCREEN-READER PASS, AND THE GROWTH IS EXACTLY WHERE AXE IS WEAKEST. axe's own
//	    published figure is "on average 57% of WCAG issues", no scanner reads an ERROR MESSAGE for meaning,
//	    and E25 added nine pages of forms — error messages being most of what a form says. This epic
//	    measured that directly: SC 1.4.11 has NO axe rule at all, and every control in this console failed
//	    it in both colour schemes until the design pass measured the contrast by hand.
//	(3) E25 ENLARGED THE SURFACE THE TIER COVERS. An identity gate, a secret-writing form, an approval
//	    decision surface, nine new pages and eight new /v1 routes. A tier must not rise in the epic that
//	    widened what it covers — advancing one here would be certifying the new half on the strength of
//	    evidence gathered about the old one, which is precisely what this gate exists to prevent.
//
// WHAT WOULD HAVE HAD TO BE TRUE to move `console` to `stable`: (i) a DEPLOYED console, reachable over TLS
// at an address an operator uses, with a captured transcript; (ii) a manual VoiceOver/screen-reader pass
// over the forms; (iii) the removal of Peer's structural "fake". None of the three exists.
func AdminConsolePromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	var refusals []Refusal

	var ap *AdminConsoleProof
	claims := 0
	for _, c := range m.Cases {
		if c.AdminConsoleClaim == "" {
			continue
		}
		claims++
		if ap == nil {
			ap = c.AdminConsoleProof
		}
	}
	switch {
	case claims > 1:
		refusals = append(refusals, Refusal{Detail: fmt.Sprintf("%d admin_console_claims in one manifest (want exactly 1): this gate judges the FIRST admin-console proof, so a second could ride behind an honest one — one release, one re-derivation (plan §T9)", claims)})
	case ap == nil:
		refusals = append(refusals, Refusal{Detail: adminConsoleIncomplete})
	default:
		// THE GATE'S OWN SWEEPS RUN FIRST, before Complete() is consulted, and the ORDER is the whole point.
		// Complete() re-derives the same counts, so putting it ahead would make every branch below
		// unreachable for exactly the inputs it exists to catch — a guard that cannot fire, which is this
		// repository's most-found defect. Running them first means a tag decision rests on THIS gate's
		// derivation and the reader is told WHICH claim failed.
		refusals = append(refusals, adminConsoleRelayRefusals(ap)...)
		refusals = append(refusals, adminConsoleRouteRefusals(ap)...)
		refusals = append(refusals, adminConsoleSecretRefusals(ap)...)
		refusals = append(refusals, adminConsoleSweepRefusals(ap)...)
		refusals = append(refusals, adminConsoleLedgerRepairRefusals(ap)...)
		refusals = append(refusals, adminConsoleReachRefusals(ap)...)
		if !ap.Complete() {
			refusals = append(refusals, Refusal{Detail: adminConsoleIncomplete})
		}
	}

	// Clause 8 — composed verbatim. E25 opens no capability and moves no tier, so it owns NO tier rule of its
	// own; it inherits E23's, which inherits E22's, which inherits E21's, E20's and E19's cross-bundle
	// comparison against the committed extensions-0.1.0 baseline.
	return append(refusals, ToolApprovalPromoteGate(raw, target)...)
}

const adminConsoleIncomplete = "no COMPLETE AdminConsoleProof (every exported relay method opening with the session gate and the two counts EQUAL, with the ONE non-relay export pinned to the login door + every route lib/routes.ts declares axe-scanned in EVERY colour scheme, declared and scanned EQUAL + ZERO sentinel hits in the DOM, in every response body and in every browser-served source map, each layer naming a harmless token it DID find + EXACTLY the two canonical SQL statements touching `ciphertext`, over a corpus that proves the parser still parses + a conformance sweep comparing more item shapes than it did before this epic, with its subjects carried + every repaired divergence-ledger row RE-OBSERVED + every shipped-runbook step on /v1 alone + at least one approval decided from the screen, at least one refused on a request-hash mismatch and NONE applied on one + the canonical vendor and measurement ledger) — a release whose identity, accessibility, secret-absence, ledger-repair or reach guard is red cannot be tagged (plan §T9, §7 exit gate)"

// adminConsoleRelayRefusals re-derives (a) from the carried relay ledger.
func adminConsoleRelayRefusals(ap *AdminConsoleProof) []Refusal {
	relay, gated, doors, err := SweepAdminConsoleRelay(ap.RelayLedger)
	if err != nil {
		return []Refusal{{Detail: "the admin-console proof's relay ledger cannot be swept, so \"no write passes without a session\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case relay != gated:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d of %d exported relay method(s) open with the session gate — the gate is TOTAL or it is decoration, and a single ungated export is a write proxy holding an unlimited credential answering to anyone who reaches the origin. The proof declares %d/%d (plan §2, §3.6 D8, §T9)",
			gated, relay, ap.RelayMethodsBehindTheIdentityGate, ap.RelayExportedMethods)}}
	case len(doors) != 1:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d non-relay export(s) in the ledger (want exactly 1, the login door): the door is the ONE method that cannot pass the gate it issues, and a second one is a second unauthenticated write path wearing the same excuse (plan §T9)",
			len(doors))}}
	case relay < 2:
		return []Refusal{{Detail: "the relay ledger carries fewer than two exported methods — \"every method is gated\" over one method is a statement about that method, and this console exports five (four on the /v1 catch-all, one on the stream, which STARTS A RUN and is therefore the most expensive of them) (plan §T9)"}}
	case ap.RelayExportedMethods != relay || ap.RelayMethodsBehindTheIdentityGate != gated:
		return []Refusal{{Detail: fmt.Sprintf(
			"the relay ledger re-derives %d exported / %d gated but the proof declares %d / %d — the counts come from the rows, never from the manifest (plan §T9)",
			relay, gated, ap.RelayExportedMethods, ap.RelayMethodsBehindTheIdentityGate)}}
	}
	return nil
}

// adminConsoleRouteRefusals re-derives (b) from the carried route ledger.
func adminConsoleRouteRefusals(ap *AdminConsoleProof) []Refusal {
	declared, scanned, unscanned, err := SweepAdminConsoleRoutes(ap.RouteLedger)
	if err != nil {
		return []Refusal{{Detail: "the admin-console proof's route ledger cannot be swept, so \"every page is scanned\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case len(unscanned) != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d declared route(s) were not axe-scanned in every colour scheme: %v — Playwright collects a new SPEC automatically but a new PAGE used to get zero coverage silently, which is why lib/routes.ts generates the sweep; and a scan in one scheme is a scan of one of the two palettes app/globals.css ships (plan §3.6 D17, §T9)",
			len(unscanned), unscanned)}}
	case declared != scanned:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d route(s) declared but %d scanned — the two are the same list by construction, so a difference means the generator was bypassed (plan §T9)", declared, scanned)}}
	case declared < 2:
		return []Refusal{{Detail: "the route ledger carries fewer than two routes — a console declaring one page cannot certify that every page was scanned (plan §T9)"}}
	case ap.DeclaredRoutes != declared || ap.AxeScannedRoutes != scanned:
		return []Refusal{{Detail: fmt.Sprintf(
			"the route ledger re-derives %d declared / %d scanned but the proof declares %d / %d — the counts come from the rows, never from the manifest (plan §T9)",
			declared, scanned, ap.DeclaredRoutes, ap.AxeScannedRoutes)}}
	}
	return nil
}

// adminConsoleSecretRefusals re-derives (c) and (d) — the two halves of "a written value comes back nowhere"
// that are measurable from carried bytes. The third layer (a projection with no Value field) is pinned by
// apps/control-plane/internal/identity/environments_view_test.go on every `make verify` and is not a number.
func adminConsoleSecretRefusals(ap *AdminConsoleProof) []Refusal {
	hits, probed, kinds, err := SweepAdminConsoleByteScan(ap.ByteScanLedger)
	if err != nil {
		return []Refusal{{Detail: "the admin-console proof's byte-scan ledger cannot be swept, so \"no response body carries a secret value\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if hits != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"the sentinel was found %d time(s) in bytes a browser received — a value an operator typed into the configuration surface came back, which is the one thing this whole screen exists not to do. The proof declares %d (plan §2, §T9)",
			hits, ap.SecretValuesInResponsesAndSourceMaps)}}
	}
	if probed != len(kinds) || len(kinds) != len(adminConsoleByteScanKinds) {
		return []Refusal{{Detail: fmt.Sprintf(
			"the byte scan covers %d of %d layers — the source-map layer is the one the other two pass over (a sentinel planted in a source COMMENT is stripped from the minified chunk, carried verbatim in the map's sourcesContent and never fetched by a browser), so a missing layer is not a smaller proof but a different one (plan §T4, §T9)",
			len(kinds), len(adminConsoleByteScanKinds))}}
	}

	touching, corpus, err := SweepAdminConsoleCiphertextQueries(ap.QueryLedger)
	if err != nil {
		return []Refusal{{Detail: "the admin-console proof's query ledger cannot be swept, so \"exactly two statements touch the ciphertext column\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case !slices.Equal(touching, AdminConsoleCiphertextQueries):
		return []Refusal{{Detail: fmt.Sprintf(
			"the statements touching `ciphertext` re-derive as %v, want exactly %v — this is the layer that fails when someone WRITES a new query, before it has a caller, a view or a route, and the shortest path to a \"show me the value\" screen is exactly one new statement in storage/queries. A third name here is a design change that has to be argued for in the diff (plan §2, §3.6 D1, §T9)",
			touching, AdminConsoleCiphertextQueries)}}
	case !slices.Equal(ap.CiphertextQueryNames, AdminConsoleCiphertextQueries):
		return []Refusal{{Detail: fmt.Sprintf(
			"the proof declares the ciphertext statements as %v but the canonical pair is %v — the names come from the rows, never from the manifest (plan §T9)",
			ap.CiphertextQueryNames, AdminConsoleCiphertextQueries)}}
	case corpus < adminConsoleQueryCorpusFloor:
		return []Refusal{{Detail: fmt.Sprintf(
			"the query ledger carries %d named statement(s), under the floor of %d — a parser that stopped finding `-- name:` markers returns an empty list for `ciphertext` AND for everything else, so counting the whole corpus is what catches the parser independently of the term (plan §T9)",
			corpus, adminConsoleQueryCorpusFloor)}}
	}
	return nil
}

// adminConsoleSweepRefusals re-derives (e) from the carried subjects.
func adminConsoleSweepRefusals(ap *AdminConsoleProof) []Refusal {
	switch {
	case ap.SweepItemsBeforeE25 != AdminConsoleSweepFloorBeforeE25:
		return []Refusal{{Detail: fmt.Sprintf(
			"the proof declares a pre-E25 sweep floor of %d, want %d — the baseline is the THREE collections a bootstrap stack seeds (organizations, projects, api-keys), and a baseline the manifest can move is a rise nobody can read (plan §T9)",
			ap.SweepItemsBeforeE25, AdminConsoleSweepFloorBeforeE25)}}
	case ap.SweepItemsCompared <= ap.SweepItemsBeforeE25:
		return []Refusal{{Detail: fmt.Sprintf(
			"the conformance sweep compared %d item shape(s) against a pre-E25 floor of %d — the arm proved ENVELOPES everywhere and ITEMS almost nowhere, which is how the fixture's knowledge-base row carried `display_name` for months against a real projection whose field is `name`, and every page-opening task in this epic owed a seed (plan §2, §T9)",
			ap.SweepItemsCompared, ap.SweepItemsBeforeE25)}}
	case len(ap.SweepSubjects) != ap.SweepItemsCompared:
		return []Refusal{{Detail: fmt.Sprintf(
			"the sweep declares %d compared item shape(s) but names %d subject(s) — a bare number told nobody which collection had dropped out, and E25 T6 spent a whole sweep run discovering by elimination that GET /v1/agents is not on the list and structurally cannot be (plan §T9)",
			ap.SweepItemsCompared, len(ap.SweepSubjects))}}
	}
	return nil
}

// adminConsoleLedgerRepairRefusals re-derives (f). This is the clause for THIS release's added claim.
func adminConsoleLedgerRepairRefusals(ap *AdminConsoleProof) []Refusal {
	ids, reobserved, err := SweepAdminConsoleDivergenceRepairs(ap.DivergenceRepairLedger)
	if err != nil {
		return []Refusal{{Detail: "the admin-console proof's divergence-repair ledger cannot be swept, so \"five ledger rows the surface rested on were wrong\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case len(ids) == 0:
		return []Refusal{{Detail: "the repair ledger is empty — this bundle's ADDED claim is that rows of the divergence ledger the console's evidence rested on were measured WRONG, and a claim over no rows certifies nothing (plan §T9)"}}
	case reobserved != len(ids):
		return []Refusal{{Detail: fmt.Sprintf(
			"%d of %d repaired ledger row(s) carry a re-observation — a row repaired on the strength of READING compose.yaml or pagination.go is the same move that put the false sentence there, and this ledger's own rule is that a closed row is a red test rather than a deleted line (plan §3.6 D6, §T9)",
			reobserved, len(ids))}}
	case !slices.Equal(ap.RepairedDivergenceRows, ids):
		return []Refusal{{Detail: fmt.Sprintf(
			"the repair ledger re-derives %v but the proof declares %v — the ids come from the rows, never from the manifest (plan §T9)",
			ids, ap.RepairedDivergenceRows)}}
	}
	return nil
}

// adminConsoleReachRefusals re-derives (g) and (h): a shipped runbook that an operator can actually execute,
// and a gated tool call an operator can actually decide.
func adminConsoleReachRefusals(ap *AdminConsoleProof) []Refusal {
	onPublicAPI, below, err := SweepAdminConsoleRunbook(ap.RunbookLedger)
	if err != nil {
		return []Refusal{{Detail: "the admin-console proof's runbook ledger cannot be swept, so \"a shipped runbook runs on the public API alone\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case len(below) != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d runbook step(s) needed something below /v1: %v — the whole finding of E25 T7 is that a SHIPPED document told operators for three and a half days to publish a revision id no /v1 response had ever carried, so a step that needs a store method or an INSERT is a step an operator cannot take (plan §3.6 D14, §T9)",
			len(below), below)}}
	case onPublicAPI < 2:
		return []Refusal{{Detail: "the runbook ledger carries fewer than two steps — a document \"executable on the public API\" over one call is a statement about that call (plan §T9)"}}
	case ap.RunbookStepsOnThePublicAPI != onPublicAPI:
		return []Refusal{{Detail: fmt.Sprintf(
			"the runbook ledger re-derives %d public-API step(s) but the proof declares %d — the count comes from the rows, never from the manifest (plan §T9)",
			onPublicAPI, ap.RunbookStepsOnThePublicAPI)}}
	}

	applied, refusedOnHash, appliedOnMismatch, err := SweepAdminConsoleApprovals(ap.ApprovalLedger)
	if err != nil {
		return []Refusal{{Detail: "the admin-console proof's approval ledger cannot be swept, so \"a gated call was decided from a screen\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case len(appliedOnMismatch) != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d decision(s) APPLIED while the request hash did not match: %v — the binding is one-shot and belongs to the row's own data, so an approval whose arguments changed must authorize nothing; a console that could re-use a stale binding would be approving a call nobody read (plan §T5, §T9)",
			len(appliedOnMismatch), appliedOnMismatch)}}
	case applied < 1:
		return []Refusal{{Detail: "no approval in the ledger APPLIED — a queue that decided nothing satisfies the mismatch zero trivially, and the whole point of E25 T5 is that a Slack-less installation can now answer a gate that otherwise fails closed until a reaper releases it half an hour later (plan §T9)"}}
	case refusedOnHash < 1:
		return []Refusal{{Detail: "no decision in the ledger was REFUSED on a request-hash mismatch — the one-shot binding is then untested, and \"nothing applied on a mismatch\" is a statement about an experiment nobody ran (plan §T5, §T9)"}}
	case ap.ApprovalsDecidedFromTheConsole != applied || ap.ApprovalsRefusedOnARequestHashMismatch != refusedOnHash:
		return []Refusal{{Detail: fmt.Sprintf(
			"the approval ledger re-derives %d applied / %d refused-on-hash but the proof declares %d / %d — the counts come from the rows, never from the manifest (plan §T9)",
			applied, refusedOnHash, ap.ApprovalsDecidedFromTheConsole, ap.ApprovalsRefusedOnARequestHashMismatch)}}
	}
	return nil
}
