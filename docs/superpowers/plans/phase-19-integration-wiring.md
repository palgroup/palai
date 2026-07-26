# Palai Integration Wiring Plan (E19 — inşa edilmiş adapter'ları GERÇEKTEN çalıştırmak)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (önerilen) veya superpowers:executing-plans ile task-by-task uygula. Adımlar `- [ ]` checkbox'lıdır. **Bu planın tanımlayıcı kuralı: her external contract GERÇEK VENDOR DOKÜMANINDAN grounding alır** (Context7 `resolve-library-id`→`query-docs`, kapsam yoksa resmî dokümanın WebFetch'i) ve kaynak URL'i şartın YANINA yazılır. §3.5 sapma tablosu bu grounding'in çıktısıdır ve her task brief'inin ilk okuması odur.

**Goal:** E17'nin ürettiği ve **mantığı inşa edilmiş, review'dan geçmiş, kanıtlanmış** altı integration yüzeyinden **hiçbir transport'a ya da caller'a bağlı OLMAYANLARI** üretim yoluna bağlamak: Slack'in HTTP + Socket Mode transport'u, A2A push DELIVERY'si, A2A remote-child dispatch'i, queue köprülerinin iki yönü, ve console'un GERÇEK bir control plane'e karşı koşumu. Exit gate: **wiring, discovery'yi ve tier'ı KENDİLİĞİNDEN ilerletmez** — E19'un ürettiği şey "gerçek kontrata karşı DOĞRU kod + credential geldiği an değişmeden koşan live leg"tir; tier flip'i hâlâ §6 operator leglerinin icrasına ve E17 T11 / E18 T10 recompute'una bağlıdır.

**Kapsam sınırı — DÜRÜST TAVAN (E14→E18 geleneğinin devamı; bu planın omurgası):** Bu plan macOS + Docker Desktop oturumunda kod-subagent'larıyla İCRA EDİLİR ve **owner gerçek credential'ları EN SONDA verir**. Bunun üç sonucu vardır ve üçü de pazarlık dışıdır:

