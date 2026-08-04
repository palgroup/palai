# Provider ailesi kaydı ve hedef katmanı — tasarım (2026-08-04)

Bir provider ailesi eklemenin **kayıt vergisini on bir yerden bire** indirir (adapter paketi
kaçınılmazdır ve sayıya dahil değildir), ve bir model hedefinin ailesini `effectiveRoute`'un
tekelinden çıkarıp **config katmanlarına** taşır.

**İki ayrı "alias" geçer, karıştırılmamalıdır:** *wire alias'ı* eski aile adıdır
(`provider-one` → `openai`, §3.1) ve bu spec'in kapsamındadır; *model alias'ı* bir modele verilen
takma addır (`fast` → belirli bir model, §6/D) ve bu spec'in kapsamında **değildir**.

Bu doküman CLAUDE.md'nin dört kuralına uyar: her sayı onu üreten komutla yazılır, her runtime
iddiası bir YAZAR gösterir, devralınan tavan tarihlidir. **Task'ın ilk adımı §2'nin komutlarını
yeniden koşmaktır** — değişen sayı anında görünür.

---

## 1. Karar

İki iş, tek seam. Ayrı yapılırsa aynı dosyalara iki kez dokunulur.

| # | Bugün | Sonra |
|---|---|---|
| **A** | Bir aile 11 yerde kayıtlı (§2.1); konsol/CLI/SDK kendi kopyalarını taşır | Tek `Family` descriptor; her projeksiyon ondan **türer**; `GET /v1/model-families` |
| **C** | Aile yalnız route'un connection'ından gelir; `ResolveInput` *"The PROVIDER is not a layer here"* der (`config.go:32-34`) | Hedef `aile:model`; aile her katmanda taşınır; request bir katman olur |

**Kapsam dışı, §6'da kayıtlı:** B (yetenek beyanı + conformance'ın family listesinden türemesi),
D (delegation tool'u + `fast` alias'ı). D, C'ye bağlıdır; B bağımsızdır.

---

## 2. Ölçümler

Hepsi 2026-08-04'te, `31c20681` üzerinde ölçüldü. **Yeniden koş.**

### 2.1 Bir aile eklemenin bugünkü maliyeti

Kaçınılmaz olan tek şey yeni adapter paketidir (wire farklı). Geri kalanı kayıt vergisi:

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
| 11 | `apps/control-plane/cmd/palai-control-plane/main.go:990` | `liveModelProvider` sabiti |

### 2.2 İkinci aile zaten yarım bağlı

```
$ grep -rn "PALAI_SECRET_PROVIDER_TWO" . | grep -v ".next/" | wc -l
1
```

Tek geçtiği yer `main.go:969` — **değişkenin adı var, onu yazan hiçbir şey yok.** Compose
`provider_one_key` bridge eder, `native.go:272` `PALAI_SECRET_PROVIDER_ONE` yazar,
`deployment.go:1213` `== "provider-one"` eşitliği kurar. Sonucu `up.go:340-344` itiraf eder:
elinde yalnız Anthropic anahtarı olan operatör stack'i ayağa kaldıramaz.

**Bu, A'nın gerekçesidir:** ikinci aile birinci sınıf vatandaş değil, üçüncüsü de olmayacak.

### 2.3 Aile bir katman değil

`config.go:104-119` model katmanları: `deployment → project_route → agent_revision → session`.
Aile hiçbirinde yok; `ResolveInput` yorumu (`config.go:32-34`) bunu açıkça söyler. `PinnedConfig`
(`packages/coordinator/store.go:1108-1128`) `Model`, `Instructions`, `Tools`, `ToolSetTools`,
`SkillPins` taşır — **aile yok, connection yok, credential yok.**

Bedeli ölçülmüş: `agentTemplates.ts:33-43`, üç şablonun `model: claude-opus-4-8` yazdığını ve

> `claude-opus-4-8` is in Anthropic's models list and in none of OpenAI's 133, so on a
> provider-one deployment every template pinned a model the project's credential cannot
> [reach] — at the first run.

kaydeder. Çözümleri **kaçınma** olmuş: model adını hiç yazmamak, route'tan miras almak. Bu
tasarım o kaçınmayı gereksizleştirir.

### 2.4 `model` alanı kabul edilir ve yok sayılır

