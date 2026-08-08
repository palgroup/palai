# Model protokolleri ve adlandırılmış bağlantılar — tasarım (2026-08-08)

Bugün tek kavrama sıkışmış iki şeyi ayırır: **konuşulan wire protokolü** (kod) ve **operatörün
kurduğu bağlantı** (veri). Ayrımın sonucu, Grok / z.ai / Azure / Bedrock gibi hedeflerin **kod
yazmadan**, konsoldan eklenebilmesidir.

Bu doküman CLAUDE.md'nin dört kuralına uyar: her sayı onu üreten komutla yazılır, her runtime
iddiası bir YAZAR gösterir, devralınan tavan tarihlidir. **Task'ın ilk adımı §2'nin komutlarını
yeniden koşmaktır.**

**Geriye uyum bu spec'in hedefi DEĞİLDİR ve bu bilinçli bir karardır** (§4). Prod deployment yoktur;
uyumluluk için ödenecek her karmaşıklık, ödenmeyecek bir fiyattır — CLAUDE.md kural 4'ün ta kendisi.
Bu spec'in ilk taslağı (`6983ca07`) o fiyatı yazmıştı ve ölçüm onu düşürdü.

---

## 1. Karar

| | Bugün | Sonra |
|---|---|---|
| **Kavram** | Tek "family": hem wire protokolü hem operatör seçimi | **Protokol** (kod, `openai`/`anthropic`) + **bağlantı** (veri, adı olan satır) |
| **Kayıt** | Bir aile 11 yerde (§2.1) | Bir protokol: 1 Go paketi + 1 descriptor |
| **Yeni hedef** | Grok/z.ai/Azure = ya yeni aile ya `openai-compatible`'a sıkıştırma | Yeni **bağlantı** — konsolda bir form, kod yok |
| **Ad** | `provider-one` / `provider-two` | `openai` / `anthropic` — **temiz rename**, alias katmanı yok |
| **Hedef ifadesi** | Yok; aile route'un connection'ından gelir | `bağlantı-adı:model`, her katmanda taşınır |
| **`openai-compatible`** | Üçüncü "aile" | **Düşer** — `openai` protokolü + üçüncü-taraf endpoint (§3.5) |

**Kapsam dışı, §6'da kayıtlı:** B (yetenek sözleşmesi), D (delegation tool'u + model alias'ları).

---

## 2. Ölçümler

Hepsi 2026-08-08'de ölçüldü. **Yeniden koş.**

### 2.1 Bir aile bugün on bir yerde kayıtlı

| # | Yer | Ne |
|---|---|---|
| 1 | `packages/model-broker/families.go:41-48` | family map |
| 2 | `adapters/models/registry/registry.go:73-81` | `Adapters()` |
| 3 | `adapters/models/registry/registry.go:97-103` | `Inspectors()` |
| 4 | `apps/control-plane/cmd/palai-control-plane/main.go:967-971` | `EnvResolver` secret→env |
| 5 | `apps/web-console/app/registry/page.tsx:95-99` | `FAMILIES` (TS kopyası) |
| 6 | `cmd/cli/internal/admin/admin.go:157, 330, 768` | üç elle yazılmış string |
| 7 | `sdks/typescript/src/resources/model-routes.ts:17` | doc yorumu |
| 8 | `tests/uat/evidence.go:991, 995` | shipped evidence sabitleri |
| 9 | `deploy/compose/compose.yaml:185, 267` + `control-plane-entrypoint.sh:7-9` | file-secret köprüsü |
| 10 | `cmd/cli/internal/stack/native.go:272` | native env köprüsü |
| 11 | `main.go:990` `liveModelProvider` | deployment default tek aileye sabit |

### 2.2 İkinci aile hiç bağlanmamış

```
$ grep -rn "PALAI_SECRET_PROVIDER_TWO" . | grep -v ".next/" | wc -l
1
```

Tek geçtiği yer `main.go:969` — **adı var, yazanı yok.** `up.go:340-344` sonucu itiraf eder:
elinde yalnız Anthropic anahtarı olan operatör stack'i ayağa kaldıramaz.

### 2.3 `openai-compatible` bir protokol değil

