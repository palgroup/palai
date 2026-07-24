# Changelog — @palai/sdk (TypeScript)

All notable changes to the Palai TypeScript SDK. This SDK is part of the E16 three-language SDK
line; versions move together across TS / Python / Go (see `docs/operations/sdk-compatibility.md`).

## 0.1.0 — E16 SDK surface (unreleased)

Target API-Version: **2026-07-16**.

### Added
- Typed `/v1` client with resources: `responses`, `sessions`, `agents`, `artifacts`, `reads`,
  `provisioning`, `secretRefs`, `modelRoutes` (list/get read-back landed with E16 T1).
- Resumable SSE streaming for `responses.stream()` (shares the `create()` wire request;
  `Last-Event-ID` resume).
- Transport-level retry with a stable `Idempotency-Key` (single retry owner — no per-resource retry).
- RFC 9457 typed errors, including the 410 retention tombstone (`GoneError`, API-015).
- Forward-compatible decoding: unknown fields are preserved.
- `Page` (cursor) vs `ListView` (`object:"list"` admin) envelope decode (E16 T1 distinction).

### Conformance
Validated against the shared cross-language corpus (`tests/conformance/sdk/`) via
`TestCorpusTypeScriptRunnerEquality`: **request-encode, event-decode, error-map, unknown-field,
envelope-decode** (5 of 6 categories).

### Honest ceiling
- **No webhook signature verify.** This SDK keeps the E13 server-relay/browser stance; webhook
  verification is a server-side concern. `signature-verify` is therefore **not** a supported cell
  in the compatibility matrix (see the `—` for TS). Use the Python or Go SDK server-side for that.
- **Not published.** `npm publish` is deliberately out of scope; the package is built + checksummed
  + signed locally (`scripts/release/sdk-package.sh`). Public-registry publish is E18.
