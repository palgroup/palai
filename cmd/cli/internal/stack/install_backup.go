package stack

// This file is the INSTALLATION-level backup/restore for a whole Palai stack: a consistent
// Postgres dump + the object-store data + a manifest, packed into one archive that restores
// into a SEPARATE clean stack (E14 T4, spec §17 install-vs-run distinction).
//
// It is a DIFFERENT layer from the RUN-level checkpoint restore in
// apps/control-plane/internal/execution/{snapshot,restore}.go, which revives ONE run inside a
// live stack. The two share no code and no names: that layer speaks "checkpoint/snapshot"; this
// one speaks "install backup". Keep it that way.
//
// Reachability: like `doctor`, this reaches the running stack's containers BY NAME through the
// Docker socket (docker exec / docker run), never through host-published ports — the production
// profile keeps Postgres and the object store on the internal network (deploy/compose/production.yml
// `ports: !reset []`). So `palai backup` works against a hardened stack with only the TLS edge
// published, exactly the deployment it is meant to protect.
//
// Credential hygiene: the archive holds real tenant DATA (that is its job), but the manifest
// carries only ids + checksums, never a secret; and the Postgres password is read from the
// in-container file-secret inside a `sh -c` wrapper, so it never appears in the host process's
// argv or in a log line.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	backupKind            = "palai-install-backup"
	backupManifestVersion = 1

	// Archive member names (stable — the manifest checksums are keyed to them).
	memberManifest    = "manifest.json"
	memberDBDump      = "db.dump"
	memberObjectStore = "object-store.tar"

	// dockerOpTimeout bounds a single pg_dump / pg_restore / volume-tar. Local-tier stacks
	// finish well inside it; a real deployment with a large DB tunes it upward.
	dockerOpTimeout = 10 * time.Minute

	// postgresImageRef is the digest-pinned postgres image (postgresDigest lives in doctor.go).
	// A one-off container of it copies the object-store VOLUME with a tool set (tar, sh) the
	// scratch-based seaweedfs image does not carry — and it is already pulled by the stack.
	postgresImageRef = "postgres@" + postgresDigest
)

// seedRows are the four identity rows a FRESH install is born with (identity/store.go firstOrg/
// firstProject/firstPrincipal/firstKey). The empty-target gate excludes exactly these by id: any
// OTHER row in an org-bearing table is provisioned/workload data the restore must not overwrite.
var seedRows = map[string]string{
	"organizations": "org_local",
	"projects":      "prj_local",
	"principals":    "prin_local",
	"api_keys":      "key_local",
}

// bootInfraTables are org-bearing tables a fresh boot fills on its own (the runner enrolls and takes
// leases). They are NOT tenant data, so the empty-target gate skips them — else a fresh target that
// has already enrolled its runner would false-positive as "not empty".
var bootInfraTables = map[string]bool{"runners": true, "runner_pools": true, "runner_leases": true}

// BackupManifest is the machine-readable index inside a backup archive. It records what the
// backup captured (migration version, the tenant/org + project ids, a sample response id for the
// run-retrieval verify) and the integrity checksums (whole-member + per-object). It holds NO
// secret — restore/verify treat it as the source of truth for what a healthy restore must match.
type BackupManifest struct {
	Kind              string    `json:"kind"`
	Version           int       `json:"version"`
	CreatedAt         time.Time `json:"created_at"`
	Project           string    `json:"project"`           // source stack project — reference only
	MigrationVersion  int       `json:"migration_version"` // max(schema_migrations.version)
	OrganizationIDs   []string  `json:"organization_ids"`  // tenant ids captured (RLS-bypassed superuser read)
	ProjectIDs        []string  `json:"project_ids"`
	SampleResponseID  string    `json:"sample_response_id,omitempty"` // one run id verify re-retrieves
	DBDumpSHA256      string    `json:"db_dump_sha256"`
	ObjectStoreSHA256 string    `json:"object_store_sha256"`
	// Objects is the per-object sha256 of the object-store copy. The packaged control-plane sets
	// no PALAI_S3_ENDPOINT, so no S3 objects are wired and these are the object-store DATA VOLUME's
	// files (byte-for-byte). Each entry lets a restore prove the copy is intact file-by-file.
	// ponytail: the volume is tar'd LIVE — crash-consistent-enough for today's empty/idle store, but
	// once artifacts are actually written to S3 a consistent copy must quiesce the store or enumerate
	// S3 objects (docs/operations/backup-restore.md ceiling), and those entries become the objects.
	Objects []objectChecksum `json:"objects"`
}

