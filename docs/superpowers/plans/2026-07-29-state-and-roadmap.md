# Palai — durum ve yol haritası (2026-07-29)

> **Bu belge bir konuşmanın tortusudur.** 29 Temmuz günü konuşulan her şey — ölçümler, kararlar,
> düzeltilen inançlar, açık kalan sorular — buraya toplandı ki hiçbiri kaybolmasın. Kalan işlerin
> planlaması bunun üzerinden yapılacak.
>
> Ölçümler `main` @ `34890e8`'e karşı. Her iddia `dosya:satır` ile atıflı; atıfsız cümle yok.

---

## SONRASI (2026-07-31) — bu belge bir KAYITTIR, güncel durum değildir

Bu belge 29 Temmuz gecesinin tortusudur ve **o hâliyle bırakılmıştır**; aşağıdaki §8 "şu an koşan"
tablosu o geceye aittir. Sonrasında kapanan:

- **E24 runner fleet** (8/8, `runner-fleet-0.1.0`) — §5'in "tek runner" ve §8'in "filo" satırları KAPANDI.
- **E25 admin console** (9/9 + tasarım geçişi, `admin-console-0.1.0`) — §4'ün "konsol 22 yüzeyin 0'ına
  yazıyor" ve §4'ün güvenlik bölümü KAPANDI: konsolda artık kimlik kapısı var.
- **E26 background execution** (7/7, `background-execution-0.1.0`) — bu belgede henüz yoktu.
- **Cloud smoke'un üç bulgusu** (§3'ün 3/5/6'sı) düzeltildi; shell wall-time'ına ölçülmüş bir varsayılan
  geldi (10m).

**Hâlâ geçerli olan:** §3'ün yürünemeyen bacakları (özellikle **`linux/amd64` doğrulanmadı** — bu
makineden kapatılamayan tek boşluk), §5'in "bir runner tool koşturmuyor" ölçümü (E24 T7 ertelendi), ve
§8'in owner'a bağlı satırları (Azure, yayın, API-3/API-4).

**Bu belgeden sonra ortaya çıkan ve burada OLMAYAN boşluk:** E24'ün filosunun ve `config_policy`'nin
konsolda ekranı yok — E25'in planı E24 var olmadan yazıldı.

---

## 0. İsimlendirme — sabitlendi

| İsim | Ne | Bugünkü karşılığı |
|---|---|---|
| **admin panel** | İnsanın ajanları, tool'ları, environment'ları yapılandırdığı web yüzeyi | `apps/web-console` — **2 sayfa** (`/`, `/runs`), 0 yazma |
| **control plane** | API'lerin koştuğu, session başlatan, SDK'in konuştuğu yer | `apps/control-plane` — **112 metod+yol / 83 ayrık yol** |
| **agent runner** | Başlarken control plane'e register olan, iş atamasında session koşturan makine | `packages/runner` + `execution.RunnerGateway` — **tek runner** |

---

## 1. Bugün ne oldu

**E23 (tool approval) kapandı — 8/8.** `main` @ `2356935` → `34890e8`.

- **T6 (merge):** owner'ın "merge de ettirebilir" isteği. Üçüncü yan etki gerçekten bir `switch case` + T1'in migration'ının zaten genişlettiği bir `CHECK` değeri çıktı. `sha` **zorunlu** tutuldu (GitHub opsiyonel işaretliyor ama uyuşmazlığa 409 dönüyor) — kaymış branch red alıyor. Merge tool'u **hiç argüman almıyor**.
- **T7 (exit gate):** planın üç iddiasını çürüttü — D7'nin **üç** kopyası vardı (plan ikisini saymış), D12'nin fiyatı **sıfırdı** (ölçüldü), ve `HIL-` hiçbir prefix listesinde değildi, yani T2'nin gönderdiği `HIL-004` ağaçtaki tek case-süpürmesinden kaçıyordu. Ayrıca kapının vacuous-olmama testinin component tier'ında **hiç koşmadığını** buldu.
- **T8 (benim eklediğim):** T7'nin `known-gaps`'e yazdığı `HIL-P8`'i **doldurdu**.

**Üç araştırma/ölçüm işi koştu:** cloud smoke, admin panel özellik listesi, ve Anthropic'in self-hosted mimarisi.

---

## 2. Günün en pahalı bulgusu — ve şekli tekrarlıyor

**`HIL-P8`:** `slack.ToolApprovalMessage` ve `coordinator.DecideToolApproval`'ın **hiçbir production caller'ı yoktu.** T1 kapıyı, T4 üç butonlu argüman tablosunu, T5 MCP yazma tool'larını yapmıştı — hiçbiri birbirine bağlı değildi. Gated bir çağrı run'ını park ediyor, kimseye sormuyor, yarım saat sonra expiry reaper'ıyla ölüyordu.

