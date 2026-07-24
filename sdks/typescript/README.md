# @palai/sdk

A thin, typed TypeScript client for the Palai control-plane HTTP API. It speaks the dated
`/v1` contract (the `API-Version` header rides every request), maps failures to typed
RFC 9457 problem errors, and retries idempotently.

## Server-only credential stance (why browser-direct tokens were dropped)

**The API key is server-side only. There is no browser-direct token, by design.**

Palai deliberately **dropped** the idea of issuing a short-lived, browser-scoped token that a
browser would use to call the control-plane directly. The correct and supported pattern is a
**server relay**: your server holds the API key, calls Palai with this SDK, and re-projects only
the curated fields your UI needs. The browser talks to *your* server route, never to Palai.

Why the relay is the right pattern (not a limitation):

- **The credential never reaches the client.** A browser-direct token is still a bearer
  credential shipped to every visitor; a relay keeps the only credential on the server.
- **You control the projection.** The relay forwards curated canonical fields — never the raw
  provider payload, never the key. See `examples/nextjs-sdk`.
- **No CORS / token-exchange surface to secure.** There is nothing browser-facing to scope,
  rotate, or leak.

How the stance is **enforced**, not just documented:

1. **Two entrypoints.** The package `browser` export condition resolves to
   `./src/index.browser.ts`, which exports the typed shapes and the RFC 9457 error surface —
   **everything a browser needs to render a response or narrow an error** — and **no** API-key
   client. The default (server) condition resolves to `./src/index.ts`, which exports the `Palai`
   client and every resource.
2. **A runtime backstop.** The `Palai` client's module chain imports `./src/server-only.ts`,
   which **throws** if it is ever evaluated in a browser (a `window`/`document` global is
   present) — so a misconfigured bundler that ignores the export condition fails loud instead of
   silently shipping the secret.
3. **Every new resource routes through that client.** Sessions, agents, artifacts, the read/LIST
   surfaces, and the admin clients (secret-refs, model-routes, tenancy provisioning) are all
   reachable **only** from the server entrypoint. None is re-exported from the browser
   entrypoint. This is the positive enforcement of the drop decision; the SDK test suite asserts
   the browser entrypoint exposes no credentialed client.

In Next.js, layer the framework's own `server-only` package on top (as `examples/nextjs-sdk`
does) for a build-time error the moment a Client Component imports the credential path.

```ts
// server code only (route handler, server action, server component)
import { Palai } from "@palai/sdk";
const palai = new Palai({ apiKey: process.env.PALAI_API_KEY });
```

```ts
// browser code — types and error narrowing only, no client, no key
import { PalaiAPIError, type Response } from "@palai/sdk/browser";
```

## Resources

All resources hang off the server-only `Palai` client:

- `responses` — create / retrieve / cancel / **stream** (resumable SSE) / **list** (run history)
- `sessions` — create / retrieve / list, and `sessions.commands.steer` / `.interrupt`
  (durable commands)
- `agents` — list / retrieve / listRevisions / publishRevision
- `artifacts` — retrieve (metadata) / **download** (authenticated byte stream + Content-Digest) /
  listForResponse
- read/LIST surfaces — `repositoryBindings`, `tools` (+ `listSets`), `mcpConnections`, `triggers`,
  all over the shared opaque, tenant-bound cursor (`after` + `limit`)
- admin — `secretRefs` (write-only value, metadata reads, rotate), `modelRoutes`
  (connections / routes / revisions / publish), and tenancy provisioning
  (`organizations` / `projects` / `apiKeys`)
- `Orchestrator` — the external-orchestrator kit: the §35.1 five-step contract
  (`start` / `waitByPoll` / `waitByStream` / `sendMessage` / `cancel` / `reconcile` / `runActivity`),
  keeping the external workflow id and Palai's canonical run id separate. See
  [docs/orchestrator-kit.md](docs/orchestrator-kit.md) for who owns which timeout/retry (§35.2).

Lists page with an opaque `after` cursor and a `limit`; the SDK passes the server's
`next_cursor` straight back — it never parses the cursor (it is tenant-bound and server-minted).

## Development

```sh
npm run typecheck   # tsc, no emit
npm test            # node --test (native, no framework)
```
