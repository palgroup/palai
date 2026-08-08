package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/macagent"
)

const testHostUUID = "11111111-2222-3333-4444-555555555555"

// recordingRun is the exec boundary as a script: it answers the commands the daemon runs and keeps
// every argv it was handed. Asserting on the argv is the point — this daemon's safety argument is that
// nothing a caller supplies reaches an exec, and the only way to check that is to look at what an exec
// would have received.
type recordingRun struct {
	calls   [][]string
	answers map[string]string
	fail    map[string]bool
	// gone records accounts a -deleteUser has already removed. IT IS NOT BOOKKEEPING: Delete verifies
	// after itself that the record actually went, and a fake that kept answering `dscl -read` would
	// make that post-condition unsatisfiable. Getting this wrong is how the check proved it is real —
	// the first run of this file failed with "sysadminctl reported success but the record still
	// exists", which is exactly what the daemon should say when a deletion silently does nothing.
	gone map[string]bool
	// made is the other half of the same idea: a `dscl . -create /Users/X UniqueID N` MAKES the record,
	// and a later read answers what was written. Scripting the read instead would let a test assert a
	// uid the creator never wrote — which is precisely the production defect this fake exists to model,
	// so the fake keeps the state rather than a lookup table.
	made map[string]string
	// substituteUID makes the creator LIE: whatever uid it is told to write, it writes this one. That is
	// what `sysadminctl -addUser -UID` was measured doing on this Mac, and a fake that could not
	// reproduce it could not be used to prove the read-back catches it.
	substituteUID string
	// chowns is what was handed to whom, by base name, in order.
	chowns []string
}

func (r *recordingRun) run(_ context.Context, name string, args ...string) (string, error) {
	argv := append([]string{name}, args...)
	r.calls = append(r.calls, argv)
	key := strings.Join(argv, " ")
	if r.fail[key] {
		return "boom", os.ErrPermission
	}
	if name == "dscl" && len(args) >= 3 && args[1] == "-delete" && strings.HasPrefix(args[2], "/Users/") {
		if r.gone == nil {
			r.gone = map[string]bool{}
		}
		account := strings.TrimPrefix(args[2], "/Users/")
		r.gone[account] = true
		delete(r.made, account)
	}
	if name == "dscl" && len(args) >= 5 && args[1] == "-create" && strings.HasPrefix(args[2], "/Users/") && args[3] == "UniqueID" {
		if r.made == nil {
			r.made = map[string]string{}
		}
		written := args[4]
		if r.substituteUID != "" {
			written = r.substituteUID
		}
		r.made[strings.TrimPrefix(args[2], "/Users/")] = written
		delete(r.gone, strings.TrimPrefix(args[2], "/Users/"))
	}
	// A read of a record that has been deleted fails, the way dscl does; a read of one that was created
	// answers the uid the creator actually wrote.
	if name == "dscl" && len(args) >= 3 && args[1] == "-read" {
		account := strings.TrimPrefix(args[2], "/Users/")
		if r.gone[account] {
			return "", os.ErrNotExist
		}
		if uid, ok := r.made[account]; ok && len(args) >= 4 && args[3] == "UniqueID" {
			return "UniqueID: " + uid, nil
		}
	}
	if out, ok := r.answers[key]; ok {
		return out, nil
	}
	// An unscripted command is an error, which is what a `dscl -read` of a record that does not exist
	// actually does. Answering "" with no error instead would make every absent record look present.
	return "", os.ErrNotExist
}

func (r *recordingRun) argv(i int) string {
	if i >= len(r.calls) {
		return ""
	}
	return strings.Join(r.calls[i], " ")
}

func newTestAccounts(t *testing.T, rec *recordingRun) *SysadminctlAccounts {
	t.Helper()
	return &SysadminctlAccounts{
		run:          rec.run,
		foldersRoot:  t.TempDir(),
		messagesRoot: t.TempDir(),
		hostUUID:     func(context.Context) (string, error) { return testHostUUID, nil },
		// The settle a delete spends after signalling the account's processes. Substituted so the shipped
		// value is driven without being waited out — never shortened in production, where a signal that
		// has not landed is the race the settle exists for.
		sleep:     func(time.Duration) {},
		usersRoot: t.TempDir(),
		// Only uid 0 may give a file to another uid. Recorded rather than performed, so the test can
		// assert WHAT was handed to WHOM without the suite needing root.
		chown: func(path string, uid, gid int) error {
			rec.chowns = append(rec.chowns, fmt.Sprintf("%s -> %d:%d", filepath.Base(path), uid, gid))
			return nil
		},
		now:  func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) },
		goos: "darwin",
	}
}

