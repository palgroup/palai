//go:build component

package postgres

// EVERY COLUMN A QUERY READS HAS A QUERY THAT WRITES IT — the durable form of a defect this repository
// shipped twice in one week, and whose family known-gaps already counts at seven.
//
// ‼️ WHAT IT CATCHES, in the exact shape it arrived: `runner_pools.isolation_mode` shipped with 000007 —
// the column, its CHECK, `fleet.Store.Register`'s ErrIsolationUnsupported refusal and that refusal's
// journal entry — and NOTHING WROTE IT. `CreatePool` built its row without the field and `InsertRunnerPool`
// had no column for it, so a real, correct, tested refusal waited on a state no operator could produce. The
// five `slack_*` tables were the same shape from the other end: schema the cutover emptied and left behind.
// Both were found BY HAND on 2026-08-07, and no tier would have found either — a column nothing writes
// breaks nothing. It answers its zero value forever, and every guard, bill and decision taken on it is
// taken on nothing.
//
// THE SUBJECT IS THE SHIPPED SQL, and it can be, because this repository already binds itself to that:
// storage/embed.go's header says the SQL files are the single source of truth and Go code loads statements
// by NAME rather than re-declaring them. A column with no writer in storage/queries has no writer.
//
// ‼️ THREE THINGS WRITE A COLUMN WITHOUT ANY QUERY MENTIONING IT, and each produced a false positive on the
// first version of this sweep — ten of its eleven candidates were wrong:
//
//	GENERATED    — `chunk_revisions.fts`, a stored generated column.
//	IDENTITY     — `events.journal_id`, whose sequence is the writer. `is_generated` reads NEVER for an
//	               identity column; the question is `is_identity`, and asking only the first is how this
//	               sweep's last false positive survived.
//	DEFAULT      — but ONLY a default that is a real VALUE: `quarantined_at DEFAULT clock_timestamp()`,
//	               `schema_version DEFAULT 1`. A default that is the type's ZERO — `isolation_mode
//	               DEFAULT ''`, `strict_enrollment DEFAULT false` — is NOT a writer. It is the word UNSET
//	               spelled in DDL, and excluding those would have excluded the very column this test exists
//	               for. The first draft did exactly that, and its own control caught it.
//
// They are decided by ASKING THE DATABASE rather than by a name list, so a column that gains a default
// tomorrow stops being a subject without anybody editing this file.
//
// AND A CTE-FRONTED UPDATE IS A WRITER. `WITH v AS (SELECT …) UPDATE idempotency_records SET
// result_purged_at = …` opens with the word WITH, so a classifier that reads a statement's FIRST keyword
// files it as a read — which is what filed three live idempotency columns as unwritten. `classify` below
// never asks what KIND a statement is: it asks whether the COLUMN appears in an INSERT's column list or on
// the left of an assignment, which is what being written actually is.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/palgroup/palai/storage"
)

// queryBlock is one `-- name: X` statement with its COMMENT LINES REMOVED. Stripping them is not tidiness:
// this tree's comments say the words INSERT and UPDATE in prose constantly — the note above
// `InsertRunnerPool` says "zero INSERT/UPDATE statements" — and a sweep that reads them counts prose as code.
type queryBlock struct {
	name string
	body string
}

var (
	blockName     = regexp.MustCompile(`^--\s*name:\s*(\S+)`)
	insertColumns = regexp.MustCompile(`(?is)INSERT\s+INTO\s+\w+\s*\(([^)]*)\)`)
	// The SET clause and nothing else. `WHERE alpha = $1` is a COMPARISON, and counting it as an assignment
	// is a false NEGATIVE — the worst kind here, because a column that is only ever compared would read as
	// written and its missing writer would never be reported. The control below is what caught this.
	setClause      = regexp.MustCompile(`(?is)\bSET\b(.*?)(?:\bWHERE\b|\bFROM\b|\bRETURNING\b|$)`)
	assignedColumn = regexp.MustCompile(`(?m)\b(\w+)\s*=\s*[^=]`)
	anyWord        = regexp.MustCompile(`\b\w+\b`)
)

