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

// 000033 introduces schema_revisions, the append-only migration journal the boot runner writes one row to
// per applied migration (E15 T1, OPS-006). It runs after 000029's blanket GRANT in the chain, so its
// self-re-asserting REVOKE UPDATE,DELETE keeps the journal append-only every boot (the usage_ledger
// precedent). It is installation-global (no organization_id), so it takes no tenant policy.
//
//go:embed migrations/000033_migration_journal.up.sql
var migrationUp33 string

//go:embed migrations/000033_migration_journal.down.sql
var migrationDown33 string

// 000034 is the expand/migrate/CONTRACT example (E15 T1): it DROPs usage_events, which 000032's
// usage_ledger superseded a release earlier and which has zero readers. It carries no tenant policy and
// no grant — a DROP only. See its up.sql for the rollback-window justification.
//
//go:embed migrations/000034_contract_usage_events.up.sql
var migrationUp34 string

//go:embed migrations/000034_contract_usage_events.down.sql
var migrationDown34 string

// 000035 adds the Slack integration store (E17 Task 1, spec §36): slack_connections (an admin-registered
// workspace binding whose signing secret + bot token are secret_ref handles, never inline) and
// slack_thread_sessions (the (team, channel, thread) -> canonical session correlation, SLK-003). Both carry
// organization_id + project_id, so each asserts its OWN org/project tenant policy (the M3 rule) and its own
// palai_app grant — they are the first tables after 000029's blanket grant. It references organizations /
// projects / sessions (000001), so it opens from the tip of the 000034 chain.
//
//go:embed migrations/000035_slack.up.sql
var migrationUp35 string

//go:embed migrations/000035_slack.down.sql
var migrationDown35 string

//go:embed queries/slack.sql
var slackSQL string

// 000036 opens the knowledge spine (E17 Task 4): the immutable FTS ingestion/index/retrieval tables. Every
// table carries organization_id + project_id, so it asserts its own project-aware policy (M3 born-secured);
// the three *_revisions tables add a self-re-asserting REVOKE UPDATE,DELETE (append-only corpus). It runs
// last in the chain, so both re-assert after 000001/000029's blanket grants every boot.
//
//go:embed migrations/000036_knowledge.up.sql
var migrationUp36 string

//go:embed migrations/000036_knowledge.down.sql
var migrationDown36 string

// 000037 adds the queue-adapter tables (E17 Task 7, spec §34.1-34.5): the durable inbound consumer queue
// (queue_messages), its append-only idempotency ledger (queue_effect_receipts, self-re-asserting REVOKE),
// and the outbound result-delivery outbox (queue_deliveries), all under queue_connections. It follows
// 000020's webhook durable-delivery discipline. It references projects (000001), so it opens from the tip
// of the chain (000035 Slack / 000036 knowledge landed first per the §1 merge order).
//
//go:embed migrations/000037_queues.up.sql
var migrationUp37 string

//go:embed migrations/000037_queues.down.sql
var migrationDown37 string

//go:embed queries/queues.sql
var queuesSQL string

// 000038 adds the A2A 1.0 server-projection tables (E17 Task 2, spec §38): a2a_interfaces (the published
// Agent Card projection + auth policy — its card columns carry only SAFE fields, the source revision's
// model/tools/instructions were dropped at publish) and a2a_task_refs (the EXTERNAL A2A task/context id
// bridged beside the CANONICAL run/session id, never replacing it — §38.2). Both carry organization_id +
// project_id, so each asserts its own project-aware tenant policy (M3 born-secured) and its own palai_app
// grant. It references projects (000001), so it opens from the tip of the chain (after 000037 queues).
//
//go:embed migrations/000038_a2a.up.sql
var migrationUp38 string

//go:embed migrations/000038_a2a.down.sql
var migrationDown38 string

// 000039 adds the A2A 1.0 CLIENT registration table (E17 Task 3, spec §38.5): a2a_remote_agents — one row per
// registered OUTBOUND remote A2A agent (an external child-run executor or tool-like specialist) pinning the
// trust envelope the client enforces: card identity/version, endpoint, the auth Connection secret_ref HANDLE
// (the remote connection's OWN credential, never the parent's — A2A-005/SUB-007), modality + extension-URI
// allowlists, data/cost policy, and the request timeout. It carries organization_id + project_id, so it
// asserts its own project-aware tenant policy (M3 born-secured) and its own palai_app grant. It references
// projects (000001), so it opens from the tip of the chain (after 000038 a2a). This is the FINAL number of the
// E17 chain in this worktree.
//
//go:embed migrations/000039_a2a_remote_agents.up.sql
var migrationUp39 string