// TestCreateRunsArgvAndNeverACommandLine pins the exact privileged commands, and pins that the home
// directory is VERIFIED rather than assumed.
//
// The second half is the one with history: `sysadminctl -home` assigns a home directory and does not
// create one — it says so in its own output — and createhomedir exits 0 on paths it did not populate.
// an earlier shell creator shipped a defect reading past exactly this, so the check is here and so is its test.
//
// ‼️ AND THE SEQUENCE NOW OPENS WITH THE GROUP, WHICH IS ORDER AND NOT DECORATION. `-GID` naming a group
// that does not exist produces an account whose primary group resolves to nothing — a user that cannot
// open its own home, reported as a successful create — so the group has to be made first and the pin is
// what keeps it there.
//
// THE GID IS 807 AND IT HAS BEEN WRONG TWICE, in opposite directions. It carried `-GID 20` until
// 2026-08-06, which is `staff`: the operator's own login group, on a machine whose home is drwxr-x---
// owned by <operator>:staff — so the account read the whole home and this expectation asserted the bug.
// It then carried a fleet-wide 700, which fixed that and left every session reachable by every other
// session the moment a workspace was made group-accessible to its own account. 807 is slot 7's OWN
// group, and a number that moves WITH the slot is the only shape under which "the account reaches its
// workspace" and "no other tenant does" are both true.
func TestCreateRunsArgvAndNeverACommandLine(t *testing.T) {
	rec := &recordingRun{answers: map[string]string{
		"dscl . -create /Groups/palai-s07-grp":                                                                                         "",
		"dscl . -create /Groups/palai-s07-grp PrimaryGroupID 807":                                                                      "",
		"dscl . -create /Groups/palai-s07-grp RealName Palai session slot 07":                                                          "",
		"dscl . -create /Users/palai-s07 UniqueID 707":                                                                                 "",
		"dscl . -create /Users/palai-s07 PrimaryGroupID 807":                                                                           "",
		"dscl . -create /Users/palai-s07 UserShell /bin/zsh":                                                                           "",
		"dscl . -create /Users/palai-s07 NFSHomeDirectory /Users/palai-s07":                                                            "",
		"dscl . -create /Users/palai-s07 RealName Palai session s07":                                                                   "",
		"dscl . -create /Users/palai-s07 " + markerAttr + " " + markerMagic + ":1:" + testHostUUID + ":palai-s07:2026-08-05T12:00:00Z": "",
		"createhomedir -c -u palai-s07":                                                                                                "",
	}}
	a := newTestAccounts(t, rec)

	// THE HOME IS MADE HERE AND `createhomedir` IS GONE, which this asserts by its absence from the
	// sequence below and by the directory existing after. createhomedir populated from the user template
	// — Music, Pictures, Desktop, Library/Containers — and macOS puts those exact paths behind TCC:
	// `sudo rm -rf /Users/palai-s05` answered `Operation not permitted` on eight of them AS ROOT on
	// 2026-08-08, and the home survived. An account whose home cannot be removed is an account that
	// cannot be destroyed, which is the whole idle half of this lifecycle.
	_, home, err := a.Create(context.Background(), 7)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if home != "/Users/palai-s07" {
		t.Errorf("home = %q, want the path directory services was told about", home)
	}
	diskHome := filepath.Join(a.usersRoot, "palai-s07")
	fi, err := os.Stat(diskHome)
	if err != nil || !fi.IsDir() {
		t.Fatalf("%s is not a directory after a successful create; the account has nowhere to run", diskHome)
	}
	// x WITHOUT r: the control plane reaches one known path inside (the worker's socket) and cannot list
	// the home. 0700 would make the socket unreachable, which is a machine that mints accounts and
	// cannot run a command as one.
	if got := fi.Mode().Perm(); got != 0o710 {
		t.Errorf("%s is mode %04o, want 0710", diskHome, got)
	}
	if _, err := os.Stat(filepath.Join(diskHome, "tmp")); err != nil {
		t.Errorf("no TMPDIR under %s: the worker's environment names $HOME/tmp and a toolchain would fail "+
			"somewhere far from here", diskHome)
	}
	// AND BOTH WERE GIVEN TO THE ACCOUNT. Only uid 0 can do this, so it is recorded rather than
	// performed — but a create that made the directories and left them owned by root is one whose
	// session cannot write its own HOME.
	wantChowns := []string{"palai-s07 -> 707:807", "tmp -> 707:807"}
	if strings.Join(rec.chowns, " | ") != strings.Join(wantChowns, " | ") {
		t.Errorf("ownership handed over was %v, want %v", rec.chowns, wantChowns)
	}

	want := []string{
		// The group, probed then made. The probe answers ErrNotExist here (nothing scripts it), which is
		// what dscl does for a record that is not there, so this run takes the create path.
		"dscl . -read /Groups/palai-s07-grp PrimaryGroupID",
		"dscl . -search /Groups PrimaryGroupID 807",
		"dscl . -create /Groups/palai-s07-grp",
		"dscl . -create /Groups/palai-s07-grp PrimaryGroupID 807",
		"dscl . -create /Groups/palai-s07-grp RealName Palai session slot 07",
		// Only then the account, written attribute by attribute, and it names the group that now exists.
		// dscl rather than `sysadminctl -addUser` because sysadminctl was measured taking `-UID` and
		// writing macOS's own next-free number instead — see the creator for what that cost.
		"dscl . -create /Users/palai-s07 UniqueID 707",
		"dscl . -create /Users/palai-s07 PrimaryGroupID 807",
		"dscl . -create /Users/palai-s07 UserShell /bin/zsh",
		"dscl . -create /Users/palai-s07 NFSHomeDirectory /Users/palai-s07",
		"dscl . -create /Users/palai-s07 RealName Palai session s07",
		// AND THE READ-BACK, which is not bookkeeping: the reason the line above is dscl is that a
		// creator reported success and wrote something else.
		"dscl . -read /Users/palai-s07 UniqueID",
		"dscl . -create /Users/palai-s07 " + markerAttr + " " + markerMagic + ":1:" + testHostUUID + ":palai-s07:2026-08-05T12:00:00Z",
	}
	// The first call is the existence probe; the privileged sequence starts after it.
	for i, w := range want {
		if got := rec.argv(i + 1); got != w {
			t.Errorf("privileged call %d was\n  %s\nwant\n  %s", i+1, got, w)
		}
	}
	for _, call := range rec.calls {
		for _, arg := range call {
			// No shell means no shell: neither the binary nor any argument may be one, and no
			// argument may carry a metacharacter, because an argv that never reaches a shell is only
			// a safe design while it stays an argv.
			if call[0] == "sh" || call[0] == "bash" || call[0] == "zsh" || call[0] == "/bin/sh" {
				t.Errorf("a privileged step ran a shell: %v", call)
			}
			if strings.ContainsAny(arg, ";|&$`><*?") && arg != "/bin/zsh" {
				t.Errorf("argument %q carries a shell metacharacter", arg)
			}
		}
	}
}

// TestCreateNeverPutsAPasswordInArgv is small and it is not decoration. On this platform argv and the
// environment of a live process are both readable by the same uid — `ps -E` was measured on 2026-08-04
// listing 62 environment variables with their values — so a secret passed to sysadminctl would be
// readable by every session on the box for as long as the command ran.
func TestCreateNeverPutsAPasswordInArgv(t *testing.T) {
	rec := &recordingRun{answers: map[string]string{}}
	a := newTestAccounts(t, rec)
	_, _, _ = a.Create(context.Background(), 7)
	if len(rec.calls) == 0 {
		t.Fatal("no command ran, so this test asserts nothing")
	}
	for _, call := range rec.calls {
		for _, arg := range call {
			if arg == "-password" || arg == "-hint" {
				t.Errorf("a privileged step passed %s in argv: %v", arg, call)
			}
		}
	}
}

// deleteScript is a directory-services record that answers the reads Delete makes, and stops answering
// the existence probe once the account has been deleted.
func deleteScript(marker string) *recordingRun {
	return &recordingRun{answers: map[string]string{
		"dscl . -read /Users/palai-s07 UniqueID":             "UniqueID: 707",
		"dscl . -read /Users/palai-s07 " + markerAttr:        markerAttr + ": " + marker,
		"dsmemberutil checkmembership -U palai-s07 -G admin": "user is not a member of the group",
		"dscl . -delete /Users/palai-s07":                    "Deleting record for palai-s07",
	}}
}

