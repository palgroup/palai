//go:build component

package coordinator

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/palgroup/palai/storage"
)

// TestMigrationBoundedLockTimesOut proves applyMigration bounds every migration with lock_timeout: a
// migration whose DDL needs a lock another transaction holds ACCESS EXCLUSIVE fails FAST with 55P03
// (lock_not_available) instead of blocking the boot indefinitely (plan §T1 bounded lock). The probe
// migration times out, so its ALTER never commits and the table it names is left unchanged.
//
// IT OWNS ITS OWN TARGET TABLE, and that is a repair rather than a tidy-up. It used to lock a table the
// schema happened to carry, which coupled it two ways: A.2 Task 6 dropped that table and left this test
// locking something that does not exist, and while the table DID exist an ACCESS EXCLUSIVE lock held for
// the length of this test was taken on a table other tests in this shared database read. A test that
// creates its own target can be neither broken by a schema change nor a source of contention.
func TestMigrationBoundedLockTimesOut(t *testing.T) {
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	cs, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	// The probe's own target: nothing else in this shared database reads or writes it.
	const target = "bounded_lock_probe_target"
	owner, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect table owner: %v", err)
	}
	defer owner.Close(context.Background())
	if _, err := owner.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+target+` (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create the probe target: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(context.Background(), `DROP TABLE IF EXISTS `+target) })

	// A separate connection (the superuser the URL carries) holds ACCESS EXCLUSIVE on it, so a migration
	// that must lock the table blocks until it either gets the lock or hits lock_timeout.
	holder, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect lock holder: %v", err)
	}
	defer holder.Close(context.Background())
	holdTx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder tx: %v", err)
	}
	defer func() { _ = holdTx.Rollback(context.Background()) }()
	if _, err := holdTx.Exec(ctx, `LOCK TABLE `+target+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("hold ACCESS EXCLUSIVE on %s: %v", target, err)
	}

	// A short lock_timeout so the blocked probe fails in ~200ms instead of hanging the test.
	t.Setenv("PALAI_MIGRATE_LOCK_TIMEOUT_MS", "200")
	probe := storage.Migration{
		// Version 1 is deliberate: the probe is meant to FAIL, so its transaction aborts before the
		// journal insert is reached — but if it ever did commit, a version already in the chain is
		// ON CONFLICT DO NOTHING rather than a marker that puts this shared database AHEAD of the
		// binary, which would make the next boot's preflight refuse it.
		Version: 1,
		Name:    "bounded_lock_probe",
		Up:      `ALTER TABLE ` + target + ` ADD COLUMN IF NOT EXISTS __bounded_lock_probe INTEGER`,
	}
	err = cs.applyMigration(ctx, probe)
	if err == nil {
		t.Fatal("applyMigration acquired a lock the holder owns; want a lock_timeout failure")
	}
	if got := pgErrCode(err); got != "55P03" {
		t.Fatalf("applyMigration under a held lock code = %q (%v), want 55P03 lock_not_available", got, err)
	}
}

// pgErrCode returns the SQLSTATE of a PostgreSQL error, or "" if err is not one.
func pgErrCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
