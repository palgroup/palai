# Palai Upgrade, Helm, Air-gap ve DR Hardening Plan (E15 — SH-2 RC)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (önerilen) veya superpowers:executing-plans ile task-by-task uygula. Adımlar `- [ ]` checkbox'lıdır. Helm chart / kind / NetworkPolicy / PDB / expand-contract migration idiom'ları brief'lerinde Context7/spec grounding alır (repo politikası, ledger 2026-07-17).

**Goal:** E14'ün SH-0 single-node alpha'sını **upgrade-edilebilir, Kubernetes-kurulabilir, air-gap-taşınabilir ve felaketten-dönebilir** bir SH-2 release candidate'a çevirmek: expand/migrate/contract migration disiplini + kesinti/resume + rollback window (§48.3), N→N+1 control-plane upgrade'i + runner drain + pinned active engine + new-run alias rollback (§48.4-48.5), restricted Helm install (§45.3), signed offline air-gap bundle (§45.9) ve ölçülmüş-RPO/RTO'lu DR drill'leri (§55.4-55.5). Exit gate: **rollback/restore kanıtı olmayan release promote edilemez** — bu cümle T6'da mekanik bir promote-gate script'i olur.

**Kapsam sınırı — DÜRÜST TAVAN (E14 §7'nin devamı, bu planın en önemli tasarım kararı):** Bu plan macOS + Docker Desktop oturumunda kod-subagent'larıyla İCRA EDİLİR: gerçek managed-Kubernetes cluster'ı, gerçek air-gapped network, gerçek KMS ve ayrı fiziksel host YOKTUR. Her task İKİYE bölünür: **(a) burada tam koşan LOCAL kanıt** — iki-stack docker-exec-by-name drill'leri (E14 T4/T7 kalıbı), `helm lint` + `helm template` + policy assert + kind cluster smoke (kind brew-installable; node'ları Docker container'ıdır), air-gap bundle'ın LOCAL build + `--network none` offline verify + `internal: true` network'te private-registry install'u — ve **(b) gerçek altyapı isteyen OPERATOR bacağı** (§6): gerçek restricted managed-K8s, gerçek air-gapped tesis, gerçek ikinci-site DR. Hiçbir task adı cloud/cluster bacağını kanıtladığını İDDİA ETMEZ; task adı yalnız gerçekten kanıtlananı söyler. E14 §6'nın koşulu AYNEN taşınır: SH-2 promote edilmeden önce E14 operator leg 1-2 (gerçek cloud-VM install + ayrı-host restore) en az bir kez koşmuş olmalıdır.

---

## 1. Yapı kararı — fork noktası, numaralandırma, dosyalar

**Fork noktası:** E14 kapandı (`main` aa30998 T7 merge, HEAD d8894a3, `self-host-0.1.0` bundle committed). E15 bu tepe ÜZERİNDEN forklanır; execution gate: `main` >= d8894a3.

**Migration:** Zincir **000032**'de (E13 T6 `usage_ledger`). Bu fazın şema dokunuşu YALNIZ T1'dedir ve numaralar SABİTTİR: **000033** (migration journal) + **000034** (contract örneği). Diğer tüm task'lar açıkça "Migration: yok" der. Öngörülemeyen ihtiyaç → ilk boş numara **000035**, önce owner onayı. Her migration guarded + idempotent (boot'ta zincir yeniden koşar) + `storage/embed.go` concat; append-only tablolar kendi self-re-asserting `REVOKE UPDATE,DELETE`'ini taşır (blanket GRANT her boot yeniden koşar — usage_ledger emsali). **Şema dokunuşu ⇒ `make generate` zorunlu.**

**İki "rollback" AYRIMI (isim çakışması yasak):** (a) **application rollback** (OPS-007, §48.5) — N+1'den N binary'sine dönüş, şema expanded kalırken; (b) **engine alias rollback** (§48.5) — YENİ run'lar için engine activation'ın alias/digest pointer'ının geri alınması; aktif run pinned engine'inde kalır. İkisi T2'de ayrı test edilir, tek "rollback" kelimesiyle karıştırılmaz. E14'ün kurulum-vs-run restore ayrımı da aynen geçerlidir.

