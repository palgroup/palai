//go:build component

// THE RESIDUE GUARD (Faz A.5 T2). One tenant's occupancy of one machine ends; nothing of that tenant
// may still be on the disk the next one gets.
//
// WHY IT EXISTS. On 2026-08-05 A.5 T4 MEASURED this on a real Mac: a marker planted through a real
// xcodebuild, `/usr/bin/grep -ral` answering 40 files before the cleanup and 0 after. The evidence was
// real and it was taken BY HAND — one machine, one marker, one pass — and nothing re-ran it. This
// tree's own rule for that is flat: a measurement taken once by hand is not a guard. These tests are
// that measurement turned into something that fires on every run of the tier.
//
// ‼️ THE POSITIVE CONTROL IS THE POINT, NOT DECORATION. The silent death of a residue test is that it
// scans paths nothing ever wrote to, finds 0, goes green, and says nothing for years. So every drive
// below is three-phase: PLANT a known marker through the shipped writer, SCAN BEFORE the release and
// ASSERT THE COUNT IS NON-ZERO, then release and scan again. The middle phase is the only line that
// proves the scan in the third phase can see anything at all; without it a build that plants nothing
// and a build that cleans everything are indistinguishable.
//
// ‼️ AND THE CLASSES ARE READ, NEVER RESTATED. testdata/residue_classes.json carries them, and the
// allocation-scoped half is cross-checked against the SHIPPED canonical list (host.SessionDirs) in
// BOTH directions. A list hard-coded here would miss the directory the toolchain starts writing
// tomorrow and stay green — the shape this tree has recorded more than any other: a sweep that narrows
// silently still reports green. An empty or unreadable fixture is a t.Fatal, never a quiet zero-class
// scan.
//
// TWO POSTURES, AND ONLY ONE IS DRIVABLE TODAY.
//
//   - ACCOUNTLESS (driven here): allocation-scoped removal. Everything this posture points a tenant's
//     tools at lives under the allocation root, and both shipped release paths reclaim that root —
//     IdleReleaser.release and WorkspaceRecovery.DestroyAllocation, each through `r.remove(hostPath)`.
//     Both are driven below, because a guard on one of them would leave the other free to stop
//     deleting.
//   - ACCOUNT (a CEILING, named in these tests' names and measured in the fixture): the per-uid
//     surfaces — the darwin bucket, /private/var/db/mds/messages/<uid>, and the operator's own
//     ~/Library, which HOME does not redirect. Their remover is palai-agentd's `delete <slot>`, and on
//     2026-08-05 the daemon was installed on NO machine while `sudo` on the measuring host was
//     passwordless for exactly two commands. No account has ever been created or destroyed here, so no
//     instance of that removal can be driven and none is claimed. The composition of that half is
//     tested where it can be: cmd/palai-agentd's own suite.
//
// WHY THIS IS NOT tests/component/fleet/residue_test.go, WHICH IS WHERE THE PLAN PUT IT. Because the
// compiler refuses, and the refusal was measured rather than assumed (2026-08-05, a probe file added
// to that package and deleted):
//
//	tests/component/fleet/zz_probe_internal_test.go:5:8: use of internal package
//	github.com/palgroup/palai/apps/control-plane/internal/execution not allowed
//
// A guard that cannot import `execution` cannot call either release path, and a residue test that
// removes the directory ITSELF proves nothing about the product — it would stay green on the day
// `r.remove(hostPath)` is deleted, which is the one perturbation it exists to fail. The tree already
// resolved this exact tension once and wrote the resolution down in scripts/test/component: "internal
// packages, so their component tests cannot live under tests/". This package is the one that already
// holds both drivers AND runs with no -run allow-list (`-run "${PALAI_SUITE_RUN:-.}"`), so a test added
// beside these is a test that runs — the property the fleet leg was chosen for in the first place.
//
// WHY t.Fatal AND NOT t.Skip, AND WHY NOTHING HERE NEEDS A MAC. The plan asked for Fatal because a
// hygiene test that SKIPS is a gate that looks green. That is honoured, and it costs nothing: the
// drivable posture is a directory removal, so these run on any platform the tier runs on. The Mac-only
// surfaces are the ceilings, and a ceiling is asserted as a written measurement, never scanned — a
// guard that failed on the system unified log would be wrong about the product rather than right about
// the machine. What IS Fatal here is the tier's own precondition: the suite's other tests skip without
// PALAI_COMPONENT_POSTGRES_URL / PALAI_S3_ENDPOINT, and on 2026-08-05 this tree caught a test whose
// `ok` was hiding exactly that skip three times over.
package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// residueFixture is the measured inventory. It is a path rather than an embed so that a run whose
// working tree lost the file FAILS instead of compiling a stale copy in.
const residueFixture = "testdata/residue_classes.json"

