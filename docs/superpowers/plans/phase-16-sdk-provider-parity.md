# Palai SDK Parity ve Provider Completeness Plan (E16)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (önerilen) veya superpowers:executing-plans ile task-by-task uygula. Adımlar `- [ ]` checkbox'lıdır. httpx sync/async idiomları (Py SDK), Anthropic Messages API streaming, npm pack / python wheel build adımları brief'lerinde Context7/spec grounding alır (repo politikası, ledger 2026-07-17).

**Goal:** Tek SDK'lı (TS) platformu **üç-dilli, provider-tamamlanmış** hale getirmek: Python (sync+async) ve Go SDK'ları TS parity'sinde sıfırdan; TEK paylaşılan fixture corpus'u + üç dil runner'ı üzerinden **MEKANİK** cross-language equality (API-012 — el yazması üç ayrı suite DEĞİL); ikinci bağımsız direct provider ailesi + aktif capability probe'lu private/OpenAI-compatible adapter (MOD-001/002); retry/fallback/cancel/partial/cache/usage/circuit/budget runtime conformance'ının iki provider ailesiyle tamamlanması (MOD-003..012); paket provenance/checksums/changelog/compatibility matrix. Exit gate: **aynı fixture üç dilde semantic eşit; gateway kapatıldığında direct paths çalışır** — ikisi de T8'de mekanik kanıta bağlanır.

