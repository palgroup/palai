package docs_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sessionAccountScript = "scripts/ops/palai-session-account"

// runSessionAccount executes the wrapper and returns combined output plus the exit code.
//
// It runs the REAL script and never with root, which is exactly the coverage that matters: everything
// asserted below must be decided BEFORE anything with authority runs, so a refusal here is a refusal that
// would have happened before sudo handed it anything.
func runSessionAccount(t *testing.T, args ...string) (string, int) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("bash", append([]string{filepath.Join(root, sessionAccountScript)}, args...)...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s %v: %v", sessionAccountScript, args, err)
	}
	return string(out), code
}

// TestSessionAccountWrapperRefusesEverythingButTwoVerbsAndAnIndex is the whole security argument for the
// sudoers entry this wrapper exists to make safe.
//
// THE PRIVILEGE IS THE SHAPE, NOT THE INTENT. A NOPASSWD line naming mac-sessions.sh directly would grant
// everything that script can do — `down --mode accounts --apply` deletes every session account on the box.
// So the wrapper accepts one verb and one two-digit index and passes NOTHING through, and this test is the
// enumeration of what that means: each case below is a thing that must not reach the privileged script.
//
// EVERY REFUSAL MUST HAPPEN BEFORE AUTHORITY IS USED, which is why these run unprivileged. A refusal that
// only happened under sudo would be a refusal that had already been handed root.
func TestSessionAccountWrapperRefusesEverythingButTwoVerbsAndAnIndex(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		why  string
	}{
		{"no arguments", nil, "a bare invocation must not default to acting on anything"},
		{"unknown verb", []string{"list", "01"}, "the wrapper has two verbs; a third is a surface nobody reviewed"},
		{"verb with no index", []string{"destroy"}, "a destroy that names no session is a destroy that could mean all of them"},
		{"two indices", []string{"destroy", "01", "02"}, "extra arguments are the pass-through this wrapper exists not to have"},

		// EVERY ONE OF THESE IS A NUMBER TO $((...)) AND NONE IS WHAT THIS TOOLING FORMATS. A range check
		// applied after arithmetic would accept them all; the shape check refuses them, because a value that
		// took a different path to the same integer took a path nobody tested.
		{"one digit", []string{"create", "1"}, "the index this tooling formats is two digits"},
		{"leading plus", []string{"create", "+1"}, "$((+1)) is 1, and this is not what any caller writes"},
		{"leading space", []string{"create", " 1"}, "whitespace around a number is a different string"},
		{"trailing space", []string{"create", "01 "}, "whitespace around a number is a different string"},
		{"scientific", []string{"create", "1e1"}, "arithmetic contexts accept it; this is not a session index"},
		{"hex", []string{"create", "0x11"}, "arithmetic contexts accept it; this is not a session index"},
		{"index zero", []string{"create", "00"}, "there is no session zero; the deletion guard refuses it too"},
		{"three digits", []string{"create", "100"}, "the range is 01..99"},

		// AND THE SHAPES THAT ARE NOT NUMBERS AT ALL. A wrapper reached through sudo is a wrapper whose
		// arguments an attacker chooses, so the ones that matter are the ones that try to become something
		// other than an index.
		{"flag as index", []string{"create", "--apply"}, "a flag must never reach the privileged script"},
		{"flag injection", []string{"create", "01 --count 99"}, "one argument is one argument, not a command line"},
		{"path traversal", []string{"destroy", "../../etc"}, "an index is not a path"},
		{"command substitution", []string{"destroy", "$(id -u)"}, "an index is not an expression"},
		{"semicolon", []string{"destroy", "01;id"}, "an index is not a statement"},
		{"newline", []string{"destroy", "01\n02"}, "an index is one line"},
		{"empty index", []string{"destroy", ""}, "an empty argument is not a session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := runSessionAccount(t, tc.args...)
			if code == 0 {
				t.Fatalf("`palai-session-account %s` was ACCEPTED (exit 0) — %s\n%s",
					strings.Join(tc.args, " "), tc.why, out)
			}
			// A refusal that reached the privileged script and was refused THERE is a refusal that already
			// had root. The wrapper's own name in the message is how this test tells the two apart.
			if !strings.Contains(out, "palai-session-account") {
				t.Errorf("`%s` was refused, but not by the wrapper — the message names no wrapper, so the "+
					"argument reached mac-sessions.sh before anything checked it:\n%s",
					strings.Join(tc.args, " "), out)
			}
		})
	}
}

// TestSessionAccountSudoersGrantsOnlyThisWrapper reads the entry the wrapper tells an operator to install.
// It is printed rather than written, so this asserts what an operator would be pasting into a file that
// grants passwordless root.
func TestSessionAccountSudoersGrantsOnlyThisWrapper(t *testing.T) {
	out, code := runSessionAccount(t, "install-sudoers", "palai")
	if code != 0 {
		t.Fatalf("install-sudoers exited %d:\n%s", code, out)
	}
	// THE GRANT MUST BE BOUNDED BY PATTERN, not by trust in the wrapper's own parsing. `NOPASSWD: <path>`
	// with no argument pattern would let any argument through to a root invocation, and the wrapper's
	// checks would then be the ONLY thing between sudo and mac-sessions.sh. Two independent bounds is the
	// difference between a bug in this script being a bug and being a root shell.
	for _, want := range []string{
		"palai ALL=(root) NOPASSWD:",
		"create [0-9][0-9]",
		"destroy [0-9][0-9]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the printed sudoers entry does not contain %q — the grant is then wider than the "+
				"wrapper's own argument check, which would make that check the only bound:\n%s", want, out)
		}
	}
	if strings.Contains(out, "mac-sessions.sh") {
		t.Errorf("the sudoers entry names mac-sessions.sh; granting that directly grants everything it can "+
			"do, including `down --mode accounts --apply` on every session:\n%s", out)
	}
	// It must not be written for the operator. A privilege nobody read is a privilege nobody agreed to.
	if !strings.Contains(strings.ToLower(out), "/etc/sudoers.d/") || !strings.Contains(out, "visudo") {
		t.Errorf("the output does not tell the operator where to put it and how to validate it:\n%s", out)
	}

	if _, code := runSessionAccount(t, "install-sudoers", "root; rm -rf /"); code == 0 {
		t.Error("install-sudoers accepted a runas value that is not an account name, so the printed line " +
			"would carry whatever an attacker put in it into a file granting root")
	}
}
