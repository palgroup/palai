package main

// WHAT A DEVICE ARTIFACT MAY CONTAIN — device plan T7: "extracting the device package must reveal no
// server/admin command implementation."
//
// ‼️ THE PROPERTY HELD AND NOTHING GUARDED IT. On 2026-08-06 `go list -deps ./cmd/runner` linked nothing
// from apps/ or cmd/cli/internal/, which is what the plan requires — but that was true by habit, not by
// rule, and one import added while wiring a panel feature would have shipped the control-plane's HTTP
// surface and the admin verbs onto every machine in the fleet. The archive is installed by an autoscaler
// onto hosts nobody logs into; what it carries is what an attacker who reaches one of them gets.
//
// The check is on the LINKED PACKAGE SET rather than on a grep of the source: an admin verb reached
// through a helper package is still an admin verb in the binary, and a grep of cmd/runner/*.go would not
// see it.

import (
	"os/exec"
	"strings"
	"testing"
)

// deniedRoots is the server tree, as an import-path prefix rather than a keyword: "admin" as a substring
// would match an unrelated identifier, while this names the thing itself.
//
// ‼️ THE ADMIN SURFACE IS NOT LISTED, AND ITS ABSENCE IS THE POINT. `cmd/cli` is a `package main` and
// `cmd/cli/internal/...` is closed by Go's own internal-package rule, so neither can be imported from
// here at all — a compile error, not a test failure. A denied root that cannot fire reads as a live rule
// and is worth less than no rule: it takes credit for a guarantee the compiler is providing.
var deniedRoots = []string{
	"github.com/palgroup/palai/apps/",
}

func packageList(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	var pkgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			pkgs = append(pkgs, line)
		}
	}
	return pkgs
}

// TestTheDeviceBinaryLinksNoServerOrAdminCode is the rule. It also asserts the denied roots NAME
// SOMETHING: a prefix that matches zero packages in the module would make this pass forever, and this
// tree has already shipped one rule whose selector had quietly gone empty.
func TestTheDeviceBinaryLinksNoServerOrAdminCode(t *testing.T) {
	// The module pattern, not "./...": a test's working directory is its own package, so "./..." would
	// list every package UNDER cmd/runner — four of them, none matching a denied root — and the emptiness
	// check below would fire on a rule that was in fact fine. It did, the first time this ran.
	all := packageList(t, "github.com/palgroup/palai/...")
	for _, root := range deniedRoots {
		found := false
		for _, p := range all {
			if strings.HasPrefix(p, root) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("no package in this module starts with %q — the rule below matches nothing and would "+
				"pass whatever the device linked", root)
		}
	}

	for _, dep := range packageList(t, "-deps", ".") {
		for _, root := range deniedRoots {
			if strings.HasPrefix(dep, root) {
				t.Errorf("the device binary links %s — a machine an autoscaler installs would carry the "+
					"%s implementation it has no business running", dep, strings.TrimSuffix(root, "/"))
			}
		}
	}
}
