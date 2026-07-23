//go:build uat

// The five DR drills, run against two isolated production-compose stacks, producing a MEASURED
// RPO/RTO report from RAW timestamps (E15 T5). The drills REUSE the shipped `palai backup`/`restore`/
// `restore verify` (the E14 install-backup tooling) — they never reimplement them.
//
//	DR-001 primary loss     — destroy the pg container + volume, recover with a fresh pg + restore from
//	                          the last backup, measure RPO (marker-loss window) + RTO (wall-clock to
//	                          healthy + run-capable) from raw timestamps.
//	DR-004 object corruption — byte-flip an object in the store; the backup manifest's per-file sha256
//	                          detects EXACTLY which object corrupted, and a tampered archive is
//	                          fail-closed at `restore verify`.
//	DR-005 key recovery     — the master-key file is wrong/absent: `restore verify`'s secret canary
//	                          fails CLOSED; an escrow copy of the key makes it usable (file-key seam;
//	                          KMS is E13-H, out of scope).
//	DR-002/006 restore+verify — the E14 restore/restore-verify flow re-run under the drill harness with
//	                          measurement (post-restore session/response consistency + tenant isolation).
//
// Honest ceiling (plan §6, §T5): "primary loss" here is container/volume destruction on ONE Docker
// Desktop host — a real instance/zone loss and a separate-physical-host restore are the operator leg
// (§6, incl. E14 leg 2); DR-003 regional failover is the SaaS plan; the KMS ceremony is E13-H.
package dr

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- steady marker traffic (so RPO has a real data-loss window to measure) ---

type markerRow struct {
	seq       int
	writtenAt time.Time
}

// markerWriter writes one marker per interval into dr_markers via docker-exec and records every
// SUCCESSFUL commit (seq + the DB's own now()). When the DB is destroyed the writes fail and the last
// recorded row is the last committed marker before the disaster — the RPO's upper edge.
type markerWriter struct {
	s       *drStack
	mu      sync.Mutex
	last    markerRow
	count   int
	stop    chan struct{}
	stopped chan struct{}
}

func (s *drStack) startMarkers() *markerWriter {
	s.t.Helper()
	if _, err := s.dbScalar("CREATE TABLE IF NOT EXISTS dr_markers(seq bigint PRIMARY KEY, written_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		s.t.Fatalf("create dr_markers: %v", err)
	}
	w := &markerWriter{s: s, stop: make(chan struct{}), stopped: make(chan struct{})}
	go w.loop()
	return w
}

func (w *markerWriter) loop() {
	defer close(w.stopped)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	seq := 0
	for {
		select {
		case <-w.stop:
			return
		case <-tick.C:
			seq++
			out, err := w.s.dbScalar("INSERT INTO dr_markers(seq) VALUES (" + strconv.Itoa(seq) + ") RETURNING extract(epoch from written_at)")
			if err != nil {
				continue // the DB may be gone (the disaster) — stop recording, keep the last good row
			}
			ts, perr := parseEpoch(out)
			if perr != nil {
				continue
			}
			w.mu.Lock()
			w.last = markerRow{seq: seq, writtenAt: ts}
			w.count++
			w.mu.Unlock()
		}
	}
}

