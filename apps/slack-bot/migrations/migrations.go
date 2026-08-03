// Package migrations embeds the slack-bot's own forward-only schema — one small table so far
// (thread_sessions, Task 8) — kept separate from storage.OrderedMigrations (storage/migrations.go)
// because the two are unrelated schemas: this one lives in the bot's own database, has no down files
// (nothing here has ever needed a rollback), and versions from 1, not from wherever storage's chain
// happens to be. apps/slack-bot/internal/store imports this package to run what it embeds; it does not
// embed the SQL itself, because go:embed can only reach a directory at or below the embedding file's
// own directory, and this package's directory (apps/slack-bot/migrations) is a SIBLING of
// apps/slack-bot/internal/store, not a descendant of it.
package migrations

import (
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed *.sql
var migrationFS embed.FS

// Migration is one forward migration: its numeric version, its name (the part between the version and
// ".sql"), and its SQL.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Ordered returns every migration in this package in ascending version order, parsed from the
// embedded *.sql files. The file name is the sole ordering authority (NNNN_name.sql, zero-padded so
// lexical order == version order) — the same contract storage.OrderedMigrations uses for its own,
// unrelated chain. It panics on a malformed file name because the migration set is compile-time
// embedded: a bad name is a build-time authoring error, not a runtime condition.
func Ordered() []Migration {
	entries, err := migrationFS.ReadDir(".")
	if err != nil {
		panic(fmt.Sprintf("migrations: read embedded migrations: %v", err))
	}
	out := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		stem := strings.TrimSuffix(name, ".sql")
		digits, label, ok := strings.Cut(stem, "_")
		if !ok {
			panic(fmt.Sprintf("migrations: %q is not NNNN_name.sql", name))
		}
		version, err := strconv.Atoi(digits)
		if err != nil {
			panic(fmt.Sprintf("migrations: %q has a non-numeric version: %v", name, err))
		}
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			panic(fmt.Sprintf("migrations: read %q: %v", name, err))
		}
		out = append(out, Migration{Version: version, Name: label, SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}