func goodMarker() string {
	return markerMagic + ":1:" + testHostUUID + ":palai-s07:2026-08-05T12:00:00Z"
}

// TestDeleteRemovesTheRecordAndTheHomeAsTwoSteps pins WHAT the deletion is, and it is two steps because
// they fail separately.
//
// It was one `sysadminctl -deleteUser <name> -secure` until 2026-08-08, when the live plane logged
// `destroy slot 01: sysadminctl reported success but the record for palai-s01 still exists` — the
// daemon's own verification firing on a deletion that exited 0 and did nothing. The same tool was
// measured lying in the other direction the same day, writing a uid nobody asked for (see
// TestACreatorThatWritesAnotherUidIsREFUSED), so both halves of this surface are dscl now.
//
// `-secure` erased the home as part of the same call, which is why the separation matters: a home that
// could not be removed and a record that could not be deleted arrived as one opaque exit code.
func TestDeleteRemovesTheRecordAndTheHomeAsTwoSteps(t *testing.T) {
	rec := deleteScript(goodMarker())
	a := newTestAccounts(t, rec)
	name, _, err := a.Delete(context.Background(), 7)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if name != "palai-s07" {
		t.Errorf("Delete named %q, want palai-s07", name)
	}
	found := false
	for _, call := range rec.calls {
		if strings.Join(call, " ") == "dscl . -delete /Users/palai-s07" {
			found = true
		}
		for _, arg := range call {
			if arg == "--secure" {
				t.Errorf("Delete passed --secure, which sysadminctl does not accept: %v", call)
			}
		}
	}
	if !found {
		t.Errorf("Delete never ran `dscl . -delete /Users/palai-s07`; it ran %v", rec.calls)
	}
}

// TestDeleteRefusesAnAccountThisDaemonDidNotCreate walks the deletion guard through every way a record
// can be named like ours and not be ours.
//
// A NAME IS NOT AUTHORITY. Every case below would pass a check that only looked at the name, which is
// why the guard exists and why its whole table runs here rather than on a Mac with root.
func TestDeleteRefusesAnAccountThisDaemonDidNotCreate(t *testing.T) {
	protected := map[int]bool{0: true, 501: true}
	cases := []struct {
		name    string
		rec     dsRecord
		mustSay string
	}{
		{
			"a name outside the namespace",
			dsRecord{name: "salih", uid: 501, marker: goodMarker()},
			"is not an account this daemon names",
		},
		{
			"the uid was edited after creation",
			dsRecord{name: "palai-s07", uid: 501, marker: goodMarker()},
			"is not the uid 707 this daemon allocates",
		},
		{
			"no marker at all, so root did not create it here",
			dsRecord{name: "palai-s07", uid: 707, marker: ""},
			"carries no " + markerMagic + " marker",
		},
		{
			"a marker minted on a different Mac",
			dsRecord{name: "palai-s07", uid: 707, marker: markerMagic + ":1:other-host:palai-s07:t"},
			"minted on host",
		},
		{
			"a marker naming a different session",
			dsRecord{name: "palai-s07", uid: 707, marker: markerMagic + ":1:" + testHostUUID + ":palai-s42:t"},
			"names \"palai-s42\"",
		},
		{
			"a marker from a version this daemon does not write",
			dsRecord{name: "palai-s07", uid: 707, marker: markerMagic + ":9:" + testHostUUID + ":palai-s07:t"},
			"carries no " + markerMagic + " marker",
		},
		{
			"an admin, which this daemon never creates",
			dsRecord{name: "palai-s07", uid: 707, marker: goodMarker(), inAdmin: true},
			"member of admin",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			why := notOurAccountBecause(tc.rec, testHostUUID, protected)
			if why == "" {
				t.Fatalf("PERMITTED: %+v", tc.rec)
			}
			if !strings.Contains(why, tc.mustSay) {
				t.Errorf("refused with %q, want it to name %q", why, tc.mustSay)
			}
		})
	}

	// THE CONTROL: a record that really is ours must be permitted, or every refusal above is green for
	// a reason that has nothing to do with the case it names.
	ours := dsRecord{exists: true, name: "palai-s07", uid: 707, marker: goodMarker()}
	if why := notOurAccountBecause(ours, testHostUUID, protected); why != "" {
		t.Fatalf("a record this daemon created was refused: %s", why)
	}
	// And a host with no identity matches nothing, rather than matching everything.
	if why := notOurAccountBecause(ours, "", protected); why == "" {
		t.Error("a host with no identity permitted a deletion")
	}
	// And the protected set wins even over a perfect record.
	if why := notOurAccountBecause(ours, testHostUUID, map[int]bool{707: true}); why == "" {
		t.Error("a protected uid was permitted")
	}
}

// TestDeleteRefusesARecordThatIsNotThere pins the class, because a caller branches on it: a delete of a
// slot nobody holds is not the same answer as a delete this daemon declined.
func TestDeleteRefusesARecordThatIsNotThere(t *testing.T) {
	rec := &recordingRun{answers: map[string]string{}}
	a := newTestAccounts(t, rec)
	_, _, err := a.Delete(context.Background(), 7)
	var e *macagent.Error
	if !errors.As(err, &e) || e.Class != macagent.ClassNotFound {
		t.Fatalf("Delete of an absent slot answered %v, want class %s", err, macagent.ClassNotFound)
	}
	for _, call := range rec.calls {
		if call[0] == "sysadminctl" {
			t.Errorf("nothing existed and sysadminctl still ran: %v", call)
		}
	}
}

// TestNotAMacIsARefusalAndNotAPanic keeps the daemon honest on the platform CI runs on: every verb says
// so rather than shelling out to binaries that are not there.
func TestNotAMacIsARefusalAndNotAPanic(t *testing.T) {
	rec := &recordingRun{answers: map[string]string{}}
	a := newTestAccounts(t, rec)
	a.goos = "linux"
	for _, call := range []func() error{
		func() error { _, _, err := a.Create(context.Background(), 7); return err },
		func() error { _, _, err := a.Delete(context.Background(), 7); return err },
		func() error { _, err := a.List(context.Background()); return err },
	} {
		err := call()
		var e *macagent.Error
		if !errors.As(err, &e) || e.Class != macagent.ClassUnsupported {
			t.Errorf("got %v, want class %s", err, macagent.ClassUnsupported)
		}
	}
	if len(rec.calls) != 0 {
		t.Errorf("a non-Mac still ran %v", rec.calls)
	}
}