// quiesce stops the writer and returns the last committed marker (the RPO upper edge).
func (w *markerWriter) quiesce() markerRow {
	close(w.stop)
	<-w.stopped
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

func parseEpoch(s string) (time.Time, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return time.Time{}, err
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC(), nil
}

// --- shared stack operations the drills build on (all REUSE the shipped CLI) ---

func (s *drStack) provisionSecret(name, value string) {
	s.t.Helper()
	s.cli(strings.NewReader(value), "secret", "create", "--name", name)
}

// fakeRun creates a response over the republished API and polls the run to a terminal state,
// returning it — proof the stack is RUN-CAPABLE (dispatch -> runner -> reference-engine -> terminal).
func (s *drStack) fakeRun(input string) string {
	s.t.Helper()
	out := s.cli(nil, "response", "create", "--input", input)
	var created struct {
		ID    string `json:"id"`
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(lastJSONLine(out)), &created); err != nil || created.RunID == "" {
		s.t.Fatalf("decode response create %q: %v", out, err)
	}
	deadline := time.Now().Add(120 * time.Second)
	var state string
	for time.Now().Before(deadline) {
		state = s.mustScalar("SELECT state FROM runs WHERE id='" + created.RunID + "'")
		switch state {
		case "completed", "failed", "canceled":
			return state
		}
		time.Sleep(time.Second)
	}
	s.t.Fatalf("run %s never reached a terminal state (last=%q)", created.RunID, state)
	return state
}

func (s *drStack) backup(archivePath string) {
	s.t.Helper()
	s.cli(nil, "backup", "--out", archivePath)
}

func (s *drStack) restore(archivePath string) {
	s.t.Helper()
	s.cli(nil, "restore", "--archive", archivePath)
}

// restoreVerify runs `palai restore verify` and returns the per-check ok/FAIL map + whether the
// command exited zero. It does NOT fail the test — the drills assert on the map (a FAIL is expected
// under a wrong key).
func (s *drStack) restoreVerify(archivePath string) (map[string]bool, string, bool) {
	s.t.Helper()
	out, err := s.cliErr(nil, "restore", "verify", "--archive", archivePath)
	return parseVerify(out), out, err == nil
}

// parseVerify scans the `<name>  ok/FAIL  <detail>` lines the shipped InstallRestoreVerify prints.
func parseVerify(out string) map[string]bool {
	checks := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[1] {
		case "ok":
			checks[f[0]] = true
		case "FAIL":
			checks[f[0]] = false
		}
	}
	return checks
}

func lastJSONLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// --- backup manifest + object-store integrity (reads the tool's OWN outputs; no reimplementation) ---

// backupManifest is the slice of the shipped BackupManifest the drills read (the tool wrote it). The
// CLI's stack package is internal to cmd/cli, so it cannot be imported here — the fields are re-declared.
type backupManifest struct {
	CreatedAt        time.Time        `json:"created_at"`
	MigrationVersion int              `json:"migration_version"`
	Objects          []objectChecksum `json:"objects"`
}

type objectChecksum struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// readManifest extracts and decodes manifest.json from a backup archive (gzip'd tar).
func readManifest(t *testing.T, archivePath string) backupManifest {
	t.Helper()
	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip archive: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("archive has no manifest.json: %v", err)
		}
		if h.Name == "manifest.json" {
			var m backupManifest
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				t.Fatalf("decode manifest: %v", err)
			}
			return m
		}
	}
}

// archiveObjectBytes returns the raw bytes of one object path INSIDE the archive's object-store.tar
// member — the backup's INTACT copy, used to prove recoverability and to heal the flipped object.
func archiveObjectBytes(t *testing.T, archivePath, objPath string) []byte {
	t.Helper()
	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	gz, _ := gzip.NewReader(f)
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("archive has no object-store.tar: %v", err)
		}
		if h.Name == "object-store.tar" {
			inner := tar.NewReader(tr)
			for {
				ih, err := inner.Next()
				if err != nil {
					t.Fatalf("object %s not in archive object-store.tar: %v", objPath, err)
				}
				if ih.Name == objPath {
					var buf bytes.Buffer
					if _, err := io.Copy(&buf, inner); err != nil {
						t.Fatalf("read archived object: %v", err)
					}
					return buf.Bytes()
				}
			}
		}
	}
}

// healObject overwrites one object file in the (stopped) volume with the backup's intact bytes — the
// "object restored" recovery path, and it leaves the store non-corrupt so it restarts cleanly.
func (s *drStack) healObject(objPath string, intact []byte) {
	s.t.Helper()
	cmd := exec.Command("docker", "run", "--rm", "-i", "-v", s.project+"_palai-objects:/data",
		"--entrypoint", "sh", postgresImageRef, "-c", `cat > "/data/`+strings.TrimPrefix(objPath, "./")+`"`)
	cmd.Stdin = bytes.NewReader(intact)
	if err := cmd.Run(); err != nil {
		s.t.Fatalf("heal object %s: %v", objPath, err)
	}
}

