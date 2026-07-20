// Package storage embeds the canonical SQL migrations and queries for the durable
// execution spine and opens tenant-scoped connection pools against them. The SQL
// files are the single source of truth; Go code loads statements by name rather
// than re-declaring them (spec §24.3 system of record).
package storage

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/000001_core.up.sql
var migrationUp string

//go:embed migrations/000001_core.down.sql
var migrationDown string

//go:embed migrations/000002_retention.up.sql
var migrationUp2 string

//go:embed migrations/000002_retention.down.sql
var migrationDown2 string

//go:embed migrations/000003_session_chaining.up.sql
var migrationUp3 string

//go:embed migrations/000003_session_chaining.down.sql
var migrationDown3 string

//go:embed migrations/000004_commands.up.sql
var migrationUp4 string

//go:embed migrations/000004_commands.down.sql
var migrationDown4 string

//go:embed migrations/000005_config_revisions.up.sql
var migrationUp5 string

//go:embed migrations/000005_config_revisions.down.sql
var migrationDown5 string

//go:embed migrations/000006_one_active_root.up.sql
var migrationUp6 string

//go:embed migrations/000006_one_active_root.down.sql
var migrationDown6 string

//go:embed migrations/000007_child_runs.up.sql
var migrationUp7 string

//go:embed migrations/000007_child_runs.down.sql
var migrationDown7 string

//go:embed migrations/000008_workspaces.up.sql
var migrationUp8 string

//go:embed migrations/000008_workspaces.down.sql
var migrationDown8 string

//go:embed queries/jobs.sql
var jobsSQL string

//go:embed queries/events.sql
var eventsSQL string

//go:embed queries/responses.sql
var responsesSQL string

//go:embed queries/identity.sql
var identitySQL string

//go:embed queries/sessions.sql
var sessionsSQL string

//go:embed queries/commands.sql
var commandsSQL string

//go:embed queries/config.sql
var configSQL string

//go:embed queries/audit.sql
var auditSQL string

//go:embed queries/workspaces.sql
var workspacesSQL string

//go:embed queries/artifacts.sql
var artifactsSQL string

// MigrationUp is the forward migration chain, applied in version order (000001..000008).
// Each file is individually idempotent, so the whole chain is safe to re-run.
func MigrationUp() string {
	return migrationUp + "\n" + migrationUp2 + "\n" + migrationUp3 + "\n" + migrationUp4 + "\n" + migrationUp5 + "\n" + migrationUp6 + "\n" + migrationUp7 + "\n" + migrationUp8
}

// MigrationDown reverses MigrationUp in the opposite order: each migration drops its added
// objects before the earlier one drops the tables that carried them.
func MigrationDown() string {
	return migrationDown8 + "\n" + migrationDown7 + "\n" + migrationDown6 + "\n" + migrationDown5 + "\n" + migrationDown4 + "\n" + migrationDown3 + "\n" + migrationDown2 + "\n" + migrationDown
}

var namedQueries = parseNamedQueries(jobsSQL, eventsSQL, responsesSQL, identitySQL, sessionsSQL, commandsSQL, configSQL, auditSQL, workspacesSQL, artifactsSQL)

// Query returns the SQL statement labelled "-- name: <name>" in storage/queries.
// It panics on an unknown name because query names are compile-time constants.
func Query(name string) string {
	statement, ok := namedQueries[name]
	if !ok {
		panic(fmt.Sprintf("storage: unknown query %q", name))
	}
	return statement
}

// parseNamedQueries splits yesql-style "-- name: X" blocks into a name->SQL map.
func parseNamedQueries(files ...string) map[string]string {
	out := map[string]string{}
	for _, file := range files {
		name := ""
		var body strings.Builder
		flush := func() {
			if name != "" {
				out[name] = strings.TrimSpace(body.String())
			}
			body.Reset()
		}
		for _, line := range strings.Split(file, "\n") {
			if marker, ok := strings.CutPrefix(line, "-- name:"); ok {
				flush()
				name = strings.TrimSpace(marker)
				continue
			}
			if name != "" {
				body.WriteString(line)
				body.WriteByte('\n')
			}
		}
		flush()
	}
	return out
}

// OpenPool opens a verified connection pool. The URL carries a local throwaway
// credential supplied by the caller; it is never embedded here.
func OpenPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("storage: database URL is required")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	config.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
