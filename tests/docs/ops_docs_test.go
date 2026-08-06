// The operations-side E18 T9 guards: the runbooks, the support matrix and the RC triage table.
// Same rule as the threat model — a document that CLAIMS something about the tree is checked against
// the tree, not trusted.
package docs_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	runbookDir   = "docs/operations/runbooks"
	matrixJSON   = "docs/operations/support-matrix.json"
	matrixMD     = "docs/operations/support-matrix.md"
	knownGapsDoc = "docs/operations/known-gaps-1.0.md"
	recorder     = "scripts/ops/record-runbook-transcripts.sh"
)

// makeTargets reads the Makefile's target names once. It is the third proof namespace: several
// surfaces are proven by a Docker-bound drill script rather than by a `go test`, and pretending
// otherwise would push a doc into citing a test that does not cover what the drill covers.
func makeTargets(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	targets := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, ok := strings.Cut(line, ":")
		if !ok || name == "" || strings.ContainsAny(name, " \t=") {
			continue
		}
		targets[name] = true
	}
	if !targets["verify"] {
		t.Fatal("Makefile parse found no `verify` target — the resolver is broken, not the docs")
	}
	return targets
}

// resolveProof extends resolveEvidence with `make:<target>`. Used by the support matrix and by the
// runbooks' drill delegations; the threat model deliberately does NOT accept it, because "a make
// target exists" is not the same claim as "an assertion passes".
func resolveProof(t *testing.T, id string) string {
	t.Helper()
	if target, ok := strings.CutPrefix(id, "make:"); ok {
		if !makeTargets(t)[target] {
			return "names `make " + target + "` which is not a target in the Makefile"
		}
		return ""
	}
	return resolveEvidence(t, id)
}

// --- runbooks ---------------------------------------------------------------------------------------

func runbookFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoRoot(t), runbookDir))
	if err != nil {
		t.Fatalf("read %s: %v", runbookDir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && e.Name() != "README.md" {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// allTranscripts concatenates the recorded transcripts. The claim these guards enforce is the one the
// task actually makes — "every command in every runbook was EXECUTED once against a running stack" —
// so a command is satisfied by appearing in any transcript of the recorded session, not necessarily
// in the one filed beside its own runbook (§5 of one runbook legitimately re-uses a command §1 of
// another already ran).
func allTranscripts(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), runbookDir, "transcripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read transcripts: %v", err)
	}
	var b strings.Builder
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".txt") {
			c, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			b.Write(c)
		}
	}
	return b.String()
}

var (
	fenceOpen   = regexp.MustCompile("^```(sh|bash)\\s*$")
	fenceClose  = regexp.MustCompile("^```\\s*$")
	drillMarker = regexp.MustCompile(`^<!--\s*drill:\s*([a-z0-9-]+)\s*-->\s*$`)
)

// runbookBlocks walks a runbook and returns its command lines, split into the ones that must have been
// executed and the make targets a drill delegation names.
func runbookBlocks(doc string) (commands []string, drills []string) {
	lines := strings.Split(doc, "\n")
	inFence, pendingDrill := false, ""
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !inFence {
			if m := drillMarker.FindStringSubmatch(line); m != nil {
				pendingDrill = m[1]
				continue
			}
			if fenceOpen.MatchString(line) {
				inFence = true
				continue
			}
			continue
		}
		if fenceClose.MatchString(line) {
			inFence, pendingDrill = false, ""
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if pendingDrill != "" {
			drills = append(drills, pendingDrill)
			continue // the delegated block is the drill invocation, not a command we ran here
		}
		commands = append(commands, line)
	}
	return commands, drills
}

