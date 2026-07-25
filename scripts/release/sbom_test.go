// E18 T2 — the SBOM/vulnerability gate. Three things are proven here, all of them mechanically:
//
//  1. the §51.3 POLICY GATE blocks a real critical finding, and only a live, owner-attributed,
//     time-bound exception lets it through (an expired one does not, a malformed one refuses the
//     whole policy file);
//  2. INDEX VERIFICATION is fail-closed — an artifact with no SBOM, an SBOM whose bytes changed by
//     one byte, an unindexed SBOM riding along, and a recorded decision that does not re-derive
//     from the raw findings each FAIL;
//  3. the generator, the scanner and the vulnerability DB are PINNED BY DIGEST (§2), not by tag.
//
// The scanner report under testdata/ is REAL output from the pinned grype 0.116.0 over the pinned
// DB snapshot, taken over testdata/vulnerable-fixture/requirements.txt (a deliberately old
// Django 1.11.0 that nothing in the product depends on). The requirements.txt is committed beside
// it so the report can be REGENERATED rather than believed — TestVulnFixtureIsRealScannerOutput
// pins the self-description it must carry, and TestSBOMPipelineLive regenerates the whole thing
// end to end when Docker and the hydrated DB are present.
package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	fixtureReport = "scripts/release/testdata/vulnerable-fixture.grype.json"
	fixtureSPDX   = "scripts/release/testdata/vulnerable-fixture.spdx.json"
	fixtureCDX    = "scripts/release/testdata/vulnerable-fixture.cdx.json"
)

