# Palai Technology Evidence Spikes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Palai'nin production package yapısını kilitlemeden önce control-plane runtime, PostgreSQL coordinator, contract generation, runner transport/OCI supervision, Next.js streaming, local object storage ve build orchestration kararlarını tekrarlanabilir kanıtlarla kabul etmek.

**Architecture:** Her spike bağımsız bir executable test harness ve ortak, content-free JSON report üretir. Quick test profili her PR'da deterministik çalışır; 1,000 SSE connection, real PostgreSQL, Docker Engine, Next production server ve S3 conformance içeren evidence profili E01 promotion sırasında çalışır. ADR-0001..0005 yalnızca report assertion'ları green ve hard rejection kriterleri kapalıysa `accepted` olur.

**Tech Stack:** Go 1.26.4, Node.js 22.22.2, TypeScript 7.0.2, Next.js 16.2.10, React 19.2.7, PostgreSQL 16 OCI, pgx 5.10.0, coder/websocket 1.8.15, Moby client 0.5.0, Docker Engine API negotiation, JSON Schema 2020-12, OpenAPI 3.2/3.1.2, uv/datamodel-code-generator 0.68.1, json-schema-to-typescript 15.0.4, go-jsonschema 0.23.1, SeaweedFS 4.39, AWS SDK for Go v2 S3 1.105.1.

---

## 1. Hard gates and report contract

No candidate is accepted when any of these is true:

- public contract loses omitted/null/empty or unknown enum/field semantics;
- PostgreSQL accepts a stale fence or produces more than one authoritative completion;
- SSE or stderr buffering grows without a configured bound;
- engine receives provider, database, object-store, Docker, or runner credentials;
- runner opens an inbound listener or skips mTLS peer verification;
- Next.js emits the server credential into browser-visible output;
- local object-store artifact lacks an immutable digest, required architectures, offline mirror path, maintained source, or license compatible with public self-host distribution;
- an evidence report lacks exact source/tool/image/environment identity.

Every report uses this shape:

```json
{
  "schema_version": 1,
  "spike": "control-plane-runtime",
  "git_commit": "40-hex-sha",
  "source_tree": "40-hex-git-tree",
  "started_at": "RFC3339Nano",
  "ended_at": "RFC3339Nano",
  "environment": {
    "os": "darwin",
    "arch": "arm64",
    "tool_versions": {"go": "go1.26.4"},
    "image_digests": []
  },
  "metrics": {"go.idle_rss_bytes": 0},
  "assertions": [
    {"name": "go.reconnect_exact", "passed": true, "detail": "100/100"}
  ],
  "passed": true
}
```

`passed` is derived from assertions and cannot be supplied independently. Reports contain no prompts, response content, credentials, raw environment, hostnames, usernames, absolute user paths, or network tokens.

Reports are generated only from a clean source commit. `git_commit` records the commit executed; `source_tree` records that commit's Git tree and is the rebase-stable verification identity. Report files are added in the immediately following evidence-only commit. After a rebase merge, `scripts/spikes/check-reports` requires a commit in current history with the same `source_tree`; release evidence later uses the exact immutable release commit in addition to its tree.

### Task 1: Common spike report and deterministic command surface

**Files:**

- Create: `spikes/internal/report/report.go`
- Test: `spikes/internal/report/report_test.go`
- Create: `scripts/spikes/run`
- Create: `scripts/spikes/check-reports`
- Test: `scripts/test/spikes.sh`
- Create: `spikes/manifest.json`
- Modify: `Makefile`
- Modify: `.gitignore`

- [x] **Step 1: Write failing report tests**

Create table tests for:

```go
func TestFinalizeDerivesPassFromEveryAssertion(t *testing.T)
func TestValidateRejectsMissingCommitAndUnboundedMetric(t *testing.T)
func TestMarshalStableOrdersMapsAndOmitsHostIdentity(t *testing.T)
func TestReportRejectsSecretLikeValues(t *testing.T)
```

The public spike API is:

