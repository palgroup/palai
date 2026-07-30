# Palai Arka Plan İcra Planı (E26 — bir tool çağrısı bir SÜRECİ döndürür, bir sonucu değil)

> **For agentic workers:** REQUIRED SUB-SKILL: `superpowers:subagent-driven-development` (önerilen) veya `superpowers:executing-plans` ile task-by-task uygula. Adımlar `- [ ]` checkbox'lıdır. **Bu planın tanımlayıcı kuralı E19→E25'inkinin devamıdır: her external contract GERÇEK VENDOR DOKÜMANINDAN grounding alır** ve kaynak URL'i + çekim tarihi şartın YANINA yazılır (§3.5). **Ve §3.6 yine ağacın KENDİ hakkındaki yanlış inançlarıdır** — E21'de on, E22'de on altı, E23'te on üç, E24'te on beş, E25'te yirmi üçtü; burada **yirmi iki**, ve bunların **dördü bu epic'i sipariş eden brief'in kendi cümleleri**, **üçü E24'ün ve E25'in yayımlanmış planlarının satırları**, ve **ikisi ölçüldüğünde bir ÖZELLİĞİN HİÇ GÖNDERİLMEMİŞ olduğunu** gösterdi.

**Goal:** Owner'ın kendi cümleleriyle — *"Kod yazıyor, build alayım dedi, 5 dk sürecek — onu beklemeyip background'a alıp kalan işlerini halledip veya kullanıcı ile sohbet edebilir mi? Sende var ya mesela 'background'ta çalıştırayım bunu' diyorsun, bitince sana haber geliyor."* ve ardından **"aynı senin gibi olsun ama birebir aynı."**

Yani hedef "uzun bir şeyi koşturmanın bir yolu" değildir. Hedef **Claude Code harness'ının arka plan icra semantiğinin birebir karşılığıdır**, ve o semantik altı maddedir (§2).

Ölçümler `main` @ **`4d27acf3`** (2026-07-30) karşısında yapıldı. Atıfsız cümle yok.

---

## §0 — Owner'ın sağlayacakları (HANDOVER CHECKLIST)

E19 §0.1, E20 §0, E21 §0, E22 §0, E23 §0, E24 §0 ve E25 §0 aynen geçerlidir. **E26 owner'dan DÖRT şey ister ve dördü de karardır, kod değil.**

### 0.1 OWNER KARARI — "E26" adı üç yere birden verilmiş durumda

Bu bir numara kavgası değil, bir kapsam kavgasıdır ve **ikisi de yazılı**:

| Kaynak | Ne diyor | Satır |
|---|---|---|
| `phase-24-runner-fleet.md` §7 | *"ölçekleyici, spawn seam'i ve bulut sağlayıcıları **E26**'ya alınmıştır"* ve *"**T7a (shell relay'i, E24) / T7b (workspace relay'i, E26)**"* | §7 son paragraf |
| `phase-25-admin-console.md` | *"Geriye 15 yüzey ve feature list'in EKSİK bölümünün 8 satırı kalır → **E26**"* | `:294`, ayrıca `:59`, `:296` |
| **Bu plan** | Arka plan icrası | — |

**Plan varsayılanı:** bu doküman `phase-26-background-execution.md` adıyla ısmarlandı ve o adı alır. Diğer ikisi **E27 (filo ölçekleyici + relay)** ve **E28 (konsolun kalan yüzeyleri)** olur. **Owner tersini derse tek maliyet dosya adıdır** — bu planın hiçbir teknik kararı numaraya bağlı değildir. Ama **karar verilmeden T1 başlamamalıdır**, çünkü UAT prefix'i ve bundle adı numaraya değil ADA bağlıdır ve `new-uat-case-id-forces-bundle-regen` kuralı bir yanlış adı pahalı yapar.

### 0.2 OWNER KARARI — arka planda koşan bir sürecin ÖMÜR TAVANI kaçtır

**Bu bir sayı ister ve varsayılanı bu plan seçemez, ama BOŞ da bırakamaz.** E24 T5 `PALAI_FLEET_PARK_TTL` için *"UNSET MEANS NEVER and that is deliberate"* dedi (`main.go:665-669`) ve haklıydı: kiralanan bir Mac'in ne zaman geleceği hakkında dürüst bir varsayılan yoktur. **Arka plan süreci için durum TERSİDİR:** tavansız bir arka plan süreci tam olarak owner'ın brief'inde adı geçen sonuçtur — *"An orphan compiling for an hour on an operator's Mac is a real outcome."*

| Karar | Plan varsayılanı | Gerekçe |
|---|---|---|
| `PALAI_BACKGROUND_MAX_WALL_TIME` varsayılanı | **`60m`, ve UNSET = 60m, ASLA "sonsuz" değil** | Bir `xcodebuild` + test suite'i onlarca dakika sürebilir; bir saat bunu kapsar ve bir gecelik orphan'ı kapsamaz. **Sonsuzu istemek açık bir değer yazmayı gerektirir** (`PALAI_BACKGROUND_MAX_WALL_TIME=0` = sınırsız), yani sessiz hâli sınırlıdır |
| Tavanı kim uygular | **Reaper (durable deadline sütunu)**, bir `context` DEĞİL | Bir `context` onu yapan process'le ölür; arka plan süreci tam olarak o process'ten sağ çıkmak için vardır (§3.6 D2) |
| Bir credential taşıyan görev için tavan | **ZORUNLU ve `0` REDDEDİLİR** | §3.6 D9 / T6: değer sürecin environ'unda görevin ömrü boyunca yaşar; ömür sonsuzsa maruziyet de sonsuzdur |

### 0.3 OWNER KARARI — bir run kaç arka plan görevi tutabilir, bir makine kaç tane

E24 `runners.capacity` sütununu açtı ve **hiçbir Go ifadesi onu okumuyor** (§3.6 D12). Bu planın kuralı: **beyan edilen her sınır bir testle uygulanır, yoksa sütun açılmaz.**

| Karar | Plan varsayılanı | Gerekçe |
|---|---|---|
| Run başına | **5** (`PALAI_BACKGROUND_MAX_PER_RUN`) | Beş paralel build bir modelin takip edebileceği en fazla şeydir; altıncısı bir hata sinyalidir |
| Makine (control-plane process) başına | **20** (`PALAI_BACKGROUND_MAX_PER_HOST`) | Bir Mac'te yirmi eşzamanlı derleme zaten makineyi bitirir; sayı bir kapı, bir hedef değil |
| Sınır aşıldığında | **Tool REDDEDER, sıraya ALMAZ** | Sıraya almak modelin göremediği bir gecikme yaratır; ret bir cevaptır ve model onunla bir şey yapabilir |

### 0.4 OWNER KARARI — arka plan görevi bir ENVIRONMENT değeri taşır mı? (bu planın en keskin sorusu)

E25 T3'ün kendi yorumu birebir şunu diyor (`packages/tool-broker/sandbox_exec.go:54-56`):

> *"Scope and expiry are the ATTEMPT itself … There is no handle to redeem and no deadline to check because the value's whole life is one Execute call."*

**Bir arka plan görevi için bu cümle YAPISAL OLARAK YANLIŞ OLUR** ve hiçbir kod değişikliği onu doğru yapamaz: `exec.Cmd.Env` (`host/exec.go:148`) veya `container.Config.Env` (`oci/docker.go:97`) ile verilen bir değer, çekirdeğin o process için tuttuğu environ kopyasında **sürecin bütün ömrü boyunca** yaşar. Control plane'i öldürmek bir çocuğun environ'unu geri almaz.

| Seçenek | Sonucu | Plan varsayılanı |
|---|---|---|
| **(a) Arka plan görevi HİÇ environment değeri almaz** | Fail-closed, savunulabilir — ve **istenen şeyi yapmaz**: token isteyen bir build tam olarak arka plana alınmak istenen şeydir | **HAYIR** |
| **(b) Alır, ve iddia YENİDEN YAZILIR** | Değerin ömrü artık GÖREV'dir; görev kendi fence'ini, kendi expiry'sini (0.2'nin tavanı) ve kendi öldürülebilirliğini taşır — yani E25 T3'ün *"gerekmiyor"* dediği worker üçlüsü (`internal/workers/store.go` `RedeemSecretHandle`: scope + fence + expiry) **burada gerekiyor** | **EVET (b)** |

**Ve (b)'nin bedeli açıkça yazılır, yumuşatılmaz:** aynı uid altında `ps -E` / `/proc/<pid>/environ` bir değeri okuyabilir. **Bu bugün SENKRON bir komut için de doğrudur** (aynı executor, aynı uid) ve `MAC-P6` onu zaten yönetiyor. Arka plan bu maruziyeti **genişletmez, uzatır** — ve uzunluğu 0.2'nin tavanıdır. Owner bunu okuyup onaylamalıdır.

---

## §1 — Yapısal karar (fork noktası, migration, dosyalar)

**Fork noktası:** `main` @ `4d27acf3` ("Merge repositories and the agent lineage…", 2026-07-30).

**MIGRATION VARDIR: `000047`.** Zincir başı bugün **`000046_environments`** (`storage/migrations/000046_environments.up.sql`, sayıldı 2026-07-30). E26 tek migration sahibidir ve **numarayı DOSYA ADINDA, BAŞLIKTA, `VALUES` işaretçisinde, embed değişkeninde ve TEST ADINDA aynı commit'te taşır**; doğrulama `git show HEAD` üzerinden yapılır, working-tree grep'i üzerinden değil (`git-mv-renumber-unstaged-edit`).

Bir epic paralel koşarsa `parallel-migration-contiguous-build` uygulanır: her plan izolasyonda bir sonraki bitişik numarada inşa eder, entegre eden merge'de sabit sıraya göre yeniden numaralandırır.

**YENİ DOSYALAR:**

- `storage/migrations/000047_background_tasks.{up,down}.sql`
- `storage/queries/background.sql`
- `packages/tool-broker/background.go` — `BackgroundRunner` seam'i + `BackgroundSpec`/`Handle`/`Status` tipleri (broker dependency-light kalır, mekanik yok)
- `adapters/sandboxes/host/background.go` — host postürünün implementasyonu (process grubu)
- `adapters/sandboxes/oci/workspace/background.go` + `adapters/sandboxes/oci/detached.go` — container postürünün implementasyonu (etiketli, silinmeyen container)
- `apps/control-plane/internal/execution/background.go` — spawn/probe/kill orkestrasyonu, park kararı, redaksiyonlu okuma
- `apps/control-plane/internal/execution/tools/background_kill.go`
- `tests/uat/cases/BGT-001..BGT-005/case.yaml`, `tests/uat/evidence_background.go`, `tests/uat/promote_background.go`, `tests/uat/background/`
- `docs/operations/background-execution.md`

**DEĞİŞEN DOSYALAR:**

