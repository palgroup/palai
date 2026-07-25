# Palai threat model — the IMPLEMENTED surface

This is **not** a copy of the aspirational security architecture in `MASTER-SPEC.md` §49. It is §49
**projected onto the code that exists in this tree**, one row at a time. A row states a control this
build actually enforces and names the **evidence** that proves it; a control §49 asks for that this
build does **not** have says `not claimed`, and says it in the same table as the ones that do.

Read `MASTER-SPEC.md` §49 for the design intent. Read this file for what is true today.

## How to read a row

Every row's **Evidence** cell is one of exactly two things:

| Cell contains | Meaning | Resolves to |
|---|---|---|
| One or more backticked IDs | The control is enforced and proven | `tests/uat/cases/<ID>/case.yaml`, or a `func <Name>(` in some `*_test.go` |
| The bare words `not claimed` | This build does not implement the control | nothing — and the row says where it lives instead |

A UAT ID looks like `SAN-004`. A test ID looks like `TestSandboxDeniesMetadataEgress`. Nothing else is
allowed in an Evidence cell: the guard `TestThreatModelEvidenceResolves`
(`tests/docs/guards_test.go`) parses **every** table in this file, resolves **every** ID against
the tree, and fails on an unresolvable ID, on prose smuggled into an Evidence cell, and on a §49
section this file forgot to cover. So a mitigation cannot be claimed here without something in the
repository that would go red if the mitigation disappeared.

Where an ID names a UAT case, the case file itself names its proof; where it names a test, the test
*is* the proof. Several controls are proven by tests whose UAT case IDs (`SEC-101`..`SEC-103`,
`PER-001`..`PER-004`) are materialized by E18 T10 — those rows cite the tests, which exist now.

## The scope line, said once

Palai as built is a **single-node self-hosted** product with a **local OCI sandbox**. Three whole
classes of §49 control therefore say `not claimed` throughout this document, and it is the same three
every time:

- **Managed/SaaS operator boundary** — there is no managed cell, no operator-to-tenant boundary, no
  per-tenant support access path. `MASTER-SPEC.md` §9 puts these in the SaaS plan.
- **Hardware-virtualized (microVM) isolation** — the sandbox is OCI. §49.12's "hardware virtualization
  for managed hostile multi-tenancy" is a managed-scope control; the local seam is proven, the microVM
  seam does not exist. E18 T10's release index marks it *managed-scope, not claimed*.
- **Browser / computer use** — there is no browser tool and no computer-use tool in this tree, so all
  of §49.15 is `not claimed`. The only network-fetching model-facing tool is the web-research
  fetch+cite tool (`EXT-004`), and its ceiling is written into its own case.

---

## 0. Objectives, actors, assets (§49.1, §49.3, §49.4)

### 0.1 Security objectives (§49.1) — what this build preserves

| Objective | Preserved here by | Evidence |
|---|---|---|
| tenant and project isolation | RLS on every tenant table plus a cross-tenant cursor rejection at the API | `MCI-003` `MCI-004` `TestEveryTenantTableIsRowLevelSecured` |
| confidentiality of content and credentials | Secrets never enter model context and are redeemed only in the executor | `MCI-002` `TestSecretRefIsRedeemedOnlyInExecutorAndNeverLeaks` |
| integrity of repository changes, artifacts, policy, audit | Approved push landing exactly once, content-digest artifact retrieval, immutable revisions, a recomputable audit chain | `REP-006` `MCI-004` `AGT-001` `TestAuditIntegrityFourArms` |
| availability under runaway or malicious workloads | Admission limits before compute, bounded queues, circuit isolation | `MCI-005` `AUT-010` `MOD-012` |
| human control over consequential actions | Approval bound to the exact operation hash | `APV-001` `SLK-007` `UI-002` |
| traceability input → model/tool/subagent → side effect | The append-only journal plus the per-tool signed halves ledger | `TOL-016` `TestAuditChainCoversEveryEventColumn` |
| recoverability without duplicate external actions | Uncertain irreversible effects never auto-replay; a lost ack reconciles | `TOL-003` `REP-007` `AUT-009` |
| supply-chain identity of every executed component | Digest-pinned bases and an offline-verifiable signed release index | `OPS-004` `TestEveryDockerfileBaseIsDigestPinned` `TestProvenanceBindsRecomputedSubjectsAndMaterials` |

### 0.2 Threat actors (§49.3) — which ones this build is modelled against