// TestRunbookCommandsWereExecuted is the anti-paper-runbook guard. Every command line a runbook tells
// an operator to type must appear in a recorded transcript, and a block delegated with
// `<!-- drill: <target> -->` must name a make target that exists. A command cannot be added to a
// runbook without being run, and a delegation cannot point at a drill that does not exist.
func TestRunbookCommandsWereExecuted(t *testing.T) {
	transcripts := allTranscripts(t)
	if len(transcripts) < 2000 {
		t.Fatalf("the recorded transcripts total %d bytes — that is not a recording of five runbooks", len(transcripts))
	}
	files := runbookFiles(t)
	if len(files) < 5 {
		t.Fatalf("expected at least 5 runbooks, found %d: %v", len(files), files)
	}
	total := 0
	for _, name := range files {
		doc := readDoc(t, filepath.Join(runbookDir, name))
		commands, drills := runbookBlocks(doc)
		if len(commands)+len(drills) == 0 {
			t.Errorf("%s/%s contains no commands at all — a runbook that tells nobody to do anything", runbookDir, name)
		}
		for _, cmd := range commands {
			total++
			if !strings.Contains(transcripts, "$ "+cmd) {
				t.Errorf("%s/%s tells an operator to run:\n    %s\n  ...which appears in NO recorded transcript. "+
					"Run it (%s) or delete it: a command nobody has executed is a guess.", runbookDir, name, cmd, recorder)
			}
		}
		for _, target := range drills {
			if why := resolveProof(t, "make:"+target); why != "" {
				t.Errorf("%s/%s delegates to a drill that %s", runbookDir, name, why)
			}
		}
	}
	if total < 25 {
		t.Errorf("only %d runbook commands checked — the block parser or the runbooks shrank", total)
	}
}

// TestRunbookGuardBites proves the executed-commands check can actually fail: a command nobody ran
// must not be found in the transcripts. Without this, a broken parser would silently pass everything.
func TestRunbookGuardBites(t *testing.T) {
	transcripts := allTranscripts(t)
	for _, fake := range []string{
		"palai definitely-not-a-command --fabricated",
		"kubectl delete namespace palai",
	} {
		if strings.Contains(transcripts, "$ "+fake) {
			t.Errorf("the transcripts contain %q, so the executed-commands check cannot distinguish run from unrun", fake)
		}
	}
	// The parser must actually find commands in a real runbook; an empty result would pass vacuously.
	doc := readDoc(t, filepath.Join(runbookDir, "incident-response.md"))
	cmds, _ := runbookBlocks(doc)
	if len(cmds) < 5 {
		t.Errorf("the block parser found %d commands in incident-response.md — it is not parsing", len(cmds))
	}
}

// TestEveryRunbookIsIndexedAndHasATranscript keeps the set closed: the README lists every runbook, and
// every runbook has a non-empty transcript filed beside it.
func TestEveryRunbookIsIndexedAndHasATranscript(t *testing.T) {
	readme := readDoc(t, filepath.Join(runbookDir, "README.md"))
	root := repoRoot(t)
	for _, name := range runbookFiles(t) {
		if !strings.Contains(readme, "("+"./"+name+")") {
			t.Errorf("%s/%s is not listed in the runbooks README", runbookDir, name)
		}
		transcript := filepath.Join(root, runbookDir, "transcripts", strings.TrimSuffix(name, ".md")+".txt")
		info, err := os.Stat(transcript)
		if err != nil {
			t.Errorf("%s has no transcript at %s: %v", name, transcript, err)
			continue
		}
		if info.Size() < 400 {
			t.Errorf("%s's transcript is %d bytes — that is a stub, not a recording", name, info.Size())
		}
	}
}

// TestRunbookTranscriptsCarryNoCredential is the redaction check on committed output. The recorder
// prints an API key exactly once by design; if its redaction ever regresses, a real credential lands
// in git.
func TestRunbookTranscriptsCarryNoCredential(t *testing.T) {
	leaky := []*regexp.Regexp{
		regexp.MustCompile(`sk_[0-9a-f]{16,}`),
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		regexp.MustCompile(`(?i)password=[^<\s"]+`),
	}
	root := repoRoot(t)
	dir := filepath.Join(root, runbookDir, "transcripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read transcripts: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range leaky {
			if m := re.Find(b); m != nil {
				t.Errorf("transcript %s contains credential-shaped text %q — the recorder's redaction regressed", e.Name(), m)
			}
		}
	}
}

