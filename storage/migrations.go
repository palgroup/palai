package storage

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// migrationFS embeds the forward migration files a SECOND time as a walkable tree, so OrderedMigrations
// can hand the boot runner each migration on its own (version, name, SQL, checksum). The individual
// //go:embed string vars in embed.go stay the source for the concatenated MigrationUp()/MigrationDown()
// (the down-mirror Rollback still uses); this FS is only the split view.
//
// ponytail: the up files are embedded twice (a few KB). The clean fold — deriving MigrationUp() itself
// from this FS and deleting the per-file up vars — waits until someone retires the hand-written concat;
// it is a bigger, riskier edit to a load-bearing file than this task warrants.
//
//go:embed migrations/*.up.sql
var migrationFS embed.FS

// Migration is one forward migration file: its numeric version, its short name (the part between the
// version and ".up.sql"), the SQL, and the sha256 checksum of the SQL bytes. The runner records the
// checksum in the schema_revisions journal (000033), so a re-applied file whose bytes drifted from the
// one that first applied it is detectable in an audit.
type Migration struct {
	Version  int
	Name     string
	Up       string
	Checksum string
}

// OrderedMigrations returns every forward migration in ascending version order (000001..N), parsed from
// the embedded migrations directory. It is the per-migration source the boot runner iterates so each
// migration commits in its OWN bounded transaction and lands its own journal row (E15 T1); the file
// names are the single ordering authority (NNNNNN_name.up.sql, zero-padded so lexical order == version
// order). It panics on a malformed file name because the migration set is compile-time embedded — a bad
// name is a build-time authoring error, not a runtime condition.
func OrderedMigrations() []Migration {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		panic(fmt.Sprintf("storage: read embedded migrations: %v", err))
	}
	migrations := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		stem := strings.TrimSuffix(name, ".up.sql")
		digits, label, ok := strings.Cut(stem, "_")
		if !ok {
			panic(fmt.Sprintf("storage: migration %q is not NNNNNN_name.up.sql", name))
		}
		version, err := strconv.Atoi(digits)
		if err != nil {
			panic(fmt.Sprintf("storage: migration %q has a non-numeric version: %v", name, err))
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			panic(fmt.Sprintf("storage: read migration %q: %v", name, err))
		}
		sum := sha256.Sum256(body)
		migrations = append(migrations, Migration{
			Version:  version,
			Name:     label,
			Up:       string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations
}
