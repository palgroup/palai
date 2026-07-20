# Palai Checkpoint, Snapshot, Recovery ve Replay Implementation Plan (E10)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Library-idiom-heavy adımlar (Docker API kill/inspect, pgx advisory/conditional writes, aws-sdk-go-v2 S3 list, Python pickle-siz durable serialization) brief'lerinde Context7 grounding alır (repo politikası, ledger 2026-07-17).

**Goal:** E08'in durable Session'ı + E09'un workspace/coding journey'si üstüne `MASTER-SPEC.md` §26/§53 kurtarma katmanını koymak: checkpoint / workspace snapshot / transcript boundary AYRI immutable objects; exact → compatible checkpoint → transcript reconstruction → explicit failure recovery ladder; pure/idempotent/reversible/irreversible/interactive replay decisions + uncertain reconciliation jobs; process / engine-container / runner-daemon / whole-host kill harness; outage sırasında queue/steer/interrupt ordering + old-host stale-fence denial; parent-detached durable child + durable parent↔child conversation; ve **RecoveryProof** — "continued" log'u tek başına ASLA kanıt saymayan resource/evidence. Journey 63.2 artık kill+recovery DAHİL geçer; duplicate external effect sıfırdır; **SH-1 ancak bundan sonra verilir**.

**Architecture:** Yeni çekirdek icat edilmez. Engine **BİZİM** `palai_engine`'imizdir (Agent SDK DEĞİL); checkpoint formatı reference-kernel'dir ve `engines/reference/src/palai_engine/checkpoint.py` bu planda doğar. Tüketilen hazır seam'ler:
1. **Sözleşmeli-ama-implemente-edilmemiş frame'ler** (E08 kalıbının aynısı): `run.restore`, `checkpoint.request` (controller→engine) ve `checkpoint.offer` (engine→controller) `protocols/engine/engine.schema.json`'da ZATEN var; `engine.ready` `checkpoint_formats` alanı da (`:69`). E10 bunları engine + supervisor + orchestrator'da hayata geçirir; yeni frame tipi eklenmez.
2. **Fencing/lease substrate** — E09 T1'in fencing token + stable-logical-id + conditional-write snapshot guard'ı (`packages/coordinator/workspace.go`; guarded-INSERT `a.fence=MAX`) host-loss'un DB yarısını zaten kapattı; `host_lost→recovering→failed` WorkspaceBinding state'leri `packages/state-machines/workspace.go:18-19,64-65`'te VAR ama kimse sürmüyor. E10 onları sürer.
3. **Kill/fault harness substrate** — `tests/fault/coordinator` (worker_kill, stale_fence), `tests/fault/runner` (container_kill, stream_kill), `tests/fault/sandbox`, `tests/fault/subagents` LP Task 7'den beri gerçek kill koşuyor; E10 `tests/fault/recovery/` ile matrisi 4 seviyeye (process/engine-container/runner-daemon/whole-host) tamamlar ve kill'in ARKASINA restore'u koyar (bugün kill → fail/reclaim; restore yok).
4. **Faithful-resume transcript yolu** — E08 T4'ün `LookupModelResult` committed-step replay'i recovery ladder'ın 3. basamağının (transcript reconstruction) çekirdeğidir; E10 onu ladder'ın altına alır ve DÜRÜSTÇE etiketler (§26.3: transcript reconstruction asla "exact resume" diye anılmaz).
5. **Artifact store** — E09 T2'nin S3 boundary'si (`apps/control-plane/internal/artifacts/`) checkpoint/snapshot byte'larının evi olur; engine DB/S3 GÖRMEZ (§24 korunur).