`adapters/models/openai_compatible/adapter.go:57-86`: `providerone.Adapter`'ı **embed eder** ve
üzerine yalnız bir capability probe ekler. Wire dönüşümü aynıdır. Yani bugünkü "üçüncü aile",
`openai` protokolü + bir davranıştır — **ayrımın kodda zaten yarım hâlde var olduğunun kanıtı.**

### 2.4 Aile bir config katmanı değil

`config.go:104-119` katmanları `deployment → project_route → agent_revision → session`; aile
hiçbirinde yok ve `ResolveInput` (`config.go:32-34`) bunu kendi sözleriyle söyler. `PinnedConfig`
(`packages/coordinator/store.go:1108-1128`) model taşır, **aile taşımaz**.

Bedeli `agentTemplates.ts:33-43`'te kayıtlı: üç şablon `model: claude-opus-4-8` yazıyormuş ve
*"on a provider-one deployment every template pinned a model the project's credential cannot
[reach] — at the first run"*. Çözümleri **kaçınma** olmuş: model adını hiç yazmamak.

### 2.5 `model` alanı kabul edilir ve yok sayılır

`responses.go:283` `req.Model`'i yalnız ilk projeksiyona kopyalar; `AdmitRequest` (`responses.go:56`)
bir `Model` alanı taşımaz. Terminal projeksiyon gerçek modeli yazar (`finalize.go:251`) — **aynı
alan iki farklı cevap verir.** Kimse göndermediği için görünmemiş: konsol yalnız okur
(`runs/page.tsx:557`), Slack bot göndermez, üç SDK'da alan yoktur.

### 2.6 Geriye uyum için ödenecek fiyat — ölçüldü

```
$ grep -rn "sha256:[0-9a-f]\{16\}" --include="*.go" tests/ apps/ packages/   # config hash: YOK
$ grep -rln "provider-one\|provider-two\|provider_one\|provider_two" --include="*_test.go" . | wc -l
103
```

- **Pinlenmiş config hash sabiti yoktur.** `effectiveConfigHash` yalnız bir component testinde
  okunur ve orada bir sabitle değil **kendi hesabıyla** karşılaştırılır
  (`pinned_revision_component_test.go:107-112`). UAT'daki sha256'lar case checksum'larıdır.
  → İlk taslağın "config adresi kımıldamaz" garantisi **hiçbir şeyi korumuyordu**.
- **103 test dosyası** literal'e bağlıdır; büyük kısmı UAT evidence/catalog'dur. Rename'in
  gerçek bedeli budur — prod değil, **kanıt paketleri.**
- `PALAI_MODEL_PROVIDER` metni `evidence/releases/managed-cloud-0.1.0/manifest.json`'da donmuştur.
- Yerel bir stack koşuyor (`docker ps` → `palai-79edc38f-postgres-1`, 8 saat). Prod yok, ama
  **yerel satırlar var**: migration onları taşımalıdır (§4.2).

---

## 3. Tasarım

### 3.1 İki kavram

**Protokol** — bir HTTP wire şekli. Kod. Bugün ikisi vardır: `openai` (ChatCompletions) ve
`anthropic` (Messages). Yeni bir protokol = yeni bir Go paketi, çünkü gövde şekli, usage
semantiği ve hata sözlüğü farklıdır (`packages/coordinator/usage.go:62-81`,
`orchestrator.go:1086-1099` bu farkları bugün de kaydeder).

**Bağlantı** — bir operatörün kurduğu erişim: **ad**, protokol, endpoint, credential ref. Veri.
`model_connections` satırıdır ve bugün de vardır — eksik olan tek şey **addır**.

Ayrımın testi: *"Grok eklemek için Go kodu yazılıyor mu?"* Hayır — Grok OpenAI wire'ı konuşur,
yani `openai` protokolünde bir bağlantıdır. *"Gemini için?"* Evet — ayrı wire, ayrı paket.

### 3.2 Protokol kaydı

`packages/model-broker` bir protokol hakkındaki **her şeyi** tek descriptor'da taşır: ad, label,
adapter yapıcısı, inspector, vendor varsayılan endpoint'i. `registry.Adapters()` ve
`registry.Inspectors()` map literali olmaktan çıkar, bu listeden **türer**. Secret env adı da
türetilir, böylece §2.2'nin "adı var, yazanı yok" durumu **yapısal olarak** imkânsızlaşır.