// objectChecksum is one stored object-store file's path + content sha256 + size.
type objectChecksum struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// safeID guards the one place a manifest value flows into a SQL literal (the sample-response
// retrieval): ids are our own (resp_/org_/prj_ + hex), so an id outside this set is skipped
// rather than interpolated. Belt-and-suspenders — the manifest is our own output.
var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// InstallBackup captures the running stack into one archive at outPath (default
// palai-backup-<project>-<UTC>.tar.gz in the cwd). It dumps Postgres consistently (pg_dump -Fc),
// copies the object-store data volume, and writes a manifest of ids + checksums.
func InstallBackup(outPath string) error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerOpTimeout)
	defer cancel()

	pg := cfg.containerName("postgres")

	migVer, err := pgQueryInt(ctx, pg, "SELECT coalesce(max(version), 0) FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	orgIDs, err := pgQueryList(ctx, pg, "SELECT id FROM organizations ORDER BY id")
	if err != nil {
		return fmt.Errorf("read organization ids: %w", err)
	}
	projIDs, err := pgQueryList(ctx, pg, "SELECT id FROM projects ORDER BY id")
	if err != nil {
		return fmt.Errorf("read project ids: %w", err)
	}
	sampleResp, err := pgQueryScalar(ctx, pg, "SELECT id FROM responses ORDER BY created_at DESC LIMIT 1")
	if err != nil {
		return fmt.Errorf("read sample response id: %w", err)
	}

	fmt.Fprintln(os.Stderr, "backup: dumping Postgres (pg_dump -Fc)…")
	dbDump, err := pgDump(ctx, pg)
	if err != nil {
		return fmt.Errorf("pg_dump: %w", err)
	}
	fmt.Fprintln(os.Stderr, "backup: copying the object-store volume…")
	objTar, err := objectStoreTar(ctx, cfg.objectVolume())
	if err != nil {
		return fmt.Errorf("copy object store: %w", err)
	}
	objSums, err := objectChecksums(objTar)
	if err != nil {
		return fmt.Errorf("checksum object store: %w", err)
	}

	m := BackupManifest{
		Kind:              backupKind,
		Version:           backupManifestVersion,
		CreatedAt:         time.Now().UTC(),
		Project:           cfg.Project,
		MigrationVersion:  migVer,
		OrganizationIDs:   orgIDs,
		ProjectIDs:        projIDs,
		SampleResponseID:  sampleResp,
		DBDumpSHA256:      sha256Hex(dbDump),
		ObjectStoreSHA256: sha256Hex(objTar),
		Objects:           objSums,
	}

	if outPath == "" {
		outPath = fmt.Sprintf("palai-backup-%s-%s.tar.gz", cfg.Project, m.CreatedAt.Format("20060102T150405Z"))
	}
	// Write to a .partial and atomically rename on success, so a failed backup never leaves a
	// fresh-mtime PARTIAL under the final name — which the retention prune (deploy/systemd/
	// palai-backup-prune.sh) would keep as "newest" while deleting the last GOOD archive. The
	// .partial suffix also sits OUTSIDE the prune glob (palai-backup-*.tar.gz), so a leaked temp
	// is invisible to prune.
	tmpPath := outPath + ".partial"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create archive %s: %w", tmpPath, err)
	}
	if err := writeBackupArchive(f, m, dbDump, objTar); err != nil {
		f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write archive: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close archive %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize archive %s: %w", outPath, err)
	}
	fmt.Fprintf(os.Stderr, "backup written: migration v%d, %d org(s), %d object-file(s)\n",
		m.MigrationVersion, len(m.OrganizationIDs), len(m.Objects))
	fmt.Println(outPath)
	return nil
}