| Actor | Modelled here | Evidence |
|---|---|---|
| unauthenticated internet attacker | Yes — auth and rate limit precede compute; the production edge exposes only `/v1/*` | `MCI-005` `OPS-002` |
| malicious or compromised tenant user | Yes — scoped keys, per-project policy, approval separation | `MCI-001` `SLK-004` |
| tenant attempting cross-tenant access | Yes — RLS plus cursor rejection, proven at both layers | `MCI-003` `TestForeignWriteIsRejected` |
| malicious repository/website/document author | Yes — repo instructions, web content and documents are untrusted input | `KNO-006` `EXT-004` `QUA-003` |
| prompt-injected external content | Yes — held-out injection suite gates release | `QUA-003` `QUA-004` |
| compromised MCP/tool/provider/integration | Yes — audience-bound tokens, default-deny sampling, crash breaker | `TOL-009` `TOL-010` `EXT-005` |
| malicious skill/plugin/image/package | Yes — archive rejection, digest pinning, vulnerability gate | `TOL-011` `TestEveryDockerfileBaseIsDigestPinned` `TestVulnGateBlocksCriticalFinding` |
| escaped sandbox workload | Partial — detection and quarantine at the OCI seam; a microVM tier does not exist | `SAN-006` `SAN-008` `TestSandboxEscapeSuite` |
| compromised runner/worker | Yes — fence advance rejects a stale writer; cordon/drain/revoke; a worker cannot be a general tunnel | `SAN-006` `SAN-011` `WRK-003` `WRK-006` |
| overprivileged or compromised operator | Partial — two-person promotion is a documented policy proven at the logic level; the ceremony itself is an operator leg | `TestSelfHost02PromotesToRCNotStable` `TestEvalPromoteGateRefusesStableWithoutAttestation` |
| leaked API/runner/provider credential | Partial — revoke and rotate exist; search by credential fingerprint does not | `MCI-002` |
| faulty or adversarial model output | Yes — output is schema-validated, never auto-fetched, never executed outside the sandbox | `EXT-001` `EXT-004` `SAN-002` |
| accidental administrator/user error | Yes — restore refuses a non-empty target, base movement is refused, an unsafe overlay fails config validate | `DR-005` `REP-010` `OPS-002` |
| supply-chain attacker | Yes — the six-arm tamper matrix denies execution and promotion | `OPS-004` `TestProvenanceRejectsTamperedSubjectDigest` `TestSBOMVerifyRefusesAOneByteTamper` |

### 0.3 Asset inventory (§49.4) — where each asset lives and what protects it

| Asset | Where it lives here | Protection | Evidence |
|---|---|---|---|
| identity and authorization state | `api_keys`, org/project rows in Postgres | RLS + scoped keys + revocation | `MCI-001` `TestEveryTenantTableIsRowLevelSecured` |
| model/repository/integration secrets | Encrypted under the file-based master key; resolved by reference | Never in model context; executor-only redemption; canary-verified across restore | `MCI-002` `DR-006` `TestSecretRefIsRedeemedOnlyInExecutorAndNeverLeaks` |
| customer repository/files/messages/artifacts | Postgres + the object store | Tenant-scoped retrieval by content digest; a wrong-tenant read is a miss | `MCI-004` `KNO-003` |
| workspace/checkpoint contents | Runner-host allocations and checkpoint blobs | Checksummed snapshots that exclude secrets; no residue between allocations | `SAN-005` `SAN-007` `ENG-009` |
| policy/config/agent/tool revisions | Immutable revision rows | A published revision is immutable and pinned by delivery | `AGT-001` `AGT-002` `EXT-003` |
| runner and signing keys | Runner CA under the data dir; release keys held out of band | mTLS enrollment; an in-bundle public key is refused by the verifier | `LP-007` `TestProvenanceRefusesInBundlePublicKey` |
| audit and usage ledgers | The `events` journal and usage rows | Append-only, chain recomputed from the rows against a signed out-of-DB anchor | `TestAuditIntegrityFourArms` `LP-006` |
| publication/signing capabilities | The promote gate | A `stable` target requires an operator attestation and is never auto-claimed | `TestSelfHost02PromotesToRCNotStable` `TestEvalPromoteGateRefusesStableWithoutAttestation` |
| billing/entitlement state | Usage/budget rows | Reservation and settlement with no overspend | `MOD-011` |
| release artifacts and update channel | `evidence/releases/` and the release index | Digest-identified, offline-verifiable, tamper-rejecting | `OPS-004` `TestReleaseBuildIsBitReproducible` |

Every asset above has an owner, a storage location and a deletion path in the product; **classification
labels and a per-asset retention/processor register are `not claimed`** — see the `DAT` rows of
`docs/operations/known-gaps-1.0.md`.