// --- the support matrix -------------------------------------------------------------------------------

type supportMatrix struct {
	Symbols   map[string]string `json:"symbols"`
	Levels    map[string]string `json:"levels"`
	Platforms []string          `json:"platforms"`
	Surfaces  []struct{ ID, Label string }
	Cells     []struct {
		Surface  string   `json:"surface"`
		Platform string   `json:"platform"`
		Level    string   `json:"level"`
		Proof    []string `json:"proof"`
	} `json:"cells"`
}

func loadSupportMatrix(t *testing.T) supportMatrix {
	t.Helper()
	var m supportMatrix
	if err := json.Unmarshal([]byte(readDoc(t, matrixJSON)), &m); err != nil {
		t.Fatalf("decode %s: %v", matrixJSON, err)
	}
	// Surfaces has anonymous-struct field tags Go cannot infer from the shorthand above.
	var raw struct {
		Surfaces []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"surfaces"`
	}
	if err := json.Unmarshal([]byte(readDoc(t, matrixJSON)), &raw); err != nil {
		t.Fatalf("decode surfaces: %v", err)
	}
	m.Surfaces = make([]struct{ ID, Label string }, len(raw.Surfaces))
	for i, s := range raw.Surfaces {
		m.Surfaces[i] = struct{ ID, Label string }{s.ID, s.Label}
	}
	return m
}

// TestSupportMatrixOnlyClaimsWhatIsProven: a cell that claims coverage must cite proof that resolves,
// a cell that claims none must cite no proof, and no (surface, platform) pair may be missing. The
// last one matters most — a matrix quietly drops the cell it cannot fill.
func TestSupportMatrixOnlyClaimsWhatIsProven(t *testing.T) {
	m := loadSupportMatrix(t)
	if len(m.Platforms) == 0 || len(m.Surfaces) == 0 {
		t.Fatal("support matrix declares no platforms or no surfaces")
	}
	seen := map[string]bool{}
	for _, c := range m.Cells {
		key := c.Surface + "\x00" + c.Platform
		if seen[key] {
			t.Errorf("duplicate cell %s / %s", c.Surface, c.Platform)
		}
		seen[key] = true

		if _, ok := m.Levels[c.Level]; !ok {
			t.Errorf("cell %s / %s has level %q which is not in the declared level set", c.Surface, c.Platform, c.Level)
			continue
		}
		switch c.Level {
		case "tested", "build-only":
			if len(c.Proof) == 0 {
				t.Errorf("cell %s / %s claims %q with NO proof — only a tested cell is filled", c.Surface, c.Platform, c.Level)
			}
			for _, p := range c.Proof {
				if why := resolveProof(t, p); why != "" {
					t.Errorf("cell %s / %s cites %q which %s", c.Surface, c.Platform, p, why)
				}
			}
		default:
			if len(c.Proof) > 0 {
				t.Errorf("cell %s / %s claims %q yet cites proof %v — a cell cannot deny and claim at once",
					c.Surface, c.Platform, c.Level, c.Proof)
			}
		}
	}
	for _, s := range m.Surfaces {
		for _, p := range m.Platforms {
			if !seen[s.ID+"\x00"+p] {
				t.Errorf("the matrix has no cell for %s / %s — an omitted cell is a silent \"not applicable\"", s.ID, p)
			}
		}
	}
}

// TestSupportMatrixMarkdownRendersTheJSON diffs the rendered table against the authoritative JSON,
// cell by cell. The prose table is what a human reads; if it can drift from the machine-readable
// source, the guard above protects the wrong file.
func TestSupportMatrixMarkdownRendersTheJSON(t *testing.T) {
	m := loadSupportMatrix(t)
	label := map[string]string{}
	for _, s := range m.Surfaces {
		label[s.ID] = s.Label
	}
	// Find the rendered rows by their first cell (the surface label).
	rows := map[string]tableRow{}
	for _, r := range parseTables(readDoc(t, matrixMD)) {
		if _, ok := r.cells["linux/amd64"]; ok {
			rows[r.first] = r
		}
	}
	for _, c := range m.Cells {
		want, ok := m.Symbols[c.Level]
		if !ok {
			t.Errorf("level %q has no rendering symbol", c.Level)
			continue
		}
		row, ok := rows[label[c.Surface]]
		if !ok {
			t.Errorf("%s renders no row for surface %q", matrixMD, label[c.Surface])
			continue
		}
		if got := row.cells[c.Platform]; got != want {
			t.Errorf("%s row %q column %s renders %q but the JSON says %q (level %q)",
				matrixMD, label[c.Surface], c.Platform, got, want, c.Level)
		}
	}
}

// --- the RC triage table ------------------------------------------------------------------------------

var (
	rcBlockerLine = regexp.MustCompile(`(?m)^RC-BLOCKERS:\s*(\d+)\s*$`)
	expiryDate    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	validDecision = map[string]bool{
		"RC-blocker": true, "post-1.0": true, "post-1.0 hardening": true,
		"§6-leg": true, "closed-verified": true,
	}
)

// gapRows returns the triage rows — those tables that carry both a decision and an owner.
func gapRows(t *testing.T) []tableRow {
	t.Helper()
	var out []tableRow
	for _, r := range parseTables(readDoc(t, knownGapsDoc)) {
		if _, hasDecision := r.cells["decision"]; !hasDecision {
			continue
		}
		if _, hasOwner := r.cells["owner"]; !hasOwner {
			continue
		}
		out = append(out, r)
	}
	return out
}

// TestKnownGapsEveryRowIsDecidedAndOwned: no undecided row, no unowned row, no invented decision.
// An open finding with no owner is how a finding becomes a surprise.
func TestKnownGapsEveryRowIsDecidedAndOwned(t *testing.T) {
	rows := gapRows(t)
	if len(rows) < 20 {
		t.Fatalf("the triage table has %d decided rows — the parser or the table shrank", len(rows))
	}
	for _, r := range rows {
		decision := strings.TrimSpace(strings.Trim(r.cells["decision"], "*` "))
		if !validDecision[decision] {
			t.Errorf("%s:%d row %q has decision %q — must be one of RC-blocker / post-1.0 / post-1.0 hardening / §6-leg / closed-verified",
				knownGapsDoc, r.line, r.first, decision)
		}
		if strings.TrimSpace(r.cells["owner"]) == "" || r.cells["owner"] == "—" {
			t.Errorf("%s:%d row %q has no owner", knownGapsDoc, r.line, r.first)
		}
		if expiry, ok := r.cells["expiry"]; ok {
			e := strings.TrimSpace(expiry)
			if e != "—" && e != "" && !expiryDate.MatchString(e) {
				t.Errorf("%s:%d row %q has expiry %q — a time-bound waiver needs a YYYY-MM-DD date (§62.2 P2)",
					knownGapsDoc, r.line, r.first, e)
			}
		}
	}
}

