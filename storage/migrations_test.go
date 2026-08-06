package storage

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// TestOrderedMigrationsIsContiguousVersionOrder proves OrderedMigrations parses every embedded forward
// migration into a gap-free, version-sorted list carrying the SQL and a non-empty checksum — the
// per-migration source the boot runner iterates (E15 T1). It also pins the chain head, so the
// preflight/journal invariant is anchored.
//
// THE CHAIN IS EIGHT LINKS AND IT WAS SIXTY-SEVEN. It was squashed to a two-file baseline on 2026-08-04;
// 000003_lease_occupancy (Faz A.4 T1) was the first ordinary forward migration written against it, then
// 000004_response_metadata, 000005_capacity_declaration (Faz A.4 T5),
// 000006_project_scoped_secrets_and_environments (Faz A.6 T0),
// 000007_device_identity_and_pool_isolation (zero-touch device agent, T2), and
// 000008_machine_tenant_exclusivity (one customer at a time on a shared Mac) is now the head.
//
// THE PIN IS A RENAME GUARD AS MUCH AS A HEAD PIN, and that is why the NAME is pinned beside the number:
// `git mv` stages the OLD content, so a renumbering whose header edit is never re-added leaves a file
// whose name says one version and whose `VALUES (n)` marker says another, and only asserting both catches
// it. It did that job three times on the old chain — 000049 was taken by three branches at once, 000052 by
// two, and 000060 by two more.
//
// IT ALSO WENT STALE FOUR TIMES, which is the other half of the lesson: it still read 000040 while 41 and
// 42 had landed, 000062 landed without moving it at all, and on 2026-08-05 000004_response_metadata landed
// while this still pinned 000003 — so `go test ./storage/` was RED on main, in the one tier CI actually
// runs, until 000005 came to update it. A head pin nobody updates is a head pin nobody reads. The squashed
// chain is short enough that keeping up with it is cheap, and adding a link means editing exactly this
// assertion — which 000003, 000005, 000006 and 000007 did and 000004 did not. 000007 was written by an
// agent that ran `go test ./storage/` and read this exact failure, which is the pin doing its job for the
// fifth time.
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

	head := migrations[len(migrations)-1]
	if head.Version != 8 || head.Name != "machine_tenant_exclusivity" {
		t.Fatalf("chain head = %06d_%s, want 000008_machine_tenant_exclusivity", head.Version, head.Name)
	}
	penultimate := migrations[len(migrations)-2]
	if penultimate.Version != 7 || penultimate.Name != "device_identity_and_pool_isolation" {
		t.Fatalf("penultimate migration = %06d_%s, want 000007_device_identity_and_pool_isolation", penultimate.Version, penultimate.Name)
	}

	// The concatenated MigrationUp() must carry exactly the same forward SQL the per-migration path
	// applies — the two forms cannot drift, or Rollback's down mirror would reverse a chain the boot
	// runner never applied. This comparison is the reason the up files stay embedded twice; see the
	// ponytail on migrationFS.
	full := MigrationUp()
	for _, m := range migrations {
		if !strings.Contains(full, m.Up) {
			t.Fatalf("MigrationUp() is missing the body of migration %06d_%s", m.Version, m.Name)
		}
	}
}

// TestEveryLateTenantTableCarriesItsOwnPolicyAndGrant is a STATIC guard, and it exists because the
// runtime one cannot work. Measured on 2026-07-29 while landing what was then 000045:
//
// Deleting a `CALL palai_apply_tenant_policy(...)` from a late migration left tests/security/tenancy
// GREEN. The reason is structural, not a bug in that corpus: the row-level-security sweep is
// catalogue-driven and the whole chain re-runs on every boot, so a table created without a policy IS
// swept on the NEXT boot — and every harness in this repository migrates more than once. No runtime tier
// can ever observe the state the omission produces.
//
// That state is real and it is not harmless: on the FIRST boot of a fresh install, between the table being
// created and the next restart, the table exists with row-level security not enabled at all. Every row in
// it is visible to every scope for that window.
//
// SO THE RULE IS ABOUT MIGRATIONS AFTER THE SWEEP, AND AFTER THE SQUASH THERE ARE NONE. The baseline
// creates every table in 000001 and 000002 secures all of them, so today this guard has zero subjects —
// and a guard with zero subjects is the vacuity this tree keeps finding. Its floor therefore moved to
// something that is NOT zero today: the pattern must still parse the baseline's own tables. That is what
// fails if the regexes stop matching, while the rule itself still binds the first migration anyone adds
// after 000002.
func TestEveryLateTenantTableCarriesItsOwnPolicyAndGrant(t *testing.T) {
	createTable := regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS (\w+) \((.*?)\n\);`)
	grantOn := regexp.MustCompile(`GRANT [A-Z, ]+ ON TABLE ([\w, ]+?) TO palai_app`)

	// The sweep lives in 000002; everything it runs over is created before it.
	const sweepVersion = 2

	baselineTables := 0
	for _, m := range OrderedMigrations() {
		if m.Version > sweepVersion {
			granted := map[string]bool{}
			for _, g := range grantOn.FindAllStringSubmatch(m.Up, -1) {
				for _, table := range strings.Split(g[1], ",") {
					granted[strings.TrimSpace(table)] = true
				}
			}
			for _, c := range createTable.FindAllStringSubmatch(m.Up, -1) {
				name, body := c[1], c[2]
				if !strings.Contains(body, "project_id") {
					continue
				}
				if !strings.Contains(m.Up, "palai_apply_tenant_policy('"+name+"'") {
					t.Errorf("migration %06d_%s creates tenant table %s without its own CALL palai_apply_tenant_policy('%s', ...): "+
						"000002's sweep ran before this file and the table did not exist yet, so the table is unsecured "+
						"until the next boot", m.Version, m.Name, name, name)
				}
				if !granted[name] {
					t.Errorf("migration %06d_%s creates tenant table %s without its own GRANT ... ON TABLE %s TO palai_app: "+
						"000001's grants ran before this file, so the runtime role fails closed with "+
						"\"permission denied\" rather than with the row-scoped policy", m.Version, m.Name, name, name)
				}
			}
			continue
		}
		baselineTables += len(createTable.FindAllStringSubmatch(m.Up, -1))
	}

	// NON-VACUITY. This guard is a pair of regexps over SQL, so a pattern that silently stopped matching
	// would pass for every future migration. There are no late tables to count today, so the floor is on
	// the baseline the patterns are shaped against: 000001 declares ninety-five tables, and a floor well
	// under that catches a broken pattern without turning a schema change into an edit here.
	if baselineTables < 80 {
		t.Fatalf("the CREATE TABLE pattern matched only %d tables in the baseline; it has stopped parsing "+
			"the chain and this guard is now vacuous", baselineTables)
	}
}
