# Palai Stable Extensions, Quality ve Integration Journeys Plan (E17)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (önerilen) veya superpowers:executing-plans ile task-by-task uygula. Adımlar `- [ ]` checkbox'lıdır. Slack Events API/Socket Mode, A2A 1.0 HTTP binding, PostgreSQL FTS (tsvector/GIN), NATS JetStream, Next.js app-router ve playwright/axe idiomları brief'lerinde Context7/spec grounding alır (repo politikası, ledger 2026-07-17).

**Goal:** Platformun dört extension alanını — **17a Slack+A2A**, **17b Knowledge+Evals**, **17c Basic Console**, **17d Queues/Workers/Orchestration** — TEK tutarlı fazda, mevcut canonical seam'lerin (webhook inbound/signature/dedupe, MCP breaker, approval one-shot, outbox/dead-letter, runner lease/fencing, E16 SDK'ları) ÜZERİNE inşa etmek ve her birini **dürüst maturity tier'ıyla** yayımlamak. Exit gate: **bir capability YALNIZ tüm UAT'ı yeşilse STABLE ilan edilir; aksi halde discovery'de preview/disabled görünür** — tier bir iddia değil, claim outcome'larından MEKANİK yeniden hesaplanan bir fonksiyondur (T11 anchor'ı).

**Kapsam sınırı — DÜRÜST TAVAN (E14→E16 geleneğinin devamı):** Bu plan macOS + Docker Desktop oturumunda kod-subagent'larıyla İCRA EDİLİR. (a) **Gerçek Slack workspace'i ve foreign A2A peer'i YOKTUR** — Slack/A2A local kanıtı canonical mapping + fake/loopback peer iledir ve öyle ADLANDIRILIR; gerçek workspace/foreign-peer external-receipt'leri §6 operator legidir → Slack ve A2A bu fazın sonunda **preview**'dur. (b) **Apple signing/Xcode İDDİA EDİLMEZ** — 17d'nin macOS ayağı, HOST macOS'unda native koşan bir **fixture worker**'la outbound-enrolled typed-operation + private-network seam'ini kanıtlar (bu oturumun tek gerçek donanım avantajı); gerçek signed macOS/iOS build §6 legidir → `apple-build` capability'si **preview/disabled**. (c) `.env.local`'da iki gerçek provider credential'ı vardır (`set -a` ile source edilir, asla argv/log/evidence/commit) — embeddings ve single-step eval cases live'dır; ama **E08 kuralı geçerlidir: engine gerçek provider'a TOOL AÇMAZ** → gerçek modelle agentic eval benchmark'ı KOŞULMAZ; eval kanıtı harness+threshold+gate MEKANİĞİDİR, model-quality sayıları değil. (d) Compose Postgres image'ı plain'dir (digest-pinned, pgvector YOK) → vector retrieval adapter-interface + fake ile deterministic kanıtlanır, gerçek vector store §6 legidir.

---

## 1. Yapı kararı — tek dosya, fork noktası, migration atamaları

**Tek cohesive plan:** master plan §E17 dört child dosya adlandırır (`phase-17a..d`). Owner kararıyla dört alt-alan TEK planın task grupları olarak burada yaşar — seam'ler bağımsız ama exit gate (tier hesabı + evidence bundle) TEKtir; dört ayrı gate dosyası suni bölünme olurdu. Master plan amendment (owner'ın yapacağı tek edit): §E17 child-plan satırı bu dosyaya çevrilir.

**Fork noktası:** E16 T8 kapanışı (`sdk-provider-parity-0.1.0` bundle) `main`'e merge olduktan sonra; execution gate: `main` >= E16 T8 merge tip.

**Migration:** Zincir **000034**'te (E16 migration-free idi). Atamalar SABİT (paralel task'lar çakışmasın — E13 emsali) ve migration-bearing merge sırası SABİTTİR:

| Sıra | Migration | Task | İçerik |
|---|---|---|---|
| 1 | **000035** | T1 | `slack_connections`, `slack_thread_sessions`, message-ts reconciliation |
| 2 | **000036** | T4 | knowledge spine: `knowledge_bases`, `knowledge_sources`, `ingestion_jobs`, `document_revisions`, `chunk_revisions`, `index_revisions` (+FTS tsvector/GIN) |
| 3 | **000037** | T7 | `queue_connections`, `queue_deliveries` (outbound outbox) |
| 4 | **000038** | T2 | `a2a_interfaces`, `a2a_task_refs` |
| 5 | **000039** | T3 | `a2a_remote_agents` |
| 6 | **000040** | T9 | `capability_workers`, `capability_jobs` |

