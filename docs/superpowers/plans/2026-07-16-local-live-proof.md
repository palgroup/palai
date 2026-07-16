# Palai Local Live Proof Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Temiz bir checkout'tan local Palai stack'i başlatmak ve bir Next.js uygulamasının TypeScript SDK üzerinden gerçek bir model sağlayıcısına streaming Response göndermesini, brokered pure tool kullanmasını, strict structured output almasını ve kanonik event/usage/audit kayıtlarıyla doğrulamasını sağlamak.

**Architecture:** Aynı release commit'inden üretilen control-plane, runner ve reference-engine image'ları Docker Compose içinde çalışır. Control plane state/idempotency/event journal'ı PostgreSQL'de, artifact byte'larını S3-compatible local store'da tutar; runner outbound bağlantı ile lease alır ve engine OCI container'ını başlatır; engine bütün model/tool erişimini broker frame'leri üzerinden yapar. Next.js yalnızca server-side Route Handler içinde SDK/API key kullanır ve canonical SSE event'lerini browser'a relay eder.

**Tech Stack:** Go 1.26.x baseline, pgx v5, Go Docker SDK, Python reference engine, Node 22+ / pnpm TypeScript SDK ve Next.js App Router example, PostgreSQL, E01'de seçilen S3-compatible store, Docker Compose, JSON Schema/OpenAPI/AsyncAPI, SSE, OpenTelemetry.

---

## 1. Execution gate

Bu plan doğrudan M4/LP-0 dikey dilimidir. Aşağıdaki koşullar gerçekleşmeden Task 2 başlamaz:

- E00 bağımsız repository ve Apache-2.0 foundation tamamdır.
- E01 ADR-0001..0005 accepted durumdadır.
- Contract generator, local object store ve runner transport kararı bu dosyadaki baseline'dan farklıysa bu plan önce mekanik olarak güncellenip review edilir.
- Gerçek provider credential'ı yalnızca local SecretRef/bootstrap input olarak sağlanır; repository, shell history, command argument veya evidence içine yazılmaz.

## 2. Bu dilimin sınırı

### Dahil

- Local organization/project/API key bootstrap.
- `POST /v1/responses`, `GET /v1/responses/{id}`, response events ve cancel'ın minimum canonical behavior'ı.
- Idempotency, Problem Details, journal-backed SSE reconnect.
- PostgreSQL coordinator, one runner, one engine attempt.
- Bir direct real provider adapter; text, streaming, tool call, strict JSON result ve usage.
- Conformance-only deterministic pure tool `palai.conformance.math.add`.
- TypeScript SDK ve Next.js consumer.
- Retained response restart ve kısa UAT TTL ile `store:false` purge.
- Audit, usage ve content-free telemetry minimumu.
- Machine-verifiable live evidence bundle.

### Dahil değil

- Repository workspace, shell/file tools, durable chat commands, model switch, subagents.
- Checkpoint/workspace recovery; yalnızca API/control-plane restart durability test edilir.
- Multi-host, Kubernetes, production TLS, backup/restore ve production isolation claim.
- Python/Go SDK'ları ve ikinci provider.
- SaaS web app veya billing.

Bu sınırlar discovery response'unda maturity/availability olarak görünür; unsupported çağrı typed `capability_unavailable` döndürür.

## 3. LP-0 acceptance contract

| ID | Scenario | Required proof |
|---|---|---|
| LP-001 | Clean bootstrap | `git clone` sonrası documented command; source edit/manual SQL yok; tüm service health green |
| LP-002 | Doctor | DB, object store, runner, image digest, provider ve clock checks machine-readable green |
| LP-003 | Real streaming response | gerçek provider request ID; ordered deltas; exactly one canonical terminal response |
| LP-004 | Brokered tool | model `palai.conformance.math.add` ister; exact arguments/result events; engine provider/tool secret görmez |
| LP-005 | Strict structured output | server-side JSON Schema validation; valid typed result; usage recorded |
| LP-006 | SSE reconnect | stream transport kesilir; `Last-Event-ID` ile resume; duplicate render yok; run cancel olmaz |
| LP-007 | Idempotent create | aynı key/request tek response/run/model dispatch; farklı body 409 |
| LP-008 | Retained restart | control plane restart sonrası response/events retrievable ve aynı terminal state |
| LP-009 | `store:false` purge | configured UAT TTL sonrası content yok; 410/tombstone; re-execution yok |
| LP-010 | Next.js consumer | API key server bundle/runtime'da kalır; browser canonical events ve final result görür |
| LP-011 | Secret isolation | repository, engine frames, DB content rows, events, logs, artifacts ve evidence secret scan green |
| LP-012 | Shutdown/restart | `local down` data volume'u silmez; `local up` retained resource'ı geri getirir |
| LP-013 | Usage/audit | model/tool usage, actor, route, request/run IDs ve outcome canonical records'da bulunur |
| LP-014 | Error contract | invalid auth/schema/idempotency errors RFC 9457; provider raw error/stack/secret yok |
| LP-015 | Reproducible evidence | manifest git SHA, image digest, migrations, provider adapter revision ve checksums içerir |

