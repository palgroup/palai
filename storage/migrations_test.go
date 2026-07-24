package storage

import (
	"fmt"
	"strings"
	"testing"
)

// TestOrderedMigrationsIsContiguousVersionOrder proves OrderedMigrations parses every embedded forward
// migration into a strictly-increasing, version-sorted list carrying the SQL and a non-empty checksum —
// the per-migration source the boot runner iterates (E15 T1) — and that each migration stamps its own
// version marker.
//
// STRICTLY INCREASING, not adjacency: the E17 migration wave (§1) reserves FIXED numbers to sibling tasks
// that build in parallel worktrees and interleave at merge — 000035 (Slack), 000036 (knowledge), 000037
// (queues), 000038/000039 (A2A), 000040 (workers). A single task's worktree therefore has a legitimate gap
// (this T7 worktree holds 000037 but not yet 000035/000036), and the chain becomes contiguous only after
// the whole wave merges. Pinning "== i+1" or an exact head would fail every isolated E17 migration task and
// force a conflicting per-task edit; presence + strictly-increasing is the invariant that holds in both the
// isolated and merged states, and still catches a duplicate, an out-of-order file, a missing marker, or an
// empty body. The concrete head/preflight version is enforced by the migration journal (000033) at boot.
func TestOrderedMigrationsIsContiguousVersionOrder(t *testing.T) {
	migrations := OrderedMigrations()
	if len(migrations) == 0 {
		t.Fatal("OrderedMigrations() is empty")
	}
	prev := 0
	present := map[int]string{}
	for _, m := range migrations {
		if m.Version <= prev {
			t.Fatalf("migration versions must be strictly increasing and unique: %d after %d", m.Version, prev)
		}
		prev = m.Version
		present[m.Version] = m.Name
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

	// The pre-E17 anchors stay in the chain: the migration journal (000033) and the usage_events contract
	// (000034, the E15 T1 head before the wave). This T7 worktree additionally carries its own 000037.
	for version, wantName := range map[int]string{33: "migration_journal", 34: "contract_usage_events", 37: "queues"} {
		if present[version] != wantName {
			t.Fatalf("migration %06d = %q, want %q", version, present[version], wantName)
		}
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