// InstallRestore restores an archive into a running EMPTY target stack. It REFUSES a target that
// already holds tenant rows (fail-closed — no data is ever overwritten), verifies the archive's
// member checksums, then replaces Postgres (pg_restore --clean) and the object-store volume with
// the writers stopped for the swap.
func InstallRestore(archivePath string) error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerOpTimeout)
	defer cancel()

	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive %s: %w", archivePath, err)
	}
	defer f.Close()
	m, dbDump, objTar, err := readBackupArchive(f)
	if err != nil {
		return err
	}

	pg := cfg.containerName("postgres")
	cp := cfg.containerName("control-plane")
	runner := cfg.containerName("runner")
	objStore := cfg.containerName("object-store")

	// M2 (no TOCTOU window): stop the writers FIRST, then run the pre-flight checks — a client write
	// that landed between the check and the stop would otherwise be destroyed by the restore. If a
	// pre-flight refuses, restart the writers so the target is left exactly as we found it (running).
	fmt.Fprintln(os.Stderr, "restore: stopping writers (control-plane, runner)…")
	if err := dockerRun(ctx, "stop", cp, runner); err != nil {
		return fmt.Errorf("stop writers: %w", err)
	}
	abort := func(err error) error {
		_ = dockerRun(ctx, "start", cp, runner)
		return err
	}

	// S4: the dump's schema must match the target's. pg_restore --clean into a MISMATCHED schema
	// leaves orphan objects and rewinds schema_migrations, so the target's next boot crash-loops in
	// Migrate with only "did not become healthy" as the symptom. Refuse before touching any data.
	liveMig, err := pgQueryInt(ctx, pg, "SELECT coalesce(max(version), 0) FROM schema_migrations")
	if err != nil {
		return abort(fmt.Errorf("read target migration version: %w", err))
	}
	if liveMig != m.MigrationVersion {
		return abort(fmt.Errorf("refusing to restore: target schema v%d != backup v%d; bring the target up on the backup's migration version first", liveMig, m.MigrationVersion))
	}

	// M1 (cardinal no-clobber): fail-closed over EVERY org-bearing (FORCE-RLS) table, so provisioned
	// data created UNDER org_local (projects, api_keys, secret_refs, model_routes, …) — none of which
	// the old orgs/responses/runs count caught — refuses the restore instead of being silently wiped.
	excess, err := tenantDataExcess(ctx, pg)
	if err != nil {
		return abort(fmt.Errorf("check target is empty: %w", err))
	}
	if len(excess) > 0 {
		return abort(fmt.Errorf("refusing to restore: target is not empty — holds tenant data [%s]; restore only into a fresh install", strings.Join(excess, ", ")))
	}

	fmt.Fprintln(os.Stderr, "restore: loading Postgres (pg_restore --clean)…")
	if err := pgRestore(ctx, pg, dbDump); err != nil {
		// N9: past this point the target is HALF-RESTORED (schema/data partly replaced, writers down);
		// tell the operator not to trust it.
		return fmt.Errorf("pg_restore: %w — the target is now HALF-RESTORED and must NOT be used; re-init it (`palai local reset --confirm`) before retrying", err)
	}

	fmt.Fprintln(os.Stderr, "restore: replacing the object-store volume…")
	if err := dockerRun(ctx, "stop", objStore); err != nil {
		return fmt.Errorf("stop object store: %w — the target is HALF-RESTORED; re-init it before retrying", err)
	}
	if err := objectStoreRestore(ctx, cfg.objectVolume(), objTar); err != nil {
		return fmt.Errorf("restore object store: %w — the target is HALF-RESTORED; re-init it before retrying", err)
	}
	if err := dockerRun(ctx, "start", objStore); err != nil {
		return fmt.Errorf("start object store: %w", err)
	}

	fmt.Fprintln(os.Stderr, "restore: starting writers…")
	if err := dockerRun(ctx, "start", cp, runner); err != nil {
		return fmt.Errorf("start writers: %w", err)
	}
	// Wait on the container's healthcheck, NOT the bootstrap-key API: the restore just replaced
	// the target's key_local row with the backup's, so the target's own bootstrap key no longer
	// authenticates (the backup's does). The healthcheck (compose's /healthz probe) is
	// unauthenticated and profile-agnostic — it needs no host-published port.
	if err := waitForHealthy(ctx, cp); err != nil {
		return fmt.Errorf("target did not come back after restore: %w", err)
	}
	fmt.Fprintf(os.Stderr, "restore complete: migration v%d, %d org(s) loaded into %s\n",
		m.MigrationVersion, len(m.OrganizationIDs), cfg.Project)
	return nil
}