**E09/E08 devirlerinin TAMAMI bu planda sahiplidir** (ledger + Fable review kayıtları): orphan-GC tek-reconcile (T3), streaming-supervisor tail-frame race (T5), SAN-005 RESTORE + SAN-006 gerçek host-kill (T6), SES-009/010 checkpoint-GEÇERLİLİK/recovery yarıları (T4/T7), workspace recovery host_lost wiring (T6), reclaim-crash-mid-fold durable delivered-message (T2), parent-detached durable conversation (T8), supervisor panic-recover + ReceiveLease doc (E08 planı §7.2 → T5), merge_records parent_run_id index (E09 T6 M3 → T6 migration rider'ı).

**Master plan bağı:** §7 M6; §8 E10 (satır 420-436); §63.2 kill+recovery yarısı (adım 8-9 + "recovery evidence complete, no duplicate tool/push/PR" pass koşulu). UAT sahipliği: ENG-004..014; TOL-001..004 + TOL-016..017; SAN-005..008; SES-009..010. TOL-016/017'nin yalnız ledger/fence yarısı buradadır (remote tool SDK/signing E12); SAN-009 microVM E15. Authored case'ler (E09 REG/DEL/APV kalıbı): REC-001..006 + DET-001..002 (§64 katalog reconciliation §7 devri).

---

## 1. Execution gate

- [ ] `main` >= **E09 merge tip** (`<SHA — E09 exit-gate merge'ünde pinlenir>`; T1-T9 + coding-0.1.0 evidence dahil). E10 workspace/artifact/tool/approval/task yüzeylerinin HEPSİNİ tüketir; foundation olmadan başlanmaz.
- [ ] Evidence intact: `make evidence-verify` `local-live-0.1.0` + `interactive-0.1.0` + `coding-0.1.0` PASS; `make verify` yeşil baseline.
- [ ] **Koşullu devir kontrolü (E09 T6 MAJOR):** `adapters/repositories/worktree.go:80-81` merge-abort hâlâ error-swallow + same-ctx recovery ise (E09 T7 STEP-0 kapatmadıysa) → 2 satırlık fix (`context.Background()` recovery + abort-fail→error) **T1 STEP-0** olur; kapattıysa bu madde işaretlenip geçilir.
- [ ] Her task RED-first TDD (master plan §14). Proof sınıfları §10.2; **daha düşük sınıfla pass edilemez** (line 641): `fault-live` simüle-string/sleep ile GEÇMEZ — kill gerçek SIGKILL/container-rm/daemon-stop'tur; `live-provider` fake ile GEÇMEZ.
- [ ] Her task gerçek **fault smoke** ve/veya gerçek-provider **LIVE smoke** ile biter (kullanıcı politikası). E08 "real runs single-step" ceiling'i E09 T4'te DÜŞTÜ (ilk gerçek multi-step forced-tool loop) — E10'un canlı kill noktaları bu forced-tool loop'un boundary'lerinde yaratılır; mid-window kill deterministik tier'da test-hook injection'la, canlı tier'da boundary-kill ile alınır (honest ceiling her smoke'ta adlandırılır).
- [ ] Credential hygiene E09'dan aynen: `.env.local`, `set -a`, hygiene grep (`sk-` + Git PAT + App key + installation token) = 0; **checkpoint/snapshot byte'ları da taranır** (checkpoint'e secret sızması = SAN-005 exclusion ihlali).
- [ ] Migration'lar `000014`'ten başlar (E09 T5/T7/T8 `000010/000012/000013`'ü tüketiyor; dispatch'te en yüksek numara yeniden doğrulanır).
- [ ] Branch: `feature/phase-10-recovery-replay` (base: bu planın commit'i).

## 2. Bu dilimin sınırı

### Dahil

- **Üç ayrı immutable recovery object** (§26.1-26.2): engine checkpoint (opak, control-plane YORUMLAMAZ), workspace snapshot (E09 create'i restore ile tamamlanır), canonical transcript boundary — paylaşılan `boundary_id`, bağımsız format/retention; birinin restore'u diğerini İMA ETMEZ. Checkpoint metadata §26.2 alan seti (format/format_version/config_snapshot_hash/transcript_sequence/workspace_snapshot_id/pending_operations/content_checksum); size-bound + checksum-integrity.
- **Recovery ladder** (§26.3-26.4): exact continuation → portable checkpoint → transcript reconstruction → explicit failure; compatibility decision 7 koşulu; seçilen seviye event'te GÖRÜNÜR; transcript reconstruction asla "exact resume" diye adlandırılmaz. Checkpoint frequency politikasının bu dilimdeki alt kümesi (§26.5): side-effect tool sonrası, publication state değişimi sonrası, pause/wait öncesi, explicit request.
- **RecoveryProof** (§26.12): önceki/yeni attempt id, recovery level, checkpoint/snapshot id'leri, transcript boundary, replayed-vs-reused tool calls, semantic-loss warning, ölçülen süre — resource + evidence-verifier kuralı; **"resumed" yazısı tek başına kanıt DEĞİL**.
- **Tool replay classes + tool-call state machine + uncertain reconciliation** (§26.6-26.7, §53.6): pure/idempotent/read-variance/reversible/irreversible/interactive; `uncertain → reconciled_completed / reconciled_not_applied / manual_resolution`; her call için request hash, lease owner, external idempotency key, reconciliation state, commit boundary.
- **Kill harness 4 seviye** (§26.8): engine process kill, engine container kill, runner daemon kill, whole-host kill (local tier'da whole-host = runner daemon + o runner'ın TÜM container'ları + workspace host-path'inin erişilmez kılınması — tek-makine yaklaşımı DÜRÜSTÇE adlandırılır; gerçek çok-host drain E14/E15).
- **Outage semantiği** (§26.9): kesinti sırasında kabul edilen queue/steer/interrupt'ların recovery sonrası kanonik sırayla teslimi; reconstructed historical model step'in İÇİNE mesaj enjekte edilmez; old host dönerse diagnostics yükleyebilir ama fence ilerledikten sonra authoritative frame REDDEDİLİR.
- **Workspace recovery** (E09 devri): `host_lost→recovering→failed` wiring, snapshot RESTORE (checksum-eşitlik kanıtı), allocation reclaim host-move'da (E09 fencing/stable-logical-id substrate'i üstünde); SAN-007 allocation-reuse hijyeni; SAN-008 failed-destroy host quarantine.
- **E08/E09 borç kapanışları:** reclaim-crash-mid-fold (durable delivered-message row — İKİ varyantı da kapatan tek çözüm, `command_pump.go:35-44` notu), streaming-supervisor tail-frame race (`adapters/sandboxes/oci/stream.go`), orphan-GC tek-reconcile (S3 bucket-list vs `artifacts` rows, İKİ yön + delete-error), supervisor panic-recover + restart counter (E08 §7.2).
- **Parent-detached durable child + durable parent↔child conversation** (master plan satır 431): release-parent (checkpoint + `run.waiting`) + durable child job + `run.restore` ile parent-detached long-lived child; child idle `waiting`; parent→child `send_message` command spine (E08 T2) üstünden; child→parent journal event; parent stop = cancel-propagation (E08 T5). E09 T7'nin model-facing yarısının (agent tool, send_to_agent, child-idle event) DURABLE/detached yarısı.

### Dahil değil

- **Checkpoint/snapshot byte'larının envelope-encryption-at-rest'i** → E13 (LP §7.1 ailesi). E10 checksum-integrity + size-bound + secret-absence kanıtlar; §26.2 "encrypted" sözü E13'ün SecretRef/sealing işi kapanmadan İDDİA EDİLMEZ — o zamana kadar hiçbir sürüm "encrypted checkpoint" demez.
- **PKI / host-attestation / runner identity registry + revocation** → E13/E14 (E08 planı §7.1, LP §7.4-3). SAN-011 customer-runner-revoke E10 işi değildir; stale-fence denial buradadır, cert revocation değil.
- **Remote tool SDK, imzalı HTTP tool'lar, MCP** → E12. TOL-016/017'nin E10 yarısı tool-call LEDGER'ıdır (stable `tool_call_id` dedupe + fence-gated late-callback) — gerçek local HTTP double'a karşı component-real; signed-request/SDK yüzeyi E12'de aynı case'lerin E12 yarısını kanıtlar.
- **Schedules/triggers/timer'lı automation** → E11. Orphan-GC ve reconciliation job'ları retention-Reaper kalıbındaki süpervizyonlu iç loop'lardır (E11 scheduler'ı değildir).
- **SAN-009 microVM tenant isolation + warm-pool PROVISIONING + fleet drain** → E15/E14. SAN-007 burada allocation-reuse hijyen İNVARYANTI olarak kanıtlanır (önceki tenant verisi/prosesi/credential'ı yok, taze writable layer); gerçek warm-pool havuzu E15.
- **Context compaction** → E17. `context.compacted` frame'i ve compaction-sonrası-checkpoint hook'u (§26.5 maddesi) compaction landığında E17'de bağlanır.
- **DB-backed model routing** → E06; **SDK recovery ergonomics** → E16 (UAT harness API'yi doğrudan sürer).
- **Recursive delegation depth>1** kapalı kalır (E08 sabitleri; detach depth'i DEĞİŞTİRMEZ).

## 3. E10 acceptance contract — canlı eşleme

Exit gate (master plan §8/E10; §63.2 pass): **journey 63.2 kill+recovery DAHİL geçer; recovery evidence complete; duplicate external effect (tool/push/PR) SIFIR; SH-1 ancak bundan sonra.** Case'ler `tests/uat/cases/<ID>/case.yaml` disiplini; isim = gerçekten assert edilen davranış (overclaim yok). Proof sınıfı §10.2; E10'un ağırlığı `fault-live`'dadır — kill gerçek, "recovered" iddiası yalnız RecoveryProof'la kabul edilir (REC-006 verifier kuralı).

| Case | Kanıt | Proof class | Ev |
|---|---|---|---|
| ENG-004 | engine process kill → yeni attempt checkpoint VEYA transcript'ten restore + RecoveryProof | fault-live | T5 |
| ENG-005 | engine container kill → workspace/checkpoint/transcript recovery + mesaj sırası korunur | fault-live | T5 |
| ENG-006 | runner host kaybı → stale fence, yeni host'ta placement, ladder evidence | fault-live | T6 |
| ENG-007 | old host dönüşü → diagnostics kabul, authoritative frame REDDİ (fence-advance sonrası) | fault-live + component-real | T6 |
| ENG-008 | exact resume → aynı sağlıklı process/lease teyitli, "exact" etiketli | e2e-deterministic | T4 |
| ENG-009 | compatible checkpoint → yeni process boundary'yi restore eder, tool replay hatası yok | e2e-deterministic + component-real | T4 |
| ENG-010 | incompatible/corrupt checkpoint → rejected event + transcript fallback veya explicit failure | e2e-deterministic | T4 |
| ENG-011 | checkpoint migration → orijinal korunur, migrated checksum/provenance, rollback semantiği | component-real | T4 |
| ENG-012 | outage sırasında queue/steer/interrupt → recovery sonrası kanonik teslim sırası | fault-live + e2e-deterministic | T7 |
| ENG-013 | terminal frame + process crash → current fence altında TEK persisted terminal | fault-live | T5 |
| ENG-014 | terminal'siz process exit → asla false success; recovery veya explicit failure | fault-live | T5 |
| TOL-001 | pure tool replay (kill sonrası) → cached/replayed etiketli, semantic duplication yok | fault-live | T7 |
| TOL-002 | destination-idempotent tool → aynı external key, TEK external object | e2e-deterministic (faithful destination double) | T7 |
| TOL-003 | irreversible tool response kaybı → uncertain/manual resolution; ASLA auto-replay | fault-live + e2e-deterministic | T7 |
| TOL-004 | reversible side effect → önce reconcile, policy'ye göre compensate/retry | e2e-deterministic | T7 |
| TOL-016 (ledger yarısı) | duplicate/retry → stable `tool_call_id` TEK execution/result; signed-transport yarısı E12 | component-real (gerçek local HTTP) | T7 |
| TOL-017 (fence yarısı) | async timeout + late callback → aktif fence/reconciliation stale silent commit'i ENGELLER | component-real + fault-live | T7 |
| SAN-005 (restore yarısı) | snapshot restore → file/index/tree checksum'lar create'tekiyle EŞİT; exclusion'lar boş/secret'sız (create E09'da kanıtlı) | component-real | T6 |
| SAN-006 (host-kill yarısı) | GERÇEK host-kill → fence advance → stale writer write/snapshot reddi (fence-advance E09'da deterministik kanıtlı) | fault-live | T6 |
| SAN-007 | allocation-reuse → önceki tenant verisi/prosesi/credential'ı YOK, taze writable layer (warm-pool provisioning E15) | component-real | T6 |
| SAN-008 | failed destroy → host yeni tenant'a KAPALI (quarantine), placement deny | component-real + fault-live | T6 |
| SES-009 (validity yarısı) | pause → GEÇERLİ checkpoint+snapshot (§26.11), resume ladder'dan restore eder (command/state yarısı E08'de kanıtlı) | e2e-deterministic | T4 |
| SES-010 (recovery yarısı) | kill/outage sırasında cancel → children/tools RECONCILED, tek monotonic terminal (command yarısı E08'de kanıtlı) | fault-live | T7 |
| REC-001 (authored) | fast-exit engine'in `run.terminal`'i ASLA düşmez (tail-frame race kapanışı; mid-stream drop zaten monotonic-sequence'a takılır) | fault-live | T5 |
| REC-002 (authored) | applied-ama-fold-öncesi crash → yeni attempt'te delivered-message row'dan TAM BİR kez redelivery | fault-live + e2e-deterministic | T2 |
| REC-003 (authored) | applied+folded user turn, resume/reclaim SONRASI provider context'inde GÖRÜNÜR (R1 varyantı) | e2e-deterministic | T2 |
| REC-004 (authored) | orphan-GC tek reconcile: bucket-list vs rows İKİ yönde (write-side orphan + delete-error) grace-window'la temiz; referanslı object ASLA silinmez | component-real | T3 |
| REC-005 (authored) | host-move: `host_lost→recovering→ready` yeni allocation'da, logical workspace id SABİT, eski allocation fenced | fault-live | T6 |
| REC-006 (authored) | RecoveryProof §26.12 alanları TAM; verifier "continued" log'u tek başına REDDEDER | e2e-deterministic + verifier kuralı | T4 |
| DET-001 (authored) | parent release (checkpoint + waiting) → child parent-detached devam eder → parent resume child sonucunu typed görür | e2e-deterministic + live smoke | T8 |
| DET-002 (authored) | detached child idle `waiting` → command spine `send_message` child'a ulaşır → child→parent journal event → konuşma durable/resumable | e2e-deterministic | T8 |

## 4. Runtime topology delta

```text
E09 (bugün):  Attempt kill ─> reclaim ─> yeni attempt (transcript replay yalnız committed steps)
              snapshot CREATE var / RESTORE yok; host_lost SM state'leri var / süren yok
              kill ⇒ fail-veya-reclaim; "recovery" İDDİASI YOK

E10 (hedef):  Run ─> Attempt N ──X (kill: process | engine-container | runner-daemon | whole-host)
                        │
                        ├─ durable objects @ boundary_id:
                        │    checkpoint (chk_, opak, checksum)  ── S3 (artifact store)
                        │    workspace snapshot (wsnap_)        ── S3 + restore
                        │    transcript boundary (bnd_)         ── events/journal (DB)
                        │
              recovery ladder (packages/coordinator/recovery/):
                 1 exact (aynı lease teyit) → 2 compatible checkpoint (run.restore)
                 → 3 transcript reconstruction (canonical msgs + completed tool results)
                 → 4 explicit failure
                        │
                        ├─ delivered_messages (durable) ─> yeni attempt'te kanonik redelivery
                        ├─ tool_calls ledger (replay class + uncertain → reconciliation job)
                        └─ Attempt N+1 + attempt.recovering + RecoveryProof (level GÖRÜNÜR)

              Workspace(logical id SABİT) ─> yeni Allocation(fence+1) ─> snapshot RESTORE (checksum ==)
              old host ─> diagnostics OK / authoritative frame DENY (stale fence)
              Parent run ─(release: checkpoint + run.waiting)─> detached child (durable job)
                 └─ send_message spine ⇄ child journal event ─> parent resume (run.restore)
              Orphan-GC: S3 list ⟷ artifacts rows tek reconcile (grace-window, iki yön)
```

- **Yeni frame yok:** `run.restore`/`checkpoint.request`/`checkpoint.offer`/`run.waiting` zaten sözleşmeli; engine (`checkpoint.py`) + supervisor + orchestrator implemente eder; `engine.ready.checkpoint_formats` gerçek listeyi ilan eder ve şema-pin testine girer (LP Task 9 kalıbı). Yeni event tipleri: `checkpoint.*`, `recovery.*`, `attempt.recovering.v1`, `workspace.restored.v1`, `tool_call.uncertain.v1`/`tool_call.reconciled.v1` — `event-types.json` + `make generate` + drift gate.
- **Checkpoint/snapshot byte'ları artifact store'a** yazılır (E09 T2 boundary'si; engine/runner S3 credential GÖRMEZ, §24). Control-plane checkpoint içeriğini YORUMLAMAZ (§26.2) — yalnız metadata + checksum.
- **Kill noktaları test-hook'la deterministikleşir:** fault harness'ler kill'i gerçek yapar (SIGKILL / `docker rm -f` / daemon stop); "kill nerede" seçimi boundary-hook'ladır, sleep-tabanlı yarış DEĞİL.
- Dependency direction korunur: state-machines pure; recovery ladder coordinator'da; engine DB/S3/credential görmez; ladder engine'e yalnız frame konuşur.
- Migration'lar `000014`'ten (T1 checkpoints/boundaries, T2 delivered_messages, T6 hosts/quarantine + merge_records index rider, T7 tool_calls ledger, T8 detach kolonları); her biri küçük + idempotent-re-run testli.

## 5. Task plan

DAG (SPEED REGIME, ledger 2026-07-19): T1/T2/T3 bağımsız açılış dalgası; T4←T1+T2; T5←T4; T6←T4(+T5 harness kalıbı); T7←T2+T4; T8←T4 (+E09 T7 model-facing yarısı); T9←hepsi. LIVE/fault smoke'lar serialized (paylaşılan :local tag).

### Task 1: Durable recovery objects — engine checkpoint + üç immutable object + boundary_id

**Files:** Create `engines/reference/src/palai_engine/checkpoint.py` (loop state serialize/deserialize; reference-kernel format_version=1; `checkpoint.offer` emit, `checkpoint.request` kabul), `storage/migrations/000014_recovery_objects.up.sql`/`.down.sql` (checkpoints + transcript_boundaries; workspace_snapshots E09 000008'de var — boundary_id kolonu rider), `packages/coordinator/recovery/objects.go` (immutable insert + checksum verify + size bound), `storage/queries/recovery.sql`; Modify `engines/reference/src/palai_engine/loop.py` + `protocol.py` (offer boundary'leri: side-effect tool sonrası, pause öncesi, explicit request — §26.5 alt kümesi; `engine.ready.checkpoint_formats=["reference-kernel/1"]`), `apps/control-plane/internal/execution/orchestrator.go` (checkpoint.offer → artifact store PUT + metadata row; supervisor geçişi), contract schema-pin + `make generate`.

Semantik (§26.1-26.2, §26.5): üç obje AYRI — restore'ları birbirini ima etmez; paylaşılan `boundary_id`. Checkpoint OPAK: control-plane yorumlamaz, yalnız §26.2 metadata + `content_checksum` + size-bound uygular; byte'lar artifact store'a (engine S3 görmez — frame'le supervisor üstünden akar, PUT control-plane'de). Checkpoint failure runı her zaman düşürmez ama pause/wait/drain GEREKLİ recoverable boundary olmadan giremez (§26.5 son cümle). SES-009 validity yarısının CREATE tarafı: pause yolu artık checkpoint + snapshot ÜRETİR (E08 T4 pause'u yalnız boundary-stop'tu).

- [ ] Failing testler: `TestCheckpointOfferPersistsImmutableRowAndBytes` (offer → row + S3 object + checksum eşleşir; ikinci yazma AYNI id'ye reddedilir — immutable); `TestCheckpointMetadataCarriesSpecFields` (§26.2 alan seti + config_snapshot_hash gerçek ConfigSnapshot'tan + transcript_sequence gerçek journal'dan); `TestBoundaryLinksThreeObjectsIndependently` (boundary_id üçünü bağlar; snapshot'sız checkpoint declare-no-workspace-dependency taşır); `TestCheckpointSizeBoundRejected`; `TestPauseProducesValidCheckpointBeforeComputeRelease` (SES-009 create tarafı); engine pytest: `test_checkpoint_roundtrip_restores_loop_state` (serialize→deserialize→AYNI kaldığı-yer; pickle DEĞİL — deterministik JSON/typed format), `test_checkpoint_request_frame_produces_offer`; migration idempotent + down.
- [ ] Fail'i doğrula; minimum implementasyon; `checkpoint_formats` şema-pin; hygiene: checkpoint byte'ları secret-scan'e girer (workspace `/secrets` exclusion'ı E09'dan; model context içi secret zaten yok — REP-003 invariantı).
- [ ] **Fault/LIVE smoke:** gerçek `provider-one` forced-tool run'ı (E09 T4 kalıbı) side-effect boundary'de checkpoint offer üretir; offer persist olduktan sonra engine SIGKILL → row+bytes+checksum DURABLE ve doğrulanır. Honest ceiling: bu smoke checkpoint CREATE'in dayanıklılığını kanıtlar; restore T4'tedir.

**Verify:** `make verify && make test-component && (cd engines/reference && pytest) && make test-e2e && make test-live-provider PROVIDER=provider-one CASE=checkpoint-create`.

### Task 2: Reclaim-crash-mid-fold kapanışı — durable delivered-message row (E08 T4 M1/R1 borcu)

**Files:** Create `storage/migrations/000015_delivered_messages.up.sql`/`.down.sql` (delivered-message row: command_id, run_id, applied_sequence, fold_state, content ref); Modify `apps/control-plane/internal/execution/command_pump.go` (`:35-44` E10 notunun kapanışı: apply anında durable row; attempt-start'ta applied-undelivered redelivery), `apps/control-plane/internal/execution/execute_run.go` / history assembly (R1: applied+folded mesajlar post-resume provider context'ine kanonik sırayla GİRER — run.start history artık yalnız prior-response-output değil), `storage/queries/commands.sql`.

Semantik (E08 T4 Fable adjudication, ledger): İKİ varyant tek çözümle kapanır — **yalnız durable delivered-message row ikisini de kapatır** (applied-undelivered-redelivery tek başına R1'i KAPATMAZ): (varyant-1) mesaj applied + fold commit'inden önce attempt crash → mesaj engine memory'sindeydi, kayıp; `command.applied.v1` yalanı. (R1) applied+folded+committed user turn, resume/reclaim sonrası run.start history'sinde YOK → model sonraki step'lerde user turn'ü görmez. Redelivery kanonik sıra §26.9: input boundary'de, TAM BİR kez (row fold_state guard'ı), reconstructed historical step'in İÇİNE asla enjekte edilmez. Bu task ENG-012 ve T4 transcript-reconstruction'ın ÖN KOŞULUDUR.

- [ ] Failing testler: `TestAppliedMessageSurvivesCrashBeforeFoldCommit` (REC-002; test-hook kill applied-commit ↔ fold arasında; yeni attempt row'dan redeliver, frame ledger'ında TAM BİR `message.deliver`); `TestRedeliveryIsExactlyOnceAcrossTwoReclaims` (iki ardışık crash → yine tek teslim); `TestAppliedFoldedTurnPresentInPostResumeHistory` (REC-003/R1; resume sonrası run.start history'sinde user turn kanonik konumda — E08 T4'ün faithful-resume testinin genişlemesi); `TestRedeliveryNeverInjectsIntoReconstructedStep` (§26.9 son madde — negative); migration idempotent + down.
- [ ] Fail'i doğrula; minimum row + redelivery; `command.applied.v1` semantiği artık yalan söyleyemez (applied = durable).
- [ ] **Fault smoke:** gerçek Postgres + gerçek engine subprocess'iyle kill-injection koşusu CI'da; LIVE yarısı: gerçek provider forced-tool run'ına steer, tool boundary'de applied, controlled kill, resume → ikinci GERÇEK provider request payload'ında steered turn görünür (provider request kaydından assert). Honest ceiling: kill noktası hook'ludur; canlı kanıt redelivered turn'ün GERÇEK model çağrısına girmesidir.

**Verify:** `make verify && make test-e2e TEST=responses && make test-fault CASE=coordinator && make test-live-provider PROVIDER=provider-one CASE=steer-survives-kill`.

### Task 3: Orphan-GC — S3 bucket-list vs artifacts rows TEK reconcile (E09 T2 borcu)

**Files:** Create `apps/control-plane/internal/artifacts/gc.go` (list-vs-rows reconcile + grace-window + delete-unreferenced), `apps/control-plane/internal/artifacts/gc_test.go`; Modify `apps/control-plane/cmd/palai-control-plane/main.go` (retention Reaper yanına supervised GC loop — aynı süpervizyon kalıbı), `apps/control-plane/internal/execution/retention.go` (`:73` Sweep-error yolunun kalıcı kapanışı: silinemeyen key artık kayıp değil — GC yakalar).

Semantik (E09 T2 Fable kaydı, ledger): **TEK GC, İKİ yön + delete-error.** (a) S3'te var / `artifacts`'ta non-empty `object_key` YOK → orphan: purge-crash orphan'ı (byte-delete best-effort DB-commit sonrası) + write-side orphan (`writer.go` object PUT sonrası row insert fail) AYNI reconcile'a düşer; (b) retention delete-error (S3 delete başarısız, row tombstoned, key kayıp) → aynı tarama yakalar. Grace-window: yeni PUT edilmiş ama henüz commit'lenmemiş object yanlışlıkla silinmez (last-modified > pencere olanlar atlanır). **Referanslı object ASLA silinmez** — silme kararı yalnız rows-join'inin boşluğuyla. Ayrı retention-GC epic'i YOKTUR (ponytail: tek reconcile, iki yönü de kapatır).

- [ ] Failing testler (gerçek SeaweedFS + gerçek PG — E09 T2 çerçevesi): `TestGCDeletesUnreferencedObjectAfterGrace` (purge-crash senaryosu: row tombstoned + object duruyor → GC siler); `TestGCDeletesWriteSideOrphan` (object var, row insert fail simülasyonu → GC siler); `TestGCSweepsFailedRetentionDelete` (delete-error injection → sonraki GC turu byte'ı temizler); `TestGCNeverDeletesReferencedOrInGraceObject` (negative: referanslı + taze object'ler INTACT — asıl güvenlik invariantı); `TestGCRunsSupervisedAndRecorded` (tur sayacı/log — sessiz ölüm yok).
- [ ] Fail'i doğrula; minimum reconcile; tenant-scoping: GC bucket-genelidir ama silme kararı org/project'ten bağımsız SAF referans yokluğuyla verilir (existence disclosure yüzeyi yok — internal loop).
- [ ] **Fault smoke:** gerçek run artifact yazarken PUT↔row-insert arasında crash injection → orphan doğar → GC turu reconcile eder; S3 list ile doğrulanır. Honest ceiling: component-real infra; model davranışı değil.

**Verify:** `make verify && make test-component TEST=artifacts && make test-fault CASE=coordinator` regress yok.

### Task 4: Recovery ladder + run.restore + RecoveryProof (ENG-008..011, SES-009 validity, REC-006)

**Files:** Create `packages/coordinator/recovery/ladder.go` (§26.3 seviye seçimi + §26.4 compatibility decision), `packages/coordinator/recovery/proof.go` (RecoveryProof resource), `apps/control-plane/internal/execution/restore.go` (run.restore frame assembly: checkpoint ref + replay boundary / transcript reconstruction input), `engines/reference/src/palai_engine/` restore yolu (checkpoint.py deserialize → loop kaldığı yerden); Modify `apps/control-plane/internal/execution/orchestrator.go` + `worker.go` reclaim yolu (reclaim artık ladder'a danışır — bugünkü düz yeni-attempt yerine), `tests/uat/evidence.go` (REC-006 verifier kuralı: recovered-run case'i RecoveryProof alanları olmadan FAIL — `^chatcmpl-`/external-receipt kurallarına paralel), contract schema (`recovery.*` event'leri + RecoveryProof) + `make generate`.

Semantik (§26.3-26.4, §26.12): ladder SIRALI dener: (1) exact — orijinal process current fenced lease'i hâlâ tutuyor ve reconnect ack'liyor → "exact" etiketi; (2) compatible checkpoint — format/protocol/checksum/config/boundary/workspace 7 koşulu geçen checkpoint `run.restore` ile yeni process'e; (3) transcript reconstruction — canonical messages + completed tool results + config snapshot + artifact refs (E08 faithful-resume `LookupModelResult` çekirdeği buraya taşınır) — ASLA "exact resume" diye anılmaz; (4) explicit failure. Seçilen seviye `attempt.recovering.v1` + final run'da GÖRÜNÜR. ENG-011: sandboxed migration step format v1→v2 (gerçek minimal transform) — orijinal KORUNUR, yeni checkpoint yeni checksum + provenance, rollback = orijinale dönüş. SES-009 validity yarısı: pause→resume artık ladder-2'den (T1 checkpoint'i) restore eder; compute release + aynı logical run + yeni attempt E08'de kanıtlıydı, GEÇERLİLİK burada kapanır. RecoveryProof §26.12 sekiz alan; "continued" log tek başına kanıt DEĞİL (verifier kuralı).

- [ ] Failing testler: `TestLadderPrefersExactWhenLeaseAlive` (ENG-008; sağlıklı process → exact etiket, checkpoint DOKUNULMAZ); `TestCompatibleCheckpointRestoresBoundaryNoToolReplay` (ENG-009; restore sonrası completed tool'lar replay EDİLMEZ — frame ledger negative); `TestIncompatibleCheckpointFallsToTranscriptWithRejectedEvent` (ENG-010; corrupt checksum + format-mismatch İKİ ayrı tetik); `TestPolicyForbidsReconstructionExplicitFailure` (ladder-4); `TestCheckpointMigrationPreservesOriginalWithProvenance` (ENG-011 + rollback); `TestResumeRestoresFromValidCheckpoint` (SES-009 validity — pause'lu run resume'da ladder-2'den döner, transcript-only DEĞİL); `TestRecoveryProofFieldsComplete` + `TestVerifierRejectsContinuedLogWithoutProof` (REC-006); engine pytest restore-roundtrip.
- [ ] Fail'i doğrula; minimum ladder + restore + proof; transcript reconstruction'a mesaj enjeksiyonu T2 kurallarıyla (reconstructed step içine asla).
- [ ] **LIVE smoke:** gerçek provider forced-tool run'ı checkpoint boundary'sinden sonra engine kill → ladder-2 GERÇEK restore → run GERÇEK provider'la tamamlanır; RecoveryProof evidence'ta (önceki/yeni attempt id + level + süre); kill-öncesi/sonrası iki gerçek chatcmpl id. Honest ceiling: kill boundary-hook'lu; canlı kanıt restore edilen run'ın gerçek modelle DOĞRU kaldığı-yerden devamıdır.

**Verify:** `make verify && make test-e2e && (cd engines/reference && pytest) && make test-fault CASE=coordinator && make test-live-provider PROVIDER=provider-one CASE=checkpoint-restore`.

### Task 5: Kill harness I — process + engine-container + terminal-frame bütünlüğü (ENG-004/005/013/014, REC-001, panic-recover)

**Files:** Create `tests/fault/recovery/` (kill matrix harness'in process/container yarısı; `tests/fault/runner` kalıpları yeniden kullanılır — greenfield değil), `adapters/sandboxes/oci/snapshot/` (container-kill anındaki workspace durumunun snapshot'lanması — E09 create kodu buraya taşınmaz, çağrılır); Modify `adapters/sandboxes/oci/stream.go` (tail-frame race: supervisor container exit'inde stream'i EOF'a kadar DRAIN eder, sonra reap — fast-exit engine'in `run.terminal`'i düşmez), `packages/coordinator/supervise.go` (E08 §7.2 M2: supervised loop'lara panic-recover + restart counter + doctor çıkışı; M3 ReceiveLease doc-comment aynı süpürmede), `packages/runner/supervisor.go` (kill-sonrası durum raporu).

Semantik (§26.8, E09 T1 Flag-B): **REC-001 tail-frame:** mid-stream drop zaten monotonic-sequence check'ine takılır (bounded damage); tehlike TAIL-only — engine terminal yazar-yazmaz exit ederse supervisor frame'i okuyamadan reap edebilir. Fix: exit'te drain-to-EOF, SONRA reap; sıra garantisi teste girer. **ENG-013:** terminal frame persist edildikten SONRA process crash → current fence altında TEK terminal (E08 T4 conditional-UPDATE zemini + tail-drain birlikte). **ENG-014:** terminal'siz exit → ASLA false success; ladder (T4) devreye girer veya explicit failure. **ENG-004/005:** process kill ve container kill (`docker rm -f`) → T4 ladder'ıyla restore + RecoveryProof; ENG-005 mesaj SIRASI da korunur (T2 redelivery + §26.9). Panic-recover: kill matrisi supervised loop'ların panic yolunu gerçekten tetikler — recover + backoff-restart + sayaç doctor'da.

- [ ] Failing testler: `TestFastExitEngineTerminalFrameNeverLost` (REC-001; N-iterasyon gerçek subprocess fast-exit — flake değil determinist drain kanıtı); `TestTerminalThenCrashSingleTerminalUnderFence` (ENG-013); `TestExitWithoutTerminalNeverFalseSuccess` (ENG-014; run completed OLMAZ — recovery veya failed); `TestEngineProcessKillRecoversViaLadder` (ENG-004; SIGKILL mid-tool-loop → yeni attempt + RecoveryProof); `TestContainerKillRecoversWorkspaceAndOrder` (ENG-005; `docker rm -f` → snapshot/transcript recovery + queued mesaj sırası); `TestSupervisedLoopRecoversPanicWithCounter` (panic injection → loop yaşar + sayaç artar).
- [ ] Fail'i doğrula; minimum drain + harness + recover; harness kill'leri GERÇEK (SIGKILL/Docker API), simülasyon yok.
- [ ] **Fault-LIVE smoke:** gerçek provider forced-tool run'ı mid-loop CONTAINER kill → ladder restore → run gerçek provider'la tamamlanır; evidence: iki attempt id + RecoveryProof + kill türü. (ENG-004 process-kill değişkeni deterministik tier'da her CI'da koşar.)

**Verify:** `make verify && make test-fault CASE=recovery && make test-fault CASE=runner && make test-live-provider PROVIDER=provider-one CASE=container-kill-recovery`.

### Task 6: Kill harness II — runner-daemon + whole-host + workspace restore/reclaim (ENG-006/007, SAN-005..008, REC-005)

**Files:** Create `tests/fault/recovery/host_kill_test.go` (runner-daemon stop + whole-host teardown), `adapters/sandboxes/oci/snapshot/restore.go` (snapshot restore: S3 → yeni allocation, checksum verify), `apps/control-plane/internal/execution/workspace_recovery.go` (host_lost tespiti → binding SM sürüşü → yeni allocation + restore + lease re-acquire), `storage/migrations/000016_host_quarantine.up.sql`/`.down.sql` (host/runner quarantine flag + **rider: `merge_records` parent_run_id index** — E09 T6 M3 devri); Modify `packages/coordinator/workspace.go` (host-move reclaim: logical id sabit, fence+1 allocation), `packages/coordinator/lease.go` (missed-heartbeat → attempt lost + workspace retain §26.8 adım 1-3), doctor (quarantine görünürlüğü).

Semantik (§26.8, §29.7-29.8, E09 T1 devri): missed heartbeat → stale fence'ten event kabulü DURUR → grace sonrası attempt lost → workspace RETAIN (lease/host reality reconcile edilene dek) → son durable checkpoint/snapshot/tool state incelenir → seviye seçilir (T4) → yeni attempt + `attempt.recovering`. **REC-005/ENG-006:** binding `leased→host_lost→recovering→ready(yeni allocation)` — logical workspace id SABİT, yeni allocation fence+1; snapshot RESTORE sonrası file/index/tree checksum'ları CREATE'tekiyle EŞİT (**SAN-005 restore yarısı** — create+checksum E09 T1'de kanıtlı). **SAN-006 host-kill yarısı:** fence-advance'i artık GERÇEK host-kill tetikler (E09 deterministik tetiklemişti); eski host'un yazma/snapshot denemesi DB'de reddedilir. **ENG-007:** dönen old host diagnostics yükleyebilir, authoritative frame'i fence reddeder. **SAN-007:** aynı host substrate'inde allocation reuse → önceki tenant'tan sıfır kalıntı (dosya/proses/credential), taze writable layer. **SAN-008:** destroy failure injection → host quarantine, yeni tenant placement DENY, doctor'da görünür. Whole-host local yaklaşımı DÜRÜSTÇE adlandırılır (runner daemon + tüm container'ları + host-path erişimi kesilir; çok-makine draini E14/E15).

- [ ] Failing testler: `TestRunnerDaemonKillAdvancesFenceAndRecovers` (ENG-006; gerçek daemon stop → heartbeat kaybı → yeni "host"ta placement + ladder evidence); `TestOldHostAuthoritativeFramesDeniedDiagnosticsAllowed` (ENG-007; eski fence'le frame → deny + audit; diagnostics yolu açık); `TestHostKillFencesStaleWriter` (SAN-006 fault-live yarısı); `TestSnapshotRestoreChecksumsMatchCreate` (SAN-005 restore; create-side checksum'la byte-byte eşitlik + exclusion'lar boş); `TestHostMoveKeepsLogicalIdNewFencedAllocation` (REC-005; SM host_lost→recovering→ready gerçek sürülür); `TestAllocationReuseLeavesNoTenantResidue` (SAN-007); `TestFailedDestroyQuarantinesHost` (SAN-008; injection → placement deny); migration idempotent + down (index rider dahil).
- [ ] Fail'i doğrula; minimum restore + recovery wiring + quarantine; `recovering→failed` yolu da test edilir (restore imkânsızsa explicit failure, sessiz düşme yok).
- [ ] **Fault-LIVE smoke:** gerçek provider run'ı gerçek workspace'te dosya yazar → checkpoint+snapshot → runner daemon + container'ları KILL (whole-host yaklaşımı) → yeni runner enroll → workspace restore (checksum eşit) → run devam edip tamamlanır; eski host'un stale frame denemesi denied. Evidence: fence çifti, restore checksum kanıtı, RecoveryProof.

**Verify:** `make verify && make test-fault CASE=recovery && make test-component && make test-live-provider PROVIDER=provider-one CASE=host-kill-restore`.

### Task 7: Tool replay classes + uncertain reconciliation + outage command ordering (TOL-001..004/016/017, ENG-012, SES-010 recovery)

**Files:** Create `storage/migrations/000017_tool_call_ledger.up.sql`/`.down.sql` (tool_calls: replay_class, request_hash, lease owner, external idempotency key, reconciliation state, commit boundary — §26.6 son paragraf), `apps/control-plane/internal/execution/tool_ledger.go` (execute-öncesi dedupe + replay decision), `apps/control-plane/internal/execution/reconcile.go` (uncertain reconciliation jobs: reconciled_completed / reconciled_not_applied / manual_resolution), `tests/fault/recovery/tool_replay_test.go`; Modify `packages/tool-broker/` (tool kayıtları replay_class DECLARE eder), `apps/control-plane/internal/execution/tool_dispatch.go` (ledger consult: kill sonrası pure→replay-etiketli, idempotent→aynı key resend, irreversible→uncertain STOP), `packages/state-machines/` (tool-call SM §26.7 — registry-sync + property, E02 kalıbı), contract schema (`tool_call.*` event'leri) + `make generate`.

Semantik (§26.6-26.7, §26.9-26.10, §53.6): her tool operasyonu SINIFLI; kill sonrası davranış sınıfa göre — pure: yeniden koş/cached, ETİKETLİ (TOL-001); idempotent: stable destination key ile resend, TEK external object (TOL-002); reversible: ÖNCE reconcile, sonra policy compensate/retry (TOL-004); irreversible: uncertain'e girer, ASLA auto-replay, manual_resolution yolu (TOL-003); interactive: client/approval yoksa sessiz replay YOK. `uncertain` sonucun reasoning'i etkileyeceği yerde continuation'ı BLOKLAR (§26.7 son cümle). **TOL-016 ledger yarısı:** duplicate/retry aynı `tool_call_id` → TEK execution (gerçek local HTTP server'a istek sayacı). **TOL-017 fence yarısı:** timeout sonrası GEÇ callback → aktif fence/reconciliation stale commit'i reddeder. **ENG-012:** outage boyunca kabul edilen queue/steer/interrupt (T2 durable rows) recovery sonrası kanonik sırada; interrupt yeni attempt BAŞLAMADAN cancellation intent'i işler (§26.9). **SES-010 recovery yarısı:** kill sırasında cancel → §26.10 adım 8-9: aktif external op'lar reconcile edilir, terminal `canceled` veya `failed_with_uncertain_side_effect`, çocuklar/tool'lar hesaplı, TEK terminal.

- [ ] Failing testler: `TestPureToolReplayLabeledNoDuplication` (TOL-001; kill→replay→result "replayed" etiketli, side-effect sayacı 1'de kalmaz-değil-PURE koşar ama semantic tek); `TestIdempotentToolSameKeySingleExternalObject` (TOL-002; faithful destination double'da tek obje); `TestIrreversibleUncertainNeverAutoReplays` (TOL-003; execute-sonrası-record-öncesi kill → uncertain → continuation bloklu → manual resolution yolu); `TestReversibleReconcilesThenCompensates` (TOL-004); `TestDuplicateToolCallIdSingleExecution` (TOL-016; gerçek local HTTP istek sayacı); `TestLateCallbackAfterFenceAdvanceDenied` (TOL-017); `TestOutageCommandsDeliverCanonicalOrderAfterRecovery` (ENG-012; kill sırasında queue+steer+interrupt → recovery sonrası sıra: interrupt-intent önce, steer safe-boundary, queue input-boundary); `TestCancelDuringKillReconcilesChildrenSingleTerminal` (SES-010 recovery); tool-call SM registry-sync + property.
- [ ] Fail'i doğrula; minimum ledger + reconcile jobs (supervised loop, T3 kalıbı); E09 push/PR reconciliation'ı (REP-007/008) DEĞİŞMEZ — bu ledger generic tool katmanıdır, publication kendi receipt'ini korur.
- [ ] **Fault-LIVE smoke:** gerçek provider forced side-effect tool (local gerçek HTTP destination) çağırır → execute-record arasında kill → reconcile job destination'ı sorgular → duplicate YOK, sonuç etiketli; irreversible varyantında run uncertain'de DURUR (auto-continue etmediği kanıtlanır). Duplicate-external-effect=0 exit-gate invariantının canlı çekirdeği budur.

**Verify:** `make verify && make test-component && make test-e2e && make test-fault CASE=recovery && make test-live-provider PROVIDER=provider-one CASE=tool-replay-reconcile`.

### Task 8: Parent-detached durable child + durable parent↔child conversation (DET-001/002)

**Files:** Create `apps/control-plane/internal/execution/detach.go` (release-parent: checkpoint + `run.waiting`; child devam; parent resume `run.restore` T4 yolundan), `storage/migrations/000018_detached_children.up.sql`/`.down.sql` (detach flag + child-conversation delivery state; E09 T7 child_message substrate'i üstüne); Modify `apps/control-plane/internal/execution/child_dispatch.go` (inline-bekleyen parent artık release EDİLEBİLİR; child.result parent resume'da typed teslim), `engines/reference/src/palai_engine/loop.py` (`run.waiting` emit — child'lar açıkken parent boundary'de release; restore'da bekleyen child.result fold), `packages/coordinator/commands.go` (send_message hedefi detached child run'ı olabilir — E08 T2 spine'ı yeniden kullanılır, yeni command kind YOK), contract schema-pin.

Semantik (master plan satır 431, §25.18-19): bugün ChildRun INLINE — parent engine child.result beklerken idle compute tutar. E10: parent safe boundary'de checkpoint alır (T1) + `run.waiting` ile compute release; child durable job olarak koşar; child terminal VEYA child-idle event'i parent'ı ladder-2'den (T4 `run.restore`) uyandırır, `child.result` typed girer. **DET-002 durable conversation:** child kendisi `waiting`'e düşebilir (uzun iş / mesaj bekliyor); parent (veya client) command spine `send_message`'ıyla child'a yazar (T2 durable delivered-message child için de geçerli); child→parent journal event'i parent'ın resume history'sine kanonik girer. Her şey durable/observable/resumable — context dolsa bile konuşma DB'de (E09 T7 REG kalıbının parent-child eşleniği). Cancel-propagation E08 T5 semantiği detached'te de KORUNUR (parent stop → child cancel); depth/fan-out sınırları DEĞİŞMEZ.

- [ ] Failing testler: `TestParentReleasesComputeWhileChildRuns` (DET-001; parent attempt kapanır — engine process YOK — child koşarken; child terminal → parent yeni attempt'te typed result); `TestDetachedChildIdleReceivesSpineMessage` (DET-002; child `waiting` → `send_message` → child yeni attempt'te mesajı fold'lar — T2 exactly-once burada da); `TestChildEventReachesParentResumeHistory` (child→parent journal event parent restore history'sinde kanonik sırada); `TestParentCancelPropagatesToDetachedChild` (E08 SUB-005'in detached varyantı); `TestDetachSurvivesParentAndChildKill` (fault: İKİSİ de kill → ladder ikisini de ayrı restore eder, konuşma intact); migration idempotent + down.
- [ ] Fail'i doğrula; minimum detach + delivery; inline yol DEFAULT KALIR (detach policy/explicit — release ancak checkpoint policy'si karşılanınca, §26.5 son cümle).
- [ ] **LIVE smoke:** gerçek provider parent, `agent` tool_call'la (E09 T7) child spawn eder → parent release (compute kanıtı: engine container YOK) → child gerçek provider'la devam → `send_message` detached child'a ulaşır → parent resume typed result + konuşma görür; iki farklı gerçek chatcmpl id + parent'ın release-window'unda sıfır provider çağrısı. Honest ceiling: spawn forced-tool'dur (E09 T7 disiplini); kanıt DETACH+DURABLE konuşmadır, modelin spontane delegasyonu değil.

**Verify:** `make verify && make test-e2e && (cd engines/reference && pytest) && make test-fault CASE=subagents && make test-live-provider PROVIDER=provider-one CASE=detached-child-conversation`.

### Task 9: Journey 63.2 kill+recovery DAHİL + UAT materialization + recovery-0.1.0 evidence → E10 kapanışı

**Files:** Create `tests/uat/recovery/` + `tests/uat/cases/ENG-*/case.yaml` + `TOL-001..004,016,017` + `SAN-005..008` + `SES-009..010` + `REC-*`/`DET-*` (bu dilimde kanıtlanan yarılar; isimler assert edileni söyler), yeni evidence release `recovery-0.1.0`; Modify `tests/e2e/coding/` journey harness'i (E09 T9 harness'ine adım 8-9 eklenir — greenfield değil), `tests/uat/evidence.go` (RecoveryProof kuralı T4'te girdi — burada release'e bağlanır), `Makefile` (`uat-recovery` hedefi + `test-fault CASE=recovery`; `uat-local-live`/`uat-interactive`/`uat-coding` DOKUNULMAZ).

- [ ] Failing testler (deterministic tier): `TestCodingJourneyWithKillRecoveryDeterministic` (63.2 TAM: adım 1-7 E09 harness + adım 8 process/container kill + adım 9 doğru tool/mesaj sırasıyla recovery + adım 10-11 changeset/push/PR — push/PR kill'den ÖNCE pending idiyse TAM BİR kez, duplicate external effect 0); her ENG/TOL/SAN/SES/REC/DET case'i kendi proof-class'ında `case.yaml` ile.
- [ ] Canlı journey (`live-provider` + `fault-live` + `external-receipt`, compose stack, gerçek OpenAI + gerçek Git destination; E09 T9 hygiene disiplini aynen): E09 canlı journey akışının ÜSTÜNE — file/shell tool loop'u ortasında **container kill** → ladder restore (RecoveryProof) → **queued+steered mesajlar kanonik sırada** teslim (T2/T7) → changeset + test evidence → **approved push + draft PR TAM BİR kez** (kill'e rağmen duplicate YOK — external receipt tekliği) → detached child konuşması (T8). Evidence manifest: RecoveryProof (level/attempt çifti/süre), kill türü + zamanı, kill-öncesi/sonrası provider request id'leri, snapshot restore checksum kanıtı, push/PR external receipt TEKLİĞİ, credential-absence scan (checkpoint/snapshot byte'ları DAHİL).
- [ ] Evidence verifier: `recovery-0.1.0` `VerifyRelease` + RecoveryProof kuralı; secret finding 0; **"continued" log'lu ama proof'suz hiçbir case PASS sayılmaz**.
- [ ] `make uat-recovery PROVIDER=provider-one` PASS; deterministik tier aynı journey'i fake adapter + hook'lu kill'le CI'da koşar, CANLI tier onsuz GEÇMEZ.

**Verify:** `make verify && make test-e2e && make test-fault && make uat-recovery PROVIDER=provider-one && make evidence-verify RELEASE=recovery-0.1.0`.

## 6. Final release check (E10 exit gate)

- [ ] `make verify`, `make test-component`, `make test-e2e`, `make test-fault` (recovery dahil TÜM CASE'ler), `make test-security` PASS.
- [ ] `make uat-recovery PROVIDER=provider-one` bütün bu-dilim case'leri PASS; hiçbir `fault-live` case simüle kill'le, hiçbir `live-provider` case fake ile geçmedi.
- [ ] `make evidence-verify RELEASE=recovery-0.1.0` PASS; secret finding 0 (checkpoint/snapshot byte taraması dahil).
- [ ] **Exit gate cümlesi kanıtlı (§63.2 + master plan §8/E10):** journey 63.2 kill+recovery DAHİL geçer; final repository SHA/diff doğru; recovery evidence complete (RecoveryProof'suz "resumed" kabul edilmedi); **duplicate external effect (tool/push/PR) = 0**.
- [ ] Regresyon yok: `make uat-local-live`, `make uat-interactive`, `make uat-coding` (PROVIDER=provider-one) hâlâ PASS — checkpoint/ladder/ledger değişiklikleri single-shot, interactive ve coding yollarını bozmadı.
- [ ] `git status --short` temiz; generated drift 0 (`make generate` sonrası diff yok — checkpoint_formats + recovery event'leri + tool_call şemaları pinli).
- [ ] **SH-1 bu kapı geçilince verilir** (master plan §8/E10 son cümle); öncesinde hiçbir sürüm "kurtarılabilir/durable execution" production iddiası taşımaz.

## 7. Bu plana girmeyen devirler (kayıt yeri: bu commit'li plan)

1. **Checkpoint/snapshot envelope-encryption-at-rest + GitHub App root key sealing (ev: E13, `phase-13-governance-data.md`; LP §7.1).** E10 checksum-integrity + size-bound + secret-absence kanıtlar; §26.2'nin "encrypted" sözü E13 SecretRef backend'i kapanmadan İDDİA EDİLMEZ.
2. **PKI / runner identity registry + revocation + host attestation (ev: E13/E14; E08 planı §7.1, LP §7.4-3).** E10 stale-fence denial'ı kanıtlar (ENG-007/SAN-006); cert-seviyesi revocation ve SAN-011 E13/E14'tür. Whole-host kill'in tek-makine yaklaşımı da E14 multi-host'ta gerçek fleet drain'e büyür.
3. **Remote tool SDK / signed HTTP / MCP (ev: E12, `phase-12-extensions.md`).** TOL-016/017'nin signed-transport + SDK yarısı E12'de; E10 ledger/fence yarısını kanıtladı — E12 aynı case ID'lerin kalan yarısını alır (shared-case disiplini, SAN-005/006 kalıbı).
4. **Warm-pool provisioning + microVM isolation + fleet (ev: E15; SAN-009).** SAN-007 burada reuse-hijyen invariantı; gerçek havuz + attested tier E15.
5. **Context compaction + compaction-sonrası checkpoint hook'u (ev: E17).** §26.5'in compaction maddesi compaction landığında bağlanır; E10 checkpoint frequency'nin kalan alt kümesini uygular.
6. **Schedules/triggers (ev: E11).** Reconciliation/GC loop'ları süpervizyonlu iç loop'lardır; kullanıcı-görünür scheduler E11.
7. **REC/DET case'lerinin spec §64 kataloğuna reconciliation'ı (ev: spec update).** E09 §7.7 kalıbı: ID'ler burada authored, katalog satırları sonraki spec revizyonunda (doc devri).
8. **E09 T6 MAJOR koşullu devri:** worktree.go:80-81 merge-abort fix'i E09 T7 STEP-0'da kapanmadıysa E10 T1 STEP-0'dur (§1 gate maddesi); kapandıysa bu satır kayıt olarak kalır. `merge_records` parent_run_id index'i (T6 M3) T6 migration rider'ı olarak BU planda kapandı.
9. **Recursive delegation depth>1 + per-project delegation config (ev: ileri E-series carve-out, E08/E09 kaydının aynısı).** Detach depth'i artırmaz.
10. **PİPELİNE NOTU:** Bu plan E09 T5/T7/T8/T9 koşarken yazıldı (regime: pipeline). Dispatch ÖNCESİ amend zorunlu alanları: §1 E09-merge SHA pini; T5/T7/T8/T9 bulgularının (production-tool-exec-topology kararı, T7 detach-öncesi child_message şekli, T8 approval/publication reconciliation detayları, T9 journey harness yüzeyi) T1/T7/T8/T9 brief'lerine yansıması; migration numaralarının fiili en-yüksek'e göre kayması.