// residueMarkerFile is the basename planted into every allocation-scoped class. One file per class, so
// a scan's count is directly comparable to the class count.
const residueMarkerFile = "residue-marker"

// residueClass is one measured place a tenant's occupancy leaves bytes. See the fixture's _comment for
// what each closed_by value MEANS — the mechanism, not a wish.
type residueClass struct {
	ID       string `json:"id"`
	Scope    string `json:"scope"`
	Env      string `json:"env"`
	Rel      string `json:"rel"`
	Path     string `json:"path"`
	ClosedBy string `json:"closed_by"`
	Ceiling  string `json:"ceiling"`
}

type residueInventory struct {
	MeasuredOn string         `json:"measured_on"`
	Source     string         `json:"source"`
	Classes    []residueClass `json:"classes"`
}

const (
	closedByAllocation = "allocation_scoped_removal"
	closedByAccount    = "session_account"
	closedByNothing    = "none"
)

// loadResidueInventory reads the fixture. Every failure mode here is FATAL and none is a skip: a
// residue guard that cannot read its class list must not go on to scan zero classes and report clean.
func loadResidueInventory(t *testing.T) residueInventory {
	t.Helper()
	raw, err := os.ReadFile(residueFixture)
	if err != nil {
		t.Fatalf("read residue class fixture %s: %v — a residue guard with no class list scans nothing and reports clean", residueFixture, err)
	}
	var inv residueInventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatalf("parse residue class fixture %s: %v", residueFixture, err)
	}
	if len(inv.Classes) == 0 {
		t.Fatalf("residue class fixture %s names 0 classes — every scan below would be vacuous", residueFixture)
	}
	return inv
}

// allocationScopedClasses returns the classes the accountless posture really closes, AFTER checking
// them against the shipped canonical list in BOTH directions.
//
// BOTH DIRECTIONS, BECAUSE THE TWO ERRORS ARE DIFFERENT. A fixture entry production dropped is a
// stale claim; a production entry the fixture never named is an UNGUARDED surface, and that is the one
// that reports green. A fifth variable added to host.sessionDirs tomorrow reddens this immediately,
// and the only way to green it is to name the new class here and let the drives below prove it is
// removed.
func allocationScopedClasses(t *testing.T, inv residueInventory) []residueClass {
	t.Helper()
	var classes []residueClass
	roots := 0
	got := map[string]string{}
	for _, c := range inv.Classes {
		if c.Scope != "allocation" {
			continue
		}
		if c.ClosedBy != closedByAllocation {
			t.Fatalf("class %s is allocation-scoped but closed_by=%q — an allocation-scoped class IS reclaimed with the directory", c.ID, c.ClosedBy)
		}
		if c.Rel == "" {
			t.Fatalf("class %s is allocation-scoped with no rel — there is nowhere to plant or scan", c.ID)
		}
		if filepath.IsAbs(c.Rel) || c.Rel != filepath.Clean(c.Rel) || strings.HasPrefix(c.Rel, "..") {
			t.Fatalf("class %s rel=%q is not a clean allocation-relative path", c.ID, c.Rel)
		}
		if c.Env == "" {
			roots++
			if c.Rel != "." {
				t.Fatalf("class %s names no variable, so it is the allocation root itself and rel must be %q, not %q", c.ID, ".", c.Rel)
			}
		} else {
			got[c.Env] = c.Rel
		}
		classes = append(classes, c)
	}
	if len(classes) == 0 {
		t.Fatal("the fixture names no allocation-scoped class, so nothing below would be planted, scanned, or proven")
	}
	if roots != 1 {
		t.Fatalf("the fixture names %d allocation-root classes, want exactly 1 — the root is where a tenant's repo and any staged credential live", roots)
	}

	want := map[string]string{}
	for _, d := range host.SessionDirs() {
		want[d[0]] = filepath.Join(host.SessionDirRoot(), d[1])
	}
	for name, rel := range want {
		if got[name] != rel {
			t.Fatalf("the shipped posture derives %s=<allocation>/%s and the residue fixture says %q — an unnamed session directory is an UNGUARDED surface, which is how a narrowing sweep keeps reporting green. Add it to %s and let the drives below prove it is removed.",
				name, rel, got[name], residueFixture)
		}
	}
	for name, rel := range got {
		if _, ok := want[name]; !ok {
			t.Fatalf("the residue fixture names %s=<allocation>/%s and the shipped posture derives no such variable — a stale class makes the count below mean less than it reads",
				name, rel)
		}
	}
	return classes
}