//go:embed migrations/000039_a2a_remote_agents.down.sql
var migrationDown39 string

//go:embed queries/a2a.sql
var a2aSQL string

// 000040 adds the CapabilityWorker contract tables (E17 Task 9, spec §31.2-31.6): capability_workers (the
// enrolled-worker registry — mutable health/heartbeat/lease_fence) and capability_jobs (the APPEND-ONLY job
// journal, one immutable entry per lifecycle transition, self-re-asserting REVOKE). Built as 000039 in the
// T9 worktree for isolated contiguity and renumbered to 000040 at merge (§1: T3 a2a-client=039 landed first).
// It references projects (000001), so it opens from the tip (000039_a2a_remote_agents).
//
//go:embed migrations/000040_capability_workers.up.sql
var migrationUp40 string

//go:embed migrations/000040_capability_workers.down.sql
var migrationDown40 string

// 000041 adds slack_reply_deliveries — the Slack RETURN LEG's durable order-to-post (E19, SLK-006). The
// enqueue runs INSIDE the run's terminal transaction, so a finished run and the recorded intent to answer
// commit together; UNIQUE (run_id) is what makes "exactly one visible message per run" a database fact
// through a retried transaction, a reconciled cancel, or a restarted poster. It also indexes
// slack_thread_sessions (session_id): the enqueue looks a terminating run's session up on EVERY terminal
// transition in the deployment, and unindexed that is a sequential scan in the hot path. It references
// runs (000001) and slack_connections (000035), so it opens from the tip (000040_capability_workers).
//
//go:embed migrations/000041_slack_replies.up.sql
var migrationUp41 string

//go:embed migrations/000041_slack_replies.down.sql
var migrationDown41 string

// 000042 adds slack_message_turns — the handle from a Slack MESSAGE to the turn it became — and
// responses.retracted_at, the withdrawn marker SessionHistory filters on (E20, SLK-005). Together they are
// what makes an edit supersede and a deletion retract instead of birthing a run: the live app answered a
// deleted message, and a deleted message that stays in the history is a leak in the other direction. It
// references responses/sessions (000001) and slack_connections (000035), so it opens from the tip
// (000041_slack_replies).
//
//go:embed migrations/000042_slack_message_turns.up.sql
var migrationUp42 string

//go:embed migrations/000042_slack_message_turns.down.sql
var migrationDown42 string

// 000043 adds requester_user_id to slack_message_turns and slack_reply_deliveries — the DURABLE identity the
// agent's own question is addressed to (E21 T3). The id arrives on every event and lived only in memory; the
// worker that posts the answer runs minutes later and across restarts, so the mention needed a column. It is
// written at admission next to the turn handle and COPIED onto the delivery at enqueue, frozen exactly as
// channel_id/thread_ts are. DEFAULT ” is the fail-closed value: a row that predates this sends its words
// with no mention rather than an invented address. Two ALTERs on tables that already carry their tenant
// policy (000041, 000042), so it opens from the tip (000042_slack_message_turns).
//
//go:embed migrations/000043_slack_requester.up.sql
var migrationUp43 string

//go:embed migrations/000043_slack_requester.down.sql
var migrationDown43 string

// 000044 makes an APPROVAL an operation-agnostic thing (E23 T1). Four riders: approvals gains a
// tool_call_id target beside a now-nullable publication_id (the table's own 000013 comment asked for this
// by name), publications.operation admits merge_pull_request (a CHECK, so the owner's third operation is a
// migration and not a switch case), tool_revisions carries an operator-declared approval_required +
// approval_label, and slack_approval_deliveries is the order-to-post keyed UNIQUE (approval_id) — a run
// can owe a human several answers, so it cannot reuse 000041's UNIQUE (run_id) claim without weakening it.
// Opens from the tip (000043).
//
//go:embed migrations/000044_tool_approvals.up.sql
var migrationUp44 string

//go:embed migrations/000044_tool_approvals.down.sql
var migrationDown44 string

