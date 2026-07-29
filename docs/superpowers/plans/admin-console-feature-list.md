# Palai Admin Console — Özellik Listesi

> **Bu bir plan değil, bir karar belgesidir.** Owner bunu okuyup neyin inşa edileceğine karar verir.
> Her satır 2026-07-29'da `main` @ `2356935`'e karşı ölçüldü ve `dosya.go:satır` ile atıflı.
> Ölçülmemiş hiçbir satır yok; ölçülemeyen her şey **EKSİK** bölümünde ve öyle etiketli.

---

## 1. Tek ekranlık özet

| Soru | Ölçülen cevap |
|---|---|
| Kaç yapılandırma yüzeyi var? | **22** (kalıcı yazma rotası olan kaynak ailesi — §8'de sayım) |
| Konsol kaçını **okuyor**? | **8** (`app/page.tsx`'in 7 paneli + `AgentDiff`'in ajan okuması) |
| Konsol kaçına **yazıyor**? | **0** |
| Konsolun toplam yazma yolu | **BİR TANE**: `POST /v1/sessions/{id}/commands` ile bir publication'ı approve/deny (`ApprovalPanel.tsx:42`) |
| Konsol sayfa sayısı | **2** — `/` ve `/runs` (`app/page.tsx`, `app/runs/page.tsx`) |
| Kontrol düzlemi `/v1` rota sayısı | **112 metod+yol çifti / 83 ayrık yol**, hepsi `apps/control-plane/api/router.go`'da (sayım yöntemi §8) |

**Tek cümlelik cevap — bir insan bugün `curl` olmadan ne YAPAMAZ:**

> Bir depo bağlantısı kuramaz, bir ajan yaratamaz, bir MCP sunucusu bağlayamaz, bir aracı
> onaylayamaz, bir tool set'i yayımlayamaz, ikinci bir model sağlayıcısı ekleyemez, bir bütçe ya da
> kota koyamaz, bir zamanlama kuramaz — **yani ilk run'dan sonraki her yapılandırma işi `curl`'dür.**

**Ve tek cümlelik ikinci cevap — bir insanın bugün `curl` olmadan YAPABİLDİĞİ şey:**

> `palai up` bir kez koşar ve **sıfır `curl` ile gerçek bir run'ı kanıtlayarak** biter
> (`up.go:290` `proveLive` — modelin `fake` olması ya da token sayacının sıfır olması bring-up'ı
> düşürür). Yani "model route'u olmadan hiçbir şey koşmaz" **yanlıştır**; deployment-default route
> `modelBrokerFromEnv` (`apps/control-plane/cmd/palai-control-plane/main.go:639-675`) tarafından
> `PALAI_MODEL_PROVIDER`'dan üretilir.

**Boyut ölçeği (her tabloda kullanılır):**

- **S** — mevcut `Panel` + tek bir form; mevcut rota; yeni paylaşılan bileşen yok.
- **M** — sayfa + çok adımlı akış (yarat → revizyon → yayımla) ya da yeni bir paylaşılan bileşen.
- **L** — **yeni backend rotası gerekir** ya da yeni bir mimari parça (kimlik, oturum) gerekir.

---

## 2. GÜVENLİK — bu bölüm en üstte, çünkü bir özellik listesi değil bir durum tespitidir

Üç olgu, üçü de bugün `main`'e karşı ayrı ayrı ölçüldü. Tek başlarına birer eksiklik; **birlikte bir
sınıf değişimidir.**

### 2.1 Konsolda hiçbir kimlik doğrulaması yok

`apps/web-console/{app,lib,components}` üzerinde `cookie|session|auth|login|password` grep'i
**tek bir kimlik eşleşmesi vermiyor** — bulunan her isabet ya bir Palai *run session* id'si
(`runs/page.tsx:45` `sessionId`, `ApprovalPanel.tsx:32`) ya da "authoritative" kelimesi
(`lib/timeline.ts:6`, `stream/route.ts:86`). Dosya düzeyinde de yok: `apps/web-console/middleware.ts`,
`proxy.ts`, `app/middleware.ts`, `app/proxy.ts` — **dördü de mevcut değil.** Login sayfası yok, cookie
yok, oturum yok.

### 2.2 Relay dört metodu da export ediyor — okuma değil, **yazma** vekili

`apps/web-console/app/api/palai/v1/[...path]/route.ts`:

| Export | Satır |
|---|---|
| `GET` | `:91` |
| `POST` | `:109` |
| `PATCH` | `:115` |
| `DELETE` | `:121` |

Dördü de aynı üç satırla başlıyor: yol çöz, `isPublicApiPath` ile doğrula, `relayJSON` ile ilet.
`isPublicApiPath` (`lib/relay.ts:33-39`) yalnız **şekli** kontrol ediyor — `/v1/` ile başlıyor mu,
`..` var mı, `://` var mı. **Kim olduğunu sormuyor.**

### 2.3 Sunucu tarafındaki anahtar sınırsız yetkili

`Scope.HasScope` (`apps/control-plane/api/middleware/auth.go:31-34`):

```go
func (s Scope) HasScope(capability string) bool {
	if len(s.Scopes) == 0 {
		return true
	}
```

Boş scope seti = **her yetkiye TRUE**. Ve konsolun kendi README'si operatöre tam olarak o anahtarı
export ettiriyor (`apps/web-console/README.md:63`: `export PALAI_API_KEY="$(cat "$PALAI_HOME/api-key")"`),
`docs/operations/admin-cli.md:15-17` de o dosyanın *"the bootstrap key… a full-capability admin/bootstrap
key (empty scope set)"* olduğunu birebir yazıyor. Konsolun kendi DIV ledger'ı bunu gerçek bir stack'te
ölçmüş: `DIV-SHP-003` (`tests/divergences.mjs:84-90`) — *"the bootstrap key's `scopes` is the EMPTY array"*.

### 2.4 Üçünün birlikte anlamı, tek cümlede

> **Konsolun origin'ine ulaşan herkes, bugün, sınırsız yetkili bir anahtarla `/v1`'in tamamına
> POST/PATCH/DELETE yapabilir** — organizasyon yaratabilir, API anahtarı basabilir, bir Slack
> bağlantısını silebilir, bir ajan revizyonu yayımlayabilir. Kimlik yok, denetim izi yok, iptal yok.

Bu risk **E23 ile gelmedi** — relay E17 T10'dan beri dört metodu da export ediyor. Değişen tek şey,
bu listedeki her özelliğin o kapının **arkasındaki yüzeyi büyütecek** olması.

**Sonuç, ve bu bir öneri değil bir sıralama şartıdır:** aşağıdaki hiçbir yazma özelliği, kimlik
kapısı olmadan inşa edilmemelidir. Kimlik kapısının kendisi listedeki ilk satırdır (§3, F0).

> **Bir ölçüm daha, çünkü aksi varsayılabilir:** `/v1/runner/` (`router.go:378`) relay'in şekil
> kontrolünden **geçerdi**, ama üretimde `NewRouter`'a `runner` argümanı **`nil` geçiliyor**
> (`apps/control-plane/cmd/palai-control-plane/main.go:316`), yani rota bu dinleyicide hiç mount
> edilmiyor. Burada bir açık yok — ve bunu yazıyoruz çünkü ölçülmeden bilinemezdi.

---

## 3. ENGELLEYEN — bunlar yapılandırılamadan taze bir self-host ürünün vaadine ulaşamaz

Test: **`palai up`'tan sonra bir yabancı bunu yapamadan ne kaybediyor?** Her satır `palai up`'ın
GERÇEKTEN ne kurduğuna karşı gerekçeli.

| # | Özellik | Bugün `curl` olmadan yapılamayan | Rota(lar) | Boyut |
|---|---|---|---|---|
| **F0** | **Konsol kimliği (login + oturum + CSRF)** | Konsolu bir yazma yüzeyine çevirmek, kimliksiz bir yazma vekilini yayımlamak demek (§2). Bugün yapılamayan şey **konsolu güvenle açmaktır** | YOK — konsolun kendi rotası (`/api/console/login`), `/v1` değil | **L** |
| **F1** | **Depo bağlantısı** — yarat + listele + detay | Palai'nin vaadi kod yazan bir ajandır; depo bağlantısı olmadan **hiçbir coding run'ı yoktur**. `palai up` bunu **yalnız Slack credential'ı varsa** kuruyor (`up.go:1075`, `resolveRepository` yolundan); Slack'siz kurulumda **sıfır** | `POST/GET /v1/repository-bindings`, `GET /v1/repository-bindings/{binding_id}` | **S** |
| **F2** | **Ajan profili + revizyon + yayımla** | `wireSlack` (`up.go:620-635`) Slack credential'ı yoksa **hiçbir şey provision etmeden dönüyor** — yorumu birebir *"a stack with no Slack app provisions nothing at all"*. Yani çoğu kurulumda bu **ikinci** ajan değil **birinci** ajandır | `POST/GET /v1/agents`, `GET /v1/agents/{id}`, `GET/POST /v1/agents/{id}/revisions`, `POST .../revisions/{rev}/publish` | **M** |
| **F3** | **Yaratılan ajanı ÇALIŞTIRMAK** | Konsolun stream rotası `client.responses.stream({input: prompt})` diyor (`app/api/palai/stream/route.ts:38`) — **`agent_revision_id` yok**. Konsol bir ajan yaratabilse bile onu **deneyemez**; yarat→yayımla→çalıştır döngüsü kapanmaz | `POST /v1/responses` (mevcut; eksik olan konsolun gövdeye alan koymaması) | **S** |
| **F4** | **MCP bağlantısı** — kaydet + keşfet + listele | Yabancının dış dünyaya açılan tek kapısı. Bugün beş `curl` çağrısı (`docs/operations/jira-mcp-connection.md:60-95`) ve **biri uygulanamaz** (bkz. F5 / §6 E1) | `POST/GET /v1/mcp-connections`, `GET /v1/mcp-connections/{id}`, `POST /v1/mcp-connections/{id}/discover` | **M** |
| **F5** | **Araç onayı** — keşfedilen draft revizyonları görüp yayımlamak | **BUGÜN PUBLIC API'DEN MÜMKÜN DEĞİL.** `publish` bir `trev_` id'si ister; o id'yi döndüren tek yer create'in yanıtıdır (`store/tools.go` create projeksiyonu). `GET /v1/tools/{id}` **yalnız** `{id, canonical_name, model_visible_name}` döndürüyor (`store/tools.go:95-103` → `toolLineageProjection`); `ListToolRevisions` ağaçta **yok**; `GetToolRevision` store'da var (`internal/extensions/registry.go:301`) ve **HTTP rotası yok**. Shipped runbook'un (c) adımı yazıldığı gibi **uygulanamaz** | Kısmen var (`POST /v1/tools/{id}/revisions/{rev}/publish`) — **okuma tarafı YOK, yeni backend gerekir** (§6 E1) | **L** |
| **F6** | **Tool set'e pinlemek + set'i yayımlamak** | MCP'siz bir tool set'in kullanıcısı yok; MCP'li bir bağlantı tool onayı olmadan hiçbir şey yapmaz. İkisi tek iştir. `$SET_REV_ID` **elde edilebilir** (`GET /v1/tool-sets` set revizyon id'lerini listeliyor, `store/tools.go:123-142`) — ama setin **NE İÇERDİĞİ** hiçbir yerde okunamıyor (§6 E2) | `GET /v1/tool-sets`, `POST /v1/tool-sets/{set}/revisions`, `POST .../revisions/{rev}/publish` | **M** |

