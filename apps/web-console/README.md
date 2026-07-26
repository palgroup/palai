# @palai/web-console — open-core console (E17 T10)

A Next.js admin + live-run console over the Palai **public API only** (§47.6). The API key lives solely in
the server-side relay (`app/api/palai/**`) and never reaches the browser — the same server-relay stance as
`examples/nextjs-sdk` (the E13/E16 decision).

## Surfaces

- **Admin (§47.1, `/`):** organizations, projects, API keys, model connections/routes, secret-ref
  METADATA (the value is never shown), agent revisions + diff, knowledge bases. Every panel fetches a
  `/v1` list through the same-origin relay.
- **Live (§47.2, `/runs`):** start a run and watch its canonical event timeline sorted into lanes
  (messages / model steps / tool+subagent / approvals / usage / recovery+attempt / terminal). The
  **exact approval UI** shows the authoritative **operation / branch / request_hash** — the detail the
  canonical `approval.requested.v1` event actually carries (`packages/coordinator/publication.go`) — and
  renders the proposal-supplied `display` string in a separate, explicitly non-authoritative region that
  never replaces it. Recovery/attempt transitions and artifact download included.

### Public-API GAP: richer approval detail

The `approval.requested.v1` event carries only `{publication_id, operation, branch, request_hash, display}`,
and there is **no `/v1/publications` or approvals READ endpoint** — so the event payload is all a real client
receives. Richer per-approval detail the exact-approval UI would ideally show — `action`, `args`, `diff`,
`destination`, `risk`, `expiry` — is **not on the public API today**. This is a named public-API GAP (the
same shape as the E13-T10 modelRoutes write-only gap), E18/hardening input, **not a console defect**: the
console renders those fields the moment the event (or a publications read endpoint) provides them. Until then
it honestly shows only what the API emits, and the proposal `display` string never stands in for it.

## Public-API-only relay

The browser talks ONLY to `/api/palai/*`:

- `app/api/palai/v1/[...path]/route.ts` — the one generic data relay. It reconstructs the upstream path
  from the browser URL, so the ONLY thing the browser can address is a `/v1/*` public-API route; it
  forwards through `@palai/sdk` (server-side Bearer) and streams artifact bytes.
- `app/api/palai/stream/route.ts` — starts a run and re-projects the canonical SSE event stream to the
  browser as ndjson (lane-tagged), staying open across an approval pause.

## Develop

```
pnpm install                # from the repo root (workspace)
PALAI_BASE_URL=http://127.0.0.1:8080 PALAI_API_KEY=palai-sk-... pnpm --filter @palai/web-console dev
```

## Prove

```
pnpm --filter @palai/web-console typecheck
pnpm --filter @palai/web-console test:e2e   # next build + playwright: public-API-only + axe + keyboard + UI-002
```

### Two profiles, one spec set (E19 T7)

`PALAI_CONSOLE_PROFILE` selects the `/v1` upstream. **Both profiles run the same spec files** — a spec that
can only pass on one of them is a *finding*, recorded in `tests/divergences.mjs`, never a reason to fork.

- **`fake`** (default) — the deterministic upstream (`tests/fake-control-plane.mjs`): fast, Docker-free, and
  the only place a hostile artifact, a scripted recovery or a paused approval can be produced on demand.
- **`real`** — a running compose control plane. The bootstrap key comes from the **environment**, never argv:

```
PALAI_DISPATCH_WORKERS=1 PALAI_MODEL_PROVIDER=fake palai local up
export PALAI_BASE_URL="$(jq -r .base_url "$PALAI_HOME/config.json")"
export PALAI_API_KEY="$(cat "$PALAI_HOME/api-key")"
pnpm --filter @palai/web-console test:e2e:real
pnpm --filter @palai/web-console sweep       # the fake-vs-real conformance sweep
```

Asking for the real profile without a real stack **fails the whole run** at config load. It never skips: a
suite that reports green for the absence of the thing it tests is worse than no suite.

### The conformance sweep (`pnpm sweep`, plan D15)

`tests/conformance.test.mjs` (node:test — no browser) diffs the objects the fixture *dispatches from* against
the surface it gathers from the **running real router**: route existence via `OPTIONS` (Go's ServeMux answers
405+`Allow` for a registered path, bare 404 for an unregistered one, so route-absence never masquerades as
resource-absence), response shapes and statuses, and the event vocabulary and payload keys of a **real run**.
Every difference must sit in `tests/divergences.mjs`; a row that *stops* being a difference fails too, so the
ledger cannot rot in either direction. It found that E17 T10's "invented approval event" is one instance of
six: five event types the fixture scripts are journaled by no production code, and three routes it serves are
not registered by the real router at all.

A **deployed** console (compose is not a deployment) plus a manual VoiceOver/screen-reader pass remains the
§6 operator leg above the automated a11y ceiling.

## Honest ceiling

Not a commercial SaaS UI (§5): no billing/team management. The operator console (cordon/drain/queue-inspect,
§47.4) is out — the E15 ops CLIs cover it. Config explainability is minimal (effective value only;
layer-by-layer attribution is a later iteration, §47.3). `console` is advertised **preview** in
`/v1/capabilities`; only the T11 exit gate may recompute it to stable.