// 000045 (E24 T1 runner fleet): the runner REGISTRY. runner_pools gains a posture/os/arch and a
// strict_enrollment gate, runner_pool_keys is the hashed per-pool enrolment credential, runners
// becomes an inventory keyed on a SERVER-minted id (the client's runner_id demotes to a label),
// runner_enrollments is the APPEND-ONLY issuance journal (self-re-asserting REVOKE), runs records
// where it was placed, and a default pool is seeded so a single-runner install is bit-unchanged.
// NOTE for the next reader: runner_pools/runners are NOT new — 000001 created them empty and 000029
// already secured them; 45 changes their shape, which is why it re-CALLs the tenant policy for both.
// Opens from the tip (000044).
//
//go:embed migrations/000045_runner_fleet.up.sql
var migrationUp45 string

//go:embed migrations/000045_runner_fleet.down.sql
var migrationDown45 string

// 000046 (E25 T3 environments): a named group of key→value pairs an agent's shell receives.
// `environments` is the grouping identity and `environment_values` is MEMBERSHIP — not ciphertext: the
// values stay in secret_refs (000031) under the derived name `env:<environment_id>:<key>`, so no second
// encryption path is opened. agent_revisions gains an `environment` column, and it lands in the same
// change as the code that reads it (000019's own comment forbids storing dead config).
// NOTE for the next reader: environment_values is the ONE table in the chain granted DELETE while its
// value store is not — removing a key removes the BINDING, never the bytes. Opens from the tip (000045).
//
//go:embed migrations/000046_environments.up.sql
var migrationUp46 string

//go:embed migrations/000046_environments.down.sql
var migrationDown46 string

// 000047 (E26 T1 background tasks): a PROCESS that belongs to the run rather than to the tool call that
// started it. Ownership was bound to the `ShellRunner.Run` call in both postures — a `defer`
// ContainerRemove in the OCI driver, a process-GROUP kill through the attempt context on the host — so
// a background process was not a missing feature but a contradiction, and this row is what resolves it.
// `handle` means a container id under one posture and `pgid:starttime` under the other, which is why
// `posture` carries a CHECK: the reaper probes a different operating-system object per posture.
// NOTE for the next reader: `exit_code` is NULL until known and NEVER a sentinel, and `state = 'lost'`
// means a process we cannot prove is ours — it is never signalled. Opens from the tip (000046).
//
//go:embed migrations/000047_background_tasks.up.sql
var migrationUp47 string

//go:embed migrations/000047_background_tasks.down.sql
var migrationDown47 string

// 000048 (E29 session list): the Sessions SCREEN. `sessions.name` is the operator's LABEL — not unique,
// because the reference screen shows several sessions sharing one — and it is the only genuinely new
// FACT here. The other three columns the screen wants are aggregates over rows that already exist:
// the agent is per-RUN (000019's runs.agent_revision_id), the tokens are usage_ledger's (000032, one
// writer, deduped), and the span is runs.created_at/updated_at. So this migration adds one column and
// four indexes, one of which is a REPAIR: `sessions` had carried only its primary key since 000001, so
// every page of every session list sequentially scanned and sorted the table. Opens from the tip
// (000047).
//
//go:embed migrations/000048_session_list.up.sql
var migrationUp48 string

//go:embed migrations/000048_session_list.down.sql
var migrationDown48 string

// 000049 (usage series): ONE covering index, so GET /v1/usage/series is a bucketed aggregate instead of
// a sequential walk of a tenant's ledger. No column and no table — the series is a different READ of
// rows 000032 already settles, and a denormalised counter would be a second writer for a number that
// has one. It is covering because the cost was never finding the rows, it was the scattered heap
// fetches after: measured on 500k seeded rows, 5042 buffers -> 54, and a NON-covering index on the same
// three columns did not help at all (the migration header carries the table). Opens from the tip
// (000048).
//
//go:embed migrations/000049_usage_series.up.sql
var migrationUp49 string

//go:embed migrations/000049_usage_series.down.sql
var migrationDown49 string

// 000050 (usage step attribution): usage_ledger.model_request_id — the turn a settlement belongs to,
// as a COLUMN rather than a substring of dedupe_key. Settlement was ALREADY per model request (the key
// has always been 'mreq:<id>:<meter>'); what did not exist was a way to read it without parsing an
// idempotency detail. Idempotency is therefore untouched — the conflict target does not move, because
// the identity was already per-step. NULL is meaningful, not missing: run.admitted is the admission
// reservation and has no model call. Opens from the tip (000049).
//
//go:embed migrations/000050_usage_step_attribution.up.sql
var migrationUp50 string

