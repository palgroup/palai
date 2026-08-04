package stack

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `palai local up` BINDS A REPOSITORY TOO, and this file is the gate on a defect whose whole shape was
// an absent call site. Measured 2026-08-04, before the fix:
//
//	grep -rn "resolveRepository" --include='*.go' cmd/ | grep -v _test   -> up.go:229 only
//	grep -c  "resolveRepository" cmd/cli/internal/stack/lifecycle.go     -> 0
//
// Nothing was broken, mis-spelled or mis-configured; the function existed, was correct, was tested, and
// the second bring-up simply never called it. That is why the guard below asks about REACHABILITY rather
// than about behaviour: a behaviour test of resolveRepository passed throughout, and would have gone on
// passing with `palai local up` binding nothing.
//
// It matters because of which bring-up it was. `palai up` REFUSES the deterministic adapter by name and
// points the operator at `palai local up` (resolveProvider) — so a credential-less stack was sent to the
// one command that could not give it a repository, and every workspace tool on it answered "no workspace
// bound for this run".

// TestLocalUpReachesTheRepositoryBinding is the regression gate proper: `palai local up` — stack.Up —
// must reach resolveRepository. It is a reachability question and not a string search, so moving the
// call behind another helper keeps it green and DELETING it does not.
func TestLocalUpReachesTheRepositoryBinding(t *testing.T) {
	calls := packageCallGraph(t)
	if !reaches(calls, "Up", "resolveRepository") {
		t.Fatal("`palai local up` (stack.Up) does not reach resolveRepository, so a local stack binds NO " +
			"repository: every run on it can read and search but cannot clone, edit or commit, and the only " +
			"symptom is workspace tools answering \"no workspace bound for this run\". This is the exact state " +
			"measured on 2026-08-04 — the function existed and nothing called it")
	}
}

// TestPalaiUpStillBindsExactlyOnce: Bootstrap resolves the binding itself, into its own report, so
// nothing it drives may resolve one as well. Two resolutions on one `palai up` would register the
// binding twice and print the line twice, which is a change to a command that was asked not to change.
//
// THE FIRST WRITING OF THIS TEST ASKED THE WRONG QUESTION AND A PERTURBATION CAUGHT IT. It asked
// whether composeUp reaches resolveRepository, which is a boolean, and the defect is a COUNT: swapping
// Bootstrap's composeUp back to Up left composeUp innocent, Bootstrap reaching resolveRepository by two
// paths, and this test green. The property that actually excludes it is that Up is a command ENTRY
// POINT — `palai local up` and nothing else — so no function in this package may call it. An internal
// caller that wants a stack wants composeUp; one that wants Up wants the reporting twice.
func TestPalaiUpStillBindsExactlyOnce(t *testing.T) {
	calls := packageCallGraph(t)
	if !reaches(calls, "Bootstrap", "resolveRepository") {
		t.Fatal("`palai up` (Bootstrap) no longer reaches resolveRepository: it has stopped binding a repository")
	}
	for fn, callees := range calls {
		if callees["Up"] {
			t.Fatalf("%s calls Up, which is `palai local up`'s entry point and resolves the repository binding "+
				"on its own. Every internal caller reaches the stack through composeUp, because a caller that "+
				"has its own report — Bootstrap does — would otherwise bind twice and say so twice", fn)
		}
	}
	if reaches(calls, "composeUp", "resolveRepository") {
		t.Fatal("composeUp reaches resolveRepository, and Bootstrap reaches it too — so `palai up` now binds " +
			"TWICE and reports it twice. composeUp is the bring-up half precisely so that the caller chooses " +
			"where the outcome is reported")
	}
}

// TestTheFakeRefusalPointsAtACommandThatCanBind ties the two halves together, because separately each
// one looks fine. `palai up` refusing the fake adapter is correct and deliberate; pointing at
// `palai local up` is the right place to point. What was wrong was the CONJUNCTION: the destination
// could not bind a repository, so following the advice landed the operator somewhere strictly less
// capable with nothing saying so.
func TestTheFakeRefusalPointsAtACommandThatCanBind(t *testing.T) {
	_, err := resolveProvider(env("PALAI_MODEL_PROVIDER", "fake", credentialEnv, "sk-test"), false)
	if err == nil {
		t.Fatal("`palai up` accepted the fake selector: the live round-trip it exists to prove is now unprovable")
	}
	if !strings.Contains(err.Error(), "palai local up") {
		t.Fatalf("the fake refusal no longer names `palai local up`, so this test cannot say anything about "+
			"where the operator is sent: %v", err)
	}
	if !reaches(packageCallGraph(t), "Up", "resolveRepository") {
		t.Fatal("`palai up` sends an operator on the deterministic adapter to `palai local up`, and that " +
			"command binds no repository — the advice is a downgrade the operator is not told about")
	}
}

