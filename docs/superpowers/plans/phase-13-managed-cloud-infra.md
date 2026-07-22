# Palai Managed-Cloud Infrastructure Completeness Plan (E13 — reshaped)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (önerilen) veya superpowers:executing-plans ile task-by-task uygula. Adımlar `- [ ]` checkbox'lıdır. RLS policy / pgx per-txn GUC / cursor-pagination idiom'ları brief'lerinde Context7/spec grounding alır (repo politikası, ledger 2026-07-17).

**Goal:** "Managed agent over cloud + web SDK" vizyonunun SaaS-OLMAYAN altyapı boşluklarını TEK tutarlı fazda kapatmak: RLS + tenant provisioning, restart'sız secret-ref write-path, temel durable budget/quota (metering, faturalama DEĞİL), read/LIST API yüzeyi, artifact retrieval, §20.12 basic edge admission, DB-backed model-routing reader (E06 carve-out'unun ödenmesi), `projects.config_policy` write-path ve @palai/sdk core-parity. Gap-analysis kaynağı: 3-boyutlu audit (SDK/client, cloud-config, lifecycle) — owner'ın INCLUDE/DROP filtresi bu planda aynen uygulanır; tek ekleme, filtrenin kendisinin işaret ettiği opsiyonel `repository_bindings.connection_ref` seam'idir (T9).

**Kapsam sınırı (owner direktifi):** SaaS-katmanı, product-UI ve §2.2 kalemleri BİLİNÇLİ dışarıda — §5'te tek tek adlandırılır. Bu faz web-console'u (E17c) İNŞA ETMEZ; onu inşa EDİLEBİLİR kılar.

---

## 1. Yapı kararı — yeni epic numarası DEĞİL, E13'ün reshape'i

**KARAR: mevcut E13 slot'u bu faz OLUR.** Başlık "E13 — Managed-Cloud Infrastructure Completeness"e döner; child plan bu dosyadır (`phase-13-managed-cloud-infra.md`; eski `phase-13-governance-data.md` hiç yazılmadı → sıfır churn).

Gerekçe:

1. **Milestone slotu zaten doğru:** master plan §7'de M8 = "Governance & data safety → production security gate" ve M9 (E14) M8'e bağımlı. Bu fazın çekirdeği (RLS, provisioning, secret, usage) TAM o slottur; API-completeness kalemleri aynı kapının "managed-cloud" yarısıdır.
2. **Downstream pointer'lar E13'e bakıyor:** E14 admin CLI'ı "E13 API'leri üzerine ince yüzey" (master plan satır 497), E17c console "yalnızca public API" der. Yeni bir epic numarası (E13.5/E19) bu pointer'ların hepsini kırar; reshape hiçbirini kırmaz.
3. **İçerik kesişimi zaten %60:** INCLUDE listesinin 1-2-3-8 kalemleri E13'ün orijinal maddeleridir. Kalan E13 maddeleri (envelope/KMS ceremony, audit linkage, deep DAT, OTel redaction) gate'i BLOKLAMAYAN **E13-H hardening tranche** olarak aynı epic içinde ayrılır (§6) — UAT sahipliği master plandan kopmaz.