`GET /v1/model-protocols` bu listeyi servis eder. Konsolun `FAMILIES` kopyası, CLI'ın üç string'i
ve SDK doc yorumu silinir. `families.go:15-18` bugün *"the console renders its picker from the
projection the API serves"* der ve öyle bir projection **yoktur** — konsolun kendi yorumu
(`registry/page.tsx:111`) *"there is no /v1 route"* diye itiraf eder. Bu iş o cümleyi doğru yapar.

### 3.3 Bağlantı: ad + protokol + endpoint + credential

`model_connections`'a `name` sütunu eklenir; `(project_id, name)` üzerinde UNIQUE. Ad operatörün
seçtiği, okunabilir bir tanımlayıcıdır: `openai`, `my-azure`, `grok`, `zai`.

Ad kavramı ağaçta yenidir ama yabancı değildir: `model_routes` zaten adlandırılmıştır (`default`).
UNIQUE'in **oradaki eksikliği kayıtlıdır** — `model_routes.go:335-336`, *"000001 declares no
UNIQUE(organization_id, project_id, name), so two concurrent creates could still both insert"*.
Bağlantı adı bir hedefin çözüldüğü şey olduğu için burada aynı boşluk bırakılamaz: iki satırlı bir
ad, `LIMIT 1`'in hangi credential'ı seçtiğine karar vermesi demektir — bu ağaçta sırasız `LIMIT 1`
iki güvenlik sonucunu belirlemiştir.

> **Ad, secret ref adı DEĞİLDİR** ve konsol formunda ikisi yan yana durur. Secret ref bir
> *credential'ın* adıdır ve birden çok bağlantı aynı ref'i kullanabilir; bağlantı adı bir
> *erişimin* adıdır ve hedefin çözüldüğü şeydir. Form ikisini ayrı alan olarak sorar ve hangisinin
> nerede göründüğünü söyler.

`base_url` artık **her** protokolde kabul edilir. Boş bırakılırsa protokolün vendor varsayılanı
kullanılır; doldurulursa üçüncü-taraf bir endpoint'tir. Bugünkü `RequiresBaseURL` /
`AcceptsBaseURL` çifti düşer — ikisi de `openai-compatible`'ı ayrı bir aile yapmak için vardı.

> Bir bağlantının **boş endpoint'i** vendor'a gider. `registry/page.tsx`'in bugünkü caveat'ı bunun
> neden önemli olduğunu zaten yazar: boş bir custom endpoint `api.openai.com`'a düşerdi, yani
> operatörün özel sunucusu için minted bir anahtarla prompt'ları OpenAI'a göndermek. Yeni şekilde
> bu kaza imkânsızdır çünkü boş endpoint **protokolün vendor'ı demektir**, sessiz bir fallback değil.

### 3.4 Hedef: `bağlantı-adı:model`

Bir model hedefi tek string'tir. Çözüm kuralı:

> İlk `:`'dan önceki parça **projenin bir bağlantı adıysa** bağlantı önekidir; değilse **tamamı
> model id'dir** ve bağlantı, route'un bağladığıdır.