- **(a) Doğruluk canlı koşumdan GELEMEZ, dokümandan gelir.** Bir live run yokken "çalışıyor" iddiası imkânsızdır; iddia edilebilecek tek şey "kod, YAYIMLANMIŞ vendor kontratının söylediğini yapıyor"dur. Bu yüzden §3.5 sapma tablosu bu planın en değerli çıktısıdır: E17 T10 zaten bir fake'in gerçek kontrattan SAPABİLDİĞİNİ kanıtladı (fixture'ı, gerçek `approval.requested.v1`'de olmayan bir approval event'i icat etmişti) ve o ders burada mekanikleştirilir.
- **(b) Hiçbir capability bu fazda stable'a FLİP ETMEZ.** `slack`/`a2a`/`queues` preview'da kalır çünkü karşı taraftaki sistemle hâlâ temas edilmemiştir; wiring bu tavanı DEĞİŞTİRMEZ, yalnız §6 legini "credential gelsin, tek oturumda koşsun" haline getirir. Tek istisna `console`'un leg 8'inin YARISIDIR (gerçek `/v1`'e karşı koşum) — üçüncü-taraf credential'ı istemez, bu fazda İCRA EDİLİR, ama manuel screen-reader yarısı durduğu için `console` yine preview kapanır.
- **(c) E08 kuralı yürürlüktedir:** engine gerçek provider'a TOOL AÇMAZ — Slack/queue/A2A üzerinden admit edilen canlı run tek adımlıktır; çok adımlı akışlar fake-engine'le sürülür ve her yerde öyle ADLANDIRILIR.
- **(d) Gerçek broker ürünü, foreign A2A peer'i ve Apple signing YOKTUR** (E17 §6 aynen devralınır) — E19 bunların hiçbirini kapatmaz.

---

## §0 — Owner'ın sağlayacakları (HANDOVER CHECKLIST, kopyala-yapıştır)

Bu bölüm handover'ın kendisidir. Kod bu credential'lar olmadan TAMAMLANIR; aşağıdakiler geldiği an live legler DEĞİŞMEDEN koşar. Hepsi `.env.local`'a yazılır, `set -a` ile source edilir, **asla argv/log/evidence/commit'e girmez**.

### 0.1 Slack — bir workspace + bir app

Slack app'i https://api.slack.com/apps → **Create New App → From scratch** ile açılır, bir test workspace'ine kurulur.

| `.env.local` değişkeni | Nereden alınır | Format | Hangi UAT legi ister |
|---|---|---|---|
| `SLACK_SIGNING_SECRET` | App → **Basic Information → App Credentials → Signing Secret** | 32 hex char | T1, T2 live (Events API + interactivity v0 verify) |
| `SLACK_BOT_TOKEN` | App → **OAuth & Permissions → Bot User OAuth Token** (workspace'e install sonrası) | `xoxb-…` | T2 live (chat.postMessage / chat.update), T1 live (file fetch) |
| `SLACK_APP_TOKEN` | App → **Basic Information → App-Level Tokens → Generate**, scope `connections:write` | `xapp-…` | T3 live (Socket Mode `apps.connections.open`) |
| `SLACK_TEAM_ID` | Workspace admin → about, ya da herhangi bir event payload'ının `team_id`'si | `T…` | Hepsi (`slack_connections` satırının workspace kimliği) |
| `SLACK_TEST_CHANNEL` | Bot'un davet edildiği bir test kanalının ID'si | `C…` | T2 live (posta/edit round-trip), T1 live (mention) |

**Bot token scopes** (App → OAuth & Permissions → **Bot Token Scopes**; kaynak: https://docs.slack.dev/reference/scopes/):

| Scope | Neyi açar | Hangi case |
|---|---|---|
| `app_mentions:read` | `app_mention` event'i | SLK-001, SLK-003 |
| `channels:history` | Bot'un üye olduğu **public** kanallardaki `message` event'leri — **`message_changed`/`message_deleted` AYRI subscription değildir, bu event'in SUBTYPE'larıdır** | SLK-002, SLK-005 |
| `chat:write` | `chat.postMessage` **ve** `chat.update` (ikisi de bu tek scope'la) | SLK-006, SLK-007 |
| `files:read` | Paylaşılan dosyanın içeriğini indirmek | SLK-005 (file→scoped fetch+scan) |
| `users:read` | Slack user id → mapped principal kimlik eşlemesi | SLK-004 (approver allow-list) |
| `groups:history`, `im:history` | **OPSİYONEL** — private kanal / DM yüzeyi istenirse | kapsam dışı, ilk koşumda VERİLMESİN |

> `users:read` vs `users:profile.read` ayrımı, çektiğimiz scope sayfası bir navigasyon indeksi olduğu için **DÜŞÜK GÜVENLİ** kalmıştır. T1'in ilk canlı koşumu bunu kesinleştirir; app-config anında `users:read` ile başlanır, `missing_scope` hatası gelirse `users:profile.read` eklenir. Bu belirsizlik burada AÇIKÇA kayıtlıdır, koda varsayım olarak GİRMEZ.

**Event Subscriptions** (App → Event Subscriptions → **Subscribe to bot events**): `app_mention`, `message.channels`. Başka event YOK — abone olunmayan bir event için kod yazılmaz.

**Interactivity** (App → Interactivity & Shortcuts): AÇIK olmalı — SLK-007'nin approve/deny butonları interactive payload üretir.

**PUBLIC URL GEREKİR Mİ?** İkiye ayrılır ve bu ayrım owner'ın en önemli kararıdır:

- **Socket Mode AÇIKSA (önerilen): HAYIR.** Socket Mode hem event'leri hem interactivity payload'larını WebSocket üzerinden taşır, Request URL alanı UI'dan kaybolur, tünel/ngrok/public DNS gerekmez. `SLACK_APP_TOKEN` yeter. **T3'ün live legi bu yolla koşar ve owner'a hiçbir ağ işi yüklemez.**
- **Events API HTTP transport'u (T1/T2'nin live legi) İÇİN: EVET** — Slack'in POST edebileceği public HTTPS bir Request URL (`…/v1/slack/events`, `…/v1/slack/interactions`) gerekir. Bu yalnız v0 signature verify'ın CANLI kanıtı içindir; Socket Mode bu şemayı hiç kullanmaz (imzasızdır, kaynak: https://docs.slack.dev/apis/events-api/using-socket-mode/). Owner public URL vermek istemezse **T1/T2'nin live legi §6'da kalır, T3'ünki koşar** — kod ve deterministic kanıtlar tam olarak aynıdır.

### 0.2 A2A push delivery — üçüncü taraf GEREKMEZ

Push receiver için local bir HTTPS sink yeter (T4 kendi loopback sink'ini getirir). Owner'dan istenen tek şey, gerçek bir foreign peer'e karşı denemek isterse: `A2A_PUSH_WEBHOOK_URL` (+ opsiyonel `A2A_PUSH_WEBHOOK_TOKEN`). **Yoksa T4'ün deterministic kanıtı tamdır**; foreign peer E17 §6 leg 2'de kalır.

### 0.3 Console — üçüncü taraf GEREKMEZ

`make compose-up` + bootstrap API key. §6 leg 8'in bu yarısı tamamen local'dir; owner'dan hiçbir şey istenmez. T7'nin bu fazda birinci sınıf task olmasının sebebi budur.

### 0.4 Queue — üçüncü taraf GEREKMEZ (ve bu fazda İSTENMEZ)

Referans adapter Postgres'tir. Gerçek broker ürünü E17 §6 leg 5'tir; E19 onu kapatmaz, yalnız köprüyü bağlar.

---

## 1. Yapı kararı — fork noktası, migration, dosyalar

**Fork noktası:** E18 T10 kapanışı (`release-1.0.0-rc1` bundle) `main`'e merge olduktan sonra; execution gate: `main` >= E18 T10 merge tip. E19 master plan'da E18'in "FİNAL epic" tanımından SONRA gelen bir **post-RC wiring epic**'idir ve varlığını E18 T9'un triage tablosuna borçludur: `a2a push delivery` ve `approval richer detail / publications read endpoint` orada **post-1.0** olarak dispoze edilmişti. E19 o dispozisyonları İCRA EDER; `docs/operations/known-gaps-1.0.md`'nin ilgili satırları T8'de `closed-verified`'a çevrilir.

**Migration: YOK — zincir 000040'ta kalır.** Ağaca karşı doğrulanan gerekçe:

- Slack'in çalışması için gereken her sütun **000035'te zaten var** (`signing_secret_ref`, `bot_token_ref`, **`app_token_ref`** — Socket Mode için, `bot_user_id` — self-loop guard, `allowed_users` — SLK-004, `default_policy JSONB` — bağlanacak run target'ı taşıyacak yer) ve `slack_thread_sessions.last_bot_message_ts` repair handle'ı hazırdır.
- Queue'nun ihtiyacı olan her şey **000037'de** (`queue_connections.config JSONB` run target'ı taşır, `queue_messages`/`queue_effect_receipts`/`queue_deliveries` tam).
- A2A push için **000038**'de `push_configs JSONB` vardır.
- **Run target'ı için yeni sütun AÇILMAZ:** hem `slack_connections.default_policy` hem `queue_connections.config` zaten "bu bağlantıdaki event'ler için varsayılan run policy / config" olarak TANIMLIDIR; `agent_revision_id`'yi oradan okumak tam olarak var oluş sebepleridir. Bir sütun eklemek migration-free bir fazı migration'lı yapmanın en pahalı yoludur.

Öngörülemeyen ihtiyaç → **000041**, önce **owner onayı**; o durumda guarded + idempotent + `storage/embed.go` concat + `palai_apply_tenant_policy` (org/project, FORCE RLS) + append-only tabloya self-re-asserting REVOKE kuralları AYNEN, ve yeni tenant tablosu `migration_test.go`'nun `allTables`'ına kaydedilir.

**Files:** `apps/control-plane/api/slack.go` (yeni), `apps/control-plane/internal/extensions/slack_admit.go` + `slack_socket.go` (yeni), `adapters/integrations/a2a/pusher.go` (yeni), `apps/control-plane/internal/automation/queue_bridge.go` (yeni), `apps/control-plane/cmd/palai-control-plane/main.go` (mount'lar), `apps/control-plane/api/capabilities.go` + `a2a.go` + `router.go`, `apps/control-plane/internal/execution/child_dispatch.go`, `apps/web-console/tests/` (gerçek-upstream harness), `tests/uat/wiring/` (yeni — T9 journey evi), `tests/uat/evidence.go`, `tests/uat/cases/` (mevcut case'lere WIRED leg eklenir).

**UAT politikası — YENİ ID AÇILMAZ.** E19 mevcut case'lere **WIRED proof legi** ekler ve honest-ceiling metnini yeniden yazar (E17 T7'nin AUT-009/010'a queue legi ekleme emsali). Bugün SLK-001..008 "hand-composed inbound leg" diyor; E19 sonrası "shipped handler" der ve eski metin FAILING bir iddiaya dönüşmez, DEĞİŞİR. Yeni bir claim gerçekten doğuyorsa (bir yüzeyin mount'a bağlanması) evidence tarafında **`WiringProof`** olarak yaşar, yeni bir UAT ID olarak değil.

---

## 2. Design invariant (task değil, her task'ın kabul şartı)

- **Wiring GERÇEK admission path'inden geçer, paralel bir yoldan ASLA.** Emsal E17 T2'dir ve ağaçta doğrulanmıştır: `api.NewA2AServer` gerçek `Admitter`'ı sarar, `a2aScopeFunc` scope'u YALNIZ authenticate edilmiş bearer'dan okur, ve idempotency namespace'i kendi route sabitiyle (`a2aAdmitRoute`) ayrılır. Her yeni transport bu üç parçayı bire bir tekrarlar. **Bir transport org/project/principal'ı OVERRIDE EDEMEZ** (§38.6 Slack/queue'ya da aynen uygulanır): payload'daki hiçbir alan tenant seçemez.
- **Discovery yalnız MOUNT EDİLENİ ilan eder.** Doğru kalıp `cfg.a2a != nil` ve `workspacesCapability()`'dir; `capabilities.go`'daki statik string'ler değildir. Bir capability'nin tier'ı, o yüzeyin ÇALIŞAN BINARY'de mount edilmiş olmasına bağlıdır (T8'in işi) — ve mount, tier'ı YÜKSELTMEZ, yalnız ilan edilebilir kılar.
- **Wiring tier'ı ilerletmez.** E17 T11'in `CapabilityTierProof`'u ve E18 T10'un `AggregateTierProof`'u tier'ı claim outcome'larından YENİDEN HESAPLAR; `uat.CapabilityOperatorLegs` §6 legi kapanmamış bir capability'yi mekanik olarak preview'da tutar. Hiçbir E19 task'ı kendi tier'ını yazamaz.
- **Kontrat dokümandan gelir, kolaylıktan değil.** Her fake **YAYIMLANMIŞ kontrata** kurulur, bizim işimize gelen şekle değil. Fixture'ın drift etmesi bir REVIEWER'ın yakalayacağı şey olamaz: her fake, kaynak URL'ini + çekim tarihini taşıyan bir `CONTRACT:` yorumu ile satır satır gerekçelenir ve **fake ile gerçek yüzey arasındaki uyum MEKANİK bir testle** karşılaştırılır (T7'nin console sweep'i bunun referans formudur, T1/T3/T4 aynı disiplini kendi fake'lerine uygular).
- **RED-first, faithful fake'e karşı.** Önce doküman, sonra fake, sonra kırmızı test, sonra kod. Dokümanın söylediği ama bizim yapmadığımız her şey (§3.5) bir RED test doğurur.
- **Credential-gated live smoke: ŞİMDİ TASARLA, SONRA KOŞ.** Her live leg `//go:build live` tag'i altındadır (verify'dan tag ile dışlanır) ve credential yoksa **`t.Skip` ile, EKSİK OLAN env değişkeninin adını ve §0'daki hangi satırın onu verdiğini söyleyerek** durur. Skip tercihi bilinçlidir: owner credential'ların BİR KISMINI verdiğinde live tier kısmi-yeşil raporlamalı, topluca kırmızı değil (repo'da fatal-eden live testler de var; onlar tüm tier'ın zorunlu credential'ı içindir, bu leg'ler için doğru davranış skip'tir).
- **Secret'lar handle'dır:** Slack signing secret / bot token / app token yalnız `secret_refs` üzerinden çözülür, çağrı anında, Authorization header'ında; log/argv/evidence/commit'e asla. `slack_connections`'ın strict decode'u zaten inline değeri reddediyor — wiring bu kapıyı GENİŞLETMEZ.
- **Tek retry sahibi (§35.2/§53.4):** her yeni transport hangi sistemin hangi timeout/retry'ı sahiplendiğini YAZAR. Slack 3× retry eder, biz de retry edersek çarpım olur; queue at-least-once redeliver eder, biz de edersek çarpım olur. Retry multiplication testle reddedilir (AUT-013 disiplini).
- **Credential/transport-auth/admission'a dokunan HER task full review alır** (T1..T6, T8).

---

## 3. Doğrulanmış seam envanteri (2026-07-25, ağaca karşı; HEAD `a2f97cc`)

| Seam | Durum (doğrulandı) |
|---|---|
| Slack adapter | `adapters/integrations/slack/`: `signature.go` (v0, `"v0:{ts}:{body}"` basestring, replay-window ÖNCE, `hmac.Equal`), `inbound.go` (`MapEvent`, `ParseChallenge`, `UnwrapSocketFrame`, Kind: message/correction/tombstone/file_share, bot-self `ErrIgnored`), `approval.go` (`ActionApprove`/`ActionDeny`, button `value`=request_hash, raw-form-body uyarısı yorumda), `ratelimit.go` (429→`Retry-After`→bounded repair, `chat.update` handle'ı). **PURE — DB yok, HTTP route yok.** |
| Slack HTTP route | **YOK.** `apps/control-plane/api/` içinde tek `slack` geçen dosya `capabilities.go`'dur (tier string'i) |
| `extensions/slack.go` | `SlackAuthorizationPolicyFor` + `ApproverAuthorized`: **SIFIR non-test caller** (yalnız `slack_journey_component_test.go` + bundle metni). E17 T11 bunu "UNWIRED DECISION PATH" olarak zaten kaydetmiş |
| Socket Mode | Üretim kodunda **connect loop YOK** — `apps.connections.open` çağrısı yok, `disconnect` handling yok, reconnect yok. Yalnız `UnwrapSocketFrame` (envelope decode) var |
| Migration 000035 | `slack_connections` (+`app_token_ref`, `bot_user_id`, `allowed_users`, `default_policy`) + `slack_thread_sessions` (+`last_bot_message_ts`); ikisi de FORCE RLS |
| A2A server | `api/a2a.go` + `main.go:195-199` GERÇEK mount: `Admitter` + `a2aScopeFunc` + `WithA2A` → `cfg.a2a != nil` discovery gating. **Doğru wiring'in referans şekli budur** |
| A2A `Pusher` | Interface tanımlı (`server.go:104`), **`NewA2AServer` nil geçer, sıfır implementasyon**. `effectivePush` kartı DÜRÜST tutuyor (Pusher yoksa `pushNotifications:false`) AMA `pushCollection`/`pushItem` route'ları **KOŞULSUZ mount** — client 200 alarak hiç ateşlenmeyecek bir target register edebiliyor. Migration 000038 default'u `push_notifications = true` |
| `RemoteChildRun` | `adapters/integrations/a2a/remote_child.go:46` VAR; caller'ı yalnız `client_test.go`. `execution/child_dispatch.go:289 dispatchChild` onu TANIMIYOR (dosyanın kendi yorumu "NOT live-wired into orchestrator.dispatchChild" diyor) |
| Queue adapter | `adapters/integrations/queue/` (Handler/Disposition/Depth/Sink contract + Memory) + `automation/queue_store.go` (`PGQueue.Consume`, `PGOutbox.Enqueue/DeliverDue`, `RecordEffect`). **`NewQueueStore`'un SIFIR üretim caller'ı** — `main.go`'da yok, API yüzeyi yok, consumer loop yok |
| Inbound admission | `automation/inbound.go:72 IngestInbound` — sıra: semaphore → trigger resolve → secret resolve → **`webhook.ParseInbound` (PER-MESSAGE HMAC)** → revision pin → backlog gate → durable INSERT (source-dedupe unique index) → inline admit. **Per-message HMAC zorunluluğu, broker-connection'ın auth boundary olduğu queue için OLDUĞU GİBİ kullanılamaz** |
| Console | `apps/web-console/` tam; **her kanıt `tests/fake-control-plane.mjs`'e karşı**. Fake elle yazılmış bir alt küme servis ediyor (`/v1/agents`, `…/revisions`, `/v1/responses`, `/v1/sessions/{id}/events`, `…/commands`, `/v1/responses/{id}`, `…/artifacts`, `/v1/artifacts/{id}/content`) ve bu alt kümeyi gerçek router'a bağlayan MEKANİK hiçbir şey yok |
| Discovery | `capabilities.go`: `a2a` (mount-gated) ve `workspaces` (env-gated) DOĞRU; **`slack`/`queues`/`capability-workers`/`console` statik string** |
| Worker gateway | `internal/workers/gateway.go` VAR ve test edilmiş; **`palai-control-plane/main.go` `internal/workers`'ı IMPORT ETMİYOR** — tek üretim importer'ı `cmd/palai-capability-worker` (worker = CLIENT tarafı). Yani `"capability-workers": "stable"`, gateway'i hiç ayağa kaldırmayan bir binary tarafından ilan ediliyor |
| Evidence | `tests/uat/evidence.go` (`Complete()` + recompute-over-copy anchor disiplini), `promote.go` gate; E18 T8 checksum recompute'u; `uat.CapabilityOperatorLegs` tier tavanı |
| Credentials | `.env.local` iki gerçek provider. **Slack / foreign A2A peer / broker ürünü / Apple signing YOK** |

## 3.5 SAPMA TABLOSU — gerçek doküman × bizim varsayımlarımız (bu planın taç çıktısı)

Her satır: **yayımlanmış kontrat** (kaynak URL'i ile) → **ağaçtaki durum** → **hangi task kapatır**. Kırmızı işaretliler kod değişikliği doğurur.

| # | Gerçek kontrat (kaynak) | Ağaçtaki durum | Task |
|---|---|---|---|
| **D1** | Events API: 3sn içinde 2xx yoksa teslim BAŞARISIZ sayılır ve **3 kez** retry edilir (hemen, +1dk, +5dk). Non-200 yanıta **`x-slack-no-retry: 1`** header'ı konarak retry SUSTURULABİLİR. (https://docs.slack.dev/apis/events-api/) | **`x-slack-no-retry` ağaçta hiç yok.** Poison/malformed bir event'e 400 dönmek onu 3 kez daha çektirir; imza reddi bir retry-amplification yüzeyi olur | **T1** |
| **D2** | Retry hem `x-slack-retry-num` (1..3) hem **`x-slack-retry-reason`** taşır (`http_timeout`, `http_error`, `connection_failed`, `ssl_error`, `too_many_redirects`, `unknown_error`). (aynı kaynak) | Adapter yalnız `HeaderRetryNum` biliyor. `reason` = "geç kaldık" ile "hata döndük" ayrımıdır; 3sn ack bütçesinin gerçekten tutup tutmadığının TEK sinyali odur | **T1** |
| **D3** | `event_id` "tüm workspace'ler genelinde globally unique"tir; **retry'ın AYNI `event_id`'yi taşıdığı resmî sayfada AÇIKÇA YAZMIYOR** (arandı, yok). (aynı kaynak) | `inbound.go:11` bunu OLGU olarak yazıyor ("The retry carries the SAME event_id") ve **tüm dedupe'muz buna dayanıyor**. Bu bir VARSAYIMDIR ve kod içinde öyle etiketlenmelidir | **T1** (varsayım olarak etiketle + live smoke'ta ASSERT et) |
| **D4** | Socket Mode: "her event'i acknowledge etmen gerekir ki Slack retry edip etmeyeceğini bilsin" — ama **saniye cinsinden BİR BÜTÇE ve retry SAYISI resmî sayfada YOK** (arandı). 3 saniye rakamı Events API HTTP ve interactivity için belgelidir, Socket Mode envelope'u için DEĞİL. (https://docs.slack.dev/apis/events-api/using-socket-mode/) | Planlama brief'i "ack-within-3s per envelope" diye bir KURAL varsayıyordu; doküman bunu vermiyor. **Belgelenmemiş bir SLA'yı koda gömmek yasak** — doğru davranış: işten ÖNCE derhal ack, ve belirsizliği yorumda adlandır | **T3** |
| **D5** | Socket Mode WSS URL'i `apps.connections.open` ile (app-level `xapp-` token, `Authorization: Bearer`) alınır, yanıt `{"ok":true,"url":"wss://…?ticket=…"}`; **URL runtime'da üretilir, kalıcı değildir**, `approximate_connection_time` ömrü tahmin ettirir, bağlantı birkaç saatte bir yenilenir. `disconnect` mesajı `{"type":"disconnect","reason":…,"debug_info":{"host":…}}` ve reason'lar: **`warning` (kapanmadan 10 saniye önce uyarı)**, `refresh_requested`, `link_disabled`. **10 eşzamanlı bağlantıya kadar desteklenir** (graceful restart için). İlk mesaj `hello`. (aynı kaynak) | Ağaçta connect loop'un HİÇBİRİ yok. Naif bir tek-socket "kapandı→yeniden bağlan" döngüsü her birkaç saatte event DÜŞÜRÜR: doğru davranış `warning` geldiğinde **eskisini kapatmadan YENİSİNİ açmak** (overlap), sonra eskisini drain edip kapatmaktır | **T3** |
| **D6** | Socket Mode envelope'u: `{"payload":…, "envelope_id":…, "type":…, "accepts_response_payload": <bool>}`; ack `{"envelope_id":…, "payload":…}` şeklinde geri yazılır. (aynı kaynak) | `socketFrame` yalnız `type`/`envelope_id`/`payload` decode ediyor — **`accepts_response_payload` YOK**. Kabul etmeyen bir envelope'a response payload göndermek protokol hatasıdır | **T3** |
| **D7** | Socket Mode envelope'ları **İMZALI DEĞİLDİR**: "pre-authenticated WebSocket üzerinden aldığın için inbound event'leri verify/validate etmene gerek yok" — HTTP'den farklı bir kalıp. (aynı kaynak) | Adapter bunu ZATEN doğru söylüyor (`signature.go:54-56`). **Sapma yok — bu satır, T3'ün imza atlamasının meşru olduğunun dokümantasyon çapasıdır** (yoksa review "verify nerede?" diye haklı olarak reddeder) | T3 (çapa) |
| **D8** | Interactivity payload'ı **`application/x-www-form-urlencoded`** gelir, tek bir **`payload`** parametresinde URL-encoded JSON taşır; **3 saniye içinde 200** zorunludur; `response_url` **30 dakika içinde 5 kez** kullanılabilir. (https://docs.slack.dev/interactivity/handling-user-interaction/) | `approval.go`'nun yorumu form-body uyarısını TAŞIYOR ama bunu uygulayan bir route yok. Ayrıca imzanın RAW form body üzerinde doğrulanıp SONRA `payload` çıkarılması gerektiği Slack'in kendi interactivity sayfasında YAZMIYOR — yalnız verify sayfasındaki "raw request body, before it has been deserialized" cümlesinden çıkar (https://docs.slack.dev/authentication/verifying-requests-from-slack/). **Bu çıkarım koda yorum olarak kaynağıyla yazılır** | **T2** |
| **D9** | v0 imza: basestring **tam olarak** `'v0:' + timestamp + ':' + request_body`, HMAC-SHA256 hex, header değeri **`v0=` PREFİKSLİ**; timestamp yerelden 5 dakikadan uzaksa reddet; karşılaştırma timing-safe. (https://docs.slack.dev/authentication/verifying-requests-from-slack/) | `signature.go` bire bir uyuyor (basestring, `DefaultTolerance = 5 * time.Minute`, replay-window ÖNCE, `hmac.Equal`). **Sapma yok** — T1 yalnız `v0=` prefix'inin uçtan uca korunduğunu RED testle pinler | T1 (regression pin) |
| **D10** | Rate limit: 429 + **`Retry-After` (saniye)**. `chat.postMessage` numaralı bir tier'da DEĞİL, **Special Tier**: **kanal başına saniyede ~1 mesaj**, kısa burst'lere izin var, ayrıca workspace-geneli bir limit var. Events API teslimi de limitli: **workspace/app başına 60 dakikada 30.000 teslim**, aşımda **`app_rate_limited` event'i** gelir. (https://docs.slack.dev/apis/web-api/rate-limits/) | `ratelimit.go` 429/`Retry-After`/bounded repair'i doğru yapıyor ama **kanal-başına PACING yok** — SLK-006'nın "bounded coalesced updates"i tek thread'e saniyede birden fazla yazarsa gerçekte limite çarpar. `app_rate_limited` event'i hiç ele alınmıyor | **T2** |
| **D11** | A2A push: sunucu webhook'a **`StreamResponse`** POST eder (`task` \| `message` \| `statusUpdate` \| `artifactUpdate` içerir) — **full Task DEĞİL**. `PushNotificationConfig`: `url` (zorunlu), `token` (opsiyonel, "client-side validation" için), `authentication`, `id`. **`token`'ın HANGİ HEADER'da taşındığı spec'te BELİRTİLMEMİŞTİR.** (https://a2a-protocol.org/latest/specification/, https://a2a-protocol.org/latest/topics/streaming-and-async/) | `Pusher.Push(ctx, cfg, payload []byte)` payload'ı OPAK alıyor — `StreamResponse` şeklini hiçbir şey pinlemiyor. **Interoperable bir token header'ı YOKTUR: seçtiğimiz header bizim SEÇİMİMİZDİR ve "spec-compliant push" diye adlandırılamaz** | **T4** |
| **D12** | A2A push güvenliği: sunucu client'ın verdiği URL'e **"körü körüne POST ETMEMELİ"** — allowlisting + ownership verification + egress firewall; receiver gelen bildirimin authenticity'sini **"titizlikle doğrulamalı"** (JWT imzası / HMAC / API key); replay koruması için timestamp + **tek kullanımlık id** (JWT `jti` ya da event id). Sunucu webhook'a config'in şemasına göre kimlik sunar (OAuth bearer / API key / HMAC / mTLS). (https://a2a-protocol.org/latest/topics/streaming-and-async/) | Push DELIVERY hiç yok, dolayısıyla bu kontrolların HİÇBİRİ yok. **`packages/egress` bu iş için hazır ve T3'ün (E17) redirect-REVALIDATION kalıbı doğru emsaldir** | **T4** |
| **D13** | — (iç tutarlılık) | `push_notifications` DB default'u `true`, kart `effectivePush` sayesinde `false` diyor, ama **`pushNotificationConfigs` CRUD'u KOŞULSUZ mount** → client "desteklenmiyor" diyen bir karta rağmen 200 ile target register ediyor, hiç ateşlenmiyor. **Sessiz-düşürme yüzeyi** | **T4** (Pusher'ı bağla VE CRUD'u da `Pusher != nil`'e gate'le ki bir daha drift edemesin) |
| **D14** | — (iç tutarlılık) | `"capability-workers": "stable"` — gateway'i mount ETMEYEN bir binary ilan ediyor. `"slack"`/`"queues"` de mount'suz statik. **Discovery-honesty invariant'ının en ağır ihlali ve bir STABLE iddiası** | **T8** |
| **D15** | — (iç tutarlılık) | Console fake'i, gerçek router'a MEKANİK olarak bağlı olmayan elle yazılmış bir alt küme. E17 T10'un icat-edilmiş approval event'i bu sınıfın bir ÖRNEĞİDİR, istisnası değil | **T7** |

---

## 4. Task breakdown

**DAG (cap 3):**
Wave 1: **T1** (Slack Events API route + admission köprüsü), **T4** (A2A push delivery), **T6** (queue köprüleri) — üç ayrık seam.
Wave 2: **T2** (Slack interactivity + exact approval; T1'e bağlı), **T3** (Socket Mode connect loop; T1'e bağlı), **T5** (RemoteChildRun → dispatchChild).
Wave 3: **T7** (console vs GERÇEK control plane), **T8** (discovery honesty + public-API gap dispozisyonları; T1/T3/T4/T6'ya bağlı — neyin mount edildiğini bilmesi gerekir).
Wave 4: **T9** (EXIT gate, hepsine bağlı).

**T2 ve T3 AYNI Slack admission köprüsüne dokunur.** Çakışmayı T1 önler: köprüyü T1 LANDLER, T2/T3 yalnız EKLER. Yine de merge sırası SABİTTİR: **T2 önce, T3 sonra**. Her paralel merge sonrası `go vet -tags="component live" ./...` (parallel-merge tag-verify kuralı) ve case.yaml/tenant-tablosu dokunuşunda `tests/uat/automation` + `tests/security/tenancy` corpora'sı KOŞULUR (re-verify kuralı). Her task RED-first TDD + green milestone başına commit + `git push origin main`.

**SECURITY-CRITICAL (full review): T1, T2, T3, T4, T5, T6, T8.**

### T1 — Slack Events API HTTP route + admission köprüsü (mig yok; SECURITY-CRITICAL)

- [ ] `apps/control-plane/api/slack.go`: `POST /v1/slack/events`. Sıra PAZARLIK DIŞI: **RAW body oku → `ParseChallenge` (url_verification, plaintext challenge echo) → `VerifySignature` (v0, `v0=` prefix, replay-window önce; secret `signing_secret_ref`'ten çözülür) → `MapEvent` → dedupe reserve → 2xx ACK → işi ASENKRON yap.** İş ACK'ten sonra: 3 saniye bütçesi (D1) bir handler'ın senkron admission süresine güvenilerek karşılanamaz.
- [ ] **D1 kapanışı:** her TERMİNAL reddiye (malformed, unknown connection, unauthorized) **`x-slack-no-retry: 1`** ile yanıtlanır — bir poison event'i 3 kez daha çekmek retry-amplification'dır. Geçici hata (DB down) no-retry header'ı ALMAZ. RED-first: terminal-reject fixture'ında header yoksa FAIL.
- [ ] **D2 kapanışı:** `x-slack-retry-reason` decode edilir ve delivery kaydına yazılır; `http_timeout` sayacı ayrı tutulur — 3sn ack bütçesinin tutup tutmadığının tek dürüst göstergesi odur.
- [ ] **D3 kapanışı:** "retry aynı `event_id`'yi taşır" ifadesi koddan OLGU olarak silinir, kaynağıyla birlikte **ASSUMPTION** olarak etiketlenir (resmî sayfa bunu söylemiyor) ve live smoke'ta ASSERT edilir. Yanlış çıkarsa fallback dedupe (event_id+event_time+team_id) follow-up'tır — şimdi İNŞA EDİLMEZ (YAGNI).
- [ ] **Admission köprüsü** (`internal/extensions/slack_admit.go`) — E17 T2 şeklinin bire biri: gerçek `Admitter`, scope YALNIZ çözülen `slack_connections` satırının org/project'inden (payload'daki hiçbir alan tenant seçemez), run target `default_policy`'nin `agent_revision_id`'sinden, idempotency namespace'i **kendi route sabitiyle** (`slackAdmitRoute = "/v1/slack/events"`, `a2aAdmitRoute` emsali) ve idempotency key'i `team_id + event_id`. Dedupe reservation ACK'TEN ÖNCE commit edilir (SLK-001/002).
- [ ] Thread↔session korelasyonu `slack_thread_sessions` üzerinden (SLK-003); bot/self loop-guard `bot_user_id` ile (SLK-008); unmapped user → constrained actor (SLK-004'ün event yarısı); edit→correction / delete→tombstone (SLK-005) — hepsi ZATEN yazılmış adapter fonksiyonlarının ÇAĞRILMASIDIR, yeniden yazılması değil.
- **Seam:** `api/slack.go` + `extensions/slack_admit.go` + `main.go` mount. **UAT:** SLK-001..005, SLK-008 (WIRED leg). **Tier:** DEĞİŞMEZ — `slack` preview.
- **Kanıt (burada koşar):** component-real (gerçek Postgres) + **dokümana kurulmuş fake Slack peer**: url_verification handshake, geçerli/bozuk/stale imza, retry fırtınası (`x-slack-retry-num` 1..3 + reason), poison→no-retry, bot-self event. Fake'in her satırı `CONTRACT:` yorumuyla kaynak URL'ine bağlanır.
- **Live (credential geldiğinde, `//go:build live`, `SLACK_SIGNING_SECRET`+`SLACK_TEAM_ID`+public URL yoksa SKIP):** gerçek workspace'ten bir `app_mention` → tek run; D3 varsayımının assert'i.
- **Honest ceiling:** public URL yoksa live leg §6'da kalır ve T3 (Socket Mode) aynı admission köprüsünü URL'siz kanıtlar. Wiring `slack`'i preview'dan ÇIKARMAZ.

### T2 — Slack interactivity route + exact approval + outbound repair (mig yok; SECURITY-CRITICAL; T1'e bağlı)

- [ ] `POST /v1/slack/interactions`: **RAW form body üzerinde v0 verify → SONRA url-decode + `payload` çıkar** (D8; `application/x-www-form-urlencoded`, tek `payload` parametresi). Ters sıra = imzasız kabul; RED-first: JSON-gövde varsayan bir fixture FAIL etmeli. 3 saniye içinde 200 (D8).
- [ ] **`ApproverAuthorized`'ın İLK GERÇEK CALLER'I:** `MapInteractiveApproval` → `SlackAuthorizationPolicyFor` → `ApproverAuthorized` → `coordinator.ApplyApprovalDecision` (one-shot, request_hash bağlı). Unauthorized clicker REDDEDİLİR ve hiçbir command enqueue edilmez. **E17 T11'in "UNWIRED DECISION PATH" notu bu task'ta silinir** — silme, testin çağrı zincirini uçtan uca sürmesiyle HAK EDİLİR.
- [ ] Outbound repair wiring: `PostMessage`/`chat.update` `bot_token_ref`'ten çözülen token ile; görünür mesajın TEK repair'i `last_bot_message_ts` üzerinden (SLK-006). **D10 kapanışı:** coalesced update'ler **kanal başına ~1/sn**'ye pace edilir (Special Tier, kaynak yanına yazılır) ve `app_rate_limited` event'i log+sayaç olarak ele alınır. `response_url` KULLANILMAZ (30dk/5-kullanım penceresi approval repair'i için yanlış araç) — `chat.update` seçimi gerekçesiyle yorumlanır.
- **Seam:** `api/slack.go` (interactions) + `extensions/slack.go` policy + `packages/coordinator`. **UAT:** SLK-004, SLK-006, SLK-007 (WIRED leg). **Tier:** DEĞİŞMEZ.
- **Kanıt:** component-real: yetkili tık → tam BİR approve command; yetkisiz tık → 0 command + typed reject; yabancı/boş-value buton → `ErrNotApproval`; 429→`Retry-After`→tek repair; rate-limit pacing asserti.
- **Live (`SLACK_BOT_TOKEN`+`SLACK_TEST_CHANNEL` yoksa SKIP):** gerçek kanala approval mesajı post + gerçek buton tıkı + `chat.update` repair'i.
- **Honest ceiling:** Block Kit mesaj üretimi minimum (approve/deny butonu + authoritative detay); zengin approval UI console'un işidir. Enterprise Grid / org-wide install kapsam dışı.

### T3 — Slack Socket Mode connect loop (mig yok; SECURITY-CRITICAL; T1'e bağlı)

- [ ] `internal/extensions/slack_socket.go`: `apps.connections.open` (`Authorization: Bearer <xapp->`, `app_token_ref`'ten çözülür) → `{"ok":true,"url":"wss://…?ticket=…"}` (D5) → WSS dial → `hello` → envelope döngüsü. Supervisor altında koşar (mevcut dispatch-worker/reconciler emsali), SIGTERM'de graceful drain.
- [ ] **D6 kapanışı:** `socketFrame`'e **`accepts_response_payload`** eklenir; ack `{"envelope_id":…}` olarak DERHAL yazılır ve response payload YALNIZ envelope kabul ediyorsa eklenir. **D4 kapanışı:** "3 saniye" belgeli bir Socket Mode SLA'sı DEĞİLDİR — koda sayı gömülmez; ack işten ÖNCE gider (bu, herhangi bir bütçeyi karşılar) ve belirsizlik kaynağıyla yorumlanır.
- [ ] **D5 kapanışı — reconnect DOĞRU biçimde:** `disconnect` reason'ları ele alınır (`warning` = 10 saniyelik ön-uyarı → **eskisini kapatmadan YENİ socket aç, overlap et, sonra eskisini drain edip kapat**; `refresh_requested` aynı; `link_disabled` = kalıcı dur + operator alert). Naif kapan-sonra-bağlan YASAK (event düşürür). Slack 10 eşzamanlı bağlantıya izin verir; overlap bu bütçenin içindedir.
- [ ] **D7 çapası:** Socket Mode envelope'ları imzasızdır ve doğrulanmaz — bu, `signature.go:54-56`'nın zaten söylediği şeydir; kaynak URL'i route yorumuna yazılır ki review "verify nerede?" sorusunu dokümanla kapatabilsin. **Transport auth = app-level token, connect anında.**
- [ ] Envelope → T1'in AYNI admission köprüsü. `type: "events_api"` ve `type: "interactive"` iki transport'ta AYNI canonical identity'yi üretir (SLK-001'in "transport değişimi korelasyon ID'lerini değiştirmez" şartı) — bu, HTTP legi ile Socket Mode leginin AYNI `event_id`'de tek run ürettiği bir testle pinlenir.
- **Seam:** `extensions/slack_socket.go` + T1 köprüsü + supervisor. **UAT:** SLK-001..008 (Socket Mode transport legi). **Tier:** DEĞİŞMEZ.
- **Kanıt:** dokümana kurulmuş **fake WSS sunucusu**: `hello` → envelope'lar → ack doğrulaması → `warning` disconnect → overlap reconnect → event kaybı SIFIR asserti; `link_disabled` → kalıcı dur.
- **Live (`SLACK_APP_TOKEN` yoksa SKIP):** gerçek `apps.connections.open` + gerçek WSS + gerçek mention → tek run. **Public URL GEREKMEZ** — owner'a en ucuz canlı kanıt budur (§0.1).
- **Honest ceiling:** çoklu-workspace fan-out (N connection = N socket) bu fazda TEK connection'la kanıtlanır; connection-pool yönetimi ölçek işidir.

### T4 — A2A push delivery: gerçek `Pusher` (mig yok; SECURITY-CRITICAL)

- [ ] `adapters/integrations/a2a/pusher.go`: `Pusher` implementasyonu, MEVCUT signed outbound webhook sender'ı üzerinden (§38 "push → mevcut signed outbound webhook modeli" — yeni bir delivery makinesi İCAT EDİLMEZ; `webhook_pump` retry/dead-letter kalıbı devralınır).
- [ ] **D11 kapanışı — payload ŞEKLİ pinlenir:** POST edilen gövde bir **`StreamResponse`**'tur (`task` | `message` | `statusUpdate` | `artifactUpdate`), full Task değil; şekil bir tipe bağlanır, `[]byte` opaklığında bırakılmaz. **`token` header'ı bizim SEÇİMİMİZDİR** (spec belirtmiyor) — seçilen header adı kartın/dokümanın yanına yazılır ve **"spec-compliant push" DENMEZ**; "A2A `StreamResponse` payload'ı, uygulamaya özgü token header'ı ile" denir.
- [ ] **D12 kapanışı — SSRF + authenticity:** client'ın verdiği webhook URL'i `packages/egress` policy'sinden geçer, **redirect REVALIDATION ile** (E17 T3'ün A2A client kalıbı bire bir); allowlist + private-range denial; replay koruması için her delivery timestamp + tek kullanımlık id taşır. RED-first: `169.254.169.254`/private-range/redirect-to-private → delivery REDDEDİLİR.
- [ ] **D13 kapanışı:** `pushCollection`/`pushItem` route'ları `Pusher != nil`'e GATE'lenir — kart `pushNotifications:false` derken CRUD'un 200 dönüp sessizce hiç ateşlememesi bir daha mümkün olmaz. `effectivePush` artık gerçekten `true` üretir (Pusher bağlı).
- [ ] Delivery failure raporlaması: başarısız teslim `queue_deliveries`/webhook-pump disiplininde retry+dead-letter'a düşer ve task'ın kendi durumunu BOZMAZ (bir push hatası canonical sonucu silmez — SLK-006 ile aynı invariant).
- **Seam:** `adapters/integrations/a2a/pusher.go` + `api/a2a.go` + webhook sender + `packages/egress`. **UAT:** A2A-003 (delivery legi — bugün config-CRUD yarısı). **Tier:** DEĞİŞMEZ — `a2a` preview.
- **Kanıt:** loopback HTTPS sink'e gerçek delivery + tamper/SSRF/redirect negatifleri + sink-down'da loss-less retry + dead-letter.
- **Honest ceiling:** foreign peer'in bizim token header seçimimizi ANLAYACAĞI iddia EDİLMEZ (spec boşluğu, D11) — interop hâlâ §6 leg 2. JWS/JCS card signing v0 dışı.

### T5 — `RemoteChildRun` → `orchestrator.dispatchChild` (mig yok; SECURITY-CRITICAL)

- [ ] `execution/child_dispatch.go`: bir `child.request` kayıtlı bir `a2a_remote_agents` satırını hedefliyorsa dispatch `RemoteChildRun`'a gider; aksi halde bugünkü inline/detached yolları AYNEN kalır (mevcut iki yol DEĞİŞMEZ — E10 T8 semantiği korunur).
- [ ] E17 T3'ün yapısal garantileri wiring'de KORUNUR ve testle pinlenir: remote çıktı **untrusted** (tool/capability grant edemez), parent credential **INHERIT EDİLEMEZ** (`RemoteSecretResolver` tek bearer kaynağıdır — yapısal, bayrakla değil), remote task id'leri connection-scoped, minimum context aktarılır.
- [ ] `remote_child.go:17` ve `client.go:293`'teki "NOT live-wired" yorumları GÜNCELLENİR; `client.go:293`'ün işaret ettiği `ingestPushedFiles` dalı wiring'le birlikte ele alınır ya da neden ele alınmadığı yazılır.
- **Seam:** `execution/child_dispatch.go` + a2a client. **UAT:** SUB-007 (WIRED leg — bugün fake-engine-driven seam). **Tier:** DEĞİŞMEZ.
- **Kanıt:** fake remote A2A agent'a karşı fake-engine parent run'ı: child dispatch → remote result fold → parent devam; credential-inheritance negatifi; remote-failure'da parent'ın dürüst terminal'i.
- **Honest ceiling:** E08 kuralı — parent run fake-engine'dir (gerçek provider'a tool açılmaz); loopback ≠ interop.

### T6 — Queue köprüleri: consume→admit ve terminal→enqueue (mig yok; SECURITY-CRITICAL)

- [ ] **Inbound köprüsü** (`automation/queue_bridge.go`): `PGQueue.Consume` → `Handler` içinde InboundEvent normalize → **connection-level source-auth admission** → `RecordEffect` (idempotency) → `Ack`. Ack YALNIZ durable kayıt + dedupe commit + admission commit SONRASI (§34.2, contract'ın zaten söylediği).
- [ ] **Connection-level source-auth varyantı — bu task'ın çekirdek kararı:** `IngestInbound` per-message HMAC (`webhook.ParseInbound`) İSTER; bir broker'da auth sınırı **BAĞLANTININ KENDİSİDİR**, mesaj başına MAC yoktur. Bu yüzden queue, `IngestInbound`'u ÇAĞIRMAZ; **admission `Admitter` üzerinden yapılır** (E17 T2 şekli): scope yalnız çözülen `queue_connections` satırının org/project'inden, run target `config` JSONB'sinin `agent_revision_id`'sinden, idempotency key mesajın `idempotency_key`'inden, route sabiti `queueAdmitRoute`. **Payload'daki hiçbir alan tenant/target seçemez** ve `enabled=false` bir connection hiçbir şey admit edemez. Bu ayrım plana ve koda AÇIKÇA yazılır: iki farklı auth modeli iki farklı admission girişi hak eder, bir bayrakla bükülmüş tek giriş değil.
- [ ] **Outbound köprüsü:** run terminal'inde, projede outbound bir `queue_connections` satırı varsa `PGOutbox.Enqueue(destinationKey, payload, maxAttempts)`. Enqueue run'ın terminal transaction'ına bağlanır (publisher down iken sonuç KAYBOLMAZ, §34.5) ve `DeliverDue` pump'ı supervisor altında koşar (`webhook_pump` emsali). Destination idempotency: aynı `destinationKey` iki kez TEK efekt.
- [ ] `NewQueueStore` üretime mount edilir + minimal `/v1/queue-connections` admin yüzeyi (E16 T1 list-envelope kalıbı) — mount olmadan T8 `queues`'u ilan EDEMEZ.
- **Seam:** `automation/queue_bridge.go` + `queue_store.go` + `main.go` + api. **UAT:** AUT-009, AUT-010 (WIRED admission legi), AUT-013 (retry-multiplication yok). **Tier:** DEĞİŞMEZ — `queues` preview (broker ürünü hâlâ yok).
- **Kanıt:** component-real (Postgres referans adapter): lost-ack redelivery → TEK run; flood → bounded buffer + backpressure + depth raporu; poison → dead-letter; terminal→enqueue publisher-down'da loss-less + tek delivery; disabled connection → 0 admission.
- **Honest ceiling:** gerçek broker ürünü YOK (E17 §6 leg 5) — kanıtlanan şey KÖPRÜdür, broker semantiği değil.

### T7 — Console GERÇEK control plane'e karşı + fake-vs-real conformance sweep (mig yok)

- [ ] Console e2e'si **compose stack'e karşı** koşacak ikinci bir profil kazanır: gerçek `/v1`, gerçek bootstrap API key (env'den, argv'den ASLA), gerçek SSE. Mevcut fake profili SİLİNMEZ — deterministic/hızlı katman olarak kalır; **iki profil de aynı spec dosyalarını koşar.**
- [ ] **D15 kapanışı — sapma MEKANİK yakalanır:** fake'in servis ettiği her `(method, path-pattern)`, çalışan gerçek router'dan toplanan yüzeyle karşılaştırılır; fake'te olup gerçekte olmayan bir route (E17 T10'un icat edilmiş approval event'i sınıfı) ve gerçekte olup fake'te farklı şekil dönen bir route **FAIL**'dir. Bu, bir reviewer'ın gözüne değil bir teste bağlanır — planın "fixture drift'i nasıl yakalanır" şartının cevabı budur.
- [ ] Gerçek `approval.requested.v1` yolu uçtan uca sürülür (fake'in scripted event'i değil): gerçek publication → gerçek event → console'un authoritative detay render'ı (UI-002) → gerçek approve command → gerçek terminal. axe + keyboard testleri gerçek profilde de yeşil.
- **Seam:** `apps/web-console/tests/` + compose stack. **UAT:** UI-001, UI-002 (REAL-upstream legi). **Tier:** DEĞİŞMEZ — `console` preview (leg 8'in manuel screen-reader yarısı duruyor) ama **tavanı MADDİ OLARAK daralır** ve case metni bunu tam olarak böyle yazar.
- **Kanıt:** iki profil de yeşil + conformance sweep raporu + gerçek-stack journey transcript'i.
- **Honest ceiling:** manuel VoiceOver/screen-reader pass §6 leg 8'in kalan yarısıdır; deployed (compose değil) bir console iddia EDİLMEZ.

### T8 — Discovery honesty onarımı + public-API gap dispozisyonları (mig yok; SECURITY-CRITICAL; T1/T3/T4/T6'ya bağlı)

- [ ] **D14 kapanışı — `capabilities.go`'daki her statik string mount-türevine çevrilir** (`cfg.a2a != nil` / `workspacesCapability()` kalıbı): `slack` T1/T3 mount'undan, `queues` T6 mount'undan, `capability-workers` **worker Gateway'inin gerçekten mount edilmesinden** türer. **`capability-workers` en ağır satırdır: bugün `stable` ilan ediliyor ve `main.go` `internal/workers`'ı import bile etmiyor.** Doğru düzeltme Gateway'i mount etmektir (yazılmış ve test edilmiş; mount birkaç satır) — mount edilemiyorsa tier DÜŞÜRÜLÜR. Sessiz bırakmak seçenek değildir.
- [ ] **RED-first guard:** mount edilmemiş bir capability'nin ilan edildiği bir fixture FAIL eder. E17 T11 `TestServedCapabilityTiersEqualTheRecompute` ve E18 T10 `AggregateTierProof` bu değişiklikten sonra YENİDEN koşar ve bit-eşit kalır — recompute kaynağı değişmediği için tier'lar DEĞİŞMEMELİDİR; değişiyorsa bu bir bulgudur, bir düzeltme değil.
- [ ] **Public-API gap kararları İCRA EDİLİR** (E18 T9 tablosunun "post-1.0" satırları): **a2a push delivery → T4'te KAPANDI**, satır `closed-verified`. **modelRoutes write-only + list-envelope → `router.go` GET/LIST doğrulanır ve satır `closed-verified`** (E16 T1'de kapanmıştı, yalnız doğrulama satırı kalmıştı). **Approval richer detail + `/v1/publications` read endpoint → burada KARAR VERİLİR:** T7 gerçek kontrata karşı koştuğu için eksiğin ne olduğu artık ölçülmüştür; öneri **minimum publications read endpoint** (approval'ın operation/branch/request_hash'inin ötesinde action/args/destination/risk/expiry) — kapsam owner onayına sunulur, onaysız satır `post-1.0` kalır ve tabloda ÖYLE görünür.
- [ ] **T8a'nın bıraktığı ÜÇ DORMANCY — T8b/T9 sahiplenir (mount edildi ≠ çalışıyor).** Gateway mount edildi ve WRK-001..007'nin dayattığı her şeyi dayatıyor, ama hiçbir şey onu SÜRMÜYOR: (1) `Gateway.IssueEnrollmentToken`'ın operator caller'ı yok (runner karşılığı `PALAI_ENROLLMENT_TOKEN_FILE`) → kimse enroll edemez; (2) `Store.DispatchJob`'un caller'ı yok → enroll olmuş bir worker sonsuza kadar 204 poll eder; (3) `Store.SetWorkerHealth`/`Store.RedispatchForRetry`'ın caller'ı yok → worker-health fence hiç ilerlemez ve süresi geçmiş lease için reaper YOKTUR. Ek iki tavan aynı yerde yaşıyor: gateway'in in-memory `artifacts`/`sessions` map'leri artık bir fixture'ın değil UZUN ÖMÜRLÜ bir process'in (artifact hiç silinmiyor; session expiry kontrol ediliyor ama süpürülmüyor) ve `workers.Gateway`'in `Drain`'i yok — control-plane swap'i her worker'ı 401'ler, `leased` job'lar journal'da kalır. Hepsi `startCapabilityWorkerGateway`'in ceiling listesinde adlandırılmıştır.
- [ ] **T8a'nın posture guard'ı korunur:** capability-worker listener'ı CLEARTEXT'tir (`listenCapabilityWorker` non-loopback bind'i REDDEDER — redeem yanıtı secret VALUE taşır, bearer'ın channel binding'i yoktur). T8b/T9 gerçek bir fleet'i ağ üstünden enroll ettirmek isterse doğru düzeltme guard'ı gevşetmek değil, runner gateway'inin mTLS paritesini (`tls.Listen` + `ClientCAs`, `PALAI_RUNNER_SERVER_CERT/KEY`) bu listener'a vermektir.
- [ ] `docs/operations/known-gaps-1.0.md` satırları güncellenir; her `closed-verified` bir kanıt ID'sine bağlanır.
- **Seam:** `capabilities.go` + `main.go` mount'ları + `docs/operations/known-gaps-1.0.md`. **UAT:** yeni ID yok; `CapabilityTierProof`/`AggregateTierProof` re-run. **Tier:** hiçbir capability YÜKSELMEZ; `capability-workers` mount edilemezse DÜŞER.
- **Kanıt:** mount-türev guard'ı + iki tier recompute'unun bit-eşit yeşili + gap tablosu diff'i.
- **Honest ceiling:** mount, "gerçek dünyada çalıştı" DEMEK DEĞİLDİR — yalnız "bu binary bu yüzeyi servis edebiliyor" demektir. Tier tavanı hâlâ §6'dır.

### T9 — EXIT gate: `integration-wiring-0.1.0` + wiring journey (mig yok)

- [ ] `tests/uat/wiring/` journey: temiz stack → Slack connection register (secret_ref handle'larıyla) → **Socket Mode fake WSS'ten mention → run** → **HTTP transport'tan AYNI event_id → İKİNCİ run YOK** (transport-invariance) → approval mesajı → yetkisiz tık REDDEDİLİR → yetkili tık → durable approve → run terminal → **outbound queue'ya loss-less delivery** → **A2A push StreamResponse'u loopback sink'e ulaşır** → console GERÇEK `/v1`'de aynı timeline'ı gösterir.
- [ ] Mevcut case'lere WIRED legler eklenir (yeni ID YOK): SLK-001..008, A2A-003, SUB-007, AUT-009/010/013, UI-001/002. Her case metni artık LOCAL seam'i **"shipped handler"** olarak adlandırır ve §6 legini AÇIKÇA korur.
- [ ] `tests/uat/evidence.go` yeni proof tipi (`Complete()` gate'li): **`WiringProof`** — her wired yüzey için (a) mount'un ÇALIŞAN stack'ten gözlenmiş olması, (b) admission'ın gerçek `Admitter`'dan geçtiğinin sayacı, (c) transport-invariance sayacı (aynı source event id → tek run), (d) **her external kontrat şartının kaynak URL'i + §3.5 sapma ID'si**. Anti-fabrication: verifier mount'u manifest'in kopyasından değil **koşan stack'in `/v1/capabilities` snapshot'ından ve router yüzeyinden** yeniden doğrular; ilan edilmiş ama mount edilmemiş bir yüzey FAIL'dir.
- [ ] **Credential-gated live leg envanteri:** her live smoke'un (T1/T2/T3 Slack, T4 push) hangi env değişkenini istediği, §0'ın hangi satırından geldiği ve credential yokken NE YAPTIĞI (skip + mesaj) tek tabloda; **owner credential verdiğinde tek komut** (`make uat-wiring-live`) hepsini koşar, hiçbir kod değişmeden.
- [ ] `make uat-wiring` + **`integration-wiring-0.1.0` evidence bundle** (redacted manifest) + `make evidence-verify` 0/0/0/0. `promote.go`'ya **wiring ailesi**: mount-türev guard'ı yeşil olmadan tag REFUSE.
- [ ] **Beklenen kapanış tablosu (verifier yeniden hesaplar; TAHMİN, iddia değil):** `slack`=preview, `a2a`=preview, `queues`=preview, `console`=preview, `knowledge`=stable, `capability-workers`=stable (mount edildiyse), `knowledge-vector`/`apple-build`=disabled. **HİÇBİR TIER İLERLEMEZ** — E19'un çıktısı tier değil, wiring + doküman-doğruluğu + koşmaya hazır live leg'dir.
- **Exit-gate proof'un evi budur. Migration:** yok.
- **Honest ceiling:** bu bundle "gerçek Slack workspace'inde çalıştı" İDDİA ETMEZ (credential henüz yok); iddia ettiği şey **"yayımlanmış vendor kontratına göre doğru, mount edilmiş, ve credential geldiği an değişmeden koşacak"**tır. Bu ayrım bundle'ın adında ve her claim metninde durur.

---

## 5. OUT OF SCOPE (bilinçli dışarıda, adres adresine)

| Kalem | Neden dışarıda | Nerede yaşıyor |
|---|---|---|
| Slack OAuth install akışı (`/slack/install`, `oauth.v2.access`, state) | Owner app'i manuel install eder (§0); çok-workspace self-serve install SaaS işidir | SaaS planı |
| Slack Enterprise Grid / org-wide install | Kurumsal dağıtım akışı (E17 kararı yürürlükte) | SaaS planı |
| Slack slash commands, shortcuts, modals, App Home | Abone olunmayan yüzey; SLK-001..008'in hiçbiri istemiyor | Talep gelirse ayrı task |
| Slack `message.groups` / `message.im` (private + DM) | §0 scope listesi bilinçli olarak public kanalla sınırlı; DM'in ayrı authorization semantiği var | Sonraki iterasyon |
| Retry'ın `event_id`'yi koruduğu doğrulanmazsa fallback dedupe | Doküman sessiz (D3); varsayım etiketlenir + live'da assert edilir. Olmayan bir soruna kod yazmak YAGNI | Live assert kırmızı dönerse follow-up |
| Gerçek broker ürünü (NATS/SQS/PubSub/Kafka) | E17 §6 leg 5 aynen; E19 KÖPRÜyü bağlar, broker'ı değil | §6 leg 5 |
| Foreign A2A peer interop | Token header'ı spec'te yok (D11) — loopback ≠ interop | §6 leg 2 |
| A2A JWS/JCS card signing + push için mTLS/OAuth şemaları | Spec "when trust policy requires"; v0'da bearer + uygulama-özgü token header yeter | Hardening iterasyonu |
| Deployed (compose değil) console + manuel screen-reader pass | T7 leg 8'in yalnız ilk yarısını kapatır | §6 leg 8 kalanı |
| `/v1/publications` read endpoint | T8'de owner onayına sunulur; onaysız `post-1.0` | T8 kararı |
| Apple signing / gerçek Xcode build | E17 §6 leg 3 aynen; E19 dokunmaz | §6 leg 3 |
| Çoklu-workspace Socket Mode connection pool yönetimi | Ölçek işi; tek connection kontratı kanıtlar | Talep gelirse ayrı task |

## 6. Operator legs — gerçek-altyapı bacağı (deferred-but-scripted; kaybolmaz)

E17 §6 ve E18 §6 AYNEN devralınır. E19'un bu tabloya katkısı, leglerin **İCRA MALİYETİNİ** düşürmesidir: kod ve live test'ler hazırdır, eksik olan yalnız credential'dır.

1. **Gerçek Slack workspace — Socket Mode yolu.** `SLACK_APP_TOKEN` + `SLACK_BOT_TOKEN` + `SLACK_TEAM_ID` + `SLACK_TEST_CHANNEL`. **Public URL GEREKMEZ.** T3'ün live legi + T2'nin post/repair legi koşar. → SLK external receipt'lerinin ÇOĞUNLUĞU.
2. **Gerçek Slack workspace — Events API HTTP yolu.** Yukarıdakilere ek `SLACK_SIGNING_SECRET` **ve public HTTPS Request URL**. T1/T2'nin HTTP legi + canlı v0 verify. → SLK'nın kalan external receipt'leri; `slack` stable flip'i ikisinin toplamına bağlıdır ve flip yine T11/T10 verifier'ından geçer.
3. **D3 varsayımının canlı doğrulaması** (retry aynı `event_id`'yi taşıyor mu). Leg 1 veya 2 ile birlikte, ek maliyeti sıfır; kırmızı dönerse fallback dedupe follow-up'ı açılır.
4. **Foreign A2A peer** — push delivery'nin token header seçimimizle çalışıp çalışmadığı da burada ölçülür (D11 spec boşluğu). → `a2a` flip'i.
5. **Gerçek broker ürünü** (E17 leg 5, herhangi bir broker ÜRÜNÜ) → `queues` flip'i.
6. **Deployed console + manuel VoiceOver/screen-reader pass** (E17 leg 8'in T7'den ARTAKALAN yarısı) → `console` flip'i.
7. **E17/E18'in devralınan tüm açık legleri** (Apple signing, pgvector, Temporal, real-model eval quality, gerçek CI/registry/Sigstore, reference-hardware PER, KMS ceremony) — E19 hiçbirine dokunmaz.

## 7. Master plan §8 için önerilen özet blok (owner paste eder)

**UAT ownership:** E19 **YENİ UAT ID AÇMAZ.** Mevcut case'lere WIRED proof legleri ekler ve honest-ceiling metinlerini yeniden yazar: SLK-001..008 (hand-composed inbound leg → **shipped handler**, iki transport'ta: Events API HTTP + Socket Mode), SLK-004/007 (`ApproverAuthorized`'ın İLK gerçek caller'ı — E17 T11'in "UNWIRED DECISION PATH" notu hak edilerek silinir), A2A-003 (config-CRUD → gerçek **delivery**), SUB-007 (materialized seam → `orchestrator.dispatchChild`'a wired), AUT-009/010/013 (queue köprüsünün consume→admit ve terminal→enqueue legleri), UI-001/002 (FAKE upstream → **GERÇEK control plane**). Evidence tarafında tek yeni proof tipi: **`WiringProof`**.

**Exit gate — wiring TIER İLERLETMEZ (E19'un tanımlayıcı kuralı):** `integration-wiring-0.1.0` bundle'ı altı yüzeyin de üretim yoluna bağlandığını kanıtlar — her biri **GERÇEK admission path'inden** (E17 T2'nin `Admitter` + authenticated-scope-only + kendi idempotency namespace'i şekli), hiçbiri paralel bir yoldan; ve `/v1/capabilities`'in her satırı artık statik bir string değil **mount türevi**dir (verifier ilan edilmiş ama mount edilmemiş bir yüzeyi FAIL eder — bugün `capability-workers` `stable` ilan edilirken control plane binary'si worker Gateway'ini hiç mount etmiyor). **Ama HİÇBİR capability stable'a flip etmez:** `slack`/`a2a`/`queues`/`console` preview kapanır, çünkü karşı taraftaki sistemle hâlâ temas edilmemiştir ve tier'ı E17 T11 `CapabilityTierProof` / E18 T10 `AggregateTierProof` claim outcome'larından yeniden hesaplar. **Doğruluk canlı koşumdan değil YAYIMLANMIŞ VENDOR DOKÜMANINDAN gelir** (credential'lar en sonda geliyor): her external kontrat şartı kaynak URL'iyle plana çapalanmıştır ve §3.5 tablosu gerçek kontrat ile fake'lerimiz arasındaki **15 sapmayı** adlandırır — Slack'in `x-slack-no-retry` ve `x-slack-retry-reason` yüzeyleri hiç yoktu, "retry aynı event_id'yi taşır" belgelenmemiş bir VARSAYIMdı, Socket Mode'un "3 saniye ack" kuralı resmî sayfada YOKTU (Events API'de var, Socket Mode'da yok), `apps.connections.open` ticket'ının birkaç saatte bir yenilenmesi ve `warning` disconnect'inin **overlap-reconnect** gerektirmesi hiç ele alınmamıştı, `chat.postMessage` Special Tier'ın kanal-başına-saniyede-1 pacing'i yoktu, A2A push'un `token` header'ı **spec'te tanımlı DEĞİL** (yani "spec-compliant push" denemez) ve `pushNotificationConfigs` CRUD'u kart `false` derken 200 dönüp sessizce hiç ateşlemiyordu. **Bu gate "gerçek Slack'te / foreign A2A peer'iyle / gerçek broker'da çalıştı" İDDİA ETMEZ** — iddia ettiği şey "yayımlanmış kontrata göre doğru, mount edilmiş, ve owner credential'ı verdiği an tek komutla değişmeden koşacak"tır; §0 o credential listesinin kopyala-yapıştır handover'ıdır ve **Socket Mode sayesinde Slack'in canlı kanıtı public URL GEREKTİRMEZ**.
