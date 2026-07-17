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

> **phase-02 ile karşılandı:** Canonical şemalar, OpenAPI 3.2 + AsyncAPI 3.1 projeksiyonu ve cross-language corpus, phase-02 contract-spine planının Task 1–6'sında uygulandı; deterministic `make generate` / `make check-generated` zero-drift ile doğrulandı (en son ilgili commit `bfd21e5`). Ayrıntı: `docs/superpowers/plans/phase-02-contract-spine.md`.

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

> **phase-02 ile karşılandı:** Yedi pure lifecycle tablosu (run/attempt/response/session/command/tool_call/workspace) ve invalid-transition / terminal-monotonicity / one-active-fence / sequence-monotonicity property + registry cross-check suite'i, phase-02 contract-spine planının Task 7–12'sinde uygulandı (state tabloları commit `02d6aff`; property/registry kanıtı aynı Task 12 commit'inde eklendi). Event-append aynı DB transaction'ı E03'te bağlanır. Ayrıntı: `docs/superpowers/plans/phase-02-contract-spine.md`.

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

### Task 11: End-to-end Response orchestration (injectable engine channel)

> **Fork kararı 1 — engine execution channel:** Task 8 supervisor'ı batch'tir (container'ı tamamlanana kadar çalıştırır, stdin'e hiçbir frame yazmaz, `supervisor.hello` göndermez); Task 9 engine'i ise stdin'den `model.result`/`tool.result` bekleyen canlı bir stdio oturumudur ve production runner gateway henüz yoktur. Orchestrator çekirdeği bu task'ta **bir kez**, enjekte edilebilir `EngineChannel` seam'ine karşı yazılır. Deterministic e2e bu seam'i, reference engine'i bare `os/exec` subprocess olarak çalıştıran test channel'ıyla sürer; OCI + mTLS dışına yalnızca bu test kanalı çıkar. Hardened production yolu aynı seam'in ikinci implementasyonu olarak Task 11b (streaming supervisor, runner tarafı) ve Task 11c (runner gateway + live parity, controller tarafı) ile ayrı ayrı kanıtlanır. Hiçbir kapsam silinmez; yalnızca yeniden sıralanır.
>
> **Fork kararı 2 — retention:** `store:false` purge → tombstone → 410 makinesi (§20.9/§8.3) bağımsız bir alt sistemdir ve sıfır implementasyonu vardı; Task 11d'ye taşındı. Bu task'ın e2e testleri live_loop ve restart'tır; `store_false_test.go` Task 11d'de yaşar.

**Files:**

- Create: `apps/control-plane/internal/execution/engine_channel.go`
- Create: `apps/control-plane/internal/execution/orchestrator.go`
- Create: `apps/control-plane/internal/execution/model_dispatch.go`
- Create: `apps/control-plane/internal/execution/tool_dispatch.go`
- Create: `apps/control-plane/internal/execution/finalize.go`
- Modify: `protocols/engine/engine.schema.json`
- Test: `tests/e2e/responses/harness_test.go`
- Test: `tests/e2e/responses/live_loop_test.go`
- Test: `tests/e2e/responses/restart_test.go`

**Interfaces:**

- Consumes: `statemachines.Apply` + Run/Attempt/ToolCall tabloları (Task 4); `store.Store` ve transition transaction'ı (Task 3); `coordinator` worker + `execution.AdvanceRun` (Task 7); `modelbroker.Broker.Route(ctx, provider string, req modelbroker.Request, onDelta func(modelbroker.Delta)) (modelbroker.Result, error)` ve `toolbroker.Broker.Execute(callID contracts.ToolCallID, name string, args map[string]any, fence uint64) (toolbroker.Outcome, error)` (Task 10); `contracts.EngineFrame` ve `runner.FrameLedger` (Task 8).
- Produces (Task 11b–11c bu seam'i production tarafında implemente eder):

```go
// engine_channel.go
type AttemptDescriptor struct {
	RunID       contracts.RunID
	AttemptID   contracts.AttemptID
	Fence       uint64
	ImageDigest string
	Limits      runner.Limits
}

// EngineChannel, handshake'i tamamlanmış tek attempt'lik frame taşıyıcısıdır.
// Receive'in verdiği ilk frame engine.ready'dir; temiz kapanış io.EOF döndürür.
type EngineChannel interface {
	Send(ctx context.Context, frame contracts.EngineFrame) error
	Receive(ctx context.Context) (contracts.EngineFrame, error)
	Close() error
}

// EngineDialer bir attempt için canlı channel açar: deterministic e2e'de subprocess
// implementasyonu, Task 11c'de runner gateway implementasyonu.
type EngineDialer interface {
	Dial(ctx context.Context, attempt AttemptDescriptor) (EngineChannel, error)
}

func NewOrchestrator(store *store.Store, dialer EngineDialer, models *modelbroker.Broker, tools *toolbroker.Broker) *Orchestrator
func (o *Orchestrator) ExecuteAttempt(ctx context.Context, attempt AttemptDescriptor) error
```

- [ ] **Step 1: Failing vertical e2e tests yaz**

`tests/e2e/responses/harness_test.go` deterministic kablolamayı kurar: gerçek PostgreSQL + Task 5 API + Task 7 coordinator + fake provider adapter + test-only `subprocessDialer`. `subprocessDialer.Dial` reference engine'i bare subprocess olarak başlatır ve `supervisor.hello`'yu stdin'e kendisi yazar:

```go
// harness_test.go — test-only EngineChannel implementasyonu.
cmd := exec.CommandContext(ctx, "uv", "run", "--locked", "--project", engineDir, "python", "-m", "palai_engine")
cmd.Env = []string{
	"PALAI_RUN_ID=" + string(a.RunID),
	"PALAI_ATTEMPT_ID=" + string(a.AttemptID),
	"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
}
// stdin pipe → Send: frame başına tek JSONL satırı + flush.
// stdout scanner → Receive: satır → contracts.EngineFrame; Limits.MaxFrameBytes
// bound'u satır okunurken uygulanır.
// Dial dönmeden önce supervisor.hello yazılır; Close process'i öldürür.
```

Test fonksiyonları ve assertion'ları:

```go
func TestLiveLoopCompletesOneResponseThroughSubprocessEngine(t *testing.T)
// admission → run.queued job → attempt (fence=1) → engine.ready → model.request →
// fake provider tool_calls({a:7,b:5}) → tool.request → committed tool.result →
// ikinci model.request → final output → exactly one run.terminal →
// GET /v1/responses/{id}: terminal status, output, usage ve contiguous events.

func TestCommitBeforeDeliverOnToolResult(t *testing.T)
// Test channel'ı Send'i intercept eder: tool.result engine'e ulaşmadan ÖNCE
// tool_calls satırı completed ve karşılık gelen event satırı DB'de commit edilmiştir.

func TestDuplicateEngineFrameDoesNotDoubleDispatch(t *testing.T)
// Aynı frame ID + aynı hash yeniden verilir → idempotent replay; model/tool dispatch
// sayaçları artmaz. Aynı ID + farklı hash → protocol violation, attempt failed.

func TestRestartPreservesTerminalResponseAndEvents(t *testing.T)
// Terminal sonrası control-plane process yeniden başlatılır; response/events aynı
// terminal state'i ve aynı contiguous canonical sequence'ı döndürür (LP-008).
```

- [ ] **Step 2: Fail'i doğrula**

Run: `make test-e2e TEST=responses`

Expected: `execution.Orchestrator`/`EngineChannel` tanımsız olduğu için derleme FAIL.

- [ ] **Step 3: Tool arguments canonical wire shape'ini şemaya yükselt**

`modelbroker.ToolCall.Arguments` provider'ın ürettiği JSON **string**'dir; engine wire'ında ise `arguments` her zaman JSON **object**'tir. Sınır tek noktadır: `model_dispatch.go` broker sonucunu `model.result` frame'ine çevirirken string'i bir kez `json.Unmarshal` ile object'e çözer; object'e çözülemeyen arguments sanitized `provider_error` sınıflı model failure olur ve raw string engine'e asla sızmaz. `protocols/engine/engine.schema.json` `$defs`'ine kanonik shape eklenir:

```json
"tool_call": {
  "type": "object",
  "required": ["name", "arguments"],
  "properties": {
    "name": {"type": "string"},
    "arguments": {"type": "object"}
  }
}
```

ve `allOf`'a iki koşul bağlanır: `type == "tool.request"` iken `data.arguments` object zorunludur; `type == "model.result"` iken `data.tool_calls` varsa her elemanı `$defs/tool_call`'a uyar.

Run: `make generate && make check-generated`

Expected: zero drift; `go test ./tests/conformance/contracts -v` PASS.

- [ ] **Step 4: Orchestrator'ı minimum state-machine glue olarak uygula**

- `orchestrator.go`: run job'ını Task 7 coordinator'ından teslim alır, `queued→provisioning→in_progress` geçişlerini Task 4 tabloları ve Task 3 transaction'ı ile yürütür, `dialer.Dial` ile channel açar, `run.start` gönderir ve frame intake döngüsünü işletir. İkinci bir agent loop yazılmaz.
- Frame intake her frame'de sırasıyla: envelope/schema doğrulaması, run/attempt identity ve fence kontrolü, `runner.FrameLedger.Admit` ile ID dedup — aynı ID + aynı hash stored result'ı replay eder, farklı hash protocol violation'dır (ENG-002'nin controller yarısı; runner yarısı Task 11b'dedir).
- `model_dispatch.go`: `model.request` önce transaction ile persist edilir (event dahil), sonra `models.Route` çağrılır, canonical result commit edilir ve ancak o zaman `model.result` Send edilir (commit-before-deliver).
- `tool_dispatch.go`: `tool.request` → strict schema + fenced `toolbroker.Execute` → commit → `tool.result` Send. Duplicate `tool_call_id` cached result'ı replay eder, executor'ı yeniden çalıştırmaz.
- `finalize.go`: `run.terminal` → exactly-one terminal transition; terminal Response projection committed run/output/usage üzerinden üretilir.

- [ ] **Step 5: E2E ve restart tests çalıştır**

Run: `make test-e2e TEST=responses`

Expected: PASS; one transient session/root run, one terminal, contiguous events, no duplicate model/tool dispatch.

- [ ] **Step 6: Commit**

```bash
git add apps/control-plane/internal/execution protocols/engine packages/contracts tests/e2e/responses
git commit -m "feat: execute responses through the common kernel"
```

### Task 11b: Live streaming supervisor ve interactive OCI channel (runner tarafı)

> Task 8'in batch supervisor'ı bilinçli minimumdu; bu task batch→streaming delta'sını kapatır ve Task 8'de deferred bırakılan iki maddeyi içerir: **stderr redaction transform** ve **forwarding loop'ta frame-ID uniqueness** (ENG-002'nin runner yarısı). Master planda E05 yüzeyidir; LP planında Task 11c gateway'inin runner-tarafı ön koşuludur.

**Files:**

- Create: `adapters/sandboxes/oci/stream.go`
- Create: `packages/runner/stream.go`
- Modify: `packages/runner/session.go`
- Modify: `protocols/runner/runner.schema.json`
- Modify: `tests/sandboxes/engine/main.go`
- Modify: `cmd/runner/main.go`
- Modify: `apps/control-plane/internal/execution/orchestrator.go` (intake sequence monotonicity — Task 11 review follow-up)
- Modify: `apps/control-plane/internal/execution/model_dispatch.go` (ad-hoc event adlarının kanonik registry adlarına katlanması — Task 11 review follow-up)
- Modify: `apps/control-plane/internal/execution/tool_dispatch.go` (aynı katlama)
- Create: `apps/control-plane/internal/execution/events_registry_test.go` (emitted ⊆ registry cross-check)
- Test: `tests/conformance/engine/stream_test.go`
- Test: `tests/fault/runner/stream_kill_test.go`
- Test: `apps/control-plane/e2e/responses/live_loop_test.go` (Modify — intake monotonicity case + event-adı assertion güncellemeleri)

**Interfaces:**

- Consumes: `oci.ContainerSpec`/`oci.Outcome`/digest-allowlist-limit kuralları (Task 8); `runner.EngineRequest`, `runner.Limits`, `runner.FrameLedger` ve Task 8 frame-envelope kuralları; `contracts.EngineFrame`, `contracts.RunnerMessage`.
- Produces (Task 11c gateway'i bunları tüketir):

```go
// adapters/sandboxes/oci/stream.go
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader // satır satır okunur; kapanış = engine çıkışı
	Stderr() io.Reader
	Wait(ctx context.Context) (Outcome, error)
	Kill(ctx context.Context) error
}
type InteractiveDriver interface {
	Start(ctx context.Context, spec ContainerSpec) (Process, error)
}

// packages/runner/stream.go
type FrameSink func(ctx context.Context, frame contracts.EngineFrame) error
type StreamSupervisor struct{ /* driver InteractiveDriver */ }
func NewStreamSupervisor(driver InteractiveDriver) *StreamSupervisor
// Stream: supervisor.hello yazar, engine.ready'yi startup deadline içinde bekler,
// inbound frame'leri stdin'e enjekte eder, her doğrulanmış stdout frame'ini sink'e
// iletir; dönüşte batch Run ile aynı outcome sınıflandırmasını uygular.
func (s *StreamSupervisor) Stream(ctx context.Context, request EngineRequest, inbound <-chan contracts.EngineFrame, sink FrameSink) (EngineResult, error)

// packages/runner/session.go — lease sonrası bağlantı açık kalır.
type LeaseSession struct{ /* ... */ }
func (s Session) OpenLease(ctx context.Context) (*LeaseSession, error) // ReceiveLease'in kalıcı hâli
func (l *LeaseSession) Lease() Lease
func (l *LeaseSession) SendEngineFrame(ctx context.Context, frame contracts.EngineFrame) error
func (l *LeaseSession) ReceiveControllerFrame(ctx context.Context) (contracts.EngineFrame, error)
func (l *LeaseSession) Complete(ctx context.Context, outcome string, stderrDigest string) error
```

Batch→streaming delta'sı (tamamı bu task'ta kapanır):

1. **Handshake:** batch `Supervisor.Run` hiç `supervisor.hello` yazmaz; `Stream` container start'tan hemen sonra stdin'e §25.6 hello frame'ini (protocol version, run identity, fence hash, limits) yazar ve `engine.ready`'yi startup deadline içinde bekler; aşım `incompatible_engine` sınıfı attempt failure'dır.
2. **Incremental okuma:** stdout post-hoc değil satır satır okunur; `MaxFrameBytes` per-frame bound ve `MaxStdoutBytes` toplam bound akış sırasında uygulanır; her frame gelişinde Task 8 envelope kuralları (monotonic sequence, run/attempt identity, RFC 3339 time) uygulanır.
3. **Frame-ID uniqueness (Task 8 deferred):** forwarding loop her frame'i `runner.FrameLedger.Admit`'ten geçirir — aynı ID + aynı hash idempotent retransmit olarak forward edilmeden düşürülür; aynı ID + farklı hash `ErrFrameHashConflict` ile attempt'i protokol ihlalinden bitirir.
4. **Stderr redaction transform (Task 8 deferred):** bounded stderr persist/forward edilmeden önce secret-pattern maskelemesinden geçer; engine kendi loglarını redact eder ama supervisor buna güvenmez.
5. **Stdin enjeksiyonu:** `model.result`, `tool.result`, `run.cancel` controller frame'leri per-frame flush ile stdin'e yazılır — batch'te bu yol hiç yoktu.
6. **Outcome paritesi:** wall-time/memory/process bounds ve timeout/exit/malformed sınıflandırması batch ile birebir aynıdır; mid-stream container kill attempt lost/failed üretir, asla false success değil.

Session relay: `ReceiveLease` bugün lease alınca bağlantıyı kapatır; `OpenLease` bağlantıyı açık tutar. Runner→controller frame'leri `engine.frame`, controller→runner frame'leri `controller.frame` message'ı içinde (`data` alanında tek `contracts.EngineFrame`) taşınır; bitişte outcome sınıfı + redacted stderr digest taşıyan `lease.complete` gönderilir. `protocols/runner/runner.schema.json` `$defs/types` enum'una `engine.frame` ve `controller.frame` eklenir.

- [ ] **Step 1: Docker-free failing stream tests yaz**

`tests/conformance/engine/stream_test.go`, in-memory pipe'larla sahte bir `oci.Process` kurar ve şunları assert eder:

```go
func TestStreamWritesHelloBeforeAnyRunInput(t *testing.T)
func TestStreamEnforcesFrameBoundMidStream(t *testing.T)
func TestStreamRejectsChangedHashDuplicateFrame(t *testing.T)
func TestStreamDropsIdenticalRetransmitWithoutForwarding(t *testing.T)
func TestStreamRedactsStderrBeforeForwarding(t *testing.T) // sentinel "sk-live-..." maskelenir
func TestStreamInjectsControllerFramesToStdin(t *testing.T)
func TestStreamHandshakeDeadlineFailsAsIncompatibleEngine(t *testing.T)
```

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/conformance/engine -run TestStream -v`

Expected: `StreamSupervisor`/`InteractiveDriver` tanımsız olduğu için FAIL.

- [ ] **Step 3: Interactive OCI process'i uygula**

`adapters/sandboxes/oci/stream.go` Docker attach ile stdin/stdout/stderr stream'lerini açar; digest verification, environment allowlist ve resource limits batch `Run` ile aynı koddan geçer. `tests/sandboxes/engine/main.go`'ya `PALAI_ENGINE_MODE=interactive` eklenir: hello'yu okur, ready yazar, `model.request` yazar, stdin'den `model.result` bekler, `run.terminal` yazar — Docker fault testinin sabit senaryosu.

- [ ] **Step 4: StreamSupervisor ve LeaseSession relay'ini uygula**

Yukarıdaki delta listesinin altı maddesi ve schema enum ekleri uygulanır.

Run: `go test ./tests/conformance/engine -run TestStream -v && make generate && make check-generated`

Expected: stream tests PASS; zero generated drift.

- [ ] **Step 5: Docker fault testini yaz ve çalıştır**

`tests/fault/runner/stream_kill_test.go`: interactive fixture engine `model.result` beklerken container kill edilir → attempt lost/failed sınıflanır, kısmi frame'ler success üretmez; ikinci senaryoda stderr'e yazılmış sentinel secret forward edilen stderr'de maskelidir.

Run: `go test ./tests/fault/runner -run TestStreamKill -count=5 -v`

Expected: PASS; false success yok.

- [ ] **Step 6: Controller intake sequence monotonicity'sini uygula (Task 11 review follow-up)**

Task 11'in orchestrator intake'i envelope/identity/time doğrular ama engine frame `sequence` monotonicity'sini doğrulamaz; Task 8 batch supervisor'ı `index+1`'i zorluyordu (`packages/runner/supervisor.go` `validateFrame`). Parite kapatılır: intake döngüsü attempt başına son kabul edilen sequence'ı tutar ve her frame'de `frame.Sequence == last+1` şartını arar; gap'li veya yeniden sıralanmış sequence protocol violation olarak attempt'i düşürür, hiçbir dispatch tetiklenmez.

Önce failing test — `apps/control-plane/e2e/responses/live_loop_test.go`'ya eklenir:

```go
func TestIntakeRejectsNonMonotonicEngineSequence(t *testing.T)
// Scripted test channel engine.ready'yi sequence=1 ile, sonraki frame'i sequence=3
// ile verir. Assert: attempt protocol violation ile failed olur, model/tool dispatch
// sayaçları sıfır kalır, run exactly-one terminal (failed) üretir.
```

Run: `go test ./apps/control-plane/e2e/responses -run TestIntakeRejects -v`

Expected: önce FAIL (gap kabul ediliyor), tek satırlık kontrol sonrası PASS.

- [ ] **Step 7: Ad-hoc event adlarını kanonik registry adlarına katla (Task 11 review follow-up)**

Task 11 dispatcher'ları `run.model_request.v1`, `run.model_result.v1`, `run.tool_result.v1` tiplerini ad-hoc emit eder; registry (`protocols/schemas/execution/event-types.json`, spec §13.3/§21.3'ten türetilmiş) aynı kavramların kanonik adlarını ZATEN içerir. Registry büyütülmez; emit edilen adlar kanonik adlara katlanır (üç eşleme de doğrulandı: aynı granülarite — brokered model call başına bir model_step — ve aynı lifecycle noktası):

- `model_dispatch.go:48` `CommitModelRequest` çağrısındaki `"run.model_request.v1"` → `"model_step.created.v1"` (model request Route'tan önce persist edilirken = step'in yaratılışı).
- `model_dispatch.go:81` `CommitModelResult` çağrısındaki `"run.model_result.v1"` → `"model_step.completed.v1"` (finalize edilmiş canonical result commit'i; §25.9 partial delta bir completed result değildir). Sanitized failure branch'i eklendiğinde adı `"model_step.failed.v1"`dir.
- `tool_dispatch.go:30` `CommitToolResult` çağrısındaki `"run.tool_result.v1"` → `"tool_call.completed.v1"` — phase-02 `statemachines.ToolCallTable`'ın Executing→Completed geçişi tam bu adı zaten üretir (`packages/state-machines/tool_call.go:55`); literal hardcode etmek yerine tablonun döndürdüğü event adını geçir, tek kaynak tablo kalsın. (`tool_call.reconciled_completed.v1` yalnızca §26 uncertain-reconciliation yoludur, bu path değil.)
- Test güncellemeleri: `apps/control-plane/e2e/responses/live_loop_test.go:102,105,110` (`run.model_request/result.v1` beklentileri) ve `:141` (SQL `type='run.tool_result.v1'`) kanonik adlara çevrilir.

Gelecekte ad-hoc ad üretimini önlemek için cross-check kalır — `events_registry_test.go`, `packages/state-machines/registry_test.go` ile aynı dosya-okuma deseniyle:

```go
func TestEmittedOrchestratorEventsAreInCanonicalRegistry(t *testing.T)
// execution package'ın emit ettiği her event type constant'ı
// protocols/schemas/execution/event-types.json listesinde bulunur.
```

Run: `go test ./apps/control-plane/internal/execution -run TestEmitted -v && make test-e2e TEST=responses`

Expected: PASS; schema değişikliği yok (registry'ye ekleme yapılmaz), `make check-generated` zero-drift kalır.

- [ ] **Step 8: cmd/runner'ı streaming loop'a geçir**

`cmd/runner/main.go`: `OpenLease` → `StreamSupervisor.Stream`; sink her frame'i `SendEngineFrame` ile iletir, `ReceiveControllerFrame` çıktısı `inbound` kanalına akar; sonunda `Complete` gönderilir.

Run: `make verify`

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add adapters/sandboxes/oci packages/runner protocols/runner packages/contracts apps/control-plane tests/sandboxes/engine tests/conformance/engine tests/fault/runner cmd/runner
git commit -m "feat: stream the engine protocol through a live supervisor"
```

### Task 11c: Production runner gateway ve live channel parity (controller tarafı)

> `tests/conformance/engine/harness_test.go` içindeki stub, "the control-plane counterpart the production runner-gateway task will replace" olarak işaretlenmişti; bu o task'tır. Gateway, Task 11 `EngineDialer` seam'inin production implementasyonudur: bağlı bir runner'a lease offer eder ve Task 11b session frame'lerini `EngineChannel` olarak köprüler. Bu task'tan sonra Task 15'in LP-003/LP-004 canlı UAT'ları gerçek topolojiden (orchestrator → gateway → runner → OCI engine) geçer; subprocess channel yalnızca deterministic suite'te kalır.

**Files:**

- Create: `apps/control-plane/internal/execution/runner_gateway.go`
- Modify: `apps/control-plane/api/router.go`
- Modify: `cmd/runner/main.go` (yalnızca "gateway does not exist" doc comment'inin kaldırılması)
- Modify: `packages/model-broker/types.go` (provider idempotency key — Task 11 review follow-up; plumbing'in evi Task 10'un model-broker'ıdır)
- Modify: `adapters/models/provider_one/adapter.go`
- Modify: `adapters/models/fake/adapter.go`
- Modify: `apps/control-plane/internal/execution/model_dispatch.go`
- Modify: `apps/control-plane/internal/execution/reconciler.go` (dead-letter → run terminal köprüsü — Task 11b review follow-up; plan genelinde yük taşıyan)
- Modify: `packages/runner/stream.go` (supervisor.hello fence hash + boundary-aware stderr redaction — Task 11b review follow-up)
- Modify: `packages/runner/supervisor.go` (`EngineRequest.Fence`; batch `parseEngineFrames` frame-ID dedup — LP8 Minor-2 kalıntısı)
- Test: `tests/conformance/engine/gateway_test.go`
- Test: `tests/conformance/engine/stream_test.go` (Modify — fence-hash + chunk-boundary redaction case'leri)
- Test: `apps/control-plane/e2e/responses/gateway_parity_test.go`
- Test: `apps/control-plane/e2e/responses/reclaim_test.go`
- Test: `apps/control-plane/e2e/responses/dead_letter_test.go`

**Interfaces:**

- Consumes: Task 11 `EngineDialer`/`EngineChannel`/`AttemptDescriptor`; Task 11b `LeaseSession` mesaj tipleri (`engine.frame`/`controller.frame`/`lease.complete`); Task 8 enrollment beklentileri (one-use token, short-lived certificate, mTLS identity). CA bu task'ta enjekte edilen `CertIssuer`'dır (testte in-test CA); `.palai/` dosya düzenine bağlama Task 12'nin işidir.
- Produces:

```go
// runner_gateway.go
type CertIssuer interface {
	// Local CA ile runner public key'ini short-lived client certificate'a imzalar.
	SignRunnerCertificate(publicKeyDER []byte, runnerDNS string) (certificateDER []byte, err error)
}
type EnrollmentTokens interface {
	Consume(token string) error // bilinmeyen veya kullanılmış token error döndürür (one-use)
}
type RunnerGateway struct{ /* ... */ }
func NewRunnerGateway(issuer CertIssuer, tokens EnrollmentTokens) *RunnerGateway
func (g *RunnerGateway) Routes() http.Handler // /v1/runner/enroll + /v1/runner/connect (WS)
// EngineDialer implementasyonu: bağlı bir runner'a lease.offer gönderir, attempt'in
// frame trafiğini WS session üzerinden EngineChannel olarak köprüler.
func (g *RunnerGateway) Dial(ctx context.Context, attempt AttemptDescriptor) (EngineChannel, error)
```

- [ ] **Step 1: Failing gateway conformance tests yaz**

`tests/conformance/engine/gateway_test.go` stub'ın kanıtladığı wire beklentilerini gerçek gateway'e karşı, in-process `runner.Session`/`OpenLease` ile assert eder:

```go
func TestGatewayEnrollmentConsumesTokenOnce(t *testing.T)
func TestGatewayConnectRequiresRunnerClientCertificate(t *testing.T)
func TestGatewayOffersLeaseWithImmutableDigestAndFence(t *testing.T)
func TestGatewayRelaysFramesBothWays(t *testing.T)
```

Mevcut stub tabanlı testler silinmez; runner-tarafı semantiği Docker'sız kanıtlamaya devam ederler.

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/conformance/engine -run TestGateway -v`

Expected: gateway package/handler tanımsız olduğu için FAIL.

- [ ] **Step 3: Gateway'i uygula ve router'a bağla**

Enroll: bearer one-use token doğrulanır, runner public key'i local CA ile kısa ömürlü client cert'e imzalanır. Connect: mTLS client chain doğrulanır, `runner.hello` beklenir, `Dial`'ın beklettiği attempt için `lease.offer` gönderilir, `engine.frame`/`controller.frame` köprüsü kurulur; `lease.complete` outcome'u attempt sınıflandırmasına işlenir. `router.go` `/v1/runner/*` altına gateway routes'u mount eder; bu endpoint'ler public API auth middleware'inden geçmez, kendi mTLS/token kimliğini kullanır.

Run: `go test ./tests/conformance/engine -run TestGateway -v`

Expected: PASS.

- [ ] **Step 4: Cross-attempt provider idempotency key'ini uygula (Task 11 review follow-up)**

Task 11 fix'inin kalıntısı: model request Route'tan önce persist edilir ve COMMITTED result replay edilir; ama "Route verildi, result commit edilmeden process düştü" penceresi reclaim'de ikinci bir provider çağrısına hâlâ izin verir. Tam kapanış provider-side idempotency key ister — §53.4 (exactly-one-retry-owner) ve §35.3 (at-least-once + idempotent effect; evrensel exactly-once değil) bunun çapalarıdır.

- `modelbroker.Request`'e `IdempotencyKey string` alanı eklenir; `model_dispatch.go` onu deterministik türetir: `string(run_id) + "/" + string(model_request_id)`. Her ikisi de attempt'ler arasında sabittir, dolayısıyla reclaim edilen attempt aynı key'i taşır.
- Adapter'lar key'i provider'a iletir: `provider_one` HTTP `Idempotency-Key` header'ı olarak; `fake` adapter gördüğü key'leri kaydeder ve aynı key'in ikinci çağrısında stored result'ı döndürüp effect sayacını artırmaz.

Failing fault testi — `apps/control-plane/e2e/responses/reclaim_test.go`:

```go
func TestReclaimAfterCrashBetweenRouteAndCommitReusesProviderIdempotencyKey(t *testing.T)
// Fake provider Route çağrısını kaydettikten sonra, result commit edilmeden
// orchestrator process'i öldürülür; lease expiry sonrası reclaim edilen attempt
// Route'u yeniden verir. Assert: provider iki çağrıda da AYNI idempotency key'i
// gördü ve provider effect sayacı 1'de kaldı — tek dış etki, tek usage settle.
```

Run: `go test ./apps/control-plane/e2e/responses -run TestReclaim -v`

Expected: önce FAIL (iki farklı çağrı/effect), key plumbing sonrası PASS.

- [ ] **Step 5: Dead-letter → run terminal köprüsünü uygula (Task 11b review follow-up; plan genelinde yük taşıyan)**

Deterministik bir engine/protokol ihlali her attempt'i düşürür → durable job §24.4 gereği dead-letter'a iner → ama RUN sonsuza dek `running` kalır: response hiçbir zaman terminale ulaşmaz ve SSE stream'i kapanmaz. Bunu hiçbir task reconcile etmiyordu; Task 7'nin reconciler'ı genişletilir. `Sweep`, dead-lettered response.run job'larını bulur ve run'ı Task 4 tablosuyla (`statemachines.RunCmdFail` — her non-terminal state'ten `run.failed.v1` üretir, §22.3 terminality) failed terminale taşır; transition + terminal event aynı transaction'dadır, böylece response projection failed olur ve SSE terminal event'ten sonra temiz kapanır. Job üzerindeki operator retry/reconcile aksiyonları kalır (§24.4); terminal monotonicity gereği run geri açılmaz — sonradan retry edilen job terminal run'ı görüp no-op yapar. Bu adım Task 15 canlı UAT'ından önce kanıtlanmış olmak ZORUNDADIR: asılı bir run LP-0'ın "gerçekten çalışıyor" kapısını düşürür.

Failing test — `apps/control-plane/e2e/responses/dead_letter_test.go`:

```go
func TestDeadLetteredRunJobIsDrivenToFailedTerminal(t *testing.T)
// Scripted channel her attempt'te protokol ihlali üretir; MaxAttempts sonrası job
// dead-letter olur. Reconciler sweep sonrası: run row'u failed, run.failed.v1 event
// journal'da, GET /v1/responses/{id} failed terminal projection döner ve SSE reader
// terminal event'i alıp stream'in kapandığını görür — asılı run kalmaz.
```

Run: `go test ./apps/control-plane/e2e/responses -run TestDeadLettered -v`

Expected: önce FAIL (run `running` asılı kalır, SSE kapanmaz), köprü sonrası PASS.

- [ ] **Step 6: supervisor.hello'ya fence hash ekle ve batch frame-ID kalıntısını kapat (Task 11b review follow-up)**

- §25.6 hello frame'i fencing token hash taşımalıdır; bugün taşımıyor. `runner.EngineRequest`'e `Fence uint64` eklenir (`Lease.Fence` runner'a zaten ulaşıyor; `cmd/runner` geçirir) ve `StreamSupervisor.Stream` hello data'sına `fence_hash` yazar: `sha256("<run_id>/<fence>")` hex digest. Engine tarafı karşılaştırma E10 recovery'de anlamlanır (engine'in kıyaslayacağı bir değeri henüz yok); burada yalnızca taşınır. `tests/conformance/engine/stream_test.go`'ya assertion: hello, herhangi bir run input'undan önce boş olmayan `fence_hash` ile yazılır.
- Aynı dosya dokunuşuyla LP8 Minor-2 kalıntısı kapanır: batch `parseEngineFrames` de her frame'i `runner.FrameLedger.Admit`'ten geçirir — duplicate ID + farklı hash `ErrFrameHashConflict` ile attempt'i düşürür (streaming yoluyla parite; batch artık ikincil yol ama tutarsız kalmaz). Docker'sız case: `TestBatchRunRejectsChangedHashDuplicateFrame` `tests/conformance/engine/stream_test.go`'ya eklenir.

Run: `go test ./tests/conformance/engine -run 'TestStream|TestBatch' -v`

Expected: PASS; hello fence_hash'siz yazılamaz, batch duplicate-changed-hash fail eder.

- [ ] **Step 7: Stderr redaction'ı chunk-boundary'ye karşı sertleştir (Task 11b review follow-up)**

Bugün runner dışına yalnızca stderr DIGEST'i çıkar (`lease.complete`); redactor ise chunk sınırında bölünen bir secret'ı (`sk-l` bir Write'ta, `ive-...` sonrakinde) kaçırabilir. Redactor'a chunk'lar arası tail carry-over penceresi eklenir (en uzun secret pattern uzunluğu − 1 bayt taşınır ve bir sonraki chunk'la birlikte taranır). Gate notu: stderr BYTE'ları controller'a ilk forward edildiğinde (gateway diagnostics bu task'ta veya sonrasında), focused regex seti bu sertleştirmeyle BİRLİKTE yeniden gözden geçirilir — byte forwarding hardening'siz eklenemez.

Failing case — `tests/conformance/engine/stream_test.go`:

```go
func TestStreamRedactsSecretSplitAcrossChunkBoundary(t *testing.T)
// Sentinel "sk-live-..." iki ayrı stderr Write'ına bölünür; persist/forward edilen
// stderr'de yine maskelidir.
```

Run: `go test ./tests/conformance/engine -run TestStreamRedacts -v`

Expected: önce FAIL (bölünmüş sentinel sızar), carry-over sonrası PASS.

- [ ] **Step 8: Live channel parity e2e testini yaz ve çalıştır**

`apps/control-plane/e2e/responses/gateway_parity_test.go` (Docker gerektirir; runner fault suite'iyle aynı guard'ı kullanır):

```go
func TestGatewayLoopMatchesSubprocessChannelOutcome(t *testing.T)
// Task 11 live_loop senaryosunun aynısı gateway → gerçek runner session →
// StreamSupervisor → reference engine image yoluyla koşar; canonical event type
// dizisi, terminal state, output ve usage subprocess channel sonucuyla birebir eşittir.

func TestNoSecretReachesEngineThroughGateway(t *testing.T)
// Sentinel provider secret hiçbir engine env/frame/redacted stderr içinde görünmez.
```

Run: `go test ./apps/control-plane/e2e/responses -run TestGateway -v`

Expected: PASS; iki channel aynı canonical sonucu üretir.

- [ ] **Step 9: Commit**

```bash
git add apps/control-plane packages/model-broker packages/runner adapters/models cmd/runner tests/conformance/engine
git commit -m "feat: bridge the orchestrator to live runners through the gateway"
```

### Task 11d: store:false retention, purge, tombstone ve 410

> **Fork kararı 2'nin uygulaması:** §20.9/§8.3 retention kuralları (purge → tombstone → 410 `idempotency_result_expired`) spec'te tanımlı ama implementasyonu yoktu. Master plan UAT matrisi DAT ailesinin "store:false subset"ini E07/LP-0'a atar; LP-009 bu task olmadan geçemez. Task 11'den taşınan `store_false_test.go` burada yaşar.

**Files:**

- Create: `storage/migrations/000002_retention.up.sql`
- Create: `storage/migrations/000002_retention.down.sql`
- Create: `apps/control-plane/internal/execution/retention.go`
- Modify: `apps/control-plane/api/middleware/idempotency.go`
- Modify: `apps/control-plane/api/responses.go` (GET /v1/responses/{id} endpoint'i — Task 11'den deferred — ve 410 yolları)
- Test: `tests/component/postgres/retention_test.go`
- Test: `apps/control-plane/e2e/responses/store_false_test.go`
- Test: `tests/conformance/api/responses_test.go` (Modify — retrieval 200/404 case'leri)

**Interfaces:**

- Consumes: Task 3 migration zinciri ve `store.Store`; Task 5 idempotency middleware ve Problem Details; Task 7 coordinator (reaper bir durable job olarak koşar); Task 11 terminal response üretimi.
- Produces:

```go
// retention.go — reaper, coordinator üzerinde periyodik durable job olarak koşar.
func NewReaper(store *store.Store, storeFalseTTL time.Duration) *Reaper
func (r *Reaper) Sweep(ctx context.Context) (purged int, err error)
```

ve iki yeni RFC 9457 problem code'u: create replay'inde `idempotency_result_expired`, purged retrieval'da `retention_expired` (her ikisi HTTP 410).

- [ ] **Step 1: Failing retention tests yaz**

`tests/component/postgres/retention_test.go`:

```go
func TestReaperPurgesOnlyExpiredStoreFalseResponses(t *testing.T)
// store=false + terminal + TTL geçmiş satırlar purge edilir; store=true ve
// TTL dolmamış satırlara dokunulmaz.

func TestPurgeKeepsTombstoneRequestHashAndFingerprint(t *testing.T)
// purge sonrası idempotency satırında yalnızca request hash, resource tombstone,
// outcome fingerprint ve purge time kalır (§20.9); cached response body kalmaz.

func TestPurgeReplacesEventPayloadsButKeepsSequence(t *testing.T)
// content taşıyan event data'ları {"purged": true} olur; satırlar ve contiguous
// sequence bütünlüğü korunur.
```

`apps/control-plane/e2e/responses/store_false_test.go` (Task 11 kapsamından taşındı; Task 11 harness'ını yeniden kullanır):

```go
func TestStoreFalseContentIsGoneAfterConfiguredTTL(t *testing.T)
// Kısa UAT TTL ile response tamamlanır, reaper koşar; DB'de output/message/event
// content'i kalmaz ve response'a bağlı artifact satırı varsa byte'ları da silinmiştir.

func TestDuplicateCreateAfterPurgeReturns410WithoutReexecution(t *testing.T)
// Aynı Idempotency-Key + aynı body → 410 idempotency_result_expired + original
// operation identity; model dispatch sayacı artmaz (re-execution yok).

func TestRetrieveAfterPurgeReturns410RetentionExpired(t *testing.T)

func TestRetainedResponseSurvivesReaper(t *testing.T)
```

- [ ] **Step 2: Fail'i doğrula**

Run: `make test-component TEST=postgres && make test-e2e TEST=responses`

Expected: retention migration/reaper/410 yolları olmadığı için FAIL.

- [ ] **Step 3: GET /v1/responses/{id} endpoint'ini inşa et (Task 11'den deferred)**

Task 11 implementasyonu retrieval'ı DB'den doğrudan okuyarak doğruladı; OpenAPI'de sözleşmeli `GET /v1/responses/{id}` sahipsiz kaldı. Bu task'ın purge testleri HTTP 410'u ancak bu endpoint üzerinden kanıtlayabilir; o yüzden endpoint burada, purge makinesinden önce inşa edilir.

- `responses.go`: `GET /v1/responses/{id}` — 200 terminal projection (status, output, model, usage, error); verified project scope dışı veya bilinmeyen ID 404 `not_found`.
- `tests/conformance/api/responses_test.go`'ya iki case eklenir:

```go
func TestRetrieveReturnsTerminalProjection(t *testing.T)
// tamamlanmış response GET ile 200 döner; status/output/usage admission+terminal
// commit'iyle birebir aynıdır.
func TestRetrieveUnknownIDReturns404(t *testing.T)
```

- Step 1'deki purge testleri (`TestRetrieveAfterPurgeReturns410RetentionExpired`, `TestDuplicateCreateAfterPurgeReturns410WithoutReexecution`) assertion'larını DB'den değil bu HTTP endpoint'i üzerinden yapar.

Run: `go test ./tests/conformance/api -run TestRetrieve -v`

Expected: iki yeni case PASS; purge case'leri hâlâ FAIL (410 yolları Step 5'te gelir).

- [ ] **Step 4: Migration ve reaper'ı uygula**

- Migration: `responses.store boolean not null default true` (admission'da persist edilir), `responses.purged_at timestamptz`; `idempotency_records`'a `result_purged_at timestamptz`, `resource_tombstone text`, `outcome_fingerprint text`.
- `retention.go`: configured TTL'i (`PALAI_RETENTION_STORE_FALSE_TTL`; UAT'ta saniye mertebesi, production default'u bu task'ta değiştirilmez) geçmiş `store=false` terminal response'ları seçer; tek transaction'da content sütunlarını temizler, event payload'larını `{"purged": true}` ile değiştirir, artifact byte'larını siler, `purged_at` yazar ve idempotency satırını §20.9 tombstone alanlarına daraltır.
- Configured retention değeri discovery yüzeyinde yayınlanır (§20.9); Task 12 `doctor --json` bu alanı okur.

- [ ] **Step 5: 410 yollarını uygula**

`idempotency.go`: reservation lookup'ı `result_purged_at` dolu satırda 410 `idempotency_result_expired` + tombstone identity döndürür ve execution başlatmaz. `responses.go`: `purged_at` dolu response'un GET'i (Step 3 endpoint'i) 410 `retention_expired` döndürür.

- [ ] **Step 6: Testleri çalıştır**

Run: `make test-component TEST=postgres && make test-e2e TEST=responses`

Expected: PASS; retained response davranışı değişmemiştir.

- [ ] **Step 7: Commit**

```bash
git add storage/migrations apps/control-plane tests/component/postgres tests/conformance/api
git commit -m "feat: purge store-false content behind idempotency tombstones"
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