Bu kural bir tuzağı kapatır: OpenAI'ın fine-tune id'leri `ft:gpt-4o-mini-2024-07-18:org::AbCd`
biçimindedir ve **içinde `:` vardır**. `ft` bir bağlantı adı olmadıkça tamamı model id sayılır.
(AI SDK'nın `createProviderRegistry` ayırıcısını yapılandırılabilir yapmasının sebebi bu sınıftır.)

> **Sonuç, ve dikkat edilmesi gereken:** çözüm artık **projeye bağlıdır** — bir bağlantı adı bir
> projede vardır, başkasında yoktur. Bu kasıtlıdır: hedef, o projenin operatörünün kurduğu erişime
> atıf yapar. Taşınabilirlik §3.7'nin listesiyle sağlanır, gizli bir global ad uzayıyla değil.

### 3.5 `openai-compatible` düşer

Bugünkü üç aile ikiye iner. `openai-compatible`'ın taşıdığı tek gerçek davranış capability
probe'dur (§2.3) ve o bir **aile özelliği değil, endpoint özelliğidir**: bağlantı vendor'ın kendi
endpoint'ini kullanıyorsa probe gereksizdir, üçüncü-taraf bir endpoint kullanıyorsa gereklidir.
Probe mantığı taşınır, tetikleme koşulu `base_url != ""` olur.

`PALAI_OPENAI_COMPATIBLE_BASE_URL` düşer: bir endpoint artık deployment-genelinde değil, bağlantı
başına tanımlanır — ki E29'un `Request.BaseURL` yorumu (`types.go:79-87`) bunun neden doğru
olduğunu zaten yazar.

### 3.6 Katmanlar, credential ve red

Model katmanlarına bir basamak eklenir:

```
deployment → project_route → agent_revision → session → request
```

`request` en üsttedir: çağıran açıkça bir model istemiştir ve bugün o alan sessizce yok
sayılmaktadır (§2.5) — üç seçenekten en kötüsü. `ResolveInput` hedefi taşır, `Resolve` bağlantıyı
da çözer, provenance **her ikisini de** kaydeder.

**Credential** çözülen bağlantının `secret_ref`'inden gelir. Adı olmayan bir bağlantıya yönelen
hedef **admission'da 400 alır** — `Broker.Route`'ta `unknown_provider` olarak değil. Deployment
credential'ına **düşülmez**; gerekçesi `model_route.go:53-55`'te yazılıdır: bir tenant'ın işini
operatörün kendi anahtarıyla sessizce koşturmak ve faturalandırmak.

`families.go:5-12` bu sınıfın bir örneğini zaten kaydeder: `{"provider":"openai"}` 201 alıp
saklanıyor, route'a yayınlanıyor ve ilk model adımında ölüyormuş. Kanıtlanması gereken **reddin
yeridir**, varlığı değil.

### 3.7 Erişilebilir hedefler

"Bu projenin erişilebilir hedefleri" tek çağrıyla cevaplanır: hangi bağlantılar var, her birinin
protokolü ne, ve her birinin listeleyebildiği modeller neler (`ModelLister` zaten vardır).

Üç tüketicisi olur: §3.6'nın admission reddi, konsolun picker'ı, ve **D'nin tool şemasındaki
enum**. Sonuncusu bu maddenin sebebidir: bir subagent tool'u modele serbest metin bir alan
sunarsa, model olmayan bir hedefi uydurur, 400 yer ve deneme-yanılmaya girer. Enum olarak
sunulduğunda olmayan bir hedefi **isteyemez** — reddedilmez, hiç aklına gelmez.

---

## 4. Rename ve migration — temiz kesim

**Geriye uyum katmanı yoktur.** Alias yok, çift-okuma yok, `omitempty` numarası yok.

**4.1 Kod ve testler.** `provider-one` → `openai`, `provider-two` → `anthropic`. 103 test dosyası
(§2.6) ve `tests/uat/evidence.go`'nun sabitleri güncellenir. Paket adları
`adapters/models/provider_one` → `adapters/models/openai` olur.

**4.2 Veri.** Bir migration `model_connections.provider`'ı yeni adlara çevirir, `name` sütununu
ekler ve mevcut satırlara protokol adından bir başlangıç adı verir. `model_route_revisions.config`
JSONB'sindeki `connection_id` referansları değişmez. Yerel stack'in satırları (§2.6) bu
migration'dan geçer — prod olmaması migration'ı gereksiz kılmaz, **ucuz** kılar.

**4.3 Kanıt paketleri.** `PALAI_MODEL_PROVIDER` metni bir yayımlanmış manifest'te donmuştur
(§2.6). Shipped metin **düzenlenmez**; hafızadaki kural: kapanan bir tavan `rested_on` ile
kaydedilir. Etkilenen bundle'lar topolojik sırada yeniden üretilir — bir evidence sabiti
`baselineManifest` üzerinden birden çok bundle'a kaskad eder.

**4.4 Ne KORUNUR.** Öneksiz her model değeri bugünkü davranışı sürdürür (§3.4) — bu geriye uyum
değil, tasarımın kendisidir: hedefin bağlantı öneki **opsiyoneldir**.

---

## 5. Test ve guard'lar

**5.1 Türeme guard'ı, iki yönlü.** `registry_test.go:22`'nin şekli korunur ama descriptor listesi
ile ondan TÜREYEN her projeksiyon arasında kurulur: adapter map, inspector map, secret env map, API
projeksiyonu. Bir protokol eklenip bir projeksiyondan düşerse **kırmızı**.

**5.2 Hedef çözümü.** Tablo: öneksiz değer, geçerli bağlantı öneki, **başka projenin** bağlantı adı
(reddedilmeli), bilinmeyen önek (model id sayılmalı), ve fine-tune id'i (`ft:...:org::id` → tamamı
model). Son ikisi §3.4'ün tuzağının regresyon testidir.