// TestListReportsOnlySlotsWhoseUIDMatchesTheAllocation: an enumeration that included records this
// daemon cannot delete would be an enumeration that lies about what it can do.
func TestListReportsOnlySlotsWhoseUIDMatchesTheAllocation(t *testing.T) {
	rec := &recordingRun{answers: map[string]string{
		"dscl . -list /Users UniqueID": strings.Join([]string{
			"salih         501",
			"palai-s01     701",
			"palai-s07     707",
			"palai-s08     999", // named like ours, uid nothing like ours
			"palai-s      705",  // no index
			"palai-s100   800",  // three digits
			"palai-s00    700",  // outside 01..99
			"_palagenttest 504",
		}, "\n"),
	}}
	a := newTestAccounts(t, rec)
	slots, err := a.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Ints(slots)
	want := []int{1, 7}
	if len(slots) != len(want) {
		t.Fatalf("List returned %v, want %v", slots, want)
	}
	for i := range want {
		if slots[i] != want[i] {
			t.Fatalf("List returned %v, want %v", slots, want)
		}
	}
}

// ---------------------------------------------------------------------------------------------------
// The darwin cache bucket, which the account deletion does not touch.
// ---------------------------------------------------------------------------------------------------

// TestEveryBucketTheUIDOwnsIsFoundNotJustTheFirst is named after the measurement that made it
// necessary: uid 504 on this machine holds TWO buckets, so a search that returned at the first match
// would leave residue behind and report success.
//
// ‼️ CEILING: this exercises the SEARCH, not the removal of a session uid's bucket. A test cannot chown
// a directory to uid 707 without root, so the composition in removeDarwinBuckets — range check, search,
// RemoveAll over what the search returned — is checked here only as far as the range check and the
// search. The five lines that join them are not covered by anything that runs without root.
func TestEveryBucketTheUIDOwnsIsFoundNotJustTheFirst(t *testing.T) {
	root := t.TempDir()
	mine := os.Getuid()

	// Two buckets at the real depth, both ours.
	for _, p := range []string{"dk/aaaaaaaaaaaaaaaa", "8h/bbbbbbbbbbbbbbbb"} {
		if err := os.MkdirAll(filepath.Join(root, p, "C"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// A third bucket with a subtree inside it. The BUCKET is ours and must be returned; the directory
	// three levels down is not a bucket and must not be returned separately. (The first version of this
	// test expected two results and got three — because `zz/cccccccccccccccc` is a perfectly good
	// bucket, and only its child is too deep. The assertion was wrong, not the search.)
	if err := os.MkdirAll(filepath.Join(root, "zz", "cccccccccccccccc", "deeper"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "dk", "a-file"), nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "top-level-file"), nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A symlink at bucket depth pointing at a directory it would be catastrophic to follow.
	victim := filepath.Join(root, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "8h", "a-symlink")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	found, err := darwinBucketsOwnedBy(root, mine)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	sort.Strings(found)
	want := []string{
		filepath.Join(root, "8h", "bbbbbbbbbbbbbbbb"),
		filepath.Join(root, "dk", "aaaaaaaaaaaaaaaa"),
		filepath.Join(root, "zz", "cccccccccccccccc"),
	}
	if len(found) != len(want) {
		t.Fatalf("search found %v, want exactly %v — MORE THAN ONE is the point: uid 504 on this machine holds two buckets, so a search that stopped at the first would leave residue and report success", found, want)
	}
	for i := range want {
		if found[i] != want[i] {
			t.Fatalf("search found %v, want exactly %v", found, want)
		}
	}
	// `deeper` sits three levels down and is not a bucket; the symlink is not followed; neither file is
	// a directory.
	for _, f := range found {
		if strings.Contains(f, "deeper") || strings.Contains(f, "a-symlink") || strings.Contains(f, "-file") {
			t.Errorf("search returned %s", f)
		}
	}
	// AND THE SYMLINK'S TARGET IS STILL THERE. "Not returned" and "not followed" are different claims,
	// and only the second one is what keeps a planted link from turning a cache sweep into a deletion
	// somewhere else.
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the symlink target is gone: %v", err)
	}
	// A uid that owns nothing here finds nothing, rather than everything.
	if other, err := darwinBucketsOwnedBy(root, mine+12345); err != nil || len(other) != 0 {
		t.Errorf("a uid that owns nothing found %v (%v)", other, err)
	}
}

// TestDeleteSweepsTheDarwinBucketAfterTheAccountAndFailsLoudlyIfItCannot is the COMPOSITION, which the
// two tests around it do not cover: they exercise removeDarwinBuckets directly, so a Delete that never
// called it — or called it with the wrong uid, or swallowed its error — would leave them both green.
//
// It matters more than it looks. Faz A.5's residue measurement found uid 701's /private/var/folders
// bucket still on disk with its `0/ C/ T/ X/` subdirectories while `id 701` answered `no such user`,
// and that bucket is where the Metal shader cache and the simulator scratch live — the two families
// that survive `sysadminctl -deleteUser -secure` because they are OUTSIDE the home directory. A Delete
// that returned success without sweeping would report the exact opposite of what happened.
//
// ‼️ CEILING, and it is the same one TestEveryBucketTheUIDOwnsIsFoundNotJustTheFirst carries: no test
// here can chown a directory to uid 707, so the RemoveAll of a real session bucket is not exercised by
// anything that runs without root. What IS pinned below is every seam around it — that the sweep is
// reached, with the retiring record's uid, AFTER the account is gone, and that its failure becomes the
// caller's failure.
//
// It also inherits one condition from the tests above rather than owning it: HomeDir is derived, not
// injected, so Delete's post-condition needs /Users/palai-s07 to be absent. On a machine that really
// has slot 7 provisioned these tests fail at that check — which is the honest outcome, not a flake.
func TestDeleteSweepsTheDarwinBucketAfterTheAccountAndFailsLoudlyIfItCannot(t *testing.T) {
	// (a) THE SWEEP IS REACHED, AND WITH AN IN-RANGE UID. foldersRoot is a regular FILE, so the read
	// inside the sweep fails with ENOTDIR — a failure only a call that got that far can produce. And the
	// message distinguishes the two ways it could have gone wrong: a Delete passing 0, or 501, or the
	// slot number instead of the uid would come back with the range refusal instead.
	notADir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	rec := deleteScript(goodMarker())
	a := newTestAccounts(t, rec)
	a.foldersRoot = notADir

	_, _, err := a.Delete(context.Background(), 7)
	if err == nil {
		t.Fatal("Delete reported success while its cache-bucket sweep could not run — the shader cache and simulator residue would be left behind under a success")
	}
	var e *macagent.Error
	if !errors.As(err, &e) || e.Class != macagent.ClassInternal {
		t.Errorf("a failed bucket sweep answered %v, want class %s", err, macagent.ClassInternal)
	}
	if !strings.Contains(err.Error(), "cache bucket") {
		t.Errorf("Delete failed with %q, which does not say the cache bucket is what is left", err)
	}
	if strings.Contains(err.Error(), "outside the session range") {
		t.Errorf("Delete reached the sweep with a uid the range check rejects, so it never swept slot 7's bucket: %v", err)
	}

	// (b) AND THE ACCOUNT WENT FIRST. The order is the property: the bucket is swept after the record and
	// the home are verified gone, so a sweep that fails leaves an account already deleted and says so,
	// rather than leaving an account alive and claiming the bucket was the problem.
	deleted := false
	for _, call := range rec.calls {
		if strings.Join(call, " ") == "dscl . -delete /Users/palai-s07" {
			deleted = true
		}
	}
	if !deleted {
		t.Errorf("the sweep failed before the account was deleted; calls were %v", rec.calls)
	}

	// (c) THE SWEEP IS SCOPED TO THE RETIRING UID. A bucket this process owns — which stands in for the
	// operator's, since 501 is exactly the uid whose caches must never be touched — is still there after
	// a successful Delete.
	root := t.TempDir()
	operatorBucket := filepath.Join(root, "8h", "td_8rv2j6fzghcrvp4q4d9ph")
	if err := os.MkdirAll(filepath.Join(operatorBucket, "C"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rec = deleteScript(goodMarker())
	a = newTestAccounts(t, rec)
	a.foldersRoot = root

	// (d) AND THE OTHER PER-UID FAMILY GOES WITH IT. Unlike the bucket, this one is fully exercisable
	// without root — the directory is identified by its NAME, which is the uid, so a test can build the
	// real shape. Slot 7's goes; the operator's, one directory over, must not.
	messages := t.TempDir()
	a.messagesRoot = messages
	slotMessages := filepath.Join(messages, "707")
	operatorMessages := filepath.Join(messages, strconv.Itoa(os.Getuid()))
	for _, d := range []string{slotMessages, operatorMessages} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(d, "se_SecurityMessages"), make([]byte, 32768), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if _, _, err := a.Delete(context.Background(), 7); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(operatorBucket, "C")); err != nil {
		t.Fatalf("deleting slot 7 removed a bucket owned by uid %d: %v", os.Getuid(), err)
	}
	if _, err := os.Lstat(slotMessages); !os.IsNotExist(err) {
		t.Errorf("%s survived the delete (%v) — the family the INWARD sweep found is still accumulating", slotMessages, err)
	}
	if _, err := os.Stat(operatorMessages); err != nil {
		t.Fatalf("deleting slot 7 removed uid %d's message directory: %v", os.Getuid(), err)
	}
}

// TestTheMessageDirectoryIsRemovedByNameAndOnlyInsideTheNamespace covers what the bucket sweep cannot:
// this family is keyed by a directory NAME that is the uid, so the whole path — range check, lookup,
// RemoveAll — runs on any machine without root. That is the difference worth stating, because the
// sibling test one screen up has to carry a ceiling instead.
//
// It exists at all because ONE SWEEP LOOKS IN ONE DIRECTION. Walking /private/var/folders finds an
// ownerless bucket; it cannot find a family that lives under /private/var/db. The list question —
// "what does a uid own outside its home?" — is what found this, and on 2026-08-05 it answered SEVEN
// paths for the orphan uid 701: six the darwin bucket, and this one.
func TestTheMessageDirectoryIsRemovedByNameAndOnlyInsideTheNamespace(t *testing.T) {
	root := t.TempDir()
	mine := strconv.Itoa(os.Getuid())

	// A directory for a session uid, one for root, one for whoever is running this.
	for _, name := range []string{"707", "0", mine} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// Every uid outside the namespace is refused, and NOTHING is removed.
	for _, uid := range []int{0, 1, 501, macagent.UIDBase, macagent.UIDBase + macagent.MaxSlot + 1, -1} {
		if err := removeMessagesDir(root, uid); err == nil {
			t.Errorf("uid %d was accepted", uid)
		}
	}
	for _, name := range []string{"707", "0", mine} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("a refused call still deleted %s: %v", filepath.Join(root, name), err)
		}
	}

	// A session uid's directory goes, and only that one.
	if err := removeMessagesDir(root, 707); err != nil {
		t.Fatalf("removeMessagesDir(707): %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "707")); !os.IsNotExist(err) {
		t.Errorf("707 survived: %v", err)
	}
	for _, name := range []string{"0", mine} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("removing 707 also removed %s: %v", name, err)
		}
	}
	// A second call is not an error: mds creates this lazily, and a slot that never ran has none.
	if err := removeMessagesDir(root, 707); err != nil {
		t.Errorf("removing an absent directory is an error: %v", err)
	}

	// A symlink planted at that path is DECLINED, not followed — and its target is still there. "Not
	// removed" and "not followed" are different claims and only the second keeps this from becoming a
	// way to delete something else as root.
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "708")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := removeMessagesDir(root, 708); err == nil {
		t.Error("a symlink at the message-directory path was accepted")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the symlink target is gone: %v", err)
	}
}

