package storage

import (
	"fmt"
	"strings"
	"testing"
)

// TestOrderedMigrationsIsContiguousVersionOrder proves OrderedMigrations parses every embedded
// forward migration into a gap-free, version-sorted list carrying the SQL and a non-empty checksum —
// the per-migration source the boot runner iterates (E15 T1). It also pins the chain head so the
// preflight/journal invariant is anchored: the head advances to 000035_slack with E17 T1's Slack store
// (000034_contract_usage_events becomes the penultimate link).
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

	// E17 T4 knowledge spine is the current chain head (built as 000035 for contiguity; renumbered to
	// 000036 at merge per the fixed order — see the migration's MERGE NOTE).
	head := migrations[len(migrations)-1]
	if head.Version != 36 || head.Name != "knowledge" {
		t.Fatalf("chain head = %06d_%s, want 000036_knowledge", head.Version, head.Name)
	}
	penultimate := migrations[len(migrations)-2]
	if penultimate.Version != 35 || penultimate.Name != "slack" {
		t.Fatalf("penultimate migration = %06d_%s, want 000035_slack", penultimate.Version, penultimate.Name)
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