//go:embed migrations/000050_usage_step_attribution.down.sql
var migrationDown50 string

// 000051 (E29 provider wiring): the ENDPOINT a model connection dials, plus the stamp of the last
// credential probe. `base_url` is what makes a custom OpenAI-compatible provider a per-PROJECT property:
// before it, the only way to move that endpoint was PALAI_OPENAI_COMPATIBLE_BASE_URL, read once at boot
// into one adapter value, so a deployment had exactly one custom endpoint and the console could not name
// it. `verified_at`/`verification_outcome` cache an OBSERVATION and gate nothing. No new table and so no
// tenant-policy call — model_connections has been under 000029's catalogue loop since that migration ran.
//
// IT WAS WRITTEN AS 000049 AND RENUMBERED AT MERGE. The branch was cut when 000048 was the tip and took
// the next contiguous number in isolation; usage_series and usage_step_attribution landed on main in the
// meantime and took 49 and 50. Nothing in this migration depends on either — it ALTERs a table 000001
// created — so the renumber is a rename plus two markers, not a reordering. Opens from the tip (000050).
//
//go:embed migrations/000051_model_connection_endpoint.up.sql
var migrationUp51 string

//go:embed migrations/000051_model_connection_endpoint.down.sql
var migrationDown51 string

// 000052 (E29 T3): a webhook endpoint becomes deletable, and the delete stops future sending without
// erasing what was already sent. It DROPS the ON DELETE CASCADE that webhook_deliveries.endpoint_id has
// carried since 000020, so a delivery — an audit record of a payload this deployment actually sent —
// outlives the endpoint it was addressed to, with its endpoint_id left unresolvable on purpose. What stops
// the sending instead is DueDeliveries' inner join to webhook_endpoints. The migration file carries the
// full argument, including what dropping the key trades away and why that is affordable.
//
// NOTE FOR THE INTEGRATOR: written as 000052 against a tip of 000051, which is the next contiguous number
// from main. The `desired-config` branch also took 000052 in parallel; whichever lands second renumbers —
// a rename plus the two markers (the file's `VALUES (52)` and the down file's `WHERE version = 52`) plus
// this embed var, since nothing here depends on any migration after 000020.
//
//go:embed migrations/000052_webhook_delivery_trail_survives_its_endpoint.up.sql
var migrationUp52 string

//go:embed migrations/000052_webhook_delivery_trail_survives_its_endpoint.down.sql
var migrationDown52 string

// 000053 (E29 desired configuration): deployment_desired — the APPEND-ONLY journal of what this MACHINE
// should be running with, written by the admin panel and applied by the next bring-up. It is the write
// half of GET /v1/deployment, which made thirty-five settings readable and thirty-two of them
// bring_up-only, so there is no live write path and a form claiming one would be the defect that surface
// exists to expose.
//
// NO organization_id, AND THAT ABSENCE IS THE SECURITY PROPERTY. Four of the eleven writable settings are
// ADMISSION BOUNDS that exist to bound a tenant (PALAI_MAX_CONCURRENT_RUNS, PALAI_MAX_QUEUED_RUNS,
// PALAI_REQUEST_RATE_PER_SEC, PALAI_REQUEST_BURST); a tenant-scoped home for them would let a tenant raise
// the limit that holds it. With no tenant column there is nothing a later per-tenant policy could key on.
// So: no palai_apply_tenant_policy call, and a BY-NAME entry in tests/security/tenancy's nonTenantTables.
// Opens from the tip. RENUMBERED 000052 -> 000053 at merge: E29 T3 took 000052 in parallel and landed first.
//
//go:embed migrations/000053_deployment_desired.up.sql
var migrationUp53 string

//go:embed migrations/000053_deployment_desired.down.sql
var migrationDown53 string