LP-003 ve LP-004 fake provider ile pass edilemez. CI deterministic suite ayrıca vardır; release evidence protected live environment'da üretilir.

## 4. Minimum runtime topology

```text
Browser
  │ fetch event stream
  ▼
Next.js Route Handler ── TypeScript SDK ──► Control Plane :8080
                                                │
                                 PostgreSQL ◄───┼───► S3-compatible store
                                                │ outbound runner session
                                                ▼
                                           Local Runner
                                                │ Docker Engine API
                                                ▼
                                      Reference Engine OCI
                                                │ JSONL broker frames
                                                ▼
                                  Model broker / Tool broker
                                                │
                                         Real provider API
```

Provider credential control plane secret backend'indedir. Engine container'ın environment'ında provider key, database URL, S3 credential, runner certificate veya Docker socket bulunmaz.

## 5. Task plan

### Task 1: Repository, toolchains ve deterministic commands

**Files:**

- Create: `LICENSE`
- Create: `README.md`
- Create: `Makefile`
- Create: `go.mod`
- Create: `pyproject.toml`
- Create: `package.json`
- Create: `pnpm-workspace.yaml`
- Create: `.tool-versions`
- Create: `.github/workflows/ci.yml`
- Create: `scripts/verify/repository-boundary.sh`
- Modify: `.gitignore`

- [ ] **Step 1: Repository-boundary failing check'i yaz**

`scripts/verify/repository-boundary.sh` şu assertions'ı yapar:

```bash
#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
test "$root" = "$PWD"
test "$(git remote get-url origin)" = "https://github.com/palgroup/palai.git"
test ! -f .gitmodules
git ls-files --stage | awk '$1 == "160000" { exit 1 }'
```

- [ ] **Step 2: Check'i çalıştır ve eksik foundation nedeniyle fail gördüğünü kaydet**

Run: `bash scripts/verify/repository-boundary.sh`

Expected: remote URL normalization farklıysa veya dosya executable değilse FAIL; neden açıkça görünür.

- [ ] **Step 3: Toolchain foundation'ı minimum içerikle oluştur**

- Go toolchain `go.mod` içinde accepted ADR version'ına pinlenir.
- Python workspace `uv` ile lock edilir; engine kendi package boundary'sini korur.
- pnpm workspace yalnızca `sdks/typescript` ve `examples/nextjs-sdk` içerir.
- `Makefile` targets: `bootstrap`, `generate`, `check-generated`, `lint`, `test-unit`, `test-component`, `test-e2e`, `verify`, `local-up`, `local-down`, `local-doctor`, `uat-local-live`.
- `bootstrap` dependency kurar ama provider credential istemez.
- [ ] **Step 4: Foundation verification çalıştır**

Run: `make bootstrap && make verify`

Expected: Henüz test paketi olmasa bile toolchain/license/repository checks PASS; command unknown veya network-floating dependency yok.

- [ ] **Step 5: Commit**

```bash
git add LICENSE README.md Makefile go.mod pyproject.toml package.json pnpm-workspace.yaml .tool-versions .github scripts .gitignore
git commit -m "chore: establish independent Palai toolchains"
```

### Task 2: Canonical minimum schemas ve generated contract fixtures

**Files:**

- Create: `protocols/schemas/common/problem.json`
- Create: `protocols/schemas/common/resource.json`
- Create: `protocols/schemas/execution/response.json`
- Create: `protocols/schemas/execution/event.json`
- Create: `protocols/schemas/execution/usage.json`
- Create: `protocols/openapi/openapi-3.2.yaml`
- Create: `protocols/asyncapi/asyncapi-3.1.yaml`
- Create: `protocols/fixtures/response-create.json`
- Create: `protocols/fixtures/events.jsonl`
- Create: `scripts/contracts/generate`
- Create: `scripts/contracts/check`
- Test: `tests/conformance/contracts/contracts_test.go`

- [ ] **Step 1: Failing fixture tests yaz**

Test assertions:

```go
func TestResponseFixtureRoundTripsWithoutLosingOmittedFields(t *testing.T)
func TestUnknownEventFieldIsIgnoredAndUnknownTypeIsPreserved(t *testing.T)
func TestProblemRequiresStableCodeAndRequestID(t *testing.T)
func TestSessionEventSequenceMustBePositive(t *testing.T)
```

- [ ] **Step 2: Testi fail durumda çalıştır**

Run: `go test ./tests/conformance/contracts -run 'Test(Response|Unknown|Problem|Session)' -v`

Expected: schema/generated package bulunmadığı için FAIL.

- [ ] **Step 3: Minimum canonical schemas yaz**