// InstallRestoreVerify proves a restored target matches its backup manifest: the archive's member
// checksums verify, the live migration version + tenant ids match the manifest, and the sample
// response is retrievable from the restored database (proving the tenant data is queryable).
func InstallRestoreVerify(archivePath string) error {
	cfg, p, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerOpTimeout)
	defer cancel()

	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive %s: %w", archivePath, err)
	}
	defer f.Close()
	// readBackupArchive re-hashes every member and errors on any mismatch — that IS the
	// "checksum match against the manifest" check.
	m, _, _, err := readBackupArchive(f)
	if err != nil {
		return fmt.Errorf("archive checksum: %w", err)
	}

	pg := cfg.containerName("postgres")
	var fails []string
	pass := func(name, detail string) { fmt.Printf("%-22s ok    %s\n", name, detail) }
	failf := func(name, format string, a ...any) {
		detail := fmt.Sprintf(format, a...)
		fmt.Printf("%-22s FAIL  %s\n", name, detail)
		fails = append(fails, name+": "+detail)
	}

	pass("archive_checksum", fmt.Sprintf("db+object-store members match manifest (%d object-file(s))", len(m.Objects)))

	// migration-version match
	liveMig, err := pgQueryInt(ctx, pg, "SELECT coalesce(max(version), 0) FROM schema_migrations")
	switch {
	case err != nil:
		failf("migration_version", "read live version: %v", err)
	case liveMig != m.MigrationVersion:
		failf("migration_version", "live v%d != manifest v%d", liveMig, m.MigrationVersion)
	default:
		pass("migration_version", fmt.Sprintf("v%d matches manifest", liveMig))
	}

	// tenant-id match. N8: the backup reads the org ids BEFORE pg_dump, so the dump (a later snapshot)
	// is a superset — an org created concurrently during a live backup lands in the restored data but
	// not the manifest. The load-bearing invariant is that NO backed-up tenant went missing, so we
	// check manifest ⊆ restored rather than strict equality (a concurrent extra org is not a failure).
	liveOrgs, err := pgQueryList(ctx, pg, "SELECT id FROM organizations ORDER BY id")
	if err != nil {
		failf("tenant_ids", "read live org ids: %v", err)
	} else if missing := missingFrom(liveOrgs, m.OrganizationIDs); len(missing) > 0 {
		failf("tenant_ids", "restored data is missing manifest org id(s) %v", missing)
	} else {
		pass("tenant_ids", fmt.Sprintf("all %d manifest org id(s) present in restored data", len(m.OrganizationIDs)))
	}

	// sample run-retrieval — prove the restored tenant data is queryable
	switch {
	case m.SampleResponseID == "":
		pass("run_retrieval", "no sample response recorded (backup captured no runs)")
	case !safeID.MatchString(m.SampleResponseID):
		failf("run_retrieval", "manifest sample id %q is not a safe id", m.SampleResponseID)
	default:
		n, err := pgQueryInt(ctx, pg, "SELECT count(*) FROM responses WHERE id = '"+m.SampleResponseID+"'")
		if err != nil {
			failf("run_retrieval", "query response %s: %v", m.SampleResponseID, err)
		} else if n != 1 {
			failf("run_retrieval", "response %s not retrievable (count=%d)", m.SampleResponseID, n)
		} else {
			pass("run_retrieval", fmt.Sprintf("response %s retrievable from restored data", m.SampleResponseID))
		}
	}

	// S5: tenant isolation must survive the restore. pg_dump carries FORCE ROW LEVEL SECURITY + the
	// tenant_isolation policies mechanically; this catches a restore that somehow landed with RLS off
	// (a silent cross-tenant breach the RLS-bypassing superuser queries above would never notice).
	forced, ferr := pgQueryInt(ctx, pg, "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relforcerowsecurity")
	policies, perr := pgQueryInt(ctx, pg, "SELECT count(*) FROM pg_policies WHERE schemaname = 'public' AND policyname = 'tenant_isolation'")
	switch {
	case ferr != nil || perr != nil:
		failf("rls_isolation", "read RLS catalog: %v / %v", ferr, perr)
	case forced == 0 || policies == 0:
		failf("rls_isolation", "RLS is DISABLED on restored data (forced=%d, tenant_isolation policies=%d)", forced, policies)
	case policies < forced:
		// Every FORCE-RLS table must carry its tenant_isolation policy; fewer means isolation is
		// incomplete on some table. (More policies than forced tables is fine — mig 000029 pattern.)
		failf("rls_isolation", "tenant_isolation policies (%d) < forced-RLS tables (%d) — isolation is incomplete", policies, forced)
	default:
		pass("rls_isolation", fmt.Sprintf("%d org-bearing tables FORCE RLS, %d tenant_isolation policies", forced, policies))
	}

	// M3: secret canary. Secrets are AES-256-GCM-sealed under the SOURCE stack's master key; a restore
	// to a target with a different master key leaves every secret undecryptable while the rows survive
	// — a silently-dead install. If secret_refs has rows, prove one decrypts under the TARGET's master
	// key (the operator must have carried the source key), so a mismatch is caught here, not at the
	// first provider call.
	secretN, serr := pgQueryInt(ctx, pg, "SELECT count(*) FROM secret_refs")
	switch {
	case serr != nil:
		failf("secret_decrypt", "count secret_refs: %v", serr)
	case secretN == 0:
		pass("secret_decrypt", "no secret_refs to verify")
	default:
		verifySecretCanary(ctx, pg, p.masterKey, pass, failf)
	}

	if len(fails) > 0 {
		return fmt.Errorf("restore verify: %d check(s) failed", len(fails))
	}
	fmt.Println("restore verify: all checks green")
	return nil
}