`apps/control-plane/internal/execution/tools/shell.go` (`background` parametresi), `.../execution/finalize.go` (park kapısı), `.../execution/reconciler.go` (iki süpürme), `.../execution/orchestrator.go` (`SetBackgroundRunner`), `.../execution/command_pump.go` (`background_notice` kind'ı), `packages/coordinator/background.go` (**YENİ**, spine yarısı), `packages/coordinator/store.go` (`ReconcileStore` genişler), `apps/control-plane/cmd/palai-control-plane/main.go` (wiring + üç env), `scripts/test/component` (yeni suite + `-run` allow-list), `tests/uat/evidence.go` (`committedBundleSurfaces` + `caseChecksumParts`), `tests/uat/checksum_sweep_test.go` (sweep tablosu), `tests/component/postgres/migration_test.go` (`allTables`), `tests/security/tenancy` (yeni tenant tablosunun politikası), `docs/operations/known-gaps-1.0.md` (`CAS-P2` kapanır/daralır).

**DOKUNULMAYANLAR:** `apps/control-plane/internal/workers/*` (gerekçe §3.6 D13 — E24'ün D14'ünün **dört** sebebinden **üçü** hâlâ geçerli), `engines/reference/*` (§3.6 D16: motor tarafında tek satır gerekmiyor), `packages/tool-broker/broker.go`'nun `ReplayClass`/`RequiresApproval` semantiği, E23'ün onay kapısı (**bir arka plan komutu da onay kapısından geçer ve bu bir testtir**), `adapters/sandboxes/oci/docker.go`'nun `Run` gövdesi (**tek bayt değişmez** — detached yol AYRI bir metottur, §2).

---

## §2 — Design invariant (task değil, her task'ın kabul şartı)

**REPLİKE EDİLECEK SEMANTİK ALTI MADDEDİR** ve altısı da §3.5 P1'de yayımlanmış vendor davranışından alınmıştır:

1. **TOOL ANINDA DÖNER, BİR HANDLE İLE — BİR SONUÇLA DEĞİL.** Görev id'si + çıktının biriktiği yol.
   **RED-first:** spawn çağrısı, süreç hâlâ koşarken döner; ölçüm bir sayaç değil bir SIRA — `Start` döndükten sonra `Probe` `running` demelidir.
2. **ÇIKTI UÇUŞ SIRASINDA OKUNABİLİR.** Bu bir kolaylık değil; modelin beklemeye devam mı, erken çıktıya göre hareket mi, yoksa öldürmeye mi karar vereceğini bu belirler.
   **RED-first:** süreç bitmeden `palai.workspace.file` okuması KISMİ baytları döndürür.
3. **ÇIKIŞTA MODEL YENİDEN ÇAĞRILIR.** Bildirim görev id'sini, çıktı referansını, exit status'ünü ve kısa bir özeti taşır.
   **RED-first:** park etmiş bir run, görev bitince **tam olarak bir kez** yeniden girer ve modelin gördüğü turda exit code vardır.
4. **HANDLE İLE ÖLDÜRÜLEBİLİR.**
   **RED-first:** kill'den sonra süreç grubu/container YOK; ölçüm işletim sisteminden (`kill -0` / `ContainerInspect`), bizim satırımızdan değil.
5. **BİRDEN ÇOK GÖREV EŞZAMANLI KOŞAR.**
   **RED-first:** üç görev aynı anda koşar, üçünün çıktısı üç ayrı dosyadadır ve karışmaz.
6. **MODEL BLOKE OLMAZ.** Bu en çok önem taşıyan ve **en çok yanlış inşa edilmeye açık** olan özelliktir.
   **RED-first:** arka plan spawn'ından SONRA, süreç hâlâ koşarken, model **başka bir tool çağırır** ve o çağrı tamamlanır.

**PARK MEKANİZMA DEĞİL SONUÇTUR.** Model işi biterse turunu doğal olarak bitirir; çıkış bildirimi yenisini başlatır. E23 T1 ve E24 T4 bu koreografiyi zaten yazdı ve kanıtladı (`checkpointBeforePause` → `RunCmdWait` → dispatch worker serbest → dış olay `applyResumeTx`/`wakeParkedRunTx` + `EnqueueJob`). **İKİNCİSİ YAZILMAZ.** İki uyandırma yolu iki uyandırma hatası demektir.

**BUGÜNKÜ DAVRANIŞ BİT-DEĞİŞMEZDİR.** `background` bayrağı olmayan bir `palai.workspace.shell` çağrısı bugünkü kodun aynısını koşar: aynı `ShellRunner.Run`, aynı redaksiyon, aynı `ShellResult`, aynı ledger satırı.
**RED-first: `background` alanını hiç kullanmayan bir run'ın tool ledger satırları ve `ShellResult` alanları alan alan değişirse FAIL.**

**ONAY KAPISI YUKARIDA KALIR.** `approval_required` bir tool, arka plana alınarak kapıyı atlayamaz. Kapı `dispatchTool`'dadır (`tool_dispatch.go:155-186`) ve spawn ondan SONRA gelir.
**RED-first: gated bir tool `background:true` ile çağrıldığında, insan karar vermeden TEK BİR süreç başlamaz — spawn sayacı SIFIR.**

**ARKA PLAN GÖREVİ RUN'A AİTTİR, ONU BAŞLATAN PROCESS'E DEĞİL.** Bu, sahiplik sorusunun cevabıdır ve her task'ın kabul şartıdır: control plane restart'ı bir görevi öldürmez, bir run'ın iptali öldürür.

**BİR PGID'Yİ SAHİPLENDİĞİMİZİ KANITLAYAMIYORSAK ONA SİNYAL GÖNDERMEYİZ.** PID yeniden kullanımı gerçektir. Kanıtlanamayan bir handle `lost` olarak işaretlenir ve **asla** sinyal almaz. Yanlış bir process'i öldürmemek, kendi orphan'ımızı reap etmekten daha önemlidir.

---

## §3 — Doğrulanmış seam envanteri (hepsi `file:line`, hepsi `4d27acf3`'e karşı ölçüldü)

### 3.1 Süreç sahipliği — brief'in "önce ölç" dediği soru

**CEVAP: BUGÜN BİR ARKA PLAN SÜRECİ İMKÂNSIZDIR, ve iki postür bunu İKİ FARKLI SEBEPLE imkânsız kılar.**

| Postür | Sahiplik | Ölçüm |
|---|---|---|
| **OCI (container)** | Container'ın ömrü **`Run` çağrısının ömrüdür** | `dockerDriver.Run` içinde `defer` ile `ContainerRemove(..., {Force: true, RemoveVolumes: true})` — `adapters/sandboxes/oci/docker.go:145-152`. **Koşulsuz**, hata yolunda da çalışır. Duvar saati `wallCtx` ile `Run`'ın içinde kurulur (`:161`) ve timeout `SIGKILL` gönderir (`:184-189`) |
| **Host (native Mac)** | Süreç GRUBUNUN ömrü **attempt context'inin ömrüdür** | `exec.CommandContext(ctx, ...)` — `adapters/sandboxes/host/exec.go:146`; `Setpgid: true` (`:152`); `c.Cancel = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)` (`:153`); `WaitDelay = 2s` (`:154`). Yani ctx iptali **bütün ağacı** öldürür — dosyanın kendi başlığı bunu bir ÖZELLİK olarak yazıyor (`:15-16`: *"a process-GROUP kill so a reaped `xcodebuild` does not leave a compiler behind"*) |

**Ctx zinciri, uçtan uca ölçüldü:**
`Worker.Run(ctx)` → `w.process(ctx, …)` → `w.handle(storage.WithTenant(ctx, …), …)` (`packages/coordinator/worker.go:131`) → `ExecuteRun` → `orch.ExecuteAttempt(ctx, …)` (`apps/control-plane/internal/execution/execute_run.go:48`) → `dispatchTool(ctx, …)` → `o.tools.Execute(ctx, …)` (`tool_dispatch.go:249`) → `shellExec` → `env.Shell.Run(ctx, …)` (`tools/shell.go:66`).

**Sonuç:** süreç, `ShellRunner.Run` dönene kadar yaşar ve o çağrı `dispatchTool`'un içindedir; `dispatchTool` ise **attempt bittiğinde biter**. Park eden bir attempt (E23 T1) `dispatchTool`'dan ÇIKAR — yani "park et ve arka planda koştur" bugünkü sahiplikte bir çelişkidir.

**NE DEĞİŞMELİ (T1'in tamamı budur):**

- **OCI:** `Run`'a dokunulmaz. **Yeni** `StartDetached(ctx, spec, label) (containerID, error)` — create + start, **defer remove YOK**. Handle = container id + `io.palai.bg=<task_id>` etiketi. Restart'tan sağ çıkması bedavadır: container daemon'ın nesnesidir, bizim process'imizin değil. `Probe` = `ContainerInspect` (running/exit code), `Kill` = `ContainerKill` + `ContainerRemove`.
- **Host:** `CommandContext` DEĞİL `exec.Command`; `Start()` (Wait değil); `Setpgid` korunur; handle = **pgid + process başlangıç zamanı**. Başlangıç zamanı PID yeniden kullanımına karşı tek dürüst kanıttır ve §2'nin "kanıtlayamıyorsak sinyal göndermeyiz" kuralı buraya dayanır. `Wait` bir gözcü goroutine'inde çağrılır (zombie bırakmamak için) **ama gözcü otorite değildir** — otorite reaper'ın probe'udur, çünkü gözcü restart'tan sağ çıkmaz.

### 3.2 Çıktı nereye gider

| Aday | Ölçüm | Karar |
|---|---|---|
| **Artifact store (E09 T2)** | `Writer.Write` `Content []byte` alır ve **bütün gövdeyi** bir kere yazar (`apps/control-plane/internal/artifacts/writer.go:43-51`, `:70`); object-önce-row-sonra (`:70-100`). Append yok, kısmi okuma yok, uçuş-sırası handle yok | **HAYIR** — artifact BİTMİŞ bir gövdedir |
| **Yeni bir stream kanalı** | `ShellRunner` tek metotlu (`packages/tool-broker/sandbox_exec.go:71-73`); `ShellResult` tek sonuç (`:90-99`). `known-gaps` `CAS-P2` bunu zaten yazıyor | **HAYIR** — dördüncü bir şey |
| **Allocation içinde bir DOSYA** | `.palai-session` zaten var (`adapters/sandboxes/oci/workspace/allocation.go:42`), snapshot onu **alt-ağaç olarak atlıyor** (`:106-111`), ve `palai.workspace.file` allocation kökü altındaki bir yolu **zaten sınırlı ve escape-kontrollü** okuyor (`execution/tools/file.go:44-48`; `workspace/exec.go:147-169`) | **EVET** |

**Ve bu vendor'ın kendi cevabıdır:** `TaskOutput` tool'u birebir *"Retrieves output from a background task. **Deprecated in favor of `Read` on the task's output file path.**"* (§3.5 P2). Yani replike edilecek harness bile ayrı bir okuma tool'unu bırakıp sıradan dosya okumasına döndü.

**Yol:** `<allocation>/.palai-session/bg/<task_id>.log`, stdout ve stderr birleşik (harness da tek dosya verir).
**Kim yazar:**
- host: `c.Stdout = f; c.Stderr = f` — doğrudan.
- OCI: container kendi yazar. Argv güvenliği KORUNUR — `sh -c 'exec "$@" >"$0" 2>&1' <logpath> <argv...>` formu argv'yi konumsal parametre olarak geçirir, **hiçbir metakarakter yorumlanmaz**. Bu, `workspace/exec.go:313-316`'daki `cmd.Shell` join'inin taşıdığı riski taşımaz ve bir testtir.

**`.palai-session`'ı KİM yaratır:** bugün yalnız host executor (`host/exec.go:214-229`); `workspace.Prepare` `repo/`, `scratch/`, `artifacts/` yaratır ve session dizinini **yaratmaz** (`allocation.go:61-68`). T1 `bg/` alt dizinini spawn anında yaratır — iki postürde de aynı yerde.

### 3.3 Run'ı kim uyandırır

`Reconciler.Sweep` (`apps/control-plane/internal/execution/reconciler.go:64-91`) — E24 T5'in kendi gerekçesiyle: *"it rides this loop rather than a reaper of its own because this loop already exists, already spans tenants, and is already supervised"* (`:83-86`). `ReconcileStore` arayüzü (`:12-29`) beş süpürme taşıyor; E26 iki tane ekler.

Uyandırmanın kendisi **yazılmaz, çağrılır**: `wakeParkedRunTx` (`packages/coordinator/approvals.go:533-568`) — waiting→running + `EnqueueJob`, aynı transaction'da, tek kazanan, waiting olmayan run'da no-op.

Bildirim `message.deliver` ile iner: `pumpCommands` (`execution/command_pump.go:45-139`, frame `:126-133`), durable delivered_messages satırı + yeniden teslim (`:150-166`), motor tarafında zaten kabul edilen bir tip (`engines/reference/tests/test_schema_pin.py:34`).

### 3.4 Diğer ölçülen seam'ler

| Seam | Konum | E26 için önemi |
|---|---|---|
| Park koreografisi | `execution/approval.go:56-66` (`parkForApproval`), `execution/detach.go:29-65`, `execution/placement.go:76+` | Üçüncü kullanıcı E26'dır; gövde kopyalanmaz, `parkForApproval` **genelleştirilir** (o zaten checkpoint-sink-suz parkı kabul eden tek varyant) |
| Run terminali | `execution/orchestrator.go:642` → `execution/finalize.go:108-140` | Park kapısı buraya girer: canlı görevi olan bir run `completed` olamaz |
| Tool ledger | `tool_dispatch.go:29-308`; `tool_calls.state` **CHECK'siz TEXT** (`000044:24-25`) | Spawn çağrısının kendisi normal bir tool çağrısıdır ve normal commit edilir; SÜREÇ ayrı bir tablodur |
| Env değerleri | `tool_dispatch.go:234-245` (`resolveEnvValues`), `packages/tool-broker/sandbox_exec.go:44-57` | §0.4 / T6 |
| Redaksiyon | `packages/tool-broker/redact.go`; uygulanışı `host/exec.go:164-166` ve `workspace/exec.go:383-385` | **Yakalanan Go string'i üzerinde**, disk üzerindeki bayt üzerinde değil (§3.6 D8) |
| İptal | `packages/coordinator/orchestration.go:301-345` (`CancelRunReconciled`) | Hiçbir process'e sinyal göndermiyor (§3.6 D10) |
| Eşzamanlılık | `main.go:507` (`PALAI_DISPATCH_WORKERS`, varsayılan 1), `deploy/compose/compose.yaml:82` (**0**), `deploy/compose/production.yml:49` (1) | §3.6 D12/D22 |
| Motor döngüsü | `engines/reference/src/palai_engine/loop.py:191-213`, `:124` | Hızlı dönen bir tool ile arka plana alınmış bir tool'u ayırt edemez → motor değişmez |

---

## §3.5 — Sapma tablosu: yayımlanmış kontrat × bizim varsayımlarımız

> Kural: **her satır bir kaynak URL'i ve bir çekim tarihi taşır.** `UNCONFIRMED` işaretli hiçbir satır koda GİRMEZ; yalnızca bir §6 ölçümü ya da bir RED testi olur.

| # | Yayımlanmış kontrat (birebir alıntı) | Kaynak + çekim | Bizim varsayımımız / sapma | Nereye |
|---|---|---|---|---|
| **P1** | *"For long-running processes such as dev servers or watch builds, Claude can set `run_in_background: true` to start the command as a background task and continue working while it runs. List and stop background tasks with `/tasks`."* | `https://code.claude.com/docs/en/tools-reference` — çekildi **2026-07-30** | **BİREBİR ALINIR:** arka plan bir **parametredir**, ayrı bir tool değil. Palai'de `palai.workspace.shell`'in `background` alanı. Sapma: Palai'de `/tasks` yok; listeleme `background_tasks` satırları + konsol (E28) | T2 |
| **P2** | *"`TaskOutput` — Retrieves output from a background task. **Deprecated in favor of `Read` on the task's output file path.**"* | aynı sayfa, **2026-07-30** | **BİREBİR ALINIR:** ayrı bir okuma tool'u YAZILMAZ; çıktı `palai.workspace.file` ile okunur. §3.2'nin kararı bu satıra dayanır | T2 |
| **P3** | *"`TaskStop` — Stops a running background task by ID."* | aynı sayfa, **2026-07-30** | **BİREBİR ALINIR:** `palai.workspace.background_kill`, argümanı yalnız `task_id`. Sapma: harness'ta bu bir kullanıcı komutu da olabilir; Palai'de yalnız model ve operatör (CLI) çağırır | T2 |
| **P4** | *"When a command reaches its timeout without finishing, Claude Code moves it to the background instead of stopping it… `Command did not complete within its 120s timeout and was moved to the background`, with the seconds matching the timeout that applied, followed by the task ID and the path of the file the output is being written to."* | aynı sayfa, **2026-07-30** | **KISMEN ALINIR ve sapma AÇIKÇA yazılır.** OTOMATİK arka plana alma E26'da **YOKTUR**: senkron bir Palai komutunun timeout'u `TimedOut` bayrağı üretir ve container/process grubu zaten SIGKILL'lenmiştir (`docker.go:183-189`, `host/exec.go:198-201`) — onu "arka plana taşımak" bir kod yolu değil, öldürülmüş bir süreci diriltmektir. **Modelin `background:true` demesi gerekir.** Otomatik taşıma §5'tedir | §5 |
| **P5** | *"Claude Code never auto-backgrounds a command that starts with `sleep`"* ve *"setting `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1` disables auto-backgrounding along with the rest of the background task functionality"* | aynı sayfa, **2026-07-30** | **KAPATMA ANAHTARI ALINIR:** `PALAI_BACKGROUND_DISABLED=1` bütün özelliği kapatır ve `background:true` **REDDEDİLİR** (sessizce senkrona düşmez — bir modelin arka planda sandığı şeyin senkron koşması, tam olarak bloke olma davranışıdır). `sleep` istisnası ALINMAZ: otomatik taşıma yok, dolayısıyla istisnaya konu yok | T2 |
| **P6** | *"A `cd`, `pushd`, `popd`, or `chdir` inside a command that is moved to the background never carries over: `Session cwd remains <dir>; directory changes made by the backgrounded command do not apply to subsequent commands.`"* | aynı sayfa, **2026-07-30** | **Palai'de yapısal olarak zaten böyle** ve bu bir kazadır, tasarım değil: her komut kendi process'inde/container'ında koşar ve `c.Dir = cmd.WorkspaceRoot` (`host/exec.go:147`) / `WorkingDir: shellMountTarget` (`workspace/exec.go:332`) her seferinde yeniden kurulur. **Bir RED testiyle KARARA dönüştürülür** | T2 |
| **P7** | *"make an API request with `background` set to `true`"* / *"use the GET endpoint for Responses. Keep polling while the request is in the queued or in_progress state."* / *"keep track of a 'cursor' corresponding to the `sequence_number`"* / *"Cancelling twice is idempotent."* / *"response data is temporarily stored to disk for roughly 10 minutes"* | `https://developers.openai.com/api/docs/guides/background` — çekildi **2026-07-30** | **AYIRT EDİCİ SAPMA, ve karıştırılması pahalı olurdu:** OpenAI'nin `background`'ı **YANITIN kendisini** asenkron yapar (istemci poll eder). E26'nın `background`'ı **BİR TOOL ÇAĞRISINI** asenkron yapar; yanıt akışı açık kalır. İkisi ortogonaldir ve Palai bir Responses-şekilli API'dir, yani **isim çakışması gerçek bir yanlış anlama riskidir** — bu yüzden Palai'nin alan adı tool argümanında `background`, response gövdesinde HİÇBİR yerde değildir. "Cancelling twice is idempotent" **alınır** (kill idempotenttir) | T2, T5 |
| **P8** | *"As a default, Docker uses the `json-file` logging driver, which caches container logs as JSON internally."* ve *"By default, no log-rotation is performed"* | `https://docs.docker.com/engine/logging/configure/` — çekildi **2026-07-30** | **KULLANILMAZ, ve sebebi yazılır:** `docker logs` ikinci bir okuma yolu olurdu ve iki okuma yolu iki redaksiyon yolu demektir (§3.6 D8). Container kendi log dosyasını bind mount'a yazar. **Yan fayda:** rotasyon yapılmadığı için json-file'a güvenmek sınırsız disk demekti; bizim dosyamızın tavanı §0.2'nin duvar saatidir | T1 |
| **P9** | **UNCONFIRMED** — sayfa, container durduktan sonra logların kalıcı olup olmadığını **söylemiyor** | aynı sayfa, **2026-07-30** | Zaten kullanılmıyor (P8). Kayda geçirilir ki bir sonraki okuyucu "docker logs yeter" demesin | — |
| **P10** | **UNVERIFIED** — `https://pkg.go.dev/os/exec` çekilemedi (`ECONNRESET`, 2026-07-30). `Cmd.Wait`'in çağrılma zorunluluğu, `Start`'tan sonra parent'ın çıkışının çocuğu öldürmemesi, ve reparenting standart POSIX davranışıdır **ama bu planda bir kaynak olarak sayılmaz** | — | **BİR VARSAYIM DEĞİL BİR ÖLÇÜM OLUR:** T1 bunu `TestAHostBackgroundProcessSurvivesTheAttempt` ile MAKİNEDE ölçer. Ölçüm yanlış çıkarsa host postürü §5'e düşer ve OCI tek postür kalır | T1 |
| **P11** | **UNCONFIRMED** — `https://code.claude.com/docs/en/settings` sayfasında adında `BASH` geçen **hiçbir** ortam değişkeni yok (arandı, **2026-07-30**). Yani harness'ın varsayılan timeout'u yayımlanmış bir sayı olarak elimizde YOK | — | **Bir vendor varsayılanı kodlanmaz.** §0.2'nin `60m`'si bizim kararımızdır ve öyle yazılır | §0.2 |

---

## §3.6 — Ağacın kendi hakkındaki yanlış inançları (**yirmi iki satır**)

> Bu bölüm her planın en değerli bölümü olmuştur. Aşağıdakilerin **dördü bu epic'i sipariş eden brief'in kendi cümleleridir** ve öyle işaretlidir; **üçü E24/E25'in yayımlanmış plan satırlarıdır**; **ikisi ölçüldüğünde bir özelliğin hiç gönderilmemiş olduğunu** gösterdi.

| # | İnanç | Ölçüm | Nereye |
|---|---|---|---|
| **D1** | ⭐⭐⭐ **BRIEF'İN CÜMLESİ:** *"A tool call is synchronous today, bounded by `PALAI_SANDBOX_WALL_TIME`."* | **SENKRON KISMI DOĞRU, SINIR KISMI HİÇBİR GÖNDERİLMİŞ DEPLOYMENT'TA YOK.** `PALAI_SANDBOX_WALL_TIME=` ataması ağacın **tamamında sıfır** kez geçiyor (grep, 2026-07-30; tek isabet `main_test.go:310`'un `t.Setenv`'i). `envDuration` set edilmemiş değişken için **0** döner (`main.go:1823-1829`). Sonra iki postür bu 0'ı **zıt yönlerde** yorumluyor: host `if e.wallTime > 0` (`host/exec.go:125`) ⇒ **TAMAMEN SINIRSIZ**; OCI ise `validate()` `WallTime <= 0`'ı **REDDEDİYOR** (`oci/driver.go:105-107`) ⇒ **her shell çağrısı sert hata**. Yani "eşleştirilecek" sınır ya yok ya da tool'u tamamen kırıyor | §0.2, T5 |
| **D2** | ⭐⭐⭐ **BRIEF'İN SORUSU:** *"does a background process survive the attempt today?"* | **HAYIR, VE İKİ POSTÜR BUNU İKİ FARKLI SEBEPLE İMKÂNSIZ KILAR.** OCI: `defer ContainerRemove(Force:true, RemoveVolumes:true)` (`oci/docker.go:145-152`) — koşulsuz, hata yolunda da. Host: `exec.CommandContext` + `c.Cancel = kill(-pgid, SIGKILL)` + `WaitDelay=2s` (`host/exec.go:146-154`). **Sahiplik `ShellRunner.Run` ÇAĞRISINA bağlıdır, run'a değil** — ve park eden bir attempt o çağrıdan çıkar, yani "park et ve arka planda koştur" bugünkü sahiplikte bir çelişkidir. §3.1 ne değişmesi gerektiğini yazıyor | T1 |
| **D3** | ⭐⭐ **BRIEF'İN ÖLÇÜMÜ:** *"`ResponseCmdRequestTool` and `ResponseWaitingForTool` exist ONLY in `packages/state-machines/response.go` and its own test."* | **DOĞRU, AMA ÖLÇÜM ÇOK DAHA BÜYÜK: BÜTÜN `ResponseTable`'IN PRODUCTION CALLER'I YOK.** `RunTable` uygulanıyor (`coordinator/store.go:466`); `ResponseTable` yalnız `response.go`, `response_test.go` ve `property_test.go`'da geçiyor (grep, 2026-07-30). **Ve yayımlanmış kontrat üç durumu ilan ediyor:** `protocols/schemas/execution/response.json:28-29` `waiting_for_tool`/`waiting_for_approval`/`waiting_for_input` sayıyor, `protocols/asyncapi/asyncapi-3.1.yaml:57-59` üç olay tipini sayıyor, `tests/conformance/contracts/response_test.go:112-113` enum'u asserte ediyor — **ve hiçbiri hiç yazılmadı.** Yani E23'ün insan-kapılı akışında bir yanıt `in_progress` okuyor. E26 `ResponseTable`'ın **ilk production caller'ı** olur | T3 |
| **D4** | ⭐⭐ **E24 bir run'ı bir Mac'e yerleştirdi, dolayısıyla shell o Mac'te koşuyor** | **E24 T7 — İCRA RELAY'İ — HİÇ GÖNDERİLMEDİ.** git log E24 T1-T6 ve T8'i taşıyor, T7'yi taşımıyor; planın adıyla ısmarladığı üç dosya yok (`execution/runner_shell.go`, `execution/runner_workspace.go`, `packages/runner/exec.go`); `tests/uat/cases/FLT-006` yok (FLT-001..005 var). **E24 T8 bunu kendi commit başlığında yazmış:** *"the fleet page did not tell an operator that a Mac pool does not run xcodebuild on the Mac"* (`f40f2a40`). Ve `o.shell` **process başına TEK** (`orchestrator.go:57`, `main.go:646`'da bir kez set edilir). **Sonuç E26 için belirleyicidir: arka plan icrasının koşacağı tam olarak BİR makine vardır, control plane'in kendi makinesi** | §2, §5 |
| **D5** | **"E26" adı boştur** | **ÜÇ ŞEY BİRDEN ONU İSTİYOR.** `phase-24-runner-fleet.md` §7: *"ölçekleyici, spawn seam'i ve bulut sağlayıcıları **E26**'ya alınmıştır"* ve *"T7b (workspace relay'i, **E26**)"*. `phase-25-admin-console.md:294`: *"Geriye 15 yüzey … → **E26**"* (ayrıca `:59`, `:296`). Bir numara bir plan değildir; §0.1 karar ister | §0.1 |
| **D6** | **Kısmi çıktı bir artifact olur (E09 T2 write-path'i var)** | **ARTIFACT BİTMİŞ BİR GÖVDEDİR.** `WriteRequest.Content []byte` (`artifacts/writer.go:43-51`), `Write` önce object'i sonra satırı yazar (`:70-100`); append yok, kısmi okuma yok, uçuş-sırası handle yok. **Ve replike edilen harness'ın kendi cevabı da artifact değil:** `TaskOutput` *"Deprecated in favor of `Read` on the task's output file path"* (§3.5 P2) | §3.2, T2 |
| **D7** | **Çıktıyı okumak için yeni bir tool gerekir** | **GEREKMİYOR, VE DİZİN DE ZATEN VAR.** `palai.workspace.file` allocation kökü altını escape-kontrollü ve sınırlı okuyor (`tools/file.go:44-48`; `workspace/exec.go:147-169`). `.palai-session` sabit (`allocation.go:42`) ve snapshot onu **alt-ağaç olarak** atlıyor (`:106-111`) — yani oraya yazılan bir log ne snapshot'a girer ne de checksum'a katılır. **Eksik olan tek şey:** dizini bugün yalnız host executor yaratıyor (`host/exec.go:214-229`), `workspace.Prepare` yaratmıyor (`allocation.go:61-68`) | §3.2, T1 |
| **D8** | ⭐ **"Redaction is a property of the result, not of the container"** (`workspace/exec.go:379-382`, birebir) | **YAKALANAN GO STRING'İNİN özelliğidir, DİSKTEKİ BAYTIN DEĞİL.** `host.Run` `stdout.String()`'i redakte ediyor (`host/exec.go:164-175`), `ShellExecutor.Run` `outcome.Stdout`'u (`workspace/exec.go:340-341`). **Kendi log dosyasını yazan bir süreç ikisini de atlar.** E26'nın okuma yolu redaksiyonu borçlanır — ve değer-tabanlı yarısı (`RedactValues`) env değerlerinin **okuma anında yeniden çözülmesini** gerektirir, ki bu E25 T3'ün iddiasının ilk kez yeniden yazılması demektir | T6 |
| **D9** | ⭐⭐⭐ **E25 T3: "the value's whole life is one Execute call"** (`tool-broker/sandbox_exec.go:54-56`, birebir) | **BİR ARKA PLAN GÖREVİ İÇİN YAPISAL OLARAK YANLIŞ, VE HİÇBİR KOD DEĞİŞİKLİĞİ ONU DÜZELTEMEZ.** `exec.Cmd.Env` (`host/exec.go:148`) / `container.Config.Env` (`oci/docker.go:97`) ile verilen bir değer çekirdeğin environ kopyasında sürecin ömrü boyunca durur; control plane'i öldürmek onu geri almaz. E25 T3 *"There is no handle to redeem and no deadline to check"* dedi ve o an haklıydı; **E26 tam olarak o üçlüyü (scope + fence + expiry) geri istiyor** — `internal/workers/store.go`'nun `RedeemSecretHandle`'ının reddedilme gerekçesi buydu | §0.4, T6 |
| **D10** | ⭐ **Bir run iptali koşan işi durdurur** | **DURDURMUYOR.** `CancelRunReconciled` (`coordinator/orchestration.go:301-345`) run'ı canceled'a sürüyor, çocukları iptal ediyor, yanıtı finalize ediyor — **hiçbir process'e sinyal göndermiyor, hiçbir context iptal etmiyor.** Worker'ın ctx'i process kökü (`worker.go:131`), run başına değil. Ağaç sonucu zaten adlandırmış: `failed_with_uncertain_side_effect` (`orchestration.go:290-300`, `contracts.UncertainSideEffectProblem`). **Yani arka plana alınmış bir build kendi run'ının iptalinden bugün de sağ çıkardı** — E26 onu açıkça öldürmezse öyle kalır | T5 |
| **D11** | **Reconciler bir reaper'dır, yeni bir süpürme eklenir, biter** | **DOĞRU — VE E24 T5 HEM DESENİ HEM ANTİ-DESENİ KANITLADI.** `SweepExpiredCapacityParks` `Reconciler.Sweep`'e bindi çünkü *"this loop already exists, already spans tenants, and is already supervised"* (`reconciler.go:83-86`). Ama TTL varsayılanı **SIFIR = ASLA** (`:41-44`), çünkü dürüst bir varsayılan yoktu. **E26 o sıfırı KOPYALAYAMAZ:** tavansız bir arka plan süreci brief'in adıyla saydığı orphan'dır | §0.2, T5 |
| **D12** | ⭐ **BRIEF'İN CÜMLESİ:** *"This is the `Capacity` field E24 measured as declared-but-never-enforced — do not repeat that."* | **DOĞRU VE SÜTUN HÂLÂ ORADA:** `runners.capacity INTEGER NOT NULL DEFAULT 1 CHECK (capacity > 0)` (`000045_runner_fleet.up.sql:115`), hiçbir Go ifadesi okumuyor (grep, 2026-07-30). **AMA GERÇEK EŞZAMANLILIK TAVANI BAŞKA BİR YERDE:** `PALAI_DISPATCH_WORKERS` varsayılan **1** (`main.go:507`), compose **0** (`compose.yaml:82`), production **1** (`production.yml:49`) — ve bir dispatch worker `ExecuteAttempt`'i run'ın **bütün ömrü** boyunca tutar. Yani "birden çok arka plan görevi" bir RUN'ın dispatcher'ının özelliğidir, filonun değil | §0.3, T5 |
| **D13** | ⭐ **BRIEF'İN SORUSU:** *"`workers` already has an async job journal with claims and fences — do E24 §3.6 D14's three reasons apply here?"* | **E24 DÖRT SEBEP SAYDI (kendi düz yazısı "üç" diyor, listesi dörttür — bu da bir düzeltmedir), VE ÜÇÜ HÂLÂ GEÇERLİ, BİRİ DEĞİL.** (1) *loopback-only cleartext listener* (`main.go:1589-1608`) — **E26 için GEÇERSİZ**, çünkü E26 bir makine sınırı geçmiyor (D4). (2) *düzlem üç şekilde uykuda* — token mintleyen yok, `DispatchJob` çağıran yok, health/reaper yok (`known-gaps` `WRK-2`, E19 T8'de yeniden doğrulandı) — **GEÇERLİ**. (3) *yapısal olarak tipli operasyon*: `ErrUntypedOperation` (`workers/types.go:127-129`) genel bir shell'i imkânsız kılıyor ve bir arka plan komutu **tam olarak** genel bir shell'dir — **GEÇERLİ**. (4) *enrollment bellekte ve tek kullanımlık* (`workers/gateway.go:33`, `:132`), restart siliyor — **GEÇERLİ**. **Cevap hâlâ hayır, ama E24'ünkinden bir sebep eksikle** — ve fark yazılır, miras alınmaz | §5 |
| **D14** | ⭐⭐ **Reconciler her deployment'ta koşar** | **KOŞMUYOR.** `startDispatch` `if workers <= 0 { return }` ile **erken dönüyor** (`main.go:508-510`) ve reconciler (`:670-672`), fleet heartbeat (`:673-678`) ve uncertain-tool reconciler (`:687-689`) **o return'ün ALTINDA**. `deploy/compose/compose.yaml:82` ise `PALAI_DISPATCH_WORKERS:-0` diyor. **Yani varsayılan compose stack'inde dead-letter süpürmesi, onay expiry'si, kapasite-park expiry'si ve belirsiz-tool uzlaştırması HİÇ ÇALIŞMIYOR.** production overlay 1 diyor, yani üretim iyi. E26'nın uyandırma yolu aynı döngüye biniyor: **plan bunu "her zaman koşar" diye varsayamaz ve T4 bunu bir testle sabitler** | T4 |
| **D15** | **Uzun bir komutun sessizliği bilinen ve KABUL EDİLMİŞ bir tavandır** | **`CAS-P2` onu yazmış VE kapatılmasını bir ön koşula bağlamış:** *"NO LIVE PROGRESS: a long build reports nothing until it ends… **Opening it before a real multi-step run exists is the YAGNI this plan rejects**"* (`docs/operations/known-gaps-1.0.md:227`, owner, 2026-07-28). **Ön koşul karşılandı** — E21/E22 gerçek çok adımlı coding run'ları gönderdi. Satır hâlâ `post-1.0` okuyor. **E26 `CAS-P2`'nin sahibidir** ve onu kapatmaz, DARALTIR: canlı akış hâlâ yok, ama sessizlik artık bir dosyayı okuyarak kırılabilir | T2, T7 |
| **D16** | **Modelin bloke olmaması motor tarafında bir değişiklik ister** | **İSTEMİYOR.** `_request_tools` bir turun tool_call'larını dağıtıp `_pending_tools`'u bekliyor (`loop.py:191-213`, `:124`); **hemen cevap veren bir tool onu hemen serbest bırakır** ve motor hızlı bir tool ile arka plana alınmış bir tool'u ayırt edemez. Bu özelliğin zor olan her şeyi control-plane tarafındadır | §2, §5 |
| **D17** | **Bildirimin motora ulaşması yeni bir frame ailesi ister** | **İSTEMİYOR:** `message.deliver` var, girdi sınırında katlanıyor (`command_pump.go:126-133`), durable exactly-once satırı + yeniden teslimi var (`:150-166`), motor tipi zaten kabul ediyor (`test_schema_pin.py:34`). **OLMAYAN ŞEY BİR ROL AYRIMI:** control-plane'in yazdığı bir bildirim motora **kullanıcı turu** olarak katlanır (`engines/reference/src/palai_engine/commands.py:19`, `context.queue_delivery`). Bu **adlandırılacak bir tavandır**, icat edilecek bir frame ailesi değil | T4 |
| **D18** | **`PALAI_SANDBOX_IMAGE` gönderilen compose'da ayarlıdır** | **DEĞİL:** `deploy/` altında sıfır atama (grep, 2026-07-30). Yani gönderilen compose stack'inde `shellRunnerFromEnv` **nil** dönüyor (`main.go:836-838`) ve **shell tool'u hiç yok.** Postür yalnız uat coding journey'sinde (`scripts/uat/coding:64`) ve native Mac yolunda kuruluyor. **E26'nın gerçek hedef deployment'ı native Mac'tir** ve plan öyle sıralanır | §6, T7 |
| **D19** | **Bundle sweep tablosu on altı sürüm taşır** (ağacın hafızasındaki sayı) | **YİRMİ ÜÇ.** `evidence/releases` altında 23 dizin var ve hepsi `committedBundleSurfaces`'ta beyanlı (`tests/uat/evidence.go:2732+`). Yeni bir bundle **hem** oraya **hem** `caseChecksumParts`'a girer ve **kendini legacy ilan EDEMEZ** — kuralın gerekçesi dosyanın kendisinde yazılı (`:2749-2753`) | T7 |
| **D20** | **Yeni bir component testi `TEST=<suite>` ile koşar** | **YALNIZCA adı `scripts/test/component`'in `case`'inde geçerse** (`:1033-1050`) **VE** `native-shell` gibi `-run` allow-list'li bir suite'e ekleniyorsa **test ADI o listede geçerse** (`:1016-1030`). Dosyanın kendi cümlesi: *"a guard this `-run` does not name is a guard this tier never runs"* (`:1024-1025`) — E24 T8 bu tuzağın **beşinci** örneğini buldu (`f4975645`) | T7 |
| **D21** | **`tool_calls.state` bir CHECK ile sınırlıdır, yeni bir durum bir migration ister** | **DEĞİL:** `000044_tool_approvals.up.sql:24-25` birebir *"tool_calls.state is TEXT with NO CHECK"*. **Ama E26'nın sütuna ihtiyacı da yok:** bir arka plan SÜRECİ bir tool ÇAĞRISI değildir — bir çağrı onu doğurur ve süreç o çağrının ledger satırından uzun yaşar. Ayrı bir tablo, tek FK | T1 |
| **D22** | **Bir `-run`'sız `go test ./...` yeni tier'ı kapsar** | Kapsamaz ve bu ağaçta **iki kez** bedel ödetti (`reverify-must-run-gate-corpora`): hedefli `-run` catalog + tenancy gate corpus'unu atlıyor. E26 **bir case.yaml, bir migration VE bir tenant tablosu** getiriyor — üçü de o corpus'ları tetikliyor. `tests/uat/automation` + `tests/security/tenancy` her task'ta koşar | her task |

---

## §4 — Task breakdown

### 4.0 Epic'in şekli, tek cümlede

**Zor olan park etmek değil, SÜREÇ SAHİPLİĞİDİR** (D2), **ve sahiplik değiştikten sonra geri kalan her şey ağaçta zaten yazılmış bir koreografinin üçüncü kullanıcısıdır.** T1 sahipliği taşır; T2 vendor semantiğini bağlar; T3 run'ın erken bitmesini engeller ve yayımlanmış bir kontratı ilk kez doğru yapar; T4 uyandırır; T5 tavanları, orphan'ları ve iptali uygular; T6 credential sorusunu cevaplar; T7 kapıyı kapatır.

### DAG

```
        T1 (mig 000047 + sahiplik + iki implementasyon)
         │
         ├──────────────► T2 (tool yüzeyi: background / kill / okuma)
         │                 │
         │                 ├──────────► T3 (park kapısı + ResponseTable'ın ilk caller'ı)
         │                 │             │
         │                 │             └──► T4 (uyandırma + bildirim)
         │                 │                   │
         │                 │                   └──► T5 (tavanlar, orphan, iptal, eşzamanlılık)
         │                 │                         │
         └─────────────────┴──► T6 (secrets + redaksiyon) ────────┤
                                                                   │
                                                                   ▼
                                                        T7 (EXIT gate: background-execution-0.1.0)
```

**Wave 1:** T1 · **Wave 2:** T2 · **Wave 3:** T3 ‖ T6 · **Wave 4:** T4 · **Wave 5:** T5 · **Wave 6:** T7.

**SECURITY-CRITICAL (full review): T1, T2, T5, T6.**

---

### T1 — Süreç sahipliği run'a geçer (**mig 000047**; SECURITY-CRITICAL)

**BU TASK EPIC'İN TAMAMIDIR; geri kalan altısı ondan çıkar.**

- [ ] **RED önce (1), OCI:** `StartDetached` ile başlatılan bir container, çağrı döndükten **sonra** hâlâ `running`'dir. Ölçüm `ContainerInspect`'ten gelir, bizim satırımızdan değil. **Bugün imkânsız** (`docker.go:145-152`).
- [ ] **RED önce (2), HOST:** başlatılan process grubu, `Start` döndükten sonra hâlâ canlıdır (`syscall.Kill(-pgid, 0)`), **ve attempt context'i iptal edildikten sonra da canlıdır.** **Bugün imkânsız** (`host/exec.go:146-154`). **Bu aynı zamanda §3.5 P10'un ölçümüdür** — POSIX reparenting bir kaynak değil, bir sonuç olarak kanıtlanır.
- [ ] **RED önce (3), BİT-DEĞİŞMEZLİK:** `background` kullanmayan bir shell çağrısının `ShellResult`'ı **alan alan** bugünküyle aynıdır ve `dockerDriver.Run` **tek bayt** değişmemiştir (`git diff --stat` üzerinde bir guard).
- [ ] **RED önce (4), SAHİPLENMEDİĞİMİZ PGID'YE SİNYAL YOK:** kaydedilmiş başlangıç zamanı canlı process'inkiyle uyuşmayan bir handle `lost` işaretlenir ve `Kill` **hiçbir sinyal göndermez**. Ölçüm: sahte bir pgid + gerçek bir process, ve o process hayatta kalır.
- [ ] **MIGRATION `000047` — `background_tasks` (YENİ tenant tablosu).** Sütunlar ve her birinin **neden kod olamayacağı**:
      `id`, `organization_id`, `project_id`, `run_id` (FK), `session_id`, `response_id`, `tool_call_id` (FK — onu doğuran çağrı), `attempt_fence` (**bir eski attempt'in yazması reddedilsin diye**), `posture` (`sandboxed-linux` | `unsandboxed-host`, CHECK — çünkü probe **postüre göre** farklı bir işletim sistemi nesnesine bakar ve tanınmayan bir postür yanlış nesneye bakardı), `handle` (container id **veya** `pgid:starttime` — postür ne demek olduğunu söyler), `state` (`running` | `exited` | `killed` | `expired` | `lost`, CHECK), `exit_code` (NULL = henüz yok, **`-1` DEĞİL** — bir exit code'u uydurmak `known-gaps`'in her satırının reddettiği şeydir), `output_path` (allocation'a göre relative), `env_keys` (TEXT[] — **ANAHTAR ADLARI, ASLA DEĞER**, T6), `deadline_at` (**reaper'ın okuduğu tek zaman**; bir `context` restart'tan sağ çıkmaz, bir sütun çıkar), `started_at`, `finished_at`, `notified_at` (**bildirimin exactly-once anahtarı**).
      Kısmi index: `(state) WHERE state = 'running'` (reaper'ın okuması), ve `UNIQUE (tool_call_id)` (bir çağrı bir süreç doğurur).
      `palai_apply_tenant_policy` + kendi `GRANT`'i; `tests/component/postgres/migration_test.go`'nun `allTables`'ına ve tenancy corpus'una kayıt (D22).
- [ ] **SEAM — `packages/tool-broker/background.go` (YENİ, ~50 satır).** `BackgroundRunner` **üç metot**: `Start(ctx, ShellCommand, BackgroundSpec) (Handle, error)`, `Probe(ctx, Handle) (Status, error)`, `Kill(ctx, Handle) error`. `ShellCommand` **yeniden kullanılır, genişletilmez** — E24'ün ölçtüğü serileştirilebilirlik korunur. `BackgroundSpec{TaskID, OutputPath}`. Broker mekanik sahiplenmez (bugünkü `ShellRunner` disiplini, `sandbox_exec.go:9-13`).
- [ ] **HOST implementasyonu** (`adapters/sandboxes/host/background.go`): `exec.Command` (CommandContext DEĞİL), `Setpgid`, `Start()`, log dosyası `c.Stdout`/`c.Stderr` olarak, `Wait`'i çağıran bir gözcü goroutine (zombie bırakmamak için) — **ama otorite gözcü değil reaper'ın probe'udur**, çünkü gözcü restart'tan sağ çıkmaz. `allowedEnv` + `LayerEnv` **aynen** kullanılır (`host/exec.go:205-247`), yani ortam allow-list'i ve çakışma reddi bit-değişmez.
- [ ] **OCI implementasyonu** (`adapters/sandboxes/oci/detached.go` + `.../workspace/background.go`): `createOptions(spec, false)` **aynen** kullanılır — yani sertleştirme (unprivileged uid, no network, read-only rootfs, CapDrop ALL, no-new-privileges, cgroup sınırları) **tek satır bile ayrışamaz**; fark yalnızca `defer remove`'un olmaması ve `io.palai.bg=<task_id>` etiketidir. Cmd: `sh -c 'exec "$@" >"$0" 2>&1' <logpath> <argv...>` (§3.2 — argv konumsal geçer, metakarakter yorumlanmaz, ve **bu bir testtir**).
- [ ] **`.palai-session/bg/` iki postürde de spawn anında yaratılır** (D7).
- **Seam:** yukarıdakiler + `execution/orchestrator.go` (`SetBackgroundRunner`), `main.go` (wiring). **UAT:** **BGT-001**. **Tier:** DEĞİŞMEZ.
- **Kanıt:** component-real gerçek Docker + gerçek host process; dört RED'in dördü; sertleştirme alanlarının `createOptions` ile **aynı** olduğunun yapısal (reflect/AST) kanıtı.
- **Honest ceiling:** **PID yeniden kullanımı host postüründe tam olarak çözülemez.** Başlangıç zamanı karşılaştırması en iyi çabadır ve §2'nin kuralı bunun üstüne kuruludur: kanıtlanamayan handle `lost` olur, öldürülmez. Bu, orphan'ı bize bırakır — ve **yanlış bir process'i öldürmemek, kendi orphan'ımızı reap etmekten önemlidir.**

---

### T2 — Tool yüzeyi: bir parametre, bir kill tool'u, ve YENİ BİR OKUMA TOOL'U YOK (mig YOK; SECURITY-CRITICAL; T1'e bağlı)

- [ ] **RED önce (1), §2.1:** `palai.workspace.shell` `background:true` ile çağrıldığında **süreç hâlâ koşarken** döner ve `{task_id, output_path, status:"running"}` verir.
- [ ] **RED önce (2), §2.2:** süreç bitmeden `palai.workspace.file` ile `output_path` okuması **kısmi** baytlar döndürür (satır sayısı artan iki ardışık okuma).
- [ ] **RED önce (3), §2.6 — BU EPIC'İN TANIMLAYICI TESTİ:** arka plan spawn'ından sonra, süreç hâlâ koşarken, model **başka bir tool çağırır ve o çağrı tamamlanır.** Bu bir park-ve-bekle değildir ve test bunu ayırt eder: parkı ölçen bir assert (attempt sonlandı mı) **ile** ikinci tool'un sonucunu ölçen bir assert aynı testte durur.
- [ ] **RED önce (4), §2.5:** üç görev eşzamanlı koşar, üç ayrı dosyaya yazar, çıktılar karışmaz.
- [ ] **RED önce (5), ONAY KAPISI:** `approval_required` bir tool `background:true` ile çağrıldığında, insan karar vermeden **spawn sayacı SIFIR**. (`tool_dispatch.go:155-186`'daki kapı spawn'dan önce durur ve bu yapısal bir sıra iddiasıdır.)
- [ ] **RED önce (6), §3.5 P6:** arka plana alınmış bir komutun `cd`'si sonraki komutu etkilemez — Palai'de yapısal olarak zaten böyle (`host/exec.go:147`, `workspace/exec.go:332`), **ve bir testle karara dönüştürülür.**
- [ ] **RED önce (7), §3.5 P5:** `PALAI_BACKGROUND_DISABLED=1` iken `background:true` **REDDEDİLİR** — sessizce senkrona düşmez. Sessiz düşüş, modelin arka planda sandığı şeyin bloke etmesi demektir ve tam olarak kaçınılan davranıştır.
- [ ] **`palai.workspace.background_kill`** — tek argüman `task_id`. `ReplayClass`: `ClassIdempotent` (iki kez öldürmek bir kez öldürmektir; §3.5 P7'nin *"Cancelling twice is idempotent"*'i).
- [ ] **Dönen gövde MİNİMUMDUR ve bu bir bağlam kararıdır:** spawn `{task_id, output_path, status}` döner — **çıktı YOKTUR.** Modelin çıktıyı görmesi açık bir okuma çağrısı ister (P2). Bir beş dakikalık build'in tamamını bağlama koymak owner'ın parasıdır.
- **Seam:** `tools/shell.go`, `tools/background_kill.go` (**YENİ**), `execution/background.go` (**YENİ**), `execution/tool_dispatch.go` (yalnız spawn dallanması). **UAT:** **BGT-002**. **Tier:** DEĞİŞMEZ.
- **Kanıt:** component-real; yedi RED; ve `background` alanı kullanılmayan bir run'ın ledger satırlarının + `ShellResult`'ının **alan alan** bit-değişmez olduğu.
- **Honest ceiling:** **Canlı ilerleme akışı YOK.** Model dosyayı okumak zorundadır; kimse ona "on satır daha geldi" demez. `CAS-P2` kapanmaz, **daralır** ve satırı öyle yeniden yazılır (T7).

---

### T3 — Park: canlı görevi olan bir run bitmez, ve bir yanıt ilk kez doğruyu söyler (mig YOK; T2'ye bağlı)

- [ ] **RED önce (1):** modeli `run.terminal{outcome:"completed"}` gönderen bir run, `running` bir arka plan görevi varken **`completed` OLMAZ** — `waiting` olur, attempt temiz biter, dispatch worker serbest kalır.
- [ ] **RED önce (2), D3:** o park sırasında **yanıtın durumu `waiting_for_tool`'dur.** Bugün `in_progress` okuyor ve yayımlanmış şema üç yıldır `waiting_for_tool`'u ilan ediyor (`protocols/schemas/execution/response.json:28-29`; `tests/conformance/contracts/response_test.go:112-113`). **E26 `ResponseTable`'ın ilk production caller'ıdır** (`packages/state-machines/response.go:46`, `:50`).
- [ ] **RED önce (3), MONOTONLUK:** `waiting_for_tool` **asla** bir terminal yanıtın üstüne yazılmaz; `FinalizeResponse`'ın koşullu UPDATE'i (`storage/queries/responses.sql:165-167`) ile aynı disiplin.
- [ ] **RED önce (4), SDK/CONSOLE KIRILMAZ:** SSE akışı park boyunca **açık kalır** — konsolun kendi yorumu bunu `waiting_for_approval` için zaten iddia ediyor (`apps/web-console/app/api/palai/stream/route.ts:15`) ve aynı iddia burada bir testtir.
- [ ] **PARK GÖVDESİ YAZILMAZ, GENELLEŞTİRİLİR.** `parkForApproval` (`execution/approval.go:56-66`) zaten checkpoint-sink-suz parkı kabul eden **tek** varyanttır ve gerekçesi (`:48-55`) burada birebir geçerlidir: uyanan attempt transkripti tekrar oynatır, aynı sınıra gelir ve aynı DB satırını okur. **İkinci bir gövde yazmak iki uyanma hatası demektir.**
- **Seam:** `execution/finalize.go`, `execution/approval.go` (yeniden adlandırma/genelleme), `packages/coordinator/responses` yazma yolu, `storage/queries/responses.sql`. **UAT:** **BGT-003**. **Tier:** DEĞİŞMEZ.
- **Honest ceiling:** yanıt açık kalır ve bu bir **ürün kararıdır**: kullanıcı build biterken yanıtın `in_progress`/`waiting_for_tool` olduğunu görür. Alternatif (run biter, bildirim YENİ bir yanıt açar) **iki uyanma yolu** demekti ve §2 onu reddediyor.

---

### T4 — Uyandırma: çıkış → bildirim → modelin bir sonraki turu (mig YOK; T3'e bağlı)

- [ ] **RED önce (1), §2.3:** park etmiş bir run, görevi bitince **tam olarak bir kez** yeniden girer ve modelin gördüğü turda `task_id`, exit code, `output_path` ve **sınırlı bir kuyruk** (son 2 KiB) vardır.
- [ ] **RED önce (2), EXACTLY-ONCE:** iki reconciler tick'i, iki paralel control plane, ve bir çökme-yeniden başlatma **tek bildirim** üretir. Anahtar `notified_at`'in tek kazananlı UPDATE'idir.
- [ ] **RED önce (3), KOŞAN RUN:** run park etmemişse (model hâlâ çalışıyorsa) bildirim **bir sonraki sınırda** katlanır ve run kesintiye uğramaz. Yani `wakeParkedRunTx` **koşulsuz çağrılmaz**; komut kuyruğa her hâlükârda yazılır, uyandırma yalnız `waiting` ise yapılır — **ikisi tek transaction'da.**
- [ ] **RED önce (4), TERMİNAL RUN:** run zaten terminal ise bildirim **yazılmaz ve kaybolmaz**: satır `orphaned_notice` damgası alır ve operatör onu görür. Sessizce düşürmek, `known-gaps`'in her satırının reddettiği şeydir.
- [ ] **RED önce (5), D14:** `PALAI_DISPATCH_WORKERS=0` bir stack'te reconciler koşmaz (`main.go:508-510`) — dolayısıyla **hiçbir bildirim inmez.** Test bunu bir sürpriz olarak değil bir **beyan** olarak sabitler ve `docs/operations/background-execution.md` bunu ilk paragrafında yazar.
- [ ] **`background_notice` komut kind'ı** `pumpCommands`'e `change_config`/`approve`/`deny` yanına eklenir (`command_pump.go:100-114`) ve **aynı** `message.deliver` frame'ini üretir. Motor değişmez (D16, D17).
- [ ] **Süpürme:** `ReconcileStore.SweepFinishedBackgroundTasks` → `Reconciler.Sweep`'e biner (`reconciler.go:64-91`), diğer beşinin yanına, aynı non-fatal disiplinle.
- **Seam:** `execution/reconciler.go`, `packages/coordinator/background.go` (**YENİ**), `execution/command_pump.go`, `storage/queries/background.sql`. **UAT:** **BGT-004**. **Tier:** DEĞİŞMEZ.
- **Honest ceiling (D17):** bildirim motora **kullanıcı turu rolüyle** katlanır (`commands.py:19`); model onu bir kullanıcı mesajından ayırt edemez. Metin control-plane tarafından yazılır ve sabit bir önekle başlar, ama bu bir sözleşme değil bir konvansiyondur. **Yükseltme yolu adıyla:** `message.deliver`'a bir `role`/`source` alanı — bir protokol sürümü, bu epic'te değil.

---

### T5 — Reaper: tavanlar, orphan'lar, restart ve iptal (mig YOK; SECURITY-CRITICAL; T4'e bağlı)

**BU TASK BRIEF'İN DÖRT SORUSUNU CEVAPLAR VE HER BİRİ AYRI BİR RED'DİR.**

- [ ] **RED önce (1), DUVAR SAATİ:** `deadline_at`'i geçmiş bir görev **öldürülür**, `expired` işaretlenir ve model bunu bir bildirimle öğrenir. Uygulayıcı **reaper'dır**, bir `context` değil (D2, D11). Varsayılan **60m**, `0` = sınırsız ve **açıkça yazılmalıdır** (§0.2).
- [ ] **RED önce (2), İPTAL:** run iptal edildiğinde **her canlı görevi öldürülür.** Bugün öldürülmüyor (D10) ve `CancelRunReconciled` bunu hiçbir yerde yapmıyor. Ölçüm işletim sisteminden gelir.
- [ ] **RED önce (3), RESTART / ADOPT:** control plane restart'ından sonra reaper, `running` satırları **sahiplenir**: OCI → `ContainerInspect`; host → pgid + başlangıç zamanı probe'u. Canlı olan yaşamaya devam eder; ölmüş-ama-reap edilmemiş olan `exited` olur ve **exit code'u bilinmiyorsa NULL kalır, uydurulmaz**; kanıtlanamayan `lost` olur ve **sinyal almaz** (T1 RED-4). **E24 T5'in dersi burada birebir geçerlidir:** bellek içi bir bayrak restart'ta silinir, bir sütun silinmez (`000045:129-132`, `runners.state`).
- [ ] **RED önce (4), EŞZAMANLILIK — BEYAN DEĞİL UYGULAMA (D12):** altıncı görev run başına 5 sınırında **REDDEDİLİR** (sıraya alınmaz), yirmi birinci görev makine sınırında reddedilir. `runners.capacity`'nin beyan-edilip-okunmayan hâli tekrarlanmaz; testler sayıyı DB'den ve process'ten sayar.
- [ ] **RED önce (5), ÇÖP:** `exited`/`killed` bir görevin log dosyası `PALAI_BACKGROUND_LOG_TTL` (varsayılan **24h**) sonra silinir; container ise kill'de hemen kaldırılır. Aksi hâlde `.palai-session` sınırsız büyür — ve snapshot onu atladığı için (D7) **kimse fark etmezdi**, ki bu tam olarak bir disk sızıntısının sessiz hâlidir.
- **Seam:** `execution/reconciler.go`, `packages/coordinator/background.go`, `packages/coordinator/orchestration.go` (`CancelRunReconciled`'a kill adımı), `main.go` (üç env). **UAT:** **BGT-005**. **Tier:** DEĞİŞMEZ.
- **Honest ceiling:** adopt yalnız **aynı makinede** çalışır. Control plane başka bir host'a taşınırsa host postüründeki pgid'ler anlamsızdır ve hepsi `lost` olur; OCI postüründe container'lar eski daemon'da kalır. **Bu D4'ün doğrudan sonucudur ve relay (E27) olmadan kapanmaz.**

---

### T6 — Credential: ömür artık GÖREV'dir, ve log dosyası okurken redakte edilir (mig YOK; SECURITY-CRITICAL; T1+T2'ye bağlı)

**BU PLANIN EN KESKİN SORUSU BUDUR (§0.4) VE CEVAP KAÇAMAZ.**

- [ ] **RED önce (1), YAZILI İDDİA YENİLENİR:** `packages/tool-broker/sandbox_exec.go:54-56`'nin *"the value's whole life is one Execute call"* cümlesi, bir arka plan görevi varlığında **yanlıştır** ve bir test bunu gösterir: spawn'dan sonra `Execute` dönmüştür, süreç environ'unda değer hâlâ vardır. Yorum **düzeltilir**, çürümeye bırakılmaz (ağacın kendi deseni: `host/exec.go:48-56`, `workspace/exec.go:369-374`).
- [ ] **RED önce (2), REDAKSİYON OKUMA YOLUNDADIR (D8):** süreç kendi log dosyasına bir env değeri yazarsa, **modele giden baytlarda o değer YOKTUR.** İki redaktör de koşar: `RedactSecrets` (şekil tabanlı) ve `RedactValues` (değer tabanlı) — ikincisi görevin `env_keys`'ini **okuma anında yeniden çözer**, çünkü satırda değer yoktur ve olmayacaktır.
- [ ] **RED önce (3), SATIRDA DEĞER YOK:** `background_tasks` satırının hiçbir sütununda, hiçbir olayda, hiçbir log satırında bir env DEĞERİ geçmez. Ölçüm **ham substring taraması DEĞİL** — satır decode edilir ve her alan taranır (`compressed-secret-scan-vacuous` ve E20 T4'ün dersi).
- [ ] **RED önce (4), SINIRSIZ + CREDENTIAL REDDEDİLİR:** `env_keys` boş olmayan bir görev `deadline_at` NULL ile **yaratılamaz.** Bu §0.4'ün (b) seçeneğinin bedelidir ve bir CHECK ile değil bir kod kapısıyla uygulanır (çünkü sınırsızlığı seçen `0` bir kullanıcı kararıdır ve credential'sız görevler için meşrudur).
- [ ] **RED önce (5), PARK EDEN RUN DEĞER TUTMAZ:** `resolveEnvValues` bugün Execute'tan hemen önce koşuyor (`tool_dispatch.go:234-245`) ve arka plan yolu **o sırayı değiştirmez** — spawn da bir Execute'tur. Onay için park eden bir run hâlâ hiçbir değer tutmaz.
- **Seam:** `execution/background.go`, `execution/environment.go`, `packages/tool-broker/redact.go` (kullanım, imza değil), `packages/tool-broker/sandbox_exec.go` (yorum düzeltmesi). **UAT:** **BGT-002**'ye ek şart. **Tier:** DEĞİŞMEZ.
- **Honest ceiling, açıkça:** aynı uid altında `ps -E` / `/proc/<pid>/environ` değeri okuyabilir. **Bu senkron bir komut için de doğrudur** (aynı executor, aynı uid) ve `MAC-P6` onu yönetiyor. Arka plan bu maruziyeti **genişletmez, uzatır**, ve uzunluğun tavanı §0.2'dir. Bir daraltma yolu adıyla var ve **bu epic'te değil**: değeri env yerine kısa ömürlü bir dosya handle'ı olarak vermek (broker'ın push credential'ı için yaptığı gibi, `execution/approval.go:229-237`).

---

### T7 — EXIT gate: `background-execution-0.1.0` + BGT journey (mig YOK)

- [ ] **Case id prefix'i `BGT-`'dir ve bu bir gate kararıdır.** `promote-gate-family-dispatch` kuralı: mevcut bir prefix'i kullanmak ya gönderilmiş bir bundle'ı yeniden ürettirir ya `PromoteGateFor`'u daha zayıf bir gate'e düşürür. **`BGT-` ağaçtaki otuz üç prefix'in hiçbiriyle çakışmıyor** (sayıldı 2026-07-30: A2A AGT API APV AUT CAS CON DEL DET DR ENG EXT FLT HIL KNO LP MCI MOD OPS PER QUA REC REG REP SAN SEC SES SLK SUB TLM TOL UI WRK).
- [ ] **Yeni bundle iki yere birden girer (D19):** `committedBundleSurfaces` (`tests/uat/evidence.go:2732+`, **`SurfaceRecomputed`, legacy DEĞİL**) ve `caseChecksumParts`'ın kendi sweep tablosu. Bugünkü sayı **23**'tür, 16 değil.
- [ ] **Yeni component suite'i `scripts/test/component`'in `case`'ine girer (D20)** ve `-run` allow-list'i kullanılıyorsa her test ADI listede geçer. **Bir kez daha:** *"a guard this `-run` does not name is a guard this tier never runs"*.
- [ ] **Journey, iddiaların GÜÇLÜ hâlini asserte eder** (`exit-gate-journey-vs-bundle`): "arka planda koştu" değil, **"model spawn'dan sonra başka bir tool çağırdı ve o çağrı tamamlandı, süreç hâlâ koşuyordu"**.
- [ ] **`CAS-P2` yeniden yazılır** — kapanmaz, **daralır**: canlı akış hâlâ yok; sessizlik artık modelin okuyabileceği bir dosyayla kırılabilir; kalan yarı (`task_update` chunk'ları + `ShellRunner`'a bir progress kanalı) `CAS-P2` olarak durur ve sahibi E27'dir.
- [ ] **`docs/operations/background-execution.md`** — ve **ilk paragrafı D14'ü yazar**: `PALAI_DISPATCH_WORKERS=0` bir stack'te hiçbir bildirim inmez.
- [ ] **`docs/operations/known-gaps-1.0.md`** yeni satırlar alır: `BGT-P1` (canlı akış yok), `BGT-P2` (PID yeniden kullanımı, host postürü), `BGT-P3` (bildirim kullanıcı rolüyle katlanıyor), `BGT-P4` (adopt yalnız aynı makinede — D4/E27).
- **UAT:** **BGT-001..005**. Tek yeni proof tipi: **`BackgroundProof`**, `Peer` alanı yapısal olarak `"fake"` değil — burada peer YOK, ölçüm işletim sistemindendir; alan `Machine` olur ve `"local"` değerini alır.
- **Tier:** **HİÇBİR TIER İLERLEMEZ.** Gerekçe bir kuraldır: bir tool'u asenkron yapmak, o tool'un koştuğu düzlemin kanıtı değildir.

### 4.2 Boyut hükmü, dürüstçe

**BU BİR EPIC'TİR VE YEDİ TASK'TIR.** Bölünmesi ÖNERİLMİYOR ve sebebi ölçülmüş: ağır iş T1'dedir (iki implementasyon + bir migration) ve T3/T4 ağaçta zaten yazılmış bir koreografinin üçüncü kullanıcısıdır — E23 T1 ve E24 T4 park+uyandırmayı iki kez gönderdi, üçüncüsü bir gövde kopyalamaz.

**Epic'i aşmaya EN YAKIN olan T5'tir** ve bölünme noktası adıyla yazılıdır: **T5a (tavanlar + iptal + eşzamanlılık) E26'da, T5b (restart-adopt) E27'de.** T5b'siz E26 dürüst bir ara durumdur ve bir yalan değildir, yeter ki şöyle yazılsın: *"bir control plane restart'ı koşan arka plan görevlerini `lost` bırakır; süreçler yaşamaya devam eder ve kimse onları reap etmez."* **Bu cümle yazılmadan T5b atlanamaz.**

---

## §5 — Kapsam dışı, adıyla

| Ne | Neden | Nereye |
|---|---|---|
| **OTOMATİK arka plana alma** (§3.5 P4) | Palai'de senkron timeout container'ı/process grubunu **zaten SIGKILL'liyor** (`docker.go:183-189`, `host/exec.go:198-201`); "arka plana taşımak" öldürülmüş bir süreci diriltmek olurdu. Modelin `background:true` demesi gerekir | E27, ve önce senkron timeout'un öldürmeyi bırakması gerekir |
| **Canlı ilerleme AKIŞI** (`CAS-P2`'nin kalan yarısı) | `ShellRunner`'a bir progress kanalı + E20'nin `task_update` chunk'ları — bu epic'in dayandığı seam'e bir değişiklik. E26 sessizliği bir DOSYAYLA kırar, bir akışla değil | E27 |
| **İcra relay'i (E24 T7)** | Hiç gönderilmedi (D4). E26 bir makine sınırı geçmiyor ve geçmiş gibi yapmıyor: arka plan görevi control plane'in kendi makinesinde koşar | E27 (T7a/T7b) |
| **`workers` (capability workers) düzlemi** | E24 D14'ün dört sebebinden **üçü** hâlâ geçerli (D13): düzlem uykuda, `ErrUntypedOperation` genel bir shell'i imkânsız kılıyor, enrollment bellekte. Paket **dokunulmaz** | — (adlandırılmış ret) |
| **Konsolda arka plan görevleri ekranı** | `background_tasks` bir okuma rotası ve bir ekran hak ediyor, ama bir okuma rotası kendi tenancy corpus satırını ve kendi kararını ister (E25'in `CON-P7` gerekçesi) | E28 (konsol) |
| **Slack'te arka plan bildirimi** | Bildirim `message.deliver` ile modele iner; Slack thread'ine ayrıca düşmesi `slack_reply_deliveries`'in exactly-once kısıtını ilgilendirir ve ayrı bir karardır | E27 |
| **`background:true` için ayrı bir onay sınıfı** | E23'ün kapısı yeterlidir ve §2 onu yukarıda tutar. "Arka plan daha tehlikeli, ayrı onay ister" bir his; ölçülmüş bir gerekçe değil | — |
| **Env değerini dosya handle'ıyla vermek** | T6'nın honest ceiling'inde adıyla yazılı daraltma yolu; broker'ın push credential deseni (`approval.go:229-237`) mevcut | E27 |
| **`ResponseTable`'ın diğer iki durumunu bağlamak** (`waiting_for_approval`, `waiting_for_input`) | D3 üçünün de yazılmadığını ölçtü. E26 **yalnız** `waiting_for_tool`'u bağlar, çünkü onu üreten yol bu epic'te. Diğer ikisi E23/E08'in yollarıdır ve onların sahipleri düzeltir | E27 |
| **Birden çok makinede arka plan** | D4'ün doğrudan sonucu; relay olmadan anlamsız | E27 |

---

## §6 — Operatör bacakları (deterministik tier'ın veremediği, ve verilmediği açıkça yazılan)

1. **GERÇEK BİR MAC'TE GERÇEK BİR `xcodebuild`, ARKA PLANDA.** `PALAI_SHELL_NATIVE=unsandboxed-host` ile, gerçek bir Xcode projesi, süre **ölçülür ve bir sayı olarak yazılır** — tahmin olarak değil. Bu, bu epic'in var olma sebebinin tek gerçek kanıtıdır ve **D18 yüzünden gönderilen compose stack'i onu veremez** (orada shell tool'u hiç yok).
2. **CONTROL PLANE RESTART'I, GERÇEK BİR BUILD KOŞARKEN.** T5b'nin adopt yolu, gerçek bir `launchctl`/systemd yeniden başlatmasında. Component testi bir process'i öldürüp yeniden başlatıyor; bir operatör bacağı **gerçek servis yöneticisini** ister.
3. **ORPHAN AVI.** Bir gecelik koşumdan sonra: kaç `io.palai.bg` etiketli container kaldı, kaç `lost` satır var, `.palai-session/bg` ne kadar yer tutuyor. **Sayılar yazılır.**
4. **PID YENİDEN KULLANIMI GERÇEKTEN OLUYOR MU.** `BGT-P2`'nin tek gerçek ölçümü: bir Mac'te pgid'ler ne hızla dönüyor, ve başlangıç zamanı karşılaştırması pratikte tutuyor mu.
5. **BİR CREDENTIAL'IN GERÇEK MARUZİYETİ.** Gerçek bir env değeri taşıyan bir arka plan görevi koşarken, aynı uid'den `ps -E` ile değerin görünüp görünmediği — T6'nın honest ceiling'i bir iddia değil bir gözlem olarak.
6. **`PALAI_DISPATCH_WORKERS=0` BİR STACK'TE HİÇBİR BİLDİRİM İNMEDİĞİNİN GÖZLENMESİ** (D14) — dokümanın ilk paragrafının doğruluğu.
7. **E17/E18/E19/E20/E21/E22/E23/E24/E25'in devralınan tüm açık legleri** — E26 hiçbirine dokunmaz.

**Tier sonucu, bir kez söylenir:** `slack` **preview**, `knowledge-vector` **disabled**, `apple-build` **disabled**, `console` **preview** kapanır. **Hiçbir tier ilerlemez**, ve sebebi bir kuraldır: bir tool'u asenkron yapmak, o tool'un koştuğu düzlemin kanıtı değildir.

---

## §7 — Master plan §8 için önerilen özet blok (owner paste eder)

**UAT ownership:** E26 **BEŞ YENİ ID** açar ve prefix'i **`BGT-`**'dir. **BGT-001** (bir süreç artık RUN'a aittir, onu başlatan çağrıya değil — ve bir pgid'yi sahiplendiğimizi kanıtlayamıyorsak ona sinyal göndermeyiz), **BGT-002** (tool anında bir HANDLE döner, çıktı uçuş sırasında okunur, ve yeni bir okuma tool'u YOKTUR), **BGT-003** (canlı görevi olan bir run bitmez — ve bir yanıt ilk kez `waiting_for_tool` der), **BGT-004** (çıkış modeli yeniden çağırır, tam olarak bir kez, ve uyandırma E23 T1'in koreografisinin ÜÇÜNCÜ kullanıcısıdır), **BGT-005** (tavan, orphan, restart ve iptal — dördü de reaper'ın, hiçbiri bir `context`'in). Tek yeni proof tipi: **`BackgroundProof`**.

**Exit gate:** `background-execution-0.1.0` bundle'ı, bir tool çağrısının bir SONUÇ yerine bir SÜREÇ döndürebildiğini ve modelin bloke olmadığını kanıtlar. **Kaynak Anthropic'in kendi yayımlanmış harness davranışıdır ve bir taklit değil bir doğrulamadır:** birebir *"Claude can set `run_in_background: true` to start the command as a background task and continue working while it runs"* ve — bu satır tasarımın yarısını belirledi — `TaskOutput` tool'unun *"**Deprecated in favor of `Read` on the task's output file path**"* olması (Claude Code tools reference, çekildi 2026-07-30). **Yani vendor bile ayrı bir okuma tool'unu bırakıp sıradan dosya okumasına döndü, ve Palai dördüncü bir çıktı mekanizması eklemez:** log, allocation'ın snapshot'tan zaten dışlanmış `.palai-session` alt ağacına yazılır ve `palai.workspace.file` ile okunur. **MIGRATION VARDIR: 000047**, tek tablo — `background_tasks` (tenant tablosu, kendi `palai_apply_tenant_policy` + `GRANT`'i, `deadline_at` ve `notified_at` dahil, ve `env_keys` sütununda **anahtar adları, asla değer**). **Doğruluk canlı koşumdan değil YAYIMLANMIŞ VENDOR DOKÜMANINDAN VE AĞACIN KENDİ ÖLÇÜMÜNDEN gelir** — §3.5 tablosu **11 satır** adlandırır (üçü UNCONFIRMED/UNVERIFIED ve hiçbiri koda girmez), §3.6 tablosu ise ağacın kendi hakkındaki **yirmi iki yanlış inancını.** Üçü diğerlerinden pahalıdır. **BİR: bugün bir arka plan süreci YAPISAL OLARAK İMKÂNSIZ, ve iki postür bunu iki farklı sebeple imkânsız kılıyor** — OCI `defer ContainerRemove(Force:true)` ile container'ı koşulsuz siliyor (`oci/docker.go:145-152`), host `exec.CommandContext` + süreç GRUBU SIGKILL'i ile bütün ağacı öldürüyor (`host/exec.go:146-154`); sahiplik `ShellRunner.Run` ÇAĞRISINA bağlı, run'a değil, ve park eden bir attempt o çağrıdan çıkıyor. **İKİ: `PALAI_SANDBOX_WALL_TIME` hiçbir gönderilmiş dosyada AYARLI DEĞİL** (grep, sıfır atama) ve set edilmemiş hâli iki postürde ZIT anlama geliyor: host **tamamen sınırsız** (`host/exec.go:125`), OCI ise **her shell çağrısını reddediyor** (`oci/driver.go:105-107`) — yani "eşleştirilecek" senkron sınır ya yok ya tool'u kırıyor, ve arka plan tavanı sıfırdan bir karar ister (varsayılan **60m**, reaper uygular). **ÜÇ: yayımlanmış Response şeması üç yıldır `waiting_for_tool`/`waiting_for_approval`/`waiting_for_input` ilan ediyor ve `ResponseTable`'ın HİÇBİR production caller'ı yok** (`RunTable` uygulanıyor, `store.go:466`; `ResponseTable` yalnız kendi testlerinde) — yani E23'ün insan-kapılı akışında bile yanıt `in_progress` okuyor, ve **E26 o tablonun ilk production caller'ı olur.** Ayrıca ölçüldü: **E24 T7 — icra relay'i — hiç gönderilmedi** (üç dosya yok, `FLT-006` yok, ve E24 T8'in kendi commit başlığı *"a Mac pool does not run xcodebuild on the Mac"* diyor), yani arka plan icrasının koşacağı tam olarak BİR makine var; **`Reconciler` `PALAI_DISPATCH_WORKERS=0` olan compose stack'inde HİÇ koşmuyor** (`main.go:508-510`, `compose.yaml:82`), yani dead-letter, onay expiry ve kapasite-park süpürmeleri orada zaten çalışmıyor; ve **E25 T3'ün *"the value's whole life is one Execute call"* iddiası bir arka plan görevi için yapısal olarak yanlış** — değer çekirdeğin environ kopyasında sürecin ömrü boyunca durur, yani E25 T3'ün gerekmediğini söylediği scope+fence+expiry üçlüsü burada gerekiyor ve `deadline_at` onun expiry'sidir. **Hiçbir tier ilerlemez**, gerekçe bir kural: bir tool'u asenkron yapmak, o tool'un koştuğu düzlemin kanıtı değildir. **VE BİR AD UYARISI:** "E26" adı bu plan yazılırken **üç şeye birden** verilmişti (`phase-24` §7 ölçekleyici+relay'i, `phase-25:294` konsolun kalan 15 yüzeyini, ve bu plan arka plan icrasını) — §0.1 owner'dan bir karar ister ve diğer ikisi **E27/E28** olur.
