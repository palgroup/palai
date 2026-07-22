# @palai/sdk — Next.js server-relay example

A minimal Next.js app that demonstrates the **server-only** stance of `@palai/sdk`: the API key
lives on the server, and the browser talks only to Route Handlers that relay curated data. This is
the positive demonstration of why Palai **dropped** browser-direct tokens — see the full stance in
[`sdks/typescript/README.md`](../../sdks/typescript/README.md).

The credential (`PALAI_API_KEY`) is read in `lib/palai.ts`, guarded by the framework's
`server-only` package **and** the SDK's own runtime guard. It appears in **no** browser surface —
the e2e test (`tests/live.spec.ts`) scans every browser artifact for the key sentinel.

## Route Handlers (all server-side; the key never leaves them)

- `POST /api/palai` — create a response and stream its canonical events to the browser as NDJSON.
- `POST /api/palai/steer` — relay a durable **steer**/**interrupt** command to a session
  (`{ sessionId, message, mode? }`). E08's steering product, driven from the SDK.
- `GET /api/palai/artifacts?id=…` — relay an authenticated **artifact download**: the SDK opens
  the byte stream with the server-side key and the handler pipes the bytes straight through, with
  the `Content-Digest` header for integrity. The browser gets the object, never the credential.

## Run

```sh
cp .env.example .env.local   # set PALAI_API_KEY + PALAI_BASE_URL (server-side only)
npm run dev                  # http://localhost:3100
```

`PALAI_API_KEY` must never carry a `NEXT_PUBLIC_` prefix — that would ship it to the browser,
exactly what the server-relay pattern exists to prevent.
