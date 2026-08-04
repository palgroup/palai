package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
)

// THE MACHINE CREATES THE ALLOCATION NOW (A.3 T5), AND THE CREATION IS A CONTROL.
//
// Before this, a lease's workspace path had to already exist because the control plane made it — on
// the control plane's own disk, which is the same disk only when the two share a filesystem. The
// machine makes it instead, from a path the CONTROL PLANE named, which is exactly the input §30.13
// draws a boundary against. workspaceUnderRoot cannot help: it symlink-resolves both sides, and a
// directory that does not exist yet cannot be resolved.
//
// So resolveNewAllocationDir walks down instead, and these tests are about the walk.

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	// macOS puts t.TempDir() behind /var -> /private/var. Resolving it here is what lets these tests
	// compare one spelling of a path against another; an unresolved root would make every assertion
	// below compare two names for the same directory.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

// TestTheMachineCreatesAnAllocationTheControlPlaneNamedUnderItsRoot is the feature: a lease whose
// allocation is not there yet is served, not rejected, and what appears is the §29.9 layout.
func TestTheMachineCreatesAnAllocationTheControlPlaneNamedUnderItsRoot(t *testing.T) {
	root := resolvedTempDir(t)
	lease := Lease{WorkspaceHostPath: filepath.Join(root, "alloc_new")}

	dir, err := openLeaseWorkspace(lease, root, false)
	if err != nil {
		t.Fatalf("openLeaseWorkspace: %v", err)
	}
	if dir != filepath.Join(root, "alloc_new") {
		t.Fatalf("opened %q, want the allocation under this machine's root", dir)
	}
	for _, sub := range []string{workspace.RepoDir, workspace.ScratchDir, workspace.ArtifactsDir} {
		if info, serr := os.Stat(filepath.Join(dir, sub)); serr != nil || !info.IsDir() {
			t.Fatalf("the §29.9 layout is missing %s (stat: %v)", sub, serr)
		}
	}
	// IDEMPOTENT, because a later run in the same session reuses the same allocation and its lease
	// arrives at a directory that is already full of a prior run's edits.
	seeded := filepath.Join(dir, workspace.RepoDir, "kept.txt")
	if werr := os.WriteFile(seeded, []byte("a prior run wrote this\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	if _, err = openLeaseWorkspace(lease, root, false); err != nil {
		t.Fatalf("second open: %v", err)
	}
	if _, serr := os.Stat(seeded); serr != nil {
		t.Fatalf("re-opening the allocation lost a prior run's file: %v", serr)
	}
}

// TestTheMachineWillNotCreateAnAllocationThroughASymlink is the control, and it is the one MkdirAll
// would not have. MkdirAll follows an existing symlink component without complaint, so a control
// plane naming <root>/link/alloc — where `link` points at somewhere else entirely — would have the
// machine create the allocation THERE and then bind-mount it into an engine.
//
// The test writes into the symlink's target afterwards to prove nothing was created in it: an
// assertion that only checked the error would pass against a walk that refused AFTER creating.
func TestTheMachineWillNotCreateAnAllocationThroughASymlink(t *testing.T) {
	root := resolvedTempDir(t)
	elsewhere := resolvedTempDir(t)
	if err := os.Symlink(elsewhere, filepath.Join(root, "link")); err != nil {
		t.Skipf("this platform will not make a symlink: %v", err)
	}

	_, err := openLeaseWorkspace(Lease{WorkspaceHostPath: filepath.Join(root, "link", "alloc_planted")}, root, false)
	if err == nil {
		t.Fatal("the machine created an allocation through a symlink out of its managed root")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("refusal = %v, want one naming the component that is not a directory", err)
	}
	if _, serr := os.Stat(filepath.Join(elsewhere, "alloc_planted")); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("the allocation was created in the symlink's target anyway (stat: %v)", serr)
	}
}

// TestTheMachineWillNotCreateAnAllocationOutsideItsRoot covers the lexical half: a `..` that climbs
// out, and an absolute path somewhere else entirely.
func TestTheMachineWillNotCreateAnAllocationOutsideItsRoot(t *testing.T) {
	root := resolvedTempDir(t)
	outside := resolvedTempDir(t)

	for _, tc := range []struct {
		name string
		path string
	}{
		{"a traversal out of the root", filepath.Join(root, "..", "escaped")},
		{"another directory entirely", filepath.Join(outside, "alloc_x")},
		{"the root itself", root},
		{"a relative path", "alloc_x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := openLeaseWorkspace(Lease{WorkspaceHostPath: tc.path}, root, false); err == nil {
				t.Fatalf("the machine accepted %q", tc.path)
			}
		})
	}
}

// TestAnUnsafeLocalBindIsHonouredWithoutBeingCreated pins the one asymmetry. A §30.13 direct host
// bind (REP-012) names a directory the OPERATOR already has and deliberately placed outside the
// managed root, so there is nothing to mint: creating it would turn a typo into a new empty directory
// somewhere on their disk, mounted as if it were their project.
func TestAnUnsafeLocalBindIsHonouredWithoutBeingCreated(t *testing.T) {
	root := resolvedTempDir(t)
	project := resolvedTempDir(t)
	lease := Lease{WorkspaceHostPath: project, WorkspaceUnsafe: true}

	dir, err := openLeaseWorkspace(lease, root, true)
	if err != nil {
		t.Fatalf("an opted-in unsafe bind was refused: %v", err)
	}
	if dir != project {
		t.Fatalf("opened %q, want the operator's own directory %q", dir, project)
	}
	if _, serr := os.Stat(filepath.Join(project, workspace.RepoDir)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("the machine laid a workspace out inside the operator's directory (stat: %v)", serr)
	}
	// And without the opt-in it is refused, which is the §24 half: a control plane alone cannot make
	// this runner mount an arbitrary host path.
	if _, err := openLeaseWorkspace(lease, root, false); err == nil {
		t.Fatal("an unsafe bind was honoured by a runner that never opted in")
	}
}

// TestAWorkspacelessLeaseOpensNothing keeps the pre-E09 path byte-identical: every non-coding run
// carries no workspace, and there is nothing to place.
func TestAWorkspacelessLeaseOpensNothing(t *testing.T) {
	dir, err := openLeaseWorkspace(Lease{}, resolvedTempDir(t), false)
	if err != nil || dir != "" {
		t.Fatalf("openLeaseWorkspace(no workspace) = %q, %v; want \"\", nil", dir, err)
	}
}