// 000054 (structured output): runs.output_contract — the §22.7 JSON Schema a run's answer must
// satisfy, resolved at admission and read by BOTH model dispatch (which turns it into the provider's
// own decoding constraint) and finalize (which checks the answer before the run may be called
// completed). It is on the run, beside `delegation` (000007), for the same reason that one is: the
// readers do not hold the request body, and re-deriving the contract per reader is how the thing
// asked of the model and the thing checked afterwards drift apart. NULL means the request named no
// format — every run before this migration, and every run that does not opt in.
//
// TAKEN AS 000052 BECAUSE THAT IS THE NEXT CONTIGUOUS NUMBER IN THIS TREE, AND IT COLLIDES.
// The `desired-config` branch also holds 000052. This migration only ALTERs a table 000001 created
// and depends on nothing in between, so the fix at merge is a rename plus two markers (the filename
// pair, the `VALUES (52)` line, and this embed var) — not a reordering. Same situation 000051 was in
// when it was written as 000049; see the note above it.
//
//go:embed migrations/000054_run_output_contract.up.sql
var migrationUp54 string

//go:embed migrations/000054_run_output_contract.down.sql
var migrationDown54 string

// 000055 (agent configuration): runs.instructions — the run-specific instruction layer (spec §25.12
// layer 5), the `instructions` string the create request carried. It is a column for the same reason
// output_contract above is: the reader is model dispatch, once per step, and it does not hold the
// request body.
//
// MEASURED, not assumed: before this migration the field reached the model as nothing. The same
// input with a 320-word `instructions` and with none both measured usage.input_tokens 48 against a
// real provider. It was accepted, hashed into the idempotency record, and stored nowhere.
//
// The PINNED REVISION's instructions (§25.12 layer 3) are NOT copied here — they already live on
// agent_revisions.instructions and the run pins the revision immutably, which is what makes an old
// run reproducible. What was missing on that side was a SELECT, not a column.
//
//go:embed migrations/000055_run_instructions.up.sql
var migrationUp55 string

//go:embed migrations/000055_run_instructions.down.sql
var migrationDown55 string

// 000056 (approvals): sessions.auto_approve_tools / auto_approve_publications / auto_approve_set_by /
// auto_approve_set_at — a session's STANDING AUTHORIZATION for the two approval families (spec §22.4).
//
// TWO FLAGS AND NOT ONE, which is the whole reason this migration exists rather than a single
// `auto_approve` boolean: running `xcodebuild` without being asked each time and pushing to somebody's
// repository without being asked are not the same risk, and an operator who wanted the first must not
// silently get the second. See the file's own header for the argument.
//
// set_by is LOAD-BEARING, not audit trim: an armed session decides as the human who armed it, so the
// decision still passes ApproverAllowed(set_by) and an armed session can never authorize more than that
// person could have authorized by clicking.
//
//go:embed migrations/000056_session_auto_approve.up.sql
var migrationUp56 string

//go:embed migrations/000056_session_auto_approve.down.sql
var migrationDown56 string

// 000057 (repositories): changesets.ignored_file_count — the .gitignore'd files the changed set leaves
// out on purpose (spec §30.6, REP-005).
//
// The changed set gained a second source: a git observation of the clone, because the shell tool writes
// to the same workspace as the file tool and keeps no ledger to project. A compiler writes thousands of
// ignored files, so the observation has to drop them — and a count is the difference between a record
// that says "nothing changed" and one that says "one file, and 1284 ignored build outputs".
//
//go:embed migrations/000057_changeset_ignored_file_count.up.sql
var migrationUp57 string

//go:embed migrations/000057_changeset_ignored_file_count.down.sql
var migrationDown57 string

// 000058 (execution): job_attempts.error — WHY a claimed job's attempt failed (spec §24.4).
//
// `Worker.process` routed a handler error through the retry policy and then dropped it: the ledger
// recorded THAT every attempt failed and nowhere why. On 2026-08-02 a live stack carried 11 dead
// response.run jobs and 208 preempted attempts whose cause was readable only in a container's stdout.
//
//go:embed migrations/000058_job_attempt_error.up.sql
var migrationUp58 string

//go:embed migrations/000058_job_attempt_error.down.sql
var migrationDown58 string

