# Known gaps — 1.0 RC triage

Every deferred finding carried into the 1.0 release candidate, with a decision, an owner, and — where
a P2 waiver applies — an expiry.

**These are decisions, not observations.** They were taken by the repository owner (`salihcnkhy`) in
the E18 T9 session on **2026-07-25**, first against commit `2f68375` and **re-scanned in full against
`803f293`** (the E18 T4 merge) the same day, and they read that way on purpose: a disposition table
with no name on it is a way of not deciding. **Amended by the same owner on 2026-07-26** in the E18 T10
session, which is where `SUP-3` closes, `SUP-2`'s open decision is taken, and the `EXT-1` and `PER-1`
T10 dependencies are discharged; each amended cell says what changed and on what date. Where a row disagrees with a later finding, the later
finding wins and the row is amended — with a new date.

```
RC-BLOCKERS: 0
```

That line is machine-read by the E18 T10 stable-release gate
(`TestKnownGapsRCBlockerCountIsAccurate`, `tests/docs/ops_docs_test.go`, keeps it equal to the number
of `RC-blocker` rows below). **A gate cannot close while it is non-zero.**

## No RC-blockers

There is **one** thing this table ever called an RC-blocker, and it is closed: `SUP-1`, the air-gap
verifier's fall-back to the bundle's own copy. (`SUP-3` also closed on 2026-07-26, but it was never a
blocker — it was a rule with no owner until E18 T10 took it.) E18 T4 landed the fail-closed resolution and the row
below carries the two tests that prove it. The count is zero because a row moved on evidence, not
because a decision column was edited — `TestKnownGapsClosedRowsCiteRealEvidence` will not let it be
the latter.

Zero blockers is **not** zero risk. Everything below is still carried into 1.0 on purpose: read
`SUP-2` (the fail-closed resolution class) and `AUD-1` (a retention purge reads as tampering) before
treating this number as an all-clear.

## Decisions

| Decision | Means |
|---|---|
| `RC-blocker` | Must be closed before the RC gate can pass |
| `post-1.0` | Deliberately out of 1.0. Not a defect to be hidden — a scope line |
| `§6-leg` | The code and harness exist and are parametric; execution needs operator-provided infrastructure, credentials or a decision |
| `closed-verified` | Was open, is now closed, and the evidence resolves |

## 1. Public-API gaps

| ID | Finding | Decision | Owner | Expiry | Evidence / where it goes |
|---|---|---|---|---|---|
| `API-1` | `modelRoutes` was write-only and the list envelope was unsplit (recorded E13 T10) | closed-verified | E16 T1 | — | `apps/control-plane/api/router.go` — the `cfg.modelRoutes` block mounts `listConnections`/`listRoutes`/`listRevisions` alongside the writes, admin `ListView`-enveloped and `provision`-gated; the envelope split is a corpus category (`API-012`, `envelope-decode` in `docs/operations/sdk-compatibility.json`). Only the verification line was outstanding; it is now verified against the tree |
| `API-2` | A2A **push delivery** is config-CRUD only — the `pushNotificationConfig` surface exists, nothing delivers | post-1.0 | E19 T4 | — | `a2a` is a **preview** capability and always was; E17 T2 recorded the honest §6 ceiling in the case text (`A2A-003`). A preview capability with an unwired half is a scope line, not a defect. E19 T4 wires a loopback sink; foreign-peer interop stays E17 §6 leg 2 |
| `API-3` | The approval detail the console can show stops at operation / branch / request-hash — no action, args, destination, risk or expiry | post-1.0 hardening | E19 T8 | — | The console is proven against the **honest** contract, not an invented one: `UI-002` is green and E17 T10 caught its own fixture inventing an approval event that `approval.requested.v1` does not carry. The gap is measured, not suspected |
| `API-4` | No `/v1/publications` read endpoint | post-1.0 hardening | E19 T8 | — | Named as a PUBLIC-API GAP in E17 T10 (`3b8f919`). E19 T8 puts the minimum read endpoint to the owner; unapproved, this row stays `post-1.0` and says so |

## 2. E13-H hardening tranche

The tranche `docs/superpowers/plans/phase-13-managed-cloud-infra.md` §6 deferred. Line by line, as
promised there. Most are SaaS-scope, consistent with the master plan §9 split.

