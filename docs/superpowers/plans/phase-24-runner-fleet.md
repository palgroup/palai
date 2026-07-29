# Palai Agent-Runner Fleet Plan (E24 — bir run'ın NEREDE koştuğu artık bir seçimdir)

> **For agentic workers:** REQUIRED SUB-SKILL: `superpowers:subagent-driven-development` (önerilen) veya
> `superpowers:executing-plans` ile task-by-task uygula. Adımlar `- [ ]` checkbox'lıdır. **Bu planın
> tanımlayıcı kuralı E19/E20/E21/E22/E23'ünkinin devamıdır: her external contract GERÇEK VENDOR
> DOKÜMANINDAN (ya da bu ağaçta ÖLÇÜLMÜŞ koddan) grounding alır** ve kaynak URL'i + çekim tarihi şartın
> YANINA yazılır (§3.5). **§3.6 ise ağacın KENDİ hakkındaki yanlış inançlarıdır** — E21'de on, E22'de on
> altı, E23'te on üçtü; burada **on altı**, ve **en pahalısı yönlendirmenin kendi D7'sinin cevabıdır:
> bir runner iş KOŞTURMUYOR, bir MOTOR koşturuyor. Her tool çağrısı control plane'in KENDİ process'inde
> çalışıyor.**

**Goal:** Owner'ın cümlesi: *"kiralanan Mac'lerden bir filo kurulsun, iş geldiğinde oraya düşsün, yük
bitince sönsün."* Bugün Palai'nin **tek** runner'ı var, o runner **motoru** koşturuyor, ve `xcodebuild`
control plane'in makinesinde çalışıyor. Bu epic o üçünün de değiştiği yerdir.

**BU PLANIN TAÇ KARARI — VE ÖNCE BİR DÜZELTME:**

> **HAVUZ BİR KUYRUKTUR VE BİR ETİKETTİR (D1 doğru). AMA BİR HAVUZ ÜYESİNİN İŞE YARAMASI İÇİN ÖNCE
> İCRANIN ORAYA TAŞINMASI GEREKİR — ÇÜNKÜ BUGÜN RUNNER İŞ KOŞTURMUYOR.** `orch.SetShellRunner(...)`
> `main.go:603`'te control plane'in process'ine bağlanır; `FileTool` `workspace.NewWorkspaceFS(env.WorkspaceRoot)`
> ile control plane'in **kendi diskine** yazar (`tools/file.go:48`). Runner'ın aldığı şey `lease.offer`
> ve içindeki `image_digest`'tir — yani **model döngüsü**. Ağaç bunu kendi kelimeleriyle yazmış
> (`main.go:591-595`): *"the tools run CP-side against the same host allocation the runner bind-mounts.
> A split CP≠runner deploy … needs a runner-relay seam … a NAMED FUTURE split-deploy hardening, not
> built here."* **Bu epic o seam'i açar, ve açmadan havuz kavramı anlamsızdır.**

**Kapsam sınırı — DÜRÜST TAVAN:**

- **(a) BU EPIC ÖLÇEKLEYİCİ (SCALER) YAZMAZ.** Havuzu, kimliği, yerleştirmeyi ve park etmeyi kurar;
  *kaç makine açılacağına karar veren* döngü **E26**'dır (§5). Gerekçe ölçüldü: bir makine açmadan önce
  bir run'ın **onu bekleyebilmesi** gerekir, ve bugün bekleyemiyor (§3.6 D12).
- **(b) BU EPIC BULUT SAĞLAYICI ENTEGRASYONU YAPMAZ.** D4 doğrudur ve tek bir **spawn seam**'i vardır;
  ama o seam'in ilk müşterisi `static` sağlayıcıdır (ofisteki makineler) ve seam'in kendisi **E26**'da
  yazılır. Core'a hiçbir bulut SDK'sı GİRMEZ, ve bu bir tercih değil bir §5 satırıdır.
- **(c) `apple-build` `disabled` KALIR.** E22 ve E23'ün gerekçesi aynen geçerlidir: hiçbir Palai
  deployment'ında imzalama materyali yok, ve `Catalog` tipsiz bir operasyonu reddediyor.
- **(d) BU EPIC `workers` PAKETİNİ MAC YOLU OLARAK KULLANMAZ, VE REDDİN ÜÇ BAĞIMSIZ ÖLÇÜMÜ VAR** (§3.6
  D14). Paket **dokunulmaz** kalır; E24 runner düzlemini büyütür, capability-worker düzlemini değil.
- **(e) TEK KİMLİK BİR SERTİFİKADIR, BİR ANAHTAR DEĞİLDİR.** Havuz anahtarı **yalnız** bir sertifika
  mintlemeye yarar; API anahtarı değildir, `Scope` taşımaz, `/v1` altında hiçbir şey açamaz.
- **(f) STRICT MODE VARSAYILAN OLARAK KAPALIDIR, VE ARTIK RİSKİ BU PLANDA ADIYLA YAZILIDIR** (§2, T6).
- **(g) MIGRATION VARDIR: 000045**, dört tablo + iki rider taşır (§1). Zincir başı bugün **000044**
  (`storage/migrations/000044_tool_approvals.up.sql`, sayıldı 2026-07-29).
