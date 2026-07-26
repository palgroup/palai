package uat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// approval is a well-formed two-person approval record: a builder, ONE other authorized maintainer who
// approved in the protected environment, no bypass. Every case below starts from this and moves ONE thing.
func approval() map[string]any {
	return map[string]any{
		"schema":       ReleaseApprovalSchema,
		"release":      "self-host-0.2.0",
		"target":       "rc",
		"environment":  ReleaseEnvironment,
		"workflow_run": "https://github.com/palgroup/palai/actions/runs/42",
		"builder":      "maintainer-a",
		"approvers":    []any{"maintainer-b"},
		"admin_bypass": false,
	}
}

var twoMaintainers = []string{"maintainer-a", "maintainer-b"}

func approvalBytes(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// refusedBecause asserts the gate REFUSED and that it named the expected reason — a gate that refuses for
// an unrelated reason (a mismatched release, say) would otherwise pass a test about self-approval.
func refusedBecause(t *testing.T, m map[string]any, want string) {
	t.Helper()
	refusals := ApprovalGate(approvalBytes(t, m), "self-host-0.2.0", "rc", twoMaintainers)
	if len(refusals) == 0 {
		t.Fatalf("the approval gate PASSED an approval it must refuse (%s):\n%s", want, approvalBytes(t, m))
	}
	for _, r := range refusals {
		if strings.Contains(r.Detail, want) {
			return
		}
	}
	t.Fatalf("the gate refused, but not for %q; got: %v", want, refusals)
}

// TestApprovalGatePassesATwoPersonApproval — the gate has a green path at all. Without this the refusal
// cases below are satisfied by a gate that refuses everything, which proves nothing about its reasons.
func TestApprovalGatePassesATwoPersonApproval(t *testing.T) {
	if r := ApprovalGate(approvalBytes(t, approval()), "self-host-0.2.0", "rc", twoMaintainers); len(r) != 0 {
		t.Fatalf("a builder + one DIFFERENT authorized approver in the protected environment must pass: %v", r)
	}
}

// TestApprovalGateRefusesASinglePersonPromotion — release-policy's two-person sentence, mechanically:
// "One maintainer starts the protected release workflow and a DIFFERENT authorized maintainer ... approves
// the protected environment. The builder cannot bypass this gate, including as a repository administrator."
//
// The self-approval comparison is the load-bearing one and it is GUILTY UNTIL PROVEN: a GitHub login is
// case-insensitive, CODEOWNERS spells it with an @, and a hand-written record carries stray whitespace, so
// each of those spellings is the SAME person and each must be caught.
func TestApprovalGateRefusesASinglePersonPromotion(t *testing.T) {
	t.Run("no approver at all", func(t *testing.T) {
		m := approval()
		m["approvers"] = []any{}
		refusedBecause(t, m, "single-person")
	})

	t.Run("an empty approver entry is not an approver", func(t *testing.T) {
		m := approval()
		m["approvers"] = []any{"   "}
		refusedBecause(t, m, "single-person")
	})

	for _, spelling := range []string{
		"maintainer-a",   // the plain case
		"Maintainer-A",   // GitHub logins are case-insensitive
		"@maintainer-a",  // the CODEOWNERS spelling
		" maintainer-a ", // hand-written whitespace
		"@Maintainer-A ", // all three at once
	} {
		t.Run("self-approval spelled "+spelling, func(t *testing.T) {
			m := approval()
			m["approvers"] = []any{spelling}
			refusedBecause(t, m, "cannot approve their own release")
		})
		t.Run("self-approval riding behind an honest approver, spelled "+spelling, func(t *testing.T) {
			m := approval()
			m["approvers"] = []any{"maintainer-b", spelling}
			refusedBecause(t, m, "cannot approve their own release")
		})
	}

	t.Run("an admin bypass is still a single-person promotion", func(t *testing.T) {
		m := approval()
		m["admin_bypass"] = true
		refusedBecause(t, m, "cannot bypass this gate")
	})
}

// TestApprovalGateRefusesAnUnauthorizedApprover — an approval by someone outside the canonical maintainer
// set is two people but not two MAINTAINERS.
func TestApprovalGateRefusesAnUnauthorizedApprover(t *testing.T) {
	m := approval()
	m["approvers"] = []any{"drive-by"}
	refusedBecause(t, m, "is not an authorized maintainer")

	m = approval()
	m["builder"] = "drive-by"
	refusedBecause(t, m, "is not an authorized maintainer")
}

// TestApprovalGateBindsToOneReleaseAndRun — an approval is for ONE release, ONE target and ONE protected
// environment. Without the binding, an approval granted for an rc could be replayed to promote something
// else entirely, which is the whole point of recording it.
func TestApprovalGateBindsToOneReleaseAndRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutet func(map[string]any)
		want  string
	}{
		{"another release", func(m map[string]any) { m["release"] = "sdk-provider-parity-0.1.0" }, "approval is for release"},
		{"another target", func(m map[string]any) { m["target"] = "stable" }, "approval is for target"},
		{"an unprotected environment", func(m map[string]any) { m["environment"] = "dev" }, "protected release environment"},
		{"no environment", func(m map[string]any) { delete(m, "environment") }, "protected release environment"},
		{"no workflow run", func(m map[string]any) { m["workflow_run"] = "" }, "names no workflow run"},
		{"no builder", func(m map[string]any) { m["builder"] = "" }, "names no builder"},
		{"another schema", func(m map[string]any) { m["schema"] = "palai.release-approval/v2" }, "approval schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := approval()
			tc.mutet(m)
			refusedBecause(t, m, tc.want)
		})
	}

	if r := ApprovalGate([]byte("not json"), "self-host-0.2.0", "rc", twoMaintainers); len(r) == 0 {
		t.Fatal("an unparsable approval record must be REFUSED, never treated as absent")
	}
}