| ID | Finding | Decision | Owner | Expiry | Evidence / where it goes |
|---|---|---|---|---|---|
| `H-AUD` | Audit integrity linkage — the `events` journal had no hash chain and no checkpoint | closed-verified | E18 T7 | — | `TestAuditIntegrityFourArms` `TestIntactJournalVerifiesGreen` `TestDeletedRowRaisesGap` `TestFlippedPayloadByteRaisesTamper` `TestCheckpointSignatureFailClosed` — chain recomputed from the rows against a signed out-of-DB anchor, with `palai audit checkpoint` / `palai audit verify` shipped |
| `H-SEC` | `SEC-001` JIT secret lease replay + `SEC-003` KMS/secret-backend outage — the master key is a **file**, not a KMS | §6-leg | operator (E18 §6 leg 6) | — | The file seam is fail-closed and proven under DR (`DR-005`, `DR-006`). A real KMS ceremony needs a real KMS; there is none in this program |
| `H-TEN` | `TEN-004` end-user token exchange — service + subject audited, body spoof denied | post-1.0 | product | — | SaaS-scope. A self-hosted single-tenant install has no end-user identity to exchange. Named in the threat model's §49.5 identity row as an open control |
| `H-DAT1` | `DAT-001` `store:false` **lifecycle depth** — provider-boundary disclosure | post-1.0 | product | — | The subset ships: the retention reaper honours `PALAI_RETENTION_STORE_FALSE_TTL` and a purged replay is a typed 410 tombstone (`API-015`). What is missing is the *disclosure* of the provider boundary, not the deletion |
| `H-DAT2` | `DAT-002` deletion across derived stores and backups — backup expiry / receipt tracking | post-1.0 | product | — | Byte-deletion in the object store ships (E09 T2 retention). Backup-expiry receipts do not exist; a backup taken before a deletion still holds the data, and `runbooks/backup-restore.md` says so |
| `H-DAT3` | `DAT-003` legal hold | post-1.0 | product | — | SaaS-scope. No hold primitive exists; nothing claims one |
| `H-DAT4` | `DAT-004` export — versioned manifest, checksums, no secret values | post-1.0 | product | — | `palai backup` produces a checksummed, manifested archive and `restore verify` recomputes it (`DR-004`), but that is an **operator** backup, not a tenant-facing export |
| `H-DAT5` | `DAT-005` residency — disallowed region/provider/runner/failover rejected | post-1.0 | product | — | The embedding half ships: a restricted-classification source is not embedded to a disallowed provider or region (`KNO-007`). Runner and failover residency are SaaS-scope |
| `H-DAT6` | `DAT-006` remainder — pre-signed artifact URL policy | post-1.0 | product | — | The basic deny is in the gate: a wrong-tenant artifact read is a miss (`MCI-004`). Pre-signed URLs are not a shipped surface, so there is no URL policy to enforce |
| `H-BIL2` | `BIL-002` fallback/child/cached usage allocated to the actual target dimensions | post-1.0 | product | — | Provider cache counters do fold into canonical usage (`MOD-010`) and a fallback records a new attempt (`MOD-005`); per-dimension **billing** allocation is commercial work |
| `H-BIL4` | `BIL-004` BYOK — provider usage visible, commercial charge distinction correct | post-1.0 | product | — | SaaS-scope: there is no commercial charge in a self-hosted install |
| `H-BIL5` | `BIL-005` adjustment — immutable compensating event and invoice trace | post-1.0 | product | — | SaaS-scope, as above |
| `H-OTL` | Content-free OpenTelemetry + redaction scanner **depth** | post-1.0 | product | — | Evidence redaction is scanned (`LP-011`) and the support bundle is redacted by construction; a content-free-by-construction OTel pipeline with its own scanner is not built |

## 3. Findings from E18 itself

