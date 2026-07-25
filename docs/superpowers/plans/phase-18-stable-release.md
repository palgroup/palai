# Palai Release Supply Chain ve Stable Sign-off Plan (E18 — FİNAL epic)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (önerilen) veya superpowers:executing-plans ile task-by-task uygula. Adımlar `- [ ]` checkbox'lıdır. syft/grype (SPDX/CycloneDX SBOM), in-toto Statement / SLSA provenance v1 predicate şeması, `docker buildx` multi-arch, GitHub Actions release/environment idiomları brief'lerinde Context7/spec grounding alır (repo politikası, ledger 2026-07-17).

**Goal:** E00–E17'nin ürettiği her şeyi **doğrulanabilir bir release'e** bağlamak: pinned hermetic build + amd64/arm64 matrisi, SBOM, in-toto/SLSA provenance, digest/imza ve **tam offline verification**; SEC-101..103 + PER-001..004; security threat model / vulnerability process / runbooks / support matrix; ve **FİNAL cross-epic stable sign-off** — önceki HER epic bundle'ını yeniden doğrulayan, ürün-geneli stable/preview/disabled posture'unu claim outcome'larından YENİDEN hesaplayan bir `release-1.0.0-rc1` bundle'ı. Exit gate: **fabrike bir cross-epic "stable" FAIL'dir; SH-3 Stable flip'i lokal ilan değil, promote gate'in mekanik olarak beklediği operator attestation'ıdır.**

