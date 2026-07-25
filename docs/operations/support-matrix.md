# Palai release support matrix

Which surface is supported on which platform, at which topology. **Only a tested cell is filled.**

The authoritative, machine-readable matrix is [`support-matrix.json`](./support-matrix.json); the table
below is rendered from it. The guard `TestSupportMatrixOnlyClaimsWhatIsProven`
(`tests/docs/ops_docs_test.go`) resolves **every** proof id in that file against the tree, fails any
`tested`/`build-only` cell whose proof does not exist, fails a cell that claims nothing while citing
proof, fails a missing (surface, platform) pair, and diffs this table against the JSON cell by cell.
So a support claim cannot be added here without something that proves it.

## Reading the platform axis

For a **server** surface the platform is the **container platform**, not the operator's laptop. Docker
Desktop on an arm64 Mac runs `linux/arm64` containers, so an arm64 Mac exercises the `linux/arm64`
column. Palai's services ship only as linux images, so `darwin/*` is `n/a` for every server surface.
For the **CLI** the platform is the workstation.

| Level | Symbol | Means |
|---|---|---|
| tested | `tested` | A real test or drill ran this surface on this platform |
| build-only | `build` | The artifact is built, digest-indexed and verified for this platform, but the topology it enables was not exercised there |
| not-tested | `—` | Nobody has run it. Not claimed, not forbidden |
| not-applicable | `n/a` | The surface does not exist on this platform by construction |

## The matrix

| Surface | linux/amd64 | linux/arm64 | darwin/amd64 | darwin/arm64 |
|---|:---:|:---:|:---:|:---:|
| Service container images (control-plane, runner, reference-engine) | build | tested | n/a | n/a |
| Local four-service compose stack (`palai local up`) | build | tested | n/a | n/a |
| Production single-node compose behind the TLS edge | — | tested | n/a | n/a |
| Kubernetes via the restricted Helm chart (runner outside the cluster) | — | tested | n/a | n/a |
| Offline signed air-gap bundle install | — | tested | n/a | n/a |
| Runner host package on a separate host | build | build | n/a | n/a |
| `palai` CLI on an operator workstation | build | build | build | tested |

## What each filled cell rests on

| Surface / platform | Proof | The part that is *not* covered |
|---|---|---|
| images `linux/arm64` | `LP-009`, `TestReleaseBuildIsBitReproducible` | — |
| images `linux/amd64` | `TestReleaseIndexShape`, `make release-matrix-smoke` | Built by the buildx matrix and boot-smoked under emulation. **A boot-smoke is not a UAT run on amd64** (E18 §6 leg 3) |
| local compose `linux/arm64` | `LP-009`, `LP-012`, `LP-013` | Every runbook transcript in [`runbooks/transcripts/`](./runbooks/transcripts/) was recorded here |
| production compose `linux/arm64` | `OPS-002`, `DR-002`, `DR-004` | A dedicated cloud VM with a real domain and certificate is E14 §6 leg 1 |
| Kubernetes `linux/arm64` | `OPS-003`, `TestNoClusterRole`, `TestNetworkPolicyDefaultDeny`, `make uat-kind` | On a **kind** cluster. kindnet does **not** enforce NetworkPolicy, so default-deny is proven as a **render**, not as enforcement — a real enforcing CNI is E15 §6 leg 1 |
| air-gap `linux/arm64` | `OPS-004` | Offline verify + tamper rejection on an internal Docker network. A physical air-gapped facility with an operator trust-root ceremony is E15 §6 leg 2 |
| runner host package `linux/arm64` | `LP-007`, `TestReleaseIndexShape` | mTLS enrollment is proven — on the **same** host, over the compose network. A separate physical host with systemd is E14 §6 leg 3 |
| runner host package `linux/amd64` | `TestReleaseIndexShape` | Built and indexed only |
| CLI `darwin/arm64` | `API-012`, `LP-012`, `OPS-002` | Every CLI command in the runbooks was executed here |
| CLI other platforms | `TestReleaseIndexShape`, `TestReleaseBinariesAreTrimpathedAndPathFree` | Cross-compiled, digest-indexed and trimpath-checked; **not run** |

## Elsewhere

- **SDKs** — [`sdk-compatibility.md`](./sdk-compatibility.md) (+ `.json`) carries SDK × capability with
  its own honest-matrix guard. It is a different axis and is not duplicated here.
- **Managed cloud** — not offered. There is no managed cell and no microVM isolation tier; see
  [`../security/threat-model.md`](../security/threat-model.md).
- **Windows** — **no support of any kind is claimed.** The CLI is not built for it and nothing has been
  run there.

## Honest ceiling

Every `tested` cell in the `linux/arm64` column was tested on **one machine**: macOS + Docker Desktop
on Apple silicon. That is a real linux/arm64 container platform, and it is also a single
configuration — a shared developer laptop, not reference hardware. The performance numbers that come
off it carry no SLO (see the performance profile stamp, and row `PER-1` of
[`known-gaps-1.0.md`](./known-gaps-1.0.md)).

The `linux/amd64` column is the honest weak spot of this release: images are built and boot, and
nothing else on that architecture has been exercised. A full UAT run on amd64 is E18 §6 leg 3.
