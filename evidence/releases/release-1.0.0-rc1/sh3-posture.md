# SH-3 POSTURE — release-1.0.0-rc1

**This is a release CANDIDATE, not a stable release.** Every line below is RECOMPUTED from the
committed evidence bundles, the materialized case corpus and the RC triage table by
`TestPrintSH3PostureReport`; nothing here is maintained by hand. SH-3 Stable is the operator
attestation `StableReleasePromoteGate` demands — it names all 11 §6 operator legs one by one, and NOT
ONE of them has been executed in this program.

## Appendix-A UAT index

| Disposition | Count | Means |
|---|---:|---|
| `bundle-carried` | 52 | a committed evidence bundle carries the case with an outcome |
| `case-materialized` | 68 | `tests/uat/cases/<ID>/case.yaml` exists and the catalog gates resolve its in-tree proofs; no bundle carries it |
| `managed-scope` | 4 | outside this program by decision (master plan §2.2/§9), not by omission |
| `unmaterialized` | 64 | no bundle, no case directory. Stated because silence would be the dishonest answer |
| **total** | **188** | every exact id in master plan Appendix A |

Bundle-carried cases not reporting PASS: **0**.

The managed-scope ids and why:

- `DR-003` — regional failover — master plan §9 ("DR-003 managed regional failover scope"). One Docker Desktop host has no second region; the local DR seam is DR-001/002/004..006
- `QUO-002` — noisy-neighbour weighted fairness — master plan §9 ("pooled fairness managed scope"). Pooled capacity is a managed-cell property; a dedicated install has no pool to be fair across
- `SAN-009` — microVM tenant isolation on a managed high-isolation fleet — master plan §9 ("SAN-009 managed microVM fleet SaaS planına bağlı") and §2.2. The local OCI seam is SEC-102; nothing here claims a microVM tier
- `TEN-005` — support JIT access — master plan §9 ("TEN-005 managed support SaaS scope"). A self-hosted install has no managed support organization to grant JIT access to

## §64.15 stable-release checklist

| Status | Item |
|---|---|
| `incomplete` | every P0/P1 UAT passed on supported local and self-host reference topology |
| `not-claimed` | managed-only P0/P1 passed on production-equivalent cell/microVM topology |
| `proven-not-bundled` | all three SDK conformance suites |
| `evidenced` | at least two direct model-provider families plus one private/compatible endpoint |
| `proven-not-bundled` | one local OCI sandbox path |
| `not-claimed` | one managed high-isolation sandbox path |
| `proven-not-bundled` | process, container, and host kill/recovery |
| `proven-not-bundled` | pure/idempotent/irreversible tool replay |
| `incomplete` | queued/steer/interrupt messaging |
| `incomplete` | secret isolation |
| `proven-not-bundled` | repository clone/diff/push/PR |
| `proven-not-bundled` | Slack and generic webhook/schedule journey |
| `evidenced` | backup/restore/upgrade |
| `incomplete` | tenant isolation and billing reconciliation |
| `evidenced` | published security model, support policy, and operational runbooks |

Statuses: `evidenced` = every claim is bundle-carried and PASS. `proven-not-bundled` = every claim is
at least a materialized case, but some carry no bundle evidence. `incomplete` = at least one claim is
unmaterialized or not PASS. `not-claimed` = managed-scope.

**every P0/P1 UAT passed on supported local and self-host reference topology** — outstanding: API-001; API-002; API-003; API-004; API-005; API-006; API-007; API-008; API-009; API-010; API-011; SES-001; SES-002; SES-003; SES-004; SES-005; SES-006; SES-007; SES-008; SES-011; SES-012; AGT-003; SUB-001; SUB-002; SUB-003; SUB-004; SUB-005; TOL-005; TOL-006; TOL-007; TOL-013; TOL-014; TOL-015; ENG-001; ENG-002; ENG-003; SAN-010; SAN-012; REP-002; REP-003; REP-004; REP-009; REP-012; TEN-001; TEN-002; TEN-003; TEN-004; SEC-001; SEC-002; SEC-003; DAT-001; DAT-002; DAT-003; DAT-004; DAT-005; DAT-006; BIL-001; BIL-002; BIL-003; BIL-004; BIL-005; BIL-006; QUO-001; OPS-001

**all three SDK conformance suites** — outstanding: API-013 (materialized case, no bundle evidence); API-014 (materialized case, no bundle evidence); API-015 (materialized case, no bundle evidence); TOL-018 (materialized case, no bundle evidence)

**one local OCI sandbox path** — outstanding: SAN-001 (materialized case, no bundle evidence); SAN-002 (materialized case, no bundle evidence); SAN-003 (materialized case, no bundle evidence); SAN-004 (materialized case, no bundle evidence); SAN-005 (materialized case, no bundle evidence); SAN-007 (materialized case, no bundle evidence); SAN-008 (materialized case, no bundle evidence); SEC-102 (materialized case, no bundle evidence)