`response.json` en az şu alanları ve ayrımı tanımlar:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://schemas.palai.dev/execution/response.json",
  "type": "object",
  "required": ["id", "object", "status", "created_at", "model", "output", "usage"],
  "properties": {
    "id": {"type": "string", "pattern": "^resp_[A-Za-z0-9_-]+$"},
    "object": {"const": "response"},
    "status": {"type": "string"},
    "model": {"type": "string"},
    "output": {"type": "array"},
    "usage": {"$ref": "usage.json"},
    "error": {"oneOf": [{"$ref": "../common/problem.json"}, {"type": "null"}]}
  },
  "additionalProperties": true
}
```

Create request schema `model`, `input`, `tools`, `output`, `store`, `stream`, `metadata` ve omitted/null kurallarını ayrı tanımlar. SDK types projection'dan üretilir; handwritten duplicate type yasaktır.

- [ ] **Step 4: OpenAPI/AsyncAPI ve generated types üret**

Run: `make generate && make check-generated`

Expected: second generation zero diff; canonical/projection semantic check PASS.

- [ ] **Step 5: Contract tests çalıştır**

Run: `go test ./tests/conformance/contracts -v`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add protocols packages/contracts scripts/contracts tests/conformance/contracts
git commit -m "feat: define minimum public execution contracts"
```

### Task 3: Minimum PostgreSQL schema ve transaction repository

**Files:**

- Create: `storage/migrations/000001_core.up.sql`
- Create: `storage/migrations/000001_core.down.sql`
- Create: `storage/queries/responses.sql`
- Create: `storage/queries/events.sql`
- Create: `storage/queries/jobs.sql`
- Create: `packages/coordinator/store.go`
- Create: `apps/control-plane/internal/store/postgres.go`
- Test: `tests/component/postgres/migration_test.go`
- Test: `tests/component/postgres/transition_test.go`

- [ ] **Step 1: Failing migration/invariant tests yaz**

Required assertions:

```text
one project owns every response/session/run row
session sequence is unique and strictly allocated in a transaction
run terminal state cannot return to non-terminal
one active attempt fence exists per run
idempotency scope + key is unique
job lease fence increases after expiry/reclaim
usage dedupe key is unique
audit records are append-only to application role
```

- [ ] **Step 2: Real PostgreSQL üzerinde fail'i doğrula**

Run: `make test-component TEST=postgres`

Expected: migration files/tables bulunmadığı için FAIL.

- [ ] **Step 3: Minimum tables oluştur**

Migration aşağıdaki tables ve foreign-key/unique/check constraints'i oluşturur:

```text
organizations, projects, principals, api_keys
idempotency_records
sessions, responses, messages, runs, attempts
session_sequences, events
durable_jobs, job_attempts, outbox, inbox
runner_pools, runners, runner_leases
model_connections, model_routes, model_route_revisions
tool_calls
artifacts
usage_events, audit_events
schema_migrations
```

Customer content JSONB olabilir; secret value plain column olarak bulunamaz. Timestamps DB time ile set edilir. Application queries tenant scope olmadan sonuç döndüremez.

- [ ] **Step 4: Transaction helpers uygula**

`postgres.go` public transition callback'ini transaction, event append ve outbox insert ile tek unit yapar. Callback DB commit olmadan response döndüremez.

- [ ] **Step 5: Component tests çalıştır**

Run: `make test-component TEST=postgres`

Expected: migration apply/rollback/reapply ve invariants PASS.

- [ ] **Step 6: Commit**

```bash
git add storage packages/coordinator apps/control-plane/internal/store tests/component/postgres
git commit -m "feat: add durable PostgreSQL execution spine"
```

### Task 4: Pure lifecycle state machines ve event transaction

**Files:**

- Create: `packages/state-machines/run.go`
- Create: `packages/state-machines/attempt.go`
- Create: `packages/state-machines/tool_call.go`
- Create: `packages/state-machines/response.go`
- Test: `packages/state-machines/run_test.go`
- Test: `packages/state-machines/property_test.go`

- [ ] **Step 1: Invalid/valid transition table tests yaz**

```go
func TestRunTerminalityIsMonotonic(t *testing.T)
func TestAttemptRequiresIncreasingFence(t *testing.T)
func TestToolCallUncertainCannotBecomeCompletedWithoutReconciliation(t *testing.T)
func TestEveryRunTransitionProducesExactlyOnePublicEvent(t *testing.T)
```

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./packages/state-machines -v`

Expected: transition functions undefined nedeniyle FAIL.

- [ ] **Step 3: Minimum explicit transition tables uygula**

Public interface:

```go
type Transition[S comparable, C comparable] struct {
    From  S
    Command C
    To    S
    Event string
}

