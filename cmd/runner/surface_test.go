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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// listenerCall matches the ways a Go program starts accepting connections. It is deliberately narrow —
// these are the calls, not a word that appears near them.
var listenerCall = regexp.MustCompile(`\b(net\.Listen|net\.ListenTCP|net\.ListenUnix|tls\.Listen|http\.ListenAndServe|http\.ListenAndServeTLS|\.ListenAndServe\(|\.Serve\()`)

// deviceSource returns the production (non-test) Go sources a packaged agent is built from.
func deviceSource(t *testing.T) map[string][]byte {
	t.Helper()
	root := repoRootOf(t)
	out := map[string][]byte{}
	for _, dir := range []string{"cmd/runner", "packages/runner"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			out[rel] = b
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if len(out) == 0 {
		t.Fatal("no device sources were read — a scan over an empty corpus reports the same clean result as a device with no listener")
	}
	return out
}

// TestTheDeviceOPENSNoListener is the outbound-only property, which is what makes a fleet installable on
// machines nobody port-forwards to and what keeps a rented Mac from being reachable from the internet.
//
// ‼️ IT HELD AND NOTHING GUARDED IT. The whole device design rests on the machine dialling the gateway —
// §3.2's "no reverse connection or device listener is added" — and the only thing enforcing it was that
// nobody had written one. A health endpoint added "just for debugging" is how this property is lost, and
// it would be lost on every machine in the fleet at once.
//
// ‼️ AND THE SCAN IS CHECKED AGAINST A POSITIVE CONTROL. A regex that stopped matching — a rename, a
// wrapper, a typo — would report the same clean result as a device with no listener, so the same pattern
// is run over the control plane's own composition root, which certainly does listen. If that finds
// nothing, this test fails instead of passing.
func TestTheDeviceOPENSNoListener(t *testing.T) {
	control, err := os.ReadFile(filepath.Join(repoRootOf(t), "apps/control-plane/cmd/palai-control-plane/main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !listenerCall.Match(control) {
		t.Fatal("the listener pattern does not match the control plane's own main.go — it has stopped " +
			"recognising a listener, so its silence about the device means nothing")
	}

	for path, body := range deviceSource(t) {
		if loc := listenerCall.FindIndex(body); loc != nil {
			line := 1 + strings.Count(string(body[:loc[0]]), "\n")
			t.Errorf("%s:%d opens a listener (%q). A device dials OUT and is never dialled: this one would "+
				"be reachable on every machine the fleet installs it on", path, line, body[loc[0]:loc[1]])
		}
	}
}

// repoRootOf resolves the module root from this package's directory.
func repoRootOf(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
