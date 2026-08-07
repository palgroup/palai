// Guards for the macOS session-host operator script and its page.
//
// scripts/ops/mac-sessions.sh is the one operator surface in this tree that runs on hardware CI does
// not have, under an account CI cannot become. So the guards here are deliberately narrow and they
// are the ones that CAN fail without a Mac: the read-only subcommands must be read-only, the argument
// handling must refuse what it says it refuses, the deletion guard must be provably capable of
// refusing (`selftest`), and the operator page must not describe a script that does not exist.
//
// Same rule as the rest of tests/docs: a document that CLAIMS something about the tree is checked
// against the tree. Untagged and Docker-free, so it rides `make verify`.
package docs_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	macScript = "scripts/ops/mac-sessions.sh"
	// macTestPrefix is the namespace runMacSessions pins; the assertions must name the same one or
	// they assert about accounts this test never scanned.
	macTestPrefix = "palaitest-s"
	macDoc        = "docs/operations/mac-sessions.md"
)

// runMacSessions executes a subcommand and returns combined output plus the exit code. The host UUID
// is pinned so the script's identity checks are deterministic off a Mac.
func runMacSessions(t *testing.T, args ...string) (string, int) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("bash", append([]string{filepath.Join(root, macScript)}, args...)...)
	cmd.Dir = root
	// A NAMESPACE NO REAL MACHINE USES, and it is not tidiness. This test drives the REAL script against
	// REAL directory services, so without it the scan finds the operator's own session accounts — the ones
	// docs/operations/mac-sessions.md tells them to create — whose markers were minted on the real host
	// UUID and therefore collide with the fake one pinned below, turning a dry run into exit 2. That was
	// measured on 2026-08-02: a single palai-s01 on the box made this test red, so following the page made
	// `make verify` impossible. The pinned UUID alone could not fix it, because the collision is found by
	// NAME before any marker is read.
	cmd.Env = append(cmd.Environ(),
		"PALAI_MAC_SESSIONS_HOST_UUID=TEST-HOST-UUID",
		"PALAI_MAC_SESSIONS_PREFIX=palaitest-s",
		// A name alone is not a namespace: index 01 still allocates uid 701, which the operator's own
		// palai-s01 holds. Both halves or neither.
		"PALAI_MAC_SESSIONS_UID_BASE=900",
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s %v: %v", macScript, args, err)
	}
	return string(out), code
}

// scriptSubcommands reads the declared subcommand list out of the script itself, so the doc guard
// below compares the doc against the script rather than against a list duplicated in this file.
func scriptSubcommands(t *testing.T) map[string]bool {
	t.Helper()
	m := regexp.MustCompile(`(?m)^SUBCOMMANDS="([a-z ]+)"\s*$`).FindStringSubmatch(readDoc(t, macScript))
	if m == nil {
		t.Fatalf(`%s declares no `+"`"+`SUBCOMMANDS="..."`+"`"+` line — the doc guard has nothing to compare against`, macScript)
	}
	set := map[string]bool{}
	for _, s := range strings.Fields(m[1]) {
		set[s] = true
	}
	if len(set) < 4 {
		t.Fatalf("%s declares only %d subcommands — the resolver is broken, not the script", macScript, len(set))
	}
	return set
}

// TestMacSessionsPlanIsReadOnlyAndNamesBothModes. `plan` is the command an operator runs first, on a
// machine they have not decided anything about yet. It must run anywhere (CI has no Mac), it must
// change nothing, and it must present the cheap option next to the expensive one — the research says
// per-session directories suffice for one customer's sessions, so a script that silently assumed
// accounts would be selling the heavy answer.
func TestMacSessionsPlanIsReadOnlyAndNamesBothModes(t *testing.T) {
	out, code := runMacSessions(t, "plan", "--count", "4")
	if code != 0 {
		t.Fatalf("`plan` exited %d — it must run on any host, including one that is not a Mac:\n%s", code, out)
	}
	for _, want := range []string{
		"--mode dirs",        // the cheap option, offered first
		"--mode accounts",    // the expensive one
		macTestPrefix + "01", // the names it would allocate
		macTestPrefix + "04",
		"901", // and their uids — the pinned base, not the shipped one
		"904",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("`plan --count 4` output does not mention %q:\n%s", want, out)
		}
	}
	// The ceiling has to be in the output an operator actually reads, not only in the header comment.
	if !strings.Contains(strings.ToLower(out), "different macs") {
		t.Errorf("`plan` never tells the operator that different customers still need different Macs:\n%s", out)
	}
	// Read-only means read-only: no mutating tool may be invoked on the plan path.
	if strings.Contains(out, "sysadminctl -addUser") && !strings.Contains(out, "would") {
		t.Errorf("`plan` looks like it ran a mutating command:\n%s", out)
	}
}