---

## 1. Trust boundaries (§49.2)

§49.2 names twelve boundaries. Eight are enforced here, two partially, two not at all.

| # | Boundary | What enforces it in this build | Evidence |
|---|---|---|---|
| 1 | public caller ↔ API edge | API-key auth at the edge, per-key request-rate limit and per-project concurrent/queued caps refusing with 429 **before** compute; production edge path-matches `/v1/*` only | `MCI-005` `OPS-002` |
| 2 | organization/project ↔ another tenant | Postgres row-level security on every tenant table, a non-owner runtime role, and a cursor that cannot be carried across tenants | `MCI-003` `MCI-004` `TestEveryTenantTableIsRowLevelSecured` `TestConnectionWithoutTenantContextSeesNoTenantRows` `TestForeignWriteIsRejected` `TestRuntimeRoleIsNotTheTableOwner` |
| 3 | control plane ↔ execution runner | Mutually-authenticated runner gateway on its own listener with one-time enrollment, plus cordon/drain/revoke lifecycle | `LP-007` `SAN-011` |
| 4 | runner host ↔ sandbox | OCI sandbox: no credential and no Docker socket reach the engine, argv-form shell under a sandbox user with resource limits, container unfindable after destroy | `SAN-002` `SAN-003` `TestEngineReceivesNoCredentialsOrDockerSocket` `TestContainerIsUnfindableAfterDestroy` |
| 4b | runner host ↔ **microVM** | not claimed | not claimed |
| 5 | engine ↔ model/tool brokers | Engine protocol over a receipted OCI boundary; only the *effective* tool set is advertised and a tool outside it is never reachable | `LP-008` `EXT-001` `EXT-002` |
| 6 | platform ↔ model provider | Per-project model routes, capability hard-filter before admission, honest attempt accounting across fallback | `MOD-004` `MOD-005` `MOD-008` `MCI-006` |
| 7 | platform ↔ tool/MCP/integration | Audience-bound MCP tokens that are never forwarded, default-deny sampling, revision-pinned annotations, a crash breaker that keeps the control plane up | `TOL-009` `TOL-010` `EXT-005` `EXT-006` `TestMCPServerCannotReplayTokenToAnotherUpstream` |
| 8 | workspace ↔ external repository | Brokered short-lived credential absent from every surface and revoked after preparation; deterministic clone at an exact commit with a receipt; approved push lands once | `REP-001` `REP-006` `TestBrokeredCredentialAbsentFromAllSurfaces` `TestReadCredentialRevokedAfterPreparation` |
| 9 | parent run ↔ child/remote agent | Child capability is a subset admitted outside model control; remote agent output is untrusted and inherits no parent credential | `SUB-006` `SUB-007` `A2A-005` `TestAdmitChildDepthAndFanoutBounded` |
| 10 | managed operator ↔ customer tenant | not claimed | not claimed |
| 11 | release pipeline ↔ runtime artifact | Digest-pinned bases, bit-reproducible binaries, SBOM + provenance bound to recomputed subjects, signature verified offline, verifier resolution fail-closed | `OPS-004` `TestEveryDockerfileBaseIsDigestPinned` `TestReleaseBuildIsBitReproducible` `TestProvenanceBindsRecomputedSubjectsAndMaterials` `TestVerifierSwapFailsClosed` |
| 12 | primary region ↔ backup/failover region | Same-host DR only: backup/restore into a separate clean stack with checksum, tenant-id, migration and retrieval verification. **Cross-region failover is not claimed** — see `docs/operations/dr-report.md` | `DR-002` `DR-004` `DR-005` `DR-006` |

---

## 2. Agentic threat–control matrix (§49.5)

§49.5's ten rows, with the controls **this build enforces** and the ones it does not.