func Apply[S comparable, C comparable](current S, command C, table []Transition[S, C]) (S, string, error)
```

Invalid transition stable `invalid_state` domain error döndürür. Terminality veya fence kuralı handler içinde dağınık `if` bloklarıyla tekrar edilmez.

- [ ] **Step 4: Unit/property tests çalıştır**

Run: `go test ./packages/state-machines -count=100`

Expected: PASS; randomized sequence hiçbir terminal rollback veya duplicate authoritative transition üretmez.

- [ ] **Step 5: Commit**

```bash
git add packages/state-machines
git commit -m "feat: add deterministic execution state machines"
```

### Task 5: API auth, RFC 9457 ve idempotent Response admission

**Files:**

- Create: `apps/control-plane/cmd/palai-control-plane/main.go`
- Create: `apps/control-plane/internal/api/router.go`
- Create: `apps/control-plane/internal/api/problem.go`
- Create: `apps/control-plane/internal/api/middleware/auth.go`
- Create: `apps/control-plane/internal/api/middleware/request_context.go`
- Create: `apps/control-plane/internal/api/middleware/idempotency.go`
- Create: `apps/control-plane/internal/api/responses.go`
- Test: `tests/conformance/api/responses_test.go`
- Test: `tests/conformance/api/errors_test.go`

- [ ] **Step 1: Contract-first failing HTTP tests yaz**

Cases: missing auth 401; invalid schema 400; missing idempotency 400; same key same body replay; same key different body 409; accepted create 202 with Location; request/API version headers.

- [ ] **Step 2: Fail'i çalıştır**

Run: `go test ./tests/conformance/api -run 'TestResponse|TestProblem' -v`

Expected: control-plane server package/handlers olmadığı için FAIL.

- [ ] **Step 3: Local bootstrap auth ve request context uygula**

- API key body/query'den kabul edilmez; `Authorization: Bearer` kullanılır.
- Stored verifier full key içermez.
- Request context verified project scope taşır; body'deki `project_id` scope değiştiremez.
- Error response `application/problem+json`, stable `code`, `request_id`, `retryable` içerir.

- [ ] **Step 4: Idempotent admission transaction'ı uygula**

Canonical request hash server defaults normalize edildikten sonra hesaplanır. Reservation, transient response/session/run ve `run.queued.v1` event'i tek transaction'da oluşur. Dispatch commit'ten önce başlamaz.

- [ ] **Step 5: HTTP tests çalıştır**

Run: `go test ./tests/conformance/api -run 'TestResponse|TestProblem' -v`

Expected: PASS; duplicate create tek run ID döndürür.

- [ ] **Step 6: Commit**

```bash
git add apps/control-plane tests/conformance/api
git commit -m "feat: admit idempotent response requests"
```

### Task 6: Event journal ve resumable SSE

**Files:**

- Create: `apps/control-plane/internal/api/events.go`
- Create: `apps/control-plane/internal/execution/journal.go`
- Create: `packages/contracts/events.go`
- Test: `tests/e2e/sse/reconnect_test.go`
- Test: `tests/e2e/sse/slow_consumer_test.go`

- [ ] **Step 1: Failing reconnect tests yaz**

Test önce üç event okur, TCP stream'i kapatır, son confirmed event ID ile reconnect eder ve terminale kadar unique IDs toplar. Disconnect'in run cancel yaratmadığını DB'den assert eder.

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/e2e/sse -v`

Expected: events endpoint olmadığı için 404/FAIL.

- [ ] **Step 3: Journal-backed SSE uygula**

- `id`, `event`, single-line JSON `data` formatı.
- `Last-Event-ID` ve `after_sequence` support.
- 15-second maximum idle heartbeat; test config daha kısa olabilir.
- Per-connection bounded buffer; slow consumer disconnect, event kaybı değil.
- Terminal event'ten sonra clean close.
- Unknown event client tarafından korunabilir.

- [ ] **Step 4: Reconnect/slow consumer tests çalıştır**

Run: `go test ./tests/e2e/sse -count=10 -v`

Expected: PASS; unique canonical sequence contiguous; process memory growth bounded.

- [ ] **Step 5: Commit**

```bash
git add apps/control-plane/internal/api/events.go apps/control-plane/internal/execution packages/contracts tests/e2e/sse
git commit -m "feat: stream resumable canonical events"
```

### Task 7: Durable coordinator ve run assignment

**Files:**

- Create: `packages/coordinator/worker.go`
- Create: `packages/coordinator/lease.go`
- Create: `apps/control-plane/internal/execution/jobs.go`
- Create: `apps/control-plane/internal/execution/reconciler.go`
- Test: `tests/fault/coordinator/worker_kill_test.go`
- Test: `tests/fault/coordinator/stale_fence_test.go`

- [ ] **Step 1: Worker-kill ve stale-fence failing tests yaz**

