# Palai Production Self-Host Plan (E14 — single-node + split-VM)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (önerilen) veya superpowers:executing-plans ile task-by-task uygula. Adımlar `- [ ]` checkbox'lıdır. systemd unit sentaksı / Prometheus rule-file / Grafana provisioning idiom'ları brief'lerinde Context7/spec grounding alır (repo politikası, ledger 2026-07-17).

**Goal:** E13'ün "managed-agent-over-cloud infra-complete" gate'inden geçen stack'i **kurulabilir, işletilebilir ve geri-döndürülebilir** bir production self-host dağıtımına çevirmek: TLS edge'li production compose profili, non-development master key + kapalı registration posture'ı, `palai org|project|apikey|secret` admin CLI'ı (E13 API'lerinin İNCE istemcisi, §47.6), kurulum-seviyesi `backup/restore/restore verify`, `config validate` + doctor v2 + `support-bundle`, signed runner host package + systemd unit'leri + scheduled-backup timer'ı ve §52.9/§52.10 Grafana/Prometheus observability bundle'ı.

**Kapsam sınırı — DÜRÜST TAVAN (bu planın en önemli tasarım kararı):** Bu plan macOS + Docker Desktop oturumunda kod-subagent'larıyla İCRA EDİLİR: gerçek cloud VM ve gerçek systemd host'u YOKTUR. Her task bu yüzden İKİYE bölünür: **(a) burada tam inşa+test edilebilen KOD/CONFIG teslimatı** (gerçek testli CLI komutları; `docker compose config` + local production-profile bring-up ile doğrulanan production.yml; yazılıp statik doğrulanan systemd unit'leri; şema-doğrulanan observability bundle'ı; LOCAL production-compose stack'ine karşı koşan tests/uat/self-host harness'ı) ve **(b) gerçekten cloud/systemd altyapısı isteyen OPERASYONEL kanıt**. (b) için ulaşılabilir kanıt HER ZAMAN local-production-compose eşdeğeri + adlandırılmış tavandır: *"dedicated-cloud-VM kurulumu ve ayrı-host restore, local-production-compose seam'inde kanıtlanır; gerçek cloud-VM bacağı operator-provided altyapı ister"* (§6). Bir kod agent'ının koşturamayacağı cloud deployment PLANLANMAZ, kanıtı İDDİA EDİLMEZ — E13'ün exit gate'ini geçiren honest-ceiling disiplininin aynısı.

---

## 1. Yapı kararı — fork noktası, numaralandırma, dosyalar

**Fork noktası:** E13 kapandı (`main` dd990aa, T1-T11 + exit gate landed). E14 bu tepe ÜZERİNDEN forklanır; execution gate: `main` >= dd990aa.

**Migration:** E14 ops/deploy fazıdır — **HİÇBİR task şema migration'ı istemez** (her task'ta açıkça "Migration: yok" denir). Zincir 000032'de; öngörülemeyen bir ihtiyaç çıkarsa ilk boş numara **000033**'tür ve önce owner onayı alınır.

**Kurulum-vs-run restore AYRIMI (isim çakışması yasak):** `apps/control-plane/internal/execution/{snapshot,restore}.go` RUN-seviyesi checkpoint/workspace restore'udur (§26.3 recovery ladder, E10) — çalışan bir stack İÇİNDE tek run'ı diriltir. Bu epic'in eklediği `palai backup / restore / restore verify` **KURULUM-seviyesidir**: Postgres dump + object-store içeriği + manifest, AYRI bir temiz kuruluma taşınır. İki katman ayrı adlandırılır, run-level koda dokunulmaz.

**Files:** `deploy/compose/production.yml`, `deploy/systemd/`, `deploy/observability/`, `docs/operations/`, `cmd/cli/`, `tests/uat/self-host/` (master plan §E14 bire bir). Ayrı docs task'ı YOK — her task kendi runbook sayfasını `docs/operations/` altına teslim eder.

---

## 2. Design invariant (task değil, her task'ın kabul şartı)