| Threat | Controls enforced here | Evidence | Not claimed |
|---|---|---|---|
| goal/prompt hijack | Retrieved and remote content is untrusted, cannot instruct and cannot grant a capability; a skill archive's instructions grant nothing; a held-out injection eval suite gates release | `KNO-006` `TOL-011` `A2A-005` `QUA-003` `QUA-004` | Real-model injection quality numbers (E18 §6 leg 5) |
| tool misuse | Schema validation, deterministic policy hooks that fail closed, exact approval bound to the operation, side-effect classes with distinct replay semantics | `APV-001` `TOL-001` `TOL-002` `TOL-003` `TOL-004` `TOL-012` | — |
| identity/privilege abuse | Audience-bound MCP tokens denied on passthrough, fence-scoped callbacks, an unmapped Slack user is a constrained actor who cannot approve | `TOL-009` `TOL-017` `SLK-004` `SLK-007` | End-user token exchange (`TEN-004`, open — see `known-gaps-1.0.md`) |
| agentic supply chain | Digest pinning everywhere, SBOM + vulnerability policy gate, in-toto/SLSA-shaped provenance over openssl signatures, offline tamper matrix, quarantine on uncertain failure | `OPS-004` `WRK-007` `SAN-008` `TestEveryDockerfileBaseIsDigestPinned` `TestSBOMVerifyRefusesAOneByteTamper` `TestVulnGateBlocksCriticalFinding` `TestProvenanceRejectsTamperedSubjectDigest` | Sigstore/Fulcio/Rekor transparency log (E18 §6 leg 1); live CVE feed (pinned snapshot only) |
| unexpected code execution | Hostile code runs in an OCI sandbox with no host socket, no credential, argv-form invocation, resource ceilings and process-group kill; the whole SAN corpus runs as one escape suite | `SAN-001` `SAN-002` `SAN-003` `TestSandboxEscapeSuite` `TestEngineReceivesNoCredentialsOrDockerSocket` | microVM / managed high-isolation tier (managed-scope) |
| memory/context poisoning | Knowledge items are immutable, versioned and checksummed; ingest records source revision; a failed refresh leaves the prior index intact; deletion excludes content on rebuild | `KNO-001` `KNO-002` `KNO-004` `EXT-006` | Model-generated prompt changes auto-accepted — the platform has no auto-accept path, so nothing to claim |
| inter-agent trust abuse | Child capability is a parent subset, depth and fan-out are admitted outside the model, remote output is untrusted, no credential inheritance, cancel reconciles children | `SUB-006` `SUB-007` `A2A-005` `SES-010` `TestAdmitChildDepthAndFanoutBounded` | — |
| cascading failure/runaway | Budget reservation and settlement with no overspend, bounded queues with visible depth and 429, per-caller circuit isolation, breaker on a crashing extension | `MOD-011` `MOD-012` `AUT-010` `EXT-005` `MCI-005` | Reference-hardware load numbers (E18 §6 leg 3) |
| human deception | The approval surface shows the authoritative operation/branch/request-hash and a display can never replace it; a changeset is compiled from the tool ledger, not from model prose; citations carry verifiable offsets | `UI-002` `REP-005` `KNO-001` `KNO-005` `SLK-007` | Manual screen-reader pass (E17 §6 leg 8) |
| data exfiltration | Secret never enters model context, JIT executor-only redemption, egress revalidation on redirect and DNS rebind, metadata endpoints denied, evidence redaction scanned | `MCI-002` `WRK-004` `AUT-012` `SAN-004` `LP-011` `TestSecretRefIsRedeemedOnlyInExecutorAndNeverLeaks` `TestShellSecretRedactedInDisplayAndOutput` | Anomaly detection on high-entropy output (no detector exists) |

---

## 3. Per-area detail

### 3.1 Prompt injection (§49.6)

| §49.6 requirement | Status here | Evidence |
|---|---|---|
| External content delimited and provenance-tagged | Enforced — retrieved sources carry source + checksum and cite verifiable offsets | `KNO-001` `KNO-005` |
| Content cannot grant capabilities or change policy | Enforced for knowledge, skills, remote agents and MCP tool descriptions | `KNO-006` `TOL-011` `A2A-005` `EXT-006` |
| Tool execution passes deterministic validation | Enforced — the policy hook is deterministic and fails closed on timeout | `TOL-012` |
| Retrieved instructions do not enter trusted layers | Enforced | `KNO-006` |
| High-risk actions require exact approval independent of model claims | Enforced — approval binds the operation hash, not the model's description | `APV-001` `SLK-007` `UI-002` |
| Web/browser tools restrict navigation/download/exfiltration | Partial — the web-research tool denies private/metadata targets after resolve and redirect and never gains a publish capability. There is no browser tool | `EXT-004` |
| Injection regression suites (direct, indirect, encoded, multilingual, tool-output) | Enforced as a **content-addressed held-out suite** that blocks a security regression independently of the aggregate score. The corpus is deterministic-engine driven | `QUA-003` `QUA-004` |
| Sensitive data withheld from context unless allowed | Enforced — a restricted-classification source is not embedded to a disallowed provider or region | `KNO-007` |

### 3.2 Excessive agency (§49.7)