```go
type Assertion struct {
    Name   string `json:"name"`
    Passed bool   `json:"passed"`
    Detail string `json:"detail"`
}

type Report struct {
    SchemaVersion int                `json:"schema_version"`
    Spike         string             `json:"spike"`
    GitCommit     string             `json:"git_commit"`
    SourceTree    string             `json:"source_tree"`
    StartedAt     time.Time          `json:"started_at"`
    EndedAt       time.Time          `json:"ended_at"`
    Environment   Environment        `json:"environment"`
    Metrics       map[string]float64 `json:"metrics"`
    Assertions    []Assertion        `json:"assertions"`
    Passed        bool               `json:"passed"`
}

func (r *Report) Finalize() error
func (r Report) MarshalStable() ([]byte, error)
```

- [x] **Step 2: Run and verify RED**

Run: `go test ./spikes/internal/report -v`

Expected: FAIL because the report package does not exist.

- [x] **Step 3: Implement the minimum report library**

Validate the exact 40-character lowercase commit and source tree, non-empty spike/tool identity, finite non-negative metrics, unique assertion names, monotonic timestamps and secret markers. Stable marshaling sorts tool versions, metrics, image digests and assertions before `json.MarshalIndent`.

- [x] **Step 4: Add spike commands**

`spikes/manifest.json` lists all six spike IDs with `planned` or `implemented` status. `scripts/spikes/run quick` runs every implemented unit-sized profile without Docker/network and prints every planned ID explicitly; planned status is permitted only during incremental E01 work. `scripts/spikes/run evidence` rejects planned entries, runs all E01 harnesses and writes temporary raw reports below `spikes/.evidence/`. `scripts/spikes/check-reports` validates committed redacted reports under `spikes/reports/`. Add `make test-spikes` and report checking to `make verify`; `make evidence-spikes` is the E01 promotion gate and raw evidence remains ignored.

- [x] **Step 5: Verify and commit**

Run: `go test ./spikes/internal/report -count=20 && make test-spikes && make verify`

Expected: report tests PASS; quick output names implemented and planned entries without silently skipping either; evidence mode returns `spike_not_implemented` while any planned entry remains.

```bash
git add Makefile .gitignore spikes/internal scripts/spikes
git commit -m "test: add deterministic technology spike reports"
```

### Task 2: Go versus Node control-plane SSE evidence

**Files:**

- Create: `spikes/control-plane/go-server/main.go`
- Create: `spikes/control-plane/node-server/package.json`
- Create: `spikes/control-plane/node-server/tsconfig.json`
- Create: `spikes/control-plane/node-server/server.ts`
- Create: `spikes/control-plane/load/main.go`
- Create: `spikes/control-plane/load/harness.go`
- Test: `spikes/control-plane/load/harness_test.go`
- Test: `spikes/control-plane/load/main_test.go`
- Create: `spikes/control-plane/README.md`
- Create: `scripts/spikes/control-plane-runtime`
- Modify: `pnpm-workspace.yaml`
- Modify: `pnpm-lock.yaml`
- Modify: `pyproject.toml`
- Modify: `uv.lock`
- Generate: `spikes/reports/control-plane-runtime.json`

- [x] **Step 1: Write failing protocol/load tests**

Test both candidate executables through the same black-box contract:

```text
GET /healthz returns 200 only after listener readiness
GET /events emits id/event/data and then heartbeat comments
Last-Event-ID=1 resumes at event 2 without replaying event 1
SIGTERM closes listeners within 5 seconds
client disconnect never creates a cancel request
bounded quick profile connects 25 clients and reconnects 10 exactly
```

- [x] **Step 2: Run and verify RED**

Run: `go test ./spikes/control-plane/load -run 'TestCandidate|TestReconnect' -v`

Expected: FAIL because candidate commands are absent.

- [x] **Step 3: Implement semantically identical candidates**

Both servers accept an OS-assigned port, write one machine-readable readiness line to stdout and human diagnostics to stderr. They serve a retained two-event fixture and hold the connection open with 15-second heartbeats. Go uses `net/http`; the TypeScript 7.0.2 candidate compiles to Node and uses typed `node:http` APIs without a web framework. No framework-specific API may enter the harness.

- [x] **Step 4: Implement the load and restart harness**

The evidence profile:

```text
connections_per_candidate = 1000
explicit_reconnects = 100
restart_cycles = 1
connect_deadline = 30s
shutdown_deadline = 5s
maximum_error_count = 0
maximum_go_idle_rss = 128 MiB
maximum_node_idle_rss = 256 MiB
```

Capture time-to-ready, RSS before/after 1,000 connections, reconnect completion, sequence duplicates/gaps and shutdown time. Do not commit PID, local port or absolute path.

- [x] **Step 5: Run repeated quick profiles and commit the clean source**

Run: `go test ./spikes/control-plane/load -count=10 && scripts/spikes/control-plane-runtime quick`

Expected: both candidates meet the bounded semantic profile ten consecutive times. Commit candidate and harness sources before generating evidence so the report can bind to a clean source commit.

```bash
git add pnpm-lock.yaml pnpm-workspace.yaml scripts/spikes/control-plane-runtime spikes/control-plane
git commit -m "test: add control-plane runtime candidates"
```

- [x] **Step 6: Generate, validate and commit evidence**

Run from the clean source commit:

```bash
PALAI_SPIKE_REPORT_OUT=spikes/.evidence/control-plane-runtime.json \
  scripts/spikes/control-plane-runtime evidence
cp spikes/.evidence/control-plane-runtime.json spikes/reports/control-plane-runtime.json
```

Expected: the report contains 1,000 connected streams and 100 exact reconnects for each candidate. Update the manifest entry to `implemented`, validate source-tree ancestry and commit only the promoted report plus manifest/plan state. ADR-0001 may choose Go using packaging/resource evidence, never familiarity alone.

```bash
git add spikes/manifest.json spikes/reports/control-plane-runtime.json docs/superpowers/plans/phase-01-technology-spikes.md
git commit -m "test: record control-plane runtime evidence"
```

### Task 3: PostgreSQL lease, fence, and outbox kill proof

**Files:**

- Create: `spikes/postgres-coordinator/schema.sql`
- Create: `spikes/postgres-coordinator/store.go`
- Create: `spikes/postgres-coordinator/worker.go`
- Create: `spikes/postgres-coordinator/worker_process_test.go`
- Create: `spikes/postgres-coordinator/store_test.go`
- Create: `spikes/postgres-coordinator/cmd/report/main.go`
- Create: `spikes/postgres-coordinator/README.md`
- Create: `scripts/spikes/postgres-coordinator`
- Generate: `spikes/reports/postgres-coordinator.json`
- Modify: `go.mod`
- Modify: `go.sum`

- [x] **Step 1: Write failing real-PostgreSQL tests**

Tests require `PALAI_SPIKE_POSTGRES_URL` and assert:

```go
func TestKilledWorkerIsReclaimedWithHigherFence(t *testing.T)
func TestStaleCompletionCannotWriteResultOrOutbox(t *testing.T)
func TestOneAuthoritativeCompletionAndOutbox(t *testing.T)
func TestTransactionKillLeavesClaimRecoverable(t *testing.T)
```

- [x] **Step 2: Run and verify RED**

Run: `scripts/spikes/postgres-coordinator test`

Expected: PostgreSQL container becomes healthy, then tests FAIL because schema/store are absent. The script always removes its named container and volume on exit.

- [x] **Step 3: Implement the minimal fenced store**

Use PostgreSQL 16 by immutable digest and pgx 5.10.0. `jobs` has `status`, `lease_owner`, `lease_expires_at`, `fence`, `attempt_count`, `result_hash`; `outbox` has a unique `(job_id, fence, event_type)`. Claim uses database time, `FOR UPDATE SKIP LOCKED`, increments the fence and commits before work. Completion updates only `WHERE id=$1 AND fence=$2 AND lease_owner=$3 AND status='running'`, then inserts outbox in the same transaction.

- [x] **Step 4: Prove real kill behavior**

The parent test process starts worker A, waits for a committed claim receipt, sends `SIGKILL`, waits for DB lease expiry, starts worker B and submits A's stale callback. Required evidence is fence B > fence A, stale affected rows = 0, one completed row, one result hash and one outbox row.

- [x] **Step 5: Verify repeatedly and commit the clean source**

Run: `scripts/spikes/postgres-coordinator quick && PALAI_SPIKE_RACE=1 scripts/spikes/postgres-coordinator test`

