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

//go:embed migrations/000009_repository_bindings.up.sql
var migrationUp9 string

//go:embed migrations/000009_repository_bindings.down.sql
var migrationDown9 string

//go:embed migrations/000010_changesets.up.sql
var migrationUp10 string

//go:embed migrations/000010_changesets.down.sql
var migrationDown10 string

//go:embed migrations/000011_merge_records.up.sql
var migrationUp11 string

//go:embed migrations/000011_merge_records.down.sql
var migrationDown11 string

// 000012 adds the durable session-scoped task/todo registry (E09 Task 7); it depends on nothing
// 000010 (sibling-branch changesets) introduces, so the two merge cleanly.
//
//go:embed migrations/000012_tasks.up.sql
var migrationUp12 string

//go:embed migrations/000012_tasks.down.sql
var migrationDown12 string

// 000013 adds publications + approvals (E09 Task 8): the durable push/PR operations and their one-shot
// approval gate. It depends only on runs/projects (000001), so it merges cleanly onto the T7 base.
//
//go:embed migrations/000013_approvals_publications.up.sql
var migrationUp13 string

//go:embed migrations/000013_approvals_publications.down.sql
var migrationDown13 string

// 000014 adds the session→binding link to the workspaces table (E09 Task 10): the repository binding
// + requested ref the root run's auto-provisioning resolves. It only ALTERs 000008's workspaces table,
// so it merges cleanly onto every E09 sibling branch.
//
//go:embed migrations/000014_workspace_repository_link.up.sql
var migrationUp14 string

//go:embed migrations/000014_workspace_repository_link.down.sql
var migrationDown14 string

// 000016 adds durable delivered-message rows (E10 Task 2): a send_message delivered at a run's safe
// boundary is journaled so a reclaimed/resumed attempt redelivers it at its original boundary
// (spec §26.9). It only references commands/runs (000004/000001), so it merges cleanly onto every
// E10 sibling branch. (000015 is E10 Task 1's recovery objects, added on that branch.)
//
//go:embed migrations/000016_delivered_messages.up.sql
var migrationUp16 string

//go:embed migrations/000016_delivered_messages.down.sql
var migrationDown16 string

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

//go:embed queries/repository_bindings.sql
var repositoryBindingsSQL string

//go:embed queries/merge_records.sql
var mergeRecordsSQL string

//go:embed queries/changesets.sql
var changesetsSQL string

//go:embed queries/tasks.sql
var tasksSQL string

//go:embed queries/publications.sql
var publicationsSQL string

// MigrationUp is the forward migration chain, applied in version order (000001..000016). Each file is
// individually idempotent, so the whole chain is safe to re-run. (000015 is added on E10 Task 1's
// branch; this branch carries only its own 000016.)
func MigrationUp() string {
	return migrationUp + "\n" + migrationUp2 + "\n" + migrationUp3 + "\n" + migrationUp4 + "\n" + migrationUp5 + "\n" + migrationUp6 + "\n" + migrationUp7 + "\n" + migrationUp8 + "\n" + migrationUp9 + "\n" + migrationUp10 + "\n" + migrationUp11 + "\n" + migrationUp12 + "\n" + migrationUp13 + "\n" + migrationUp14 + "\n" + migrationUp16
}

// MigrationDown reverses MigrationUp in the opposite order: each migration drops its added
// objects before the earlier one drops the tables that carried them.
func MigrationDown() string {
	return migrationDown16 + "\n" + migrationDown14 + "\n" + migrationDown13 + "\n" + migrationDown12 + "\n" + migrationDown11 + "\n" + migrationDown10 + "\n" + migrationDown9 + "\n" + migrationDown8 + "\n" + migrationDown7 + "\n" + migrationDown6 + "\n" + migrationDown5 + "\n" + migrationDown4 + "\n" + migrationDown3 + "\n" + migrationDown2 + "\n" + migrationDown
}

var namedQueries = parseNamedQueries(jobsSQL, eventsSQL, responsesSQL, identitySQL, sessionsSQL, commandsSQL, configSQL, auditSQL, workspacesSQL, artifactsSQL, repositoryBindingsSQL, mergeRecordsSQL, changesetsSQL, tasksSQL, publicationsSQL)

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
