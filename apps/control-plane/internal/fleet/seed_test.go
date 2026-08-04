package fleet_test

// BIT-UNCHANGEDNESS OF THE POOL A TENANT IS BORN WITH (E28 T1 RED 3).
//
// T1 gives a pool a birth path, and the whole risk of that is the row every existing installation ALREADY
// HAS. `InsertDefaultRunnerPool` writes `'default'`, `'sandboxed-linux'` and `false` as LITERALS
// (storage/queries/runners.sql) — the three values that made a second pool impossible — and the temptation
// while opening a create route is to parameterise the statement rather than add one. That would silently
// re-point every tenant's birth at whatever a caller passed.
//
// THIS GUARD IS DOCKER-FREE ON PURPOSE. The component tier proves the row a real Postgres ends up with
// (TestStrictIsOffOnEveryPoolThisTreeCreates, and TestPoolBirth… in internal/execution); this proves the
// STATEMENT, so a change to it is caught by `make verify` rather than by a tier that needs a container.
//
// A bit-unchangedness assertion is green before the change it guards and green after — its RED is against
// the WRONG implementation, and it was demonstrated by pointing the statement's posture literal at
// 'unsandboxed-host' and watching this fail.

import (
	"strings"
	"testing"

	"github.com/palgroup/palai/storage"
)

// TestTheDefaultPoolSeedStatementIsUnchanged pins the three literals and the conflict clause. It compares
// the whitespace-collapsed statement rather than the file's bytes, so a re-indent is not a failure and a
// changed value is.
func TestTheDefaultPoolSeedStatementIsUnchanged(t *testing.T) {
	// A.2 Task 6 removed organization_id from the column list (migration 000067 dropped the column). The
	// pin moved with it, and what the pin is ABOUT did not: the three LITERALS and the conflict clause are
	// byte-for-byte what they were. A tenant's birth row is re-pointed by parameterising 'default',
	// 'sandboxed-linux' or false — never by the statement naming one tenant column instead of two.
	const want = "INSERT INTO runner_pools (id, project_id, name, posture, strict_enrollment) " +
		"VALUES ($1, $2, 'default', 'sandboxed-linux', false) ON CONFLICT DO NOTHING;"

	got := collapse(statementBody(storage.Query("InsertDefaultRunnerPool")))
	if got != want {
		t.Fatalf("InsertDefaultRunnerPool is now\n  %s\nwant\n  %s\n\nEvery project alive today was born with the row this statement writes. A parameterised name, a "+
			"parameterised posture or a strict_enrollment that is not the literal false would re-point that birth — "+
			"E28 T1 adds a SECOND statement for a created pool and leaves this one alone", got, want)
	}
}

// TestTheSeedStatementTakesExactlyTwoParameters is the same claim from the other side, and it is the one
// that catches the cheap mistake: a third placeholder is a value a caller now chooses.
//
// IT WAS THREE UNTIL A.2 TASK 6 and the bound moved DOWN with the dropped organization_id, which is the
// direction that needs saying: a guard against "one more parameter" is only as strong as the number it
// starts from, so leaving it at $4 would have let the statement grow a third id back without a word.
func TestTheSeedStatementTakesExactlyTwoParameters(t *testing.T) {
	body := statementBody(storage.Query("InsertDefaultRunnerPool"))
	for _, placeholder := range []string{"$3", "$4", "$5", "$6"} {
		if strings.Contains(body, placeholder) {
			t.Errorf("InsertDefaultRunnerPool now takes %s: the tenant-birth statement writes two ids and three literals, and a third parameter is a decision moved to a caller", placeholder)
		}
	}
}

// statementBody drops the leading `-- …` comment lines storage.Query keeps with the statement.
func statementBody(statement string) string {
	var out []string
	for _, line := range strings.Split(statement, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// collapse folds every run of whitespace into one space, so indentation is not part of the claim.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }
