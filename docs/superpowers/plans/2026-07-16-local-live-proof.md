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

> **Adjudication (2026-07-19, LP-0 merge `ddf2501` sonrası):** Shipped UAT case'i LP-004 (`tests/uat/cases/LP-004/case.yaml`, `live-provider-second-round-trip`) canlı bir İKİNCİ TEXT round-trip'idir; bu tablonun LP-004 satırındaki "brokered tool LIVE" kanıtı stack-level UAT'ta üretilmedi ve üretilmiyor. Canlı tool-call kapsamı adapter seviyesinde kalır: `make test-live-provider PROVIDER=provider-one CASE=text-stream-tool-schema` (`tests/live/provider/live_test.go`, gerçek provider'a karşı text+stream+tool+strict-schema). Stack-level canlı brokered-tool kanıtı SİLİNMEDİ, sahibine devredildi: master plan E06 (model/tool broker + reference kernel) canlı tool yolunu stack UAT'a bir case olarak eklemekle yükümlüdür. O kapanışa kadar LP-004 satırı "ikinci canlı text round-trip + adapter-level canlı tool" olarak okunur.

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

> **Fork kararı 3 — Task 12 kapsam revizyonu (üç çatal; implementer bulguları + plan kararı):**
>
> 1. **Secret — Option B (env-backed resolver + native Compose file-secret).** Provider credential LP10'da kanıtlanan `modelbroker.EnvResolver` yolunda kalır. "Provider secret Compose environment'ına yayılmaz" şartı native Docker file-secret ile karşılanır: top-level `secrets:` bloğu `.palai/secrets/<ref>` dosyasını yalnızca control-plane'e `/run/secrets/` altında mount eder; entrypoint değeri EnvResolver'ın okuduğu process env'ine yükler. Raw değer compose `environment:` bloğunda, `docker inspect .Config.Env` çıktısında, repo'da, argv'de veya loglarda görünmez (§20.9 secret asla cache/log'da; §41.5 file-based delivery; LP-011 secret scan). Encrypted-at-rest backend + write-only `/v1/secret-refs` admission API (§41.1/§41.2) SİLİNMEDİ: sahibi master plan E13'tür ve carve-out §7.1'de TDD adımlarıyla sıralanmıştır — bu task'ta AEAD/key-management yazmak, exec path'i tüketmeyen kapıların önüne net-new kripto koymaktır.
> 2. **Object store — present, not consumed.** SeaweedFS, ADR-0004'ün immutable index digest'i ile health-checked Compose servisi olarak kurulur ve doctor S3 ping'i yapar (§44.2: `local up` object store'u BAŞLATIR; LP-002 doctor check ister). Control-plane LP-0'da bu store'u TÜKETMEZ: kernel output/usage'ı PG JSONB'de kanıtladı ve `artifacts` tablosunun yazarı yok — write-path'siz S3 client YAGNI'dir. İlk gerçek tüketici (artifact write-path) E09'dadır — carve-out §7.2.
> 3. **Live-exec binding.** Bu task'ın runner kanıtı: gateway mTLS listener'ı local CA ile ayakta + compose runner'ı her `local up`'ta taze mint edilen one-use token ile enroll/connect oluyor + doctor non-mTLS bağlantının reddedildiğini raporluyor. TAM production exec-path (main.go'nun Orchestrator + broker + `RunnerGateway`'i EngineDialer olarak kurması) ve gerçek canlı round-trip Task 15'tedir: tek round-trip one-use token + one-shot lease tüketir, `-count=3` idempotent clean-boot döngüsüyle çelişir, tek seferlik UAT ile çelişmez. Enroll/connect wire semantiği zaten Docker'sız kanıtlıdır (`tests/conformance/engine/gateway_test.go`).

**Files:**

- Create: `deploy/compose/compose.yaml`
- Create: `deploy/compose/compose.env.example`
- Create: `deploy/compose/control-plane.Dockerfile`
- Create: `deploy/compose/control-plane-entrypoint.sh`
- Create: `deploy/compose/runner.Dockerfile`
- Create: `cmd/cli/main.go`
- Create: `cmd/cli/internal/local/up.go`
- Create: `cmd/cli/internal/local/down.go`
- Create: `cmd/cli/internal/local/doctor.go`
- Create: `cmd/cli/internal/provider/add.go`
- Create: `cmd/cli/internal/response/create.go`
- Create: `apps/control-plane/api/capabilities.go`
- Create: `apps/control-plane/internal/execution/local_credentials.go`
- Create: `apps/control-plane/internal/store/bootstrap.go`
- Modify: `apps/control-plane/cmd/palai-control-plane/main.go` (mTLS runner listener — dosyadaki "Task 12 binds the local CA and that listener" notunun kapanışı; exec-path wiring DEĞİL, o Task 15)
- Test: `tests/e2e/local/clean_boot_test.go`
- Test: `tests/e2e/local/doctor_test.go`

**Interfaces:**

- Consumes: `execution.NewRunnerGateway(issuer CertIssuer, tokens EnrollmentTokens)` + `Routes()` + `handleConnect`'in peer-cert zorlaması (Task 11c); `modelbroker.EnvResolver` semantiği (Task 10 — bu task yalnızca file→env köprüsünü kurar, broker koduna dokunmaz); `PALAI_RETENTION_STORE_FALSE_TTL` (Task 11d); `coordinator.HashAPIKey` (Task 3/5 verifier); ADR-0004 SeaweedFS digest; CI'ın pinlediği PostgreSQL digest (`.github/workflows/ci.yml:65`).
- Produces (Task 15 bunları tüketir): `palai` CLI — `init | local up | local down | local reset --confirm | local doctor [--json] | provider add | response create` (stdlib `flag`; go.mod'a cobra benzeri dependency EKLENMEZ); `.palai/` layout (aşağıda); `GET /v1/capabilities` discovery JSON'u (maturity/isolation + §20.9 configured retention); compose env sözleşmesi: `PALAI_RUNNER_LISTEN_ADDR`, `PALAI_RUNNER_CA_CERT`, `PALAI_RUNNER_CA_KEY`, `PALAI_RUNNER_SERVER_CERT`, `PALAI_RUNNER_SERVER_KEY`, `PALAI_ENROLLMENT_TOKEN_FILE`, `PALAI_BOOTSTRAP_API_KEY_FILE`, `PALAI_ENGINE_IMAGE`.

`palai init`'in ürettiği `.palai/` layout (dizin 0700, dosyalar 0600):

```text
.palai/
├── config.json          # data dir, portlar, project ID, base URL
├── api-key              # bootstrap dev API key (bir kez mint edilir)
├── ca/ca.crt  ca/ca.key # local CA (gateway mTLS) + server cert/key
├── runner-token         # her `local up`'ta yeniden mint edilen one-use enrollment token
└── secrets/<ref>        # `provider add`'in yazdığı secret dosyaları
```

- [ ] **Step 1: Failing clean-boot/doctor tests yaz**

Tests izole temp `PALAI_HOME` + random portlar kullanır; gerçek `docker compose` çalıştırır (CI'daki component lane guard'ı ile aynı Docker gereksinimi):

```go
func TestCleanBootUpDoctorDownRetainsData(t *testing.T)
// palai init + local up: dört servis healthy; doctor --json bütün check'ler green.
// local down volume silmez; ikinci local up aynı veriyi geri getirir (LP-012).
// Gövde -count=3 ile idempotenttir: her up taze one-use enrollment token mint eder.

func TestResetRequiresConfirm(t *testing.T)
// local reset, --confirm bayrağı olmadan non-zero exit döner ve hiçbir volume silinmez;
// --confirm ile volume'lar gerçekten gider (§44.4).

func TestDoctorJSONShape(t *testing.T)
// doctor --json alanları: api, migration, object_store, runner, image_digests,
// provider, clock, retention_ttl, runner_tls_reject — her biri {status, detail}.
// retention_ttl değeri GET /v1/capabilities'ten okunur (Task 11d discovery şartı);
// object_store ADR-0004 digest'inin çalıştığını S3 ping ile raporlar.

func TestRunnerPortRejectsNonMTLS(t *testing.T)
// Client cert'siz TLS bağlantısı /v1/runner/connect'te 401 alır; plain HTTP handshake
// reddedilir. (Binding-1 kanıtı — canlı round-trip Task 15'te.)

func TestProviderSecretIsAbsentFromComposeSurfaces(t *testing.T)
// Sentinel değerle provider add sonrası: compose.yaml, `docker compose config` çıktısı,
// control-plane container'ının `docker inspect` .Config.Env çıktısı ve CLI argv'si
// sentinel içermez; sentinel yalnızca .palai/secrets/<ref> (0600) dosyasındadır.

func TestResponseCreateAdmitsOverBootstrapKey(t *testing.T)
// palai response create --input "hello" 202 + response ID döner (LP-001'in "documented
// command, no manual SQL" kanıtı); GET /v1/responses/{id} aynı key ile erişilebilir.
// Terminal outcome bu task'ın kapsamı değildir (exec path Task 15'te bağlanır).
```

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./tests/e2e/local -v`

Expected: CLI/Compose assets absent nedeniyle FAIL.

- [ ] **Step 3: Compose stack'i oluştur**

`deploy/compose/compose.yaml` dört servis + bir build edilen engine image:

```yaml
services:
  postgres:
    image: postgres@sha256:17e67d7b9890c99b055ba1e0d5c5be4ec27c9d3a72bda32db24a5e5d8a85af0c  # CI ile aynı pin
    environment: {POSTGRES_USER: palai, POSTGRES_PASSWORD_FILE: /run/secrets/pg_password, POSTGRES_DB: palai}
    secrets: [pg_password]
    volumes: [palai-pg:/var/lib/postgresql/data]
    healthcheck: {test: ["CMD-SHELL", "pg_isready -U palai"], interval: 2s, timeout: 2s, retries: 30}
  object-store:
    image: docker.io/chrislusf/seaweedfs@sha256:c7d6c721b30ae711db766bbbfd40192776e263d4e51e22f57baef7bef93c12c6  # ADR-0004
    command: ["server", "-s3", "-dir=/data"]
    volumes: [palai-objects:/data]
    healthcheck: {test: ["CMD-SHELL", "wget -q -O- http://127.0.0.1:8333/ || exit 1"], interval: 2s, retries: 60}
  control-plane:
    build: {context: ../.., dockerfile: deploy/compose/control-plane.Dockerfile}
    environment:  # secret DEĞERİ burada bulunmaz
      PALAI_DATABASE_URL: postgres://palai@postgres:5432/palai
      PALAI_RUNNER_LISTEN_ADDR: ":8443"
      PALAI_RUNNER_CA_CERT: /palai/ca/ca.crt
      PALAI_RUNNER_CA_KEY: /palai/ca/ca.key
      PALAI_RUNNER_SERVER_CERT: /palai/ca/server.crt
      PALAI_RUNNER_SERVER_KEY: /palai/ca/server.key
      PALAI_ENROLLMENT_TOKEN_FILE: /palai/runner-token
      PALAI_BOOTSTRAP_API_KEY_FILE: /run/secrets/bootstrap_api_key
      PALAI_ENGINE_IMAGE: ${PALAI_ENGINE_IMAGE}
      PALAI_RETENTION_STORE_FALSE_TTL: ${PALAI_RETENTION_STORE_FALSE_TTL:-}
    secrets: [provider_one_key, bootstrap_api_key, pg_password]
    volumes: ["${PALAI_HOME}/ca:/palai/ca:ro", "${PALAI_HOME}/runner-token:/palai/runner-token:ro"]
    ports: ["${PALAI_API_PORT}:8080", "${PALAI_RUNNER_PORT}:8443"]
    depends_on: {postgres: {condition: service_healthy}, object-store: {condition: service_healthy}}
  runner:
    build: {context: ../.., dockerfile: deploy/compose/runner.Dockerfile}
    environment:
      PALAI_CONTROLLER_URL: https://control-plane:8443
      PALAI_RUNNER_CA_CERT: /palai/ca/ca.crt
      PALAI_ENROLLMENT_TOKEN_FILE: /palai/runner-token  # one-use; connect'te tüketilir, runner disk'te tutmaz
      PALAI_ENGINE_IMAGE: ${PALAI_ENGINE_IMAGE}
    volumes: ["/var/run/docker.sock:/var/run/docker.sock", "${PALAI_HOME}/ca/ca.crt:/palai/ca/ca.crt:ro", "${PALAI_HOME}/runner-token:/palai/runner-token:ro"]
    depends_on: {control-plane: {condition: service_started}}
secrets:
  provider_one_key: {file: "${PALAI_HOME}/secrets/provider-one"}
  bootstrap_api_key: {file: "${PALAI_HOME}/api-key"}
  pg_password: {file: "${PALAI_HOME}/secrets/pg-password"}
volumes: {palai-pg: {}, palai-objects: {}}
```

Kurallar: yalnızca runner Docker socket mount eder; engine container'ına socket/DB/S3/provider secret asla geçmez (Task 8 allowlist zaten zorluyor). Reference engine bir compose servisi DEĞİLDİR — `local up` onu `engines/reference/Dockerfile`'dan build eder ve image referansını `PALAI_ENGINE_IMAGE` olarak geçirir. `control-plane-entrypoint.sh` file-secret → env köprüsüdür:

```sh
#!/bin/sh
set -eu
# ponytail: file-secret -> process env köprüsü; E13 encrypted backend bu köprüyü söker (LP planı §7.1)
if [ -f /run/secrets/provider_one_key ]; then
  PALAI_SECRET_PROVIDER_ONE="$(cat /run/secrets/provider_one_key)"; export PALAI_SECRET_PROVIDER_ONE
fi
exec /usr/local/bin/palai-control-plane
```

- [ ] **Step 4: mTLS runner listener'ını ve capabilities discovery'yi uygula**

- `apps/control-plane/internal/execution/local_credentials.go`:

```go
// NewFileCertIssuer, .palai CA cert/key dosyalarıyla CertIssuer'ı uygular.
func NewFileCertIssuer(certPath, keyPath string) (*FileCertIssuer, error)
// NewFileEnrollmentTokens, tek satırlık token dosyasını one-use tüketir:
// Consume başarılı ilk çağrıda in-memory işaretler, sonrakiler error döner.
func NewFileEnrollmentTokens(path string) *FileEnrollmentTokens
```

- `main.go`: `PALAI_RUNNER_LISTEN_ADDR` doluysa ikinci bir `http.Server` açılır — `Handler: gw.Routes()`, `TLSConfig: &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: caPool, ClientAuth: tls.VerifyClientCertIfGiven}`. Enroll bearer token'la cert'siz erişir; `handleConnect` peer certificate'ı zaten zorlar (Task 11c). Public router'daki `nil` runner handler AYNEN kalır; gateway bu task'ta hiçbir worker'a EngineDialer olarak bağlanmaz.
- `apps/control-plane/internal/store/bootstrap.go`: startup'ta `api_keys` boşsa dev organization/project/principal + `coordinator.HashAPIKey(bootstrap key)` verifier'lı bir api_keys satırı yazar (key değeri `PALAI_BOOTSTRAP_API_KEY_FILE`'dan okunur; DB'ye yalnızca hash girer). LP-001'in "manual SQL yok" şartının sunucu yarısı budur.
- `apps/control-plane/api/capabilities.go`: `GET /v1/capabilities` (bearer auth'lu) şunu döner:

```json
{"object": "capabilities", "maturity": "preview", "isolation": "development",
 "retention": {"store_false_ttl_seconds": 0},
 "capabilities": {"responses": "preview", "sessions": "unavailable", "workspaces": "unavailable"}}
```

`store_false_ttl_seconds` configured TTL'den doldurulur (§20.9 "publish configured retention through discovery"; LP planı §2 maturity ilanı).

Run: `go test ./tests/conformance/api -run TestCapabilities -v` (yeni küçük conformance case'i aynı adımda eklenir)

Expected: PASS.

- [ ] **Step 5: CLI lifecycle ve doctor'ı uygula**

`cmd/cli` stdlib `flag` ile subcommand dispatch yapar (`os.Args[1:]` üzerinde manuel switch; yeni dependency yok):

- `palai init`: `.palai/` layout'u üretir — random API key + pg password mint, local CA + server cert üret, `config.json` yaz.
- `palai local up`: (1) `docker build engines/reference` → `PALAI_ENGINE_IMAGE`; (2) taze one-use runner token mint edip `.palai/runner-token`'a yazar; (3) `docker compose up -d --build --wait`; (4) migration/health'i doctor'ın api/migration check'leri green olana kadar bekler.
- `palai local down`: `docker compose down` — volume SİLMEZ. `palai local reset --confirm`: `down --volumes` + `.palai` data temizliği; `--confirm` yoksa non-zero exit.
- `palai provider add <ref>`: secret değerini stdin'den okur (argv'ye asla yazılmaz), `.palai/secrets/<ref>` dosyasına 0600 yazar; compose `secrets:` mount'u sonraki `up`'ta control-plane'e taşır.
- `palai local doctor [--json]`: check'ler — `api` (`/v1/capabilities` 200), `migration` (schema_migrations current), `object_store` (aws-sdk-go-v2 ile S3 endpoint ping; go.mod'da zaten var), `runner` (gateway'in connected-runner state'i), `image_digests` (postgres + seaweedfs resolved digest'leri pin'lerle birebir; control-plane/runner/engine locally-built label — release digest pinning E18 işi), `provider` (secret dosyası mevcut + boş değil; canlı probe Task 15 `--live`), `clock` (DB `now()` ile host saat farkı < 2s), `retention_ttl` (capabilities'ten), `runner_tls_reject` (cert'siz connect denemesi 401 aldı).
- `palai response create --input <text>`: bootstrap key ile `POST /v1/responses` çağırır, response ID + status basar.

- [ ] **Step 6: Clean local tests çalıştır**

Run: `go test ./tests/e2e/local -count=3 -v`

Expected: PASS; repeated up/down idempotent (her `up` taze token mint eder); data retained; doctor bütün check'ler green; sentinel secret hiçbir compose yüzeyinde yok.

- [ ] **Step 7: Commit**

```bash
git add deploy/compose cmd/cli apps/control-plane tests/e2e/local
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
- Modify: `apps/control-plane/internal/execution/finalize.go` (Step 1 — projection'a model + error)
- Modify: `apps/control-plane/internal/store/postgres.go` (Step 1 — GET model/error'ı geri yansıtır)
- Modify: `packages/coordinator/lease.go` (Step 1 — dead-letter projection'a problem-shaped error)
- Modify: `storage/queries/responses.sql` (Step 1 — scrub_events ceiling yorumu)
- Test: `apps/control-plane/e2e/responses/live_loop_test.go` (Modify — completed GET model assertion)
- Test: `apps/control-plane/e2e/responses/dead_letter_test.go` (Modify — failed GET error assertion)

- [ ] **Step 1: Terminal projection'ı model ve error ile tamamla (Task 11d review follow-up)**

> Task 11d Step 3 GET sözleşmesini "(status, output, model, usage, error)" olarak kurdu; ama finalize durable projection'a yalnızca `{output, usage}` yazar (`apps/control-plane/internal/execution/finalize.go:50`), orphaned-run köprüsü sabit `{"output":[],"usage":{}}` yazar (`packages/coordinator/lease.go` `deadLetterProjection`) ve `store/postgres.go` `GetResponse` yalnızca bu iki alanı geri okur. Gerçek sunucuda GET bu yüzden `model:""` döndürür ve failed bir response'un GET'inde `error` alanı hiç yoktur — oysa `protocols/schemas/execution/response.json` `model`'i required sayar ve `error`'ı `oneOf[problem, null]` modeller (§8.3 retrieval sözleşmesi). Conformance suite bu açığı yakalayamaz çünkü fake backend admission gövdesini verbatim saklar; kanıt Task 11 e2e harness'ında yaşar. SDK `retrieve()`/`finalResponse()` bu şekle bağlanmadan ve Task 15 failed-response retrieval'ı canlı UAT'lamadan önce sunucu tarafı tamamlanmalıdır.

Önce iki failing test yazılır (Task 11 harness'ının `getResponse` HTTP helper'ı ile):

```go
// apps/control-plane/e2e/responses/live_loop_test.go
func TestRetrieveCompletedResponseCarriesUsedModel(t *testing.T)
// live-loop akışı terminal'e ulaştıktan sonra GET /v1/responses/{id} gövdesindeki
// "model" boş değildir ve committed model result'taki gerçekten kullanılan modeldir.

// apps/control-plane/e2e/responses/dead_letter_test.go
func TestRetrieveDeadLetteredResponseCarriesProblemError(t *testing.T)
// reconciler köprüsünün failed'a sürdüğü response'un GET gövdesindeki "error"
// problem-shaped bir objedir (code/message dolu); completed response GET'inde
// error alanı yoktur (null/absent).
```

Sonra implementasyon:

- `finalize.go`: attemptState, committed model result'tan gerçekten kullanılan modeli taşır (`modelbroker.Result.Model`; hem taze route hem `LookupModelResult` replay branch'i doldurur) ve projection `{output, usage, model}` olur; failed/timed_out/budget_exceeded/canceled outcome'larında projection'a sanitize edilmiş problem-shaped `error` eklenir (raw provider/engine detayı sızmaz, §22.3).
- `packages/coordinator/lease.go`: `deadLetterProjection` problem-shaped bir `error` taşır; run hiç model step'e ulaşmadıysa model boş kalabilir (schema `model`'i boş string olarak kabul eder — non-empty model assertion'ı yalnızca completed yolundadır).
- `store/postgres.go` `GetResponse`: inline projection struct'ına `Model` ve `Error` eklenir ve GET gövdesine yansıtılır; 106. satırdaki "Model is not part of the durable terminal projection" yorumu artık yanlıştır, kaldırılır.
- `storage/queries/responses.sql` (Task 11d review follow-up 2 — ceiling notu, davranış değişikliği yok): `scrub_events` CTE'sinin üstüne yorum eklenir: `-- ponytail: session-level scrub — create her response'a taze session açtığı için bugün doğru (session:response 1:1); session reuse / previous_response_id chaining gelirse purge response başına yeniden anahtarlanmalı, yoksa store:false purge aynı session'daki retained kardeş response'ların journal'ını da siler (events'te per-response anahtar yok).`

Run: `make test-e2e TEST=responses && go test ./tests/conformance/api -run TestRetrieve -v`

Expected: iki yeni test PASS; mevcut retrieve/store-false/dead-letter case'leri regress etmez.

```bash
git add apps/control-plane packages/coordinator storage/queries
git commit -m "fix: carry model and error in the terminal response projection"
```

- [ ] **Step 2: Failing SDK contract tests yaz**

Cases: constructor config precedence; no unrelated provider env discovery; automatic stable idempotency key across retry; AsyncIterable SSE; reconnect/dedupe; explicit cancel; RFC 9457 typed error; unknown event/enum preservation; browser import of server credential path fails.

- [ ] **Step 3: Fail'i doğrula**

Run: `pnpm --dir sdks/typescript test`

Expected: package/source absent nedeniyle FAIL.

- [ ] **Step 4: Generated transport types ve handwritten ergonomic layer uygula**

Public surface:

```ts
const client = new Palai({ baseURL, apiKey, project });
const response = await client.responses.create(request, options);
const stream = client.responses.stream(request, options);
const stored = await client.responses.retrieve(responseID);
await client.responses.cancel(responseID);
```

`stream.finalResponse()` canonical terminal object döndürür. Iterator close yalnızca transport'u kapatır. API key browser-safe entrypoint'ten export edilmez; package conditional exports ile server path ayırır.

- [ ] **Step 5: SDK tests ve generated drift çalıştır**

Run: `pnpm --dir sdks/typescript test && make check-generated`

Expected: PASS; retry capture aynı idempotency key gösterir.

- [ ] **Step 6: Commit**

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

### Task 14b: Sözleşmeli response cancel endpoint'ini mount et ve kanıtla (Task 13 review follow-up)

> **Sözleşme boşluğu:** `POST /v1/responses/{response_id}/cancel` kanonik OpenAPI'de sözleşmelidir (`protocols/openapi/openapi-3.2.yaml:107` — 202 "Cancellation accepted" + kanonik response projection) ve Task 13 SDK'sı `client.responses.cancel()` ile tam bu path'i çağırır; ama `apps/control-plane/api/router.go` cancel route'u mount ETMEZ — canlı sunucuda cancel bugün 404 döner. Kanonik makine hazırdır: `statemachines.RunCmdCancel` queued/provisioning/running/waiting'den `run.canceled.v1` üretir (`packages/state-machines/run.go:40`), `run.canceled.v1` SSE terminal-close listesindedir (`apps/control-plane/api/events.go` `terminalEventTypes`) ve `finalize.go:40` canceled terminal projection'ın problem shape'ini zaten tanımlar. Eksik olan HTTP mount + cancel transaction + bir invariant'tır: bugün `CommitModelRequest`/`CommitModelResult`/`CommitToolResult` (`packages/coordinator/orchestration.go`) run state'ine bakmaz; running bir response cancel edildiğinde in-flight attempt'in sonraki commit'i terminal event'ten SONRA journal'a event sızdırır ve "exactly one canonical terminal, contiguous events" sözleşmesini kırar. Bu task Task 15'ten ÖNCE kapanmak ZORUNDADIR: §2 kapsamı "cancel'ın minimum canonical behavior'ını" içerir ve canlı UAT'ta SDK cancel'ının 404 alması LP-0'ın "gerçekten çalışıyor" kapısını düşürür. Çapalar: OpenAPI cancel sözleşmesi (`openapi-3.2.yaml:107`), §22.3 (run/response terminality monotonic), §8.3 (retrieval projection).

**Files:**

- Modify: `apps/control-plane/api/router.go` (cancel route mount)
- Modify: `apps/control-plane/api/responses.go` (cancel handler; `Admitter` seam'ine `CancelResponse`)
- Modify: `apps/control-plane/internal/store/postgres.go` (`CancelResponse` transaction)
- Modify: `packages/coordinator/orchestration.go` (terminal-run commit guard)
- Modify: `apps/control-plane/internal/execution/orchestrator.go` (dispatch'te `ErrRunTerminal` → temiz abort)
- Test: `apps/control-plane/e2e/responses/cancel_test.go`
- Test: `tests/conformance/api/responses_test.go` (Modify — cancel 401/404 case'leri)

**Interfaces:**

- Consumes: `statemachines.RunCmdCancel` + RunTable cancel geçişleri (phase-02); `coordinator.ApplyRunTransition`/`ErrRunTerminal`/`FinalizeResponse` (Task 3/7/11); `finalize.go` canceled problem shape'i (Task 13 Step 1); Task 11 e2e harness + `dead_letter_test.go`'nun SSE-close assertion kalıbı; Task 6 SSE terminal close.
- Produces: `POST /v1/responses/{response_id}/cancel` — authenticated + tenant-scoped, 202 + kanonik response projection; Task 15 UAT'ı ve SDK `responses.cancel()` canlı sunucuda bunu tüketir.

- [ ] **Step 1: Failing cancel tests yaz**

`apps/control-plane/e2e/responses/cancel_test.go` (Task 11 harness'ını yeniden kullanır):

```go
func TestCancelRunningResponseReachesCanceledTerminal(t *testing.T)
// Scripted channel engine.ready + ilk model.request'i verir, model.result'ı aldıktan
// sonra bekler (run `running`, attempt in-flight). POST /v1/responses/{id}/cancel →
// 202 + status=canceled projection. Assert: run satırı canceled; journal'ın SON
// event'i run.canceled.v1 ve SSE reader terminal event'ten sonra stream'in temiz
// kapandığını görür; GET /v1/responses/{id} canceled terminal + problem-shaped error
// döner. Sonra scripted engine İKİNCİ bir model.request verir: hiçbir yeni event
// oluşmaz (terminalden sonra journal büyümez), fake provider çağrı sayacı artmaz,
// attempt temiz düşer ve job dead-letter'a inmeden settle olur — exactly-one terminal.

func TestCancelIsScopedToProject(t *testing.T)
// İkinci projenin API key'i ile cancel → 404 not_found (LP5 scope-immunity
// precedent'i: foreign id varlık sızdırmaz); run cancel OLMAZ, event üretilmez.

func TestCancelAfterTerminalIsSafe(t *testing.T)
// Tamamlanmış (completed) response'a cancel → 202 + MEVCUT completed projection;
// hiçbir transition/event üretilmez (§22.3 terminal monotonicity — double-terminal
// yok); GET hâlâ completed döner. Canceled bir response'a cancel tekrarı da aynı
// no-op sözleşmesindedir — cancel retry-safe'tir.
```

`tests/conformance/api/responses_test.go`'ya iki küçük case eklenir: `TestCancelUnknownIDReturns404` ve `TestCancelRequiresAuth` (401). Cancel `Idempotency-Key` GEREKTİRMEZ — OpenAPI cancel operasyonu key parametresi tanımlamaz; route `RequireIdempotencyKey` ile sarılmaz (doğal idempotent).

- [ ] **Step 2: Fail'i doğrula**

Run: `go test ./apps/control-plane/e2e/responses -run TestCancel -v && go test ./tests/conformance/api -run TestCancel -v`

Expected: route mount edilmediği için tüm case'ler 404 → FAIL.

- [ ] **Step 3: Cancel transaction'ını, handler'ı ve terminal-commit guard'ını uygula**

- `store/postgres.go` — `CancelResponse(ctx, scope, id)`: tek transaction'da scope'lu response→run çözümü (bilinmeyen/yabancı id not-found; `get` ile aynı 404 sözleşmesi). Non-terminal run'a `ApplyRunTransition(RunCmdCancel)` uygulanır (RunTable `run.canceled.v1` terminal event'ini aynı transaction'da journal'a yazar — Task 3 kuralı, SSE bu event'le kapanır) ve `FinalizeResponse(tenant, responseID, "canceled", projection)` canceled projection'ı yazar. Projection dead-letter köprüsüyle aynı kalıptadır: boş output, boş usage özeti (kanonik usage kaydı committed `usage_events` satırlarında kalır — LP-013), model boş olabilir (schema non-completed'da boş model kabul eder — Task 13 Step 1), `error` = `finalize.go:40` canceled problem shape'i. `ErrRunTerminal` → no-op branch: transition/event üretmeden mevcut terminal projection okunur ve döndürülür.
- `responses.go` — `cancel` handler: `middleware.ScopeFrom` + `admitter.CancelResponse`; not-found → `WriteProblem(404, "not_found", ...)` (`get` ile aynı); başarı → 202 + projection + korelasyon header'ları. `router.go`: `mux.HandleFunc("POST /v1/responses/{response_id}/cancel", responses.cancel)`.
- `packages/coordinator/orchestration.go` — invariant: `CommitModelRequest`/`CommitModelResult`/`CommitToolResult` kendi transaction'ları içinde run satırını kilitleyip non-terminal olduğunu doğrular; terminal run'a commit `ErrRunTerminal` döndürür. Terminal event'ten sonra journal'a hiçbir event giremez — "terminal is the journal's end" sözleşmesi cancel altında da korunur.
- `orchestrator.go` — dispatch yolunda `errors.Is(err, coordinator.ErrRunTerminal)` temiz abort'tur (attempt hatasız biter; run zaten terminal). ExecuteAttempt'in açılıştaki mevcut `ErrRunTerminal` no-op'u (`orchestrator.go:74`) queued'dan cancel edilen run'ların sonraki claim'ini zaten susturur — yeni davranış yalnızca mid-attempt yakalamadır. Ayrı bir in-memory cancel sinyali/registry EKLENMEZ (ponytail: DB guard cross-process doğrudur; graceful `run.cancel` engine frame'i E10 graceful-cancel işidir).

- [ ] **Step 4: Testleri yeşil çalıştır**

Run: `go test ./apps/control-plane/e2e/responses -run TestCancel -v && go test ./tests/conformance/api -run TestCancel -v && make test-e2e TEST=responses`

Expected: cancel case'leri PASS; live_loop/dead_letter/reclaim/store_false regress etmez; `make check-generated` zero-drift kalır (OpenAPI değişmedi — route zaten sözleşmeliydi).

- [ ] **Step 5: Commit**

```bash
git add apps/control-plane packages/coordinator tests/conformance/api
git commit -m "feat: mount the contracted response cancel endpoint"
```

### Task 15: Live UAT harness, production exec-path wiring ve evidence verifier

> **Fork kararı 3'ün canlı yarısı — sert kısıt:** LP-0'ın adı "Local Live Proof"tur; e2e harness'ta kanıtlanmış bir orchestrator, main.go onu hiç kurmuyorsa "gerçekten çalışıyor" sayılmaz. Task 12 bilinçli olarak yalnızca listener + reject-check bağladı (idempotent `-count=3` döngüsü one-use token/one-shot lease tüketemez). TAM production exec-path — main.go'nun broker (EnvResolver + gerçek provider adapter) + `RunnerGateway` EngineDialer + non-nil execute handler kurması — BU task'ta bağlanır ve LP-003/LP-004 gerçek topolojiden (create → gateway → runner → OCI engine → gerçek provider → terminal) TEK canlı round-trip ile kanıtlanır. One-use token ve one-shot lease burada sorun değildir: UAT bir kez koşar, `-count=3` koşmaz. Bu adımlar kapanmadan LP suite çalıştırılamaz.

> **Fork kararı 4 — model route seçimi (Option A; implementer bulgusu + plan kararı):** Orchestrator'ın broker koordinatları derleme sabitiydi (`model_dispatch.go`: provider/model `"fake"`, secret ref `"model"`) ve hiçbir şey env'den seçilen provider/model/secret'ı `models.Route` çağrısına taşımıyordu. Bu task Orchestrator'a, default'u mevcut sabitler olan tek bir `ModelRoute{Provider, Model, Secret}` alanı ekler (mevcut deterministic testler yeşil kalır) ve `main.go` `PALAI_MODEL_PROVIDER=provider-one` seçildiğinde alanı env'den doldurur. Bu, LP-0'ın "actually works" kapsamıdır; DB-backed routing DEĞİLDİR. Şemada var olan ama okuyucusuz `model_routes`/`model_route_revisions`/`model_connections` tablolarının tüketimi (§27.6/§27.7 per-project route seçimi) SİLİNMEDİ: sahibi master plan E06'dır ve carve-out §7.3'te TDD adımlarıyla sıralanmıştır — o carve-out kapanırken bu env-configured ModelRoute sökülür.

**Files:**

- Create: `apps/control-plane/internal/execution/execute_run.go`
- Modify: `apps/control-plane/cmd/palai-control-plane/main.go` (exec-path wiring — Task 12'nin bıraktığı son boşluk)
- Modify: `deploy/compose/compose.env.example` (`PALAI_MODEL_PROVIDER` seçimi)
- Test: `tests/e2e/local/live_wiring_test.go`
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

**Interfaces:**

- Consumes: `execution.NewOrchestrator(st, dialer, models, tools)` + `ExecuteAttempt` (Task 11); `RunnerGateway.Dial` EngineDialer implementasyonu (Task 11c); `modelbroker.New(Config{Secrets: EnvResolver{...}})` + `provider_one.Adapter`/`fake` adapter + `toolbroker.New(toolbroker.ConformanceMathAdd())` (Task 10); `execution.AdvanceRun`'ın assignment plan'ı ve `coordinator.Claim.Fence` (Task 7); Task 12'nin compose env sözleşmesi, `.palai/` layout'u, doctor ve CLI komutları.
- Produces: `execution.ExecuteRun(spine *coordinator.Store, st *store.Store, orch *Orchestrator) coordinator.Handler` — production worker handler'ı; LP-001..015 case runner + evidence verifier.

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

- [ ] **Step 5: Failing production-wiring testi yaz (deterministic, fake provider, gerçek topoloji)**

`tests/e2e/local/live_wiring_test.go` — Task 12'nin compose harness'ını yeniden kullanır; canlı UAT'tan ÖNCE aynı wiring'i shipped binary üzerinden fake provider ile kanıtlar (proof class `e2e-deterministic`; LP-003/004'ün live-provider sınıfının yerine GEÇMEZ):

```go
func TestShippedBinaryCompletesOneResponseThroughLiveTopology(t *testing.T)
// PALAI_MODEL_PROVIDER=fake ile local up; palai response create --input "hello".
// Assert: response TERMINAL completed'a ulaşır; runner compose mTLS üzerinden enroll
// oldu (doctor runner check + gateway state); engine GERÇEK OCI container olarak
// koştu (docker inspect/events receipt, Task 12 image'ı); canonical event dizisi
// contiguous ve exactly-one terminal. Orchestration in-process test kablosu DEĞİL,
// main.go'nun kendi wiring'idir.
```

- [ ] **Step 6: Fail'i doğrula, sonra production exec-path'i main.go'ya bağla**

Run: `go test ./tests/e2e/local -run TestShippedBinary -v`

Expected: FAIL — run `running`'de asılı kalıp dead-letter köprüsüyle failed olur (Task 11c köprüsü), completed'a asla ulaşmaz; çünkü `main.go` yalnızca `AdvanceRun` (assignment-only) kurar, hiçbir broker/dialer/engine yolu yoktur.

Sonra implementasyon:

- `apps/control-plane/internal/execution/execute_run.go` — `ExecuteRun(spine, st, orch) coordinator.Handler`: `AdvanceRun`'ın idempotent assignment plan'ını aynen uygular, sonra `AttemptDescriptor{RunID, AttemptID, Fence: claim.Fence, ImageDigest: cfg engine image, Limits: defaults}` kurup `orch.ExecuteAttempt(ctx, desc)` çağırır. Reclaim edilen job daha yüksek `claim.Fence` ile yeni attempt açar (Task 11c reclaim/idempotency-key testlerinin kanıtladığı yol); hata coordinator retry/dead-letter politikasına düşer, gizli retry yok.
- `main.go` `startDispatch`: `PALAI_MODEL_PROVIDER` (`fake` | `provider-one`) adapter'ı seçer; broker `modelbroker.New(modelbroker.Config{Secrets: modelbroker.EnvResolver{"provider-one": "PALAI_SECRET_PROVIDER_ONE"}, ...})` ile kurulur (env değeri Task 12 file-secret köprüsünden gelir); `tools := toolbroker.New(toolbroker.ConformanceMathAdd())`; Task 12'nin gateway'i `EngineDialer` olarak `execution.NewOrchestrator(repo, gw, models, tools)`'a verilir ve worker handler'ı `execution.AdvanceRun(spine)` yerine `execution.ExecuteRun(spine, repo, orch)` olur. Runner listener kapalıysa (env yok) eski assignment-only davranış korunur — SSE read-path e2e'leri kırılmaz.

- [ ] **Step 7: Wiring testini yeşil çalıştır**

Run: `go test ./tests/e2e/local -run TestShippedBinary -v && make test-e2e TEST=responses`

Expected: PASS; deterministic suite regress etmez; subprocess channel yalnızca deterministic suite'te kalır.

- [ ] **Step 8: Protected real-provider LP suite çalıştır**

LP-003/LP-004 artık gerçek production topolojisinden geçer: `main.go` wiring → gateway → compose runner (one-use token, tek enroll) → reference-engine OCI → gerçek provider. Evidence şunları içermek ZORUNDADIR: gerçek provider request ID'leri, runner'ın mTLS enroll/connect audit kaydı, engine container'ının digest'li OCI receipt'i ve exactly-one canonical terminal — "e2e harness'ta çalışıyor" bu kapıyı geçmez.

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

- [ ] **Step 9: Evidence bundle verify et**

Run: `make evidence-verify RELEASE=local-live-0.1.0`

Expected: `15 passed, 0 failed, 0 missing, 0 secret findings`.

- [ ] **Step 10: Commit redacted fixtures ve summary**

```bash
git add apps/control-plane deploy/compose tests/e2e/local tests/uat scripts/evidence protocols/schemas/evidence evidence/releases/local-live-0.1.0
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

1. `phase-08-interactive-sessions.md` ile durable chat/commands/config revisions. Not (Task 11d review follow-up): session reuse / `previous_response_id` chaining, store:false purge'ü response başına yeniden anahtarlamadan gelemez — bugünkü `scrub_events` (`storage/queries/responses.sql`) session'ın TÜM event'lerini temizler ve yalnızca session:response 1:1 olduğu için doğrudur.
2. `phase-09-repository-coding.md` ile gerçek coding workspace (§7.2 object-store carve-out'unu içerir).
3. `phase-10-recovery-replay.md` ile process/container/host kill ve side-effect replay.
4. `phase-14-production-self-host.md` ile TLS/backup/cloud VM (§7.1 kapanmadan production secret iddiası verilmez).

Bu dört kapı geçmeden local proof production self-host iddiasına çevrilmez.

### 7.1 Carve-out: Encrypted SecretRef backend ve write-only admission API (ev: E13, `phase-13-governance-data.md`)

Task 12 fork kararı 1'in borcu. LP-0 provider secret'ı için geçici mekanizma: `.palai/secrets/<ref>` dosyası → Compose file-secret → entrypoint env köprüsü → `modelbroker.EnvResolver`. §41.1 ("Creation accepts a write-only value"; GET asla value döndürmez) ve §41.2 (baseline self-host backend = envelope encryption) LP-0'da UYGULANMADI; master plan E13'ün "Envelope-encrypted SecretRef backend" maddesi bu carve-out'un sahibidir. E13 child planı şu task'ı içermek ZORUNDADIR:

**Files:** Create `apps/control-plane/internal/identity/secrets.go`, `apps/control-plane/api/secret_refs.go`, `storage/migrations/00000N_secret_refs.up.sql`; Modify `apps/control-plane/cmd/palai-control-plane/main.go` (EnvResolver → DB-backed sealing resolver), `cmd/cli/internal/provider/add.go` (dosya yazmak yerine `POST /v1/secret-refs`), `deploy/compose/control-plane-entrypoint.sh` (file-secret köprüsü SÖKÜLÜR).

- [ ] Failing testler: `TestSecretValueIsAEADSealedAtRest` (DB satırında plaintext yok; `pgcrypto` değil uygulama-seviyesi AEAD — random per-secret data key, master key ile wrap, §41.2); `TestGetSecretRefNeverReturnsValue` (§41.1); `TestProviderAddCreatesWriteOnlySecretRef` (CLI → `POST /v1/secret-refs`, value yalnızca request body'de, audit/log/idempotency cache'te yok — §20.9); `TestBrokerRedeemsSealedRefOnlyAtCallTime` (resolver decrypt'i yalnızca executor çağrısında yapar; Task 10 `SecretResolver` seam'i DEĞİŞMEZ).
- [ ] Fail'i doğrula; sonra envelope encryption backend + `/v1/secret-refs` create/versions/revoke endpoint'lerini (§20.7 route listesi) uygula; master key `.palai/ca` gibi `palai init`'in ürettiği dosyadan gelir, production'da §45 deployment master key'idir.
- [ ] `make test-component && go test ./tests/security/secrets -v` PASS; LP-011 secret scan'i yeni yüzeylerde yeniden koşulur.

Bu carve-out kapanana kadar discovery `secret_refs: "unavailable"` ilan eder ve hiçbir sürüm "production secret handling" iddiası taşımaz.

### 7.2 Carve-out: Object-store consumption / artifact write-path (ev: E09, `phase-09-repository-coding.md`)

Task 12 fork kararı 2'nin borcu. LP-0'da SeaweedFS (ADR-0004 digest'i ile) compose'da sağlıklı çalışır ve doctor S3 ping'i yapar; ama control-plane'de tek S3 çağrısı yoktur — `responses.output` PG JSONB'dedir ve `artifacts` tablosunun yazarı yoktur. İlk gerçek artifact üreticisi E09'un patch/test artifacts işidir; S3 client oraya, write-path ile BİRLİKTE gelir. E09 child planı şu task'ı içermek ZORUNDADIR:

**Files:** Create `apps/control-plane/internal/artifacts/store.go` (S3-compatible boundary; aws-sdk-go-v2 zaten go.mod'da), `apps/control-plane/internal/artifacts/writer.go`; Modify `apps/control-plane/internal/execution/retention.go` (Task 11d'nin bugün vacuous olan "artifact byte'larını sil" adımı gerçek delete olur).

- [ ] Failing testler (gerçek SeaweedFS'e karşı component test): `TestArtifactPutRecordsRowAndBytes` (write → `artifacts` satırı + S3 object + checksum eşleşir); `TestArtifactReadIsTenantScoped`; `TestStoreFalsePurgeDeletesArtifactBytes` (Task 11d reaper'ı artifact byte'larını S3'ten gerçekten siler — bugünkü no-op'un kapanışı).
- [ ] Fail'i doğrula; minimum put/get/delete boundary'yi uygula; engine/runner S3 credential GÖRMEZ (§24 topolojisi — yalnızca control-plane).
- [ ] `make test-component TEST=artifacts` PASS; doctor `object_store` check'i değişmez (ping zaten var).

Bu carve-out kapanana kadar `artifacts` tablosu yazarsızdır ve bu bilinen bir durumdur; SeaweedFS'in compose'da hazır olması E09'un tek satır config ile tüketmeye başlamasını sağlar.

### 7.3 Carve-out: DB-backed model routing — `model_routes` + `model_connections` tüketimi (ev: E06, `phase-06-reference-execution.md`)

Task 15 fork kararı 4'ün borcu. LP-0'da model seçimi instance-wide tek env-configured route'tur: Orchestrator'ın `ModelRoute{Provider, Model, Secret}` alanı default olarak deterministic sabitleri (`"fake"`/`"fake"`/`"model"`) taşır ve `main.go` onu `PALAI_MODEL_PROVIDER` + Task 12 file-secret env köprüsünden doldurur — bütün projeler aynı provider/model/secret-ref'e gider. `model_routes`, `model_route_revisions` ve `model_connections` tabloları şemada mevcut (`storage/migrations/000001_core.up.sql`) ama tek Go okuyucusu yoktur; yalnızca migration testi adlarını sayar. §27.2 (Connection = yalnızca secret reference + non-secret endpoint settings taşıyan project/org-scoped binding), §27.6 ("Runs pin the route revision"; her model step seçilen target'ı kaydeder) ve §27.7 deterministic selection LP-0'da UYGULANMADI; master plan E06'nın "Canonical model request/result, route revision, capability probe..." maddesi bu carve-out'un sahibidir. E06 child planı şu task'ı içermek ZORUNDADIR:

**Files:** Create `apps/control-plane/internal/execution/route_resolver.go`, `storage/queries/model_routes.sql`; Modify `apps/control-plane/internal/execution/model_dispatch.go` (tek `ModelRoute` alanı → run'ın project'i için resolve edilen route/connection), `apps/control-plane/cmd/palai-control-plane/main.go` (env-selected route sökülür; bootstrap default route + connection satırlarını seed eder), `cmd/cli/internal/provider/add.go` (`palai provider add` artık `model_connections` + default `model_routes`/`model_route_revisions` satırlarını yazar).

- [ ] Failing testler: `TestRunRoutesViaProjectModelRoute` (bir project için `model_routes` + `model_route_revisions` + `model_connections` seed edilir; orchestrator o project'in run'ını env default'una DEĞİL, revision config'indeki provider/model'e ve connection'ın `secret_ref`'ine yönlendirir); `TestModelStepPinsRouteRevision` (run, route alias'ını değil revision'ı pin'ler ve model step seçilen target'ı kaydeder — §27.6); `TestUnroutedProjectFailsAdmission` (route'u olmayan project sessizce env default'a DÜŞMEZ, admission'da reddedilir — §27.7'nin "cannot silently select" kuralı; local UX bu yüzden bootstrap'ın seed ettiği default route ile korunur).
- [ ] Fail'i doğrula; sonra route resolver + bootstrap seed'i uygula; Task 15'in env-configured `ModelRoute` alanı ve `PALAI_MODEL_PROVIDER` seçim dalı SÖKÜLÜR (env köprüsü yalnızca secret DEĞERİNİ taşımaya devam eder; o köprünün sökümü §7.1'in işidir, bu carve-out yalnızca secret-REF seçimini DB'ye taşır).
- [ ] `make test-component TEST=models && make test-e2e TEST=responses` PASS; LP deterministic suite regress etmez.

Bu carve-out kapanana kadar model seçimi instance-wide tek env route'tur ve `model_routes`/`model_connections` tabloları okuyucusuzdur; bu bilinen bir durumdur — şemanın LP-0'dan beri hazır olması E06'nın migration yazmadan tüketmeye başlamasını sağlar.

> **KAPANDI — E13 Task 8 (`phase-13-managed-cloud-infra.md`).** Tablolar artık okunuyor (`packages/coordinator/model_routes.go` + `storage/queries/model_routes.sql`, migration YOK) ve `dispatchModel` per-project route çözüyor: model id revision'ın config'inden, credential connection'ın secret-ref'inden (T3 secret store üzerinden, org-qualified handle ile). İki sapma bilinçlidir ve E13 planında yazılıdır: (1) env route SÖKÜLMEDİ, deployment-default FALLBACK katmanına indi — bu yüzden `TestUnroutedProjectFailsAdmission` yerine route'suz proje env default'unda koşar; (2) route revision run satırına PIN'lenmez (000001'de kolon yok) — attempt route'u bir kez çözer ve her model step seçtiği revision'ı `model_requests.result`'a yazar (§27.6'nın "her step seçilen target'ı kaydeder" yarısı).

### 7.4 Runtime follow-up'ları (whole-branch review, 2026-07-19)

Kayıt yeri bu commit'li plandır — `.superpowers/` ledger gitignored ve makine-yereldir, oraya yazılan kaybolur. LP-0 merge'ünden (`ddf2501`) sonra tespit edildi; hiçbiri LP-0 kanıtını geçersiz kılmaz, her biri sahibinin epic child planına taşınmak ZORUNDADIR:

1. **Engine dial + lease handshake üst sınırsız (ev: E03 controller yarısı; E05 runner yarısı).** `apps/control-plane/internal/execution/orchestrator.go:90` `dialer.Dial(ctx, attempt)` per-attempt deadline taşımaz (yalnızca worker ctx'ine bağlı); runner tarafında `packages/runner/session.go:70` `OpenLease` ve `packages/runner/session.go:188` `Complete` handshake'leri de sınırsız bloklayabilir. Upgrade: attempt-scoped `context.WithTimeout` — dial+handshake toplamı coordinator lease penceresinden (30s, `main.go` WorkerConfig) kısa tutulur; aşım sessiz askı değil, sınıflandırılmış attempt failure olur.

2. **Worker/reconciler/retention supervision yok (ev: E03).** `apps/control-plane/cmd/palai-control-plane/main.go:99` `go func() { _ = w.Run(ctx) }()` — `packages/coordinator/worker.go:87` `Run` claim/poll hatasında return eder, launcher hatayı discard eder ve goroutine sessizce ölür (dispatch kapasitesi kalıcı düşer); `main.go:102` reconciler ve `main.go:144` retention reaper aynı discard kalıbındadır. Upgrade: üçü için tek backoff'lu supervised-restart helper + ölüm/restart sayacının doctor'a çıkması.

3. **Runner cert renewal >5m serving (ev: E05; production kapısı E14).** `cmd/runner/main.go:62-63`'teki ponytail notunun plan kaydı: `runnerCertTTL` (5m) tek UAT tier'ının penceresidir; daha uzun yaşayan runner cert expiry ile ölür. Upgrade: TTL'in ~%80'inde sessiz re-enroll/renew ve lease-safe rollover; E14 production self-host iddiası bu kapanmadan verilmez.

4. **Engine container reaping compose teardown'da yok (ev: E07 CLI teardown; label sözleşmesi E05'te hazır).** Engine container'ları compose service değildir; runner onları Docker API ile açar ve `io.palai.sandbox=engine` label'ını zaten basar (`packages/runner/supervisor.go:20` + `:170`). `palai local reset`/`down` (`cmd/cli/internal/stack/lifecycle.go`) yalnızca compose'u söker; mid-run kesilen bir stack yetim engine container'ı bırakabilir. Upgrade: teardown'a label-filtreli sweep (`docker ps -aq --filter label=io.palai.sandbox=engine` → force remove); sweep'in başka bir eşzamanlı stack'in engine'ini vurmaması için label'a compose project adı da eklenir (driver `Labels` map'ine tek satır).