// TestApprovalGateCannotBeSatisfiedByOneMaintainer — the honest ceiling, mechanically. release-policy:
// "Until two maintainers and a protected release environment exist, Palai may publish development
// snapshots but must not publish an RC or stable release." The gate reads the canonical maintainer set
// rather than believing the record's own idea of who is authorized, so in a one-owner repository EVERY
// approval is refused — including one that names two people.
func TestApprovalGateCannotBeSatisfiedByOneMaintainer(t *testing.T) {
	m := approval()
	if r := ApprovalGate(approvalBytes(t, m), "self-host-0.2.0", "rc", []string{"maintainer-a"}); len(r) == 0 {
		t.Fatal("a repository with ONE authorized maintainer cannot satisfy the two-person rule")
	}
	refusals := ApprovalGate(approvalBytes(t, m), "self-host-0.2.0", "rc", []string{"maintainer-a"})
	if !strings.Contains(refusals[0].Detail, "until two maintainers") {
		t.Fatalf("the refusal must name release-policy's own sentence; got %v", refusals)
	}
}

// TestMaintainersFromCODEOWNERS — the canonical set is RECOMPUTED from the git-tracked owners file, never
// taken from the approval's own copy (plan §2 recompute-over-copy). A team handle is not a person: its
// membership lives outside this repository, so it cannot stand in for the second human.
func TestMaintainersFromCODEOWNERS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CODEOWNERS")
	if err := os.WriteFile(path, []byte(
		"# a comment @not-an-owner\n"+
			"\n"+
			"* @Maintainer-A @maintainer-b\n"+
			"docs/ @palgroup/docs-team\n"+
			"scripts/ @maintainer-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := MaintainersFromCODEOWNERS(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "maintainer-a,maintainer-b" {
		t.Fatalf("maintainers = %v, want the two humans, normalized and deduped (no team, no comment)", got)
	}

	// The repository's REAL owners file is the set the shipped gate uses. Whatever it says, the gate must
	// be able to read it — a canonical source that does not parse is a gate that fails open.
	real, err := MaintainersFromCODEOWNERS(filepath.Join("..", "..", ".github", "CODEOWNERS"))
	if err != nil {
		t.Fatalf("the shipped .github/CODEOWNERS must parse: %v", err)
	}
	if len(real) == 0 {
		t.Fatal("the shipped .github/CODEOWNERS names no maintainer at all")
	}
}
