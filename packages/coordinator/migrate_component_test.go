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
// migration times out, so its ALTER never commits and the shared organizations table is left unchanged.
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

	// A separate connection (the superuser the URL carries) holds ACCESS EXCLUSIVE on organizations, so a
	// migration that must lock the table blocks until it either gets the lock or hits lock_timeout.
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
	if _, err := holdTx.Exec(ctx, `LOCK TABLE organizations IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("hold ACCESS EXCLUSIVE on organizations: %v", err)
	}

	// A short lock_timeout so the blocked probe fails in ~200ms instead of hanging the test.
	t.Setenv("PALAI_MIGRATE_LOCK_TIMEOUT_MS", "200")
	probe := storage.Migration{
		Version: 1, // below journalIntroVersion, so no journal row is attempted
		Name:    "bounded_lock_probe",
		Up:      `ALTER TABLE organizations ADD COLUMN IF NOT EXISTS __bounded_lock_probe INTEGER`,
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
