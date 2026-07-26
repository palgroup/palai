package stablerelease

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// The release-1.0.0-rc1 evidence bundle is GENERATED, not hand-maintained. It carries nine entries — the
// seven UAT cases E18 owns plus two release-level anchors — and every field is DERIVED from a canonical
// source in the tree:
//
//	proof_class + db_assertions   <- expectedE18Catalog (a bundle assertion cannot describe a proof the
//	                                 catalog gate does not resolve)
//	release index + checklist      <- uat.RecomputeReleaseIndex over the fifteen OTHER committed bundles
//	                                 and the materialized case corpus
//	rc_blockers                    <- uat.RecomputeRCBlockers, read from the E18 T9 triage table
//	product-wide capability tiers  <- uat.AggregateCapabilityTiers over every committed bundle's outcomes
//	performance percentiles        <- RAW samples read out of the T6 harness's own samples.jsonl artifacts
//	checksum                       <- hashParts(id, run_id, uat.ReleaseIndexAnchor())
//
// TestCommittedStableReleaseBundleIsTheGeneratorOutput asserts the committed file EQUALS this generator
// byte for byte, so the bundle can never drift from the tree, and the gates below verify it through the
// SHIPPED verifier. Regenerate with: PALAI_WRITE_STABLE_RELEASE_BUNDLE=1 go test ./tests/uat/stable-release/
//
// THE NAME IS THE CLAIM. This is `release-1.0.0-rc1`, not `stable-1.0.0`: the local closure of this gate is
// a release CANDIDATE, and SH-3 Stable is the operator attestation StableReleasePromoteGate demands.
const stableRelease = uat.StableReleaseBundle

// The two release-level anchor entries. Neither has a tests/uat/cases directory, deliberately: an index
// over every UAT id and a product-wide capability posture are not behaviours ONE case asserts — they are
// the recomputation over all of them. Both ids are namespaced to E18 so they can never be mistaken for a
// UAT case.
const (
	indexAnchorCaseID   = "E18-INDEX"
	postureAnchorCaseID = "E18-POSTURE"
)

// syntheticImageDigest is an obviously-unservable digest (the sdk-provider-parity / extensions precedent).
// The E18 evidence is script-tier and component-tier: the release verifier runs over real files and the PER
// harness starts real containers, but no ENGINE image serves these cases, so there is no real engine digest
// to name. The shape verifier requires the field for a non-external-receipt case, so it carries a value that
// could never be a real registry digest — and every case's db_assertions says so rather than borrowing
// another release's real digest and implying a run that did not happen.
var syntheticImageDigest = "sha256:" + strings.Repeat("e18", 21) + "e"

// --- values captured from the REAL runs that produced this bundle -------------------------------------
//
// These are not derivable from committed bytes and they do not pretend to be. They are what the shipping
// code PRINTED on the runs recorded here, the same way every other bundle carries its run ids:
//
//   - the supply-chain digests come from scripts/release/build.sh -> sbom-tool.py rollup -> provenance.sh
//     staged and signed by the real chain (release_verify_test.go's attestedRelease), and the index digest
//     moves with `built_at` and `commit`, so it pins THAT release directory and no other;
//   - the audit head comes from the real packages/audit chain over a real journal;
//   - the performance numbers are read out of the T6 harness's samples.jsonl by the generator itself, so
//     they are not typed here at all.
//
// The JOURNEY re-runs the real verifier, the real escape suite and the real audit chain in the same
// invocation that verifies this bundle (scripts/uat/stable-release), which is what keeps them honest
// between runs — the E17 T11 co-run discipline.
const (
	supplyChainIndexDigest = "sha256:acc83d895025a67d95cccd5152360e690f2bc68d4efd55a90ff9ba166e8561db"
	supplyChainSignedRoot  = "sha256:c485a9e25696784822c6a1d118792afd8b5b08303df019798f2be24700504ee2"
	supplyChainReleaseDir  = "scripts/release: build.sh --no-images --version 18.0.0 --cli-targets darwin/arm64 --runner-archs arm64, rolled up by sbom-tool.py and attested + signed by provenance.sh (release_verify_test.go attestedRelease), verified by scripts/release/release-verify.sh"
	supplyChainVulnDB      = "pinned offline grype DB v6.1.9, snapshot 2026-07-25T00:39:09Z (scripts/release/vulndb.lock.json) — a snapshot, never a live CVE feed"

	auditCheckpointHead = "sha256:9a4c04dcbeaf58c1cbee2fa8e3e05a5f8c4bb9f14e5b3ff59a0d02b47c8dc1e2"
)

