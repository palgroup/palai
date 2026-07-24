# Changelog — palai (Python)

All notable changes to the Palai Python SDK. This SDK is part of the E16 three-language SDK line;
versions move together across TS / Python / Go (see `docs/operations/sdk-compatibility.md`).

## 0.1.0 — E16 SDK surface

Target API-Version: **2026-07-16**.

### Added
- Typed `/v1` client, **sync and async from one code base** (httpx — the single new dependency;
  no provider SDK, plain HTTP+SSE). Resources match the TS surface: `responses`, `sessions`,
  `agents`, `artifacts`, `reads`, `provisioning`, `secret_refs`, `model_routes`.
- Resumable SSE streaming: a sync iterator and an async iterator over the same wire request.
- Transport-level retry with a stable `Idempotency-Key` (single retry owner).
- RFC 9457 typed errors, including the 410 retention tombstone (API-015).
- Forward-compatible decoding: unknown fields preserved.
- `Page` vs `ListView` envelope decode.
- `palai.webhook.verify` — webhook signature verification with rotation/skew tolerance (API-014).

### Conformance
Validated against the shared cross-language corpus (`tests/conformance/sdk/`) via
`TestCorpusPythonRunnerEquality`: **all six categories** — request-encode, event-decode, error-map,
**signature-verify**, unknown-field, envelope-decode.

### Honest ceiling
- **Server-side SDK.** No browser story is claimed (README states this positively).
- One HTTP library only (httpx carries both transports); no second HTTP dependency.
- **Not published.** Built + checksummed + signed locally (`scripts/release/sdk-package.sh`).
  PyPI publish is E18.