Test job claim sonrası worker'ı kill eder; lease expiry'den sonra ikinci worker aynı logical job'u higher fence ile claim eder; birinci worker callback'i 409 `lease_conflict` alır.

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/fault/coordinator -v`

Expected: coordinator worker/lease functions olmadığı için FAIL.

- [ ] **Step 3: Claim/heartbeat/complete/retry loop'u uygula**

Database time, bounded batch, `SKIP LOCKED`, monotonic fence, persisted attempt/ready-at, full-jitter retry ve dead-letter kullan. Worker hidden retry yapmaz; attempt sayısı canonical row'dadır.

- [ ] **Step 4: Fault tests çalıştır**

Run: `go test ./tests/fault/coordinator -count=20 -v`

Expected: PASS; one authoritative completion/event; stale callback rejected.

- [ ] **Step 5: Commit**

```bash
git add packages/coordinator apps/control-plane/internal/execution tests/fault/coordinator
git commit -m "feat: coordinate durable fenced run jobs"
```

### Task 8: Runner enrollment, outbound session ve OCI supervisor

**Files:**

- Create: `cmd/runner/main.go`
- Create: `cmd/runner/internal/enrollment.go`
- Create: `cmd/runner/internal/session.go`
- Create: `cmd/runner/internal/supervisor.go`
- Create: `adapters/sandboxes/oci/driver.go`
- Create: `adapters/sandboxes/oci/docker.go`
- Create: `protocols/runner/runner.schema.json`
- Create: `protocols/engine/engine.schema.json`
- Test: `tests/conformance/engine/handshake_test.go`
- Test: `tests/fault/runner/container_kill_test.go`
- Test: `tests/security/runner/isolation_test.go`

- [ ] **Step 1: Failing engine/runner protocol tests yaz**

Cases: one-use enrollment; reconnect with short-lived identity; handshake timeout; protocol major mismatch; oversized/malformed stdout; duplicate ID same hash accepted; changed hash violation; stderr bound; container kill terminal classification.

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/conformance/engine ./tests/fault/runner ./tests/security/runner -v`

Expected: runner/driver/supervisor implementation olmadığı için FAIL.

- [ ] **Step 3: Runner session ve enrollment uygula**

Enrollment token disk'te retained credential olmaz. Runner local keypair üretir, short-lived cert alır ve yalnızca outbound control connection açar. Lease messages run/attempt/fence/image digest/limits taşır.

- [ ] **Step 4: OCI driver ve supervisor uygula**

Docker API version negotiation kullan. Image digest verify et; mutable tag development resolution dışında kabul edilmez. Engine stdin/stdout JSONL; stderr ayrı redacted bounded stream. Container environment allowlist'tir ve provider/DB/S3/runner secrets içermez. Destroy sonunda container/volume/network allocation ID ile bulunamaz.

- [ ] **Step 5: Protocol/fault/security tests çalıştır**

Run: `go test ./tests/conformance/engine ./tests/fault/runner ./tests/security/runner -count=5 -v`

Expected: PASS; container kill attempt lost/failed olarak görünür, false success yok.

- [ ] **Step 6: Commit**

```bash
git add cmd/runner adapters/sandboxes/oci protocols/runner protocols/engine tests/conformance/engine tests/fault/runner tests/security/runner
git commit -m "feat: supervise engines on an outbound local runner"
```

### Task 9: Python reference engine safe-boundary loop

**Files:**

- Create: `engines/reference/pyproject.toml`
- Create: `engines/reference/src/palai_engine/__main__.py`
- Create: `engines/reference/src/palai_engine/protocol.py`
- Create: `engines/reference/src/palai_engine/loop.py`
- Create: `engines/reference/src/palai_engine/context.py`
- Create: `engines/reference/src/palai_engine/output.py`
- Create: `engines/reference/tests/test_protocol.py`
- Create: `engines/reference/tests/test_loop.py`
- Create: `engines/reference/Dockerfile`

- [ ] **Step 1: Failing protocol/loop tests yaz**

Cases: hello→ready; run.start before hello denied; model.request stable ID; tool.request stable ID; tool result resumes; cancellation at safe boundary; one terminal frame; stdout contains JSON only.

- [ ] **Step 2: Fail'i doğrula**

Run: `uv run --project engines/reference pytest engines/reference/tests -q`

Expected: package/modules absent nedeniyle FAIL.

- [ ] **Step 3: Minimal deterministic loop uygula**

Loop states: `awaiting_start`, `before_model`, `awaiting_model`, `awaiting_tools`, `validating_output`, `terminal`. Model/tool kararları yalnızca supervisor result frame'leriyle ilerler. Provider SDK import edilmez. Human logs stderr'e structured/redacted yazılır.

- [ ] **Step 4: Engine tests ve image smoke çalıştır**

Run: `uv run --project engines/reference pytest engines/reference/tests -q && docker build -t palai-reference-engine:test engines/reference`

Expected: tests PASS; image stdout handshake fixture ile byte-valid JSONL üretir.

- [ ] **Step 5: Commit**

```bash
git add engines/reference
git commit -m "feat: add the protocol-driven reference engine"
```

### Task 10: Model broker, real provider adapter ve pure tool broker

**Files:**

- Create: `packages/model-broker/broker.go`
- Create: `packages/model-broker/types.go`
- Create: `packages/model-broker/budget.go`
- Create: `adapters/models/provider_one/adapter.go`
- Create: `adapters/models/fake/adapter.go`
- Create: `packages/tool-broker/broker.go`
- Create: `packages/tool-broker/conformance_math.go`
- Test: `tests/conformance/models/provider_test.go`
- Test: `tests/conformance/tools/math_test.go`
- Test: `tests/security/secrets/model_broker_test.go`

