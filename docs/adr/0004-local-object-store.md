# ADR-0004: Local object-store baseline

- Status: accepted
- Date: 2026-07-16
- Owners: Palai maintainers
- Supersedes: none
- Superseded by: none
- Hard-gate exceptions: none
- Readiness scope: E01 technology baseline only

## Context

The local distribution needs an S3-compatible store with retained bytes,
checksums, multipart operations, range reads, immutable multi-architecture
artifacts, and a license/distribution path that can be reviewed and reproduced
offline.

## Evidence and options

- [Object-store report](../../spikes/reports/object-store.json) records five
  repetitions of authenticated S3 bucket and object operations, checksum
  round-trips, conditional create, ranges, multipart complete/abort, restart
  persistence, exact cleanup, multi-architecture indexes, and local archive
  re-import with the same image identity.
- SeaweedFS 4.39 is active, Apache-2.0, and supplied the eligible immutable
  multi-architecture image used by the functional evidence.
- Garage 2.3.0 remains technically viable, but its AGPL distribution path needs
  policy review; E01 makes no legal compatibility conclusion and does not select
  it as the default.
- MinIO Community was rejected as the default because its repository is archived
  and the remaining image trails its source-only release, not because of S3 API
  maturity.

An absent discoverable upstream signature was not treated as verification. The
archive test proves local availability of the selected image identity; it does
not prove that all layers were fetched without daemon cache.

## Decision

Use a SeaweedFS 4.39 S3 adapter for the local stack, selected by immutable OCI
index. Palai must mirror that exact artifact and sign the Palai release artifact
before the release gate; an unsigned mutable upstream tag is not deployable
input.

## Scope

This selects the local S3-compatible adapter only. External S3 services remain
valid self-host configuration, and backup, restore, encryption, HA, and upgrade
behavior require later conformance gates.

## Version and digest policy

The selected reference is
`docker.io/chrislusf/seaweedfs@sha256:c7d6c721b30ae711db766bbbfd40192776e263d4e51e22f57baef7bef93c12c6`.
Its measured Linux manifests are recorded in the checksummed report. Deployment
must consume a Palai-mirrored equivalent by immutable index digest; version,
platform-manifest, mirror, or signature changes require registry metadata and S3
conformance to be repeated. The client evidence used AWS SDK for Go S3 1.105.1.

## Consequences

- Local Compose can depend on one selected S3 adapter while application code
  stays behind an S3-compatible boundary.
- Mirroring and signing are mandatory release work, not evidence inferred from
  an upstream TLS pull.
- Distribution-policy review remains explicit for any future candidate change.

## Verification

Run `scripts/spikes/object-store quick`, `scripts/spikes/check-reports`, and
`bash scripts/verify/e01.sh`.

## Revisit triggers

Revisit if SeaweedFS maintenance stops, its license or distribution changes, the
pinned index loses a supported architecture, mirror/signature policy cannot be
met, retained-byte or multipart conformance fails, or another reviewed candidate
passes the same evidence with a material operational advantage.
