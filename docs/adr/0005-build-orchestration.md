# ADR-0005: Build and verification orchestration baseline

- Status: accepted
- Date: 2026-07-16
- Owners: Palai maintainers
- Supersedes: none
- Superseded by: none
- Hard-gate exceptions: none
- Production readiness: not established

## Context

Palai is intentionally polyglot. Contributors and CI need one stable command
surface without hiding the native Go, Python, and TypeScript dependency locks or
mixing credential-free quick tests with Docker-backed live evidence.

## Evidence and options

The accepted baseline is backed by the complete checksummed E01 set:

- [Contract toolchain](../../spikes/reports/contract-toolchain.json)
- [Control-plane runtime](../../spikes/reports/control-plane-runtime.json)
- [Next.js streaming](../../spikes/reports/nextjs-streaming.json)
- [Object store](../../spikes/reports/object-store.json)
- [PostgreSQL coordinator](../../spikes/reports/postgres-coordinator.json)
- [Runner supervisor](../../spikes/reports/runner-supervisor.json)

Native language commands alone were rejected as the public repository interface
because they fragment the verification contract. A new custom orchestration
program was rejected because Make already supplies dependency ordering and
failure propagation. Running Docker evidence in every quick check was rejected
because quick verification must remain credential-free and deterministic.

## Decision

Use Make as the stable repository facade over Go, uv, and pnpm. Keep native lock
files authoritative, and expose Docker-backed technology evidence through
explicit evidence commands rather than the default quick-test path.

## Scope

This covers developer and CI command orchestration. It does not select a runtime
deployment orchestrator, replace language package managers, or assert that E01
evidence is a product release suite.

## Version and digest policy

Keep the Makefile compatible with GNU Make 3.81 or newer. Pin Go 1.26.4, Node
22.22.2, pnpm 11.9.0, Python 3.14.3, and uv 0.8.2 in repository metadata, with
exact lock files for language dependencies. Docker inputs used by evidence or
deployment must use immutable image digests. A tool, lock, action, or image
change must pass its narrow suite and the stable `make verify` facade.

## Consequences

- `make verify` remains fast and credential-free; heavyweight live measurements
  use explicit evidence targets and committed content-free reports.
- Make coordinates commands but does not duplicate package resolution or code
  generation logic owned by native tools.
- CI and local development share command names while environment-specific
  provisioning remains visible in CI or evidence scripts.

## Verification

Run `make verify` and `bash scripts/verify/e01.sh`. E01 promotion additionally
requires the report-index checksums and all hard gates to remain closed.

## Revisit triggers

Revisit if GNU Make 3.81 compatibility blocks a required deterministic workflow,
the facade cannot express correct dependency ordering, native lock verification
diverges from CI, or another tool demonstrably reduces bootstrap and maintenance
cost without obscuring the underlying commands or evidence boundary.