| ID | Finding | Decision | Owner | Expiry | Evidence / where it goes |
|---|---|---|---|---|---|
| `SUP-1` | **Air-gap verifier fell back to the bundle's own copy** (fail-open), so a tampered bundle could verify itself | closed-verified | E18 T4 | — | `TestVerifierSwapFailsClosed` `TestVerifierResolutionRefusesWithNoOutOfBandSigner` — `deploy/airgap/verify.sh` now resolves the signer from **outside** the bundle (a sibling `runner-verify.sh`, else the git-tracked `scripts/package/runner/verify.sh` from a checkout) and **exits 2** if neither is reachable. `PALAI_AIRGAP_ALLOW_BUNDLED_VERIFIER=1` is the explicit same-session opt-in and prints a WARNING naming what it does not prove. Fixed at commit `5796698`, re-scanned green here. **This was the table's only RC-blocker** |
| `SUP-2` | **Fail-closed path/membership comparisons ship defeated.** Four of them in E18 alone, each shipped believing it was a fence: an unlisted-file check blind to what it was not told about (T3), an unanchored `grep -qF` where `grep -qxF` was needed (T2), a string-prefix containment rule an ordinary symlink walked through in both directions (T7), and a path-equality fence broken **four** ways — a `tools/` subdirectory, a symlinked release, APFS case-respelling, and plain macOS `/tmp` → `/private/tmp` **with no attacker action at all** (T4) | post-1.0 hardening | E18 T10 | — | Recorded as a **class**, not four rows, because four rows would read as four unlucky bugs. All four are fixed and each has the attack as its RED test: `TestProvenanceRejectsUnlistedFile` `TestVerifyRejectsATruncatedRiderName` `TestPubkeyContainmentSurvivesSymlinks` `TestReleaseVerifyRefusesAVerifierInsideTheRelease` `TestReleaseVerifyRefusesABundledKeyByAnySpelling`. **The standing rule: a comparison a security decision rests on is guilty until a RED test proves otherwise** — and the `/tmp` case is why "no attacker in the threat model" is not a defence, since an everyday alias defeated it. Two shapes are now known-good and should be reused rather than re-derived: containment is **device+inode** (`inside()` in `provenance-verify.sh` / `verify.sh`, `EvalSymlinks`+`os.SameFile` in `packages/audit`), and set membership is **whole-line** (`grep -qxF`). The residual is that no gate enumerates these comparisons repo-wide — a reviewer finds them, which is how all four were found. **E18 T10's decision (2026-07-26): the RC does NOT get that sweep, and the row stays `post-1.0 hardening`.** The reasoning, so it can be argued with: a repo-wide enumeration of "comparisons a security decision rests on" has no mechanical definition — every `==`, `strings.HasPrefix` and `filepath.Join` in a verifier is a candidate, so a grep-shaped gate would either be trivially evadable or drown a reviewer in false positives, and a gate nobody can act on is worse than none. What T10 did instead is narrower and real: the two known-good shapes are named above and reused verbatim, and every new fence this task added is a SET-MEMBERSHIP or EQUALITY check over a CANONICAL code table (`AppendixAUATIDs`, `StableAttestationLegs`, `SupplyChainTamperArms`, `CapabilityClaims`) with its own RED negative — no new path comparison was introduced. The sweep remains the right hardening work; it is not a blocker for an RC |
| `SUP-3` | **A promote with no `PALAI_RELEASE_DIR` verifies no artifacts at all.** `scripts/release/promote.sh` runs the offline release verifier only when the env var names a directory; unset, the tag is blessed on the evidence gate alone | closed-verified | E18 T10 | — | **T10 TOOK THE RULE (2026-07-26).** The wrapper's behaviour is unchanged and deliberate — a fence refusing an unnamed dir would run **before** the evidence gate and shadow the E15 T6 operator-leg refusal that `scripts/uat/sh2` and `scripts/uat/sdk-parity` both grep for, pinned by `TestPromoteReachesTheEvidenceGate`. The rule now lives where T9 said it had to: `SupplyChainProof.Complete()` REQUIRES a named release directory plus an offline verification and the six-arm tamper matrix, and `StableReleasePromoteGate` refuses a promote without a COMPLETE one. Proven by `TestPromoteRefusesAReleaseWithNoVerifiedArtifactSet` and `TestPromoteRefusesAnUnverifiedArtifactSet` (`tests/uat/stable-release/promote_test.go`). **RESIDUAL, stated rather than rounded off:** the rule binds the E18 stable-release family ONLY. The E15/E16/E17 promote families are unchanged and still bless a tag with no artifact verification, because retrofitting it there would change gates those epics closed against. A future release family inherits nothing automatically |
| `EVD-1` | `automation-0.1.0`'s `AUT-001` (and three sibling) checksums were **fabricated** — shape-valid, reproducing nothing | closed-verified | E18 T8 | — | `TestCommittedBundleChecksumSweep` `TestCorrectedChecksumsRecompute` `TestSweepCatchesFabricatedChecksum` `TestPreCorrectionChecksumConstructionSearch` — corrected and re-derived from the bundle's own committed canonical surface, with the correction and its ceiling written into the manifest's `checksum_note`; the case checksum is now **recomputed** in `VerifyManifest` rather than shape-checked. E18 T8's own review follow-ups were still landing at this commit; further corrections belong to that task, not to a new row |
| `PER-1` | **The performance profile stamps the machine but not the concurrent load.** The same code measured **229 ms, 7.5 s and 32.4 s** for `trigger_delivery_accept` depending only on what else was running on the laptop | post-1.0 | E18 T6 | — | The `Profile` stamp (`tests/performance/harness.go`) carries machine, cores, memory, OS, Docker version and the harness's **own** `load_shape` — but nothing about co-tenant load, so two runs 100× apart look equally well-stamped. Two consequences, both already true and both stated: **(a)** no number from this harness carries an SLO, and the profile's own `ceiling` field says so; **(b)** a threshold gate tuned on a quiet machine will flap on a busy one, which is why the PER gates are configurable thresholds proving the *mechanism*. Disclosing contention in the stamp is the fix; reference-hardware numbers are E18 §6 leg 3. **T10 consumed this row (2026-07-26):** `PerformanceProfileProof` RE-DERIVES every percentile, the gated value and the pass verdict from the RAW samples it carries and refuses a profileless or fabricated one (`TestPerformanceProofRefusalMatrix`), and it requires `no_slo_claim` true and `reference_hardware` false — so the gate certifies the MECHANISM and the honesty of the stamp, never a capacity number |
| `AUD-1` | **A retention purge is indistinguishable from tampering.** `scrub_events` UPDATEs an anchored row's payload to `{"purged": true}` rather than deleting it, so on a stack with `PALAI_RETENTION_STORE_FALSE_TTL` set the reaper raises the **highest-severity** alert during routine maintenance — and an attacker who edits a row inherits "the reaper did it" as cover | post-1.0 hardening | E18 T7 | — | Pinned by `TestARetentionPurgeIsIndistinguishableFromTamper` (`packages/audit/chain_test.go`), which fails if the behaviour ever silently changes. **Correct and unavoidable at this design point** — the chain covers payload bytes, and an authorised rewrite of payload bytes is a payload-byte change. What is *not* acceptable is an operator meeting the alert without knowing: the caveat is in the alert text itself, in `audit.Ceilings`, in `runbooks/audit-integrity-alert.md`, and in the transcript where arms 1 and 2 print byte-for-byte the same alert. **Operational rule: re-cut the checkpoint immediately after any purge.** A real fix is a purge-aware chain (a tombstone the chain understands, or excluding payload from the digest and losing payload integrity) — a design change, not a patch, and 1.0 ships the documented trap instead of a rushed one |