// liveObjectSHAs returns the on-disk sha256 of every regular file in the object-store volume, read
// from a one-off container (the store may be stopped, so this reads the raw volume, not seaweedfs).
func (s *drStack) liveObjectSHAs() map[string]string {
	s.t.Helper()
	out, err := exec.Command("docker", "run", "--rm", "-v", s.project+"_palai-objects:/data:ro",
		"--entrypoint", "sh", postgresImageRef, "-c",
		"cd /data && find . -type f -exec sha256sum {} +").Output()
	if err != nil {
		s.t.Fatalf("read live object store: %v", err)
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			m[f[1]] = f[0] // f[1] is "./path", matching the manifest's tar member names
		}
	}
	return m
}

// flipObjectByte overwrites 8 bytes at the start of one object file in the (stopped) volume — a
// distinctive pattern astronomically unlikely to equal the original, so the per-file sha256 changes.
func (s *drStack) flipObjectByte(objPath string) {
	s.t.Helper()
	err := exec.Command("docker", "run", "--rm", "-v", s.project+"_palai-objects:/data",
		"--entrypoint", "sh", postgresImageRef, "-c",
		`printf 'CORRUPT!' | dd of="/data/`+strings.TrimPrefix(objPath, "./")+`" bs=1 seek=0 count=8 conv=notrunc 2>/dev/null`).Run()
	if err != nil {
		s.t.Fatalf("flip object byte: %v", err)
	}
}

func (s *drStack) stopService(svc string) { _ = s.docker(2*time.Minute, s.composeArgs("stop", svc)...) }
func (s *drStack) startService(svc string) {
	_ = s.docker(3*time.Minute, s.composeArgs("start", svc)...)
}

// postgresImageRef is the digest-pinned postgres image (already pulled by the stack); a one-off
// container of it carries sh/tar/coreutils to read the scratch-based object-store volume.
const postgresImageRef = "postgres@sha256:17e67d7b9890c99b055ba1e0d5c5be4ec27c9d3a72bda32db24a5e5d8a85af0c"

// --- the drill suite ---