| §49.7 requirement | Status here | Evidence |
|---|---|---|
| Finite cost/token budgets | Enforced — reservation and settlement, no overspend | `MOD-011` |
| Finite tool/subagent/depth/fan-out budgets | Enforced outside model control | `TestAdmitChildDepthAndFanoutBounded` `TestChildRunDepthAndFanoutBounded` |
| Explicit capability set | Enforced — only the effective set is advertised; anything outside it is unreachable | `EXT-001` `EXT-002` |
| Network policy | Enforced for the research tool and sandbox egress | `EXT-004` `SAN-004` |
| Repository scope | Enforced — clone is pinned to an exact commit, push is approved and lands once | `REP-001` `REP-006` |
| Side-effect approval policy | Enforced per side-effect class | `APV-001` `TOL-001` `TOL-002` `TOL-003` `TOL-004` |
| Cancellation owner and terminal contract | Enforced — one terminal under a fence, never a false success | `ENG-013` `ENG-014` `SES-010` |
| A model cannot extend its own budget/expiry/depth/capability/approval | Enforced — every one of those is admitted by the control plane | `MOD-011` `EXT-002` `SLK-007` `TestAdmitChildDepthAndFanoutBounded` |

### 3.3 Secret exfiltration (§49.8)

| §49.8 defence | Status here | Evidence |
|---|---|---|
| Secret not supplied to model/engine | Enforced | `MCI-002` `TestEngineReceivesNoCredentialsOrDockerSocket` |
| JIT executor-only credential | Enforced — redeemed in the executor, never elsewhere | `TestSecretRefIsRedeemedOnlyInExecutorAndNeverLeaks` |
| Destination-scoped, short-lived token | Enforced — repository push token destroyed after use; worker secret handle is job-scoped and never journaled | `REP-006` `WRK-004` |
| Egress allowlist / proxy | Enforced for outbound webhooks, A2A cards, MCP metadata and research fetches; redirects are revalidated | `AUT-012` `A2A-004` `EXT-004` |
| Output redaction and secret scanning | Enforced — shell output redaction, evidence redaction scan, escape-report redaction, release secret scan (decompressed members) | `LP-011` `TestShellSecretRedactedInDisplayAndOutput` `TestRedactStripsEveryCredentialShapeTheTiersEmit` `TestSecretScanIsNotVacuous` |
| Artifact classification/quarantine | Partial — a failed sandbox destroy quarantines the host and an uncertain worker job quarantines rather than retrying; artifact **classification** is not a shipped surface | `SAN-008` `WRK-007` |
| Model provider data policy | Enforced at the routing/embedding boundary | `KNO-007` |
| Anomaly detection for encoded/high-entropy output | not claimed | not claimed |
| Revocation and incident search by fingerprint | Partial — API keys and secrets are revocable/rotatable via the admin CLI; **search by credential fingerprint** is not a shipped surface | `MCI-002` |

### 3.4 Data poisoning (§49.9)

| §49.9 requirement | Status here | Evidence |
|---|---|---|
| Knowledge/memory items retain source and checksum | Enforced | `KNO-001` |
| Index ingestion authenticates source and records revision | Enforced — ingest is immutable and versioned | `KNO-001` |
| Automated memory writes reviewable and capability-limited | Enforced — retrieval authorization is server-derived ACL-first, never a post-filtered top-k | `KNO-003` |
| Retrieved sources filterable by trust class | Enforced via classification/region policy | `KNO-007` |
| Conflicting sources remain visible | Enforced — hybrid retrieval records its strategy and cites offsets | `KNO-005` |
| Release does not auto-accept model-generated prompt changes | Enforced — a revision is immutable and a published pin is required | `AGT-001` `AGT-002` |
| Poisoning tests over repo instructions, tool descriptions, prior output | Enforced as held-out eval vectors (`malicious-repo-instruction`, `tool-description-poisoning`, `mcp-card-poisoning`, `a2a-card-poisoning`) | `QUA-003` `QUA-004` |
| A failed refresh never replaces a good index | Enforced | `KNO-002` |

### 3.5 Confused deputy prevention (§49.10)

| §49.10 binding | Status here | Evidence |
|---|---|---|
| Original principal / delegation chain | Enforced — a child never exceeds the parent intersection and a remote agent inherits no credential | `SUB-006` `SUB-007` |
| Run/attempt fence | Enforced — a late callback after fence advance is denied | `TOL-017` |
| Capability, destination, exact arguments | Enforced — a duplicate tool-call id executes once with signed halves | `TOL-016` |
| Connection identity | Enforced — a tenant-scoped connection ref resolves the credential | `MCI-007` |
| Approval decision | Enforced — only a minted, hash-bearing approval is accepted; an unmapped actor cannot approve | `SLK-004` `SLK-007` `UI-002` |
| MCP/OAuth tokens audience-bound, not forwarded | Enforced | `TOL-009` `TestMCPServerCannotReplayTokenToAnotherUpstream` `TestMCPServerCannotCallPlatformWithReceivedCredentials` |