// TestKnownGapsRCBlockerCountIsAccurate keeps the machine-readable line the E18 T10 gate reads equal
// to the table. If these could drift, the gate would read a comforting zero off a table with an open
// blocker in it — which is exactly the failure the count exists to prevent.
func TestKnownGapsRCBlockerCountIsAccurate(t *testing.T) {
	doc := readDoc(t, knownGapsDoc)
	m := rcBlockerLine.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("%s carries no `RC-BLOCKERS: <n>` line — the T10 gate has nothing to read", knownGapsDoc)
	}
	declared := m[1]

	actual := 0
	for _, r := range gapRows(t) {
		if strings.TrimSpace(strings.Trim(r.cells["decision"], "*` ")) == "RC-blocker" {
			actual++
		}
	}
	if declared != itoa(actual) {
		t.Errorf("%s declares `RC-BLOCKERS: %s` but the table contains %d RC-blocker row(s)", knownGapsDoc, declared, actual)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestKnownGapsClosedRowsCiteRealEvidence: a row cannot be closed by editing its decision column. A
// `closed-verified` row must cite at least one id that resolves against the tree.
func TestKnownGapsClosedRowsCiteRealEvidence(t *testing.T) {
	closed := 0
	for _, r := range gapRows(t) {
		if strings.TrimSpace(strings.Trim(r.cells["decision"], "*` ")) != "closed-verified" {
			continue
		}
		closed++
		cell := r.cells["evidence / where it goes"]
		var resolved int
		for _, m := range backticked.FindAllStringSubmatch(cell, -1) {
			if resolveProof(t, m[1]) == "" {
				resolved++
			}
		}
		if resolved == 0 {
			t.Errorf("%s:%d row %q is closed-verified but cites nothing that resolves against the tree",
				knownGapsDoc, r.line, r.first)
		}
	}
	if closed == 0 {
		t.Error("no closed-verified rows were checked — the parser is not reading the decision column")
	}
}

// TestKnownGapsNamesItsOwner enforces the plan's honest-naming rule: these are one session owner's
// decisions and must read that way, not as an anonymous verdict.
func TestKnownGapsNamesItsOwner(t *testing.T) {
	doc := readDoc(t, knownGapsDoc)
	for _, want := range []string{"repository owner", "2026-07-25"} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s does not carry %q — a disposition table with no name and no date on it is a way of not deciding",
				knownGapsDoc, want)
		}
	}
}