// TestNoBucketIsRemovedForAUIDOutsideTheSessionRange is the guard that keeps this from being a way to
// delete the operator's caches. Everything on disk must still be there afterwards.
func TestNoBucketIsRemovedForAUIDOutsideTheSessionRange(t *testing.T) {
	root := t.TempDir()
	bucket := filepath.Join(root, "dk", "aaaaaaaaaaaaaaaa")
	if err := os.MkdirAll(bucket, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, uid := range []int{0, 1, 501, macagent.UIDBase, macagent.UIDBase + macagent.MaxSlot + 1, -1} {
		removed, err := removeDarwinBuckets(root, uid)
		if err == nil {
			t.Errorf("uid %d was accepted, removing %v", uid, removed)
		}
		if len(removed) != 0 {
			t.Errorf("uid %d removed %v", uid, removed)
		}
	}
	if _, err := os.Stat(bucket); err != nil {
		t.Fatalf("a refused call still deleted %s: %v", bucket, err)
	}
	// A session uid is accepted, and finds nothing here because nothing here belongs to it.
	if removed, err := removeDarwinBuckets(root, macagent.UIDBase+7); err != nil || len(removed) != 0 {
		t.Errorf("uid %d: removed %v, err %v", macagent.UIDBase+7, removed, err)
	}
	if _, err := os.Stat(bucket); err != nil {
		t.Fatalf("a session uid that owns nothing still deleted %s: %v", bucket, err)
	}
}

// TestTheSlotGroupCarriesBothPrincipals — A GROUP WITH ONE MEMBER IS A ONE-WAY DOOR.
//
// The control plane clones the repository and runs the file tools as itself, so it reaches that tree as
// its OWNER and the group is what lets the session in. The session's own commands write files too, owned
// by the session account in the session's group — and a control plane outside that group cannot read
// back the work the tenant just did, which is the diff a human approves. So both principals are members,
// and this test drives the create path with an allocation root it OWNS rather than one it inherits: the
// production skip when allocationRoot is empty is exactly the shape that would make this pass by having
// nothing to check.
func TestTheSlotGroupCarriesBothPrincipals(t *testing.T) {
	rec := &recordingRun{answers: map[string]string{
		"dscl . -create /Groups/palai-s07-grp":                                "",
		"dscl . -create /Groups/palai-s07-grp PrimaryGroupID 807":             "",
		"dscl . -create /Groups/palai-s07-grp RealName Palai session slot 07": "",
		"dseditgroup -o edit -a operator -t user palai-s07-grp":               "",
	}}
	a := newTestAccounts(t, rec)
	a.allocationRoot = t.TempDir()
	a.ownerOf = func(string) (int, error) { return 501, nil }
	a.lookupUser = func(uid int) (string, error) {
		if uid != 501 {
			t.Errorf("lookupUser(%d), want the uid that owns the allocation root (501)", uid)
		}
		return "operator", nil
	}

	if err := a.ensureSlotGroup(context.Background(), 7); err != nil {
		t.Fatalf("ensureSlotGroup: %v", err)
	}

	const want = "dseditgroup -o edit -a operator -t user palai-s07-grp"
	found := false
	for _, call := range rec.calls {
		if strings.Join(call, " ") == want {
			found = true
		}
	}
	if !found {
		var got []string
		for _, call := range rec.calls {
			got = append(got, "  "+strings.Join(call, " "))
		}
		t.Errorf("the control plane was never added to slot 7's group — it would own what it writes and be "+
			"locked out of everything the SESSION writes, so the diff a human approves would be unreadable.\n"+
			"want\n  %s\ngot\n%s", want, strings.Join(got, "\n"))
	}
}

// TestARootOwnedAllocationRootIsRefusedRatherThanJoined — root in a tenant's group is a lie that reads
// as success. Reaching this branch means the allocation root was made by the installer instead of by the
// control plane, and root already reaches every path on the machine: adding it would change nothing and
// would leave the ACTUAL control-plane user outside the group, discovering it one session later.
func TestARootOwnedAllocationRootIsRefusedRatherThanJoined(t *testing.T) {
	rec := &recordingRun{answers: map[string]string{
		"dscl . -create /Groups/palai-s07-grp":                                "",
		"dscl . -create /Groups/palai-s07-grp PrimaryGroupID 807":             "",
		"dscl . -create /Groups/palai-s07-grp RealName Palai session slot 07": "",
		"dseditgroup -o edit -a operator -t user palai-s07-grp":               "",
	}}
	a := newTestAccounts(t, rec)
	a.allocationRoot = t.TempDir()
	a.ownerOf = func(string) (int, error) { return 0, nil }
	a.lookupUser = func(int) (string, error) { return "root", nil }

	err := a.ensureSlotGroup(context.Background(), 7)
	if err == nil {
		t.Fatal("a root-owned allocation root was accepted; the control-plane user would never join the group")
	}
	if !strings.Contains(err.Error(), "owned by root") {
		t.Errorf("the refusal was %q, want one naming root ownership", err)
	}
	for _, call := range rec.calls {
		if len(call) > 0 && call[0] == "dseditgroup" {
			t.Errorf("root was added to a tenant's group anyway: %s", strings.Join(call, " "))
		}
	}
}

// TestAnExistingAccountIsMOVEDIntoItsSlotGroup — "ALREADY EXISTS" IS SUCCESS TO THE CALLER.
//
// execution.SlotAccounts treats ClassExists as the outcome it wanted, so an account left over from a
// previous run is one a session is about to USE. This machine's accounts were created in one shared
// group until 2026-08-08 and the same records now belong in a group of their own: an upgrade that only
// changed what NEW accounts get would leave every existing slot in the old group while adopt sets its
// workspace to the new one — a session owning nothing it can read, with every layer reporting success.
func TestAnExistingAccountIsMOVEDIntoItsSlotGroup(t *testing.T) {
	rec := &recordingRun{answers: map[string]string{
		// The record is there, in the OLD shared group.
		"dscl . -read /Users/palai-s07 UniqueID":                "UniqueID: 707",
		"dscl . -read /Users/palai-s07 PrimaryGroupID":          "PrimaryGroupID: 700",
		"dscl . -read /Groups/palai-s07-grp PrimaryGroupID":     "PrimaryGroupID: 807",
		"dscl . -create /Users/palai-s07 PrimaryGroupID 807":    "",
		"dseditgroup -o edit -a operator -t user palai-s07-grp": "",
	}}
	a := newTestAccounts(t, rec)
	a.allocationRoot = t.TempDir()
	a.ownerOf = func(string) (int, error) { return 501, nil }
	a.lookupUser = func(int) (string, error) { return "operator", nil }

	_, _, err := a.Create(context.Background(), 7)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Create returned %v, want the ClassExists answer for a record that is there", err)
	}

	const want = "dscl . -create /Users/palai-s07 PrimaryGroupID 807"
	for _, call := range rec.calls {
		if strings.Join(call, " ") == want {
			return
		}
	}
	var got []string
	for _, call := range rec.calls {
		got = append(got, "  "+strings.Join(call, " "))
	}
	t.Errorf("an existing account was handed back still in its old group — its session would own a "+
		"workspace it cannot read, and every layer would report success.\nwant\n  %s\ngot\n%s",
		want, strings.Join(got, "\n"))
}