**Kapsam sınırı — DÜRÜST TAVAN (E14→E17 geleneğinin devamı; bu planın omurgası):** Bu plan macOS + Docker Desktop oturumunda kod-subagent'larıyla İCRA EDİLİR; iki gerçek provider credential'ı vardır (`.env.local`, `set -a`, asla argv/log/evidence/commit) ve **GERÇEK release altyapısı YOKTUR** — ne GitHub protected environment'ı, ne ikinci maintainer, ne registry/Sigstore/KMS credential'ı, ne referans donanım. Bu yüzden: (a) **Signing** — imza + OFFLINE-VERIFY MEKANİZMASI lokal kanıtlanır, E14 T5 openssl P-256 signer'ı VERBATIM (tek imza aracı invariant'ı; fixture/self-managed key); gerçek Sigstore/cosign keyless (Fulcio/Rekor) veya org-KMS imza kimliği §6 operator legidir — **bu plan hiçbir yerde gerçek transparency-log entry'si İDDİA ETMEZ**. (b) **SBOM** — fiilen build edilen artifact'lar üzerinde lokal + gerçektir (digest-pinned syft/grype sınıfı üreteç, pinned offline vuln-DB snapshot'ı; canlı CVE feed güncelliği §6). (c) **Provenance** — attestation MEKANİZMASI + offline doğrulaması lokaldir; builder identity alanı dürüstçe `local-macos-session` yazar; gerçek GitHub-Actions-imzalı provenance / hermetic CI koşumu §6 legidir. `.github/workflows/release.yml` AUTHOR edilir ve mantığı script-level test edilir; **gerçek bir CI koşumu §6'dır**. (d) **Performance (PER-001..004)** — harness + deterministic smoke lokaldir (E17 T6 disiplini: harness + gate MEKANİĞİ kanıtlanır, sayılar değil); GERÇEK hardware/load-profile SAYILARI §6 legidir — lokal sayılar yalnız ZORUNLU macOS+Docker-Desktop profil damgasıyla kaydedilir, SLO iddiası taşımaz. (e) **Two-person promotion** — release-policy.md'nin iki-kişi/protected-environment sözleşmesinin kanıtı logic-level'dır (onaysız promote REFUSE edilir); ceremony'nin kendisi tek-maintainer bu oturumda İCRA EDİLEMEZ → §6. (f) **E08 kuralı geçerliliğini korur:** engine gerçek provider'a TOOL AÇMAZ — final journey'nin canlı bacağı tek adımlık gerçek run'dır; real-model eval quality sayıları (E17 §6 leg 7) lokalde ÜRETİLEMEZ, stable attestation'ın operator INPUT'udur.

---

## 1. Yapı kararı — fork noktası, migration, dosyalar

**Fork noktası:** E17 T11 kapanışı (`extensions-0.1.0` bundle + tier promotion) `main`'e merge olduktan sonra; execution gate: `main` >= E17 T11 merge tip. E17'nin findings-so-far'ı bu plana işlenmiştir; T11 kapanışında yeni bulgu çıkarsa plan amend edilir.

**Migration:** Zincir **000040**'ta (E17 T9). **Bu fazın HER task'ı migration-FREE'dir** — supply chain işi CI/script/docs/tests/evidence'tır, şema istemez. Tek aday T7'nin audit-integrity checkpoint'iydi; kararı: checkpoint MUTABLE STORE DIŞINDA yaşar (release signer'la imzalı dosya/object-store — anchor'ı tamper edilebilir yere koymak tamper-evidence'ı boşaltır), yani migration yok. Öngörülemeyen ihtiyaç → **000041**, önce owner onayı; o durumda guarded + idempotent + `storage/embed.go` concat + RLS/REVOKE kuralları aynen.

**Files:** `.github/workflows/release.yml` (yeni), `scripts/release/` (mevcut — genişler), `docs/security/` (release-policy.md var; threat-model + vulnerability-process yeni), `docs/operations/runbooks/` (yeni dizin), `tests/performance/` (yeni), `evidence/releases/` — master plan §E18 bire bir; artı `tests/uat/stable-release/` (final journey evi) + `tests/uat/cases/SEC-10x, PER-00x` (owner §7 bloğunu paste ederken görür).

---

## 2. Design invariant (task değil, her task'ın kabul şartı)

- **Tek imza aracı:** E14 T5 openssl P-256 signer'ı VERBATIM (`scripts/package/runner/build.sh` komut seti); ikinci bir imza aracı/keyring = review reject. Provenance predicate'i standart (in-toto Statement / SLSA v1) yazılır ama imza openssl'dir ve her yerde böyle ADLANDIRILIR — "cosign-verified" kelimesi kullanılamaz.
- **Fail-closed verifier resolution (E16 T7 e4aeb6f kalıbı):** hiçbir verifier bundle İÇİNDEN fallback'lenmez; bundled-verifier kullanımı yalnız same-session local proof için EXPLICIT env opt-in'idir. `deploy/airgap/verify.sh`'ın bugünkü fail-open fallback'i T4'te kapatılır.
- **Digest her yerde, mutable tag hiçbir yerde:** installer/manifest/evidence/config bir artifact'ı YALNIZ sha256 digest'iyle tanır (§51.3); mutable tag hareketi pinned run'ı değiştiremez. Base image'lar dahil: digest'siz `FROM` guard-test'le reddedilir.
- **Sayı ancak profille:** hardware/load profile stanza'sı (makine, çekirdek, bellek, Docker Desktop sürümü, load şekli) olmayan HİÇBİR performans sayısı evidence'a giremez — verifier profilsiz PerformanceProfileProof'u FAIL eder (matrix satırı: "hardware/load profile zorunlu").
- **Recompute-over-copy (E13..E17 anchor disiplini):** her yeni proof tipinin verifier'ı iddiayı manifest'in kendi kopyasından DEĞİL kanonik kaynaktan yeniden hesaplar — digest'ler artifact byte'larından, percentile'lar ham örneklerden, tier'lar claim outcome'larından, release index per-bundle manifest'lerden. Journey her claim'in GÜÇLÜ formunu assert eder (failed-run'ın da bastığı bir substring asla — E15 T6 dersi).
- **Secret'lar:** `.env.local` `set -a`; signing key'ler asla commit/log/evidence (ephemeral key mint yalnız local proof, E16 T7 emsali); secret-scan gate'i yeni tüm yüzeyleri (SBOM/provenance/perf çıktıları) kapsar. Sıkıştırılmış üye İÇİ scan decompress-sonra-scan'dır (E14 T7 vacuous-scan dersi).
- **Honest naming:** "hermetic / reproducible / signed / stable" kelimeleri yalnız kanıtlanan biçimiyle: binary-level repro ≠ image-layer repro; openssl ≠ Sigstore; RC ≠ stable. Kanıtlanmayan şey İLAN EDİLMEZ.

---

## 3. Doğrulanmış seam envanteri (2026-07-24, ağaca karşı)

| Seam | Durum |
|---|---|
| `scripts/release/` | build.sh (E15 T2 version stamp + manifest), promote.sh→`tests/uat/cmd/promote` (E15 T6), airgap-build.sh (E15 T4), sdk-package.sh + sdk-verify.sh (E16 T7; sbom/provenance alanları TANIMLI-null, "E18" notlu), runner-verify.sh (E14 T5 verifier VERBATIM byte-copy) |
| `deploy/airgap/verify.sh` | **fail-OPEN fallback DOĞRULANDI:** OOB verifier yoksa `verifier="$(pwd)/runner-verify.sh"` (bundle'ın kopyası) — E16 T7'nin fail-closed + `ALLOW_BUNDLED_VERIFIER=1` opt-in fix'i buraya uygulanmamış → T4 kapatır (deferred finding 1) |
| `tests/uat/evidence.go` + `promote.go` | Complete()/VerifyManifest + anchor disiplini; case `Checksum` yalnız SHAPE-check'tir (`evidence.go:1046-1048` — sha256:hex64 regex, recompute YOK) → fabrike checksum bugün yeşil geçer, T8'in işi; `PromoteGateFor` iki promote ailesi (E15 upgrade / E16 sdk-parity; `stable` target operator_attestation İSTER, asla auto-claim) — E17 T11 üçüncü aileyi ekler, T10 dördüncüyü |
| `evidence/releases/` | 15 committed bundle + `extensions-0.1.0` (E17 T11 ile 16); ledger'a göre `automation-0.1.0` AUT-001 checksum'ı FABRİKE (shape-valid olduğu için verifier'dan geçiyor) → T8 sweep'i yakalar (deferred finding 2) |
| Dockerfiles | `engines/reference/Dockerfile` digest-pinli; `deploy/compose/{control-plane,runner}.Dockerfile` base'leri TAG-only (`golang:1.26.4`, `alpine:3.21`) → T1 gerçek iş; compose Postgres digest-pinli |
| `.github/workflows/` | yalnız ci.yml — actions SHA-pinned (release.yml için emsal); release.yml YOK |
| `tests/performance/` | YOK (spec §59 ağacında adı var) |
| `docs/security/` | release-policy.md VAR ve "E18 implements..." implementation gate'ini açıkça bu faza devreder (two-person, protected env, SBOM/provenance, offline verifier); threat-model/vulnerability-process YOK |
| `docs/operations/` | upgrade/backup-restore/dr-drills/observability/airgap/sdk-compatibility(+json guard) VAR; `runbooks/` dizini YOK — runbook'lar mevcut docs'a link verir, kopyalamaz |
| Audit journal | `events.seq` monotonic journal; hash-chain/checkpoint YOK — E13-H "audit integrity linkage" AÇIK → T7'nin evi (SEC-103) |
| E13-H kalanı | phase-13 §6: SEC-001/003 (KMS ceremony), TEN-004, DAT-001..005 + DAT-006 kalanı, BIL-002/004/005, audit linkage, OTel redaction — audit linkage T7'de kapanır, kalanı T9 triage'ında tek tek dispoze edilir |
| Public-API gap kayıtları | modelRoutes write-only + list-envelope: E16 T1'de KAPANDI (`router.go:219-224` GET/LIST doğrulandı — yalnız verification satırı kalır); a2a push delivery: config-CRUD var, DELIVERY unwired (E17 T2 f1ce5c9, dürüst case metniyle); approval richer detail / `/v1/publications` read endpoint YOK (E17 T10 3b8f919, named PUBLIC-API GAP) → T9 karar tablosu |
| UAT cases | `tests/uat/cases/` 126 case; SEC-10x / PER-00x SIFIR materialization |
| Credentials | `.env.local` iki gerçek provider; registry (npm/PyPI/Go proxy/GHCR), Sigstore, KMS, referans donanım YOK |

---

## 4. Task breakdown

**DAG (cap 3):**
Wave 1: **T1** (hermetic build + matris), **T6** (performance harness), **T8** (anti-fabrication sweep) — üç ayrık seam. Wave 2: **T2** (SBOM — T1 artifact'ları üzerinde), **T3** (provenance+imza — T1 build-input'unu bağlar), **T7** (SEC-102/103). Wave 3: **T4** (unified offline verifier — T2/T3 çıktıları), **T9** (docs + RC triage — T7/T8 bulgularını yer). Wave 4: **T5** (release.yml — T1..T4 script'lerini orkestre eder). Wave 5: **T10** (FİNAL EXIT gate, hepsine bağlı). Her paralel merge sonrası `go vet -tags="component live" ./...`; her task RED-first TDD + green milestone başına commit; canlı bacaklar `set -a` + `.env.local`.

**SECURITY-CRITICAL işareti taşıyan task'lar (T1, T2, T3, T4, T5, T7, T8, T10 — signing/verification/supply-chain integrity'ye dokunan her şey) full Fable review alır.**

### T1 — Pinned hermetic build + amd64/arm64 release matrisi (mig yok; SECURITY-CRITICAL)

- [ ] Digest-pin: `control-plane.Dockerfile` + `runner.Dockerfile` base'leri (`golang`/`alpine`) `@sha256` pin alır (reference-engine emsali); **guard test** her `FROM`'un digest'li olduğunu ve toolchain sürümlerinin pinli olduğunu mekanik reddeder (RED-first: pinsiz FROM → FAIL).
- [ ] Hermetic build: build stage `GOPROXY=off` + warmed module cache (vendoring YOK — lazy doğru yol), `-trimpath` + buildid strip, `SOURCE_DATE_EPOCH` (tar'lar E14 T5 fixed-mtime idiomuyla). **Binary-level reproducibility:** aynı commit'ten İKİ build → CLI/runner binary sha256'ları BİT-EŞİT (RED-first: repro bozan flag fixture'ı → FAIL).
- [ ] Release matrisi: `build.sh` genişler — control-plane/runner/reference-engine image'ları **linux/amd64 + linux/arm64** (buildx), CLI **darwin/linux × amd64/arm64** (GOOS/GOARCH), runner host package per-arch; çıktı **`release-index.json`**: her artifact → digest + arch + tür (release-policy "Required release artifacts" listesinin iskeleti; sbom/provenance alanları T2/T3'e tanımlı-boş).
- **Seam:** `scripts/release/build.sh` + `deploy/compose/*.Dockerfile`. **UAT:** SEC-101'in üretim yarısı (T4 tüketir); OPS regression yeşil kalır. **Kanıt (burada koşar):** double-build repro eşitliği; amd64 image'ına qemu boot-smoke (container ayağa kalkar + healthz).
- **Honest ceiling:** image-LAYER reproducibility İDDİA EDİLMEZ (layer timestamp'leri) — kanıt binary-level'dır ve öyle adlandırılır; amd64 üzerinde TAM UAT koşumu §6 legidir (qemu boot-smoke yeterli kabul edilmez, edilmediği yazılır).

### T2 — SBOM + vulnerability scan + license inventory (mig yok; SECURITY-CRITICAL)

- [ ] Digest-pinned SBOM üreteci (syft sınıfı, container'da pinli): her image + üç SDK paketi + CLI binary'si için **SPDX ve CycloneDX** SBOM (§51.2); license inventory; release-index'e digest'le girer.
- [ ] Vulnerability scan (grype sınıfı, pinli): **pinned offline DB snapshot'ı** (snapshot tarihi manifest'e yazılır); §51.3 critical-vuln policy gate — critical bulgu promotion'ı BLOKlar, exception yalnız time-bound + owner'lı (§62.2 P2 disiplini; şekli manifest'te).
- [ ] `sdk-package.sh` manifest'inin `sbom:null / provenance:null` dürüst-boş alanları GERÇEK değerle dolar (E16 T7 notu emekli edilir — yorum yeni gerçeği söyler).
- [ ] RED-first: bilinen-vulnerable fixture (eski sürümlü paket) policy gate'te FAIL; SBOM'u eksik artifact → index doğrulaması FAIL.
- **Seam:** `scripts/release/` (sbom.sh sınıfı) + release-index. **UAT:** SEC-101'in SBOM-varlığı bacağı. **Kanıt (burada koşar):** gerçek build çıktıları üzerinde SBOM üretimi + scan + tamper (SBOM'da 1 byte → digest FAIL).
- **Honest ceiling:** vuln-DB pinned snapshot'tır — canlı CVE feed güncelliği ve süreklilik taraması §6 legidir; scan'in kapsamı SBOM'un gördüğüdür (statik binary'lerde Go module listesi, image'larda paket DB'si).

### T3 — in-toto/SLSA provenance + imza (mig yok; SECURITY-CRITICAL)

- [ ] Provenance attestation: **in-toto Statement + SLSA provenance v1 predicate** JSON — subject = artifact digest'leri (release-index'ten), materials = git commit + base image digest'leri + toolchain sürümleri (T1 build-input'u), builder id = **dürüstçe `local-macos-session`** (GitHub workflow-identity alanı TANIMLI, T5'in CI koşumu §6'da doldurur); invocation = build komutu + parametreler.
- [ ] İmza: openssl P-256 detached signature (E14 T5 signer VERBATIM — design invariant); imza zarfı sha256sums-root kalıbıyla (E15 T4/E16 T7 emsali) provenance + SBOM + index'i TEK signed root'a bağlar.
- [ ] Offline verify: provenance→subject digest binding'i + imza `--network none`'da re-verify (T4'ün tükettiği primitive).
- [ ] RED-first: subject digest oynat → FAIL; materials'tan base-image digest'i düşür → FAIL; yanlış/eksik out-of-band key → FAIL (E14 T5 trust modeli aynen).
- **Seam:** `scripts/release/provenance.sh` sınıfı + runner-verify.sh. **UAT:** SEC-101'in provenance bacağı. **Kanıt (burada koşar):** gerçek build üzerinde attestation + offline verify + tamper matrisi.
- **Honest ceiling:** predicate cosign-uyumlu ŞEKİLDE yazılır ama imza openssl'dir — **Sigstore/Fulcio/Rekor/transparency-log İDDİASI YOK** (§6 leg 1); "SLSA L2" hedefi ancak gerçek CI koşumuyla (§6) iddia edilebilir, lokalde yalnız "L2-shaped mechanism" denir.

### T4 — Unified offline release verifier + SEC-101 + airgap fail-closed fix (mig yok; SECURITY-CRITICAL)

- [ ] `scripts/release/release-verify.sh`: release-index'in TAMAMI offline — her artifact digest'i re-compute, SBOM varlık+digest, provenance binding, signed-root imzası; `--network none` container kanıtı (E15 T4 kalıbı); fail-closed verifier resolution (design invariant).
- [ ] **`deploy/airgap/verify.sh` verifier-swap fix (deferred finding 1):** bundled-verifier fallback'i kaldırılır → fail-CLOSED; same-session local proof için `PALAI_AIRGAP_ALLOW_BUNDLED_VERIFIER=1` explicit opt-in (E16 T7 e4aeb6f kalıbı bire bir; `--network-none` yolu opt-in'i kendisi geçirir, host yolunda default kapalı). RED-first: OOB verifier yok + opt-in yok → exit≠0.
- [ ] **SEC-101 materialize** (`tests/uat/cases/SEC-101/`): tamper matrisi — image tar / SDK paketi / SBOM / provenance / imza / index'te 1 byte → **execution/promotion DENIED** (promotion yarısı: release-verify FAIL → promote refuse; execution yarısı: pinned-digest mismatch'te runner/config admission refuse — mevcut digest-pin seam'ine negative test).
- **Seam:** `scripts/release/release-verify.sh` (yeni) + deploy/airgap/verify.sh + promote zinciri. **UAT:** SEC-101. **Kanıt (burada koşar):** temiz verify yeşil + 6-kollu tamper matrisi FAIL + `--network none` koşumu.
- **Honest ceiling:** trust root'un out-of-band teslimi operatör ceremony'sidir (E14 T5 modeli); revocation listesi ŞEKİL olarak index'te tanımlıdır, gerçek advisory akışı T9 process dokümanı + §6.