// 000059 (repository): repository_bindings.archived_at — the binding lifecycle that made its CREDENTIAL
// fixable and its duplicates retirable (spec §30.1).
//
// Measured before it was written: 20 bindings on the live stack, connection_ref absent on all 20, and 8
// of them the same repository_identity. There was no code path anywhere that could set that column after
// the insert — not a route, not a store method, not a statement, not a CLI verb — so a binding registered
// against a private repository before its credential existed could never be given one, and "register
// another" was the only move a console could offer.
//
// ONLY THE CREDENTIAL BECOMES MUTABLE. Identity (provider / repository_identity / clone_url) stays fixed
// because preparation_receipts rows already assert which repository a run cloned, and a mutable identity
// would falsify them retroactively. The file's own header carries that argument and the reason
// allowed_operations and policy are excluded too.
//
// The column is what an archived binding is; what makes archiving MEAN anything is one clause in
// queries/repository_bindings.sql — RepositoryBindingExists, the run-admission guard, gains
// `archived_at IS NULL`, so an archived binding refuses new runs rather than merely leaving the list.
//
//go:embed migrations/000059_repository_binding_lifecycle.up.sql
var migrationUp59 string

//go:embed migrations/000059_repository_binding_lifecycle.down.sql
var migrationDown59 string

// 000060 (fleet/config): the desired configuration reaches a machine that is ALREADY RUNNING, is
// addressable per MACHINE and not only per pool, and the machine says back what it did with it.
//
// Measured 2026-08-03, which is what made it a migration rather than an opinion: the live pool document
// asking for PALAI_RUNNER_CONCURRENCY=4 was written at 08:08, and the runner it configures started at
// 21:47 the previous evening. Delivery rides ENROLMENT, so that machine had never seen the document.
//
// The same measurement nearly reported the opposite: `docker inspect` shows that runner holding
// PALAI_RUNNER_CONCURRENCY=4, because compose sets it and planeIntDefault falls back to the environment
// when the plane sent nothing. The panel and the machine agreed on 4 and no byte had travelled between
// them. Every proof of delivery below therefore uses a value the machine's environment does not hold.
//
// THREE CHANGES, one theme — a document is worth nothing until a machine is observed running it:
// the `runner_machine` plane (scope_id = runner id, overlaid ON TOP OF the pool's document, key by key);
// three columns on `runners` carrying the machine's OWN answer; and the applied/pending_restart
// distinction that keeps a panel from reporting a value as live when the machine has merely received it.
//
//go:embed migrations/000060_machine_desired_config.up.sql
var migrationUp60 string

//go:embed migrations/000060_machine_desired_config.down.sql
var migrationDown60 string

// integration_bots (2026-08-03 plan Task 4): the kind-agnostic bot registry. See the migration's own
// header for the rule it exists to hold — no FK ever points at this table. Renumbered 60 -> 61: 000060
// was taken by machine_desired_config above, which merged into main first.
//
//go:embed migrations/000061_integration_bots.up.sql
var migrationUp61 string

//go:embed migrations/000061_integration_bots.down.sql
var migrationDown61 string

// tenant_policy_by_project (2026-08-03 plan A.2 Task 2): rekeys row-level-security isolation from
// organization_id to project_id — see the migration's own header for the three named exclusions
// (api_keys, principals, usage_ledger) that keep their organization_id-keyed form.
//
//go:embed migrations/000062_tenant_policy_by_project.up.sql
var migrationUp62 string

//go:embed migrations/000062_tenant_policy_by_project.down.sql
var migrationDown62 string

// relax_organization_id (2026-08-03 plan A.2 Task 4): organization_id becomes nullable and its foreign
// keys to organizations are dropped, so Task 5 can stop writing the column without the first INSERT
// after that change failing on a constraint. No writer stops supplying it here — see the migration's
// own header for the three primary-key members that keep NOT NULL.
//
//go:embed migrations/000063_relax_organization_id.up.sql
var migrationUp63 string

//go:embed migrations/000063_relax_organization_id.down.sql
var migrationDown63 string

// background_task_machine (2026-08-03 plan A.3 Task 6): a background task records which MACHINE started
// it, because a probe on the wrong kernel answers `exited` rather than failing. ” is "unknown", which
// the reader treats as `lost` — see the migration's own header.
//
//go:embed migrations/000064_background_task_machine.up.sql
var migrationUp64 string

//go:embed migrations/000064_background_task_machine.down.sql
var migrationDown64 string

// The runner registry statements (E24 T1, migration 000045). They back internal/fleet, which the
// runner gateway takes as an interface — see the package comment there for why the store does not
// live inside execution.
//
//go:embed queries/runners.sql
var runnersSQL string

