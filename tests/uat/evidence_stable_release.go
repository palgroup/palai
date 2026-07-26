package uat

// The E18 T10 FINAL cross-epic EXIT-gate proof types (plan §T10) — the `release-1.0.0-rc1` sign-off.
//
// They live in their own file rather than growing evidence.go past 3000 lines, but they are the SAME
// package and the SAME discipline: Complete() gates the structure a claim marker requires, and every
// cross-case or cross-bundle RECOMPUTE runs in VerifyManifest (and again, independently, in the promote
// gate) against a CANONICAL source enforced here — never against the manifest's own copy of the thing it
// is certifying. That rule is the whole reason these types exist: E18 T8 found six fabricated checksums
// that were shape-valid and reproduced nothing.
//
// HONEST CEILING, once, for everything below: THE LOCAL CLOSURE OF THIS GATE IS AN RC. Nothing in this
// file may be read as a claim that a stable release was published, that a real CI run signed anything,
// that a transparency log holds an entry, that a registry received a package, or that a number was taken
// on reference hardware. Those are the §6 operator legs, and StableReleasePromoteGate REFUSES a `stable`
// promote that does not name every one of them in an operator attestation.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// StableReleaseBundle is the RC bundle's own release name. It is EXCLUDED from every cross-bundle
// recompute below: an index that could count itself would certify its own claims.
//
// The name is deliberately not "stable-1.0.0". The local closure of this gate is a release CANDIDATE;
// the stable flip is the operator's attested act (plan §T10 honest ceiling).
const StableReleaseBundle = "release-1.0.0-rc1"

// --- the canonical sources every recompute reads --------------------------------------------------------

// repoRootFromSource resolves the repository root relative to THIS source file (the canonicalEvalsRoot
// idiom), so a recompute finds the same committed bytes no matter the process working directory —
// `scripts/evidence/verify` runs from the repo root, `go test` runs from tests/uat/ and from
// tests/uat/stable-release/.
func repoRootFromSource() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..")
}

// AppendixAUATIDs is the master plan's Appendix A — every exact UAT id this program promised not to lose,
// in Appendix A order. It is a CODE table, which is what stops the release index from shrinking: an index
// that supplied its own id list could drop the ids it could not account for and still verify clean.
var AppendixAUATIDs = []string{
	// E02/E04/E07/E16 — API
	"API-001", "API-002", "API-003", "API-004", "API-005", "API-006",
	"API-007", "API-008", "API-009", "API-010", "API-011", "API-012",
	"API-013", "API-014", "API-015",
	// E08/E10 — Sessions
	"SES-001", "SES-002", "SES-003", "SES-004", "SES-005", "SES-006",
	"SES-007", "SES-008", "SES-009", "SES-010", "SES-011", "SES-012",
	// E08/E11 — Agents
	"AGT-001", "AGT-002", "AGT-003",
	// E08/E09/E17 — Subagents
	"SUB-001", "SUB-002", "SUB-003", "SUB-004", "SUB-005", "SUB-006",
	"SUB-007",
	// E06/E16 — Models
	"MOD-001", "MOD-002", "MOD-003", "MOD-004", "MOD-005", "MOD-006",
	"MOD-007", "MOD-008", "MOD-009", "MOD-010", "MOD-011", "MOD-012",
	// E17 — Knowledge
	"KNO-001", "KNO-002", "KNO-003", "KNO-004", "KNO-005", "KNO-006",
	"KNO-007", "KNO-008",
	// E06/E10/E12/E13 — Tools
	"TOL-001", "TOL-002", "TOL-003", "TOL-004", "TOL-005", "TOL-006",
	"TOL-007", "TOL-008", "TOL-009", "TOL-010", "TOL-011", "TOL-012",
	"TOL-013", "TOL-014", "TOL-015", "TOL-016", "TOL-017", "TOL-018",
	// E05/E10 — Engine/recovery
	"ENG-001", "ENG-002", "ENG-003", "ENG-004", "ENG-005", "ENG-006",
	"ENG-007", "ENG-008", "ENG-009", "ENG-010", "ENG-011", "ENG-012",
	"ENG-013", "ENG-014",
	// E05/E09/E10/E15 — Sandbox
	"SAN-001", "SAN-002", "SAN-003", "SAN-004", "SAN-005", "SAN-006",
	"SAN-007", "SAN-008", "SAN-009", "SAN-010", "SAN-011", "SAN-012",
	// E09/E10 — Repository
	"REP-001", "REP-002", "REP-003", "REP-004", "REP-005", "REP-006",
	"REP-007", "REP-008", "REP-009", "REP-010", "REP-011", "REP-012",
	// E11 — Automation
	"AUT-001", "AUT-002", "AUT-003", "AUT-004", "AUT-005", "AUT-006",
	"AUT-007", "AUT-008", "AUT-009", "AUT-010", "AUT-011", "AUT-012",
	"AUT-013",
	// E17 — Slack
	"SLK-001", "SLK-002", "SLK-003", "SLK-004", "SLK-005", "SLK-006",
	"SLK-007", "SLK-008",
	// E17 — A2A
	"A2A-001", "A2A-002", "A2A-003", "A2A-004", "A2A-005",
	// E13 — Tenancy
	"TEN-001", "TEN-002", "TEN-003", "TEN-004", "TEN-005",
	// E13 — Secrets
	"SEC-001", "SEC-002", "SEC-003",
	// E13/E14 — Data
	"DAT-001", "DAT-002", "DAT-003", "DAT-004", "DAT-005", "DAT-006",
	// E13 — Usage/billing
	"BIL-001", "BIL-002", "BIL-003", "BIL-004", "BIL-005", "BIL-006",
	// E13 — Quotas
	"QUO-001", "QUO-002",
	// E07/E14/E15 — Packaging/upgrade
	"OPS-001", "OPS-002", "OPS-003", "OPS-004", "OPS-005", "OPS-006",
	"OPS-007", "OPS-008",
	// E14/E15 — DR
	"DR-001", "DR-002", "DR-003", "DR-004", "DR-005", "DR-006",
	// E17 — Quality
	"QUA-001", "QUA-002", "QUA-003", "QUA-004",
	// E18 — Supply-chain security
	"SEC-101", "SEC-102", "SEC-103",
	// E18 — Performance
	"PER-001", "PER-002", "PER-003", "PER-004",
	// E17 — Console quality
	"UI-001", "UI-002",
}

// ManagedScopeUATIDs are the exact ids master plan §9 and MASTER-SPEC §64.15 place OUTSIDE this program:
// the managed/SaaS surface. They are marked "managed-scope, not claimed" in the release index and in the
// §64.15 checklist, which is the honest form — a control that was never in scope is a SCOPE LINE, not a
// gap, and pretending it is missing evidence would be as wrong as pretending it is covered.
var ManagedScopeUATIDs = map[string]string{
	"SAN-009": "microVM tenant isolation on a managed high-isolation fleet — master plan §9 (\"SAN-009 managed microVM fleet SaaS planına bağlı\") and §2.2. The local OCI seam is SEC-102; nothing here claims a microVM tier",
	"TEN-005": "support JIT access — master plan §9 (\"TEN-005 managed support SaaS scope\"). A self-hosted install has no managed support organization to grant JIT access to",
	"DR-003":  "regional failover — master plan §9 (\"DR-003 managed regional failover scope\"). One Docker Desktop host has no second region; the local DR seam is DR-001/002/004..006",
	"QUO-002": "noisy-neighbour weighted fairness — master plan §9 (\"pooled fairness managed scope\"). Pooled capacity is a managed-cell property; a dedicated install has no pool to be fair across",
}