**5.3 Red kanıtı.** Bağlantısı olmayan bir hedef admission'da 400 alır. Test projesini kendi kurar
ve o bağlantıyı **kasten yaratmaz** — koşulunu sahiplenir, harness'tan devralmaz. CLAUDE.md'nin
"harness'a ait yeşil" kaydı bu şeklin dört örneğini taşır.

**5.4 Migration kanıtı.** Migration'dan önce yazılmış bir `provider-one` satırı, sonrasında
`openai` olarak okunur ve aynı route aynı modeli dispatch eder. Kanıt DB'den okunur, migration
dosyasından değil.

**5.5 Koşan selector.** Yeni testler `scripts/test/component`'in `-run` allow-list'ine girer ya da
untagged kalıp `make verify`'a biner. Hangisi olursa **shipped selector koşturulur** ve `--- PASS`
bacakları diff'lenir; allow-list'i okumak kanıt değildir.

**5.6 Etiketli derleme.** Rename 103 test dosyasına dokunur ve bunların çoğu etiketli tier'lardadır.
`go vet -tags="component live security"` üç etiketi kapsar, yedi tane vardır — e2e/fault/
performance/uat bu vet'e **binmez**. Doğrulama her etiketi ayrı sürmelidir.

---

## 6. Kapsam dışı — kayıt

**B — yetenek sözleşmesi.** Conformance bugünkü kapsamı:

| Conformance | Kapsam |
|---|---|
| canonical tool result | 4/4 |
| usage cache folding | 4/4 |
| mid-stream cancel | 3/4 |
| truncated stream → partial | 2/4 |
| capability filter | 1/4 |
| vision / image | **0/4 — conformance'ta yok** |

```
$ grep -rn "Families()\|LookupFamily" tests/ --include="*.go" | wc -l
0
```

Conformance tabloları elle yazılmıştır; bir protokolü eklemeyi unutmak **sessiz** kalır. Vision her
adapter'ın kendi paketinde ayrı test edilir (`provider_one/vision_test.go` 117 satır,
`provider_two/vision_test.go` 314 satır): aynı özellik, iki ayrı iddia, ortak sözleşme yok. B
bağımsızdır ama §3.2'nin descriptor'ından faydalanır — probe'un endpoint özelliğine taşınması
(§3.5) B'nin ilk adımını zaten atar.

**D — delegation tool'u.** Modele sunulan araçların tam listesi (`main.go:665-687`): math, file,
text_editor, glob, grep, shell, background_kill, media, commit, push, pull_request, merge,
research.fetch, knowledge.retrieve. **Delegation yoktur.** Mekanizmanın tamamı vardır — `childSpec`,
`admitChild`, ChildRun, detach, remote agent — ama modele tool olarak sunulmaz;
`orchestrator.go:745-747`: *"Config-driven, so a real single-step run delegates."*

D iki şey ekler: bir delegation tool'u (görev + araçlar + hedef), ve `fast`/`cheap` gibi **model
alias'ları** (Cursor'ın `model: fast`'i, AI SDK'nın `customProvider`'ı). D **bu spec'e bağlıdır**:
onsuz bir subagent parent'ının bağlantısında kalır ve `gemini:flash` senaryosu çalışmaz.

---

## 7. Açık kalan karar yok

İlk taslak burada bir bilinmeyen bırakmıştı — system-scoped bağlantıların §3.4'teki yeri. Ölçüldü
ve **karar gerektirmediği** anlaşıldı:

```
$ docker exec …-postgres-1 psql -tAc \
    "select count(*) filter (where project_id is null), count(*) from model_connections"
0|1
```

Ve sayı değil, YAZAR karar veriyor — `packages/coordinator/model_routes.go:306-308`:

> *000001 allows a NULL project_id (an org-wide connection), but migration 000029's policy compares
> project_id to the scoped project, so a NULL-project row is invisible to a [scoped read].*

Yani NULL-project bir satır yazılabilir ama **RLS onu okunamaz kılar**. Proje-scoped arama bir
varsayım değil, ağacın zaten tek mümkün kıldığı okumadır. §3.4 olduğu gibi durur.