- **Aynı binary'ler, farklı posture:** production profili YENİ server yüzeyi açmaz; mevcut control-plane/runner imajları + E13 API'leri, sıkılaştırılmış konfigürasyonla dağıtılır. Admin CLI hiçbir yeni endpoint eklemez — router.go'daki mevcut mount'ların ince authenticated HTTP istemcisidir.
- **Secret'lar asla environment/argv/log'da:** mevcut file-secret kalıbı (compose top-level `secrets:`, entrypoint bridge) production profilinde AYNEN korunur; CLI secret değerlerini stdin'den alır; support-bundle/evidence çıktıları mevcut secret-scan gate'inin kapsamındadır.
- **Host-agnostik kanıt:** T7 journey harness'ı base-URL + compose-file parametriktir — aynı harness bugün local production-compose stack'ine, yarın operator'ün cloud VM'ine POINT edilir. Kanıt kodu cloud'a özel HİÇBİR varsayım içermez.
- **Registration posture:** stack'te public self-registration endpoint'i ZATEN yok (bootstrap key + `provision` capability); production profili bunu İNŞA etmez, ASSERT eder (config validate + doctor check'i).

---

## 3. Doğrulanmış seam envanteri (2026-07-23, dd990aa ağacına karşı)

| Seam | Durum |
|---|---|
| `cmd/cli/` | `main.go` elle `os.Args` dispatch (cobra yok); `internal/stack/{config,lifecycle,doctor,certs,provider,response}.go`. `doctor` MEVCUT: 11 check (api/migration/object-store/runner/image-digests/provider/clock/quarantine/retention/supervisor/runner-tls-reject). backup/restore/support-bundle/config-validate/admin subcommand'ları YOK |
| `PALAI_COMPOSE_FILE` | `config.go:57 composeFile()` override'ı ZATEN VAR — production.yml için yeni mekanizma gerekmez |
| `deploy/compose/` | `compose.yaml` (4 servis, digest-pinned, file-secrets, runner'a docker.sock — engine'e ASLA) + `compose.env.example` + 2 entrypoint + 2 Dockerfile. `production.yml` YOK |
| `deploy/systemd/`, `deploy/observability/`, `docs/operations/` | YOK — sıfırdan yazılır |
| E13 admin API'leri | `router.go:152-187`: org/project(+config_policy PATCH)/api-key mount'ları + secret-refs (create/list/get/rotate), `provision` capability gated — admin CLI bunların istemcisi, YENİ server yüzeyi yok |
| Metrics | Repo'da SIFIR `prometheus`/`/metrics` hit'i; yalnız unauthenticated `/healthz` (`router.go:258`) — §52.9/§52.10 bundle'ından ÖNCE metrics-exposure KODU gerekir (T6'nın ilk yarısı) |
| Run-level restore | `execution/{snapshot,restore}.go` — run checkpoint'u (§26.3); kurulum-seviyesi backup'tan FARKLI katman (§1'deki ayrım) |
| Master key | `PALAI_SECRET_MASTER_KEY_FILE` (`cmd/palai-control-plane/main.go:79-92`); yokken secret-ref route'ları mount edilmez — production profili bunu ZORUNLU kılar |
| CA/cert üretimi | `certs.go` local CA + server cert yazar — TLS edge'in LOCAL kanıt cert'i için reuse edilir (operator gerçek cert'le değiştirir) |
| Runner | Outbound-only enroll (`PALAI_CONTROLLER_URL`), compose'da HİÇ listen portu yok — split-VM local kanıtının temeli |
| UAT altyapısı | `tests/uat/` harness + `tests/uat/cases/` authored kalıbı + `make uat-*` + evidence verifier (E13 `managed-cloud-0.1.0` emsali); OPS-*/DR-* case'leri henüz materialize DEĞİL |

---

## 4. Task breakdown

**DAG:** Wave 1 (paralel, cap 3): **T1, T2, T6** — üç ayrık seam (deploy/compose; cmd/cli admin istemcisi; control-plane metrics). Wave 2 (paralel, cap 2): **T3** (T1+T2'ye bağlı), **T4** (T1'e bağlı) — ikisi de `cmd/cli/main.go` dispatch switch'ine case ekler; entegrasyon merge sırası SABİT: önce T3, sonra T4 (E13'ün migration-numarası-pinleme emsali). Wave 3: **T5** (T1+T4'e bağlı — backup timer `palai backup`'ı çağırır). Wave 4: **T7** (hepsine bağlı, exit-gate evi).

### T1 — Production compose profili + TLS edge (`deploy/compose/production.yml`)

- [ ] `production.yml`: digest-pinned TLS-terminating reverse-proxy edge servisi (cert/key dosya mount'u — YENİ cert altyapısı yazılmaz, local kanıt `certs.go` CA'sının minted çiftini kullanır, operator gerçek cert'le değiştirir); tüm servislere `restart: always` + named volume'lar; control-plane API'si yalnız edge arkasında (host'a publish edilmez); `PALAI_DISPATCH_WORKERS` default 1; `PALAI_SECRET_MASTER_KEY_FILE` ZORUNLU (entrypoint yoksa fail-fast); bootstrap key dosyadan, dev-default reddi.
- [ ] `production.env.example` (compose.env.example kalıbı) + `docs/operations/install.md` runbook'u. `palai prod` komutu YOK — profil operator-driven `docker compose -f production.yml`'dir; harness ve runbook bunu scriptler (`PALAI_COMPOSE_FILE` override'ı zaten CLI'ı bu profile bağlayabiliyor).
- **Seam:** `deploy/compose/` (compose.yaml'a dokunulmaz — local profil aynen kalır). **UAT:** OPS-002 (local-production-compose seam'inde). **Migration:** yok.
- **Kanıt (burada koşar):** `docker compose -f production.yml config` yeşil; Docker Desktop'ta tam production-profile bring-up; CA-pinned `curl https://edge/healthz` + edge üzerinden gerçek bir run round-trip.
- **Honest ceiling:** gerçek domain + ACME/gerçek cert + dedicated cloud VM kurulumu = operator leg (§6); burada kanıtlanan TLS edge SELF-MINTED cert'ledir.

### T2 — Admin CLI: `palai org|project|apikey|secret` (§47.6; E17 console'a kadar tek insan arayüzü)

- [ ] Yeni `cmd/cli/internal/admin/`: `router.go:152-187` mount'larının ince authenticated HTTP istemcisi — org create/list/get, project create/list/get + `config_policy` PATCH, apikey create/list/get/revoke, secret create/list/get/rotate. Base URL + API key: flag → env → `.palai` fallback; RFC9457 problem-details insan-okur render; `--json` çıktısı; secret DEĞERİ yalnız stdin'den (asla argv/env).
- [ ] `main.go` dispatch'e dört case; `docs/operations/admin-cli.md`.
- **Seam:** `cmd/cli/` + mevcut provisioning/secret-ref API'leri — YENİ server yüzeyi yok. **UAT:** §47.6 API+CLI şartı; T7 journey'nin provisioning adımları bu CLI'dan geçer. **Migration:** yok.
- **Kanıt (burada koşar):** component testler `httptest` + GERÇEK router (in-proc, Docker'sız) — tam CRUD + revoke + rotate + yanlış-key 401/403 + RFC9457 render; live smoke: local stack'te CLI ile org→project→key→secret→run.
- **Honest ceiling:** YOK — bu task burada TAM kanıtlanır. (Roles/OIDC yüzeyi E13-H/E17'dir, CLI'a eklenmez.)

### T3 — Operability komutları: `config validate` + doctor v2 + `support-bundle`

- [ ] `palai config validate`: stack'siz statik kontrol — compose env kontratı, production posture'ı (master-key file mevcut + dev-default değil, bootstrap key non-default, cert çifti okunur, dispatch>=1, registration posture), eksik/fazla env teşhisi.
- [ ] Doctor v2: mevcut 11 check'in ÜZERİNE **disk** (data-dir/volume boş alan eşiği), **queue** (queued/running derinliği + en yaşlı admitted yaşı), **callback** (webhook delivery backlog/failure oranı) eklenir — provider/object-store/clock check'leri ZATEN VAR (`doctor.go`), yeniden yazılmaz.
- [ ] `palai support-bundle`: doctor JSON + `compose ps`/redacted config + son N log satırı + secret'sız stack config → tek tar.gz; redaction'ı mevcut secret-scan gate'i kapsar (bundle'da sıfır secret asserti test edilir). `docs/operations/operability.md`.
- **Seam:** `cmd/cli/internal/stack/doctor.go` + yeni dosyalar; `main.go` dispatch (merge sırası: T3 önce). **UAT:** §63.6 journey'nin doctor/support-bundle adımları; disk/queue/callback teşhisleri master plan satır 500'ün doctor yarısı (alert yarısı T6'da). **Migration:** yok.
- **Kanıt (burada koşar):** unit+component testler; local production stack'te doctor v2 yeşil + bundle üretimi + redaction assert.
- **Honest ceiling:** YOK — bu task burada TAM kanıtlanır.

### T4 — Kurulum-seviyesi `palai backup / restore / restore verify`

- [ ] `palai backup`: çalışan production stack'ten tutarlı Postgres dump (custom format) + object-store içerik kopyası + **manifest** (migration version, tenant/org id'leri, per-object sha256, created_at) → tek arşiv. RUN-seviyesi checkpoint restore'uyla (§1 ayrımı) kod/isim paylaşımı YOK.
- [ ] `palai restore`: yalnız BOŞ hedef stack'e (dolu hedefi reddeder — veri ezme yok); `palai restore verify`: manifest'e karşı checksum + tenant-id + migration-version eşleşmesi + örnek run-retrieval sorgusu.
- [ ] `docs/operations/backup-restore.md` (retention/prune politika örneğiyle).
- **Seam:** `cmd/cli/` yeni backup dosyaları (merge sırası: T4 sonra) + production stack'in pg/object-store servisleri. **UAT:** DR-002, DR-004..006 (local-production-compose seam'inde). **Migration:** yok (manifest dosyadır, tablo değil).
- **Kanıt (burada koşar):** local production stack-A'da veri üret → backup → **AYRI temiz local production stack-B** (farklı PALAI_HOME/port/volume seti) → restore → verify yeşil → stack-B'den run GET byte-doğru. Bu, "backup'ı ayrı clean install'a restore et" maddesinin local-production-compose eşdeğeridir.
- **Honest ceiling:** ayrı FİZİKSEL host/cloud-VM'e restore = operator leg (§6); local kanıt aynı Docker Desktop'ta iki izole stack'tir.

