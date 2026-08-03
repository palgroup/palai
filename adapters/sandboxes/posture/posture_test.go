package posture

import (
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
)

// TestRefusesAPostureNobodyCouldReadOffTheDeployment pins the two configurations that make "where did
// this command run" unanswerable: both variables set, and a PALAI_SHELL_NATIVE that is not the exact
// declaration. `=1` is what an operator types to switch a feature on, and deleting the sandbox boundary
// is not a feature.
func TestRefusesAPostureNobodyCouldReadOffTheDeployment(t *testing.T) {
	native, err := Resolve("palai/sandbox@sha256:"+strings.Repeat("a", 64), Native)
	if err == nil {
		t.Fatalf("both postures declared and it was accepted (native=%v)", native)
	}
	if !strings.Contains(err.Error(), "PALAI_SANDBOX_IMAGE") || !strings.Contains(err.Error(), "PALAI_SHELL_NATIVE") {
		t.Fatalf("the refusal does not name both variables an operator must fix: %v", err)
	}

	for _, value := range []string{"1", "true", "yes", "on", "TRUE", "unsandboxed_host", "UNSANDBOXED-HOST", " unsandboxed-host", "unsandboxed-host "} {
		if native, err := Resolve("", value); err == nil || native {
			t.Fatalf("PALAI_SHELL_NATIVE=%q was accepted as the unsandboxed host posture", value)
		}
	}

	// No posture at all is the DEFAULT and not an error — it is how every pre-E22 deployment runs.
	if native, err := Resolve("", ""); err != nil || native {
		t.Fatalf("no posture declared = %v, %v; want (false, nil)", native, err)
	}
}

// TestTheNativePostureIsBoundedRatherThanUnbounded is the one number that had been written down twice.
// Zero is not neutral on this seam — it is UNBOUNDED (adapters/sandboxes/host/exec.go) — so a machine
// that took its wall time from an unset variable would bound a command less tightly than the control
// plane that asked for it, and neither side would say so.
func TestTheNativePostureIsBoundedRatherThanUnbounded(t *testing.T) {
	t.Setenv("PALAI_SANDBOX_IMAGE", "")
	t.Setenv("PALAI_SHELL_NATIVE", Native)
	t.Setenv("PALAI_SANDBOX_WALL_TIME", "")

	shell, err := RunnerFromEnv()
	if err != nil {
		t.Fatalf("the native posture was refused: %v", err)
	}
	executor, ok := shell.(*host.Executor)
	if !ok {
		t.Fatalf("the native posture built %T, want the host executor", shell)
	}
	if executor.WallTime() != DefaultWallTime {
		t.Fatalf("wall time = %v, want %v — an unset variable must not mean unbounded", executor.WallTime(), DefaultWallTime)
	}
	if Limits().WallTime != DefaultWallTime {
		t.Fatalf("the container posture's wall time = %v, want the same %v the host posture got", Limits().WallTime, DefaultWallTime)
	}

	// A declared value is honoured on both postures, from the one derivation.
	t.Setenv("PALAI_SANDBOX_WALL_TIME", "90s")
	if got := WallTime(); got != 90*time.Second {
		t.Fatalf("PALAI_SANDBOX_WALL_TIME=90s read as %v", got)
	}
	if got := Limits().WallTime; got != 90*time.Second {
		t.Fatalf("the container posture read PALAI_SANDBOX_WALL_TIME as %v", got)
	}
}

// TestAnUndeclaredPostureIsNilAndNotAnError holds the nil discipline both roots depend on: an
// unconfigured deployment gets no executor and its shell tool refuses cleanly, which is different from
// a REFUSAL and must stay different — a caller that conflated them could fall back to the other
// posture, which is how a deleted security boundary arrives by accident.
func TestAnUndeclaredPostureIsNilAndNotAnError(t *testing.T) {
	t.Setenv("PALAI_SANDBOX_IMAGE", "")
	t.Setenv("PALAI_SHELL_NATIVE", "")
	shell, err := RunnerFromEnv()
	if err != nil {
		t.Fatalf("no posture declared was reported as an error: %v", err)
	}
	if shell != nil {
		t.Fatalf("no posture declared and an executor was built: %T", shell)
	}
}

// TestNeitherCompositionRootBuildsItsOwnExecutor is the guard for the defect this package exists to
// remove, and it reads the SHIPPED files rather than a description of them.
//
// The OCI posture carries four bounds and the host posture one, and until A.3 the control plane derived
// them and the runner derived a subset of them separately — with a comment in the second saying its
// number MUST match the first, which is the shape of a number that eventually will not. A bound that
// disagrees between two composition roots is a containment an operator cannot reason about, because
// nothing on either machine names which one applied.
//
// A missing file FAILS rather than passing quietly: a guard that cannot find what it checks has not
// checked it.
func TestNeitherCompositionRootBuildsItsOwnExecutor(t *testing.T) {
	root := repoRoot(t)
	roots := []string{
		"apps/control-plane/cmd/palai-control-plane/main.go",
		"cmd/runner/main.go",
	}
	// Constructing an executor, or spelling the bounds one is built from, is the copy this forbids.
	forbidden := []string{
		"host.NewExecutor(",
		"workspace.NewShellExecutor(",
		"oci.Limits{",
		"PALAI_SANDBOX_MAX_MEMORY_BYTES",
		"PALAI_SANDBOX_MAX_PROCS",
		"PALAI_SANDBOX_NANO_CPUS",
	}
	for _, rel := range roots {
		code := codeWithoutComments(t, filepath.Join(root, rel))
		// The guard's own liveness: a parse that yielded nothing would clear every check below while
		// having read no composition root at all. Every root declares main.
		if !strings.Contains(code, "func main()") {
			t.Fatalf("%s printed %d bytes with no func main(): this guard read nothing and would pass on anything", rel, len(code))
		}
		for _, token := range forbidden {
			if strings.Contains(code, token) {
				t.Errorf("%s spells %q itself: the sandbox's bounds must come from adapters/sandboxes/posture, "+
					"so the two roots cannot disagree about what contained a command", rel, token)
			}
		}
	}
}

// codeWithoutComments returns a file's source with its comments removed, so the guard above judges what
// the root DOES and not what it says about itself. A raw byte scan would flag the doc comment that
// names the very variables it delegates — which is a true sentence about a function that reads none of
// them, and failing on it would teach the next reader to delete an accurate comment.
func codeWithoutComments(t *testing.T, path string) string {
	t.Helper()
	fset := token.NewFileSet()
	// Parsed WITHOUT ParseComments, so printing the AST back yields code alone.
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("cannot parse the composition root %s: %v (a guard that cannot read what it checks has not checked it)", path, err)
	}
	var code strings.Builder
	if err := printer.Fprint(&code, fset, file); err != nil {
		t.Fatalf("cannot print %s: %v", path, err)
	}
	return code.String()
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory, so the repository root is unknown")
		}
		dir = parent
	}
}
