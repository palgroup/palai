# Palai Interactive Sessions Implementation Plan (E08)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Library-idiom-heavy adımlar (SSE client'ları, Docker, pgx) brief'lerinde Context7 grounding alır (repo politikası, ledger 2026-07-17).

**Goal:** LP-0'ın tek-atımlık Response dikey dilimini, `MASTER-SPEC.md` §9/§22 durable interactive Session'ına büyütmek: multi-response chaining, queue/steer/interrupt command'ları, safe-boundary config değişimi, pause/resume/cancel/fork/close yaşam döngüsü, workspace'siz subagent'lar ve çok-client attach — hepsi gerçek provider'la canlı kanıtlanır.

**Architecture:** Yeni hiçbir çekirdek icat edilmez. E02'nin hazır durduğu sözleşmeler tüketilir: `SessionTable`/`CommandTable` state machine'leri (`packages/state-machines/session.go`, `command.go`), engine protokolünün zaten sözleşmeli ama implemente edilmemiş frame'leri (`protocols/engine/engine.schema.json`: `message.deliver`, `config.change`, `run.pause`, `run.cancel`, `child.request`, `child.result`) ve `engine.ready`'nin `commands` capability listesi. Orchestrator agent loop'una karışmaz; frame sınırlarında command pompalayan correlate+commit+dispatch rolünü korur (LP Task 11 invariantı).

**Master plan bağı:** §7 tablosu M5 Interactive Coding; §8 E08; §15 madde 7; LP planı §7 zorunlu sıra madde 1. UAT sahipliği: SES-001..012, AGT-003, SUB-001..005 (master plan §8/E08). SES-009/010'un checkpoint/snapshot GEÇERLİLİK yarısı E10 ile paylaşımlıdır (master plan E10 UAT satırı) — bu plan command/state yarısını kanıtlar.

---

## 1. Execution gate

