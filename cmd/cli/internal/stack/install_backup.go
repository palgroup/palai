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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	// seedOrgID is the control-plane's fixed boot-seeded first organization
	// (apps/control-plane/internal/identity/store.go firstOrg). A fresh install carries exactly
	// this tenant, so the empty-target gate excludes it when deciding a target is unused.
	seedOrgID = "org_local"
)

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
	// files (byte-for-byte). When the S3 write-path is wired they are the stored objects. Either
	// way each entry lets a restore prove the object-store copy is intact file-by-file.
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
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create archive %s: %w", outPath, err)
	}
	defer f.Close()
	if err := writeBackupArchive(f, m, dbDump, objTar); err != nil {
		return fmt.Errorf("write archive: %w", err)
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

	// Fail-closed empty-target gate: any provisioned tenant (an organization beyond the fixed
	// boot seed) OR any workload (a response/run) refuses the restore. A fresh install carries
	// only the seeded org_local tenant and no runs, so it passes; a stack that has been
	// provisioned or used does not — no live data is ever overwritten.
	count, err := pgQueryInt(ctx, pg,
		"SELECT (SELECT count(*) FROM organizations WHERE id <> '"+seedOrgID+"') "+
			"+ (SELECT count(*) FROM responses) + (SELECT count(*) FROM runs)")
	if err != nil {
		return fmt.Errorf("check target is empty: %w", err)
	}
	if err := assertEmptyTarget(count); err != nil {
		return err
	}

	// The writers hold the DB pool and are the only object-store writers; stop them for the swap.
	cp := cfg.containerName("control-plane")
	runner := cfg.containerName("runner")
	objStore := cfg.containerName("object-store")
	fmt.Fprintln(os.Stderr, "restore: stopping writers (control-plane, runner)…")
	if err := dockerRun(ctx, "stop", cp, runner); err != nil {
		return fmt.Errorf("stop writers: %w", err)
	}

	fmt.Fprintln(os.Stderr, "restore: loading Postgres (pg_restore --clean)…")
	if err := pgRestore(ctx, pg, dbDump); err != nil {
		return fmt.Errorf("pg_restore: %w", err)
	}

	fmt.Fprintln(os.Stderr, "restore: replacing the object-store volume…")
	if err := dockerRun(ctx, "stop", objStore); err != nil {
		return fmt.Errorf("stop object store: %w", err)
	}
	if err := objectStoreRestore(ctx, cfg.objectVolume(), objTar); err != nil {
		return fmt.Errorf("restore object store: %w", err)
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

	// tenant-id match (set equality)
	liveOrgs, err := pgQueryList(ctx, pg, "SELECT id FROM organizations ORDER BY id")
	if err != nil {
		failf("tenant_ids", "read live org ids: %v", err)
	} else if !sameIDSet(liveOrgs, m.OrganizationIDs) {
		failf("tenant_ids", "live %v != manifest %v", liveOrgs, m.OrganizationIDs)
	} else {
		pass("tenant_ids", fmt.Sprintf("%d org id(s) match manifest", len(liveOrgs)))
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

// assertEmptyTarget is the fail-closed restore gate: a target with any tenant row is refused so
// a restore can never overwrite live data.
func assertEmptyTarget(tenantRows int) error {
	if tenantRows != 0 {
		return fmt.Errorf("refusing to restore: target stack is not empty (%d tenant row(s)); restore only into a fresh install", tenantRows)
	}
	return nil
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

// sameIDSet reports set-equality of two id slices (order-independent).
func sameIDSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, x := range a {
		seen[x]++
	}
	for _, y := range b {
		seen[y]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

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
