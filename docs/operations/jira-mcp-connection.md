# Connecting Jira — the Atlassian Rovo MCP server

Jira reaches a Palai agent as an **MCP connection**: an admin-registered upstream MCP server whose tools,
once approved, are advertised to a run like any other tool. Palai has **no Jira client** and does not want
one — Atlassian publishes and operates the server, we connect to it.

Everything below uses machinery that already existed (E12 T5/T6): SSRF-vetted egress, request-time secret
resolution, a per-connection circuit breaker, and the approval gate that keeps an untrusted tool description
out of a model's context until an admin publishes it.

---

## 1. What you need

| | |
|---|---|
| An Atlassian Cloud site with Jira | the account must be able to see the project you care about |
| An API token | https://id.atlassian.com/manage-profile/security/api-tokens |
| **API-token auth enabled for Rovo MCP by an org admin** | without it the endpoint only accepts interactive OAuth 2.1 |

That third row is the one that bites. See §5.

## 2. Build the credential

The Authorization scheme depends on **which kind of token you have**:

| Token | Header |
|---|---|
| Personal API token | `Basic base64(email:api_token)` |
| Service-account API key | `Bearer <api_key>` |

Source: https://support.atlassian.com/atlassian-rovo-mcp-server/docs/configuring-authentication-via-api-token/
(fetched 2026-07-27).

For a personal token:

```sh
printf 'you@example.com:YOUR_API_TOKEN' | base64
```

Store the **whole header value, scheme included**. The value is read from stdin so it never enters `argv`,
shell history, or a log:

```sh
printf %s 'Basic <the base64 from above>' | palai secret create --name jira-api-token
```

The connection's `secret_ref` is that **name**. It resolves through the DB-backed secret store, which needs a
master key configured on the stack. Without one, the resolver falls back to an env-file bridge — the env var
holds a **file path**, never the secret inline:

```sh
PALAI_MCP_SECRET_FILE_<ORG>__JIRA_API_TOKEN=/run/secrets/jira-api-token
```

> Palai sends a secret that names its own scheme verbatim (case-insensitively) and defaults a bare secret to
> `Bearer`. Storing the scheme with the credential is what lets one connection type serve both token kinds.
> A trailing newline in a secret file is trimmed, so a file written by `echo` will not corrupt the header.

## 3. Register, discover, approve, grant

Five calls. `discover` is the only one that touches Atlassian.

```sh
# (a) Register the connection. The credential is a HANDLE — never inline.
curl -X POST "$PALAI_BASE_URL/v1/mcp-connections" -H "Authorization: Bearer $PALAI_API_KEY" \
  -d '{"name":"jira","transport":"http",
       "config":{"url":"https://mcp.atlassian.com/v1/mcp"},
       "secret_ref":"jira-api-token"}'
# → {"id":"mcpc_...", ...}

# (b) Discover its tools. Each becomes a DRAFT revision named mcp.jira.<tool>,
#     model-visible as jira__<tool>. Nothing is advertised yet.
curl -X POST "$PALAI_BASE_URL/v1/mcp-connections/$CONN_ID/discover" -H "Authorization: Bearer $PALAI_API_KEY"

# (c) Approve the tools you want. Find the ids with GET /v1/tools, then publish each revision.
curl -X POST "$PALAI_BASE_URL/v1/tools/$TOOL_ID/revisions/$REV_ID/publish" -H "Authorization: Bearer $PALAI_API_KEY"

# (d) Pin the approved revisions into a tool set, and publish the set.
curl -X POST "$PALAI_BASE_URL/v1/tool-sets/jira/revisions" -H "Authorization: Bearer $PALAI_API_KEY" \
  -d '{"tools":[{"tool_revision_id":"'"$REV_ID"'"}]}'
curl -X POST "$PALAI_BASE_URL/v1/tool-sets/jira/revisions/$SET_REV_ID/publish" -H "Authorization: Bearer $PALAI_API_KEY"

# (e) Grant them to an agent revision. BOTH fields are required:
#     tool_sets grants the tools, mcp_connections is the capability ceiling.
curl -X POST "$PALAI_BASE_URL/v1/agents/$AGENT_ID/revisions" -H "Authorization: Bearer $PALAI_API_KEY" \
  -d '{"model":"...","tool_sets":["'"$SET_REV_ID"'"],"mcp_connections":["'"$CONN_ID"'"]}'
```

