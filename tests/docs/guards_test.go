// Package docs_test holds the E18 T9 documentation guards. These docs make CLAIMS about the tree —
// that a mitigation is enforced, that a support cell is tested, that a finding is closed — and a
// markdown file cannot be trusted to stay true on its own. Each guard here RESOLVES the doc's claims
// against the repository, so a doc that drifts into an untested claim turns `go test ./...` red.
//
// The package is deliberately UNTAGGED and Docker-free: it rides `make verify` (test-unit runs
// `go test ./...`), which is the only tier that runs on every change.
package docs_test

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// repoRoot resolves the worktree root once. Every guard reads real committed files by absolute path.
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func readDoc(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// --- the two resolvable evidence namespaces -------------------------------------------------------

var (
	uatOnce  sync.Once
	uatCases map[string]bool

	testOnce  sync.Once
	testFuncs map[string]bool
)

// uatCaseIDs is the set of materialized UAT cases (a directory with a case.yaml under tests/uat/cases).
func uatCaseIDs(t *testing.T) map[string]bool {
	t.Helper()
	uatOnce.Do(func() {
		uatCases = map[string]bool{}
		dir := filepath.Join(repoRoot(t), "tests", "uat", "cases")
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read UAT cases: %v", err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "case.yaml")); err == nil {
				uatCases[e.Name()] = true
			}
		}
	})
	if len(uatCases) == 0 {
		t.Fatal("no UAT cases found — the resolver is broken, not the docs")
	}
	return uatCases
}

var funcDecl = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)

// goTestFuncs is the set of Go test function names in the tree, tags included: a guard that only saw
// untagged files would reject a perfectly real `//go:build security` proof. Walk, don't `go list` —
// build tags are exactly what we must NOT filter on.
func goTestFuncs(t *testing.T) map[string]bool {
	t.Helper()
	testOnce.Do(func() {
		testFuncs = map[string]bool{}
		root := repoRoot(t)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", ".palai", "vendor", ".claude":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range funcDecl.FindAllStringSubmatch(string(b), -1) {
				testFuncs[m[1]] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk for test functions: %v", err)
		}
	})
	if len(testFuncs) == 0 {
		t.Fatal("no Go test functions found — the resolver is broken, not the docs")
	}
	return testFuncs
}

var (
	uatIDShape  = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,4}-[0-9]{3}$`)
	testIDShape = regexp.MustCompile(`^Test[A-Za-z0-9_]+$`)
	backticked  = regexp.MustCompile("`([^`]+)`")
)

// resolveEvidence checks one evidence ID and returns "" when it resolves, or the reason it does not.
func resolveEvidence(t *testing.T, id string) string {
	t.Helper()
	switch {
	case uatIDShape.MatchString(id):
		if !uatCaseIDs(t)[id] {
			return "looks like a UAT id but tests/uat/cases/" + id + "/case.yaml does not exist"
		}
	case testIDShape.MatchString(id):
		if !goTestFuncs(t)[id] {
			return "looks like a Go test but no `func " + id + "(` exists in any *_test.go"
		}
	default:
		return "is neither a UAT case id (SAN-004) nor a Go test name (TestFoo) — an evidence cell may not contain prose"
	}
	return ""
}

// --- markdown table parsing -----------------------------------------------------------------------

type tableRow struct {
	line  int
	cells map[string]string // header (lower-cased) -> cell
	first string            // the first cell, used to name the row in a failure
}

var separatorCell = regexp.MustCompile(`^:?-{2,}:?$`)

// splitRow splits a Markdown table row the way a RENDERER does: on a pipe that is not escaped.
//
// ‼️ IT USED TO SPLIT ON EVERY PIPE, SO THIS PARSER AND THE PAGE AN OPERATOR READS WERE DIFFERENT
// DOCUMENTS. A cell containing a literal pipe — a status enumeration, a shell pipeline — renders as one
// cell and parsed as several, which pushes every later cell one column left: the guards in this package
// read `decision`, `owner` and `evidence` BY HEADER, so they would silently check the wrong text, or
// none. Measured 2026-08-06 in known-gaps-1.0.md. The escape is written `\|` and the cell keeps the
// literal pipe, because what the guard compares must be what the reader sees.
var unescapedPipe = regexp.MustCompile(`(^|[^\\])\|`)

func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	// Mark the unescaped pipes, split on the marker, then restore every escaped one as the literal pipe it
	// renders as. \x00 cannot occur in a source document.
	marked := unescapedPipe.ReplaceAllString(line, "${1}\x00")
	parts := strings.Split(marked, "\x00")
	for i := range parts {
		parts[i] = strings.TrimSpace(strings.ReplaceAll(parts[i], `\|`, "|"))
	}
	return parts
}