Kurallar aynen: guarded + idempotent + `storage/embed.go` concat; **her yeni tenant tablosu RLS policy alır** (E13 T1 tenancy corpus'u mekanik gate'ler — org/project policy + FORCE RLS); append-only tablolar (`*_revisions`, `capability_jobs` journal'ı) **self-re-asserting REVOKE**; contracts/şema dokunuşu ⇒ `make generate`. T5/T6/T8/T10/T11 migration-free'dir; öngörülemeyen ihtiyaç → 000041+, önce owner onayı.

**Files:** `adapters/integrations/slack/` (yeni), `adapters/integrations/a2a/` (yeni), `apps/control-plane/internal/knowledge/` (yeni), `tests/evals/` (yeni), `apps/web-console/` (yeni) — master plan §E17 bire bir; artı `adapters/integrations/queue/` (yeni), `apps/control-plane/internal/workers/` (yeni), `apps/control-plane/api/` (yeni endpoint'ler), `storage/migrations/`, `sdks/typescript/` (orchestrator kit + console'un tükettiği ince client'lar), `tests/uat/extensions/` (T11 journey evi).

---

## 2. Design invariant (task değil, her task'ın kabul şartı)

- **Tier bir fonksiyondur, iddia değil:** her yeni capability `/v1/capabilities` map'ine (mevcut `capabilities.go` şekli) **"preview"** olarak girer; **hiçbir task kendi capability'sini "stable" yazamaz**. Stable flip'i YALNIZ T11 yapar ve verifier tier'ı claim outcome'larından YENİDEN HESAPLAR (§4 T11 anchor'ı). Discovery, deployment'ın servis edemediğini asla ilan etmez (`workspacesCapability` emsali).
- **Canonical mapping tek yerde:** Slack/A2A/queue adapter'ları kendi run-identity/dedupe/retry mekanizmasını İCAT ETMEZ — hepsi mevcut InboundEvent normalize + durable-insert-önce-ack + dedupe-commit + poison→`failed` + outbox/dead-letter kalıbını (webhook/automation seam, §34) yeniden kullanır. Bir kaynağın offset/receipt-handle'ı canonical run identity olamaz (§34.1).
- **Untrusted content asla privileged instruction'a:** knowledge source içeriği, A2A remote çıktısı, Slack mesajı, queue payload'u — hepsi untrusted; tool/capability GRANT EDEMEZ. ACL/authorization filtreleri skorlamadan ÖNCE uygulanır — post-filter top-K yasaktır (§25.15.4: ranking/existence leak).
- **Tek retry sahibi (§35.2/§53.4):** her integration dokümanı hangi sistemin hangi timeout/retry'ı sahiplendiğini yazar; retry multiplication testle reddedilir (AUT-013).
- **Secret'lar handle'dır:** worker'a job-scoped kısa ömürlü handle (deadline'la ölür), console'a ASLA (API key yalnız server-side relay'de), evidence/log'a hiçbir zaman; mevcut secret-scan gate'i tüm yeni yüzeyleri kapsar.
- **Console yalnız public API (§47.6):** privileged backchannel YOK; her UI aksiyonu `/v1`'den geçer ve bu MEKANİK kanıtlanır (T10 network-intercept asserti).

---

## 3. Doğrulanmış seam envanteri (2026-07-24, ağaca karşı)

| Seam | Durum |
|---|---|
| `adapters/integrations/` | Yalnız `mcp/` + `webhook/`; slack/a2a/queue YOK |
| Inbound canonical kalıbı | `webhook/inbound.go` ParseInbound: verify-STRİKT-önce-decode, constant-time multi-secret rotation, typed reject'ler (401 stale/bad-MAC vs 400 malformed); `internal/automation/inbound.go`: durable insert + dedupe + poison→`failed` (§34.3); `webhook_pump.go`: retry/dead-letter 72h/20 (§21.6); `callback.go`: transactional callback_state outbox |
| Approvals | `internal/execution/approval.go` one-shot + publication approvals (mig 000013); APV-001 case'i var — Slack exact-approval binding'inin (SLK-007) tüketeceği seam |
| Knowledge / evals / console | `internal/knowledge/`, `tests/evals/`, `apps/web-console/` ÜÇÜ DE YOK; `examples/nextjs-sdk` (playwright 1.51.1) console'un relay + e2e EMSALİ |
| Discovery | `api/capabilities.go`: `map[string]string` per-capability tier (responses/sessions/workspaces) — E17 tier makinesinin takılacağı şekil HAZIR |
| Migrations | Zincir 000034; RLS 000029'dan beri FORCE (tenancy corpus gate'li); `secret_refs` 000031 (worker secret-handle kaynağı) |
| AUT-009/010/013 | `tests/uat/cases/`'te VAR — webhook-inbound seam'iyle kanıtlı (AUT-009 proof: `inbound_component_test.go`); **queue-adapter legi YOK** — E17 case'lere leg EKLER, yeni ID açmaz |
| SUB-006 / SUB-007 | SUB-006 var; SUB-007 (remote A2A child) YOK — T3'ün işi |
| ChildRun + egress | E08 T5 ChildRun seam'i + `packages/egress` (SSRF policy evi) — A2A client'ın tüketeceği ikili |
| Runner lease/fencing | `packages/runner` + coordinator: outbound enrollment, one-time token, fence token — CapabilityWorker (§31.2 "same lease/fencing semantics as a runner") bire bir buradan |
| SDK'lar + broker | üç dil SDK (E16), `packages/model-broker` (pinned embedding route T4'ün gireceği yol), TS SDK tam resource seti |
| Evidence | `tests/uat/evidence.go` (1367 satır, Complete() + anti-fabrication anchor disiplini), `promote.go` gate, `evidence/releases/` 15 bundle; `make evidence-verify` |
| Compose Postgres | plain `postgres@sha256:…` — pgvector YOK (vector = adapter interface + §6) |
| Credentials | `.env.local`: iki gerçek provider — embeddings + single-step live mümkün; Slack/Apple/cloud-queue credential'ı YOK |

---

## 4. Task breakdown