// The release index's disposition vocabulary. Every one of them is RECOMPUTED — none is a free-text field
// a bundle author can choose, which is the difference between an index and a claim.
const (
	// DispositionBundleCarried: a committed evidence bundle carries this case with an outcome.
	DispositionBundleCarried = "bundle-carried"
	// DispositionCaseMaterialized: no bundle carries it, but tests/uat/cases/<ID>/case.yaml exists, so the
	// case is materialized and the catalog gates resolve its in-tree proofs. Weaker than bundle-carried and
	// named differently for exactly that reason.
	DispositionCaseMaterialized = "case-materialized"
	// DispositionManagedScope: the id is in ManagedScopeUATIDs — out of scope by decision, not by omission.
	DispositionManagedScope = "managed-scope"
	// DispositionUnmaterialized: no bundle, no case directory, not managed-scope. The honest name for "this
	// id has no case.yaml in this repository". The gate REPORTS these rather than refusing them: the plan
	// asks the index to list every exact id with what carries it, and silence would be the dishonest answer.
	DispositionUnmaterialized = "unmaterialized"
)

// ReleaseIndexEntry is one Appendix-A id's row in the release index: what carries it, with what outcome,
// under which disposition, and — where the carrying case's checksum could only be shape-checked — the E18
// T8 legacy label it ships with.
type ReleaseIndexEntry struct {
	ID              string `json:"id"`
	Bundle          string `json:"bundle"`
	Outcome         string `json:"outcome"`
	Disposition     string `json:"disposition"`
	ChecksumSurface string `json:"checksum_surface,omitempty"`
}

// bundleCarrier is one (release, status, checksum-surface) sighting of an id while scanning the committed
// bundles.
type bundleCarrier struct {
	Release string
	Status  string
	Surface string
}

// CommittedBundleOutcomes gathers every committed bundle's per-case outcomes from
// evidence/releases/*/manifest.json — the CANONICAL source the release index and the aggregate tier table
// are both recomputed from. StableReleaseBundle is excluded (an index may not count itself).
//
// It fails CLOSED: an unreadable releases directory or an undecodable manifest is an error, never an empty
// map, because "no bundle carries anything" is exactly what a fabricated index would want the recompute to
// conclude.
func CommittedBundleOutcomes() (map[string][]bundleCarrier, error) {
	dir := filepath.Join(repoRootFromSource(), "evidence", "releases")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read the committed evidence bundles (the canonical index source): %w", err)
	}
	out := map[string][]bundleCarrier{}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() || e.Name() == StableReleaseBundle {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name(), "manifest.json"))
		if err != nil {
			return nil, fmt.Errorf("read bundle %s: %w", e.Name(), err)
		}
		var m evidenceManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("decode bundle %s: %w", e.Name(), err)
		}
		if m.Release != e.Name() {
			return nil, fmt.Errorf("bundle directory %s ships a manifest declaring release %q — the index keys off the release name", e.Name(), m.Release)
		}
		seen++
		for _, c := range m.Cases {
			out[c.ID] = append(out[c.ID], bundleCarrier{Release: m.Release, Status: c.Status, Surface: c.ChecksumSurface})
		}
	}
	if seen == 0 {
		return nil, fmt.Errorf("no committed evidence bundle was read from %s — refusing to recompute an index over nothing", dir)
	}
	return out, nil
}

