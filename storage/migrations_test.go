package storage

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// TestOrderedMigrationsIsContiguousVersionOrder proves OrderedMigrations parses every embedded
// forward migration into a gap-free, version-sorted list carrying the SQL and a non-empty checksum —
// the per-migration source the boot runner iterates (E15 T1). It also pins the chain head so the
// preflight/journal invariant is anchored: the head is 000043_slack_requester (E21 T3 — the durable Slack
// requester the agent's own question is addressed to), with 000042_slack_message_turns (E20) the penultimate
// link — strict, no gaps.
//
// THIS PIN WAS STALE FOR TWO EPICS and the staleness is worth naming: it still read 000040 while 000041 and
// 000042 had landed, so the one test that anchors the chain head was RED on main rather than guarding it. A
// head pin nobody updates is a head pin nobody reads.
func TestOrderedMigrationsIsContiguousVersionOrder(t *testing.T) {
	migrations := OrderedMigrations()
	if len(migrations) == 0 {
		t.Fatal("OrderedMigrations() is empty")
	}
	for i, m := range migrations {
		wantVersion := i + 1
		if m.Version != wantVersion {
			t.Fatalf("migration[%d].Version = %d, want %d (contiguous, no gaps)", i, m.Version, wantVersion)
		}
		if m.Name == "" {
			t.Fatalf("migration %d has an empty name", m.Version)
		}
		if m.Up == "" {
			t.Fatalf("migration %d has empty SQL", m.Version)
		}
		if len(m.Checksum) != 64 {
			t.Fatalf("migration %d checksum = %q, want a 64-char sha256 hex", m.Version, m.Checksum)
		}
		// Every migration must stamp ITS OWN version into schema_migrations. This is the invariant the
		// runner's "journal head can never exceed the schema head" rests on — gate it here instead of
		// trusting each author to include the marker.
		marker := fmt.Sprintf("INSERT INTO schema_migrations (version) VALUES (%d)", m.Version)
		if !strings.Contains(m.Up, marker) {
			t.Fatalf("migration %06d_%s does not stamp its version: missing %q", m.Version, m.Name, marker)
		}
	}

	// E29's model_connection_endpoint is the current chain head; usage_step_attribution is the link before
	// it. THE NAME IS PINNED AS WELL AS THE NUMBER, and that is what makes this a rename guard too:
	// `git mv` stages the OLD content, so a renumbering whose header edit is never re-added would leave a
	// file whose name says 51 and whose marker says 49, and only the pair below catches it.
	//
	// THIS PIN HAS NOW DONE ITS JOB TWICE ON ONE NUMBER. 000049 was taken by three branches in flight at
	// once — usage_series, usage_step_attribution and this one — and the model_connection_endpoint branch
	// was parked long enough that the other two landed first. It renumbered 49 -> 51 at merge, and the
	// assertion below is what forces that rename to be FINISHED rather than half-done: the file name, the
	// `VALUES (51)` marker inside it and the embed var all have to agree before this test goes green.
	//
	// It is a pin on the CHAIN, not a claim that one epic owns one migration — E29 holds both 48 and 51,
	// and the "one migration per epic" reasoning an earlier revision of this comment carried was never
	// structural, only a property of the four epics that happened to need one each.
	// AND IT IS ABOUT TO DO IT A THIRD TIME, ON 000052. The structured-output branch below and the
	// `desired-config` branch both took 52 as the next contiguous number in their own trees, because
	// each was cut when 51 was the tip. Whichever lands second renames — filename pair, `VALUES (52)`
	// marker, embed var, and this assertion — and this test is what makes the rename finish.
	head := migrations[len(migrations)-1]
	if head.Version != 52 || head.Name != "run_output_contract" {
		t.Fatalf("chain head = %06d_%s, want 000052_run_output_contract", head.Version, head.Name)
	}
	penultimate := migrations[len(migrations)-2]
	if penultimate.Version != 51 || penultimate.Name != "model_connection_endpoint" {
		t.Fatalf("penultimate migration = %06d_%s, want 000051_model_connection_endpoint", penultimate.Version, penultimate.Name)
	}

	// The concatenated MigrationUp() must carry exactly the same forward SQL the per-migration path
	// applies — the two forms cannot drift, or Rollback's down mirror would reverse a chain the boot
	// runner never applied.
	full := MigrationUp()
	for _, m := range migrations {
		if !strings.Contains(full, m.Up) {
			t.Fatalf("MigrationUp() is missing the body of migration %06d_%s", m.Version, m.Name)
		}
	}
}

