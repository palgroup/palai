package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/macagent"
)

// spawnSpy is the spawn boundary as a recorder. Asserting on the sessionWorker it was handed is the
// only measurement available here and the reason is worth stating rather than glossing: ACTUALLY
// dropping to uid 707 needs root, this machine is uid 501, and every machine an agent works on is
// unprivileged. So what is measured is the ARGUMENT — the uid, the gid, the program, the directory and
// the environment the kernel would have been given — and what is NOT measured is the kernel applying
// it. That ceiling is real and it is named here rather than papered over; the same shape is recorded in
// adapters/sandboxes/host/privilege_test.go, which measures the refusing branch for the same reason.
type spawnSpy struct {
	workers []sessionWorker
	err     error
}

func (s *spawnSpy) spawn(_ context.Context, w sessionWorker) (int, error) {
	s.workers = append(s.workers, w)
	if s.err != nil {
		return 0, s.err
	}
	return 4242, nil
}

// ourRecord scripts the directory-services reads for a slot whose account this daemon created here: a
// uid that is the one the namespace allocates, and a marker minted against this host.
//
// IT IS DELIBERATELY NOT GENEROUS. A fake that answered every dscl read would make the guard below
// unfalsifiable, which is how a fake mirroring production ends up mirroring its bug; this one answers
// exactly the two reads the record path makes and nothing else.
func ourRecord(slot int) *recordingRun {
	name, _ := macagent.AccountName(slot)
	uid, _ := macagent.AccountUID(slot)
	marker := markerMagic + ":" + markerVersion + ":" + testHostUUID + ":" + name + ":2026-08-05T12:00:00Z"
	return &recordingRun{answers: map[string]string{
		"dscl . -read /Users/" + name + " UniqueID":      "UniqueID: " + strconv.Itoa(uid),
		"dscl . -read /Users/" + name + " " + markerAttr: markerAttr + ": " + marker,
	}}
}

// newSpawnAccounts wires a daemon whose worker binary is a real file this test owns, and whose spawn
// boundary records instead of forking.
func newSpawnAccounts(t *testing.T, rec *recordingRun, spy *spawnSpy) *SysadminctlAccounts {
	t.Helper()
	accounts := newTestAccounts(t, rec)
	accounts.allocationRoot = t.TempDir()
	accounts.spawn = spy.spawn
	accounts.workerPath = writeWorker(t, 0o755)
	return accounts
}