- [ ] **Step 1: Adapter/tool failing conformance tests yaz**

Provider tests canonical text/delta/tool/schema/usage/cancel/error conversion'ı shared fixtures ile assert eder. Tool tests `{a: 7, b: 5}` input'unu strict output `{sum: 12}` yapar; same `tool_call_id` duplicate execution counter'ını artırmaz.

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/conformance/models ./tests/conformance/tools ./tests/security/secrets -v`

Expected: broker/adapters absent nedeniyle FAIL.

- [ ] **Step 3: Model broker ve first adapter uygula**

Connection SecretRef sadece broker executor'da redeem edilir. Canonical request route revision, model step ID, deadline, privacy flags ve budget reservation taşır. Canonical result actual model/provider request ID, deltas, tool requests, usage ve sanitized error içerir. Provider hidden retry kapatılır veya attempt olarak raporlanır.

- [ ] **Step 4: Pure tool broker uygula**

`palai.conformance.math.add` yalnızca explicit conformance tool set içinde discoverable olur. JSON Schema validation, request hash, fenced ToolCall row, cached completed result ve usage event üretir.

- [ ] **Step 5: Conformance/security tests çalıştır**

Run: `go test ./tests/conformance/models ./tests/conformance/tools ./tests/security/secrets -count=5 -v`

Expected: PASS; test sentinel secret hiçbir captured frame/event/log/artifact içinde yok.

- [ ] **Step 6: Protected live adapter smoke çalıştır**

Run: `make test-live-provider PROVIDER=provider-one CASE=text-stream-tool-schema`

Expected: PASS with real provider request IDs and usage; credential output'ta yok.

- [ ] **Step 7: Commit**

```bash
git add packages/model-broker packages/tool-broker adapters/models tests/conformance/models tests/conformance/tools tests/security/secrets
git commit -m "feat: broker real models and idempotent tools"
```

### Task 11: End-to-end Response orchestration

**Files:**

- Create: `apps/control-plane/internal/execution/orchestrator.go`
- Create: `apps/control-plane/internal/execution/runner_gateway.go`
- Create: `apps/control-plane/internal/execution/model_dispatch.go`
- Create: `apps/control-plane/internal/execution/tool_dispatch.go`
- Create: `apps/control-plane/internal/execution/finalize.go`
- Test: `tests/e2e/responses/live_loop_test.go`
- Test: `tests/e2e/responses/restart_test.go`
- Test: `tests/e2e/responses/store_false_test.go`

- [ ] **Step 1: Failing vertical e2e tests yaz**

Deterministic test fake provider ile run admission → runner lease → engine ready → model tool request → tool result → final model output → terminal response zincirini assert eder. Restart test terminal DB state'ini process restart sonrası okur. Purge test short UAT TTL ve tombstone behavior'ını assert eder.

- [ ] **Step 2: Fail'i doğrula**

Run: `make test-e2e TEST=responses`

Expected: frame dispatch/orchestrator absent nedeniyle FAIL.

- [ ] **Step 3: Orchestrator'ı minimum state-machine glue olarak uygula**

Orchestrator yalnızca canonical transitions ve durable jobs çağırır; ikinci agent loop yazmaz. Her engine frame önce schema/run/fence/hash doğrular, sonra transaction ile state/event yazar. Provider/tool result DB commit edilmeden engine'e teslim edilmez. Terminal response projection committed run/output/usage üzerinden üretilir.

- [ ] **Step 4: E2E ve restart tests çalıştır**

Run: `make test-e2e TEST=responses`

Expected: PASS; one transient session/root run, one terminal, contiguous events, no duplicate model/tool dispatch.

- [ ] **Step 5: Commit**

```bash
git add apps/control-plane/internal/execution tests/e2e/responses
git commit -m "feat: execute responses through the common kernel"
```

### Task 12: Local Compose distribution ve CLI lifecycle

**Files:**

- Create: `deploy/compose/compose.yaml`
- Create: `deploy/compose/compose.env.example`
- Create: `deploy/compose/control-plane.Dockerfile`
- Create: `deploy/compose/runner.Dockerfile`
- Create: `cmd/cli/main.go`
- Create: `cmd/cli/internal/local/up.go`
- Create: `cmd/cli/internal/local/down.go`
- Create: `cmd/cli/internal/local/doctor.go`
- Create: `cmd/cli/internal/provider/add.go`
- Create: `cmd/cli/internal/response/create.go`
- Test: `tests/e2e/local/clean_boot_test.go`
- Test: `tests/e2e/local/doctor_test.go`

- [ ] **Step 1: Failing clean-boot/doctor tests yaz**

Tests isolated temp data directory ve random ports kullanır. `down` sonrası volumes kalır; explicit `reset --confirm` olmadan silinmez.

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/e2e/local -v`

Expected: CLI/Compose assets absent nedeniyle FAIL.