func isSeparator(cells []string) bool {
	for _, c := range cells {
		if !separatorCell.MatchString(c) {
			return false
		}
	}
	return len(cells) > 0
}

// parseTables returns every data row of every pipe table in doc, keyed by lower-cased header.
func parseTables(doc string) []tableRow {
	var rows []tableRow
	lines := []string{}
	sc := bufio.NewScanner(strings.NewReader(doc))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	var headers []string
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "|") {
			headers = nil
			continue
		}
		cells := splitRow(line)
		if isSeparator(cells) {
			continue
		}
		if headers == nil {
			// A header row is one whose NEXT line is the separator.
			if i+1 < len(lines) && isSeparator(splitRow(lines[i+1])) {
				headers = make([]string, len(cells))
				for j, c := range cells {
					headers[j] = strings.ToLower(c)
				}
			}
			continue
		}
		row := tableRow{line: i + 1, cells: map[string]string{}}
		if len(cells) > 0 {
			row.first = cells[0]
		}
		for j, c := range cells {
			if j < len(headers) {
				row.cells[headers[j]] = c
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// --- the threat model -----------------------------------------------------------------------------

const threatModelPath = "docs/security/threat-model.md"

// TestThreatModelEvidenceResolves is the guard the threat model exists for: EVERY mitigation row
// either cites evidence that resolves against this tree, or says the bare words "not claimed". There
// is no third option — prose in an evidence cell fails here, which is what stops a row from claiming a
// control the code does not have by writing something plausible-sounding in the proof column.
func TestThreatModelEvidenceResolves(t *testing.T) {
	doc := readDoc(t, threatModelPath)
	rows := parseTables(doc)

	checked := 0
	for _, row := range rows {
		cell, ok := row.cells["evidence"]
		if !ok {
			continue // a table with no evidence axis is not making a mitigation claim
		}
		checked++
		if cell == "not claimed" {
			// A not-claimed row must not ALSO cite evidence: that would be a claim wearing a denial.
			if backticked.MatchString(cell) {
				t.Errorf("%s:%d row %q says \"not claimed\" and still cites evidence", threatModelPath, row.line, row.first)
			}
			continue
		}
		ids := backticked.FindAllStringSubmatch(cell, -1)
		if len(ids) == 0 {
			t.Errorf("%s:%d row %q has an evidence cell with no backticked id and does not say \"not claimed\": %q",
				threatModelPath, row.line, row.first, cell)
			continue
		}
		// Nothing but ids and separators may live in an evidence cell.
		residue := strings.TrimSpace(backticked.ReplaceAllString(cell, ""))
		residue = strings.Trim(residue, " ,;/—-")
		if residue != "" {
			t.Errorf("%s:%d row %q smuggles prose into the evidence cell: %q", threatModelPath, row.line, row.first, residue)
		}
		for _, m := range ids {
			if why := resolveEvidence(t, m[1]); why != "" {
				t.Errorf("%s:%d row %q cites %q which %s", threatModelPath, row.line, row.first, m[1], why)
			}
		}
	}
	if checked < 80 {
		t.Errorf("only %d evidence-bearing rows parsed from %s — the parser or the document shrank", checked, threatModelPath)
	}
}

// TestThreatModelCoversEverySpecSection stops the document from quietly dropping a §49 subsection.
// A threat model that omits a section is not a smaller threat model, it is a silent "not applicable".
func TestThreatModelCoversEverySpecSection(t *testing.T) {
	doc := readDoc(t, threatModelPath)
	for _, section := range []string{
		"49.1", "49.2", "49.3", "49.4", "49.5", "49.6", "49.7", "49.8", "49.9",
		"49.10", "49.11", "49.12", "49.13", "49.14", "49.15", "49.16", "49.17",
	} {
		// The trailing boundary matters: a bare Contains("§49.1") is satisfied by "§49.17", which would
		// let the document drop §49.1 silently — the exact failure this guard exists to prevent.
		if !regexp.MustCompile(`§` + regexp.QuoteMeta(section) + `\D`).MatchString(doc) {
			t.Errorf("§%s is never modelled in %s — every spec section is either modelled or explicitly not claimed", section, threatModelPath)
		}
	}
}

// TestThreatModelGuardBites proves the guard is genuine rather than decorative: a fabricated row of
// each failing shape must be detected by the SAME resolution the guard runs. A guard that could not
// fail would rubber-stamp any threat model (the E16 honest-matrix precedent).
func TestThreatModelGuardBites(t *testing.T) {
	for _, tc := range []struct{ name, id string }{
		{"invented UAT id", "ZZZ-999"},
		{"invented test name", "TestThisProofDoesNotExistAnywhere"},
		{"prose", "enforced by design"},
	} {
		if why := resolveEvidence(t, tc.id); why == "" {
			t.Errorf("resolveEvidence accepted a fabricated %s (%q) — the guard is inert", tc.name, tc.id)
		}
	}
	// ... and a real id of each shape must still pass, or the guard is merely broken.
	for _, id := range []string{"SAN-004", "TestSandboxEscapeSuite"} {
		if why := resolveEvidence(t, id); why != "" {
			t.Errorf("resolveEvidence rejected the real id %q: %s", id, why)
		}
	}
}

// TestThreatModelIsNotACopyOfTheSpec enforces the review rule in the plan: a copy of the aspirational
// §49 text is an automatic reject. The mechanical form of "not a copy" is that the document's own
// prose must not reproduce §49's sentences. We sample the spec's distinctive normative lines and
// require the model not to contain them verbatim.
func TestThreatModelIsNotACopyOfTheSpec(t *testing.T) {
	doc := readDoc(t, threatModelPath)
	spec := readDoc(t, "MASTER-SPEC.md")

	start := strings.Index(spec, "## 49. Security architecture and threat model")
	end := strings.Index(spec, "## 50. Audit and security operations")
	if start < 0 || end <= start {
		t.Fatal("could not locate §49 in MASTER-SPEC.md")
	}
	var copied []string
	for _, line := range strings.Split(spec[start:end], "\n") {
		line = strings.TrimSpace(line)
		// Only long prose lines are interesting: a shared heading or a two-word bullet proves nothing.
		if len(line) < 70 || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "|") {
			continue
		}
		if strings.Contains(doc, line) {
			copied = append(copied, line)
		}
	}
	if len(copied) > 0 {
		t.Errorf("%s reproduces %d line(s) of MASTER-SPEC §49 verbatim — this document models the IMPLEMENTED "+
			"surface, it does not restate the spec. First: %q", threatModelPath, len(copied), copied[0])
	}
}

// --- the vulnerability process ---------------------------------------------------------------------

const (
	vulnProcessPath   = "docs/security/vulnerability-process.md"
	releasePolicyPath = "docs/security/release-policy.md"
)

// severityFromSpec extracts the §62.2 blocking-severity bullets from MASTER-SPEC — the ONE source.
func severityFromSpec(t *testing.T) []string {
	t.Helper()
	spec := readDoc(t, "MASTER-SPEC.md")
	start := strings.Index(spec, "### 62.2 Blocking severity")
	if start < 0 {
		t.Fatal("could not locate §62.2 in MASTER-SPEC.md")
	}
	end := strings.Index(spec[start:], "### 62.3")
	if end < 0 {
		t.Fatal("could not locate the end of §62.2")
	}
	var bullets []string
	for _, line := range strings.Split(spec[start:start+end], "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- P") {
			bullets = append(bullets, strings.TrimPrefix(line, "- "))
		}
	}
	if len(bullets) != 4 {
		t.Fatalf("expected 4 severity bullets in §62.2, got %d", len(bullets))
	}
	return bullets
}

