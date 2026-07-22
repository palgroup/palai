//go:build security

// Package tenancy is the E13 Task 1 cross-tenant negative corpus (TEN-001/TEN-002). It proves the
// isolation the application's WHERE clauses claim is ALSO enforced one layer down, by Postgres row
// level security: a deliberately WHERE-less query issued on the runtime role returns only the
// tenant named by the transaction's palai.org_id, a connection that never set the GUC sees nothing,
// and a write that names a foreign organization is refused by the policy's WITH CHECK.
//
// The corpus drives raw pgx pools on purpose — it tests the DATABASE, not the Go pool wrapper, so it
// stays honest if the pool's context plumbing ever regresses.
//
// Honest ceiling: one database, one runtime role reached by SET ROLE from the owner connection. This
// stops a missing WHERE clause in application SQL; it does NOT stop a compromised control-plane
// process (which can RESET ROLE) or a hostile DBA, and nothing here is encrypted at rest. Those are
// E13-H/E15 (see storage/migrations/000029_row_level_security.up.sql).
package tenancy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// runtimeRole is the non-owner role migration 000029 creates and the app connection runs as.
const runtimeRole = "palai_app"

// nonTenantTables are the tables migration 000029 deliberately leaves outside RLS because they hold
// no tenant data: the migration ledger, the coordinator's host quarantine registry, and the
// per-session monotonic counter (a session id and an integer). Listing them here is the point: a NEW
// tenant-scoped table that forgets its policy fails TestEveryTenantTableIsRowLevelSecured rather than
// silently leaking.
var nonTenantTables = map[string]bool{
	"schema_migrations": true,
	"host_quarantine":   true,
	"session_sequences": true,
}

// suite holds the two connections the corpus contrasts: the migration owner (which seeds fixtures
// and is deliberately NOT subject to its own policies except under FORCE) and the runtime role the
// control plane actually serves on.
type suite struct {
	owner *pgxpool.Pool
	app   *pgxpool.Pool
	orgA  string
	orgB  string
}

func newSuite(t *testing.T) *suite {
	t.Helper()
	url := os.Getenv("PALAI_SECURITY_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_SECURITY_POSTGRES_URL is not set; run via `make test-security TEST=tenancy`")
	}
	ctx := context.Background()

	owner, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("open owner pool: %v", err)
	}
	t.Cleanup(owner.Close)
	if _, err := owner.Exec(ctx, storage.MigrationUp()); err != nil {
		t.Fatalf("apply migration chain: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse app URL: %v", err)
	}
	// Every app connection runs as the non-owner runtime role — the same switch storage.OpenPool
	// performs in production.
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+runtimeRole)
		return err
	}
	app, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	t.Cleanup(app.Close)

	s := &suite{owner: owner, app: app, orgA: newID("org"), orgB: newID("org")}
	s.seedTenant(t, s.orgA)
	s.seedTenant(t, s.orgB)
	return s
}