// TestACreatorThatWritesAnotherUidIsREFUSED — THE DEFECT THIS GUARD WAS WRITTEN FROM, MEASURED ON A REAL
// MAC ON 2026-08-08.
//
// Five of the six accounts on this machine carried a uid the daemon never allocated: `sysadminctl
// -addUser -UID 702` through `-UID 706` produced 505, 506, 507, 508 and 509 — macOS's own next-free
// sequence — while 702..706 were every one of them FREE. sysadminctl took the flag, ignored it, and
// exited 0.
//
// WHAT IT COST: the control plane hands the DERIVED uid to whatever drops privilege, so a session would
// have dropped to a uid no account holds; and the deletion guard refuses any record whose uid is not the
// derived one — correctly — so all five became undeletable and their slots were poisoned for the
// machine's lifetime. The live plane said it in as many words: `destroy slot 05: uid 508 is not the uid
// 705 this daemon allocates`.
//
// The creator is dscl now, which cannot substitute a value. This test is about the OTHER half: whatever
// the creator is, the record is read back, and a uid that disagrees is a refusal rather than a success.
func TestACreatorThatWritesAnotherUidIsREFUSED(t *testing.T) {
	rec := &recordingRun{answers: map[string]string{
		"dscl . -create /Groups/palai-s07-grp":                                "",
		"dscl . -create /Groups/palai-s07-grp PrimaryGroupID 807":             "",
		"dscl . -create /Groups/palai-s07-grp RealName Palai session slot 07": "",
		"dseditgroup -o edit -a operator -t user palai-s07-grp":               "",
		"dscl . -create /Users/palai-s07 PrimaryGroupID 807":                  "",
		"dscl . -create /Users/palai-s07 UserShell /bin/zsh":                  "",
		"dscl . -create /Users/palai-s07 NFSHomeDirectory /Users/palai-s07":   "",
		"dscl . -create /Users/palai-s07 RealName Palai session s07":          "",
	}}
	a := newTestAccounts(t, rec)
	a.allocationRoot = t.TempDir()
	a.ownerOf = func(string) (int, error) { return 501, nil }
	a.lookupUser = func(int) (string, error) { return "operator", nil }
	// THE CREATOR LIES THE WAY sysadminctl DID: it is asked for 707 and it writes 509.
	rec.made = map[string]string{}
	rec.answers["dscl . -create /Users/palai-s07 UniqueID 707"] = ""
	rec.substituteUID = "509"

	_, _, err := a.Create(context.Background(), 7)
	if err == nil {
		t.Fatal("an account carrying a uid this daemon never allocated was reported as created; it would be " +
			"handed to a session that drops to a uid no account holds, and it could never be deleted")
	}
	if !strings.Contains(err.Error(), "509") || !strings.Contains(err.Error(), "707") {
		t.Errorf("the refusal was %q, want one naming BOTH the uid that was written and the uid that was asked for", err)
	}
	for _, call := range rec.calls {
		if call[0] == "createhomedir" {
			t.Error("the home directory was populated for an account with the wrong uid: the refusal must come first")
		}
	}
}