// --- container / volume identity (compose names them <project>-<service>-1 and <project>_<vol>) ---

func (c Config) containerName(service string) string { return c.Project + "-" + service + "-1" }
func (c Config) objectVolume() string                { return c.Project + "_palai-objects" }

// --- pure helpers (unit-tested) ---

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// tenantDataExcess reports the org-bearing tables that hold data beyond a fresh install's baseline —
// the empty-target gate's evidence. It enumerates the FORCE-RLS (tenant-scoped) tables from the live
// catalog (so a table a future migration adds is covered automatically, matching mig 000029's intent)
// and counts each, excluding the 4 boot-seed rows and the runner-enrollment tables. Any returned
// "table=count" means the target holds provisioned/workload data a restore would clobber.
func tenantDataExcess(ctx context.Context, container string) ([]string, error) {
	tables, err := pgQueryList(ctx, container,
		"SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace "+
			"WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relforcerowsecurity ORDER BY c.relname")
	if err != nil {
		return nil, err
	}
	q := buildExcessQuery(tables)
	if q == "" {
		return nil, nil
	}
	return pgQueryList(ctx, container, q)
}

// buildExcessQuery assembles the per-table count query over the FORCE-RLS tables: the 4 identity
// tables exclude their seed row by id, the runner-enrollment tables are skipped, everything else is
// counted in full. Table names come from the catalog and the seed ids are constants, so the built
// SQL carries no caller input. Returns "" for an empty table list.
func buildExcessQuery(tables []string) string {
	var parts []string
	for _, t := range tables {
		if bootInfraTables[t] {
			continue
		}
		where := ""
		if seed, ok := seedRows[t]; ok {
			where = " WHERE id <> '" + seed + "'"
		}
		parts = append(parts, fmt.Sprintf("SELECT '%s' AS t, count(*) AS n FROM %s%s", t, t, where))
	}
	if len(parts) == 0 {
		return ""
	}
	return "SELECT t || '=' || n FROM (" + strings.Join(parts, " UNION ALL ") + ") s WHERE n > 0 ORDER BY t"
}

// missingFrom returns the want ids not present in have (order-independent).
func missingFrom(have, want []string) []string {
	present := make(map[string]bool, len(have))
	for _, h := range have {
		present[h] = true
	}
	var missing []string
	for _, w := range want {
		if !present[w] {
			missing = append(missing, w)
		}
	}
	return missing
}

// verifySecretCanary proves the target can decrypt a stored secret under its master key — the M3
// guard against a restore whose master key differs from the backup's (every secret otherwise dead).
// It reads the target master-key file the operator must have carried, pulls one ciphertext, and
// AES-256-GCM-opens it. The CLI cannot import the control-plane's internal secret store, so it
// mirrors that store's seal format (openSealed); the component test proves the two interoperate.
func verifySecretCanary(ctx context.Context, container, masterKeyPath string, pass func(string, string), failf func(string, string, ...any)) {
	keyHex, err := readTrimmed(masterKeyPath)
	if err != nil {
		failf("secret_decrypt", "secret_refs present but master key %s is unreadable (%v); the SOURCE master key MUST be carried to the target — the restored secrets are undecryptable without it", masterKeyPath, err)
		return
	}
	key, err := parseMasterKeyHex(keyHex)
	if err != nil {
		failf("secret_decrypt", "target master key: %v", err)
		return
	}
	ctHex, err := pgQueryScalar(ctx, container, "SELECT encode(ciphertext, 'hex') FROM secret_refs LIMIT 1")
	if err != nil {
		failf("secret_decrypt", "read a stored ciphertext: %v", err)
		return
	}
	sealed, err := hex.DecodeString(ctHex)
	if err != nil {
		failf("secret_decrypt", "decode stored ciphertext: %v", err)
		return
	}
	if _, err := openSealed(key, sealed); err != nil {
		failf("secret_decrypt", "a stored secret does NOT decrypt under the target master key (%v) — the SOURCE master key was not carried; every restored secret is dead", err)
		return
	}
	pass("secret_decrypt", "a stored secret decrypts under the target master key")
}

