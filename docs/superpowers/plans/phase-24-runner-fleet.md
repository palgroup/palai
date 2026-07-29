# Palai Agent-Runner Fleet Plan (E24 â bir run'Ä±n NEREDE koÅtuÄu artÄ±k bir seÃ§imdir)

> **For agentic workers:** REQUIRED SUB-SKILL: `superpowers:subagent-driven-development` (Ã¶nerilen) veya
> `superpowers:executing-plans` ile task-by-task uygula. AdÄ±mlar `- [ ]` checkbox'lÄ±dÄ±r. **Bu planÄ±n
> tanÄ±mlayÄ±cÄ± kuralÄ± E19/E20/E21/E22/E23'Ã¼nkinin devamÄ±dÄ±r: her external contract GERÃEK VENDOR
> DOKÃMANINDAN (ya da bu aÄaÃ§ta ÃLÃÃLMÃÅ koddan) grounding alÄ±r** ve kaynak URL'i + Ã§ekim tarihi ÅartÄ±n
> YANINA yazÄ±lÄ±r (Â§3.5). **Â§3.6 ise aÄacÄ±n KENDÄ° hakkÄ±ndaki yanlÄ±Å inanÃ§larÄ±dÄ±r** â E21'de on, E22'de on
> altÄ±, E23'te on Ã¼Ã§tÃ¼; burada **on altÄ±**, ve **en pahalÄ±sÄ± yÃ¶nlendirmenin kendi D7'sinin cevabÄ±dÄ±r:
> bir runner iÅ KOÅTURMUYOR, bir MOTOR koÅturuyor. Her tool Ã§aÄrÄ±sÄ± control plane'in KENDÄ° process'inde
> Ã§alÄ±ÅÄ±yor.**

**Goal:** Owner'Ä±n cÃ¼mlesi: *"kiralanan Mac'lerden bir filo kurulsun, iÅ geldiÄinde oraya dÃ¼ÅsÃ¼n, yÃ¼k
bitince sÃ¶nsÃ¼n."* BugÃ¼n Palai'nin **tek** runner'Ä± var, o runner **motoru** koÅturuyor, ve `xcodebuild`
control plane'in makinesinde Ã§alÄ±ÅÄ±yor. Bu epic o Ã¼Ã§Ã¼nÃ¼n de deÄiÅtiÄi yerdir.

**BU PLANIN TAÃ KARARI â VE ÃNCE BÄ°R DÃZELTME:**

> **HAVUZ BÄ°R KUYRUKTUR VE BÄ°R ETÄ°KETTÄ°R (D1 doÄru). AMA BÄ°R HAVUZ ÃYESÄ°NÄ°N Ä°ÅE YARAMASI Ä°ÃÄ°N ÃNCE
> Ä°CRANIN ORAYA TAÅINMASI GEREKÄ°R â ÃÃNKÃ BUGÃN RUNNER Ä°Å KOÅTURMUYOR.** `orch.SetShellRunner(...)`
> `main.go:603`'te control plane'in process'ine baÄlanÄ±r; `FileTool` `workspace.NewWorkspaceFS(env.WorkspaceRoot)`
> ile control plane'in **kendi diskine** yazar (`tools/file.go:48`). Runner'Ä±n aldÄ±ÄÄ± Åey `lease.offer`
> ve iÃ§indeki `image_digest`'tir â yani **model dÃ¶ngÃ¼sÃ¼**. AÄaÃ§ bunu kendi kelimeleriyle yazmÄ±Å
> (`main.go:591-595`): *"the tools run CP-side against the same host allocation the runner bind-mounts.
> A split CPâ runner deploy â¦ needs a runner-relay seam â¦ a NAMED FUTURE split-deploy hardening, not
> built here."* **Bu epic o seam'i aÃ§ar, ve aÃ§madan havuz kavramÄ± anlamsÄ±zdÄ±r.**

**Kapsam sÄ±nÄ±rÄ± â DÃRÃST TAVAN:**

- **(a) BU EPIC ÃLÃEKLEYÄ°CÄ° (SCALER) YAZMAZ.** Havuzu, kimliÄi, yerleÅtirmeyi ve park etmeyi kurar;
  *kaÃ§ makine aÃ§Ä±lacaÄÄ±na karar veren* dÃ¶ngÃ¼ **E26**'tir (Â§5). GerekÃ§e Ã¶lÃ§Ã¼ldÃ¼: bir makine aÃ§madan Ã¶nce
  bir run'Ä±n **onu bekleyebilmesi** gerekir, ve bugÃ¼n bekleyemiyor (Â§3.6 D12).
- **(b) BU EPIC BULUT SAÄLAYICI ENTEGRASYONU YAPMAZ.** D4 doÄrudur ve tek bir **spawn seam**'i vardÄ±r;
  ama o seam'in ilk mÃ¼Återisi `static` saÄlayÄ±cÄ±dÄ±r (ofisteki makineler) ve seam'in kendisi **E26**'te
  yazÄ±lÄ±r. Core'a hiÃ§bir bulut SDK'sÄ± GÄ°RMEZ, ve bu bir tercih deÄil bir Â§5 satÄ±rÄ±dÄ±r.
- **(c) `apple-build` `disabled` KALIR.** E22 ve E23'Ã¼n gerekÃ§esi aynen geÃ§erlidir: hiÃ§bir Palai
  deployment'Ä±nda imzalama materyali yok, ve `Catalog` tipsiz bir operasyonu reddediyor.
- **(d) BU EPIC `workers` PAKETÄ°NÄ° MAC YOLU OLARAK KULLANMAZ, VE REDDÄ°N ÃÃ BAÄIMSIZ ÃLÃÃMÃ VAR** (Â§3.6
  D14). Paket **dokunulmaz** kalÄ±r; E24 runner dÃ¼zlemini bÃ¼yÃ¼tÃ¼r, capability-worker dÃ¼zlemini deÄil.
- **(e) TEK KÄ°MLÄ°K BÄ°R SERTÄ°FÄ°KADIR, BÄ°R ANAHTAR DEÄÄ°LDÄ°R.** Havuz anahtarÄ± **yalnÄ±z** bir sertifika
  mintlemeye yarar; API anahtarÄ± deÄildir, `Scope` taÅÄ±maz, `/v1` altÄ±nda hiÃ§bir Åey aÃ§amaz.
- **(f) STRICT MODE VARSAYILAN OLARAK KAPALIDIR, VE ARTIK RÄ°SKÄ° BU PLANDA ADIYLA YAZILIDIR** (Â§2, T6).
- **(g) MIGRATION VARDIR: 000045**, dÃ¶rt tablo + iki rider taÅÄ±r (Â§1). Zincir baÅÄ± bugÃ¼n **000044**
  (`storage/migrations/000044_tool_approvals.up.sql`, sayÄ±ldÄ± 2026-07-29).
