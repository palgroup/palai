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

	"github.com/jackc/pgx/v5"
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

// 000022 adds the schedule tables (schedules, schedule_occurrences, E11 Task 3): a cron/one-time cadence
// that fires an existing trigger on a wall-clock schedule, plus the durable occurrence rows whose
// UNIQUE(schedule_id, schedule_revision, planned_at) invariant makes each firing exactly-once (spec §33).
// It references triggers (000021) for the FK a firing admits through, so it opens from the tip of the E11
// T2 merge — the last link in the 000001..000022 chain.
//
//go:embed migrations/000022_schedules.up.sql
var migrationUp22 string

//go:embed migrations/000022_schedules.down.sql
var migrationDown22 string

//go:embed queries/schedules.sql
var schedulesSQL string

// 000023 adds the inbound-trigger auth rider (triggers.created_by + inbound_secret_ref +
// inbound_secret_ref_next, E11 Task 5): the principal a signed-inbound run admits AS and the two
// source-secret rotation handles the receiver verifies under (spec §20.2.2/§21.7). The inbound DATA path
// (source cols + source-dedupe index + principal_id) was pre-provisioned in 000021, so this is the only
// T5 schema change. It only ALTERs triggers (000021), so it opens from the tip of the E11 T2 merge.
//
//go:embed migrations/000023_inbound_trigger_auth.up.sql
var migrationUp23 string

//go:embed migrations/000023_inbound_trigger_auth.down.sql
var migrationDown23 string

// 000024 adds the extensibility registry (E12 Task 2, spec §28.2-28.4): the tools lineage table, the
// immutable tool_revisions + tool_set_revisions tables, and the four E12 rider columns
// (tool_sets/mcp_connections/skills/hooks) on both agent_revisions and run_template_revisions. It
// references projects/agent_revisions/run_template_revisions (000001/000019), so it opens from the tip
// of the E11 chain — the first link past 000023.
//
//go:embed migrations/000024_tools.up.sql
var migrationUp24 string

//go:embed migrations/000024_tools.down.sql
var migrationDown24 string

//go:embed queries/tools.sql
var toolsSQL string

// 000025 adds the remote-tool async-operation ledger (remote_tool_operations, E12 Task 4, spec
// §28.24-28.25): the durable 202-invoke row a remote_http tool opens before its signed invoke, carrying
// the one-use callback token hash + deadline + fence, so a signed callback commits under a live fence
// and a late one reconciles instead of silently committing. It references projects (000001) only
// (tool_call_id is a soft correlation key, not an FK — the operation opens before the tool_calls row is
// committed), so it opens from the tip of the E12 chain — the first link past 000024.
//
//go:embed migrations/000025_remote_tools.up.sql
var migrationUp25 string

//go:embed migrations/000025_remote_tools.down.sql
var migrationDown25 string

//go:embed queries/remote_tools.sql
var remoteToolsSQL string

// 000026 adds the mcp_connections registry (E12 Task 5, spec §28.13-28.14): admin-registered upstream MCP
// server bindings a project discovers connection-namespaced tool_revisions from. It references
// projects/agent_revisions/run_template_revisions (000001/000019/000024 riders) and adds NO tool columns
// (a discovered tool is a 000024 tool_revisions row linked by executor_config->>'connection_id'), so it
// opens from the tip of the 000025 chain (T4 landed first at merge — sequential, no gap).
//
//go:embed migrations/000026_mcp.up.sql
var migrationUp26 string

//go:embed migrations/000026_mcp.down.sql
var migrationDown26 string

//go:embed queries/mcp.sql
var mcpSQL string

// 000027 adds the skills registry (E12 Task 7, spec §28.15-28.16, TOL-011): tenant-scoped skill
// lineages + immutable SkillRevisions (sanitized archive + digest + scan findings + metadata) and the
// runs.skill_pins rider that freezes a run's resolved skill digests at run-start. It references
// projects/runs (000001) and ALTERs runs (000001), so it opens from the tip of the 000026 chain (T5
// landed first at merge — sequential, no gap).
//
//go:embed migrations/000027_skills.up.sql
var migrationUp27 string

//go:embed migrations/000027_skills.down.sql
var migrationDown27 string

//go:embed queries/skills.sql
var skillsSQL string

// 000028 adds the hooks registry (E12 Task 8, spec §28.17, TOL-012): admin-registered extension points that
// fire inside the run's single dispatch loop at five pinned points. It references projects (000001) and adds
// NO run/tool columns (a hook is looked up per (org, project, point) at fire time, not pinned on a run), so it
// opens from the tip of the 000027 chain (T7 landed first at merge — sequential, no gap).
//
//go:embed migrations/000028_hooks.up.sql
var migrationUp28 string

