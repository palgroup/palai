# @palai/sdk (Go) — `github.com/palgroup/palai/sdks/go`

The **server-side** Go SDK for the Palai control plane. It is at parity with the TypeScript SDK's
public surface and is proven identical, vector-for-vector, by the shared cross-language conformance
corpus (`tests/conformance/sdk`, E16 T2/T4, API-012).

## Server-side by design (a positive stance, not a limitation)

This SDK holds a Palai API key, so it is built for **trusted server processes** — a backend, a job
runner, a webhook receiver. There is deliberately **no browser entrypoint and no browser-direct
token**: that separation is the platform's server-relay decision (E13), and Go is a server language,
so the browser story is simply not this SDK's job. A browser talks to *your* server; your server
talks to Palai through this SDK.

It is **stdlib-only** — `net/http` + `bufio` SSE + `encoding/json`, no third-party dependency — and
its own Go module, so it never imports the monorepo's internal packages and stays independently
movable.

## Install

```go
import palai "github.com/palgroup/palai/sdks/go"
```

## Quick start

```go
client, err := palai.New(palai.WithAPIKey(os.Getenv("PALAI_API_KEY")))
if err != nil { log.Fatal(err) }

// Create — a stable Idempotency-Key is minted once and reused across transport retries, so a
// retried create settles exactly one response.
resp, err := client.Responses.Create(ctx, palai.ResponseCreateRequest{
    Input: "Summarize the onboarding guide in three bullets.",
    Model: "fake-1",
})

// Stream — a resumable, typed event stream; reconnects from Last-Event-ID on a drop.
stream, err := client.Responses.Stream(ctx, palai.ResponseCreateRequest{Input: "..."})
for event, err := range stream.Events(ctx) {
    if err != nil { /* typed *palai.APIError or *palai.ConnectionError */ }
    // unknown event types are DELIVERED, not dropped (forward-compat, API-009)
}
final, err := stream.FinalResponse(ctx)

// Read back
one, err := client.Responses.Retrieve(ctx, "resp_123")
page, err := client.Responses.List(ctx, palai.ListParams{Limit: 20})

// Model-routing admin (write + E16 T1 read-back; needs a key with `provision`)
routes, err := client.ModelRoutes.ListRoutes(ctx)     // ListView envelope (full, small, no cursor)
```

## What's in the box

- **Client** — functional options (`WithBaseURL`, `WithAPIKey`, `WithHTTPClient`, `WithMaxRetries`,
  `WithTimeout`, `WithBackoff`), context-first, Bearer auth, the dated `API-Version`.
- **Idempotent retry** — a retryable status (408/429/5xx) or a network blip retries within budget
  with the SAME `Idempotency-Key` (full-jitter backoff, `Retry-After` honored). A non-idempotent
  mutation fails **closed** on a torn connection rather than risking a double-commit.
- **Typed RFC 9457 errors** — `*APIError` with `.Class()` (`InvalidRequestError`, `NotFoundError`,
  `GoneError`, …), `.Code`, `.Status`, `.RequestID`, `.Retryable()`; the stable code set stays open,
  so an unknown code from a newer server is preserved, not rejected. `*ConnectionError` for a
  transport failure before any status.
- **Forward-compatible decode** — `Response`, `Event`, `Problem`, and the model types keep typed
  fields for ergonomics **and** preserve any unknown field a newer server adds (round-trips
  losslessly). A naive struct decode would strip those; this doesn't.
- **Two list envelopes** — `Page[T]` (cursor data-plane) vs `ListView[T]` (un-paginated admin), kept
  distinct, never conflated.
- **Webhook verify** — `VerifyWebhook` / `SignWebhook`: constant-time HMAC-SHA-256 over the exact raw
  body, the replay-window tolerance, and rotation-overlap (multiple `v1=` values), for a Go server
  receiving Palai's tool-call callbacks (API-014).

## Conformance

The SDK's decode is not "the same as TypeScript by assertion" — it is proven so **mechanically**:
`sdks/go/runner` feeds the shared corpus through this SDK's real surface and the harness
canonical-bytes-diffs its output against the reference, the TypeScript runner, and (T3) the Python
runner. A divergence FAILS. Run it: `go test ./tests/conformance/sdk/ -run Go`.

## Ceilings (v0)

- Minimal ergonomics by design (a simple options pattern; richer helpers as demand appears).
- `Stream` fires its create eagerly (the wire contract is identical to the lazy TS form).
- Package publishing (a Go module proxy tag) is E18 supply-chain work, not this SDK.