`POST /v1/responses` gövdesindeki `model`, `responses.go:283`'te yalnız ilk projeksiyona
kopyalanır. `AdmitRequest` (`responses.go:56`) bir `Model` alanı taşımaz — çalıştırmaya hiç
geçmez. Terminal projeksiyon gerçek modeli yazar (`finalize.go:251`), yani **aynı alan iki farklı
cevap verir.**

Kimse göndermediği için bugüne kadar görünmemiş:

```
$ grep -rn '"model"' --include="*.go" --include="*.ts" --include="*.tsx" \
    apps/slack-bot/ apps/web-console/app apps/web-console/lib cmd/ | grep -v ".next/|_test|spec.ts"
```

Konsol yalnız **okur** (`runs/page.tsx:557`), Slack bot göndermez, CLI'daki `model` route
CRUD'una aittir, üç SDK'nın hiçbirinde `responses.create` için `model` alanı yoktur.

### 2.5 Conformance family listesinden türemez

```
$ grep -rn "Families()\|LookupFamily" tests/ --include="*.go" | wc -l
0
```

`registry_test.go:22` family↔adapter eşitliğini **iki yönlü** pinler. Family↔conformance tablosu
eşitliğini pinleyen hiçbir şey yoktur: bir aileyi tabloya eklemeyi unutmak **sessiz** kalır. (Bu
gözlem §6'ya, B'ye aittir; burada A'nın guard'ının neden iki yönlü olması gerektiğini gösterir.)

---

## 3. Tasarım

### 3.1 Tek `Family` descriptor (A)

`packages/model-broker/families.go` kanonik kayıt olmayı sürdürür, ama bir aile hakkındaki **her
şeyi** taşır: kanonik ad, **wire alias'ları** (DB'de duran eski adlar), label, adapter yapıcısı,
inspector, secret env adı, base-URL kuralları.

`registry.Adapters()` ve `registry.Inspectors()` artık map literali **değildir** — descriptor
listesinden türer. Secret env adı da türetilir (`PALAI_SECRET_` + kanonik adın upper-snake hâli),
böylece §2.2'deki "adı var, yazanı yok" durumu yapısal olarak imkânsızlaşır.

**Yeni bir aile = bir Go paketi (kaçınılmaz) + bir descriptor kaydı.** §2.1'in on bir kayıt
noktası **bire** iner, ve o bir tek satırdır.

`GET /v1/model-families` bu listeyi servis eder. Konsolun `FAMILIES` kopyası, CLI'ın üç string'i
ve SDK'nın doc yorumu silinir; hepsi API'den okur. `families.go:15-18` bugün *"the console renders
its picker from the projection the API serves"* der ve **öyle bir projection yoktur** — konsolun
kendi yorumu (`registry/page.tsx:111`) *"there is no /v1 route"* diye itiraf eder. Bu iş o cümleyi
doğru yapar.

`liveModelProvider` sabiti düşer: `PALAI_MODEL_PROVIDER` kayıtlı **herhangi** bir aileyi seçebilir,
ve compose secret dosyası aile adından türer.

### 3.2 Hedef: `aile:model`

Bir model hedefi tek string'tir. Çözüm kuralı:

> İlk `:`'dan önceki parça **kayıtlı bir kanonik aile adı veya wire alias'ıysa** aile önekidir;
> değilse **tamamı model id'dir** ve aile mevcut route'unkidir.