func splitBlocks(sql string) []queryBlock {
	var blocks []queryBlock
	name, body := "", &strings.Builder{}
	flush := func() {
		if name != "" {
			blocks = append(blocks, queryBlock{name: name, body: body.String()})
		}
		body.Reset()
	}
	for _, line := range strings.Split(sql, "\n") {
		if m := blockName.FindStringSubmatch(line); m != nil {
			flush()
			name = m[1]
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	flush()
	return blocks
}

// classify returns, for every word in the SQL, the named blocks that WRITE it and the ones that only
// mention it. ONE function, used by both the sweep and its control — a control exercising a second copy
// would prove a mechanism the sweep does not run, which is the shape this tree keeps paying for.
func classify(blocks []queryBlock) (written, mentioned map[string][]string) {
	written, mentioned = map[string][]string{}, map[string][]string{}
	for _, block := range blocks {
		writes := map[string]bool{}
		for _, group := range insertColumns.FindAllStringSubmatch(block.body, -1) {
			for _, field := range strings.Split(group[1], ",") {
				if field = strings.TrimSpace(field); field != "" {
					writes[field] = true
				}
			}
		}
		for _, clause := range setClause.FindAllStringSubmatch(block.body, -1) {
			for _, m := range assignedColumn.FindAllStringSubmatch(clause[1], -1) {
				writes[m[1]] = true
			}
		}
		for column := range writes {
			written[column] = append(written[column], block.name)
		}
		for _, word := range anyWord.FindAllString(block.body, -1) {
			if !writes[word] {
				mentioned[word] = append(mentioned[word], block.name)
			}
		}
	}
	return written, mentioned
}

func loadQueryBlocks(t *testing.T, root string) []queryBlock {
	t.Helper()
	dir := filepath.Join(root, "storage", "queries")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var blocks []queryBlock
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		blocks = append(blocks, splitBlocks(string(raw))...)
	}
	return blocks
}

// TestTheClassifierTellsAWriteFromAProjection is the control, and it runs on SYNTHETIC SQL on purpose.
//
// ‼️ THE FIRST DRAFT CONTROLLED ON runner_pools.isolation_mode, AND THAT WAS THE BUG THIS TREE KEEPS
// RECORDING. That column is also a SUBJECT of the sweep, so deleting its writer — the exact defect the
// sweep exists to catch — made the control fail with the words "the classifier is broken". A reader would
// have gone hunting a parser bug while the true answer was the finding the sweep was built to report. A
// control must not be able to fail for the reason its own test is about.
func TestTheClassifierTellsAWriteFromAProjection(t *testing.T) {
	written, mentioned := classify(splitBlocks(`
-- name: WriteOne
INSERT INTO widgets (alpha, beta) VALUES ($1, $2);
-- name: PatchOne
WITH chosen AS (SELECT id FROM widgets WHERE alpha = $1)
UPDATE widgets SET gamma = $2 FROM chosen WHERE widgets.id = chosen.id;
-- name: ReadOne
SELECT alpha, delta FROM widgets WHERE beta = $1;
`))
	for _, want := range []struct {
		column string
		writes int
		reads  int
	}{
		{column: "alpha", writes: 1, reads: 2}, // INSERT writes it; the CTE's WHERE and the projection read it
		{column: "beta", writes: 1, reads: 1},  // written by the INSERT; `WHERE beta = $1` is a read
		{column: "gamma", writes: 1, reads: 0}, // a SET behind a CTE, which a first-keyword classifier misses
		{column: "delta", writes: 0, reads: 1}, // projected only — the shape a finding has
	} {
		if got := len(written[want.column]); got != want.writes {
			t.Fatalf("the classifier calls %q written by %d block(s), want %d — it cannot tell an INSERT column "+
				"list or a SET assignment from a projection, so every column would look written and the sweep "+
				"would report nothing forever", want.column, got, want.writes)
		}
		if got := len(mentioned[want.column]); got != want.reads {
			t.Fatalf("the classifier calls %q read by %d block(s), want %d — a sweep that sees no readers reports "+
				"no findings for the same reason a clean schema does", want.column, got, want.reads)
		}
	}
}

// TestEveryColumnAQueryReadsHasAQueryThatWritesIt is the sweep.
//
// IT ASKS ABOUT COLUMNS A QUERY READS, not about every column in the schema. A column nothing mentions at
// all is a different and much noisier question — a projection nobody selects yet, a column a future epic
// added — and folding the two together would bury the case that matters under the one that does not.
func TestEveryColumnAQueryReadsHasAQueryThatWritesIt(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	blocks := loadQueryBlocks(t, repoRootForQueries(t))
	if len(blocks) < 200 {
		t.Fatalf("parsed %d named queries out of storage/queries, want at least 200 — the block parser is broken and every column below would look unread", len(blocks))
	}
	written, mentioned := classify(blocks)

	rows, err := cs.Pool().Query(storage.WithSystemScope(ctx), `
		SELECT table_name, column_name, coalesce(column_default, '')
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND is_identity = 'NO'
		   AND is_generated = 'NEVER'
		 ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatalf("read the schema's columns: %v", err)
	}
	defer rows.Close()

	var findings, subjects []string
	for rows.Next() {
		var table, column, columnDefault string
		if err := rows.Scan(&table, &column, &columnDefault); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		// `id`, `project_id` and the timestamps are written by every insert in the tree and carry no
		// information here; excluding them keeps a failure list about columns somebody added for a reason.
		switch column {
		case "id", "project_id", "created_at", "updated_at":
			continue
		}
		if columnDefault != "" && !defaultIsUnset(columnDefault) {
			continue
		}
		subjects = append(subjects, table+"."+column)
		if len(written[column]) > 0 || len(mentioned[column]) == 0 {
			continue
		}
		findings = append(findings, table+"."+column+" (read by "+strings.Join(first(mentioned[column], 3), ", ")+")")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}
	if len(subjects) < 100 {
		t.Fatalf("the schema offered %d subject columns, want at least 100 — the exclusions above are eating the sweep and a clean result would mean nothing", len(subjects))
	}

	sort.Strings(findings)
	if len(findings) > 0 {
		t.Fatalf("%d column(s) the platform reads have no statement that writes them:\n  %s\n\n"+
			"A column with no writer answers its zero value forever — which is exactly how "+
			"runner_pools.isolation_mode armed an enrolment refusal no operator could reach for a year. If "+
			"the writer is a trigger or a real default, say so in the SCHEMA (this sweep asks the database, "+
			"not a name list); if the column is dead, drop it.",
			len(findings), strings.Join(findings, "\n  "))
	}
}

// defaultIsUnset reports whether a column default is the type's ZERO VALUE rather than a real one.
//
// THE DISTINCTION IS THE WHOLE TEST. `DEFAULT clock_timestamp()` means the database supplies the value and
// no statement needs to; `DEFAULT ”` means the row starts out with nothing in it and SOMETHING had better
// be able to put a value there. runner_pools.isolation_mode carried the second kind for a year while every
// pool answered the empty string and the refusal it armed could never fire.
func defaultIsUnset(columnDefault string) bool {
	switch strings.TrimSpace(strings.SplitN(columnDefault, "::", 2)[0]) {
	case "''", "0", "false", "'{}'", "'[]'":
		return true
	}
	return false
}

func first(values []string, n int) []string {
	if len(values) <= n {
		return values
	}
	return values[:n]
}

// repoRootForQueries walks up to the go.mod, the way the UAT reachability sweeps find the tree they read.
// The subject here is the SQL this repository ships, so it is read from the repository.
func repoRootForQueries(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