// writeWorker puts a stand-in worker binary on disk at the given mode. It is a real file because
// requireSafeWorker stats one, and a test that substituted the stat would be testing its own fake.
func writeWorker(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "palai-session-worker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// ---------------------------------------------------------------------------------------------------
// 1. THE SPAWNED WORKER'S UID IS THE SLOT'S UID.
// ---------------------------------------------------------------------------------------------------

// TestASpawnedWorkerRunsAsTheSlotsAccount is the sentence this whole phase exists for, and until it
// existed the tree's answer was uid 501 for everybody.
//
// The chain it pins is: a caller sends an integer -> the daemon derives a name, a uid and a home from
// it -> those three, and nothing a caller chose, become the process the kernel is asked to start. Every
// link is an arithmetic step over macagent's own constants, so the assertions below recompute them
// rather than writing 707 down twice.
func TestASpawnedWorkerRunsAsTheSlotsAccount(t *testing.T) {
	const slot = 7
	// A secret in the DAEMON's own environment. palai-agentd is a root LaunchDaemon, so what it holds is
	// whatever launchd and the machine's plist decided root should have — and `ps -E` on this platform
	// shows a live process's environment to the same uid (measured 2026-08-04), so an inherited value
	// would not merely be present in the worker, it would be PUBLISHED to the tenant that worker is for.
	t.Setenv("PALAI_TEST_INHERITED_SECRET", "this-must-not-reach-the-tenant")

	spy := &spawnSpy{}
	accounts := newSpawnAccounts(t, ourRecord(slot), spy)

	name, pid, err := accounts.Spawn(context.Background(), slot)
	if err != nil {
		t.Fatalf("spawning a worker for a slot this daemon owns was refused: %v", err)
	}
	if len(spy.workers) != 1 {
		t.Fatalf("the daemon started %d processes, want exactly 1", len(spy.workers))
	}
	w := spy.workers[0]

	wantUID, err := macagent.AccountUID(slot)
	if err != nil {
		t.Fatal(err)
	}
	if w.UID != wantUID {
		t.Errorf("the worker was started as uid %d, want %d (macagent.AccountUID(%d)). A worker at the wrong "+
			"uid is the defect this phase exists to remove, not a smaller boundary.", w.UID, wantUID, slot)
	}
	if w.GID != macagent.GIDBase+7 {
		t.Errorf("the worker was started with gid %d, want %d — a gid the account was not created with is a "+
			"process that cannot open its own home directory", w.GID, macagent.GIDBase+7)
	}
	wantName, _ := macagent.AccountName(slot)
	if name != wantName {
		t.Errorf("the daemon answered name %q, want %q", name, wantName)
	}
	if pid != 4242 {
		t.Errorf("the daemon answered pid %d, want the one the boundary returned (4242) — a pid a caller "+
			"cannot observe is a claim rather than a measurement", pid)
	}

	// ‼️ THE SLOT DIRECTORY AND NOT THE ACCOUNT'S HOME, WHICH IT WAS UNTIL 2026-08-08. The worker binds
	// its socket in whatever directory it is started in, and the control plane has to reach that socket:
	// it OWNS the slot directory, while it can never join the account's own group — a process's
	// supplementary groups are fixed at exec, and a session account is created while the plane is already
	// running. Measured that night: `id -Gn` in a fresh shell listed palai-s01-grp while the plane
	// serving that session did not hold it, and every dial into the home answered EACCES.
	//
	// The property the old assertion protected is unchanged and still asserted: it is NOT the directory
	// launchd left root in.
	wantDir := filepath.Join(accounts.allocationRoot, SlotDirName(slot))
	if w.Dir != wantDir {
		t.Errorf("the worker's working directory is %q, want the slot directory %q — that is where it binds "+
			"its socket, and the only place the control plane can reach it by ownership", w.Dir, wantDir)
	}
	if w.Dir == "/" || w.Dir == "" {
		t.Error("the worker starts where launchd left root, and writes its scratch files into root's directory")
	}
	if w.Path != accounts.workerPath {
		t.Errorf("the worker's program is %q, want %q — the daemon chooses the program and this is the only "+
			"place it comes from", w.Path, accounts.workerPath)
	}

	// THE ENVIRONMENT IS BUILT, NOT FILTERED, and this is the sweep that says so: it looks for the value
	// rather than the name, because a redaction that hid the key and shipped the value would pass a
	// name-only check. This tree has already paid that bill once — see the value-based redaction in
	// d22b799c.
	for _, entry := range w.Env {
		if strings.Contains(entry, "this-must-not-reach-the-tenant") {
			t.Errorf("the worker inherited %q from the root daemon's own environment; `ps -E` would then show "+
				"it to the tenant", entry)
		}
		if strings.HasPrefix(entry, "PALAI_TEST_INHERITED_SECRET=") {
			t.Errorf("the worker's environment carries %q: it is meant to be built from a named list, so a "+
				"variable nobody named cannot arrive", entry)
		}
	}
	// HOME is still the ACCOUNT'S OWN, which is a different question from where the process starts: the
	// working directory is where its socket lives, HOME is where its tools write.
	wantHome, _ := macagent.HomeDir(slot)
	if !hasEnv(w.Env, "HOME="+wantHome) {
		t.Errorf("the worker's HOME is not %s; its environment was %v — root's HOME reaching a tenant process "+
			"is the residue this phase removes", wantHome, w.Env)
	}
}

func hasEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// TestSpawnRefusesEveryAccountItDidNotCreate is the other direction, and it is the one with teeth: a
// spawn that went ahead would start a process AS whatever principal the record named.
//
// Each case below is one way an account can be named like ours and not be ours, and the last is the
// positive control — without it every assertion here would be satisfied by a daemon that refused
// everything, which is a daemon nobody could ship.
func TestSpawnRefusesEveryAccountItDidNotCreate(t *testing.T) {
	const slot = 7
	name, _ := macagent.AccountName(slot)
	uid, _ := macagent.AccountUID(slot)
	marker := markerMagic + ":" + markerVersion + ":" + testHostUUID + ":" + name + ":2026-08-05T12:00:00Z"

	cases := []struct {
		name      string
		rec       *recordingRun
		worker    os.FileMode
		wantClass macagent.Class
		wantIn    string
	}{
		{
			name:      "a slot with no account at all",
			rec:       &recordingRun{answers: map[string]string{}},
			worker:    0o755,
			wantClass: macagent.ClassNotFound,
			wantIn:    "create the slot first",
		},
		{
			name: "a record carrying no marker, so root did not create it here",
			rec: &recordingRun{answers: map[string]string{
				"dscl . -read /Users/" + name + " UniqueID": "UniqueID: 707",
			}},
			worker:    0o755,
			wantClass: macagent.ClassRefused,
			wantIn:    markerMagic,
		},
		{
			name: "a marker minted on another Mac",
			rec: &recordingRun{answers: map[string]string{
				"dscl . -read /Users/" + name + " UniqueID": "UniqueID: 707",
				"dscl . -read /Users/" + name + " " + markerAttr: markerAttr + ": " + markerMagic + ":" +
					markerVersion + ":99999999-0000-0000-0000-000000000000:" + name + ":2026-08-05T12:00:00Z",
			}},
			worker:    0o755,
			wantClass: macagent.ClassRefused,
			wantIn:    "minted on host",
		},
		{
			name: "a record whose uid is not the one this namespace allocates",
			rec: &recordingRun{answers: map[string]string{
				"dscl . -read /Users/" + name + " UniqueID":      "UniqueID: 501",
				"dscl . -read /Users/" + name + " " + markerAttr: markerAttr + ": " + marker,
			}},
			worker:    0o755,
			wantClass: macagent.ClassRefused,
			wantIn:    "is not the uid",
		},
		{
			// A worker binary every session account can write is a worker binary slot 07 can replace
			// before slot 08 runs it. The accounts share gid 20, so this is one tenant reaching another.
			name:      "a worker binary a session account could rewrite",
			rec:       ourRecord(slot),
			worker:    0o777,
			wantClass: macagent.ClassInternal,
			wantIn:    "group- or world-writable",
		},
		{
			name:      "the account this daemon created here, which must SERVE",
			rec:       ourRecord(slot),
			worker:    0o755,
			wantClass: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := &spawnSpy{}
			accounts := newTestAccounts(t, tc.rec)
			accounts.allocationRoot = t.TempDir()
			accounts.spawn = spy.spawn
			accounts.workerPath = writeWorker(t, tc.worker)

			_, _, err := accounts.Spawn(context.Background(), slot)
			if tc.wantClass == "" {
				if err != nil {
					t.Fatalf("the positive control was refused: %v", err)
				}
				if len(spy.workers) != 1 {
					t.Fatalf("the positive control started %d processes, want 1", len(spy.workers))
				}
				return
			}
			if err == nil {
				t.Fatalf("a process was started as uid %d for a record this daemon does not own", uid)
			}
			// NOTHING WAS STARTED, which is the assertion with the teeth: a refusal returned after the
			// fork would be a refusal that reported a process it had already created.
			if len(spy.workers) != 0 {
				t.Errorf("the daemon refused AND started %d processes; the refusal is a report, not a guard", len(spy.workers))
			}
			var classed *macagent.Error
			if !asError(err, &classed) {
				t.Fatalf("the refusal carries no class, so a caller cannot branch on it: %v", err)
			}
			if classed.Class != tc.wantClass {
				t.Errorf("class = %q, want %q (message: %s)", classed.Class, tc.wantClass, classed.Message)
			}
			if tc.wantIn != "" && !strings.Contains(classed.Message, tc.wantIn) {
				t.Errorf("the message does not say %q, so an operator cannot tell which condition fired: %s", tc.wantIn, classed.Message)
			}
		})
	}
}