### T5 — systemd unit'leri + signed runner host package + scheduled-backup timer

- [ ] `deploy/systemd/`: `palai-stack.service` (production compose wrapper, `Restart=always`), `palai-runner.service` (host-package runner, outbound-only, hiçbir listen portu), `palai-backup.service` + `.timer` (`ExecStart=palai backup`, `OnCalendar` örneği) + retention/prune scripti.
- [ ] Runner host package: `scripts/package/runner` — deterministik tarball (runner binary + unit + env template) + sha256sums manifesti + detached imza + `verify` scripti (imza aracı seçimi task brief'inde; ladder: mevcut toolchain'de olan kazanır).
- [ ] Outbound-only + no-runtime-socket-to-workload asserti: runner'ın listen portu olmadığı ve engine'in socket almadığı compose-config seviyesinde ASSERT edilir (mevcut invariant, yeniden inşa edilmez). `docs/operations/runner-host.md`.
- **Seam:** `deploy/systemd/` (yeni) + T1 production profili + T4 `palai backup`. **UAT:** OPS-002'nin runner-package yarısı; §63.6 split-VM adımı local seam'de. **Migration:** yok.
- **Kanıt (burada koşar):** TÜM unit dosyalarına Linux container'da `systemd-analyze verify` (Docker Desktop'ta koşar) + shellcheck; package build+verify script testi; **split-VM local kanıtı:** host-package'ten çıkarılan runner, production stack ağının DIŞINDAN published runner portuna outbound-only ENROLL olur ve gerçek bir run koşar.
- **Honest ceiling:** gerçek `systemctl enable --now`, boot-persistence ve gerçek iki-VM ağı = operator leg (§6); burada unit'ler STATİK doğrulanır, split-VM ağı Docker-network seam'inde temsil edilir.