// TestMacSessionsRefusesWhatItSaysItRefuses. Argument handling is the half of a destructive script
// that CI can test, so it is tested exhaustively. A count that is not a small positive integer, an
// unknown subcommand, an unknown mode and a missing count must all fail loudly rather than default.
func TestMacSessionsRefusesWhatItSaysItRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown subcommand", []string{"nuke"}},
		{"no subcommand", nil},
		{"count zero", []string{"plan", "--count", "0"}},
		{"count negative", []string{"plan", "--count", "-1"}},
		{"count not a number", []string{"plan", "--count", "four"}},
		{"count above the name space", []string{"plan", "--count", "100"}},
		{"count with an injection", []string{"plan", "--count", "1; rm -rf /"}},
		{"unknown mode", []string{"plan", "--mode", "vms"}},
		{"unknown flag", []string{"plan", "--force"}},
		{"up without a count", []string{"up", "--mode", "accounts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := runMacSessions(t, tc.args...)
			// Exit 2 specifically, not merely non-zero: 127 (script missing) or 1 (something blew
			// up mid-run) would otherwise let this whole table pass without the script refusing
			// anything.
			if code != 2 {
				t.Errorf("`%s` exited %d, want 2 (usage refusal):\n%s", strings.Join(tc.args, " "), code, out)
			}
		})
	}
}

// TestMacSessionsDestructiveSubcommandsNeedAnExplicitFlag. `up` and `down` without `--apply` are dry
// runs that report and stop. This repo's scripts run non-interactively, so a y/n prompt is not a
// guard; the flag is.
func TestMacSessionsDestructiveSubcommandsNeedAnExplicitFlag(t *testing.T) {
	for _, args := range [][]string{
		{"up", "--mode", "accounts", "--count", "2"},
		{"down", "--mode", "accounts"},
		{"down", "--mode", "dirs"},
	} {
		out, code := runMacSessions(t, args...)
		if code != 0 {
			t.Errorf("`%s` (no --apply) exited %d — a dry run must succeed and report:\n%s", strings.Join(args, " "), code, out)
			continue
		}
		if !strings.Contains(out, "--apply") {
			t.Errorf("`%s` (no --apply) does not tell the operator which flag would make it act:\n%s", strings.Join(args, " "), out)
		}
		if !strings.Contains(strings.ToLower(out), "nothing was changed") {
			t.Errorf("`%s` (no --apply) does not state that it changed nothing:\n%s", strings.Join(args, " "), out)
		}
	}
}

// TestMacSessionsDeletionGuardBites is the important one. `down` removes user accounts, and the whole
// safety of it is one predicate. `selftest` runs that predicate against a table that includes the
// account this script created (permit) and nine ways of being something else (refuse) — a name with
// no marker, a marker minted on another Mac, a uid that does not match the name, an admin, the
// operator's own account. It exits non-zero if any row decides the wrong way, so this guard fails if
// the predicate is ever loosened.
func TestMacSessionsDeletionGuardBites(t *testing.T) {
	out, code := runMacSessions(t, "selftest")
	if code != 0 {
		t.Fatalf("`selftest` exited %d — the deletion guard decides at least one case wrongly:\n%s", code, out)
	}
	permit := strings.Count(out, "PERMIT")
	refuse := strings.Count(out, "REFUSE")
	if permit == 0 {
		t.Errorf("`selftest` permitted nothing — a guard that refuses everything is untested, not safe:\n%s", out)
	}
	if refuse < 8 {
		t.Errorf("`selftest` exercised only %d refusal cases — the table shrank:\n%s", refuse, out)
	}
}

// TestMacSessionsPutsNoPasswordInArgv. Anything in argv is in `ps` for every user on the box, which
// on a multi-session host is every session. sysadminctl takes `-` to prompt instead; the only
// accepted forms here are that one and a comment.
func TestMacSessionsPutsNoPasswordInArgv(t *testing.T) {
	bad := regexp.MustCompile(`-(admin[Pp]assword|new[Pp]assword|password)\s+(?:-[a-zA-Z]|[^-\s])`)
	for i, line := range strings.Split(readDoc(t, macScript), "\n") {
		code, _, _ := strings.Cut(line, "#")
		if m := bad.FindString(code); m != "" {
			t.Errorf("%s:%d passes a password in argv (%q) — every user on the host can read it from `ps`",
				macScript, i+1, strings.TrimSpace(m))
		}
	}
	// kcpassword is recoverable plaintext. The script may name it in prose; it may not write it.
	for i, line := range strings.Split(readDoc(t, macScript), "\n") {
		code, _, _ := strings.Cut(line, "#")
		if strings.Contains(code, "kcpassword") || strings.Contains(code, "-autologin set") {
			t.Errorf("%s:%d writes auto-login credential material — that is the operator's decision to make, not the script's", macScript, i+1)
		}
	}
}

