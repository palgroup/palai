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

// THE CHAIN OPENS WITH A TWO-FILE BASELINE, AND IT USED TO BE SIXTY-SEVEN FILES. It was squashed on
// 2026-08-04: fifty of those sixty-seven files built a tenant boundary above the project and the last six
// took it back out, so a fresh install spent its whole boot constructing a boundary this product does not
// have and then dismantling it. 000001 and 000002 are where that chain ARRIVED, derived from a schema
// dump of a database the shipped binary had migrated — see 000001_core.up.sql's own header for the
// derivation command and for why every statement in it is idempotent. Everything from 000003 on is
// ordinary forward work written against that baseline.
//
// THE SPLIT BETWEEN THE TWO BASELINE FILES IS THE CHAIN'S OWN SEAM, not a preference: a policy can only be
// applied to a table that exists, which is why the old chain put its row-level-security sweep two thirds of
// the way along rather than at the start. It also keeps an interrupted boot observable — stopping between
// them leaves tables present and unsecured, a state the resume drill (OPS-006) can be ABOUT. A one-file
// baseline has no partway, and squashing to a single file would have retired that proof silently.

//go:embed migrations/000001_core.up.sql
var migrationUp string

//go:embed migrations/000001_core.down.sql
var migrationDown string

//go:embed migrations/000002_row_level_security.up.sql
var migrationUp2 string

//go:embed migrations/000002_row_level_security.down.sql
var migrationDown2 string

// The first migration after the baseline (Faz A.4 T1): runner_leases records an OCCUPANCY — the interval
// in which one session held one machine — so the machine half of what a customer is billed has a durable
// row at all. It creates no table, which is why it carries no policy call of its own; 000002's sweep
// already secured runner_leases.
//
//go:embed migrations/000003_lease_occupancy.up.sql
var migrationUp3 string

//go:embed migrations/000003_lease_occupancy.down.sql
var migrationDown3 string

// The second migration after the baseline: `responses` gains the `metadata` column, so the field both
// published schemas have always declared has a writer and a reader. It creates no table, which is why it
// carries no policy call of its own; 000002's sweep already secured responses.
//
//go:embed migrations/000004_response_metadata.up.sql
var migrationUp4 string

//go:embed migrations/000004_response_metadata.down.sql
var migrationDown4 string

// The third migration after the baseline (Faz A.4 T5): `runners.capacity` gains a way to say NOTHING. Its
// CHECK refused zero, so the clamp in internal/fleet/store.go had to invent a 1 for every machine that
// declared nothing — which is all of them — and a placement ceiling reading that column would have been
// enforcing a number no operator chose. It creates no table, which is why it carries no policy call of its
// own; 000002's sweep already secured runners.
//
//go:embed migrations/000005_capacity_declaration.up.sql
var migrationUp5 string

//go:embed migrations/000005_capacity_declaration.down.sql
var migrationDown5 string

//go:embed queries/agents.sql
var agentsSQL string

//go:embed queries/webhooks.sql
var webhooksSQL string

//go:embed queries/triggers.sql
var triggersSQL string

//go:embed queries/schedules.sql
var schedulesSQL string

//go:embed queries/tools.sql
var toolsSQL string

//go:embed queries/remote_tools.sql
var remoteToolsSQL string

//go:embed queries/mcp.sql
var mcpSQL string

//go:embed queries/skills.sql
var skillsSQL string

//go:embed queries/hooks.sql
var hooksSQL string

// The DB-backed model-routing queries (E13 Task 8) need no table of their own: they are the first
// reader/writer of model_connections / model_routes / model_route_revisions.
//
//go:embed queries/model_routes.sql
var modelRoutesSQL string

//go:embed queries/queues.sql
var queuesSQL string

//go:embed queries/a2a.sql
var a2aSQL string

// The runner registry statements (E24 T1). They back internal/fleet, which the runner gateway takes as an
// interface — see the package comment there for why the store does not live inside execution.
//
//go:embed queries/runners.sql
var runnersSQL string

// The machine-occupancy statements (Faz A.4 T1). They live beside their own file rather than inside
// runners.sql because the registry and the occupancy are different subjects: runners.sql is an INVENTORY
// of machines, and these five are about an INTERVAL — who held which machine, from when to when, and what
// of it is billed.
//
//go:embed queries/leases.sql
var leasesSQL string

//go:embed queries/workers.sql
var workersSQL string

//go:embed queries/usage.sql
var usageSQL string

//go:embed queries/secrets.sql
var secretsSQL string

// The environment statements (E25 T3). NOT ONE mentions `ciphertext`: the values are secret_refs rows
// addressed by the derived name `env:<environment_id>:<key>`, and the ciphertext sweep in
// tests/uat/promote_admin_console.go pins that property rather than trusting it.
//
//go:embed queries/environments.sql
var environmentsSQL string