### T6 — Metrics exposure + §52.9/§52.10 observability bundle (`deploy/observability/`)

- [ ] KOD (önce bu — bugün SIFIR metrics yüzeyi var): control-plane'e Prometheus text-exposition `/metrics` (iç ağda; edge PUBLISH ETMEZ) — seri seti: run state count'ları, queue depth+age, dispatch latency, provider call/error, webhook delivery backlog/failure, object-store op error, clock skew, disk. Ladder: bir avuç counter/gauge için stdlib text-writer; yeni dependency ancak histogram ihtiyacı kanıtlanırsa.
- [ ] `deploy/observability/`: `prometheus.yml` scrape config + §52.10 alert rule'ları (disk/queue/runner-down/provider-error/object-store/clock/callback — master plan satır 500'ün alert yarısı) + §52.9 Grafana dashboard JSON'ları + provisioning; production.yml'e OPSİYONEL observability overlay'i (digest-pinned prometheus+grafana). `docs/operations/observability.md`.
- **Seam:** `apps/control-plane/api/` (unauth top mux `/healthz` emsali, iç-ağ bind) + `deploy/observability/` (yeni). **UAT:** §63.6 journey'nin metrics/alert probe adımı. **Migration:** yok.
- **Kanıt (burada koşar):** `promtool check rules` yeşil; dashboard JSON lint (panel/datasource şema kontrolü); LOCAL CANLI kanıt: overlay'li production stack'te Prometheus target UP → runner durdurulur → runner-down alert'i FIRING'e geçer.
- **Honest ceiling:** gerçek üretim yükü altında alert davranışı ve uzun-pencere kurallar (ör. disk-trend) yalnız sentaks + sentetik tetikle kanıtlanır; gerçek-yük tuning'i operator telemetrisi ister.