// requireResidueHarness is openArtifactsHarness with the SKIP taken out. The rest of this suite skips
// when the tier's services are absent, which is right for a write-path test and wrong for a hygiene
// one: a hygiene proof that quietly does not run is a gate that looks green, and on 2026-08-05 this
// tree measured a component test reporting `ok` three times in a row while skipping every time.
func requireResidueHarness(t *testing.T) *artifactsHarness {
	t.Helper()
	if os.Getenv("PALAI_COMPONENT_POSTGRES_URL") == "" || os.Getenv("PALAI_S3_ENDPOINT") == "" {
		t.Fatal("PALAI_COMPONENT_POSTGRES_URL and PALAI_S3_ENDPOINT are required: run `make test-component TEST=artifacts`. This one FAILS rather than skipping — an unrun residue proof is a green gate over an unmeasured disk.")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required to lay down a real allocation: %v — skipping here would report a clean disk nobody looked at", err)
	}
	return openArtifactsHarness(t)
}

// plantTenantResidue writes a known marker into every allocation-scoped class THROUGH THE SHIPPED
// WRITER — adapters/sandboxes/host's Executor, the same one a run's shell tool goes through — and
// returns where each file landed.
//
// IT USES THE PRODUCT'S OWN VARIABLES ($HOME, $TMPDIR, $PALAI_SIMCTL_SET, $PALAI_DERIVED_DATA) RATHER
// THAN JOINING PATHS ITSELF, which is what makes the existence check at the end an assertion rather
// than a tautology: if the posture ever pointed one of them outside the allocation, the file would land
// outside and the expected path would be missing. That is not hypothetical — measured 2026-08-05, HOME
// does NOT redirect what the Apple toolchain writes, which is why the operator's DerivedData is a
// ceiling in the fixture and not a class this drives.
func plantTenantResidue(t *testing.T, hostPath, marker string, classes []residueClass) []string {
	t.Helper()
	stmts := make([]string, 0, len(classes))
	planted := make([]string, 0, len(classes))
	for _, c := range classes {
		target := "./" + residueMarkerFile
		if c.Env != "" {
			target = `"${` + c.Env + `}"/` + residueMarkerFile
		}
		// The marker is hex, so single quotes carry it with nothing for the shell to interpret.
		stmts = append(stmts, "printf %s '"+marker+"' > "+target)
		planted = append(planted, filepath.Join(hostPath, c.Rel, residueMarkerFile))
	}
	res, err := host.NewExecutor(2*time.Minute).Run(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"set -e; " + strings.Join(stmts, "; ")},
		WorkspaceRoot: hostPath,
		Shell:         true,
	})
	if err != nil {
		t.Fatalf("plant residue through the shipped host executor: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("plant residue: exit %d, stderr=%q — nothing was planted, so every scan below would be vacuous", res.ExitCode, res.Stderr)
	}
	for i, path := range planted {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("class %s: the shipped posture did not point %s inside the allocation — nothing at %s (%v)",
				classes[i].ID, classes[i].Env, path, err)
		}
	}
	return planted
}

// scanForMarker walks a tree and returns every file whose BYTES contain the marker.
//
// BYTES, AND THE WORD IS LOAD-BEARING. The measurement this guard replaces first answered "0 files"
// because the measuring machine's `grep` is a ugrep wrapper that SKIPS BINARIES — while the tenant's
// string was inside an installed binary. A reader that treats every file as bytes cannot make that
// mistake, and it is also why the guard reads files rather than shelling out to anything.
func scanForMarker(t *testing.T, root, marker string) []string {
	t.Helper()
	needle := []byte(marker)
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A tree removed under the walk is the thing being proven, not a scan failure.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			if os.IsNotExist(rerr) || os.IsPermission(rerr) {
				return nil
			}
			return rerr
		}
		if bytes.Contains(b, needle) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("scan %s for the tenant marker: %v — an errored scan is not an empty one", root, err)
	}
	sort.Strings(found)
	return found
}

