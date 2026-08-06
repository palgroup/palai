package docs_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/macagent"
)

// THE GROUP A SESSION ACCOUNT IS CREATED IN IS THE BOUNDARY, and until 2026-08-06 it was `staff` — the
// operator's own login group — which meant the uid drop isolated sessions from each other and left the
// machine's owner completely exposed. Measured from inside a live session on this Mac: `id -un` answered
// the operator, and `ls /Users/salih/workspace` listed sixteen unrelated checkouts. A macOS home is
// `drwxr-x---` owned by `<operator>:staff`, so the group bit alone was the whole opening.
//
// Two claims are measured here, and neither was measured before:
//
//  1. the group is not one the operator is already in (macagent.AccountGID);
//  2. BOTH creators spend the same one. packages/macagent/protocol.go's comment claimed "the number
//     lives in one place and both creators spend it" while mac-sessions.sh carried a literal `-GID 20`.
//     A comment is what a reader audits instead of the caller, which is why the claim is now a test.

const macSessionsScript = "scripts/ops/mac-sessions.sh"

// operatorGroups are the macOS groups a human account is a member of on a stock machine. A session
// account in any of them reaches whatever that human reaches, which is the thing this whole layer
// exists to prevent — so naming one is a refusal rather than a preference.
var operatorGroups = map[int]string{
	0:  "wheel",
	12: "everyone",
	20: "staff",
	80: "admin",
}

func TestASessionAccountIsNotCreatedInAGroupTheOperatorIsIn(t *testing.T) {
	if name, bad := operatorGroups[macagent.AccountGID]; bad {
		t.Fatalf("macagent.AccountGID is %d (%s), which the operator's own account is in: a session account "+
			"created there traverses the operator's home by the group bit — measured 2026-08-06, `ls "+
			"/Users/<operator>/workspace` from inside a session listed every checkout under it. The uid drop "+
			"is not a boundary while the gid is a shared one.", macagent.AccountGID, name)
	}
	if macagent.AccountGroupName == "" {
		t.Fatal("macagent.AccountGroupName is empty: the creators name a group by NAME and a gid by number, " +
			"and an unnamed group cannot be made before the account that needs it")
	}
}

func TestBothAccountCreatorsSpendTheSameGroup(t *testing.T) {
	script := readRepoFile(t, macSessionsScript)

	gid := shellAssignment(t, script, "SESSION_GID")
	got, err := strconv.Atoi(gid)
	if err != nil {
		t.Fatalf("%s sets SESSION_GID to %q, which is not a number", macSessionsScript, gid)
	}
	if got != macagent.AccountGID {
		t.Fatalf("%s creates accounts with gid %d and macagent.AccountGID is %d: the control plane hands the "+
			"Go number to whatever drops privilege, so an account created with the other one cannot open its "+
			"own home", macSessionsScript, got, macagent.AccountGID)
	}

	if name := strings.Trim(shellAssignment(t, script, "SESSION_GROUP"), `"'`); name != macagent.AccountGroupName {
		t.Fatalf("%s names the group %q and macagent.AccountGroupName is %q: two names is two groups, and the "+
			"one an account lands in decides what it can read", macSessionsScript, name, macagent.AccountGroupName)
	}

	// AND NO LITERAL SURVIVES ANYWHERE, which is the form the defect actually took: the constant existed,
	// the comment claimed both creators spent it, and one of them passed `-GID 20` three lines away.
	if m := regexp.MustCompile(`-GID\s+(\d+)`).FindStringSubmatch(script); m != nil {
		t.Fatalf("%s passes a LITERAL gid to sysadminctl (-GID %s): it must spend $SESSION_GID, or the two "+
			"creators drift silently the way they already did once", macSessionsScript, m[1])
	}
}

// shellAssignment reads a top-level `NAME=value` out of a shell script. It deliberately does not run the
// script: the assignment is what an operator reads and what sysadminctl is handed, and sourcing a file
// that expects root would prove nothing about either.
func shellAssignment(t *testing.T, script, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `=(.*)$`)
	m := re.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("%s has no top-level %s= assignment", macSessionsScript, name)
	}
	return strings.TrimSpace(m[1])
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}
