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

// 000015 adds the durable recovery objects (E10 Task 1): the engine checkpoint metadata + the
// shared transcript boundary as SEPARATE immutable rows (spec §26.1-26.2), plus a boundary_id rider
// on 000008's workspace_snapshots. It depends only on runs/attempts/projects (000001) and
// workspace_snapshots (000008), so it merges cleanly onto the E09 tip.
//
//go:embed migrations/000015_recovery_objects.up.sql
var migrationUp15 string

//go:embed migrations/000015_recovery_objects.down.sql
var migrationDown15 string

// 000016 adds durable delivered-message rows (E10 Task 2): a send_message delivered at a run's safe
// boundary is journaled so a reclaimed/resumed attempt redelivers it at its original boundary
// (spec §26.9). It only references commands/runs (000004/000001), so it merges cleanly onto every
// E10 sibling branch.
//
//go:embed migrations/000016_delivered_messages.up.sql
var migrationUp16 string

//go:embed migrations/000016_delivered_messages.down.sql
var migrationDown16 string

// 000017 adds host_quarantine (E10 Task 6, SAN-008), the workspace_snapshots byte-archive rider
// (object_key/archive_checksum/size_bytes for the SAN-005 restore), and the merge_records
// parent_run_id index (the E09 Task 6 M3 deferral). It only references merge_records (000011) and
// ALTERs workspace_snapshots (000008), so it merges cleanly onto the E10 tip.
//
//go:embed migrations/000017_host_quarantine.up.sql
var migrationUp17 string

//go:embed migrations/000017_host_quarantine.down.sql
var migrationDown17 string

// 000018 adds the tool-call replay ledger rider (E10 Task 7): replay_class + reconciliation columns on
// 000001's tool_calls, so a kill-after-execute row is classified and an uncertain row is reconciled
// before its result re-enters reasoning (spec §26.6-26.7). It only ALTERs tool_calls, so it merges
// cleanly onto every E10 sibling branch.
//
//go:embed migrations/000018_tool_call_ledger.up.sql
var migrationUp18 string

//go:embed migrations/000018_tool_call_ledger.down.sql
var migrationDown18 string

// 000019 adds the automation-agent tables (E11 Task 1): agent_profiles + immutable publishable
// agent_revisions + profile-free run_template_revisions, plus the runs.agent_revision_id /
// run_template_revision_id pin riders (spec §10, §32.2). It references projects/runs (000001) and its
// own new tables, so it merges cleanly onto the E10 tip; the E11 opening wave assigns it 000019 and T4
// takes 000020 (merge order, no gap — E11 §1).
//
//go:embed migrations/000019_agents.up.sql
var migrationUp19 string

//go:embed migrations/000019_agents.down.sql
var migrationDown19 string

//go:embed queries/agents.sql
var agentsSQL string

// 000020 adds the outbound-webhook tables (webhook_endpoints, webhook_deliveries, delivery_attempts,
// E11 Task 4) plus the events journal_id IDENTITY rider — the global monotonic cursor the delivery
// pump fans out on, which 000001's per-session seq did not provide (spec §21.4-21.6). 000019 is E11
// Task 1's parallel migration; the two land independently and interleave here at merge.
//
//go:embed migrations/000020_webhooks.up.sql
var migrationUp20 string

//go:embed migrations/000020_webhooks.down.sql
var migrationDown20 string

//go:embed queries/webhooks.sql
var webhooksSQL string

// 000021 adds the trigger tables (triggers, immutable trigger_revisions, trigger_deliveries, E11 Task 2):
// a versioned source-event → canonical-action binding and the TriggerDelivery record its ingestion
// advances through the §20.2.2 state machine, born via the SAME §20.9 admission path as /v1/responses.
// It references projects/agent_revisions/run_template_revisions (000001/000019) and webhook_endpoints
// (000020) for its T6-ready callback column, so it opens from the tip of the E11 T1+T4 merges.
//
//go:embed migrations/000021_triggers.up.sql
var migrationUp21 string

//go:embed migrations/000021_triggers.down.sql
var migrationDown21 string

//go:embed queries/triggers.sql
var triggersSQL string

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

//go:embed queries/recovery.sql
var recoverySQL string

// MigrationUp is the forward migration chain, applied in version order (000001..000021). Each file is
// individually idempotent, so the whole chain is safe to re-run. 000019 (E11 Task 1 agents) and 000020
// (E11 Task 4 webhooks + events cursor rider) land in parallel and interleave here at merge; 000021 (E11
// Task 2 triggers) opens from the tip of both.
func MigrationUp() string {
	return migrationUp + "\n" + migrationUp2 + "\n" + migrationUp3 + "\n" + migrationUp4 + "\n" + migrationUp5 + "\n" + migrationUp6 + "\n" + migrationUp7 + "\n" + migrationUp8 + "\n" + migrationUp9 + "\n" + migrationUp10 + "\n" + migrationUp11 + "\n" + migrationUp12 + "\n" + migrationUp13 + "\n" + migrationUp14 + "\n" + migrationUp15 + "\n" + migrationUp16 + "\n" + migrationUp17 + "\n" + migrationUp18 + "\n" + migrationUp19 + "\n" + migrationUp20 + "\n" + migrationUp21
}

// MigrationDown reverses MigrationUp in the opposite order: each migration drops its added
// objects before the earlier one drops the tables that carried them.
func MigrationDown() string {
	return migrationDown21 + "\n" + migrationDown20 + "\n" + migrationDown19 + "\n" + migrationDown18 + "\n" + migrationDown17 + "\n" + migrationDown16 + "\n" + migrationDown15 + "\n" + migrationDown14 + "\n" + migrationDown13 + "\n" + migrationDown12 + "\n" + migrationDown11 + "\n" + migrationDown10 + "\n" + migrationDown9 + "\n" + migrationDown8 + "\n" + migrationDown7 + "\n" + migrationDown6 + "\n" + migrationDown5 + "\n" + migrationDown4 + "\n" + migrationDown3 + "\n" + migrationDown2 + "\n" + migrationDown
}

var namedQueries = parseNamedQueries(agentsSQL, jobsSQL, eventsSQL, responsesSQL, identitySQL, sessionsSQL, commandsSQL, configSQL, auditSQL, workspacesSQL, artifactsSQL, repositoryBindingsSQL, mergeRecordsSQL, changesetsSQL, tasksSQL, publicationsSQL, recoverySQL, webhooksSQL, triggersSQL)

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