Expected: all four fault assertions pass against the pinned PostgreSQL container and container/volume counts return to their pre-test values.

```bash
git add go.mod go.sum spikes/postgres-coordinator scripts/spikes/postgres-coordinator
git commit -m "test: prove PostgreSQL fenced coordination"
```

- [x] **Step 6: Generate, validate and commit evidence**

Run: `PALAI_SPIKE_REPORT_OUT=spikes/.evidence/postgres-coordinator.json scripts/spikes/postgres-coordinator evidence`

Expected: all four fault assertions PASS for 20 iterations; container and volume counts return to their pre-test values.

```bash
git add spikes/manifest.json spikes/reports/postgres-coordinator.json docs/superpowers/plans/phase-01-technology-spikes.md
git commit -m "test: record PostgreSQL coordinator evidence"
```

### Task 4: Contract-generation semantic bake-off

**Files:**

- Create: `spikes/contracts/schemas/fixture.json`
- Create: `spikes/contracts/fixtures/omitted.json`
- Create: `spikes/contracts/fixtures/null.json`
- Create: `spikes/contracts/fixtures/empty.json`
- Create: `spikes/contracts/fixtures/unknown.json`
- Create: `spikes/contracts/fixtures/invalid.json`
- Create: `spikes/contracts/openapi-3.2.yaml`
- Create: `spikes/contracts/generate.sh`
- Create: `spikes/contracts/generator/`
- Create: `spikes/contracts/candidate-findings.json`
- Create: `spikes/contracts/cmd/candidate-check/main.go`
- Create: `spikes/contracts/cmd/report/main.go`
- Create: `spikes/contracts/tooling/package.json`
- Create: `spikes/contracts/semantic_check.go`
- Test: `spikes/contracts/semantic_check_test.go`
- Create: `scripts/spikes/contract-toolchain`
- Generate: `spikes/contracts/generated/`
- Generate: `spikes/reports/contract-toolchain.json`
- Modify: `pnpm-workspace.yaml`
- Modify: `pnpm-lock.yaml`

- [x] **Step 1: Write failing semantic corpus tests**

The source fixture contains a required ID, optional nullable `note`, open `status`, open object fields, RFC3339 timestamp and 64-bit sequence. Tests require every language projection to preserve:

```text
omitted note != explicit null note != empty note
unknown status round-trips byte-semantically
unknown object field survives decode/encode
sequence 9007199254740993 is not rounded in Go/Python and is represented safely in TypeScript
canonical OpenAPI 3.2 and generated 3.1.2 accept/reject the same corpus
second generation produces zero diff
```

- [x] **Step 2: Run and verify RED**

Run: `go test ./spikes/contracts -v`

Expected: FAIL because schemas, projections and generated packages are absent.

- [x] **Step 3: Execute pinned generator candidates**

Run json-schema-to-typescript 15.0.4, datamodel-code-generator 0.68.1, go-jsonschema 0.23.1 and oapi-codegen 2.7.2 against isolated output directories. Record tool exit, input dialect, emitted types and each semantic assertion. A tool may be retained as a partial backend even when a wrapper is required; a silent semantic loss is a hard rejection.

- [x] **Step 4: Implement the minimum lossless proof path**

Build a small canonical-schema IR and deterministic templates only for the corpus constructs. Use explicit optional wrappers where the target language cannot distinguish missing and null, string-backed open enums, unknown-field bags, and string/BigInt-safe TypeScript 64-bit integers. Generate OpenAPI 3.1.2 mechanically from the 3.2 source and compare normalized schema semantics.

- [x] **Step 5: Verify and commit the clean source**

Run: `spikes/contracts/generate.sh evidence && git diff --exit-code -- spikes/contracts/generated && scripts/spikes/contract-toolchain quick`

Expected: corpus passes in TS/Python/Go; all four external candidate results match the recorded partial/rejected semantics and second generation has zero diff.

```bash
git add pnpm-lock.yaml pnpm-workspace.yaml scripts/spikes/contract-toolchain spikes/contracts
git commit -m "test: prove lossless cross-language contracts"
```

- [x] **Step 6: Generate, validate and commit evidence**

