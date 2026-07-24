# Changelog — github.com/palgroup/palai/sdks/go

All notable changes to the Palai Go SDK. This SDK is part of the E16 three-language SDK line;
versions move together across TS / Python / Go (see `docs/operations/sdk-compatibility.md`).

Go modules are consumed **as source by tag** — there is no wheel/tgz build artifact. The published
unit is the git tag; `scripts/release/sdk-package.sh` snapshots + checksums the module source as
provenance, and the tag/publish itself is E18.

## 0.1.0 — E16 SDK surface

Target API-Version: **2026-07-16**.

### Added
- Its **own module** (`go.mod`) — never imports the monorepo's internal packages, so the SDK stays
  independently movable. **Stdlib-only**: `net/http` + `bufio` SSE + `encoding/json`; no third-party
  or provider dependency.
- Typed `/v1` client matching the TS surface: responses, sessions, agents, artifacts, reads,
  provisioning, secret-refs, model-routes.
- SSE streaming (`bufio`, 1 MiB frame ceiling mirroring the provider-one adapter idiom).
- Transport-level retry with a stable `Idempotency-Key` (single retry owner).
- Typed errors, including the 410 retention tombstone (API-015).
- Forward-compatible decoding: unknown fields preserved (struct-based decode does not strip them).
- `Page` vs `ListView` envelope decode.
- Webhook signature verification with rotation/skew tolerance (API-014).

### Conformance
Validated against the shared cross-language corpus (`tests/conformance/sdk/`) via
`TestCorpusGoRunnerEquality`: **all six categories** — request-encode, event-decode, error-map,
**signature-verify**, unknown-field, envelope-decode. This is the leg that completes the mechanical
three-language equality claim (asserted at the E16 T8 gate).

### Honest ceiling
- **Server-side SDK**; no browser story.
- v0 ergonomics are minimal (a simple options pattern); helper richness follows demand.
- **Not published.** Module source is snapshotted + checksummed + signed locally; the git tag +
  module-proxy publish is E18.