var supplyChainArtifactDigests = []string{
	"sha256:50d4e982894ec109eb4eaa7fed591eb588e0db136fb93e7b7c528d574edda999", // cli/palai-darwin-arm64
	"sha256:39fb91e882a671669c0e1563cff2775fec3a6510a5a2e2ddcf61bcd7eace37d1", // runner-package/arm64/palai-runner-host-18.0.0-linux-arm64.tar.gz
	"sha256:84b142c1275fea19812abc75fced3f5e424296fcb76c00ac5386757066abed17", // images/fixture-engine-linux-arm64.tar
}

// perfArtifactRoot is where the T6 harness wrote the runs this bundle carries. Set
// PALAI_PERF_OUT to a fresh run's directory to regenerate against new measurements; the committed
// numbers below came from the run recorded in perfRuns.
func perfArtifactRoot() string { return os.Getenv("PALAI_PERF_OUT") }

// perfCarriedMetrics names, per PER case, the GATED metrics this bundle carries the RAW SAMPLES for. The
// full run is larger (PER-003 alone records 914 samples) and is pinned by samples_sha256; carrying every
// gauge would put ten thousand floats in a manifest without strengthening anything. What IS carried is
// every gated metric small enough to travel, INCLUDING all the zero-ceiling invariants — those are the
// strongest claims in this tier (no delivery lost, no producer starved, no gap on resume) and they are the
// ones a shrunken selection would be tempted to drop.
var perfCarriedMetrics = map[string][]string{
	"PER-001": {"api_metadata_read", "mutation_accept", "sse_first_event"},
	"PER-002": {"assign_to_ready_cold", "assign_to_ready_warm"},
	"PER-003": {"control_plane_rss_growth_bytes", "journal_bytes_per_event", "sse_reconnect_gaps", "sse_reconnect_resume"},
	"PER-004": {"queue_depth", "queue_unserved_per_producer", "trigger_deliveries_lost", "trigger_runs_admitted_per_key"},
}

// harnessSummary / harnessProfile mirror the fields tests/performance writes. They are decoded rather than
// imported because tests/performance is behind the `performance` build tag and this package rides
// `make verify`.
type harnessSummary struct {
	Case             string  `json:"case"`
	Ceiling          string  `json:"ceiling"`
	PercentileMethod string  `json:"percentile_method"`
	SamplesSHA256    string  `json:"samples_sha256"`
	SampleCount      int     `json:"sample_count"`
	MaxErrorRate     float64 `json:"max_error_rate"`
	Stats            []struct {
		Metric    string  `json:"metric"`
		Unit      string  `json:"unit"`
		Errors    int     `json:"errors"`
		P50       float64 `json:"p50"`
		P95       float64 `json:"p95"`
		P99       float64 `json:"p99"`
		GateValue float64 `json:"gate_value"`
		Pass      bool    `json:"pass"`
		Gate      *struct {
			Percentile int     `json:"percentile"`
			Max        float64 `json:"max"`
			Unit       string  `json:"unit"`
			Source     string  `json:"source"`
		} `json:"gate"`
	} `json:"stats"`
	NoSLOClaim        bool `json:"no_slo_claim"`
	ReferenceHardware bool `json:"reference_hardware"`
}

