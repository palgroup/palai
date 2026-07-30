package storage

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// THE CIPHERTEXT SWEEP — layer 1 of E25's three-layer "no secret value comes back" proof (plan §2).
//
// The other two layers are a projection pin (no view struct has a Value field) and a byte scan (no
// response body carries the sentinel). This one is the layer neither of those can be: it is the only
// check that fails when someone WRITES A NEW QUERY, before that query has a caller, a view or a route.
//
// The property: exactly two statements in the whole of storage/queries touch the `ciphertext` column,
// and both belong to the secret store — InsertSecretRef writes a sealed blob, ResolveSecretRef reads one
// back for internal decryption. ResolveSecretRef's single caller is SecretStore.Resolve, which is
// reachable from no /v1 route. Everything else that speaks about a secret speaks about its NAME, its
// VERSION and when it was written.
//
// E25 T3 is why this exists now rather than later: environments store their values in secret_refs under
// a derived name, and the shortest way to build a "show me the value" console screen would have been one
// new query in storage/queries/environments.sql. This turns that into a red test, and the red is the
// decision — a third name on this list is a design change that has to be argued for in the diff.
//
// WHY IT LIVES IN package storage AND NOT IN tests/uat. It is untagged here, so it rides `make verify`,
// which is the only routine gate in this tree that runs on every change. The plan filed it under §2's
// evidence layers; the evidence is stronger for running more often.
func TestOnlyTwoQueriesTouchTheCiphertextColumn(t *testing.T) {
	want := []string{"InsertSecretRef", "ResolveSecretRef"}

	got := queriesMentioning(t, "ciphertext")
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf(`queries touching the ciphertext column = %v, want %v

A new query reading or writing `+"`ciphertext`"+` is how a secret VALUE acquires a read-back path, and every
projection/route/view guard downstream of it is defeated the moment one exists. If this list grew
deliberately, change it here WITH the reason. If it grew accidentally, the query is the bug.`, got, want)
	}

	// NON-VACUITY, in the two ways this sweep could rot into a permanent pass.
	//
	// (a) The parser still finds statements at all. A regexp or split that stopped matching `-- name:`
	// would return an empty list for `ciphertext` AND for everything else, and an empty list compared
	// against a two-element want would at least fail — but a want that someone had reduced to empty
	// would then pass forever. Counting the whole corpus catches the parser independently of the term.
	if all := allQueryNames(t); len(all) < 300 {
		t.Fatalf("the query parser found only %d named statements in storage/queries; it has stopped parsing and this sweep is vacuous", len(all))
	}
	// (b) The term is searched in the STATEMENT, not in the comments — a comment mentioning ciphertext
	// (there are several, including this table's own header) must not count, or the list would be
	// padded with false positives and a real one could hide among them. Proven by searching for a token
	// that appears ONLY in a comment in storage/queries/secrets.sql.
	if hits := queriesMentioning(t, "envelope"); len(hits) != 0 {
		t.Errorf("a comment-only term matched %v — the sweep is reading comments as statements, so its ciphertext list is not trustworthy", hits)
	}
}

// queriesMentioning returns the NAMES of every named statement whose SQL body contains term
// (case-insensitively). The body is the lines after a `-- name: X` marker up to the next marker, with
// comment lines dropped — so a term that only ever appears in prose is not a hit.
func queriesMentioning(t *testing.T, term string) []string {
	t.Helper()
	var names []string
	for _, path := range queryFiles(t) {
		for name, body := range namedStatements(t, path) {
			if strings.Contains(strings.ToLower(body), strings.ToLower(term)) {
				names = append(names, name)
			}
		}
	}
	return names
}

func allQueryNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, path := range queryFiles(t) {
		for name := range namedStatements(t, path) {
			names = append(names, name)
		}
	}
	return names
}

// namedStatements splits one .sql file into name → statement body. A `-- name: X` line opens a
// statement; the body runs to the next such line (or EOF) and every `--` line inside it is dropped.
func namedStatements(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]string{}
	name := ""
	var body strings.Builder
	flush := func() {
		if name != "" {
			out[name] = body.String()
		}
		body.Reset()
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if marker, ok := strings.CutPrefix(trimmed, "-- name:"); ok {
			flush()
			name = strings.TrimSpace(marker)
			continue
		}
		if strings.HasPrefix(trimmed, "--") {
			continue // prose, not SQL
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	flush()
	return out
}

// queryFiles lists storage/queries/*.sql. It reads the DIRECTORY rather than the embed declarations, so a
// query file someone forgot to embed is still swept — an unembedded file is a bug, not an exemption.
func queryFiles(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("queries", "*.sql"))
	if err != nil {
		t.Fatalf("glob queries: %v", err)
	}
	if len(paths) < 30 {
		t.Fatalf("found only %d query files under storage/queries; the glob is wrong and this sweep is vacuous", len(paths))
	}
	return paths
}