### 3.6 SSRF and network attacks (§49.11)

| §49.11 control | Status here | Evidence |
|---|---|---|
| Normalized parsing, allowed schemes | Enforced on every server-side fetch surface | `AUT-012` `A2A-004` `EXT-004` |
| DNS + resolved-IP policy; private/link-local/metadata denial | Enforced — denial happens **after** resolution, so a rebind does not slip through | `AUT-012` `SAN-004` `EXT-004` |
| Redirect revalidation | Enforced — the A2A client revalidates each hop rather than blanket-denying | `A2A-004` `AUT-012` |
| Response size/time limits | Enforced — bounded stdout/stderr and bounded fetch | `TestStderrIsBoundedSeparatelyFromStdout` `EXT-004` |
| TLS validation | Enforced on the runner gateway (mTLS) and on outbound HTTPS | `LP-007` |
| Audit destination | Enforced — egress destinations land in the journal | `TestAuditChainCoversEveryEventColumn` |
| User-controlled Host/forwarding headers cannot change target policy | Enforced — the policy reads the resolved target, not the header | `AUT-012` |

### 3.7 Sandbox escape (§49.12)

| §49.12 mitigation | Status here | Evidence |
|---|---|---|
| Hardware virtualization for hostile multi-tenancy | not claimed | not claimed |
| No privileged mode / host namespaces / raw devices / runtime sockets | Enforced | `TestEngineReceivesNoCredentialsOrDockerSocket` `SAN-002` |
| Workspace confinement (traversal, absolute path, escaping symlink) | Enforced | `SAN-001` |
| Resource limits and bounded termination | Enforced | `SAN-003` |
| Workload network separation and metadata denial | Enforced | `SAN-004` |
| Short-lived host, no residue between allocations | Enforced | `SAN-005` `SAN-007` |
| Immutable verified images | Enforced — digest-pinned bases, no mutable tag in an installer or manifest | `TestEveryDockerfileBaseIsDigestPinned` |
| Escape detection / quarantine | Enforced — a failed destroy quarantines the host and denies new placement; a suspected escape fences the runner | `SAN-006` `SAN-008` `SAN-011` |
| Separate runner identity from workload | Enforced | `TestEngineReceivesNoCredentialsOrDockerSocket` |
| Runtime/kernel vulnerability response | Partial — the release pipeline scans images against a **pinned** vulnerability DB snapshot and blocks Criticals; a continuous live feed is an operator leg | `TestVulnGateBlocksCriticalFinding` `TestVulnDBSnapshotIsPinnedAndDated` |
| The whole corpus runs as one escape suite with one report | Enforced | `TestSandboxEscapeSuite` `TestEveryMaterializedSANCaseIsInTheSuite` `TestAnArmThatRanNothingIsNotAPass` |

### 3.8 Denial of service and cost exhaustion (§49.13)

| §49.13 control | Status here | Evidence |
|---|---|---|
| Auth and rate limit before model/sandbox allocation | Enforced — 429 before compute | `MCI-005` |
| Bounded queues and per-tenant fairness | Enforced — bounded memory under flood with visible depth | `AUT-010` `TestFloodBoundsMemoryReportsDepthApplies429` |
| Cost reservation | Enforced | `MOD-011` |
| Sandbox resource limits | Enforced | `SAN-003` |
| Tool/provider circuit breakers | Enforced — and a caller-invalid error never trips the shared circuit | `MOD-012` `EXT-005` |
| Subagent fan-out limits | Enforced | `TestAdmitChildDepthAndFanoutBounded` |
| Webhook loop detection | Enforced — bot and self events are ignored | `SLK-008` |
| Decompression/archive bombs blocked | Enforced for skill archives | `TOL-011` |
| Per-operation deadlines | Enforced — a hook that times out fails closed | `TOL-012` |
| Slow-consumer / connection limits | Partial — SSE reconnect and bounded buffers are measured by the performance harness under a **macOS + Docker Desktop** profile with no SLO claim | `TestPER003LongSessionSoak` `TestPER004BurstTriggerQueue` |
| Size/depth/schema limits before expensive parsing | Enforced at admission | `MCI-005` `MOD-004` |