// parseMasterKeyHex decodes the 32-byte AES-256 master key from hex — mirrors identity.ParseMasterKey
// (the CLI cannot import that internal package).
func parseMasterKeyHex(s string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("not valid hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("got %d bytes, want 32 (AES-256)", len(key))
	}
	return key, nil
}

// openSealed AES-256-GCM-decrypts a nonce||ciphertext blob — byte-compatible with the control-plane
// secret store's seal (identity/secrets.go: a 12-byte GCM nonce prefix). A wrong key or a truncated
// blob fails, which is exactly the master-key-mismatch signal the canary needs.
func openSealed(masterKey, sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	n := gcm.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("ciphertext shorter than the nonce")
	}
	return gcm.Open(nil, sealed[:n], sealed[n:], nil)
}

// objectChecksums walks the object-store tar and records each regular file's content sha256.
func objectChecksums(objTar []byte) ([]objectChecksum, error) {
	tr := tar.NewReader(bytes.NewReader(objTar))
	var sums []objectChecksum
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read object-store tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read %s from object-store tar: %w", hdr.Name, err)
		}
		sums = append(sums, objectChecksum{Path: hdr.Name, SHA256: sha256Hex(body), Size: int64(len(body))})
	}
	sort.Slice(sums, func(i, j int) bool { return sums[i].Path < sums[j].Path })
	return sums, nil
}

func marshalManifest(m BackupManifest) ([]byte, error) { return json.MarshalIndent(m, "", "  ") }

// --- archive (gzip'd tar of manifest.json + db.dump + object-store.tar) ---