type harnessProfile struct {
	Case        string `json:"case"`
	LoadShape   string `json:"load_shape"`
	Machine     string `json:"machine"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Cores       int    `json:"cores"`
	MemoryBytes int64  `json:"memory_bytes"`
	Docker      string `json:"docker"`
	Ceiling     string `json:"ceiling"`
}

type harnessSample struct {
	Metric string  `json:"metric"`
	Value  float64 `json:"value"`
	OK     bool    `json:"ok"`
	Unit   string  `json:"unit"`
}

// perfRuns is the committed measurement set: the fields the generator would otherwise read out of
// PALAI_PERF_OUT. It is written by the generator itself when PALAI_PERF_OUT points at a harness run
// (`PALAI_WRITE_STABLE_RELEASE_BUNDLE=1 PALAI_PERF_OUT=... go test ./tests/uat/stable-release/`), so the
// numbers in this file are always a transcription of a real samples.jsonl and never typed by hand.
//
// It lives in a committed JSON fixture rather than in Go source because it is DATA — 200-odd raw float
// samples across four cases — and because the fixture is what makes the bundle regenerable on a machine
// with no Docker: the percentiles re-derive from these very samples, and uat.PerformanceProfileProof
// re-derives them again at verification time.
const perfFixture = "testdata/performance-runs.json"

type perfRun struct {
	Profile harnessProfile   `json:"profile"`
	Summary harnessSummary   `json:"summary"`
	Samples []harnessSample  `json:"samples"`
	Metrics []metricSnapshot `json:"-"`
}

type metricSnapshot struct {
	Metric  string
	Unit    string
	Samples []float64
	Errors  int
}

// loadPerfRuns reads the committed measurement fixture, refreshing it from PALAI_PERF_OUT first when the
// generator is being re-run against a live harness output.
func loadPerfRuns(t *testing.T) map[string]*perfRun {
	t.Helper()
	if root := perfArtifactRoot(); root != "" && os.Getenv("PALAI_WRITE_STABLE_RELEASE_BUNDLE") != "" {
		refreshPerfFixture(t, root)
	}
	raw, err := os.ReadFile(perfFixture)
	if err != nil {
		t.Fatalf("read the committed performance measurement fixture: %v", err)
	}
	var runs map[string]*perfRun
	if err := json.Unmarshal(raw, &runs); err != nil {
		t.Fatalf("decode %s: %v", perfFixture, err)
	}
	return runs
}

// refreshPerfFixture transcribes a live T6 harness output into the committed fixture, keeping ONLY the
// carried metrics' raw samples. It is the only path by which a performance number enters this bundle.
func refreshPerfFixture(t *testing.T, root string) {
	t.Helper()
	runs := map[string]*perfRun{}
	for caseID, carried := range perfCarriedMetrics {
		dir := filepath.Join(root, caseID)
		var sum harnessSummary
		var prof harnessProfile
		readJSONFile(t, filepath.Join(dir, "summary.json"), &sum)
		readJSONFile(t, filepath.Join(dir, "profile.json"), &prof)
		rawSamples, err := os.ReadFile(filepath.Join(dir, "samples.jsonl"))
		if err != nil {
			t.Fatalf("%s: read samples.jsonl: %v", caseID, err)
		}
		keep := map[string]bool{}
		for _, m := range carried {
			keep[m] = true
		}
		run := &perfRun{Profile: prof, Summary: sum}
		for _, line := range strings.Split(strings.TrimRight(string(rawSamples), "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var s harnessSample
			if err := json.Unmarshal([]byte(line), &s); err != nil {
				t.Fatalf("%s: decode sample: %v", caseID, err)
			}
			if keep[s.Metric] {
				run.Samples = append(run.Samples, s)
			}
		}
		runs[caseID] = run
	}
	body, err := json.MarshalIndent(runs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(perfFixture, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("refreshed %s from %s", perfFixture, root)
}

func readJSONFile(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

// performanceProof assembles one PER case's proof from the committed measurement fixture. Every percentile
// is DERIVED here from the raw samples — nothing is copied out of the harness's summary — so the value the
// bundle carries and the value uat.PerformanceProfileProof.Complete() recomputes come from the same bytes.
func performanceProof(t *testing.T, caseID string, run *perfRun) map[string]any {
	t.Helper()
	byMetric := map[string][]harnessSample{}
	for _, s := range run.Samples {
		byMetric[s.Metric] = append(byMetric[s.Metric], s)
	}
	gates := map[string]struct {
		Percentile int
		Max        float64
		Source     string
		Unit       string
	}{}
	for _, st := range run.Summary.Stats {
		if st.Gate == nil {
			continue
		}
		gates[st.Metric] = struct {
			Percentile int
			Max        float64
			Source     string
			Unit       string
		}{st.Gate.Percentile, st.Gate.Max, st.Gate.Source, st.Gate.Unit}
	}

	carried := append([]string(nil), perfCarriedMetrics[caseID]...)
	sort.Strings(carried)
	metrics := make([]map[string]any, 0, len(carried))
	total := 0
	for _, name := range carried {
		group := byMetric[name]
		if len(group) == 0 {
			t.Fatalf("%s: the measurement fixture carries no samples for gated metric %q", caseID, name)
		}
		gate, ok := gates[name]
		if !ok {
			t.Fatalf("%s: metric %q has no gate in the harness summary — this bundle carries GATED metrics only", caseID, name)
		}
		values := make([]float64, 0, len(group))
		errs := 0
		for _, s := range group {
			values = append(values, s.Value)
			if !s.OK {
				errs++
			}
		}
		sorted := append([]float64(nil), values...)
		sort.Float64s(sorted)
		gateValue := nearestRank(sorted, gate.Percentile)
		metrics = append(metrics, map[string]any{
			"metric": name, "unit": group[0].Unit, "samples": values,
			"p50": nearestRank(sorted, 50), "p95": nearestRank(sorted, 95), "p99": nearestRank(sorted, 99),
			"errors":          errs,
			"gate_percentile": gate.Percentile, "gate_max": gate.Max, "gate_source": gate.Source,
			"gate_value": gateValue,
			"pass":       gateValue <= gate.Max && float64(errs)/float64(len(values)) <= run.Summary.MaxErrorRate,
		})
		total += len(values)
	}

	return map[string]any{
		"case": run.Profile.Case, "load_shape": run.Profile.LoadShape, "machine": run.Profile.Machine,
		"os": run.Profile.OS, "arch": run.Profile.Arch, "cores": run.Profile.Cores,
		"memory_bytes": run.Profile.MemoryBytes, "docker": run.Profile.Docker, "ceiling": run.Profile.Ceiling,
		"percentile_method": run.Summary.PercentileMethod,
		"samples_sha256":    run.Summary.SamplesSHA256,
		"sample_count":      total,
		"run_sample_count":  run.Summary.SampleCount,
		"max_error_rate":    run.Summary.MaxErrorRate,
		"metrics":           metrics,
		"no_slo_claim":      run.Summary.NoSLOClaim,
		// The stamp is the claim's negative space: these numbers were NOT taken on reference hardware.
		"reference_hardware": run.Summary.ReferenceHardware,
	}
}

// nearestRank is the harness's documented percentile method, reproduced here so the generator derives the
// same value the verifier will.
// ponytail: a 9-line copy, the same one every generator in this tree keeps of hashParts.
func nearestRank(sorted []float64, pct int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (pct*len(sorted) + 99) / 100
	idx--
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// caseCeilings are per-case honesty clauses the generic assertion lines cannot express: what a case's proof
// does NOT reach. They are recorded IN the bundle, not only in a code comment, because the bundle is what a
// reader outside this repository sees.
var caseCeilings = map[string][]string{
	"SEC-101": {
		"SIGNED BY openssl, NOT BY SIGSTORE: the signature over the sha256sums root is the E14 T5 openssl P-256 signer reused VERBATIM (the one-signing-tool invariant). The provenance predicate is written in the in-toto Statement / SLSA v1 SHAPE and the builder identity says `local-macos-session` because that is what produced it. NO transparency-log entry, NO identity service and NO SLSA level is claimed anywhere — SupplyChainProof.Complete() REFUSES a proof that says otherwise, including one whose prose merely uses the words cosign/Sigstore/Fulcio/Rekor/keyless/SLSA-level",
		"SUP-3 IS ENFORCED HERE, NOT AT THE WRAPPER: scripts/release/promote.sh runs the offline verifier only when PALAI_RELEASE_DIR names a directory, and with it unset the tag is blessed on the evidence gate alone. That is deliberate — a fence there would run BEFORE the evidence gate and shadow the E15 T6 operator-leg refusal that scripts/uat/sh2 and scripts/uat/sdk-parity both grep for (TestPromoteReachesTheEvidenceGate). The rule that a release FAMILY must always carry a verified artifact set is this proof's: StableReleasePromoteGate refuses a promote with no COMPLETE SupplyChainProof, and a SupplyChainProof is not complete without a NAMED, offline-verified release directory",
		"THE ARTIFACT SET IS A SUBSET, NAMED AS ONE: the release directory verified here was built --no-images with one CLI target and one runner arch, plus a docker-save-SHAPED image tar. The full six-image amd64+arm64 matrix is `make release-matrix-smoke` and a boot-smoke of a foreign-arch image is NOT a full UAT run on that arch (§6 operator leg 3)",
	},
	"SEC-102": {
		"LOCAL OCI SEAM ONLY: the suite aggregates the denials this repository already proved on the local OCI sandbox. The microVM / managed high-isolation path is MANAGED-SCOPE, NOT CLAIMED — it is SAN-009, which the release index marks managed-scope and the §64.15 checklist closes `not-claimed` — and kernel-exploit research is out of scope. What is proven is DENIAL and QUARANTINE mechanics",
		"SAN-009, SAN-010 and SAN-012 are RESERVED AND UNOWNED in this repository and the proof says so in its own cases_unowned list, so \"not written\" can be told from \"written and quietly dropped from the suite\"",
	},
	"SEC-103": {
		"AN AUTHORISED RETENTION PURGE IS INDISTINGUISHABLE FROM TAMPERING, and the proof DECLARES it rather than hiding it: §22.2 scrub_events UPDATEs an anchored row's payload to {\"purged\": true} — same rows, same seq, different bytes — which is precisely the `tamper` signature, so a routine reaper sweep raises the highest-severity alert and an attacker who edits a row inherits \"the reaper did it\" as cover. Correct and unavoidable at this design point (the chain covers payload bytes); the operational rule is to RE-CUT the checkpoint immediately after any purge, and it is written into the alert text, audit.Ceilings, runbooks/audit-integrity-alert.md and the RC triage as AUD-1",
		"THIS CHAINS THE `events` SESSION JOURNAL, NOT `audit_events` (§50.3, protected by a REVOKE of UPDATE/DELETE instead). Neither control substitutes for the other. A checkpoint anchors a PREFIX; cadence is operator policy and continuous live verification wired to alerting is a §6 operator leg — this is the mechanism, not a running watchdog",
	},
	"PER-001": {
		"NO SLO, NO REFERENCE HARDWARE, AND NO CO-TENANT STAMP: these are macOS + Docker-Desktop numbers from a shared laptop. §54.3's targets are PRODUCT GOALS whose real measurement is §6 operator leg 3. Worse than that and stated on purpose (PER-1 in the RC triage): the profile stamps machine/cores/memory/OS/Docker but NOT concurrent load, so the same code measured 229 ms, 7.5 s and 32.4 s for one metric depending only on what else was running. The thresholds are configurable and prove the GATE MECHANISM; the numbers are an input",
		"DISPATCH IS OFF (E08 rule: the engine opens no tool to a real provider), so these are API + journal spine latencies — admission, read-back and stream start — never model or engine time",
	},
	"PER-002": {
		"'COLD' IS A POSITION, NOT A TEMPERATURE: it is the first attempt in this process, on an already-warm daemon and an already-migrated database, with the fixture image built locally moments before. Its p95 is a SINGLE observation and on this profile it is routinely the FASTEST attempt of the run. §54.3's cold-start budget is NOT established here — §6 operator leg 3 measures it. The number also excludes image pull, which is §54.3's own carve-out",
		"Attempts run one at a time, so NO capacity or saturation behaviour is measured or claimed",
	},
	"PER-003": {
		"'SOAK' IS A BOUNDED WINDOW OF MINUTES ON A LAPTOP — the §64.15 RC soak is §6 operator leg 7 and is claimed nowhere here. 'COMPACTION' is the implemented §8.3/§22.2 RETENTION payload scrub, NOT §25.14 context compaction, which is evaluation-gated and needs real model steps: dispatch is off per the E08 rule, so §25.14 is neither exercised nor claimed",
		"The journal gate bounds the SIZE of an event (mean payload bytes). It is NOT a proof that growth is linear in events rather than in session length — that would need a gate on the slope",
	},
	"PER-004": {
		"NO BROKER PRODUCT: the queue leg is the E17 T7 IN-PROCESS reference adapter and the trigger leg is the durable per-key admission gate behind the real API. No NATS, SQS, Pub/Sub or Kafka exists anywhere in this tree, which is the same reason the `queues` capability closes PREVIEW rather than stable",
	},
}

// buildStableReleaseManifest assembles the bundle. Everything is derived; nothing is typed twice.
func buildStableReleaseManifest(t *testing.T) []byte {
	t.Helper()

	index, err := uat.RecomputeReleaseIndex()
	if err != nil {
		t.Fatalf("recompute the release index: %v", err)
	}
	anchor, err := uat.ReleaseIndexAnchor()
	if err != nil {
		t.Fatalf("recompute the release index anchor: %v", err)
	}
	blockers, err := uat.RecomputeRCBlockers()
	if err != nil {
		t.Fatalf("read the RC-blocker count: %v", err)
	}
	tiers, err := uat.AggregateCapabilityTiers()
	if err != nil {
		t.Fatalf("recompute the product-wide capability posture: %v", err)
	}
	runs := loadPerfRuns(t)

	runIDFor := func(id string) string {
		return "run_e18_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
	}

	newCase := func(id string, assertions []string) map[string]any {
		runID := runIDFor(id)
		return map[string]any{
			"id": id, "status": "PASS", "run_id": runID,
			"image_digest": syntheticImageDigest,
			// The receipt fields a non-external-receipt case must carry. Their values are deliberately
			// unservable and every case says so in its own assertions.
			"provider_request_id": "chatcmpl-e18-not-a-live-run",
			"mtls_enroll":         "e18-no-runner-enrollment",
			"terminal":            map[string]any{"type": "completed", "count": 1},
			"db_assertions":       assertions,
			"checksum":            hashParts(id, runID, anchor),
		}
	}

	catalogAssertions := func(id string) []string {
		entry := expectedE18Catalog[id]
		out := []string{
			fmt.Sprintf("%s: proven by %d in-tree proof(s) at the %s tier — %s",
				id, len(entry.proofs), entry.class, strings.Join(entry.proofs, "; ")),
			"the case text NAMES its local seam and its §6 operator leg; TestE18CatalogMaterialized enforces both mechanically and TestE18CatalogGuardBites proves the enforcement bites",
			"AUTHORED BUNDLE, script/component tier: no ENGINE image serves these cases, so image_digest / provider_request_id / mtls_enroll carry deliberately unservable placeholder values and are never a claim that a container ran a model",
		}
		return append(out, caseCeilings[id]...)
	}

	cases := []map[string]any{}

	// SEC-101 — the supply chain.
	sec101 := newCase("SEC-101", catalogAssertions("SEC-101"))
	sec101["supply_chain_claim"] = "verified-offline-and-tamper-denied"
	sec101["supply_chain_proof"] = map[string]any{
		"release_dir": supplyChainReleaseDir, "index_digest": supplyChainIndexDigest,
		"artifact_digests": supplyChainArtifactDigests, "signed_root": supplyChainSignedRoot,
		"signature_algorithm": uat.CanonicalReleaseSigner,
		"offline_verified":    true,
		"offline_evidence":    "scripts/release/release_verify_test.go TestReleaseVerifyOfflineNetworkNone — the whole verify re-run inside a container with NO network device, printing `release-verify: OK ... verified OFFLINE` plus the two ceilings the container really has (GIT ABSENT, and the SDK packages UNVERIFIED by that run). The leg is operator-gated on an already-loaded openssl+python3 image (PALAI_RELEASE_TOOL_IMAGE) and SKIPS without one, so scripts/uat/stable-release RESOLVES the image and then asserts `--- PASS: TestReleaseVerifyOfflineNetworkNone` BY NAME, exiting rather than reporting green over a skip",
		"tamper_arms":         uat.SupplyChainTamperArms,
		"tamper_rejected":     len(uat.SupplyChainTamperArms),
		"sbom_formats":        uat.CanonicalSBOMFormats,
		"vuln_db_snapshot":    supplyChainVulnDB,
		"provenance_builder":  uat.CanonicalProvenanceBuilder,
		"transparency_log":    false,
	}
	cases = append(cases, sec101)

	// SEC-102 IS DELIBERATELY ABSENT FROM THIS BUNDLE, and that absence is the finding.
	//
	// `make uat-escape` is RED at this commit. Run in full it reports `no_escape=false`: the SAN-006 arm
	// (host-kill-fences-stale-writer) binds TestHostKillFencesStaleWriter, which requires
	// PALAI_COMPONENT_POSTGRES_URL — and `run_runner_suite`, the harness the arm runs under, never supplies
	// one, so the test SKIPS. E18 T7's own MUST-FIX 4 landed the rule that catches this ("an arm that ran
	// NOTHING is no longer a denial"), and the rule is doing its job. Supplying a URL by hand gets further
	// and then fails on the post-kill re-allocation ("workspace ... not found"), which is an E10/E13 seam.
	//
	// So there is no honest SandboxEscapeProof to carry: the type requires no_escape AND an empty
	// arms_not_attempted, and this session cannot produce either. Carrying SEC-102 as PASS would be the
	// exact fabrication this gate exists to refuse; carrying it as FAIL would make every future
	// `evidence-verify` red over a harness defect that is not an escape. It is recorded as `ESC-1` in the RC
	// triage instead, the release index reports it `case-materialized`, and §64.15's local-OCI item reports
	// `proven-not-bundled` rather than `evidenced`. The seven denial arms that DO run all pass; what is
	// unproven is the AGGREGATE report, not a denial.

	// SEC-103 — audit integrity.
	sec103 := newCase("SEC-103", catalogAssertions("SEC-103"))
	sec103["audit_integrity_claim"] = "chain-recomputes-and-all-four-alerts-raise"
	sec103["audit_integrity_proof"] = map[string]any{
		"algorithm":       "palai-audit-chain-sha256-v1",
		"checkpoint_head": auditCheckpointHead, "recomputed_head": auditCheckpointHead,
		"anchored_rows":                       6,
		"alerts_raised":                       uat.AuditAlertKinds,
		"checkpoint_outside_store":            true,
		"purge_indistinguishable_from_tamper": true,
	}
	cases = append(cases, sec103)

	// PER-001..004 — the profiled harness runs.
	for _, id := range []string{"PER-001", "PER-002", "PER-003", "PER-004"} {
		run, ok := runs[id]
		if !ok {
			t.Fatalf("the measurement fixture carries no run for %s", id)
		}
		c := newCase(id, catalogAssertions(id))
		c["performance_profile_claim"] = "profiled-and-percentiles-rederive-from-raw-samples"
		c["performance_profile_proof"] = performanceProof(t, id, run)
		cases = append(cases, c)
	}

	// E18-INDEX — the Appendix-A release index + the §64.15 checklist posture.
	indexCase := newCase(indexAnchorCaseID, []string{
		"RELEASE-LEVEL entry, not a UAT case (it has no tests/uat/cases directory): an index over every exact UAT id is not a behaviour ONE case asserts, it is the recomputation over all of them",
		"the verifier RE-GATHERS every row from the fifteen OTHER committed bundles' manifests and the materialized case corpus and refuses any row that disagrees — this manifest's own copy of the index is a rendering, never an input (plan §T10)",
		fmt.Sprintf("SH-3 POSTURE, NOT A BLANKET \"STABLE\": %d of the %d Appendix-A ids are carried by a committed evidence bundle; the rest are indexed with the honest disposition the recompute produces (case-materialized / managed-scope / unmaterialized), and the §64.15 checklist reports per-item status rather than a pass mark", countBundleCarried(index), len(index)),
		"SEC-102 IS ABSENT FROM THIS BUNDLE ON PURPOSE: `make uat-escape` reports no_escape=false at this commit because the SAN-006 arm's test SKIPS for want of PALAI_COMPONENT_POSTGRES_URL, which its harness never supplies (E18 T7's own \"an arm that ran NOTHING is not a denial\" rule catching a real gap). No honest SandboxEscapeProof exists, so none is carried — recorded as `ESC-1` in the RC triage; the index reports SEC-102 case-materialized and §64.15's local-OCI item reports proven-not-bundled",
		fmt.Sprintf("ZERO OPEN P0/P1 IS READ MECHANICALLY: rc_blockers is %d, re-read by the verifier from docs/operations/known-gaps-1.0.md's `RC-BLOCKERS:` line (kept equal to the table's RC-blocker rows by tests/docs TestKnownGapsRCBlockerCountIsAccurate). A non-zero count REFUSES this gate", blockers),
		"MANAGED-ONLY §64.15 ITEMS ARE MARKED \"managed-scope, not claimed\" AND NOT ROUNDED UP: production-equivalent cell/microVM topology and the managed high-isolation sandbox path close `not-claimed`, which is the honest disposition for a topology this program never had (master plan §2.2)",
	})
	indexCase["release_index_claim"] = "every-appendix-a-id-indexed-and-the-64-15-checklist-recomputed"
	indexCase["release_index_proof"] = map[string]any{
		"entries": index, "checklist": uat.RecomputeStableChecklist(index),
		"rc_blockers": blockers, "index_anchor": anchor,
	}
	cases = append(cases, indexCase)

	// E18-POSTURE — the product-wide capability posture.
	declarations := make([]map[string]any, 0, len(uat.CapabilityTierOrder))
	for _, capability := range uat.CapabilityTierOrder {
		declarations = append(declarations, map[string]any{
			"capability": capability, "declared_tier": tiers[capability],
			"claim_case_ids": uat.CapabilityClaims[capability],
		})
	}
	postureCase := newCase(postureAnchorCaseID, []string{
		"RELEASE-LEVEL entry, not a UAT case: a product-wide maturity posture is the RECOMPUTATION over every epic's claim outcomes",
		"the verifier recomputes each tier from the canonical code tables + the union of EVERY committed bundle's per-case outcomes (worst outcome wins when two bundles disagree) and refuses any declared tier or /v1/capabilities snapshot that differs — A FABRICATED CROSS-EPIC \"STABLE\" IS A FAIL (plan §T10)",
		"EXT-1, TAKEN EXPLICITLY: snapshot_source NAMES the test's fullyMountedRouter() and served_by_deployed_config is FALSE, because NO shipped deployment config sets PALAI_CAPABILITY_WORKER_LISTEN_ADDR — so no deployed binary serves this exact map. The E18 T9 triage recorded that the extensions-0.1.0 wording invited the deployed reading; this proof cannot, and Complete() refuses a proof that flips the flag or drops the reason",
		"only `knowledge` and `capability-workers` close STABLE. slack / a2a / queues / console close PREVIEW because the counterpart system was never contacted (§6 legs 1, 2, 5-EXTENDED, 8); knowledge-vector and apple-build close DISABLED because no vector store and no Apple signing material exist anywhere. Every one of those is COMPUTED from claim outcomes, not declared",
	})
	postureCase["aggregate_tier_claim"] = "product-wide-posture-recomputed-from-every-bundle"
	postureCase["aggregate_tier_proof"] = map[string]any{
		"capabilities": declarations, "snapshot": tiers,
		"snapshot_source":           "GET /v1/capabilities over real HTTP against the router built by fullyMountedRouter() in apps/control-plane/api (A2A and the capability-worker gateway BOTH mounted). It is a REAL api.NewRouter, and it is NOT a deployed binary's map",
		"claims_digest":             uat.CapabilityClaimsDigest(),
		"outcome_source":            "the union of every committed evidence bundle under evidence/releases/ except this one, re-gathered by uat.CommittedBundleOutcomes",
		"served_by_deployed_config": false,
		"unmounted_reason":          "no shipped deployment config sets PALAI_CAPABILITY_WORKER_LISTEN_ADDR (deploy/compose, deploy/helm and the production overlay all leave it unset), so the capability-worker gateway is not mounted in any deployment and no deployed binary serves this map (EXT-1, docs/operations/known-gaps-1.0.md)",
	}
	cases = append(cases, postureCase)

	manifest := map[string]any{
		"release": stableRelease, "git_sha": committedGitSHA, "api_version": "v1", "migration": "000040",
		"captured_at": capturedAt, "maturity": "rc",
		"cases": cases,
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return append(body, '\n')
}

func countBundleCarried(index []uat.ReleaseIndexEntry) int {
	n := 0
	for _, e := range index {
		if e.Disposition == uat.DispositionBundleCarried {
			n++
		}
	}
	return n
}

// capturedAt / committedGitSHA are the bundle's provenance stamp. Both are CONSTANTS because the generator
// must be byte-deterministic (the E17 precedent): a bundle whose content moved with the clock or the
// working tree could never be asserted equal to its committed copy.
const (
	capturedAt      = "2026-07-26T11:00:00Z"
	committedGitSHA = "92a3db925607471304227f7a6fc0368cbb1d8762"
)

// hashParts reproduces the tests/uat construction (sha256 of each part followed by a NUL, hex,
// sha256:-prefixed) so this generator and the gate derive the same re-derivable values.
// ponytail: a 6-line copy, the same one the managed-cloud / self-host / extensions gates keep.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// bundleDir is the committed bundle's home.
func bundleDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "evidence", "releases", stableRelease)
}

// TestCommittedStableReleaseBundleIsTheGeneratorOutput is the drift gate: the committed manifest must equal
// this generator byte for byte. Regenerate with PALAI_WRITE_STABLE_RELEASE_BUNDLE=1.
func TestCommittedStableReleaseBundleIsTheGeneratorOutput(t *testing.T) {
	want := buildStableReleaseManifest(t)
	path := filepath.Join(bundleDir(t), "manifest.json")
	if os.Getenv("PALAI_WRITE_STABLE_RELEASE_BUNDLE") != "" {
		if err := os.MkdirAll(bundleDir(t), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(want))
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the committed bundle: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the committed %s manifest is not this generator's output — regenerate with "+
			"PALAI_WRITE_STABLE_RELEASE_BUNDLE=1 go test ./tests/uat/stable-release/ (committed %d bytes, generated %d)",
			stableRelease, len(got), len(want))
	}
}

// TestCommittedStableReleaseBundleVerifiesClean drives the committed bundle through the SHIPPED verifier —
// the same call `make evidence-verify` makes. 0 failed / 0 missing / 0 secret findings, or the gate is not
// closed.
func TestCommittedStableReleaseBundleVerifiesClean(t *testing.T) {
	summary, err := uat.VerifyRelease(bundleDir(t), nil)
	if err != nil {
		t.Fatalf("verify the committed bundle: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("%s did not verify clean: %s\n%v", stableRelease, summary, summary.Findings)
	}
	t.Logf("%s: %s", stableRelease, summary)
}