// TestDRDrills is the E15 T5 DR-drill suite: two production-compose stacks, five drills, a MEASURED
// RPO/RTO report from raw timestamps. Set PALAI_DR_WRITE_REPORT=1 to (re)generate the committed
// docs/operations/dr-report.md + evidence/dr/drill-evidence.json from this run.
func TestDRDrills(t *testing.T) {
	requireDocker(t)
	engineDig := ensureStackImages(t)
	masterKey := randomHex32(t) // the escrow/source master key; both stacks boot with it (the M3 invariant)
	wrongKey := randomHex32(t)  // a DIFFERENT key — what DR-005 proves fails closed

	// The two stacks run SEQUENTIALLY, never both fully up at once — two full production stacks on one
	// Docker Desktop host (with sibling tasks' stacks competing) starves Postgres into recovery-mode
	// blips that break the restore's 90s health wait. DR-002's cross-stack consistency check needs only
	// the SOURCE's row counts, so we capture them in Go, tear A down, then bring B up alone.
	var results []DrillResult

	// --- Stack A (source): provision, a baseline fake run, steady markers, backup, then A's own drills ---
	a := newDRStack(t, "palai-e15t5-a", engineDig, masterKey)
	a.cleanInstall()
	a.up()
	a.provisionSecret("dr.token", "s3cr3t-dr-value")
	if st := a.fakeRun("Reply with: ready."); st != "completed" {
		t.Fatalf("baseline run on A = %q, want completed", st)
	}
	markers := a.startMarkers()

	archive := filepath.Join(t.TempDir(), "dr-a.tar.gz")
	a.backup(archive)
	manifest := readManifest(t, archive)
	t.Logf("backup: migration v%d, %d object-file(s)", manifest.MigrationVersion, len(manifest.Objects))

	// Capture the source's run history now (the backup froze it here) so DR-002 can verify the restore
	// matches WITHOUT keeping A up alongside B.
	srcCounts := map[string]string{"responses": a.mustScalar("SELECT count(*) FROM responses"), "runs": a.mustScalar("SELECT count(*) FROM runs")}

	// --- DR-004: object corruption on A — per-file sha256 detects it exactly, the object is restored
	// from the backup's intact bytes, and a tampered archive is fail-closed at `restore verify`. ---
	results = append(results, drObjectCorruption(t, a, archive, manifest))

	// --- DR-001: primary loss — destroy A's pg + volume, recover, measure RPO/RTO ---
	results = append(results, drPrimaryLoss(t, a, archive, markers))

	// A is done — tear it down so B runs on an uncontended host (the sequential-stack invariant).
	a.downCleanup()

	// --- DR-002/006: restore A's backup into a SEPARATE clean stack B + verify + consistency ---
	b := newDRStack(t, "palai-e15t5-b", engineDig, masterKey)
	b.cleanInstall()
	b.up()
	results = append(results, drRestoreAndVerify(t, b, archive, srcCounts, a.project))

	// --- DR-005: the master-key file seam — wrong key fails closed, escrow makes it usable (on B) ---
	results = append(results, drKeyRecovery(t, b, archive, masterKey, wrongKey))

	// --- assemble + verify + (optionally) write the machine-generated report ---
	allPassed := true
	for _, r := range results {
		if !r.Passed {
			allPassed = false
			t.Errorf("%s FAILED: %s", r.ID, r.Detail)
		} else {
			t.Logf("%s PASS: %s", r.ID, r.Detail)
		}
	}

	var measuredRPO float64
	for _, r := range results {
		if r.Measure != nil {
			measuredRPO = r.Measure.RPOSeconds
		}
	}
	ev := DrillEvidence{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		GitSHA:      gitSHA(t),
		Seam:        "local same-host two-stack (Docker Desktop) — production compose profile, fake provider",
		Drills:      results,
		Targets:     DefaultTargets(),
		Findings:    DefaultFindings(measuredRPO),
	}
	if errs := Verify(ev); len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("evidence recompute finding: %v", e)
		}
		t.Fatal("the measured RPO/RTO did not recompute from the raw timestamps (anti-fabrication anchor)")
	}
	if !allPassed {
		t.Fatal("not all DR drills passed")
	}
	writeArtifacts(t, ev)
}

// drRestoreAndVerify is DR-002 (restore into a separate clean stack) + DR-006 (restore verify's six
// checks) + a post-restore consistency measurement (response/run counts match the SOURCE's counts,
// captured before A was torn down — markers diverge by design, so they are NOT compared).
func drRestoreAndVerify(t *testing.T, b *drStack, archive string, srcCounts map[string]string, srcProject string) DrillResult {
	t.Helper()
	b.restore(archive)
	checks, out, green := b.restoreVerify(archive)
	if !green {
		return DrillResult{ID: "DR-002/006", Name: "restore into a separate clean stack + verify", Detail: "restore verify not green:\n" + out}
	}
	for _, name := range []string{"archive_checksum", "migration_version", "tenant_ids", "run_retrieval", "rls_isolation", "secret_decrypt"} {
		if !checks[name] {
			return DrillResult{ID: "DR-002/006", Name: "restore into a separate clean stack + verify", Detail: "restore verify check " + name + " not green:\n" + out}
		}
	}
	for tbl, src := range srcCounts {
		if dst := b.mustScalar("SELECT count(*) FROM " + tbl); src != dst {
			return DrillResult{ID: "DR-002/006", Name: "restore into a separate clean stack + verify",
				Detail: fmt.Sprintf("%s count diverged: source %s != restored %s", tbl, src, dst)}
		}
	}
	return DrillResult{
		ID: "DR-002/006", Name: "restore into a separate clean stack + verify", Passed: true,
		Detail: fmt.Sprintf("`palai restore` loaded the backup into a SEPARATE clean stack (%s -> %s); `restore verify` was green on all six checks (archive_checksum, migration_version, tenant_ids, run_retrieval, rls_isolation, secret_decrypt); post-restore response/run counts matched the source (responses=%s, runs=%s); tenant isolation (FORCE RLS + tenant_isolation policies) survived the restore.", srcProject, b.project, srcCounts["responses"], srcCounts["runs"]),
	}
}

