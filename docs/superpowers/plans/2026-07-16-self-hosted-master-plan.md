# Palai Self-Hosted Platform Implementation Master Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `MASTER-SPEC.md` içindeki Core API, Execution Host ve Self-host conformance profillerini; önce temiz bir makinede gerçek sağlayıcıyla kanıtlanan local stack, sonra güvenilir tek-node/split-VM/Kubernetes self-host dağıtımı olarak üretmek.

**Architecture:** Palai; PostgreSQL ve S3-compatible object storage kullanan modüler bir Go control plane, outbound bağlantı kuran Go runner, OCI içinde çalışan Python reference engine ve sözleşmeden üretilen SDK'ların oluşturduğu tek-kernel bir sistemdir. Public API, event journal ve state machine semantiği local ile self-host arasında aynıdır; deployment yalnızca base URL, kimlik, kapasite ve ilan edilen kabiliyetlerde farklılaşır.

**Tech Stack:** Go control plane/coordinator/runner/CLI; Python reference engine; TypeScript-first SDK ve Next.js proof app; daha sonra eşit Python/Go SDK'ları; PostgreSQL; S3-compatible object storage; Docker/OCI development driver; JSON Schema 2020-12, OpenAPI 3.2 + generator-compatible projection, AsyncAPI 3.1; SSE; mTLS WebSocket runner transport; OpenTelemetry; Docker Compose ve Helm.

---

## 1. Doküman statüsü ve kullanım şekli

- Kaynak ürün sözleşmesi: `MASTER-SPEC.md`, revision `1.0-review`, 2026-07-16.
- Bu doküman SaaS ürün planı değildir. Managed cell mimarisi, ticari abonelik, global tenant routing, SaaS web ürünü ve managed support operasyonu kapsam dışıdır.
- Bu doküman program sırasını, bağımlılıkları, release kapılarını, dosya sınırlarını ve UAT sahipliğini kilitler.
- Her büyük epic uygulanmadan önce burada belirtilen exact child-plan dosyası yazılır ve review edilir. Birden fazla bağımsız sistemi tek dev implementation planına sıkıştırmak yasaktır.
- İlk child plan hazırdır: `docs/superpowers/plans/2026-07-16-local-live-proof.md`.
- Public sözleşmeyi değiştiren uygulama kolaylığı bu plan içinde kabul edilemez; `docs/adr/` kaydı yetmez, `MASTER-SPEC.md` için RFC/spec revision gerekir.
- `0.x` release'ler maturity label ile dar kapsam ilan edebilir. “v1 stable” ancak bu plandaki stable kapı geçildiğinde kullanılabilir.

## 2. Kapsam kararı

### 2.1 Bu planın içinde

- Profile-free Responses; durable Sessions/Runs; ordered/reconnectable SSE.
- Tek reference engine ve brokered model/tool loop'u.
- PostgreSQL-backed coordinator, leases, fencing, outbox/inbox, idempotency.
- Local OCI runner, workspace, snapshot, checkpoint ve recovery ladder.
- En az iki direct model provider family ve bir private/OpenAI-compatible endpoint.
- Built-in file/shell tools, approvals, remote tools, MCP ve skills.
- Repository clone/edit/test/changeset; ayrı yetkiyle branch push ve draft PR.
- Agent revisions, triggers, schedules, inbound/outbound webhooks.
- Basic organization/project isolation, API keys, RBAC, secret references, audit, usage, budgets ve quotas.
- TypeScript, Python ve Go SDK'ları ile CLI.
- Docker Compose local/single-node; split-VM runner; Helm/Kubernetes; backup, restore, upgrade ve air-gap artifacts.
- Next.js örnek consumer: API key sadece server-side kalacak, stream browser'a güvenli şekilde aktarılacak.
- Self-host için gerekli operational diagnostics, metrics, traces, logs ve support bundle.
- Spec'in self-host stable iddiası için gerekli Slack, A2A, knowledge/evals ve basic open-core console işleri yalnızca çekirdek kanıtlandıktan sonra.

### 2.2 Bu planın dışında

- `MASTER-SPEC.md` §46 managed SaaS regional cell implementation.
- SaaS signup, plan, subscription, entitlement, invoice, Stripe settlement ve commercial pricing.
- Next.js ile yazılacak ticari SaaS dashboard/marketing/admin platformu.
- Managed abuse-review ve JIT support organization operasyonları.
- Global home-region directory, managed failover SLA ve customer-facing status/incident product.
- Arbitrary hostile tenants için işletilen shared microVM fleet. Driver contract bu planda kalır; Palai tarafından işletilen fleet sonraki SaaS planındadır.
- Premium enterprise SAML/SCIM/compliance paketleri.

### 2.3 Cloud'da kullanılabilir self-host yorumumuz

İlk production iddiası, bir müşterinin veya güvenilen ekibin kendi domain'i altında dedicated Palai installation çalıştırmasıdır. Aynı kurulum birden fazla organization/project ve kullanıcıyı destekler; ancak plain-container single-node kurulum hostile public multi-tenancy iddiasında bulunmaz. Bu sınır `/v1/capabilities`, `palai doctor` ve dokümantasyonda açıkça görünür.

## 3. Başarı katmanları

| Katman | Kullanıcıya verilen söz | Zorunlu canlı kanıt |
|---|---|---|
| LP-0 Local Live Proof | Temiz checkout'tan local stack açılır ve Next.js projesi gerçek model/tool akışını SDK ile kullanır. | Gerçek provider; streaming; strict structured output; pure tool; retained response; `store:false`; restart; event/usage/audit/secret-scan evidence. |
| SH-0 Self-host Alpha | Aynı artifact tek Linux VM'de TLS ve kalıcı data ile ayağa kalkar. | Cloud VM, external TLS, API key, host runner, backup, restore-to-fresh-target, SDK base-URL swap. |
| SH-1 Self-host Beta | Gerçek repository coding session, approval, push/PR ve process/container/host recovery çalışır. | Interactive coding journey; kill points; exact/checkpoint/transcript evidence; no duplicate side effect. |
| SH-2 Self-host RC | Upgrade/rollback, basic multi-user governance, automation ve operational runbooks tamamdır. | N→N+1, runner drain, backup/restore, schedules/webhooks, RLS/secret/usage conformance. |
| SH-3 Self-host Stable | Applicable P0/P1 UAT'lar, üç SDK ve release supply-chain kapıları geçer. | Signed artifacts, two direct providers, private endpoint, all release evidence, zero open P0/P1. |

LP-0 geçmeden “çalışıyor”, SH-1 geçmeden “production-ready”, SH-3 geçmeden “stable v1” ifadesi kullanılmaz.

## 4. Uygulama yaklaşımı kararı

### 4.1 Değerlendirilen seçenekler

| Seçenek | Artı | Eksi | Karar |
|---|---|---|---|
| A. Go control plane/runner + Python engine + TS SDK | Tek binary daemon/CLI, güçlü concurrency, resmi Docker Go SDK, provider/agent ekosistemi için Python, Next.js consumer ergonomisi | Polyglot toolchain ve cross-language protocol test disiplini gerekir | **Önerilen başlangıç** |
| B. Tam TypeScript/Node core + TS engine | En hızlı ilk demo, tek package manager, Next.js ile doğrudan yakınlık | Host supervisor/runner dağıtımı daha kırılgan; engine ile control contract'ın yanlışlıkla birleşme riski; Python/Go SDK parity yine gerekir | Reddedilmedi; spike A'yı yenemezse fallback |
| C. Rust control plane/runner + Python engine + TS SDK | Güçlü runtime güvenliği ve düşük kaynak kullanımı | İlk dikey dilim ve contributor onboarding daha yavaş; ürün riskinden önce toolchain riski yaratır | Local proof için seçilmedi |

### 4.2 Neden A başlangıç noktası

- Go'nun destek politikası son iki major release'i kapsar; plan yazıldığı tarihte Go 1.26 stable'dır. Toolchain image digest ile pinlenir: <https://go.dev/doc/devel/release>.
- `pgx` PostgreSQL-specific transaction, pool, `LISTEN/NOTIFY`, tracing ve JSONB kabiliyetlerini doğrudan sağlar: <https://github.com/jackc/pgx/>.
- Docker resmi olarak Go SDK yayımlar; API version negotiation desteklenir: <https://docs.docker.com/reference/api/engine/sdk/>.
- Next.js Route Handlers Web Streams ile server-side SDK stream'ini browser'a aktarabilir: <https://nextjs.org/docs/13/app/building-your-application/routing/route-handlers>.
- Python reference engine provider SDK'ları veya agent framework'lerini public contract yapmadan adapter olarak kullanabilir.

Bu gerekçeler nihai seçim değildir. E01 spike sonuçları ADR-0001..0005 ile ölçülür; başarısız kriter varsa seçenek B değerlendirilir.

### 4.3 Schema/generator uyarısı

`oapi-codegen` stable hattı halen OpenAPI 3.0 odaklıdır; 3.1/3.2 desteği experimental parser tarafındadır. Bu nedenle canonical spec'i generator uğruna 3.0'a düşürmek yasaktır. Canonical JSON Schema/OpenAPI 3.2 korunur; generator-compatible projection mekanik olarak üretilir ve semantic diff ile doğrulanır. Kaynak: <https://github.com/oapi-codegen/oapi-codegen>.

## 5. Kilitlenen repository yapısı

```text
/
├── MASTER-SPEC.md
├── README.md
├── LICENSE
├── Makefile
├── go.mod
├── go.sum
├── pyproject.toml
├── package.json
├── pnpm-workspace.yaml
├── apps/
│   ├── control-plane/
│   │   ├── cmd/palai-control-plane/main.go
│   │   └── internal/
│   │       ├── api/
│   │       ├── identity/
│   │       ├── sessions/
│   │       ├── execution/
│   │       ├── artifacts/
│   │       ├── automation/
│   │       └── operations/
│   └── web-console/                 # çekirdekten sonra, SaaS ürünü değil
├── cmd/
│   ├── cli/
│   │   └── main.go
│   └── runner/
│       └── main.go
├── engines/
│   └── reference/
│       ├── pyproject.toml
│       ├── src/palai_engine/
│       └── tests/
├── packages/
│   ├── contracts/
│   ├── state-machines/
│   ├── policy/
│   ├── model-broker/
│   ├── tool-broker/
│   ├── coordinator/
│   └── extension-sdk/
├── adapters/
│   ├── models/
│   ├── sandboxes/
│   ├── repositories/
│   ├── integrations/
│   ├── orchestration/
│   └── observability/
├── sdks/
│   ├── typescript/
│   ├── python/
│   └── go/
├── protocols/
│   ├── schemas/                     # canonical JSON Schema 2020-12
│   ├── openapi/                     # 3.2 + generated compatibility projection
│   ├── asyncapi/
│   ├── engine/
│   ├── runner/
│   └── extension/
├── storage/
│   ├── migrations/
│   └── queries/
├── deploy/
│   ├── compose/
│   ├── helm/
│   ├── systemd/
│   ├── observability/
│   └── airgap/
├── tests/
│   ├── conformance/
│   ├── component/
│   ├── e2e/
│   ├── fault/
│   ├── security/
│   ├── evals/
│   ├── performance/
│   └── uat/
├── scripts/
│   ├── contracts/
│   ├── evidence/
│   └── release/
├── docs/
│   ├── adr/
│   ├── architecture/
│   ├── api/
│   ├── operations/
│   ├── security/
│   └── superpowers/
└── examples/
    ├── nextjs-sdk/
    ├── single-shot/
    ├── interactive-session/
    ├── scheduled-investigation/
    └── customer-runner/
```

### 5.1 Dependency direction

```text
protocols/contracts + pure state machines
                    ↑
policy/coordinator/model/tool domain services
                    ↑
control plane / runner / reference engine
                    ↑
HTTP API / SDK / CLI / examples / adapters / console
```

- `packages/contracts` provider SDK, Docker, Kubernetes, Next.js veya consumer import edemez.
- Domain service doğrudan concrete adapter'a bağlı olamaz; port interface inward package'da, implementation `adapters/` altında olur.
- Engine database'e, object-store master credential'a veya provider secret'a erişemez.
- SDK ve example private endpoint kullanamaz.
- Generated files source files ile aynı commit'te tutulur ve drift CI'da fail eder.

## 6. İş akışları ve sahiplik rolleri

| Workstream | Accountable rol | Sorumluluk |
|---|---|---|
| Contracts & Control Plane | Platform Lead | schemas, API, state machines, coordinator, PostgreSQL |
| Runtime & Security | Runtime Lead | runner, sandbox, engine protocol, workspace, recovery, secrets |
| SDK & Developer Experience | DX Lead | SDK'lar, CLI, Next.js example, docs, local bootstrap |
| Operations & Release | Infra/Security Lead | Compose/Helm, telemetry, backups, upgrades, supply chain, UAT evidence |

Bir kişi birden fazla rolü taşıyabilir; fakat security-sensitive approval ve release promotion aynı kişinin tek başına yaptığı gizli bir işlem olamaz. Takvim tahmini E01 spike ölçümleri ve gerçek ekip kapasitesi çıkmadan plana eklenmez.

## 7. Milestone sırası

| Milestone | Bağımlılık | Demo/kanıt | Çıkış iddiası |
|---|---|---|---|
| M0 Repository & decision gates | yok | bağımsız repo, toolchain ve spike ADR'leri | uygulama başlayabilir |
| M1 Contract spine | M0 | generated schema/types + state-machine property tests | contract skeleton |
| M2 Durable control core | M1 | auth, idempotent mutation, event journal, SSE reconnect, coordinator | Core API preview |
| M3 Real execution vertical | M2 | outbound runner, OCI engine, real provider, brokered tool | gerçek execution preview |
| M4 Local Live Proof | M3 | CLI local up + TS SDK + Next.js live evidence | LP-0 |
| M5 Interactive coding | M4 | sessions, steering, repository workspace, approval, changeset, PR | SH-0 alpha hazırlığı |
| M6 Recovery & replay | M5 | process/container/host kill matrix | SH-1 beta |
| M7 Automation & extensions | M6 | agents, schedule, webhook, MCP/skills | automation beta |
| M8 Governance & data safety | M6 | RLS, keys, secrets, audit, usage, deletion | production security gate |
| M9 Self-host operations | M7+M8 | cloud VM, backup/restore, N→N+1, Helm/airgap | SH-2 RC |
| M10 Stable conformance | M9 | 3 SDK, 2 providers, full applicable UAT, signed release | SH-3 stable |

M4 bilinçli olarak erken konmuştur: platformun bütün uç özelliklerini beklemeden gerçek local kullanım kanıtlanır. Ancak M4 yalnızca preview'dır; recovery ve production iddiası taşımaz.

## 8. Epic execution plan

### E00 — Independent repository ve governance foundation

**Child plan:** `docs/superpowers/plans/phase-00-repository-toolchain.md`

**Files:** `LICENSE`, `README.md`, `.gitignore`, `CODEOWNERS`, `SECURITY.md`, `CONTRIBUTING.md`, `Makefile`, `.github/workflows/ci.yml`, `docs/adr/0000-template.md`

- [x] `/Users/salih/workspace/poc-ios-render/palai` içinde bağımsız Git repository başlat.
- [x] Public `palgroup/palai` remote repository oluştur ve `origin` bağla.
- [x] Apache-2.0 license ve public contribution/security policy ekle.
- [x] Root toolchain manifest'lerini oluştur; hiçbir secret veya consumer-specific isim ekleme.
- [x] Parent repository'nin Palai'yi gitlink/submodule/normal tracked directory olarak taşımadığını test et.
- [x] Branch protection, required CI ve signed release policy kur.

**Verify:**

```bash
test "$(git rev-parse --show-toplevel)" = "$PWD"
git remote get-url origin
git ls-files | rg '(^|/)(\.env|credentials|secrets)(\.|$)' && exit 1 || true
```

**Expected:** repository root mevcut klasördür; origin `palgroup/palai`; tracked secret yoktur.

**Exit gate:** E01 dışında production package oluşturulamaz.

### E01 — Technology evidence spikes

**Child plan:** `docs/superpowers/plans/phase-01-technology-spikes.md`

**Files:** `spikes/contracts/`, `spikes/postgres-coordinator/`, `spikes/runner-supervisor/`, `spikes/nextjs-streaming/`, `docs/adr/0001-*.md` … `0005-*.md`

- [x] Go vs TypeScript control-plane spike: 1,000 concurrent idle SSE, 100 reconnect ve graceful process restart ölç.
- [x] PostgreSQL lease/fence/outbox spike: worker transaction'ı kill et, yalnızca bir authoritative completion olduğunu DB assertion ile kanıtla.
- [x] Runner spike: outbound mTLS WebSocket üzerinden lease al, Docker SDK ile digest-pinned engine başlat, JSONL stdout ve bounded stderr ayır.
- [x] Contract-generation bake-off: canonical JSON Schema → OpenAPI 3.2 → 3.1.2 projection → TS/Python/Go types; omitted/null/open-enum round-trip fixture'larını doğrula.
- [x] Next.js spike: server-only SDK credential ile Route Handler üzerinden SSE relay ve AbortSignal behavior doğrula.
- [x] Local object-store adaylarını license, signed image, multi-arch, checksum/multipart ve offline availability kriterleriyle ölç.
- [x] Sonuçları ADR-0001 language/runtime, ADR-0002 contract toolchain, ADR-0003 runner transport, ADR-0004 local object store, ADR-0005 build orchestration olarak kaydet.

**Hard criteria:** Contract semantic loss, stale fence acceptance, unbounded memory, secret leak veya unsupported platform varsa aday elenir. “Daha tanıdık” ölçüt değildir.

**Exit gate:** M1 dosya yapısı ve dependency lock'ları sadece accepted ADR'lerden sonra oluşturulur.

### E02 — Canonical contracts ve state-machine spine

**Child plan:** `docs/superpowers/plans/phase-02-contract-spine.md`

**Files:** `protocols/schemas/`, `protocols/openapi/`, `protocols/asyncapi/`, `protocols/engine/`, `protocols/runner/`, `packages/contracts/`, `packages/state-machines/`, `tests/conformance/contracts/`

- [x] Opaque IDs, common resource fields, Problem Details, content items, events ve pagination schemas yaz.
- [x] Response/Session/Run/Attempt/Command/ToolCall/Workspace state transition tablolarını executable pure functions olarak ekle.
- [x] Invalid transitions, terminal monotonicity, one-active-fence ve sequence monotonicity property tests yaz.
- [x] OpenAPI 3.2 ve AsyncAPI 3.1 üret; compatibility projection'ın canonical semantiğini değiştirmediğini diff ile doğrula.
- [x] Cross-language fixtures için omitted/null/empty, unknown enum/field, RFC 3339 ve integer-boundary corpus oluştur.
- [x] `make contracts-generate` ve `make contracts-check` komutlarını deterministic yap.

**UAT ownership:** API-009, API-011, ENG-001..003'ün schema tarafı.

**Exit gate:** generated drift zero; tüm state-machine properties green; API handler henüz business logic içermez.

### E03 — PostgreSQL system of record ve built-in coordinator

**Child plan:** `docs/superpowers/plans/phase-03-durable-coordinator.md`

**Files:** `storage/migrations/`, `storage/queries/`, `packages/coordinator/`, `apps/control-plane/internal/execution/`, `tests/component/postgres/`, `tests/fault/coordinator/`

- [ ] Organizations/projects, idempotency, sessions/runs/attempts, event journal, jobs/timers/leases, outbox/inbox ve audit/usage minimum tables oluştur.
- [ ] Her public state transition ile event append'ini aynı DB transaction içinde uygula.
- [ ] `FOR UPDATE SKIP LOCKED`, lease expiry ve monotonic fencing kullanan bounded coordinator yaz.
- [ ] Job retry owner, ready-at, attempt count, cancellation/pause ve dead-letter state ekle.
- [ ] Coordinator kill, DB deadlock, transaction abort ve duplicate delivery fault tests yaz.
- [ ] Migration interruption ve idempotent re-run testini gerçek PostgreSQL üzerinde çalıştır.

**UAT ownership:** API-004..006; ENG-013..014; BIL-001 temel dedupe; AUT-007 temel uniqueness.

**Exit gate:** accepted mutation process memory'ye bağımlı değildir; stale fence hiçbir authoritative row/event yazamaz.

### E04 — Public API foundation, identity, idempotency ve SSE

**Child plan:** `docs/superpowers/plans/phase-04-core-api-events.md`

**Files:** `apps/control-plane/internal/api/`, `apps/control-plane/internal/identity/`, `apps/control-plane/internal/sessions/`, `packages/policy/`, `tests/conformance/api/`, `tests/e2e/sse/`

- [ ] Local bootstrap organization/project ve project-scoped API key doğrulaması ekle.
- [ ] Request context: principal, org, project, API revision, request ID ve trace context üret.
- [ ] Mutation middleware: required idempotency, canonical semantic hash, replay/mismatch/in-progress/tombstone behavior.
- [ ] `/v1/responses`, `/v1/sessions`, `/v1/runs`, `/v1/capabilities` minimum resources ve RFC 9457 errors ekle.
- [ ] Session journal'dan SSE stream; heartbeat, `Last-Event-ID`, bounded buffer, expired cursor ve reconnect uygula.
- [ ] API instance kill sırasında accepted mutation ve SSE recovery e2e testi yaz.