Bu kural bir tuzağı kapatır: OpenAI'ın fine-tune id'leri `ft:gpt-4o-mini-2024-07-18:org::AbCd`
biçimindedir ve **içinde `:` vardır**. Naif bir "ilk `:`'da böl" kuralı bunları bozar; `ft` hiçbir
zaman bir family adı olmayacağı için yukarıdaki kural onları kendiliğinden doğru tarafa düşürür.
(AI SDK'nın `createProviderRegistry` ayırıcısını yapılandırılabilir yapmasının sebebi bu sınıftır.)

Öneksiz her değer bugünkü davranışı **aynen** korur. Migration yoktur.

### 3.3 Katmanlar ve credential

Model katmanlarına bir basamak eklenir:

```
deployment → project_route → agent_revision → session → request
```

`request` en üsttedir: çağıran açıkça bir model istemiştir, ve bugün o alan kabul edilip sessizce
yok sayılmaktadır (§2.4) — üç seçenekten en kötüsü. `ResolveInput` hedefi taşır, `Resolve` aileyi
de çözer, provenance **her ikisini de** kaydeder.

**Credential.** Çözülen aile route'unkinden farklıysa, projenin o aile için yayınladığı
`model_connections` satırı aranır. `model_connections.project_id` sütunu vardır
(`storage/migrations/000001_core.up.sql:609-618`), yani "projenin `anthropic` connection'ı"
gerçek bir kavramdır. Bulunamazsa **reddedilir** — deployment credential'ına düşülmez.
Gerekçe `model_route.go:53-55`'te zaten yazılıdır: bir tenant'ın işini operatörün kendi anahtarıyla
sessizce koşturmak ve faturalandırmak.

> **Ölçülmemiş, plan ölçecek:** `project_id` sütunu NULL kabul eder. Bir system-scoped connection'ın
> bugün var olup olmadığı ve varsa fallback'te yeri, planın ilk adımında ölçülür. Bu tasarım
> proje-scoped aramayı varsayar.

### 3.4 Red admission'da olur

Bir hedef, projenin connection'ı olmayan bir aileyi işaret ederse **admission 400 döner** — form
gönderildiği anda, `Broker.Route`'ta `unknown_provider` olarak değil. Bütün amaç budur:
`families.go:5-12` bu sınıfın bir örneğini zaten kaydeder — `{"provider":"openai"}` 201 alıp
saklanıyor, route'a yayınlanıyor ve ilk model adımında ölüyormuş.

Kanıtlanması gereken şey **reddin yerdir**, varlığı değil: red testi, o connection'ın yokluğunu
**sahiplenmelidir** (kendi projesini kurup connection'ı kasten yaratmamalı), harness'tan
devralmamalı. CLAUDE.md'nin "harness'a ait yeşil" kaydı bu şeklin dört örneğini taşır.

### 3.5 Erişilebilir hedefler seam'i

C, "bu projenin erişilebilir hedefleri" sorusunu **tek çağrıyla** cevaplanabilir bırakır: proje
hangi ailelere connection yayınlamış, ve her birinin listeleyebildiği modeller neler
(`ModelLister` zaten vardır).

Üç tüketicisi olur: §3.4'ün admission reddi, konsolun picker'ı, ve **D'nin tool şemasındaki enum**.
Sonuncusu bu maddenin sebebidir: bir subagent tool'u modele serbest metin bir model alanı
sunarsa, model olmayan bir hedefi uydurur, 400 yer ve deneme-yanılmaya girer. Erişilebilir
hedefler enum olarak sunulduğunda model olmayan bir aileyi **isteyemez** — reddedilmez, hiç aklına
gelmez.

---

## 4. Geriye uyum garantileri

Bunlar iddia değil, tasarım kısıtıdır — her biri bir yazara veya bir ölçüme bağlıdır.

**4.1 Config adresi kımıldamaz.** `configContentHash` (`config.go:292-306`) `Skills`'i `omitempty`
ile taşır ve yorumu şunu garanti eder: *"a skill-less run hashes over EXACTLY the pre-skills fields
— the address never moves"*. Aile **aynı deseni** kullanır: hash'e yalnız route'unkinden farklı
olduğunda girer. Bugün koşan her run'ın config hash'i bit-aynı kalır ve checkpoint'ler bozulmaz.

**4.2 Hiçbir çağıran kırılmaz.** §2.4 ölçümü: `model` alanını bugün kimse göndermiyor.

**4.3 DB'deki satırlar öksüz kalmaz.** `provider-one` / `provider-two` wire adları
`model_connections.provider`'da kalır ve okunurken kanonik ada normalize edilir. Yeni yazılan
satırlar kanonik adı taşır. `families.go:20-22`'nin rename reddi böyle ödenir — silinerek değil,
alias'lanarak.

**4.4 Shipped evidence dokunulmaz.** `tests/uat/evidence.go:991, 995` wire adlarını taşır ve
onlar DB'de duran değerlerdir; normalizasyon bu sabitleri değiştirmez. (Hafıza kaydı: bir evidence
sabiti yedi bundle'a `baselineManifest` üzerinden kaskad eder.)

**4.5 Öneksiz değer = bugünkü davranış.** §3.2.

---

## 5. Test ve guard'lar

**5.1 Türeme guard'ı, iki yönlü.** `registry_test.go:22`'nin şekli korunur ama artık descriptor
listesi ile ondan TÜREYEN her projeksiyon arasında kurulur: adapter map, inspector map, secret env
map, API projeksiyonu. Bir aile eklenip bir projeksiyondan düşerse **kırmızı**.

**5.2 Hedef çözümü.** Tablo: öneksiz değer, kanonik aile öneki, wire alias öneki, **bilinmeyen
önek** (model id sayılmalı), ve fine-tune id'i (`ft:...:org::id` → tamamı model). Son ikisi
§3.2'nin tuzağının regresyon testidir.

**5.3 Red kanıtı.** Connection'ı olmayan aileye yönelen bir istek admission'da 400 alır. Test
projesini kendi kurar ve o connection'ı **kasten yaratmaz** (§3.4).

**5.4 Bit-uyum kanıtı.** Aile route'unkiyle aynı olan bir run'ın config hash'i, bu değişiklikten
ÖNCEKİ hash ile aynıdır. Karşılaştırma önceki sürümün **committed** değerine karşı yapılır, testin
kendi yeniden hesabına karşı değil — kendi baseline'ını yeniden hesaplayan bir no-regression
guard'ı vacuous'tur (E19 T9 kaydı).

**5.5 Koşan selector.** Yeni testler `scripts/test/component`'in `-run` allow-list'ine girer, ya da
untagged kalıp `make verify`'a biner. Hangisi seçilirse **shipped selector koşturulur** ve
`--- PASS` bacakları diff'lenir; allow-list'i okumak kanıt değildir.

---

## 6. Kapsam dışı — kayıt

Bu iki iş yapılacaktır; burada **yapılmıyor**, ve sebepleri kayıtlıdır.

**B — yetenek sözleşmesi.** Her aile ne yapabildiğini beyan etsin; conformance suite family
listesinden türesin; yetenek uyuşmazlığı admission'da reddedilsin. Bugünkü durum ölçülü:

| Conformance | Kapsam |
|---|---|
| canonical tool result | 4/4 |
| usage cache folding | 4/4 |
| mid-stream cancel | 3/4 |
| truncated stream → partial | 2/4 |
| capability filter | 1/4 (yalnız `openai-compatible`) |
| vision / image | **0/4 — conformance'ta yok** |

Vision her adapter'ın kendi paketinde ayrı test edilir (`provider_one/vision_test.go` 117 satır,
`provider_two/vision_test.go` 314 satır): aynı özellik, iki ayrı iddia, ortak sözleşme yok. Ve
`ErrCapabilityUnsupported` yalnız `openai_compatible/adapter.go`'dadır. B bağımsızdır — A/C'yi
beklemez, ama A'nın descriptor'ından faydalanır.

**D — delegation tool'u.** Modele sunulan araçların tam listesi (`main.go:665-687`): math, file,
text_editor, glob, grep, shell, background_kill, media, commit, push, pull_request, merge,
research.fetch, knowledge.retrieve. **Delegation yoktur.** Mekanizmanın tamamı vardır — `childSpec`,
`admitChild`, ChildRun, detach, remote agent — ama modele bir tool olarak sunulmaz;
`orchestrator.go:745-747` şunu yazar: *"Config-driven, so a real single-step run delegates."*

D iki şey ekler: modele sunulan bir delegation tool'u (görev + araçlar + hedef), ve `fast`/`cheap`
gibi **alias**'lar (Cursor'ın `model: fast`'i, AI SDK'nın `customProvider`'ı). Alias tablosunun tek
müşterisi bu tool'dur; C'den önce kurulursa müşterisi olmayan bir dolaylılık olur. D, C'ye bağlıdır:
C olmadan bir subagent aynı ailenin içinde kalır ve `gemini:flash` senaryosu çalışmaz.

---

## 7. Bilinmeyenler

Planın ilk adımında ölçülür, tasarım bunlara bir cevap **varsaymaz**:

1. `model_connections.project_id` NULL olan bir satır bugün var mı? Varsa system-scoped fallback'in
   §3.3'teki yeri nedir?
2. Bir projenin aynı aile için birden fazla connection'ı olabilir mi? Olabiliyorsa §3.3'ün araması
   `ORDER BY` + ikinci-satır-reddi ister — bu ağaç sırasız `LIMIT 1`'in iki güvenlik kararını
   belirlediğini kaydeder.
3. `PALAI_MODEL_PROVIDER`'ı kayıtlı herhangi bir aileye açmak, `deployment.go:1199-1213`'ün
   operatöre söylediği cümleyi değiştirir; o metnin hangi kanıt bundle'ında dondurulduğu ölçülmeli.