// TestEveryLateTenantTableCarriesItsOwnPolicyAndGrant is a STATIC guard, and it exists because the
// runtime one cannot work. Measured on 2026-07-29 while landing 000045:
//
// Deleting `CALL palai_apply_tenant_policy('runner_pool_keys', ...)` from 000045 left
// tests/security/tenancy GREEN. The reason is structural, not a bug in that corpus: 000029's sweep is
// catalogue-driven and the whole chain re-runs on every boot, so a table 45 creates without a policy
// IS swept by 29 on the NEXT boot — and every harness in this repository migrates more than once
// (tenancy's newSuite applies the chain per TEST, and the component package shares one database across
// dozens of openHarness calls). So no runtime tier can ever observe the state the omission produces.
//
// That state is real and it is not harmless: on the FIRST boot of a fresh install, between 45 creating
// the table and the next restart, the table exists with row-level security not enabled at all. Every
// row in it is visible to every tenant for that window.
//
// So the rule is checked where it is checkable: in the SQL. A migration after 000029 that creates a
// table carrying organization_id must carry its own policy CALL and its own GRANT — 29's sweep and 29's
// blanket grant both run BEFORE it and the table does not exist yet.
func TestEveryLateTenantTableCarriesItsOwnPolicyAndGrant(t *testing.T) {
	createTable := regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS (\w+) \((.*?)\n\);`)
	grantOn := regexp.MustCompile(`GRANT [A-Z, ]+ ON ([\w, ]+?) TO palai_app`)

	checked := 0
	for _, m := range OrderedMigrations() {
		// 000029 is where the procedure and the blanket grant are defined; everything before it is
		// covered by its sweep, which runs after those tables exist.
		if m.Version <= 29 {
			continue
		}
		granted := map[string]bool{}
		for _, g := range grantOn.FindAllStringSubmatch(m.Up, -1) {
			for _, table := range strings.Split(g[1], ",") {
				granted[strings.TrimSpace(table)] = true
			}
		}
		for _, c := range createTable.FindAllStringSubmatch(m.Up, -1) {
			name, body := c[1], c[2]
			if !strings.Contains(body, "organization_id") {
				continue
			}
			checked++
			if !strings.Contains(m.Up, "palai_apply_tenant_policy('"+name+"'") {
				t.Errorf("migration %06d_%s creates tenant table %s without its own CALL palai_apply_tenant_policy('%s', ...): "+
					"000029's sweep ran before this file and the table did not exist yet, so the table is unsecured "+
					"until the next boot", m.Version, m.Name, name, name)
			}
			if !granted[name] {
				t.Errorf("migration %06d_%s creates tenant table %s without its own GRANT ... ON %s TO palai_app: "+
					"000029's blanket grant ran before this file, so the runtime role fails closed with "+
					"\"permission denied\" rather than with the row-scoped policy", m.Version, m.Name, name, name)
			}
		}
	}
	// NON-VACUITY: this guard is a regexp over SQL, so a pattern that silently stops matching would make
	// it pass for every future migration. 000045 alone contributes two tables; the chain contributed 26
	// when this was written, and a floor well under that catches a broken pattern without turning every
	// new migration into an edit here.
	if checked < 20 {
		t.Fatalf("the CREATE TABLE pattern matched only %d late tenant tables; it has stopped parsing the chain "+
			"and this guard is now vacuous", checked)
	}
}