### 3.9 Model output handling (§49.14)

| §49.14 rule | Status here | Evidence |
|---|---|---|
| JSON schema-validated | Enforced — tool arguments and engine frames validate before dispatch | `EXT-001` `TOL-016` |
| Code is not executed outside the sandbox | Enforced | `SAN-002` `TestEngineReceivesNoCredentialsOrDockerSocket` |
| URLs are not auto-fetched | Enforced — a fetch is a capability-gated tool call, never a side effect of output | `EXT-004` |
| Shell commands parsed/displayed and policy checked | Enforced — argv form, redacted display, policy hook | `SAN-002` `TOL-012` `TestShellSecretRedactedInDisplayAndOutput` |
| Citations verified against source records | Enforced — offsets are verifiable against the stored source | `KNO-001` `KNO-005` |
| File paths normalized | Enforced | `SAN-001` |
| UI distinguishes model claims from platform receipts | Enforced — the console shows the authoritative approval detail and a display can never replace it; a changeset comes from the tool ledger | `UI-002` `REP-005` |
| HTML/Markdown rendered with sanitization | Partial — the console renders text content only and is `preview`; there is no rich-HTML rendering surface to sanitize | `UI-001` |

### 3.10 Browser and computer use (§49.15)

**Entirely not claimed.** No browser tool, no computer-use tool, no durable browser profile, no
clipboard or screenshot surface exists in this tree. §49.15 is a design section with no implementation
to model.

| §49.15 control | Status here | Evidence |
|---|---|---|
| Fresh browser profile per tenant/run | not claimed | not claimed |
| Download/upload artifact policy | not claimed | not claimed |
| Clipboard isolation | not claimed | not claimed |
| Local-network and metadata denial for browser navigation | not claimed | not claimed |
| Brokered credential autofill | not claimed | not claimed |
| Screenshot/video classification inheritance | not claimed | not claimed |
| Destructive click/form/payment approval | not claimed | not claimed |

The nearest implemented surface is the research tool's fetch+cite path with resolved-IP and redirect
denial (`EXT-004`) — which is a server-side fetch, not a browser.

### 3.11 Multi-agent security (§49.16)

| §49.16 rule | Status here | Evidence |
|---|---|---|
| Child/remote agent identity explicit | Enforced | `SUB-007` `A2A-002` |
| Capability never exceeds the parent intersection | Enforced | `SUB-006` `TestAdmitChildDepthAndFanoutBounded` |
| Shared workspace write off by default | Enforced — the child gets an isolated worktree and a conflict-aware merge | `SUB-006` `REP-011` |
| Remote agent cannot request parent secret | Enforced structurally — the remote resolver is the only bearer source, there is no parent-token field | `A2A-005` `SUB-007` |
| Parent treats child output as untrusted | Enforced | `A2A-005` `SUB-007` |
| Delegation chain and cost visible | Enforced — a detached child returns a typed result on the durable spine | `DEL-001` `DET-001` `DET-002` |
| Colluding children cannot exceed aggregate limits | Enforced — fan-out and depth are admitted against the parent's remaining budget | `TestAdmitChildDepthAndFanoutBounded` |
| Depth/fan-out enforced outside model control | Enforced | `TestChildRunDepthAndFanoutBounded` |

### 3.12 Security defaults (§49.17)

| §49.17 default | Status here | Evidence |
|---|---|---|
| No public registration for self-host | Enforced — provisioning is an admin-CLI/API operation under a scoped key | `MCI-001` |
| No broad internet in high-trust profiles | Enforced — sandbox egress denies metadata and private targets | `SAN-004` `EXT-004` |
| No protected-branch push/merge/release without approval | Enforced — push is approved, base movement is refused, a duplicate PR opens one | `REP-006` `REP-008` `REP-010` |
| No recursive delegation | Enforced by the depth bound | `TestAdmitChildDepthAndFanoutBounded` |
| No arbitrary skill/plugin install | Enforced — a malicious archive is rejected and its instructions grant nothing | `TOL-011` |
| No support content access | not claimed | not claimed |
| No secret in model context | Enforced | `MCI-002` `TestSecretRefIsRedeemedOnlyInExecutorAndNeverLeaks` |
| No direct Docker socket in workload | Enforced | `TestEngineReceivesNoCredentialsOrDockerSocket` |
| No mutable image tags | Enforced — every `FROM` is digest-pinned and the guard rejects an unpinned fixture | `TestEveryDockerfileBaseIsDigestPinned` `TestPinGuardRejectsUnpinnedFixtures` |
| No provider/model fallback across data policy | Enforced — the capability hard-filter never relaxes before admission | `MOD-004` `KNO-007` |
| Approvals expire | Enforced | `APV-001` |
| Audit enabled | Enforced — an append-only journal whose chain is recomputable from the rows against a signed out-of-DB anchor | `TestAuditIntegrityFourArms` `TestIntactJournalVerifiesGreen` |
| Managed hostile code uses microVM isolation | not claimed | not claimed |
| Unsafe development overrides visibly labeled and not model-enablable | Enforced — the production posture audit refuses an unsafe overlay | `OPS-002` |

