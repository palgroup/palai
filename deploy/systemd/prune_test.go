// E14 T5 — checks for the systemd deliverables that a Go test can prove without a systemd host:
// the retention/prune script's keep-newest logic, and the outbound-only shape of the runner unit
// (no socket activation). `systemd-analyze verify` (in a Linux container) is the separate gate
// that validates the unit SYNTAX; this file guards the two behaviours a syntax check would miss.
package systemd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runPrune execs the prune script against dir with the given retention-days.
func runPrune(t *testing.T, dir, days string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "palai-backup-prune.sh", dir, days)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("prune %s %s: %v\n%s", dir, days, err, out)
	}
}

// mkArchive writes a backup-named file and back-dates its mtime by ageDays.
func mkArchive(t *testing.T, dir, name string, ageDays int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// TestPruneRemovesOldKeepsRecent: archives older than the retention window are removed, recent
// ones stay.
func TestPruneRemovesOldKeepsRecent(t *testing.T) {
	dir := t.TempDir()
	old1 := mkArchive(t, dir, "palai-backup-p-20250101T000000Z.tar.gz", 40)
	old2 := mkArchive(t, dir, "palai-backup-p-20250102T000000Z.tar.gz", 30)
	recent := mkArchive(t, dir, "palai-backup-p-20260722T000000Z.tar.gz", 1)
	// A non-backup file must never be touched.
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(other, time.Now().Add(-99*24*time.Hour), time.Now().Add(-99*24*time.Hour))
	// A leaked .partial (a crashed backup, before install_backup.go's atomic rename) is OUTSIDE the
	// prune glob, so it is never counted as an archive nor deleted (S8 — the two layers cooperate).
	partial := mkArchive(t, dir, "palai-backup-p-20250103T000000Z.tar.gz.partial", 35)

	runPrune(t, dir, "14")

	if exists(old1) || exists(old2) {
		t.Fatal("prune did not remove archives older than the retention window")
	}
	if !exists(recent) {
		t.Fatal("prune removed a recent archive")
	}
	if !exists(other) {
		t.Fatal("prune removed a non-backup file — it must only touch palai-backup-*.tar.gz")
	}
	if !exists(partial) {
		t.Fatal("prune deleted a .partial — it sits outside the palai-backup-*.tar.gz glob and must be ignored")
	}
}

// TestPruneAlwaysKeepsNewest: when EVERY archive is older than the window, the single newest is
// still kept — a stalled install never prunes itself to zero.
func TestPruneAlwaysKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	oldest := mkArchive(t, dir, "palai-backup-p-20240101T000000Z.tar.gz", 400)
	newest := mkArchive(t, dir, "palai-backup-p-20240601T000000Z.tar.gz", 300)

	runPrune(t, dir, "14")

	if exists(oldest) {
		t.Fatal("prune kept an older archive when a newer one exists")
	}
	if !exists(newest) {
		t.Fatal("prune removed the newest archive — it must always keep at least the newest")
	}
}

// directives returns a unit file's directive lines (comments and blanks stripped), joined — so a
// comment that MENTIONS a directive name is not mistaken for the directive itself.
func directives(t *testing.T, unit string) string {
	t.Helper()
	raw, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		out = append(out, trimmed)
	}
	return strings.Join(out, "\n")
}

// TestRunnerUnitIsOutboundOnly: the runner unit reflects the outbound-only contract — it never
// activates a socket. (The binary + compose invariant prove the property; this pins that the
// unit does not silently re-introduce a listener via socket activation.)
func TestRunnerUnitIsOutboundOnly(t *testing.T) {
	body := directives(t, "palai-runner.service")
	for _, forbidden := range []string{"ListenStream", "ListenDatagram", "[Socket]"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("palai-runner.service has directive %q — the runner is outbound-only, it must not be socket-activated", forbidden)
		}
	}
	if !strings.Contains(body, "Restart=always") {
		t.Fatal("palai-runner.service lost Restart=always")
	}
}

// TestBackupServiceRequisiteAndNoEarlyChdir pins MF3 + MF4: the backup unit must NOT pull a stopped
// stack up (Requisite, not Wants) and must NOT chdir via WorkingDirectory (which 200/CHDIRs on a
// clean host before any ExecStartPre could create the dir — the unit mkdirs inside ExecStart).
func TestBackupServiceRequisiteAndNoEarlyChdir(t *testing.T) {
	body := directives(t, "palai-backup.service")
	if strings.Contains(body, "Wants=palai-stack.service") {
		t.Fatal("palai-backup.service Wants=palai-stack.service — a stopped stack would be STARTED by the timer; use Requisite=")
	}
	if !strings.Contains(body, "Requisite=palai-stack.service") {
		t.Fatal("palai-backup.service must Requisite=palai-stack.service (fail if the stack is not already up)")
	}
	if strings.Contains(body, "WorkingDirectory=") {
		t.Fatal("palai-backup.service has WorkingDirectory= — chdir happens before ExecStartPre and 200/CHDIRs on a clean host; mkdir+cd inside ExecStart instead")
	}
}