**UAT ownership:** API-001..007, API-010..011, SES-001..002'nin transport/auth kısmı.

**Exit gate:** fake execution result ile contract testleri geçer; gerçek model henüz M3'tedir.

### E05 — Runner, sandbox driver ve engine supervisor

**Child plan:** `docs/superpowers/plans/phase-05-runner-engine-protocol.md`

**Files:** `cmd/runner/`, `protocols/runner/`, `protocols/engine/`, `adapters/sandboxes/oci/`, `tests/conformance/engine/`, `tests/fault/runner/`

- [ ] One-time enrollment token → runner keypair → short-lived certificate flow oluştur.
- [ ] Runner outbound mTLS WebSocket lease offer/accept/renew/complete/revoke protokolünü uygula.
- [ ] OCI driver interface ve local Docker implementation: digest verification, resource/network settings, create/start/kill/destroy.
- [ ] Immutable EnvironmentRevision resolution, minimum isolation/resource/network requirements ve image-digest compatibility check ekle.
- [ ] JSONL supervisor: handshake, max line, independent sequences, ACK, stdout protocol-only, bounded/redacted stderr.
- [ ] Fencing token'ı runner lease, attempt, workspace writer ve callback'lerde zorunlu yap.
- [ ] Malformed frame, duplicate changed-hash frame, engine timeout, container kill ve stale runner return testleri yaz.

**UAT ownership:** ENG-001..007, ENG-013..014; SAN-001..004, SAN-006, SAN-011..012'nin local-driver kısmı.

**Exit gate:** engine yalnızca broker handles görür; Docker socket/runner credential workload içinde yoktur.

### E06 — Model broker, tool broker ve reference kernel

**Child plan:** `docs/superpowers/plans/phase-06-reference-execution.md`

**Files:** `packages/model-broker/`, `packages/tool-broker/`, `adapters/models/`, `engines/reference/`, `tests/conformance/models/`, `tests/conformance/tools/`

