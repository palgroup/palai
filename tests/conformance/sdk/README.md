# SDK conformance corpus + mechanical cross-language equality harness (E16 T2, API-012)

One shared, language-agnostic fixture corpus proves the Palai SDKs decode identically. The
equality is checked in **one place** (`harness_test.go`) — no SDK asserts "same as TypeScript"
in its own suite (design invariant, plan §2). A vector whose decoded output diverges FAILS
mechanically: the harness canonical-bytes-diffs each runner's normalized output against the
corpus's expected output.

## Layout

- `corpus/<category>.json` — the fixtures. Every file is `{"vectors":[{name, input, expected}]}`.
  `input` is language-agnostic data; `expected` is the **normalized decoded output** every SDK
  must produce. The category is the filename.
- `harness_test.go` — the harness. It runs two independent implementations against the corpus:
  1. a Go **reference decode** using the server's own `packages/contracts` types +
     `webhook.Verify` (validates every vector, in-process);
  2. each registered language **runner** (the TS SDK now; Python/Go in T3/T4), driven over the
     stdin/stdout contract below.

## Categories

| Category | `input` | `expected` (normalized output) |
|---|---|---|
| `request-encode` | `{resource, method, args, options}` — a logical SDK call | `{method, path, idempotency_key?, body?}` — the exact wire request the SDK emits |
| `event-decode` | `{transcript}` — a raw SSE byte transcript | `{events:[...], terminal_index}` — decoded event sequence + index of first terminal event |
| `error-map` | `{status, body, request_id?}` — a wire error response | `{class, status, code, retryable, request_id}` — the typed error projection (incl. 410 tombstone, API-015) |
| `signature-verify` | `{secret, webhook_id, timestamp, body, signature, now, tolerance_seconds, expect_signature?}` | `{valid, signature?}` — webhook rotation verify (API-014) |
| `unknown-field` | `{value}` — an object carrying an unknown field | the same object, unknown field **preserved** (forward-compat) |
| `envelope-decode` | `{envelope}` — a `Page` or `ListView` body | `{kind:"page"\|"list", ...}` — the normalized envelope (the T1 Page/ListView distinction) |

## Runner contract (STABLE — T3/T4 register against this, no corpus change)

A runner is any executable that:

1. reads one JSON object on **stdin**:
   ```json
   { "vectors": [ { "category": "error-map", "name": "not-found-404", "input": { ... } } ] }
   ```
2. writes one JSON object on **stdout**:
   ```json
   { "outputs": [ { "category": "error-map", "name": "not-found-404", "output": { ... } } ] }
   ```
   `output` is the normalized decoded value for that vector. **Omit** any vector the SDK does not
   expose (e.g. the TS SDK ships no webhook verify, so it omits `signature-verify`); the
   reference decode still validates omitted vectors.
3. exits non-zero on internal error (writing the message to stderr).

Registering a new language is one entry in `TestCorpus…RunnerEquality` (an argv), plus the
runner executable in that SDK's tree. The corpus and the harness's compare are untouched:

```go
// T3: outputs := runExternalRunner(t, []string{python, ".../runner.py"}, all)
// T4: outputs := runExternalRunner(t, []string{"go", "run", ".../runner"}, all)
```

The TS runner lives at `sdks/typescript/test/conformance-runner.ts` and is driven as
`node --experimental-strip-types conformance-runner.ts`.

## Running

```
go test ./tests/conformance/sdk/ -v
```

`TestHarnessFailsOnDivergence` is the anti-fabrication guard: it mutates one byte of a real
expected output and asserts the diff DETECTS it — the harness cannot pass a corrupted corpus.

## Honest ceiling (T2)

- **Two implementations today**, not three: the Go reference validates all six categories; the
  **TS SDK runner** validates the five it exposes (`signature-verify` is reference-only — the TS
  SDK has no webhook verify). The **"three languages semantically equal" claim is made only by
  the T8 gate**, once the Python (T3) and Go (T4) SDK runners register against this same corpus.
- The corpus proves **decoded-output equality on the shipped vectors** — it is not a fuzzer and
  not a live-server test (fixtures are deterministic data; no provider/credential involved).
- `request-encode` bodies are omitempty-canonical (only non-zero fields), so the corpus tests
  field/nesting/path/query encoding — not zero-value omission (which the SDK does not do anyway).
- `unknown-field` and `envelope-decode` are near-identity for a structurally-typed SDK (TS); they
  become load-bearing for the struct-based Go/Python runners, where a naive decode would strip
  unknown fields or conflate the two envelopes — which this corpus is built to catch.