- **(h) BU BİR EPIC'TEN BÜYÜKTÜR VE BÖLÜNMESİ ÖNERİLİYOR** (§4 sonu). E24 sekiz task'tır ve T7 (icra
  relay'i) tek başına bir task'ın bütçesini aşabilir; aşarsa bölünme noktası **adıyla** yazılıdır.

---

## §0 — Owner'ın sağlayacakları (HANDOVER CHECKLIST)

E19 §0.1, E20 §0, E21 §0, E22 §0 ve E23 §0 aynen geçerlidir. **E24 owner'dan ÜÇ şey ister ve
üçü de kod değil, karardır.**

### 0.1 Kaç Mac, ve kim için — çünkü bu bir güvenlik kararıdır

`known-gaps-1.0.md` `MAC-P6` birebir şunu diyor ve bu epic onu **değiştirmiyor**:

> **Different customers → different Macs (or different uids). Same customer → one Mac, per-session
> directories plus `simctl --set`.**

E24 havuzları **tenant kapsamlı** yapar (RLS), ama bir Mac'in içindeki iki run hâlâ aynı uid'dir.
**Owner beyan eder:** havuzlar tek müşterili mi (öyleyse tenant kapsamı yeterlidir), yoksa bir havuzda
birden çok müşteri mi koşacak (öyleyse `MAC-P6` kapanana kadar bu epic o havuzu **açmaz**).

### 0.2 Havuz anahtarı nerede gösterilir — ve cevap konsol DEĞİLDİR

Konsolda **hiçbir kimlik doğrulaması yok** (durum belgesi §4). Bu yüzden havuz anahtarı **CLI'dan**
mintlenir ve **stdout'a bir kez** basılır — `palai apikey create`'in shipped deseni
(`cmd/cli/internal/admin/admin.go:157,228`):

```sh
palai admin pool create --project <prj> --name mac-pool --posture unsandboxed-host --os darwin --arch arm64
palai admin pool key create --pool <pool_id>        # anahtarı BİR KEZ basar; DB'de yalnız sha256'sı durur
palai admin pool key revoke <key_id>
```

**Owner onayı gereken tek şey:** anahtarın varsayılan ömrü. Öneri **90 gün**, ve gerekçesi
`FileEnrollmentTokens`'ın bugünkü hâlidir — süresiz bir bootstrap credential'ı zaten var, E24 ona bir
son kullanma tarihi ekliyor.

### 0.3 Değişmeyen her şey

`PALAI_ENROLLMENT_TOKEN_FILE` / `PALAI_RUNNER_CA_*` / `PALAI_RUNNER_SERVER_*` / `PALAI_RUNNER_ID` /
`PALAI_CONTROLLER_URL` / `PALAI_CONTROLLER_DNS` / `PALAI_ENGINE_IMAGE` / `PALAI_RUNNER_CONCURRENCY` /
`PALAI_WORKSPACE_ROOT` / `PALAI_SANDBOX_IMAGE` / `PALAI_SHELL_NATIVE` — **hepsi aynen kalır.** Tek
kullanımlık dosya token'ı **silinmez**: o, kimliği süresi dolmuş bir runner'ın tek kurtuluş yoludur
(§3.6 D4) ve E24 onu bir havuz anahtarıyla DEĞİŞTİRMEZ, yanına koyar.

---

## §1 — Yapı kararı: fork noktası, migration, dosyalar

**Fork noktası:** `main` >= `2933055` (E23 tamamı ağaçtadır; `HIL-` prefix'i ve
`tool-approval-0.1.0` bundle'ı shipped).

**MIGRATION: VAR — `000045_runner_fleet`. Tek migration, tek task (T1).** Zincir başı bugün
**000044**'tür (sayıldı 2026-07-29). Paralel migration tuzağı yapısal olarak imkânsız kılınır:
`storage/migrations/` altına dosya koyan **tek** task T1'dir.

| # | Ne | Neden migration ZORUNLU |
|---|---|---|
| **R1** | **`runner_pools`** (YENİ tenant tablosu) — `id`, `organization_id`, `project_id`, `name`, `posture`, `os`, `arch`, `strict_enrollment BOOLEAN NOT NULL DEFAULT false`, `created_at`; `UNIQUE (organization_id, project_id, name)`; `CHECK (posture IN ('sandboxed-linux','unsandboxed-host'))` | Havuz kalıcı bir nesnedir; bir env var ya da `config_policy` alanı **olamaz**, çünkü bir runner ona **enroll** olur ve enrollment bir yabancıdan gelir |
| **R2** | **`runner_pool_keys`** (YENİ tenant tablosu) — `id`, `organization_id`, `project_id`, `pool_id`, `key_sha256 TEXT NOT NULL`, `key_prefix TEXT NOT NULL`, `created_at`, `expires_at`, `revoked_at`, `last_used_at`; `UNIQUE (key_sha256)` | Credential **hash'i** saklanır, değeri asla. `UNIQUE (key_sha256)` bir anahtarın iki havuza takılamamasıdır |
| **R3** | **`runners`** (YENİ tenant tablosu) — `id` (**sunucu mintler**), `organization_id`, `project_id`, `pool_id`, `runner_dns`, `public_key_sha256`, `state` (`CHECK IN ('pending','active','cordoned','revoked')`), `enrolled_via_key_id`, `os`, `arch`, `posture`, `capacity`, `cert_not_after`, `enrolled_at`, `last_seen_at` | `runner_gateway.go:73`'ün kendi cümlesi: *"there is no hosts/runners table in this tier … that is the SaaS/post-SH-0 upgrade path"*. Ve `local_credentials.go:122` aynı şeyi bağımsız olarak ikinci kez yazıyor |
| **R4** | **`runner_enrollments`** (YENİ, **APPEND-ONLY** journal) — `id`, `organization_id`, `project_id`, `runner_id`, `pool_id`, `key_id`, `entry_kind` (`CHECK IN ('requested','approved','refused','issued','revoked','renewed')`), `entry_seq`, `detail JSONB`, `created_at`; `UNIQUE (runner_id, entry_seq)` | `capability_jobs`'ın (000040) şekli AYNEN: `GRANT SELECT, INSERT` + **`REVOKE UPDATE, DELETE`**. Bir enrollment defteri, silinebiliyorsa defter değildir |
| **R5** | Rider: **`runs.pool_id TEXT NULL REFERENCES runner_pools(id)`** | Yerleştirme KARARI denetlenebilir ve bir resume **aynı havuza** döner. NULL = yerleştirme kararı yok (bugünkü her run) |
| **R6** | Rider: **`runner_pools` boot-seed** — bootstrap org/project için `pool_default`, `posture='sandboxed-linux'`, `strict_enrollment=false` | **Tek runner'lı deployment'ı bozmamanın yolu budur** (§2) |

**DÖRT YENİ TENANT TABLOSU ⇒ DÖRT KEZ AYNI DİSİPLİN** (mig 000029/000030 M3 kuralı, `000040`'ın deseni):
her biri **kendi** `CALL palai_apply_tenant_policy('<tablo>', 'organization_id', true)` satırını taşır
(000029'un boot sweep'i **bu boot'ta geç kalır**: 29 numarası 45'ten önce koşar, tablo henüz yoktur),
her biri **kendi** `GRANT`'ini alır (000029'un blanket grant'i de aynı sebeple geç kalır), her biri
`tests/component/postgres/migration_test.go:29` `allTables`'a eklenir, ve dördü de
`tests/security/tenancy` corpus'una **otomatik** girer — `TestEveryTenantTableIsRowLevelSecured`
(`tenancy_test.go:242-249`) `organization_id` taşıyan her tabloyu katalogdan bulup ENABLE+FORCE
arıyor, yani politikayı unutan tablo **sessizce kapsam dışı kalmıyor, kırmızı yakıyor.**

**MIGRATION İSTEMEYEN ve bu yüzden ayrıca yazılan şeyler:**

- **Yeniden kullanılabilir enrollment key migration İSTEMEZ — ÇÜNKÜ ZATEN VAR.** `FileEnrollmentTokens`
  **tek kullanımlık değildir** ve başlığı birebir *"WHY THIS IS NOT ONE-USE, AND WHAT REPLACED THAT"*
  (`local_credentials.go:97`). E24'ün eklediği şey yeniden kullanılabilirlik değil, **kapsam, hash,
  iptal ve kayıt**tır (§3.6 D4).
- **"Anahtarı iptal et, makineler çalışmaya devam etsin" migration İSTEMEZ ve BUGÜN DE DOĞRUDUR.**
  Yenileme `handleRenew` üzerinden **sertifikayla** kimlik doğruluyor (`runner_gateway.go:265-284`) ve
  `Consume` o yolda **hiç yok**. Anahtarı silmek yalnız *yeni* enrollment'ı ve *süresi dolmuş kimlik*
  kurtarma yolunu kapatır (§3.6 D5).
- **Run'ın kapasite için park etmesi migration İSTEMEZ.** `RunCmdWait` / `applyResumeTx` /
  `checkpointBeforePause` E08+E10'da var, ve **E23 T1 bunu dışarıdan uyandırma ile birlikte yeni
  kanıtladı** (`phase-23-tool-approval.md` T1). E24 aynı koreografiyi ikinci kez kullanır, yenisini
  yazmaz.
- **Havuz POLİTİKASI migration İSTEMEZ.** Bir ajanın hangi havuzu istediği `config_policy` JSONB'sinde
  yaşar (`PATCH /v1/projects/{id}`, `admin.go:203`) — E23 T2'nin `approvers` için verdiği kararın
  aynısı, aynı gerekçeyle.
- **Drain migration İSTEMEZ ve YENİDEN YAZILMAZ.** `RunnerGateway.Drain` (`runner_gateway.go:170-184`)
  E15 T2'nin işidir, `active atomic.Int64` üzerinde bekler ve E10 recovery katmanını **yeniden
  kullanır**. E24 onu runner id'ye **anahtarlar**, gövdesini değiştirmez.

**Files:** `storage/migrations/000045_runner_fleet.{up,down}.sql` (**YENİ**),
`storage/queries/runners.sql` (**YENİ**), `apps/control-plane/internal/fleet/` (**YENİ paket** —
`store.go`, `pools.go`, `keys.go`, `placement.go`),
`apps/control-plane/internal/execution/runner_gateway.go` (registry + havuz + tenant),
`apps/control-plane/internal/execution/local_credentials.go` (`PoolEnrollmentKeys`),
`apps/control-plane/internal/execution/engine_channel.go` (`AttemptDescriptor` += `Tenant`, `PoolID`),
`apps/control-plane/internal/execution/orchestrator.go` (yerleştirme + kapasite parkı),
`apps/control-plane/internal/execution/runner_shell.go` (**YENİ** — `ShellRunner` relay'i),
`apps/control-plane/internal/execution/runner_workspace.go` (**YENİ** — workspace relay'i),
`packages/runner/serve.go` + `packages/runner/exec.go` (**YENİ** — runner tarafı icra),
`packages/contracts/` (`controller.exec` / `runner.exec_result` frame tipleri),
`cmd/runner/main.go` (posture beyanı), `cmd/cli/internal/admin/admin.go` (`pool` komut ailesi),
`cmd/cli/internal/stack/up.go` (varsayılan havuz uyarısı),
`apps/control-plane/api/router.go` (`/v1/runner-pools`, `/v1/runners`),
`tests/uat/cases/FLT-001..006` (**YENİ**), `tests/uat/evidence_fleet.go` +
`promote_fleet.go` (**YENİ**), `tests/uat/fleet/` (**YENİ**), `scripts/test/component`,
`scripts/uat/fleet` (**YENİ**), `docs/operations/runner-fleet.md` (**YENİ**),
`docs/operations/known-gaps-1.0.md`.

**DOKUNULMAYANLAR:** `apps/control-plane/internal/workers/*` (**E22/E23'ün bit-değişmezliği sürer** —
gerekçe §3.6 D14), `adapters/integrations/slack/interactions.go`'nun AST taraması,
`packages/tool-broker/broker.go`'nun `ReplayClass`/`RequiresApproval` semantiği, E23'ün onay kapısı
(bir relay'lenmiş shell **de** onay kapısından geçer ve bu bir testtir).

---

## §2 — Design invariant (task değil, her task'ın kabul şartı)

- **TEK RUNNER'LI DEPLOYMENT BİT-DEĞİŞMEZDİR.** Havuz beyan etmeyen bir runner `pool_default`'a düşer;
  havuz politikası olmayan bir run `pool_default`'a yerleşir; sonuç bugünkü davranıştır.
  **RED-first: bugünkü compose stack'i, hiçbir havuz yapılandırması olmadan, aynı run'ı aynı şekilde
  koşturmazsa FAIL.** Bu pazarlık dışıdır ve bir yorumla değil bir testle durur.
- **YERLEŞTİRME BİR REDDİR, BİR TERCİH DEĞİLDİR.** `Dial` bir havuz ister ve **yalnız o havuzun**
  üyesine offer yapar. Yanlış havuzdaki bir runner'a lease **verilmez** — sıraya alınmaz, "en yakın"
  seçilmez. **RED-first: `unsandboxed-host` posture'ı isteyen bir attempt, `sandboxed-linux`
  posture'lı bir runner'a offer edilirse FAIL.**
- **TENANT RUNNER DÜZLEMİNE GİRER.** Bugün girmiyor (§3.6 D8): enrollment org/project taşımıyor,
  `AttemptDescriptor` taşımıyor, `leaseOffer` taşımıyor. **RED-first: A tenant'ının runner'ına B
  tenant'ının attempt'i offer edilirse FAIL.** Bu, `MAC-P6`'nın makine-başına kuralının **altındaki**
  katmandır, yerine geçen değil.
- **RUNNER ID'Sİ SUNUCU MINTLER.** Bugün enroll eden taraf kendi adını söylüyor ve gateway o adı
  imzalıyor (`runner_gateway.go:218-221,247`); compose ise adı sabitliyor
  (`runner-entrypoint.sh:10` — `runner-local`). **RED-first: iki makine aynı `runner_id`'yi talep
  ederse ikisi de kendi kimliğini alır ve ikisi de kayıtta ayrı satırdır.**
- **ANAHTAR YALNIZ ENROLL EDER.** Havuz anahtarı `Scope` taşımaz, `/v1` altında hiçbir şey açmaz, ve
  **yalnız kendi havuzuna** enroll eder. **RED-first: Mac havuzunun anahtarıyla Linux havuzuna enroll
  denemesi REDDEDİLİR; aynı anahtarla `/v1/*` çağrısı 401 alır.**
- **ANAHTAR HASH'LENİR, BİR KEZ GÖSTERİLİR, SABİT ZAMANDA KARŞILAŞTIRILIR.** DB'de `sha256` durur;
  değer yalnız mint anında stdout'a basılır; karşılaştırma `crypto/subtle.ConstantTimeCompare`'dır.
  Bugünkü hâl `strings.TrimSpace(string(raw)) != token` (`local_credentials.go:159`) — bir bearer
  credential'ın sabit-zamansız karşılaştırması. **Argv'de, log'da, evidence'ta, journal'da anahtar
  DEĞERİ YOKTUR**; `runner_enrollments` yalnız `key_id` yazar.
- **ANAHTARI İPTAL ETMEK ENROLL OLMUŞ MAKİNELERİ DURDURMAZ, VE BUNUN SEBEBİ YAPISALDIR:** yenileme
  sertifikayla kimlik doğrular, anahtarla değil (`handleRenew`). **RED-first: anahtar iptal edildikten
  sonra (a) yeni enroll REDDEDİLİR, (b) enroll olmuş runner'ın `renew`'ü BAŞARILI olur, (c) o
  runner'ın lease'i kesilmez.** Üçü ayrı ayrı.
- **BİR RUNNER'I İPTAL ETMEK KALICI OLMALIDIR.** Bugün `revoked atomic.Bool` process-içi ve **hiçbir
  production caller'ı yok** (§3.6 D15). E24 iptali `runners.state`'e yazar; gateway her connect'te
  okur. **RED-first: control plane restart'ından sonra iptal edilmiş bir runner yeniden bağlanabilirse
  FAIL.**
- **KAPASİTE YOKSA RUN PARK EDER, ÖLMEZ.** Bugün `Dial` 20 saniyede düşüyor ve run beş denemede
  dead-letter oluyor (§3.6 D12). **RED-first: hedef havuzunda hiç runner olmayan bir run,
  `dead_letter` olursa FAIL — `waiting` olmalı; ve o havuza bir runner bağlandığında UYANMALI.**
  Koreografi E23 T1'inkidir, yenisi yazılmaz.
- **İCRA RUNNER'A TAŞINIR, VE TAŞINIRKEN CREDENTIAL SINIRINI GEÇİRMEZ** (§24, `main.go:587`).
  Relay frame'i `argv` + workspace yolu + read-only bayrağını taşır; **credential taşımaz.**
  **RED-first: relay frame'lerinin baytları süpürülür ve içinde bir credential bulunursa FAIL** —
  süpürme JSON decode ederek yapılır (E20 T4'ün dersi).
- **ONAY KAPISI RELAY'İN ALTINDA DEĞİL, ÜSTÜNDEDİR.** E23'ün `approval_pending` dalı `dispatchTool`'da,
  yani control plane'de kalır. **RED-first: `approval_required` bir tool, relay üzerinden de insan
  kararı olmadan runner'a bir tek frame göndermez — runner'ın exec sayacı SIFIR.**
- **Kontrat dokümandan ya da AĞACIN ÖLÇÜMÜNDEN gelir.** Doğrulanamayan hiçbir şey koda VARSAYIM olarak
  girmez — §3.5'e **UNCONFIRMED** olarak girer.
- **Credential-gated live smoke: `//go:build live`, eksik env değişkeninin ADIYLA `t.Skip`.**
- **Yüzeye, credential'a, enrollment'a, yerleştirmeye ya da İCRA YOLUNA dokunan HER task full review
  alır: T1–T7.**

---

## §3 — Doğrulanmış seam envanteri (2026-07-29, ağaca karşı; HEAD `2933055`)

| Seam | Durum (doğrulandı) |
|---|---|
| **Runner gateway** | `execution/runner_gateway.go:52-84`. Üç route: `/v1/runner/{enroll,renew,connect}` (:210-216). Ayrı mTLS listener, `PALAI_RUNNER_LISTEN_ADDR` (`main.go:134,1444`) |
| **Enrollment isteği** | `enrollRequest{RunnerID, PublicKey}` (:218-221) — **org yok, project yok, havuz yok, posture yok.** Gateway `runnerDNS(request.RunnerID)` ile imzalar (:247) |
| **Enrollment token** | Tek üretim implementasyonu `FileEnrollmentTokens` (`local_credentials.go:93-169`). **Tek kullanımlık DEĞİL**; `minInterval` = sertifika TTL'i (varsayılan 5 dk), `lastIssued` **bellek içi** (:127) |
| **Yenileme** | `handleRenew` (:265-284) mTLS ile — **token yolda değil.** "Anahtarı iptal et, makineler çalışsın" özelliğinin yapısal kaynağı budur |
| **Bağlantı havuzu** | `available chan *pendingRunner` **buffersız** (:129). `handleConnect`'te runner sayısı guard'ı **YOK** ⇒ **N runner bugün de bağlanabiliyor** (:342-355) |
| **Lease teklifi** | `leaseOffer` (:564-586): `image_digest`, `limits`, ve varsa `workspace_host_path`/`workspace_read_only`/`workspace_unsafe`. **Tenant YOK, havuz YOK** |
| **Attempt tanımlayıcısı** | `AttemptDescriptor` (`engine_channel.go:13-33`): RunID, AttemptID, Fence, ImageDigest, Limits, Workspace*, JobID. **Tenant YOK.** Ama `tenant` çağrı yerinde ZATEN elde (`orchestrator.go:393` civarı, yerel değişken) ⇒ threading UCUZ |
| **Dial bütçesi** | `dialHandshakeDeadline = 20 * time.Second` (`orchestrator.go:38`), `context.WithTimeout` (:390). Retry: `MaxAttempts: 5, BaseBackoff: 100ms, MaxBackoff: 30s` (`main.go:477`) ⇒ **~2.5 dk'da dead-letter** |
| **Dispatch eşzamanlılığı** | `PALAI_DISPATCH_WORKERS` **varsayılan 1** (`main.go:472`; `production.yml:44`). Bir dispatch worker `ExecuteAttempt`'i run'ın TÜM ÖMRÜ boyunca tutar (:613-622) |
| **Cordon / Drain / Revoke** | `runner_gateway.go:146,154,170`. **Tek production caller `serveWithGracefulDrain` (SIGTERM) → `Drain` → `Cordon`** (`main.go:351,436`). `Revoke()` ve `Resume()`'un production caller'ı **YOK** |
| **Kimlik kaydı** | `identity atomic.Pointer[RunnerIdentity]` (:83) — **tek slot, son yazan kazanır.** `palai local doctor`'ın okuduğu şey budur |
| **Shell seam'i** | `toolbroker.ShellRunner` = **tek metot**: `Run(ctx, ShellCommand) (ShellResult, error)`; `ShellCommand{Argv, WorkspaceRoot, ReadOnly, Shell}` (`packages/tool-broker/sandbox_exec.go:56-67`). **Tamamen serileştirilebilir ⇒ relay'lenebilir** |
| **Posture çözümü** | `resolveShellPosture` (`main.go:740-753`) + `shellRunnerFromEnv` (:768-795), `main.go:71` ve `:603`'te **control plane process'inde**. `PALAI_SANDBOX_IMAGE` XOR `PALAI_SHELL_NATIVE=unsandboxed-host` |
| **Dosya tool'u** | `FileTool` → `workspace.NewWorkspaceFS(env.WorkspaceRoot)` (`execution/tools/file.go:48`) — **control plane'in diskinde** |
| **Paylaşılan workspace** | `orch.SetWorkspaceProvisioner(root, ...)`, `PALAI_WORKSPACE_ROOT` (`main.go:596-597`). Tavan `main.go:591-595`'te **adıyla yazılı** |
| **Mac deployment'ı** | `docs/operations/palai-on-a-mac.md:230,238-242`: *"only the control plane goes native"*, *"`--native` … selects **where the control plane runs** — nothing else"*. Runner container'da kalır |
| **Split-VM kanıtı** | `scripts/package/runner/splitvm-proof.sh:1-16` — **workspace'siz** bir run. `docs/operations/runner-host.md` workspace'ten hiç bahsetmiyor |
| **Park + uyandırma** | E23 T1'in koreografisi: `checkpointBeforePause` → `ApplyRunTransition(RunCmdWait)` → dışarıdan `applyResumeTx` (`waiting → running` + `EnqueueJob("response.run")`). **E24 bunu ikinci kez kullanır** |
| **Capability worker düzlemi** | `workers/*` + `capability_workers`/`capability_jobs` (mig 000040). Claim: `ReadyCapabilityJob` (`storage/queries/workers.sql:103-117`) |
| **Tenancy disiplini** | `palai_apply_tenant_policy` (mig `000029:45`), boot sweep (`000029:65-82`), `allTables` (`migration_test.go:29`), `TestEveryTenantTableIsRowLevelSecured` (`tenancy_test.go:242-249`) |
| **Append-only deseni** | `capability_jobs`: `GRANT SELECT, INSERT` + `REVOKE UPDATE, DELETE` (mig `000040` sonu). **R4 bunu AYNEN kopyalar** |
| **UAT** | `committedBundleSurfaces` (`evidence.go:2721`) **22 kayıt**, `evidence/releases/` **22 dizin** (ikisi de sayıldı 2026-07-29). Case prefix'leri: `A2A AGT API APV AUT CAS DEL DET DR ENG EXT HIL KNO LP MCI MOD OPS PER QUA REC REG REP SAN SEC SES SLK SUB TLM TOL UI WRK` — **`FLT-` boşta** |
| **Promote dispatch** | `PromoteGateFor` (`promote.go:66`) **E23'ü İLK** dispatch ediyor (`carriesE23ToolApprovalCase`) |

## §3.5 SAPMA TABLOSU — gerçek kontrat × varsayımlarımız

Her satır: **yayımlanmış kontrat** (kaynak + çekim tarihi) → **bizim varsayımımız / ağaçtaki durum** →
**hangi task kapatır**. **UNCONFIRMED satırlar koda VARSAYIM olarak GİRMEZ.**

| # | Gerçek kontrat | Varsayım / ağaçtaki durum | Task |
|---|---|---|---|
| **P1** | **⭐⭐ D1'İN KAYNAĞI, BİREBİR.** *"An environment worker is a process you run on your own infrastructure… The `self_hosted` environment acts as a work queue: when a session is assigned to it, Anthropic enqueues the session as a work item. Your worker claims work items from that queue, spawns an execution context for each one, downloads the agent's skills, runs the tool calls, and posts the results back."* (https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes, çekildi 2026-07-29) | **Environment İÇİNDE routing'den bahseden tek cümle yok** — sayfanın tamamı çekildi ve arandı. Yerleştirme primitifi environment'ın KENDİSİ. `WorkerSpec.PoolLabel` (`workers/types.go:59`) bunun runner düzlemindeki karşılığı DEĞİL (§3.6 D1) | **T2** |
| **P2** | *"Work items are claimed by polling the environment's queue: either by an **always-on worker** that polls continuously, or a **webhook-triggered handler** that wakes on `session.status_run_started` and starts polling."* (aynı kaynak) | **Bizim runner'ımız polling YAPMIYOR** — outbound WebSocket açıp **park ediyor** ve control plane ona `lease.offer` **push ediyor** (`runner_gateway.go:342-402`). Bu farklı ve **bizimki daha iyi**: kuyruk derinliğini poll aralığı belirlemiyor. **E24 push modelini KORUR**, poll'a geçmez; havuz seçimi push tarafında yapılır | **T2, T4** |
| **P3** | **⭐ İKİ CREDENTIAL, VE BİZİM D2'MİZİN KAYNAĞI.** *"**Two credentials:** an environment key (generated in the Console in the steps that follow) authenticates the worker to its queue; your Claude API key creates sessions and reads queue stats from outside the worker host. Key generation is Console-only."* Ve `export ANTHROPIC_ENVIRONMENT_KEY="sk-ant-oat01-..."` / `ANTHROPIC_ENVIRONMENT_ID="env_..."` (aynı kaynak) | **Ayrım aynen alınır ve BİZDE DAHA GÜÇLÜ olur:** onların environment key'i worker'ın **sürekli** kimliğidir; bizimki yalnız **bir sertifika mintler** ve sonra kullanılmaz. Yani sızmış bir environment key kuyruğa süresiz erişimdir; sızmış bir Palai havuz anahtarı **bir enrollment**tır ve iptal edilebilir. **"Key generation is Console-only" bizde CLI-only'dir** (§0.2), çünkü konsolda auth yok | **T3** |
| **P4** | *"Setting `ANTHROPIC_API_KEY` on the worker host **exposes an organization-scoped credential to agent tool calls**."* (aynı kaynak) | **Vendor'ın kendi uyarısı, bizim §2 invariant'ımızın aynısı.** Palai'de credential broker CP-side'dır (`main.go:587`) ve E24'ün relay'i bunu **bozamaz** — RED-first bir bayt süpürmesi olarak yazılır | **T7** |
| **P5** | **⭐ D4'ÜN KAYNAĞI.** *"Then write a spawn script that forwards session details into a fresh sandbox. The poller injects `ANTHROPIC_SESSION_ID`, `ANTHROPIC_WORK_ID`, `ANTHROPIC_ENVIRONMENT_ID`, and `ANTHROPIC_ENVIRONMENT_KEY` into the script's environment."* + platform entegrasyonları listesi: AWS Lambda MicroVMs, Blaxel, Cloudflare, Daytona, E2B, GKE Agent Sandbox, Modal, Namespace, Superserve, Vercel (aynı kaynak) | **On entegrasyonun hepsi TEK bir spawn seam'i.** Core'a hiçbiri girmiyor. **E24 bu seam'i AÇMIYOR — E26 açıyor** (§5), ve gerekçe (a)'da: spawn edilen makinenin gelmesini **bekleyebilen** bir run olmadan spawn anlamsızdır | **E26** |
| **P6** | *"**A Linux host** with `/bin/bash` at that exact path. The worker's bash tool invokes it directly, without consulting `PATH`."* (aynı kaynak) | **Rakibin self-hosted worker'ı Linux-only, kendi dokümanıyla.** Owner'ın ürün tezi (*"bir Mac'te Mac ürünleri"*) tam olarak rakibin yapmadığı şey. **Ama bu farklılaştırıcı bugün Palai'de de YOK** — bir Mac runner Mac tool'u koşturmuyor (§3.6 D9). **T7 farklılaştırıcıyı gerçek yapan task'tır** | **T7** |
| **P7** | *"Use `work.stop` to ask the worker handling a specific session to shut it down. By default the work item moves to `stopping`: **the worker notices on its next lease heartbeat**, cancels the session's in-flight tool call, and confirms the shutdown, at which point the work item becomes `stopped`. Pass `force: true` … to mark the work item `stopped` immediately."* (aynı kaynak) | **İki aşamalı durdurma — nazik + zorla — ve bizde tam karşılığı var:** `Cordon` (nazik) / `Revoke` (zorla), `runner_gateway.go:143-157`. **Ama ikisinin de production caller'ı yok** (§3.6 D15). Vendor'ın `stopping → stopped` ayrımı T5'in state makinesinin şeklidir | **T5** |
| **P8** | *"`reclaim_older_than_ms`: re-claim work items that were claimed but never acknowledged within this many milliseconds."* — SDK örneklerinde **2000** (aynı kaynak) | **Bizde karşılığı `RedispatchForRetry` + lease fence** (`workers/store.go`), ama **çağıranı yok** (`known-gaps` `WRK-2`). Runner düzleminde karşılığı `active` sayacı + E10 recovery. **E24 yeni bir reclaim yazmaz**, runner id'ye anahtarlar | **T5** |
| **P9** | **⭐ 24 SAAT APPLE'IN ŞARTIDIR, SATICI TERCİHİ DEĞİL — birebir:** *"Billing is per second, with a **24-hour minimum allocation period for the Dedicated Host to comply with the Apple macOS Software License Agreement**."* ve *"At the end of the 24-hour minimum allocation period, the host can be released at any time with no further commitment."* (https://aws.amazon.com/ec2/instance-types/mac/faqs/, çekildi 2026-07-29) | **D5 doğrulandı ve gerekçesi lisans.** Mac havuzunun bir **tabanı** ve saatlerle ölçülen bir sönme süresi olur. **Ama bu E26'nın problemi** — E24 yalnız havuzun `min_size`'ını **taşır**, kullanan yoktur | **E26**, §5 |
| **P10** | **⭐ D5'İN İÇİNDE ÖLÇÜLMÜŞ SÜRPRİZ, VE YÖNLENDİRMENİN "saniyeler / ~1 dakika" TABLOSUNU DÜZELTİYOR:** *"For an AWS vended AMI with a x86 Mac instance or a Apple silicon Mac instance, **the launch time can range from approximately 6 minutes to 20 minutes**."* Ayrıca: *"Mac instances are available only as bare metal instances on Dedicated Hosts, with a **minimum allocation period of 24 hours before you can release the Dedicated Host**. You can launch one Mac instance per Dedicated Host."* ve *"The **unit of billing is the dedicated host**. The instances running on that host have no additional charge."* (https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-mac-instances.html, çekildi 2026-07-29) | **AÇILIŞ SÜRESİ 20 SANİYELİK DIAL BÜTÇESİNİN 18–60 KATI** (`orchestrator.go:38`). Retry ladder'ıyla birlikte ~2.5 dakikada dead-letter (§3.6 D12). **Yani "yük gelince Mac aç" bugün YAPISAL OLARAK ULAŞILAMAZ, ekonomik olarak değil.** Bu satır T4'ün (kapasite parkı) var olma gerekçesidir | **T4** |
| **P11** | **Scaleway API bir ALAN olarak veriyor:** `"deletable_at": "2022-03-22T12:34:56.123456Z"`, ve *"Apple silicon-as-a-Service comes with a minimum allocation period of 24 hours."* (https://www.scaleway.com/en/developers/api/apple-silicon, çekildi 2026-07-29) | **Taban bir TIMESTAMP olarak okunabiliyor, hesaplanması gerekmiyor.** Bir scaler `now + 24h` hesaplamak yerine `deletable_at` okumalıdır — çünkü saat kayması ve sağlayıcı farkı hesabı bozar. **E26'nın ilk satırı bu olur** | **E26** |
| **P12** | **UNCONFIRMED:** Scaleway'in "bir dakikanın altında başlatma" iddiası ve 24 saatte otomatik silme seçeneği — FAQ sayfası (https://www.scaleway.com/en/docs/apple-silicon/faq/, çekildi 2026-07-29) **yalnız navigasyon döndürdü**, `how-to/delete-mac-mini` de öyle | **Koda VARSAYIM olarak GİRMEZ.** E24 hiçbir açılış süresi sabiti taşımaz; T4'ün parkı **süresiz**dir ve bir süre sabitine dayanmaz. Ölçüm §6'dadır | **§6** |
| **P13** | **UNCONFIRMED:** Anthropic'in `Environments Work` endpoint'lerinin **eşzamanlılık/kota** semantiği — bir environment'ın kaç work item'ı aynı anda claim edilebilir, ve claim'lerin sırası FIFO mu | **Koda VARSAYIM olarak GİRMEZ.** E24 kendi sırasını **açıkça** seçer ve yazar (T2: havuz içinde `created_at` FIFO), rakibin varsayılan davranışını taklit etmez | **T2**, §6 |
| **P14** | **DEVRALINAN (E22 §3.5):** macOS'ta `simctl --set` bir **argv** bayrağıdır ve argv modele aittir; per-session device set **tavsiyedir, zorlama değil** (`docs/research/macos-isolation-without-accounts.md` §6) | **E24 bunu DEĞİŞTİRMİYOR ve değiştirdiğini iddia etmiyor.** Bir Mac havuzu tenant kapsamlıdır (RLS), ama bir Mac'in **içi** hâlâ tek uid'dir. `MAC-P6` açık kalır ve §0.1 owner'a sorar | §0.1, §6 |

## §3.6 AĞACIN KENDİ SAPMALARI

**On altı satır.** İkisi yönlendirmenin premise'ini tersine çeviriyor, biri epic'in tamamının şeklini
belirliyor, ve **dördü aynı yanlış cümlenin dört kopyasıdır.**

| # | Taşınan inanç | Ağaçtaki gerçek (file:line ile) | Sonuç |
|---|---|---|---|
| **D1** | **Yönlendirmenin D1'i:** *"`WorkerSpec.PoolLabel` zaten var ve hiçbir kararda kullanılmıyor; `OS`, `Arch`, `Capacity` de aynı şekilde ölü."* | **DÖRDÜ DE ÖLÜ, AMA YANLIŞ DÜZLEMDE — VE BU AYRIM EPIC'İN ŞEKLİNİ DEĞİŞTİRİYOR.** `WorkerSpec` (`workers/types.go:52-61`) **capability-worker** düzlemine aittir; `capability_workers` tablosunda `os`, `arch`, `capacity`, `pool_label` kolonları gerçekten var (mig `000040`) ve claim predikatı onları hiç görmüyor (`storage/queries/workers.sql:111`: `WHERE organization_id = $1 AND project_id = $2 AND capability = $3`); indeks bile onları taşımıyor (`capability_workers_capability_idx`, mig `000040:56-57` — **ve o indeks de ölü**: hiçbir sorgu worker'ı capability'ye göre seçmiyor, yalnız `WHERE id = $1`, `workers.sql:18`). **`Capacity` ise ölüden beter — bir sınır GİBİ duruyor ve değil:** `ClaimNext` hiçbir şey saymıyor (`store.go:175-229`, tek dal `worker.Health != "healthy"` :180) ve `handleClaim` `sess.claims`'e kardinalite kontrolü olmadan ekliyor (`gateway.go:190`), yani **`capacity: 1` beyan etmiş bir worker N lease tutabilir.** **AMA AJANLARINIZI KOŞTURAN DÜZLEM O DEĞİL.** Run'lar runner düzleminden geçer ve orada bu alanlar **ölü değil, YOK**: `AttemptDescriptor` (`engine_channel.go:13-33`) ne label ne os ne arch taşır, `enrollRequest` (`runner_gateway.go:218-221`) ne posture ne havuz. **Yani "PoolLabel'ı yük taşır hale getir" yanlış düzlemin ölü alanını diriltirdi ve tek bir run'ı bile yönlendirmezdi** | **T2**: havuz runner düzleminde YENİDEN kurulur; `workers` paketi **dokunulmaz** |
| **D2** | *"Havuz içinde claim zaten FIFO; E24 sadece havuzu ekliyor."* | **FIFO DEĞİL.** `ReadyCapabilityJob` (`storage/queries/workers.sql:116-117`) birebir `ORDER BY latest.job_id` + `LIMIT 1` — **opak, rastgele bir id üzerinde sözlük sırası.** `job_id` `mintID("cjob")` → `middleware.NewID` (`api/middleware/request_context.go:40-44`) = prefix + `crypto/rand` 16 bayt hex, yani **monoton değil** ⇒ en eski iş değil, **hex'i en küçük** iş kazanır ve **her poll'da yeniden kazanır: bu bir sıra değil, bir açlık (starvation).** Sıralanabilecek iki kolon tabloda ZATEN var — `created_at` (mig `000040:98`) ve `entry_seq` (`:80`) — **ikisi de kuyruk sırası için hiç kullanılmıyor.** **FIFO bir korunan davranış değil, YENİ bir davranıştır** ve T2 onu açıkça seçip yazmak zorundadır | **T2** |
| **D3** | *"Claim bir kuyruktur."* | **BİR POLL'DUR VE KAYBEDENİ VARDIR.** Sorguda `FOR UPDATE SKIP LOCKED` **yok**; at-most-once garantisi append anındaki fence predikatı (`workers.sql:65-66`) + `UNIQUE (job_id, entry_seq)` çakışmasından (mig `000040:102`) geliyor. **Ve kaybedene ne söylendiği daha kötü:** çakışma `errFenceGuardMiss`'e (`store.go:510-512`) dönüyor ve orada **"claim yok"a yutuluyor** (`store.go:213-215`) — yani kaybeden poller *"iş var, sen kaptırdın"* değil *"iş yok"* duyuyor ve geri çekiliyor. N poller ⇒ iş başına N-1 kaybeden, ve hepsi yanlış bilgilendirilmiş. Sorgunun **kendi ponytail notu** tavanı yazmış (`workers.sql:99-101`): *"DISTINCT ON scans the capability's journal per claim — fine at fixture/reference scale; a ready-jobs materialized view or a status column is the upgrade **if a real fleet polls hard**."* **E24 tam olarak o filodur** — ve bu, T2'nin runner düzleminde **push** modelini korumasının (P2) ikinci gerekçesidir | **T2** (reddedilen seam) |
| **D4** | **⭐ Yönlendirmenin D2'si:** *"Bugün `EnrollmentTokens.Consume` tek kullanımlık — güvenli ama filo ölçeğinde makine başına token mintlemek gerekir."* | **PREMISE TERS. AĞAÇ ZATEN YENİDEN KULLANILABİLİR BİR ANAHTARA GEÇMİŞ, VE BUNU DÖRT YERDE İNKÂR EDİYOR.** Tek üretim implementasyonu `FileEnrollmentTokens`'ın başlığı birebir: ***"WHY THIS IS NOT ONE-USE, AND WHAT REPLACED THAT"*** (`local_credentials.go:97`) — bir sertifika ömrü başına bir mint (`minInterval`, :163-166), süresi dolmuş kimliğin tek kurtuluş yolu. **Ama "tek kullanımlık" cümlesi ağaçta DÖRT KEZ yazılı:** interface yorumu (`runner_gateway.go:40-44`, *"an unknown or already-spent token"*), CLI (`cmd/cli/internal/stack/lifecycle.go:118`, *"mints a fresh **one-use** runner enrollment token"*), runner'ın kendisi (`cmd/runner/main.go:68-69`, *"the one-use bootstrap token is spent once … and never presented again"* — **yirmi satır altında yeniden-enroll fonksiyonunu kuran dosya**), ve compose yorumu (`compose.yaml:168-171`, **tek doğru olan** — *"re-presented ONLY if the runner's identity expires … the control plane rate-limits it to one certificate per issued lifetime"*). **E24'ün eklediği şey yeniden kullanılabilirlik DEĞİL: kapsam, hash, son kullanma, iptal ve KAYIT** | **T3.** Dört kopyanın üçü düzeltilir |
| **D5** | **Yönlendirmenin sorusu:** *"Anahtarı iptal etmek enroll olmuş makineleri çalışır bırakmalı — bu özellik nasıl garanti edilir?"* | **ZATEN GARANTİ, VE BEDAVA — sebep yapısal.** Yenileme `handleRenew` (`runner_gateway.go:265-284`) **mevcut mTLS kimliğiyle** doğrulanır ve `tokens.Consume` o yolda **hiç çağrılmaz**; sadece `handleEnroll` (:233) çağırır. Token dosyasını silmek yalnız (a) yeni enrollment'ı ve (b) *süresi dolmuş kimlik* kurtarmasını kapatır. **Yani özellik VAR; olmayan şey, hangi anahtarın hangi sertifikayı mintlediğini KAYDEDEN yerdir** — bu yüzden iptal bugün **hedeflenemez**, yalnız topyekûndur. R2'nin `enrolled_via_key_id`'si tam olarak o eksik kayıttır | **T3** |
| **D6** | *"`minInterval` sızmış bir anahtarı sınırlar."* | **HACMİ SINIRLAR, KİMLİĞİ DEĞİL — VE BELLEKTE YAŞAR.** `lastIssued map[string]time.Time` (`local_credentials.go:127`) process içidir: **bir control-plane restart'ı sayacı sıfırlar**, ve `restart: always` ile bir VM reboot'u bunu düzenli olarak yapar. Yorumun kendisi de dürüst: *"Be honest about what minInterval is: a RATE LIMIT, not an exclusion"* (:117). Ayrıca karşılaştırma `strings.TrimSpace(string(raw)) != token` (:159) — **bir bearer credential'ın sabit-zamansız karşılaştırması**, ve `crypto/subtle` ağacın başka yerlerinde zaten kullanılıyor | **T3** |
| **D7** | *"`runner_id` bir makineyi tanımlar."* | **KENDİ BEYANIDIR, VE COMPOSE ONU SABİTLEMİŞ.** `enrollRequest{RunnerID, PublicKey}` (`runner_gateway.go:218-221`) — gateway hangi ad istenirse onu imzalıyor (`runnerDNS(request.RunnerID)`, :247), doğrulama yok. Ve `deploy/compose/runner-entrypoint.sh:10` birebir: `export PALAI_RUNNER_ID="runner-local"`. **`docker compose up --scale runner=3` üç makineye tek bir ad verir**, üçü de `runner-local.runners.palai.internal` için sertifika alır. Bir filoda "runner X'i iptal et" ancak X'i **sunucu** mintlerse anlamlıdır | **T1 + T3** |
| **D8** | **⭐ Yönlendirmenin kısıtı:** *"RLS tenancy holds everywhere; a pool and its enrollment key are tenant-scoped."* | **DB SATIRLARINDA HOLD EDİYOR; RUNNER DÜZLEMİNDE TENANT KAVRAMI HİÇ YOK.** Enrollment org/project taşımıyor (:218-221); `AttemptDescriptor` taşımıyor (`engine_channel.go:13-33`); `leaseOffer` `image_digest` + `limits` + workspace yolundan başka bir şey taşımıyor (`runner_gateway.go:564-586`); `Dial` hiçbir tenant kontrolü yapmıyor (:368-416). **Sonuç, açıkça söylenmesi gereken hâliyle: bugün enroll olmuş HERHANGİ bir runner, HERHANGİ bir tenant'ın attempt'ini alabilir.** Tek runner'lı topolojide bu bir bulgu değil bir tanımdır; **iki müşterinin Mac'i olduğu anda bir açıktır.** İyi haber: `tenant` çağrı yerinde zaten yerel bir değişken (`orchestrator.go:393` civarı), yani threading ucuz | **T3** (kısıt), **T4** (yerleştirme) |
| **D9** | **⭐⭐ Yönlendirmenin D7'si:** *"Bir run'ın nerede koştuğu boot-time env'den config'e taşınmalı — posture per-pool olabilir mi?"* | **SORU DAHA DERİN BİR ŞEYİ AÇIYOR: BİR RUNNER TOOL KOŞTURMUYOR.** Runner'ın aldığı `lease.offer` `image_digest` taşır — yani **motor**, model döngüsü. Her tool control plane'in process'inde koşar: shell `orch.SetShellRunner(shellRunnerFromEnv())` ile (`main.go:603`, `:768-795`), dosya tool'u `workspace.NewWorkspaceFS(env.WorkspaceRoot)` ile (`execution/tools/file.go:48`), ve ikisi de CP ile runner'ın **paylaştığı** `PALAI_WORKSPACE_ROOT`'a bakar (`main.go:596`). **Ağaç bunu kendi kelimeleriyle yazmış** (`main.go:591-595`): *"the tools run CP-side against the same host allocation the runner bind-mounts. A split CP≠runner deploy … needs a runner-relay seam — the CP-side tool dispatch would ship the file/shell op to the runner that holds the mount — a NAMED FUTURE split-deploy hardening, not built here."* **Ve bugünkü Mac deployment'ı bununla TUTARLI:** `palai-on-a-mac.md:238-242` birebir *"`--native` … selects **where the control plane runs** — nothing else"*, `:230` *"only the control plane goes native"*. **Yani posture'ın CP process'inde çözülmesi bir kaza değil, bugünkü mimarinin doğru ifadesidir.** Cevap §4'ün başındadır | **T7** |
| **D10** | *"Split-VM bacağı off-host bir runner'ı kanıtlıyor."* | **WORKSPACE'SİZ BİR RUN İÇİN KANITLIYOR.** `scripts/package/runner/splitvm-proof.sh:1-16` adım adım: *"Create a response over the API and poll it to `completed`"* — klon yok, workspace yok, shell yok. `docs/operations/runner-host.md` **workspace kelimesini hiç geçirmiyor** (grep, 2026-07-29). Workspace'li bir run runner'a `workspace_host_path` verir — **control plane'in host'undaki mutlak bir yol** — ve off-host runner onu **kendi** dosya sisteminden bind etmeye çalışır. **Yani shipped off-host topoloji bir coding run'ı koşturamaz; ki bir Mac'in var olma sebebi tam olarak odur** | **T7** |
| **D11** | *"Daha çok runner ekle, daha çok ajan koşsun."* | **KOŞMAZ.** Eşzamanlılık `min(PALAI_DISPATCH_WORKERS, Σ lease slot)`'tur ve **ilki varsayılan 1**: `workers := envIntDefault("PALAI_DISPATCH_WORKERS", 1)` (`main.go:472`), `production.yml:44` `${PALAI_DISPATCH_WORKERS:-1}`, `compose.yaml:82` `${PALAI_DISPATCH_WORKERS:-0}`. Bir dispatch worker `ExecuteAttempt`'i run'ın **tüm ömrü** boyunca tutar (`main.go:613-622`). **İkinci runner, kimsenin ulaşamayacağı park etmiş bir slot ekler.** Ve `E21`'de bulunan `PALAI_RUNNER_CONCURRENCY` boşluğu **kapanmış**: `compose.yaml:179` bugün `${PALAI_RUNNER_CONCURRENCY:-1}` — yani o hafıza artık geçersiz, bu satır onu da düzeltiyor | **T4** |
| **D12** | **⭐ Yönlendirmenin D5'i:** *"Mac 24 saatlik faturayla açılır; teknik olarak mümkün, ekonomik olarak bir gün satın almaktır."* | **EKONOMİDEN ÖNCE YAPISAL BİR DUVAR VAR, VE ÖLÇÜLDÜ.** `Dial` `dialHandshakeDeadline = 20 * time.Second` ile sınırlı (`orchestrator.go:38,390`), retry `MaxAttempts: 5, MaxBackoff: 30s` (`main.go:477`) ⇒ **~2.5 dakikada dead-letter.** AWS kendi dokümanında Mac açılışını *"approximately 6 minutes to 20 minutes"* diyor (P10). **Run, Mac boot etmeden dört kez ölür.** Yani "yük gelince Mac aç" bugün ekonomik bir tercih değil, **ulaşılamaz bir davranış**. Çözüm bir timeout büyütmesi DEĞİL — bir run'ın **park etmesi**, ve o koreografi E23 T1'de yeni yazıldı | **T4** |
| **D13** | *"Gateway tek-runner'lıdır."* | **N RUNNER'I BUGÜN DE KABUL EDİYOR.** `handleConnect`'te sayı guard'ı yok ve `available` buffersız bir kanal (`runner_gateway.go:129,342-355`): her park eden runner kendini gönderir, her `Dial` birini alır. **Tek-runner olan şey ikisi:** (a) `cordoned`/`revoked` **process-global `atomic.Bool`** (:75-76), (b) `identity atomic.Pointer[RunnerIdentity]` (:83) — **tek slot, son yazan kazanır**, yani iki runner varken `palai local doctor` en son sertifika sunanı okur ve diğeri hakkında hiçbir şey bilmez. Gateway'in kendi yorumu (:72-74) registry'yi upgrade path olarak adlandırıyor, ve **`local_credentials.go:122` aynı şeyi bağımsız olarak ikinci kez söylüyor**: *"Bounding concurrent identities per token needs a runner registry the single-runner SH-0 topology does not have; that is the upgrade path."* İki dosya, aynı eksik, hiç konuşmamışlar | **T1** |
| **D14** | *"Mac havuzu için `workers` düzlemi (capability workers) doğru ev."* | **ÜÇ BAĞIMSIZ RED, VE HER BİRİ TEK BAŞINA YETERLİ.** (1) **Listener non-loopback bind'ı REDDEDİYOR:** `listenCapabilityWorker` (`main.go:1589-1608`) `0.0.0.0`'ı, routable adresi ve isimle verileni bind'dan ÖNCE reddediyor — çünkü cleartext, ve üzerinde enrollment token'ı, her istekteki workload bearer'ı ve **redeem edilmiş secret DEĞERİ** taşınıyor. Kiralanan bir Mac tanımı gereği loopback değildir. (2) **Düzlem üç şekilde uykuda:** token mintleyen yok, `DispatchJob` çağıran yok, health/reaper yok — `known-gaps-1.0.md` `WRK-2`, **E19 T8 tarafından 2026-07-26'da yeniden doğrulandı ve hâlâ açık**. (3) **Yapısal olarak tipli-operasyon:** `ErrUntypedOperation` (`workers/types.go:128-130`) genel bir shell'i imkânsız kılıyor — bir Mac'e verilecek şey ise tam olarak genel bir shell'dir. (4) **Enrollment'ı BELLEKTE ve gerçekten tek kullanımlık:** `Gateway.enrollment map[string]enrollGrant` (`workers/gateway.go:33`), `delete(g.enrollment, token)` ilk kullanımda (:132) — yani bir control-plane restart'ı **her bekleyen enrollment'ı siler** ve `IssueEnrollmentToken`'ın (:67-70) zaten operatör caller'ı yok. **Bu düzlemi Mac yoluna çevirmek relay'den DAHA ÇOK iş olurdu ve DAHA AZ verirdi** | **§5** (adlandırılmış ret) |
| **D15** | **Yönlendirmenin D6'sı:** *"Cordon/drain/revoke bugün whole-gateway `atomic.Bool`; runner id'ye anahtarla."* | **DOĞRU, VE EKSİK YARISI DAHA KÖTÜ: ÜÇÜNÜN TEK PRODUCTION GİRİŞİ SIGTERM.** Ağaç genelinde `.Revoke()` ve `.Resume()`'un **hiçbir production caller'ı yok**; `.Cordon()`'un tek çağıranı `Drain`'in kendi ilk satırı (`runner_gateway.go:171`); `.Drain(` tek çağıranı `serveWithGracefulDrain` (`main.go:351,436`), o da SIGTERM'de. **Yani `Revoke` — SAN-011'in "hard stop"u, ele geçirilmiş bir runner için yazılmış olan — testlerle kanıtlı ve kimse tarafından ulaşılamaz.** Üstüne `revoked` bellek içi bir `atomic.Bool`: **bir restart iptali siler.** Bir filoda "şu Mac'i devre dışı bırak" bir CLI komutu ve **kalıcı bir satır** ister | **T5** |
| **D16** | *"Bir migration'ın numarası dosya adındadır, dolayısıyla tektir."* | **EN AZ BİR KEZ DEĞİLDİ, VE T1 AYNI TUZAĞIN ÖNÜNDEN GEÇİYOR.** Dosya `storage/migrations/000040_capability_workers.up.sql`, ama **kendi başlığı 000039 diyor** (`:1`, ve `:15-20` renumber talimatını geçmiş zamanla anlatıyor), `store.go:17` 000039 diyor, `workers.sql:4` 000039 diyor, ve testin **adı** `TestMigration39CapabilityWorkerTables` (`workers_component_test.go:181`). Yalnız dosya adı, `VALUES (40)` ve embed değişkeni taşındı. **`git mv` eski içeriği stage'ler ve sonraki Edit'ler düşer** — ağacın kendi hafızasındaki desen. **T1 tek migration sahibidir ve 000045'i açarken numarayı DOSYA ADINDA, BAŞLIKTA, `VALUES`'ta, embed'de, `store.go` yorumunda ve TEST ADINDA aynı commit'te taşımak zorundadır**; doğrulama `git show HEAD` üzerinden yapılır, working-tree grep'i üzerinden değil | **T1** |

---

## §4 — Task breakdown

### D9'un cevabı, ve bu epic'in şekli

**Yönlendirmenin en çok doğru cevap istediği soru buydu, cevap şudur:**

> **Posture bugün per-pool OLAMAZ, ve sebebi "boot'ta okunuyor" değildir. Sebep, runner'ın tool
> koşturmuyor olmasıdır.** Posture (`PALAI_SANDBOX_IMAGE` XOR `PALAI_SHELL_NATIVE`) *doğru yerde*
> yaşıyor: tool'ları çalıştıran process'te. Bir "Mac havuzu" bugünkü anlamda kurulsa, o havuza düşen
> run **model döngüsünü** bir Mac'te koşturur ve `xcodebuild`'i hâlâ control plane'in makinesinde
> çalıştırır — yani hiçbir şey kazandırmaz. **Bu yüzden posture zorunlu olarak runner/worker tarafına
> aittir, ama ancak icra oraya taşındıktan SONRA.** Taşınmadan önce "per-pool posture" eksik bir
> özellik değil, **anlamsız bir cümledir.**

**Ve taşınma sanıldığı kadar pahalı değil, çünkü seam ZATEN dar ve ZATEN tek:**
`toolbroker.ShellRunner` **tek metotludur** — `Run(ctx, ShellCommand) (ShellResult, error)` — ve
`ShellCommand{Argv, WorkspaceRoot, ReadOnly, Shell}` tamamen serileştirilebilir
(`packages/tool-broker/sandbox_exec.go:56-67`). Yani shell yarısı **var olan bir arayüzün ikinci bir
implementasyonudur**: frame'i lease bağlantısından gönder, sonucu bekle. Orchestrator, ledger, onay
kapısı, hook'lar — hepsi CP-side kalır ve **bit-değişmezdir**. Pahalı olan yarı workspace'tir: klon,
dosya op'ları, snapshot ve changeset bugün CP'nin diskinde. **T7 ikisini birden taşır ve bu epic'in en
büyük task'ıdır.**

**DAG (cap 3):**

```
Wave 1: T1 (registry + mig 000045)
Wave 2: T2 (havuzlar + yerleştirme sırası; T1)   T3 (havuz anahtarı; T1)
Wave 3: T4 (yerleştirme + tenant + kapasite parkı; T2+T3)   T5 (keyed cordon/drain/revoke; T1)
Wave 4: T6 (strict mode + CLI + rotalar; T3+T5)   T7 (İCRA RELAY'İ; T4)
Wave 5: T8 (EXIT gate; hepsine bağlı)
```

**T1 TEK MIGRATION SAHİBİDİR.** Diğer yedi task `storage/migrations/` altına dosya koymaz — "iki
paralel task'ın ikisi de 000045'i alır" tuzağı bir dikkat kuralıyla değil **yapısal olarak** imkânsız
kılınır.

Her paralel merge sonrası **`go vet -tags="component live" ./...`**, ve **case.yaml / migration / yeni
tenant tablosu dokunuşunda `tests/uat/automation` + `tests/security/tenancy` corpora'sı KOŞULUR**
(T1 **dört** yeni tenant tablosu açıyor ⇒ dördü için de `allTables` + `palai_apply_tenant_policy` +
`GRANT` + tenancy corpus **zorunlu**). Her task RED-first TDD + green milestone başına commit +
`git push origin main`.

**SECURITY-CRITICAL (full review): T1, T2, T3, T4, T5, T6, T7.**

---

### T1 — Runner registry: gateway'in kendi notunun tarif ettiği tablo (**mig 000045**; SECURITY-CRITICAL)

**BU TASK'I İKİ DOSYA BAĞIMSIZ OLARAK ISMARLAMIŞ.** `runner_gateway.go:73` birebir *"there is no
hosts/runners table in this tier … that is the SaaS/post-SH-0 upgrade path"*, ve `local_credentials.go:122`
birbirinden habersiz ikinci kez: *"Bounding concurrent identities per token needs a runner registry the
single-runner SH-0 topology does not have; that is the upgrade path."*

- [ ] **RED önce (1):** iki runner enroll eder; registry **iki satır** taşır ve ikisi **ayrı** id'lidir.
      Bugün `identity atomic.Pointer` tek slot (D13) ⇒ test **RED doğar.**
- [ ] **RED önce (2), KİMLİK:** iki makine **aynı** `runner_id`'yi talep eder; **ikisi de kendi
      kimliğini alır**, ikisi de ayrı satırdır, ve hiçbiri diğerinin sertifikasını geçersiz kılmaz.
      Bugün ikisi de `runner-local` için sertifika alır ve kayıt tutulmaz (D7).
- [ ] **RUNNER ID'Sİ SUNUCU MINTLER** (`middleware.NewID` deseni, `rnr_` prefix'i). İstemcinin
      gönderdiği `runner_id` artık **bir etiket**tir (`runners.label`), bir kimlik değil; sertifikanın
      DNS'i sunucunun mintlediği id'den türetilir. **Eski runner'lar için geriye uyumluluk:** id
      göndermeyen ya da gönderen fark etmez — sunucu her hâlde kendi id'sini mintler ve enroll cevabına
      **ekler**, runner onu `renew`'de taşır.
- [ ] **Migration 000045, altı rider** (§1 R1–R6). **Dört yeni tenant tablosu**; her biri kendi
      `palai_apply_tenant_policy` + `GRANT`'ini taşır (000029'un sweep'i ve blanket grant'i bu boot'ta
      **geç kalır** — 29 < 45 ve tablo henüz yoktur), dördü de `allTables`'a girer, R4 ayrıca
      `REVOKE UPDATE, DELETE` alır (`capability_jobs`'ın deseni).
- [ ] **`fleet` paketi (YENİ, `apps/control-plane/internal/fleet/`)** ve gerekçesi bir sınır:
      `execution` paketi zaten 40+ dosya ve gateway'in içine bir store koymak onu store'a bağımlı
      yapardı. Gateway `fleet.Registry` arayüzünü alır (dört metot), üretimde Postgres, testte fake.
- [ ] **`GET /v1/runners` + `GET /v1/runners/{id}`** (okuma, RLS-scoped, `ListView` envelope'u —
      `pagination.go`'nun deseni). **Yazma rotası YOK** (T5/T6 açar).
- **Seam:** `000045_runner_fleet.{up,down}.sql` (YENİ), `storage/queries/runners.sql` (YENİ),
  `internal/fleet/store.go` (YENİ), `runner_gateway.go`, `api/router.go`,
  `tests/component/postgres/migration_test.go`. **UAT:** **FLT-001** (YENİ). **Tier:** DEĞİŞMEZ.
- **Kanıt:** untagged — id mintleme, journal append-only'liği. component-real gerçek Postgres — iki
  RED'in ikisi; dört tablonun dördünde de RLS ENABLE+FORCE; `runner_enrollments`'a UPDATE/DELETE'in
  **reddedildiği**; migration'ın ileri-geri koştuğu.
- **Live (`PALAI_DATABASE_URL` yoksa SKIP):** `000044`'ten gelen bir veritabanının yükseldiği ve
  mevcut satırların **kaybolmadığı**.
- **Honest ceiling:** **Registry bir ENVANTERdir, bir SAĞLIK KAYNAĞI değildir** — `last_seen_at`
  connect/renew'da güncellenir, heartbeat T5'in işidir. **İkinci tavan:** eski bir runner sunucunun
  mintlediği id'yi `renew`'de taşımaz (protokol alanı yok) ⇒ o runner her `renew`'de sertifikasının
  DNS'inden eşleşir; bu bir isim eşleşmesidir ve T3'ün anahtar bağını **almaz**.

---

### T2 — Havuz = kuyruk = etiket, ve sıra AÇIKÇA seçilir (mig YOK; SECURITY-CRITICAL; T1'e bağlı)

**D1 DOĞRU, AMA DÜZLEMİ YANLIŞTI** (§3.6 D1): `WorkerSpec.PoolLabel` capability-worker düzleminin ölü
alanıdır; run'ları koşturan runner düzleminde havuz kavramı **hiç yok**. Bu task onu **runner
düzleminde** kurar ve `workers` paketine **dokunmaz**.

- [ ] **RED önce (1):** `posture='unsandboxed-host'` havuzunu isteyen bir attempt,
      `posture='sandboxed-linux'` havuzuna enroll olmuş bir runner'a **offer edilirse FAIL** —
      sıraya alınmaz, "en yakın" seçilmez, **reddedilir**.
- [ ] **RED önce (2), BİT-DEĞİŞMEZLİK:** hiçbir havuz yapılandırması olmayan bugünkü compose stack'i
      **birebir aynı** davranır. Bu testin adı iddiasını söyler:
      `TestASingleRunnerDeploymentWithNoPoolConfigurationIsBitUnchanged`.
- [ ] **SIRA AÇIKÇA SEÇİLİR VE YAZILIR: havuz içinde `created_at` FIFO.** Gerekçe §3.6 D2'dir —
      capability düzleminin `ORDER BY job_id`'si FIFO **değil**, yani FIFO korunan bir davranış değil
      **yeni** bir karardır ve bir kararın gerekçesi yazılır: bir insanın beklediği run, bir
      makinenin id'si daha küçük olduğu için geçilmemelidir.
- [ ] **PUSH MODELİ KORUNUR, POLL'A GEÇİLMEZ** (P2 + §3.6 D3). Anthropic worker'ı poll ediyor; bizim
      runner'ımız park edip push alıyor ve bu **daha iyi**: kuyruk gecikmesini poll aralığı
      belirlemiyor, ve `SKIP LOCKED`'sız bir poll'un kaybeden sürüsü yok. `available` tek bir kanaldan
      **havuz başına bir kanala** çıkar — `map[poolID]chan *pendingRunner`, RWMutex ile.
- [ ] **HAVUZ POLİTİKASI `config_policy`'DEDİR** (migration YOK, yazma yolu shipped —
      `admin.go:203`): `{"pool":"mac-pool"}`. Çözüm sırası: run'ın `pool_id`'si (resume) → agent
      revision'ın binding'i → project `config_policy` → `pool_default`. **Dört basamağın hepsi tek
      bir fonksiyonda** (`fleet.ResolvePool`), çağıran başına değil.
- [ ] **POSTURE HAVUZUN ALANIDIR, RUNNER'IN BEYANI DEĞİL** — runner enroll ederken posture'ını
      **söyler**, gateway onu havuzunkiyle **karşılaştırır** ve uyuşmuyorsa enrollment'ı **reddeder**
      (journal'a `refused`). Beyanı doğrulamıyoruz — doğrulayamayız — ama **uyuşmazlığı yakalıyoruz**,
      ve bu iki farklı iddiadır. Tavan §T2'de yazılır.
- **Seam:** `internal/fleet/pools.go` (YENİ), `internal/fleet/placement.go` (YENİ),
  `runner_gateway.go` (havuz başına kanal), `api/router.go` (`/v1/runner-pools` okuma).
  **UAT:** **FLT-002** (YENİ). **Tier:** DEĞİŞMEZ.
- **Kanıt:** untagged — havuz çözümünün dört basamağı, FIFO sırası. component-real gerçek Postgres +
  gerçek gateway — iki RED'in ikisi; iki havuz, iki runner, iki run, **çaprazlama sıfır**;
  yapılandırmasız stack'in bit-değişmezliği.
- **Honest ceiling:** **Bir havuz içinde önceliklendirme YOKTUR** — FIFO'dur, ve acil bir run'ın öne
  geçmesi ifade edilemez. **İkinci tavan:** posture beyanı **doğrulanmaz**; yalan söyleyen bir runner
  `sandboxed-linux` havuzuna girip host'ta koşabilir. Doğrulama bir attestation ister ve bu epic onu
  kurmuyor — `known-gaps`'e `FLT-P*` satırı olarak girer.

---

### T3 — Havuz anahtarı: kolay taraf enrollment'ta, güç taraf sonrasında (mig YOK — R2 T1'de; SECURITY-CRITICAL; T1'e bağlı)

**BU TASK'IN PREMISE'İ TERS ÇEVRİLDİ VE BU BİR KAZANÇTIR** (§3.6 D4): ağaç zaten yeniden kullanılabilir
bir anahtara geçmiş ve dört yerde bunu inkâr ediyor. Eklenecek şey yeniden kullanılabilirlik değil —
**kapsam, hash, son kullanma, iptal, kayıt.**

- [ ] **RED önce (1), KAPSAM:** Mac havuzunun anahtarıyla Linux havuzuna enroll denemesi **REDDEDİLİR**
      ve `runner_enrollments`'a `refused` düşer. Bugün havuz kavramı yok ⇒ RED doğar.
- [ ] **RED önce (2), İPTALİN ÜÇ YARISI — VE ÜÇÜ AYRI TESTTİR:** anahtar iptal edildikten sonra
      (a) **yeni enroll REDDEDİLİR**, (b) o anahtarla enroll olmuş bir runner'ın **`renew`'ü BAŞARILI
      olur**, (c) o runner'ın **devam eden lease'i kesilmez**. (b) ve (c) bugün de doğrudur ve
      **doğru kalmaları bu epic'in vaadidir** — yani bu test bir regression fence'idir, bir özellik
      değil. Yapısal sebep: `handleRenew` mTLS ile doğrular, `Consume` o yolda yoktur (D5).
- [ ] **RED önce (3), ANAHTAR BİR API ANAHTARI DEĞİLDİR:** havuz anahtarıyla `/v1/*` altında herhangi
      bir çağrı **401** alır, ve `Scope` çözümüne hiç girmez. **`Scope.HasScope`'un boş-scope =
      her-yetki davranışı** (`auth.go:30`, durum belgesi §4) bu anahtara **ulaşamaz**, çünkü anahtar
      `api_keys` tablosunda değildir.
- [ ] **HASH + SABİT ZAMAN + BİR KEZ GÖSTERİM:** DB'de `sha256(anahtar)` ve bir `key_prefix` (ilk 8
      karakter, yalnız listeleme için); karşılaştırma `crypto/subtle.ConstantTimeCompare`; değer
      **yalnız** mint anında stdout'a. Bugünkü karşılaştırma `strings.TrimSpace(...) != token`
      (`local_credentials.go:159`) ve **o da düzeltilir** — dosya token'ı da bir bearer credential'dır.
- [ ] **`PoolEnrollmentKeys` `EnrollmentTokens`'ı UYGULAR** — arayüz değişmez, ikinci bir
      implementasyon eklenir (`FileEnrollmentTokens`'ın kardeşi). **Dosya token'ı SİLİNMEZ**: süresi
      dolmuş kimliğin tek kurtuluş yoludur ve `local_credentials.go:97-122` bunu uzun uzun anlatıyor.
      Gateway ikisini **sırayla** dener: havuz anahtarı → dosya token'ı → red.
- [ ] **`enrolled_via_key_id` KAYDEDİLİR** (R3) — hedeflenmiş iptalin bugün eksik olan tek parçası
      (D5). Bir anahtar iptal edildiğinde operatöre **o anahtarla enroll olmuş makinelerin listesi**
      gösterilir; **hiçbiri durdurulmaz**, ve durdurulmadığı testle gösterilir.
- [ ] **DÖRT KOPYANIN ÜÇÜ DÜZELTİLİR** (D4): `runner_gateway.go:40-44`, `lifecycle.go:118`,
      `cmd/runner/main.go:68-69`. **Kod değişikliği değil, üç yorum** — ama E23 T7'nin D7 dersi aynen:
      bir düzeltmenin planın adlandırdığı dosyaya gidip inancın her bulunduğu yere gitmemesi, inancı
      gönderilmeye devam ettirir.
- [ ] **`palai admin pool key create|list|revoke`** (`admin.go`'nun `apikey` ailesinin deseni,
      `admin.go:157,228`). **Anahtar argv'ye girmez** — mint stdout'a basar, iptal `key_id` ile.
- **Seam:** `execution/local_credentials.go`, `internal/fleet/keys.go` (YENİ), `runner_gateway.go`,
  `cmd/cli/internal/admin/admin.go`, `api/router.go`. **UAT:** **FLT-003** (YENİ).
  **Tier:** DEĞİŞMEZ.
- **Kanıt:** untagged — hash, sabit-zaman, prefix. component-real gerçek Postgres — üç RED'in üçü;
  anahtar değerinin **hiçbir** log/journal/evidence baytında olmadığı (JSON decode ederek süpürme).
- **Live (`PALAI_DATABASE_URL` yoksa SKIP):** gerçek Postgres'te iptal + `renew` sürekliliği.
- **Honest ceiling:** **Anahtarı elinde tutan biri, o havuza sahte bir makine enroll edip iş
  claim edebilir** — strict mode kapalıyken (T6, ve varsayılan kapalıdır). Savunmalar **anahtar
  gizliliği ve iptal hızıdır**, başka bir şey değil. **İkinci tavan:** anahtar başına eşzamanlı kimlik
  sayısı sınırlanmaz; `minInterval`'ın bellek-içi hâli (D6) kalıcı hâle getirilir ama bir **kota**
  değildir. **Üçüncü tavan:** bir sertifika çalınırsa iptal T5'in işidir, bu task'ın değil.

---

### T4 — Yerleştirme, tenant, ve kapasite için PARK (mig YOK; SECURITY-CRITICAL; T2+T3'e bağlı)

**BU TASK İKİ ÖLÇÜMDEN DOĞDU:** runner düzleminde tenant **hiç yok** (D8), ve boş bir havuza düşen
bir run **2.5 dakikada ölüyor** (D12) — Mac'in açılması 6–20 dakika (P10).

- [ ] **RED önce (1), TENANT:** A tenant'ının runner'ına B tenant'ının attempt'i offer edilirse
      **FAIL**. Bugün hiçbir kontrol yok (D8) ⇒ RED doğar. **`AttemptDescriptor` += `Tenant`**;
      `tenant` çağrı yerinde zaten yerel (`orchestrator.go:393` civarı), yani threading tek satır.
- [ ] **RED önce (2), PARK:** hedef havuzunda **hiç runner olmayan** bir run `dead_letter` olursa
      **FAIL** — `waiting` olmalı. Bugün `Dial` 20 sn'de düşüyor, retry beş kez deniyor, run ölüyor.
- [ ] **RED önce (3), UYANMA:** park etmiş run, o havuza **bir runner bağlandığında** uyanır ve
      koşar. **RED-first: 30 dakika sonra hâlâ `waiting`'deyse FAIL.**
- [ ] **PARK, E23 T1'İN KOREOGRAFİSİNİ AYNEN İZLER** — `checkpointBeforePause` →
      `ApplyRunTransition(RunCmdWait)` → `errRunAwaitingCapacity` → `ExecuteAttempt` `nil` döner
      (dispatch worker serbest). **Yeni bir park mekanizması YAZILMAZ**, ve bu bir tasarruf değil bir
      doğruluk kararıdır: iki park yolu, iki uyanma hatası demektir.
- [ ] **UYANDIRMA `handleConnect`'İN İÇİNDEDİR VE TEK TX'TİR:** bir runner havuza park ettiğinde o
      havuzun **en eski** `waiting` run'ı `running`'e çekilir + `EnqueueJob("response.run")`
      (`applyResumeTx`'in gövdesi). **Bir uyandırma bir runner'ı REZERVE ETMEZ** — uyanan run
      dial eder, ve o sırada başka bir run onu kapabilir; bu yarış **iyi huyludur** (ikinci run yeniden
      park eder) ve bir testle gösterilir.
- [ ] **DISPATCH WORKER SAYISI BİR UYARI KAZANIR** (D11): `palai up`, `PALAI_DISPATCH_WORKERS=1` iken
      **iki ya da daha çok runner** görürse birebir uyarır: *"N runners are enrolled but
      PALAI_DISPATCH_WORKERS=1 — concurrent runs are bounded by the control plane, not the fleet"*.
      **Kod değişikliği değil, ölçülmüş bir yalanın düzeltilmesi.**
- [ ] **`runs.pool_id` YAZILIR** (R5) — yerleştirme kararı denetlenebilir, ve bir resume **aynı
      havuza** döner. Bir kill sonrası run'ın başka bir posture'da uyanması, workspace'ini bulamaması
      demektir.
- **Seam:** `engine_channel.go` (`AttemptDescriptor` += `Tenant`, `PoolID`), `orchestrator.go`
  (çözüm + park), `runner_gateway.go` (`Dial` tenant+havuz, `handleConnect` uyandırma),
  `internal/fleet/placement.go`, `cmd/cli/internal/stack/up.go`. **UAT:** **FLT-004** (YENİ).
  **Tier:** DEĞİŞMEZ.
- **Kanıt:** component-real gerçek Postgres + gerçek gateway + iki fake runner — üç RED'in üçü; park
  etmiş run'ın **hiçbir dispatch worker'ı tutmadığı**; uyandıktan sonra **bir kez** koştuğu;
  çapraz-tenant offer'ın sıfır olduğu.
- **Live (`PALAI_DATABASE_URL` yoksa SKIP):** gerçek Postgres'te park + uyanma.
- **Honest ceiling:** **Park etmiş bir run için KOTA YOKTUR** — boş bir havuza yüz run düşerse yüzü de
  park eder ve hiçbiri zaman aşımına uğramaz (E23 T1'in aynı tavanı, aynı sebeple). `PALAI_QUEUE_DEADLINE`
  admission kuyruğuna bakar, buna değil. **İkinci tavan:** havuzu silinen ya da hiç runner'ı olmayacak
  bir havuza park etmiş run **sonsuza kadar bekler** — bir reaper T5'in işidir ve **bu epic'te
  yazılır**, ama süresi operatörün seçimidir ve varsayılanı yoktur.

---

### T5 — Cordon / drain / revoke runner id'ye anahtarlanır, ve iptal KALICI olur (mig YOK; SECURITY-CRITICAL; T1'e bağlı)

**E15 T2'NİN DRAIN'İ KORUNUR, YENİDEN YAZILMAZ** — yönlendirmenin talimatı ve doğru olanı. Değişen şey
**kimin** drain edildiğidir. Ve ölçüm bir sürpriz getirdi: `Revoke()`'un **hiçbir production caller'ı
yok** (§3.6 D15).

- [ ] **RED önce (1):** iki runner'lı bir gateway'de **birini** cordon etmek, **diğerine** yeni lease
      vermeyi durdurursa **FAIL**. Bugün `cordoned` process-global bir `atomic.Bool`.
- [ ] **RED önce (2), KALICILIK:** iptal edilmiş bir runner, **control plane restart'ından sonra**
      yeniden bağlanabilirse **FAIL**. Bugün `revoked atomic.Bool` bellek içi.
- [ ] **RED önce (3), ULAŞILABİLİRLİK:** `palai admin runner revoke <id>` **yoksa FAIL** — yani bu
      test, bugün var olmayan bir yüzeyin var olmasını talep eder. `Revoke`'un testlerle kanıtlı ve
      kimse tarafından ulaşılamaz olması, E23'ün `HIL-P8`'inin (onay mesajının production caller'ı
      yok) **aynı şeklidir** ve durum belgesi §2'nin kuralı burada da geçer: **bir epic'in çıkış
      kapısında, exported sembolleri production caller'a karşı süz.**
- [ ] **`Drain` GÖVDESİ DEĞİŞMEZ, SAYAÇ RUNNER BAŞINA OLUR:** `active atomic.Int64` →
      `map[runnerID]*atomic.Int64`. Bekleme mantığı, E10 recovery katmanına devretme, ve
      `ctx.Err()` dönüşü **birebir** korunur. **Whole-gateway drain de korunur** (SIGTERM yolu),
      çünkü bir control-plane swap'i hâlâ hepsini drain eder.
- [ ] **HEARTBEAT + REAPER, VE İKİSİ DE T4'ÜN PARKINI BESLER — VE HEARTBEAT NEREDEYSE BEDAVA, ÇÜNKÜ
      FRAME ZATEN GELİYOR VE ÇÖPE ATILIYOR:** `readLoop`'un `switch`'inin `default` kolu birebir
      *"heartbeat or other non-frame messages carry nothing to relay"* (`runner_gateway.go:472-474`).
      E24 o kolu `last_seen_at`'i ilerletmeye bağlar. **İkinci düzlemde aynı hikâye daha da ileri:**
      `HeartbeatCapabilityWorker` (`storage/queries/workers.sql:28-32`) **yazılmış ve sıfır Go
      caller'ı var** — yani bir heartbeat sorgusu bile hazır duruyor ve kimse çağırmıyor. E24 runner
      düzleminde olanı bağlar; capability düzlemine dokunmaz (§5). `runners.last_seen_at` connect/renew
      ve o heartbeat'te ilerler; bir reaper `last_seen_at` bayatlamış runner'ı `unhealthy`
      yapar, lease'ini keser ve **o havuza park etmiş run'ları uyandırmaz** (kapasite hâlâ yok) ama
      **havuzun sağlık sayısını düşürür**. Ayrıca **T4'ün ikinci tavanını kapatan reaper burada**:
      `PALAI_FLEET_PARK_TTL` (varsayılan **YOK** — operatör açar) dolduğunda park etmiş run uyandırılır
      ve model *"no capacity"* cevabını **öğrenir**, sessizce ölmez.
- [ ] **`palai admin runner cordon|resume|revoke|list`** + `POST /v1/runners/{id}/{cordon,resume,revoke}`.
      **Revoke geri alınamaz** ve bu bugünkü semantiğin aynısıdır (`runner_gateway.go:153`:
      *"a revoked runner identity is decommissioned, not paused"*).
- **Seam:** `runner_gateway.go`, `internal/fleet/store.go`, `api/router.go`,
  `cmd/cli/internal/admin/admin.go`, `main.go` (reaper supervisor). **UAT:** **FLT-005** (YENİ).
  **Tier:** DEĞİŞMEZ.
- **Kanıt:** component-real gerçek Postgres + iki fake runner — üç RED'in üçü; whole-gateway drain'in
  **bit-değişmez** kaldığı (SIGTERM yolu); iptalin restart'tan sağ çıktığı.
- **Honest ceiling:** **İptal edilen sertifika CRL'e girmez** — gateway her connect'te DB'ye bakar,
  yani iptal **gateway'e bağlıdır**, sertifikaya değil. Başka bir mTLS tüketicisi olsaydı o sertifikayı
  kabul ederdi; bugün yok. **İkinci tavan:** heartbeat aralığı ve bayatlama eşiği sabittir, havuz
  başına ayarlanamaz.

---

### T6 — Strict mode: bekleme odası, ve KAPALI olmasının gerekçesi (mig YOK — R1'de `strict_enrollment`; SECURITY-CRITICAL; T3+T5'e bağlı)

**D3 AYNEN UYGULANIR VE VARSAYILAN KAPALIDIR.** Autoscale eden bir havuzda makine başına insan
beklemek, autoscale'in kendisini iptal eder.

- [ ] **RED önce (1):** `strict_enrollment=true` bir havuza enroll olan makine **`pending`** durumunda
      kalır, **hiçbir lease alamaz**, ve bir insan onaylayana kadar öyle kalır. Sertifikayı **alır**
      (yoksa `renew` yolu hiç açılmaz) ama `Dial` onu **hiç görmez**.
- [ ] **RED önce (2), BİT-DEĞİŞMEZLİK:** `strict_enrollment=false` (varsayılan) iken davranış
      **birebir** T3'ünkidir.
- [ ] **ONAY YÜZEYİ E23'ÜN BOĞAZINI KULLANMAZ VE SEBEBİ YAZILIR:** `ApplyApprovalDecision` bir
      **tool çağrısının** ya da bir **publication'ın** onayıdır ve `request_hash`'e bağlıdır; bir
      makine enrollment'ının bağlanacağı bir request hash'i yoktur. **Ayrı bir yol açmak burada
      doğrudur**, ve bunu yazmak E23'ün *"kontrol tek boğaza konur"* kuralına aykırı değil, onun
      kapsamının dürüst okunmasıdır. Onay `POST /v1/runners/{id}/approve` + `palai admin runner approve`.
- [ ] **ONAYLAYAN, E23 T2'NİN LİSTESİDİR** — `config_policy.approvers` (migration YOK, yazma yolu
      shipped). **Liste yoksa davranış bit-değişmezdir** (E23 T2'nin kuralı aynen).
- [ ] **REZİDÜEL RİSK PLANDA VE `known-gaps`'TE ADIYLA YAZILIR** (yönlendirmenin talebi): *"strict
      mode kapalıyken, havuz anahtarını elinde tutan biri o havuza sahte bir makine enroll edip iş
      claim edebilir; savunmalar anahtar gizliliği ve iptal hızıdır."* **Bir yorumda değil, bir
      `FLT-P*` satırında.**
- [ ] **`palai up` strict mode'u ve havuz durumunu BASAR** — kaç havuz, kaç aktif runner, kaç
      `pending`. Sessiz bir bekleme odası, bekleme odası değildir (E21 T2'nin sessiz-SKIP dersi).
- **Seam:** `runner_gateway.go`, `internal/fleet/store.go`, `api/router.go`, `admin.go`,
  `cmd/cli/internal/stack/up.go`, `docs/operations/runner-fleet.md` (YENİ).
  **UAT:** **FLT-003 GENİŞLETİLİR.** **Tier:** DEĞİŞMEZ.
- **Kanıt:** component-real — iki RED'in ikisi; `pending` bir runner'ın **hiçbir** attempt görmediği;
  yetkisiz bir principal'ın onaylayamadığı.
- **Honest ceiling:** **Onay bir MAKİNEYİ değil bir ENROLLMENT'ı onaylar.** Aynı makine yeniden enroll
  ederse yeniden onay ister — ki doğrusu budur, ama bir Mac'in her yeniden başlatılmasında bir insan
  demektir. **İkinci tavan:** `MAC-P6` aynen açık; strict mode kimin enroll ettiğini sorar, o Mac'in
  içinde kaç müşteri olduğunu değil.

---

### T7 — İCRA RELAY'İ: bir Mac'in Mac olması (mig YOK; SECURITY-CRITICAL; T4'e bağlı; **BU TASK'IN BÜYÜKLÜĞÜ AYRICA TARTIŞILIR**)

**BU TASK D9'UN CEVABIDIR VE EPIC'İN ÜRÜN DEĞERİDİR.** Onsuz E24, koşamayan bir düzleme yerleştirme
kurar. Ağaç seam'i **adıyla ısmarlamış** (`main.go:591-595`) ve **shipped off-host topoloji bugün bir
coding run'ı koşturamıyor** (§3.6 D10).

- [ ] **RED önce (1), TAÇ TEST:** `unsandboxed-host` posture'lı bir havuzdaki runner'a düşen bir run'ın
      `palai.workspace.shell` çağrısı **o runner'ın makinesinde** koşar. Ölçüm bir sayaç üzerinden:
      **control plane'in host executor'ı SIFIR kez çağrılır.**
- [ ] **RED önce (2), WORKSPACE:** aynı run'ın `palai.workspace.file` yazması **runner'ın diskinde**
      görünür ve **control plane'in diskinde GÖRÜNMEZ**. Bugün tersi doğru (`tools/file.go:48`).
- [ ] **RED önce (3), CREDENTIAL SINIRI:** relay frame'lerinin baytları JSON decode edilerek süpürülür
      ve içinde bir credential (repo token'ı, model anahtarı, DB URL'i, master key) bulunursa **FAIL**.
      Vendor bile bu uyarıyı yazıyor (P4). **Ham substring iddiası vacuous'tur** (E20 T4'ün dersi).
- [ ] **RED önce (4), ONAY KAPISI ÜSTTE KALIR:** `approval_required` bir tool, relay üzerinden de
      insan kararı olmadan **tek bir frame** göndermez — runner'ın exec sayacı **SIFIR**. E23'ün
      kapısı `dispatchTool`'da, yani CP'de kalır ve **bu bir mimari karardır**: kapı icranın
      yanında değil, kararın yanında durur.
- [ ] **SHELL YARISI — VAR OLAN TEK METOTLU ARAYÜZÜN İKİNCİ İMPLEMENTASYONU.**
      `execution.RunnerShellRunner` `toolbroker.ShellRunner`'ı uygular; `ShellCommand`'ı bir
      `controller.exec` frame'i olarak lease bağlantısından gönderir, `runner.exec_result`'ı bekler.
      **Orchestrator, ledger, hook'lar, onay kapısı bit-değişmez.** Runner tarafında
      `packages/runner/exec.go` (YENİ) argv'yi kendi posture'ında koşturur: container (`sandboxed-linux`)
      ya da host (`unsandboxed-host`) — **`shellRunnerFromEnv`'in gövdesi runner'a taşınır**, yeniden
      yazılmaz.
- [ ] **WORKSPACE YARISI — VE BU TASK'IN PAHALI OLAN KISMI.** Klon, dosya op'ları, snapshot ve
      changeset compile bugün CP'nin diskinde. Relay'lenen şey `workspace.WorkspaceFS`'in **dar**
      yüzeyidir (read/write/list/stat/checksum) — yeni bir protokol değil, ikinci bir frame ailesi.
      **Klon CP-side KALIR ve bu bilinçlidir:** credential broker CP-side'dır (§24, `main.go:587`),
      ve bir klonu runner'a taşımak credential'ı da taşımak olurdu. Klon **relay üzerinden** yapılır:
      CP credential'ı redeem eder, git komutunu **argv olarak** üretir ve **credential'ı bir
      helper üzerinden değil, kısa ömürlü bir handle olarak** gönderir — **ya da klon CP'de yapılıp
      workspace runner'a aktarılır.** **BU İKİ SEÇENEK ARASINDAKİ KARAR T7'NİN İLK ADIMIDIR ve
      ölçümle verilir**, bu planda varsayım olarak sabitlenmez.
- [ ] **POSTURE ARTIK RUNNER'IN BEYANIDIR VE HAVUZUNKİYLE KARŞILAŞTIRILIR** (T2). `resolveShellPosture`
      **korunur** — ama artık runner process'inde koşar; control plane'deki kopya **eski
      deployment'lar için** yerinde kalır ve `fleet` yolu yokken devreye girer. **RED-first: eski
      compose stack'i bit-değişmez.**
- **Seam:** `execution/runner_shell.go` (YENİ), `execution/runner_workspace.go` (YENİ),
  `packages/runner/exec.go` (YENİ), `packages/runner/serve.go`, `packages/contracts/`,
  `cmd/runner/main.go`, `execution/tools/file.go`, `orchestrator.go`. **UAT:** **FLT-006** (YENİ).
  **Tier:** DEĞİŞMEZ.
- **Kanıt:** component-real gerçek Postgres + gerçek gateway + gerçek runner process — dört RED'in
  dördü; relay'lenmiş bir shell'in `ShellResult`'ının CP-side'ınkiyle **alan alan aynı** olduğu;
  eski (relay'siz) stack'in bit-değişmez olduğu.
- **Live (`PALAI_FLEET_LIVE_RUNNER_HOST` yoksa SKIP):** **gerçek bir off-host runner'da gerçek bir
  `xcodebuild -version`** — yani §3.6 D10'un kapandığı yer.
- **Honest ceiling:** **İLERLEME AKIŞI YOK, VE ARTIK BİR AĞ ÜZERİNDEN YOK.** `ShellResult` tek sonuç
  döner (`known-gaps` `CAS-P2`) ve relay bunu değiştirmez — dört dakikalık bir `xcodebuild` hâlâ dört
  dakika sessizliktir, ama şimdi bir de bağlantı zaman aşımı riski taşır. **İkinci tavan:** relay
  gecikme ekler; her dosya op'u bir round-trip'tir ve büyük bir changeset compile'ı ölçülebilir
  şekilde yavaşlar — **ölçüm §6'dadır ve bir sayı olarak yazılır, bir tahmin olarak değil.**
  **Üçüncü tavan:** `MAC-P6` aynen açık.

> **BÜYÜKLÜK UYARISI — VE BÖLÜNME NOKTASI ADIYLA:** T7 iki yarımdır ve ikisi bağımsız olarak
> gönderilebilir. **Shell yarısı** (var olan tek metotlu arayüzün ikinci implementasyonu) bir task'tır.
> **Workspace yarısı** (klon kararı + dosya relay'i + snapshot) **ayrı bir task, muhtemelen ayrı bir
> epic'tir.** Owner bölmek isterse bölünme noktası budur: **T7a shell relay'i E24'te, T7b workspace
> relay'i E26'da.** O hâlde E24 bir Mac'te `xcodebuild` koşturur ama workspace'i hâlâ paylaşılan bir
> dosya sistemi ister — yani **aynı yerel ağdaki** bir Mac çalışır, kiralanan bir Mac çalışmaz. Bu
> dürüst bir ara durumdur ve **bir yalan değildir**, yeter ki böyle yazılsın.

---

### T8 — EXIT gate: `runner-fleet-0.1.0` + fleet journey (mig YOK)

- [ ] **Case id prefix'i `FLT-`'dir, ve bu bir gate kararıdır.** `promote-gate-family-dispatch` kuralı:
      bir `WRK-`/`OPS-`/`SAN-` id'si ya gönderilmiş bir bundle'ı yeniden ürettirir ya `PromoteGateFor`'u
      daha zayıf bir gate'e düşürür. **`FLT-` ağaçtaki otuz bir prefix'in hiçbiriyle çakışmıyor**
      (sayıldı 2026-07-29).
- [ ] `tests/uat/extensions/catalog_test.go:69`: `extensionIDPrefixes` bugün **dokuz** üyeli
      (`SLK- A2A- KNO- QUA- TLM- CAS- HIL- UI- WRK-`, sayıldı 2026-07-29); `FLT-` **onuncu** olur.
      **Sahiplik `uat.FleetCaseIDs`'te yaşayabilir; SÜPÜRMEDEN KAÇMAK yaşayamaz** — dosyanın kendi
      cümlesi: *"this sweep is the ONLY place in the tree that walks the cases DIRECTORY, so a prefix
      left outside it is a family whose dirs nothing checks."*
- [ ] `tests/uat/fleet/` journey: temiz stack → `palai up` → **iki havuz** yaratılır (`linux-pool`
      `sandboxed-linux`, `mac-pool` `unsandboxed-host`) → her birine **bir havuz anahtarı** →
      **iki runner** aynı anahtarlarla enroll olur ve registry **iki ayrı satır** taşır (T1) → Mac
      havuzunun anahtarıyla Linux havuzuna enroll denemesi **reddedilir** (T3) → bir run `mac-pool`
      politikasıyla açılır ve **yalnız** Mac runner'ına düşer (T2/T4) → **havuz boşaltılır**, yeni bir
      run **park eder ve ölmez** (T4) → runner geri gelir, run **uyanır ve koşar** (T4) → havuz
      anahtarı **iptal edilir**: yeni enroll düşer, **enroll olmuş runner çalışmaya devam eder** (T3)
      → bir runner **cordon** edilir, **diğeri lease almaya devam eder** (T5) → iptal edilen runner
      **control plane restart'ından sonra da** bağlanamaz (T5) → ve bir shell çağrısı **runner'ın
      makinesinde** koşar (T7).
- [ ] Yeni case'ler: **FLT-001** (bir filo bir ENVANTERdir — iki makine iki satırdır ve kimliği sunucu
      mintler), **FLT-002** (havuz bir kuyruk ve bir etikettir; yanlış havuz bir sıra değil bir
      REDDİR; ve havuz yokken davranış bit-değişmezdir), **FLT-003** (anahtar yalnız enroll eder,
      havuzuna kilitlidir, hash'lenir, bir kez gösterilir — ve **iptali enroll olmuş makineleri
      durdurmaz**), **FLT-004** (bir run kapasite için PARK EDER, ölmez; ve bir runner'ın tenant'ı
      vardır), **FLT-005** (cordon/drain/revoke bir MAKİNEYE uygulanır ve iptal restart'tan sağ
      çıkar), **FLT-006** (bir tool çağrısı runner'ın makinesinde koşar, ve credential o sınırı
      geçmez).
- [ ] `tests/uat/evidence_fleet.go` yeni proof tipi (`Complete()` gate'li): **`FleetProof`** —
      (a) **yanlış havuza yapılan offer sayısı (SIFIR)**, (b) **çapraz-tenant offer sayısı (SIFIR)**,
      (c) **kapasite yokluğu yüzünden dead-letter olan run sayısı (SIFIR)**, (d) **anahtar iptalinden
      sonra düşen enroll olmuş runner sayısı (SIFIR)**, (e) **restart'tan sonra geri gelen iptal
      edilmiş runner sayısı (SIFIR)**, (f) **relay frame'lerinde bulunan credential bayt sayısı
      (SIFIR)**, (g) registry'deki ayrık runner sayısı ve **hepsinin id'sinin sunucu tarafından
      mintlendiği**, (h) her vendor şartının kaynak URL'i + çekim tarihi + §3.5 sapma ID'si.
      **Anti-fabrication:** `Peer` alanı birebir **`"fake"`** olmak ZORUNDA. **Ve (a)/(b)/(f)/(g)
      beyan edilen sayıya GÜVENMEZ** — taşınan byte'lardan **yeniden hesaplanır**
      (`SweepActionableElements`'in deseni).
- [ ] **(d) İÇİN ÖZEL BİR FENCE, VE BU EPIC'İN EN UCUZ GÜVENLİK TESTİDİR:** proof, iptal ANINDAN
      SONRAKİ `renew` çağrılarını sayar ve **hepsinin başarılı** olduğunu yeniden hesaplar. Bir sonraki
      okuyucu *"iptal ettiysek bağlantıyı da kesmeliyiz"* dediğinde cevabı bu satır verir — **kesmek,
      kolay enrollment'ı güvenli yapan tam olarak o özelliği silerdi.**
- [ ] `tests/uat/promote_fleet.go`: **`FleetPromoteGate`** ve `PromoteGateFor`'da **E23'TEN ÖNCE**
      dispatch (`carriesE24FleetCase`). Gate: tam olarak bir COMPLETE `FleetProof`; **hiçbir tier
      ilerlemez**; E23'ün tool-approval gate'i **birebir compose** edilir.
- [ ] `evidence.go` `committedBundleSurfaces` **22 → 23**: **`runner-fleet-0.1.0`**
      (`SurfaceRecomputed`) + `caseChecksumParts` dalı. **`LegacyShapeOnly` OLAMAZ.**
      `PALAI_WRITE_FLEET_BUNDLE=1` ile üretilir ve committed bundle jeneratör çıktısıyla **bit-eş**
      olmak zorundadır. **E18 T8'in checksum sweep tablosuna 6 case ⇒ 12 kayıt** girer.
      **`release-1.0.0-rc1`'in release index'i yeni bir bundle ADIYLA kırmızıya döner** (E22 T7'nin
      ölçtüğü tuzak) ⇒ RC de yeniden üretilir ve **fiyat burada yazılıdır.**
- [ ] `scripts/test/component`'in `-run` allow-list'i + `scripts/uat/fleet`'in seçicisi **yeni test
      adlarını içerir.** Atlanırsa yeni component testi **hiç koşmaz** ve gate yeşil görünür (**üç
      kez** düşülen tuzak: E18 T8, E21 T7, E23 T7).
- [ ] `make uat-fleet` + `make uat-fleet-live` + `scripts/uat/fleet`.
- [ ] **§3.6 D4'ÜN DÖRT KOPYASININ ÜÇÜ DÜZELTİLMİŞ OLDUĞU DOĞRULANIR** (T3'te yapıldı, burada
      **süpürülür**): ağaçta *"one-use"* / *"already-spent"* / *"spent once"* geçen ve enrollment
      token'ından bahseden bir satır kalırsa **FAIL**. Bir düzeltmenin planın adlandırdığı dosyaya
      gidip inancın her bulunduğu yere gitmemesi, E23 T7'nin D7'sinin aynı hatasıdır.
- [ ] **TIER KARARI — iki yönlü tartışılır ve kayda geçer.**

  **Karşı argüman (gerçek):** *"Artık gerçek bir filo var: makineler kayıtlı, havuzlar kapsamlı,
  anahtarlar iptal edilebilir, iş doğru makineye düşüyor ve bir tool gerçekten uzak bir makinede
  koşuyor. `workspaces` `stable` olmalı."*

  **REDDEDİLİYOR, üç sebeple:**
  1. **§6 leg 1 hâlâ açık ve E24 onu YİNE BÜYÜTTÜ:** artık gerçek bir uzak makinede gerçek bir icra
     da içinde. `Peer` yapısal olarak `"fake"`; **yakalanmış bir receipt yok.**
  2. **BİR KONTROL EKLEMEK, O KONTROLÜN GERÇEK BİR FİLODA ÇALIŞTIĞININ KANITI DEĞİLDİR.** E22 bir
     sınırı sildiği için, E23 bir sınır eklediği için ilerletmemişti; E24 bir **düzlem** ekliyor ve
     yine ilerletmiyor — çünkü üçünün de kanıtı aynı fake peer'dır.
  3. **T2'nin posture tavanı açık:** yalan söyleyen bir runner yanlış havuza girebilir, ve bunu
     yakalayan bir attestation YOK.

  **`workspaces`'i `stable`'a taşımak için NE DOĞRU OLMALIYDI:** (i) **iki fiziksel makinede**
  koşmuş, yakalanmış ve yeniden türetilebilir bir receipt; (ii) `Peer`'ın yapısal `"fake"` kısıtının
  kalkması; (iii) `linux/amd64` doğrulaması (durum belgesi §3: *"Bu makineden kapatılamayacak tek
  boşluk"*). **Üçü de yok.**
- [ ] `docs/operations/known-gaps-1.0.md`: **`FLT-P*` satırları** — strict mode kapalıyken anahtar
      sahibinin sahte makine enroll edebilmesi (T3/T6), posture beyanının doğrulanamaması (T2), havuz
      içinde önceliklendirme olmayışı (T2), park kotasının olmayışı (T4), iptalin CRL değil
      gateway-bağımlı olması (T5), relay'in ilerleme akışı taşımaması ve gecikme eklemesi (T7),
      `MAC-P6`'nın aynen açık kalması, P12/P13'ün unconfirmed'ları — **birer satır olarak.**
- **Migration:** yok — **T1'in 000045'i tek migration'dır ve zincir orada durur.**
- **Honest ceiling:** bu bundle *"artık bir bulut filosu var"* İDDİA ETMEZ. İddia ettiği şey:
  **"birden çok makinenin kimlikli, tenant kapsamlı, havuzlanmış ve iptal edilebilir bir envanter
  olarak var olabildiği; bir run'ın hangi havuzda koşacağının bir yapılandırma olduğu; kapasite
  yokken bir run'ın öldüğü değil park ettiği; bir havuz anahtarının iptalinin enroll olmuş makineleri
  ÇALIŞIR bıraktığı; ve bir tool çağrısının control plane'in değil, runner'ın makinesinde
  koşabildiği bir filo omurgası."**

---

## §5 — OUT OF SCOPE (bilinçli dışarıda, adres adresine)

| Kalem | Neden dışarıda | Nerede yaşıyor |
|---|---|---|
| **Ölçekleyici (scaler) — kuyruk derinliği → kapasite** | **E24 onu ANLAMLI kılan şeyi kuruyor ve orada duruyor.** Bir makine açmadan önce bir run'ın onu **bekleyebilmesi** gerekir; bekleyemiyordu (§3.6 D12) ve T4 onu düzeltiyor. Ölçekleyici o düzeltmenin **üstüne** yazılır, yanına değil | **E26 T1** |
| **Spawn seam + bulut sağlayıcılar (Scaleway, AWS, Docker, k8s)** | D4 doğrudur ve tek bir seam'dir (P5: on entegrasyonun hepsi aynı hook). **Ama seam'in girdisi bir ölçek kararıdır ve o karar E26'dadır.** Core'a hiçbir bulut SDK'sı girmez — bu bir §5 satırı olarak **kayıt altına alınır**, bir niyet olarak değil | **E26 T2-T3** |
| **Mac ekonomisi: 24 saatlik taban, sönme, `deletable_at`** | P9/P10/P11 ölçüldü ve **R1 `min_size` kolonunu taşıyor** — ama **kullanan yok.** Bir tabanı olan havuz, tabanı **uygulayan** bir döngü ister | **E26 T1**, `runner_pools.min_size` hazır |
| **`workers` paketi / capability-worker düzlemi Mac yolu olarak** | **Üç bağımsız ölçülmüş red** (§3.6 D14): non-loopback bind **reddediliyor** (`main.go:1589-1608`), düzlem **üç şekilde uykuda** (`known-gaps` `WRK-2`, 2026-07-26'da yeniden doğrulandı), ve **yapısal olarak tipli-operasyon** (`ErrUntypedOperation`) — genel bir shell veremez. **Relay'den daha çok iş, daha az sonuç** | Hiçbir yerde — ölçümle ret. `WRK-1`/`WRK-2` açık kalır |
| **Admin panel (havuz/anahtar/runner ekranları)** | **DIŞARIDA, ve gerekçesi bir güvenlik ölçümüdür:** konsolda **hiçbir kimlik doğrulaması yok** (`middleware.ts` yok, login yok — durum belgesi §4) ve relay `POST/PATCH/DELETE` export ediyor. **Bir havuz anahtarını kimliksiz bir yazma vekilinin arkasında mintletmek, bu epic'in kurduğu her şeyi geçersiz kılardı.** E24 anahtarı **CLI'dan** mintler (§0.2) ve konsolu **hiç beklemez.** Konsol auth'u geldiğinde ekranlar bedavadır — okuma rotaları T1/T5'te zaten açılıyor | **E25** — ve o plan ZATEN YAZILMIŞ (`docs/superpowers/plans/phase-25-admin-console.md`, 2026-07-29), yani bu satır bir öneri değil bir ATIF. §0.2 |
| **Attestation — bir runner'ın posture beyanının doğrulanması** | T2 uyuşmazlığı yakalıyor, **beyanı doğrulamıyor**. Doğrulama TPM/Secure Enclave ya da bir imzalı ölçüm ister ve o ayrı bir tasarımdır. **Bir tavan olarak yazılır, bir eksiklik olarak değil** | Talep gelirse ayrı task; `FLT-P*` |
| **`MAC-P6` — bir Mac'in içinde iki müşteri** | E22'nin ölçümü aynen: per-session ayrım **kaza önlemedir, sınır değildir**, ve `simctl --set` argv'dir (P14). E24 havuzu tenant kapsamlı yapar; **bir Mac'in içini değiştirmez** | `known-gaps` `MAC-P6`, §0.1 owner kararı |
| **Havuz içi önceliklendirme / acil run** | T2 FIFO seçiyor ve gerekçesini yazıyor. Öncelik bir politika alanı ve bir sıralama anahtarı ister | Talep gelirse ayrı task |
| **CRL / sertifika iptal listesi** | İptal gateway'de DB'ye bakarak uygulanır (T5). Gerçek bir CRL/OCSP ancak ikinci bir mTLS tüketicisi olduğunda anlam kazanır; bugün yok | Talep gelirse ayrı task |
| **İlerleme akışı (`ShellRunner`'a progress kanalı)** | `known-gaps` `CAS-P2` aynen devralınır, ve **E24 onu büyütüyor** (artık bir ağ üzerinden sessizlik). Kanal `ShellRunner`'ın **imzasını** değiştirir — yani T7'nin dayandığı seam'i | `CAS-P2`, E22 §5 |
| **`PALAI_DISPATCH_WORKERS`'ın otomatik ayarlanması** | T4 bir **uyarı** basıyor; otomatik ayarlama bir kapasite modeli ister ve o ölçekleyicinin işidir | **E26** |
| **Yeni bir discovery capability'si (`fleet`)** | `CapabilityTierOrder`'a üye eklemek `CapabilityClaimsDigest`'i oynatır → **23 bundle'ın her checksum'ı kırmızı.** Az-iddia etmek güvenlidir (E23 §5 aynen) | Hiçbir yerde |
| **`API-3`/`API-4` (publication okuma rotaları)** | E23 §5 aynen; E24 onlara ihtiyaç duymuyor ve **onay VARSAYMIYOR** — satır `post-1.0` kalır | `known-gaps`, owner kararı |

## §6 — Operator legs — gerçek-altyapı bacağı (deferred-but-scripted)

E17 §6 … E23 §6 AYNEN devralınır. E24'ün katkısı **leg 1'i yine büyütmek** ve **dört yeni ölçüm
bacağı** eklemektir.

1. **İKİ FİZİKSEL MAKİNE — YAKALANMIŞ receipt.** Kapsam yine büyüdü: artık gerçek bir uzak enrollment,
   gerçek bir uzak icra ve gerçek bir iptal de içinde. `make uat-fleet-live` E23'ün kardeşidir.
   **Gerçek koşumlar bu legi KAPATMAZ** — yakalanmış, yeniden türetilebilir bir receipt bırakmıyorlar
   ve `Peer` yapısal olarak `"fake"`. → `workspaces` flip'i buna bağlıdır.
2. **`linux/amd64` DOĞRULAMASI** — durum belgesi §3'ün *"bu makineden kapatılamayacak tek boşluk"*u,
   ve **E24 onu daha kritik yapıyor**: bir filo tanımı gereği heterojendir, ve `runner_pools.arch`
   bugüne kadar hiç koşmamış bir mimariyi adlandırabilir.
3. **P12'NİN ÖLÇÜMÜ:** Scaleway'in gerçek açılış süresi ve otomatik-silme seçeneği. **Koda varsayım
   olarak girmedi** — bu yüzden T4'ün parkı süresizdir ve hiçbir açılış sabitine dayanmaz.
4. **P13'ÜN ÖLÇÜMÜ:** Anthropic'in `Environments Work` endpoint'lerinin eşzamanlılık/sıra semantiği.
   **Cevap ne olursa olsun kod değişmez** (T2 kendi sırasını açıkça seçti) — ama bir UNCONFIRMED'ın
   kapanması bir sonraki okuyucuya bir saat kazandırır.
5. **RELAY GECİKMESİNİN ÖLÇÜMÜ (T7), VE BİR SAYI OLARAK.** Aynı changeset'i (a) paylaşılan dosya
   sistemiyle, (b) yerel ağda relay ile, (c) WAN üzerinden relay ile compile et. **Üç sayı, aynı
   tabloda.** Bir tahmin bu satırı kapatmaz.
6. **BİR HAVUZ ANAHTARININ GERÇEKTEN İPTAL EDİLMESİ, GERÇEK ZAMANDA**, ve enroll olmuş bir makinenin
   **sertifika ömrü boyunca** çalışmaya devam etmesi. Component testi saati ileri alıyor; bir operatör
   bacağı gerçek bir yenileme döngüsü bekler.
7. **BİR MAC'İN GERÇEKTEN KİRALANMASI VE 24 SAAT SONRA SİLİNMESİ** — P9/P10/P11'in fatura tarafının
   tek gerçek kanıtı, ve **E26'nın ön koşulu.**
8. **E17/E18/E19/E20/E21/E22/E23'ün devralınan tüm açık legleri** — E24 hiçbirine dokunmaz.

**Tier sonucu, bir kez söylenir:** `slack` **preview**, `knowledge-vector` **disabled**, `apple-build`
**disabled**, `console` **preview** kapanır; `workspaces` ve `capability-workers` E22/E23'ün türettiği
cevapları vermeye devam eder. **Hiçbir tier ilerlemez, ve ilerlememesinin sebebi bir eksiklik değil bir
kural: bir düzlem eklemek, o düzlemin gerçek bir filoda çalıştığının kanıtı değildir.**

## §7 — Master plan §8 için önerilen özet blok (owner paste eder)

**UAT ownership:** E24 **ALTI YENİ ID** açar ve prefix'i **`FLT-`**'dir. **FLT-001** (bir filo bir
ENVANTERdir: iki makine iki satırdır, kimliği sunucu mintler, ve enrollment defteri append-only'dir),
**FLT-002** (havuz bir kuyruk VE bir etikettir; yanlış havuz bir sıra değil bir REDDİR; ve hiçbir havuz
yapılandırılmamışken davranış bit-değişmezdir), **FLT-003** (havuz anahtarı yalnız enroll eder,
havuzuna kilitlidir, hash'lenir, bir kez gösterilir — ve **iptali enroll olmuş makineleri
DURDURMAZ**), **FLT-004** (bir run kapasite için PARK EDER, ölmez; ve bir runner'ın artık bir tenant'ı
vardır), **FLT-005** (cordon/drain/revoke bir MAKİNEYE uygulanır ve iptal control-plane restart'ından
sağ çıkar), **FLT-006** (bir tool çağrısı runner'ın makinesinde koşar, ve hiçbir credential o sınırı
geçmez). Tek yeni proof tipi: **`FleetProof`**, `Peer` alanı yapısal olarak `"fake"`.

**Exit gate — FİLO TIER İLERLETMEZ:** `runner-fleet-0.1.0` bundle'ı, E23'ün insan kapılı hattının
**birden çok makineye dağılabilen** bir hatta dönüştüğünü kanıtlar. **Bu epic'in tanımlayıcı kararı
Anthropic'in kendi mimarisinden alınmıştır ve bir taklit değil bir doğrulamadır:** birebir *"The
`self_hosted` environment **acts as a work queue**: when a session is assigned to it, Anthropic
enqueues the session as a work item"* ve *"**Two credentials:** an environment key … authenticates the
worker to its queue; your Claude API key creates sessions"* (platform docs, çekildi 2026-07-29) —
**yani havuz bir kuyruktur, bir etikettir, ve içinde routing yoktur; ve enrollment credential'ı bir
API anahtarı DEĞİLDİR.** Palai bunu kendi güçlü tarafıyla birleştirir: havuz anahtarı **makine başına
bir sertifikaya takas edilir** (kubeadm'in TLS bootstrapping'i, Tailscale'in reusable auth key'i), ve
**anahtarın iptali enroll olmuş makineleri çalışır bırakır — çünkü yenileme sertifikayla kimlik
doğrular, anahtarla değil** (`handleRenew`, `runner_gateway.go:265-284`; `Consume` o yolda hiç yok).
**MIGRATION VARDIR: 000045**, altı rider taşır — `runner_pools`, `runner_pool_keys`, `runners`, ve
append-only `runner_enrollments` (dördü de tenant tablosu, dördü de kendi `palai_apply_tenant_policy`
+ `GRANT`'ini taşır), artı `runs.pool_id` ve boot-seed'li bir `pool_default`. **Doğruluk canlı
koşumdan değil YAYIMLANMIŞ VENDOR DOKÜMANINDAN VE AĞACIN KENDİ ÖLÇÜMÜNDEN gelir** — §3.5 tablosu
**14 sapmayı** adlandırır (ikisi UNCONFIRMED ve hiçbiri koda girmez), §3.6 tablosu ise **ağacın kendi
hakkındaki on altı yanlış inancını.** Üçü diğerlerinden pahalıdır. **BİR: bir runner iş koşturmuyor,
bir MOTOR koşturuyor** — her tool control plane'in process'inde çalışıyor (`main.go:603`,
`tools/file.go:48`) ve ağaç bunu kendi kelimeleriyle yazmış (`main.go:591-595`), yani "Mac havuzu"
bugünkü anlamda `xcodebuild`'i hâlâ control plane'in makinesinde koşturur ve **shipped split-VM
kanıtı yalnız workspace'SİZ bir run'ı kanıtlıyor** (`splitvm-proof.sh:1-16`). **İKİ: enrollment
token'ı zaten tek kullanımlık DEĞİL** — tek implementasyonun başlığı birebir *"WHY THIS IS NOT
ONE-USE"* (`local_credentials.go:97`) ve "tek kullanımlık" cümlesi ağaçta **dört yerde** yazılı, biri
yeniden-enroll fonksiyonunu kuran dosyanın yirmi satır üstünde. **ÜÇ: bir Mac'i yükte açmak
ekonomik değil YAPISAL olarak imkânsız** — `Dial` 20 saniyede düşüyor (`orchestrator.go:38`), retry
beş kez deniyor (`main.go:477`), ve AWS Mac açılışını *"approximately 6 minutes to 20 minutes"* diyor;
**run, makine boot etmeden dört kez ölür.** Çözüm bir timeout büyütmesi değil, E23 T1'in park-ve-uyandır
koreografisinin ikinci kullanımıdır. Ayrıca ölçüldü: runner düzleminde **tenant kavramı hiç yok** (ne
enrollment'ta, ne `AttemptDescriptor`'da, ne `leaseOffer`'da), `runner_id` **kendi beyanıdır** ve
compose onu `runner-local` olarak **sabitlemiş**, gateway **N runner'ı bugün de kabul ediyor** ama
`identity` **tek slottur**, ve `Revoke()` — SAN-011'in hard stop'u — **testlerle kanıtlı ve hiçbir
production caller'ı yok.** **Hiçbir tier ilerlemez**, ve gerekçe bir kuraldır: bir düzlem EKLEMEK, o
düzlemin gerçek bir filoda çalıştığının kanıtı değildir — §6 leg 1 yine büyüdü, `linux/amd64` hâlâ
doğrulanmadı ve `Peer` yapısal olarak `"fake"`. **VE BİR BOYUT UYARISI:** ölçekleyici, spawn seam'i ve
bulut sağlayıcıları **E26**'ya alınmıştır (admin panel **E25**'tir ve planı ayrıca yazıldı); T7'nin workspace yarısı tek bir
task'ın bütçesini aşarsa bölünme noktası **T7a (shell relay'i, E24) / T7b (workspace relay'i, E26)**
olarak adıyla yazılıdır.