### T7 — Self-host journey + evidence gate (`tests/uat/self-host/`)

- [ ] Journey harness'ı (HOST-AGNOSTİK — base URL + compose-file + PALAI_HOME parametrik, cloud'a özel sıfır varsayım): temiz install (taze PALAI_HOME) → production bring-up → TLS edge → `config validate` + doctor v2 → admin CLI ile org/project/apikey/secret provisioning → `examples/nextjs-sdk` YALNIZ base-URL/key değişikliğiyle gerçek run → metrics/alert probe → `palai backup` → AYRI temiz stack'e `restore` + `restore verify` (checksums/tenant-ids/run-retrieval) → `support-bundle` → hepsi restart'sız provisioning ile.
- [ ] `make uat-self-host` + **`self-host-0.1.0` evidence bundle** (redacted manifest, LP/E13 kalıbı); **OPS-002, DR-002, DR-004..006** + §63.6 install/backup-subset case'leri `tests/uat/cases/` altında materialize edilir (§64 authored kalıp, MCI-00x emsali). Her case metni kanıtın LOCAL-production-compose seam'inde koştuğunu ve cloud-VM bacağının operator legi olduğunu AÇIKÇA yazar.
- **Exit-gate proof'un evi budur.** **Migration:** yok.
- **Honest ceiling:** aynı harness operator'ün cloud VM'ine point edilerek AYNEN koşabilir (tasarım şartı) — ama bu planın gate kanıtı LOCAL seam'dir; cloud koşusu §6 operator legidir.

---

