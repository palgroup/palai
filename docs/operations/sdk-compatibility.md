# Palai SDK compatibility matrix

Which SDK version supports which capability at which API-Version. **Only tested cells are filled.**
A cell is `✓` (supported) **only if a real conformance test proves it**; an untested/unsupported
capability is left `—` and is never claimed.

The authoritative, machine-readable matrix is
[`sdk-compatibility.json`](./sdk-compatibility.json). The table below is rendered from it. The
**honest-matrix guard** — `TestSDKCompatibilityMatrixHonest` in
`tests/conformance/sdk/compat_matrix_test.go` — recomputes each SDK's covered capabilities by
running that SDK's conformance runner against the shared corpus and **fails** if the JSON claims a
capability the runner does not actually cover-and-match (or omits one it does). So the matrix cannot
drift into an untested claim without turning the suite red.

## Server API-Version: `2026-07-16`

The single dated API version this server build serves
(`apps/control-plane/api/middleware/request_context.go`). The SDKs target it; a future dated version
adds a column, filled only when a corpus proof exists for it.

| SDK | Version | request-encode | event-decode | error-map (incl. 410) | signature-verify (webhook) | unknown-field | envelope-decode |
|---|---|:---:|:---:|:---:|:---:|:---:|:---:|
| `@palai/sdk` (TypeScript) | 0.1.0 | ✓ | ✓ | ✓ | — | ✓ | ✓ |
| `palai` (Python) | 0.1.0 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `github.com/palgroup/palai/sdks/go` | 0.1.0 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |

### Capabilities (the columns)

Each column is a `tests/conformance/sdk/corpus/<capability>.json` category — a decode/encode surface
the SDKs must agree on. They are proven mechanically by the cross-language equality harness
(`harness_test.go`), where each SDK's normalized runner output is canonical-bytes-diffed against the
corpus's expected output.

- **request-encode** — a logical SDK call → the exact wire request (method/path/idempotency/body).
- **event-decode** — a raw SSE transcript → the decoded event sequence + first terminal index.
- **error-map** — a wire error → the typed error projection, including the 410 retention tombstone (API-015).
- **signature-verify** — webhook signature verify with rotation/skew tolerance (API-014).
- **unknown-field** — forward-compat: an unknown field is preserved, not stripped.
- **envelope-decode** — the `Page` (cursor) vs `ListView` (`object:"list"` admin) distinction (E16 T1).

### The one honest `—`

The TypeScript SDK ships **no webhook signature verify** — it keeps the E13 server-relay/browser
stance, so `signature-verify` is a server-side concern for that SDK. The harness deliberately omits
that category for the TS runner (`tsCovers` in `harness_test.go`); the matrix reflects that as `—`,
not a false `✓`. Use the Python or Go SDK server-side for webhook verification.

### Proof map (which test fills each row)

| SDK row | Conformance test |
|---|---|
| TypeScript | `TestCorpusTypeScriptRunnerEquality` (5 of 6 categories) |
| Python | `TestCorpusPythonRunnerEquality` (all 6) |
| Go | `TestCorpusGoRunnerEquality` (all 6) |

Run them: `go test ./tests/conformance/sdk/ -v` (needs `node`, `uv`, `go` for the three runner legs;
opt out a missing toolchain with `PALAI_SDK_CONFORMANCE_ALLOW_NO_NODE=1` / `..._NO_UV=1`).

## Honest ceiling

- The matrix asserts **decoded-output equality on the shipped corpus vectors** at API-Version
  `2026-07-16` — it is not a fuzzer and not a live-server test.
- The "three languages semantically equal" end-to-end claim (journey 63.1) is made by the **E16 T8**
  exit gate, not here; this matrix is the per-capability provenance half.
- **No publish.** SDK packages are built + checksummed + signed locally
  (`scripts/release/sdk-package.sh`); npm/PyPI/Go-proxy publish + SBOM/provenance attestation is E18.