Run from the clean source commit: `PALAI_SPIKE_REPORT_OUT=spikes/.evidence/contract-toolchain.json scripts/spikes/contract-toolchain evidence`

Expected: the corpus passes 20 consecutive times in all three languages; the report names which external candidates need wrappers and why. ADR-0002 accepts canonical JSON Schema/OpenAPI 3.2 plus a generated 3.1.2 projection and lossless generated transport types.

```bash
git add spikes/manifest.json spikes/reports/contract-toolchain.json docs/superpowers/plans/phase-01-technology-spikes.md
git commit -m "test: record cross-language contract evidence"
```

### Task 5: Outbound mTLS WebSocket runner and OCI supervisor

**Files:**

- Create: `spikes/runner/certificates.go`
- Create: `spikes/runner/controller.go`
- Create: `spikes/runner/runner.go`
- Create: `spikes/runner/supervisor.go`
- Create: `spikes/runner/engine/main.go`
- Create: `spikes/runner/engine/Dockerfile`
- Create: `spikes/runner/cmd/report/main.go`
- Create: `spikes/runner/README.md`
- Test: `spikes/runner/runner_test.go`
- Test: `spikes/runner/supervisor_test.go`
- Create: `scripts/spikes/runner`
- Generate: `spikes/reports/runner-supervisor.json`
- Modify: `go.mod`
- Modify: `go.sum`

- [x] **Step 1: Write failing transport/supervisor tests**

Cases:

```text
runner with trusted short-lived client cert connects outbound and receives one lease
missing, expired, wrong-CA, or wrong-SAN client certificate is rejected
server certificate hostname mismatch is rejected
lease contains immutable image ID/digest and explicit timeout/output bounds
engine stdout accepts valid bounded JSONL only
malformed or oversized stdout fails the attempt
stderr is separately capped and marked truncated
container receives no Docker socket, provider key, DB URL, S3 key, or runner private key
container is absent after destroy
```

- [x] **Step 2: Run and verify RED**

Run: `go test ./spikes/runner -v`

Expected: FAIL because runner/controller/supervisor are absent.

- [x] **Step 3: Implement mTLS WebSocket lease exchange**

Use coder/websocket 1.8.15. Test PKI uses an in-memory CA, exact DNS SANs and one-minute leaf lifetime. Controller requires and verifies the client certificate. Runner is always the WebSocket client and has no inbound listener. Messages carry protocol version, runner ID, run/attempt IDs, fence, image identity, deadline and resource/output limits.

- [x] **Step 4: Implement Docker SDK supervision**

Use Moby client 0.5.0 with API negotiation. `scripts/spikes/runner` cross-compiles a tiny JSONL fixture engine for the Docker daemon architecture and builds a `FROM scratch` image, then resolves and passes its immutable image ID. Create/start/wait/log/kill/remove calls have contexts and labels. Stdout and stderr use separate bounded readers. Environment is an explicit allowlist.

- [x] **Step 5: Verify and commit the clean source**

Run: `scripts/spikes/runner quick && PALAI_SPIKE_RUNNER_IMAGE_ID="$(docker image inspect palai-runner-spike:fixture --format '{{.Id}}')" go test -race ./spikes/runner -count=1`

Expected: all mTLS negative cases and OCI lifecycle assertions PASS; no labeled container remains and exactly one intentionally cached fixture image remains.

```bash
git add go.mod go.sum spikes/runner scripts/spikes/runner docs/superpowers/plans/phase-01-technology-spikes.md
git commit -m "test: prove outbound runner supervision"
```

- [x] **Step 6: Generate, validate and commit evidence**

Run from the clean source commit: `PALAI_SPIKE_REPORT_OUT=spikes/.evidence/runner-supervisor.json scripts/spikes/runner evidence`

Expected: all mTLS negative cases and OCI lifecycle assertions PASS five times; no labeled container remains and exactly one intentionally cached fixture image remains. Promote the redacted report, update the manifest entry to `implemented`, and commit only evidence state.

```bash
git add spikes/manifest.json spikes/reports/runner-supervisor.json docs/superpowers/plans/phase-01-technology-spikes.md
git commit -m "test: record outbound runner evidence"
```

### Task 6: Next.js server-only stream relay and abort behavior

**Files:**