// The background-task statements (E26). They live beside their own file rather than inside jobs.sql or
// tools.sql because a background PROCESS is neither: it is spawned by a tool call and outlives that
// call's ledger row, and it is swept by the reconciler without ever being a durable job.
//
//go:embed queries/background.sql
var backgroundSQL string

// The desired-configuration statements (E29). An INSERT and a SELECT and nothing else: deployment_desired
// grants no UPDATE and no DELETE, so a third statement would be a permission error dressed as a feature.
// The SELECT's ORDER BY is load-bearing — the row it returns is the one a bring-up turns into the
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

// The metrics queries (E14 Task 6) back the internal /metrics exposition. They need no table of their
// own: every one is an installation-aggregate read over tables 000001 already declares.
//
//go:embed queries/metrics.sql
var metricsSQL string

// The knowledge spine queries (E17 Task 4) back the FTS ingestion/index/retrieval store.
//
//go:embed queries/knowledge.sql
var knowledgeSQL string

// MigrationUp is the forward chain, applied in version order. Each file is individually idempotent, so
// the whole chain is safe to re-run — which it is, in full, on every boot.
func MigrationUp() string {
	return migrationUp + "\n" + migrationUp2 + "\n" + migrationUp3 + "\n" + migrationUp4 + "\n" + migrationUp5
}

// MigrationDown reverses MigrationUp in the opposite order: 000005 makes zero illegal on runners.capacity
// again, 000004 takes `metadata` back off responses,
// 000003 takes its columns back off runner_leases, 000002 drops the policies and the procedures that
// install them, then 000001 drops the
// tables those policies were on and last the role that held the grants. Store.Rollback runs it, and the
// component tier leans on that to return a shared database to empty between tests — so "reverses" has to
// mean NOTHING is left behind, including the role, which is a cluster object the next database in the same
// cluster would see.
func MigrationDown() string {
	return migrationDown5 + "\n" + migrationDown4 + "\n" + migrationDown3 + "\n" + migrationDown2 + "\n" + migrationDown
}

var namedQueries = parseNamedQueries(usageSQL, agentsSQL, jobsSQL, eventsSQL, responsesSQL, identitySQL, provisioningSQL, secretsSQL, sessionsSQL, commandsSQL, configSQL, auditSQL, workspacesSQL, artifactsSQL, repositoryBindingsSQL, mergeRecordsSQL, changesetsSQL, tasksSQL, publicationsSQL, recoverySQL, webhooksSQL, triggersSQL, schedulesSQL, toolsSQL, remoteToolsSQL, mcpSQL, skillsSQL, hooksSQL, modelRoutesSQL, metricsSQL, knowledgeSQL, queuesSQL, a2aSQL, workersSQL, runnersSQL, leasesSQL, environmentsSQL, backgroundSQL, deploymentSQL, botsSQL)

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
//   - Switches the session onto the non-owner RuntimeRole, so migration 000002's row-level-security
//     policies actually apply (they are inert for the owner or a superuser). The role is looked up
//     rather than named literally so a pool opened against a database whose chain has not run yet —
//     the very first boot, before 000001 creates it — still connects and can migrate.
//   - Publishes the acquiring context's tenant into palai.project_id / palai.system, which is what the
//     policies read. A non-system, non-installation scope with no project (an unmarked context
//     included) never reaches this statement at all — PrepareConn below refuses the acquisition first
//     (A.2 Task 1, ErrProjectRequired) rather than publishing an empty project_id that the policies
//     would read as a row of their own.
//
// THERE IS NO SECOND TENANT GUC. The old chain published one for the boundary above the project, and by
// the end of that chain nothing read it. A GUC that nothing writes and nothing reads is not harmless — it
// reads as a boundary that is still there.
const applyScope = `SELECT set_config('palai.project_id', $1, false),
       set_config('palai.system', $2, false),
       set_config('role', coalesce((SELECT rolname FROM pg_roles WHERE rolname = $3), 'none'), false)`

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
		} else if s.project == "" && !s.installation {
			// A.2 Task 1: a non-system tenant scope with no project is not a boundary, it is the
			// absence of one — the RLS policy's own empty-project fallback read it as "every project
			// in the tenant above", and with that tenant gone it means every project, full stop. Refuse
			// the acquisition rather than publish a project_id that admits everything;
			// WithInstallationScope is the one deliberate, narrow exception, for the two tables that
			// carry no project column at all.
			return false, fmt.Errorf("apply tenant scope: %w", ErrProjectRequired)
		}
		if _, err := conn.Exec(ctx, applyScope, s.project, system, RuntimeRole); err != nil {
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