// TestVulnerabilityProcessSeverityMatchesTheSpec binds the process document's severity ladder to
// §62.2 rather than to a hand-written paraphrase. Two severity ladders that drift apart is exactly the
// contradiction this task was asked to make impossible.
func TestVulnerabilityProcessSeverityMatchesTheSpec(t *testing.T) {
	doc := readDoc(t, vulnProcessPath)
	for _, bullet := range severityFromSpec(t) {
		if !strings.Contains(doc, bullet) {
			t.Errorf("%s does not carry §62.2 verbatim; missing: %q\n"+
				"  (the severity ladder has ONE source — quote the spec line, do not paraphrase it)", vulnProcessPath, bullet)
		}
	}
}

// TestVulnerabilityProcessDoesNotForkTheReleasePolicy is the contradiction guard. The revocation and
// two-person rules live in release-policy.md; this document must LINK to them, never restate them.
// A restated rule is a second policy source, and a second source is a future contradiction.
func TestVulnerabilityProcessDoesNotForkTheReleasePolicy(t *testing.T) {
	doc := readDoc(t, vulnProcessPath)
	policy := readDoc(t, releasePolicyPath)

	if !strings.Contains(doc, "release-policy.md") {
		t.Errorf("%s never links to %s — the revocation and promotion rules must point at their one source",
			vulnProcessPath, releasePolicyPath)
	}
	var copied []string
	for _, line := range strings.Split(policy, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 60 || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(doc, line) {
			copied = append(copied, line)
		}
	}
	if len(copied) > 0 {
		t.Errorf("%s restates %d line(s) of %s verbatim instead of linking to it — one policy, one source. First: %q",
			vulnProcessPath, len(copied), releasePolicyPath, copied[0])
	}
}