- **(h) BU BÄ°R EPIC'TEN BÃYÃKTÃR VE BÃLÃNMESÄ° ÃNERÄ°LÄ°YOR** (Â§4 sonu). E24 sekiz task'tÄ±r ve T7 (icra
  relay'i) tek baÅÄ±na bir task'Ä±n bÃ¼tÃ§esini aÅabilir; aÅarsa bÃ¶lÃ¼nme noktasÄ± **adÄ±yla** yazÄ±lÄ±dÄ±r.

---

## Â§0 â Owner'Ä±n saÄlayacaklarÄ± (HANDOVER CHECKLIST)

E19 Â§0.1, E20 Â§0, E21 Â§0, E22 Â§0 ve E23 Â§0 aynen geÃ§erlidir. **E24 owner'dan ÃÃ Åey ister ve
Ã¼Ã§Ã¼ de kod deÄil, karardÄ±r.**

### 0.1 KaÃ§ Mac, ve kim iÃ§in â Ã§Ã¼nkÃ¼ bu bir gÃ¼venlik kararÄ±dÄ±r

`known-gaps-1.0.md` `MAC-P6` birebir Åunu diyor ve bu epic onu **deÄiÅtirmiyor**:

> **Different customers â different Macs (or different uids). Same customer â one Mac, per-session
> directories plus `simctl --set`.**

E24 havuzlarÄ± **tenant kapsamlÄ±** yapar (RLS), ama bir Mac'in iÃ§indeki iki run hÃ¢lÃ¢ aynÄ± uid'dir.
**Owner beyan eder:** havuzlar tek mÃ¼Återili mi (Ã¶yleyse tenant kapsamÄ± yeterlidir), yoksa bir havuzda
birden Ã§ok mÃ¼Återi mi koÅacak (Ã¶yleyse `MAC-P6` kapanana kadar bu epic o havuzu **aÃ§maz**).

### 0.2 Havuz anahtarÄ± nerede gÃ¶sterilir â ve cevap konsol DEÄÄ°LDÄ°R

Konsolda **hiÃ§bir kimlik doÄrulamasÄ± yok** (durum belgesi Â§4). Bu yÃ¼zden havuz anahtarÄ± **CLI'dan**
mintlenir ve **stdout'a bir kez** basÄ±lÄ±r â `palai apikey create`'in shipped deseni
(`cmd/cli/internal/admin/admin.go:157,228`):

```sh
palai admin pool create --project <prj> --name mac-pool --posture unsandboxed-host --os darwin --arch arm64
palai admin pool key create --pool <pool_id>        # anahtarÄ± BÄ°R KEZ basar; DB'de yalnÄ±z sha256'sÄ± durur
palai admin pool key revoke <key_id>
```

**Owner onayÄ± gereken tek Åey:** anahtarÄ±n varsayÄ±lan Ã¶mrÃ¼. Ãneri **90 gÃ¼n**, ve gerekÃ§esi
`FileEnrollmentTokens`'Ä±n bugÃ¼nkÃ¼ hÃ¢lidir â sÃ¼resiz bir bootstrap credential'Ä± zaten var, E24 ona bir
son kullanma tarihi ekliyor.

### 0.3 DeÄiÅmeyen her Åey

`PALAI_ENROLLMENT_TOKEN_FILE` / `PALAI_RUNNER_CA_*` / `PALAI_RUNNER_SERVER_*` / `PALAI_RUNNER_ID` /
`PALAI_CONTROLLER_URL` / `PALAI_CONTROLLER_DNS` / `PALAI_ENGINE_IMAGE` / `PALAI_RUNNER_CONCURRENCY` /
`PALAI_WORKSPACE_ROOT` / `PALAI_SANDBOX_IMAGE` / `PALAI_SHELL_NATIVE` â **hepsi aynen kalÄ±r.** Tek
kullanÄ±mlÄ±k dosya token'Ä± **silinmez**: o, kimliÄi sÃ¼resi dolmuÅ bir runner'Ä±n tek kurtuluÅ yoludur
(Â§3.6 D4) ve E24 onu bir havuz anahtarÄ±yla DEÄÄ°ÅTÄ°RMEZ, yanÄ±na koyar.

---

## Â§1 â YapÄ± kararÄ±: fork noktasÄ±, migration, dosyalar

**Fork noktasÄ±:** `main` >= `2933055` (E23 tamamÄ± aÄaÃ§tadÄ±r; `HIL-` prefix'i ve
`tool-approval-0.1.0` bundle'Ä± shipped).

**MIGRATION: VAR â `000045_runner_fleet`. Tek migration, tek task (T1).** Zincir baÅÄ± bugÃ¼n
**000044**'tÃ¼r (sayÄ±ldÄ± 2026-07-29). Paralel migration tuzaÄÄ± yapÄ±sal olarak imkÃ¢nsÄ±z kÄ±lÄ±nÄ±r:
`storage/migrations/` altÄ±na dosya koyan **tek** task T1'dir.

| # | Ne | Neden migration ZORUNLU |
|---|---|---|
| **R1** | **`runner_pools`** (YENÄ° tenant tablosu) â `id`, `organization_id`, `project_id`, `name`, `posture`, `os`, `arch`, `strict_enrollment BOOLEAN NOT NULL DEFAULT false`, `created_at`; `UNIQUE (organization_id, project_id, name)`; `CHECK (posture IN ('sandboxed-linux','unsandboxed-host'))` | Havuz kalÄ±cÄ± bir nesnedir; bir env var ya da `config_policy` alanÄ± **olamaz**, Ã§Ã¼nkÃ¼ bir runner ona **enroll** olur ve enrollment bir yabancÄ±dan gelir |
| **R2** | **`runner_pool_keys`** (YENÄ° tenant tablosu) â `id`, `organization_id`, `project_id`, `pool_id`, `key_sha256 TEXT NOT NULL`, `key_prefix TEXT NOT NULL`, `created_at`, `expires_at`, `revoked_at`, `last_used_at`; `UNIQUE (key_sha256)` | Credential **hash'i** saklanÄ±r, deÄeri asla. `UNIQUE (key_sha256)` bir anahtarÄ±n iki havuza takÄ±lamamasÄ±dÄ±r |
| **R3** | **`runners`** (YENÄ° tenant tablosu) â `id` (**sunucu mintler**), `organization_id`, `project_id`, `pool_id`, `runner_dns`, `public_key_sha256`, `state` (`CHECK IN ('pending','active','cordoned','revoked')`), `enrolled_via_key_id`, `os`, `arch`, `posture`, `capacity`, `cert_not_after`, `enrolled_at`, `last_seen_at` | `runner_gateway.go:73`'Ã¼n kendi cÃ¼mlesi: *"there is no hosts/runners table in this tier â¦ that is the SaaS/post-SH-0 upgrade path"*. Ve `local_credentials.go:122` aynÄ± Åeyi baÄÄ±msÄ±z olarak ikinci kez yazÄ±yor |
| **R4** | **`runner_enrollments`** (YENÄ°, **APPEND-ONLY** journal) â `id`, `organization_id`, `project_id`, `runner_id`, `pool_id`, `key_id`, `entry_kind` (`CHECK IN ('requested','approved','refused','issued','revoked','renewed')`), `entry_seq`, `detail JSONB`, `created_at`; `UNIQUE (runner_id, entry_seq)` | `capability_jobs`'Ä±n (000040) Åekli AYNEN: `GRANT SELECT, INSERT` + **`REVOKE UPDATE, DELETE`**. Bir enrollment defteri, silinebiliyorsa defter deÄildir |
| **R5** | Rider: **`runs.pool_id TEXT NULL REFERENCES runner_pools(id)`** | YerleÅtirme KARARI denetlenebilir ve bir resume **aynÄ± havuza** dÃ¶ner. NULL = yerleÅtirme kararÄ± yok (bugÃ¼nkÃ¼ her run) |
| **R6** | Rider: **`runner_pools` boot-seed** â bootstrap org/project iÃ§in `pool_default`, `posture='sandboxed-linux'`, `strict_enrollment=false` | **Tek runner'lÄ± deployment'Ä± bozmamanÄ±n yolu budur** (Â§2) |

**DÃRT YENÄ° TENANT TABLOSU â DÃRT KEZ AYNI DÄ°SÄ°PLÄ°N** (mig 000029/000030 M3 kuralÄ±, `000040`'Ä±n deseni):
her biri **kendi** `CALL palai_apply_tenant_policy('<tablo>', 'organization_id', true)` satÄ±rÄ±nÄ± taÅÄ±r
(000029'un boot sweep'i **bu boot'ta geÃ§ kalÄ±r**: 29 numarasÄ± 45'ten Ã¶nce koÅar, tablo henÃ¼z yoktur),
her biri **kendi** `GRANT`'ini alÄ±r (000029'un blanket grant'i de aynÄ± sebeple geÃ§ kalÄ±r), her biri
`tests/component/postgres/migration_test.go:29` `allTables`'a eklenir, ve dÃ¶rdÃ¼ de
`tests/security/tenancy` corpus'una **otomatik** girer â `TestEveryTenantTableIsRowLevelSecured`
(`tenancy_test.go:242-249`) `organization_id` taÅÄ±yan her tabloyu katalogdan bulup ENABLE+FORCE
arÄ±yor, yani politikayÄ± unutan tablo **sessizce kapsam dÄ±ÅÄ± kalmÄ±yor, kÄ±rmÄ±zÄ± yakÄ±yor.**

**MIGRATION Ä°STEMEYEN ve bu yÃ¼zden ayrÄ±ca yazÄ±lan Åeyler:**

- **Yeniden kullanÄ±labilir enrollment key migration Ä°STEMEZ â ÃÃNKÃ ZATEN VAR.** `FileEnrollmentTokens`
  **tek kullanÄ±mlÄ±k deÄildir** ve baÅlÄ±ÄÄ± birebir *"WHY THIS IS NOT ONE-USE, AND WHAT REPLACED THAT"*
  (`local_credentials.go:97`). E24'Ã¼n eklediÄi Åey yeniden kullanÄ±labilirlik deÄil, **kapsam, hash,
  iptal ve kayÄ±t**tÄ±r (Â§3.6 D4).
- **"AnahtarÄ± iptal et, makineler Ã§alÄ±Åmaya devam etsin" migration Ä°STEMEZ ve BUGÃN DE DOÄRUDUR.**
  Yenileme `handleRenew` Ã¼zerinden **sertifikayla** kimlik doÄruluyor (`runner_gateway.go:265-284`) ve
  `Consume` o yolda **hiÃ§ yok**. AnahtarÄ± silmek yalnÄ±z *yeni* enrollment'Ä± ve *sÃ¼resi dolmuÅ kimlik*
  kurtarma yolunu kapatÄ±r (Â§3.6 D5).
- **Run'Ä±n kapasite iÃ§in park etmesi migration Ä°STEMEZ.** `RunCmdWait` / `applyResumeTx` /
  `checkpointBeforePause` E08+E10'da var, ve **E23 T1 bunu dÄ±ÅarÄ±dan uyandÄ±rma ile birlikte yeni
  kanÄ±tladÄ±** (`phase-23-tool-approval.md` T1). E24 aynÄ± koreografiyi ikinci kez kullanÄ±r, yenisini
  yazmaz.
- **Havuz POLÄ°TÄ°KASI migration Ä°STEMEZ.** Bir ajanÄ±n hangi havuzu istediÄi `config_policy` JSONB'sinde
  yaÅar (`PATCH /v1/projects/{id}`, `admin.go:203`) â E23 T2'nin `approvers` iÃ§in verdiÄi kararÄ±n
  aynÄ±sÄ±, aynÄ± gerekÃ§eyle.
- **Drain migration Ä°STEMEZ ve YENÄ°DEN YAZILMAZ.** `RunnerGateway.Drain` (`runner_gateway.go:170-184`)
  E15 T2'nin iÅidir, `active atomic.Int64` Ã¼zerinde bekler ve E10 recovery katmanÄ±nÄ± **yeniden
  kullanÄ±r**. E24 onu runner id'ye **anahtarlar**, gÃ¶vdesini deÄiÅtirmez.

**Files:** `storage/migrations/000045_runner_fleet.{up,down}.sql` (**YENÄ°**),
`storage/queries/runners.sql` (**YENÄ°**), `apps/control-plane/internal/fleet/` (**YENÄ° paket** â
`store.go`, `pools.go`, `keys.go`, `placement.go`),
`apps/control-plane/internal/execution/runner_gateway.go` (registry + havuz + tenant),
`apps/control-plane/internal/execution/local_credentials.go` (`PoolEnrollmentKeys`),
`apps/control-plane/internal/execution/engine_channel.go` (`AttemptDescriptor` += `Tenant`, `PoolID`),
`apps/control-plane/internal/execution/orchestrator.go` (yerleÅtirme + kapasite parkÄ±),
`apps/control-plane/internal/execution/runner_shell.go` (**YENÄ°** â `ShellRunner` relay'i),
`apps/control-plane/internal/execution/runner_workspace.go` (**YENÄ°** â workspace relay'i),
`packages/runner/serve.go` + `packages/runner/exec.go` (**YENÄ°** â runner tarafÄ± icra),
`packages/contracts/` (`controller.exec` / `runner.exec_result` frame tipleri),
`cmd/runner/main.go` (posture beyanÄ±), `cmd/cli/internal/admin/admin.go` (`pool` komut ailesi),
`cmd/cli/internal/stack/up.go` (varsayÄ±lan havuz uyarÄ±sÄ±),
`apps/control-plane/api/router.go` (`/v1/runner-pools`, `/v1/runners`),
`tests/uat/cases/FLT-001..006` (**YENÄ°**), `tests/uat/evidence_fleet.go` +
`promote_fleet.go` (**YENÄ°**), `tests/uat/fleet/` (**YENÄ°**), `scripts/test/component`,
`scripts/uat/fleet` (**YENÄ°**), `docs/operations/runner-fleet.md` (**YENÄ°**),
`docs/operations/known-gaps-1.0.md`.

**DOKUNULMAYANLAR:** `apps/control-plane/internal/workers/*` (**E22/E23'Ã¼n bit-deÄiÅmezliÄi sÃ¼rer** â
gerekÃ§e Â§3.6 D14), `adapters/integrations/slack/interactions.go`'nun AST taramasÄ±,
`packages/tool-broker/broker.go`'nun `ReplayClass`/`RequiresApproval` semantiÄi, E23'Ã¼n onay kapÄ±sÄ±
(bir relay'lenmiÅ shell **de** onay kapÄ±sÄ±ndan geÃ§er ve bu bir testtir).

---

## Â§2 â Design invariant (task deÄil, her task'Ä±n kabul ÅartÄ±)

- **TEK RUNNER'LI DEPLOYMENT BÄ°T-DEÄÄ°ÅMEZDÄ°R.** Havuz beyan etmeyen bir runner `pool_default`'a dÃ¼Åer;
  havuz politikasÄ± olmayan bir run `pool_default`'a yerleÅir; sonuÃ§ bugÃ¼nkÃ¼ davranÄ±ÅtÄ±r.
  **RED-first: bugÃ¼nkÃ¼ compose stack'i, hiÃ§bir havuz yapÄ±landÄ±rmasÄ± olmadan, aynÄ± run'Ä± aynÄ± Åekilde
  koÅturmazsa FAIL.** Bu pazarlÄ±k dÄ±ÅÄ±dÄ±r ve bir yorumla deÄil bir testle durur.
- **YERLEÅTÄ°RME BÄ°R REDDÄ°R, BÄ°R TERCÄ°H DEÄÄ°LDÄ°R.** `Dial` bir havuz ister ve **yalnÄ±z o havuzun**
  Ã¼yesine offer yapar. YanlÄ±Å havuzdaki bir runner'a lease **verilmez** â sÄ±raya alÄ±nmaz, "en yakÄ±n"
  seÃ§ilmez. **RED-first: `unsandboxed-host` posture'Ä± isteyen bir attempt, `sandboxed-linux`
  posture'lÄ± bir runner'a offer edilirse FAIL.**
- **TENANT RUNNER DÃZLEMÄ°NE GÄ°RER.** BugÃ¼n girmiyor (Â§3.6 D8): enrollment org/project taÅÄ±mÄ±yor,
  `AttemptDescriptor` taÅÄ±mÄ±yor, `leaseOffer` taÅÄ±mÄ±yor. **RED-first: A tenant'Ä±nÄ±n runner'Ä±na B
  tenant'Ä±nÄ±n attempt'i offer edilirse FAIL.** Bu, `MAC-P6`'nÄ±n makine-baÅÄ±na kuralÄ±nÄ±n **altÄ±ndaki**
  katmandÄ±r, yerine geÃ§en deÄil.
- **RUNNER ID'SÄ° SUNUCU MINTLER.** BugÃ¼n enroll eden taraf kendi adÄ±nÄ± sÃ¶ylÃ¼yor ve gateway o adÄ±
  imzalÄ±yor (`runner_gateway.go:218-221,247`); compose ise adÄ± sabitliyor
  (`runner-entrypoint.sh:10` â `runner-local`). **RED-first: iki makine aynÄ± `runner_id`'yi talep
  ederse ikisi de kendi kimliÄini alÄ±r ve ikisi de kayÄ±tta ayrÄ± satÄ±rdÄ±r.**
- **ANAHTAR YALNIZ ENROLL EDER.** Havuz anahtarÄ± `Scope` taÅÄ±maz, `/v1` altÄ±nda hiÃ§bir Åey aÃ§maz, ve
  **yalnÄ±z kendi havuzuna** enroll eder. **RED-first: Mac havuzunun anahtarÄ±yla Linux havuzuna enroll
  denemesi REDDEDÄ°LÄ°R; aynÄ± anahtarla `/v1/*` Ã§aÄrÄ±sÄ± 401 alÄ±r.**
- **ANAHTAR HASH'LENÄ°R, BÄ°R KEZ GÃSTERÄ°LÄ°R, SABÄ°T ZAMANDA KARÅILAÅTIRILIR.** DB'de `sha256` durur;
  deÄer yalnÄ±z mint anÄ±nda stdout'a basÄ±lÄ±r; karÅÄ±laÅtÄ±rma `crypto/subtle.ConstantTimeCompare`'dÄ±r.
  BugÃ¼nkÃ¼ hÃ¢l `strings.TrimSpace(string(raw)) != token` (`local_credentials.go:159`) â bir bearer
  credential'Ä±n sabit-zamansÄ±z karÅÄ±laÅtÄ±rmasÄ±. **Argv'de, log'da, evidence'ta, journal'da anahtar
  DEÄERÄ° YOKTUR**; `runner_enrollments` yalnÄ±z `key_id` yazar.
- **ANAHTARI Ä°PTAL ETMEK ENROLL OLMUÅ MAKÄ°NELERÄ° DURDURMAZ, VE BUNUN SEBEBÄ° YAPISALDIR:** yenileme
  sertifikayla kimlik doÄrular, anahtarla deÄil (`handleRenew`). **RED-first: anahtar iptal edildikten
  sonra (a) yeni enroll REDDEDÄ°LÄ°R, (b) enroll olmuÅ runner'Ä±n `renew`'Ã¼ BAÅARILI olur, (c) o
  runner'Ä±n lease'i kesilmez.** ÃÃ§Ã¼ ayrÄ± ayrÄ±.
- **BÄ°R RUNNER'I Ä°PTAL ETMEK KALICI OLMALIDIR.** BugÃ¼n `revoked atomic.Bool` process-iÃ§i ve **hiÃ§bir
  production caller'Ä± yok** (Â§3.6 D15). E24 iptali `runners.state`'e yazar; gateway her connect'te
  okur. **RED-first: control plane restart'Ä±ndan sonra iptal edilmiÅ bir runner yeniden baÄlanabilirse
  FAIL.**
- **KAPASÄ°TE YOKSA RUN PARK EDER, ÃLMEZ.** BugÃ¼n `Dial` 20 saniyede dÃ¼ÅÃ¼yor ve run beÅ denemede
  dead-letter oluyor (Â§3.6 D12). **RED-first: hedef havuzunda hiÃ§ runner olmayan bir run,
  `dead_letter` olursa FAIL â `waiting` olmalÄ±; ve o havuza bir runner baÄlandÄ±ÄÄ±nda UYANMALI.**
  Koreografi E23 T1'inkidir, yenisi yazÄ±lmaz.
- **Ä°CRA RUNNER'A TAÅINIR, VE TAÅINIRKEN CREDENTIAL SINIRINI GEÃÄ°RMEZ** (Â§24, `main.go:587`).
  Relay frame'i `argv` + workspace yolu + read-only bayraÄÄ±nÄ± taÅÄ±r; **credential taÅÄ±maz.**
  **RED-first: relay frame'lerinin baytlarÄ± sÃ¼pÃ¼rÃ¼lÃ¼r ve iÃ§inde bir credential bulunursa FAIL** â
  sÃ¼pÃ¼rme JSON decode ederek yapÄ±lÄ±r (E20 T4'Ã¼n dersi).
- **ONAY KAPISI RELAY'Ä°N ALTINDA DEÄÄ°L, ÃSTÃNDEDÄ°R.** E23'Ã¼n `approval_pending` dalÄ± `dispatchTool`'da,
  yani control plane'de kalÄ±r. **RED-first: `approval_required` bir tool, relay Ã¼zerinden de insan
  kararÄ± olmadan runner'a bir tek frame gÃ¶ndermez â runner'Ä±n exec sayacÄ± SIFIR.**
- **Kontrat dokÃ¼mandan ya da AÄACIN ÃLÃÃMÃNDEN gelir.** DoÄrulanamayan hiÃ§bir Åey koda VARSAYIM olarak
  girmez â Â§3.5'e **UNCONFIRMED** olarak girer.
- **Credential-gated live smoke: `//go:build live`, eksik env deÄiÅkeninin ADIYLA `t.Skip`.**
- **YÃ¼zeye, credential'a, enrollment'a, yerleÅtirmeye ya da Ä°CRA YOLUNA dokunan HER task full review
  alÄ±r: T1âT7.**

---

## Â§3 â DoÄrulanmÄ±Å seam envanteri (2026-07-29, aÄaca karÅÄ±; HEAD `2933055`)

| Seam | Durum (doÄrulandÄ±) |
|---|---|
| **Runner gateway** | `execution/runner_gateway.go:52-84`. ÃÃ§ route: `/v1/runner/{enroll,renew,connect}` (:210-216). AyrÄ± mTLS listener, `PALAI_RUNNER_LISTEN_ADDR` (`main.go:134,1444`) |
| **Enrollment isteÄi** | `enrollRequest{RunnerID, PublicKey}` (:218-221) â **org yok, project yok, havuz yok, posture yok.** Gateway `runnerDNS(request.RunnerID)` ile imzalar (:247) |
| **Enrollment token** | Tek Ã¼retim implementasyonu `FileEnrollmentTokens` (`local_credentials.go:93-169`). **Tek kullanÄ±mlÄ±k DEÄÄ°L**; `minInterval` = sertifika TTL'i (varsayÄ±lan 5 dk), `lastIssued` **bellek iÃ§i** (:127) |
| **Yenileme** | `handleRenew` (:265-284) mTLS ile â **token yolda deÄil.** "AnahtarÄ± iptal et, makineler Ã§alÄ±ÅsÄ±n" Ã¶zelliÄinin yapÄ±sal kaynaÄÄ± budur |
| **BaÄlantÄ± havuzu** | `available chan *pendingRunner` **buffersÄ±z** (:129). `handleConnect`'te runner sayÄ±sÄ± guard'Ä± **YOK** â **N runner bugÃ¼n de baÄlanabiliyor** (:342-355) |
| **Lease teklifi** | `leaseOffer` (:564-586): `image_digest`, `limits`, ve varsa `workspace_host_path`/`workspace_read_only`/`workspace_unsafe`. **Tenant YOK, havuz YOK** |
| **Attempt tanÄ±mlayÄ±cÄ±sÄ±** | `AttemptDescriptor` (`engine_channel.go:13-33`): RunID, AttemptID, Fence, ImageDigest, Limits, Workspace*, JobID. **Tenant YOK.** Ama `tenant` Ã§aÄrÄ± yerinde ZATEN elde (`orchestrator.go:393` civarÄ±, yerel deÄiÅken) â threading UCUZ |
| **Dial bÃ¼tÃ§esi** | `dialHandshakeDeadline = 20 * time.Second` (`orchestrator.go:38`), `context.WithTimeout` (:390). Retry: `MaxAttempts: 5, BaseBackoff: 100ms, MaxBackoff: 30s` (`main.go:477`) â **~2.5 dk'da dead-letter** |
| **Dispatch eÅzamanlÄ±lÄ±ÄÄ±** | `PALAI_DISPATCH_WORKERS` **varsayÄ±lan 1** (`main.go:472`; `production.yml:44`). Bir dispatch worker `ExecuteAttempt`'i run'Ä±n TÃM ÃMRÃ boyunca tutar (:613-622) |
| **Cordon / Drain / Revoke** | `runner_gateway.go:146,154,170`. **Tek production caller `serveWithGracefulDrain` (SIGTERM) â `Drain` â `Cordon`** (`main.go:351,436`). `Revoke()` ve `Resume()`'un production caller'Ä± **YOK** |
| **Kimlik kaydÄ±** | `identity atomic.Pointer[RunnerIdentity]` (:83) â **tek slot, son yazan kazanÄ±r.** `palai local doctor`'Ä±n okuduÄu Åey budur |
| **Shell seam'i** | `toolbroker.ShellRunner` = **tek metot**: `Run(ctx, ShellCommand) (ShellResult, error)`; `ShellCommand{Argv, WorkspaceRoot, ReadOnly, Shell}` (`packages/tool-broker/sandbox_exec.go:56-67`). **Tamamen serileÅtirilebilir â relay'lenebilir** |
| **Posture Ã§Ã¶zÃ¼mÃ¼** | `resolveShellPosture` (`main.go:740-753`) + `shellRunnerFromEnv` (:768-795), `main.go:71` ve `:603`'te **control plane process'inde**. `PALAI_SANDBOX_IMAGE` XOR `PALAI_SHELL_NATIVE=unsandboxed-host` |
| **Dosya tool'u** | `FileTool` â `workspace.NewWorkspaceFS(env.WorkspaceRoot)` (`execution/tools/file.go:48`) â **control plane'in diskinde** |
| **PaylaÅÄ±lan workspace** | `orch.SetWorkspaceProvisioner(root, ...)`, `PALAI_WORKSPACE_ROOT` (`main.go:596-597`). Tavan `main.go:591-595`'te **adÄ±yla yazÄ±lÄ±** |
| **Mac deployment'Ä±** | `docs/operations/palai-on-a-mac.md:230,238-242`: *"only the control plane goes native"*, *"`--native` â¦ selects **where the control plane runs** â nothing else"*. Runner container'da kalÄ±r |
| **Split-VM kanÄ±tÄ±** | `scripts/package/runner/splitvm-proof.sh:1-16` â **workspace'siz** bir run. `docs/operations/runner-host.md` workspace'ten hiÃ§ bahsetmiyor |
| **Park + uyandÄ±rma** | E23 T1'in koreografisi: `checkpointBeforePause` â `ApplyRunTransition(RunCmdWait)` â dÄ±ÅarÄ±dan `applyResumeTx` (`waiting â running` + `EnqueueJob("response.run")`). **E24 bunu ikinci kez kullanÄ±r** |
| **Capability worker dÃ¼zlemi** | `workers/*` + `capability_workers`/`capability_jobs` (mig 000040). Claim: `ReadyCapabilityJob` (`storage/queries/workers.sql:103-117`) |
| **Tenancy disiplini** | `palai_apply_tenant_policy` (mig `000029:45`), boot sweep (`000029:65-82`), `allTables` (`migration_test.go:29`), `TestEveryTenantTableIsRowLevelSecured` (`tenancy_test.go:242-249`) |
| **Append-only deseni** | `capability_jobs`: `GRANT SELECT, INSERT` + `REVOKE UPDATE, DELETE` (mig `000040` sonu). **R4 bunu AYNEN kopyalar** |
| **UAT** | `committedBundleSurfaces` (`evidence.go:2721`) **22 kayÄ±t**, `evidence/releases/` **22 dizin** (ikisi de sayÄ±ldÄ± 2026-07-29). Case prefix'leri: `A2A AGT API APV AUT CAS DEL DET DR ENG EXT HIL KNO LP MCI MOD OPS PER QUA REC REG REP SAN SEC SES SLK SUB TLM TOL UI WRK` â **`FLT-` boÅta** |
| **Promote dispatch** | `PromoteGateFor` (`promote.go:66`) **E23'Ã¼ Ä°LK** dispatch ediyor (`carriesE23ToolApprovalCase`) |

## Â§3.5 SAPMA TABLOSU â gerÃ§ek kontrat Ã varsayÄ±mlarÄ±mÄ±z

Her satÄ±r: **yayÄ±mlanmÄ±Å kontrat** (kaynak + Ã§ekim tarihi) â **bizim varsayÄ±mÄ±mÄ±z / aÄaÃ§taki durum** â
**hangi task kapatÄ±r**. **UNCONFIRMED satÄ±rlar koda VARSAYIM olarak GÄ°RMEZ.**

| # | GerÃ§ek kontrat | VarsayÄ±m / aÄaÃ§taki durum | Task |
|---|---|---|---|
| **P1** | **â­â­ D1'Ä°N KAYNAÄI, BÄ°REBÄ°R.** *"An environment worker is a process you run on your own infrastructureâ¦ The `self_hosted` environment acts as a work queue: when a session is assigned to it, Anthropic enqueues the session as a work item. Your worker claims work items from that queue, spawns an execution context for each one, downloads the agent's skills, runs the tool calls, and posts the results back."* (https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes, Ã§ekildi 2026-07-29) | **Environment Ä°ÃÄ°NDE routing'den bahseden tek cÃ¼mle yok** â sayfanÄ±n tamamÄ± Ã§ekildi ve arandÄ±. YerleÅtirme primitifi environment'Ä±n KENDÄ°SÄ°. `WorkerSpec.PoolLabel` (`workers/types.go:59`) bunun runner dÃ¼zlemindeki karÅÄ±lÄ±ÄÄ± DEÄÄ°L (Â§3.6 D1) | **T2** |
| **P2** | *"Work items are claimed by polling the environment's queue: either by an **always-on worker** that polls continuously, or a **webhook-triggered handler** that wakes on `session.status_run_started` and starts polling."* (aynÄ± kaynak) | **Bizim runner'Ä±mÄ±z polling YAPMIYOR** â outbound WebSocket aÃ§Ä±p **park ediyor** ve control plane ona `lease.offer` **push ediyor** (`runner_gateway.go:342-402`). Bu farklÄ± ve **bizimki daha iyi**: kuyruk derinliÄini poll aralÄ±ÄÄ± belirlemiyor. **E24 push modelini KORUR**, poll'a geÃ§mez; havuz seÃ§imi push tarafÄ±nda yapÄ±lÄ±r | **T2, T4** |
| **P3** | **â­ Ä°KÄ° CREDENTIAL, VE BÄ°ZÄ°M D2'MÄ°ZÄ°N KAYNAÄI.** *"**Two credentials:** an environment key (generated in the Console in the steps that follow) authenticates the worker to its queue; your Claude API key creates sessions and reads queue stats from outside the worker host. Key generation is Console-only."* Ve `export ANTHROPIC_ENVIRONMENT_KEY="sk-ant-oat01-..."` / `ANTHROPIC_ENVIRONMENT_ID="env_..."` (aynÄ± kaynak) | **AyrÄ±m aynen alÄ±nÄ±r ve BÄ°ZDE DAHA GÃÃLÃ olur:** onlarÄ±n environment key'i worker'Ä±n **sÃ¼rekli** kimliÄidir; bizimki yalnÄ±z **bir sertifika mintler** ve sonra kullanÄ±lmaz. Yani sÄ±zmÄ±Å bir environment key kuyruÄa sÃ¼resiz eriÅimdir; sÄ±zmÄ±Å bir Palai havuz anahtarÄ± **bir enrollment**tÄ±r ve iptal edilebilir. **"Key generation is Console-only" bizde CLI-only'dir** (Â§0.2), Ã§Ã¼nkÃ¼ konsolda auth yok | **T3** |
| **P4** | *"Setting `ANTHROPIC_API_KEY` on the worker host **exposes an organization-scoped credential to agent tool calls**."* (aynÄ± kaynak) | **Vendor'Ä±n kendi uyarÄ±sÄ±, bizim Â§2 invariant'Ä±mÄ±zÄ±n aynÄ±sÄ±.** Palai'de credential broker CP-side'dÄ±r (`main.go:587`) ve E24'Ã¼n relay'i bunu **bozamaz** â RED-first bir bayt sÃ¼pÃ¼rmesi olarak yazÄ±lÄ±r | **T7** |
| **P5** | **â­ D4'ÃN KAYNAÄI.** *"Then write a spawn script that forwards session details into a fresh sandbox. The poller injects `ANTHROPIC_SESSION_ID`, `ANTHROPIC_WORK_ID`, `ANTHROPIC_ENVIRONMENT_ID`, and `ANTHROPIC_ENVIRONMENT_KEY` into the script's environment."* + platform entegrasyonlarÄ± listesi: AWS Lambda MicroVMs, Blaxel, Cloudflare, Daytona, E2B, GKE Agent Sandbox, Modal, Namespace, Superserve, Vercel (aynÄ± kaynak) | **On entegrasyonun hepsi TEK bir spawn seam'i.** Core'a hiÃ§biri girmiyor. **E24 bu seam'i AÃMIYOR â E26 aÃ§Ä±yor** (Â§5), ve gerekÃ§e (a)'da: spawn edilen makinenin gelmesini **bekleyebilen** bir run olmadan spawn anlamsÄ±zdÄ±r | **E26** |
| **P6** | *"**A Linux host** with `/bin/bash` at that exact path. The worker's bash tool invokes it directly, without consulting `PATH`."* (aynÄ± kaynak) | **Rakibin self-hosted worker'Ä± Linux-only, kendi dokÃ¼manÄ±yla.** Owner'Ä±n Ã¼rÃ¼n tezi (*"bir Mac'te Mac Ã¼rÃ¼nleri"*) tam olarak rakibin yapmadÄ±ÄÄ± Åey. **Ama bu farklÄ±laÅtÄ±rÄ±cÄ± bugÃ¼n Palai'de de YOK** â bir Mac runner Mac tool'u koÅturmuyor (Â§3.6 D9). **T7 farklÄ±laÅtÄ±rÄ±cÄ±yÄ± gerÃ§ek yapan task'tÄ±r** | **T7** |
| **P7** | *"Use `work.stop` to ask the worker handling a specific session to shut it down. By default the work item moves to `stopping`: **the worker notices on its next lease heartbeat**, cancels the session's in-flight tool call, and confirms the shutdown, at which point the work item becomes `stopped`. Pass `force: true` â¦ to mark the work item `stopped` immediately."* (aynÄ± kaynak) | **Ä°ki aÅamalÄ± durdurma â nazik + zorla â ve bizde tam karÅÄ±lÄ±ÄÄ± var:** `Cordon` (nazik) / `Revoke` (zorla), `runner_gateway.go:143-157`. **Ama ikisinin de production caller'Ä± yok** (Â§3.6 D15). Vendor'Ä±n `stopping â stopped` ayrÄ±mÄ± T5'in state makinesinin Åeklidir | **T5** |
| **P8** | *"`reclaim_older_than_ms`: re-claim work items that were claimed but never acknowledged within this many milliseconds."* â SDK Ã¶rneklerinde **2000** (aynÄ± kaynak) | **Bizde karÅÄ±lÄ±ÄÄ± `RedispatchForRetry` + lease fence** (`workers/store.go`), ama **Ã§aÄÄ±ranÄ± yok** (`known-gaps` `WRK-2`). Runner dÃ¼zleminde karÅÄ±lÄ±ÄÄ± `active` sayacÄ± + E10 recovery. **E24 yeni bir reclaim yazmaz**, runner id'ye anahtarlar | **T5** |
| **P9** | **â­ 24 SAAT APPLE'IN ÅARTIDIR, SATICI TERCÄ°HÄ° DEÄÄ°L â birebir:** *"Billing is per second, with a **24-hour minimum allocation period for the Dedicated Host to comply with the Apple macOS Software License Agreement**."* ve *"At the end of the 24-hour minimum allocation period, the host can be released at any time with no further commitment."* (https://aws.amazon.com/ec2/instance-types/mac/faqs/, Ã§ekildi 2026-07-29) | **D5 doÄrulandÄ± ve gerekÃ§esi lisans.** Mac havuzunun bir **tabanÄ±** ve saatlerle Ã¶lÃ§Ã¼len bir sÃ¶nme sÃ¼resi olur. **Ama bu E26'in problemi** â E24 yalnÄ±z havuzun `min_size`'Ä±nÄ± **taÅÄ±r**, kullanan yoktur | **E26**, Â§5 |
| **P10** | **â­ D5'Ä°N Ä°ÃÄ°NDE ÃLÃÃLMÃÅ SÃRPRÄ°Z, VE YÃNLENDÄ°RMENÄ°N "saniyeler / ~1 dakika" TABLOSUNU DÃZELTÄ°YOR:** *"For an AWS vended AMI with a x86 Mac instance or a Apple silicon Mac instance, **the launch time can range from approximately 6 minutes to 20 minutes**."* AyrÄ±ca: *"Mac instances are available only as bare metal instances on Dedicated Hosts, with a **minimum allocation period of 24 hours before you can release the Dedicated Host**. You can launch one Mac instance per Dedicated Host."* ve *"The **unit of billing is the dedicated host**. The instances running on that host have no additional charge."* (https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-mac-instances.html, Ã§ekildi 2026-07-29) | **AÃILIÅ SÃRESÄ° 20 SANÄ°YELÄ°K DIAL BÃTÃESÄ°NÄ°N 18â60 KATI** (`orchestrator.go:38`). Retry ladder'Ä±yla birlikte ~2.5 dakikada dead-letter (Â§3.6 D12). **Yani "yÃ¼k gelince Mac aÃ§" bugÃ¼n YAPISAL OLARAK ULAÅILAMAZ, ekonomik olarak deÄil.** Bu satÄ±r T4'Ã¼n (kapasite parkÄ±) var olma gerekÃ§esidir | **T4** |
| **P11** | **Scaleway API bir ALAN olarak veriyor:** `"deletable_at": "2022-03-22T12:34:56.123456Z"`, ve *"Apple silicon-as-a-Service comes with a minimum allocation period of 24 hours."* (https://www.scaleway.com/en/developers/api/apple-silicon, Ã§ekildi 2026-07-29) | **Taban bir TIMESTAMP olarak okunabiliyor, hesaplanmasÄ± gerekmiyor.** Bir scaler `now + 24h` hesaplamak yerine `deletable_at` okumalÄ±dÄ±r â Ã§Ã¼nkÃ¼ saat kaymasÄ± ve saÄlayÄ±cÄ± farkÄ± hesabÄ± bozar. **E26'in ilk satÄ±rÄ± bu olur** | **E26** |
| **P12** | **UNCONFIRMED:** Scaleway'in "bir dakikanÄ±n altÄ±nda baÅlatma" iddiasÄ± ve 24 saatte otomatik silme seÃ§eneÄi â FAQ sayfasÄ± (https://www.scaleway.com/en/docs/apple-silicon/faq/, Ã§ekildi 2026-07-29) **yalnÄ±z navigasyon dÃ¶ndÃ¼rdÃ¼**, `how-to/delete-mac-mini` de Ã¶yle | **Koda VARSAYIM olarak GÄ°RMEZ.** E24 hiÃ§bir aÃ§Ä±lÄ±Å sÃ¼resi sabiti taÅÄ±maz; T4'Ã¼n parkÄ± **sÃ¼resiz**dir ve bir sÃ¼re sabitine dayanmaz. ÃlÃ§Ã¼m Â§6'dadÄ±r | **Â§6** |
| **P13** | **UNCONFIRMED:** Anthropic'in `Environments Work` endpoint'lerinin **eÅzamanlÄ±lÄ±k/kota** semantiÄi â bir environment'Ä±n kaÃ§ work item'Ä± aynÄ± anda claim edilebilir, ve claim'lerin sÄ±rasÄ± FIFO mu | **Koda VARSAYIM olarak GÄ°RMEZ.** E24 kendi sÄ±rasÄ±nÄ± **aÃ§Ä±kÃ§a** seÃ§er ve yazar (T2: havuz iÃ§inde `created_at` FIFO), rakibin varsayÄ±lan davranÄ±ÅÄ±nÄ± taklit etmez | **T2**, Â§6 |
| **P14** | **DEVRALINAN (E22 Â§3.5):** macOS'ta `simctl --set` bir **argv** bayraÄÄ±dÄ±r ve argv modele aittir; per-session device set **tavsiyedir, zorlama deÄil** (`docs/research/macos-isolation-without-accounts.md` Â§6) | **E24 bunu DEÄÄ°ÅTÄ°RMÄ°YOR ve deÄiÅtirdiÄini iddia etmiyor.** Bir Mac havuzu tenant kapsamlÄ±dÄ±r (RLS), ama bir Mac'in **iÃ§i** hÃ¢lÃ¢ tek uid'dir. `MAC-P6` aÃ§Ä±k kalÄ±r ve Â§0.1 owner'a sorar | Â§0.1, Â§6 |

## Â§3.6 AÄACIN KENDÄ° SAPMALARI

**On altÄ± satÄ±r.** Ä°kisi yÃ¶nlendirmenin premise'ini tersine Ã§eviriyor, biri epic'in tamamÄ±nÄ±n Åeklini
belirliyor, ve **dÃ¶rdÃ¼ aynÄ± yanlÄ±Å cÃ¼mlenin dÃ¶rt kopyasÄ±dÄ±r.**

| # | TaÅÄ±nan inanÃ§ | AÄaÃ§taki gerÃ§ek (file:line ile) | SonuÃ§ |
|---|---|---|---|
| **D1** | **YÃ¶nlendirmenin D1'i:** *"`WorkerSpec.PoolLabel` zaten var ve hiÃ§bir kararda kullanÄ±lmÄ±yor; `OS`, `Arch`, `Capacity` de aynÄ± Åekilde Ã¶lÃ¼."* | **DÃRDÃ DE ÃLÃ, AMA YANLIÅ DÃZLEMDE â VE BU AYRIM EPIC'Ä°N ÅEKLÄ°NÄ° DEÄÄ°ÅTÄ°RÄ°YOR.** `WorkerSpec` (`workers/types.go:52-61`) **capability-worker** dÃ¼zlemine aittir; `capability_workers` tablosunda `os`, `arch`, `capacity`, `pool_label` kolonlarÄ± gerÃ§ekten var (mig `000040`) ve claim predikatÄ± onlarÄ± hiÃ§ gÃ¶rmÃ¼yor (`storage/queries/workers.sql:111`: `WHERE organization_id = $1 AND project_id = $2 AND capability = $3`); indeks bile onlarÄ± taÅÄ±mÄ±yor (`capability_workers_capability_idx`, mig `000040:56-57` â **ve o indeks de Ã¶lÃ¼**: hiÃ§bir sorgu worker'Ä± capability'ye gÃ¶re seÃ§miyor, yalnÄ±z `WHERE id = $1`, `workers.sql:18`). **`Capacity` ise Ã¶lÃ¼den beter â bir sÄ±nÄ±r GÄ°BÄ° duruyor ve deÄil:** `ClaimNext` hiÃ§bir Åey saymÄ±yor (`store.go:175-229`, tek dal `worker.Health != "healthy"` :180) ve `handleClaim` `sess.claims`'e kardinalite kontrolÃ¼ olmadan ekliyor (`gateway.go:190`), yani **`capacity: 1` beyan etmiÅ bir worker N lease tutabilir.** **AMA AJANLARINIZI KOÅTURAN DÃZLEM O DEÄÄ°L.** Run'lar runner dÃ¼zleminden geÃ§er ve orada bu alanlar **Ã¶lÃ¼ deÄil, YOK**: `AttemptDescriptor` (`engine_channel.go:13-33`) ne label ne os ne arch taÅÄ±r, `enrollRequest` (`runner_gateway.go:218-221`) ne posture ne havuz. **Yani "PoolLabel'Ä± yÃ¼k taÅÄ±r hale getir" yanlÄ±Å dÃ¼zlemin Ã¶lÃ¼ alanÄ±nÄ± diriltirdi ve tek bir run'Ä± bile yÃ¶nlendirmezdi** | **T2**: havuz runner dÃ¼zleminde YENÄ°DEN kurulur; `workers` paketi **dokunulmaz** |
| **D2** | *"Havuz iÃ§inde claim zaten FIFO; E24 sadece havuzu ekliyor."* | **FIFO DEÄÄ°L.** `ReadyCapabilityJob` (`storage/queries/workers.sql:116-117`) birebir `ORDER BY latest.job_id` + `LIMIT 1` â **opak, rastgele bir id Ã¼zerinde sÃ¶zlÃ¼k sÄ±rasÄ±.** `job_id` `mintID("cjob")` â `middleware.NewID` (`api/middleware/request_context.go:40-44`) = prefix + `crypto/rand` 16 bayt hex, yani **monoton deÄil** â en eski iÅ deÄil, **hex'i en kÃ¼Ã§Ã¼k** iÅ kazanÄ±r ve **her poll'da yeniden kazanÄ±r: bu bir sÄ±ra deÄil, bir aÃ§lÄ±k (starvation).** SÄ±ralanabilecek iki kolon tabloda ZATEN var â `created_at` (mig `000040:98`) ve `entry_seq` (`:80`) â **ikisi de kuyruk sÄ±rasÄ± iÃ§in hiÃ§ kullanÄ±lmÄ±yor.** **FIFO bir korunan davranÄ±Å deÄil, YENÄ° bir davranÄ±ÅtÄ±r** ve T2 onu aÃ§Ä±kÃ§a seÃ§ip yazmak zorundadÄ±r | **T2** |
| **D3** | *"Claim bir kuyruktur."* | **BÄ°R POLL'DUR VE KAYBEDENÄ° VARDIR.** Sorguda `FOR UPDATE SKIP LOCKED` **yok**; at-most-once garantisi append anÄ±ndaki fence predikatÄ± (`workers.sql:65-66`) + `UNIQUE (job_id, entry_seq)` Ã§akÄ±ÅmasÄ±ndan (mig `000040:102`) geliyor. **Ve kaybedene ne sÃ¶ylendiÄi daha kÃ¶tÃ¼:** Ã§akÄ±Åma `errFenceGuardMiss`'e (`store.go:510-512`) dÃ¶nÃ¼yor ve orada **"claim yok"a yutuluyor** (`store.go:213-215`) â yani kaybeden poller *"iÅ var, sen kaptÄ±rdÄ±n"* deÄil *"iÅ yok"* duyuyor ve geri Ã§ekiliyor. N poller â iÅ baÅÄ±na N-1 kaybeden, ve hepsi yanlÄ±Å bilgilendirilmiÅ. Sorgunun **kendi ponytail notu** tavanÄ± yazmÄ±Å (`workers.sql:99-101`): *"DISTINCT ON scans the capability's journal per claim â fine at fixture/reference scale; a ready-jobs materialized view or a status column is the upgrade **if a real fleet polls hard**."* **E24 tam olarak o filodur** â ve bu, T2'nin runner dÃ¼zleminde **push** modelini korumasÄ±nÄ±n (P2) ikinci gerekÃ§esidir | **T2** (reddedilen seam) |
| **D4** | **â­ YÃ¶nlendirmenin D2'si:** *"BugÃ¼n `EnrollmentTokens.Consume` tek kullanÄ±mlÄ±k â gÃ¼venli ama filo Ã¶lÃ§eÄinde makine baÅÄ±na token mintlemek gerekir."* | **PREMISE TERS. AÄAÃ ZATEN YENÄ°DEN KULLANILABÄ°LÄ°R BÄ°R ANAHTARA GEÃMÄ°Å, VE BUNU DÃRT YERDE Ä°NKÃR EDÄ°YOR.** Tek Ã¼retim implementasyonu `FileEnrollmentTokens`'Ä±n baÅlÄ±ÄÄ± birebir: ***"WHY THIS IS NOT ONE-USE, AND WHAT REPLACED THAT"*** (`local_credentials.go:97`) â bir sertifika Ã¶mrÃ¼ baÅÄ±na bir mint (`minInterval`, :163-166), sÃ¼resi dolmuÅ kimliÄin tek kurtuluÅ yolu. **Ama "tek kullanÄ±mlÄ±k" cÃ¼mlesi aÄaÃ§ta DÃRT KEZ yazÄ±lÄ±:** interface yorumu (`runner_gateway.go:40-44`, *"an unknown or already-spent token"*), CLI (`cmd/cli/internal/stack/lifecycle.go:118`, *"mints a fresh **one-use** runner enrollment token"*), runner'Ä±n kendisi (`cmd/runner/main.go:68-69`, *"the one-use bootstrap token is spent once â¦ and never presented again"* â **yirmi satÄ±r altÄ±nda yeniden-enroll fonksiyonunu kuran dosya**), ve compose yorumu (`compose.yaml:168-171`, **tek doÄru olan** â *"re-presented ONLY if the runner's identity expires â¦ the control plane rate-limits it to one certificate per issued lifetime"*). **E24'Ã¼n eklediÄi Åey yeniden kullanÄ±labilirlik DEÄÄ°L: kapsam, hash, son kullanma, iptal ve KAYIT** | **T3.** DÃ¶rt kopyanÄ±n Ã¼Ã§Ã¼ dÃ¼zeltilir |
| **D5** | **YÃ¶nlendirmenin sorusu:** *"AnahtarÄ± iptal etmek enroll olmuÅ makineleri Ã§alÄ±ÅÄ±r bÄ±rakmalÄ± â bu Ã¶zellik nasÄ±l garanti edilir?"* | **ZATEN GARANTÄ°, VE BEDAVA â sebep yapÄ±sal.** Yenileme `handleRenew` (`runner_gateway.go:265-284`) **mevcut mTLS kimliÄiyle** doÄrulanÄ±r ve `tokens.Consume` o yolda **hiÃ§ Ã§aÄrÄ±lmaz**; sadece `handleEnroll` (:233) Ã§aÄÄ±rÄ±r. Token dosyasÄ±nÄ± silmek yalnÄ±z (a) yeni enrollment'Ä± ve (b) *sÃ¼resi dolmuÅ kimlik* kurtarmasÄ±nÄ± kapatÄ±r. **Yani Ã¶zellik VAR; olmayan Åey, hangi anahtarÄ±n hangi sertifikayÄ± mintlediÄini KAYDEDEN yerdir** â bu yÃ¼zden iptal bugÃ¼n **hedeflenemez**, yalnÄ±z topyekÃ»ndur. R2'nin `enrolled_via_key_id`'si tam olarak o eksik kayÄ±ttÄ±r | **T3** |
| **D6** | *"`minInterval` sÄ±zmÄ±Å bir anahtarÄ± sÄ±nÄ±rlar."* | **HACMÄ° SINIRLAR, KÄ°MLÄ°ÄÄ° DEÄÄ°L â VE BELLEKTE YAÅAR.** `lastIssued map[string]time.Time` (`local_credentials.go:127`) process iÃ§idir: **bir control-plane restart'Ä± sayacÄ± sÄ±fÄ±rlar**, ve `restart: always` ile bir VM reboot'u bunu dÃ¼zenli olarak yapar. Yorumun kendisi de dÃ¼rÃ¼st: *"Be honest about what minInterval is: a RATE LIMIT, not an exclusion"* (:117). AyrÄ±ca karÅÄ±laÅtÄ±rma `strings.TrimSpace(string(raw)) != token` (:159) â **bir bearer credential'Ä±n sabit-zamansÄ±z karÅÄ±laÅtÄ±rmasÄ±**, ve `crypto/subtle` aÄacÄ±n baÅka yerlerinde zaten kullanÄ±lÄ±yor | **T3** |
| **D7** | *"`runner_id` bir makineyi tanÄ±mlar."* | **KENDÄ° BEYANIDIR, VE COMPOSE ONU SABÄ°TLEMÄ°Å.** `enrollRequest{RunnerID, PublicKey}` (`runner_gateway.go:218-221`) â gateway hangi ad istenirse onu imzalÄ±yor (`runnerDNS(request.RunnerID)`, :247), doÄrulama yok. Ve `deploy/compose/runner-entrypoint.sh:10` birebir: `export PALAI_RUNNER_ID="runner-local"`. **`docker compose up --scale runner=3` Ã¼Ã§ makineye tek bir ad verir**, Ã¼Ã§Ã¼ de `runner-local.runners.palai.internal` iÃ§in sertifika alÄ±r. Bir filoda "runner X'i iptal et" ancak X'i **sunucu** mintlerse anlamlÄ±dÄ±r | **T1 + T3** |
| **D8** | **â­ YÃ¶nlendirmenin kÄ±sÄ±tÄ±:** *"RLS tenancy holds everywhere; a pool and its enrollment key are tenant-scoped."* | **DB SATIRLARINDA HOLD EDÄ°YOR; RUNNER DÃZLEMÄ°NDE TENANT KAVRAMI HÄ°Ã YOK.** Enrollment org/project taÅÄ±mÄ±yor (:218-221); `AttemptDescriptor` taÅÄ±mÄ±yor (`engine_channel.go:13-33`); `leaseOffer` `image_digest` + `limits` + workspace yolundan baÅka bir Åey taÅÄ±mÄ±yor (`runner_gateway.go:564-586`); `Dial` hiÃ§bir tenant kontrolÃ¼ yapmÄ±yor (:368-416). **SonuÃ§, aÃ§Ä±kÃ§a sÃ¶ylenmesi gereken hÃ¢liyle: bugÃ¼n enroll olmuÅ HERHANGÄ° bir runner, HERHANGÄ° bir tenant'Ä±n attempt'ini alabilir.** Tek runner'lÄ± topolojide bu bir bulgu deÄil bir tanÄ±mdÄ±r; **iki mÃ¼Återinin Mac'i olduÄu anda bir aÃ§Ä±ktÄ±r.** Ä°yi haber: `tenant` Ã§aÄrÄ± yerinde zaten yerel bir deÄiÅken (`orchestrator.go:393` civarÄ±), yani threading ucuz | **T3** (kÄ±sÄ±t), **T4** (yerleÅtirme) |
| **D9** | **â­â­ YÃ¶nlendirmenin D7'si:** *"Bir run'Ä±n nerede koÅtuÄu boot-time env'den config'e taÅÄ±nmalÄ± â posture per-pool olabilir mi?"* | **SORU DAHA DERÄ°N BÄ°R ÅEYÄ° AÃIYOR: BÄ°R RUNNER TOOL KOÅTURMUYOR.** Runner'Ä±n aldÄ±ÄÄ± `lease.offer` `image_digest` taÅÄ±r â yani **motor**, model dÃ¶ngÃ¼sÃ¼. Her tool control plane'in process'inde koÅar: shell `orch.SetShellRunner(shellRunnerFromEnv())` ile (`main.go:603`, `:768-795`), dosya tool'u `workspace.NewWorkspaceFS(env.WorkspaceRoot)` ile (`execution/tools/file.go:48`), ve ikisi de CP ile runner'Ä±n **paylaÅtÄ±ÄÄ±** `PALAI_WORKSPACE_ROOT`'a bakar (`main.go:596`). **AÄaÃ§ bunu kendi kelimeleriyle yazmÄ±Å** (`main.go:591-595`): *"the tools run CP-side against the same host allocation the runner bind-mounts. A split CPâ runner deploy â¦ needs a runner-relay seam â the CP-side tool dispatch would ship the file/shell op to the runner that holds the mount â a NAMED FUTURE split-deploy hardening, not built here."* **Ve bugÃ¼nkÃ¼ Mac deployment'Ä± bununla TUTARLI:** `palai-on-a-mac.md:238-242` birebir *"`--native` â¦ selects **where the control plane runs** â nothing else"*, `:230` *"only the control plane goes native"*. **Yani posture'Ä±n CP process'inde Ã§Ã¶zÃ¼lmesi bir kaza deÄil, bugÃ¼nkÃ¼ mimarinin doÄru ifadesidir.** Cevap Â§4'Ã¼n baÅÄ±ndadÄ±r | **T7** |
| **D10** | *"Split-VM bacaÄÄ± off-host bir runner'Ä± kanÄ±tlÄ±yor."* | **WORKSPACE'SÄ°Z BÄ°R RUN Ä°ÃÄ°N KANITLIYOR.** `scripts/package/runner/splitvm-proof.sh:1-16` adÄ±m adÄ±m: *"Create a response over the API and poll it to `completed`"* â klon yok, workspace yok, shell yok. `docs/operations/runner-host.md` **workspace kelimesini hiÃ§ geÃ§irmiyor** (grep, 2026-07-29). Workspace'li bir run runner'a `workspace_host_path` verir â **control plane'in host'undaki mutlak bir yol** â ve off-host runner onu **kendi** dosya sisteminden bind etmeye Ã§alÄ±ÅÄ±r. **Yani shipped off-host topoloji bir coding run'Ä± koÅturamaz; ki bir Mac'in var olma sebebi tam olarak odur** | **T7** |
| **D11** | *"Daha Ã§ok runner ekle, daha Ã§ok ajan koÅsun."* | **KOÅMAZ.** EÅzamanlÄ±lÄ±k `min(PALAI_DISPATCH_WORKERS, Î£ lease slot)`'tur ve **ilki varsayÄ±lan 1**: `workers := envIntDefault("PALAI_DISPATCH_WORKERS", 1)` (`main.go:472`), `production.yml:44` `${PALAI_DISPATCH_WORKERS:-1}`, `compose.yaml:82` `${PALAI_DISPATCH_WORKERS:-0}`. Bir dispatch worker `ExecuteAttempt`'i run'Ä±n **tÃ¼m Ã¶mrÃ¼** boyunca tutar (`main.go:613-622`). **Ä°kinci runner, kimsenin ulaÅamayacaÄÄ± park etmiÅ bir slot ekler.** Ve `E21`'de bulunan `PALAI_RUNNER_CONCURRENCY` boÅluÄu **kapanmÄ±Å**: `compose.yaml:179` bugÃ¼n `${PALAI_RUNNER_CONCURRENCY:-1}` â yani o hafÄ±za artÄ±k geÃ§ersiz, bu satÄ±r onu da dÃ¼zeltiyor | **T4** |
| **D12** | **â­ YÃ¶nlendirmenin D5'i:** *"Mac 24 saatlik faturayla aÃ§Ä±lÄ±r; teknik olarak mÃ¼mkÃ¼n, ekonomik olarak bir gÃ¼n satÄ±n almaktÄ±r."* | **EKONOMÄ°DEN ÃNCE YAPISAL BÄ°R DUVAR VAR, VE ÃLÃÃLDÃ.** `Dial` `dialHandshakeDeadline = 20 * time.Second` ile sÄ±nÄ±rlÄ± (`orchestrator.go:38,390`), retry `MaxAttempts: 5, MaxBackoff: 30s` (`main.go:477`) â **~2.5 dakikada dead-letter.** AWS kendi dokÃ¼manÄ±nda Mac aÃ§Ä±lÄ±ÅÄ±nÄ± *"approximately 6 minutes to 20 minutes"* diyor (P10). **Run, Mac boot etmeden dÃ¶rt kez Ã¶lÃ¼r.** Yani "yÃ¼k gelince Mac aÃ§" bugÃ¼n ekonomik bir tercih deÄil, **ulaÅÄ±lamaz bir davranÄ±Å**. ÃÃ¶zÃ¼m bir timeout bÃ¼yÃ¼tmesi DEÄÄ°L â bir run'Ä±n **park etmesi**, ve o koreografi E23 T1'de yeni yazÄ±ldÄ± | **T4** |
| **D13** | *"Gateway tek-runner'lÄ±dÄ±r."* | **N RUNNER'I BUGÃN DE KABUL EDÄ°YOR.** `handleConnect`'te sayÄ± guard'Ä± yok ve `available` buffersÄ±z bir kanal (`runner_gateway.go:129,342-355`): her park eden runner kendini gÃ¶nderir, her `Dial` birini alÄ±r. **Tek-runner olan Åey ikisi:** (a) `cordoned`/`revoked` **process-global `atomic.Bool`** (:75-76), (b) `identity atomic.Pointer[RunnerIdentity]` (:83) â **tek slot, son yazan kazanÄ±r**, yani iki runner varken `palai local doctor` en son sertifika sunanÄ± okur ve diÄeri hakkÄ±nda hiÃ§bir Åey bilmez. Gateway'in kendi yorumu (:72-74) registry'yi upgrade path olarak adlandÄ±rÄ±yor, ve **`local_credentials.go:122` aynÄ± Åeyi baÄÄ±msÄ±z olarak ikinci kez sÃ¶ylÃ¼yor**: *"Bounding concurrent identities per token needs a runner registry the single-runner SH-0 topology does not have; that is the upgrade path."* Ä°ki dosya, aynÄ± eksik, hiÃ§ konuÅmamÄ±Ålar | **T1** |
| **D14** | *"Mac havuzu iÃ§in `workers` dÃ¼zlemi (capability workers) doÄru ev."* | **ÃÃ BAÄIMSIZ RED, VE HER BÄ°RÄ° TEK BAÅINA YETERLÄ°.** (1) **Listener non-loopback bind'Ä± REDDEDÄ°YOR:** `listenCapabilityWorker` (`main.go:1589-1608`) `0.0.0.0`'Ä±, routable adresi ve isimle verileni bind'dan ÃNCE reddediyor â Ã§Ã¼nkÃ¼ cleartext, ve Ã¼zerinde enrollment token'Ä±, her istekteki workload bearer'Ä± ve **redeem edilmiÅ secret DEÄERÄ°** taÅÄ±nÄ±yor. Kiralanan bir Mac tanÄ±mÄ± gereÄi loopback deÄildir. (2) **DÃ¼zlem Ã¼Ã§ Åekilde uykuda:** token mintleyen yok, `DispatchJob` Ã§aÄÄ±ran yok, health/reaper yok â `known-gaps-1.0.md` `WRK-2`, **E19 T8 tarafÄ±ndan 2026-07-26'da yeniden doÄrulandÄ± ve hÃ¢lÃ¢ aÃ§Ä±k**. (3) **YapÄ±sal olarak tipli-operasyon:** `ErrUntypedOperation` (`workers/types.go:128-130`) genel bir shell'i imkÃ¢nsÄ±z kÄ±lÄ±yor â bir Mac'e verilecek Åey ise tam olarak genel bir shell'dir. (4) **Enrollment'Ä± BELLEKTE ve gerÃ§ekten tek kullanÄ±mlÄ±k:** `Gateway.enrollment map[string]enrollGrant` (`workers/gateway.go:33`), `delete(g.enrollment, token)` ilk kullanÄ±mda (:132) â yani bir control-plane restart'Ä± **her bekleyen enrollment'Ä± siler** ve `IssueEnrollmentToken`'Ä±n (:67-70) zaten operatÃ¶r caller'Ä± yok. **Bu dÃ¼zlemi Mac yoluna Ã§evirmek relay'den DAHA ÃOK iÅ olurdu ve DAHA AZ verirdi** | **Â§5** (adlandÄ±rÄ±lmÄ±Å ret) |
| **D15** | **YÃ¶nlendirmenin D6'sÄ±:** *"Cordon/drain/revoke bugÃ¼n whole-gateway `atomic.Bool`; runner id'ye anahtarla."* | **DOÄRU, VE EKSÄ°K YARISI DAHA KÃTÃ: ÃÃÃNÃN TEK PRODUCTION GÄ°RÄ°ÅÄ° SIGTERM.** AÄaÃ§ genelinde `.Revoke()` ve `.Resume()`'un **hiÃ§bir production caller'Ä± yok**; `.Cordon()`'un tek Ã§aÄÄ±ranÄ± `Drain`'in kendi ilk satÄ±rÄ± (`runner_gateway.go:171`); `.Drain(` tek Ã§aÄÄ±ranÄ± `serveWithGracefulDrain` (`main.go:351,436`), o da SIGTERM'de. **Yani `Revoke` â SAN-011'in "hard stop"u, ele geÃ§irilmiÅ bir runner iÃ§in yazÄ±lmÄ±Å olan â testlerle kanÄ±tlÄ± ve kimse tarafÄ±ndan ulaÅÄ±lamaz.** ÃstÃ¼ne `revoked` bellek iÃ§i bir `atomic.Bool`: **bir restart iptali siler.** Bir filoda "Åu Mac'i devre dÄ±ÅÄ± bÄ±rak" bir CLI komutu ve **kalÄ±cÄ± bir satÄ±r** ister | **T5** |
| **D16** | *"Bir migration'Ä±n numarasÄ± dosya adÄ±ndadÄ±r, dolayÄ±sÄ±yla tektir."* | **EN AZ BÄ°R KEZ DEÄÄ°LDÄ°, VE T1 AYNI TUZAÄIN ÃNÃNDEN GEÃÄ°YOR.** Dosya `storage/migrations/000040_capability_workers.up.sql`, ama **kendi baÅlÄ±ÄÄ± 000039 diyor** (`:1`, ve `:15-20` renumber talimatÄ±nÄ± geÃ§miÅ zamanla anlatÄ±yor), `store.go:17` 000039 diyor, `workers.sql:4` 000039 diyor, ve testin **adÄ±** `TestMigration39CapabilityWorkerTables` (`workers_component_test.go:181`). YalnÄ±z dosya adÄ±, `VALUES (40)` ve embed deÄiÅkeni taÅÄ±ndÄ±. **`git mv` eski iÃ§eriÄi stage'ler ve sonraki Edit'ler dÃ¼Åer** â aÄacÄ±n kendi hafÄ±zasÄ±ndaki desen. **T1 tek migration sahibidir ve 000045'i aÃ§arken numarayÄ± DOSYA ADINDA, BAÅLIKTA, `VALUES`'ta, embed'de, `store.go` yorumunda ve TEST ADINDA aynÄ± commit'te taÅÄ±mak zorundadÄ±r**; doÄrulama `git show HEAD` Ã¼zerinden yapÄ±lÄ±r, working-tree grep'i Ã¼zerinden deÄil | **T1** |

---

## Â§4 â Task breakdown

### D9'un cevabÄ±, ve bu epic'in Åekli

**YÃ¶nlendirmenin en Ã§ok doÄru cevap istediÄi soru buydu, cevap Åudur:**

> **Posture bugÃ¼n per-pool OLAMAZ, ve sebebi "boot'ta okunuyor" deÄildir. Sebep, runner'Ä±n tool
> koÅturmuyor olmasÄ±dÄ±r.** Posture (`PALAI_SANDBOX_IMAGE` XOR `PALAI_SHELL_NATIVE`) *doÄru yerde*
> yaÅÄ±yor: tool'larÄ± Ã§alÄ±ÅtÄ±ran process'te. Bir "Mac havuzu" bugÃ¼nkÃ¼ anlamda kurulsa, o havuza dÃ¼Åen
> run **model dÃ¶ngÃ¼sÃ¼nÃ¼** bir Mac'te koÅturur ve `xcodebuild`'i hÃ¢lÃ¢ control plane'in makinesinde
> Ã§alÄ±ÅtÄ±rÄ±r â yani hiÃ§bir Åey kazandÄ±rmaz. **Bu yÃ¼zden posture zorunlu olarak runner/worker tarafÄ±na
> aittir, ama ancak icra oraya taÅÄ±ndÄ±ktan SONRA.** TaÅÄ±nmadan Ã¶nce "per-pool posture" eksik bir
> Ã¶zellik deÄil, **anlamsÄ±z bir cÃ¼mledir.**

**Ve taÅÄ±nma sanÄ±ldÄ±ÄÄ± kadar pahalÄ± deÄil, Ã§Ã¼nkÃ¼ seam ZATEN dar ve ZATEN tek:**
`toolbroker.ShellRunner` **tek metotludur** â `Run(ctx, ShellCommand) (ShellResult, error)` â ve
`ShellCommand{Argv, WorkspaceRoot, ReadOnly, Shell}` tamamen serileÅtirilebilir
(`packages/tool-broker/sandbox_exec.go:56-67`). Yani shell yarÄ±sÄ± **var olan bir arayÃ¼zÃ¼n ikinci bir
implementasyonudur**: frame'i lease baÄlantÄ±sÄ±ndan gÃ¶nder, sonucu bekle. Orchestrator, ledger, onay
kapÄ±sÄ±, hook'lar â hepsi CP-side kalÄ±r ve **bit-deÄiÅmezdir**. PahalÄ± olan yarÄ± workspace'tir: klon,
dosya op'larÄ±, snapshot ve changeset bugÃ¼n CP'nin diskinde. **T7 ikisini birden taÅÄ±r ve bu epic'in en
bÃ¼yÃ¼k task'Ä±dÄ±r.**

**DAG (cap 3):**

```
Wave 1: T1 (registry + mig 000045)
Wave 2: T2 (havuzlar + yerleÅtirme sÄ±rasÄ±; T1)   T3 (havuz anahtarÄ±; T1)
Wave 3: T4 (yerleÅtirme + tenant + kapasite parkÄ±; T2+T3)   T5 (keyed cordon/drain/revoke; T1)
Wave 4: T6 (strict mode + CLI + rotalar; T3+T5)   T7 (Ä°CRA RELAY'Ä°; T4)
Wave 5: T8 (EXIT gate; hepsine baÄlÄ±)
```

**T1 TEK MIGRATION SAHÄ°BÄ°DÄ°R.** DiÄer yedi task `storage/migrations/` altÄ±na dosya koymaz â "iki
paralel task'Ä±n ikisi de 000045'i alÄ±r" tuzaÄÄ± bir dikkat kuralÄ±yla deÄil **yapÄ±sal olarak** imkÃ¢nsÄ±z
kÄ±lÄ±nÄ±r.

Her paralel merge sonrasÄ± **`go vet -tags="component live" ./...`**, ve **case.yaml / migration / yeni
tenant tablosu dokunuÅunda `tests/uat/automation` + `tests/security/tenancy` corpora'sÄ± KOÅULUR**
(T1 **dÃ¶rt** yeni tenant tablosu aÃ§Ä±yor â dÃ¶rdÃ¼ iÃ§in de `allTables` + `palai_apply_tenant_policy` +
`GRANT` + tenancy corpus **zorunlu**). Her task RED-first TDD + green milestone baÅÄ±na commit +
`git push origin main`.

**SECURITY-CRITICAL (full review): T1, T2, T3, T4, T5, T6, T7.**

---

### T1 â Runner registry: gateway'in kendi notunun tarif ettiÄi tablo (**mig 000045**; SECURITY-CRITICAL)

**BU TASK'I Ä°KÄ° DOSYA BAÄIMSIZ OLARAK ISMARLAMIÅ.** `runner_gateway.go:73` birebir *"there is no
hosts/runners table in this tier â¦ that is the SaaS/post-SH-0 upgrade path"*, ve `local_credentials.go:122`
birbirinden habersiz ikinci kez: *"Bounding concurrent identities per token needs a runner registry the
single-runner SH-0 topology does not have; that is the upgrade path."*

- [ ] **RED Ã¶nce (1):** iki runner enroll eder; registry **iki satÄ±r** taÅÄ±r ve ikisi **ayrÄ±** id'lidir.
      BugÃ¼n `identity atomic.Pointer` tek slot (D13) â test **RED doÄar.**
- [ ] **RED Ã¶nce (2), KÄ°MLÄ°K:** iki makine **aynÄ±** `runner_id`'yi talep eder; **ikisi de kendi
      kimliÄini alÄ±r**, ikisi de ayrÄ± satÄ±rdÄ±r, ve hiÃ§biri diÄerinin sertifikasÄ±nÄ± geÃ§ersiz kÄ±lmaz.
      BugÃ¼n ikisi de `runner-local` iÃ§in sertifika alÄ±r ve kayÄ±t tutulmaz (D7).
- [ ] **RUNNER ID'SÄ° SUNUCU MINTLER** (`middleware.NewID` deseni, `rnr_` prefix'i). Ä°stemcinin
      gÃ¶nderdiÄi `runner_id` artÄ±k **bir etiket**tir (`runners.label`), bir kimlik deÄil; sertifikanÄ±n
      DNS'i sunucunun mintlediÄi id'den tÃ¼retilir. **Eski runner'lar iÃ§in geriye uyumluluk:** id
      gÃ¶ndermeyen ya da gÃ¶nderen fark etmez â sunucu her hÃ¢lde kendi id'sini mintler ve enroll cevabÄ±na
      **ekler**, runner onu `renew`'de taÅÄ±r.
- [ ] **Migration 000045, altÄ± rider** (Â§1 R1âR6). **DÃ¶rt yeni tenant tablosu**; her biri kendi
      `palai_apply_tenant_policy` + `GRANT`'ini taÅÄ±r (000029'un sweep'i ve blanket grant'i bu boot'ta
      **geÃ§ kalÄ±r** â 29 < 45 ve tablo henÃ¼z yoktur), dÃ¶rdÃ¼ de `allTables`'a girer, R4 ayrÄ±ca
      `REVOKE UPDATE, DELETE` alÄ±r (`capability_jobs`'Ä±n deseni).
- [ ] **`fleet` paketi (YENÄ°, `apps/control-plane/internal/fleet/`)** ve gerekÃ§esi bir sÄ±nÄ±r:
      `execution` paketi zaten 40+ dosya ve gateway'in iÃ§ine bir store koymak onu store'a baÄÄ±mlÄ±
      yapardÄ±. Gateway `fleet.Registry` arayÃ¼zÃ¼nÃ¼ alÄ±r (dÃ¶rt metot), Ã¼retimde Postgres, testte fake.
- [ ] **`GET /v1/runners` + `GET /v1/runners/{id}`** (okuma, RLS-scoped, `ListView` envelope'u â
      `pagination.go`'nun deseni). **Yazma rotasÄ± YOK** (T5/T6 aÃ§ar).
- **Seam:** `000045_runner_fleet.{up,down}.sql` (YENÄ°), `storage/queries/runners.sql` (YENÄ°),
  `internal/fleet/store.go` (YENÄ°), `runner_gateway.go`, `api/router.go`,
  `tests/component/postgres/migration_test.go`. **UAT:** **FLT-001** (YENÄ°). **Tier:** DEÄÄ°ÅMEZ.
- **KanÄ±t:** untagged â id mintleme, journal append-only'liÄi. component-real gerÃ§ek Postgres â iki
  RED'in ikisi; dÃ¶rt tablonun dÃ¶rdÃ¼nde de RLS ENABLE+FORCE; `runner_enrollments`'a UPDATE/DELETE'in
  **reddedildiÄi**; migration'Ä±n ileri-geri koÅtuÄu.
- **Live (`PALAI_DATABASE_URL` yoksa SKIP):** `000044`'ten gelen bir veritabanÄ±nÄ±n yÃ¼kseldiÄi ve
  mevcut satÄ±rlarÄ±n **kaybolmadÄ±ÄÄ±**.
- **Honest ceiling:** **Registry bir ENVANTERdir, bir SAÄLIK KAYNAÄI deÄildir** â `last_seen_at`
  connect/renew'da gÃ¼ncellenir, heartbeat T5'in iÅidir. **Ä°kinci tavan:** eski bir runner sunucunun
  mintlediÄi id'yi `renew`'de taÅÄ±maz (protokol alanÄ± yok) â o runner her `renew`'de sertifikasÄ±nÄ±n
  DNS'inden eÅleÅir; bu bir isim eÅleÅmesidir ve T3'Ã¼n anahtar baÄÄ±nÄ± **almaz**.

---

### T2 â Havuz = kuyruk = etiket, ve sÄ±ra AÃIKÃA seÃ§ilir (mig YOK; SECURITY-CRITICAL; T1'e baÄlÄ±)

**D1 DOÄRU, AMA DÃZLEMÄ° YANLIÅTI** (Â§3.6 D1): `WorkerSpec.PoolLabel` capability-worker dÃ¼zleminin Ã¶lÃ¼
alanÄ±dÄ±r; run'larÄ± koÅturan runner dÃ¼zleminde havuz kavramÄ± **hiÃ§ yok**. Bu task onu **runner
dÃ¼zleminde** kurar ve `workers` paketine **dokunmaz**.

- [ ] **RED Ã¶nce (1):** `posture='unsandboxed-host'` havuzunu isteyen bir attempt,
      `posture='sandboxed-linux'` havuzuna enroll olmuÅ bir runner'a **offer edilirse FAIL** â
      sÄ±raya alÄ±nmaz, "en yakÄ±n" seÃ§ilmez, **reddedilir**.
- [ ] **RED Ã¶nce (2), BÄ°T-DEÄÄ°ÅMEZLÄ°K:** hiÃ§bir havuz yapÄ±landÄ±rmasÄ± olmayan bugÃ¼nkÃ¼ compose stack'i
      **birebir aynÄ±** davranÄ±r. Bu testin adÄ± iddiasÄ±nÄ± sÃ¶yler:
      `TestASingleRunnerDeploymentWithNoPoolConfigurationIsBitUnchanged`.
- [ ] **SIRA AÃIKÃA SEÃÄ°LÄ°R VE YAZILIR: havuz iÃ§inde `created_at` FIFO.** GerekÃ§e Â§3.6 D2'dir â
      capability dÃ¼zleminin `ORDER BY job_id`'si FIFO **deÄil**, yani FIFO korunan bir davranÄ±Å deÄil
      **yeni** bir karardÄ±r ve bir kararÄ±n gerekÃ§esi yazÄ±lÄ±r: bir insanÄ±n beklediÄi run, bir
      makinenin id'si daha kÃ¼Ã§Ã¼k olduÄu iÃ§in geÃ§ilmemelidir.
- [ ] **PUSH MODELÄ° KORUNUR, POLL'A GEÃÄ°LMEZ** (P2 + Â§3.6 D3). Anthropic worker'Ä± poll ediyor; bizim
      runner'Ä±mÄ±z park edip push alÄ±yor ve bu **daha iyi**: kuyruk gecikmesini poll aralÄ±ÄÄ±
      belirlemiyor, ve `SKIP LOCKED`'sÄ±z bir poll'un kaybeden sÃ¼rÃ¼sÃ¼ yok. `available` tek bir kanaldan
      **havuz baÅÄ±na bir kanala** Ã§Ä±kar â `map[poolID]chan *pendingRunner`, RWMutex ile.
- [ ] **HAVUZ POLÄ°TÄ°KASI `config_policy`'DEDÄ°R** (migration YOK, yazma yolu shipped â
      `admin.go:203`): `{"pool":"mac-pool"}`. ÃÃ¶zÃ¼m sÄ±rasÄ±: run'Ä±n `pool_id`'si (resume) â agent
      revision'Ä±n binding'i â project `config_policy` â `pool_default`. **DÃ¶rt basamaÄÄ±n hepsi tek
      bir fonksiyonda** (`fleet.ResolvePool`), Ã§aÄÄ±ran baÅÄ±na deÄil.
- [ ] **POSTURE HAVUZUN ALANIDIR, RUNNER'IN BEYANI DEÄÄ°L** â runner enroll ederken posture'Ä±nÄ±
      **sÃ¶yler**, gateway onu havuzunkiyle **karÅÄ±laÅtÄ±rÄ±r** ve uyuÅmuyorsa enrollment'Ä± **reddeder**
      (journal'a `refused`). BeyanÄ± doÄrulamÄ±yoruz â doÄrulayamayÄ±z â ama **uyuÅmazlÄ±ÄÄ± yakalÄ±yoruz**,
      ve bu iki farklÄ± iddiadÄ±r. Tavan Â§T2'de yazÄ±lÄ±r.
- **Seam:** `internal/fleet/pools.go` (YENÄ°), `internal/fleet/placement.go` (YENÄ°),
  `runner_gateway.go` (havuz baÅÄ±na kanal), `api/router.go` (`/v1/runner-pools` okuma).
  **UAT:** **FLT-002** (YENÄ°). **Tier:** DEÄÄ°ÅMEZ.
- **KanÄ±t:** untagged â havuz Ã§Ã¶zÃ¼mÃ¼nÃ¼n dÃ¶rt basamaÄÄ±, FIFO sÄ±rasÄ±. component-real gerÃ§ek Postgres +
  gerÃ§ek gateway â iki RED'in ikisi; iki havuz, iki runner, iki run, **Ã§aprazlama sÄ±fÄ±r**;
  yapÄ±landÄ±rmasÄ±z stack'in bit-deÄiÅmezliÄi.
- **Honest ceiling:** **Bir havuz iÃ§inde Ã¶nceliklendirme YOKTUR** â FIFO'dur, ve acil bir run'Ä±n Ã¶ne
  geÃ§mesi ifade edilemez. **Ä°kinci tavan:** posture beyanÄ± **doÄrulanmaz**; yalan sÃ¶yleyen bir runner
  `sandboxed-linux` havuzuna girip host'ta koÅabilir. DoÄrulama bir attestation ister ve bu epic onu
  kurmuyor â `known-gaps`'e `FLT-P*` satÄ±rÄ± olarak girer.

---

### T3 â Havuz anahtarÄ±: kolay taraf enrollment'ta, gÃ¼Ã§ taraf sonrasÄ±nda (mig YOK â R2 T1'de; SECURITY-CRITICAL; T1'e baÄlÄ±)

**BU TASK'IN PREMISE'Ä° TERS ÃEVRÄ°LDÄ° VE BU BÄ°R KAZANÃTIR** (Â§3.6 D4): aÄaÃ§ zaten yeniden kullanÄ±labilir
bir anahtara geÃ§miÅ ve dÃ¶rt yerde bunu inkÃ¢r ediyor. Eklenecek Åey yeniden kullanÄ±labilirlik deÄil â
**kapsam, hash, son kullanma, iptal, kayÄ±t.**

- [ ] **RED Ã¶nce (1), KAPSAM:** Mac havuzunun anahtarÄ±yla Linux havuzuna enroll denemesi **REDDEDÄ°LÄ°R**
      ve `runner_enrollments`'a `refused` dÃ¼Åer. BugÃ¼n havuz kavramÄ± yok â RED doÄar.
- [ ] **RED Ã¶nce (2), Ä°PTALÄ°N ÃÃ YARISI â VE ÃÃÃ AYRI TESTTÄ°R:** anahtar iptal edildikten sonra
      (a) **yeni enroll REDDEDÄ°LÄ°R**, (b) o anahtarla enroll olmuÅ bir runner'Ä±n **`renew`'Ã¼ BAÅARILI
      olur**, (c) o runner'Ä±n **devam eden lease'i kesilmez**. (b) ve (c) bugÃ¼n de doÄrudur ve
      **doÄru kalmalarÄ± bu epic'in vaadidir** â yani bu test bir regression fence'idir, bir Ã¶zellik
      deÄil. YapÄ±sal sebep: `handleRenew` mTLS ile doÄrular, `Consume` o yolda yoktur (D5).
- [ ] **RED Ã¶nce (3), ANAHTAR BÄ°R API ANAHTARI DEÄÄ°LDÄ°R:** havuz anahtarÄ±yla `/v1/*` altÄ±nda herhangi
      bir Ã§aÄrÄ± **401** alÄ±r, ve `Scope` Ã§Ã¶zÃ¼mÃ¼ne hiÃ§ girmez. **`Scope.HasScope`'un boÅ-scope =
      her-yetki davranÄ±ÅÄ±** (`auth.go:30`, durum belgesi Â§4) bu anahtara **ulaÅamaz**, Ã§Ã¼nkÃ¼ anahtar
      `api_keys` tablosunda deÄildir.
- [ ] **HASH + SABÄ°T ZAMAN + BÄ°R KEZ GÃSTERÄ°M:** DB'de `sha256(anahtar)` ve bir `key_prefix` (ilk 8
      karakter, yalnÄ±z listeleme iÃ§in); karÅÄ±laÅtÄ±rma `crypto/subtle.ConstantTimeCompare`; deÄer
      **yalnÄ±z** mint anÄ±nda stdout'a. BugÃ¼nkÃ¼ karÅÄ±laÅtÄ±rma `strings.TrimSpace(...) != token`
      (`local_credentials.go:159`) ve **o da dÃ¼zeltilir** â dosya token'Ä± da bir bearer credential'dÄ±r.
- [ ] **`PoolEnrollmentKeys` `EnrollmentTokens`'Ä± UYGULAR** â arayÃ¼z deÄiÅmez, ikinci bir
      implementasyon eklenir (`FileEnrollmentTokens`'Ä±n kardeÅi). **Dosya token'Ä± SÄ°LÄ°NMEZ**: sÃ¼resi
      dolmuÅ kimliÄin tek kurtuluÅ yoludur ve `local_credentials.go:97-122` bunu uzun uzun anlatÄ±yor.
      Gateway ikisini **sÄ±rayla** dener: havuz anahtarÄ± â dosya token'Ä± â red.
- [ ] **`enrolled_via_key_id` KAYDEDÄ°LÄ°R** (R3) â hedeflenmiÅ iptalin bugÃ¼n eksik olan tek parÃ§asÄ±
      (D5). Bir anahtar iptal edildiÄinde operatÃ¶re **o anahtarla enroll olmuÅ makinelerin listesi**
      gÃ¶sterilir; **hiÃ§biri durdurulmaz**, ve durdurulmadÄ±ÄÄ± testle gÃ¶sterilir.
- [ ] **DÃRT KOPYANIN ÃÃÃ DÃZELTÄ°LÄ°R** (D4): `runner_gateway.go:40-44`, `lifecycle.go:118`,
      `cmd/runner/main.go:68-69`. **Kod deÄiÅikliÄi deÄil, Ã¼Ã§ yorum** â ama E23 T7'nin D7 dersi aynen:
      bir dÃ¼zeltmenin planÄ±n adlandÄ±rdÄ±ÄÄ± dosyaya gidip inancÄ±n her bulunduÄu yere gitmemesi, inancÄ±
      gÃ¶nderilmeye devam ettirir.
- [ ] **`palai admin pool key create|list|revoke`** (`admin.go`'nun `apikey` ailesinin deseni,
      `admin.go:157,228`). **Anahtar argv'ye girmez** â mint stdout'a basar, iptal `key_id` ile.
- **Seam:** `execution/local_credentials.go`, `internal/fleet/keys.go` (YENÄ°), `runner_gateway.go`,
  `cmd/cli/internal/admin/admin.go`, `api/router.go`. **UAT:** **FLT-003** (YENÄ°).
  **Tier:** DEÄÄ°ÅMEZ.
- **KanÄ±t:** untagged â hash, sabit-zaman, prefix. component-real gerÃ§ek Postgres â Ã¼Ã§ RED'in Ã¼Ã§Ã¼;
  anahtar deÄerinin **hiÃ§bir** log/journal/evidence baytÄ±nda olmadÄ±ÄÄ± (JSON decode ederek sÃ¼pÃ¼rme).
- **Live (`PALAI_DATABASE_URL` yoksa SKIP):** gerÃ§ek Postgres'te iptal + `renew` sÃ¼rekliliÄi.
- **Honest ceiling:** **AnahtarÄ± elinde tutan biri, o havuza sahte bir makine enroll edip iÅ
  claim edebilir** â strict mode kapalÄ±yken (T6, ve varsayÄ±lan kapalÄ±dÄ±r). Savunmalar **anahtar
  gizliliÄi ve iptal hÄ±zÄ±dÄ±r**, baÅka bir Åey deÄil. **Ä°kinci tavan:** anahtar baÅÄ±na eÅzamanlÄ± kimlik
  sayÄ±sÄ± sÄ±nÄ±rlanmaz; `minInterval`'Ä±n bellek-iÃ§i hÃ¢li (D6) kalÄ±cÄ± hÃ¢le getirilir ama bir **kota**
  deÄildir. **ÃÃ§Ã¼ncÃ¼ tavan:** bir sertifika Ã§alÄ±nÄ±rsa iptal T5'in iÅidir, bu task'Ä±n deÄil.

---

### T4 â YerleÅtirme, tenant, ve kapasite iÃ§in PARK (mig YOK; SECURITY-CRITICAL; T2+T3'e baÄlÄ±)

**BU TASK Ä°KÄ° ÃLÃÃMDEN DOÄDU:** runner dÃ¼zleminde tenant **hiÃ§ yok** (D8), ve boÅ bir havuza dÃ¼Åen
bir run **2.5 dakikada Ã¶lÃ¼yor** (D12) â Mac'in aÃ§Ä±lmasÄ± 6â20 dakika (P10).

- [ ] **RED Ã¶nce (1), TENANT:** A tenant'Ä±nÄ±n runner'Ä±na B tenant'Ä±nÄ±n attempt'i offer edilirse
      **FAIL**. BugÃ¼n hiÃ§bir kontrol yok (D8) â RED doÄar. **`AttemptDescriptor` += `Tenant`**;
      `tenant` Ã§aÄrÄ± yerinde zaten yerel (`orchestrator.go:393` civarÄ±), yani threading tek satÄ±r.
- [ ] **RED Ã¶nce (2), PARK:** hedef havuzunda **hiÃ§ runner olmayan** bir run `dead_letter` olursa
      **FAIL** â `waiting` olmalÄ±. BugÃ¼n `Dial` 20 sn'de dÃ¼ÅÃ¼yor, retry beÅ kez deniyor, run Ã¶lÃ¼yor.
- [ ] **RED Ã¶nce (3), UYANMA:** park etmiÅ run, o havuza **bir runner baÄlandÄ±ÄÄ±nda** uyanÄ±r ve
      koÅar. **RED-first: 30 dakika sonra hÃ¢lÃ¢ `waiting`'deyse FAIL.**
- [ ] **PARK, E23 T1'Ä°N KOREOGRAFÄ°SÄ°NÄ° AYNEN Ä°ZLER** â `checkpointBeforePause` â
      `ApplyRunTransition(RunCmdWait)` â `errRunAwaitingCapacity` â `ExecuteAttempt` `nil` dÃ¶ner
      (dispatch worker serbest). **Yeni bir park mekanizmasÄ± YAZILMAZ**, ve bu bir tasarruf deÄil bir
      doÄruluk kararÄ±dÄ±r: iki park yolu, iki uyanma hatasÄ± demektir.
- [ ] **UYANDIRMA `handleConnect`'Ä°N Ä°ÃÄ°NDEDÄ°R VE TEK TX'TÄ°R:** bir runner havuza park ettiÄinde o
      havuzun **en eski** `waiting` run'Ä± `running`'e Ã§ekilir + `EnqueueJob("response.run")`
      (`applyResumeTx`'in gÃ¶vdesi). **Bir uyandÄ±rma bir runner'Ä± REZERVE ETMEZ** â uyanan run
      dial eder, ve o sÄ±rada baÅka bir run onu kapabilir; bu yarÄ±Å **iyi huyludur** (ikinci run yeniden
      park eder) ve bir testle gÃ¶sterilir.
- [ ] **DISPATCH WORKER SAYISI BÄ°R UYARI KAZANIR** (D11): `palai up`, `PALAI_DISPATCH_WORKERS=1` iken
      **iki ya da daha Ã§ok runner** gÃ¶rÃ¼rse birebir uyarÄ±r: *"N runners are enrolled but
      PALAI_DISPATCH_WORKERS=1 â concurrent runs are bounded by the control plane, not the fleet"*.
      **Kod deÄiÅikliÄi deÄil, Ã¶lÃ§Ã¼lmÃ¼Å bir yalanÄ±n dÃ¼zeltilmesi.**
- [ ] **`runs.pool_id` YAZILIR** (R5) â yerleÅtirme kararÄ± denetlenebilir, ve bir resume **aynÄ±
      havuza** dÃ¶ner. Bir kill sonrasÄ± run'Ä±n baÅka bir posture'da uyanmasÄ±, workspace'ini bulamamasÄ±
      demektir.
- **Seam:** `engine_channel.go` (`AttemptDescriptor` += `Tenant`, `PoolID`), `orchestrator.go`
  (Ã§Ã¶zÃ¼m + park), `runner_gateway.go` (`Dial` tenant+havuz, `handleConnect` uyandÄ±rma),
  `internal/fleet/placement.go`, `cmd/cli/internal/stack/up.go`. **UAT:** **FLT-004** (YENÄ°).
  **Tier:** DEÄÄ°ÅMEZ.
- **KanÄ±t:** component-real gerÃ§ek Postgres + gerÃ§ek gateway + iki fake runner â Ã¼Ã§ RED'in Ã¼Ã§Ã¼; park
  etmiÅ run'Ä±n **hiÃ§bir dispatch worker'Ä± tutmadÄ±ÄÄ±**; uyandÄ±ktan sonra **bir kez** koÅtuÄu;
  Ã§apraz-tenant offer'Ä±n sÄ±fÄ±r olduÄu.
- **Live (`PALAI_DATABASE_URL` yoksa SKIP):** gerÃ§ek Postgres'te park + uyanma.
- **Honest ceiling:** **Park etmiÅ bir run iÃ§in KOTA YOKTUR** â boÅ bir havuza yÃ¼z run dÃ¼Åerse yÃ¼zÃ¼ de
  park eder ve hiÃ§biri zaman aÅÄ±mÄ±na uÄramaz (E23 T1'in aynÄ± tavanÄ±, aynÄ± sebeple). `PALAI_QUEUE_DEADLINE`
  admission kuyruÄuna bakar, buna deÄil. **Ä°kinci tavan:** havuzu silinen ya da hiÃ§ runner'Ä± olmayacak
  bir havuza park etmiÅ run **sonsuza kadar bekler** â bir reaper T5'in iÅidir ve **bu epic'te
  yazÄ±lÄ±r**, ama sÃ¼resi operatÃ¶rÃ¼n seÃ§imidir ve varsayÄ±lanÄ± yoktur.

---

### T5 â Cordon / drain / revoke runner id'ye anahtarlanÄ±r, ve iptal KALICI olur (mig YOK; SECURITY-CRITICAL; T1'e baÄlÄ±)

**E15 T2'NÄ°N DRAIN'Ä° KORUNUR, YENÄ°DEN YAZILMAZ** â yÃ¶nlendirmenin talimatÄ± ve doÄru olanÄ±. DeÄiÅen Åey
**kimin** drain edildiÄidir. Ve Ã¶lÃ§Ã¼m bir sÃ¼rpriz getirdi: `Revoke()`'un **hiÃ§bir production caller'Ä±
yok** (Â§3.6 D15).

- [ ] **RED Ã¶nce (1):** iki runner'lÄ± bir gateway'de **birini** cordon etmek, **diÄerine** yeni lease
      vermeyi durdurursa **FAIL**. BugÃ¼n `cordoned` process-global bir `atomic.Bool`.
- [ ] **RED Ã¶nce (2), KALICILIK:** iptal edilmiÅ bir runner, **control plane restart'Ä±ndan sonra**
      yeniden baÄlanabilirse **FAIL**. BugÃ¼n `revoked atomic.Bool` bellek iÃ§i.
- [ ] **RED Ã¶nce (3), ULAÅILABÄ°LÄ°RLÄ°K:** `palai admin runner revoke <id>` **yoksa FAIL** â yani bu
      test, bugÃ¼n var olmayan bir yÃ¼zeyin var olmasÄ±nÄ± talep eder. `Revoke`'un testlerle kanÄ±tlÄ± ve
      kimse tarafÄ±ndan ulaÅÄ±lamaz olmasÄ±, E23'Ã¼n `HIL-P8`'inin (onay mesajÄ±nÄ±n production caller'Ä±
      yok) **aynÄ± Åeklidir** ve durum belgesi Â§2'nin kuralÄ± burada da geÃ§er: **bir epic'in Ã§Ä±kÄ±Å
      kapÄ±sÄ±nda, exported sembolleri production caller'a karÅÄ± sÃ¼z.**
- [ ] **`Drain` GÃVDESÄ° DEÄÄ°ÅMEZ, SAYAÃ RUNNER BAÅINA OLUR:** `active atomic.Int64` â
      `map[runnerID]*atomic.Int64`. Bekleme mantÄ±ÄÄ±, E10 recovery katmanÄ±na devretme, ve
      `ctx.Err()` dÃ¶nÃ¼ÅÃ¼ **birebir** korunur. **Whole-gateway drain de korunur** (SIGTERM yolu),
      Ã§Ã¼nkÃ¼ bir control-plane swap'i hÃ¢lÃ¢ hepsini drain eder.
- [ ] **HEARTBEAT + REAPER, VE Ä°KÄ°SÄ° DE T4'ÃN PARKINI BESLER â VE HEARTBEAT NEREDEYSE BEDAVA, ÃÃNKÃ
      FRAME ZATEN GELÄ°YOR VE ÃÃPE ATILIYOR:** `readLoop`'un `switch`'inin `default` kolu birebir
      *"heartbeat or other non-frame messages carry nothing to relay"* (`runner_gateway.go:472-474`).
      E24 o kolu `last_seen_at`'i ilerletmeye baÄlar. **Ä°kinci dÃ¼zlemde aynÄ± hikÃ¢ye daha da ileri:**
      `HeartbeatCapabilityWorker` (`storage/queries/workers.sql:28-32`) **yazÄ±lmÄ±Å ve sÄ±fÄ±r Go
      caller'Ä± var** â yani bir heartbeat sorgusu bile hazÄ±r duruyor ve kimse Ã§aÄÄ±rmÄ±yor. E24 runner
      dÃ¼zleminde olanÄ± baÄlar; capability dÃ¼zlemine dokunmaz (Â§5). `runners.last_seen_at` connect/renew
      ve o heartbeat'te ilerler; bir reaper `last_seen_at` bayatlamÄ±Å runner'Ä± `unhealthy`
      yapar, lease'ini keser ve **o havuza park etmiÅ run'larÄ± uyandÄ±rmaz** (kapasite hÃ¢lÃ¢ yok) ama
      **havuzun saÄlÄ±k sayÄ±sÄ±nÄ± dÃ¼ÅÃ¼rÃ¼r**. AyrÄ±ca **T4'Ã¼n ikinci tavanÄ±nÄ± kapatan reaper burada**:
      `PALAI_FLEET_PARK_TTL` (varsayÄ±lan **YOK** â operatÃ¶r aÃ§ar) dolduÄunda park etmiÅ run uyandÄ±rÄ±lÄ±r
      ve model *"no capacity"* cevabÄ±nÄ± **Ã¶Ärenir**, sessizce Ã¶lmez.
- [ ] **`palai admin runner cordon|resume|revoke|list`** + `POST /v1/runners/{id}/{cordon,resume,revoke}`.
      **Revoke geri alÄ±namaz** ve bu bugÃ¼nkÃ¼ semantiÄin aynÄ±sÄ±dÄ±r (`runner_gateway.go:153`:
      *"a revoked runner identity is decommissioned, not paused"*).
- **Seam:** `runner_gateway.go`, `internal/fleet/store.go`, `api/router.go`,
  `cmd/cli/internal/admin/admin.go`, `main.go` (reaper supervisor). **UAT:** **FLT-005** (YENÄ°).
  **Tier:** DEÄÄ°ÅMEZ.
- **KanÄ±t:** component-real gerÃ§ek Postgres + iki fake runner â Ã¼Ã§ RED'in Ã¼Ã§Ã¼; whole-gateway drain'in
  **bit-deÄiÅmez** kaldÄ±ÄÄ± (SIGTERM yolu); iptalin restart'tan saÄ Ã§Ä±ktÄ±ÄÄ±.
- **Honest ceiling:** **Ä°ptal edilen sertifika CRL'e girmez** â gateway her connect'te DB'ye bakar,
  yani iptal **gateway'e baÄlÄ±dÄ±r**, sertifikaya deÄil. BaÅka bir mTLS tÃ¼keticisi olsaydÄ± o sertifikayÄ±
  kabul ederdi; bugÃ¼n yok. **Ä°kinci tavan:** heartbeat aralÄ±ÄÄ± ve bayatlama eÅiÄi sabittir, havuz
  baÅÄ±na ayarlanamaz.

---

### T6 â Strict mode: bekleme odasÄ±, ve KAPALI olmasÄ±nÄ±n gerekÃ§esi (mig YOK â R1'de `strict_enrollment`; SECURITY-CRITICAL; T3+T5'e baÄlÄ±)

**D3 AYNEN UYGULANIR VE VARSAYILAN KAPALIDIR.** Autoscale eden bir havuzda makine baÅÄ±na insan
beklemek, autoscale'in kendisini iptal eder.

- [ ] **RED Ã¶nce (1):** `strict_enrollment=true` bir havuza enroll olan makine **`pending`** durumunda
      kalÄ±r, **hiÃ§bir lease alamaz**, ve bir insan onaylayana kadar Ã¶yle kalÄ±r. SertifikayÄ± **alÄ±r**
      (yoksa `renew` yolu hiÃ§ aÃ§Ä±lmaz) ama `Dial` onu **hiÃ§ gÃ¶rmez**.
- [ ] **RED Ã¶nce (2), BÄ°T-DEÄÄ°ÅMEZLÄ°K:** `strict_enrollment=false` (varsayÄ±lan) iken davranÄ±Å
      **birebir** T3'Ã¼nkidir.
- [ ] **ONAY YÃZEYÄ° E23'ÃN BOÄAZINI KULLANMAZ VE SEBEBÄ° YAZILIR:** `ApplyApprovalDecision` bir
      **tool Ã§aÄrÄ±sÄ±nÄ±n** ya da bir **publication'Ä±n** onayÄ±dÄ±r ve `request_hash`'e baÄlÄ±dÄ±r; bir
      makine enrollment'Ä±nÄ±n baÄlanacaÄÄ± bir request hash'i yoktur. **AyrÄ± bir yol aÃ§mak burada
      doÄrudur**, ve bunu yazmak E23'Ã¼n *"kontrol tek boÄaza konur"* kuralÄ±na aykÄ±rÄ± deÄil, onun
      kapsamÄ±nÄ±n dÃ¼rÃ¼st okunmasÄ±dÄ±r. Onay `POST /v1/runners/{id}/approve` + `palai admin runner approve`.
- [ ] **ONAYLAYAN, E23 T2'NÄ°N LÄ°STESÄ°DÄ°R** â `config_policy.approvers` (migration YOK, yazma yolu
      shipped). **Liste yoksa davranÄ±Å bit-deÄiÅmezdir** (E23 T2'nin kuralÄ± aynen).
- [ ] **REZÄ°DÃEL RÄ°SK PLANDA VE `known-gaps`'TE ADIYLA YAZILIR** (yÃ¶nlendirmenin talebi): *"strict
      mode kapalÄ±yken, havuz anahtarÄ±nÄ± elinde tutan biri o havuza sahte bir makine enroll edip iÅ
      claim edebilir; savunmalar anahtar gizliliÄi ve iptal hÄ±zÄ±dÄ±r."* **Bir yorumda deÄil, bir
      `FLT-P*` satÄ±rÄ±nda.**
- [ ] **`palai up` strict mode'u ve havuz durumunu BASAR** â kaÃ§ havuz, kaÃ§ aktif runner, kaÃ§
      `pending`. Sessiz bir bekleme odasÄ±, bekleme odasÄ± deÄildir (E21 T2'nin sessiz-SKIP dersi).
- **Seam:** `runner_gateway.go`, `internal/fleet/store.go`, `api/router.go`, `admin.go`,
  `cmd/cli/internal/stack/up.go`, `docs/operations/runner-fleet.md` (YENÄ°).
  **UAT:** **FLT-003 GENÄ°ÅLETÄ°LÄ°R.** **Tier:** DEÄÄ°ÅMEZ.
- **KanÄ±t:** component-real â iki RED'in ikisi; `pending` bir runner'Ä±n **hiÃ§bir** attempt gÃ¶rmediÄi;
  yetkisiz bir principal'Ä±n onaylayamadÄ±ÄÄ±.
- **Honest ceiling:** **Onay bir MAKÄ°NEYÄ° deÄil bir ENROLLMENT'Ä± onaylar.** AynÄ± makine yeniden enroll
  ederse yeniden onay ister â ki doÄrusu budur, ama bir Mac'in her yeniden baÅlatÄ±lmasÄ±nda bir insan
  demektir. **Ä°kinci tavan:** `MAC-P6` aynen aÃ§Ä±k; strict mode kimin enroll ettiÄini sorar, o Mac'in
  iÃ§inde kaÃ§ mÃ¼Återi olduÄunu deÄil.

---

### T7 â Ä°CRA RELAY'Ä°: bir Mac'in Mac olmasÄ± (mig YOK; SECURITY-CRITICAL; T4'e baÄlÄ±; **BU TASK'IN BÃYÃKLÃÄÃ AYRICA TARTIÅILIR**)

**BU TASK D9'UN CEVABIDIR VE EPIC'Ä°N ÃRÃN DEÄERÄ°DÄ°R.** Onsuz E24, koÅamayan bir dÃ¼zleme yerleÅtirme
kurar. AÄaÃ§ seam'i **adÄ±yla Ä±smarlamÄ±Å** (`main.go:591-595`) ve **shipped off-host topoloji bugÃ¼n bir
coding run'Ä± koÅturamÄ±yor** (Â§3.6 D10).

- [ ] **RED Ã¶nce (1), TAÃ TEST:** `unsandboxed-host` posture'lÄ± bir havuzdaki runner'a dÃ¼Åen bir run'Ä±n
      `palai.workspace.shell` Ã§aÄrÄ±sÄ± **o runner'Ä±n makinesinde** koÅar. ÃlÃ§Ã¼m bir sayaÃ§ Ã¼zerinden:
      **control plane'in host executor'Ä± SIFIR kez Ã§aÄrÄ±lÄ±r.**
- [ ] **RED Ã¶nce (2), WORKSPACE:** aynÄ± run'Ä±n `palai.workspace.file` yazmasÄ± **runner'Ä±n diskinde**
      gÃ¶rÃ¼nÃ¼r ve **control plane'in diskinde GÃRÃNMEZ**. BugÃ¼n tersi doÄru (`tools/file.go:48`).
- [ ] **RED Ã¶nce (3), CREDENTIAL SINIRI:** relay frame'lerinin baytlarÄ± JSON decode edilerek sÃ¼pÃ¼rÃ¼lÃ¼r
      ve iÃ§inde bir credential (repo token'Ä±, model anahtarÄ±, DB URL'i, master key) bulunursa **FAIL**.
      Vendor bile bu uyarÄ±yÄ± yazÄ±yor (P4). **Ham substring iddiasÄ± vacuous'tur** (E20 T4'Ã¼n dersi).
- [ ] **RED Ã¶nce (4), ONAY KAPISI ÃSTTE KALIR:** `approval_required` bir tool, relay Ã¼zerinden de
      insan kararÄ± olmadan **tek bir frame** gÃ¶ndermez â runner'Ä±n exec sayacÄ± **SIFIR**. E23'Ã¼n
      kapÄ±sÄ± `dispatchTool`'da, yani CP'de kalÄ±r ve **bu bir mimari karardÄ±r**: kapÄ± icranÄ±n
      yanÄ±nda deÄil, kararÄ±n yanÄ±nda durur.
- [ ] **SHELL YARISI â VAR OLAN TEK METOTLU ARAYÃZÃN Ä°KÄ°NCÄ° Ä°MPLEMENTASYONU.**
      `execution.RunnerShellRunner` `toolbroker.ShellRunner`'Ä± uygular; `ShellCommand`'Ä± bir
      `controller.exec` frame'i olarak lease baÄlantÄ±sÄ±ndan gÃ¶nderir, `runner.exec_result`'Ä± bekler.
      **Orchestrator, ledger, hook'lar, onay kapÄ±sÄ± bit-deÄiÅmez.** Runner tarafÄ±nda
      `packages/runner/exec.go` (YENÄ°) argv'yi kendi posture'Ä±nda koÅturur: container (`sandboxed-linux`)
      ya da host (`unsandboxed-host`) â **`shellRunnerFromEnv`'in gÃ¶vdesi runner'a taÅÄ±nÄ±r**, yeniden
      yazÄ±lmaz.
- [ ] **WORKSPACE YARISI â VE BU TASK'IN PAHALI OLAN KISMI.** Klon, dosya op'larÄ±, snapshot ve
      changeset compile bugÃ¼n CP'nin diskinde. Relay'lenen Åey `workspace.WorkspaceFS`'in **dar**
      yÃ¼zeyidir (read/write/list/stat/checksum) â yeni bir protokol deÄil, ikinci bir frame ailesi.
      **Klon CP-side KALIR ve bu bilinÃ§lidir:** credential broker CP-side'dÄ±r (Â§24, `main.go:587`),
      ve bir klonu runner'a taÅÄ±mak credential'Ä± da taÅÄ±mak olurdu. Klon **relay Ã¼zerinden** yapÄ±lÄ±r:
      CP credential'Ä± redeem eder, git komutunu **argv olarak** Ã¼retir ve **credential'Ä± bir
      helper Ã¼zerinden deÄil, kÄ±sa Ã¶mÃ¼rlÃ¼ bir handle olarak** gÃ¶nderir â **ya da klon CP'de yapÄ±lÄ±p
      workspace runner'a aktarÄ±lÄ±r.** **BU Ä°KÄ° SEÃENEK ARASINDAKÄ° KARAR T7'NÄ°N Ä°LK ADIMIDIR ve
      Ã¶lÃ§Ã¼mle verilir**, bu planda varsayÄ±m olarak sabitlenmez.
- [ ] **POSTURE ARTIK RUNNER'IN BEYANIDIR VE HAVUZUNKÄ°YLE KARÅILAÅTIRILIR** (T2). `resolveShellPosture`
      **korunur** â ama artÄ±k runner process'inde koÅar; control plane'deki kopya **eski
      deployment'lar iÃ§in** yerinde kalÄ±r ve `fleet` yolu yokken devreye girer. **RED-first: eski
      compose stack'i bit-deÄiÅmez.**
- **Seam:** `execution/runner_shell.go` (YENÄ°), `execution/runner_workspace.go` (YENÄ°),
  `packages/runner/exec.go` (YENÄ°), `packages/runner/serve.go`, `packages/contracts/`,
  `cmd/runner/main.go`, `execution/tools/file.go`, `orchestrator.go`. **UAT:** **FLT-006** (YENÄ°).
  **Tier:** DEÄÄ°ÅMEZ.
- **KanÄ±t:** component-real gerÃ§ek Postgres + gerÃ§ek gateway + gerÃ§ek runner process â dÃ¶rt RED'in
  dÃ¶rdÃ¼; relay'lenmiÅ bir shell'in `ShellResult`'Ä±nÄ±n CP-side'Ä±nkiyle **alan alan aynÄ±** olduÄu;
  eski (relay'siz) stack'in bit-deÄiÅmez olduÄu.
- **Live (`PALAI_FLEET_LIVE_RUNNER_HOST` yoksa SKIP):** **gerÃ§ek bir off-host runner'da gerÃ§ek bir
  `xcodebuild -version`** â yani Â§3.6 D10'un kapandÄ±ÄÄ± yer.
- **Honest ceiling:** **Ä°LERLEME AKIÅI YOK, VE ARTIK BÄ°R AÄ ÃZERÄ°NDEN YOK.** `ShellResult` tek sonuÃ§
  dÃ¶ner (`known-gaps` `CAS-P2`) ve relay bunu deÄiÅtirmez â dÃ¶rt dakikalÄ±k bir `xcodebuild` hÃ¢lÃ¢ dÃ¶rt
  dakika sessizliktir, ama Åimdi bir de baÄlantÄ± zaman aÅÄ±mÄ± riski taÅÄ±r. **Ä°kinci tavan:** relay
  gecikme ekler; her dosya op'u bir round-trip'tir ve bÃ¼yÃ¼k bir changeset compile'Ä± Ã¶lÃ§Ã¼lebilir
  Åekilde yavaÅlar â **Ã¶lÃ§Ã¼m Â§6'dadÄ±r ve bir sayÄ± olarak yazÄ±lÄ±r, bir tahmin olarak deÄil.**
  **ÃÃ§Ã¼ncÃ¼ tavan:** `MAC-P6` aynen aÃ§Ä±k.

> **BÃYÃKLÃK UYARISI â VE BÃLÃNME NOKTASI ADIYLA:** T7 iki yarÄ±mdÄ±r ve ikisi baÄÄ±msÄ±z olarak
> gÃ¶nderilebilir. **Shell yarÄ±sÄ±** (var olan tek metotlu arayÃ¼zÃ¼n ikinci implementasyonu) bir task'tÄ±r.
> **Workspace yarÄ±sÄ±** (klon kararÄ± + dosya relay'i + snapshot) **ayrÄ± bir task, muhtemelen ayrÄ± bir
> epic'tir.** Owner bÃ¶lmek isterse bÃ¶lÃ¼nme noktasÄ± budur: **T7a shell relay'i E24'te, T7b workspace
> relay'i E26'te.** O hÃ¢lde E24 bir Mac'te `xcodebuild` koÅturur ama workspace'i hÃ¢lÃ¢ paylaÅÄ±lan bir
> dosya sistemi ister â yani **aynÄ± yerel aÄdaki** bir Mac Ã§alÄ±ÅÄ±r, kiralanan bir Mac Ã§alÄ±Åmaz. Bu
> dÃ¼rÃ¼st bir ara durumdur ve **bir yalan deÄildir**, yeter ki bÃ¶yle yazÄ±lsÄ±n.

---

### T8 â EXIT gate: `runner-fleet-0.1.0` + fleet journey (mig YOK)

- [ ] **Case id prefix'i `FLT-`'dir, ve bu bir gate kararÄ±dÄ±r.** `promote-gate-family-dispatch` kuralÄ±:
      bir `WRK-`/`OPS-`/`SAN-` id'si ya gÃ¶nderilmiÅ bir bundle'Ä± yeniden Ã¼rettirir ya `PromoteGateFor`'u
      daha zayÄ±f bir gate'e dÃ¼ÅÃ¼rÃ¼r. **`FLT-` aÄaÃ§taki otuz bir prefix'in hiÃ§biriyle Ã§akÄ±ÅmÄ±yor**
      (sayÄ±ldÄ± 2026-07-29).
- [ ] `tests/uat/extensions/catalog_test.go:69`: `extensionIDPrefixes` bugÃ¼n **dokuz** Ã¼yeli
      (`SLK- A2A- KNO- QUA- TLM- CAS- HIL- UI- WRK-`, sayÄ±ldÄ± 2026-07-29); `FLT-` **onuncu** olur.
      **Sahiplik `uat.FleetCaseIDs`'te yaÅayabilir; SÃPÃRMEDEN KAÃMAK yaÅayamaz** â dosyanÄ±n kendi
      cÃ¼mlesi: *"this sweep is the ONLY place in the tree that walks the cases DIRECTORY, so a prefix
      left outside it is a family whose dirs nothing checks."*
- [ ] `tests/uat/fleet/` journey: temiz stack â `palai up` â **iki havuz** yaratÄ±lÄ±r (`linux-pool`
      `sandboxed-linux`, `mac-pool` `unsandboxed-host`) â her birine **bir havuz anahtarÄ±** â
      **iki runner** aynÄ± anahtarlarla enroll olur ve registry **iki ayrÄ± satÄ±r** taÅÄ±r (T1) â Mac
      havuzunun anahtarÄ±yla Linux havuzuna enroll denemesi **reddedilir** (T3) â bir run `mac-pool`
      politikasÄ±yla aÃ§Ä±lÄ±r ve **yalnÄ±z** Mac runner'Ä±na dÃ¼Åer (T2/T4) â **havuz boÅaltÄ±lÄ±r**, yeni bir
      run **park eder ve Ã¶lmez** (T4) â runner geri gelir, run **uyanÄ±r ve koÅar** (T4) â havuz
      anahtarÄ± **iptal edilir**: yeni enroll dÃ¼Åer, **enroll olmuÅ runner Ã§alÄ±Åmaya devam eder** (T3)
      â bir runner **cordon** edilir, **diÄeri lease almaya devam eder** (T5) â iptal edilen runner
      **control plane restart'Ä±ndan sonra da** baÄlanamaz (T5) â ve bir shell Ã§aÄrÄ±sÄ± **runner'Ä±n
      makinesinde** koÅar (T7).
- [ ] Yeni case'ler: **FLT-001** (bir filo bir ENVANTERdir â iki makine iki satÄ±rdÄ±r ve kimliÄi sunucu
      mintler), **FLT-002** (havuz bir kuyruk ve bir etikettir; yanlÄ±Å havuz bir sÄ±ra deÄil bir
      REDDÄ°R; ve havuz yokken davranÄ±Å bit-deÄiÅmezdir), **FLT-003** (anahtar yalnÄ±z enroll eder,
      havuzuna kilitlidir, hash'lenir, bir kez gÃ¶sterilir â ve **iptali enroll olmuÅ makineleri
      durdurmaz**), **FLT-004** (bir run kapasite iÃ§in PARK EDER, Ã¶lmez; ve bir runner'Ä±n tenant'Ä±
      vardÄ±r), **FLT-005** (cordon/drain/revoke bir MAKÄ°NEYE uygulanÄ±r ve iptal restart'tan saÄ
      Ã§Ä±kar), **FLT-006** (bir tool Ã§aÄrÄ±sÄ± runner'Ä±n makinesinde koÅar, ve credential o sÄ±nÄ±rÄ±
      geÃ§mez).
- [ ] `tests/uat/evidence_fleet.go` yeni proof tipi (`Complete()` gate'li): **`FleetProof`** â
      (a) **yanlÄ±Å havuza yapÄ±lan offer sayÄ±sÄ± (SIFIR)**, (b) **Ã§apraz-tenant offer sayÄ±sÄ± (SIFIR)**,
      (c) **kapasite yokluÄu yÃ¼zÃ¼nden dead-letter olan run sayÄ±sÄ± (SIFIR)**, (d) **anahtar iptalinden
      sonra dÃ¼Åen enroll olmuÅ runner sayÄ±sÄ± (SIFIR)**, (e) **restart'tan sonra geri gelen iptal
      edilmiÅ runner sayÄ±sÄ± (SIFIR)**, (f) **relay frame'lerinde bulunan credential bayt sayÄ±sÄ±
      (SIFIR)**, (g) registry'deki ayrÄ±k runner sayÄ±sÄ± ve **hepsinin id'sinin sunucu tarafÄ±ndan
      mintlendiÄi**, (h) her vendor ÅartÄ±nÄ±n kaynak URL'i + Ã§ekim tarihi + Â§3.5 sapma ID'si.
      **Anti-fabrication:** `Peer` alanÄ± birebir **`"fake"`** olmak ZORUNDA. **Ve (a)/(b)/(f)/(g)
      beyan edilen sayÄ±ya GÃVENMEZ** â taÅÄ±nan byte'lardan **yeniden hesaplanÄ±r**
      (`SweepActionableElements`'in deseni).
- [ ] **(d) Ä°ÃÄ°N ÃZEL BÄ°R FENCE, VE BU EPIC'Ä°N EN UCUZ GÃVENLÄ°K TESTÄ°DÄ°R:** proof, iptal ANINDAN
      SONRAKÄ° `renew` Ã§aÄrÄ±larÄ±nÄ± sayar ve **hepsinin baÅarÄ±lÄ±** olduÄunu yeniden hesaplar. Bir sonraki
      okuyucu *"iptal ettiysek baÄlantÄ±yÄ± da kesmeliyiz"* dediÄinde cevabÄ± bu satÄ±r verir â **kesmek,
      kolay enrollment'Ä± gÃ¼venli yapan tam olarak o Ã¶zelliÄi silerdi.**
- [ ] `tests/uat/promote_fleet.go`: **`FleetPromoteGate`** ve `PromoteGateFor`'da **E23'TEN ÃNCE**
      dispatch (`carriesE24FleetCase`). Gate: tam olarak bir COMPLETE `FleetProof`; **hiÃ§bir tier
      ilerlemez**; E23'Ã¼n tool-approval gate'i **birebir compose** edilir.
- [ ] `evidence.go` `committedBundleSurfaces` **22 â 23**: **`runner-fleet-0.1.0`**
      (`SurfaceRecomputed`) + `caseChecksumParts` dalÄ±. **`LegacyShapeOnly` OLAMAZ.**
      `PALAI_WRITE_FLEET_BUNDLE=1` ile Ã¼retilir ve committed bundle jeneratÃ¶r Ã§Ä±ktÄ±sÄ±yla **bit-eÅ**
      olmak zorundadÄ±r. **E18 T8'in checksum sweep tablosuna 6 case â 12 kayÄ±t** girer.
      **`release-1.0.0-rc1`'in release index'i yeni bir bundle ADIYLA kÄ±rmÄ±zÄ±ya dÃ¶ner** (E22 T7'nin
      Ã¶lÃ§tÃ¼ÄÃ¼ tuzak) â RC de yeniden Ã¼retilir ve **fiyat burada yazÄ±lÄ±dÄ±r.**
- [ ] `scripts/test/component`'in `-run` allow-list'i + `scripts/uat/fleet`'in seÃ§icisi **yeni test
      adlarÄ±nÄ± iÃ§erir.** AtlanÄ±rsa yeni component testi **hiÃ§ koÅmaz** ve gate yeÅil gÃ¶rÃ¼nÃ¼r (**Ã¼Ã§
      kez** dÃ¼ÅÃ¼len tuzak: E18 T8, E21 T7, E23 T7).
- [ ] `make uat-fleet` + `make uat-fleet-live` + `scripts/uat/fleet`.
- [ ] **Â§3.6 D4'ÃN DÃRT KOPYASININ ÃÃÃ DÃZELTÄ°LMÄ°Å OLDUÄU DOÄRULANIR** (T3'te yapÄ±ldÄ±, burada
      **sÃ¼pÃ¼rÃ¼lÃ¼r**): aÄaÃ§ta *"one-use"* / *"already-spent"* / *"spent once"* geÃ§en ve enrollment
      token'Ä±ndan bahseden bir satÄ±r kalÄ±rsa **FAIL**. Bir dÃ¼zeltmenin planÄ±n adlandÄ±rdÄ±ÄÄ± dosyaya
      gidip inancÄ±n her bulunduÄu yere gitmemesi, E23 T7'nin D7'sinin aynÄ± hatasÄ±dÄ±r.
- [ ] **TIER KARARI â iki yÃ¶nlÃ¼ tartÄ±ÅÄ±lÄ±r ve kayda geÃ§er.**

  **KarÅÄ± argÃ¼man (gerÃ§ek):** *"ArtÄ±k gerÃ§ek bir filo var: makineler kayÄ±tlÄ±, havuzlar kapsamlÄ±,
  anahtarlar iptal edilebilir, iÅ doÄru makineye dÃ¼ÅÃ¼yor ve bir tool gerÃ§ekten uzak bir makinede
  koÅuyor. `workspaces` `stable` olmalÄ±."*

  **REDDEDÄ°LÄ°YOR, Ã¼Ã§ sebeple:**
  1. **Â§6 leg 1 hÃ¢lÃ¢ aÃ§Ä±k ve E24 onu YÄ°NE BÃYÃTTÃ:** artÄ±k gerÃ§ek bir uzak makinede gerÃ§ek bir icra
     da iÃ§inde. `Peer` yapÄ±sal olarak `"fake"`; **yakalanmÄ±Å bir receipt yok.**
  2. **BÄ°R KONTROL EKLEMEK, O KONTROLÃN GERÃEK BÄ°R FÄ°LODA ÃALIÅTIÄININ KANITI DEÄÄ°LDÄ°R.** E22 bir
     sÄ±nÄ±rÄ± sildiÄi iÃ§in, E23 bir sÄ±nÄ±r eklediÄi iÃ§in ilerletmemiÅti; E24 bir **dÃ¼zlem** ekliyor ve
     yine ilerletmiyor â Ã§Ã¼nkÃ¼ Ã¼Ã§Ã¼nÃ¼n de kanÄ±tÄ± aynÄ± fake peer'dÄ±r.
  3. **T2'nin posture tavanÄ± aÃ§Ä±k:** yalan sÃ¶yleyen bir runner yanlÄ±Å havuza girebilir, ve bunu
     yakalayan bir attestation YOK.

  **`workspaces`'i `stable`'a taÅÄ±mak iÃ§in NE DOÄRU OLMALIYDI:** (i) **iki fiziksel makinede**
  koÅmuÅ, yakalanmÄ±Å ve yeniden tÃ¼retilebilir bir receipt; (ii) `Peer`'Ä±n yapÄ±sal `"fake"` kÄ±sÄ±tÄ±nÄ±n
  kalkmasÄ±; (iii) `linux/amd64` doÄrulamasÄ± (durum belgesi Â§3: *"Bu makineden kapatÄ±lamayacak tek
  boÅluk"*). **ÃÃ§Ã¼ de yok.**
- [ ] `docs/operations/known-gaps-1.0.md`: **`FLT-P*` satÄ±rlarÄ±** â strict mode kapalÄ±yken anahtar
      sahibinin sahte makine enroll edebilmesi (T3/T6), posture beyanÄ±nÄ±n doÄrulanamamasÄ± (T2), havuz
      iÃ§inde Ã¶nceliklendirme olmayÄ±ÅÄ± (T2), park kotasÄ±nÄ±n olmayÄ±ÅÄ± (T4), iptalin CRL deÄil
      gateway-baÄÄ±mlÄ± olmasÄ± (T5), relay'in ilerleme akÄ±ÅÄ± taÅÄ±mamasÄ± ve gecikme eklemesi (T7),
      `MAC-P6`'nÄ±n aynen aÃ§Ä±k kalmasÄ±, P12/P13'Ã¼n unconfirmed'larÄ± â **birer satÄ±r olarak.**
- **Migration:** yok â **T1'in 000045'i tek migration'dÄ±r ve zincir orada durur.**
- **Honest ceiling:** bu bundle *"artÄ±k bir bulut filosu var"* Ä°DDÄ°A ETMEZ. Ä°ddia ettiÄi Åey:
  **"birden Ã§ok makinenin kimlikli, tenant kapsamlÄ±, havuzlanmÄ±Å ve iptal edilebilir bir envanter
  olarak var olabildiÄi; bir run'Ä±n hangi havuzda koÅacaÄÄ±nÄ±n bir yapÄ±landÄ±rma olduÄu; kapasite
  yokken bir run'Ä±n Ã¶ldÃ¼ÄÃ¼ deÄil park ettiÄi; bir havuz anahtarÄ±nÄ±n iptalinin enroll olmuÅ makineleri
  ÃALIÅIR bÄ±raktÄ±ÄÄ±; ve bir tool Ã§aÄrÄ±sÄ±nÄ±n control plane'in deÄil, runner'Ä±n makinesinde
  koÅabildiÄi bir filo omurgasÄ±."**

---

## Â§5 â OUT OF SCOPE (bilinÃ§li dÄ±ÅarÄ±da, adres adresine)

| Kalem | Neden dÄ±ÅarÄ±da | Nerede yaÅÄ±yor |
|---|---|---|
| **ÃlÃ§ekleyici (scaler) â kuyruk derinliÄi â kapasite** | **E24 onu ANLAMLI kÄ±lan Åeyi kuruyor ve orada duruyor.** Bir makine aÃ§madan Ã¶nce bir run'Ä±n onu **bekleyebilmesi** gerekir; bekleyemiyordu (Â§3.6 D12) ve T4 onu dÃ¼zeltiyor. ÃlÃ§ekleyici o dÃ¼zeltmenin **Ã¼stÃ¼ne** yazÄ±lÄ±r, yanÄ±na deÄil | **E26 T1** |
| **Spawn seam + bulut saÄlayÄ±cÄ±lar (Scaleway, AWS, Docker, k8s)** | D4 doÄrudur ve tek bir seam'dir (P5: on entegrasyonun hepsi aynÄ± hook). **Ama seam'in girdisi bir Ã¶lÃ§ek kararÄ±dÄ±r ve o karar E26'tedir.** Core'a hiÃ§bir bulut SDK'sÄ± girmez â bu bir Â§5 satÄ±rÄ± olarak **kayÄ±t altÄ±na alÄ±nÄ±r**, bir niyet olarak deÄil | **E26 T2-T3** |
| **Mac ekonomisi: 24 saatlik taban, sÃ¶nme, `deletable_at`** | P9/P10/P11 Ã¶lÃ§Ã¼ldÃ¼ ve **R1 `min_size` kolonunu taÅÄ±yor** â ama **kullanan yok.** Bir tabanÄ± olan havuz, tabanÄ± **uygulayan** bir dÃ¶ngÃ¼ ister | **E26 T1**, `runner_pools.min_size` hazÄ±r |
| **`workers` paketi / capability-worker dÃ¼zlemi Mac yolu olarak** | **ÃÃ§ baÄÄ±msÄ±z Ã¶lÃ§Ã¼lmÃ¼Å red** (Â§3.6 D14): non-loopback bind **reddediliyor** (`main.go:1589-1608`), dÃ¼zlem **Ã¼Ã§ Åekilde uykuda** (`known-gaps` `WRK-2`, 2026-07-26'da yeniden doÄrulandÄ±), ve **yapÄ±sal olarak tipli-operasyon** (`ErrUntypedOperation`) â genel bir shell veremez. **Relay'den daha Ã§ok iÅ, daha az sonuÃ§** | HiÃ§bir yerde â Ã¶lÃ§Ã¼mle ret. `WRK-1`/`WRK-2` aÃ§Ä±k kalÄ±r |
| **Admin panel (havuz/anahtar/runner ekranlarÄ±)** | **DIÅARIDA, ve gerekÃ§esi bir gÃ¼venlik Ã¶lÃ§Ã¼mÃ¼dÃ¼r:** konsolda **hiÃ§bir kimlik doÄrulamasÄ± yok** (`middleware.ts` yok, login yok â durum belgesi Â§4) ve relay `POST/PATCH/DELETE` export ediyor. **Bir havuz anahtarÄ±nÄ± kimliksiz bir yazma vekilinin arkasÄ±nda mintletmek, bu epic'in kurduÄu her Åeyi geÃ§ersiz kÄ±lardÄ±.** E24 anahtarÄ± **CLI'dan** mintler (Â§0.2) ve konsolu **hiÃ§ beklemez.** Konsol auth'u geldiÄinde ekranlar bedavadÄ±r â okuma rotalarÄ± T1/T5'te zaten aÃ§Ä±lÄ±yor | **E26** (eski E26). Â§0.2 |
| **Attestation â bir runner'Ä±n posture beyanÄ±nÄ±n doÄrulanmasÄ±** | T2 uyuÅmazlÄ±ÄÄ± yakalÄ±yor, **beyanÄ± doÄrulamÄ±yor**. DoÄrulama TPM/Secure Enclave ya da bir imzalÄ± Ã¶lÃ§Ã¼m ister ve o ayrÄ± bir tasarÄ±mdÄ±r. **Bir tavan olarak yazÄ±lÄ±r, bir eksiklik olarak deÄil** | Talep gelirse ayrÄ± task; `FLT-P*` |
| **`MAC-P6` â bir Mac'in iÃ§inde iki mÃ¼Återi** | E22'nin Ã¶lÃ§Ã¼mÃ¼ aynen: per-session ayrÄ±m **kaza Ã¶nlemedir, sÄ±nÄ±r deÄildir**, ve `simctl --set` argv'dir (P14). E24 havuzu tenant kapsamlÄ± yapar; **bir Mac'in iÃ§ini deÄiÅtirmez** | `known-gaps` `MAC-P6`, Â§0.1 owner kararÄ± |
| **Havuz iÃ§i Ã¶nceliklendirme / acil run** | T2 FIFO seÃ§iyor ve gerekÃ§esini yazÄ±yor. Ãncelik bir politika alanÄ± ve bir sÄ±ralama anahtarÄ± ister | Talep gelirse ayrÄ± task |
| **CRL / sertifika iptal listesi** | Ä°ptal gateway'de DB'ye bakarak uygulanÄ±r (T5). GerÃ§ek bir CRL/OCSP ancak ikinci bir mTLS tÃ¼keticisi olduÄunda anlam kazanÄ±r; bugÃ¼n yok | Talep gelirse ayrÄ± task |
| **Ä°lerleme akÄ±ÅÄ± (`ShellRunner`'a progress kanalÄ±)** | `known-gaps` `CAS-P2` aynen devralÄ±nÄ±r, ve **E24 onu bÃ¼yÃ¼tÃ¼yor** (artÄ±k bir aÄ Ã¼zerinden sessizlik). Kanal `ShellRunner`'Ä±n **imzasÄ±nÄ±** deÄiÅtirir â yani T7'nin dayandÄ±ÄÄ± seam'i | `CAS-P2`, E22 Â§5 |
| **`PALAI_DISPATCH_WORKERS`'Ä±n otomatik ayarlanmasÄ±** | T4 bir **uyarÄ±** basÄ±yor; otomatik ayarlama bir kapasite modeli ister ve o Ã¶lÃ§ekleyicinin iÅidir | **E26** |
| **Yeni bir discovery capability'si (`fleet`)** | `CapabilityTierOrder`'a Ã¼ye eklemek `CapabilityClaimsDigest`'i oynatÄ±r â **23 bundle'Ä±n her checksum'Ä± kÄ±rmÄ±zÄ±.** Az-iddia etmek gÃ¼venlidir (E23 Â§5 aynen) | HiÃ§bir yerde |
| **`API-3`/`API-4` (publication okuma rotalarÄ±)** | E23 Â§5 aynen; E24 onlara ihtiyaÃ§ duymuyor ve **onay VARSAYMIYOR** â satÄ±r `post-1.0` kalÄ±r | `known-gaps`, owner kararÄ± |

## Â§6 â Operator legs â gerÃ§ek-altyapÄ± bacaÄÄ± (deferred-but-scripted)

E17 Â§6 â¦ E23 Â§6 AYNEN devralÄ±nÄ±r. E24'Ã¼n katkÄ±sÄ± **leg 1'i yine bÃ¼yÃ¼tmek** ve **dÃ¶rt yeni Ã¶lÃ§Ã¼m
bacaÄÄ±** eklemektir.

1. **Ä°KÄ° FÄ°ZÄ°KSEL MAKÄ°NE â YAKALANMIÅ receipt.** Kapsam yine bÃ¼yÃ¼dÃ¼: artÄ±k gerÃ§ek bir uzak enrollment,
   gerÃ§ek bir uzak icra ve gerÃ§ek bir iptal de iÃ§inde. `make uat-fleet-live` E23'Ã¼n kardeÅidir.
   **GerÃ§ek koÅumlar bu legi KAPATMAZ** â yakalanmÄ±Å, yeniden tÃ¼retilebilir bir receipt bÄ±rakmÄ±yorlar
   ve `Peer` yapÄ±sal olarak `"fake"`. â `workspaces` flip'i buna baÄlÄ±dÄ±r.
2. **`linux/amd64` DOÄRULAMASI** â durum belgesi Â§3'Ã¼n *"bu makineden kapatÄ±lamayacak tek boÅluk"*u,
   ve **E24 onu daha kritik yapÄ±yor**: bir filo tanÄ±mÄ± gereÄi heterojendir, ve `runner_pools.arch`
   bugÃ¼ne kadar hiÃ§ koÅmamÄ±Å bir mimariyi adlandÄ±rabilir.
3. **P12'NÄ°N ÃLÃÃMÃ:** Scaleway'in gerÃ§ek aÃ§Ä±lÄ±Å sÃ¼resi ve otomatik-silme seÃ§eneÄi. **Koda varsayÄ±m
   olarak girmedi** â bu yÃ¼zden T4'Ã¼n parkÄ± sÃ¼resizdir ve hiÃ§bir aÃ§Ä±lÄ±Å sabitine dayanmaz.
4. **P13'ÃN ÃLÃÃMÃ:** Anthropic'in `Environments Work` endpoint'lerinin eÅzamanlÄ±lÄ±k/sÄ±ra semantiÄi.
   **Cevap ne olursa olsun kod deÄiÅmez** (T2 kendi sÄ±rasÄ±nÄ± aÃ§Ä±kÃ§a seÃ§ti) â ama bir UNCONFIRMED'Ä±n
   kapanmasÄ± bir sonraki okuyucuya bir saat kazandÄ±rÄ±r.
5. **RELAY GECÄ°KMESÄ°NÄ°N ÃLÃÃMÃ (T7), VE BÄ°R SAYI OLARAK.** AynÄ± changeset'i (a) paylaÅÄ±lan dosya
   sistemiyle, (b) yerel aÄda relay ile, (c) WAN Ã¼zerinden relay ile compile et. **ÃÃ§ sayÄ±, aynÄ±
   tabloda.** Bir tahmin bu satÄ±rÄ± kapatmaz.
6. **BÄ°R HAVUZ ANAHTARININ GERÃEKTEN Ä°PTAL EDÄ°LMESÄ°, GERÃEK ZAMANDA**, ve enroll olmuÅ bir makinenin
   **sertifika Ã¶mrÃ¼ boyunca** Ã§alÄ±Åmaya devam etmesi. Component testi saati ileri alÄ±yor; bir operatÃ¶r
   bacaÄÄ± gerÃ§ek bir yenileme dÃ¶ngÃ¼sÃ¼ bekler.
7. **BÄ°R MAC'Ä°N GERÃEKTEN KÄ°RALANMASI VE 24 SAAT SONRA SÄ°LÄ°NMESÄ°** â P9/P10/P11'in fatura tarafÄ±nÄ±n
   tek gerÃ§ek kanÄ±tÄ±, ve **E26'in Ã¶n koÅulu.**
8. **E17/E18/E19/E20/E21/E22/E23'Ã¼n devralÄ±nan tÃ¼m aÃ§Ä±k legleri** â E24 hiÃ§birine dokunmaz.

**Tier sonucu, bir kez sÃ¶ylenir:** `slack` **preview**, `knowledge-vector` **disabled**, `apple-build`
**disabled**, `console` **preview** kapanÄ±r; `workspaces` ve `capability-workers` E22/E23'Ã¼n tÃ¼rettiÄi
cevaplarÄ± vermeye devam eder. **HiÃ§bir tier ilerlemez, ve ilerlememesinin sebebi bir eksiklik deÄil bir
kural: bir dÃ¼zlem eklemek, o dÃ¼zlemin gerÃ§ek bir filoda Ã§alÄ±ÅtÄ±ÄÄ±nÄ±n kanÄ±tÄ± deÄildir.**

## Â§7 â Master plan Â§8 iÃ§in Ã¶nerilen Ã¶zet blok (owner paste eder)

**UAT ownership:** E24 **ALTI YENÄ° ID** aÃ§ar ve prefix'i **`FLT-`**'dir. **FLT-001** (bir filo bir
ENVANTERdir: iki makine iki satÄ±rdÄ±r, kimliÄi sunucu mintler, ve enrollment defteri append-only'dir),
**FLT-002** (havuz bir kuyruk VE bir etikettir; yanlÄ±Å havuz bir sÄ±ra deÄil bir REDDÄ°R; ve hiÃ§bir havuz
yapÄ±landÄ±rÄ±lmamÄ±Åken davranÄ±Å bit-deÄiÅmezdir), **FLT-003** (havuz anahtarÄ± yalnÄ±z enroll eder,
havuzuna kilitlidir, hash'lenir, bir kez gÃ¶sterilir â ve **iptali enroll olmuÅ makineleri
DURDURMAZ**), **FLT-004** (bir run kapasite iÃ§in PARK EDER, Ã¶lmez; ve bir runner'Ä±n artÄ±k bir tenant'Ä±
vardÄ±r), **FLT-005** (cordon/drain/revoke bir MAKÄ°NEYE uygulanÄ±r ve iptal control-plane restart'Ä±ndan
saÄ Ã§Ä±kar), **FLT-006** (bir tool Ã§aÄrÄ±sÄ± runner'Ä±n makinesinde koÅar, ve hiÃ§bir credential o sÄ±nÄ±rÄ±
geÃ§mez). Tek yeni proof tipi: **`FleetProof`**, `Peer` alanÄ± yapÄ±sal olarak `"fake"`.

**Exit gate â FÄ°LO TIER Ä°LERLETMEZ:** `runner-fleet-0.1.0` bundle'Ä±, E23'Ã¼n insan kapÄ±lÄ± hattÄ±nÄ±n
**birden Ã§ok makineye daÄÄ±labilen** bir hatta dÃ¶nÃ¼ÅtÃ¼ÄÃ¼nÃ¼ kanÄ±tlar. **Bu epic'in tanÄ±mlayÄ±cÄ± kararÄ±
Anthropic'in kendi mimarisinden alÄ±nmÄ±ÅtÄ±r ve bir taklit deÄil bir doÄrulamadÄ±r:** birebir *"The
`self_hosted` environment **acts as a work queue**: when a session is assigned to it, Anthropic
enqueues the session as a work item"* ve *"**Two credentials:** an environment key â¦ authenticates the
worker to its queue; your Claude API key creates sessions"* (platform docs, Ã§ekildi 2026-07-29) â
**yani havuz bir kuyruktur, bir etikettir, ve iÃ§inde routing yoktur; ve enrollment credential'Ä± bir
API anahtarÄ± DEÄÄ°LDÄ°R.** Palai bunu kendi gÃ¼Ã§lÃ¼ tarafÄ±yla birleÅtirir: havuz anahtarÄ± **makine baÅÄ±na
bir sertifikaya takas edilir** (kubeadm'in TLS bootstrapping'i, Tailscale'in reusable auth key'i), ve
**anahtarÄ±n iptali enroll olmuÅ makineleri Ã§alÄ±ÅÄ±r bÄ±rakÄ±r â Ã§Ã¼nkÃ¼ yenileme sertifikayla kimlik
doÄrular, anahtarla deÄil** (`handleRenew`, `runner_gateway.go:265-284`; `Consume` o yolda hiÃ§ yok).
**MIGRATION VARDIR: 000045**, altÄ± rider taÅÄ±r â `runner_pools`, `runner_pool_keys`, `runners`, ve
append-only `runner_enrollments` (dÃ¶rdÃ¼ de tenant tablosu, dÃ¶rdÃ¼ de kendi `palai_apply_tenant_policy`
+ `GRANT`'ini taÅÄ±r), artÄ± `runs.pool_id` ve boot-seed'li bir `pool_default`. **DoÄruluk canlÄ±
koÅumdan deÄil YAYIMLANMIÅ VENDOR DOKÃMANINDAN VE AÄACIN KENDÄ° ÃLÃÃMÃNDEN gelir** â Â§3.5 tablosu
**14 sapmayÄ±** adlandÄ±rÄ±r (ikisi UNCONFIRMED ve hiÃ§biri koda girmez), Â§3.6 tablosu ise **aÄacÄ±n kendi
hakkÄ±ndaki on altÄ± yanlÄ±Å inancÄ±nÄ±.** ÃÃ§Ã¼ diÄerlerinden pahalÄ±dÄ±r. **BÄ°R: bir runner iÅ koÅturmuyor,
bir MOTOR koÅturuyor** â her tool control plane'in process'inde Ã§alÄ±ÅÄ±yor (`main.go:603`,
`tools/file.go:48`) ve aÄaÃ§ bunu kendi kelimeleriyle yazmÄ±Å (`main.go:591-595`), yani "Mac havuzu"
bugÃ¼nkÃ¼ anlamda `xcodebuild`'i hÃ¢lÃ¢ control plane'in makinesinde koÅturur ve **shipped split-VM
kanÄ±tÄ± yalnÄ±z workspace'SÄ°Z bir run'Ä± kanÄ±tlÄ±yor** (`splitvm-proof.sh:1-16`). **Ä°KÄ°: enrollment
token'Ä± zaten tek kullanÄ±mlÄ±k DEÄÄ°L** â tek implementasyonun baÅlÄ±ÄÄ± birebir *"WHY THIS IS NOT
ONE-USE"* (`local_credentials.go:97`) ve "tek kullanÄ±mlÄ±k" cÃ¼mlesi aÄaÃ§ta **dÃ¶rt yerde** yazÄ±lÄ±, biri
yeniden-enroll fonksiyonunu kuran dosyanÄ±n yirmi satÄ±r Ã¼stÃ¼nde. **ÃÃ: bir Mac'i yÃ¼kte aÃ§mak
ekonomik deÄil YAPISAL olarak imkÃ¢nsÄ±z** â `Dial` 20 saniyede dÃ¼ÅÃ¼yor (`orchestrator.go:38`), retry
beÅ kez deniyor (`main.go:477`), ve AWS Mac aÃ§Ä±lÄ±ÅÄ±nÄ± *"approximately 6 minutes to 20 minutes"* diyor;
**run, makine boot etmeden dÃ¶rt kez Ã¶lÃ¼r.** ÃÃ¶zÃ¼m bir timeout bÃ¼yÃ¼tmesi deÄil, E23 T1'in park-ve-uyandÄ±r
koreografisinin ikinci kullanÄ±mÄ±dÄ±r. AyrÄ±ca Ã¶lÃ§Ã¼ldÃ¼: runner dÃ¼zleminde **tenant kavramÄ± hiÃ§ yok** (ne
enrollment'ta, ne `AttemptDescriptor`'da, ne `leaseOffer`'da), `runner_id` **kendi beyanÄ±dÄ±r** ve
compose onu `runner-local` olarak **sabitlemiÅ**, gateway **N runner'Ä± bugÃ¼n de kabul ediyor** ama
`identity` **tek slottur**, ve `Revoke()` â SAN-011'in hard stop'u â **testlerle kanÄ±tlÄ± ve hiÃ§bir
production caller'Ä± yok.** **HiÃ§bir tier ilerlemez**, ve gerekÃ§e bir kuraldÄ±r: bir dÃ¼zlem EKLEMEK, o
dÃ¼zlemin gerÃ§ek bir filoda Ã§alÄ±ÅtÄ±ÄÄ±nÄ±n kanÄ±tÄ± deÄildir â Â§6 leg 1 yine bÃ¼yÃ¼dÃ¼, `linux/amd64` hÃ¢lÃ¢
doÄrulanmadÄ± ve `Peer` yapÄ±sal olarak `"fake"`. **VE BÄ°R BOYUT UYARISI:** Ã¶lÃ§ekleyici, spawn seam'i ve
bulut saÄlayÄ±cÄ±larÄ± **E26**’ya alÄ±nmÄ±ÅtÄ±r (admin panel **E25**’tir ve planÄ± ayrÄ±ca yazÄ±ldÄ±); T7'nin workspace yarÄ±sÄ± tek bir
task'Ä±n bÃ¼tÃ§esini aÅarsa bÃ¶lÃ¼nme noktasÄ± **T7a (shell relay'i, E24) / T7b (workspace relay'i, E26)**
olarak adÄ±yla yazÄ±lÄ±dÄ±r.
