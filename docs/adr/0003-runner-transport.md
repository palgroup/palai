# ADR-0003: Runner transport and supervisor baseline

- Status: accepted
- Date: 2026-07-16
- Owners: Palai maintainers
- Supersedes: none
- Superseded by: none
- Hard-gate exceptions: none
- Production readiness: not established

## Context

Private execution hosts must obtain work without opening an inbound runner port,
and untrusted engines must not receive control-plane credentials or a Docker
socket. The engine boundary also needs explicit framing, time, and output bounds.

## Evidence and options

- [Runner supervisor report](../../spikes/reports/runner-supervisor.json) records
  five repetitions of outbound-only mutual TLS, invalid client and hostname
  rejection, immutable lease identity, digest-pinned OCI execution, isolated
  credentials/socket, valid and malformed JSONL handling, timeout termination,
  bounded stdout, and separately bounded stderr.
- An inbound runner listener was rejected because it expands customer-network
  exposure and conflicts with the private-host connection model.
- Long polling remains a compatibility fallback, not the baseline, because the
  runner protocol needs efficient bidirectional lease and revocation messages.
- Shelling out to a Docker CLI was rejected for the daemon integration baseline;
  the Go client provides typed lifecycle operations and API negotiation.
- Embedding engine objects in the runner was rejected because it would erase the
  language-neutral isolation and protocol boundary.

## Decision

Use an outbound mutually authenticated TLS WebSocket from runner to control
plane. Use a versioned JSONL protocol between runner supervisor and engine, and
use the Moby Go client for digest-pinned OCI lifecycle operations.

## Scope

This fixes the E01 runner transport and local Docker-driver baseline. It does not
define enrollment PKI, multi-runner scheduling, Kubernetes drivers, or release
hardening, all of which remain later gates.

## Version and digest policy

The accepted measurements used `coder/websocket` 1.8.15, Moby client 0.5.0,
Docker 24.0.2, and engine image digest
`sha256:f1d9b8ef102dbf9a2ebbfcbd21e9a7fbb559b0853315d43ecdc735ac2ec546ea`.
Engine execution and deployment configuration must use immutable digests, never
mutable tags. Dependency or daemon-version changes must repeat protocol,
isolation, timeout, and cleanup checks.

## Consequences

- Runner credentials are short lived and terminate at the runner; engine input
  excludes runner identity, provider credentials, and the daemon socket.
- JSONL frames are capped at 512 bytes in the spike fixture, stdout at 32,768
  bytes, stderr at 128 bytes, and execution at 200 ms; product limits may grow
  only as explicit bounded configuration with equivalent tests.
- The control plane must support runner reconnect and protocol-version policy.

## Verification

Run `scripts/spikes/runner quick`, `scripts/spikes/check-reports`, and
`bash scripts/verify/e01.sh`.

## Revisit triggers

Revisit if supported networks cannot carry WebSocket TLS, mutual TLS enrollment
cannot meet rotation requirements, Moby cannot negotiate a supported Docker API,
the JSONL boundary cannot remain bounded, or another transport passes the same
outbound-only and isolation suite with a measurable operational benefit.