// TestALocalBringUpNamesADeploymentThatCannotMountWhatItBound. Binding a repository on the compose
// posture is HONEST but not sufficient, and the difference is invisible: the binding row is real and
// durable, and every workspace tool still refuses.
//
// Measured 2026-08-04: `grep -c PALAI_WORKSPACE_ROOT deploy/compose/compose.yaml` -> 0. The variable is
// ABSENT from both `environment:` blocks rather than merely unset, so on that posture it cannot be
// supplied at all. The warning asks the SERVER (`workspaces` in GET /v1/capabilities derives from the
// same env the provisioner is gated on) instead of reading the file, so it reports the deployment that
// is running rather than the deployment this command assumes.
func TestALocalBringUpNamesADeploymentThatCannotMountWhatItBound(t *testing.T) {
	for _, tc := range []struct {
		name       string
		workspaces string
		bound      bool
		wantWarn   bool
	}{
		{name: "bound but unmountable", workspaces: "unavailable", bound: true, wantWarn: true},
		{name: "bound and mountable", workspaces: "available", bound: true, wantWarn: false},
		// Nothing was bound, so there is nothing to be wrong about — resolveRepository's own warning
		// already covers the unconfigured case and this one must not double up on it.
		{name: "nothing bound", workspaces: "unavailable", bound: false, wantWarn: false},
		// The read failed. A warning that cannot tell "cannot mount" from "could not ask" is the one an
		// operator learns to ignore, and then misses the real one.
		{name: "capabilities unreadable", workspaces: "", bound: true, wantWarn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := fakeCapabilitiesAPI(t, tc.workspaces)
			repo := repositoryBinding{}
			if tc.bound {
				repo.id = "repo_test"
			}
			got := workspacelessBindingWarning(api, repo)
			if (got != "") != tc.wantWarn {
				t.Fatalf("warning=%q, want warning=%v", got, tc.wantWarn)
			}
			if tc.wantWarn && !strings.Contains(got, "PALAI_WORKSPACE_ROOT") {
				t.Fatalf("the warning does not name the variable an operator would have to set: %q", got)
			}
		})
	}
}

// fakeCapabilitiesAPI serves GET /v1/capabilities with the given `workspaces` word, or 404 for an empty
// one — the shape a client with no route to ask meets.
func fakeCapabilitiesAPI(t *testing.T, workspaces string) *apiClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if workspaces == "" || r.URL.Path != "/v1/capabilities" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"capabilities": map[string]string{"workspaces": workspaces, "responses": "preview"},
		})
	}))
	t.Cleanup(srv.Close)
	return &apiClient{baseURL: srv.URL, key: "test", http: &http.Client{Timeout: 5 * time.Second}}
}

// packageCallGraph maps each top-level function in THIS package to the names it calls. Test files are
// excluded: a call that exists only in a test proves nothing about what the command does.
func packageCallGraph(t *testing.T) map[string]map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	graph := map[string]map[string]bool{}
	parsed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil {
				continue
			}
			callees := graph[fn.Name.Name]
			if callees == nil {
				callees = map[string]bool{}
				graph[fn.Name.Name] = callees
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// Package-local calls only: `pkg.Fn` is somebody else's graph, and a selector on a value
				// is a method this walk deliberately does not follow.
				if id, ok := call.Fun.(*ast.Ident); ok {
					callees[id.Name] = true
				}
				return true
			})
		}
	}
	// A walk that parsed nothing would answer "unreachable" for every question and read as a pass on the
	// inverted guard — the vacuous-scan shape this tree keeps paying for.
	if parsed == 0 {
		t.Fatal("the call-graph walk parsed ZERO files, so every reachability answer below is vacuous")
	}
	if _, ok := graph["Up"]; !ok {
		t.Fatal("the call-graph walk found no function named Up, so it is looking at the wrong package")
	}
	return graph
}

// reaches reports whether `from` calls `target`, directly or through any package-local function.
func reaches(graph map[string]map[string]bool, from, target string) bool {
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for callee := range graph[cur] {
			if callee == target {
				return true
			}
			if !seen[callee] {
				seen[callee] = true
				queue = append(queue, callee)
			}
		}
	}
	return false
}