// seedTenant writes one organization -> project -> session -> run, as the owner. The run row is the
// corpus's canary: it is the row a WHERE-less SELECT must not return across the tenant boundary.
func (s *suite) seedTenant(t *testing.T, org string) {
	t.Helper()
	ctx := context.Background()
	project, session, response, run := newID("prj"), newID("ses"), newID("resp"), newID("run")
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id) VALUES ($1)`, []any{org}},
		{`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, []any{project, org}},
		{`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, []any{session, org, project}},
		{`INSERT INTO responses (id, organization_id, project_id, session_id, input) VALUES ($1, $2, $3, $4, '{}'::jsonb)`,
			[]any{response, org, project, session}},
		{`INSERT INTO runs (id, organization_id, project_id, session_id, response_id) VALUES ($1, $2, $3, $4, $5)`,
			[]any{run, org, project, session, response}},
	}
	for _, stmt := range stmts {
		if _, err := s.owner.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed %s: %v", org, err)
		}
	}
}

// asOrg runs fn inside one transaction whose palai.org_id GUC names org. It mirrors the per-request scope
// the auth middleware resolves, though the mechanism differs on purpose: this test sets the GUC
// transaction-locally (set_config is_local=true) for isolation between subtests, whereas production sets it
// session-level once per pool acquisition (storage.OpenPool, is_local=false). Both reach the same policy.
// An empty org leaves the GUC unset — the "connection that never declared a tenant" case.
func (s *suite) asOrg(t *testing.T, org string, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.app.Begin(ctx)
	if err != nil {
		t.Fatalf("begin app tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if org != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('palai.org_id', $1, true)", org); err != nil {
			t.Fatalf("set tenant GUC: %v", err)
		}
	}
	fn(tx)
}

// TestWhereLessQueryIsRejectedByTheDatabase is the essence of TEN-002: the fixture query is
// deliberately unscoped — no organization_id predicate at all, the shape an application bug would
// produce — and the database still returns only the caller's tenant.
func TestWhereLessQueryIsRejectedByTheDatabase(t *testing.T) {
	s := newSuite(t)
	for _, table := range []string{"runs", "responses", "sessions", "projects"} {
		s.asOrg(t, s.orgA, func(tx pgx.Tx) {
			var foreign int
			// The only predicate names the OTHER tenant: the query asks for exactly the rows the
			// caller must never see, so a non-zero count is a leak and nothing else.
			query := fmt.Sprintf(`SELECT count(*) FROM %s WHERE organization_id = $1`, table)
			if err := tx.QueryRow(context.Background(), query, s.orgB).Scan(&foreign); err != nil {
				t.Fatalf("%s: count foreign rows: %v", table, err)
			}
			if foreign != 0 {
				t.Fatalf("%s: WHERE-less query saw %d row(s) of the foreign tenant; RLS did not deny", table, foreign)
			}
			var visible int
			if err := tx.QueryRow(context.Background(), fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&visible); err != nil {
				t.Fatalf("%s: count visible rows: %v", table, err)
			}
			if visible != 1 {
				t.Fatalf("%s: own tenant sees %d row(s), want exactly the 1 seeded", table, visible)
			}
		})
	}
}

// TestConnectionWithoutTenantContextSeesNoTenantRows proves the deny-by-default half: a runtime
// connection that never set palai.org_id — a forgotten scope, a background path that skipped the
// context — reads zero rows from every tenant-scoped table, rather than everything.
func TestConnectionWithoutTenantContextSeesNoTenantRows(t *testing.T) {
	s := newSuite(t)
	for _, table := range tenantTables(t, s.owner) {
		s.asOrg(t, "", func(tx pgx.Tx) {
			var visible int
			if err := tx.QueryRow(context.Background(), fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&visible); err != nil {
				t.Fatalf("%s: count: %v", table, err)
			}
			if visible != 0 {
				t.Fatalf("%s: connection with no tenant GUC saw %d row(s), want 0", table, visible)
			}
		})
	}
}

// TestForeignWriteIsRejected proves the WITH CHECK half: a scoped connection cannot plant a row into
// another organization, so a compromised or buggy handler cannot write across the boundary either.
func TestForeignWriteIsRejected(t *testing.T) {
	s := newSuite(t)
	s.asOrg(t, s.orgA, func(tx pgx.Tx) {
		_, err := tx.Exec(context.Background(),
			`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`,
			newID("ses"), s.orgB, newID("prj"))
		if err == nil {
			t.Fatal("insert into the foreign organization succeeded; the WITH CHECK policy did not deny it")
		}
		if code := sqlState(err); code != "42501" {
			t.Fatalf("insert failed with SQLSTATE %q, want 42501 (insufficient_privilege from the RLS policy): %v", code, err)
		}
	})
}

// TestRuntimeRoleIsNotTheTableOwner keeps the corpus honest: RLS is silently inert for a superuser or
// a table owner, so the guarantee above only means something while the runtime role is neither.
func TestRuntimeRoleIsNotTheTableOwner(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()
	var superuser, bypassRLS bool
	if err := s.owner.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1`, runtimeRole).Scan(&superuser, &bypassRLS); err != nil {
		t.Fatalf("read runtime role attributes: %v", err)
	}
	if superuser || bypassRLS {
		t.Fatalf("runtime role %s is superuser=%v bypassrls=%v; RLS would be inert", runtimeRole, superuser, bypassRLS)
	}
	var owned int
	if err := s.owner.QueryRow(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = $1`, runtimeRole).Scan(&owned); err != nil {
		t.Fatalf("count owned tables: %v", err)
	}
	if owned != 0 {
		t.Fatalf("runtime role %s owns %d table(s); FORCE would be the only thing left holding the boundary", runtimeRole, owned)
	}
}

// TestEveryTenantTableIsRowLevelSecured is the regression gate for every LATER migration: a new table
// carrying organization_id must arrive with RLS enabled AND forced, or this fails. A table that is
// genuinely not tenant-scoped goes on the nonTenantTables allowlist above, deliberately and visibly.
func TestEveryTenantTableIsRowLevelSecured(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()
	rows, err := s.owner.Query(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
		       EXISTS (SELECT 1 FROM information_schema.columns col
		               WHERE col.table_schema = 'public' AND col.table_name = c.relname
		                 AND col.column_name = 'organization_id')
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'r'
		 ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var enabled, forced, tenantScoped bool
		if err := rows.Scan(&name, &enabled, &forced, &tenantScoped); err != nil {
			t.Fatalf("scan table row: %v", err)
		}
		if nonTenantTables[name] {
			continue
		}
		if !enabled || !forced {
			t.Errorf("table %s: row security enabled=%v forced=%v, want both true (tenant column present=%v)",
				name, enabled, forced, tenantScoped)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
}

// tenantTables lists the public tables under RLS, so the deny-by-default test covers whatever the
// migration chain currently defines rather than a hand-copied list that rots.
func tenantTables(t *testing.T, owner *pgxpool.Pool) []string {
	t.Helper()
	rows, err := owner.Query(context.Background(), `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relrowsecurity
		 ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("list tenant tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan tenant table: %v", err)
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		t.Fatal("no table has row level security enabled; migration 000029 did not take effect")
	}
	return out
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// newID mints a collision-free fixture identifier, matching the prefixed-opaque id shape the
// control plane uses.
func newID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(raw[:])
}