**Master plan amendment (owner'ın yapacağı tek edit):** §8/E13 bloğunda child-plan pointer'ı bu dosyaya çevrilir, başlık + checkbox listesi §7'deki task özetiyle değiştirilir, UAT ownership satırı §7/§6'ya göre bölünür (gate vs hardening). Bu doküman read-only görevde yazıldığından master plan burada DEĞİŞTİRİLMEMİŞTİR.

**Fork noktası + numaralandırma:** Bu faz **E12 T10 kapanıp `main`'e merge olduktan sonra** forklanır (execution gate: `main` >= E12 merge tip). Migration'lar **000029'dan itibaren sıralı** (E12'nin son migration'ı 000028); paralel task'lar numara çakışmasın diye §4'teki sabit atamayı kullanır.

**Files:** `apps/control-plane/api/`, `apps/control-plane/internal/identity/` (yeni), `apps/control-plane/internal/execution/`, `storage/migrations/`, `sdks/typescript/`, `tests/security/tenancy/`, `tests/uat/managed-cloud/`

---

## 2. Design invariant (task değil, her task'ın kabul şartı)

**Self-host = altyapı; SaaS = üstüne binen katman.** Bu fazın açtığı HER yüzey, ileride ticari bir SaaS'ın çekirdeğe COUPLING OLMADAN üstüne binebileceği şekilde açılır:

- Her endpoint `/v1` + `API-Version` header disiplini + RFC9457 problem-details altındadır; tenant identity HER ZAMAN doğrulanmış API key'den gelir, asla body'den (mevcut auth middleware kalıbı).
- Her liste cursor'ı **opaque + tenant-bound**'dur (cursor başka tenant'a taşınamaz — TEN-001 fuzz yüzeyi).
- Usage ledger **append-only + versioned şema**dır: harici bir billing exporter'ın (Stripe/OpenMeter, SaaS planı) yalnızca ledger'ı OKUYARAK faturalayabileceği kadar öz-yeterli — çekirdek hiçbir billing kavramı öğrenmez.
- RLS + provisioning API, "tek kurulum çok organization" (§2.3 yorumu) sınırında kalır; hostile public multi-tenancy İDDİA EDİLMEZ (`/v1/capabilities` bunu söylemeye devam eder).

---

## 3. Doğrulanmış seam envanteri (2026-07-22, ağaca karşı)

| Seam | Durum |
|---|---|
| RLS | `storage/migrations/`'da SIFIR `ROW LEVEL SECURITY` hit'i; izolasyon tamamen app-level WHERE'lerde |
| Tenancy routes | `apps/control-plane/api/router.go`: org/project/apikey/secret route'u YOK; seed tek sefer `internal/store/bootstrap.go` |
| api_keys | 000001'de `revoked_at` VAR; `scope`/`expires_at` kolonu YOK → T2 migration gerektirir |
| model_routes ailesi | 000001'den beri tablolar var; Go'da yalnızca YORUMLARDA geçiyor (`main.go:263` ponytail notu, `model_dispatch.go:37`) — reader-less, E06 §7.3 carve-out |
| Secrets | Tümü env-file köprüsü `PALAI_*_SECRET_FILE_<ORG>__<REF>` (main.go); secret_refs tablosu/write-API yok → rotate = restart |
| config resolver | `internal/execution/config.go` deployment<project<agent_revision<session; `projects.config_policy` OKUNUYOR ama hiçbir API YAZMIYOR |
| Artifacts | `artifacts` tablosu 000001'de, write-path E09'da; router'da SIFIR artifact route'u |
| LIST yüzeyi | Yalnız `GET /v1/skills`, webhook-endpoints/-deliveries, schedule occurrences; runs/sessions/agents/tools listesi YOK |
| Rate limit | Router'da hiçbir edge rate-limit middleware'i yok; §20.12 hiç başlatılmamış |
| connection_ref | `000009_repository_bindings`'te kolon var (`DEFAULT ''`), `repository.go` TÜKETMİYOR — global GitHub App |
| usage_events | 000001'den beri var (settlement kaynağı hazır); budget/quota tablosu yok |

---

## 4. Task breakdown

Sıra bağımlılığa göredir; T3/T4/T5/T6/T7 kendi aralarında paralellenebilir (migration numaraları sabitlendi).

### T1 — RLS spine + verified tenant context (mig **000029**)

- [ ] Tenant-scoped TÜM tablolara `ENABLE + FORCE ROW LEVEL SECURITY` + org (ve varsa project) policy; app bağlantısı için tablo-sahibi OLMAYAN yeni runtime DB rolü (compose + store pool değişir).
- [ ] Doğrulanmış tenant context: auth middleware'in çözdüğü org/project, pgx üzerinden per-transaction GUC (`palai.org_id`) olarak set edilir; GUC set edilmemiş bağlantı HİÇBİR tenant satırı göremez.
- [ ] Cross-tenant negative corpus `tests/security/tenancy/`: kasıtlı WHERE-suz sorgu fixture'ı DB tarafından reddedilir (TEN-002'nin özü).
- **Seam:** `internal/store/` pool + middleware/auth. **UAT:** TEN-001 (fuzz temeli), TEN-002. **Live-smoke:** iki org'lu stack'te org-A key'i ile org-B run'ı 404 + DB-level deny kanıtı.
- **Honest ceiling:** tek DB, tek runtime rolü — hostile-DBA/şifreli-at-rest iddiası yok (E13-H/E15).

### T2 — Tenancy provisioning API + project policy write (mig **000030**: api_keys `scope`/`expires_at`)

- [ ] `POST|GET|LIST /v1/organizations`, `/v1/projects`, `/v1/api-keys` (+ `POST .../revoke`); key yalnız creation response'ta plaintext, DB'de hash (mevcut kalıp); scope/expiry enforce.
- [ ] `PATCH /v1/projects/{id}` ile `config_policy` write-path (strict şema, unknown-field reject — E11 T1 decode kalıbı): §14 resolver'ının project katmanı ilk kez API-erişilir olur.
- [ ] `bootstrap.go` seed'i "ilk org + ilk admin key" bootstrap'ına daralır; ikinci tenant DOĞRUDAN API ile açılır.
- **Seam:** `api/router.go` + yeni `internal/identity/`. **UAT:** TEN-003; MCI-001 (2. tenant restart'sız provisioning). **Live-smoke:** API ile açılan taze org→project→key, resolver'da görünen config_policy ile gerçek run koşturur.
- **Honest ceiling:** basic scope'lar; roles/relationships/OIDC yok (E13-H/E17).

### T3 — Secret-ref write-path, restart'sız (mig **000031**: `secret_refs`)

- [ ] `POST /v1/secret-refs` (write-only value) + `GET/LIST` (yalnız metadata: name/version/updated_at) + rotate (yeni version insert).
- [ ] Resolver zinciri: mevcut env-file köprüsünün ÖNÜNE DB-backed secret store; ref bulunamazsa env köprüsü fallback (E09 credential-broker seam'i korunur). Value asla response/log/event'e çıkmaz (mevcut secret-scan gate'i yüzeyi kapsar).
- **Seam:** `main.go` secret-bridge bloğu + broker resolver. **UAT:** SEC-002 (rotation-without-restart subset'i); MCI-002. **Live-smoke:** çalışan stack'te API ile MCP secret'ı ekle → restart YOK → run secret'ı kullanır.
- **Honest ceiling:** at-rest koruması tek master-key AES-GCM envelope; KMS backend + one-operation audience/fence lease ceremony E13-H'dedir (SEC-001/003 oraya).

### T4 — Read/LIST API (migration yok)

- [ ] `GET+LIST`: `/v1/responses` (run listesi), `/v1/sessions`, `/v1/agents`(+revisions), `/v1/tools`+`/v1/tool-sets`, `/v1/mcp-connections`, `/v1/repository-bindings`, `/v1/triggers`. Contracts'taki mevcut list/pagination tipleri İLK kez tüketilir.
- [ ] Opaque tenant-bound cursor + temel filtreler (status, created_at aralığı) — filtre DSL'i YOK.
- **Seam:** `api/` handler'ları + store read'leri (RLS altında doğar). **UAT:** MCI-003; TEN-001'in cursor-fuzz yarısı burada canlanır. **Live-smoke:** SDK'sız curl ile bir tenant'ın run geçmişi listelenir; başka tenant cursor'ı reddedilir.
- **Honest ceiling:** search/FTS yok (E17 knowledge); webhook/schedule listeleri zaten var, sadece boşluklar kapanır.

### T5 — Artifact retrieval API (migration gerekirse **000032**: eksik §22.6 metadata kolonları)

- [ ] `GET /v1/artifacts/{id}` (metadata) + `GET /v1/artifacts/{id}/content` (object-store'dan authenticated streaming download) + `GET /v1/responses/{id}/artifacts` (run-scoped liste). E09 write-path'inin hiç açılmamış read-path'i.
- [ ] Yanlış tenant/artifact → 404 (existence disclosure sıfır); Content-Digest header ile byte-bütünlük.
- **Seam:** `api/` + E09 object-store adapter'ı. **UAT:** DAT-006'nın basic yarısı (yanlış tenant deny); MCI-004. **Live-smoke:** coding run'ın ürettiği dosya SDK ile indirilir, checksum workspace'tekiyle bit-identical.
- **Honest ceiling:** authenticated direct download; pre-signed URL policy + expiry ceremony'si E13-H (DAT-006'nın kalanı).

### T6 — Usage ledger + durable budget/quota (mig **000033**: `usage_ledger`, `budgets`, `quotas`)

- [ ] Mevcut `usage_events`'ten beslenen append-only, versioned `usage_ledger` (deterministic ledger ID → replay'de double-settlement yok).
- [ ] `POST|GET /v1/budgets` + `/v1/quotas` (org/project scoped): basic reservation→settlement; admission budget'ı aşan run'ı reddeder, quota dolan tenant'a stable remediation body'si döner.
- [ ] `GET /v1/usage` (tenant-scoped özet + ledger sayfası) — metering görünürlüğü.
- **Seam:** admission (`api/responses.go` Admitter) + model settlement yolu. **UAT:** BIL-001, BIL-003 (basic reservation), QUO-001. **Live-smoke:** düşük budget'lı proje 2. run'da reddedilir; ledger toplamı provider usage ile uyuşur.
- **Honest ceiling:** metering-only — invoice/adjustment/BYOK ayrımı/exporter YOK (BIL-004/005/006 → E13-H/SaaS).

### T7 — API-edge admission control / rate limit (§20.12 basic tier; migration yok)

- [ ] Per-API-key request-rate (in-process token bucket), per-project concurrent-run cap + queued-run bound (DB sayaçları admission'da zaten okunabilir durumda).
- [ ] Aşımda 429 + `Retry-After` + RFC9457; queue-deadline dolan run `timed_out` (billable compute başlamadan) — §20.12 cümlesi bire bir.
- **Seam:** `api/` top-level middleware + Admitter. **UAT:** MCI-005; QUO-001 ile sınır paylaşımı (quota=durable, rate=anlık — ikisi ayrı test edilir). **Live-smoke:** burst script 429 alır, tek run kaybı/duplicate olmadan drain olur.
- **Honest ceiling:** `// ponytail:` in-process bucket — tek-replica compose için doğru; multi-replica distributed limiter + weighted fairness (QUO-002) SaaS scope.

### T8 — DB-backed model-routing reader (E06 §7.3 carve-out'un ödenmesi; migration yok — tablolar 000001'de)

- [ ] `model_routes`/`model_route_revisions`/`model_connections` İLK okuyucusu: `dispatchModel` per-project route resolution (model id + connection credential'ı DB'den, credential secret-ref T3 üzerinden).
- [ ] `POST /v1/model-connections` + `/v1/model-routes`(+revisions/publish) write yüzeyi (E11 revision kalıbı); env route (`PALAI_MODEL_PROVIDER/MODEL`) deployment-default FALLBACK'e iner — sökülür değil, en alt katman olur.
- **Seam:** `internal/execution/model_dispatch.go:37` + `main.go:263` env bloğu + `config.go` resolver'ın deployment katmanı. **UAT:** MOD-004'ün routing yarısı; MCI-006 (iki projenin farklı model/credential'la aynı stack'te koşması). **Live-smoke:** proje-A ve proje-B farklı model id + farklı API key ile gerçek provider'a çıkar.
- **Honest ceiling:** TEK provider ailesi (provider-one) — 2. bağımsız adapter + capability probe E16'dır; bu task yalnız "hangi model + hangi credential" seçimini per-project yapar.

### T9 — repository_bindings.connection_ref resolver seam (opsiyonel include, owner filtresi)

- [ ] `repository.go` Git credential çözümünde binding'in `connection_ref`'i doluysa secret-ref (T3) üzerinden token/App credential'ı; boşsa mevcut global GitHub App fallback.
- **Seam:** `internal/execution/repository.go` + 000009 kolonu. **UAT:** MCI-007 (binding-scoped credential ile clone; ref'siz binding eski yoldan). **Honest ceiling:** per-tenant GitHub App ONBOARDING yüzeyi yok (ürün işi, SaaS) — yalnız resolver seam'i tüketilir.

### T10 — @palai/sdk core-parity (TS)

- [ ] SDK'ya: `sessions` (create/get/list + `commands` steer/interrupt — E08'in ürünü ilk kez SDK'dan kullanılır), `agents` (+revisions/publish), `artifacts` (metadata + download stream), tüm T4 list yüzeyleri, `secretRefs`/`modelRoutes`/tenancy admin ince client'ları.
- [ ] Server-only credential guard AYNEN korunur; `examples/nextjs-sdk` relay örneğine steer + artifact download eklenir ve **server-only stance docs'a yazılır** (browser-direct token DROP kararının pozitif dokümantasyonu).
- **Seam:** `sdks/typescript/` (mevcut Responses kalıbı çoğaltılır). **UAT:** MCI-008; API-013 regresyonu yeşil kalır. **Honest ceiling:** Py/Go SDK parity E16.

### T11 — Managed-cloud journey + evidence gate

- [ ] Journey UAT `tests/uat/managed-cloud/`: API ile tenant provision → secret-ref → model route + config_policy → SDK session+run → steer → run listele → artifact indir → budget/rate sınırı kanıtı → cross-tenant negative → hepsi restart'sız.
- [ ] `make uat-managed-cloud` + `managed-cloud-0.1.0` evidence bundle (redacted manifest, LP kalıbı); MCI-001..008 case'leri `tests/uat/cases/` altında materialize edilir (§64 katalog-dışı authored kalıp, E09/E12 emsali).
- **Exit-gate proof'un evi budur.**

---

## 5. OUT OF SCOPE — SaaS-layer / deferred (owner DROP filtresi, bire bir)

| Kalem | Neden dışarıda | Nerede yaşıyor |
|---|---|---|
| Browser-direct token + CORS | Server-relay altyapısal olarak doğru kalıp; SDK guard + Next.js relay bunu zaten kanıtlıyor — T10 stance'ı dokümante eder | SaaS planı (gerekirse) |
| Per-tenant GitHub App onboarding | Ürün akışı; self-host için tek global App yeter — yalnız `connection_ref` resolver seam'i alındı (T9) | SaaS planı |
| Pooled-fairness / noisy-neighbor (QUO-002) | Weighted fairness hostile multi-tenancy işidir | §2.2 SaaS |
| Stripe/invoice/billing export (BIL-004..006 ticari yarısı) | Ledger metering'i yeter; faturalama üst katman | SaaS planı (ledger'ı okur) |
| Managed regional cell / hostile microVM fleet (SAN-009) | §2.2 açık istisna | SaaS planı |
| DR-003 regional failover | Managed SLA ürünü | SaaS planı |
| Web-console UI | Bu faz onu API'siyle İNŞA EDİLEBİLİR kılar, inşa etmez | E17c |
| Full envelope/KMS ceremony + JIT lease (SEC-001/003) | Gate T3 ceiling'iyle geçer; ceremony hardening'dir | E13-H (§6) |
| Py/Go SDK, 2. provider adapter + probe | Parity işi | E16 |
| Roles/relationships/OIDC | Basic scope yeter; console gelince anlamlanır | E13-H → E17 |

## 6. E13-H hardening tranche (aynı epic, gate'i BLOKLAMAZ; E14/E15 penceresinde koşar)

Master plan E13'ünün UAT sahipliğinden gate'e alınmayanlar burada korunur — kaybolmaz: **SEC-001, SEC-003** (KMS/lease ceremony), **TEN-004** (end-user token exchange), **DAT-001..005** (deletion/legal-hold/export/residency derinliği), **DAT-006 kalanı** (pre-signed URL policy), **BIL-002/004/005**, audit integrity linkage, content-free OTel + redaction scanners. SH-2 (RC) iddiasından önce kapanmaları gerekir; bu fazın exit gate'inden önce DEĞİL. (TEN-005 zaten SaaS scope — master plan §9 notu geçerli.)

## 7. Master plan §8 için önerilen özet blok (owner paste eder)

**UAT ownership (gate):** TEN-001..003; SEC-002 (rotation subset); DAT-006 (basic deny); BIL-001, BIL-003; QUO-001; MOD-004 (routing yarısı); MCI-001..008. **(E13-H):** §6 listesi.

**Exit gate — "managed-agent-over-cloud infra-complete":** Bir web SDK (server-relay) — API ile provision edilmiş, restart'sız secret'lı, kendi model route'u ve config_policy'si olan bir tenant'ta — run yaratır, steer/interrupt eder, run/session/agent listeler ve artifact indirir; tamamı RLS ile tenant-izole (negative corpus DB-level yeşil), edge'de rate-limited, budget/quota-metered ve versioned `/v1` public API'den geçer; `managed-cloud-0.1.0` evidence verifier yeşildir. Bu gate geçmeden "managed cloud hazır" ifadesi kullanılmaz; hostile public multi-tenancy bu gate'te de İDDİA EDİLMEZ.