**DAG (cap 3; dört alt-alan bağımsız seam'ler olarak paralellenir):**
Wave 1: **T1** (17a Slack), **T4** (17b knowledge spine), **T7** (17d queue). Wave 2: **T2** (17a A2A server), **T5** (17b retrieval — T4'e bağlı), **T8** (17d orchestrator). Wave 3: **T3** (17a A2A client — T2'ye bağlı), **T6** (17b evals — T1/T7 adapter'larına integration-benchmark koşar), **T9** (17d worker). Wave 4: **T10** (17c console — yayında olan public API'nin tamamına karşı). Wave 5: **T11** (exit gate, hepsine bağlı). Migration-bearing merge sırası §1 tablosundaki gibi SABİT; her paralel merge sonrası `go vet -tags="component live" ./...` (parallel-merge tag-verify kuralı). Her task RED-first TDD + green milestone başına commit; gerçek entegrasyon dokunan legler `set -a` + `.env.local`.

**SECURITY-CRITICAL işareti taşıyan task'lar (T1, T2, T3, T5, T9, T10) full Fable review alır.**

### T1 — Slack adapter: Socket Mode + Events API, canonical mapping (17a; mig **000035**; SECURITY-CRITICAL)

- [ ] 000035: `slack_connections` (workspace/team/enterprise id, bot identity, OAuth scopes, signing-secret REF + token REF'leri — değerler secret_refs'te, allowed channels/users, identity mapping, default policy — §36.2), `slack_thread_sessions` (korelasyon: team+channel+thread_ts → session), message-ts reconciliation kaydı; hepsi RLS.
- [ ] İki transport, TEK normalized event (§36.3): Events API HTTP callback (signature verify — ParseInbound'un verify-önce-decode disiplini Slack'in v0 signing şemasıyla) + Socket Mode WS; transport değişimi korelasyon ID'lerini DEĞİŞTİRMEZ. **3-sn ack:** durable insert + dedupe commit ACK'TEN ÖNCE, iş sonra (SLK-001).
- [ ] Event-ID dedupe (SLK-002); thread↔session korelasyonu + web console attach aynı session (SLK-003); unmapped/unauthorized user → constrained integration actor, approve/model-change/content EDEMEZ (SLK-004); edit→correction event, delete→tombstone, file→scoped fetch + scan + artifact + token discard (SLK-005); bot/self-event ignore — loop yok (SLK-008).
- [ ] Live output: bounded coalesced updates, rate-limit'te canonical events'ten replay + görünür mesajın BİR KEZ repair'i, delivery-ID başına tek terminal summary (SLK-006); Slack failure canonical result'ı silmez.
- [ ] Exact approval: interactive payload user+workspace+request-hash+one-shot bağlı (`approval.go` seam'i — SLK-007); "yes" yazmak yüksek-risk için YETMEZ.
- **Seam:** `adapters/integrations/slack/` (yeni) + automation inbound pipeline + approval.go. **UAT:** SLK-001..008. **Tier:** **preview** — discovery `"slack": "preview"`.
- **Kanıt (burada koşar):** component + e2e vs **FAKE Slack peer** (local HTTP+WS fixture: event push, retry storm, rate-limit yanıtları, chat.postMessage kayıt receipts) — fake olduğu her yerde böyle ADLANDIRILIR.
- **Honest ceiling:** gerçek workspace external-receipt'leri (SLK'nın stable flip şartı) §6 leg 1; Enterprise Grid / org-wide OAuth install akışı SaaS (§5).

### T2 — A2A 1.0 server projection (17a; mig **000038**; SECURITY-CRITICAL)

- [ ] 000038: `a2a_interfaces` (published interface/card revizyonu, auth policy), `a2a_task_refs` (A2A task/context id ↔ canonical run/session — external ref, canonical ID asla replace edilmez §38.2); RLS.
- [ ] §38.1 HTTP binding tamamı (12 endpoint: agent-card.json, message:send/stream, tasks CRUD/cancel/subscribe, pushNotificationConfigs, extendedAgentCard). Agent Card: yalnız published AgentRevision projeksiyonu — provider model adı/internal tool/tenant envanteri SIZMAZ; exact version/binding/auth ilanı (A2A-001); revisioned + cacheable; sensitive için authenticated extended card.
- [ ] Canonical mapping: send-message yalnız gerçekten complete non-durable yanıt için direct Message, aksi Task; stream → status/artifact updates, terminal consistency (A2A-002); input-required→waiting_for_input, cancel→cancel Command + non-cancelable uncertain side-effect raporu, push → MEVCUT signed outbound webhook modeli (A2A-003).
- [ ] Inbound file/part → ingest + scan + artifact (A2A-004'ün server yarısı); A2A metadata org/project identity'yi OVERRIDE EDEMEZ (§38.6).
- **Seam:** `adapters/integrations/a2a/` (yeni) + api router + webhook sender. **UAT:** A2A-001, A2A-002, A2A-003. **Tier:** **preview** — `"a2a": "preview"`.
- **Kanıt (burada koşar):** protokol conformance suite (fixture'lı) + **generic loopback HTTP client** (T3'ün client'ı DEĞİL — bağımsız doğrulama).
- **Honest ceiling:** JWS/JCS card signing v0 DIŞI ("when trust policy requires" — §5); foreign generic-client interop §6 leg 2.

### T3 — A2A client: outbound remote agent / remote child (17a; mig **000039**; SECURITY-CRITICAL)

- [ ] 000039: `a2a_remote_agents` — registration Card identity/version, endpoint, auth Connection, allowed modalities, data/cost policy, timeout PINLER (§38.5); RLS.
- [ ] Remote agent = external child-run executor (E08 T5 ChildRun seam'i) veya tool-like specialist; remote output **untrusted**, minimum context/artifact alır, parent credential İNHERİT EDEMEZ (A2A-005 + SUB-007).
- [ ] SSRF controls: card/endpoint retrieval `packages/egress` policy'sinden; redirect/endpoint değişimi revalidation ister; extension URI allowlist; remote task ID'leri connection-scoped; pushed file ingest/scan (A2A-004 case'inin evi — client yarısı + T2 server yarısı tek case metninde adreslenir).
- [ ] Version negotiation: unsupported A2A versiyonu explicit fail (§38.7).
- **Seam:** `adapters/integrations/a2a/` client + ChildRun + egress. **UAT:** A2A-004, A2A-005, SUB-007 (case'i İLK kez materialize olur). **Tier:** a2a preview'unun parçası.
- **Kanıt (burada koşar):** fake remote A2A agent fixture'ı (kötü card/redirect/extension/secret-isteyen varyantlarıyla) + **loopback interop**: kendi client × kendi server (T2) gerçek bir A2A 1.0 alışverişidir ama AYNI repo'dur — foreign-peer iddiası YAPILMAZ.
- **Honest ceiling:** loopback ≠ interop; foreign peer §6 leg 2. SUB-007 remote child'ı fake-engine run'larla sürülür (E08 kuralı).

### T4 — Knowledge ingestion/index spine: PostgreSQL FTS (17b; mig **000036**)

- [ ] 000036: `knowledge_bases`, `knowledge_sources` (identity/authorization/sync-mode/classification/region/parser/retention pinleri), `ingestion_jobs`, `document_revisions`, `chunk_revisions` (source ACL + checksum + byte offsets + provenance), `index_revisions` — hepsi RLS; revision tabloları append-only self-re-asserting REVOKE; FTS: chunk generated tsvector kolonu + GIN, index-revision scoped.
- [ ] §25.15.2 dokuz-adım ingestion: snapshot→fetch→scan/validate→immutable DocumentRevision→deterministic chunk (parser/chunker revizyonu pinli)→ACL/checksum/offset/provenance→(optional) pinned embedding route→IndexRevision→**completeness check sonrası atomic activation**. Failed refresh prior aktif index'i BOZMAZ (KNO-002). Bytes object-store'da canonical, metadata PostgreSQL'de (§25.15.3).
- [ ] Source connector v0: uploaded artifact + repository path (mevcut E09 artifact/repo seam'leri) — web/DB connector YOK (§5). Connector credential'ı documents/chunks/model context'e GİRMEZ.
- [ ] Update/delete: stale revision pin reproducible; latest, silinen içeriği ve türev cache'leri DIŞLAR (KNO-004); provenance zinciri source→document→chunk→index (KNO-001).
- [ ] API: `/v1/knowledge-bases` (+sources, +ingest tetikleme, +index revisions) — admin ListView kalıbı (E16 T1 envelope kararı); `make generate`.
- **Seam:** `apps/control-plane/internal/knowledge/` (yeni) + api + model-broker (embedding route). **UAT:** KNO-001, KNO-002, KNO-004. **Tier:** T5 ile birlikte karar — `"knowledge": "preview"` girer, stable ADAYI.
- **Kanıt (burada koşar):** component-real (gerçek PostgreSQL + object store); live smoke: gerçek embedding çağrısı (tek API call — tool değil, E08'e uygun) pinned route üzerinden.
- **Honest ceiling:** parser v0 = text/markdown/code; office/PDF parser yok (§5). Vector index bu task'ta YOK — T5 interface'i.

### T5 — Retrieval: ACL-first filtering + knowledge security (17b; mig yok; SECURITY-CRITICAL)

- [ ] §25.15.4 retrieval request (query/modality, active-veya-pinned IndexRevision, principal/policy context, filters/trust class, max docs/chunks/tokens, keyword/vector/hybrid, optional pinned rerank route, freshness deadline, citation şartı) + §25.15.5 typed result (revizyon/source identity, exact offsets, skorlar, timestamps, trust, stable citation ref, checksum).
- [ ] **ACL-first:** authorization filtreleri skorlamadan ÖNCE ve return'den önce İKİNCİ kez; unauthorized document ne sızar ne görünür ranking'i etkiler (KNO-003) — post-filter top-K reddi RED-first testle pinlenir.
- [ ] Strateji: keyword=FTS (gerçek, local); **vector = adapter interface** (record ID'leri tenant+kb+doc+chunk+index-rev taşır, source-of-truth OLMAZ — §25.15.3) + deterministic fake adapter; hybrid birleşim + pinned strategy/route/scores/cost kaydı + citation offsets (KNO-005).
- [ ] Güvenlik: source içeriği untrusted — tool-result katmanında kalır, privileged instruction'a giremez, tool/capability GRANT EDEMEZ (KNO-006); restricted classification'lı source disallowed embedding provider/region'a GİTMEZ (KNO-007); embeddings source region/retention'ını izler; ACL değişimi türev cache'leri anında invalidate eder; cross-tenant/project vector search yasak (RLS + record-ID scoping).
- [ ] Freshness: karşılanamıyorsa policy'ye göre fail/warn — asla sessiz stale (KNO-008). `/v1/knowledge-bases/{id}/query` endpoint'i + retrieval TOOL'u (yalnız fake-engine run'larında — E08).
- **Seam:** `internal/knowledge/` + tool broker + egress. **UAT:** KNO-003, KNO-005, KNO-006, KNO-007, KNO-008. **Tier:** tüm KNO yeşilse `"knowledge"` (FTS core) **stable ADAYI**; `"knowledge-vector"` **disabled** (adapter var, gerçek store yok).
- **Kanıt (burada koşar):** component-real ACL negative corpus (iki tenant + iki proje, kasıtlı sızıntı fixture'ları DB-level reject); e2e-deterministic: fake-engine run retrieval tool'uyla cite eder, citation offset'leri chunk byte'larından doğrulanır.
- **Honest ceiling:** cross-encoder rerank yok — rerank optional pinned model route skorudur; gerçek vector store (pgvector/external) §6 leg 4.

### T6 — Eval suites + held-out release thresholds (17b; mig yok; `tests/evals/`)

- [ ] `tests/evals/` harness: EvalCase/DatasetRevision **content-addressed fixture formatı** (immutable, digest'li; DB-backed EvalSuite resource DEĞİL — §5'te gerekçe) + Go runner; grader öncelik sırası deterministic (schema/test/invariant/cost) > trace grader > model-as-judge (yalnız kalibrasyonlu, ASLA tek gate — §57.6: destructive-safety/secret/tenant/protokol gate'i olamaz). Train/validation/**held-out release** set ayrımı dosya düzeyinde.
- [ ] Dört suite: **coding** (§57.8 repo fixture'ları: bug fix, multi-file, hidden test, malicious repo instruction, secret-like fixture — deterministic reference engine ile), **research/citation** (§57.9 claim/source alignment, injection-from-source), **recovery** (§57.11 kill-point'leri — mevcut fault harness'ı hedef alır, yeniden yazmaz), **security/red-team** (§57.12: direct/indirect injection, tool-description/MCP/A2A card poisoning, secret extraction, SSRF, cross-tenant ID/cursor, approval deception — T1/T2/T3/T5 yüzeylerine koşar).
- [ ] **Integration benchmark** (§57.10): duplicate/out-of-order/identity/attachment/rate-limit/tek-terminal-yanıt fixture'ları T1 Slack + T2 A2A + T7 queue adapter'larına parametrik koşar.
- [ ] **Release threshold + gate mekaniği:** held-out eşikler `promote.go` zincirine bağlanır; MEKANİK kanıt: kasıtlı eşik-altı fake candidate promotion'da REFUSE edilir; security regression aggregate skordan bağımsız BLOKLAR (§57.13) — QUA-004'ün gate yarısı. QUA-001..004 case'leri materialize; her case metni deterministic legi ile gerçek-model legini (operator) AYIRIR.
- **Seam:** `tests/evals/` (yeni) + promote.go + fault harness. **UAT:** QUA-001..004 (harness+gate yarıları). **Tier:** evals discovery capability DEĞİLDİR — release makinesidir; QUA-003 security suite'i DİĞER capability'lerin stable flip'ine ön şarttır (T11).
- **Kanıt (burada koşar):** deterministic dört suite yeşil + refuse kanıtı; live subset: single-step research/citation cases gerçek provider'la + model-judge calibration smoke (`set -a` + `.env.local`).
- **Honest ceiling:** **gerçek modelle agentic benchmark KOŞULMAZ** (E08: engine gerçek provider'a tool açmaz) — sayılar deterministic engine'le HARNESS'I kanıtlar; real-model quality numbers §6 leg 7'dir ve E18 RC girdisidir. "Thresholds met" iddiası bu fazda gate-mekaniği iddiasıdır, model-quality iddiası değil.

### T7 — Queue adapter contract + outbound result delivery (17d; mig **000037**)

- [ ] QueueAdapter contract'ı (`adapters/integrations/queue/`): consume → InboundEvent normalize (mevcut automation pipeline'a — yeni run-identity YOK, §34.1); **ack YALNIZ** source auth + durable kayıt + dedupe commit + mapping admission commit/terminal-reject SONRASI (§34.2); uzun iş ack sonrası; visibility/lease extension; ordering yalnız correlation key'de queue/singleton concurrency istendiğinde (§34.3); poison → dead-letter view + advance; backpressure: bounded buffer + threshold'da consumption pause + queue depth/oldest-age raporu (§34.4); priority asla untrusted payload'dan.
- [ ] 000037: `queue_connections` + `queue_deliveries` (outbound outbox: stable delivery ID, destination idempotency alanı, retry/dead-letter state — `webhook_pump` kalıbı publisher'a); RLS. **Outbound result delivery:** run sonucu publisher down iken KAYBOLMAZ (§34.5).
- [ ] Referans adapter: **TEK gerçek broker** — NATS JetStream container'ı (spec §34.1 listesinde; en küçük gerçek durable broker) + in-memory fake (deterministic contract testleri). SQS/PubSub/Kafka-class contract AYNI conformance fixture setiyle parametriktir — adapter'ları YAZILMAZ (§5).
- [ ] **AUT-009/AUT-010 mevcut case'leri queue-adapter proof LEGİ kazanır** (case.yaml proof append — webhook legi durur, yeni ID açılmaz): redelivery-after-lost-ack duplicate üretmez; flood'da bounded memory + backpressure.
- **Seam:** `adapters/integrations/queue/` (yeni) + automation inbound/outbox. **UAT:** AUT-009, AUT-010 (queue legleri). **Tier:** `"queues"` (referans adapter) **stable ADAYI** — gerçek broker container'ıyla component-real yeşilse; SQS/PubSub discovery'de LİSTELENMEZ (yazılmamış şey ilan edilmez).
- **Kanıt (burada koşar):** component-real vs gerçek NATS JetStream container: lost-ack redelivery, flood backpressure, poison dead-letter, publisher-down'da loss-less outbound + tek delivery.
- **Honest ceiling:** cloud queue'lar (SQS/PubSub) credential ister — contract + fixtures hazır, icra §6 leg 5; Kafka-class semantik farkları (partition offset) contract'ta modellidir ama gerçek Kafka koşumu yoktur.

### T8 — External orchestrator helper + conformance (17d; mig yok)

- [ ] §35.1 beş-adım contract'ın conformance fixture suite'i: create-with-workflow-ID-metadata+idempotency-key → wait (webhook/SSE/poll üçü de) → message/approval/cancel command → structured result + artifacts → timeout sonrası reconcile-by-key. Fixtures HERHANGİ orchestrator'ın (Temporal/Restate/CI/script) uyacağı sözleşmeyi pinler.
- [ ] TS SDK **orchestrator kit** (`sdks/typescript/` — tek dil; Py/Go mirror talep gelirse): activity/step helper'ları, cancel propagation, external workflow ID ↔ run ID ayrımı, hangi sistem hangi timeout/retry'ın sahibi dokümantasyonu (§35.2 şartları bire bir; workflow history asla tek kopya değildir).
- [ ] **AUT-013 mevcut case'i orchestrator-kit legi kazanır:** scripted fake orchestrator retry fırtınası (aynı workflow ID + idempotency key) → TEK run, retry multiplication yok.
- **Seam:** `sdks/typescript/` + `tests/conformance/` + scripted fake orchestrator. **UAT:** AUT-013 (kit legi). **Tier:** discovery capability değil — SDK+docs+fixtures; interop iddiası AUT-013 yeşiliyle sınırlı adlandırılır.
- **Kanıt (burada koşar):** fixture suite + fake-orchestrator e2e (kill-and-reconcile dahil).
- **Honest ceiling:** native Temporal/Restate adapter'ı YAZILMAZ (§35.2 MAY; §5); gerçek Temporal koşumu §6 leg 6.

### T9 — CapabilityWorker contract + macOS fixture worker (17d; mig **000040**; SECURITY-CRITICAL)

- [ ] 000040: `capability_workers` (typed capability/version, os/arch, toolchain digests, capacity, tenant/pool/trust labels, health — §31.2), `capability_jobs` (job ID + idempotency key, run/attempt identity, exact capability+operation, input artifact refs, job-scoped secret handle refs, deadline/resource limits, output schema, network policy, fence token — §31.3); RLS + job journal append-only REVOKE.
- [ ] **Outbound enrollment** runner emsali bire bir: one-time token + short-lived workload identity + AYNI lease/fencing semantiği; worker'a inbound port AÇILMAZ. Job dispatch → progress/structured result/logs/artifacts/usage/execution receipt döner; worker model'den geniş credential İSTEYEMEZ.
- [ ] **Secret handle'lar:** job-scoped, kısa ömürlü (deadline'la expire), secret_refs (000031) üzerinden çözülür; değer worker sonucuna/log'a/evidence'a çıkmaz.
- [ ] Failure/idempotency (§31.6): read-only retry başka uyumlu worker'da; side-effect destination idempotency; stale fence REJECT; uncertain failure → quarantine; health/capability değişimi yeni lease'i keser.
- [ ] **macOS fixture worker:** HOST macOS'unda native process (Docker DIŞI — container network'ünden gerçekten ayrı = **gerçek private-network durumu**), compose control-plane'e outbound-enrolled; typed operation `swift.build-check` (host'ta `swiftc`/`xcrun` VARSA gerçek derleme-doğrulama, yoksa deterministic toy build — runtime detect, sonuç DÜRÜST adlandırılır); **NEGATİF kanıt:** ordinary sandbox worker'ı general tunnel olarak KULLANAMAZ — typed operation dışı istek reject, SOCKS-benzeri passthrough yok (§31.5); signing credential HİÇBİR yerde mevcut değil.
- [ ] **WRK-001..007 authored cases** (§64-katalog-dışı, MCI/EXT emsali): enrollment, typed dispatch, fence stale-reject, secret-handle scope+expiry, artifact round-trip, no-tunnel negative, quarantine-on-uncertain — §31 worker conformance'ının materializasyonu.
- **Seam:** `apps/control-plane/internal/workers/` (yeni) + coordinator lease + secret_refs + artifact store. **UAT:** WRK-001..007 (authored). **Tier:** `"capability-workers"` (contract, fixture capability) **stable ADAYI**; `"apple-build"` **disabled** — discovery gerçek Xcode+signing kanıtı olmadan bu capability'yi İLAN ETMEZ.
- **Kanıt (burada koşar):** iki-taraflı live: container'daki control-plane + macOS native fixture worker; typed job round-trip (artifact in → build-check → artifact out + receipt); fence + no-tunnel negative'leri.
- **Honest ceiling:** **BU PLANIN EN ÖNEMLİ TAVANI** — "macOS/iOS build" İDDİA EDİLMEZ: signing cert/provisioning profile/store credential YOKTUR; kanıtlanan şey outbound-enrolled typed-operation + private-network seam + no-tunnel + fenced-job invariant'larıdır. Gerçek signed macOS/iOS build (ephemeral keychain, result bundle, store publication ayrı capability) §6 leg 3'tür.

### T10 — Basic open-core console (17c; mig yok; SECURITY-CRITICAL: public-API-only)

- [ ] `apps/web-console/`: Next.js + @palai/sdk, `examples/nextjs-sdk` relay kalıbı — API key YALNIZ server-side relay'de, browser'a asla (server-relay stance E13/E16 kararı).
- [ ] §47.1 admin yüzeyi: organizations/projects/API keys (E13 provisioning API), model connections/routes (E16 T1 read-back), secret-ref METADATA (değer asla), agent revisions + diff, knowledge base yönetimi (T4 API'si).
- [ ] Live surface: session listesi + run timeline (mevcut SSE events), §47.2 ayrımı (canonical messages / progress / model steps / tool+subagent activity / approvals / usage / recovery-attempt transitions / terminal result); **exact approval UI**: action/args/diff/destination + risk + expiry — model summary authoritative detayı İKAME ETMEZ (UI-002); recovery/attempt display; artifact download.
- [ ] Accessibility (UI-001): keyboard nav, ARIA/screen-reader semantics, status asla yalnız renk, reduced-motion, timestamp UTC-on-demand (§47.5) — **axe-core automated checks + keyboard-nav playwright testleri** (mevcut 1.51.1 emsali).
- [ ] **Public-API-only MEKANİK kanıt:** playwright network intercept — console'un TÜM istekleri `/v1/*` relay'ine gider; privileged backchannel/DB erişimi SIFIR (§47.6).
- **Seam:** `apps/web-console/` (yeni) + @palai/sdk. **UAT:** UI-001, UI-002. **Tier:** `"console"` basic surface **stable ADAYI** — UI-001/002 otomatik kanıtlarla yeşilse; case metni otomatik-a11y'nin kanıtladığını ve manuel screen-reader pass'in §6 legi olduğunu AÇIKÇA yazar.
- **Kanıt (burada koşar):** playwright e2e local stack'e karşı: provision→key→model route→run başlat→timeline izle→approval ver→recovery göster→artifact indir; axe raporu temiz.
- **Honest ceiling:** ticari SaaS UI DEĞİL (§5); operator console (§47.4 cordon/drain/queue-inspect) DIŞARIDA — E15 ops CLI'ları var; config explainability (§47.3) minimal: effective değer gösterilir, katman-katman attribution sonraki iterasyon.

### T11 — EXIT gate: `extensions-0.1.0` + journeys + mekanik tier promotion (mig yok)

- [ ] `tests/uat/extensions/` journey'ler: **63.3 Slack journey TAMAMI fake-peer'de** (10 adım: install→mention→duplicate→stream→web attach→queued msg→exact approval→route change→cancel→rate-limit/kesinti; pass kriterleri bire bir); **knowledge journey** (ingest→ACL negative→retrieve+cite→source delete→propagation); **worker journey** (enroll→typed job→artifact round-trip→fence reject→no-tunnel negative).
- [ ] SLK-001..008, A2A-001..005, SUB-007, KNO-001..008, QUA-001..004, UI-001..002, WRK-001..007 case'leri `tests/uat/cases/` altında TAM materialize (AUT-009/010/013 legleri T7/T8'de eklendi); her case metni LOCAL seam'i (fake/loopback/deterministic/live) ve §6 operator bacağını AÇIKÇA adlandırır.
- [ ] `tests/uat/evidence.go` yeni claim/proof tipleri (Complete() gates): **SlackMappingProof** (fake-peer receipts + dedupe/terminal-summary sayaçları), **A2AConformanceProof** (endpoint×fixture matrisi + loopback transcript digest), **KnowledgeACLProof** (ACL-first negative sonuçları + citation offset'leri — verifier offset'leri chunk BYTE'larından yeniden hesaplar), **EvalGateProof** (held-out threshold + eşik-altı REFUSE kanıtı + suite skor digest'leri), **QueueDeliveryProof** (broker receipts'ten redelivery/dead-letter/loss-less sayaçları), **WorkerFenceProof** (fence reject + no-tunnel negative + secret-handle expiry), **ConsoleProof** (axe rapor digest + `/v1`-only network trace).
- [ ] **Anti-fabrication anchor (E13..E16 MUST-FIX-1 şekli):** **CapabilityTierProof** — manifest her capability için ilan edilen tier'ı + sahip olduğu claim ID listesini taşır; verifier tier'ı **manifest'in kendi kopyasından DEĞİL**, CANONICAL kaynaktan — bundle'daki per-case outcome'lardan — YENİDEN HESAPLAR (tüm claim'ler yeşil ⇒ stable, aksi ⇒ preview/disabled) ve çalışan stack'in `/v1/capabilities` snapshot'ının bu hesapla bit-eşit olduğunu assert eder. Fabrike "stable" değeri FAIL'dir; discovery flip'i bu doğrulamadan GEÇEREK yapılır.
- [ ] `make uat-extensions` + **`extensions-0.1.0` evidence bundle** (redacted manifest; ad dört alt-alanı tek çatıda DÜRÜST adlandırır — "stable-extensions" adı verilmez çünkü bazı capability'ler preview kapanır) + `make evidence-verify` 0/0/0/0; `promote.go` gate: tier tablosu + eval security-suite yeşili olmadan tag REFUSE.
- [ ] **Beklenen kapanış tablosu (T11 yeniden hesaplar; bu bir tahmindir, iddia değil):** `knowledge`=stable-aday, `queues`=stable-aday, `capability-workers`=stable-aday, `console`=stable-aday; `slack`=preview, `a2a`=preview; `knowledge-vector`=disabled, `apple-build`=disabled.
- **Exit-gate proof'un evi budur.** **Migration:** yok.
- **Honest ceiling:** journey 63.3'ün kanıtı fake-peer'dir ve bundle'da böyle etiketlenir; hiçbir preview capability'nin evidence'ı "stable" kelimesini taşımaz.

---

## 5. OUT OF SCOPE (bilinçli dışarıda, adres adresine)

| Kalem | Neden dışarıda | Nerede yaşıyor |
|---|---|---|
| Ticari SaaS UI (billing/team mgmt/product UI) | Master plan açık istisnası; console open-core §47.1 yüzeyidir | SaaS planı |
| SAN-009 managed microVM fleet | §2.2 açık istisna | SaaS planı |
| Operator console (§47.4 cordon/drain/queue-inspect UI) | E15 ops CLI'ları core workflow'u karşılar (§47.6) | Talep gelirse E18 sonrası |
| SQS/PubSub/Kafka gerçek cloud adapter'ları | Contract + parametrik conformance fixtures hazır; cloud credential ister | §6 leg 5; talep gelirse ayrı task |
| Temporal/Restate native adapter'ları | §35.2 MAY; canonical contract + kit yeter | §6 leg 6 |
| Web/DB connector'lı knowledge ingestion + office/PDF parser | SSRF/parser yüzeyi büyük; artifact+repo source'ları core'u kanıtlar | Sonraki knowledge iterasyonu |
| pgvector/external vector engine gerçek koşumu | Compose image plain; adapter interface + fake deterministic kanıt | §6 leg 4 |
| JWS/JCS Agent Card signing + key rotation | "when trust policy requires" (§38.3); v0 auth'lu extended card yeter | Hardening iterasyonu |
| Real-model eval quality numbers | E08 kuralı agentic live'ı engeller; harness+gate mekaniği kanıtlanır | §6 leg 7 → E18 RC |
| Slack Enterprise Grid / org-wide OAuth install | Kurumsal dağıtım akışı | SaaS planı |
| DB-backed EvalSuite/EvalRun canonical API resource'ları | Content-addressed dosya fixture'ları release-gate kullanımını karşılar; §57.18 portability korunur | Talep gelirse 000041+, owner onayı |
| Online evaluation (§57.15) | Managed-service özelliği | SaaS planı |
| SES-006..008 mid-chat model switch journey'leri | Session yüzeyi işi (master plan SES sahipliği) | İlgili epic |

## 6. Operator legs — gerçek-altyapı bacağı (deferred-but-scripted; kaybolmaz)

Her biri için KOD/harness bu fazda hazırdır ve parametriktir; İCRA operator-provided credential/altyapı/karar ister. **Preview→stable flip'leri bu leglerin icrasına bağlıdır ve flip yine T11 verifier'ından geçer:**

1. **Gerçek Slack workspace** — 63.3 journey Events API (public callback) + Socket Mode ile; SLK external-receipt'leri → `slack` stable flip.
2. **Foreign A2A peer** (a2aproject sample agent sınıfı bağımsız implementasyon) — server+client interop → `a2a` stable flip.
3. **Gerçek Xcode + Apple Developer signing** — ephemeral keychain, signed build/test, result bundle; store publication AYRI capability+approval → `apple-build` flip. (En kritik leg: bu plan yalnız fixture-worker seam'ini kanıtlar.)
4. **pgvector'lü Postgres veya external vector engine** — vector/hybrid retrieval gerçek store'da → `knowledge-vector` flip.
5. **Gerçek SQS/PubSub** conformance koşumu (fixtures parametrik).
6. **Gerçek Temporal instance'ı** ile orchestrator kit.
7. **Real-model eval benchmark koşumu** (quality numbers) — E18 RC girdisi.
8. **Deployed console + manuel VoiceOver/screen-reader pass** — UI-001'in otomatik-kanıt tavanının üstü.

## 7. Master plan §8 için önerilen özet blok (owner paste eder)

**UAT ownership:** SLK-001..008; A2A-001..005; SUB-007; KNO-001..008; QUA-001..004 (harness+gate yarıları; real-model quality §6→E18); UI-001..002; AUT-009..010, AUT-013 (queue/orchestrator legleri mevcut case'lere eklenir); WRK-001..007 (§31 worker conformance, authored); §57.10 integration benchmark — tamamı `tests/uat/cases/` altında materialize, kanıt seam'i (fake-peer/loopback/deterministic/live) case metninde adlandırılır.

**Exit gate — stable-vs-preview (E17'nin tanımlayıcı kuralı):** Bir capability YALNIZ sahip olduğu TÜM UAT claim'leri yeşilse STABLE ilan edilir; aksi halde `/v1/capabilities` discovery'sinde preview/disabled görünür. Tier, evidence verifier'ın per-case outcome'lardan YENİDEN HESAPLADIĞI bir fonksiyondur (CapabilityTierProof anchor'ı) — manifest'teki tier kopyasına güvenilmez, fabrike "stable" FAIL'dir. Local kapanışta beklenen: knowledge (FTS core), queues (referans adapter), capability-workers (fixture contract) ve console (basic surface) stable ADAYI; slack + a2a preview (gerçek-peer legleri §6); knowledge-vector + apple-build disabled. `extensions-0.1.0` evidence verifier 0/0/0/0 yeşildir; journey 63.3 fake-peer'de, knowledge ve worker journey'leri local stack'te koşar. **Bu gate "gerçek Slack'te/foreign A2A peer'iyle/signed Apple build'le çalıştı" İDDİA ETMEZ** — o bacaklar §6 operator legleridir; real-model eval quality sayıları E18 RC'ye aittir.
