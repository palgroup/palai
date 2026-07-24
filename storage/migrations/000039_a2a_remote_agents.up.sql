-- 000039 adds the A2A 1.0 CLIENT registration table (E17 Task 3, spec §38.5): a2a_remote_agents. One row is
-- a registered OUTBOUND remote A2A agent this tenant may dial as an external child-run executor or a
-- tool-like specialist. The row PINS the trust envelope the client enforces on every call: the Agent Card
-- identity/version it negotiated, the endpoint it may reach, the auth Connection HANDLE (a secret_ref — the
-- remote connection's OWN credential, NEVER the parent/platform token: A2A-005/SUB-007), the modality +
-- extension-URI allowlists, the data/cost policy, and the request TIMEOUT pin.
--
-- SECURITY posture baked into the schema:
--   * auth_connection_ref is a secret_ref HANDLE, never a bearer value — the credential is redeemed at call
--     time and is the remote connection's own, so a remote agent can never inherit the caller's credential.
--   * allowed_extension_uris is an ALLOWLIST: a remote card advertising an extension URI outside it is refused
--     by the client, never silently honored.
--   * endpoint_url / card_url are vetted through packages/egress on every retrieval (SSRF home) — the schema
--     only stores them; the client is the enforcement point.
--
-- CREATE ... IF NOT EXISTS keeps the chain re-runnable. It carries organization_id + project_id and takes the
-- standard tenant policy (M3: a new tenant-scoped table asserts its OWN policy here rather than leaning on
-- 000029's boot sweep; tests/security/tenancy fails a table that ships without ENABLE+FORCE). It was created
-- AFTER 000029's blanket GRANT, so it needs its own grant too.
CREATE TABLE IF NOT EXISTS a2a_remote_agents (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    name TEXT NOT NULL,
    -- Agent Card discovery URL + the interface base the client POSTs message:send / GETs tasks to. Both are
    -- vetted through egress on retrieval; a redirect or endpoint change forces revalidation (client-side).
    card_url TEXT NOT NULL,
    endpoint_url TEXT NOT NULL,
    -- The A2A protocol version negotiated at registration. The client fails EXPLICITLY (§38.7) if a fetched
    -- card no longer advertises this version — it never silently downgrades.
    protocol_version TEXT NOT NULL DEFAULT '1.0',
    -- The remote connection's OWN auth credential, as a secret_ref handle (000031). Redeemed at call time to
    -- the ONLY Authorization the client sends outbound. It is NEVER the parent run's or the platform's token
    -- (A2A-005/SUB-007: no credential inheritance) and never stored as a bearer value here.
    auth_connection_ref TEXT NOT NULL DEFAULT '',
    -- Modality allowlists (§38.5): the input/output media types this remote may be sent / may return. A part
    -- outside them is refused rather than forwarded.
    allowed_input_modes TEXT[] NOT NULL DEFAULT ARRAY['text/plain']::text[],
    allowed_output_modes TEXT[] NOT NULL DEFAULT ARRAY['text/plain']::text[],
    -- Extension-URI ALLOWLIST: a remote card's advertised A2A extension is honored only if its URI is listed
    -- here. Empty = no extensions allowed. This is the crown SSRF/capability guard against a malicious card.
    allowed_extension_uris TEXT[] NOT NULL DEFAULT ARRAY[]::text[],
    -- Data/cost policy pins (§38.5). data_policy names the max data class the remote may receive (default
    -- 'minimum': just the objective, no parent artifacts). max_cost_cents caps the spend a single dispatch may
    -- incur (0 = unbounded, honestly recorded).
    data_policy TEXT NOT NULL DEFAULT 'minimum',
    max_cost_cents INTEGER NOT NULL DEFAULT 0,
    -- Request timeout PIN (§38.5). The client bounds every outbound call by this; a remote that hangs past it
    -- is a terminal timeout, never an unbounded wait.
    timeout_ms INTEGER NOT NULL DEFAULT 30000,
    -- Bounds the UNTRUSTED remote output the client will accept back (A2A-005: remote output is tool-result
    -- data, and it is size-bounded like any untrusted ingress).
    max_output_bytes INTEGER NOT NULL DEFAULT 1048576,
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- List a project's registered remote agents newest-first (admin ListView envelope).
CREATE INDEX IF NOT EXISTS a2a_remote_agents_project_idx
    ON a2a_remote_agents (organization_id, project_id, created_at DESC);

-- The table asserts its own policy (M3). It carries project_id, so has_project=true; the CALL is idempotent
-- (the procedure DROPs+CREATEs the policy).
CALL palai_apply_tenant_policy('a2a_remote_agents', 'organization_id', true);

-- Created AFTER 000029's blanket `GRANT ... ON ALL TABLES`, so that sweep never saw it: a new table needs its
-- own grant or the runtime role fails closed with "permission denied" instead of the row-scoped policy.
GRANT SELECT, INSERT, UPDATE, DELETE ON a2a_remote_agents TO palai_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO palai_app;

INSERT INTO schema_migrations (version) VALUES (39) ON CONFLICT DO NOTHING;