// --- links resolve, everywhere ----------------------------------------------------------------------

var mdLink = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)

// backtickedPath matches an inline `path/like/this.md` reference — the form these documents use far
// more often than a markdown link, and therefore the form a link guard must also resolve.
var backtickedPath = regexp.MustCompile("`((?:docs|tests|scripts|apps|packages|deploy|evidence|storage|adapters|engines|sdks|cmd)/[A-Za-z0-9_./*-]+)`")

// TestEveryDocReferenceResolves resolves both markdown links and inline backticked repo paths in every
// document this task owns. A runbook whose "see X" points at nothing is worse than no runbook: it is a
// runbook an operator follows into a dead end at 3am.
func TestEveryDocReferenceResolves(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range ownedDocs(t) {
		doc := readDoc(t, rel)
		dir := filepath.Dir(filepath.Join(root, rel))
		for _, m := range mdLink.FindAllStringSubmatch(doc, -1) {
			target := m[1]
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "#") {
				continue
			}
			target = strings.SplitN(target, "#", 2)[0]
			if target == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, target)); err != nil {
				t.Errorf("%s links to %q which does not exist", rel, m[1])
			}
		}
		for _, m := range backtickedPath.FindAllStringSubmatch(doc, -1) {
			target := m[1]
			if strings.ContainsAny(target, "*") { // a glob is a description, not a link
				continue
			}
			target = strings.SplitN(target, ":", 2)[0]
			if _, err := os.Stat(filepath.Join(root, target)); err != nil {
				t.Errorf("%s names the repo path %q which does not exist", rel, target)
			}
		}
	}
}

// ownedDocs is the set of documents E18 T9 owns and therefore guards.
func ownedDocs(t *testing.T) []string {
	t.Helper()
	docs := []string{
		threatModelPath,
		vulnProcessPath,
		"docs/operations/support-matrix.md",
		"docs/operations/known-gaps-1.0.md",
		// E24 T6: the fleet operator page. It is guarded for the reason the rows above are — it cites repo
		// paths and links to four sibling pages, and a runbook that names a file which has moved is a runbook
		// an operator follows into a dead end.
		"docs/operations/runner-fleet.md",
	}
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "docs", "operations", "runbooks"))
	if err != nil {
		t.Fatalf("read runbooks dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			docs = append(docs, filepath.Join("docs", "operations", "runbooks", e.Name()))
		}
	}
	return docs
}

// --- the two-planes architecture page ----------------------------------------------------------------

const twoPlanesPath = "docs/architecture/two-planes.md"

// TestTwoPlanesEvidenceResolves is the guard that keeps an architecture page from becoming the thing it
// was written to prevent.
//
// ‼️ THE PAGE EXISTS BECAUSE A READER COULD REACH TWO OPPOSITE WRONG CONCLUSIONS from this repository —
// that a remote Mac already worked end to end, or that remote execution was absent entirely (device plan
// T0). A page that answers that and is then never re-measured becomes a third wrong conclusion, stated
// more confidently than either. So every claim on it cites a Go test or a UAT case, and a citation that
// stops resolving fails HERE rather than misleading somebody a year from now.
//
// It counts what it checked: a page whose tables stopped parsing would otherwise report the same clean
// result as a page whose every citation holds.
func TestTwoPlanesEvidenceResolves(t *testing.T) {
	checked := 0
	for _, row := range parseTables(readDoc(t, twoPlanesPath)) {
		cell, ok := row.cells["evidence"]
		if !ok {
			continue
		}
		ids := backticked.FindAllStringSubmatch(cell, -1)
		if len(ids) == 0 {
			t.Errorf("%s:%d row %q cites no evidence — a claim on this page without one is prose",
				twoPlanesPath, row.line, row.first)
			continue
		}
		for _, m := range ids {
			checked++
			if why := resolveEvidence(t, m[1]); why != "" {
				t.Errorf("%s:%d row %q cites %q which %s", twoPlanesPath, row.line, row.first, m[1], why)
			}
		}
	}
	if checked < 15 {
		t.Fatalf("only %d citations were checked on %s — the tables stopped parsing, and a guard that reads "+
			"nothing reports no defects", checked, twoPlanesPath)
	}
}