**Bu altı satırın toplam yeni backend maliyeti: İKİ okuma rotası** (§6 E1, E2). Geri kalanı mevcut
rotalar üzerine sayfa.

---

## 4. RAHATSIZ EDİCİ — CLI ya da env ile mümkün, ama bir insan bundan nefret eder

| # | Özellik | Bugün `curl` olmadan yapılamayan | Rota(lar) | Boyut |
|---|---|---|---|---|
| **F7** | **Model connection + route + revizyon yayımla** | **Engelleyici DEĞİL** — deployment-default route çalışıyor (`main.go:639-675`). Gerekli olduğu yer: **ikinci sağlayıcı** (Anthropic) ve **proje-başına yönlendirme**. `palai` CLI'ında bunun için **tek bir verb yok** | `POST/GET /v1/model-connections`, `GET /v1/model-connections/{id}`, `POST/GET /v1/model-routes`, `GET /v1/model-routes/{id}`, `GET/POST /v1/model-routes/{id}/revisions`, `GET .../revisions/{rev}`, `POST .../publish` | **M** |
| **F8** | **Secret-ref ADLARINI seçtirme (değer DEĞİL)** | Değer yolu CLI'da ve **oraya ait**: `palai secret create` değeri **stdin'den** okuyor ve loopback POST gövdesinde gönderiyor (`up.go:1680-1695`). Konsolun eksiği, F1/F4/F7'nin formlarında `connection_ref`/`secret_ref` alanının **var olan handle'lardan seçilebilmesi**; serbest metin kutusu bir hatadır | `GET /v1/secret-refs` (mevcut; `compose.yaml:116` `PALAI_SECRET_MASTER_KEY_FILE` sabit yazıyor, yani rota compose'da **mount ediliyor**) | **S** |
| **F9** | **Bütçe ve kota koymak** | Bir self-host'ta harcamayı sınırlayan tek şey. Konsol bugün ne bütçeyi ne kotayı gösteriyor, ne de koyabiliyor. CLI verb'ü **yok** | `POST/GET /v1/budgets`, `POST/GET /v1/quotas` | **S** |
| **F10** | **Proje `config_policy` yazmak** | Proje düzeyi politika (izinli araçlar, model kısıtı) yalnız PATCH ile yazılıyor; CLI'da `palai project set-policy` var (`admin/admin.go` alt komutu) ama JSON'u elle yazmak gerekiyor | `PATCH /v1/projects/{project_id}`, `GET /v1/projects/{project_id}` | **M** |
| **F11** | **Slack bağlantısını onarmak** | `palai up` Slack'i kuruyor ama bir alanı sonradan düzeltmek (ör. eksik `app_token_ref`) daha önce **yalnız ham SQL ile** mümkündü; rotalar var, insan yüzeyi yok. Tam CRUD olan **tek** yapılandırma ailesi | `POST/GET /v1/slack-connections`, `GET/PATCH/DELETE /v1/slack-connections/{connection_id}` | **S** |
| **F12** | **API anahtarı yaratmak/iptal etmek — DAR KAPSAMLA** | CLI kapsıyor (`palai apikey create --scope`), ama §2.3 gereği konsolun kendisine dar kapsamlı bir anahtar üretmek **konsoldan** yapılabilmeli. Panel bugün anahtarları **okuyor** (`app/page.tsx:41-50`), yazamıyor | `POST /v1/api-keys`, `POST /v1/api-keys/{key_id}/revoke` | **S** |
| **F13** | **Organizasyon / proje yaratmak** | CLI dört verb'le kapsıyor ve dokümante (`docs/operations/admin-cli.md`). Panel ikisini de okuyor. İkinci bir yazma yüzeyi **düşük öncelikli** | `POST /v1/organizations`, `POST/GET /v1/projects` | **S** |

---

## 5. API-ONLY — API'de var, insan yüzeyi yok, ve bugün kimse bundan engellenmiyor

Bunlar "ikinci hafta" yüzeyleridir: ürün kendini kanıtladıktan sonra ulaşılır ve o noktada `curl`
kabul edilebilir. Hepsinin **hem yazma hem okuma** rotası var, yani istendiği gün sayfası **S**'dir.

| Kaynak | Rotalar | Neden bugün engellemiyor |
|---|---|---|
| **Knowledge bases + kaynaklar + sorgu** | `POST/GET /v1/knowledge-bases`, `POST/GET /v1/knowledge-bases/{kb}/sources`, `DELETE .../sources/{id}`, `POST .../ingest`, `POST .../query`, `GET .../index-revisions` | Panel zaten listeliyor (`app/page.tsx:88-90`); ingest bir kerelik operatör işi |
| **Skills** | `POST/GET /v1/skills`, `POST /v1/skills/{id}/revisions`, `POST .../revisions/{rev}/enable` | Skill kurulumu URL ile, tek seferlik; **ama revizyon okuması yok** (§6 E5) |
| **Triggers + teslimatlar** | `POST/GET /v1/triggers`, `GET/PATCH /v1/triggers/{id}`, `POST /v1/triggers/{id}/revisions`, `POST .../deliveries`, `GET /v1/trigger-deliveries/{id}` | Otomasyon yüzeyi; ilk kurulumda kimse tetikleyici kurmuyor |
| **Webhook endpoints + teslimatlar** | `POST/GET /v1/webhook-endpoints`, `GET /v1/webhook-deliveries`, `GET /v1/webhook-deliveries/{id}`, `POST .../redeliver` | Dışa bildirim; redeliver bir operatör eylemi ve nadir |
| **Queue connections** | `POST/GET /v1/queue-connections` | Kuyruk köprüsü ileri düzey entegrasyon |
| **Run templates** | `POST /v1/run-templates/{tpl}/revisions`, `POST .../revisions/{rev}/publish` | Profilsiz çalıştırma şablonu; **liste/okuma rotası yok** (§6 E7) |
| **A2A uzak ajanlar** | `/v1/a2a/` (prefix mount, `router.go:274`) + public card `GET /v1/a2a/interfaces/{id}/agent-card.json` | Ajanlar-arası protokol; self-host'un ilk haftasında kimse ikinci bir Palai'ye bağlanmıyor |

---

## 6. EKSİK — hiçbir rota yok, backend işi gerekir

Bunlar **API'nin kendisindeki boşluklardır**. Her satır: ne eksik, kimi engelliyor, maliyeti.

| # | Eksik | Kimi engelliyor | Kanıt | Boyut |
|---|---|---|---|---|
| **E1** | **`GET /v1/tools/{tool_id}/revisions`** (+ tek kayıt GET) | **F5'i tamamen engelliyor**, ve shipped bir runbook'un adımını **uygulanamaz** kılıyor. `jira-mcp-connection.md:77` birebir *"Find the ids with `GET /v1/tools`, then publish each revision"* diyor — ama `GET /v1/tools` revizyon id'si döndürmüyor | `store/tools.go:95-103` yalnız `toolLineageProjection`; `ListToolRevisions` ağaçta yok; `GetToolRevision` store'da var (`internal/extensions/registry.go:301`), rotası yok. **E23 T5 bunu KÖTÜLEŞTİRDİ:** publish artık opsiyonel bir onay gövdesi alıyor (`api/tools.go:69-80`, `approval_required`/`approval_label`) — yani elde edilemeyen bir id'ye artık **daha çok karar** bağlı | **M** (handler + projeksiyon + `ListView` zarfı + tenancy corpus; migration YOK) |
| **E2** | **`GET /v1/tool-sets/{set}/revisions/{id}` — set İÇERİĞİ** | Bir operatör yayımladığı set'in **hangi araçları pinlediğini** hiçbir yerden okuyamıyor. Liste projeksiyonu `{id, object, set, revision_number, digest, status}` (`store/tools.go:134-137`) — pinlenmiş araç dizisi yok | Ağacın kendi `ponytail:` notu bunu zaten kayda geçirmiş: `store/tools.go:121-122` — *"no single-resource GET /v1/tool-sets/{id} … Add one if a console needs it."* **Bir konsol istiyor** | **S** |
| **E3** | **Genel araç onayı için konsol/API karar yüzeyi** | E23 bir MCP write tool'unun **insan onayı** ile geçmesini sağladı; ama karar yüzeyi **YALNIZCA SLACK**. `coordinator.DecideToolApproval`'ın tek üretim çağıranı `internal/extensions/slack_decision.go:283`, o da yalnız `POST /v1/slack/interactions`'tan (`api/slack_interactions.go:201`) besleniyor. **Slack'siz bir self-host'ta gated bir araç çağrısı run'ı park eder, kimseye sormaz ve expiry reaper tarafından serbest bırakılır** | `PendingToolApprovalForSession` coordinator'da var (`packages/coordinator/approvals.go:490`) ve **hiçbir HTTP rotası onu açmıyor**. Konsolun `ApprovalPanel`'i `POST /v1/sessions/{id}/commands` kullanıyor, o da **publication**'a bağlı (`internal/execution/command_pump.go:106-113`, yorumu birebir *"approve/deny transition the session's pending publication"*) | **L** (liste rotası + karar rotası + olay yayını) |
| **E4** | **`GET /v1/schedules` (liste)** | Bir operatör kurduğu zamanlamaların **listesini** göremiyor. `router.go:86-94` create/get-one/patch/pause/resume/delete/occurrences mount ediyor; **liste yok** | Rota tablosunda `GET /v1/schedules` **yok** — yalnız `GET /v1/schedules/{schedule_id}` var | **S** |
| **E5** | **Hooks için HİÇBİR okuma rotası** | `router.go:139-142` **yalnız** `POST /v1/hooks` ve `POST /v1/hooks/{id}/disable` mount ediyor. Bir operatör hangi hook'ların kayıtlı ve etkin olduğunu **hiçbir şekilde** göremiyor — kurduğunu unutursa geri bulamaz | Rota tablosunda hooks için tek bir `GET` yok | **S** |
| **E6** | **Skills okuma yarısı** — `GET /v1/skills/{id}`, `GET /v1/skills/{id}/revisions` | Hangi skill revizyonunun kurulu ve etkin olduğu okunamıyor; `enable` bir revizyon id'si istiyor ve o id yalnız install'ın yanıtında | `GET /v1/skills` (liste) var, tekil GET ve revizyon listesi yok | **S** |
| **E7** | **Run-templates okuma** — liste ve tekil GET | Yalnız `POST .../revisions` ve `POST .../publish` mount edilmiş; hangi şablonların var olduğu okunamıyor | `router.go:60-61` | **S** |
| **E8** | **`GET /v1/knowledge-bases/{kb_id}`** (tekil) | Liste var, tekil yok — bir KB'nin detayına derin bağlantı verilemiyor | Rota tablosu | **S** |
| **E9** | **Webhook endpoint tekil GET + DELETE; queue connection tekil GET + DELETE** | Yanlış kurulmuş bir endpoint/binding **silinemiyor**, yalnız yenisiyle gölgelenebiliyor | `router.go:251-255`, `:287-288` | **S** |
| **E10** | **`GET /v1/publications` + `GET /v1/publications/{id}`** (API-3/API-4) | Bir push'u onaylayan kişi **hangi remote'a ve hangi head SHA'sına** onay verdiğini görmüyor — bugün `operation`/`branch`/`request_hash`, üç alan (`ApprovalPanel.tsx:59-71`) | `docs/operations/known-gaps-1.0.md`'de `API-3`/`API-4` olarak kayıtlı, **E23 T7 tarafından 2026-07-29'da yeniden dosyalandı ve HÂLÂ ONAYLANMADI** (`post-1.0`). Satırlar zaten var — migration yok | **M** — **owner onayı bekliyor** |
| **E11** | **Audit zinciri okuma rotası** | `palai audit` bir CLI verb'ü (`cmd/cli/main.go:52`); `/v1` altında audit için **hiçbir rota yok**. Konsol denetim izini gösteremez | Rota tablosunda audit yok | **M** |
| **E12** | **Ajan / MCP / depo bağlantısı için PATCH ve DELETE** | Konsol yaratır ve okur, **düzeltmez**. Yanlış bir bağlantı yenisiyle gölgelenir, silinmez | `router.go:42-46` (repository-bindings), `:52-61` (agents), `:115-120` (mcp-connections) — hiçbirinde PATCH/DELETE yok | **M** |

---

## 7. YAPILANDIRMA DEĞİL — konsolun göstermesi gereken gözlemlenebilirlik

Owner "bir sürü şey olması lazım" dedi. Yalnız yapılandırmadan ibaret bir konsol ürünün yarısıdır.
Aşağıdaki her satır **var olan bir rotaya** dayanıyor.

| # | Ekran | Ne gösterir | Rota(lar) | Boyut |
|---|---|---|---|---|
| **O1** | **Run listesi** | Bugün **yok**: `/runs` yalnız bir prompt kutusu + canlı stream. Geçmiş run'lar hiçbir yerde listelenmiyor, oysa rota var | `GET /v1/responses`, `GET /v1/responses/{id}` | **S** |
| **O2** | **Run zaman çizelgesi (geçmiş)** | `Timeline` bileşeni var (`components/Timeline.tsx`) ama yalnız **canlı** stream'i besliyor. Biten bir run'ın olay akışı SSE ile yeniden okunabilir | `GET /v1/sessions/{id}/events` | **S** |
| **O3** | **Bekleyen onaylar kuyruğu** | Bugün bir onay **yalnız canlı bir stream'in içinde** görülebiliyor (`runs/page.tsx:163`). Tarayıcı sekmesi kapanırsa onay görünmez. Publication onayları için liste rotası **yok** (E10), araç onayları için karar yüzeyi **yok** (E3) | Publication: **YOK** (E10) · Araç: **YOK** (E3) | **L** |
| **O4** | **Kullanım özeti + defter** | Kim ne harcadı. Konsol bugün ikisini de göstermiyor | `GET /v1/usage`, `GET /v1/usage/ledger` | **S** |
| **O5** | **Bütçe / kota durumu** | Konulan limit ve ona karşı durum — F9'un okuma yarısı | `GET /v1/budgets`, `GET /v1/quotas` | **S** |
| **O6** | **Artefakt tarayıcısı** | Bir run'ın ürettiği dosyalar. Rotalar var ve indirme yolu **zaten sertleştirilmiş** (`route.ts:63-77` — tip zorlama, `nosniff`, `attachment`, sanitize edilmiş dosya adı, `default-src 'none'; sandbox`) | `GET /v1/responses/{id}/artifacts`, `GET /v1/artifacts/{id}`, `GET /v1/artifacts/{id}/content` | **S** |
| **O7** | **Webhook teslimat durumu + redeliver** | Başarısız teslimatlar ve bir düğmeyle yeniden gönderim | `GET /v1/webhook-deliveries`, `GET /v1/webhook-deliveries/{id}`, `POST .../redeliver` | **S** |
| **O8** | **Zamanlama çalışma geçmişi** | Bir zamanlamanın gerçekten çalışıp çalışmadığı. **Liste rotası olmadığı için** (E4) yalnız id bilinen bir zamanlama için | `GET /v1/schedules/{id}/occurrences` | **S** |
| **O9** | **Yetenek (capability) tablosu** | Bu deployment neyi sunuyor, hangi tier'da. `palai up` bunu terminale basıyor (`up.go:1203` `capabilityRows`); konsolda yok | `GET /v1/capabilities` | **S** |
| **O10** | **Bilgi tabanı indeks geçmişi** | Bir KB'nin ne zaman yeniden indeklendiği | `GET /v1/knowledge-bases/{kb}/index-revisions` | **S** |
| **O11** | **Trigger teslimat izleme** | Bir tetikleyicinin çalışıp çalışmadığı | `GET /v1/trigger-deliveries/{id}` | **S** |
| **O12** | **Liste kırpması görünür olmalı** *(çapraz kesen)* | `Panel.tsx:34-36` `body.data`'yı okuyup **`has_more`'u yok sayıyor**; `pagination.go:28` `defaultPageLimit = 20`. **Yirmi birinci satır yok gibi görünüyor.** Sessiz kesme bir yalandır ve yukarıdaki HER liste bundan etkileniyor | `renderPage` `has_more` **ve** `next_cursor` yazıyor; `?before=` **400 ile reddediliyor** → ileri sayfalama var, "önceki" düğmesi yapısal olarak **yok** | **S** |

---

## 8. Sayım — nasıl saydım, neyi hariç tuttum

**Rota sayımı.** Komut:

```
grep -E '\.(Handle|HandleFunc)\("(GET|POST|PATCH|DELETE|PUT) /v1' apps/control-plane/api/*.go
```

`_test.go` dosyaları hariç tutuldu. **112 isabetin tamamı tek bir dosyada**: `apps/control-plane/api/router.go`.
Bu 112 **metod+yol çifti**, **83 ayrık yol**. Metod dağılımı: 53 GET, 52 POST, 4 PATCH, 3 DELETE.
(*"~55 rota" rakamı büyük olasılıkla yalnız GET'lerin sayısıdır — 53.*)

**Metodsuz prefix mount'lar (yukarıdaki 112'ye dahil değil):** `mux.Handle("/v1/a2a/", …)`
(`router.go:274`, auth'un İÇİNDE) ve `top.Handle("/v1/runner/", …)` (`router.go:378`, üretimde `nil`
geçildiği için mount edilmiyor — `main.go:316`).

**Admin yüzeyinden hariç tuttuklarım ve gerekçeleri** — beşi de kimliksiz `top` mux'ta ve bir insanın
tıklayacağı şeyler değil, imza doğrulayan alıcılar:

| Yol | Neden admin yüzeyi değil |
|---|---|
| `POST /v1/slack/events` | Slack Events API alıcısı; auth'u v0 imzası (`router.go:357-360`) |
| `POST /v1/slack/interactions` | Slack etkileşim alıcısı; aynı imza (`router.go:365-368`) |
| `POST /v1/inbound/{trigger_id}` | İmzalı inbound webhook alıcısı; auth'u per-source HMAC (`router.go:340-343`) |
| `POST /v1/tool-callbacks/{operation_id}` | İmzalı uzak-araç sonuç callback'i; tek kullanımlık token (`router.go:349-351`) |
| `GET /v1/a2a/interfaces/{id}/agent-card.json` | Public A2A kartı; kimliksiz yayımlanmış projeksiyon (`router.go:374-376`) |

Yani **relay üzerinden bir insanın adresleyebileceği admin yüzeyi: 112 − 5 = 107 metod+yol çifti.**

**"22 yapılandırma yüzeyi" sayımı.** En az bir **kalıcı yapılandırma yazma** rotası olan kaynak ailesi
(bir run/response/session eylemi değil): repository-bindings, agents, run-templates, triggers, schedules,
tools, tool-sets, mcp-connections, skills, hooks, organizations, projects, api-keys, secret-refs,
knowledge-bases, budgets, quotas, model-connections, model-routes, webhook-endpoints, queue-connections,
slack-connections. **Hariç:** responses, sessions, artifacts, usage, capabilities, a2a (yapılandırma
değil, çalışma yüzeyi).

**Konsolun okuduğu 8:** organizations, projects, api-keys, model-connections, model-routes, secret-refs,
knowledge-bases (`app/page.tsx`'in yedi `Panel`'i) + agents (`components/AgentDiff.tsx`, ilk ajanın
revizyonlarını çekiyor).

---

## 9. Owner'ın KARAR VERMESİ gerekenler — cevabın maliyetiyle birlikte

Bunlar öneri değil, sorudur. Her seçeneğin maliyeti ölçülmüş haliyle yanında.

### K1 — Konsol parolasız açılabilmeli mi? (§2'nin doğrudan sonucu)

| Cevap | Maliyeti |
|---|---|
| **Hayır, fail-closed** (`PALAI_CONSOLE_PASSWORD_HASH` yoksa süreç servis etmez) | Bir kurulum adımı daha. Karşılığında: yanlış yapılandırmanın **sessiz hali AÇIK olamaz**. `lib/palai.ts:30-39`'un `requiredEnv` deseni zaten aynı davranışı `PALAI_API_KEY` için uyguluyor — yani desen ağaçta zaten var |
| **Evet, uyarı yeterli** | Bugünkü durum devam eder: yayımlanmış bir konsol imajı, sınırsız yetkili bir anahtar tutan kimliksiz bir yazma vekilidir (§2.4) |

### K2 — Konsol tek operatörlü mü, çok kullanıcılı mı?

| Cevap | Maliyeti |
|---|---|
| **Tek operatör** | Bir env değişkeni + imzalı cookie. Kullanıcı tablosu yok, migration yok. **Tavan:** rol yok, denetim izi yok, oturum iptali yok — kim ne yaptı kaydedilmez |
| **Çok kullanıcı** | Bir kullanıcı tablosu, davet akışı, rol modeli, oturum iptali. Migration zinciri `000043`'ten ileri gider. Bu bir SaaS kapsamıdır |

### K3 — Konsol nereye deploy edilir?

| Cevap | Maliyeti |
|---|---|
| **Ayrı deployable kalır** | Değişiklik yok. `capabilities_mount_test.go` tier türetmesini birebir buna dayandırıyor: *"apps/web-console is a separate deployable; this binary serves no console route to derive it from"* |
| **`deploy/compose`'a servis olarak girer** | Tier türetmesi değişir; `console` capability'sinin gerekçesi yeniden yazılır. Karşılığında: `palai up` konsolu da ayağa kaldırır ve operatör tek komutla bir ekrana kavuşur |

### K4 — E3 (Slack'siz araç onayı) açılıyor mu? — **YENİ, ve §9'un en pahalı satırı**

E23 bir araç çağrısına insan onayı ekledi, **ama karar yüzeyi yalnız Slack**. Bu, "Slack'siz self-host"
senaryosunda gated bir aracın **kimseye sorulmadan expiry ile serbest bırakılması** demek.

| Cevap | Maliyeti |
|---|---|
| **Açılır** | Bekleyen onayları listeleyen bir rota + bir karar rotası + konsol ekranı. `PendingToolApprovalForSession` (`packages/coordinator/approvals.go:490`) ve `DecideToolApproval` (`:217`) **zaten var** — eksik olan HTTP yüzeyi. Migration muhtemelen yok |
| **Açılmaz** | Slack, gated araç kullanmak isteyen her self-host için **zorunlu bir bağımlılık** olur, ve bu bir yerde açıkça yazılmalıdır |

### K5 — E10 (API-3/API-4, onay detayı) onaylanıyor mu? — **hâlâ açık, üçüncü kez dosyalandı**

`known-gaps-1.0.md` her iki satırı da `post-1.0` ve *"NOT approved"* diyor; **E23 T7 2026-07-29'da
ikisini de yeniden dosyaladı ve ikisi de onaylanmadı**. E23'ün eklediği yeni gerekçe: onay artık
publication'a değil **herhangi bir araç çağrısına** uygulanıyor, yani gösterilecek alan sayısı
beşten **en az yediye** çıktı.

| Cevap | Maliyeti |
|---|---|
| **Onaylanır** | Bir rota çifti + `ListView` zarfı + RLS-kapsamlı iki sorgu + tenancy corpus satırı + `UI-002`'nin yeniden kanıtı. **Migration YOK** — `publications` tablosu `remote`, `branch`, `base`, `head_sha`, `args`, `display`, `operation`, `state` alanlarını zaten taşıyor |
| **Onaylanmaz** | Konsol bugünkü dürüst render'ını korur: bir onaylayan `operation`/`branch`/`request_hash` görür, hangi remote'a ve hangi SHA'ya onay verdiğini görmez |

---

## 10. `phase-23-admin-console.md`'de BAYATLAYANLAR (plan 2026-07-28 / `36e7623`; ölçüm 2026-07-29 / `2356935`)

Planı okuyan biri şu beş satıra güvenmemeli:

| Planın satırı | Bugünkü ölçüm |
|---|---|
| §3 "DIV ledger — **24 satır**, 7 kind" | **28 satır**, 7 kind (`grep -cE '^    id: "' tests/divergences.mjs` → 28). Dağılım: DAT 3, EVT 5, RTE 3, SHP 7, STA 1, UI 4, UNX 5 |
| §3.6 D13 "`GetToolRevision` store'da var (`extensions/registry.go:240`)" | Doğru ama satır kaydı: **`registry.go:301`** (E23 T5 dosyayı büyüttü). **İddianın kendisi HÂLÂ GEÇERLİ:** rotası yok, `ListToolRevisions` yok |
| §3 "`GET /v1/tool-sets` … tek-kaynak GET'in yokluğu `store/tools.go:116-117`'de kayıtlı" | Aynı `ponytail:` notu, **`store/tools.go:121-122`**'de |
| §3.6 D20 / §3 "`api/capabilities.go:59` `console` → preview" | `console` hâlâ **preview** ve `capabilities_mount_test.go:42`'nin gerekçesi birebir duruyor; ama dosya E23 T7'de değişti: `apple-build` gerekçesi yeniden yazıldı (D7 düzeltmesi), yani satır numaraları kaydı. `console` satırının kendisi dokunulmadı |
| §3 "checksum sweep — `committedBundleSurfaces` **20 kayıt**" | E23 `tool-approval-0.1.0` bundle'ını ekledi; sayı artmış durumda — yeni bir bundle planlayan biri güncel sayıyı yeniden ölçmeli |

**Ve planın DEĞİŞMEDEN duran en önemli üç ölçümü** (bugün yeniden doğrulandı):

- `apps/web-console` **`36e7623`'ten beri tek satır değişmedi** — konsol iki sayfa, beş bileşen, sıfır kimlik.
- `apps/control-plane/api/router.go` **de değişmedi** — E23 T1-T8 **tek bir yeni `/v1` rotası açmadı**;
  onay ceremonisi var olan `publish` çağrısının opsiyonel gövdesine bindi (`api/tools.go:69-80`).
- §3.6 D3 (deployment-default model route) hâlâ doğru, §3.6 D4 (Slack'siz kurulumda sıfır ajan) hâlâ
  doğru, §3.6 D13 (tool revision id'si okunamıyor) hâlâ doğru.

**Ve planın bilmediği bir şey:** E23, `HIL-P8`'i (*"gated bir araç çağrısının üretim karar yüzeyi yok"*)
kapattı — **ama Slack'e kapattı**. Bu, plan yazıldığında var olmayan yeni bir EKSİK'tir (§6 E3) ve
§9 K4'ün konusudur.