//go:embed queries/workers.sql
var workersSQL string

//go:embed queries/usage.sql
var usageSQL string

//go:embed queries/secrets.sql
var secretsSQL string

// The environment statements (E25 T3, migration 000046). NOT ONE mentions `ciphertext`: the values are
// secret_refs rows addressed by the derived name `env:<environment_id>:<key>`, and the ciphertext sweep
// in tests/uat/promote_admin_console.go pins that property rather than trusting it.
//
//go:embed queries/environments.sql
var environmentsSQL string

// The background-task statements (E26, migration 000047). They live beside the migration's own file
// rather than inside jobs.sql or tools.sql because a background PROCESS is neither: it is spawned by a
// tool call and outlives that call's ledger row, and it is swept by the reconciler without ever being
// a durable job.
//
//go:embed queries/background.sql
var backgroundSQL string

// The desired-configuration statements (E29, migration 000052). An INSERT and a SELECT and nothing else:
// the table grants no UPDATE and no DELETE, so a third statement would be a permission error dressed as a
// feature. The SELECT's ORDER BY is load-bearing — the row it returns is the one a bring-up turns into the
// process's environment.
//
//go:embed queries/deployment.sql
var deploymentSQL string

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

//go:embed queries/bots.sql
var botsSQL string

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

// The metrics queries (E14 Task 6) back the internal /metrics exposition. They need no migration:
// every one is an installation-aggregate read over tables 000001/000020 already declared.
//
//go:embed queries/metrics.sql
var metricsSQL string

// The knowledge spine queries (E17 Task 4) back the FTS ingestion/index/retrieval store (migration 000035).
//
//go:embed queries/knowledge.sql
var knowledgeSQL string

// MigrationUp is the forward migration chain, applied in version order (000001..000023). Each file is
// individually idempotent, so the whole chain is safe to re-run. 000019 (E11 Task 1 agents) and 000020
// (E11 Task 4 webhooks + events cursor rider) land in parallel and interleave here at merge; 000021 (E11
// Task 2 triggers) opens from the tip of both; 000022 (E11 Task 3 schedules) opens from the tip of 000021.
func MigrationUp() string {
	return migrationUp + "\n" + migrationUp2 + "\n" + migrationUp3 + "\n" + migrationUp4 + "\n" + migrationUp5 + "\n" + migrationUp6 + "\n" + migrationUp7 + "\n" + migrationUp8 + "\n" + migrationUp9 + "\n" + migrationUp10 + "\n" + migrationUp11 + "\n" + migrationUp12 + "\n" + migrationUp13 + "\n" + migrationUp14 + "\n" + migrationUp15 + "\n" + migrationUp16 + "\n" + migrationUp17 + "\n" + migrationUp18 + "\n" + migrationUp19 + "\n" + migrationUp20 + "\n" + migrationUp21 + "\n" + migrationUp22 + "\n" + migrationUp23 + "\n" + migrationUp24 + "\n" + migrationUp25 + "\n" + migrationUp26 + "\n" + migrationUp27 + "\n" + migrationUp28 + "\n" + migrationUp29 + "\n" + migrationUp30 + "\n" + migrationUp31 + "\n" + migrationUp32 + "\n" + migrationUp33 + "\n" + migrationUp34 + "\n" + migrationUp35 + "\n" + migrationUp36 + "\n" + migrationUp37 + "\n" + migrationUp38 + "\n" + migrationUp39 + "\n" + migrationUp40 + "\n" + migrationUp41 + "\n" + migrationUp42 + "\n" + migrationUp43 + "\n" + migrationUp44 + "\n" + migrationUp45 + "\n" + migrationUp46 + "\n" + migrationUp47 + "\n" + migrationUp48 + "\n" + migrationUp49 + "\n" + migrationUp50 + "\n" + migrationUp51 + "\n" + migrationUp52 + "\n" + migrationUp53 + "\n" + migrationUp54 + "\n" + migrationUp55 + "\n" + migrationUp56 + "\n" + migrationUp57 + "\n" + migrationUp58 + "\n" + migrationUp59 + "\n" + migrationUp60 + "\n" + migrationUp61 + "\n" + migrationUp62 + "\n" + migrationUp63 + "\n" + migrationUp64

}

