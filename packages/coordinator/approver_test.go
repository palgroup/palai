package coordinator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// E23 T2 — the canonical approver principal, and the proof that the check on it lives in ONE place.

// TestApproverPrincipalRendersEachSurfacesIdentity pins the two forms an operator writes into
// config_policy.approvers. The Slack form is WORKSPACE-QUALIFIED and that is load bearing: a Slack user id
// is unique only within its workspace, so an unqualified id in a list would admit a stranger from another
// workspace who happens to share it.
func TestApproverPrincipalRendersEachSurfacesIdentity(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		surface, workspace, sid string
		want                    string
	}{
		{"slack", ApproverSurfaceSlack, "T0123ABCD", "U0123ABCD", "slack:T0123ABCD:U0123ABCD"},
		{"api key", ApproverSurfaceKey, "", "key_9f2c", "key:key_9f2c"},

		// Every way an identity can fail to be one renders "", which no list can name.
		{"slack with no workspace", ApproverSurfaceSlack, "", "U0123ABCD", ""},
		{"slack with no user", ApproverSurfaceSlack, "T0123ABCD", "", ""},
		{"key with no id", ApproverSurfaceKey, "", "", ""},
		{"an unknown surface", "webhook", "", "whatever", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApproverPrincipal(tc.surface, tc.workspace, tc.sid); got != tc.want {
				t.Fatalf("ApproverPrincipal(%q,%q,%q) = %q, want %q", tc.surface, tc.workspace, tc.sid, got, tc.want)
			}
		})
	}
}

// TestApproverAllowedIsDenyByDefaultOnlyOnceAListExists is the whole policy in one table, and the second
// row is the one that is not negotiable: NO list means BIT-UNCHANGED, so every deployment that has not
// configured this behaves exactly as it did.
func TestApproverAllowedIsDenyByDefaultOnlyOnceAListExists(t *testing.T) {
	for _, tc := range []struct {
		name      string
		approvers []string
		principal string
		want      bool
	}{
		{"no list admits anyone, including nobody", nil, "", true},
		{"no list admits a named principal", nil, "key:key_1", true},
		{"an empty list is still no list", []string{}, "key:key_1", true},

		{"a list admits what it names", []string{"key:key_1", "slack:T1:U1"}, "slack:T1:U1", true},
		{"a list refuses what it does not name", []string{"key:key_1"}, "key:key_2", false},
		{"a list refuses an unidentified caller", []string{"key:key_1"}, "", false},
		{"a list entry cannot be matched by a prefix", []string{"key:key_1"}, "key:key_1x", false},
		{"a list entry cannot be matched by a suffix", []string{"key:key_1"}, "xkey:key_1", false},

		// The fail-open trap: an operator writes an empty string into the list. It must not become a
		// wildcard for every caller whose identity did not resolve.
		{"an empty entry does not admit an unidentified caller", []string{""}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := ConfigPolicy{Approvers: tc.approvers}
			if got := p.ApproverAllowed(tc.principal); got != tc.want {
				t.Fatalf("ConfigPolicy{Approvers:%q}.ApproverAllowed(%q) = %v, want %v", tc.approvers, tc.principal, got, tc.want)
			}
		})
	}
}

// TestApproverAllowedHasExactlyOneProductionCallSite is the structural half of "the check goes in the one
// throat both surfaces pass through". Putting the check in each caller is how the NEXT caller forgets it,
// so the guard is not a comment: it counts.
//
// It scans every non-test .go file in the tree — recursively, unlike the package-local scan in
// adapters/integrations/slack/blocks_test.go, whose narrow reach is its own documented ceiling — and
// requires exactly one call per SUBJECT. A second call site is not necessarily wrong, but it is a second
// place to forget, and it must be argued for by editing this test.
//
// IT HAS BEEN ARGUED FOR ONCE, IN E24 T6, AND THE ARGUMENT IS THE SCOPE OF E23'S RULE. The rule is that a
// decision about a gated OPERATION goes through one function, `ApplyApprovalDecision` — whose subject is a
// tool call or a publication, keyed by a tool-call id and bound to a `request_hash` so that arguments
// changed after a human looked leave no approval. A RUNNER ENROLMENT has no arguments, no parked tool call
// and NO REQUEST HASH TO BIND TO: the certificate was issued before anybody was asked. Routing it through
// that throat would have meant fabricating a tool call and a hash per machine that boots, and the binding
// would then bind nothing. So `fleet.Approve` is a second PATH and not a second POLICY: it asks THIS
// function — the one definition of who may decide, including the asymmetry that an empty list permits
// everything — and it reimplements no part of it. If that rule ever changes it changes for tool calls,
// publications and machines together.
//
// THE COUNT IS STILL ONE PER FILE and that is the property worth keeping: this test went red on
// fleet/strict.go's first draft, which asked twice (once for a project row that was absent), and the fix
// was to make the zero-value policy carry that case. Two asks in one function are two places for the
// answer to differ.
func TestApproverAllowedHasExactlyOneProductionCallSite(t *testing.T) {
	root := repoRootForApproverScan(t)
	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // not parseable as Go (a testdata fixture); nothing to count
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ApproverAllowed" {
				rel, _ := filepath.Rel(root, path)
				found = append(found, rel)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	// One call site per SUBJECT, each named: the tool-call/publication throat, and the runner-enrolment path
	// whose subject has no request hash to bind to (see the header). A file appearing TWICE is a failure even
	// if it is on this list — two asks in one function are two places for the answer to differ — so the
	// comparison is on the exact slice rather than on a set.
	want := []string{
		filepath.Join("apps", "control-plane", "internal", "fleet", "strict.go"),
		filepath.Join("packages", "coordinator", "publication.go"),
	}
	sort.Strings(found)
	if !slices.Equal(found, want) {
		t.Fatalf("ApproverAllowed is called from %v, want exactly %v — the approver POLICY has one definition, "+
			"and a new call site is a new PATH that must be argued for in this test's header", found, want)
	}
}

// repoRootForApproverScan resolves the tree root and PROVES it is the root, because a scan rooted at the
// wrong directory finds nothing and passes vacuously.
func repoRootForApproverScan(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve repo root symlinks: %v", err)
	}
	// The anchor: this file's own package has to be under the root the walk will take, and go.mod has to
	// be at it. Either one alone is satisfiable by an empty directory.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s; the scan would be rooted somewhere that is not the tree: %v", root, err)
	}
	if _, err := os.Stat(filepath.Join(root, "packages", "coordinator", "publication.go")); err != nil {
		t.Fatalf("publication.go is not under %s; this scan cannot see the file it is about: %v", root, err)
	}
	return root
}
