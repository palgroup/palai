# ADR-0001: Language and runtime baseline

- Status: accepted
- Date: 2026-07-16
- Owners: Palai maintainers
- Supersedes: none
- Superseded by: none
- Hard-gate exceptions: none
- Readiness scope: E01 technology baseline only

## Context

Palai needs a durable control plane and runner, a provider-friendly reference
engine, and an SDK that is natural in the first Next.js proof application. The
choice must follow measured concurrency, recovery, and resource behavior rather
than language familiarity.

## Evidence and options

- [Control-plane runtime report](../../spikes/reports/control-plane-runtime.json)
  records 1,000 connected streams, 100 exact reconnects, one restart, no event
  gaps or duplicates, and bounded shutdown for both Go and Node. Go used
  11,042,816 idle RSS bytes versus Node's 47,120,384 and 42,090,496 connected
  RSS bytes versus Node's 64,733,184.
- [PostgreSQL coordinator report](../../spikes/reports/postgres-coordinator.json)
  records 20/20 transaction-kill recoveries, higher fencing tokens, stale
  completion rejection, and exactly one authoritative outbox completion.
- A TypeScript-only core passed the stream spike but was not selected because
  its measured memory footprint was higher and it does not remove the need for
  a host runner or cross-language stable-release SDKs.
- Rust remains a viable future implementation option, but E01 did not measure a
  Rust candidate; selecting it now would add an unmeasured toolchain path.

Hard rejection criteria were semantic event loss, stale-fence acceptance,
unbounded memory, leaked credentials, or an unsupported target platform. No
accepted report contains such an exception.

## Decision

Use Go for the control plane, coordinator, runner, and CLI. Use Python for the
reference engine behind a versioned protocol boundary. Build the TypeScript SDK
first for the Next.js local proof, while preserving the requirement that
TypeScript, Python, and Go SDKs ship with semantic parity at stable release.

## Scope

This fixes the implementation baseline for work after E01. It does not make Go
types, Python objects, or TypeScript interfaces the canonical public contract,
and it does not establish any LP-0 or self-host release claim.

## Version and digest policy

The accepted measurements used Go 1.26.4, Node 22.22.2, and Python 3.14.3. The
repository pins those exact tool versions and lock files. Build images and
released binaries must additionally be tied to immutable image digests and
checksums; a mutable language-runtime tag is not a release input. Go upgrades
follow the supported-release policy and require the same bounded stream and
coordinator checks.

## Consequences

- Go services must stay below the measured 128 MiB idle RSS hard bound and the
  five-second graceful shutdown bound used by the spike.
- The Python engine and TypeScript-first SDK require generated, language-neutral
  transport contracts and cross-language fixtures.
- Runtime packaging has more than one toolchain, so bootstrap and verification
  remain lock-driven behind the repository build facade.

## Verification

Run `scripts/spikes/check-reports` and `bash scripts/verify/e01.sh`. The report
index authenticates the exact evidence bytes used by this decision.

## Revisit triggers

Supersede this ADR if Go breaches the resource or recovery bounds, the protocol
cannot remain lossless across all three languages, a supported deployment
platform cannot run the pinned artifacts, or another measured candidate shows a
material operational advantage without weakening the public contract.