Then publish the agent revision and start a run with `agent_revision_id` set. The tool is advertised to the
model, which calls it by its model-visible name (`jira__getJiraIssue`).

**You do not need to write a `config_policy`.** A fresh project's is NULL, and the tool-set grant unions onto
that empty baseline. (Guarded by `TestResolveGrantsToolSetsOnANullProjectBaseline`, because the failure mode
otherwise is silent: a model that simply never calls the tool.)

## 4. Both fields in step (e), or nothing happens

`tool_sets` and `mcp_connections` do different jobs and the failure is quiet:

- **`tool_sets` missing** → the tool is not in the run's effective set, so it is never advertised.
- **`mcp_connections` missing** → the tool resolves to nothing (`ErrUnknownTool`) even if advertised. The
  rider is the capability ceiling: a run may only reach connections its revision names.

## 5. When it doesn't work

**Symptom: the agent behaves as if Jira has no tools, and `discover` returns a handful of
`...TeamworkGraph...` tools with nothing Jira-shaped.**

This is an **unaccepted credential**. Observed against the live endpoint on 2026-07-27: a credential the
server does not accept does **not** produce a 401. `initialize` and `tools/list` both succeed and you are
silently dropped to a 3-tool anonymous set. Check, in order:

1. The scheme — a personal API token is `Basic`, not `Bearer` (§2).
2. That an **org admin has enabled API-token auth** for the Rovo MCP server. This is the most common cause.
3. That the token has not expired or been revoked.
4. That the account can actually see a Jira project.

**Symptom: `discover` fails with `http status 401`.** No `Authorization` header reached the server — the
`secret_ref` does not resolve. Confirm `palai secret list` shows the name you registered.

**Symptom: `discover` fails with `mcp: protocol error: server protocol "..." != "2025-11-25"`.** Atlassian
moved protocol versions. Our client negotiates exactly one and disconnects on anything else; widening the
accepted set in `adapters/integrations/mcp/client.go` is the fix, as the tools subset we use is stable across
the 2025 revisions.

Diagnose any of these without touching Palai's database by running the live leg, which dials the real server
read-only and prints what it found:

```sh
PALAI_JIRA_MCP_CREDENTIAL='Basic ...' go test -tags=live -run TestLiveJiraMCP -v ./adapters/integrations/mcp/
```

## 6. What the connection can and cannot do

- **The server's output is untrusted data.** It cannot advertise a tool, widen the run's effective set, or
  make anything outside the rider callable — the same rule E17 T3 established for remote A2A results. A Jira
  ticket whose description was written by an attacker reaches the model only as the result of the one tool
  you approved. Proven by `TestJiraMCPServerOutputCannotGrantCapability`.
- **The credential never reaches the model, argv, a log, or the connection row.** It is resolved from the
  handle at request time and used only as the Authorization header.
- **Egress is vetted** and redirects are denied outright (MCP is stricter than A2A here, which revalidates).
- **Re-discovery does not silently change what the model sees.** A changed tool description creates a new
  draft; the published revision stays published until an admin approves the new one.
- **Tenant scope comes from the connection row**, never from anything the server returns.

## 7. Contract divergences (published docs × this tree)

Recorded in the §3.5 style. Every row was checked against a primary source on the date shown.

