package storage

import (
	"fmt"
	"strings"
	"testing"
)

// TestOrderedMigrationsIsContiguousVersionOrder proves OrderedMigrations parses every embedded
// forward migration into a gap-free, version-sorted list carrying the SQL and a non-empty checksum —
// the per-migration source the boot runner iterates (E15 T1). It also pins the chain head so the
// preflight/journal invariant is anchored: after E17 wave-3 the head is 000040_capability_workers (E17 T9),
// with 000039_a2a_remote_agents (E17 T3) the penultimate link — strict, no gaps (T1 slack=035 →
// T4 knowledge=036 → T7 queue=037 → T2 a2a=038 → T3 a2a-client=039 → T9 capability-workers=040, merged in
// the fixed order §1).
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

	// E17 T9 CapabilityWorker contract is the current chain head (built as 000039 in isolation, renumbered
	// to 000040 at merge — §1 assigns T9 000040, with T3 a2a-client=039 merging first).
	head := migrations[len(migrations)-1]
	if head.Version != 40 || head.Name != "capability_workers" {
		t.Fatalf("chain head = %06d_%s, want 000040_capability_workers", head.Version, head.Name)
	}
	penultimate := migrations[len(migrations)-2]
	if penultimate.Version != 39 || penultimate.Name != "a2a_remote_agents" {
		t.Fatalf("penultimate migration = %06d_%s, want 000039_a2a_remote_agents", penultimate.Version, penultimate.Name)
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