- [ ] Canonical model request/result, route revision, capability probe, budget reservation ve usage settlement uygula. (Carve-out devri: LP-0 Task 15, model seçimini geçici olarak tek env-configured `ModelRoute{Provider, Model, Secret}` ile bağladı; şemada var olan `model_routes`/`model_route_revisions`/`model_connections` tablolarının ilk okuyucusu — §27.6/§27.7 per-project DB-backed route seçimi — bu epic'tedir ve env route'u söker; zorunlu TDD çerçevesi LP planı §7.3'tedir.)
- [ ] İlk direct provider adapter'ını text/stream/tool/strict-schema/cancel/usage yollarıyla ekle.
- [ ] ToolCall state machine, request hash, replay class, approval hook ve normalized result uygula.
- [ ] Minimum built-in pure tool ile sandbox file/shell tool interface'lerini ekle.
- [ ] Python reference engine'de explicit safe-boundary loop, brokered model/tool frames ve terminal output uygula.
- [ ] CI için deterministic fake provider; canlı kanıt için gerçek provider suite oluştur. Fake test asla live gate yerine geçemez.

**UAT ownership:** MOD-004..009, MOD-011; TOL-001..007, TOL-013..015; API-008.

**Exit gate:** real provider secret engine frame/log/artifact/snapshot'ta yok; bir model-tool-model loop'u canonical events ile terminal olur.

### E07 — Local distribution, TypeScript SDK ve Next.js live proof

**Child plan:** `docs/superpowers/plans/2026-07-16-local-live-proof.md`

**Files:** `cmd/cli/`, `deploy/compose/`, `sdks/typescript/`, `examples/nextjs-sdk/`, `tests/uat/local-live/`, `scripts/evidence/`

- [ ] `palai init`, `palai local up|down|status|doctor|logs`, `palai provider add`, `palai response create` komutlarını ekle.
- [ ] Compose stack: PostgreSQL, S3-compatible store, control plane/coordinator, local runner, reference engine image.
- [ ] TS SDK: server-only credential, create/stream/retrieve/cancel, AsyncIterable reconnect/dedupe, typed errors.
- [ ] Next.js App Router example: server-side Palai client, browser stream relay, visible canonical event/final response.
- [ ] Clean-machine, real-provider, tool-call, structured-output, restart, retained/store:false ve secret-scan UAT çalıştır.
- [ ] Redacted evidence manifest üret ve commit'e/release digest'lerine bağla.

**UAT ownership:** OPS-001; API-001..008, API-013; local journey 63.1'in TypeScript/CLI subset'i.

**Exit gate:** LP-0 evidence bundle verifier green. Bu noktaya kadar cloud deploy veya production claim yoktur.

### E08 — Durable sessions, commands, config revision ve subagents

**Child plan:** `docs/superpowers/plans/phase-08-interactive-sessions.md`

**Files:** `apps/control-plane/internal/sessions/`, `packages/state-machines/`, `engines/reference/src/palai_engine/commands.py`, `tests/e2e/sessions/`, `tests/fault/subagents/`

- [ ] Queue/steer/interrupt delivery semantics ve `applied_sequence` uygula.
- [ ] Normal/immediate model/tool config change ve immutable ConfigSnapshot provenance oluştur.
- [ ] Pause/resume/cancel/fork/close commands ile one-active-root invariant uygula.
- [ ] ChildRun, parent budget intersection, required/optional delegation, cancel propagation ve read-only workspace mode ekle.
- [ ] İki client attach, unauthorized attach ve disconnect/reconnect tests yaz.

**UAT ownership:** SES-001..012; AGT-003; SUB-001..005.

**Exit gate:** concurrent clients aynı journal/final state'i görür; config change yalnızca safe boundary'de uygulanır.

### E09 — Workspace, repository ve coding journey

**Child plan:** `docs/superpowers/plans/phase-09-repository-coding.md`

**Files:** `adapters/repositories/`, `apps/control-plane/internal/artifacts/`, `adapters/sandboxes/oci/workspace/`, `tests/e2e/coding/`, `tests/security/repository/`

- [ ] Logical Workspace/Binding/Allocation ve single writer lease uygula.
- [ ] Deterministic clone at exact commit; hooks/unsafe config/submodule/LFS policy ve scoped GitHub App credential broker ekle.
- [ ] File/shell tools, changeset, patch/test artifacts ve secret scan ekle. (Carve-out devri: LP-0, SeaweedFS'i ADR-0004 digest'i ile compose'da başlatır ama tüketmez; ilk object-store consumer — artifact write-path + Task 11d purge'ünün gerçek byte silmesi — bu epic'tedir, zorunlu TDD çerçevesi LP planı §7.2'dedir.)
- [ ] **Built-in durable tool registry** (`task`/`agent`/`todo` + custom) model-facing tool surface üstünde: DB-backed, session-scoped, **durable** task state — multi-client görünür + context-recovery (AI, "ne bitti/ne bitmedi"yi DB'den okur; context dolsa/reset olsa bile uzun kodlamaya kaldığı yerden devam eder), task'a ek metadata girilebilir. `agent` tool E08 T5 ChildRun (`child.request` frame) altyapısını model-facing yapar; `task`/`todo` yeni durable primitive'ler (session/run/command gibi). Bu, orchestrator'ın ledger + lossless-recovery pattern'inin ürün karşılığı ve Palai'nin "API'den durable ajan çalıştıran platform" diferansiyatörü — Claude Agent SDK'nin ephemeral Todo'sunun durable/observable/resumable hâli. (Vizyon kaydı 2026-07-19; formal task tasarımı + UAT case'leri child planda.)
- [ ] **Model-driven delegation** (E08 T5 config-driven devri): model bir `agent`/`spawn` tool_call yapar → engine `child.request` emit eder (T5'te delegation raw-body `delegations` alanıyla config-driven idi; E09 tool surface bunu MODEL-driven yapar — Claude'un Agent tool'unun karşılığı, aynı ChildRun'a düşer). Parent↔child bidirectional konuşmanın model-facing yarısı da burada (`send_to_agent`/`agent_message` tool + child-idle event → parent model); durable child-detach yarısı E10'da. (Vizyon 2026-07-20.)
- [ ] Child branch/worktree ve explicit conflict-aware merge uygula.
- [ ] Push branch ve draft PR'ı ayrı exact approvals, idempotency ve reconciliation ile ekle.
- [ ] Unsafe local bind'i explicit local-only flag ve prominent warning ile uygula.

**UAT ownership:** REP-001..012; SAN-001..006, SAN-010; SUB-006.

**Exit gate:** journey 63.2 kill olmadan geçer; credential engine/events/process args/snapshot'ta yoktur.

### E10 — Checkpoint, snapshot, recovery ve replay

**Child plan:** `docs/superpowers/plans/phase-10-recovery-replay.md`

**Files:** `packages/coordinator/recovery/`, `adapters/sandboxes/oci/snapshot/`, `engines/reference/src/palai_engine/checkpoint.py`, `tests/fault/recovery/`, `tests/uat/recovery/`

- [ ] Checkpoint, workspace snapshot ve transcript boundary'yi ayrı immutable objects olarak uygula.
- [ ] Exact → compatible checkpoint → transcript reconstruction → explicit failure recovery ladder yaz.
- [ ] Pure/idempotent/reversible/irreversible/interactive replay decisions ve uncertain reconciliation jobs ekle.
- [ ] Process, engine container, runner daemon ve whole-host kill harness oluştur.
- [ ] Outage sırasında queue/steer/interrupt ordering ve old-host stale fence denial testleri yaz.
- [ ] **Parent-detached durable child + parent-child agent conversation** (E08 T5 honest-ceiling devri): inline ChildRun parent engine'i idle tutuyor; release-parent + durable child job + `run.restore` ile child parent-detached long-lived session olur. Bu, Claude'un ephemeral parent-child mailbox'ının DURABLE karşılığını mümkün kılar — child idle (`waiting`), parent command-spine `send_message`→child (T2), child→parent journal event, parent stop=cancel-propagation (T5); hepsi durable/observable/resumable (context dolsa bile konuşma DB'de duruyor). Primitive'ler T2/T4/T5'te kanıtlı, birleşim + detach burada. (Vizyon 2026-07-20.)
- [ ] RecoveryProof resource/evidence üret; “continued” log'u tek başına kabul etme.

**UAT ownership:** ENG-004..014; TOL-001..004, TOL-016..017; SAN-005..008; SES-009..010.

**Exit gate:** journey 63.2 kill/recovery dahil geçer ve duplicate external effect sıfırdır. SH-1 ancak bundan sonra verilir.

### E11 — Agents, schedules, triggers ve webhooks

**Child plan:** `docs/superpowers/plans/phase-11-automation-agents.md`

**Files:** `apps/control-plane/internal/automation/`, `adapters/integrations/webhook/`, `tests/e2e/automation/`, `tests/fault/scheduler/`

- [ ] AgentProfile/immutable AgentRevision publication ve RunTemplateRevision uygula.
- [ ] Trigger revisions, input mapping, source dedupe, correlation ve concurrency policies ekle.
- [ ] PostgreSQL timer-backed five-field cron, timezone, DST, deterministic occurrence ve bounded misfire uygula.
- [ ] Outbound webhook raw-body HMAC, retries, DNS/redirect safety, redelivery ve dead-letter ekle.
- [ ] Inbound signed webhook'u durably ack edip asynchronous run başlat.

**UAT ownership:** AGT-001..003; AUT-001..013.

**Exit gate:** duplicated inbound event ve scheduler replica tek canonical action üretir; callback failure run sonucunu silmez.

### E12 — MCP, skills, hooks ve remote tools

**Child plan:** `docs/superpowers/plans/phase-12-extensibility.md`

**Files:** `packages/extension-sdk/`, `adapters/integrations/mcp/`, `adapters/tools/http/`, `tests/security/extensions/`, `tests/conformance/tool-sdk/`

- [ ] MCP stdio/Streamable HTTP discovery/call/progress/cancel ve namespacing uygula.
- [ ] OAuth audience, PKCE, origin ve token-passthrough defenses ekle.
- [ ] Skill quarantine, archive/path/decompression scan, digest pinning ve no-authority invariant uygula.
- [ ] Hook category timeout/fail mode ve isolated execution ekle.
- [ ] Remote HTTP synchronous/async tool protocol, signed callbacks, late callback reconciliation ekle.
- [ ] Tool SDK TypeScript/Python/Go schema/signature parity ekle.

**UAT ownership:** TOL-008..012, TOL-016..018.

**Exit gate:** malicious skill/MCP metadata capability genişletemez; extension crash core process'i düşürmez.

### E13 — Managed-Cloud Infrastructure Completeness (reshaped 2026-07-22: tenancy/secrets/usage + the managed-agent-over-cloud infra gaps)

**Child plan:** `docs/superpowers/plans/phase-13-managed-cloud-infra.md` — the reshape gathers all TRULY-NEEDED non-SaaS infra into one gate (RLS + tenant provisioning API, restart-less secret-refs, read/LIST API, artifact retrieval, edge admission/rate-limit, DB-backed model-routing reader, config_policy write, `@palai/sdk` core-parity). SaaS-layer items are explicitly OUT (child plan §5: browser-token/CORS, per-tenant GitHub onboarding, pooled-fairness/Stripe, managed-cell/microVM/DR-3, web-console, full-KMS/OIDC). The original envelope/KMS + OIDC + audit-integrity items below run as the gate-non-blocking **E13-H hardening tranche** (child plan §6). Design invariant: every exposed API + the usage-ledger is versioned + tenant-scoped + stable, so a commercial SaaS layers on top without coupling into the core.

**Files:** `apps/control-plane/internal/identity/`, `packages/policy/`, `apps/control-plane/internal/operations/`, `storage/migrations/`, `tests/security/tenancy/`, `tests/security/secrets/`

- [ ] PostgreSQL RLS, verified tenant context ve cross-tenant negative corpus ekle.
- [ ] API key hash/scope/expiry/revoke; roles/relationships; optional OIDC ekle.
- [ ] Envelope-encrypted SecretRef backend ve one-operation audience/fence-bound leases uygula. (Carve-out devri: LP-0 Task 12, provider secret'ı geçici olarak `.palai` file-secret → env → `EnvResolver` yoluyla taşıdı; bu epic o köprüyü söker ve write-only `POST /v1/secret-refs` admission + §41.2 envelope encryption'ı getirir — zorunlu TDD çerçevesi LP planı §7.1'dedir.)
- [ ] Append-only usage ledger, reservations/settlement, budgets ve quotas ekle; commercial invoice üretme.
- [ ] Audit integrity linkage, retention, `store:false`, deletion/export ve signed artifact URL policy ekle.
- [ ] Default content-free OpenTelemetry signals ve redaction/secret scanners ekle.

**UAT ownership (gate):** TEN-001..003; SEC-002 (rotation subset); DAT-006 (basic deny); BIL-001, BIL-003; QUO-001; MOD-004 (routing yarısı); MCI-001..008. **(E13-H, gate-bloklamaz — SH-2 öncesi/E14-E15 penceresi):** TEN-004, SEC-001/003, DAT-001..005, BIL-002/004/005, OIDC/roles, audit-integrity linkage. TEN-005 ve managed billing export (Stripe) SaaS scope'unda ayrıca ele alınır.

**Exit gate — "managed-agent-over-cloud infra-complete":** Bir web SDK (server-relay) — API ile provision edilmiş, restart'sız secret'lı, kendi model route'u ve config_policy'si olan bir tenant'ta — run yaratır, steer/interrupt eder, run/session/agent listeler ve artifact indirir; tamamı RLS ile tenant-izole (cross-project negative corpus DB-level yeşil), edge'de rate-limited, budget/quota-metered ve versioned `/v1` public API'den geçer; `managed-cloud-0.1.0` evidence verifier yeşildir. Bu gate geçmeden "managed cloud hazır" ifadesi kullanılmaz; hostile public multi-tenancy bu gate'te de İDDİA EDİLMEZ (E17+/SaaS).

### E14 — Single-node ve split-VM production self-host

**Child plan:** `docs/superpowers/plans/phase-14-production-self-host.md`

**Files:** `deploy/compose/production.yml`, `deploy/systemd/`, `deploy/observability/`, `docs/operations/`, `cmd/cli/`, `tests/uat/self-host/`

- [ ] External TLS/reverse proxy, non-development master key, public registration off ve persistent services kur.
- [ ] Runner'ı signed host package/systemd unit olarak outbound-only kur; workload'a runtime socket verme.
- [ ] `palai backup`, `restore`, `restore verify`, `config validate`, `doctor`, `support-bundle` komutlarını ekle.
- [ ] `palai org|project|apikey|secret` admin subcommand'larını E13 API'leri üzerine ince yüzey olarak ekle (§47.6 API+CLI şartı; E17 console'a kadar tek insan arayüzü).
- [ ] `deploy/systemd/` içine scheduled backup timer ve retention/prune örneği ekle.
- [ ] §52.9 dashboard'larını ve §52.10 alert rule'larını hazır Grafana/Prometheus bundle olarak `deploy/observability/` altında yayımla.
- [ ] Disk/queue/runner/provider/object-store/clock/callback diagnostics ve alerts ekle.
- [ ] Dedicated cloud VM'ye clean install; SDK'da yalnızca base URL/key değiştirerek Next.js example çalıştır.
- [ ] Backup'ı ayrı clean installation'a restore edip checksums/tenant IDs/run retrieval doğrula.

**UAT ownership:** OPS-002; DR-002, DR-004..006; self-host journey 63.6'nın install/backup subset'i.

**Exit gate:** SH-0 tek-node alpha; SH-2 için upgrade ve Kubernetes işleri halen gerekir.

### E15 — Upgrade, Helm, air-gap ve DR hardening

**Child plan:** `docs/superpowers/plans/phase-15-upgrade-kubernetes-airgap.md`

**Files:** `deploy/helm/`, `deploy/airgap/`, `scripts/release/`, `docs/operations/upgrade.md`, `tests/uat/upgrade/`, `tests/uat/kubernetes/`

- [ ] Expand/migrate/contract migration discipline, interrupted migration resume ve rollback window uygula.
- [ ] N→N+1 control plane, runner drain, pinned active engine ve new-run alias rollback testi yaz.
- [ ] Restricted Helm install; external PostgreSQL/S3; NetworkPolicy; PDB; migration job; no ongoing cluster-admin doğrula.
- [ ] Signed offline bundle manifest, private registry/model/Git ve telemetry-free air-gap install uygula.
- [ ] Database primary loss/object corruption/KMS key recovery drills ve measured RPO/RTO raporu üret.

**UAT ownership:** OPS-003..008; DR-001..002, DR-004..006. Managed cross-region DR-003 SaaS planındadır.

**Exit gate:** SH-2 RC; rollback/restore kanıtı olmayan release promote edilemez.

### E16 — SDK parity ve provider completeness

**Child plan:** `docs/superpowers/plans/phase-16-sdk-provider-parity.md`

**Files:** `sdks/typescript/`, `sdks/python/`, `sdks/go/`, `adapters/models/`, `tests/conformance/sdk/`, `tests/conformance/models/`

- [ ] TS, Python sync/async ve Go SDK public ergonomics/parity tamamla.
- [ ] Shared request/event/error/signature/unknown-field fixtures tüm dillere uygula.
- [ ] İkinci independent direct provider ve private/OpenAI-compatible adapter capability probe ekle.
- [ ] Retry/fallback/cancel/partial stream/cache/usage/circuit/budget conformance tamamla.
- [ ] Package provenance, checksums, changelog ve compatibility matrix yayımla.

**UAT ownership:** API-012..015; MOD-001..012; local journey 63.1'in üç SDK tamamı.

**Exit gate:** aynı fixture üç dilde semantic eşit; gateway kapatıldığında direct paths çalışır.

### E17 — Stable extensions, quality ve integration journeys

**Child plans:** `phase-17a-slack-a2a.md`, `phase-17b-knowledge-evals.md`, `phase-17c-basic-console.md`, `phase-17d-queues-workers-orchestration.md`

**Files:** `adapters/integrations/slack/`, `adapters/integrations/a2a/`, `apps/control-plane/internal/knowledge/`, `tests/evals/`, `apps/web-console/`

- [ ] Slack Socket Mode/Events API aynı canonical mapping ile; dedupe, rate-limit repair ve exact approvals.
- [ ] A2A 1.0 server/client projection; card/version/auth/SSRF controls.
- [ ] PostgreSQL FTS + optional vector adapter ile immutable ingestion/index/retrieval ve ACL-first filtering.
- [ ] Coding/research/recovery/security eval suites ve held-out release thresholds.
- [ ] Yalnızca public API kullanan basic open-core console; §47.1 admin yüzeyi (organizations/projects/API keys), live timeline/exact approval/recovery display/accessibility. Ticari SaaS UI burada yapılmaz.
- [ ] SQS/PubSub/Kafka-class queue adapter contract'ı; durable ack/dedupe/backpressure/dead-letter ve outbound result delivery.
- [ ] External orchestrator helper/adapters; canonical API IDs, single retry owner, cancel propagation ve reconciliation.
- [ ] Outbound-enrolled CapabilityWorker contract'ı; typed capability/version/capacity, fenced jobs, artifact input/output ve short-lived secret handles.
- [ ] macOS/iOS build ile private-network typed operation'ı fixture worker üzerinde kanıtla; ordinary sandbox'a general tunnel veya signing credential verme.

**UAT ownership:** SLK-001..008; A2A-001..005; KNO-001..008; QUA-001..004; UI-001..002; SUB-007; AUT-009..010, AUT-013; §31 worker conformance ve integration benchmark.

**Exit gate:** ilgili capability stable olarak ilan edilecekse tüm UAT green; aksi halde capability preview/disabled olarak discovery'de görünür.

### E18 — Release supply chain ve stable sign-off

**Child plan:** `docs/superpowers/plans/phase-18-stable-release.md`

**Files:** `.github/workflows/release.yml`, `scripts/release/`, `docs/security/`, `docs/operations/runbooks/`, `tests/performance/`, `evidence/releases/`

- [ ] Pinned hermetic builds, SBOM, provenance, digest/signature ve offline verification ekle.
- [ ] Control plane image, runner host package, reference engine image ve CLI binary'leri için amd64+arm64 release matrisini yayımla ve doğrula.
- [ ] API/SSE/load/cold-warm/long-session/burst performance tests çalıştır; published hardware/load profile kaydet.
- [ ] Security threat model, vulnerability process, operational runbooks ve release support matrix yayımla.
- [ ] Applicable P0/P1 UAT evidence manifest'lerini tek release index'inde doğrula.
- [ ] RC soak; zero open P0/P1; exception'lar sadece P2 ve owner/expiry ile.
- [ ] Signed tag, immutable images/packages/checksums ve upgrade guide yayımla.

**UAT ownership:** SEC-101..103; PER-001..004; OPS/DR regression; §64.15 stable-release gate.

**Exit gate:** SH-3 Stable.

## 9. UAT coverage ve applicability matrisi

| UAT ailesi | Epic | LP-0 | SH Beta | SH Stable | Not |
|---|---|---:|---:|---:|---|
| API-001..015 | E02, E04, E07, E16 | subset | tümü | tümü | üç SDK E16'da |
| SES-001..012 | E08, E10 | — | tümü | tümü | recovery E10 |
| AGT-001..003 | E08, E11 | — | subset | tümü | |
| SUB-001..007 | E08, E09, E17 | — | 001..006 | tümü | A2A child E17 |
| MOD-001..012 | E06, E16 | provider-1 subset | core | tümü | two providers E16 |
| KNO-001..008 | E17 | — | — | capability stable ise tümü | preview kapalı olabilir |
| TOL-001..018 | E06, E10, E12 | pure subset | core/replay | tümü | |
| ENG-001..014 | E05, E10 | basic | tümü | tümü | |
| SAN-001..012 | E05, E09, E10, E15 | development subset | local/trusted tier | applicable tümü | SAN-009 managed microVM fleet SaaS planına bağlı |
| REP-001..012 | E09, E10 | — | tümü | tümü | |
| AUT-001..013 | E11 | — | subset | tümü | queue adapters maturity'ye bağlı |
| SLK-001..008 | E17 | — | — | stable Slack claim için tümü | |
| A2A-001..005 | E17 | — | — | stable A2A claim için tümü | |
| TEN-001..005 | E13 | — | 001..004 | applicable | TEN-005 managed support SaaS scope |
| SEC-001..003 | E13 | secret scan subset | tümü | tümü | |
| DAT-001..006 | E07, E13, E14 | store:false subset | tümü | tümü | |
| BIL-001..006 | E13 | usage subset | 001..005 | applicable | commercial exporter/Stripe SaaS scope |
| QUO-001..002 | E13 | — | 001 | applicable | pooled fairness managed scope |
| OPS-001..008 | E07, E14, E15 | 001 | 001,002,005..008 | tümü | |
| DR-001..006 | E14, E15 | — | 002,004..006 | applicable | DR-003 managed regional failover scope |
| QUA-001..004 | E17 | — | critical security subset | tümü | |
| SEC-101..103 | E18 | image checksum subset | tümü | tümü | |
| PER-001..004 | E18 | smoke | target topology | tümü | hardware/load profile zorunlu |
| UI-001..002 | E17 | Next example smoke | — | basic console claim için | SaaS product UI bu planın dışında |

Her UAT case `tests/uat/cases/<ID>/case.yaml` ile exact environment, setup, action, assertions ve evidence requirements taşır. Range ile sahiplik atamak case'i atlamak anlamına gelmez; release index her exact ID'yi tek tek listeler.

## 10. Live evidence sistemi

### 10.1 Evidence bundle yapısı

```text
evidence/releases/<release-id>/<uat-id>/
├── manifest.json
├── environment.json
├── requests.jsonl
├── events.jsonl
├── assertions.json
├── audit.jsonl
├── usage.jsonl
├── external-receipts.jsonl
├── traces.json
├── secret-scan.json
└── artifacts.json
```

`manifest.json` zorunlu alanları: spec revision, UAT ID, git commit, image digests, DB migration version, API/engine/runner versions, provider adapter/model route revision, started/ended time, test command, outcome, evidence checksums ve redaction policy.

Raw provider payloadları ve secret değerleri evidence'e girmez. Local raw evidence gitignored olur; repository yalnızca redacted verified manifests/fixtures ve release summary taşır.

### 10.2 Proof sınıfları

- `unit`: pure state/policy invariant.
- `component-real`: gerçek PostgreSQL/S3/OCI; external provider fake olabilir.
- `e2e-deterministic`: bütün Palai component'leri gerçek, provider recorded/fake.
- `live-provider`: gerçek external provider; fake ile değiştirilemez.
- `fault-live`: process/container/host veya network gerçekten kesilir.
- `external-receipt`: Git/Slack/webhook gibi destination exact receipt verir.

Bir UAT hangi proof sınıfını gerektiriyorsa daha düşük sınıfla pass edilemez.

### 10.3 Standard commands

```bash
make verify
make test-component
make test-e2e
make uat-local-live PROVIDER=<configured-alias>
make uat-self-host TARGET=<ssh-config-alias>
make test-fault CASE=ENG-006
make evidence-verify RELEASE=<release-id>
```

Komutlar sonunda human log değil, machine-readable summary ve non-zero failure exit code üretir.

## 11. CI/CD kapıları

| Lane | Ne zaman | İçerik | Blocker |
|---|---|---|---|
| PR-fast | her PR | format, lint, unit, schema examples, generated drift, secret/license scan | herhangi failure |
| PR-component | her PR | PostgreSQL/S3/OCI component, migration up/down-safe checks | herhangi failure |
| Main-e2e | merge sonrası | Compose deterministic end-to-end, SDK TS smoke | herhangi failure |
| Nightly-fault | gece | kill points, duplicate/reorder, reconnect storm, stale fence | P0/P1 failure |
| Nightly-live | protected secret env | real provider canary, no raw content retention | regression |
| RC | release candidate | all applicable UAT, load, security, restore/upgrade, 3 SDK | open P0/P1 |
| Release | manual two-person promotion | verify provenance/signatures/evidence index | missing artifact/evidence |

Flaky test otomatik retry ile gizlenmez. Retry sonucu ayrı attempt olarak kaydedilir; owner ve deadline olmadan quarantine yoktur.

## 12. Git ve değişiklik disiplini

- Her child-plan task küçük, review edilebilir ve tek amaçlı commit üretir.
- Schema değişikliği + generated output + compatibility diff aynı commit'tedir.
- Migration ve code deploy ordering aynı PR açıklamasında bulunur.
- Generated dosyalar elle editlenmez.
- Unrelated refactor yok; her changed line task acceptance criterion'a bağlanır.
- Public contract/security architecture change için RFC; implementation choice için ADR.
- Feature branch merge edilmeden `make verify` ve ilgili narrow tests zorunludur.

## 13. Ana riskler ve karşılıkları

| Risk | Erken sinyal | Karşılık |
|---|---|---|
| Kapsamın local proof'u geciktirmesi | M4'ten önce optional adapter işleri | Dependency gate; optional işler E17'ye |
| Schema generator semantic loss | null/omitted/open-enum fixture fail | Canonical schema korunur, projection diff; generator değiştirilir |
| Runner Docker socket'un workload'a sızması | sandbox içinde socket/path bulunması | Runner-only access; SAN-002; production host runner |
| Model testlerinin nondeterministic/flaky olması | live test geçişleri rastgele | deterministic CI + ayrı live evidence; structural assertions |
| Recovery'nin final output'a bakılarak yanlış geçmesi | DB/effect receipt yok | RecoveryProof + canonical state + external receipt |
| Secret'ın engine/log/event'e sızması | scan finding | JIT handle, exact-value/pattern scan, release blocker |
| Polyglot drift | aynı fixture farklı request üretir | generated contracts + shared corpus + API-012/TOL-018 |
| Local ile self-host semantic fork | deployment-specific private endpoint | conformance aynı binary/API; base URL swap UAT |
| PostgreSQL coordinator'ın general workflow engine'e büyümesi | user-authored DAG/code isteği | fixed product state machines; external orchestrator adapter |
| Single-node'un hostile multi-tenant diye pazarlanması | discovery isolation tier eksik | explicit development/trusted tier; no false claim |
| Object-store dependency/license sürprizi | unpinned/unavailable image | E01 ADR, digest/offline mirror, adapter conformance |

## 14. Her task için Definition of Done

Bir checkbox ancak şu koşulların tamamı sağlanınca kapanır:

1. İlgili failing test önce görülmüştür veya testin neden pre-existing pass olduğu açıklanmıştır.
2. Minimum implementation ile targeted test geçer.
3. Narrow suite ve affected conformance suite geçer.
4. Error, retry, cancellation ve observability behavior'ı test edilmiştir.
5. Secret/content capture policy doğrulanmıştır.
6. Schema/docs/examples generated drift bırakmaz.
7. İlgili UAT/evidence mapping günceldir.
8. Commit yalnızca task kapsamındaki dosyaları içerir.

## 15. İlk uygulanacak sıra

1. E00 kalan governance/toolchain dosyaları.
2. E01 beş technology spike ve ADR kararı.
3. `2026-07-16-local-live-proof.md` içindeki Task 1–4 contract/durable spine.
4. Aynı plandaki runner/engine/model/tool vertical slice.
5. CLI + TS SDK + Next.js proof.
6. LP-0 live evidence review.
7. Ancak LP-0 onayından sonra E08 interactive/coding ve E10 recovery planları.

Bu sıra, ilk gerçek SDK kullanımını kritik olmayan SaaS/console/integration işlerinden bağımsız tutar ve her sonraki production iddiasını ölçülebilir bir önceki kanıta bağlar.

## Appendix A — Exact UAT ownership index

Bu index `MASTER-SPEC.md` §64 içindeki her exact ID'nin bu programda kaybolmadığını mekanik olarak doğrulamak içindir. Applicability ve release timing §9'daki matristen okunur.

- **E02/E04/E07/E16 — API:** API-001, API-002, API-003, API-004, API-005, API-006, API-007, API-008, API-009, API-010, API-011, API-012, API-013, API-014, API-015.
- **E08/E10 — Sessions:** SES-001, SES-002, SES-003, SES-004, SES-005, SES-006, SES-007, SES-008, SES-009, SES-010, SES-011, SES-012.
- **E08/E11 — Agents:** AGT-001, AGT-002, AGT-003.
- **E08/E09/E17 — Subagents:** SUB-001, SUB-002, SUB-003, SUB-004, SUB-005, SUB-006, SUB-007.
- **E06/E16 — Models:** MOD-001, MOD-002, MOD-003, MOD-004, MOD-005, MOD-006, MOD-007, MOD-008, MOD-009, MOD-010, MOD-011, MOD-012.
- **E17 — Knowledge:** KNO-001, KNO-002, KNO-003, KNO-004, KNO-005, KNO-006, KNO-007, KNO-008.
- **E06/E10/E12/E13 — Tools:** TOL-001, TOL-002, TOL-003, TOL-004, TOL-005, TOL-006, TOL-007, TOL-008, TOL-009, TOL-010, TOL-011, TOL-012, TOL-013, TOL-014, TOL-015, TOL-016, TOL-017, TOL-018.
- **E05/E10 — Engine/recovery:** ENG-001, ENG-002, ENG-003, ENG-004, ENG-005, ENG-006, ENG-007, ENG-008, ENG-009, ENG-010, ENG-011, ENG-012, ENG-013, ENG-014.
- **E05/E09/E10/E15 — Sandbox:** SAN-001, SAN-002, SAN-003, SAN-004, SAN-005, SAN-006, SAN-007, SAN-008, SAN-009, SAN-010, SAN-011, SAN-012.
- **E09/E10 — Repository:** REP-001, REP-002, REP-003, REP-004, REP-005, REP-006, REP-007, REP-008, REP-009, REP-010, REP-011, REP-012.
- **E11 — Automation:** AUT-001, AUT-002, AUT-003, AUT-004, AUT-005, AUT-006, AUT-007, AUT-008, AUT-009, AUT-010, AUT-011, AUT-012, AUT-013.
- **E17 — Slack:** SLK-001, SLK-002, SLK-003, SLK-004, SLK-005, SLK-006, SLK-007, SLK-008.
- **E17 — A2A:** A2A-001, A2A-002, A2A-003, A2A-004, A2A-005.
- **E13 — Tenancy:** TEN-001, TEN-002, TEN-003, TEN-004, TEN-005.
- **E13 — Secrets:** SEC-001, SEC-002, SEC-003.
- **E13/E14 — Data:** DAT-001, DAT-002, DAT-003, DAT-004, DAT-005, DAT-006.
- **E13 — Usage/billing:** BIL-001, BIL-002, BIL-003, BIL-004, BIL-005, BIL-006.
- **E13 — Quotas:** QUO-001, QUO-002.
- **E07/E14/E15 — Packaging/upgrade:** OPS-001, OPS-002, OPS-003, OPS-004, OPS-005, OPS-006, OPS-007, OPS-008.
- **E14/E15 — DR:** DR-001, DR-002, DR-003, DR-004, DR-005, DR-006.
- **E17 — Quality:** QUA-001, QUA-002, QUA-003, QUA-004.
- **E18 — Supply-chain security:** SEC-101, SEC-102, SEC-103.
- **E18 — Performance:** PER-001, PER-002, PER-003, PER-004.
- **E17 — Console quality:** UI-001, UI-002.