// runSBOM execs the GIT-TRACKED scripts/release/sbom.sh — the same entry point the release chain
// calls — and returns its combined output and exit code.
func runSBOM(t *testing.T, args ...string) (string, int) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("/usr/bin/env", "bash", filepath.Join(root, "scripts/release/sbom.sh"))
	cmd.Args = append(cmd.Args, args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("sbom.sh %v: %v\n%s", args, err, out)
	}
	return string(out), code
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func writeJSON(t *testing.T, path string, doc any) {
	t.Helper()
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// criticalIDs reads the blocking finding ids out of the fixture report, so the exception cases
// below are built from what the scanner actually found rather than from four hard-coded strings
// that would silently stop matching if the fixture were regenerated.
func criticalIDs(t *testing.T) []string {
	t.Helper()
	var report struct {
		Matches []struct {
			Vulnerability struct{ ID, Severity string } `json:"vulnerability"`
		} `json:"matches"`
	}
	readJSON(t, filepath.Join(repoRoot(t), fixtureReport), &report)
	var ids []string
	for _, m := range report.Matches {
		if strings.EqualFold(m.Vulnerability.Severity, "critical") {
			ids = append(ids, m.Vulnerability.ID)
		}
	}
	if len(ids) == 0 {
		t.Fatal("the vulnerable fixture reports NO critical finding — it cannot prove a blocking gate")
	}
	return ids
}

type exception struct {
	ID       string `json:"id"`
	Owner    string `json:"owner"`
	Reason   string `json:"reason"`
	Opened   string `json:"opened"`
	Expires  string `json:"expires"`
	Artifact string `json:"artifact,omitempty"`
}

// policyWith writes a temp policy whose exceptions cover every critical in the fixture, opened
// `openedDaysAgo` days ago and expiring `expiresInDays` days from now (negative = already expired).
func policyWith(t *testing.T, openedDaysAgo, expiresInDays int, mutate func(*exception)) string {
	t.Helper()
	now := time.Now().UTC()
	var excs []exception
	for _, id := range criticalIDs(t) {
		e := exception{
			ID:      id,
			Owner:   "release-owner <owner@example.com>",
			Reason:  "test fixture: proving the exception path, not accepting a real risk",
			Opened:  now.AddDate(0, 0, -openedDaysAgo).Format("2006-01-02"),
			Expires: now.AddDate(0, 0, expiresInDays).Format("2006-01-02"),
		}
		if mutate != nil {
			mutate(&e)
		}
		excs = append(excs, e)
	}
	path := filepath.Join(t.TempDir(), "vuln-policy.json")
	writeJSON(t, path, map[string]any{
		"schema":     "palai.vuln-policy/v1",
		"blocks":     []string{"Critical"},
		"exceptions": excs,
	})
	return path
}

// --- 1. the policy gate ---------------------------------------------------------------------

func TestVulnGateBlocksCriticalFinding(t *testing.T) {
	out, code := runSBOM(t, "--gate", fixtureReport)
	if code == 0 {
		t.Fatalf("the gate PASSED a report carrying %d critical findings:\n%s", len(criticalIDs(t)), out)
	}
	for _, id := range criticalIDs(t) {
		if !strings.Contains(out, "BLOCK Critical "+id) {
			t.Errorf("the gate did not name %s as a blocking finding:\n%s", id, out)
		}
	}
	if !strings.Contains(out, `"result": "blocked"`) {
		t.Errorf("the decision does not say blocked:\n%s", out)
	}
}

func TestVulnGateLetsALiveOwnedExceptionThrough(t *testing.T) {
	out, code := runSBOM(t, "--gate", fixtureReport, "--policy", policyWith(t, 1, 30, nil))
	if code != 0 {
		t.Fatalf("a live, owner-attributed exception did not pass the gate (exit %d):\n%s", code, out)
	}
	for _, id := range criticalIDs(t) {
		if !strings.Contains(out, "EXCEPTED Critical "+id) {
			t.Errorf("%s was not reported as excepted:\n%s", id, out)
		}
	}
}

func TestVulnGateRefusesAnExpiredException(t *testing.T) {
	// Opened 60 days ago, expired 30 days ago: inside the 90-day ceiling, so the policy FILE is
	// valid — it is the exception that no longer applies. That is the case an "is it well-formed?"
	// check would wave through.
	out, code := runSBOM(t, "--gate", fixtureReport, "--policy", policyWith(t, 60, -30, nil))
	if code == 0 {
		t.Fatalf("an EXPIRED exception still suppressed the critical findings:\n%s", out)
	}
	if !strings.Contains(out, "expired on") {
		t.Errorf("the refusal does not say the exception expired:\n%s", out)
	}
}

func TestVulnGateRefusesMalformedExceptions(t *testing.T) {
	cases := []struct {
		name   string
		policy string
		want   string
	}{
		{"no owner", policyWith(t, 1, 30, func(e *exception) { e.Owner = "" }), "has no owner"},
		{"no reason", policyWith(t, 1, 30, func(e *exception) { e.Reason = "" }), "has no reason"},
		{"no expiry", policyWith(t, 1, 30, func(e *exception) { e.Expires = "" }), "has no expires"},
		{"open-ended", policyWith(t, 1, 400, nil), "the ceiling is 90"},
		{"expires before it opened", policyWith(t, 1, -30, nil), "on or before it opened"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := runSBOM(t, "--gate", fixtureReport, "--policy", tc.policy)
			if code == 0 {
				t.Fatalf("a malformed exception was accepted:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("refusal does not say %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestVulnGateRefusesAPolicyThatBlocksNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vuln-policy.json")
	writeJSON(t, path, map[string]any{"schema": "palai.vuln-policy/v1", "blocks": []string{}})
	out, code := runSBOM(t, "--gate", fixtureReport, "--policy", path)
	if code == 0 || !strings.Contains(out, "not a gate") {
		t.Fatalf("a policy with an empty block list was accepted (exit %d):\n%s", code, out)
	}
}

// The shipped policy must itself be well-formed and must actually block criticals — a repo whose
// committed policy silently accepts everything has a gate in name only.
func TestShippedPolicyBlocksCritical(t *testing.T) {
	var policy struct {
		Schema     string   `json:"schema"`
		Blocks     []string `json:"blocks"`
		Exceptions []struct {
			ID, Owner, Reason, Opened, Expires string
		} `json:"exceptions"`
	}
	readJSON(t, filepath.Join(repoRoot(t), "scripts/release/vuln-policy.json"), &policy)
	if policy.Schema != "palai.vuln-policy/v1" {
		t.Fatalf("shipped policy schema is %q", policy.Schema)
	}
	found := false
	for _, s := range policy.Blocks {
		if strings.EqualFold(s, "critical") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the shipped policy does not block Critical: %v", policy.Blocks)
	}
	for _, e := range policy.Exceptions {
		if e.Expires == "" || e.Owner == "" {
			t.Errorf("shipped exception %s is not time-bound + owner-attributed", e.ID)
		}
	}
}

// --- 2. index verification is fail-closed -----------------------------------------------------

// stageDir builds a populated release directory around the REAL fixture SBOMs and scanner report
// by running the REAL rollup (so every digest in the index is computed by the code under test, not
// by the test). `pass` picks a policy that excepts the fixture's criticals, which is how the clean
// baseline is reached without a second, artificially clean scanner report.
func stageDir(t *testing.T, pass bool) string {
	t.Helper()
	root := repoRoot(t)
	dir := t.TempDir()
	sbomDir := filepath.Join(dir, "sbom")
	if err := os.MkdirAll(sbomDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "art.bin"), []byte("release artifact bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for src, dst := range map[string]string{
		fixtureSPDX:   "art.bin.spdx.json",
		fixtureCDX:    "art.bin.cdx.json",
		fixtureReport: "art.bin.grype.json",
	} {
		b, err := os.ReadFile(filepath.Join(root, src))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sbomDir, dst), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	policy := filepath.Join(root, "scripts/release/vuln-policy.json")
	if pass {
		policy = policyWith(t, 1, 30, nil)
	}
	b, err := os.ReadFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sbomDir, "vuln-policy.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	writeJSON(t, filepath.Join(dir, "release-index.json"), map[string]any{
		"schema":    "palai.release-index/v1",
		"artifacts": []map[string]any{{"kind": "cli", "file": "art.bin", "os": "linux", "arch": "arm64"}},
	})
	tsv := filepath.Join(t.TempDir(), "artifacts.tsv")
	if err := os.WriteFile(tsv, []byte("cli\tart.bin\tart.bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("python3", filepath.Join(root, "scripts/release/sbom-tool.py"), "rollup")
	cmd.Env = append(os.Environ(),
		"DIR="+dir, "MANIFEST=release-index.json", "SDK=0", "SCANNED=1", "TSV="+tsv,
		"LOCK="+filepath.Join(root, "scripts/release/vulndb.lock.json"),
		"POLICY="+filepath.Join(sbomDir, "vuln-policy.json"),
		"SYFT_IMAGE=test", "GRYPE_IMAGE=test")
	out, err := cmd.CombinedOutput()
	if pass && err != nil {
		t.Fatalf("rollup: %v\n%s", err, out)
	}
	if !pass && err == nil {
		t.Fatalf("rollup did NOT block on the fixture's criticals:\n%s", out)
	}
	return dir
}

func verifyDir(t *testing.T, dir string) (string, int) {
	t.Helper()
	return runSBOM(t, "--dir", dir, "--verify")
}

func TestSBOMVerifyAcceptsAWellFormedDir(t *testing.T) {
	out, code := verifyDir(t, stageDir(t, true))
	if code != 0 {
		t.Fatalf("a well-formed dir failed verification (exit %d):\n%s", code, out)
	}
	if !strings.Contains(out, "re-derived from") {
		t.Errorf("verify does not say it re-derived the decision:\n%s", out)
	}
}

func TestSBOMVerifyRefusesAnArtifactWithNoSBOM(t *testing.T) {
	dir := stageDir(t, true)
	index := filepath.Join(dir, "release-index.json")
	var doc map[string]any
	readJSON(t, index, &doc)
	delete(doc["artifacts"].([]any)[0].(map[string]any), "sbom")
	writeJSON(t, index, doc)

	out, code := verifyDir(t, dir)
	if code == 0 {
		t.Fatalf("an artifact with NO sbom slot passed index verification:\n%s", out)
	}
	if !strings.Contains(out, "fails index verification") {
		t.Errorf("refusal does not name the missing SBOM:\n%s", out)
	}
}

func TestSBOMVerifyRefusesAOneByteTamper(t *testing.T) {
	dir := stageDir(t, true)
	// One byte, in a place that keeps the document valid JSON: the point is that the DIGEST moves,
	// not that the file stops parsing.
	path := filepath.Join(dir, "sbom/art.bin.spdx.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(b), "1.11.0", "1.11.9", 1)
	if tampered == string(b) {
		t.Fatal("nothing to tamper with in the fixture SBOM")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := verifyDir(t, dir)
	if code == 0 {
		t.Fatalf("a tampered SBOM passed verification:\n%s", out)
	}
	if !strings.Contains(out, "the SBOM bytes changed") {
		t.Errorf("refusal does not name the digest mismatch:\n%s", out)
	}
}

func TestSBOMVerifyRefusesAnUnindexedRider(t *testing.T) {
	dir := stageDir(t, true)
	// The sibling of T3's "every file must be in the signed sha256sums" hardening: an SBOM no
	// indexed artifact claims must not be able to ride along and look official.
	if err := os.WriteFile(filepath.Join(dir, "sbom/rider.spdx.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := verifyDir(t, dir)
	if code == 0 || !strings.Contains(out, "no indexed artifact claims") {
		t.Fatalf("an unindexed SBOM rode along (exit %d):\n%s", code, out)
	}
}

func TestSBOMVerifyRederivesTheDecisionRatherThanReadingIt(t *testing.T) {
	// Flip the RECORDED verdict to "pass" while the raw findings still carry four criticals. A
	// verifier that trusted the manifest's own copy would go green here (§2 recompute-over-copy).
	dir := stageDir(t, true)
	sbomDir := filepath.Join(dir, "sbom")
	shipped, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts/release/vuln-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sbomDir, "vuln-policy.json"), shipped, 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := verifyDir(t, dir)
	if code == 0 {
		t.Fatalf("verify accepted a recorded 'pass' that does not re-derive:\n%s", out)
	}
	if !strings.Contains(out, "re-deriving it from the raw findings") &&
		!strings.Contains(out, "blocking finding") {
		t.Errorf("refusal does not name the re-derivation:\n%s", out)
	}
}

// --- 3. everything is pinned by digest --------------------------------------------------------

var pinRE = regexp.MustCompile(`^(SYFT|GRYPE)_IMAGE="([^"$]+)"`)

func TestGeneratorAndScannerArePinnedByDigest(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts/release/sbom.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pinned := 0
	for _, line := range strings.Split(string(b), "\n") {
		m := pinRE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		pinned++
		if !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(m[2]) {
			t.Errorf("%s_IMAGE is %q — §2 wants a @sha256 digest, never a mutable tag", m[1], m[2])
		}
	}
	if pinned != 2 {
		t.Fatalf("expected the syft and grype pins in sbom.sh, found %d", pinned)
	}
}

func TestVulnDBSnapshotIsPinnedAndDated(t *testing.T) {
	var lock struct {
		Schema        string `json:"schema"`
		ScannerImage  string `json:"scanner_image"`
		SnapshotDate  string `json:"snapshot_date"`
		ArchiveSHA256 string `json:"archive_sha256"`
		ArchiveURL    string `json:"archive_url"`
		Note          string `json:"note"`
	}
	readJSON(t, filepath.Join(repoRoot(t), "scripts/release/vulndb.lock.json"), &lock)
	if lock.Schema != "palai.vulndb-lock/v1" {
		t.Fatalf("lock schema is %q", lock.Schema)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(lock.ArchiveSHA256) {
		t.Errorf("the DB archive is not pinned by a sha256: %q", lock.ArchiveSHA256)
	}
	if _, err := time.Parse(time.RFC3339, lock.SnapshotDate); err != nil {
		t.Errorf("snapshot_date %q is not a timestamp: %v", lock.SnapshotDate, err)
	}
	if !strings.Contains(lock.ScannerImage, "@sha256:") {
		t.Errorf("the locked scanner image is not digest-pinned: %q", lock.ScannerImage)
	}
	// Honest naming (§2): the reader must meet the word SNAPSHOT, not an implied live feed.
	for _, want := range []string{"PINNED OFFLINE SNAPSHOT", "not a live CVE feed", "§6"} {
		if !strings.Contains(lock.Note, want) {
			t.Errorf("the lock's note does not say %q: %s", want, lock.Note)
		}
	}
}

// The committed fixture must be REAL scanner output, self-describing, over the LOCKED snapshot —
// otherwise the gate tests above are proving a gate against a hand-written file.
func TestVulnFixtureIsRealScannerOutput(t *testing.T) {
	var report struct {
		Descriptor struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			DB      struct {
				Status struct {
					From  string `json:"from"`
					Built string `json:"built"`
					Valid bool   `json:"valid"`
				} `json:"status"`
			} `json:"db"`
		} `json:"descriptor"`
	}
	readJSON(t, filepath.Join(repoRoot(t), fixtureReport), &report)
	var lock struct {
		ArchiveSHA256 string `json:"archive_sha256"`
		ScannerImage  string `json:"scanner_image"`
	}
	readJSON(t, filepath.Join(repoRoot(t), "scripts/release/vulndb.lock.json"), &lock)

	if report.Descriptor.Name != "grype" || report.Descriptor.Version == "" {
		t.Fatalf("the fixture does not name its scanner: %q %q",
			report.Descriptor.Name, report.Descriptor.Version)
	}
	// The binding that matters is to the DB, not to the image string: an image is pinned by digest,
	// which carries no version text to compare against. TestGeneratorAndScannerArePinnedByDigest
	// covers the image pin; this asserts the fixture was scanned against the LOCKED snapshot.
	if !strings.Contains(report.Descriptor.DB.Status.From, lock.ArchiveSHA256) {
		t.Errorf("the fixture was scanned against a DB that is not the locked snapshot %s:\n  %s",
			lock.ArchiveSHA256, report.Descriptor.DB.Status.From)
	}
	if !report.Descriptor.DB.Status.Valid {
		t.Error("the fixture's scanner reported an invalid DB")
	}
	if _, err := os.Stat(filepath.Join(repoRoot(t), "scripts/release/testdata/vulnerable-fixture/requirements.txt")); err != nil {
		t.Errorf("the fixture's SOURCE is missing, so the report cannot be regenerated: %v", err)
	}
}

// --- the live leg -----------------------------------------------------------------------------

// TestSBOMPipelineLive runs the REAL pipeline — pinned syft + pinned grype over a real build — when
// Docker and the hydrated DB snapshot are both present. It SKIPS loudly otherwise: the ~2GB DB is
// not committed, so this cannot be a default-on gate, and everything above is designed to prove the
// mechanism without it.
func TestSBOMPipelineLive(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	root := repoRoot(t)
	db := os.Getenv("PALAI_VULN_DB_DIR")
	if db == "" {
		db = filepath.Join(root, "dist/vulndb")
	}
	var lock struct {
		DBSchema string `json:"db_schema"`
	}
	readJSON(t, filepath.Join(root, "scripts/release/vulndb.lock.json"), &lock)
	if _, err := os.Stat(filepath.Join(db, lock.DBSchema, "import.json")); err != nil {
		t.Skipf("the pinned vulnerability DB is not hydrated at %s — run "+
			"`scripts/release/sbom.sh --hydrate-db` (the only networked step, ~2GB)", db)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	out := t.TempDir()
	build := exec.Command("/usr/bin/env", "bash", filepath.Join(root, "scripts/release/build.sh"),
		"--no-images", "--out", out, "--cli-targets", "linux/arm64", "--runner-archs", "arm64")
	build.Dir = root
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build.sh: %v\n%s", err, b)
	}
	before := listFiles(t, out)

	if got, code := runSBOM(t, "--dir", out); code != 0 {
		t.Fatalf("the live SBOM run failed (exit %d):\n%s", code, got)
	}
	if got, code := verifyDir(t, out); code != 0 {
		t.Fatalf("the live dir failed its own verification (exit %d):\n%s", code, got)
	}

	// Both formats, for every artifact, with a digest that re-hashes.
	var index struct {
		Artifacts []struct {
			File string `json:"file"`
			SBOM struct {
				SPDX, CycloneDX string
			} `json:"sbom"`
		} `json:"artifacts"`
		SBOM struct {
			VulnerabilityScan struct {
				Scanned bool   `json:"scanned"`
				Result  string `json:"result"`
				DB      struct {
					SnapshotDate string `json:"snapshot_date"`
				} `json:"db"`
			} `json:"vulnerability_scan"`
		} `json:"sbom"`
	}
	readJSON(t, filepath.Join(out, "release-index.json"), &index)
	for _, a := range index.Artifacts {
		for _, rel := range []string{a.SBOM.SPDX, a.SBOM.CycloneDX} {
			if rel == "" {
				t.Fatalf("%s: the index names no SBOM for one of the two formats", a.File)
			}
			if _, err := os.Stat(filepath.Join(out, rel)); err != nil {
				t.Fatalf("%s: %v", a.File, err)
			}
		}
	}
	if !index.SBOM.VulnerabilityScan.Scanned || index.SBOM.VulnerabilityScan.DB.SnapshotDate == "" {
		t.Fatalf("the live run recorded no dated DB snapshot: %+v", index.SBOM.VulnerabilityScan)
	}

	// CONTAINMENT — the contract with T3/T4. sbom.sh must add files ONLY under sbom/ (and patch
	// release-index.json in place). Everything it writes therefore lands before the attestation in
	// a directory provenance.sh globs for byproducts and hashes into the signed sha256sums root; a
	// stray file anywhere else would become an unsigned rider the hardened verifier rejects.
	for f := range listFiles(t, out) {
		if before[f] || strings.HasPrefix(f, "sbom/") || f == "release-index.json" {
			continue
		}
		t.Errorf("sbom.sh wrote %s outside sbom/ — T3 binds the release dir as it finds it", f)
	}

	// The secret-scan gate covers the surfaces this run created, and it decompresses first
	// (TestSecretScanIsNotVacuous proves the walk is not a no-op on compressed members).
	hits, scanned := scanForSecrets(t, out, secretNeedles(t))
	if len(hits) > 0 {
		t.Fatalf("secret material in the release dir: %v", hits)
	}
	if scanned < len(index.Artifacts) {
		t.Fatalf("the secret scan only looked at %d files", scanned)
	}
	fmt.Fprintf(os.Stderr, "live: %d artifacts, scan %s against snapshot %s\n",
		len(index.Artifacts), index.SBOM.VulnerabilityScan.Result,
		index.SBOM.VulnerabilityScan.DB.SnapshotDate)
}

// --- the secret-scan gate over the new surfaces -------------------------------------------------

// secretNeedles is what must never appear in an SBOM, a scanner report or a license inventory: the
// live provider credentials from .env.local (when present) plus the generic shapes a leaked key
// takes. SBOM generation walks INSIDE our artifacts, so a credential that ever got packaged would
// surface here as a file path, a package name, or a captured string.
func secretNeedles(t *testing.T) [][]byte {
	t.Helper()
	needles := [][]byte{
		[]byte("BEGIN EC PRIVATE KEY"), []byte("BEGIN PRIVATE KEY"),
		[]byte("sk-ant-"), []byte("sk-proj-"), []byte("AKIA"),
	}
	b, err := os.ReadFile(filepath.Join(repoRoot(t), ".env.local"))
	if err != nil {
		return needles
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || len(v) < 20 || !strings.ContainsAny(k, "KEYTOKENSECRET") {
			continue
		}
		needles = append(needles, []byte(strings.Trim(v, `"'`)))
	}
	return needles
}

// scanForSecrets walks every file under dir and RETURNS the hits (so a test can assert either that
// there are none, or — for the vacuity guard below — that there is one). A compressed member is
// scanned AFTER decompression: a raw-byte scan of a gzip stream can never fail, because deflate
// bit-packs literals and a plaintext secret does not survive as a substring (E14 T7's lesson).
func scanForSecrets(t *testing.T, dir string, needles [][]byte) (hits []string, scanned int) {
	t.Helper()
	check := func(where string, body []byte) {
		for _, n := range needles {
			if bytes.Contains(body, n) {
				hits = append(hits, fmt.Sprintf("%s (needle %q…)", where, string(n[:min(8, len(n))])))
			}
		}
	}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if !strings.HasSuffix(path, ".gz") && !strings.HasSuffix(path, ".tgz") {
			check(path, body)
			return nil
		}
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			check(path, body)
			return nil
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil { // not a tar inside the gzip — scan the inflated bytes whole
				inflated, _ := io.ReadAll(gz)
				check(path+" (inflated)", inflated)
				return nil
			}
			member, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			scanned++
			check(path+"!"+h.Name, member)
		}
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return hits, scanned
}

// TestSecretScanIsNotVacuous is the guard on the guard: it plants a canary inside a gzip'd tar and
// proves the RAW bytes do not contain it (so a naive scan would pass) while scanForSecrets does.
func TestSecretScanIsNotVacuous(t *testing.T) {
	canary := []byte("sk-ant-CANARY-000000000000000000")
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	body := append(append([]byte("token: "), canary...), '\n')
	if err := tw.WriteHeader(&tar.Header{Name: "leaked.txt", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	zw.Close()
	if bytes.Contains(buf.Bytes(), canary) {
		t.Skip("this gzip stream happens to keep the canary as a literal substring; the point of " +
			"the test is the case where it does not")
	}
	path := filepath.Join(dir, "packed.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	// A raw scan of the compressed bytes finds nothing — that is the vacuous scan.
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, canary) {
		t.Fatal("precondition failed: the canary survived compression as a literal")
	}
	// scanForSecrets decompresses first, so it must catch exactly what the raw scan missed.
	hits, _ := scanForSecrets(t, dir, [][]byte{canary})
	if len(hits) != 1 {
		t.Fatalf("scanForSecrets found %d hits inside the gzip member, want 1 — the scan is vacuous: %v",
			len(hits), hits)
	}
}

// listFiles returns every file under dir as a set of slash-separated relative paths.
func listFiles(t *testing.T, dir string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		found[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return found
}