**process, container, and host kill/recovery** — outstanding: ENG-005 (materialized case, no bundle evidence); ENG-006 (materialized case, no bundle evidence); SAN-006 (materialized case, no bundle evidence)

**pure/idempotent/irreversible tool replay** — outstanding: TOL-001 (materialized case, no bundle evidence); TOL-002 (materialized case, no bundle evidence); TOL-003 (materialized case, no bundle evidence)

**queued/steer/interrupt messaging** — outstanding: SES-003 (unmaterialized); SES-004 (unmaterialized); SES-005 (unmaterialized)

**secret isolation** — outstanding: SEC-002 (unmaterialized); TOL-013 (unmaterialized); REP-003 (unmaterialized)

**repository clone/diff/push/PR** — outstanding: REP-001 (materialized case, no bundle evidence); REP-005 (materialized case, no bundle evidence); REP-008 (materialized case, no bundle evidence)

**Slack and generic webhook/schedule journey** — outstanding: AUT-011 (materialized case, no bundle evidence)

**tenant isolation and billing reconciliation** — outstanding: TEN-001 (unmaterialized); TEN-002 (unmaterialized); BIL-001 (unmaterialized); BIL-003 (unmaterialized)

## Product-wide capability posture

Recomputed from EVERY committed bundle's claim outcomes and asserted bit-equal to the fully-mounted
router's `/v1/capabilities` (`TestServedCapabilityTiersEqualTheAggregateRecompute`). **No shipped
deployment config sets `PALAI_CAPABILITY_WORKER_LISTEN_ADDR`, so no deployed binary serves this exact
map** — EXT-1 in the RC triage, and the bundle's proof declares it.

| Capability | Tier |
|---|---|
| `a2a` | preview |
| `apple-build` | disabled |
| `capability-workers` | stable |
| `console` | preview |
| `knowledge` | stable |
| `knowledge-vector` | disabled |
| `queues` | preview |
| `slack` | preview |

## Zero open P0/P1

`RC-BLOCKERS: 0`, read mechanically from `docs/operations/known-gaps-1.0.md` at verification time.
A non-zero count REFUSES this gate. Zero blockers is not zero risk: read `SUP-2` (fail-closed
comparisons ship defeated) and `AUD-1` (a retention purge reads as tampering) before treating it as an
all-clear.

## The live anchor is a JOURNEY leg, not a bundle case

`scripts/uat/stable-release PROVIDER=provider-one` drives ONE real single-step provider run — E08's
rule holds, the engine opens no tool to a real provider, so a live run is single-step BY
CONSTRUCTION. It is deliberately NOT a case in this bundle: E18 owns no live-provider UAT id, and
writing a real provider receipt into a manifest with no live-provider case to gate it would be the
marker-alone-is-never-proof pattern this verifier refuses everywhere else. The receipt lives in the
journey transcript; what the bundle carries is what the bundle can gate.

## What this gate does NOT claim

- **L-1** — a real CI release run: protected environment, two maintainers, workflow-identity provenance, a Sigstore/KMS signature and a transparency-log entry (E18 §6 leg 1)
- **L-2** — a real registry publish: npm, PyPI, a Go module tag and GHCR immutable images (E18 §6 leg 2, inherited from E16 §6 leg 3)
- **L-3** — PER-001..004 on reference hardware, and a FULL UAT run on amd64 above the qemu boot-smoke (E18 §6 leg 3)
- **L-4** — a real air-gap facility drill with an operator trust-root ceremony (E18 §6 leg 4, inherited from E15 §6 leg 2)
- **L-5** — real-model eval QUALITY numbers — E08's rule holds, the engine exposes no tools to a real provider, so they cannot be produced locally and are an INPUT to this attestation (E18 §6 leg 5, inherited from E17 §6 leg 7)
- **L-6** — a KMS-backed master-key ceremony above the file seam (E18 §6 leg 6; E13-H SEC-001/003)
- **L-7** — a real RC soak on the target topology — the local soak is a bounded window of MINUTES and is named that way (E18 §6 leg 7)
- **L-8a** — a cloud-VM clean install and a restore onto a SEPARATE physical host; systemd boot-persistence on a real two-VM split runner (E14 §6 legs 1-3)
- **L-8b** — a real restricted managed-Kubernetes install with an ENFORCING CNI; a real second-site DR drill; a published-release-to-release upgrade (E15 §6 legs 1, 3, 4)
- **L-8c** — a real LiteLLM instance and a real private model server (vLLM/Ollama class); journey 63.1 on a Linux and a Windows workstation (E16 §6 legs 1, 2, 4)
- **L-8d** — a real Slack workspace; a foreign A2A peer; real Xcode + Apple signing; pgvector or an external vector engine; a real broker PRODUCT; a real Temporal instance; a deployed console with a manual screen-reader pass (E17 §6 legs 1-6, 8)

Each is refused BY NAME by `StableReleasePromoteGate` when a `stable` promote's attestation omits it
(`TestStablePromoteRefusesAnAttestationMissingALeg` drives all 11 in turn). The local closure of this
gate is an RC.

END POSTURE