// assertResidueGone is the third phase: a fresh scan of the surface the allocation sat on finds
// nothing, AND the allocation is gone. Both, because either alone is weaker than it reads — an empty
// scan of a directory that was never written to proves nothing at all (which is what the positive
// control before the release is for), and a missing directory does not prove no byte escaped into a
// sibling.
//
// ‼️ THE SCAN IS ASSERTED FIRST, AND THE ORDER IS THE FINDING, NOT STYLE. Both assertions fail
// together on a build that stopped deleting, and whichever fires first is the sentence the next reader
// acts on. "the allocation directory survived" reads as a bookkeeping slip; "tenant residue survived
// in 5 files: session_derived_data=…, session_home=…" is what actually happened. This tree has already
// paid for the inverted order once — a traversal test checked the answer CODE before the leak, so
// reading another tenant's disk surfaced as a naming complaint.
func assertResidueGone(t *testing.T, hostPath, scanRoot, marker string, classes []residueClass) {
	t.Helper()
	if found := scanForMarker(t, scanRoot, marker); len(found) != 0 {
		t.Fatalf("tenant residue survived the release in %d file(s): %s", len(found), classIDsFor(found, hostPath, classes))
	}
	if _, err := os.Stat(hostPath); !os.IsNotExist(err) {
		t.Fatalf("the allocation directory survived the release (stat err=%v) — the machine still holds the tenant's bytes", err)
	}
}

// classIDsFor names the measured class each surviving path belongs to, so a failure reads as "the
// derived-data class leaked" rather than as an unattributed path.
func classIDsFor(found []string, hostPath string, classes []residueClass) string {
	var ids []string
	for _, path := range found {
		id := "unclassified"
		best := -1
		for _, c := range classes {
			prefix := filepath.Join(hostPath, c.Rel)
			if (path == prefix || strings.HasPrefix(path, prefix+string(filepath.Separator))) && len(prefix) > best {
				id, best = c.ID, len(prefix)
			}
		}
		ids = append(ids, id+"="+path)
	}
	return strings.Join(ids, ", ")
}

// TestAnIdleReleaseLeavesNoTenantResidueInAnyClassTheAccountlessPostureCloses drives the path a Mac is
// actually handed back on: a session goes quiet, IdleReleaser.release archives the allocation and calls
// `r.remove(c.HostPath)`. Every allocation-scoped class in the measured inventory is planted through
// the shipped writer, COUNTED BEFORE THE RELEASE, and required to be gone after.
//
// The ceiling is in the name: the per-uid surfaces (the darwin bucket, mds/messages/<uid>, the
// operator's own ~/Library) are closed by an ACCOUNT, and no account exists to destroy on any machine
// this runs on. They are asserted as written measurements by the inventory test below, never scanned.
func TestAnIdleReleaseLeavesNoTenantResidueInAnyClassTheAccountlessPostureCloses(t *testing.T) {
	h := requireResidueHarness(t)
	ctx := context.Background()
	classes := allocationScopedClasses(t, loadResidueInventory(t))

	_, _, workspaceID, _, hostPath := h.seedIdleWorkspace(t)
	scanRoot := filepath.Dir(hostPath)
	marker := newID("A5RESIDUE")

	plantTenantResidue(t, hostPath, marker, classes)

	// PHASE 2 — THE POSITIVE CONTROL. Without this line a build that planted nothing would reach the
	// same green as a build that cleaned everything.
	before := scanForMarker(t, scanRoot, marker)
	if len(before) < len(classes) {
		t.Fatalf("pre-release scan found %d marked file(s) for %d classes: %v — the scan cannot see what it is about to require gone",
			len(before), len(classes), before)
	}
	t.Logf("positive control: %d marked file(s) across %d measured allocation-scoped classes before the release", len(before), len(classes))

	releaser := execution.NewIdleReleaser(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), idleTestTTL).
		WithSessionAccounts(&recordingAccounts{})
	if _, err := releaser.Sweep(ctx); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}

	assertResidueGone(t, hostPath, scanRoot, marker, classes)
	// The workspace is paused, not destroyed: this hand-back is the resumable one, and a release that
	// cleaned the disk by ending the thread would be a different (and wrong) behaviour passing this test.
	if state := workspaceState(t, h, workspaceID); state != "paused" {
		t.Fatalf("workspace state = %q, want paused", state)
	}
}