// TestSpawnRefusesAWorkerBinaryThatIsNotThere is the state EVERY machine is in today, and it is a test
// rather than a comment because the honest reading of this phase depends on it: the account is real,
// the uid is real and the drop is real, and nothing installs a worker binary yet. macagent.Install lays
// down the daemon, the group, the plist and the job, and no step of it writes InstalledWorkerPath.
func TestSpawnRefusesAWorkerBinaryThatIsNotThere(t *testing.T) {
	spy := &spawnSpy{}
	accounts := newTestAccounts(t, ourRecord(7))
	accounts.allocationRoot = t.TempDir()
	accounts.spawn = spy.spawn
	accounts.workerPath = filepath.Join(t.TempDir(), "absent")

	_, _, err := accounts.Spawn(context.Background(), 7)
	if err == nil {
		t.Fatal("a spawn succeeded with no worker binary on disk")
	}
	if len(spy.workers) != 0 {
		t.Errorf("the daemon reached its exec boundary %d times with nothing to execute", len(spy.workers))
	}
	if !strings.Contains(err.Error(), accounts.workerPath) {
		t.Errorf("the refusal does not name the path an operator has to put a binary at: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------------
// 2. THE CALLER CANNOT SAY WHAT TO RUN.
// ---------------------------------------------------------------------------------------------------

// TestSpawnCannotBeToldWhatToRun is the property that makes a `run` verb on a ROOT daemon defensible,
// and it is asserted in both of the only two places it can live.
//
// The BEHAVIOURAL half: every spelling of "spawn, and also run this" is refused by the parser before
// anything looks at the slot, so a caller cannot smuggle a program through the one field there is.
//
// The DECLARATION half: macagent.InstalledWorkerPath is a const. That is not decoration — a `var` is
// assignable, and a root daemon whose program is a package-level variable is a root daemon one
// misplaced init() away from executing something else. There is no behaviour to observe for it, the way
// there is none for Request having no string field, so the declaration is what gets read.
func TestSpawnCannotBeToldWhatToRun(t *testing.T) {
	// Nothing here is a slot, and each is a shape somebody would reach for to pass a program.
	for _, line := range []string{
		"spawn 07 /bin/sh",
		"spawn /bin/sh",
		"spawn 07 --program /bin/sh",
		"spawn 07 PALAI_KEY=secret",
		"spawn 07 07",
		"spawn",
		"spawn 7",
		"spawn 007",
		"spawn 07;/bin/sh",
	} {
		if req, err := macagent.ParseRequest(line + "\n"); err == nil {
			t.Errorf("%q parsed as %+v: a spawn request has room for exactly one two-digit slot and nothing else", line, req)
		}
	}

	// And the one well-formed spelling round-trips, or the refusals above would be a parser that
	// refuses everything.
	req, err := macagent.ParseRequest("spawn 07\n")
	if err != nil {
		t.Fatalf("the one well-formed spawn request was refused: %v", err)
	}
	if req.Verb != macagent.VerbSpawn || req.Slot != 7 {
		t.Fatalf("`spawn 07` parsed as %+v", req)
	}
	line, err := macagent.Request{Verb: macagent.VerbSpawn, Slot: 7}.Line()
	if err != nil || line != "spawn 07\n" {
		t.Fatalf("encoding a spawn produced %q (%v), want %q", line, err, "spawn 07\n")
	}

	// THE DECLARATION HALF.
	if !declaredAsConst(t, filepath.Join("..", "..", "packages", "macagent"), "InstalledWorkerPath") {
		t.Error("macagent.InstalledWorkerPath is not declared as a const. A root daemon's one executable " +
			"must not be assignable at run time.")
	}
	// The anchor: if the scan cannot find a constant that certainly is one, the check above is vacuous.
	if !declaredAsConst(t, filepath.Join("..", "..", "packages", "macagent"), "UIDBase") {
		t.Fatal("the const scan did not find macagent.UIDBase, so it asserts nothing about InstalledWorkerPath either")
	}
}

// declaredAsConst reports whether a package declares name in a `const` block.
func declaredAsConst(t *testing.T, dir, name string) bool {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", dir, err)
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, ident := range vs.Names {
						if ident.Name == name {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// asError is errors.As with the daemon's classed error, kept local so the assertion above reads as one
// line.
func asError(err error, target **macagent.Error) bool { return errors.As(err, target) }

// TestSpawnPreparesTheSlotDirectoryItStartsTheWorkerIn — ORDER, NOT OWNERSHIP.
//
// Spawn happens at Acquire, the moment a session gets its account; adopt happens later, when a workspace
// has been planned. A worker started before adopt found the slot directory at whatever mode its creator
// left, and `fork/exec` reports `permission denied` when the CHILD cannot enter its own working
// directory — measured on a real Mac on 2026-08-08 against a 0710 directory whose group the account
// holds, because x alone is not enough to bind a socket in.
//
// The owner is deliberately NOT moved: the control plane created this directory and reaches it by
// ownership, which is the half no group list can take away.
func TestSpawnPreparesTheSlotDirectoryItStartsTheWorkerIn(t *testing.T) {
	const slot = 7
	rec := ourRecord(slot)
	accounts := newTestAccounts(t, rec)
	accounts.allocationRoot = t.TempDir()

	var gave []string
	accounts.chown = func(path string, uid, gid int) error {
		gave = append(gave, filepath.Base(path)+" -> uid:"+strconv.Itoa(uid)+" gid:"+strconv.Itoa(gid))
		return nil
	}
	var started sessionWorker
	accounts.spawn = func(_ context.Context, w sessionWorker) (int, error) { started = w; return 4242, nil }
	worker := filepath.Join(t.TempDir(), "palai-session-worker")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	accounts.workerPath = worker

	if _, _, err := accounts.Spawn(context.Background(), slot); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	slotDir := filepath.Join(accounts.allocationRoot, SlotDirName(slot))
	fi, err := os.Stat(slotDir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("%s was not created, so the worker had nowhere to start: %v", slotDir, err)
	}
	if got := fi.Mode().Perm(); got != slotRootMode {
		t.Errorf("%s is mode %04o, want %04o — the group needs w to bind the worker's socket, and the child "+
			"needs x to enter its own working directory", slotDir, got, slotRootMode)
	}
	want := SlotDirName(slot) + " -> uid:-1 gid:807"
	found := false
	for _, g := range gave {
		if g == want {
			found = true
		}
	}
	if !found {
		t.Errorf("ownership calls were %v, want %q — the group moves and the OWNER does not, because the "+
			"control plane reaches this directory by owning it", gave, want)
	}
	if started.Dir != slotDir {
		t.Errorf("the worker was started in %q, want the slot directory %q", started.Dir, slotDir)
	}
}