// MigrationDown reverses MigrationUp in the opposite order: each migration drops its added
// objects before the earlier one drops the tables that carried them.
func MigrationDown() string {
	return migrationDown63 + "\n" + migrationDown62 + "\n" + migrationDown61 + "\n" + migrationDown60 + "\n" + migrationDown59 + "\n" + migrationDown58 + "\n" + migrationDown57 + "\n" + migrationDown56 + "\n" + migrationDown55 + "\n" + migrationDown54 + "\n" + migrationDown53 + "\n" + migrationDown52 + "\n" + migrationDown51 + "\n" + migrationDown50 + "\n" + migrationDown49 + "\n" + migrationDown48 + "\n" + migrationDown47 + "\n" + migrationDown46 + "\n" + migrationDown45 + "\n" + migrationDown44 + "\n" + migrationDown43 + "\n" + migrationDown42 + "\n" + migrationDown41 + "\n" + migrationDown40 + "\n" + migrationDown39 + "\n" + migrationDown38 + "\n" + migrationDown37 + "\n" + migrationDown36 + "\n" + migrationDown35 + "\n" + migrationDown34 + "\n" + migrationDown33 + "\n" + migrationDown32 + "\n" + migrationDown31 + "\n" + migrationDown30 + "\n" + migrationDown29 + "\n" + migrationDown28 + "\n" + migrationDown27 + "\n" + migrationDown26 + "\n" + migrationDown25 + "\n" + migrationDown24 + "\n" + migrationDown23 + "\n" + migrationDown22 + "\n" + migrationDown21 + "\n" + migrationDown20 + "\n" + migrationDown19 + "\n" + migrationDown18 + "\n" + migrationDown17 + "\n" + migrationDown16 + "\n" + migrationDown15 + "\n" + migrationDown14 + "\n" + migrationDown13 + "\n" + migrationDown12 + "\n" + migrationDown11 + "\n" + migrationDown10 + "\n" + migrationDown9 + "\n" + migrationDown8 + "\n" + migrationDown7 + "\n" + migrationDown6 + "\n" + migrationDown5 + "\n" + migrationDown4 + "\n" + migrationDown3 + "\n" + migrationDown2 + "\n" + migrationDown

}

var namedQueries = parseNamedQueries(usageSQL, agentsSQL, jobsSQL, eventsSQL, responsesSQL, identitySQL, provisioningSQL, secretsSQL, sessionsSQL, commandsSQL, configSQL, auditSQL, workspacesSQL, artifactsSQL, repositoryBindingsSQL, mergeRecordsSQL, changesetsSQL, tasksSQL, publicationsSQL, recoverySQL, webhooksSQL, triggersSQL, schedulesSQL, toolsSQL, remoteToolsSQL, mcpSQL, skillsSQL, hooksSQL, modelRoutesSQL, metricsSQL, slackSQL, knowledgeSQL, queuesSQL, a2aSQL, workersSQL, runnersSQL, environmentsSQL, backgroundSQL, deploymentSQL, botsSQL)

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
//     which is what the policies read. A non-system, non-org-only scope with no project (an unmarked
//     context included) never reaches this statement at all — PrepareConn below refuses the
//     acquisition first (A.2 Task 1, ErrProjectRequired) rather than publishing an empty project_id
//     that the RLS policy would read as "every project in the organization".
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
		} else if s.project == "" && !s.orgOnly {
			// A.2 Task 1: a non-system tenant scope with no project is not a boundary, it is the
			// absence of one — the RLS policy's own empty-project fallback reads it as "every project
			// in the organization" (000029), and once organizations are gone that means every project,
			// full stop. Refuse the acquisition rather than publish a project_id that admits everything;
			// WithOrgScope is the one deliberate, narrow exception (orgOnly), for the identity/secret
			// surfaces that still span a whole organization.
			return false, fmt.Errorf("apply tenant scope: %w", ErrProjectRequired)
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
	// System-scoped, not the caller's bare ctx (A.2 Task 1): this Ping is OpenPool's own liveness check,
	// issued before any tenant is known — the same reason VerifyAPIKey and the migration boot path
	// (packages/coordinator/migrate.go) run under WithSystemScope. Every OTHER caller of OpenPool passes
	// a boot-time context with no tenant scope at all, so without this the very first acquisition —
	// OpenPool's own — would trip the project-required refusal below and no pool could ever open.
	if err := pool.Ping(WithSystemScope(ctx)); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