// TestDestroyingAnAllocationLeavesNoTenantResidueInAnyClassTheAccountlessPostureCloses is the same
// three phases over the OTHER shipped remover, WorkspaceRecovery.DestroyAllocation.
//
// IT IS NOT REDUNDANT WITH THE IDLE PATH, AND THE REASON IS THE TREE'S OWN HISTORY: these are two
// call sites of the same removal that have drifted from each other before (the account release read
// the allocation id on one and the session id on the other until 2026-08-05, so one of them destroyed
// nothing while returning nil). A guard on one leaves the other free to stop deleting.
func TestDestroyingAnAllocationLeavesNoTenantResidueInAnyClassTheAccountlessPostureCloses(t *testing.T) {
	h := requireResidueHarness(t)
	ctx := context.Background()
	classes := allocationScopedClasses(t, loadResidueInventory(t))

	project, workspaceID, _, hostPath := h.seedAllocationOnDisk(t)
	tenant := coordinator.Tenant{Project: project}
	scanRoot := filepath.Dir(hostPath)
	marker := newID("A5RESIDUE")

	plantTenantResidue(t, hostPath, marker, classes)

	before := scanForMarker(t, scanRoot, marker)
	if len(before) < len(classes) {
		t.Fatalf("pre-destroy scan found %d marked file(s) for %d classes: %v — the scan cannot see what it is about to require gone",
			len(before), len(classes), before)
	}
	t.Logf("positive control: %d marked file(s) across %d measured allocation-scoped classes before the destroy", len(before), len(classes))

	// The seed leaves the workspace `leased`; destroy is legal from ready/paused/failed.
	if err := h.repo.Spine().AdvanceWorkspace(ctx, tenant, workspaceID, "release"); err != nil {
		t.Fatalf("release workspace: %v", err)
	}
	recovery := execution.NewWorkspaceRecovery(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), t.TempDir())
	if err := recovery.DestroyAllocation(ctx, tenant, execution.DestroyInput{
		WorkspaceID: workspaceID, SessionID: sessionOf(t, h, workspaceID), HostPath: hostPath,
	}); err != nil {
		t.Fatalf("DestroyAllocation() error = %v", err)
	}

	assertResidueGone(t, hostPath, scanRoot, marker, classes)
}

// TestTheResidueInventoryTracksTheShippedSessionDirectoriesAndEveryUnclosedClassCarriesAMeasuredCeiling
// is the half that needs no disk: the inventory itself.
//
// It holds two properties the drives above cannot. First, the allocation-scoped half EQUALS the shipped
// canonical list in both directions — a variable added to host.sessionDirs reddens here on the same
// commit that adds it, instead of silently widening the surface while every scan stays green. Second,
// every class NO posture closes carries a WRITTEN, MEASURED reason, and is outside the allocation the
// release removes. A ceiling with no measurement is not a ceiling, it is an unexamined belief with a
// label; and a "ceiling" that turned out to be inside the allocation would be a surface someone
// stopped driving.
//
// It never scans a ceiling. The system unified log cannot be closed by anything the product does —
// macOS's only eraser takes every tenant's history at once — so a test that failed on it would be
// reporting the machine's design as this product's defect.
func TestTheResidueInventoryTracksTheShippedSessionDirectoriesAndEveryUnclosedClassCarriesAMeasuredCeiling(t *testing.T) {
	inv := loadResidueInventory(t)
	if inv.MeasuredOn == "" || inv.Source == "" {
		t.Fatalf("the residue inventory carries measured_on=%q source=%q — an inventory with no date and no origin cannot be re-measured", inv.MeasuredOn, inv.Source)
	}
	allocation := allocationScopedClasses(t, inv)
	t.Logf("%d allocation-scoped class(es) driven, measured %s (%s)", len(allocation), inv.MeasuredOn, inv.Source)

	seen := map[string]bool{}
	ceilings := 0
	for _, c := range inv.Classes {
		if c.ID == "" {
			t.Fatal("a residue class carries no id")
		}
		if seen[c.ID] {
			t.Fatalf("residue class %s is listed twice — a duplicate inflates every count this file reports", c.ID)
		}
		seen[c.ID] = true
		switch c.Scope {
		case "allocation":
			continue // already checked, in both directions, by allocationScopedClasses
		case "shared":
		default:
			t.Fatalf("residue class %s has scope %q, want \"allocation\" or \"shared\"", c.ID, c.Scope)
		}
		switch c.ClosedBy {
		case closedByAccount, closedByNothing:
		case closedByAllocation:
			t.Fatalf("residue class %s is outside the allocation and claims to be closed by removing it", c.ID)
		default:
			t.Fatalf("residue class %s has closed_by %q", c.ID, c.ClosedBy)
		}
		if c.Path == "" {
			t.Fatalf("residue class %s names no path", c.ID)
		}
		if c.Rel != "" || c.Env != "" {
			t.Fatalf("residue class %s is shared and must not carry an allocation-relative rel/env (rel=%q env=%q)", c.ID, c.Rel, c.Env)
		}
		if len(strings.TrimSpace(c.Ceiling)) < 80 {
			t.Fatalf("residue class %s is not closed by the release and carries no measured ceiling — a ceiling asserted without a measurement is a belief with a label", c.ID)
		}
		ceilings++
		t.Logf("CEILING %-28s closed_by=%-16s %s", c.ID, c.ClosedBy, c.Path)
	}
	if ceilings == 0 {
		t.Fatal("the inventory records no ceiling at all, which on this platform is the one thing the measurement did NOT find")
	}
}