---

## 4. Audit and integrity (§50, the part §49 leans on)

The audit journal is the backstop under most rows above: if a control is bypassed, the journal is what
shows it. Its own integrity is therefore in the threat model.

| Property | Status here | Evidence |
|---|---|---|
| Append-only journal covering every audited column | Enforced — the chain digest covers every column, and a new column that is not chained fails the guard | `TestAuditChainCoversEveryEventColumn` `TestEveryRowFieldIsInTheDigest` |
| Integrity recomputed from the rows, not from a stored hash | Enforced | `TestIntactJournalVerifiesGreen` `TestAuditIntegrityFourArms` |
| Anchor lives outside the mutable store and is signed | Enforced — a signed checkpoint file, verified against an out-of-band public key; an in-bundle key is refused | `TestCheckpointSignatureFailClosed` `TestPubkeyContainmentSurvivesSymlinks` |
| A removed row raises `gap`; a changed byte raises `tamper` | Enforced, and both exit non-zero | `TestDeletedRowRaisesGap` `TestFlippedPayloadByteRaisesTamper` `TestAuditVerifyCommandExitsNonZeroOnTamper` |
| A stale or vacuous checkpoint is not silently green | Enforced | `TestAnOldValidlySignedCheckpointIsStale` `TestAVacuousCheckpointIsRefused` |
| **A retention purge is indistinguishable from tampering** | **Known trap, pinned by a test, not fixed.** `scrub_events` rewrites an anchored row's payload instead of deleting it, so on a stack with a store:false TTL configured the reaper raises the highest-severity alert during routine maintenance — and an attacker gets perfect cover. See `docs/operations/runbooks/audit-integrity-alert.md` and the `AUD-1` row of `docs/operations/known-gaps-1.0.md` | `TestARetentionPurgeIsIndistinguishableFromTamper` |
| Audit read refuses a row-level-scoped connection | Enforced — integrity verification runs system-scoped or not at all | `TestAuditReadRefusesARowLevelScopedConnection` |

---

## 5. Everything this document does not claim, collected

The rows above are scattered; this is the same set in one place, so nobody has to trust that the
`not claimed` cells were read.

| Not claimed | Why | Where it lives |
|---|---|---|
| Managed operator ↔ tenant boundary, support access | No managed cell exists | SaaS plan (`MASTER-SPEC.md` §9) |
| microVM / hardware-virtualized hostile isolation | The sandbox is OCI | Managed-scope; E18 T10 marks it in the release index |
| Browser and computer use (all of §49.15) | No browser tool exists | Not planned in this program |
| Anomaly detection on encoded/high-entropy output | No detector exists | `known-gaps-1.0.md` |
| Incident search by credential fingerprint | Not a shipped surface | `known-gaps-1.0.md` |
| Artifact classification labels | Not a shipped surface | `known-gaps-1.0.md` (`DAT` depth) |
| End-user token exchange (`TEN-004`) | Never built | `known-gaps-1.0.md` |
| Cross-region failover | Single-node DR only | `docs/operations/dr-report.md`, E18 §6 |
| Live CVE feed / continuous scanning | Pinned DB snapshot only | E18 §6 leg 1 |
| Transparency-log-backed signing identity | openssl P-256, not Sigstore | E18 §6 leg 1 |
| Real-model security-eval quality numbers | E08 rule: no tools are exposed to a real provider | E18 §6 leg 5 |
| Reference-hardware performance under a stated SLO | Local profile numbers only | E18 §6 leg 3 |

## 6. Related documents

- `docs/security/release-policy.md` — release identity, two-person promotion, signing key boundary, revocation.
- `docs/security/vulnerability-process.md` — how a report becomes a triaged severity, an advisory and a rebuild.
- `docs/operations/known-gaps-1.0.md` — the RC disposition of every open finding, including the ones named above.
- `docs/operations/runbooks/` — what an operator does when one of these controls fires.