**Fail-CLOSED olduğu için altı task ve her yeşil test bunu kaçırdı.**

Ve kapattıktan **sonra** ölçüldü ki delik bir katman daha dışarıda devam ediyor: `DecideToolApproval`'ın tek caller'ı `slack_decision.go:283`, oraya giren tek yol `POST /v1/slack/interactions` (`router.go:367`). **Slack'siz bir self-host'ta hâlâ kimse onaylayamıyor.** → şu an implement ediliyor.

### Aynı şeklin diğer örnekleri, bugün ölçülenler

| Ne | Nerede | Şekli |
|---|---|---|
| `WorkerSpec.OS/Arch/PoolLabel/Capacity` | `workers/types.go:52-61` | **Yazılıyor, geri okunuyor, hiçbir kararda kullanılmıyor.** Claim predikatı (`workers.sql:111`) sadece `capability = $3` |
| `/healthz` sağlık kontrolü | `install.md` (eski) + `production.yml:97` | Edge sadece `/v1/*` proxy'liyor (bilerek), yani probe Caddy'nin boş cevabına çarpıyor: **200, 0 byte, control-plane ölüyken bile** |
| `TestToolWithoutApprovalDeclaredIsBitUnchanged` | `scripts/test/component` | Go `-run` **ismi** eşler; allow-list'teki sekiz alternatifin hiçbiri bu ismi tutmuyordu → test **hiç koşmadı** |
| `destinationFieldTokens` | E22 sweep | Merge'ün `pull_request_number`'ını tanımıyordu |
| `PALAI_RUNNER_CONCURRENCY` | (E21'de bulundu) | Hiç export edilmiyordu — dispatch=4, runner=1 |

**Kural:** bir epic'in çıkış kapısında, o epic'in **yeni exported sembollerini production caller'a karşı süz**. Fail-closed delikler davranış testlerine görünmez.

---

## 3. Cloud smoke — ölçülmüş hüküm

`install.md` hiç görülmemiş gibi harfiyen takip edildi. **8 sapma.**

**Çalışan:** beş servis 27 saniyede healthy, TLS edge tek yayınlanan yüzey, `/metrics` gerçekten kapalı, **gerçek bir OpenAI run'ı uçtan uca**, `backup → restore → verify` altı kontrolü yeşil, backup öncesi run restore sonrası edge üzerinden okunabiliyor.

**Kırık — üçü de dokümanın kendi mutlu yolunda:**

| # | Ne | Önem | Durum |
|---|---|---|---|
| 1 | Sağlık kontrolü **vakum** — ölü stack'e yeşil | Yüksek | Düzeltildi (`/v1/capabilities`) |
| 2 | `PALAI_COMPOSE_PROJECT` ≠ `config.json`'daki isim → **backup komple ölü**, stack sağlıklı görünürken | Yüksek | Düzeltildi (doküman) |
| 3 | Edge sertifikası runner gateway ile **paylaşılıyor**; runner tam **tek SAN** pinliyor (`enrollment.go:142`) → gerçek alan adı sertifikası runner'ı **gecikmeli** öldürüyor | **Kritik** | Düzeltiliyor |
| 4 | `up -d edge` sertifikayı reload etmiyor | Orta | Düzeltildi |
| 5 | Production stack için **sağlık komutu yok** — `doctor` host portlarına bakıyor, overlay yayınlamıyor | Orta | Düzeltiliyor |
| 6 | Disk kontrolü **oran** tabanlı (`free/total < %10`) — 20 GB VM'de 2 GB **yeşil** | Orta | Düzeltiliyor |
| 7 | İmajların nasıl üretileceği yazmıyor | Düşük | Düzeltildi |
| 8 | `docker compose config` 4 uyarı basıyor | Düşük | Filed |

**Bulgu 3'ün mekaniği** — çünkü tekrar okunacak: edge (`production.yml:70-71`) ve runner gateway (`compose.yaml:61-62`) **aynı dosyayı** kullanıyor; runner `len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != config.ControllerDNS` diyor. Değişimden **hemen sonra her şey sağlıklı**; kırılma bir sonraki control-plane restart'ında (yani `restart: always` ile ilk VM reboot'unda); semptom *"run'lar sonsuza kadar queued kalıyor"*; hiçbir mesajda "certificate" geçmiyor.

**Yürünemeyen 9 bacak**, adıyla sayılı. En önemlisi: **`linux/amd64` doğrulanmadı** (makine disiplini qemu'yu yasakladı). İmajları qemu'suz *üretmek* mümkün; *koşturmak* gerçek bir amd64 host istiyor. **Bu makineden kapatılamayacak tek boşluk.**

---

## 4. Admin panel — ölçülmüş boşluk

| Soru | Cevap |
|---|---|
| Yapılandırma yüzeyi | **22** |
| Konsolun okuduğu | **8** |
| Konsolun **yazdığı** | **0** |
| Toplam yazma yolu | **bir tane** — bir publication'ı approve/deny |

> **`curl` olmadan yapılamayan:** depo bağlantısı, ajan yaratma, MCP sunucusu bağlama, araç onaylama, tool set yayımlama, ikinci model sağlayıcı, bütçe/kota, zamanlama. **İlk run'dan sonraki her yapılandırma işi `curl`.**

Dürüst öbür yarı: `palai up` **sıfır `curl` ile gerçek bir run kanıtlayarak** bitiyor (`up.go:290` `proveLive` — model `fake` ise ya da token sayacı sıfırsa bring-up **düşüyor**). "Model route olmadan hiçbir şey koşmaz" inancı **yanlış**.

### Güvenlik — üç olgu, birlikte bir sınıf değişimi

1. `apps/web-console/{app,lib,components}` — **hiçbir kimlik doğrulaması yok.** `middleware.ts` yok, login yok; her `auth`/`session`/`cookie` isabeti ya bir Palai *run session* id'si ya "authoritative" kelimesi.
2. Relay `GET`, **`POST`, `PATCH`, `DELETE`** export ediyor.
3. `Scope.HasScope` (`auth.go:30`) `len(s.Scopes) == 0` iken **her yetkiye `true`** — README operatöre tam da o anahtarı export ettiriyor.

**Sonuç:** konsolun origin'ine ulaşan herkes bugün sınırsız yetkili bir anahtarla `POST /api/palai/v1/organizations` çağırabilir. Lokalde sorun değil; **yayımlanmış bir konsol imajı için kimliksiz bir yazma vekilidir.**

### En sinsi UI hatası

**O12:** `Panel.tsx:34-36` `body.data`'yı okuyup **`has_more`'u yok sayıyor**; `pagination.go:28` `defaultPageLimit = 20`. **Yirmi birinci satır yok gibi görünüyor** — her listede. `?before=` de 400 dönüyor, yani "önceki sayfa" yapısal olarak imkânsız.

### EKSİK — hiç rota olmayanlar (12 satır, hiçbiri migration istemiyor)

Öne çıkanlar: `GET /v1/tools/{id}/revisions` (**shipped bir runbook adımını uygulanamaz kılıyor**), **hook'lar için tek bir okuma rotası yok**, `GET /v1/schedules` listesi yok, audit için `/v1` altında hiçbir rota yok, ajan/MCP/depo bağlantısı için **PATCH ve DELETE yok** (konsol yaratır ve okur, **düzeltmez**).

---

## 5. Mimari — konuşulanın özeti ve düzeltilen inançlar

### Doğru olan

**admin panel → control plane → agent runner** ayrımı gerçek. `deploy/systemd/palai-runner.service` + imzalı runner host paketi var; `runner-host.md` split-VM bacağını belgeliyor: runner **kendi host'unda koşar ve control plane'in runner gateway'ine dial eder**.

Deploy hedefleri: `deploy/helm` (k8s), `deploy/compose` + `deploy/systemd` (VM), `deploy/airgap`.

**SDK → configure edilmiş ajanı çağırma ZATEN VAR ve BAĞLI:**
```
POST /v1/responses  { "agent_revision_id": "...", "input": ... }
```
`coordinator/store.go:646` — **YAYIMLANMIŞ** bir revizyona pinliyor; admission bunu **idempotency rezervinden önce** doğruluyor: bilinmeyen **404**, taslak **409**, ikisi de ne idempotency kaydı ne run bırakıyor.

### Düzeltilen iki inanç

1. **SDK runner ile hiç konuşmuyor.** Sadece control plane ile. Runner'ın public endpoint'i yok; **dışarı doğru** dial ediyor. → **Runner'ın inbound portu, public IP'si, DNS'i gerekmiyor.** Kiralanan Mac'in sadece dışarı çıkması yeterli.
2. **Secret değeri web'den girilmiyor** (bugün). Değer CLI'dan **stdin** ile; argv/history/log'a girmiyor.
   **Revize:** owner'ın önerdiği **write-only, geri okunamaz** alan materyal olarak farklı ve doğru (Vercel/GitHub/Netlify aynen böyle). **Ön koşul:** konsol auth'u.

### Ölçülen boşluk

**Tek runner.** `runner_gateway.go:73` birebir:
> *"SH-0 is a single-runner topology (**there is no hosts/runners table in this tier**), so 'the runner' IS the gateway. A multi-runner fleet would key these by runner id; that is the SaaS/post-SH-0 upgrade path."*

Ve **hangi ajanın nerede koşacağı config'de yaşamıyor**: `PALAI_SANDBOX_IMAGE` XOR `PALAI_SHELL_NATIVE`, `main.go:594`'te **boot'ta bir kez** okunuyor. Bir kurulum ya sandbox'lı Linux'tur ya native host'tur — ikisi birden olamaz.

---

## 6. Araştırma — Anthropic'in self-hosted mimarisi

Owner'ın "environment key diye bir şey yapmışlar gibi" sezgisi **doğru çıktı.**

| Anthropic | Kaynak |
|---|---|
| `self_hosted` environment **bir iş kuyruğudur**; session ona atanınca Anthropic onu **work item olarak enqueue eder** | platform docs |
| Worker kuyruktan **claim eder** — sürekli **polling** ya da webhook ile uyanıp polling | " |
| **İki credential:** *environment key* worker'ı **kendi kuyruğuna** doğrular; *API key* dışarıdan session yaratır. **Key üretimi yalnız Console'dan** | " |
| `ANTHROPIC_ENVIRONMENT_KEY` + `ANTHROPIC_ENVIRONMENT_ID` | " |
| Always-on worker **yalnız outbound HTTPS** ister | " |
| Session başına izolasyon: poller `ANTHROPIC_SESSION_ID`/`WORK_ID`/`ENVIRONMENT_ID`/`ENVIRONMENT_KEY`'i bir **spawn script'ine** enjekte eder | " |
| Platform entegrasyonları (AWS Lambda MicroVM, Cloudflare, E2B, Modal, Vercel, GKE, Daytona…) **core değişikliği değil, o spawn seam'i** | " |

### Bundan çıkan üç tasarım cevabı

**(a) Kuyruk yerleştirme primitifidir, label matcher değil.** Anthropic environment içinde routing yapmıyor — **environment'ın kendisi etikettir**. Mac havuzu ve Linux havuzu = iki environment, iki key. Bu, bir saat önce önerdiğim label-eşleştiren predikattan **daha basit ve daha doğru**. `WorkerSpec.PoolLabel` zaten var; **pool = environment**.

**(b) Enrollment: paylaşılan key + makine başına kimlik.** Owner'ın önerisi ("admin panelden bir tane oluştur, tüm makinelerde aynı key ile register et") doğru. Palai bugün **tek kullanımlık** token kullanıyor (`runner_gateway.go:40-44`: *"Consume returns an error for an unknown or already-spent token"*) — güvenli ama filo ölçeğinde makine başına token mintlemek gerekir.
**Sentez:** havuz başına **yeniden kullanılabilir** enrollment key (kolay, Anthropic'in environment key'i gibi) → **makine başına sertifikaya** takas edilir (güvenli, Palai'nin bugün yaptığı). Bu kubeadm'in TLS bootstrapping'i ve Tailscale'in reusable auth key'i. **Kolay tarafı enrollment'ta, güç tarafı sonrasında.**

**(c) Platform tabanı = tek bir spawn seam.** Core'a N provider adapter'ı değil; poller'ın session değişkenlerini enjekte ettiği **bir script hook'u**. AWS/Azure/E2B/Modal farkı o script'te yaşar.

### Ve rakibin dokümanının söylediği en önemli cümle

> *"**A Linux host** with `/bin/bash` at that exact path."*

**Anthropic'in self-hosted worker'ı Linux-only.** Owner'ın ürün tezi — *"bizim olayımız bir Mac'te Mac ürünlerini kullanabilmek"* — tam olarak rakibin yapmadığı şey. Bu, kendi dokümanlarıyla doğrulanmış bir farklılaştırıcıdır.

---

## 7. Mac autoscale — ölçülmüş ekonomik kısıt

| | Container/Linux | Mac |
|---|---|---|
| Açılış | saniyeler | **~1 dakika** (Scaleway API) |
| Minimum fatura | yok | **24 saat** |
| Politika | elastik, sıfıra iner | **sıcak havuz**, N'e iner |

**24 saat Apple'ın lisans şartıdır, satıcı tercihi değil** — AWS ve Scaleway aynı. AWS'de fatura host'u *allocate* ettiğin an başlar, instance duruyor olsa bile işler, ve 24 saat dolmadan release edilemez. Scaleway "bir dakikanın altında" başlatıyor ve 24 saat sonra otomatik silme sunuyor.

⇒ *"Anlık yük gelince Mac kaldır"* **teknik olarak** mümkün, **ekonomik olarak** bir gün satın almaktır. Mac havuzunun bir **tabanı** ve **saatlerle** ölçülen bir sönme süresi olur; container'lar istek başına ölçeklenir.

---

## 8. Kalan işler — sıralı

### Şu an koşan

| İş | Ne |
|---|---|
| `approval-http` | Slack'siz onay yüzeyi: `/v1` altında listeleme + karar, tek boğaz `DecideToolApproval`, request-hash bağlaması korunur |
| `cloud-fixes` | Smoke bulgu 3 (edge sertifikası), 5 (production sağlık komutu), 6 (disk tabanı + kaynak minimumları) |

### E24 — Filo (önerilen sıra)

1. **Havuz = environment.** `PoolLabel`'ı yük taşır hale getir; claim havuz içinde FIFO. Mac havuzu / Linux havuzu iki ayrı havuz.
2. **Havuz başına yeniden kullanılabilir enrollment key.** Admin panelden üret, tüm makinelerde CLI ile aynı key. Makine başına sertifikaya takas (bugünkü mekanizma korunur). Key rotasyonu ve iptal.
3. **Engine düzlemine runner registry.** Cordon/drain/revoke'u runner id'ye anahtarla — gateway'in kendi notunun tarif ettiği şey.
4. **Scaler + static provider.** Havuz başına kuyruk derinliği → kapasite. İlk provider "static" (ofisteki makineler).
5. **Cloud provider'lar** — spawn seam üzerinden; Scaleway (Mac), Docker/k8s (Linux). AWS/Azure kullanıcının seçimi.

### E25 — Admin panel

0. **Kimlik doğrulama** — her şeyden önce. Bugün yok, ve write-only secret alanının ön koşulu.
1. **Environment nesnesi** — gruplanmış, write-only, geri okunamaz key-value; ajanda seçilir; session başlarken claim edilir. Primitif zaten var: `JobSpec.SecretHandleRefs` (*"secret_refs NAMES; never values"*) + `RedeemSecretHandle` (kapsam+süre, journal'a asla yazılmaz).
2. **Yazma yüzeyleri** — 22 yapılandırma yüzeyinin engelleyen kümesi.
3. **Gözlemlenebilirlik** — run listesi, geçmiş timeline, bekleyen onaylar, usage/budget/quota, artefakt tarayıcısı, capability tablosu.
4. **O12 sayfalama yalanı** ve 12 EKSİK rotanın küçük olanları.

### Owner'a bağlı (kod değil)

- **amd64 doğrulaması** — gerçek bir amd64 VM gerekiyor; bu Mac'ten kapatılamıyor.
- Gerçek bir iOS repo + gerçek ticket ile uçtan uca deneme.
- Yayın (registry, npm org, tek CODEOWNER'la **her zaman reddeden** iki-kişilik `ApprovalGate`) — **şimdilik rafta**, owner'ın kararı.
- `API-3`/`API-4` (publication okuma rotaları) — üçüncü kez dosyalandı, hâlâ `post-1.0`, **onaylanmadı**.

---

## 9. Açık tavanlar

| Kod | Ne | Nerede |
|---|---|---|
| `HIL-P10` | Modaldeki deny sebebi hiçbir yere ulaşmıyor (deny'ın kendisi çalışıyor) | `known-gaps-1.0.md` |
| `HIL-P2` | Platformun kullanıcı kimliği yok; bir onay bir kişidir | " |
| `HIL-P5` | Yanlış yapılandırılmış bir MCP kaydı kapıyı sessizce atlayabilir | " |
| Smoke 5 | Production stack'te doctor anlamsız | düzeltiliyor |
| §6 leg 1 | Gerçek bir workspace'ten yakalanmış receipt yok; her bundle'ın `Peer`'ı yapısal olarak `"fake"` | tüm bundle'lar |

**Hiçbir tier ilerlemedi ve gerekçe bir kuraldır:** bir kontrol eklemek, o kontrolün gerçek bir workspace'te çalıştığının kanıtı değildir.

---

## Kaynaklar

- [Anthropic — Self-hosted sandboxes](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes)
- [Amazon EC2 Mac instances FAQs](https://aws.amazon.com/ec2/instance-types/mac/faqs/)
- [Amazon EC2 Mac instances docs](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-mac-instances.html)
- [Scaleway Apple silicon FAQ](https://www.scaleway.com/en/docs/apple-silicon/faq/)
- [Scaleway Apple silicon API](https://www.scaleway.com/en/developers/api/apple-silicon)