### T5 — `release.yml` + two-person/protected-env logic + publish dry-run (mig yok; SECURITY-CRITICAL)

- [ ] `.github/workflows/release.yml` AUTHOR edilir: SHA-pinned actions (ci.yml emsali), ephemeral runner varsayımı, iş mantığı YALNIZ `scripts/release/*.sh` çağrılarıdır (workflow ince kalır = mantık lokalde test edilebilir), protected environment + **two-person gate** (release-policy.md sözleşmesi bire bir: builder ≠ approver, admin bypass yok), signed tag + immutable release assets, evidence checksum doğrulaması.
- [ ] Publish mekaniği DRY-RUN: `npm publish --dry-run` + wheel/sdist `twine check` sınıfı doğrulama + Go module tag provası (E16 §6 leg 3 devralınır) — credential YOKTUR, komutlar registry'ye DOKUNMAZ.
- [ ] Workflow mantığının lokal kanıtı: YAML schema/lint + çağırdığı script zincirinin uçtan uca lokal koşumu (T1→T2→T3→T4 sırası release.yml'deki adım sırasıyla BİT-EŞİT — drift testi); promote-gate logic'i: onaysız/tek-kişi promote → REFUSE (unit-pinned, promote.go zinciri).
- **Seam:** `.github/workflows/release.yml` (yeni) + scripts/release + promote.go. **UAT:** yok (destek görevi; SEC-101 promotion yarısını ve T10 SupplyChainProof'unu besler). **Kanıt (burada koşar):** script-zinciri e2e lokal + dry-run çıktıları + refuse testleri.
- **Honest ceiling:** **GERÇEK GitHub koşumu YOK** — protected environment, ikinci maintainer, gerçek OIDC workflow identity §6 legidir (release-policy'nin "Until two maintainers..." cümlesi yürürlükte kalır: bu oturum RC-üstü YAYIMLAYAMAZ); act-sınıfı emülasyon iddiası da yapılmaz.

### T6 — Performance harness: PER-001..004 (`tests/performance/`; mig yok)

- [ ] Go harness (yeni bağımlılık YOK — stdlib + mevcut compose stack + fake engine, E08 kuralı): **PER-001** single-shot + SSE load — p50/p95/p99 + error rate, §54.3 hedefleri KONFİGÜRE edilebilir eşik (gate mekaniği); **PER-002** cold/warm sandbox faz bütçeleri (E09 workspace/lease seam'i — assignment→engine-ready fazları ölçülür); **PER-003** long-session soak — bounded memory/journal büyümesi + compaction + SSE reconnect (bounded lokal pencere); **PER-004** burst trigger/queue — E11 scheduler + E17 T7 queue backpressure/fairness (bounded buffer + depth raporu asserti).
- [ ] **Zorunlu profil damgası:** her koşum `profile.json` (makine/çekirdek/bellek/OS/Docker Desktop sürümü/load şekli) yazmadan sonuç ÜRETEMEZ; ham örnekler JSONL — percentile'lar ham örneklerden türetilir (T10 verifier'ı YENİDEN hesaplar).
- [ ] RED-first: profilsiz koşum reject; kasıtlı-yavaş fake engine fixture'ında threshold gate FAIL; PER-004'te backpressure kapalı fixture → unbounded-buffer asserti FAIL.
- **Seam:** `tests/performance/` (yeni) + compose stack + fake engine. **UAT:** PER-001..004 (harness + gate-mekaniği yarıları; case metinleri T10'da materialize). **Kanıt (burada koşar):** dört harness lokal profille yeşil + refuse/FAIL negative'leri.
- **Honest ceiling:** sayılar macOS + Docker Desktop profilinin sayılarıdır — **SLO/reference-hardware İDDİASI TAŞIMAZ** (§54 hedefleri "product goals"tur, ölçümü §6 leg 3); "soak" bounded-window'dur (dakikalar) ve öyle adlandırılır; gerçek uzun soak §6 leg 7'nin parçasıdır.

### T7 — SEC-102 sandbox-escape suite + SEC-103 audit integrity (mig yok; SECURITY-CRITICAL)

- [ ] **SEC-102:** mevcut SAN corpus'unu (SAN-001..008, 011 negative'leri) TEK escape-suite koşumuna aggregate eden harness — yeni escape sınıfı İCAT EDİLMEZ, mevcut denial'lar suite olarak toplanır + eksik olan **finding→quarantine davranış** testi eklenir (uncertain-failure quarantine seam'i E10/E17 T9'dan); sonuç: "no escape + quarantine works" tek raporda.
- [ ] **SEC-103 (E13-H "audit integrity linkage" burada kapanır):** `events.seq` journal'ı üzerinden rolling-hash integrity — `palai audit verify` sınıfı komut chain'i SATIRLARDAN yeniden hesaplar, **release-signer'la imzalı checkpoint dosyasıyla** karşılaştırır (checkpoint DB DIŞINDA — §1 kararı, migration yok); seq boşluğu → "gap" alert, byte oynaması → "tamper" alert; alert görünür (exit≠0 + typed rapor).
- [ ] RED-first: satır sil → gap FAIL; payload'da 1 byte → tamper FAIL; sağlam journal + doğru checkpoint → yeşil; yanlış checkpoint imzası → fail-closed.
- **Seam:** `tests/uat/` harness + audit verify komutu + runner-verify imza primitive'i. **UAT:** SEC-102, SEC-103. **Kanıt (burada koşar):** iki suite lokal stack'te yeşil + üç-kollu negative.
- **Honest ceiling:** SEC-102 LOKAL OCI seam'idir — microVM/managed high-isolation yolu SaaS planıdır (§64.15'in o maddesi T10'da "managed-scope, not claimed" işaretlenir); kernel-exploit research scope dışıdır — suite DENIAL + quarantine MEKANİĞİNİ kanıtlar. SEC-103 checkpoint cadence'ı operatör politikasıdır; canlı sürekli-doğrulama (§50 operations) §6.

### T8 — Anti-fabrication cross-bundle sweep (mig yok; SECURITY-CRITICAL)

- [ ] Case `checksum`'ının KANONİK YÜZEYİ tanımlanır (bugün yalnız shape-check — `evidence.go:1046`, recompute YOK): checksum = hashParts(kanonik case yüzeyi) ve **recompute mekaniği** `VerifyManifest`'e girer — yüzeyi bundle'da committed olan her case'te checksum YENİDEN hesaplanır.
- [ ] **Sweep: 16 bundle'ın tamamı** yeni verifier'dan geçirilir; `automation-0.1.0` AUT-001'in FABRİKE checksum'ı yakalanır (deferred finding 2) → düzeltme dürüsttür: değer gerçek koşumdan regenerate edilir ya da kanonik yüzeyden yeniden türetilir, düzeltme commit'i neyin neden değiştiğini söyler ve release-index'e not düşülür; sweep başka fabrike değer bulursa aynı işlem.
- [ ] Tarihsel dürüstlük: recompute yüzeyi committed OLMAYAN eski case'ler sessiz geçmez — **"legacy shape-only" explicit label'ı** alır (T10 release index'i bu label'ı taşır; label'sız shape-only = FAIL).
- [ ] RED-first: kasıtlı fabrike-checksum'lı fixture bundle → sweep FAIL; label'sız legacy case → FAIL; düzeltilmiş AUT-001 → yeşil.
- **Seam:** `tests/uat/evidence.go` + `evidence/releases/*`. **UAT:** SEC-103'ün evidence-integrity kardeşi (case'i yok; T10 ReleaseIndexProof'unu besler). **Kanıt (burada koşar):** 16-bundle sweep raporu + AUT-001 fix + negative'ler.
- **Honest ceiling:** eski bundle'ların ham evidence'ı yeniden ÜRETİLMEZ (koşumları tarihseldir) — kanıt, committed yüzey üzerinde recompute + dürüst labeling'dir; "tüm tarih yeniden koşuldu" iddiası yoktur.

### T9 — Security/ops docs + RC triage (mig yok; `docs/security/`, `docs/operations/runbooks/`)

- [ ] `docs/security/threat-model.md`: spec §49'un İMPLEMENTE EDİLMİŞ yüzeye projeksiyonu — her mitigasyon claim'i bir kanıt ID'sine (UAT case / test) bağlanır; aspirational spec kopyası = review reject. `docs/security/vulnerability-process.md`: report/triage/severity (§62.2)/advisory/rebuild — release-policy.md'nin revocation bölümüyle tutarlı, çelişki testi (link'ler + tek policy kaynağı).
- [ ] `docs/operations/runbooks/`: incident-response, key-compromise/revocation, backup-restore, upgrade-rollback, audit-integrity-alert (T7'nin alert'ine oynanır) — mevcut E14/E15 docs'a LINK eden ince runbook'lar (kopya değil); her runbook'un komutları çalışan stack'te bir kez İCRA edilip çıktısı pin'lenir (kağıt-runbook reddi).
- [ ] Release support matrix: `docs/operations/support-matrix.md` (+json guard, E16 sdk-compatibility emsali) — platform × arch × topology; **yalnız test edilmiş hücre dolu**.
- [ ] **RC triage — deferred-findings disposition tablosu** (`docs/operations/known-gaps-1.0.md`; owner sign-off): her satır finding → karar (**RC-blocker / post-1.0 / §6-leg / closed-verified**) + owner + (P2 ise) expiry. Kararlar: modelRoutes+list-envelope → **closed-verified** (E16 T1, `router.go:219-224`); a2a push delivery → **post-1.0** (a2a zaten preview; delivery E17 T2'de dürüst §6 ceiling'i); approval richer detail / publications read endpoint → **post-1.0 hardening** (console dürüst kontrata karşı kanıtlı, UI-002 yeşil); E13-H kalanları → SEC-001/003 KMS ceremony **§6 leg 6**, TEN-004/DAT derinliği/BIL kalemleri satır satır (çoğu SaaS-scope, master plan §9 uyumlu); real-model eval quality → **§6 leg 5 (stable attestation input)**. RC-blocker çıkan satır varsa T10 gate'i onsuz KAPANAMAZ.
- **Seam:** docs + küçük guard testleri. **UAT:** §64.15'in "published security model / support policy / runbooks" maddeleri. **Kanıt (burada koşar):** guard'lar + runbook icra çıktıları + owner-onaylı tablo.
- **Honest ceiling:** threat model implemente-edilmişin modelidir (SaaS/microVM satırları "not claimed"); triage kararları bu OTURUMUN owner'ının kararlarıdır ve tabloda öyle görünür.

### T10 — FİNAL EXIT gate: `release-1.0.0-rc1` cross-epic stable sign-off (mig yok; SECURITY-CRITICAL)

- [ ] `tests/uat/stable-release/` journey: temiz stack → T1 matrisinden build → T4 offline verify (`--network none`) → SEC-101 tamper-negative → **gerçek provider'la TEK adımlık canlı run** (E08 kuralı; release'in gerçek bir run servis ettiğinin canlı çapası) → T6 PER smoke (profilli) → T7 SEC-102/103 → **önceki HER epic bundle'ının re-verify'ı** (16 bundle × strengthened verifier, her biri 0 finding; kendi promote ailesi olan bundle kendi gate'inden geçer).
- [ ] **SEC-101..103 + PER-001..004 case'leri** `tests/uat/cases/` altında TAM materialize; her case metni LOCAL seam'i ve §6 operator bacağını AÇIKÇA adlandırır (E14..E17 emsali).
- [ ] `tests/uat/evidence.go` yeni claim/proof tipleri (Complete() gates): **SupplyChainProof** (index digest'leri + offline-verify + tamper-negative sayaçları — verifier imzayı ve digest zincirini artifact byte'larından YENİDEN doğrular), **PerformanceProfileProof** (ZORUNLU profil + ham-örnek digest'i — verifier percentile'ları HAM örneklerden yeniden hesaplar; profilsiz veya fabrike p95 FAIL), **SandboxEscapeProof** + **AuditIntegrityProof** (T7 negative sayaçları), **ReleaseIndexProof** (master plan Appendix A'daki HER exact UAT ID → taşıyan bundle + outcome + applicability + varsa "legacy shape-only" label'ı — verifier index'i per-bundle manifest'lerden YENİDEN toplar; index'in kendi kopyası kanıt değildir).
- [ ] **Anti-fabrication anchor'ın final formu — AggregateTierProof:** ürün-geneli capability posture'u (stable/preview/disabled) TÜM epiclerin claim outcome'larından YENİDEN hesaplanır (E17 CapabilityTierProof disiplini ürün genelinde) ve koşan stack'in `/v1/capabilities` snapshot'ıyla bit-eşit assert edilir; **fabrike bir cross-epic "stable" FAIL'dir**; tier hesabının kod kaynağı Complete()'te enforce edilen KANONİK kaynaktır, manifest kopyası değil.
- [ ] **§64.15 checklist'i mekanik forma:** 13 madde → claim-ID setleri; managed-only maddeler (production-equivalent cell/microVM, managed high-isolation) release index'te **"managed-scope, not claimed"** olarak DÜRÜST işaretlenir — gate'in çıktısı bir **SH-3 POSTURE raporudur**, blanket "stable" değil. Zero open P0/P1: T9 tablosundan MEKANİK okunur (RC-blocker satırı varsa gate REFUSE).
- [ ] `PromoteGateFor`'a **stable-release ailesi**: `rc` promote lokal seam'le geçer; **`stable` target operator_attestation İSTER** (E15/E16 kalıbı — asla auto-claim) ve attestation §6 leglerini TEK TEK adlandırır (gerçek CI koşumu, gerçek imza kimliği+transparency log, gerçek publish, reference-hardware PER, real-model eval quality, KMS ceremony, önceki epiclerden devralınan açık legler). RED-first: attestation'sız `promote stable` → REFUSE; eksik-leg'li attestation → REFUSE.
- [ ] `make uat-stable-release` + **`evidence/releases/release-1.0.0-rc1` bundle** (redacted manifest; ad DÜRÜSTTÜR — "stable-1.0.0" DEĞİL, çünkü stable flip'i operatörün attested aktıdır) + `make evidence-verify` 0/0/0/0.
- **Exit-gate proof'un evi budur.** **Migration:** yok.
- **Honest ceiling:** bu gate'in lokal kapanışı **RC'dir**; SH-3 Stable, promote gate'inin beklediği operator attestation'ıyla flip olur — bu plan hiçbir koşulda lokal oturumdan "stable yayımlandı" İLAN ETMEZ.

---

## 5. OUT OF SCOPE (bilinçli dışarıda, adres adresine)

| Kalem | Neden dışarıda | Nerede yaşıyor |
|---|---|---|
| Gerçek registry publish (npm/PyPI/Go proxy/GHCR immutable images) | Credential yok; dry-run mekaniği T5'te | §6 leg 2 |
| Gerçek Sigstore/cosign keyless + Rekor transparency log | CI/credential yok; predicate cosign-uyumlu yazılır | §6 leg 1 |
| SLSA Build L3 / hosted build isolation | §51.2: L2 initial-stable hedefi, L3 managed | SaaS planı |
| Extension marketplace (§51.5) | "If offered" — sunulmuyor | SaaS/sonrası |
| Managed SLO/SLA ölçümü + error budget (§54) | Targets "product goals"; SLA ticari terim işi | SaaS planı |
| Reference-hardware performance publication | Donanım yok; harness parametrik | §6 leg 3 |
| KMS-backed master key ceremony (E13-H SEC-001/003) | Gerçek KMS yok; file-seam DR-005'te fail-closed kanıtlı | §6 leg 6 + T9 posture satırı |
| a2a push delivery + `/v1/publications` read endpoint / richer approval detail | Preview-capability gap'leri; RC-blocker değil (T9 karar tablosu, owner onaylı) | post-1.0 hardening |
| Real-model agentic eval koşumu (quality numbers) | E08 kuralı: engine gerçek provider'a tool açmaz | §6 leg 5 → stable attestation input |
| E13-H kalanı: TEN-004, DAT-001..005 derinliği, DAT-006 kalanı, BIL-002/004/005, OTel redaction derinliği | T9'da satır satır disposition; çoğu SaaS-scope (master plan §9) | T9 tablosu + SaaS planı |
| Operator console UI (§47.4) | E17 kararı yürürlükte: E15 ops CLI'ları karşılar | Talep gelirse post-1.0 |
| WAL archiving / streaming replica (RPO→0) | Tek-node drill dışı (E15 dr-report notu) | §6 operatör altyapısı |

## 6. Operator legs — gerçek-altyapı bacağı (deferred-but-scripted; kaybolmaz)

Her biri için KOD/harness bu fazda hazır ve parametriktir; İCRA operator-provided credential/altyapı/karar ister. **`release-1.0.0` STABLE promote'u bu leglerin attestation'ına bağlıdır ve attestation T10 promote gate'inden geçer (asla auto-claim):**

1. **Gerçek CI release koşumu** — GitHub protected environment + iki maintainer + ephemeral runner'da `release.yml`; gerçek workflow-identity'li provenance (builder alanı CI kimliğiyle dolar) + gerçek Sigstore/cosign keyless veya org-KMS imzası + transparency-log entry.
2. **Gerçek registry publish** — npm/PyPI publish, Go module tag, GHCR immutable image'lar (E16 §6 leg 3 devri); dependency-confusion namespace politikasının registry'de doğrulanması.
3. **Reference hardware/load profile'da PER-001..004** — published numbers (§54 hedeflerine karşı); amd64 topolojisinde TAM UAT (T1 qemu-smoke'un üstü).
4. **Gerçek air-gap facility drill'i** — T4'ün fail-closed verifier'ıyla operatör trust-root ceremony'si (E15 §6 devri).
5. **Real-model eval quality koşumu** (E17 §6 leg 7 devri) — quality numbers; stable attestation'ın INPUT'u.
6. **KMS-backed master key ceremony** (E13-H SEC-001/003) — file-seam'in üstü.
7. **Gerçek RC soak** — hedef topolojide uzun pencere (§64.15 "RC soak"un tam formu; lokal bounded soak T6/T10'dadır).
8. **Devralınan tüm açık epic legleri** (E14 cloud-VM install/separate-host restore, E16 LiteLLM/registry, E17 Slack/A2A/Apple/pgvector/SQS/Temporal/console-a11y) — stable attestation her birinin DİSPOZİSYONUNU adlandırır: icra edildi ya da bilinçli preview/disabled kaldı.

## 7. Master plan §8 için önerilen özet blok (owner paste eder)

**UAT ownership:** SEC-101..103 (SEC-101 tamper-denial T4; SEC-102 escape-suite aggregation + quarantine T7; SEC-103 audit tamper/gap alert T7); PER-001..004 (harness + gate mekaniği + ZORUNLU hardware/load profile — lokal sayılar macOS+Docker-Desktop profillidir, published numbers §6); OPS/DR regression (16 committed bundle'ın strengthened-verifier re-verify'ı + deterministic suite'lerin yeniden koşumu); §64.15 stable-release gate'in mekanik formu — tamamı `tests/uat/cases/` altında materialize, kanıt seam'i case metninde adlandırılır.

**Exit gate — cross-epic stable sign-off (E18'in ve programın tanımlayıcı kuralı):** `release-1.0.0-rc1` bundle'ı önceki HER epic bundle'ını yeniden doğrular (0/0/0/0), master plan Appendix A'daki her exact UAT ID'yi taşıyan-bundle+outcome ile indexler (verifier index'i per-bundle manifest'lerden YENİDEN toplar) ve ürün-geneli stable/preview/disabled posture'unu claim outcome'larından YENİDEN hesaplayıp koşan stack'in `/v1/capabilities`'iyle bit-eşit assert eder — **fabrike bir cross-epic "stable" FAIL'dir**. Pinned hermetic build (binary-repro, digest-pinned base'ler, amd64+arm64 matrisi), SBOM+vuln-scan, in-toto/SLSA-shaped provenance ve openssl imzası `--network none`'da offline verify edilir; tamper matrisi execution+promotion'ı reddeder (SEC-101); airgap verifier'ın fail-open fallback'i kapanmıştır; automation-0.1.0 AUT-001'in fabrike checksum'ı sweep'te yakalanıp düzeltilmiştir ve evidence checksum'ları artık recompute edilir (legacy'ler explicit label'lı). Zero open P0/P1 triage tablosundan mekanik okunur. **Bu gate'in lokal kapanışı RC'dir: "stable yayımlandı / gerçek CI'da imzalandı / transparency log'da / reference donanımda ölçüldü" İDDİA ETMEZ** — o bacaklar §6 operator legleridir ve `stable` promote'u, bu legleri TEK TEK adlandıran operator_attestation olmadan promote gate tarafından REFUSE edilir. SH-3 Stable = o attestation'ın kendisi.