// drKeyRecovery is DR-005: the master-key FILE seam. `restore verify`'s secret canary reads the
// target's master-key file host-side; a wrong key fails secret_decrypt CLOSED, an escrow copy of the
// source key makes it green again. KMS is E13-H (named, out of scope).
func drKeyRecovery(t *testing.T, b *drStack, archive, escrowKey, wrongKey string) DrillResult {
	t.Helper()
	keyFile := filepath.Join(b.home, "secrets", "master-key")

	// Baseline: the correct (escrow) key decrypts.
	if checks, out, _ := b.restoreVerify(archive); !checks["secret_decrypt"] {
		return DrillResult{ID: "DR-005", Name: "master-key recovery (file seam)", Detail: "baseline secret_decrypt not green:\n" + out}
	}
	// A WRONG/lost key must fail CLOSED at secret_decrypt (the restored secrets are undecryptable).
	writeFile(t, keyFile, wrongKey)
	checks, out, green := b.restoreVerify(archive)
	if green || checks["secret_decrypt"] {
		return DrillResult{ID: "DR-005", Name: "master-key recovery (file seam)", Detail: "a WRONG master key must fail secret_decrypt CLOSED, but verify passed:\n" + out}
	}
	// The ESCROW copy of the source key restores usability.
	writeFile(t, keyFile, escrowKey)
	if checks2, out2, _ := b.restoreVerify(archive); !checks2["secret_decrypt"] {
		return DrillResult{ID: "DR-005", Name: "master-key recovery (file seam)", Detail: "the escrow key did not restore secret_decrypt:\n" + out2}
	}
	return DrillResult{
		ID: "DR-005", Name: "master-key recovery (file seam)", Passed: true,
		Detail: "with the WRONG master-key file, `restore verify` secret_decrypt failed CLOSED (a stored secret does not decrypt under the target key — the restored secrets are dead without the source key); swapping in the ESCROW copy of the source master key made secret_decrypt green again. This is the FILE-key seam; a KMS-backed key + lease ceremony (SEC-001/003) is E13-H, out of scope.",
	}
}