## 5. OUT OF SCOPE (bilinçli dışarıda, adres adresine)

| Kalem | Neden dışarıda | Nerede yaşıyor |
|---|---|---|
| Gerçek cloud-VM clean install + ayrı-host restore İCRASI | Kod agent'ı cloud VM koşturamaz; kanıt local-production-compose seam'inde, adımlar runbook'ta | §6 operator legs |
| Upgrade discipline (expand/migrate/contract, N→N+1, rollback window) | SH-2 işi | E15 |
| Helm/Kubernetes, NetworkPolicy, PDB | SH-2 işi | E15 |
| Air-gap bundle, private registry, release-signing pipeline'ı | SH-2 işi | E15 (`scripts/release/`) |
| DR drill'leri (primary loss, KMS recovery, ölçülmüş RPO/RTO) | DR hardening | E15 (DR-001 + rapor) |
| DR-003 regional failover | Managed SLA ürünü | SaaS planı |
| Web-console UI | Admin CLI tek insan arayüzü kalır | E17c |
| OPS-001, OPS-003..008 | Packaging'in E07 yarısı + upgrade/k8s/air-gap yarısı | E07 (kapalı) / E15 |
| Multi-runner fleet / autoscale | Tek-node + tek split-VM runner yeter (SH-0) | E15/SaaS |
| KMS-backed master key ceremony | Production profili file-based non-dev master key ZORUNLU kılar; KMS backend hardening'dir | E13-H |

## 6. Operator legs — cloud-VM bacağı (deferred-but-scripted; kaybolmaz)

Her biri için KOD/harness bu fazda hazırdır, İCRA operator-provided altyapı ister. `docs/operations/` runbook'ları adım adım scriptler; T7 harness'ı parametrik olduğundan operator aynı kanıtı kendi VM'inde yeniden koşabilir:

1. **Dedicated cloud VM'ye clean install** (T1 runbook'u + T7 harness'ı VM'in base-URL'ine point edilir) — gerçek domain/cert dahil.
2. **Ayrı fiziksel host'a restore** (T4 runbook'u; stack-B = ikinci VM) — DR-002/DR-004..006'nın cloud icrası.
3. **systemd enable + boot-persistence + gerçek iki-VM split-runner** (T5 unit'leri + package'ı gerçek Linux host'ta `systemctl enable --now`).

Bu üç leg SH-0 gate'ini BLOKLAMAZ (gate kanıtı local seam'dedir, §7); SH-2 (E15 RC) iddiasından önce en az 1. ve 2. legin bir kez gerçek altyapıda koşmuş olması gerekir.

## 7. Master plan §8 için önerilen özet blok (owner paste eder)

**UAT ownership:** OPS-002; DR-002, DR-004..006; §63.6 self-host journey'nin install/backup subset'i — tamamı `tests/uat/cases/` altında materialize, kanıt seam'i case metninde adlandırılır.

**Exit gate — SH-0 single-node alpha (local-production-compose seam'inde):** Temiz bir production-profile kurulum — TLS edge, non-development master key, kapalı registration posture'ı, `restart: always` kalıcı servisler — `palai` admin CLI'ıyla provision edilir; `examples/nextjs-sdk` YALNIZ base-URL/key değişikliğiyle gerçek run koşar; kurulum-seviyesi backup AYRI temiz bir kuruluma restore edilip `restore verify` ile checksum/tenant-id/run-retrieval doğrulanır; observability bundle'ı canlı alert-fire kanıtı verir; doctor v2 + support-bundle yeşildir; `self-host-0.1.0` evidence verifier yeşildir. systemd unit'leri + signed runner package statik-doğrulanmış ve split-VM enrollment'ı Docker-network seam'inde kanıtlanmıştır. **Bu gate "cloud'da kanıtlandı" İDDİA ETMEZ** — cloud-VM bacağı §6 operator legidir ve SH-2 öncesi gerçek altyapıda bir kez koşmalıdır. SH-2 (upgrade + Kubernetes) E15'tir.