| # | Published contract (source) | State in tree | Disposition |
|---|---|---|---|
| **J1** | A personal API token authenticates as `Basic base64(email:token)`; only a service-account key is `Bearer`. (https://support.atlassian.com/atlassian-rovo-mcp-server/docs/configuring-authentication-via-api-token/, fetched 2026-07-27) | `http.go` hardcoded `"Bearer "+secret`, so a Basic credential went out as `Bearer Basic ...`. **The one genuine code gap.** | **CLOSED** — the secret carries its own scheme; RED-verified, fails at discovery with `http status 401` without the fix |
| **J2** | Endpoint is `https://mcp.atlassian.com/v1/mcp`; docs also show `https://mcp.atlassian.com/v1/mcp/authv2` for the OAuth/IDE flow. Transport is `"type": "http"`. (same source; https://support.atlassian.com/rovo/docs/setting-up-ides/, fetched 2026-07-27) | Our HTTP transport is MCP Streamable HTTP (POST per message, `Accept: application/json, text/event-stream`, SSE responses parsed) — **compatible** | No change. `/v1/mcp` is the documented API-token endpoint and the doc's default |
| **J3** | Version negotiation: server responds with the same version if it supports it, else another; client SHOULD disconnect if it cannot. (https://modelcontextprotocol.io/specification/2025-06-18/basic/lifecycle, fetched 2026-07-27) | Our client accepts exactly `2025-11-25` and disconnects otherwise — spec-conformant but single-version | **VERIFIED LIVE 2026-07-27**: `initialize` against the real endpoint succeeded, so the real server negotiates `2025-11-25` today. No change; §5 names the fix if that moves |
| **J4** | Atlassian tool names are camelCase (`getJiraIssue`, `searchJiraIssuesUsingJql`). (https://support.atlassian.com/atlassian-rovo-mcp-server/, fetched 2026-07-27) | `validateCanonicalName`/`isASCIIName` accept `A-Z`, so `mcp.jira.getJiraIssue` is valid — **no divergence** | Pinned by a component assertion, since a rejection here would silently drop every Atlassian tool at discovery |
| **J5** | — (observed behaviour, no published statement found) | An unaccepted credential does **not** 401 on `initialize`/`tools/list`; the server degrades to a 3-tool anonymous set (`getTeamworkGraphContext`, `getTeamworkGraphObject`, `addTeamworkGraphContext`). A request with **no** `Authorization` header at all does 401. | **UNCONFIRMED by any Atlassian document** — measured directly on 2026-07-27, labelled as an observation. It is why the live leg asserts a Jira tool is present rather than that the call merely succeeded |
| **J6** | OAuth 2.1 with dynamic client registration and browser consent is the primary auth path. (https://support.atlassian.com/atlassian-rovo-mcp-server/docs/configuring-oauth-2-1/, fetched 2026-07-27) | Palai has **no OAuth authorization-code flow** for MCP connections. `validateConnectionConfig` accepts and passively validates an `oauth` config block, but nothing performs the flow | **NOT CLOSED — out of scope for this task.** API-token auth is the supported path today. Wiring interactive OAuth is an epic (browser consent, token storage, refresh), not a task |

---

## Proofs

| Claim | Where |
|---|---|
| The whole chain — register → discover → approve → pin → grant → advertise → call — against a fake MCP server built to the published protocol, driving the **real** manager over real TLS | `apps/control-plane/internal/extensions/mcp_jira_component_test.go` (`TestJiraMCPConnectionEndToEnd`) |
| An MCP server's output grants no capability | same file (`TestJiraMCPServerOutputCannotGrantCapability`) |
| The Authorization scheme comes from the secret, asserted on the bytes the server received | `adapters/integrations/mcp/http_auth_test.go` |
| A NULL `config_policy` does not block advertisement | `apps/control-plane/internal/execution/config_test.go` (`TestResolveGrantsToolSetsOnANullProjectBaseline`) |
| The real Atlassian server is reachable and enumerable with a real credential | `adapters/integrations/mcp/jira_live_test.go` — credential-gated, skips with setup instructions |