- [ ] `main` >= `e0ec4e8` (pre-E08 runtime hardening merged: attempt-scoped dial deadline, worker/reconciler/retention supervision, runner cert renewal, engine reaping). Bu dört kapanış olmadan interactive canlı kanıt güvenilir koşmaz; plan bu yüzden hardening'i ön şart sayar.
- [ ] LP-0 evidence intact: `make evidence-verify RELEASE=local-live-0.1.0` PASS ve `make verify` yeşil bir baseline'dan başlanır.
- [ ] Her task RED-first TDD (master plan §14). Proof sınıfları §10.2'ye göre atanır; live-provider gerektiren case fake ile GEÇİLEMEZ.
- [ ] Branch: `feature/phase-08-interactive-sessions` (base: bu planın commit'i).

## 2. Bu dilimin sınırı

### Dahil

- `POST /v1/sessions`, `GET /v1/sessions/{id}`, `POST /v1/sessions/{id}/commands` (§9.1'in bu dilimde gereken alt kümesi; `messages` delivery'si `send_message` command kind'ı olarak commands üstünden gelir).
- Multi-response chaining: `session_id` ve `previous_response_id` artık GERÇEK semantik taşır (LP Task 5 minör-a kararı burada kapanır — aşağıda T1).
- `send_message` queue/steer/interrupt delivery (§9.2) + `applied_sequence` (§22.4) + duplicate-command-ID original result.
- Session config revisions + content-addressed redacted `ConfigSnapshot` + provenance (§9.3, §14); normal/immediate model-step-boundary uygulaması.
- `pause`/`resume`/`cancel`/`fork_session`/`close_session` command'ları + one-active-root-run invariantı (§22.3, §22.8).
- ChildRun subagent'lar (§11, §25.18-19): depth/fan-out/budget sınırları, capability intersection, required delegation, cancel propagation — workspace'siz (`workspace mode` alanı sözleşmede taşınır, default `none`; enforce edecek workspace E09'da).
- Çok-client attach: iki yetkili client aynı ordered journal'ı görür; yetkisiz attach existence disclosure vermez + audit denial (`audit_events` append-only tablosu mevcut).
- store:false purge'ün per-response yeniden anahtarlanması (`scrub_events` ponytail ceiling'inin kapanışı — LP planı §7 not-1 + Task 11d minör-1).

### Dahil değil

- Repository workspace, changeset, SUB-006, approval-üreten tool'lar ve `approve`/`deny` command'larının uygulanması → E09 (`phase-09-repository-coding.md`; command KIND'ları tabloda kabul edilir ama pending approval kaynağı olmadığından `command.rejected` döner — bu davranış T2'de test edilir).
- Checkpoint/snapshot GEÇERLİLİĞİ, recovery ladder, SES-009/010'un kurtarma yarısı → E10.
- AgentProfile/AgentRevision, triggers, schedules → E11. (AGT-003 tam tersini kanıtlar: profil YOKKEN her şey çalışır.)
- MCP/skills/hooks → E12; context compaction/knowledge → E17.
- Provider-level DB-backed model routing → E06 (LP planı §7.3). SES-006/007 model switch bu dilimde AYNI provider içinde model-id değişimiyle kanıtlanır (`ModelRoute.Model` per-step efektif olur); route-revision/connection seçimi E06'nın işidir.
- SDK session ergonomics → E16 (e2e/UAT harness API'yi doğrudan sürer; SSE attach için mevcut TS SDK stream'i yeterlidir, yeni SDK yüzeyi açılmaz).
- Slack/A2A attach yüzeyleri → E17 (§36.8 semantiği yalnız API tarafında kanıtlanır).

## 3. E08 acceptance contract — canlı eşleme

Exit gate (master plan §8/E08): **eşzamanlı client'lar aynı journal/final state'i görür; config change yalnızca safe boundary'de uygulanır.** Case'ler `tests/uat/cases/<ID>/case.yaml` disiplinini izler; isim = gerçekten assert edilen davranış (LP-0 follow-up dersi: overclaim yok).

| Case | Kanıt | Proof class | Ev |
|---|---|---|---|
| SES-001 | iki yetkili client attach → aynı ordered journal + final state (journal hash eşitliği) | e2e-deterministic + live tier'da tekrar | T6 |
| SES-002 | yetkisiz attach → tenant-scoped 404, içerik/existence sızıntısı yok, `audit_events` denial satırı | component-real | T6 |
| SES-003 | model/tool step sırasında queue → BİR kez, sonraki input boundary'de teslim | e2e-deterministic | T2 |
| SES-004 | steer → sonraki safe loop boundary; `applied_sequence` journal'da model step'ler arasında | e2e-deterministic + live | T2 |
| SES-005 | interrupt → cancelable step partial/canceled biter; yeni step mesajı içerir | e2e-deterministic | T2 |
| SES-006 | normal model switch → aktif step eski model'le biter, bir sonraki `model_requests` satırı yeni model'i taşır | live-provider (model-id kanıtı gerçek) | T3 |
| SES-007 | immediate switch → in-flight attempt canceled/partial; yeni config sonraki step'te; warning event | e2e-deterministic | T3 |
| SES-008 | policy'nin reddettiği tool-set değişimi → typed denial, silent fallback/broadening yok | unit + component-real | T3 |
| SES-009 (command yarısı) | pause → compute release (engine biter, attempt kapanır), resume → AYNI logical run YENİ attempt devam eder; checkpoint geçerliliği E10 | e2e-deterministic | T4 |
| SES-010 | tekrarlanan cancel → tek monotonic terminal; child'lar/tool'lar reconciled, duplicate terminal yok | e2e-deterministic + fault | T4+T5 |
| SES-011 | fork → history boundary'ye kadar kopya, yeni journal, gelecek izole; workspace'siz | component-real | T4 |
| SES-012 | close/delete → yeni mesaj reject; retention davranışı (11d reaper) doğru; legal hold E13 | component-real | T4 |
| AGT-003 | bütün yukarıdaki akış `agent_revision_id = null` iken çalışır (explicit assert) | e2e-deterministic | T6 |
| SUB-001 | optional delegation: model kullanabilir/atlayabilir; davranış explicit event'lerde | e2e-deterministic | T5 |
| SUB-002 | required child, daha ucuz model-id'li route → en az bir conforming child terminal; route/cost/result parent'a bağlı | live-provider (child gerçek provider'a gider) | T5+T6 |
| SUB-003 | required child karşılanamıyor → typed capability failure; parent-only sahte başarı yok | unit + e2e-deterministic | T5 |
| SUB-004 | depth/fan-out/budget sınırında deterministik deny; kaçak child yok | component-real | T5 |
| SUB-005 | parent cancel → children'a propagation + terminal accounting | fault-live (mid-run kill) | T5 |

## 4. Runtime topology delta

```text
LP-0 (bugün):  POST /v1/responses → taze session → tek run → engine loop → terminal → SSE
E08 (hedef):   Session (durable) ──> Response N (chained) ──> Run ──> Attempt(s)
                   │                        │
                   │   POST commands ───────┤ safe-boundary command pump (orchestrator)
                   │   (durable rows)       │   message.deliver / config.change /
                   │                        │   run.pause / run.cancel  → engine
                   │                        └─ child.request → ChildRun → child.result
                   └─ N client SSE attach (aynı journal), events per-response anahtarlı
```

- Engine protokolüne YENİ frame eklenmez; `protocols/engine/engine.schema.json`'da sözleşmeli olup implemente edilmemiş frame'ler engine (`palai_engine`) + supervisor + orchestrator'da hayata geçer. `engine.ready.commands` listesi gerçekten desteklenen command kind'larını ilan eder ve pin testine girer (LP Task 9 şema-pin kalıbı).
- §25.11 in-flight abort'un CONTROLLER yarısı (LP Task 9 minör-3 borcu) interrupt ile kapanır.
- Migration'lar task başına küçük ve idempotent-re-run testli (repo kalıbı: 000002 retention): 000003 chaining, 000004 commands, 000005 config revisions, 000006 child runs.
- Dependency direction korunur (master plan §5.1): state-machines pure kalır; api → execution → coordinator/storage; engine DB görmez.
- `events` tablosu `response_id` (nullable) kazanır; run-scoped append'ler doldurur, session-scoped event'ler NULL kalır (customer content taşımazlar, purge kapsamı dışında kalmaları doğrudur).

## 5. Task plan

### Task 1: Session chaining, lone-id semantiği ve per-response purge rekey

**Files:** Create `storage/migrations/000003_session_chaining.up.sql`/`.down.sql`, `storage/queries/sessions.sql`, `apps/control-plane/api/sessions.go`; Modify `apps/control-plane/api/responses.go`, `apps/control-plane/api/router.go`, `storage/queries/responses.sql` (scrub rekey), `apps/control-plane/internal/execution/` (run.start input assembly), ilgili contract schema + `make generate` çıktıları.

**Karar (LP Task 5 minör-a kapanışı):** continuation landığı için lone `session_id` ve lone `previous_response_id` artık ne ignore ne 400'dür — DESTEKLENİR. `session_id` → var olan active session'a yeni response açar; `previous_response_id` → önceki response'un session'ında devam eder ve history'ye bağlanır. Bilinmeyen/başka-tenant id → tenant-scoped 404 (existence disclosure yok, LP Task 5 kalıbı); closed/closing session'a create → 409 conflict problem'i. İkisi birlikte → mevcut 400 korunur.

- [ ] Failing testler: `TestCreateWithSessionIDAppendsToExistingSession` (aynı session'da iki response; journal TEK monotonic sequence hattı; ikinci run'ın engine input'u ilk response'un output'unu history olarak taşır); `TestCreateWithPreviousResponseIDContinuesSameSession`; `TestLoneSessionIDUnknownIs404TenantScoped` (cross-tenant negative dahil); `TestCreateOnClosedSessionConflicts`; `TestStoreFalsePurgeLeavesSiblingResponseEvents` (aynı session'da retained kardeş + store:false victim; reaper sonrası victim event'leri purged, kardeşinkiler INTACT — bugünkü session-level scrub'da bu test KIRMIZI yanar, `storage/queries/responses.sql:139` ponytail ceiling'inin birebir senaryosu); migration idempotent re-run + down.
- [ ] Fail'i doğrula; sonra: migration (`sessions.active_root_run_id` sütunu — T4'ün constraint'i için hazırlık; `events.response_id` nullable + index), `POST /v1/sessions` + `GET /v1/sessions/{id}` (idempotency + auth mevcut middleware'le), admission'da session resolve/create dallanması, `scrub_events` WHERE'inin `response_id`'ye anahtarlanması, run.start history assembly (minimum: aynı session'ın önceki retained response output'ları sıralı; purged olanlar `redacted_content` marker — §22.2; compaction YOK).
- [ ] History assembly run.start şemasına işlenir ve engine şema-pin testi güncellenir (LP Task 9 kalıbı; sessiz shape drift yasak).

**Verify:** `make verify && make test-component && make test-e2e TEST=responses` — yeni testler dahil yeşil; LP deterministic suite regress etmez.

### Task 2: Command spine — durable commands, queue/steer/interrupt, applied_sequence

**Step 0 (ÖN ŞART — hardening review F1(b)):** `apps/control-plane/e2e/responses/harness_test.go` subprocess seam'i `exec.CommandContext(dialCtx)` ile engine PROSESİNİN ömrünü H1'in 20s dial deadline'ına bağlıyor; 20s'i aşan mid-run steer e2e'si "signal: killed" flake üretir. Fix (~10 satır): plain `exec.Command` + `subprocessChannel.Receive` ctx'i honor eder; mevcut e2e yeşil kalır. Bu adım kırmızı uzun-koşu testinden ÖNCE yapılır, yoksa T2'nin kendi testi flake'e çarpar.

**Files:** Create `storage/migrations/000004_commands.up.sql`/`.down.sql`, `storage/queries/commands.sql`, `apps/control-plane/api/commands.go`, `apps/control-plane/internal/execution/command_pump.go`, `engines/reference/src/palai_engine/commands.py`; Modify `apps/control-plane/api/router.go` (`POST /v1/sessions/{session_id}/commands`), `apps/control-plane/internal/execution/orchestrator.go` (frame-boundary hook), `engines/reference/src/palai_engine/loop.py` + `protocol.py` (`message.deliver` kabulü, `engine.ready.commands` ilanı), engine testleri.

Semantik (§9.2, §22.4): kabul = durable satır + 202 + idempotent (duplicate command_id orijinal result'ı döner — idempotency_records DEĞİL command tablosunun kendi unique'i; command_id caller-supplied). `CommandTable` transitions (queued→applying→applied/rejected/expired) event'leriyle sürülür. `queue` → aktif step'e DOKUNMAZ, sonraki input boundary'de (aktif run'ın model.result→model.request arası İLK input noktası; run terminal olduysa session'daki bir SONRAKİ response'un history'sine) BİR kez teslim. `steer` → sonraki safe loop boundary'de `message.deliver` frame'i. `interrupt` → cancelable step abort: outstanding `model.request`'in §25.11 in-flight-abort CONTROLLER yarısı burada implemente edilir (adapter cancel — LP Task 10 cancel yolu mevcut), partial kayıt, aynı run yeni step'te mesajı içerir. `applied_sequence` = etkinin journal sequence'ı; `command.applied.v1` event'i onu taşır. Orchestrator agent loop'u YENİDEN YAZMAZ: pump, frame commit noktalarında pending command okur ve frame yazar.

- [ ] Failing testler: `TestQueuedMessageDeliversOnceAtInputBoundary` (SES-003; iki kez teslim edilmediği frame ledger'ından kanıtlı); `TestSteerAppliesAtNextLoopBoundaryWithSequence` (SES-004; applied_sequence iki model step'in event sequence'ları ARASINDA); `TestInterruptEndsCancelableStepPartial` (SES-005; in-flight abort + partial event + yeni step'te mesaj); `TestDuplicateCommandIDReturnsOriginalResult`; `TestCommandOnTerminalRunRejected` (typed `command.rejected`); `TestApproveWithoutPendingApprovalRejected` (E09 devri davranışı); uzun-koşu subprocess e2e: >20s süren steer akışı "signal: killed" OLMADAN tamamlanır (Step 0 kanıtı).
- [ ] Engine tarafı pytest: `commands.py` `message.deliver`'ı input/loop boundary'de deterministik işler; `engine.ready.commands` gerçek listeyi ilan eder ve şema-pin testi günceller; desteklenmeyen frame'e `protocol.error` (mevcut kalıp).
- [ ] Fail'i doğrula; minimum implementasyon; command pump'ın fence disiplini: yalnız aktif attempt'in fence'iyle frame yazılır (stale attempt command teslim edemez).

**Verify:** `make verify && make test-e2e TEST=responses && (cd engines/reference && pytest)` + `make test-fault CASE=coordinator` regress yok.

### Task 3: Config revisions, ConfigSnapshot ve normal/immediate switch

**Files:** Create `storage/migrations/000005_config_revisions.up.sql`/`.down.sql`, `apps/control-plane/internal/execution/config.go`; Modify `apps/control-plane/internal/execution/model_dispatch.go` (per-step efektif model), `apps/control-plane/api/commands.go` (`change_config` kind payload'ı), engine `config.change` kabulü, contract schema + generate.

Kapsam çiti: provider DEĞİŞMEZ (E06 §7.3 carve-out'u aynen durur; `ModelRoute.Provider` env-selected kalır). Değişebilenler: model id (aynı provider içinde — `model_requests` satırı per-step efektif modeli zaten kaydediyor) ve tool allowlist (pure tool seti üstünde). Resolution zinciri minimum: deployment → project → session config revision (§14 sırasının bu dilimde var olan katmanları); çıktı content-addressed (SHA-256, canonical JSON — LP Task 11 content_hash kalıbı) redacted `ConfigSnapshot` + her efektif değerin katman provenance'ı. Normal change: aktif step biter, SONRAKİ model step yeni config; immediate: in-flight step interrupt yoluyla (T2 mekanizması yeniden kullanılır) partial + warning event + yeni config. Policy deny (project'in izin listesi dışında tool/model) → typed `command.rejected` + problem; silent fallback YOK (SES-008).

- [ ] Failing testler: `TestNormalModelSwitchAppliesNextStep` (SES-006'nın deterministic yarısı: ardışık `model_requests` satırları eski→yeni model'i kanıtlar); `TestImmediateModelSwitchInterruptsStep` (SES-007; partial + warning + yeni step yeni model); `TestDeniedToolChangeIsTypedRejection` (SES-008; fallback/broadening yok); `TestConfigSnapshotContentAddressedWithProvenance` (aynı girdi → aynı hash; provenance katman adları doğru); `TestConfigChangeAppliesOnlyAtStepBoundary` (exit gate'in ikinci cümlesi: mid-step config sızmaz — frame ledger'ından negative kanıt); migration idempotent.
- [ ] Fail'i doğrula; minimum resolver + snapshot; secret ref'ler ref olarak kalır (redaksiyon testi — LP secret hygiene kalıbı).

**Verify:** `make verify && make test-component TEST=postgres && make test-e2e TEST=responses`.

### Task 4: Lifecycle commands — pause/resume/cancel/fork/close + one-active-root + 14b borç kapanışı

**Files:** Modify `apps/control-plane/internal/execution/` (pause/resume/fork/close yolları), `apps/control-plane/internal/execution/finalize.go` + `packages/contracts/` (canceledProjection'ın contracts'a taşınması), `apps/control-plane/internal/execution/lease.go`, `storage/queries/responses.sql` (terminal-aware UPDATE), `storage/migrations/000003`'ün hazırladığı `active_root_run_id` üstüne partial unique constraint (bu task'ın migration'ı 000006 DEĞİL — T1 migration'ına sığmıyorsa küçük 000005b; implementer tek dosyada tutabiliyorsa T1'de birleştirir, plan bunu serbest bırakır).

Semantik: one-active-root (§22.3) DB constraint'iyle (partial unique index, terminal-olmayan root run başına tek satır — app-code yarışı yerine constraint; ihlal 409 problem). `pause` → engine'e `run.pause`, cooperative stop, attempt kapanır, compute release (container biter — reaper değil normal teardown), run `waiting`; `resume` → AYNI run yeni attempt, history mevcut journal'dan (checkpoint GEÇERLİLİĞİ E10 — burada transcript-boundary devamı yeter, SES-009 command yarısı). `cancel` → 14b guard + `e08a898` fix üstüne üç borç kapanır: (1) `TestCancelOnPurgedResponseIs410` (ledger minörü); (2) `canceledProjection`'ın canonical Problem'i contracts'a taşınır, finalize'daki kopya silinir (ledger minör-3); (3) 2-tx cancel penceresi KALICI fix: `responses` terminal UPDATE'i conditional olur (`WHERE state NOT IN (terminal)`) — geç gelen terminal projection'ı DB seviyesinde kaybeder, `e08a898`'in kapattığı yarışın kalıcı sınıf çözümü (ledger minör-2). `fork_session` (§22.8): fork sequence boundary'ye kadar immutable messages/config referans kopyası, YENİ journal, workspace yok, pending approval/lease inherit yok. `close_session`/delete: `SessionTable` closing→closed→deleted; closed session'a create/command → reject; retention 11d reaper'la etkileşir (scrub artık per-response — T1).

- [ ] Failing testler: `TestSecondConcurrentRootRunConflicts` (constraint teetiği; SQLSTATE-level assert — LP Task 3 reviewer kalıbı); `TestPauseReleasesComputeResumeSameRunNewAttempt` (SES-009 command yarısı; attempt_id değişir, run_id sabit); `TestRepeatedCancelSingleMonotonicTerminal` (SES-010; ikinci cancel no-op + tek terminal event — 14b testinin genişlemesi); `TestCancelOnPurgedResponseIs410`; `TestLateTerminalCannotOverwriteTerminalRow` (fault: iki tx arasında kill; conditional UPDATE kanıtı — `e08a898` regression'ı DB seviyesine iner); `TestForkCopiesHistoryBoundaryIsolatesFuture` (SES-011; fork'tan sonra parent'a yazılan mesaj child journal'da YOK); `TestClosedSessionRejectsNewWork` (SES-012).
- [ ] Fail'i doğrula; minimum implementasyon; `RunTable`/`SessionTable`'da eksik transition çıkarsa satır ekleme registry-sync + property testleriyle yapılır (E02 kalıbı), spec §22.1/22.3 anchor'ı commit mesajına yazılır.

**Verify:** `make verify && make test-e2e TEST=responses && make test-fault CASE=coordinator && make test-component`.

### Task 5: Subagents — ChildRun, budget intersection, cancel propagation

**Files:** Create `storage/migrations/000006_child_runs.up.sql`/`.down.sql` (`runs.parent_run_id`, `runs.depth`), `apps/control-plane/internal/execution/child_dispatch.go`, `tests/fault/subagents/`; Modify `engines/reference/src/palai_engine/loop.py` (`child.request` emit / `child.result` kabul), `apps/control-plane/internal/execution/orchestrator.go` (child pump — command pump kalıbının aynısı), contract schema + generate.

Semantik (§25.18-19): engine `child.request` frame'i yayar (role, objective, model-route alias, tool subset, budget, deadline, `workspace_mode` — default `none`, alan sözleşmede taşınır ama enforce edecek workspace yok, E09 devri). Controller admission: depth/fan-out/eşzamanlı child sınırları deterministik deny (SUB-004); capability = parent ∩ project intersection; secret inheritance yok (LP invariantı zaten: secret engine'e inmez, child de aynı broker'dan geçer); budget = parent'ın KALANI ile intersect (aşan istek deterministik clamp/deny). ChildRun = `runs` satırı `parent_run_id`'li, kendi attempt/journal event'leri, mevcut `ExecuteRun` yoluyla dispatch (yeni execution motoru YOK — durable job aynı makine). Child terminal → `child.result` frame'i parent'a TYPED result olarak döner (hidden transcript değil); parent final output child run id'lerini identify eder (§25.19 son madde). Required delegation: admission'da conforming route yoksa typed fail, parent-only sahte başarı yasak (SUB-003). Parent cancel → bütün non-terminal children'a propagation (detach policy bu dilimde yok — hepsi propagate) + terminal accounting (SUB-005, SES-010'un child ayağı).

- [ ] Failing testler: `TestChildRunDepthAndFanoutBounded` (sınırda deterministik deny; kaçak child satırı yok); `TestChildBudgetIntersectsParentRemainder`; `TestRequiredDelegationFailsTypedWhenUnroutable` (SUB-003); `TestChildResultEntersParentAsTypedResult` (SUB-001/002 deterministic yarısı; parent journal'ında child.result + child run linki); `TestParentCancelPropagatesToChildren` (fault-live: parent mid-run cancel, child'lar canceled terminal, accounting tutarlı); `TestChildRunCarriesOwnJournalScopedEvents` (child event'leri parent'ın SSE'sinde parent-scoped görünümle — child'ın AYRI journal'ı parent stream'ini şişirmez; görünürlük kuralı: parent journal'a yalnız child lifecycle + result event'leri girer); migration idempotent.
- [ ] Fail'i doğrula; minimum implementasyon; recursive delegation OFF default (depth>1 istek → deny; §25.18).

**Verify:** `make verify && make test-e2e TEST=responses && make test-fault CASE=subagents` (yeni fault dizini Makefile'a bağlanır).

### Task 6: Çok-client attach + canlı interactive UAT journey → E08 kapanışı

**Files:** Create `tests/e2e/sessions/` (çok-client harness), `tests/uat/cases/SES-*/case.yaml` + `SUB-*` + `AGT-003` (bu dilimde kanıtlanan yarılar; isimler assert edileni söyler), yeni evidence release `interactive-0.1.0`; Modify `tests/uat/` harness (LP `newStack`/`runCase` yeniden kullanılır — greenfield değil), `Makefile` (`uat-interactive` hedefi; `uat-local-live` DOKUNULMAZ kalır), `scripts/uat/` gerekiyorsa.

- [ ] Failing testler (deterministic tier): `TestTwoClientsSeeIdenticalOrderedJournal` (SES-001: iki eşzamanlı SSE reader, Last-Event-ID reconnect dahil, event-id+payload hash dizisi EŞİT); `TestUnauthorizedAttachIsTenantScoped404WithAuditDenial` (SES-002: cross-tenant key ile attach → 404 + `audit_events` denial satırı, content-free); `TestProfileFreeSessionRunsAllCoreFeatures` (AGT-003: `agent_revision_id` null assert'i journey harness'ine gömülü).
- [ ] Canlı journey (live-provider, compose stack, gerçek OpenAI; LP Task 15 hygiene disiplini aynen — `.env.local`, set -a, credential asla argv/log/evidence): create session → response-1 → **normal model switch** (aynı provider, farklı model id; ardışık `model_requests` satırları old→new kanıt) → **interrupt** (applied_sequence gerçek iki model step'i ARASINDA — mid-run command'ın applied_sequence canlı proof'unu interrupt taşır) → response-2 `previous_response_id` ile (history taşındı) → **required child** (SUB-002: child farklı/ucuz model id ile gerçek provider'a gider; iki farklı gerçek provider request id) → **ikinci client attach** journal hash eşitliği (SES-001 canlı; R1-scoped — stream ilk run terminal'inde kapanır, events.go tavanı, dürüstçe adlandırılmış) → **cancel** → **fork** → fork'ta kısa response-3 → **close**. Evidence manifest: provider request id'ler, applied_sequence'lar, iki client'ın journal hash'i, child run linki. **NOT (T2 bulgusu / teslim edilen Option A):** mid-run **steer/queue** single-step canlı provider'da structurally-unobservable (`pumpCommands` tool-boundary gerektirir, gerçek provider tool döndürmez) — canlı applied_sequence proof'unu bu yüzden **interrupt** taşır; steer/queue deterministic-tier'da kanıtlanır (SES-003/004 e2e-deterministic, pause/resume ile aynı disiplin). Canlı journey'de "live steer" İDDİA EDİLMEZ.
- [ ] Evidence verifier: `interactive-0.1.0` release'i için mevcut `VerifyRelease` + live-provider `^chatcmpl-` kuralı (b20627c) aynen çalışır; manifest şeması gerekiyorsa alan ekler (generate + drift gate).
- [ ] `make uat-interactive PROVIDER=provider-one` PASS; hygiene grep (secret literalleri) = 0; deterministik tier aynı journey'i fake adapter'la CI'da koşar (e2e-deterministic), CANLI tier onsuz GEÇMEZ.

**Verify:** `make verify && make test-e2e && make uat-interactive PROVIDER=provider-one && make evidence-verify RELEASE=interactive-0.1.0`.

## 6. Final release check (E08 exit gate)

- [ ] `make verify`, `make test-component`, `make test-e2e`, `make test-fault` PASS.
- [ ] `make uat-interactive PROVIDER=provider-one` bütün bu-dilim case'leri PASS; hiçbir live-provider case fake ile geçmedi.
- [ ] `make evidence-verify RELEASE=interactive-0.1.0` PASS; secret finding 0.
- [ ] LP-0 regressyonu yok: `make uat-local-live PROVIDER=provider-one` hâlâ PASS (chaining/command değişiklikleri tek-atımlık yolu bozmadı).
- [ ] Exit gate cümlesi kanıtlı: eşzamanlı client'lar aynı journal/final state (SES-001 canlı + deterministic); config change yalnız safe boundary (T3 negative frame-ledger kanıtı).
- [ ] `git status --short` temiz; generated drift 0.

## 7. Bu plana girmeyen devirler (kayıt yeri: bu commit'li plan)

1. **Hardening review M1 — cert re-sign registry/revocation yok (ev: E14 PKI).** Renewal 5m sert tavanı kaldırdı; çalınan cert+key süresiz yeniden imzalatılabilir. Runner identity registry + revocation listesi + enrollment audit E14 production self-host planının PKI maddesidir (LP planı §7.4-3 ile birleşir). E08 işi değildir.
2. **Hardening review M2 — supervisor panic-recover etmez; M3 — ReceiveLease doc eksiği (ev: E10).** Supervised loop'lar error-return'ü restart eder ama panic'i etmez; E10 kill-matrix harness'i panic yolunu zaten tetikleyeceğinden recover+counter oraya eklenir. M3 tek doc-comment'tir, aynı süpürmede gider.
3. **`approve`/`deny` command uygulaması (ev: E09).** İlk approval-üreten tool yüzeyi (repository/side-effect tool'ları) ile gelir; bu dilimde kind kabul + `command.rejected` (T2 testi).
4. **SES-009/010 checkpoint/snapshot geçerlilik yarısı (ev: E10).** RecoveryProof olmadan "pause/resume çalışıyor" iddiası verilmez; bu plan yalnız command/state yarısını iddia eder.
5. **SDK session ergonomics (ev: E16).** `sessions.*` SDK yüzeyi üç-dil parity işiyle birlikte açılır.
6. **SUB-006 child workspace + SUB-007 A2A child (ev: E09 / E17).** Master plan sahiplik matrisi aynen.