**Kapsam sınırı — DÜRÜST TAVAN (E14 §7 / E15 geleneğinin devamı):** Bu plan macOS + Docker Desktop oturumunda kod-subagent'larıyla İCRA EDİLİR. `.env.local`'da **iki gerçek provider credential'ı vardır** (`OPENAI_API_KEY` + `ANTROPHIC_API_KEY` — değişken adı dosyadaki haliyle source edilir, asla argv/log/evidence/commit), yani ikinci-provider live smoke GERÇEKTİR, fake değil. Buna karşılık: (a) engine gerçek provider'a TOOL AÇMAZ (E08 kuralı — gerçek run'lar tek adımdır); tool-path conformance'ı deterministic wire-fixture seviyesindedir, artı broker-seviyesi tek tool-call smoke; (b) LiteLLM enhanced adapter İNŞA EDİLMEZ (spec §27.8'de opsiyonel) — "gateway" kanıtı OpenAI-compatible adapter + local stand-in proxy iledir ve öyle ADLANDIRILIR; gerçek LiteLLM instance'ı §6 operator legidir; (c) public registry publish (npm/PyPI/Go proxy) YOKTUR — E18 supply-chain işidir; "provenance" burada manifest + imza + build-input kaydıdır.

---

## 1. Yapı kararı — fork noktası, numaralandırma, dosyalar

**Fork noktası:** E15 kapanışı (T6 `self-host-0.2.0` bundle + promote gate) `main`'e merge olduktan sonra; execution gate: `main` >= E15 T6 merge tip.

**Migration:** Zincir **000034**'te (E15 T1 contract örneği). **Bu fazın HER task'ı migration-free'dir** — `model_routes`/`model_route_revisions`/`model_connections` tabloları 000001'den beri var, read-back API şema istemez; capability probe sonucu in-process cache'tir (aşağıda `ponytail:` notu). Öngörülemeyen ihtiyaç → ilk boş numara **000035**, önce owner onayı; o durumda guarded + idempotent + `storage/embed.go` concat + append-only'ye self-re-asserting REVOKE + `make generate` kuralları aynen. Contracts/generated-type dokunuşu (usage cache field'ları, model-route projeksiyonları) şema DEĞİLDİR ama **`make generate` zorunludur** (TS `generated/types.ts` emsali).

**modelRoutes write-only + list-envelope gap'inin kararı (E13 T10 bayrağı):** İKİ gap de GERÇEK ve İKİSİ DE bu fazda kapanır, SDK task'ı içinde değil **ayrı bir API-completeness task'ında (T1)**: (a) sunucu `model-connections/routes/revisions` için GET/LIST kazanır (`router.go:213-216` bugün POST-only); (b) list-envelope AYRIMI (cursor'lu `Page` = data-plane, cursor'suz `ListView` = küçük full-set admin — `shared.ts:41-53`) BİRLEŞTİRİLMEZ, **canonical İLAN EDİLİR**: generated contracts iki envelope'u da tanımlar, model-route read'leri admin ailesi olarak ListView alır, corpus iki envelope'un decode'unu üç dilde test eder. Birleştirme reddi gerekçesi: mevcut provisioning/secret-ref admin API'si yayında ve API-013 regression yüzeyi; churn'ün getirisi yok.

**Files:** `sdks/typescript/`, `sdks/python/` (yeni), `sdks/go/` (yeni), `adapters/models/`, `tests/conformance/sdk/` (yeni), `tests/conformance/models/` (E06'dan var — GENİŞLETİLİR, yeniden yazılmaz) — master plan §E16 bire bir; artı `apps/control-plane/api/` (T1 read-back), `scripts/release/` (T7 paketleme, E15'in açtığı dizin), `tests/uat/sdk-parity/` (T8 journey evi; master plan file listesine ek, owner §7 bloğunu paste ederken görür).

---

## 2. Design invariant (task değil, her task'ın kabul şartı)

- **Parity iddiası TEK yerde ölçülür:** hiçbir SDK kendi suite'inde "TS ile aynı" ASSERT ETMEZ. Her dil runner'ı vektör başına NORMALİZE JSON çıktı üretir; `tests/conformance/sdk/` harness'ı üç çıktıyı canonical-bytes diff'ler. El yazması per-dil beklenti dosyası = review reject (polyglot drift, master plan §11).
- **Tek retry sahibi (§27.9, §53.4):** adapter'larda hidden retry YOK (provider_one emsali: net/http, tek deneme); SDK retry'ı yalnız transport seviyesinde ve Idempotency-Key ile (API-013); üç dilde de PROVIDER SDK'SI YOK, plain HTTP+SSE (yeni bağımlılık tavanı: Py'de yalnız httpx).
- **Credential asla environment dışına çıkmaz:** live smoke `set -a` + `.env.local`; secret-scan gate'i SDK test çıktıları + evidence'ı kapsar; adapter credential'ı tek Authorization header kullanımı (provider_one disiplini).
- **Generated types tek kaynak:** üç SDK'nın tip yüzeyi `make generate` çıktısından gelir; elde kopyalanmış tip drift'tir. `model-routes.ts`'in open `[key: string]: unknown` interface'leri T1 ile generated projeksiyona döner.
- **Server-relay stance korunur:** TS browser guard AYNEN (E13 T10); Py/Go server-side dillerdir — browser story İDDİA EDİLMEZ, her SDK README'si bunu pozitif yazar.
- **Capability dürüstlüğü:** desteklenmeyen özellik capability record'da `false/unknown` olarak durur ve admission run ÖNCESİ fail eder (§27.5); "stale" label'sız eski probe sonucu kullanılamaz.

---

## 3. Doğrulanmış seam envanteri (2026-07-23, ağaca karşı)

| Seam | Durum |
|---|---|
| `sdks/typescript/` | @palai/sdk tam resource seti (responses/sessions/agents/artifacts/reads/provisioning/secret-refs/model-routes + shared.ts), 41 test; `model-routes.ts:39` "write-only by design" yorumu; tipler kısmen open interface (canonical şema yok) |
| Server model-routing | `router.go:213-216` yalnız 4 POST route'u — GET/LIST YOK (gap doğrulandı) |
| List envelope | `shared.ts:41-53`: `Page<T>` (cursor, contracts.Page) vs `ListView<T>` ({object:"list"}) ayrımı yaşıyor |
| `adapters/models/` | `fake` (scripted) + `provider_one` (OpenAI ChatCompletions; SDK'sız, no-retry, 1 MiB SSE tavanı, Idempotency-Key); registry `main.go:397` `map[string]ModelAdapter{"provider-one": …}` — ikinci aile = map entry |
| `tests/conformance/` | `contracts/` (E02 corpus), `models/` (fake-driven deterministic + live tier, `provider_one_test.go`), `tool-sdk/` (**üç-dil corpus EMSALİ**: TS/Py/Go AYNI JSON fixture'ları — TOL-018), `api/`; `sdk/` YOK |
| Python toolchain | `engines/reference/` pytest + `palai_tool_sdk.py` (Py conformance bacağı emsali); `sdks/python` YOK |
| Go SDK | `sdks/go` YOK |
| Credentials | `.env.local`: `OPENAI_API_KEY` + `ANTROPHIC_API_KEY` mevcut → iki GERÇEK provider live-smoke edilebilir |
| API-015 sunucu yarısı | `responses.go:343-401`: purged replay = 410 tombstone, no re-execution, no content disclosure — VAR; SDK'larda typed yüzeyi yalnız TS'te |
| UAT cases | `tests/uat/cases/` altında SIFIR MOD-* / API-012..015 materialization |
| Migrations | Zincir 000034; model_routes ailesi 000001'den beri; `make generate` = `scripts/contracts/generate` |
| E13 T6 ledger | `usage_ledger` + budget reservation/settlement canlı — MOD-011 conformance'ının tüketeceği seam |

---

## 4. Task breakdown

**DAG:** Wave 1 (paralel, cap 3): **T1, T2, T5** — üç ayrık seam (api/ + TS SDK; tests/conformance/sdk corpus; adapters/models). Wave 2 (paralel, cap 3): **T3** (Py SDK — T1 yüzeyi + T2 corpus'u tüketir), **T4** (Go SDK — aynı), **T6** (runtime conformance — T5 adapter'larını sürer). T3 ve T4 ikisi de `tests/conformance/sdk/`'ye runner kaydeder; entegrasyon merge sırası SABİT: önce T3, sonra T4 (E14 T3→T4 emsali). Wave 3: **T7** (T3/T4 paketlenecek paketleri ister). Wave 4: **T8** (hepsine bağlı, exit-gate evi). Her paralel merge sonrası `go vet -tags="component live" ./...`. Her task RED-first TDD + green milestone başına commit; provider dokunan her task'ta real-provider live smoke `set -a` + `.env.local`.

### T1 — Model-routing read-back API + envelope formalization + TS SDK binding (migration: yok)

- [ ] `GET|LIST /v1/model-connections`, `/v1/model-routes`, `/v1/model-routes/{id}/revisions` (+tekil GET'ler): admin ailesi → **ListView** envelope, `provision` capability şartı, tenant-scoped (RLS altında doğar); secret_ref YALNIZ ref adı olarak döner, value asla.
- [ ] `ModelConnection/ModelRoute/ModelRouteRevision` canonical projeksiyonları generated contracts'a (`make generate`); OpenAPI'de **Page + ListView iki envelope'un da** ilanı — ayrım artık kaza değil sözleşme.
- [ ] TS SDK: `ModelRoutes`'a `list/get` + generated tipler; `model-routes.ts:39` write-only yorumu emekli edilir (dürüst adlandırma: yorum yeni gerçeği söyler).
- [ ] API-015 sunucu davranışı conformance/api'de pinlenir (RED-first: 410 tombstone + no re-execution + no content disclosure asserti — davranış var, testi yok).
- **Seam:** `api/router.go` + model-routing handler'ları + `scripts/contracts/generate` + `sdks/typescript/src/resources/model-routes.ts`. **UAT:** API-015 (sunucu yarısı); MCI-006 regression yeşil kalır. **Migration:** yok.
- **Kanıt (burada koşar):** component testler + live smoke: provision edilmiş stack'te route yaz → LIST'te gör → publish edilen revision GET ile okunur → aynı route gerçek provider run'ı servis eder.
- **Honest ceiling:** admin full-set ListView — filtre/cursor yok (küçük set, provisioning emsali); write-surface şeması değişmez.

### T2 — Shared fixture corpus + mekanik cross-language equality harness (`tests/conformance/sdk/`; migration: yok)

- [ ] Corpus `tests/conformance/sdk/corpus/*.json` (tool-sdk corpus emsali): **request-encode** (Responses create/stream paramları → wire body), **event-decode** (SSE transcript'leri → canonical event dizisi), **error-map** (RFC9457 problem bodies → typed error projeksiyonu; 410 tombstone dahil — API-015'in SDK yüzü), **signature-verify** (webhook rotation vektörleri: valid/tampered/stale — API-014), **unknown-field preserve** (forward-compat: bilinmeyen alan üç dilde AYNI şekilde korunur), **envelope-decode** (Page + ListView — T1'in ilan ettiği ayrım).
- [ ] **Runner protokolü** (dil-bağımsız sözleşme): runner stdin'den vektör alır, stdout'a NORMALİZE JSON çıktı yazar; Go harness'ı runner çıktılarını canonical-bytes diff'ler — parity assert TEK yerde (design invariant).
- [ ] TS runner (mevcut SDK üzerinden) + Go REFERANS decode'u (`packages/contracts` — SDK değil, sunucunun kendi tipleri): corpus'un kendisi iki bağımsız implementasyonla valide olur; vektör hatası ikisinde birden patlar.
- **Seam:** `tests/conformance/sdk/` (yeni) + `sdks/typescript/test/`. **UAT:** API-012'nin temeli; API-014 vektörleri. **Migration:** yok.
- **Kanıt (burada koşar):** harness TS + referans-Go üzerinde yeşil; tamper testi (vektör çıktısında 1 byte oynat → diff FAIL).
- **Honest ceiling:** üç-dil equality T3/T4 runner'ları gelene kadar iki-implementasyondur; "üç dilde eşit" iddiasını YALNIZ T8 gate'i yapar.

### T3 — Python SDK sync+async (`sdks/python/`; migration: yok)

- [ ] `sdks/python/` paketi: client (retry + Idempotency-Key + timeout + typed RFC9457 errors), SSE stream iterator (sync) + async iterator (tek kod tabanı, httpx — TEK yeni bağımlılık), TS resource setinin bire bir karşılığı (responses/sessions/agents/artifacts/reads/provisioning/secret-refs/model-routes T1-read-back dahil), unknown-field preserve, generated tipler `make generate`'ten.
- [ ] API-013 kalıbı RED-first: injected transport failure boyunca AYNI Idempotency-Key, sunucuda TEK mutation (TS `retry.test.ts` senaryosu Py'de).
- [ ] T2 runner protokolünde Python runner + pytest suite; README server-side stance'ı pozitif yazar.
- **Seam:** `sdks/python/` (yeni) + `tests/conformance/sdk/` runner kaydı. **UAT:** API-013 (Py yarısı); API-012'ye Py bacağı. **Migration:** yok.
- **Kanıt (burada koşar):** pytest + harness'ta TS↔Py equality yeşil; live smoke: gerçek stack'e karşı create+stream+retrieve, gerçek provider'la tek adım (E08 kuralı).
- **Honest ceiling:** sync+async httpx'in iki transport'udur, ikinci bir HTTP kütüphanesi eklenmez; PyPI publish YOK (T7/E18 ayrımı).

### T4 — Go SDK (`sdks/go/`; migration: yok)

- [ ] `sdks/go/` KENDİ modülü (`go.mod`) — monorepo iç paketlerine import YOK (public SDK bağımsız taşınabilir kalır); tipler `make generate`'in Go emit'inden (elde kopya değil — drift invariant'ı). net/http + bufio SSE (provider_one'ın 1 MiB frame tavanı idiomu SDK tarafına ayna), retry + Idempotency-Key, typed errors, TS resource setinin karşılığı.
- [ ] API-013 kalıbı RED-first (Go yarısı); T2 runner protokolünde Go SDK runner'ı (referans-decode'dan AYRI — SDK'nın kendi decode'u test edilir).
- **Seam:** `sdks/go/` (yeni) + `scripts/contracts/generate` Go-emit + `tests/conformance/sdk/` runner kaydı (T3 SONRASI merge — sabit sıra). **UAT:** API-013 (Go yarısı); API-012'ye Go bacağı. **Migration:** yok.
- **Kanıt (burada koşar):** go test + harness'ta TS↔Py↔Go ÜÇ-dil equality İLK kez yeşil; live smoke: create+stream+retrieve.
- **Honest ceiling:** v0 ergonomics minimaldir (basit options pattern); helper zenginliği talep geldikçe. `// ponytail:` referans-decode ile SDK-decode'un kısmen örtüşmesi bilinçlidir — biri corpus'u valide eder, öteki SDK'yı.

### T5 — İkinci direct provider + OpenAI-compatible capability probe (`adapters/models/`; migration: yok)

- [ ] `adapters/models/provider_two`: Anthropic Messages API streaming adapter'ı — provider_one disiplini BİRE BİR (SDK'sız plain HTTPS+SSE, no-retry by construction, credential tek header, sanitized error, canonical `modelbroker.Result`, usage + gerçek provider request id). Registry map'e `"provider-two"` (`main.go:397`).
- [ ] `adapters/models/openai_compatible`: BaseURL'li generic ChatCompletions adapter'ı (provider_one'ın parametrize edilmesi — kod paylaşımı brief kararı, iki kopya YASAK) + **AKTİF capability probe** (§27.5): endpoint'e küçük probe istekleri (streaming/tool-call/strict-JSON destekleri) → capability record (modalites, limits, last_validated); desteklenmeyen HARD requirement admission'da run ÖNCESİ reddedilir (MOD-002). `// ponytail:` probe cache in-process + last_validated + stale label — tek-binary self-host için doğru; kalıcı capability tablosu gerekirse 000035+ owner onayıyla.
- [ ] Deterministic conformance: `tests/conformance/models/` suite'i adapter-PARAMETRİK hale gelir (fake + provider_one + provider_two + openai_compatible aynı canonical assert setinden geçer; wire-fixture'lı).
- **Seam:** `adapters/models/` + `main.go` registry + `tests/conformance/models/`. **UAT:** MOD-001; MOD-002; MOD-003'ün proxy-target yarısı (OpenAI-compatible adapter LiteLLM'in proxy moduna da işaret edebilir — §27.8). **Migration:** yok.
- **Kanıt (burada koşar):** deterministic suite dört adapter'da yeşil; live smoke İKİ GERÇEK provider'a: text+stream+usage+cancel, artı broker-seviyesi TEK tool-call smoke'u her ailede (engine stance'ı DEĞİŞMEZ — E08: engine gerçek provider'a tool açmaz); probe live: gerçek OpenAI endpoint'i (compatible-by-definition) + local fake private endpoint (eksik-özellikli → admission reject kanıtı).
- **Honest ceiling:** LiteLLM enhanced adapter YOK (opsiyonel); gerçek LiteLLM/vLLM/Ollama endpoint'i §6 operator legi. provider_two v0 capability record'unda Anthropic'in cache/reasoning zenginlikleri `false/unknown` etiketlidir — yalan claim yerine dürüst eksik.

### T6 — Runtime conformance tamamı: fallback/cancel/partial/cache/usage/circuit/budget (migration: yok)

- [ ] E06'nın provider-1 subset'i iki aile + fake üzerinden TAMAMLANIR (`tests/conformance/models/`, broker seam, scripted fake + wire-fixture — deterministik): **MOD-005** fallback-before-output = yeni attempt kaydı + doğru target/usage; **MOD-006** partial sonrası fallback = partial GÖRÜNÜR kalır, hidden/seamless retry yok; **MOD-007** ambiguous acceptance = blind tool-producing replay YOK, reconciliation/typed failure; **MOD-008** attempt count = hidden-retry çarpanı olmadığının kanıtı; **MOD-009** cancel = provider cancel denenir, partial/final/usage tutarlı; **MOD-012** circuit = caller-invalid hatalar shared circuit'i TETİKLEMEZ, izinli route failover eder.
- [ ] **MOD-010** prompt cache: provider cache alanları (cached-token sayaçları) canonical Usage'a taşınır (`make generate`), tenant/provider izolasyonu + cache usage/cost görünürlüğü assert edilir.
- [ ] **MOD-011** budget: E13 T6 `usage_ledger` reservation→settlement'ı concurrent step'lerle sürülür — dokümante estimate varyansı ötesinde overspend İMKANSIZ (deterministic ledger ID emsali).
- [ ] **MOD-004** kalan yarısı: E13 T8 routing'ine capability/price hard filtreleri (T5 capability record'u tüketilir) — hard filter ASLA gevşemez, admission öncesi reject.
- **Seam:** `packages/model-broker/` + `internal/execution/model_dispatch.go` + `tests/conformance/models/`. **UAT:** MOD-004..012. **Migration:** yok.
- **Kanıt (burada koşar):** deterministic suite yeşil; live smoke: gerçek provider'da cancel + usage settlement tutarlılığı (tek adım) + ledger toplamının provider usage ile uyuşması.
- **Honest ceiling:** hedging/speculative (§27.11) v0 DIŞI (§5); circuit tek-process'tir (multi-replica distributed circuit SaaS); MOD-010 kanıtı provider'ın RAPORLADIĞI cache sayaçlarına dayanır — provider-side cache davranışının kendisi kontrol edilemez.

### T7 — Paket provenance, checksums, changelog, compatibility matrix (`scripts/release/`; migration: yok)

- [ ] `scripts/release/sdk-package.sh`: üç paketin LOCAL build'i — `npm pack` (@palai/sdk), python wheel+sdist (hatchling/uv build; brief Context7), Go module (dizin + sürüm tag planı; Go'da paket = kaynak, artifact üretilmez — dürüst not). Çıktı: sha256sums manifest + **openssl P-256 detached signature** (E14 T5/E15 T4 aracı AYNEN — ikinci imza aracı eklenmez) + build-input kaydı (git ref, toolchain sürümleri).
- [ ] `scripts/release/sdk-verify.sh`: offline checksum + imza re-verify (`--network none` container'da, E15 T4 kalıbı).
- [ ] Her SDK'ya CHANGELOG.md + `docs/operations/sdk-compatibility.md`: SDK sürümü × `API-Version` × server sürümü matrisi — YALNIZ test edilmiş hücre dolu; test edilmemiş hücre boş kalır, iddia edilmez.
- **Seam:** `scripts/release/` + `sdks/*/`. **UAT:** yok (destek görevi; T8 PackagingProof'unu besler). **Migration:** yok.
- **Kanıt (burada koşar):** build → offline verify yeşil → tamper testi (1 byte → FAIL).
- **Honest ceiling:** public registry PUBLISH YOK; SBOM/provenance ATTESTATION üretimi E18 (manifest alanları tanımlı-boş, E15 T4 emsali).

### T8 — EXIT gate: üç-dil journey 63.1 + gateway-off + evidence bundle (`tests/uat/sdk-parity/`; migration: yok)

- [ ] Journey 63.1 TAMAMI harness'ta: temiz stack → model connection (T1 API'siyle) → Responses **TypeScript + Python + Go + CLI** dördünden → streaming text + strict structured output → usage/events/audit + store:false purge (410 üç SDK'da typed) → stack restart → retained retrieval tekrarı. "identical semantic result across SDKs" = T2 harness'ının journey ÇIKTILARINA uygulanması — mekanik diff, göz kararı değil.
- [ ] **Gateway-off direct-path LIVE** (exit cümlesinin ikinci yarısı): openai_compatible route'u local stand-in proxy'ye (FAKE olarak adlandırılır) işaret ederken proxy ÖLDÜRÜLÜR → gateway route'u typed fail olur, direct provider-one + provider-two route'ları GERÇEK run servis etmeye devam eder (MOD-003'ün direct-path yarısı).
- [ ] `tests/uat/evidence.go`'ya yeni claim/proof tipleri (anchor disiplini): **SDKParityProof** (vektör/journey-adımı başına üç dilin HAM normalize çıktıları + digest'leri — verifier üç çıktıyı YENİDEN hash'ler ve eşitliği YENİDEN hesaplar; fabrike "equal" değeri FAIL = anti-fabrication anchor), **ProviderConformanceProof** (MOD claim × adapter matrisi + attempt sayaçları), **CapabilityProbeProof** (probe ham yanıt digest + last_validated recompute), **GatewayOffProof** (config digest + proxy-ölümü sonrası tamamlanan run id + gerçek provider request id), **PackagingProof** (sha256 manifest + imzanın offline re-verify'ı).
- [ ] **API-012..015 + MOD-001..012** case'leri `tests/uat/cases/` altında materialize (bugün SIFIR); her case metni LOCAL seam'i ve varsa §6 operator bacağını AÇIKÇA adlandırır (E14/E15 emsali).
- [ ] `make uat-sdk-parity` + **`sdk-provider-parity-0.1.0` evidence bundle** (redacted manifest; ad iki yarıyı da söyler — yalnız "sdk-parity" provider workstream'ini gizlerdi) + `make evidence-verify` 0/0/0/0.
- **Exit-gate proof'un evi budur.** **Migration:** yok.
- **Honest ceiling:** journey 63.1'in "clean supported workstation" iddiası bu oturumda macOS + Docker Desktop'tır; Linux/Windows workstation matrisi §6 operator legi. Gateway-off kanıtı stand-in proxy iledir; gerçek LiteLLM ile tekrarı §6.

---

## 5. OUT OF SCOPE (bilinçli dışarıda, adres adresine)

| Kalem | Neden dışarıda | Nerede yaşıyor |
|---|---|---|
| LiteLLM enhanced adapter (health/routing/budget import) | §27.8'de opsiyonel; OpenAI-compatible proxy-target yolu MOD-003'ü karşılar | Talep gelirse ayrı task; gerçek-instance koşumu §6 |
| Public registry publish (npm/PyPI/Go proxy) + SBOM/provenance attestation + imzalı release pipeline | Supply-chain işi | E18 (`scripts/release/` devralır) |
| Hedging / speculative requests (§27.11) | MOD katalogunda claim'i yok; retry/fallback conformance'ı yeter | Talep gelirse E18 sonrası |
| Multi-replica distributed circuit/rate-limit | Tek-process compose topolojisi; weighted fairness hostile multi-tenancy işi | SaaS planı |
| Browser-direct token + CORS (SDK'larda browser story) | E13 server-relay kararı korunur; TS guard + Py/Go server-side | SaaS planı (gerekirse) |
| Anthropic cache/extended-reasoning zenginlikleri v0 adapter'da | Capability record dürüstçe `false/unknown` der; yalan claim yok | Adapter iterasyonu, talep geldikçe |
| Mid-chat model switch journey (SES-006..008) | Session yüzeyi işi, SDK/provider parity değil | İlgili epic (master plan SES sahipliği) |
| Eval suites, knowledge, console | Ayrı fazlar | E17b/E17c |
| Kalıcı capability tablosu / probe geçmişi | In-process cache + stale label yeter (T5 ponytail notu) | Gerekirse 000035+, owner onayı |

## 6. Operator legs — gerçek-altyapı bacağı (deferred-but-scripted; kaybolmaz)

Her biri için KOD/harness bu fazda hazırdır ve parametriktir; İCRA operator-provided altyapı/karar ister:

1. **Gerçek LiteLLM instance'ına karşı** openai_compatible adapter + capability probe + gateway-off drill'inin tekrarı (T5/T8 harness'ları BaseURL parametriktir).
2. **Gerçek private/self-hosted model server** (vLLM/Ollama sınıfı) probe + run — eksik-özellik admission reject'inin gerçek endpoint'te kanıtı.
3. **Registry publish provası** (npm/PyPI dry-run + Go module tag) — E18 pipeline'ına devir; T7 manifest'i girdi olur.
4. **Linux/Windows workstation'da journey 63.1** — "clean supported workstation" iddiasının tam matrisi.

Bu legler T8 gate'inin LOCAL kanıtını BLOKLAMAZ; SDK'ların stable ilanı (E18) 3. legin icrasını bekler.

## 7. Master plan §8 için önerilen özet blok (owner paste eder)

**UAT ownership:** API-012..015; MOD-001..012 (MOD-004 E13 routing yarısının üstüne capability/price tamamlaması); local journey 63.1'in üç SDK tamamı — tamamı `tests/uat/cases/` altında materialize, kanıt seam'i case metninde adlandırılır.

**Exit gate — SDK parity + provider completeness (local seam'de):** Aynı fixture corpus'u TS+Py+Go runner'larında koşar ve üç dilin normalize çıktıları MEKANİK diff ile eşittir (verifier eşitliği ham çıktılardan YENİDEN hesaplar); journey 63.1 dört client'tan (üç SDK + CLI) aynı semantic sonucu üretir; ikinci bağımsız gerçek provider ailesi text/stream/tool/schema conformance'ından geçer ve İKİ gerçek provider'a live smoke koşar; OpenAI-compatible endpoint aktif capability probe ile desteklenmeyen hard requirement'ı run öncesi reddeder; stand-in gateway ÖLDÜRÜLDÜĞÜNDE direct route'lar gerçek run servis etmeye devam eder; MOD-005..012 runtime conformance'ı iki aile + fake üzerinde deterministik yeşildir; üç paketin checksums+imzalı manifest'i offline verify edilir ve compatibility matrix yalnız test edilmiş hücreleri doldurur; `sdk-provider-parity-0.1.0` evidence verifier 0/0/0/0 yeşildir. **Bu gate "gerçek LiteLLM/gerçek private model server'da/PyPI-npm'de yayımlandı" İDDİA ETMEZ** — o bacaklar §6 operator legleridir; publish E18'dedir.