//go:embed migrations/000028_hooks.down.sql
var migrationDown28 string

//go:embed queries/hooks.sql
var hooksSQL string

// The DB-backed model-routing queries (E13 Task 8) need no migration of their own: they are the first
// reader/writer of 000001's model_connections / model_routes / model_route_revisions.
//
//go:embed queries/model_routes.sql
var modelRoutesSQL string

// 000029 turns tenant isolation into a database guarantee (E13 Task 1, TEN-001/TEN-002): ENABLE + FORCE
// ROW LEVEL SECURITY with one identical org (and, where the table has it, project) policy per
// tenant-scoped table, enforced against 000001's already-declared non-owner `palai_app` role that
// OpenPool now switches every application connection onto. It creates no table and alters no column —
// it re-asserts policies over whatever the chain has defined by then, so it must stay LAST in the
// chain's semantics even as 000030+ append after it (each new tenant table carries its own policy via
// the catalogue-driven loop on the next boot; tests/security/tenancy fails if one does not).
//
//go:embed migrations/000029_row_level_security.up.sql
var migrationUp29 string

//go:embed migrations/000029_row_level_security.down.sql
var migrationDown29 string

// 000030 gives the tenancy provisioning API (E13 Task 2, TEN-003/MCI-001) its enforceable key store:
// the api_keys.scopes / expires_at columns VerifyAPIKey checks, plus two least-privilege hardening steps
// (revoke ledger writes from the runtime role; a guarded palai_app role-membership grant for managed PG).
// It ALTERs api_keys only (no new table), so it opens from the tip of the 000029 chain — sequential, no gap.
//
// M3 RULE for every migration after this one: a NEW tenant-scoped table (any table carrying
// organization_id) MUST call palai_apply_tenant_policy in its OWN up.sql. 000029's catalogue loop covers
// it on the next boot, but tests/security/tenancy fails a table that ships without ENABLE+FORCE, so make
// the policy explicit where the table is born. 000030's re-assertion of the api_keys policy is the pattern
// to copy (see storage/migrations/000030_api_key_scope.up.sql).
//
//go:embed migrations/000030_api_key_scope.up.sql
var migrationUp30 string

//go:embed migrations/000030_api_key_scope.down.sql
var migrationDown30 string

// 000031 adds the secret_refs store behind the restart-less secret write-path (E13 Task 3, SEC-002/MCI-002):
// a tenant POSTs a secret value over the API, it is envelope-encrypted at rest (single master-key AES-256-GCM),
// and the resolver reads the latest version fresh so a rotation takes effect with no restart. It is the first
// NEW table after 000029, so it asserts its OWN tenant policy (the M3 rule) AND its own palai_app grant.
//
//go:embed migrations/000031_secret_refs.up.sql
var migrationUp31 string

//go:embed migrations/000031_secret_refs.down.sql
var migrationDown31 string

// 000032 opens the metering half of E13 Task 6: the append-only usage_ledger every settled meter lands in,
// plus the durable budgets/quotas admission limits read against it. It runs LAST, so its append-only REVOKE
// and its deliberately ORG-level tenant policy both re-assert after the earlier blanket grants and 000029's
// project-aware catalogue sweep on every boot.
//
//go:embed migrations/000032_usage_ledger.up.sql
var migrationUp32 string

//go:embed migrations/000032_usage_ledger.down.sql
var migrationDown32 string

//go:embed queries/usage.sql
var usageSQL string

//go:embed queries/secrets.sql
var secretsSQL string

//go:embed queries/jobs.sql
var jobsSQL string

//go:embed queries/events.sql
var eventsSQL string

//go:embed queries/responses.sql
var responsesSQL string

//go:embed queries/identity.sql
var identitySQL string

//go:embed queries/provisioning.sql
var provisioningSQL string

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