- Create: `spikes/nextjs-streaming/package.json`
- Create: `spikes/nextjs-streaming/next.config.ts`
- Create: `spikes/nextjs-streaming/app/api/relay/route.ts`
- Create: `spikes/nextjs-streaming/lib/server-client.ts`
- Create: `spikes/nextjs-streaming/test/fake-upstream.mjs`
- Test: `spikes/nextjs-streaming/test/relay.test.mjs`
- Generate: `spikes/reports/nextjs-streaming.json`
- Modify: `pnpm-workspace.yaml`
- Modify: `pnpm-lock.yaml`

- [ ] **Step 1: Write failing production-server tests**

Node tests start a fake canonical SSE upstream and a built Next production server, then assert:

```text
route emits ordered id/event/data frames without buffering the terminal event
PALAI_SPIKE_API_KEY is present on upstream Authorization only
API key is absent from response, build output, source map, static chunk and Next log
browser abort closes the upstream transport
browser abort does not call the explicit /cancel endpoint
route forwards Last-Event-ID on reconnect
```

- [ ] **Step 2: Run and verify RED**

Run: `pnpm --dir spikes/nextjs-streaming test`

Expected: FAIL because the Next application and route are absent.

- [ ] **Step 3: Implement the server-only relay**

Pin Next 16.2.10, React/ReactDOM 19.2.7 and TypeScript 7.0.2. `lib/server-client.ts` imports `server-only`, validates only Palai-specific environment names and creates the upstream request. The Route Handler returns a Web `ReadableStream`, forwards canonical frames, propagates `Last-Event-ID`, and aborts only its upstream fetch when the browser disconnects.

- [ ] **Step 4: Verify production build and commit**

Run: `pnpm --dir spikes/nextjs-streaming build && pnpm --dir spikes/nextjs-streaming test`

Expected: all stream/abort/secret assertions PASS against `next start`; report records time-to-first-frame and exact scan targets.

```bash
git add pnpm-workspace.yaml pnpm-lock.yaml spikes/nextjs-streaming spikes/reports/nextjs-streaming.json
git commit -m "test: prove Next.js server-only streaming"
```

### Task 7: Local S3-compatible object-store selection

**Files:**

- Create: `spikes/object-store/candidates.json`
- Create: `spikes/object-store/evaluate.go`
- Create: `spikes/object-store/s3_conformance_test.go`
- Create: `scripts/spikes/object-store`
- Generate: `spikes/reports/object-store.json`
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1: Write failing candidate/conformance tests**

The evaluator rejects candidates missing source URL, SPDX license, active-maintenance receipt, immutable multi-arch manifest digest, amd64/arm64 entries, offline export/import path, health check, and S3 conformance command. Runtime conformance covers bucket create, put/get/head/delete, conditional request, multipart upload, abort, range read, SHA-256 checksum metadata and restart persistence.

- [ ] **Step 2: Run and verify RED**

Run: `go test ./spikes/object-store -v`

Expected: FAIL because candidate evidence and conformance adapter are absent.

- [ ] **Step 3: Record current primary-source candidates**

Evaluate at least:

```text
SeaweedFS 4.39: active, Apache-2.0, official S3 mode, amd64/arm64 digest
Garage current stable: active, AGPL-3.0, amd64/arm64 digest
MinIO community: archived upstream, AGPL-3.0, source-only/community distribution change
```

Registry metadata is measured live and reduced to digests/platforms in the report. Mutable tags are never deployment input. Absence of an upstream signature is explicit; Palai must mirror the selected source/image digest and sign its release artifact before E18. An unsigned upstream tag cannot be called verified merely because TLS pull succeeded.

- [ ] **Step 4: Run selected candidate S3 and offline proof**

Start SeaweedFS 4.39 by the measured manifest/platform digest with random credentials and an isolated named volume. Use AWS SDK for Go v2 S3 1.105.1 with path-style addressing. After conformance, stop and start the same container/volume and re-read retained bytes. Export the exact image to a tar archive, remove only a disposable retag, import it with networking disabled for the command, and verify the same local image ID; delete the tar in cleanup.

- [ ] **Step 5: Verify and commit**

Run: `scripts/spikes/object-store evidence spikes/reports/object-store.json`