func writeBackupArchive(w io.Writer, m BackupManifest, dbDump, objTar []byte) error {
	manifestJSON, err := marshalManifest(m)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, member := range []struct {
		name string
		body []byte
	}{
		{memberManifest, manifestJSON},
		{memberDBDump, dbDump},
		{memberObjectStore, objTar},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name:     member.name,
			Mode:     0o600,
			Size:     int64(len(member.body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		if _, err := tw.Write(member.body); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// readBackupArchive reads the three members and verifies db.dump and object-store.tar against the
// manifest's checksums. A mismatch (corruption/tamper) is an error, so a restore/verify never
// proceeds on damaged bytes.
func readBackupArchive(r io.Reader) (BackupManifest, []byte, []byte, error) {
	var m BackupManifest
	gz, err := gzip.NewReader(r)
	if err != nil {
		return m, nil, nil, fmt.Errorf("open archive gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	members := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, nil, nil, fmt.Errorf("read archive tar: %w", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return m, nil, nil, fmt.Errorf("read member %s: %w", hdr.Name, err)
		}
		members[hdr.Name] = body
	}
	manifestJSON, ok := members[memberManifest]
	if !ok {
		return m, nil, nil, fmt.Errorf("archive missing %s", memberManifest)
	}
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return m, nil, nil, fmt.Errorf("decode %s: %w", memberManifest, err)
	}
	if m.Kind != backupKind {
		return m, nil, nil, fmt.Errorf("not a palai install backup (kind=%q)", m.Kind)
	}
	dbDump, ok := members[memberDBDump]
	if !ok {
		return m, nil, nil, fmt.Errorf("archive missing %s", memberDBDump)
	}
	objTar, ok := members[memberObjectStore]
	if !ok {
		return m, nil, nil, fmt.Errorf("archive missing %s", memberObjectStore)
	}
	if got := sha256Hex(dbDump); got != m.DBDumpSHA256 {
		return m, nil, nil, fmt.Errorf("db.dump checksum mismatch: archive %s, manifest %s", got, m.DBDumpSHA256)
	}
	if got := sha256Hex(objTar); got != m.ObjectStoreSHA256 {
		return m, nil, nil, fmt.Errorf("object-store.tar checksum mismatch: archive %s, manifest %s", got, m.ObjectStoreSHA256)
	}
	return m, dbDump, objTar, nil
}

// --- docker-mediated stack access (by container name / volume; no host ports, no compose file) ---

// pgShell wraps a Postgres client tool so the password is read from the in-container file-secret
// and exported to the tool's environment — never placed in the host process's argv or a log line.
// A trust-auth local socket ignores it; a password-auth socket uses it. Either way it is invisible
// outside the container.
func pgShell(tool string) string {
	return `export PGPASSWORD="$(cat /run/secrets/pg_password 2>/dev/null)"; exec ` + tool
}

// pgDump captures a consistent custom-format dump (pg_dump runs in a single snapshot). Superuser
// palai bypasses the FORCE'd RLS, so every tenant's rows are captured.
func pgDump(ctx context.Context, container string) ([]byte, error) {
	return dockerCapture(ctx, nil, "exec", container, "sh", "-c",
		pgShell("pg_dump -Fc -U palai -d palai"))
}

// pgRestore loads a custom-format dump with --clean --if-exists: it drops the fresh target's
// schema objects and recreates them from the dump, then loads the data. --no-owner maps ownership
// to the connecting superuser. pg_restore reports a non-zero exit only on a real error.
func pgRestore(ctx context.Context, container string, dbDump []byte) error {
	_, err := dockerCapture(ctx, dbDump, "exec", "-i", container, "sh", "-c",
		pgShell("pg_restore --clean --if-exists --no-owner --exit-on-error -U palai -d palai"))
	return err
}

// pgQueryScalar feeds one SQL statement over stdin (so no value is quoted into argv) and returns
// the single trimmed cell (empty when no rows).
func pgQueryScalar(ctx context.Context, container, query string) (string, error) {
	out, err := dockerCapture(ctx, []byte(query+";\n"), "exec", "-i", container, "sh", "-c",
		pgShell("psql -tAq -U palai -d palai"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func pgQueryInt(ctx context.Context, container, query string) (int, error) {
	s, err := pgQueryScalar(ctx, container, query)
	if err != nil {
		return 0, err
	}
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

func pgQueryList(ctx context.Context, container, query string) ([]string, error) {
	s, err := pgQueryScalar(ctx, container, query)
	if err != nil {
		return nil, err
	}
	if s == "" {
		return nil, nil
	}
	return strings.Split(s, "\n"), nil
}

// objectStoreTar streams the object-store data VOLUME as a tar from a one-off postgres-image
// container (read-only mount). It reads the volume directly, so it does not depend on the
// scratch-based object-store image carrying a shell/tar.
func objectStoreTar(ctx context.Context, volume string) ([]byte, error) {
	return dockerCapture(ctx, nil, "run", "--rm", "-v", volume+":/data:ro",
		"--entrypoint", "tar", postgresImageRef, "-cf", "-", "-C", "/data", ".")
}

// objectStoreRestore clears the (stopped) object-store volume and extracts the backup tar into it,
// from a one-off postgres-image container.
func objectStoreRestore(ctx context.Context, volume string, objTar []byte) error {
	_, err := dockerCapture(ctx, objTar, "run", "--rm", "-i", "-v", volume+":/data",
		"--entrypoint", "sh", postgresImageRef, "-c",
		`rm -rf /data/* /data/.[!.]* /data/..?* 2>/dev/null; exec tar -xf - -C /data`)
	return err
}

// dockerCapture runs `docker <args...>` with an optional stdin, returning captured stdout and
// wrapping a failure with the captured stderr. ponytail: the dump/tar are buffered whole in
// memory — fine for local-tier stacks; a large DB wants a streamed pipe to a temp file.
func dockerCapture(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return out.Bytes(), nil
}

// waitForHealthy polls a container's Docker healthcheck (compose's /healthz probe) until it is
// healthy — profile-agnostic readiness that needs no host-published port and no API credential.
func waitForHealthy(ctx context.Context, container string) error {
	deadline := time.Now().Add(90 * time.Second)
	var last string
	for {
		out, err := dockerCapture(ctx, nil, "inspect", "--format", "{{.State.Health.Status}}", container)
		if err == nil {
			last = strings.TrimSpace(string(out))
			if last == "healthy" {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not become healthy (last status %q)", container, last)
		}
		time.Sleep(time.Second)
	}
}

// dockerRun runs `docker <args...>` (stop/start) with progress on stderr, under ctx's deadline.
func dockerRun(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