- [ ] **Step 3: Compose stack oluştur**

Services health checks ve immutable image digests/locally built release labels taşır. Runner runtime socket'a erişebilir; engine workload erişemez. Random passwords/local CA/setup token `.palai/` data directory'de strict permissions ile bulunur. Provider secret Compose environment'ına yayılmaz; bootstrap write-only API ile encrypted SecretRef olur.

- [ ] **Step 4: CLI lifecycle ve doctor uygula**

`palai init` config/data dir ve local project oluşturur. `local up` migrations/health bekler. `doctor --json` API, migration, object store, runner, image pull/digest, provider capability ve clock results verir. Provider key stdin/OS keychain/file descriptor ile alınır; argument değildir.

- [ ] **Step 5: Clean local tests çalıştır**

Run: `go test ./tests/e2e/local -count=3 -v`

Expected: PASS; repeated up/down idempotent; data retained.

- [ ] **Step 6: Commit**

```bash
git add deploy/compose cmd/cli tests/e2e/local
git commit -m "feat: package the complete local stack"
```

### Task 13: TypeScript SDK transport, streaming ve typed errors

**Files:**

- Create: `sdks/typescript/package.json`
- Create: `sdks/typescript/tsconfig.json`
- Create: `sdks/typescript/src/generated/types.ts`
- Create: `sdks/typescript/src/client.ts`
- Create: `sdks/typescript/src/resources/responses.ts`
- Create: `sdks/typescript/src/stream.ts`
- Create: `sdks/typescript/src/errors.ts`
- Create: `sdks/typescript/src/server-only.ts`
- Test: `sdks/typescript/test/responses.test.ts`
- Test: `sdks/typescript/test/stream.test.ts`
- Test: `sdks/typescript/test/retry.test.ts`

- [ ] **Step 1: Failing SDK contract tests yaz**

Cases: constructor config precedence; no unrelated provider env discovery; automatic stable idempotency key across retry; AsyncIterable SSE; reconnect/dedupe; explicit cancel; RFC 9457 typed error; unknown event/enum preservation; browser import of server credential path fails.

- [ ] **Step 2: Fail'i doğrula**

Run: `pnpm --dir sdks/typescript test`

Expected: package/source absent nedeniyle FAIL.

- [ ] **Step 3: Generated transport types ve handwritten ergonomic layer uygula**

Public surface:

```ts
const client = new Palai({ baseURL, apiKey, project });
const response = await client.responses.create(request, options);
const stream = client.responses.stream(request, options);
const stored = await client.responses.retrieve(responseID);
await client.responses.cancel(responseID);
```

`stream.finalResponse()` canonical terminal object döndürür. Iterator close yalnızca transport'u kapatır. API key browser-safe entrypoint'ten export edilmez; package conditional exports ile server path ayırır.

- [ ] **Step 4: SDK tests ve generated drift çalıştır**

Run: `pnpm --dir sdks/typescript test && make check-generated`

Expected: PASS; retry capture aynı idempotency key gösterir.

- [ ] **Step 5: Commit**

```bash
git add sdks/typescript protocols scripts/contracts
git commit -m "feat: add the TypeScript responses SDK"
```

### Task 14: Next.js SDK consumer ve browser stream proof

**Files:**

- Create: `examples/nextjs-sdk/package.json`
- Create: `examples/nextjs-sdk/next.config.ts`
- Create: `examples/nextjs-sdk/app/layout.tsx`
- Create: `examples/nextjs-sdk/app/page.tsx`
- Create: `examples/nextjs-sdk/app/api/palai/route.ts`
- Create: `examples/nextjs-sdk/lib/palai.ts`
- Create: `examples/nextjs-sdk/components/live-response.tsx`
- Create: `examples/nextjs-sdk/tests/live.spec.ts`
- Create: `examples/nextjs-sdk/.env.example`

- [ ] **Step 1: Failing browser e2e test yaz**

Playwright test prompt gönderir; connection/status, text delta, tool requested/completed, usage ve final structured result görünmesini bekler. Browser request headers/source maps/static chunks içinde `PALAI_API_KEY` olmadığını assert eder.

- [ ] **Step 2: Fail'i doğrula**

Run: `pnpm --dir examples/nextjs-sdk test:e2e`

Expected: Next app/routes absent nedeniyle FAIL.

- [ ] **Step 3: Server-only client ve Route Handler uygula**

`lib/palai.ts` `server-only` import eder ve env validation yapar. Route Handler SDK stream'ini `ReadableStream` ile newline-delimited canonical event projection olarak browser'a geçirir; raw provider payload/secret göndermez. Browser abort transport'u kapatır ama server run'ı cancel etmez.

- [ ] **Step 4: Minimal proof UI uygula**

UI prompt, ordered event timeline, actual selected model, tool args/result, usage ve final output gösterir. Hidden reasoning göstermez. Error panel stable code/request ID gösterir.

- [ ] **Step 5: Deterministic browser tests çalıştır**