// TestNoTriageRowLosesCellsToAnUnescapedPipe is the guard that would have caught the way FLT-P15 hid.
//
// ‼️ EVERY GUARD IN THIS FILE READS A ROW BY ITS HEADER — decision, owner, evidence — and parseTables maps
// only the cells a header names. A row with MORE cells than the header therefore loses its tail SILENTLY:
// the checks pass because they never see the missing columns, and the rendered page shows an operator a
// row spilling into columns that do not exist. Measured 2026-08-06: three rows in this table were ragged.
// One of them, FLT-P15 — the fleet's largest ceiling — was glued onto FLT-P13's row by a stray `||`, so
// its decision and owner had never been checked by TestKnownGapsEveryRowIsDecidedAndOwned at all. The
// other two carried a literal pipe inside prose (a status enumeration, a shell pipeline) that Markdown
// read as a column break.
//
// A literal pipe in a cell is written `\|`. That is the whole rule.
func TestNoTriageRowLosesCellsToAnUnescapedPipe(t *testing.T) {
	doc := readDoc(t, knownGapsDoc)
	lines := strings.Split(doc, "\n")

	var headers int
	var checked int
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			headers = 0
			continue
		}
		cells := splitRow(trimmed)
		if isSeparator(cells) {
			continue
		}
		if headers == 0 {
			if i+1 < len(lines) && isSeparator(splitRow(lines[i+1])) {
				headers = len(cells)
			}
			continue
		}
		checked++
		if len(cells) != headers {
			t.Errorf("%s:%d has %d cells but its header has %d — the extra ones are dropped by every guard "+
				"that reads this table by header, and the page renders them as columns that do not exist. "+
				"A literal pipe inside a cell is written \\|. Row starts: %.60s",
				knownGapsDoc, i+1, len(cells), headers, cells[0])
		}
	}
	if checked < 20 {
		t.Fatalf("only %d rows were checked — the parser or the table shrank, and a guard that reads nothing "+
			"reports no defects", checked)
	}
}