// TestDeleteStopsTheAccountsProcessesBeforeRemovingItsRecord — THERE IS ALWAYS A PROCESS HOLDING THIS
// uid, BY CONSTRUCTION.
//
// A session worker lives exactly as long as the account it is: no idle exit, deliberately, because
// nothing on the control-plane side could start a second one. So at delete time this uid is held, and
// removing a record out from under a running process leaves a live uid with no name. That is both a
// leak and the most likely reading of what this Mac produced on 2026-08-08 — `sysadminctl reported
// success but the record for palai-s01 still exists`.
func TestDeleteStopsTheAccountsProcessesBeforeRemovingItsRecord(t *testing.T) {
	rec := deleteScript(goodMarker())
	rec.answers["pkill -TERM -u 707"] = ""
	rec.answers["pkill -KILL -u 707"] = ""
	a := newTestAccounts(t, rec)

	if _, _, err := a.Delete(context.Background(), 7); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	killedAt, deletedAt := -1, -1
	for i, call := range rec.calls {
		switch joined := strings.Join(call, " "); {
		case joined == "pkill -TERM -u 707" && killedAt < 0:
			killedAt = i
		case joined == "dscl . -delete /Users/palai-s07":
			deletedAt = i
		}
	}
	if killedAt < 0 {
		t.Fatal("nothing signalled the account's processes: its session worker holds this uid, and the " +
			"record would be deleted out from under a live process")
	}
	if deletedAt < 0 {
		t.Fatal("the record was never deleted")
	}
	if killedAt > deletedAt {
		t.Errorf("the processes were signalled at call %d and the record deleted at %d: a uid whose name is "+
			"already gone is one no later reconciliation can name", killedAt, deletedAt)
	}
}

// TestTheHomeOnDiskIsTheHomeInTheDirectory pins the two halves of one path together. macagent.HomeDir
// is what directory services is TOLD; usersRoot joined with the account name is what this daemon
// WRITES. They are separate so a test can relocate the second; a production pair that disagreed would
// be an account whose NFSHomeDirectory points at a directory nothing ever created.
func TestTheHomeOnDiskIsTheHomeInTheDirectory(t *testing.T) {
	for _, slot := range []int{1, 7, 42, 99} {
		name, err := macagent.AccountName(slot)
		if err != nil {
			t.Fatalf("AccountName(%d): %v", slot, err)
		}
		declared, err := macagent.HomeDir(slot)
		if err != nil {
			t.Fatalf("HomeDir(%d): %v", slot, err)
		}
		if written := filepath.Join(darwinUsersRoot, name); written != declared {
			t.Errorf("slot %02d: this daemon writes %s and tells directory services %s", slot, written, declared)
		}
	}
}