**Files:** `deploy/helm/`, `deploy/airgap/`, `scripts/release/`, `docs/operations/upgrade.md`, `tests/uat/upgrade/`, `tests/uat/kubernetes/` (master plan §E15 bire bir) + bu planın eklediği `tests/uat/dr/` (DR drill harness'ının evi; master plan file listesine ek, owner §7 bloğunu paste ederken görür). Ayrı docs task'ı YOK — her task kendi runbook sayfasını `docs/operations/` altına teslim eder.

---

## 2. Design invariant (task değil, her task'ın kabul şartı)

- **Aynı binary'ler, farklı version stamp:** upgrade N ve N+1'i iki GERÇEK build'dir (pinned ref + current tree); version stamp build metadata'dır (`-ldflags -X`), davranış forklamaz. Helm chart ve air-gap bundle YENİ server yüzeyi açmaz — var olan imajları/binary'leri paketler.
- **Chart gerçeği yansıtır, spec'i değil:** §45.3'ün tam ayrıştırması (coordinator/integration workers/console) bugün TEK control-plane binary'sidir; chart v0 VAR OLANI şablonlar (control-plane Deployment + migration Job + policies), olmayan component İCAT ETMEZ. External PostgreSQL/S3 config-only'dir (stack zaten postgres URL + S3-compatible endpoint konuşur).
- **Secret'lar asla environment/argv/log/evidence'ta:** live smoke credential'ı `set -a` + `.env.local`; air-gap bundle ve evidence çıktıları mevcut secret-scan gate'inin kapsamındadır.
- **İmza disiplini tek araç:** E14 T5'in seçtiği `openssl dgst -sha256` + ECDSA P-256 detached signature kalıbı air-gap manifest'inde AYNEN yeniden kullanılır — ikinci bir imza aracı eklenmez.
- **Ölçüm fabrikasyon-korumalıdır:** RPO/RTO gibi her ÖLÇÜLMÜŞ değer evidence'ta ham timestamp'leriyle yatar; verifier değeri HAM veriden YENİDEN HESAPLAR ve uyuşmazsa FAIL eder (E13/E14 anti-fabrication anchor disiplininin ölçüme genişlemesi).
- **Zero-egress iddiası topolojiyle kanıtlanır:** air-gap "telemetry-free/no-heartbeat" asserti, egress'in İMKANSIZ olduğu `internal: true` Docker network kurgusuyla yapılır — bir log satırına güvenilmez.

---

## 3. Doğrulanmış seam envanteri (2026-07-23, d8894a3 ağacına karşı)

| Seam | Durum |
|---|---|
| Migrations | Zincir 000032'de; boot'ta idempotent yeniden koşum + blanket GRANT reassert; `usage_ledger` self-re-asserting REVOKE emsali; journal/version tablosu YOK, preflight YOK, bounded-lock sarmalayıcı YOK |
| `usage_events` | Go'da SIFIR non-test okuyucu/yazıcı (grep 2026-07-23); 000032 yorumu halefi (`usage_ledger`) bir ÖNCEKİ release'te (E13) yayınladı → GERÇEK contract-after-rollback-window örneği |
| Version stamp | Repo'da HİÇBİR yerde build-version stamping yok (`ldflags -X` yalnız `-s -w` için kullanılıyor); `scripts/release/` YOK |
| Runner drain | `runner_gateway.go`'da SIFIR drain/cordon hit'i; enroll seam: `runner_gateway.go` + `packages/runner/serve.go` + `router.go`; E10 checkpoint recovery (`workspace_recovery.go`, host_kill live testi) drain'in yeniden kullanacağı alt katman |
| E14 varlıkları | `production.yml` + TLS edge; `palai backup/restore/restore verify` (per-file sha256 manifest, boş-hedef kuralı); doctor v2 + support-bundle; signed runner host package (`scripts/package/runner`, openssl P-256); systemd units; observability bundle; iki-stack docker-exec-by-name kanıt kalıbı |
| K8s toolchain | `helm` + `kubectl` kurulu; `kind`/`kubeconform` KURULU DEĞİL (ikisi de brew-installable, kind node'ları Docker container'ı — Docker Desktop'ta koşar); Docker Desktop build-hang workaround'u bilinir (crane-load memory) |
| Object store / DB | seaweedfs S3 endpoint (8333) + postgres URL — Helm external-PG/S3 values config-only |
| Evidence | `tests/uat/evidence.go` claim/proof tipleri + anti-fabrication anchor'lar (digest recompute) + `make evidence-verify`; committed bundle emsalleri `self-host-0.1.0`, `managed-cloud-0.1.0`; `tests/uat/cases/` authored kalıp |
| Mevcut case'ler | OPS-002; DR-002/004/005/006 (E14, local seam); SAN-001..008 (E05/E09/E10); OPS-003..008, DR-001, SAN-011 case'i YOK |
| Hedef dizinler | `deploy/helm/`, `deploy/airgap/`, `scripts/release/`, `tests/uat/upgrade/`, `tests/uat/kubernetes/`, `docs/operations/upgrade.md` — HİÇBİRİ YOK, sıfırdan yazılır |

---

## 4. Task breakdown

**DAG:** Wave 1 (paralel, cap 3): **T1, T3, T5** — üç ayrık seam (storage/migrations + boot; deploy/helm; tests/uat/dr + E14 backup araçları). Wave 2 (paralel, cap 2): **T2** (T1'e bağlı — upgrade drill'i expand tranche'ını uygular), **T4** (T3'e bağlı — bundle chart'ları içerir); ikisi de `scripts/release/`'e dosya ekler; entegrasyon merge sırası SABİT: önce T2, sonra T4 (E14'ün T3→T4 dispatch-switch emsali). Wave 3: **T6** (hepsine bağlı, exit-gate evi). Her paralel merge sonrası `go vet -tags="component live" ./...` (tagged-test stale-caller kuralı). Her task RED-first TDD + green milestone başına commit; real-provider live smoke `set -a` + `.env.local`.

### T1 — Expand/migrate/contract disiplini + migration journal (mig **000033**, **000034**)

- [ ] **000033_migration_journal**: append-only `schema_revisions` journal'ı (migration no, checksum, applied_at, applied_by version stamp) + self-re-asserting REVOKE (usage_ledger emsali). Boot migration koşucusuna: **preflight** (version/disk/backup-status kontrolü, §48.3.1), per-migration **bounded lock** (`lock_timeout`/`statement_timeout`), journal'a first-apply kaydı (idempotent re-run no-op kalır).
- [ ] **Kesinti/resume:** migration zinciri ortasında fault-injection (test-only hook) ile control-plane öldürülür → restart → zincir tamamlanır, journal doğru head'i gösterir, veri bozulmaz (pre/post row-checksum) — OPS-006 bire bir.
- [ ] **000034_contract_usage_events**: GERÇEK contract örneği — `usage_events` DROP'u. In-file kanıt yorumu: sıfır okuyucu (grep), halef 000032 bir önceki release'te, rollback hedefi (E14 binary'si) tabloya hiç dokunmaz → rollback window kuralına UYARAK contract. Resumable background-migration kalıbı component testte fixture-migration ile kanıtlanır (canlı zincire demo numarası eklenmez).
- [ ] `docs/operations/upgrade.md`'nin migration yarısı (expand/migrate/contract kuralları, rollback window, journal okuma).
- **Seam:** `storage/migrations/` + `storage/embed.go` + boot migration koşucusu. **UAT:** OPS-006; OPS-007'nin şema yarısı ("expanded schema supports prior version" T2'de icra edilir). **Migration:** 000033, 000034 (+`make generate`).
- **Kanıt (burada koşar):** component testler (preflight, bounded lock, journal, fault-injection resume, idempotent re-run); live: gerçek stack'te kesinti/resume drill'i.
- **Honest ceiling:** "background data migration" kalıbı fixture ile kanıtlanır — canlı zincirde büyük-veri backfill gerektiren gerçek bir vaka bugün yok; ilk gerçek vaka bu kalıbı kullanmak ZORUNDADIR.

### T2 — N→N+1 upgrade + runner drain/revoke + pinned engine + alias rollback (`scripts/release/`)

- [ ] `scripts/release/build.sh`: git-describe version stamp'i `-ldflags -X` ile control-plane/runner/CLI'a; release manifest (imaj digest'leri + versiyonlar). Enroll handshake'ine runner version taşınır; control-plane §48.2 penceresini (current + previous two minors) enforce eder — unsupported skew **gerekli ara-yol mesajıyla REDDEDİLİR** (OPS-008).
- [ ] Runner **cordon/drain** (yeni lease durur; aktif run §26.3 checkpoint recovery ile taşınır/tamamlanır) + **revoke** (yeni lease'ler ve stale event'ler reddedilir — SAN-011). E10 recovery katmanı YENİDEN KULLANILIR, yeniden yazılmaz.
- [ ] `palai upgrade` (compose-driven §48.4 sırası: backup + restore-status → signature/compat verify → expand → control-plane swap → runner drain → new-run engine alias roll → smoke) + `palai upgrade rollback` (app image N'e döner, şema expanded kalır; engine alias pointer'ı YENİ run'lar için geri alınır — aktif run pinned engine'de). `docs/operations/upgrade.md`'nin sequence yarısı.
- **Seam:** `scripts/release/` (yeni) + `runner_gateway.go`/`serve.go` handshake + `cmd/cli/` upgrade komutu. **UAT:** OPS-005, OPS-007, OPS-008; SAN-011. **Migration:** yok (T1'in tranche'ını UYGULAR).
- **Kanıt (burada koşar):** N = pinned fork-point ref'ten worktree docker build, N+1 = current tree — iki GERÇEK build. Local drill: N stack'te uzun (fake-provider) run AKTİFKEN upgrade → run pinned engine'iyle hayatta kalır ve biter; N+1'de yeni run + BİR real-provider smoke; app rollback sonrası N binary'si expanded şemayla çalışır; eski-stamp'li runner ara-yol mesajıyla reddedilir; revoke sonrası stale event reddedilir.
- **Honest ceiling:** N→N+1 İKİ LOCAL build arasındadır — henüz yayınlanmış bir önceki release yoktur; gerçek published-release-arası upgrade §6 operator legidir. Drain tek-runner batch'tir (multi-runner fleet yok, SH-0 topolojisi).

### T3 — Restricted Helm chart + kind install smoke (`deploy/helm/`)

- [ ] `deploy/helm/palai/`: control-plane Deployment (default replicas=1 — HA İDDİA EDİLMEZ, §45.2) + **migration Job** (pre-install/pre-upgrade hook; control-plane binary'sine küçük `--migrate-and-exit` mode'u eklenir) + external PostgreSQL/S3 values (+existingSecret ref'leri; in-cluster DB YOK) + NetworkPolicy (default-deny + DNS + PG/S3 egress + ingress-controller'dan ingress) + PDB + Pod Security **restricted** uyumlu securityContext + resource requests + ingress TLS + RuntimeClass value passthrough + YALNIZ namespace-scoped Role/RoleBinding (**hiçbir ClusterRole yok** — no ongoing cluster-admin).
- [ ] Runner CHART'TA DEĞİL: §45.4 kalıbı — E14 signed host package dışarıdan outbound-only enroll olur (runner docker.sock'u host'ta; cluster'a sokulmaz). Chart README + `docs/operations/kubernetes.md` bunu açıkça yazar.
- [ ] Render-assert suite `tests/uat/kubernetes/`: `helm lint` + `helm template` çıktısı üzerinde Go assert'leri (ClusterRole yok, runAsNonRoot, privileged yok, NetworkPolicy/PDB/Job-hook mevcut) + `kubeconform` şema doğrulaması (brew install).
- **Seam:** `deploy/helm/` (yeni) + `cmd/palai-control-plane` migrate-only flag + `tests/uat/kubernetes/` (yeni). **UAT:** OPS-003 (kind-local seam'inde). **Migration:** yok.
- **Kanıt (burada koşar):** lint + template + policy assert'leri yeşil; **kind smoke** (brew install kind): `kind load` ile imaj → chart install → migration Job biter → healthz → admin CLI ile provisioning → host'tan E14 runner package enroll → fake-provider run tamamlanır. "External" PG/S3 values'ü, chart-DIŞI fixture manifest'leriyle deploy edilen DB/store'a point edilerek kanıtlanır.
- **Honest ceiling:** kind'ın default CNI'ı (kindnet) NetworkPolicy'yi ENFORCE ETMEZ — policy doğruluğu render/şema seviyesinde kanıtlanır, enforcement gerçek CNI'lı cluster ister (§6). Gerçek managed-K8s (EKS/GKE), HA/topology-spread davranışı, gerçek LB/cert-manager = operator legi.

### T4 — Signed offline air-gap bundle (`deploy/airgap/` + `scripts/release/`)

- [ ] `scripts/release/airgap-build.sh` + `deploy/airgap/`: §45.9 bundle — OCI imajları digest'le (`docker save`), runner host package (E14 tarball'ı), CLI binary, compose + helm chart'ları, `storage/migrations/` kopyası, sha256sums + **openssl P-256 detached signature'lı manifest** (E14 T5 aracı AYNEN). Manifest'te SBOM/provenance ALANLARI tanımlı ama üretimi E18'dedir (bilinçli boş, manifest bunu söyler).
- [ ] `deploy/airgap/verify.sh`: **offline** doğrulama (`--network none` container'da imza + digest zinciri). `deploy/airgap/install.sh`: private registry'ye mirror (digest-pinned `registry:2`) + oradan stack bring-up.
- [ ] Telemetry-free/no-heartbeat kanıtı: install `internal: true` Docker network'te — private model endpoint (in-network fake provider) + in-network Git fixture (stock git-http-backend/daemon container; araç seçimi brief'te, ladder: toolchain'de olan kazanır) ile GERÇEK run tamamlanır; egress topolojik olarak imkansızdır. `docs/operations/airgap.md`.
- **Seam:** `deploy/airgap/` (yeni) + `scripts/release/` (T2 SONRASI merge — sabit sıra). **UAT:** OPS-004 (internal-network local seam'inde). **Migration:** yok.
- **Kanıt (burada koşar):** bundle build → `--network none` verify yeşil → tamper testi (1 byte oynat → verify FAIL) → internal-network'te registry-mirror install → fake-provider + internal-Git run tamamlanır → zero-egress asserti.
- **Honest ceiling:** gerçek air-gapped tesis, operator trust-root mirror ceremony'si ve gerçek private model server = operator legi (§6); fake provider private-model-endpoint'in yerine geçer. SBOM/provenance üretimi E18.

### T5 — DR drill'leri + ÖLÇÜLMÜŞ RPO/RTO raporu (`tests/uat/dr/`)

- [ ] Drill harness'ı (E14 iki-stack docker-exec-by-name kalıbı; saniyelik marker-write trafiği altında): **DR-001** primary loss — pg container + volume YOK EDİLİR → scripted recovery (taze pg + son `palai backup`'tan restore) → RPO (kaybolan marker penceresi) + RTO (healthy + run-capable'a kadar duvar saati) HAM timestamp'lerden ölçülür.
- [ ] **DR-004** object corruption — store'da byte flip → `restore verify` per-file sha256 ile TESPİT eder → obje restore edilir veya kayıp TAM olarak raporlanır. **DR-005** key recovery — master-key dosyası kayıp: backup yanlış/eksik key'le FAIL-CLOSED; escrow kopyasıyla kullanılır (file-key seam'i; KMS E13-H). **DR-002/DR-006** — E14 restore/verify akışı drill harness'ı altında ölçümlü yeniden koşar (post-restore session/event/artifact tutarlılığı + tenant izolasyonu).
- [ ] Makine-üretimi DR raporu (`docs/operations/dr-report.md` + evidence artifact'ı): drill başına ÖLÇÜLMÜŞ RPO/RTO tablosu + §55.5 gereği self-host'un YAYINLANAN ulaşılabilir hedefleri (SaaS hedefleri MİRAS ALINMAZ) + §55.4 findings/remediation. `docs/operations/dr-drills.md` runbook'u.
- **Seam:** `tests/uat/dr/` (yeni) + E14 backup/restore araçları (yeniden kullanılır, yeniden yazılmaz). **UAT:** DR-001; DR-002/004..006 drill-ölçümlü derinleşme. **Migration:** yok.
- **Kanıt (burada koşar):** beş drill'in tamamı local iki-stack'te yeşil; rapor HAM timestamp'lerden üretilir (T6 verifier'ı yeniden hesaplar).
- **Honest ceiling:** "primary loss" aynı Docker Desktop'ta container/volume imhasıdır — gerçek instance/zone kaybı ve ayrı fiziksel host restore'u operator legidir (§6, E14 leg 2 dahil); DR-003 regional failover SaaS planıdır; KMS ceremony E13-H.

### T6 — SH-2 RC journey + evidence gate + promote kuralı (`tests/uat/upgrade/`)

- [ ] Journey harness'ı `tests/uat/upgrade/` (HOST-AGNOSTİK, E14 T7 kalıbı): temiz N install → provisioning + gerçek run → backup → **aktif run hayattayken N→N+1 upgrade** → app rollback + engine-alias rollback kanıtı → DR-001 drill + restore verify (ölçümlü) → air-gap bundle build + offline verify → helm render/policy assert (+kind smoke referansı) → hepsi evidence'a.
- [ ] `tests/uat/evidence.go`'ya yeni claim/proof tipleri (anchor disiplini): **UpgradeProof** (N/N+1 stamp'leri, hayatta-kalan run id, event-continuity digest — verifier digest'i yeniden hesaplar), **MigrationJournalProof** (journal head + kesinti/resume marker'ları), **DrillProof** (HAM drill timestamp'leri — verifier RPO/RTO'yu YENİDEN HESAPLAR, rapor değeriyle uyuşmazsa FAIL: ölçüm-fabrikasyonu anchor'ı), **AirgapProof** (manifest digest + imzanın offline re-verify'ı), **HelmRenderProof** (render hash + policy assert listesi recompute).
- [ ] **OPS-003..008, DR-001, SAN-011** case'leri `tests/uat/cases/` altında materialize; **DR-002/004..006** case'leri drill-ölçümlü güncellenir. Her case metni LOCAL seam'i ve operator bacağını AÇIKÇA adlandırır (E14 emsali).
- [ ] `make uat-sh2` + **`self-host-0.2.0` evidence bundle** (redacted manifest, `maturity: rc` alanı; E14'ün self-host track'inin devamı) + `make evidence-verify` 0/0/0/0. **Promote gate:** `scripts/release/promote.sh` — bundle'da UpgradeProof(rollback dahil) + restore/DR proof YOKSA tag/promote REDDEDİLİR; RC-ötesi promote ayrıca E14 §6 leg 1-2 operator icrasını bekler (attestation notu manifest'te).
- **Exit-gate proof'un evi budur.** **Migration:** yok.
- **Honest ceiling:** gate kanıtı LOCAL seam'dir (iki-local-build upgrade, kind cluster, internal-network air-gap, aynı-host DR) — §6 operator legleri adlarıyla dışarıda kalır ve journey harness'ı hepsine parametrik olarak point edilebilir (tasarım şartı).

---

## 5. OUT OF SCOPE (bilinçli dışarıda, adres adresine)

| Kalem | Neden dışarıda | Nerede yaşıyor |
|---|---|---|
| Gerçek managed-K8s install (EKS/GKE), NetworkPolicy ENFORCEMENT, HA/topology davranışı | Kod agent'ı cluster işletemez; kanıt kind + render-assert seam'inde | §6 operator legs |
| Gerçek air-gapped tesis + trust-root mirror ceremony + gerçek private model server | Aynı sebep; kanıt internal-network + fake-provider seam'inde | §6 operator legs |
| Ayrı fiziksel host / ikinci site DR; gerçek instance/zone kaybı | Aynı sebep; kanıt aynı-host iki-stack drill'idir | §6 operator legs (E14 leg 2 dahil) |
| DR-003 regional failover | Managed SLA ürünü | SaaS planı |
| KMS-backed master key + lease ceremony (SEC-001/003) | DR-005 file-key seam'iyle geçer | E13-H |
| SBOM/provenance/hermetic-build pipeline'ı | Air-gap manifest alanları tanımlar, üretmez | E18 (`scripts/release/` devralır) |
| Performance/soak, release support matrix, stable sign-off | RC sonrası | E18 |
| Multi-runner fleet, autoscale, elastic runner pools | SH-0 topolojisi (tek runner) yeter; drain tek-batch | SaaS / E18 sonrası |
| SAN-009 (microVM fleet) | §2.2 istisnası | SaaS planı |
| SAN-010 (preview/terminal auth), SAN-012 (runner clock skew) | E15'in beş workstream'ine girmiyor; case'leri hâlâ materialize değil — **owner'a bayrak:** Appendix A E15'i SAN co-owner sayar, bu iki ID'nin evi netleşmeli (öneri: SAN-010 → E10/E17 yüzeyi, SAN-012 → doctor/quarantine zaten var, case E18 regression'da) | owner kararı |
| Web-console UI | Admin CLI tek insan arayüzü | E17c |
| Py/Go SDK, 2. provider | Parity işi | E16 |

## 6. Operator legs — gerçek-altyapı bacağı (deferred-but-scripted; kaybolmaz)

Her biri için KOD/harness bu fazda hazırdır ve parametriktir; İCRA operator-provided altyapı ister:

1. **Gerçek restricted managed-K8s install** (T3 chart'ı + `tests/uat/kubernetes/` assert'leri gerçek cluster'a point edilir) — enforcing CNI ile NetworkPolicy davranışı, PSS restricted admission, gerçek ingress/LB/cert dahil.
2. **Gerçek air-gapped tesis install'u** (T4 bundle'ı fiziksel olarak taşınır; operator trust-root/mirror ceremony'si; gerçek private model + Git endpoint'leri).
3. **Gerçek ikinci-host/site DR drill'i** (T5 harness'ı ayrı fiziksel host'a point edilir; gerçek instance kaybı; DR-003'ün öncül ölçümleri).
4. **Published-release-arası N→N+1** (ilk yayınlanmış release çıktığında T2 drill'i iki gerçek release arasında yeniden koşar).
5. **E14 §6 leg 1-2** (gerçek cloud-VM clean install + ayrı-host restore) — E14'ün koyduğu koşul: SH-2 PROMOTE edilmeden önce en az bir kez koşmuş olmalı; `promote.sh` attestation notu bunu izler.

Bu legler T6 gate'inin LOCAL kanıtını BLOKLAMAZ; SH-2'nin RC-ötesi promote'u 1, 2 ve 5'in icrasını bekler.

## 7. Master plan §8 için önerilen özet blok (owner paste eder)

**UAT ownership:** OPS-003..008; DR-001..002, DR-004..006 (drill-ölçümlü); SAN-011 (drain/revoke yarısı — SAN-010/012 evi için §5 bayrağı) — tamamı `tests/uat/cases/` altında materialize, kanıt seam'i case metninde adlandırılır. DR-003 SaaS planındadır.

**Exit gate — SH-2 RC (local seam'de):** Aktif bir run hayattayken N→N+1 upgrade'i pinned engine'le tamamlanır; app rollback expanded şemada, engine-alias rollback yeni run'da kanıtlanır; kesintiye uğratılan migration resume olur ve journal doğru head'i gösterir; unsupported runner skew'u ara-yol mesajıyla reddedilir; restricted Helm chart'ı lint/render/policy-assert + kind install smoke'u geçer (ClusterRole'süz, migration Job'lı, external PG/S3'lü); signed air-gap bundle'ı offline verify edilir ve internal-network'te telemetry-free install'dan gerçek run koşar; DB-primary-loss/object-corruption/key-recovery drill'leri ÖLÇÜLMÜŞ RPO/RTO raporu üretir ve verifier ölçümleri ham timestamp'lerden yeniden hesaplar; `self-host-0.2.0` evidence verifier yeşildir ve `promote.sh` rollback/restore kanıtı olmayan release'i REDDEDER. **Bu gate "gerçek cluster'da/air-gap'te/ikinci-site'ta kanıtlandı" İDDİA ETMEZ** — o bacaklar §6 operator legleridir ve RC-ötesi promote §6'daki 1, 2 ve 5. leglerin icrasını bekler.
