# palai (Python SDK)

A thin, typed Python client for the Palai control-plane HTTP API, in **both a synchronous and an
asynchronous flavor**. It speaks the dated `/v1` contract (the `API-Version` header rides every
request), maps failures to typed RFC 9457 problem errors, retries idempotently, and streams a run's
events over resumable SSE. It is the Python leg at parity with the TypeScript SDK — the same shapes,
the same wire, proven by the shared cross-language conformance corpus (`tests/conformance/sdk/`).

## Server-side by design (there is no browser story — and that is the point)

**Python is a server-side language, and this SDK is a server-side client.** The API key lives on your
server; there is no browser-direct token, by design (the same stance the TypeScript SDK enforces with
two entrypoints). The correct pattern is a **server relay**: your server holds the key, calls Palai
with this SDK, and re-projects only the curated fields your UI needs. The browser talks to *your*
server, never to Palai.

Why this is right, not a limitation:

- **The credential never reaches a client.** A browser-direct token is still a bearer credential
  shipped to every visitor; a server relay keeps the only credential on the server.
- **You control the projection.** The relay forwards curated canonical fields — never the raw provider
  payload, never the key.
- **No CORS / token-exchange surface to secure.** There is nothing browser-facing to scope or leak.

There is nothing to disable and no browser entrypoint to avoid: importing `palai` gives you the
credentialed client, and that client is meant to run only where your API key already lives.

## Install / develop

The package is a `uv` project (no PyPI publish — that is E18 supply-chain work).

```sh
uv sync --locked            # install httpx + the package (editable)
uv run pytest               # the unit suite (native pytest, no extra frameworks)
```

```python
from palai import Palai

client = Palai(api_key="...")            # or set PALAI_API_KEY / PALAI_BASE_URL
resp = client.responses.create({"input": "Summarize the onboarding guide.", "model": "fake-1"})
final = client.responses.stream({"input": "Stream a haiku."}).final_response()
client.close()                            # or use `with Palai(...) as client:`
```

```python
import asyncio
from palai import AsyncPalai

async def main():
    async with AsyncPalai(api_key="...") as client:
        resp = await client.responses.create({"input": "hi"})
        async for event in client.responses.stream({"input": "stream me"}):
            print(event["type"])

asyncio.run(main())
```

## Resources

Every resource hangs off the client (sync `Palai` / async `AsyncPalai` — identical surface, the
async methods are awaitable):

- `responses` — `create` / `retrieve` / `cancel` / `stream` (resumable SSE) / `list` (run history)
- `sessions` — `create` / `retrieve` / `list`, and `sessions.commands.steer` / `.interrupt`
- `agents` — `list` / `retrieve` / `list_revisions` / `publish_revision`
- `artifacts` — `retrieve` (metadata) / `download` (authenticated byte stream + `Content-Digest`) /
  `list_for_response`
- read/LIST surfaces — `repository_bindings`, `tools` (+ `list_sets`), `mcp_connections`, `triggers`,
  all over the shared opaque, tenant-bound cursor (`after` + `limit`)
- admin — `secret_refs` (write-only value, metadata reads, rotate), `model_routes` (connections /
  routes / revisions / publish **and read-back** — E16 T1), and tenancy provisioning
  (`organizations` / `projects` / `api_keys`)
- `palai.webhook` — inbound webhook signature `verify` / `sign` (§21.5, API-014)

Lists page with an opaque `after` cursor and a `limit`; the SDK passes the server's `next_cursor`
straight back — it never parses the cursor (it is tenant-bound and server-minted). Decoded bodies are
plain dicts, so unknown fields a newer server adds survive a round-trip (the open-world stance,
API-009).

## Retry & idempotency

There is a **single retry owner**: the SDK's own transport (no hidden provider-SDK retry — this SDK
ships no provider SDK, just plain HTTP + SSE over `httpx`). A retryable failure (a request timeout, a
rate limit, or a 5xx) is retried with full-jitter backoff, bounded by `max_retries` and the total
`timeout_ms` deadline. A create carries a stable `Idempotency-Key` minted once and reused across every
retry, so a create retried after a network blip settles exactly one response. A non-idempotent POST
with no key is **not** re-sent after a network drop — it fails closed rather than risk a double-commit.

## Honest ceilings

- **Sync + async are httpx's two transports** — no second HTTP library is added (the one-dependency
  ceiling, plan §2). Cancellation of an in-flight call is available on the async client via ordinary
  task cancellation; the sync client exposes no cancel token (idiomatic Python would block anyway).
- **Typed return values are plain dicts**, not generated model classes: the "generated types are the
  single source" invariant forbids a hand-copied Python type surface, and a Python emitter for
  `make generate` is a follow-up. Method docstrings name each shape; unknown fields are preserved.
- **No PyPI publish** — packaging/provenance is T7, publish is E18.