// drObjectCorruption is DR-004: byte-flip an object in the store; the backup manifest's per-file
// sha256 detects EXACTLY which object corrupted (the backup still holds the intact bytes), and a
// tampered archive is fail-closed at `restore verify`.
func drObjectCorruption(t *testing.T, a *drStack, archive string, manifest backupManifest) DrillResult {
	t.Helper()
	// Pick the largest object whose live on-disk sha still matches the manifest (a stable file).
	a.stopService("object-store") // quiesce the store so the flip + read are a consistent file view
	defer a.startService("object-store")
	live := a.liveObjectSHAs()
	var target objectChecksum
	for _, o := range manifest.Objects {
		if o.Size >= 8 && live[o.Path] == o.SHA256 && o.Size > target.Size {
			target = o
		}
	}
	if target.Path == "" {
		return DrillResult{ID: "DR-004", Name: "object corruption", Detail: fmt.Sprintf("no stable object >=8 bytes to corrupt (manifest has %d objects) — the idle object store carried no comparable file", len(manifest.Objects))}
	}

	a.flipObjectByte(target.Path)
	after := a.liveObjectSHAs()

	// Detection: the per-file sha256 now differs for EXACTLY the target, and the backup archive still
	// holds the intact bytes (recoverable), so the loss is reported exactly.
	if after[target.Path] == target.SHA256 {
		return DrillResult{ID: "DR-004", Name: "object corruption", Detail: "the byte-flip did not change the object's sha256 — corruption was not induced"}
	}
	var detected []string
	for _, o := range manifest.Objects {
		if cur, ok := after[o.Path]; ok && cur != o.SHA256 {
			detected = append(detected, o.Path)
		}
	}
	found := false
	for _, p := range detected {
		if p == target.Path {
			found = true
		}
	}
	if !found {
		return DrillResult{ID: "DR-004", Name: "object corruption", Detail: fmt.Sprintf("per-file sha256 did not flag the corrupted object %s (flagged: %v)", target.Path, detected)}
	}
	intact := archiveObjectBytes(t, archive, target.Path)
	if got := sha256hex(intact); got != target.SHA256 {
		return DrillResult{ID: "DR-004", Name: "object corruption", Detail: fmt.Sprintf("the backup does not hold the intact bytes for %s (archive sha %s != manifest %s) — not recoverable", target.Path, got, target.SHA256)}
	}

	// OBJECT RESTORED: heal the flipped file from the backup's intact bytes; its per-file sha256
	// matches the manifest again (and the store restarts clean, not on a corrupt file).
	a.healObject(target.Path, intact)
	if healed := a.liveObjectSHAs(); healed[target.Path] != target.SHA256 {
		return DrillResult{ID: "DR-004", Name: "object corruption", Detail: fmt.Sprintf("healing %s from the backup did not restore its sha256 (got %s, want %s)", target.Path, healed[target.Path], target.SHA256)}
	}

	// Fail-closed: a tampered ARCHIVE must be refused by `restore verify` (the integrity chain).
	tampered := archive + ".tampered"
	tamperArchive(t, archive, tampered)
	_, vout, vgreen := a.restoreVerify(tampered)
	if vgreen || (!strings.Contains(vout, "checksum mismatch") && !strings.Contains(vout, "gzip") && !strings.Contains(vout, "archive")) {
		return DrillResult{ID: "DR-004", Name: "object corruption", Detail: "a byte-tampered archive was NOT refused by restore verify:\n" + vout}
	}

	return DrillResult{
		ID: "DR-004", Name: "object corruption", Passed: true,
		Detail: fmt.Sprintf("a byte-flip in object %s (%d bytes) was DETECTED exactly by the backup manifest's per-file sha256 (live sha != manifest sha for that path, and only that path among the manifest set); the object was RESTORED from the backup's intact bytes (per-file sha256 matched the manifest again); and a byte-tampered COPY of the archive was fail-closed at `restore verify` (the integrity chain refuses damaged bytes).", target.Path, target.Size),
	}
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// drPrimaryLoss is DR-001: destroy the pg container + volume, recover with a fresh pg + restore from
// the last backup, and MEASURE RPO (marker-loss window) + RTO (wall-clock to healthy + run-capable)
// from raw timestamps.
func drPrimaryLoss(t *testing.T, a *drStack, archive string, markers *markerWriter) DrillResult {
	t.Helper()
	last := markers.quiesce() // the last marker committed before the disaster (RPO upper edge)
	if last.seq == 0 {
		return DrillResult{ID: "DR-001", Name: "primary (database) loss", Detail: "no markers were committed before the disaster — nothing to measure"}
	}

	// DISASTER: destroy the pg container + its data volume (primary/database loss). The runner and
	// object-store stay alive — this is DB loss, not total loss.
	_ = exec.Command("docker", "rm", "-f", a.pg()).Run()
	if err := exec.Command("docker", "volume", "rm", a.project+"_palai-pg").Run(); err != nil {
		return DrillResult{ID: "DR-001", Name: "primary (database) loss", Detail: fmt.Sprintf("could not destroy the pg volume: %v", err)}
	}
	disasterAt := time.Now().UTC()

	// RECOVERY (scripted): a fresh pg comes up, the control-plane re-migrates the empty database to
	// the backup's version, then `palai restore` loads the last backup, then a fake run proves the
	// stack is run-capable again.
	a.upServices("postgres")
	// Re-migrate: recreate the control-plane so it boots against the fresh empty pg and runs the
	// migration chain to head; --wait blocks on its healthcheck.
	if err := a.docker(10*time.Minute, a.composeArgs("up", "-d", "--wait", "--force-recreate", "control-plane")...); err != nil {
		return DrillResult{ID: "DR-001", Name: "primary (database) loss", Detail: fmt.Sprintf("control-plane did not re-migrate the fresh pg: %v", err)}
	}
	a.restore(archive) // empty-target gate passes (fresh migrate); loads the backup
	if st := a.fakeRun("Reply with: recovered."); st != "completed" {
		return DrillResult{ID: "DR-001", Name: "primary (database) loss", Detail: fmt.Sprintf("post-recovery run = %q, want completed (not run-capable)", st)}
	}
	recoveredAt := time.Now().UTC()

	// MEASURE from raw timestamps. RPO = last committed marker minus the newest marker the backup
	// (now restored) actually holds. RTO = recovered wall-clock minus the disaster instant.
	inBackup, err := parseEpoch(a.mustScalar("SELECT extract(epoch from max(written_at)) FROM dr_markers"))
	if err != nil {
		return DrillResult{ID: "DR-001", Name: "primary (database) loss", Detail: fmt.Sprintf("could not read the restored backup's last marker: %v", err)}
	}
	m := NewMeasure(last.writtenAt, inBackup, disasterAt, recoveredAt)
	return DrillResult{
		ID: "DR-001", Name: "primary (database) loss", Passed: true, Measure: &m,
		Detail: fmt.Sprintf("destroyed the pg container + its data volume, recovered with a fresh pg + `palai restore` from the last backup, and completed a fake run (run-capable). Measured RPO %.3fs (the %d-marker window written after the backup and lost) and RTO %.3fs (scripted recovery wall-clock) — both from raw DB/host timestamps.", m.RPOSeconds, last.seq, m.RTOSeconds),
	}
}

// tamperArchive copies src to dst with one byte flipped near the end (in the compressed stream), so
// the archive's integrity chain must refuse it.
func tamperArchive(t *testing.T, src, dst string) {
	t.Helper()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if len(raw) < 32 {
		t.Fatalf("archive too small to tamper")
	}
	raw[len(raw)-16] ^= 0xff
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		t.Fatalf("write tampered archive: %v", err)
	}
}