## 4. Findings from E17 / E19 that 1.0 carries

| ID | Finding | Decision | Owner | Expiry | Evidence / where it goes |
|---|---|---|---|---|---|
| `EXT-1` | The `extensions-0.1.0` bundle's `CapabilityTierProof.SnapshotSource` says the snapshot came from *"GET /v1/capabilities served by the real api.NewRouter"* — but the map it describes is produced only by the test's `fullyMountedRouter()` (A2A **and** the capability-worker gateway both mounted). **No shipped deployment config sets `PALAI_CAPABILITY_WORKER_LISTEN_ADDR`**, so no deployed binary serves that map | post-1.0 | E19 T8 | — | The sentence is defensible as written (the named test really does assert bit-equality against a real `NewRouter`), but it invites the reading that a *deployed* binary serves it, and it does not. E19 T8 owns the wording and the mount derivation. **T10 dependency: DISCHARGED (2026-07-26).** `AggregateTierProof.Complete()` REFUSES a proof whose `snapshot_source` does not name `fullyMountedRouter`, whose `served_by_deployed_config` is true, or whose `unmounted_reason` does not name `PALAI_CAPABILITY_WORKER_LISTEN_ADDR` — proven by `TestPostureAnchorRefusesADeployedClaim`, `TestPostureAnchorRefusesAnUnnamedSnapshotSource` and `TestPostureAnchorRefusesADroppedUnmountedReason`. The row stays open because E19 T8 still owns the `extensions-0.1.0` wording and the mount derivation |
| `WRK-1` | The capability-worker gateway listens over **plain HTTP**. The worker binary dials with a stock `http.Client` and has no CA flag, so TLS here would mean changing the client E17 T9 proved | post-1.0 hardening | E19 T8a | — | Recorded at the mount, `startCapabilityWorkerGateway` in `apps/control-plane/cmd/palai-control-plane/main.go`. The listener is separate from `/v1` and the production edge path-matches `/v1/*` only, so it is not edge-reachable; the operator terminates TLS in front of it. **The runner gateway's mTLS is the upgrade path.** Stated exactly, because the comfortable version of this row is wrong: there is **no loopback guard** — the mount is a plain `net.Listen("tcp", addr)` over `PALAI_CAPABILITY_WORKER_LISTEN_ADDR`, so it binds whatever the operator names, `0.0.0.0` included. Binding it anywhere but a trusted network is an operator error the product does not prevent, and in practice `WRK-2` is what keeps it harmless today |
| `WRK-2` | The capability-worker surface is **dormant in three ways**: no production path mints an enrollment token (`Gateway.IssueEnrollmentToken` has no caller outside tests), nothing calls `DispatchJob`, and nothing calls `SetWorkerHealth` or `RedispatchForRetry` | post-1.0 | E19 T8 | — | Verified against the tree at this commit: the gateway is **served and enforcing** everything `WRK-001`..`WRK-007` proved, and a real worker still cannot enroll because the operator ceremony does not exist. That is a missing operator path, not a missing capability — and it is why `capability-workers` being advertised at all depends on a listener nobody configures (`EXT-1`) |