Run: `pnpm --dir examples/nextjs-sdk test:e2e`

Expected: PASS against local fake-provider profile; browser secret scan zero findings.

- [ ] **Step 6: Commit**

```bash
git add examples/nextjs-sdk
git commit -m "feat: prove SDK streaming from Next.js"
```

### Task 15: Live UAT harness ve evidence verifier

**Files:**

- Create: `tests/uat/cases/LP-001/case.yaml`
- Create: `tests/uat/cases/LP-002/case.yaml`
- Create: `tests/uat/cases/LP-003/case.yaml`
- Create: `tests/uat/cases/LP-004/case.yaml`
- Create: `tests/uat/cases/LP-005/case.yaml`
- Create: `tests/uat/cases/LP-006/case.yaml`
- Create: `tests/uat/cases/LP-007/case.yaml`
- Create: `tests/uat/cases/LP-008/case.yaml`
- Create: `tests/uat/cases/LP-009/case.yaml`
- Create: `tests/uat/cases/LP-010/case.yaml`
- Create: `tests/uat/cases/LP-011/case.yaml`
- Create: `tests/uat/cases/LP-012/case.yaml`
- Create: `tests/uat/cases/LP-013/case.yaml`
- Create: `tests/uat/cases/LP-014/case.yaml`
- Create: `tests/uat/cases/LP-015/case.yaml`
- Create: `tests/uat/local_live_test.go`
- Create: `scripts/evidence/capture`
- Create: `scripts/evidence/verify`
- Create: `protocols/schemas/evidence/manifest.json`

- [ ] **Step 1: Failing evidence verifier tests yaz**

Missing git SHA/image digest/migration/API version/provider request ID/checksum/redaction scan/DB assertion bundle fail olmalıdır. Plaintext sentinel credential içeren fixture kesin fail olmalıdır.

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/uat -run TestEvidenceVerifier -v`

Expected: verifier absent nedeniyle FAIL.

- [ ] **Step 3: Case runner ve evidence capture uygula**

Runner setup/action/assert/cleanup aşamalarını machine-readable kaydeder. Canonical API/events/audit/usage ve direct DB/object assertions aynı run ID ile bağlanır. Process restart gerçekten PID/container değiştirir. SSE network cut gerçekten transport'u kapatır. Store-false test UAT config'te kısa TTL kullanır ve production default'u değiştirmez.

- [ ] **Step 4: Verifier tests çalıştır**

Run: `go test ./tests/uat -run TestEvidenceVerifier -v`

Expected: valid fixture PASS; eksik/tampered/secret fixture FAIL.

- [ ] **Step 5: Protected real-provider LP suite çalıştır**

Run: `make uat-local-live PROVIDER=provider-one`

Expected terminal summary:

```text
LP-001 PASS
LP-002 PASS
LP-003 PASS
LP-004 PASS
LP-005 PASS
LP-006 PASS
LP-007 PASS
LP-008 PASS
LP-009 PASS
LP-010 PASS
LP-011 PASS
LP-012 PASS
LP-013 PASS
LP-014 PASS
LP-015 PASS
```

- [ ] **Step 6: Evidence bundle verify et**

Run: `make evidence-verify RELEASE=local-live-0.1.0`

Expected: `15 passed, 0 failed, 0 missing, 0 secret findings`.

- [ ] **Step 7: Commit redacted fixtures ve summary**

```bash
git add tests/uat scripts/evidence protocols/schemas/evidence evidence/releases/local-live-0.1.0
git commit -m "test: prove the local stack with a live provider"
```

## 6. Final release check

- [ ] `make verify` PASS.
- [ ] `make test-component` PASS with real PostgreSQL, object store ve Docker Engine.
- [ ] `make test-e2e` PASS with deterministic provider.
- [ ] `make uat-local-live PROVIDER=provider-one` bütün LP cases PASS.
- [ ] `make evidence-verify RELEASE=local-live-0.1.0` PASS.
- [ ] `git status --short` clean.
- [ ] Public image/package artifacts exact commit ve digest ile üretildi.
- [ ] `palai local up` clean supported machine'de source edit olmadan tekrarlandı.
- [ ] Next.js example yalnızca `PALAI_BASE_URL`, `PALAI_API_KEY`, `PALAI_PROJECT` değiştirerek aynı semantiği kullandı.
- [ ] Discovery bu release'i preview/development isolation olarak doğru ilan etti.

## 7. LP-0 sonrası zorunlu sıra

LP-0, yalnızca gerçek local vertical slice kanıtıdır. Sonraki implementation sırası:

1. `phase-08-interactive-sessions.md` ile durable chat/commands/config revisions.
2. `phase-09-repository-coding.md` ile gerçek coding workspace.
3. `phase-10-recovery-replay.md` ile process/container/host kill ve side-effect replay.
4. `phase-14-production-self-host.md` ile TLS/backup/cloud VM.

Bu dört kapı geçmeden local proof production self-host iddiasına çevrilmez.
