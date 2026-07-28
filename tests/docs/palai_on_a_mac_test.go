// Guards for the Mac deployment page. Same rule as the rest of tests/docs: a document that CLAIMS
// something about the tree is checked against the tree.
//
// This page is unusual in that it is an AGENT instruction file as much as an operator one — a model
// reads §5 and writes argv from it. So the two things that must not rot are (a) the evidence table,
// which names the tests that prove each claim, and (b) the posture strings, which the binary parses
// byte for byte. Untagged and Docker-free, so it rides `make verify`.
package docs_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const macHostDoc = "docs/operations/palai-on-a-mac.md"

// TestPalaiOnAMacCitesTestsThatExist walks every `TestX` the page names and fails on one this tree
// cannot produce. The page's evidence table is its whole claim to being true rather than merely
// confident.
func TestPalaiOnAMacCitesTestsThatExist(t *testing.T) {
	doc := readDoc(t, macHostDoc)
	root := repoRoot(t)

	declared := map[string]bool{}
	for _, m := range regexp.MustCompile("`(Test[A-Za-z0-9_]+)`").FindAllStringSubmatch(doc, -1) {
		declared[m[1]] = true
	}
	if len(declared) < 8 {
		t.Fatalf("%s cites only %d test names — the evidence table has been gutted or the parser is broken", macHostDoc, len(declared))
	}

	// One walk, all names: reading the tree once beats grepping it per name.
	found := map[string]bool{}
	funcRe := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, m := range funcRe.FindAllStringSubmatch(string(b), -1) {
			found[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk tree: %v", err)
	}
	for name := range declared {
		if !found[name] {
			t.Errorf("%s cites %s, which no test in this tree defines", macHostDoc, name)
		}
	}
}

// TestPalaiOnAMacQuotesThePostureTheBinaryParses pins the strings an operator copies out of this page
// against the ones the binary actually accepts. A page that documents a value the binary refuses is
// worse than no page: the operator's stack fails to boot and the page says it should have worked.
func TestPalaiOnAMacQuotesThePostureTheBinaryParses(t *testing.T) {
	doc := readDoc(t, macHostDoc)
	main := readDoc(t, "apps/control-plane/cmd/palai-control-plane/main.go")

	for _, literal := range []string{"PALAI_SHELL_NATIVE", "unsandboxed-host", "PALAI_SANDBOX_IMAGE", "PALAI_WORKSPACE_ROOT"} {
		if !strings.Contains(doc, literal) {
			t.Errorf("%s does not mention %s — an operator cannot configure what the page will not name", macHostDoc, literal)
		}
		if !strings.Contains(main, literal) {
			t.Errorf("%s documents %s, which the control-plane binary does not read", macHostDoc, literal)
		}
	}

	// The allow-list is the page's sharpest promise to an agent AND its sharpest promise to an
	// operator; it must match the runner's own list, name for name.
	exec := readDoc(t, "adapters/sandboxes/host/exec.go")
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "LANG", "DEVELOPER_DIR"} {
		if !strings.Contains(exec, `"`+name+`"`) {
			t.Errorf("%s promises the command receives %s and the host runner does not pass it", macHostDoc, name)
		}
	}
	if !strings.Contains(doc, "PATH  HOME  TMPDIR  LANG  DEVELOPER_DIR") {
		t.Errorf("%s no longer states the environment allow-list verbatim", macHostDoc)
	}
}

// TestPalaiOnAMacKeepsTheHonestCeilingsVisible fails if the page is edited into a sales sheet. Each
// phrase below is a cost the epic took on deliberately; a Mac page that stops saying them is how an
// operator ends up believing there is a sandbox.
func TestPalaiOnAMacKeepsTheHonestCeilingsVisible(t *testing.T) {
	doc := readDoc(t, macHostDoc)
	for _, phrase := range []string{
		"The boundary is the uid", // §1, the whole cost in one clause
		"No egress denial",        // §1, the backstop that is gone
		"No resource bound",       // §1, the cgroup bounds that are gone
		"different Macs",          // §2, the operating rule
		"docs/research/macos-isolation-without-accounts.md", // §1-2, the measurement it rests on
		"private", // §5, axe's API surface
	} {
		if !strings.Contains(doc, phrase) {
			t.Errorf("%s no longer says %q", macHostDoc, phrase)
		}
	}
}