## 5. Operator legs inherited from earlier epics

Not gaps in the code — gaps in what has been **run**. Each has a parametric harness in the tree and
needs infrastructure, credentials or a decision this program does not have. All are `§6-leg`; all are
named individually in the `stable` promote attestation, which refuses a leg it cannot name.

| ID | Leg | Owner | From |
|---|---|---|---|
| `L-1` | Real CI release run: protected environment, two maintainers, workflow-identity provenance, Sigstore/KMS signature, transparency-log entry | operator | E18 §6 leg 1 |
| `L-2` | Real registry publish (npm, PyPI, Go module tag, GHCR immutable images) | operator | E18 §6 leg 2, inherited from E16 §6 leg 3 |
| `L-3` | PER-001..004 on reference hardware, and a **full UAT run on amd64** (above the qemu boot-smoke) | operator | E18 §6 leg 3 |
| `L-4` | A real air-gap facility drill with an operator trust-root ceremony | operator | E18 §6 leg 4, inherited from E15 §6 leg 2 |
| `L-5` | **Real-model eval quality numbers.** E08's rule holds — the engine exposes no tools to a real provider — so quality numbers cannot be produced locally. They are an **input to the stable attestation**, not a local claim | operator | E18 §6 leg 5, inherited from E17 §6 leg 7 |
| `L-6` | KMS-backed master-key ceremony (`H-SEC` above) | operator | E18 §6 leg 6 |
| `L-7` | A real RC soak on the target topology (the local soak is a bounded window of minutes and is named that way) | operator | E18 §6 leg 7 |
| `L-8a` | Cloud-VM clean install and restore onto a **separate** physical host; systemd boot-persistence on a real two-VM split runner | operator | E14 §6 legs 1–3 |
| `L-8b` | A real restricted managed-Kubernetes install with an **enforcing** CNI; a real second-site DR drill; a published-release-to-release upgrade | operator | E15 §6 legs 1, 3, 4 |
| `L-8c` | A real LiteLLM instance and a real private model server (vLLM/Ollama class); journey 63.1 on a Linux and a Windows workstation | operator | E16 §6 legs 1, 2, 4 |
| `L-8d` | A real Slack workspace; a foreign A2A peer; real Xcode + Apple signing; pgvector or an external vector engine; a real broker product; a real Temporal instance; a deployed console with a manual screen-reader pass | operator | E17 §6 legs 1–6, 8 |

**Tier consequence, stated once:** `L-8d` is why `slack`, `a2a`, `queues` and `console` close
**preview** and `knowledge-vector` and `apple-build` close **disabled**. Only `knowledge` and
`capability-workers` close **stable**, and those tiers are recomputed from claim outcomes rather than
declared. Nothing in this table raises a tier.

## What is not in this table

- Anything the [`../security/threat-model.md`](../security/threat-model.md) marks `not claimed` and
  that nobody has asked for: no browser tool, no microVM tier, no managed operator boundary. A control
  that was never in scope is a **scope line**, not a gap, and duplicating the threat model's
  not-claimed list here would create a second source for it.
- Findings fixed inside the epic that produced them, with no residue. Those live in the git history.

## How a row leaves this table

`closed-verified` requires evidence that **resolves** — a UAT case id or a test name that exists — and
`TestKnownGapsClosedRowsCiteRealEvidence` (`tests/docs/ops_docs_test.go`) fails a `closed-verified` row
whose evidence does not. A row cannot be closed by editing its decision column.
