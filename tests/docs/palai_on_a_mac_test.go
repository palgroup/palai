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

// macOperatingRule is the whole of E22 T2's answer to "can two customers share a Mac". It is asserted
// WORD FOR WORD in both operator pages because the two halves are not separable: the second sentence
// reads as a general permission without the first, and the first reads as a prohibition on
// concurrency without the second.
const macOperatingRule = "Different customers → different Macs (or different uids). " +
	"Same customer → one Mac, per-session directories plus `simctl --set`."

// collapse folds every whitespace run to a single space, so a rule quoted verbatim may still be
// WRAPPED to fit a markdown page. Wrapping is not an edit; a changed word is.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestTheMacOperatingRuleIsVerbatimInBothOperatorPages is the one guard E22 T2 needs most, because
// the deliverable is a SENTENCE rather than a mechanism. The per-session separation this task builds
// is accident prevention and nothing more (research T22: any same-uid process can point `simctl
// --set` at another session's device set), so the rule is the only thing standing between two
// customers on one machine. A page that carries the code but drops the sentence has shipped the
// weaker half and kept the stronger name.
func TestTheMacOperatingRuleIsVerbatimInBothOperatorPages(t *testing.T) {
	want := collapse(macOperatingRule)
	for _, page := range []string{macHostDoc, "docs/operations/known-gaps-1.0.md"} {
		doc := collapse(readDoc(t, page))
		if !strings.Contains(doc, want) {
			t.Errorf("%s does not carry the operating rule verbatim:\n\t%s", page, want)
		}
		// A rule with no provenance is an opinion. Both pages must name the measurement and its date.
		for _, cite := range []string{"docs/research/macos-isolation-without-accounts.md", "2026-07-28"} {
			if !strings.Contains(doc, cite) {
				t.Errorf("%s states the operating rule without citing %q", page, cite)
			}
		}
	}
}

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
	// 607cf84f moved posture derivation out of main.go so BOTH binaries (control plane and runner) read
	// one answer instead of two: main.go's own shellPostureNative became `= posture.Native`, a reference
	// rather than a literal, and posture.Native is itself `string(toolbroker.PostureUnsandboxedHost)` —
	// so the literal "unsandboxed-host" now lives in packages/tool-broker/background.go, the one place
	// both postures compare against. PALAI_SHELL_NATIVE/PALAI_SANDBOX_IMAGE/PALAI_WORKSPACE_ROOT are
	// still read by os.Getenv directly in main.go. "main" is the union of wherever each literal is
	// actually declared today, not a claim that one file holds all four.
	main := readDoc(t, "apps/control-plane/cmd/palai-control-plane/main.go") +
		readDoc(t, "packages/tool-broker/background.go")

	for _, literal := range []string{"PALAI_SHELL_NATIVE", "unsandboxed-host", "PALAI_SANDBOX_IMAGE", "PALAI_WORKSPACE_ROOT"} {
		if !strings.Contains(doc, literal) {
			t.Errorf("%s does not mention %s — an operator cannot configure what the page will not name", macHostDoc, literal)
		}
		if !strings.Contains(main, literal) {
			t.Errorf("%s documents %s, which the control-plane binary does not read", macHostDoc, literal)
		}
	}

	// The allow-list is the page's sharpest promise to an agent AND its sharpest promise to an
	// operator; it must match the runner's own list, name for name. Since E22 T2 the list has two
	// halves — three variables INHERITED from the control plane and four DERIVED from the run's own
	// allocation — and the page states both, because an agent that believes HOME is the operator's
	// home writes argv against the wrong directory.
	//
	// PALAI_DERIVED_DATA joined the derived half in Faz A.5 T4, and it is the one whose ABSENCE from
	// this page costs the most: measured 2026-08-05, `HOME` does not redirect DerivedData, so an agent
	// that is not told to pass `-derivedDataPath` writes gigabytes into the operator's own ~/Library.
	exec := readDoc(t, "adapters/sandboxes/host/exec.go")
	for _, name := range []string{"PATH", "LANG", "DEVELOPER_DIR", "HOME", "TMPDIR", "PALAI_SIMCTL_SET", "PALAI_DERIVED_DATA"} {
		if !strings.Contains(exec, `"`+name+`"`) {
			t.Errorf("%s promises the command receives %s and the host runner does not pass it", macHostDoc, name)
		}
		if !strings.Contains(doc, name) {
			t.Errorf("%s does not name %s, which the host runner puts in every command's environment", macHostDoc, name)
		}
	}
	if !strings.Contains(doc, "PATH  LANG  DEVELOPER_DIR") || !strings.Contains(doc, "HOME  TMPDIR  PALAI_SIMCTL_SET  PALAI_DERIVED_DATA") {
		t.Errorf("%s no longer states the environment allow-list verbatim, inherited half and derived half", macHostDoc)
	}

	// The per-session directory name is DUPLICATED rather than imported: the oci workspace package
	// pulls in the whole Docker client and the native posture's executor is the one package that must
	// not. Two spellings that drift silently would put a run's HOME back into every snapshot.
	if !strings.Contains(exec, `sessionDir = ".palai-session"`) ||
		!strings.Contains(readDoc(t, "adapters/sandboxes/oci/workspace/allocation.go"), `SessionDir = ".palai-session"`) {
		t.Error("the per-session directory name no longer agrees between the host executor and the snapshot walker")
	}
}

// TestPalaiOnAMacKeepsTheHonestCeilingsVisible fails if the page is edited into a sales sheet. Each
// phrase below is a cost the epic took on deliberately; a Mac page that stops saying them is how an
// operator ends up believing there is a sandbox.
func TestPalaiOnAMacKeepsTheHonestCeilingsVisible(t *testing.T) {
	doc := readDoc(t, macHostDoc)
	for _, phrase := range []string{
		"the boundary is the uid", // §1, the whole cost in one clause
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