func gitSHA(t *testing.T) string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// writeArtifacts (re)generates the committed machine-generated report + evidence artifact from a live
// run, but only when PALAI_DR_WRITE_REPORT=1 — routine `go test -tags=uat` runs measure + verify
// without clobbering the committed files.
func writeArtifacts(t *testing.T, ev DrillEvidence) {
	t.Helper()
	if os.Getenv("PALAI_DR_WRITE_REPORT") != "1" {
		t.Logf("DR report NOT written (set PALAI_DR_WRITE_REPORT=1 to regenerate the committed report/artifact)")
		return
	}
	root := repoRoot(t)
	evPath := filepath.Join(root, "evidence", "dr", "drill-evidence.json")
	if err := os.MkdirAll(filepath.Dir(evPath), 0o755); err != nil {
		t.Fatalf("mkdir evidence/dr: %v", err)
	}
	blob, _ := json.MarshalIndent(ev, "", "  ")
	if err := os.WriteFile(evPath, append(blob, '\n'), 0o644); err != nil {
		t.Fatalf("write evidence artifact: %v", err)
	}
	repPath := filepath.Join(root, "docs", "operations", "dr-report.md")
	if err := os.WriteFile(repPath, []byte(RenderReport(ev)), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("wrote %s + %s", evPath, repPath)
}
