package storage

import (
	"fmt"
	"strings"
	"testing"
)

// TestOrderedMigrationsIsContiguousVersionOrder proves OrderedMigrations parses every embedded
// forward migration into a gap-free, version-sorted list carrying the SQL and a non-empty checksum —
// the per-migration source the boot runner iterates (E15 T1). It also pins the chain head so the
// preflight/journal invariant is anchored: in the E17 T9 worktree the head is 000039_capability_workers
// (built at 000039 in ISOLATION for strict, no-gap contiguity off the 000038 head), with 000038_a2a (E17 T2)
// the penultimate link. The integrator RENUMBERS this to 000040 at merge (§1 assigns T9 000040; T3
// a2a-client=039 merges first) — bump this head-pin to 000040_capability_workers / penult 000039_a2a-client
// then.
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

	// E17 T9 CapabilityWorker contract is the current chain head in this worktree (built as 000039 in
	// ISOLATION for strict contiguity; the integrator renumbers it to 000040 at merge — §1 assigns T9
	// 000040, with T3 a2a-client=039 merging first). Penult is 000038_a2a (E17 T2).
	head := migrations[len(migrations)-1]
	if head.Version != 39 || head.Name != "capability_workers" {
		t.Fatalf("chain head = %06d_%s, want 000039_capability_workers", head.Version, head.Name)
	}
	penultimate := migrations[len(migrations)-2]
	if penultimate.Version != 38 || penultimate.Name != "a2a" {
		t.Fatalf("penultimate migration = %06d_%s, want 000038_a2a", penultimate.Version, penultimate.Name)
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
