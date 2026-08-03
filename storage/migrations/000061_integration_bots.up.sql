-- 000061: integration_bots — A KIND-AGNOSTIC BOT REGISTRY.
--
-- RENUMBERED 60 -> 61 AT LANDING TIME: 000060 was taken by machine_desired_config, which merged into
-- main first (measured at commit time, not assumed from the dispatch-time count). The filename pair, the
-- `VALUES` marker below, the embed vars in storage/embed.go and both pins in storage/migrations_test.go
-- all had to move together — this comment is the marker a reviewer can use to confirm none of them was
-- missed.
--
-- THE ONE RULE THIS TABLE EXISTS TO ENFORCE: the control plane stores a bot's identity (a name), the
-- agent it speaks as, the repository it may attach, the principal it acts as, and an OPAQUE `config`
-- document — and it never learns what a `kind` MEANS. `kind` is a value from a list the console offers
-- ('slack' today; 'whatsapp'/'telegram'/'x' later per the plan); nothing here, in the migration or in
-- apps/control-plane/api/bots.go, special-cases any one of them. No statement in storage/queries/bots.sql
-- inspects config's interior, so a field a future channel needs (a team id, a phone number, whatever)
-- never requires a migration here.
--
-- `config` IS `JSON`, NOT `JSONB` — MEASURED, NOT ASSUMED. `jsonb` decomposes its input into a binary
-- form that reorders object keys and drops insignificant whitespace on the way back out (Postgres's own
-- documented behaviour); a component test against a real database (self-verification for this task)
-- wrote `{"team_id":"T1","channels":["C1"],"anything":42}` through a `jsonb` column and read back
-- `{"team_id": "T1", "anything": 42, "channels": ["C1"]}` — same values, reordered keys, added spaces.
-- That is a silent reshape, and it is exactly what this table exists to never do. `json` retains the
-- caller's exact input text, which is what "stored and returned verbatim" has to mean literally. The
-- cost is real and accepted: no `->`/`->>`/`@>` operator and no GIN index on config's interior — which
-- this design does not want anyway, since no statement is ever meant to look inside it.
--
-- NO FOREIGN KEY ONTO THIS TABLE, ANYWHERE, EVER — that is the plan's own rule (T4 in
-- docs/superpowers/plans/2026-08-03-slack-bot-as-sdk-relay.md), restated here because a migration is
-- where a reader would look for one. `sessions.bot_id` (a later task) carries this row's id as a plain
-- opaque string precisely so the control plane's own schema never has to know this table exists to stay
-- consistent — the same reason `agent_revisions.environment` is a string and not an FK (000046).
--
-- agent_revision_id / repository_binding_id / principal_id default to '' rather than being NULL-able FKs:
-- a bot can be registered before any of the three is chosen (the console wizard fills them in over
-- separate PATCH calls), and an empty string reads the same as "not set yet" everywhere this row is
-- rendered — the agent_revisions.environment idiom again.
--
-- UNIQUE (organization_id, project_id, name): a project's bot picker is keyed on a name a human typed,
-- and this tree has decided a security outcome on an ambiguous lookup resolved by an unordered LIMIT 1
-- more than once (E19 T1, E23) — a name collision is refused at INSERT rather than left to whichever
-- row a later query happens to pick first.
CREATE TABLE IF NOT EXISTS integration_bots (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    project_id TEXT NOT NULL REFERENCES projects (id),
    name TEXT NOT NULL,
    kind TEXT NOT NULL,
    agent_revision_id TEXT NOT NULL DEFAULT '',
    repository_binding_id TEXT NOT NULL DEFAULT '',
    principal_id TEXT NOT NULL DEFAULT '',
    config JSON NOT NULL DEFAULT '{}',
    disabled BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (organization_id, project_id, name)
);

CALL palai_apply_tenant_policy('integration_bots', 'organization_id', true);

-- This table is created AFTER 000029's blanket `GRANT ... ON ALL TABLES`, which re-runs every boot but
-- ran BEFORE this file on the boot that creates it, so it needs its own grant or the runtime role fails
-- closed with "permission denied for table integration_bots" instead of with the row-scoped policy.
GRANT SELECT, INSERT, UPDATE, DELETE ON integration_bots TO palai_app;

INSERT INTO schema_migrations (version) VALUES (61) ON CONFLICT DO NOTHING;