// TestMacSessionsDocDescribesThisScript keeps the page and the script from drifting apart: every
// `mac-sessions.sh <subcommand>` the doc tells an operator to type must be a subcommand the script
// declares, and every subcommand the script declares must appear in the doc.
func TestMacSessionsDocDescribesThisScript(t *testing.T) {
	declared := scriptSubcommands(t)
	doc := readDoc(t, macDoc)

	invoked := map[string]bool{}
	for _, m := range regexp.MustCompile(`mac-sessions\.sh\s+([a-z]+)`).FindAllStringSubmatch(doc, -1) {
		invoked[m[1]] = true
	}
	if len(invoked) == 0 {
		t.Fatalf("%s invokes the script nowhere — the drift guard would pass vacuously", macDoc)
	}
	for sub := range invoked {
		if !declared[sub] {
			t.Errorf("%s tells an operator to run `mac-sessions.sh %s`, which the script does not accept", macDoc, sub)
		}
	}
	for sub := range declared {
		if !invoked[sub] {
			t.Errorf("%s never documents the `%s` subcommand", macDoc, sub)
		}
	}
}

// TestMacSessionsDocIsSourcedAndHonest. The page's job is to stop an operator believing in a boundary
// that was never measured, so: it carries a guarantee table where every row cites where the claim
// comes from, it has the troubleshooting section, and it states the ceiling and the one unknown the
// fleet design depends on.
func TestMacSessionsDocIsSourcedAndHonest(t *testing.T) {
	doc := readDoc(t, macDoc)

	rows := 0
	for _, r := range parseTables(doc) {
		src, ok := r.cells["source"]
		if !ok {
			continue
		}
		rows++
		if s := strings.TrimSpace(src); s == "" || s == "—" {
			t.Errorf("%s:%d row %q claims something with no source", macDoc, r.line, r.first)
		}
	}
	if rows < 8 {
		t.Errorf("%s has %d sourced guarantee rows — the table or the parser shrank", macDoc, rows)
	}

	for _, want := range []string{
		"When it doesn't work",                // the section the house style requires
		"different Macs",                      // the ceiling
		"macos-session-isolation.md",          // where the measurements live
		"macos-isolation-without-accounts.md", //
		"UNVERIFIED",                          // and what is not measured is labelled
		"scripts/ops/mac-sessions.sh",         //
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s does not contain %q", macDoc, want)
		}
	}
}

// TestTheAccountsPageTellsAnOperatorHowTheControlPlaneUSESTheAccounts — CREATING A BOUNDARY AND USING IT
// ARE TWO CLAIMS, AND THIS PAGE MADE ONLY THE FIRST.
//
// ‼️ MEASURED 2026-08-07: docs/operations/mac-sessions.md named `PALAI_SESSION_ACCOUNT_HELPER` and
// `palai-session-account` ZERO times. §3 tells an operator to create the accounts and §4 proves they are
// isolated FROM EACH OTHER — and nothing on the page said the control plane still runs every session's
// commands as its own uid until the sudoers entry is installed and the variable is set. An operator who
// followed it end to end got four accounts, a green `verify`, and no boundary. §4's own title is "a
// boundary you did not test is a boundary you do not have"; the untested half was the one it omitted.
//
// THE GUARD IS KEYED ON THE INSTRUCTION, NOT ON A FILENAME. Any operator page that tells somebody to bring
// accounts up owes them the step that makes the accounts load-bearing — so the subject is "a page
// containing `--mode accounts`", and a second page that grows the same instruction inherits the same duty
// without anybody remembering to add it here.
func TestTheAccountsPageTellsAnOperatorHowTheControlPlaneUSESTheAccounts(t *testing.T) {
	root := repoRoot(t)
	pages, err := filepath.Glob(filepath.Join(root, "docs/operations/*.md"))
	if err != nil {
		t.Fatalf("list the operations pages: %v", err)
	}
	// NON-VACUITY: a glob that matched nothing, or a corpus where nobody brings accounts up, would pass
	// this test while saying nothing at all.
	if len(pages) < 5 {
		t.Fatalf("found %d operations page(s), want at least 5 — the glob is wrong and every assertion below is vacuous", len(pages))
	}
	subjects := 0
	for _, page := range pages {
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		text := string(body)
		if !strings.Contains(text, "--mode accounts") {
			continue
		}
		subjects++
		rel := strings.TrimPrefix(page, root+"/")
		for _, owed := range []struct{ token, why string }{
			{"palai-session-account", "the privileged wrapper the control plane invokes; without its sudoers entry the plane cannot mint an account at all"},
			{"PALAI_SESSION_ACCOUNT_HELPER", "the variable that makes the control plane USE the accounts; unset, every session's commands run as the plane's own uid"},
		} {
			if !strings.Contains(text, owed.token) {
				t.Errorf("%s tells an operator to bring accounts up with `--mode accounts` and never names %s — %s. "+
					"A page that creates a boundary and does not say how to switch it on leaves a reader with a green "+
					"verify over a mechanism nothing uses", rel, owed.token, owed.why)
			}
		}
	}
	if subjects == 0 {
		t.Fatal("no operations page contains `--mode accounts`, so this guard has no subject — either the instruction moved and this test must follow it, or the mode is gone and this test should go with it")
	}
}