// TestAPreviousTenantsHomeIsMovedAsideAndNeverInherited — SLOTS ARE REUSED, AND A DIRECTORY IS NOT.
//
// A home left by a crashed destroy, by a machine that outlived a scheme change, or by the template
// homes this daemon used to create and could not remove, would arrive owned by the NEW account with the
// OLD session's files in it. That is a cross-tenant read with every layer reporting success — the one
// outcome per-session accounts exist to prevent.
func TestAPreviousTenantsHomeIsMovedAsideAndNeverInherited(t *testing.T) {
	rec := &recordingRun{answers: map[string]string{
		"dscl . -create /Groups/palai-s07-grp":                                                                                         "",
		"dscl . -create /Groups/palai-s07-grp PrimaryGroupID 807":                                                                      "",
		"dscl . -create /Groups/palai-s07-grp RealName Palai session slot 07":                                                          "",
		"dseditgroup -o edit -a operator -t user palai-s07-grp":                                                                        "",
		"dscl . -create /Users/palai-s07 UniqueID 707":                                                                                 "",
		"dscl . -create /Users/palai-s07 PrimaryGroupID 807":                                                                           "",
		"dscl . -create /Users/palai-s07 UserShell /bin/zsh":                                                                           "",
		"dscl . -create /Users/palai-s07 NFSHomeDirectory /Users/palai-s07":                                                            "",
		"dscl . -create /Users/palai-s07 RealName Palai session s07":                                                                   "",
		"dscl . -create /Users/palai-s07 " + markerAttr + " " + markerMagic + ":1:" + testHostUUID + ":palai-s07:2026-08-05T12:00:00Z": "",
	}}
	a := newTestAccounts(t, rec)
	a.allocationRoot = t.TempDir()
	a.ownerOf = func(string) (int, error) { return 501, nil }
	a.lookupUser = func(int) (string, error) { return "operator", nil }

	// The previous tenant's home, with the previous tenant's work in it.
	stale := filepath.Join(a.usersRoot, "palai-s07")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const secret = "the previous session's private key"
	if err := os.WriteFile(filepath.Join(stale, "leftover"), []byte(secret), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, _, err := a.Create(context.Background(), 7); err != nil {
		t.Fatalf("Create: %v", err)
	}

	entries, err := os.ReadDir(stale)
	if err != nil {
		t.Fatalf("read the new home: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "leftover" {
			t.Fatal("the new account inherited the previous tenant's file: a session would open another " +
				"customer's work with every layer reporting a clean create")
		}
	}
	// AND IT IS MOVED, NOT DELETED, because deleting is exactly what macOS TCC refuses on a home — the
	// residue has to stay somewhere an operator can see it.
	moved := false
	all, _ := os.ReadDir(a.usersRoot)
	for _, e := range all {
		if strings.HasPrefix(e.Name(), "palai-s07.orphaned-") {
			moved = true
			body, _ := os.ReadFile(filepath.Join(a.usersRoot, e.Name(), "leftover"))
			if string(body) != secret {
				t.Errorf("the orphaned home does not carry what was in it: %q", body)
			}
		}
	}
	if !moved {
		t.Error("the previous home was neither inherited nor moved aside; it was destroyed, which is the " +
			"one thing TCC will not let this daemon do reliably")
	}
}

// TestAnExistingAccountWhoseHomeIsGONEGetsItBack — A RECORD THAT OUTLIVED ITS DIRECTORY.
//
// Measured on this Mac on 2026-08-08. palai-s01's record survived while its home did not, so Create
// took its idempotent branch, aligned the group, and returned ClassExists — which the caller treats as
// the outcome it wanted. The spawn that followed answered
//
//	fork/exec /usr/local/libexec/palai-session-worker: no such file or directory
//
// naming a file that was present, executable and root-owned: fork/exec reports ENOENT for a missing
// WORKING DIRECTORY too, and the worker's is the account's home. Every session then ran its commands as
// the control plane's own uid, and every layer above reported success.
func TestAnExistingAccountWhoseHomeIsGONEGetsItBack(t *testing.T) {
	rec := &recordingRun{answers: map[string]string{
		"dscl . -read /Users/palai-s07 UniqueID":            "UniqueID: 707",
		"dscl . -read /Users/palai-s07 PrimaryGroupID":      "PrimaryGroupID: 807",
		"dscl . -read /Groups/palai-s07-grp PrimaryGroupID": "PrimaryGroupID: 807",
	}}
	a := newTestAccounts(t, rec)
	a.allocationRoot = t.TempDir()
	a.ownerOf = func(string) (int, error) { return 501, nil }
	a.lookupUser = func(int) (string, error) { return "operator", nil }

	// The record is there and the directory is not — nothing is seeded under usersRoot.
	_, _, err := a.Create(context.Background(), 7)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Create returned %v, want the ClassExists answer for a record that is there", err)
	}

	diskHome := filepath.Join(a.usersRoot, "palai-s07")
	fi, serr := os.Stat(diskHome)
	if serr != nil || !fi.IsDir() {
		t.Fatalf("%s was not restored: the session worker's working directory does not exist, so fork/exec "+
			"answers ENOENT naming the BINARY and the machine reads as a broken install", diskHome)
	}
	if _, serr := os.Stat(filepath.Join(diskHome, "tmp")); serr != nil {
		t.Errorf("no TMPDIR under the restored %s", diskHome)
	}
	if got := fi.Mode().Perm(); got != 0o710 {
		t.Errorf("%s is mode %04o, want 0710 — the control plane must reach the worker's socket", diskHome, got)
	}
}

// TestAReusedSlotDoesNotLoseTheRUNNINGSessionsHome is the other side of the same branch, and the reason
// ensureHome takes a flag at all. The exists path is the SAME account still holding the slot — a control
// plane that restarted mid-session — so renaming its home there would take the running session's own
// files away from it while it works.
func TestAReusedSlotDoesNotLoseTheRUNNINGSessionsHome(t *testing.T) {
	rec := &recordingRun{answers: map[string]string{
		"dscl . -read /Users/palai-s07 UniqueID":            "UniqueID: 707",
		"dscl . -read /Users/palai-s07 PrimaryGroupID":      "PrimaryGroupID: 807",
		"dscl . -read /Groups/palai-s07-grp PrimaryGroupID": "PrimaryGroupID: 807",
	}}
	a := newTestAccounts(t, rec)
	a.allocationRoot = t.TempDir()
	a.ownerOf = func(string) (int, error) { return 501, nil }
	a.lookupUser = func(int) (string, error) { return "operator", nil }

	diskHome := filepath.Join(a.usersRoot, "palai-s07")
	if err := os.MkdirAll(diskHome, 0o710); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const inFlight = "the running session's own file"
	if err := os.WriteFile(filepath.Join(diskHome, "work"), []byte(inFlight), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, _, err := a.Create(context.Background(), 7); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Create returned %v, want ClassExists", err)
	}

	body, err := os.ReadFile(filepath.Join(diskHome, "work"))
	if err != nil || string(body) != inFlight {
		t.Fatalf("the running session's file is gone: a control plane that restarted mid-session would have "+
			"taken the tenant's work away from it (%v)", err)
	}
}
