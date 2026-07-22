//go:build component

package postgres

import (
	"context"
	"testing"
)

// TestToolsMigration proves 000024 adds the extensibility registry — the tools lineage table, the
// immutable tool_revisions + tool_set_revisions tables, and the four E12 rider columns on both
// agent_revisions and run_template_revisions (tool_sets/mcp_connections/skills/hooks) — idempotently
// and reversibly (spec §28.2-28.4, E12 Task 2; the 000019/000023 re-run-safety pattern). A usable-row
// assert proves the shape: a tool + a draft revision insert, the conditional publish flips published_at
// exactly once (immutable publish), a duplicate canonical name is rejected per project, and a duplicate
// model_visible_name is rejected per project (the deterministic-short-name collision guard).
func TestToolsMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op (CREATE TABLE / ADD COLUMN IF NOT EXISTS).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"tools", "tool_revisions", "tool_set_revisions"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after apply, %s is missing", name)
		}
	}
	for _, tbl := range []string{"agent_revisions", "run_template_revisions"} {
		for _, col := range []string{"tool_sets", "mcp_connections", "skills", "hooks"} {
			if !columnExists(t, pool, tbl, col) {
				t.Fatalf("after apply, %s.%s rider is missing", tbl, col)
			}
		}
	}

	// Usable shape: a tool lineage row + a draft revision; publish flips once, a second publish is a no-op.
	tenant, _, _ := seedRun(t, pool)
	toolID, revID := newID("tool"), newID("trev")
	exec(t, pool, `INSERT INTO tools (id, organization_id, project_id, canonical_name, model_visible_name) VALUES ($1,$2,$3,'acme.search.fetch','fetch')`,
		toolID, tenant.Organization, tenant.Project)
	exec(t, pool, `INSERT INTO tool_revisions (id, organization_id, project_id, tool_id, revision_number, executor, input_schema, digest)
	               VALUES ($1,$2,$3,$4,1,'control_plane','{"type":"object"}','sha256:abc')`,
		revID, tenant.Organization, tenant.Project, toolID)

	tag, err := pool.Exec(ctx, `UPDATE tool_revisions SET published_at = clock_timestamp() WHERE id=$1 AND published_at IS NULL`, revID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("first publish rows = %d err = %v, want exactly 1", tag.RowsAffected(), err)
	}
	tag2, err := pool.Exec(ctx, `UPDATE tool_revisions SET published_at = clock_timestamp() WHERE id=$1 AND published_at IS NULL`, revID)
	if err != nil || tag2.RowsAffected() != 0 {
		t.Fatalf("second publish rows = %d err = %v, want 0 (already published)", tag2.RowsAffected(), err)
	}

	// A duplicate canonical name in the same project is rejected (the UNIQUE lineage key).
	if got := pgCode(mustFail(pool.Exec(ctx,
		`INSERT INTO tools (id, organization_id, project_id, canonical_name, model_visible_name) VALUES ($1,$2,$3,'acme.search.fetch','fetch2')`,
		newID("tool"), tenant.Organization, tenant.Project))); got != "23505" {
		t.Fatalf("duplicate canonical_name code = %q, want 23505 unique_violation", got)
	}
	// A duplicate model_visible_name in the same project is rejected (the deterministic short-name guard).
	if got := pgCode(mustFail(pool.Exec(ctx,
		`INSERT INTO tools (id, organization_id, project_id, canonical_name, model_visible_name) VALUES ($1,$2,$3,'acme.other.fetch','fetch')`,
		newID("tool"), tenant.Organization, tenant.Project))); got != "23505" {
		t.Fatalf("duplicate model_visible_name code = %q, want 23505 unique_violation", got)
	}

	// A tool_set_revision pins revisions by JSON id list; a second row for the same (set, number) is rejected.
	setRevID := newID("tsrev")
	exec(t, pool, `INSERT INTO tool_set_revisions (id, organization_id, project_id, set_name, revision_number, tool_pins, digest)
	               VALUES ($1,$2,$3,'reviewers',1,'[{"tool_revision_id":"trev_x"}]','sha256:def')`,
		setRevID, tenant.Organization, tenant.Project)
	if got := pgCode(mustFail(pool.Exec(ctx,
		`INSERT INTO tool_set_revisions (id, organization_id, project_id, set_name, revision_number, tool_pins, digest)
		 VALUES ($1,$2,$3,'reviewers',1,'[]','sha256:ghi')`,
		newID("tsrev"), tenant.Organization, tenant.Project))); got != "23505" {
		t.Fatalf("duplicate (set_name, revision_number) code = %q, want 23505 unique_violation", got)
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, name := range []string{"tools", "tool_revisions", "tool_set_revisions"} {
		if tableExists(t, pool, name) {
			t.Fatalf("after rollback, %s still exists", name)
		}
	}
	if columnExists(t, pool, "agent_revisions", "tool_sets") {
		t.Fatal("after rollback, agent_revisions.tool_sets rider still exists")
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"tools", "tool_revisions", "tool_set_revisions"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after reapply, %s is missing", name)
		}
	}
	if !columnExists(t, pool, "run_template_revisions", "hooks") {
		t.Fatal("after reapply, run_template_revisions.hooks rider is missing")
	}
}
