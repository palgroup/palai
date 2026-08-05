package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
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
}

func (r *recordingRun) run(_ context.Context, name string, args ...string) (string, error) {
	argv := append([]string{name}, args...)
	r.calls = append(r.calls, argv)
	key := strings.Join(argv, " ")
	if r.fail[key] {
		return "boom", os.ErrPermission
	}
	if name == "sysadminctl" && len(args) >= 2 && args[0] == "-deleteUser" {
		if r.gone == nil {
			r.gone = map[string]bool{}
		}
		r.gone[args[1]] = true
	}
	// A read of a record that has been deleted fails, the way dscl does.
	if name == "dscl" && len(args) >= 3 && args[1] == "-read" {
		if account := strings.TrimPrefix(args[2], "/Users/"); r.gone[account] {
			return "", os.ErrNotExist
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
		run:         rec.run,
		foldersRoot: t.TempDir(),
		hostUUID:    func(context.Context) (string, error) { return testHostUUID, nil },
		now:         func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) },
		goos:        "darwin",
	}
}

// TestCreateRunsArgvAndNeverACommandLine pins the exact privileged commands, and pins that the home
// directory is VERIFIED rather than assumed.
//
// The second half is the one with history: `sysadminctl -home` assigns a home directory and does not
// create one — it says so in its own output — and createhomedir exits 0 on paths it did not populate.
// mac-sessions.sh shipped a defect reading past exactly this, so the check is here and so is its test.
func TestCreateRunsArgvAndNeverACommandLine(t *testing.T) {
	rec := &recordingRun{answers: map[string]string{
		"sysadminctl -addUser palai-s07 -fullName Palai session s07 -UID 707 -GID 20 -shell /bin/zsh -home /Users/palai-s07":           "",
		"dscl . -create /Users/palai-s07 " + markerAttr + " " + markerMagic + ":1:" + testHostUUID + ":palai-s07:2026-08-05T12:00:00Z": "",
		"createhomedir -c -u palai-s07": "",
	}}
	a := newTestAccounts(t, rec)

	// It gets as far as the post-condition and then refuses, because /Users/palai-s07 does not exist on
	// the machine running this test. That refusal IS the assertion: a Create that returned success here
	// would be one that handed out an account with nowhere to run.
	_, _, err := a.Create(context.Background(), 7)
	if err == nil {
		t.Fatal("Create succeeded although /Users/palai-s07 does not exist; the home post-condition is not firing")
	}
	if !strings.Contains(err.Error(), "/Users/palai-s07 is not a directory") {
		t.Errorf("Create failed with %q, want a refusal naming the missing home", err)
	}

	want := []string{
		"sysadminctl -addUser palai-s07 -fullName Palai session s07 -UID 707 -GID 20 -shell /bin/zsh -home /Users/palai-s07",
		"dscl . -create /Users/palai-s07 " + markerAttr + " " + markerMagic + ":1:" + testHostUUID + ":palai-s07:2026-08-05T12:00:00Z",
		"createhomedir -c -u palai-s07",
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
		"sysadminctl -deleteUser palai-s07 -secure":          "Deleting record for palai-s07",
	}}
}

func goodMarker() string {
	return markerMagic + ":1:" + testHostUUID + ":palai-s07:2026-08-05T12:00:00Z"
}

// TestDeleteUsesTheSecureFlagThisMachineActuallyHas pins the spelling, which was measured rather than
// copied: `sysadminctl` on Darwin 25.3.0 prints `-deleteUser <user name> [-secure || -keepHome]`. The
// plan for this phase wrote `--secure`, and that is not a flag this binary has.
func TestDeleteUsesTheSecureFlagThisMachineActuallyHas(t *testing.T) {
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
		if strings.Join(call, " ") == "sysadminctl -deleteUser palai-s07 -secure" {
			found = true
		}
		for _, arg := range call {
			if arg == "--secure" {
				t.Errorf("Delete passed --secure, which sysadminctl does not accept: %v", call)
			}
		}
	}
	if !found {
		t.Errorf("Delete never ran `sysadminctl -deleteUser palai-s07 -secure`; it ran %v", rec.calls)
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
		if strings.Join(call, " ") == "sysadminctl -deleteUser palai-s07 -secure" {
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
	if _, _, err := a.Delete(context.Background(), 7); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(operatorBucket, "C")); err != nil {
		t.Fatalf("deleting slot 7 removed a bucket owned by uid %d: %v", os.Getuid(), err)
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