Expected: SeaweedFS functional, persistence, multi-arch and offline assertions PASS; MinIO is rejected as the default due to archived/source-only upstream status; no candidate is falsely marked upstream-signed without signature evidence.

```bash
git add go.mod go.sum spikes/object-store spikes/reports/object-store.json scripts/spikes/object-store
git commit -m "test: select the local object store from evidence"
```

### Task 8: ADR acceptance and E01 promotion gate

**Files:**

- Create: `docs/adr/0001-language-runtime.md`
- Create: `docs/adr/0002-contract-toolchain.md`
- Create: `docs/adr/0003-runner-transport.md`
- Create: `docs/adr/0004-local-object-store.md`
- Create: `docs/adr/0005-build-orchestration.md`
- Create: `spikes/reports/index.json`
- Create: `scripts/verify/e01.sh`
- Test: `scripts/test/e01.sh`
- Modify: `README.md`
- Modify: `docs/superpowers/plans/2026-07-16-self-hosted-master-plan.md`
- Modify: `docs/superpowers/plans/phase-01-technology-spikes.md`

- [ ] **Step 1: Write the failing promotion verifier**

`scripts/test/e01.sh` proves missing, failed, tampered, wrong-commit, secret-bearing or unreferenced report/ADR fixtures are rejected. `scripts/verify/e01.sh` requires five accepted ADRs, six passing reports, SHA-256 checksums, exact report-to-ADR links and no hard-gate exception.

- [ ] **Step 2: Run and verify RED**

Run: `bash scripts/test/e01.sh`

Expected: FAIL because ADRs and report index are absent.

- [ ] **Step 3: Write evidence-backed ADRs**

Decisions and required links:

```text
ADR-0001: Go control plane/runner/CLI + Python reference engine + TypeScript-first SDK; cites SSE and PostgreSQL reports
ADR-0002: canonical JSON Schema/OpenAPI 3.2 + mechanical 3.1.2 projection + lossless per-language generation; cites contract report
ADR-0003: outbound mTLS WebSocket runner transport + versioned JSONL engine protocol + Moby SDK; cites runner report
ADR-0004: SeaweedFS 4.39 local S3 adapter by immutable digest, Palai-mirrored/signed release artifact; cites object-store report
ADR-0005: Make as stable facade over Go/uv/pnpm plus Docker-backed evidence scripts; cites all reports and Next report
```

Every ADR records rejected options, measurable consequences, scope, exact version/digest policy and revisit triggers. It cannot claim production readiness from a spike.

- [ ] **Step 4: Build and verify the report index**

Index each report path, SHA-256, spike name, pass state and owning ADR. Run: `bash scripts/test/e01.sh && bash scripts/verify/e01.sh && make verify`.

Expected: `e01_verification=PASS reports=6 adrs=5 hard_gate_exceptions=0`.

- [ ] **Step 5: Commit**

```bash
git add docs/adr README.md docs/superpowers/plans spikes/reports/index.json scripts/test/e01.sh scripts/verify/e01.sh
git commit -m "docs: accept the evidence-backed Palai technology baseline"
```

## 2. E01 exit audit

- [ ] Quick spike suite passes without Docker or external credentials.
- [ ] Evidence suite passes with real PostgreSQL, Docker Engine, Next production server and selected S3 store.
- [ ] Control-plane report proves 1,000 connections and 100 reconnects for both candidates.
- [ ] PostgreSQL report proves worker kill, higher fence, stale rejection and one authoritative outbox.
- [ ] Contract report proves omitted/null/open-enum/unknown-field/integer semantics across three languages.
- [ ] Runner report proves outbound mTLS, digest-pinned OCI, bounded output and secret isolation.
- [ ] Next report proves streaming, reconnect, abort semantics and zero credential findings.
- [ ] Object-store report proves current license/maintenance/multi-arch/digest, S3 persistence and offline availability.
- [ ] ADR-0001..0005 are accepted and linked to checksummed reports.
- [ ] `make verify`, `bash scripts/verify/e01.sh`, GitHub Foundation CI and branch-protection verification pass.
- [ ] Only after this audit may LP-0 Task 2 create canonical production contracts.