// MigrationUp is the forward migration chain, applied in version order (000001..000023). Each file is
// individually idempotent, so the whole chain is safe to re-run. 000019 (E11 Task 1 agents) and 000020
// (E11 Task 4 webhooks + events cursor rider) land in parallel and interleave here at merge; 000021 (E11
// Task 2 triggers) opens from the tip of both; 000022 (E11 Task 3 schedules) opens from the tip of 000021.
func MigrationUp() string {
	return migrationUp + "\n" + migrationUp2 + "\n" + migrationUp3 + "\n" + migrationUp4 + "\n" + migrationUp5 + "\n" + migrationUp6 + "\n" + migrationUp7 + "\n" + migrationUp8 + "\n" + migrationUp9 + "\n" + migrationUp10 + "\n" + migrationUp11 + "\n" + migrationUp12 + "\n" + migrationUp13 + "\n" + migrationUp14 + "\n" + migrationUp15 + "\n" + migrationUp16 + "\n" + migrationUp17 + "\n" + migrationUp18 + "\n" + migrationUp19 + "\n" + migrationUp20 + "\n" + migrationUp21 + "\n" + migrationUp22 + "\n" + migrationUp23 + "\n" + migrationUp24 + "\n" + migrationUp25 + "\n" + migrationUp26 + "\n" + migrationUp27 + "\n" + migrationUp28 + "\n" + migrationUp29 + "\n" + migrationUp30 + "\n" + migrationUp31 + "\n" + migrationUp32
}

// MigrationDown reverses MigrationUp in the opposite order: each migration drops its added
// objects before the earlier one drops the tables that carried them.
func MigrationDown() string {
	return migrationDown32 + "\n" + migrationDown31 + "\n" + migrationDown30 + "\n" + migrationDown29 + "\n" + migrationDown28 + "\n" + migrationDown27 + "\n" + migrationDown26 + "\n" + migrationDown25 + "\n" + migrationDown24 + "\n" + migrationDown23 + "\n" + migrationDown22 + "\n" + migrationDown21 + "\n" + migrationDown20 + "\n" + migrationDown19 + "\n" + migrationDown18 + "\n" + migrationDown17 + "\n" + migrationDown16 + "\n" + migrationDown15 + "\n" + migrationDown14 + "\n" + migrationDown13 + "\n" + migrationDown12 + "\n" + migrationDown11 + "\n" + migrationDown10 + "\n" + migrationDown9 + "\n" + migrationDown8 + "\n" + migrationDown7 + "\n" + migrationDown6 + "\n" + migrationDown5 + "\n" + migrationDown4 + "\n" + migrationDown3 + "\n" + migrationDown2 + "\n" + migrationDown
}

var namedQueries = parseNamedQueries(usageSQL, agentsSQL, jobsSQL, eventsSQL, responsesSQL, identitySQL, provisioningSQL, secretsSQL, sessionsSQL, commandsSQL, configSQL, auditSQL, workspacesSQL, artifactsSQL, repositoryBindingsSQL, mergeRecordsSQL, changesetsSQL, tasksSQL, publicationsSQL, recoverySQL, webhooksSQL, triggersSQL, schedulesSQL, toolsSQL, remoteToolsSQL, mcpSQL, skillsSQL, hooksSQL, modelRoutesSQL)

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

// applyScope is the statement every acquisition runs before the caller's first query. It does two
// things in one round trip:
//
//   - Switches the session onto the non-owner RuntimeRole, so migration 000029's row-level-security
//     policies actually apply (they are inert for the owner or a superuser). The role is looked up
//     rather than named literally so a pool opened against a database whose chain has not run yet —
//     the very first boot, before 000001 creates it — still connects and can migrate.
//   - Publishes the acquiring context's tenant into palai.org_id / palai.project_id / palai.system,
//     which is what the policies read. An unmarked context publishes empty strings, and the policies
//     then match nothing: a query that forgot to declare its tenant returns zero rows instead of
//     the whole installation.
const applyScope = `SELECT set_config('palai.org_id', $1, false),
       set_config('palai.project_id', $2, false),
       set_config('palai.system', $3, false),
       set_config('role', coalesce((SELECT rolname FROM pg_roles WHERE rolname = $4), 'none'), false)`

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
	// The scope is re-applied per ACQUISITION, not per connection: a pooled connection is handed to
	// one caller at a time, so the tenant it published cannot outlive the acquisition that set it and
	// cannot bleed into the next borrower.
	//
	// ponytail: one extra round trip per acquire. Correct and stateless; if it ever shows up in
	// latency, cache the last-applied scope per *pgx.Conn (keyed off a map cleaned in BeforeClose) and
	// skip the statement when it is unchanged.
	config.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		s := scopeFrom(ctx)
		system := ""
		if s.system {
			system = "on"
		}
		if _, err := conn.Exec(ctx, applyScope, s.organization, s.project, system, RuntimeRole); err != nil {
			// Destroy the connection and fail the instigating query: a connection whose scope could
			// not be published must never serve a query, because it would serve it unscoped.
			return false, fmt.Errorf("apply tenant scope: %w", err)
		}
		return true, nil
	}
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