// RecomputeReleaseIndex is the AUTHORITY the manifest's own index copy is judged against: for every
// Appendix-A id, gathered from the committed per-bundle manifests plus the materialized case corpus.
//
// The carrier-selection rule matters and is deliberately pessimistic: carriers are sorted by release name
// and the FIRST NON-PASS carrier wins when the bundles disagree. Picking the green one would let an id
// that failed in one bundle and passed in another be indexed as passing — the index would launder it.
func RecomputeReleaseIndex() ([]ReleaseIndexEntry, error) {
	carriers, err := CommittedBundleOutcomes()
	if err != nil {
		return nil, err
	}
	casesDir := filepath.Join(repoRootFromSource(), "tests", "uat", "cases")
	out := make([]ReleaseIndexEntry, 0, len(AppendixAUATIDs))
	for _, id := range AppendixAUATIDs {
		entry := ReleaseIndexEntry{ID: id}
		if seen := carriers[id]; len(seen) > 0 {
			ordered := slices.Clone(seen)
			sort.Slice(ordered, func(i, j int) bool { return ordered[i].Release < ordered[j].Release })
			pick := ordered[0]
			for _, c := range ordered {
				if c.Status != "PASS" {
					pick = c
					break
				}
			}
			entry.Bundle, entry.Outcome, entry.ChecksumSurface = pick.Release, pick.Status, pick.Surface
			entry.Disposition = DispositionBundleCarried
			out = append(out, entry)
			continue
		}
		switch {
		case ManagedScopeUATIDs[id] != "":
			entry.Disposition = DispositionManagedScope
		default:
			if _, err := os.Stat(filepath.Join(casesDir, id, "case.yaml")); err == nil {
				entry.Disposition = DispositionCaseMaterialized
			} else {
				entry.Disposition = DispositionUnmaterialized
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// ReleaseIndexAnchor is hashParts over the RECOMPUTED index — the canonical anchor the RC bundle's case
// checksums are taken against. It is re-derivable in a clean checkout because its inputs are the committed
// bundle manifests and the committed case corpus, and it changes the moment any of them does, which is
// what makes a hand-written checksum in this bundle impossible to reproduce.
func ReleaseIndexAnchor() (string, error) {
	index, err := RecomputeReleaseIndex()
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, 4*len(index))
	for _, e := range index {
		parts = append(parts, e.ID, e.Bundle, e.Outcome, e.Disposition)
	}
	return hashParts(parts...), nil
}

// --- the §64.15 stable-release checklist, mechanically ---------------------------------------------------

// The four statuses a §64.15 checklist item can hold. They are RECOMPUTED from the release index, never
// declared: "evidenced" is the only one that means a committed bundle carries the proof.
const (
	ChecklistEvidenced       = "evidenced"          // every claim is bundle-carried and PASS
	ChecklistProvenNotBundle = "proven-not-bundled" // every claim is at least a materialized case; some carry no bundle evidence
	ChecklistIncomplete      = "incomplete"         // at least one claim is unmaterialized, or a carried claim is not PASS
	ChecklistNotClaimed      = "not-claimed"        // managed-scope: outside this program by decision
)

// StableChecklistItem is one MASTER-SPEC §64.15 bullet reduced to the claim-ID set that discharges it.
// Managed items carry Managed=true and close "managed-scope, not claimed" — that is the honest answer for
// a topology this program never had, and it is why the gate's output is an SH-3 POSTURE report rather than
// a blanket "stable".
type StableChecklistItem struct {
	Item       string
	Claims     []string
	Managed    bool
	WholeIndex bool     // item 1: the claim set IS the whole Appendix-A index
	Docs       []string // repo-relative paths that must exist (item 15 is a docs obligation, not a UAT one)
	Note       string
}

// StableChecklistItems is MASTER-SPEC §64.15's list, bullet by bullet and in its order. The "one local OCI
// and one managed high-isolation sandbox path" bullet is SPLIT into two rows because its two halves have
// different dispositions and a single row would have to round one of them off.
var StableChecklistItems = []StableChecklistItem{
	{
		Item:       "every P0/P1 UAT passed on supported local and self-host reference topology",
		WholeIndex: true,
		Note:       "the claim set is the WHOLE Appendix-A index; the posture report prints the per-disposition counts rather than a yes/no, because 188 ids do not collapse to one bit",
	},
	{
		Item:    "managed-only P0/P1 passed on production-equivalent cell/microVM topology",
		Claims:  []string{"SAN-009"},
		Managed: true,
		Note:    "managed-scope, not claimed — master plan §2.2 puts the managed cell outside this program entirely",
	},
	{
		Item:   "all three SDK conformance suites",
		Claims: []string{"API-012", "API-013", "API-014", "API-015", "TOL-018"},
	},
	{
		Item:   "at least two direct model-provider families plus one private/compatible endpoint",
		Claims: []string{"MOD-001", "MOD-002", "MOD-003"},
	},
	{
		Item:   "one local OCI sandbox path",
		Claims: []string{"SAN-001", "SAN-002", "SAN-003", "SAN-004", "SAN-005", "SAN-007", "SAN-008", "SEC-102"},
		Note:   "the first half of §64.15's \"one local OCI and one managed high-isolation sandbox path\" bullet",
	},
	{
		Item:    "one managed high-isolation sandbox path",
		Claims:  []string{"SAN-009"},
		Managed: true,
		Note:    "the second half of the same bullet — managed-scope, not claimed; SEC-102 proves the LOCAL OCI seam and says so",
	},
	{
		Item:   "process, container, and host kill/recovery",
		Claims: []string{"ENG-004", "ENG-005", "ENG-006", "SAN-006"},
	},
	{
		Item:   "pure/idempotent/irreversible tool replay",
		Claims: []string{"TOL-001", "TOL-002", "TOL-003"},
	},
	{
		Item:   "queued/steer/interrupt messaging",
		Claims: []string{"SES-003", "SES-004", "SES-005"},
	},
	{
		Item:   "secret isolation",
		Claims: []string{"SEC-002", "TOL-013", "REP-003"},
	},
	{
		Item:   "repository clone/diff/push/PR",
		Claims: []string{"REP-001", "REP-005", "REP-006", "REP-008"},
	},
	{
		Item:   "Slack and generic webhook/schedule journey",
		Claims: []string{"SLK-001", "AUT-001", "AUT-007", "AUT-011"},
	},
	{
		Item:   "backup/restore/upgrade",
		Claims: []string{"DR-002", "DR-004", "OPS-005", "OPS-006"},
	},
	{
		Item:   "tenant isolation and billing reconciliation",
		Claims: []string{"TEN-001", "TEN-002", "BIL-001", "BIL-003"},
	},
	{
		Item: "published security model, support policy, and operational runbooks",
		Docs: []string{
			"docs/security/threat-model.md",
			"docs/security/vulnerability-process.md",
			"docs/security/release-policy.md",
			"docs/operations/support-matrix.md",
			"docs/operations/runbooks/README.md",
		},
		Note: "a docs obligation, not a UAT one: the guard is that every named document EXISTS and its own tests/docs gates are green (they resolve every mitigation claim to an evidence id)",
	},
}

// ChecklistStatus is one §64.15 item's recomputed posture.
type ChecklistStatus struct {
	Item    string   `json:"item"`
	Status  string   `json:"status"`
	Claims  []string `json:"claims,omitempty"`
	Missing []string `json:"missing,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// RecomputeStableChecklist derives every §64.15 item's status from the release index. Nothing in a manifest
// contributes: this is the function the gate's posture report is printed from and the function a declared
// checklist is judged against.
func RecomputeStableChecklist(index []ReleaseIndexEntry) []ChecklistStatus {
	byID := make(map[string]ReleaseIndexEntry, len(index))
	for _, e := range index {
		byID[e.ID] = e
	}
	out := make([]ChecklistStatus, 0, len(StableChecklistItems))
	for _, item := range StableChecklistItems {
		st := ChecklistStatus{Item: item.Item, Claims: item.Claims, Note: item.Note}
		switch {
		case item.Managed:
			st.Status = ChecklistNotClaimed
		case item.WholeIndex:
			st.Status = ChecklistProvenNotBundle
			for _, e := range index {
				if e.Disposition == DispositionUnmaterialized {
					st.Missing = append(st.Missing, e.ID)
				}
			}
			if len(st.Missing) > 0 {
				st.Status = ChecklistIncomplete
			}
		case len(item.Docs) > 0:
			st.Status = ChecklistEvidenced
			for _, doc := range item.Docs {
				if _, err := os.Stat(filepath.Join(repoRootFromSource(), doc)); err != nil {
					st.Missing = append(st.Missing, doc)
					st.Status = ChecklistIncomplete
				}
			}
		default:
			st.Status = ChecklistEvidenced
			for _, id := range item.Claims {
				e, known := byID[id]
				switch {
				case !known:
					st.Missing = append(st.Missing, id+" (not an Appendix-A id)")
					st.Status = ChecklistIncomplete
				case e.Disposition == DispositionBundleCarried && e.Outcome == "PASS":
					// evidenced by this claim
				case e.Disposition == DispositionBundleCarried:
					st.Missing = append(st.Missing, id+" ("+e.Bundle+" carries it as "+e.Outcome+")")
					st.Status = ChecklistIncomplete
				case e.Disposition == DispositionCaseMaterialized:
					st.Missing = append(st.Missing, id+" (materialized case, no bundle evidence)")
					if st.Status == ChecklistEvidenced {
						st.Status = ChecklistProvenNotBundle
					}
				default:
					st.Missing = append(st.Missing, id+" ("+e.Disposition+")")
					st.Status = ChecklistIncomplete
				}
			}
		}
		out = append(out, st)
	}
	return out
}

// --- ReleaseIndexProof ------------------------------------------------------------------------------------

// ReleaseIndexProof is the evidence a release_index_claim requires (plan §T10): the master plan's
// "release index her exact ID'yi tek tek listeler" sentence, mechanically. Entries is the per-id index and
// Checklist is the §64.15 posture. NEITHER is trusted — verifyReleaseIndex RE-GATHERS both from the
// committed per-bundle manifests + the case corpus and refuses any row that disagrees, so the index's own
// copy is a rendering of the recompute rather than evidence for it.
type ReleaseIndexProof struct {
	Entries   []ReleaseIndexEntry `json:"entries"`
	Checklist []ChecklistStatus   `json:"checklist"`
	// RCBlockers is the count the E18 T9 triage table declares. VerifyManifest re-reads it from
	// docs/operations/known-gaps-1.0.md: a gate that took the manifest's word for "zero open P0/P1" would
	// be certifying a number the release wrote about itself.
	RCBlockers int `json:"rc_blockers"`
	// IndexAnchor is hashParts over the recomputed index. Carried so a reader can tell at a glance which
	// index this bundle was cut against; recomputed like everything else.
	IndexAnchor string `json:"index_anchor"`
}

// Complete gates the STRUCTURE: one entry per Appendix-A id in Appendix-A order, one status per §64.15
// item in §64.15 order, a disposition from the vocabulary, and a well-formed anchor. The VALUES are the
// cross-bundle recompute in verifyReleaseIndex — a tier is a function of outcomes this struct cannot see
// alone, and so is an index.
func (p ReleaseIndexProof) Complete() bool {
	if !checksumPattern.MatchString(p.IndexAnchor) || p.RCBlockers < 0 {
		return false
	}
	if len(p.Entries) != len(AppendixAUATIDs) || len(p.Checklist) != len(StableChecklistItems) {
		return false
	}
	for i, e := range p.Entries {
		if e.ID != AppendixAUATIDs[i] {
			return false // an index that could reorder or substitute ids is not an index of Appendix A
		}
		switch e.Disposition {
		case DispositionBundleCarried:
			if e.Bundle == "" || e.Outcome == "" {
				return false
			}
		case DispositionCaseMaterialized, DispositionManagedScope, DispositionUnmaterialized:
			if e.Bundle != "" || e.Outcome != "" {
				return false // a non-carried id may not name a carrier
			}
		default:
			return false
		}
	}
	for i, c := range p.Checklist {
		if c.Item != StableChecklistItems[i].Item {
			return false
		}
		switch c.Status {
		case ChecklistEvidenced, ChecklistProvenNotBundle, ChecklistIncomplete, ChecklistNotClaimed:
		default:
			return false
		}
	}
	return true
}

// rcBlockerLine is the machine-readable count the E18 T9 triage table publishes. tests/docs
// TestKnownGapsRCBlockerCountIsAccurate keeps it equal to the number of RC-blocker ROWS, so reading the
// line here reads the table.
var rcBlockerLine = regexp.MustCompile(`(?m)^RC-BLOCKERS:\s*(\d+)\s*$`)

// RecomputeRCBlockers reads the open-blocker count out of the E18 T9 triage table. It fails CLOSED: an
// unreadable or unparseable table is an error, never a comforting zero.
func RecomputeRCBlockers() (int, error) {
	path := filepath.Join(repoRootFromSource(), "docs", "operations", "known-gaps-1.0.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read the RC triage table (the gate reads its blocker count mechanically): %w", err)
	}
	m := rcBlockerLine.FindStringSubmatch(string(raw))
	if m == nil {
		return 0, fmt.Errorf("%s carries no `RC-BLOCKERS: <n>` line — the gate has nothing to read", path)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("RC-BLOCKERS is not a number: %w", err)
	}
	return n, nil
}

// verifyReleaseIndex is the E18 T10 anti-fabrication RECOMPUTE for the index and the checklist. It reads
// nothing from the proof as input: the index is re-gathered from the committed bundles, the checklist is
// re-derived from that index, and the RC-blocker count is re-read from the triage table. Returns one detail
// string per disagreement.
func verifyReleaseIndex(p *ReleaseIndexProof) []string {
	want, err := RecomputeReleaseIndex()
	if err != nil {
		return []string{"cannot recompute the release index from the committed bundles — an index that cannot be re-gathered is not evidence (fail closed): " + err.Error()}
	}
	blockers, blockersErr := RecomputeRCBlockers()
	return VerifyReleaseIndexAgainst(p, want, blockers, blockersErr)
}

// VerifyReleaseIndexAgainst is verifyReleaseIndex's pure core, exported for the same reason
// EvalPromoteGateAgainst is: it
// judges the proof against an index and a blocker count already in hand. Splitting it out is what makes the
// NON-ZERO-blockers branch drivable — a gate whose "an open RC-blocker refuses this release" clause could
// only be exercised by editing a committed document would be a clause nobody has ever seen fire.
func VerifyReleaseIndexAgainst(p *ReleaseIndexProof, want []ReleaseIndexEntry, blockers int, blockersErr error) []string {
	var problems []string
	byID := make(map[string]ReleaseIndexEntry, len(p.Entries))
	for _, e := range p.Entries {
		byID[e.ID] = e
	}
	for _, w := range want {
		got, ok := byID[w.ID]
		if !ok {
			problems = append(problems, fmt.Sprintf("UAT id %s is absent from the release index — the master plan's own rule is that the index lists EVERY exact id one by one", w.ID))
			continue
		}
		if got.Bundle != w.Bundle || got.Outcome != w.Outcome || got.Disposition != w.Disposition || got.ChecksumSurface != w.ChecksumSurface {
			problems = append(problems, fmt.Sprintf(
				"UAT id %s is indexed as {bundle:%q outcome:%q disposition:%q surface:%q} but the recompute over the committed bundles is {bundle:%q outcome:%q disposition:%q surface:%q} — the index is RE-GATHERED from the per-bundle manifests, never read out of this manifest",
				w.ID, got.Bundle, got.Outcome, got.Disposition, got.ChecksumSurface, w.Bundle, w.Outcome, w.Disposition, w.ChecksumSurface))
		}
	}

	if anchor := hashPartsOfIndex(want); p.IndexAnchor != anchor {
		problems = append(problems, fmt.Sprintf("index_anchor %s does not reproduce from the recomputed index (want %s) — a fabricated anchor", p.IndexAnchor, anchor))
	}

	wantChecklist := RecomputeStableChecklist(want)
	for i, wc := range wantChecklist {
		if i >= len(p.Checklist) {
			break // Complete() already refused a short checklist
		}
		gc := p.Checklist[i]
		if gc.Status != wc.Status {
			problems = append(problems, fmt.Sprintf(
				"§64.15 item %q is declared %q but recomputes to %q — a checklist item is a FUNCTION of the index, never a declaration (missing: %v)",
				wc.Item, gc.Status, wc.Status, wc.Missing))
		}
		if !slices.Equal(gc.Missing, wc.Missing) {
			problems = append(problems, fmt.Sprintf(
				"§64.15 item %q declares missing=%v but recomputes to %v — the shortfall is part of the posture, not a footnote the bundle may edit",
				wc.Item, gc.Missing, wc.Missing))
		}
	}

	switch {
	case blockersErr != nil:
		problems = append(problems, "cannot read the RC-blocker count from the E18 T9 triage table (fail closed): "+blockersErr.Error())
	case p.RCBlockers != blockers:
		problems = append(problems, fmt.Sprintf(
			"rc_blockers is %d but docs/operations/known-gaps-1.0.md declares %d — the count is read from the TABLE, never from the release that wants to pass",
			p.RCBlockers, blockers))
	case blockers != 0:
		problems = append(problems, fmt.Sprintf(
			"the RC triage table declares %d open RC-blocker(s) — a stable-release gate cannot close over an open blocker (plan §T10, §64.15 \"zero open P0/P1\")", blockers))
	}
	return problems
}

// hashPartsOfIndex is ReleaseIndexAnchor's pure half, over an index already in hand.
func hashPartsOfIndex(index []ReleaseIndexEntry) string {
	parts := make([]string, 0, 4*len(index))
	for _, e := range index {
		parts = append(parts, e.ID, e.Bundle, e.Outcome, e.Disposition)
	}
	return hashParts(parts...)
}

// carriesE18AreaClaim reports whether a case carries ANY E18 stable-release area claim — the FAMILY
// marker. It is the single owner of that list, read by both the manifest verifier and PromoteGateFor, so
// the two can never disagree about what an E18 release is.
//
// The tier and index claims are deliberately IN the list. Recognizing the family by them alone would be
// the defect the promote-gate family dispatch already has a rule about (never dispatch on the claim the
// gate enforces): a release could DROP the index claim, fall through to a weaker family's gate and pass.
func carriesE18AreaClaim(c evidenceCase) bool {
	return c.SupplyChainClaim != "" || c.PerformanceProfileClaim != "" || c.SandboxEscapeClaim != "" ||
		c.AuditIntegrityClaim != "" || c.ReleaseIndexClaim != "" || c.AggregateTierClaim != ""
}

// verifyE18AnchorPresence is what stops the two cross-bundle recomputes from being OPTIONAL. Both only run
// for a case whose marker is non-empty, so a bundle that DROPPED the markers while keeping every area proof
// (and every fabricated tier in the proof body) would verify 0 findings with the crown anchors silently not
// running. Mirroring verifyE17TierTablePresence: a manifest carrying ANY E18 area claim MUST carry EXACTLY
// ONE release_index_claim and EXACTLY ONE aggregate_tier_claim, each with its proof.
//
// "Exactly one" and not "at least one" because the promote gate reads the FIRST while this verifier checks
// all of them, so a second fabricated table could ride behind an honest one. Findings are release-level, so
// VerifyRelease fails the whole bundle — the right blast radius for a missing anchor.
func verifyE18AnchorPresence(m evidenceManifest) []Finding {
	family := false
	index, indexProof, tier, tierProof := 0, 0, 0, 0
	for _, c := range m.Cases {
		if carriesE18AreaClaim(c) {
			family = true
		}
		if c.ReleaseIndexClaim != "" {
			index++
			if c.ReleaseIndexProof != nil {
				indexProof++
			}
		}
		if c.AggregateTierClaim != "" {
			tier++
			if c.AggregateTierProof != nil {
				tierProof++
			}
		}
	}
	if !family {
		return nil
	}
	var findings []Finding
	add := func(format string, args ...any) {
		findings = append(findings, Finding{Kind: "invalid", Detail: fmt.Sprintf(format, args...)})
	}
	switch {
	case index == 0:
		findings = append(findings, Finding{Kind: "missing", Detail: "release_index_claim (this manifest carries E18 stable-release claims, so it is an E18 release and MUST carry the Appendix-A release index; without the claim marker the ENTIRE index recompute does not run and every declared outcome stands unverified — plan §T10)"})
	case index > 1:
		add("%d release_index_claims (want exactly 1): the promote gate judges the FIRST index while this verifier checks all of them, so a second index could ride behind an honest one — one release, one recomputed index (plan §T10)", index)
	case indexProof != index:
		findings = append(findings, Finding{Kind: "missing", Detail: "release_index_proof for the manifest's release_index_claim (a claim marker with no proof leaves the index unrecomputed — plan §T10)"})
	}
	switch {
	case tier == 0:
		findings = append(findings, Finding{Kind: "missing", Detail: "aggregate_tier_claim (this manifest carries E18 stable-release claims, so it MUST carry the product-wide capability posture; without the claim marker the cross-epic tier recompute does not run and a fabricated \"stable\" stands unverified — plan §T10)"})
	case tier > 1:
		add("%d aggregate_tier_claims (want exactly 1): a second posture table could ride behind an honest one — one release, one recomputed posture (plan §T10)", tier)
	case tierProof != tier:
		findings = append(findings, Finding{Kind: "missing", Detail: "aggregate_tier_proof for the manifest's aggregate_tier_claim (a claim marker with no proof leaves the posture unrecomputed — plan §T10)"})
	}
	return findings
}

// --- SupplyChainProof --------------------------------------------------------------------------------------

// CanonicalReleaseSigner is the ONE signing tool this program has (plan §2 design invariant): the E14 T5
// openssl P-256 signer, reused verbatim. A proof naming anything else is refused, and so is one whose
// algorithm string reaches for a word this repo has not earned.
const CanonicalReleaseSigner = "openssl-ecdsa-p256-sha256"

// CanonicalProvenanceBuilder is the builder identity the local session may honestly write. A CI
// workflow-identity here would be a lie: no GitHub Actions run has ever produced these bytes (§6 leg 1).
const CanonicalProvenanceBuilder = "local-macos-session"

// forbiddenSigningWords are the words a supply-chain proof may NOT use about itself. Each names an
// external system this program has never contacted, and each has been the shape of an overclaim before:
// "cosign-verified" for an openssl signature, "SLSA L2" for a local build, a Rekor entry for nothing.
var forbiddenSigningWords = []string{"cosign", "sigstore", "fulcio", "rekor", "transparency", "keyless", "slsa l2", "slsa level"}

// SupplyChainTamperArms is SEC-101's canonical six-arm matrix (plan §T4). A proof must declare exactly
// these and report every one REJECTED: a five-arm matrix would let the dropped arm be the one that passes.
var SupplyChainTamperArms = []string{"image", "index", "provenance", "sbom", "sdk-package", "signature"}

// CanonicalSBOMFormats is §51.2's required pair. One format is not "SPDX and CycloneDX".
var CanonicalSBOMFormats = []string{"cyclonedx", "spdx"}

// SupplyChainProof is the evidence a supply_chain_claim requires (plan §T10): the release artifact set the
// offline verifier blessed, the signed root it hangs from, and the tamper matrix that was rejected.
//
// IT TAKES SUP-3's RULE, and that is the reason ReleaseDir is a required field. `scripts/release/promote.sh`
// runs the offline verifier only when PALAI_RELEASE_DIR names a directory; unset, the tag is blessed on the
// evidence gate alone. That is deliberate AT THE WRAPPER — a fence there would run BEFORE the evidence gate
// and shadow the E15 T6 operator-leg refusal that scripts/uat/sh2 and scripts/uat/sdk-parity both grep for
// (TestPromoteReachesTheEvidenceGate pins the ordering). So the rule lives HERE instead: a release in the
// stable-release FAMILY cannot be promoted at all without a COMPLETE SupplyChainProof, and a SupplyChainProof
// is not complete unless it NAMES the artifact directory that was verified and records the verification as
// having happened OFFLINE. T9 wrote "if T10 does not take this rule, no rule enforces it"; this is T10
// taking it.
//
// HONEST CEILING: the byte-level re-verification of these digests and this signature is the JOURNEY's
// (scripts/uat/stable-release runs the real scripts/release/release-verify.sh over a real build output, the
// E15 AirgapProof / E16 PackagingProof precedent). What THIS type enforces at the manifest tier is the
// structure plus the honest-naming fences below, which are the claims a bundle could otherwise inflate for
// free.
type SupplyChainProof struct {
	ReleaseDir         string   `json:"release_dir"`
	IndexDigest        string   `json:"index_digest"`
	ArtifactDigests    []string `json:"artifact_digests"`
	SignedRoot         string   `json:"signed_root"`
	SignatureAlgorithm string   `json:"signature_algorithm"`
	OfflineVerified    bool     `json:"offline_verified"`
	OfflineEvidence    string   `json:"offline_evidence"`
	TamperArms         []string `json:"tamper_arms"`
	TamperRejected     int      `json:"tamper_rejected"`
	SBOMFormats        []string `json:"sbom_formats"`
	VulnDBSnapshot     string   `json:"vuln_db_snapshot"`
	ProvenanceBuilder  string   `json:"provenance_builder"`
	// TransparencyLog must be FALSE. It exists so the absence is an assertion rather than a silence: a
	// bundle that flips it to true is refused, which is the mechanical form of "this plan never claims a
	// transparency-log entry" (plan §2, §T3 honest ceiling).
	TransparencyLog bool `json:"transparency_log"`
}

// Complete gates the structure AND the honest-naming fences. Every clause below has been an overclaim
// somewhere in this program's history, which is why each is a refusal rather than a comment.
func (p SupplyChainProof) Complete() bool {
	if strings.TrimSpace(p.ReleaseDir) == "" {
		return false // SUP-3: a release family that carries no VERIFIED artifact set has nothing to promote
	}
	if !checksumPattern.MatchString(p.IndexDigest) || !checksumPattern.MatchString(p.SignedRoot) {
		return false
	}
	if len(p.ArtifactDigests) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, d := range p.ArtifactDigests {
		if !checksumPattern.MatchString(d) || seen[d] {
			return false // a duplicated digest inflates the artifact count for free
		}
		seen[d] = true
	}
	if p.SignatureAlgorithm != CanonicalReleaseSigner {
		return false
	}
	if p.ProvenanceBuilder != CanonicalProvenanceBuilder || p.TransparencyLog {
		return false
	}
	lower := strings.ToLower(p.SignatureAlgorithm + " " + p.OfflineEvidence + " " + p.VulnDBSnapshot)
	for _, word := range forbiddenSigningWords {
		if strings.Contains(lower, word) {
			return false // the signature is openssl and is NAMED that way everywhere (plan §2)
		}
	}
	if !p.OfflineVerified || strings.TrimSpace(p.OfflineEvidence) == "" {
		return false
	}
	arms := slices.Clone(p.TamperArms)
	sort.Strings(arms)
	if !slices.Equal(arms, SupplyChainTamperArms) || p.TamperRejected != len(SupplyChainTamperArms) {
		return false
	}
	formats := slices.Clone(p.SBOMFormats)
	sort.Strings(formats)
	if !slices.Equal(formats, CanonicalSBOMFormats) {
		return false
	}
	return strings.TrimSpace(p.VulnDBSnapshot) != ""
}

// --- PerformanceProfileProof --------------------------------------------------------------------------------

// PerformanceMetric is one gated metric with the RAW samples behind it. Samples is not decoration: the
// percentiles below are RECOMPUTED from it by the harness's own documented method, so a fabricated p95 is
// caught at the manifest tier without any file on disk.
type PerformanceMetric struct {
	Metric  string    `json:"metric"`
	Unit    string    `json:"unit"`
	Samples []float64 `json:"samples"`
	P50     float64   `json:"p50"`
	P95     float64   `json:"p95"`
	P99     float64   `json:"p99"`
	Errors  int       `json:"errors"`
	// Gate is the configured threshold: the percentile it reads and the ceiling that percentile must stay
	// under, plus where the number CAME FROM (a §54.3 target, or a budget the harness chose for itself).
	GatePercentile int     `json:"gate_percentile"`
	GateMax        float64 `json:"gate_max"`
	GateSource     string  `json:"gate_source"`
	GateValue      float64 `json:"gate_value"`
	Pass           bool    `json:"pass"`
}

// PerformanceNearestRank is the percentile method the T6 harness documents in every summary and the ONLY
// one this verifier will use: index = ceil(p/100*n)-1 on the ascending sort, clamped. Stating it in both
// places is what stops a re-computation from silently choosing a different interpolation.
const PerformanceNearestRank = "nearest-rank: sort ascending, index = ceil(p/100*n)-1 clamped to [0,n-1]"

func nearestRank(sorted []float64, pct int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(float64(pct)/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// PerformanceProfileProof is the evidence a performance_profile_claim requires (plan §T10, design
// invariant §2 "sayı ancak profille"): the MANDATORY hardware/load profile, the raw-sample digest, and the
// metrics whose percentiles this verifier RE-DERIVES from the raw samples carried alongside them.
//
// HONEST CEILING, carried in the proof itself and enforced: NoSLOClaim must be true and ReferenceHardware
// must be false. §54's targets are PRODUCT GOALS whose real measurement is §6 operator leg 3. And the
// profile stamps the machine but NOT co-tenant load (recorded as PER-1 in the RC triage: the same metric
// measured 229 ms, 7.5 s and 32.4 s depending only on what else was running), so a threshold here proves
// the GATE MECHANISM and nothing about capacity.
type PerformanceProfileProof struct {
	Case             string `json:"case"`
	LoadShape        string `json:"load_shape"`
	Machine          string `json:"machine"`
	OS               string `json:"os"`
	Arch             string `json:"arch"`
	Cores            int    `json:"cores"`
	MemoryBytes      int64  `json:"memory_bytes"`
	Docker           string `json:"docker"`
	Ceiling          string `json:"ceiling"`
	PercentileMethod string `json:"percentile_method"`
	SamplesSHA256    string `json:"samples_sha256"`
	// SampleCount is the number of raw samples CARRIED in Metrics below, and Complete() requires it to be
	// exactly that — "n=200" must not be able to describe three numbers. RunSampleCount is the whole run's
	// samples.jsonl, which SamplesSHA256 pins: a bundle carries the GATED metrics rather than every gauge
	// (PER-003's soak alone records 914 samples), and the two counts are separate fields so the difference
	// is visible instead of being rounded off into one flattering number.
	SampleCount    int     `json:"sample_count"`
	RunSampleCount int     `json:"run_sample_count"`
	MaxErrorRate   float64 `json:"max_error_rate"`

	Metrics []PerformanceMetric `json:"metrics"`

	NoSLOClaim        bool `json:"no_slo_claim"`
	ReferenceHardware bool `json:"reference_hardware"`
}

// Complete refuses a profileless run, a fabricated percentile and a gate that measured nothing.
func (p PerformanceProfileProof) Complete() bool {
	// (1) NO NUMBER WITHOUT A PROFILE. Blank means blank: a stamp of spaces is not a stamp, which is the
	// same rule harness.Profile.validate applies at the producing end.
	blank := func(s string) bool { return strings.TrimSpace(s) == "" }
	if blank(p.Case) || blank(p.LoadShape) || blank(p.Machine) || blank(p.OS) || blank(p.Arch) ||
		blank(p.Docker) || blank(p.Ceiling) || p.Cores <= 0 || p.MemoryBytes <= 0 {
		return false
	}
	if p.PercentileMethod != PerformanceNearestRank || !checksumPattern.MatchString(p.SamplesSHA256) || p.SampleCount <= 0 {
		return false
	}
	// (2) HONEST CEILING, ENFORCED. A proof that drops the no-SLO stamp or claims reference hardware is
	// refused — those two booleans are the claim's negative space and deleting them is the overclaim.
	if !p.NoSLOClaim || p.ReferenceHardware {
		return false
	}
	if len(p.Metrics) == 0 {
		return false
	}
	total := 0
	for _, m := range p.Metrics {
		if blank(m.Metric) || blank(m.Unit) || blank(m.GateSource) || len(m.Samples) == 0 {
			return false // a gate with no samples behind it is the vacuous gate the harness already refuses
		}
		if m.Errors < 0 || m.Errors > len(m.Samples) {
			return false
		}
		// A ZERO ceiling is legal and is the STRONGEST claim this tier makes — no delivery lost, no producer
		// starved, no gap on resume. Refusing GateMax == 0 would refuse exactly the invariants worth having.
		if m.GatePercentile <= 0 || m.GatePercentile > 100 || m.GateMax < 0 {
			return false
		}
		total += len(m.Samples)

		// (3) RECOMPUTE-OVER-COPY: every percentile and the gated value are re-derived from the RAW samples
		// this proof carries. A hand-written p95 cannot survive this.
		sorted := slices.Clone(m.Samples)
		sort.Float64s(sorted)
		if !closeEnough(m.P50, nearestRank(sorted, 50)) ||
			!closeEnough(m.P95, nearestRank(sorted, 95)) ||
			!closeEnough(m.P99, nearestRank(sorted, 99)) ||
			!closeEnough(m.GateValue, nearestRank(sorted, m.GatePercentile)) {
			return false
		}
		// (4) THE VERDICT IS RECOMPUTED TOO, on the derived value and the derived error rate — never read
		// out of the proof's own `pass`.
		errorRate := float64(m.Errors) / float64(len(m.Samples))
		want := nearestRank(sorted, m.GatePercentile) <= m.GateMax && errorRate <= p.MaxErrorRate
		if m.Pass != want {
			return false
		}
		if !want {
			// A failed gate is still a real measurement — the harness writes its artifacts — but it is not
			// evidence that the case it belongs to passed. Carrying one here would let a bundle report a
			// green PER case over a threshold that was exceeded.
			return false
		}
	}
	// The declared sample count must be the samples actually carried, or "n=200" could describe 3 numbers,
	// and the whole run must be at least as large as the part this proof carries.
	return total == p.SampleCount && p.RunSampleCount >= p.SampleCount
}

// closeEnough compares two float64 measurements. Percentiles are SELECTIONS from the sample set (nearest
// rank picks an existing element), so the only drift possible is JSON round-tripping, and 1e-9 relative is
// far tighter than any real gap between two distinct samples.
func closeEnough(a, b float64) bool {
	if a == b {
		return true
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return math.Abs(a-b) <= 1e-9*math.Max(scale, 1)
}

// --- SandboxEscapeProof ------------------------------------------------------------------------------------

// SandboxEscapeSuiteCases is the SEC-102 suite's canonical coverage: every SAN case this repository has
// MATERIALIZED, plus SEC-102's own added quarantine arm. A proof declaring fewer would be reporting "no
// escape" over a corpus it did not run.
var SandboxEscapeSuiteCases = []string{
	"SAN-001", "SAN-002", "SAN-003", "SAN-004", "SAN-005",
	"SAN-006", "SAN-007", "SAN-008", "SAN-011", "SEC-102",
}

// SandboxEscapeUnownedCases are the SAN ids the family reserves that this repository has never
// materialized. They are DECLARED rather than omitted so "not written" can be told from "written and
// quietly dropped from the suite".
var SandboxEscapeUnownedCases = []string{"SAN-009", "SAN-010", "SAN-012"}

// SandboxEscapeQuarantineArms are the arms whose PASS is what "quarantine works" MEANS. The claim is
// computed from these, never declared: if either is red the claim is false no matter how green the denials
// are.
var SandboxEscapeQuarantineArms = []string{
	"allocation-hygiene-and-substrate-quarantine",
	"uncertain-failure-job-quarantine",
}

// SandboxEscapeProof is the evidence a sandbox_escape_claim requires (plan §T10, SEC-102): the aggregated
// suite's outcome. It invents no escape class — every denial it reports was already written and already
// proven, which is the point of an aggregation.
type SandboxEscapeProof struct {
	Arms            []string `json:"arms"`
	CasesCovered    []string `json:"cases_covered"`
	CasesUnowned    []string `json:"cases_unowned"`
	QuarantineArms  []string `json:"quarantine_arms"`
	NoEscape        bool     `json:"no_escape"`
	QuarantineWorks bool     `json:"quarantine_works"`
	Failures        []string `json:"failures"`
	// LocalOCIOnly must be TRUE: this is the LOCAL OCI seam. The microVM / managed high-isolation path is
	// managed-scope and is not claimed, and kernel-exploit research is out of scope — the suite proves
	// DENIAL and QUARANTINE mechanics.
	LocalOCIOnly bool `json:"local_oci_only"`
}

func (p SandboxEscapeProof) Complete() bool {
	covered := slices.Clone(p.CasesCovered)
	sort.Strings(covered)
	if !slices.Equal(covered, SandboxEscapeSuiteCases) {
		return false
	}
	unowned := slices.Clone(p.CasesUnowned)
	sort.Strings(unowned)
	if !slices.Equal(unowned, SandboxEscapeUnownedCases) {
		return false
	}
	quarantine := slices.Clone(p.QuarantineArms)
	sort.Strings(quarantine)
	if !slices.Equal(quarantine, SandboxEscapeQuarantineArms) {
		return false
	}
	for _, want := range p.QuarantineArms {
		if !slices.Contains(p.Arms, want) {
			return false // a quarantine arm that is not in the suite did not run
		}
	}
	return len(p.Arms) > 0 && p.NoEscape && p.QuarantineWorks && len(p.Failures) == 0 && p.LocalOCIOnly
}

// --- AuditIntegrityProof -----------------------------------------------------------------------------------

// AuditAlertKinds is the typed alert vocabulary SEC-103 exercises, in sorted order. All four must have been
// RAISED by the negatives: an integrity verifier that only ever reports green has not been shown to alert.
var AuditAlertKinds = []string{"gap", "signature", "stale", "tamper"}

// AuditIntegrityProof is the evidence an audit_integrity_claim requires (plan §T10, SEC-103 — the E13-H
// "audit integrity linkage" closure): the green arm's recomputed head against the signed out-of-database
// checkpoint, plus the four typed alerts the negatives raised.
type AuditIntegrityProof struct {
	Algorithm      string   `json:"algorithm"`
	CheckpointHead string   `json:"checkpoint_head"`
	RecomputedHead string   `json:"recomputed_head"`
	AnchoredRows   int      `json:"anchored_rows"`
	AlertsRaised   []string `json:"alerts_raised"`
	// CheckpointOutsideStore must be TRUE — the anchor lives outside the mutable store (plan §1). An anchor
	// an attacker can already write is not tamper evidence, and this is the field that says so.
	CheckpointOutsideStore bool `json:"checkpoint_outside_store"`
	// PurgeIndistinguishableFromTamper must be TRUE. It is the AUD-1 admission, mechanically: §22.2
	// scrub_events UPDATEs an anchored row's payload, which is precisely the `tamper` signature, so arms 1
	// and 2 print byte-for-byte the same alert. Declaring FALSE would claim a purge-aware chain this design
	// point does not have — the operational rule is to re-cut the checkpoint immediately after any purge.
	PurgeIndistinguishableFromTamper bool `json:"purge_indistinguishable_from_tamper"`
}

func (p AuditIntegrityProof) Complete() bool {
	if strings.TrimSpace(p.Algorithm) == "" || p.AnchoredRows <= 0 {
		return false
	}
	if !checksumPattern.MatchString(p.CheckpointHead) || p.CheckpointHead != p.RecomputedHead {
		return false // the green arm's whole content: the chain RECOMPUTED from the rows equals the anchor
	}
	alerts := slices.Clone(p.AlertsRaised)
	sort.Strings(alerts)
	if !slices.Equal(alerts, AuditAlertKinds) {
		return false
	}
	return p.CheckpointOutsideStore && p.PurgeIndistinguishableFromTamper
}

// --- AggregateTierProof ------------------------------------------------------------------------------------

// AggregateTierProof is the FINAL form of the anti-fabrication anchor (plan §T10): the PRODUCT-WIDE
// capability posture, recomputed from the claim outcomes of EVERY committed bundle rather than one epic's,
// and asserted BIT-EQUAL to a running stack's `/v1/capabilities`. A fabricated cross-epic "stable" is a
// FAIL — that sentence is this type's entire reason to exist.
//
// EXT-1, TAKEN EXPLICITLY. The E18 T9 triage recorded that extensions-0.1.0's CapabilityTierProof describes
// a map produced only by the test's fullyMountedRouter() — no shipped deployment config sets
// PALAI_CAPABILITY_WORKER_LISTEN_ADDR, so NO DEPLOYED BINARY SERVES IT. This proof rests on the same seam
// and refuses to blur it: SnapshotSource must NAME the fully-mounted router, ServedByDeployedConfig must be
// FALSE, and UnmountedReason must name the environment variable no shipped config sets. A proof that
// flipped ServedByDeployedConfig to true would be making exactly the claim T9 said this must not make.
type AggregateTierProof struct {
	Capabilities   []CapabilityTierDeclaration `json:"capabilities"`
	Snapshot       map[string]string           `json:"snapshot"`
	SnapshotSource string                      `json:"snapshot_source"`
	ClaimsDigest   string                      `json:"claims_digest"`
	// OutcomeSource names WHERE the outcomes were gathered from. It is prose for the reader; the recompute
	// below reads the committed bundles itself and does not consult it.
	OutcomeSource          string `json:"outcome_source"`
	ServedByDeployedConfig bool   `json:"served_by_deployed_config"`
	UnmountedReason        string `json:"unmounted_reason"`
}

// aggregateSnapshotRequiredPhrase is what SnapshotSource must contain. Naming the constructor rather than
// "the real router" is the difference between a sentence that invites the deployed reading and one that
// cannot.
const aggregateSnapshotRequiredPhrase = "fullyMountedRouter"

// aggregateUnmountedRequiredPhrase is the environment variable no shipped deployment config sets. Requiring
// it in the reason means the proof cannot admit the ceiling in words vague enough to be forgotten.
const aggregateUnmountedRequiredPhrase = "PALAI_CAPABILITY_WORKER_LISTEN_ADDR"

// Complete gates the structure and the EXT-1 fences. The tier VALUES are the cross-bundle recompute in
// verifyAggregateTiers — a product-wide tier is a function of every epic's outcomes, which this struct
// cannot see alone.
func (p AggregateTierProof) Complete() bool {
	if p.ClaimsDigest != CapabilityClaimsDigest() || len(p.Capabilities) != len(CapabilityTierOrder) {
		return false
	}
	if !strings.Contains(p.SnapshotSource, aggregateSnapshotRequiredPhrase) {
		return false
	}
	if p.ServedByDeployedConfig || !strings.Contains(p.UnmountedReason, aggregateUnmountedRequiredPhrase) {
		return false
	}
	if strings.TrimSpace(p.OutcomeSource) == "" {
		return false
	}
	byName := make(map[string]CapabilityTierDeclaration, len(p.Capabilities))
	for _, d := range p.Capabilities {
		byName[d.Capability] = d
	}
	for _, capability := range CapabilityTierOrder {
		d, ok := byName[capability]
		if !ok || !capabilityTiers[d.DeclaredTier] {
			return false
		}
		if !slices.Equal(d.ClaimCaseIDs, CapabilityClaims[capability]) {
			return false
		}
		if _, ok := p.Snapshot[capability]; !ok {
			return false
		}
	}
	return true
}

// AggregateCapabilityTiers recomputes the product-wide tier table from EVERY committed bundle's per-case
// outcomes. Where E17's CapabilityTierProof recomputed over one bundle's cases, this reads the union — the
// cross-epic form the exit sentence names. A case carried by two bundles takes its WORST outcome (the same
// pessimistic rule the release index uses), so a red case in one epic's bundle cannot be washed out by a
// green copy in another's.
func AggregateCapabilityTiers() (map[string]string, error) {
	carriers, err := CommittedBundleOutcomes()
	if err != nil {
		return nil, err
	}
	status := make(map[string]string, len(carriers))
	for id, seen := range carriers {
		ordered := slices.Clone(seen)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].Release < ordered[j].Release })
		pick := ordered[0].Status
		for _, c := range ordered {
			if c.Status != "PASS" {
				pick = c.Status
				break
			}
		}
		status[id] = pick
	}
	return RecomputeCapabilityTiers(status), nil
}

// verifyAggregateTiers is the cross-epic RECOMPUTE. Neither the declaration nor the running stack's
// snapshot is an input: both are judged against the tier table derived from the committed bundles, and a
// disagreement is a refusal. Returns one detail string per disagreement.
func verifyAggregateTiers(p *AggregateTierProof) []string {
	want, err := AggregateCapabilityTiers()
	if err != nil {
		return []string{"cannot recompute the product-wide capability posture from the committed bundles — a posture that cannot be re-derived is a declaration (fail closed): " + err.Error()}
	}
	return verifyAggregateTiersAgainst(p, want)
}

// verifyAggregateTiersAgainst is verifyAggregateTiers' pure core, split for the same reason.
func verifyAggregateTiersAgainst(p *AggregateTierProof, want map[string]string) []string {
	byName := make(map[string]CapabilityTierDeclaration, len(p.Capabilities))
	for _, d := range p.Capabilities {
		byName[d.Capability] = d
	}
	var problems []string
	for _, capability := range CapabilityTierOrder {
		expected := want[capability]
		if got := byName[capability].DeclaredTier; got != expected {
			problems = append(problems, fmt.Sprintf(
				"capability %q declares product-wide tier %q but the recompute across EVERY committed bundle's claim outcomes is %q — a fabricated cross-epic tier is a FAIL (plan §T10)",
				capability, got, expected))
		}
		if got := p.Snapshot[capability]; got != expected {
			problems = append(problems, fmt.Sprintf(
				"capability %q: the fully-mounted router's /v1/capabilities served %q but the cross-epic recompute is %q — discovery must be BIT-EQUAL to the recomputed posture (plan §2, §T10)",
				capability, got, expected))
		}
	}
	return problems
}
