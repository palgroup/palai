# Konsol ekran planı — canlı durum önce, düzyazı yerine yapı

**Tarih:** 2026-07-31. **Karar sahibi:** owner. **Yazan:** bu oturum.
**Önceki tur:** `console-design` dalı (`a6ba99c1`) bir kenar çubuğu, kartlar ve ad-önce tablolar getirdi.
Yeterli değildi ve sebebi aşağıda §1'de. Bu plan onun üstüne yazar, yerine değil.

---

## §1 — Asıl kusur: bu arayüz operatöre değil, bir gözden geçirene yazılmış

Her panelin altında bir açıklama paragrafı var. Örnekler, ağaçtan, bugün:

- `/policy` API anahtarları paneli: *"Metadata only. A key's value is returned by the create call and by
  nothing else — there is no route that reads one back."*
- `/runs` revizyon seçici: *"Only a PUBLISHED revision can be run. A draft is refused by the server, which
  is why it is listed rather than hidden."*
- `/` altbilgi: *"Open-core console — public API only. This is not a commercial SaaS UI: no billing or team
  management (§5)…"*

Bunlar doğru cümleler ve **yanlış yerdeler**. Referans olarak verilen Claude Console'da bu tür tek bir
paragraf yok; oradaki bilgi **yapıda** taşınıyor: kolon başlığı, durum pill'i, göreli zaman, filtre chip'i,
satır sonu menüsü, boş-durum cümlesi.

Sonucu iki katmanlı:
1. Her sayfa aynı ağırlıkta gri metin duvarı — göz nereye bakacağını bilmiyor.
2. Ekran bir README gibi okunuyor, bir kontrol paneli gibi değil.

**§1 kuralı — HER task'ın kabul şartı:** bir paneli açıklayan düzyazı, ya bir yapıya dönüşür (kolon,
rozet, boş-durum cümlesi, tooltip) ya da `docs/operations/console.md`'ye taşınır. Ekranda kalmaz.
Ölçüsü: `<p className="muted">` sayısı. Bugün → `grep -rc 'className="muted"' apps/web-console/app
apps/web-console/components | awk -F: '{s+=$2} END {print s}'` (task ilk adımda ölçer ve hedefi yazar).

---

## §2 — Ölçülmüş seam envanteri (2026-07-31, `main` @ `d41029eb`)

Komut: `grep -oE '"GET /v1/[a-z0-9{}_/-]+"' apps/control-plane/api/router.go | sort -u` → 63 rota.

Canlı durum ekranının ihtiyacı olan **her şey bugün okunabilir** — bu ekran için sıfır backend işi:

| İhtiyaç | Rota | Durum |
|---|---|---|
| Koşan/kuyruktaki run'lar | `GET /v1/responses` | ✅ |
| Bekleyen onaylar | `GET /v1/approvals` | ✅ |
| Runner sağlığı | `GET /v1/runners`, `GET /v1/runner-pools` | ✅ |
| Token/kullanım | `GET /v1/usage`, `GET /v1/usage/ledger` | ✅ |
| Bütçe/kota | `GET /v1/budgets`, `GET /v1/quotas` | ✅ |
| Oturumlar | `GET /v1/sessions`, `.../events` | ✅ |
| Slack bağlantıları | `GET /v1/slack-connections`, `.../{id}` | ✅ — **Slack ekranı saf arayüz işi** |

**Bilinen delik, bu planın dışında:** `GET /v1/schedules` (liste) **yok**, yalnız `{schedule_id}` ile
tekil okuma var. Zamanlama ekranı E29 T1'i bekler.

---

## §3 — Tasarım dili: koyu, sakin, tipografik

`docs/superpowers/plans/console-design-language.md`'nin 124 token'ı ve WCAG ölçümleri **korunur**.
Eksik olan üstündeki katman:

**Punto hiyerarşisi — bugün yok, en büyük tek kazanç.** Şu an her şey aynı büyüklükte.
- Sayfa başlığı: 28/32, ağırlık 600
- Bölüm başlığı: 15/20, ağırlık 600, harf aralığı +0.01em, **büyük harf değil**
- Metrik sayısı: 32/36, ağırlık 500, `font-variant-numeric: tabular-nums`
- Metrik etiketi: 12/16, ağırlık 500, ikincil renk, büyük harf + 0.06em aralık
- Tablo satırı: 14/20 — ad ağırlık 500 birincil renk, ID 12/16 mono ikincil
- Yardımcı metin: 13/18 ikincil — **panel başına en fazla bir satır**

**Boşluk ritmi:** 4px tabanlı ölçek, ama bölümler arası **32px**, panel içi **16px**. Bugün ikisi de aynı
ve sayfa bu yüzden tek bir blok gibi duruyor.

**Renk:** tek vurgu. Durum rengi yalnız **durum** için — koşan, bekleyen, başarısız, iptal. Bir rozet
rengi asla tek başına anlam taşımaz, her zaman metinle birlikte (WCAG 1.4.1, süitte zaten kanıtlı).

**Kenarlık yerine zemin.** Bugün her panel 1px kenarlıklı kutu; sonuç bir tablo ızgarası. Panel ayrımı
zeminle ve boşlukla yapılır, kenarlık yalnız tablo satır ayracında kalır.

---

## §4 — `/` Genel bakış: ne gösterir

Karar: **"şu an ne oluyor"**. Kayıt listeleri (agent, key, secret, org, proje) `/registry`'de kalır.

```
Overview                                        Local Org / Default Project ▾

  3            7                2 / 2           1.2M
  RUNNING      WAITING ON YOU   RUNNERS HEALTHY  TOKENS · 24H
                     ↳ Approvals

Active runs                                                    View all →
  ● running   Push the release branch      slack      12s
  ● queued    Review the open PR           slack      just now

Waiting on you                                                 View all →
  Deploy to production        slack · 4m ago      Approve   Decline
  Delete the staging bucket   slack · 1h ago      Approve   Decline

Fleet                                                          View all →
  runner-local     pool_default     active     macOS arm64     up 2h
```

**Boş durumlar bir cümle söyler ve bir eylem verir**, bugünkü *"None yet."* yerine:
*"Bu deployment henüz hiç run başlatmadı."* + `Start a run →`

**Yasak:** bu ekranda açıklama paragrafı yok. Bir kartın ne olduğu etiketinden anlaşılmıyorsa etiket yanlış.

---

## §5 — Liste ekranı deseni (tek bir bileşen, altı ekran)

Başlık satırı: ad + satır sayısı + arama kutusu + filtre + birincil eylem.
Tablo: **ad birinci kolon** (ağırlık 500), ID altında mono/ikincil, durum pill'i, göreli zaman, satır sonu `⋯`.
Alt: `Showing N — Load more` (sessiz kırpma yasak, bu zaten `console-design`'da var ve korunur).

**Test verisi ayrımı — bugün gerçek bir kullanılabilirlik kusuru:** `/runs` agent seçicisinde on sekiz
`t6-agent-1785500138299-5670` kaydı tek gerçek agent'ın (`slack`) yanında duruyor ve operatör ayırt
edemiyor. Bu planın kapsamında **arayüz tarafı** çözülür: liste varsayılan olarak ada göre sıralanır ve
arama kutusu ilk odak alır. Veritabanı temizliği ayrı bir iştir ve bu plan onu üstlenmez.

---

## §5.5 — Sessions: owner'ın gösterdiği referans, ve neyin bugün kurulamayacağı

Owner üç ekran gösterdi (2026-07-31): **Sessions listesi**, **session transcript'i**, ve şablon tarayıcı.
İlk ikisi bu konsolun eksik olan asıl yüzeyi. Bugünkü `/runs` "başlat ve izle" sayfasıdır; bu ise
**geçmiş ve canlı oturumları inceleme** yüzeyidir ve ayrı bir şeydir.

**Ölçüm (2026-07-31, canlı yığın `127.0.0.1:60351`):**

```
GET /v1/sessions?limit=2
  zarf : data, has_more, next_cursor
  satır: created_at, id, object, organization_id, project_id, status

GET /v1/sessions/{id}            → aynı altı alan
GET /v1/sessions/{id}/events     → SSE; olaylar tipli ve SIRALI
                                   event: run.queued.v1
                                   data: { run_id, state, sequence: 1, session_id, source, … }
```

### Bugün kurulabilir — TRANSCRIPT

Olaylar tipli ve `sequence` taşıyor, yani referansın zaman çizgisi, olay satırları ve sağ detay paneli
mevcut veriyle kurulur. Referanstan alınan yapı:

- Breadcrumb `Sessions / ses_…`, sonra oturumun kimliği büyük puntoyla
- Başlığın yanında **satır içi çipler**: durum, agent, environment, süre, token, göreli zaman —
  paragraf değil, çip
- `Transcript` / `Debug` sekmeleri, olay türü filtresi, arama
- Olay satırı: rol rozeti (User / Tool / Agent), içerik önizlemesi, hata pill'i, token, süre, `0:00:04`
- Sağ panel: `Rendered` / `Raw` seçimi

### Bugün KURULAMAZ — liste kolonları, ve sebebi API

Referansın `Name`, `Agent`, `Tokens in / out` kolonlarının hiçbiri session satırında **yok**.

| Kolon | Bugünkü durum |
|---|---|
| `Name` (ör. "Gece Doğrulama") | Session'da ad kavramı **yok** |
| `Agent` | Session satırında agent **yok** |
| `Tokens in / out` | Session satırında token **yok** |
| `Duration` | Satırda yok; olaylardan türetilebilir ama listede N+1 istek demek |
| `Status`, `Created` | ✅ var |

**Sonuç: Sessions listesi bir API işidir, bir ekran işi değil** — ve bu ayrım tam olarak bu ağacın
tekrar eden hatasının tersidir: ekran çizilip verinin geleceği varsayılmadı, önce ölçüldü.
İş sırası bu yüzden: **önce zarf zenginleşir, sonra ekran doğar.**

---

## §6 — Sıra

1. **§3 tipografi + boşluk + zemin** — tek başına en büyük görsel fark, hiçbir veri değişikliği yok
2. **§1 düzyazı süpürmesi** — ölç, taşı, sıfırla
3. **§4 Genel bakış** — dört metrik + üç bölüm, hepsi mevcut rotalardan
4. **§5 liste deseni** — mevcut altı ekrana uygulanır
5. **Slack ekranı** — `GET /v1/slack-connections` hazır, §5 desenine göre doğar

Her adım kendi başına merge edilebilir olmalı; hiçbiri diğerini beklemez.

---

## §7 — Değişmezler

- Erişilebilirlik gerilemez: axe taramaları, WCAG 2.2 etiket seti, `role="alert"` retleri, atlama
  bağlantısı, klavyeyle gönderim. **İki renk şemasında da** koşar.
- `public-api-only.spec.ts`, `auth.spec.ts`, `secret-never-returns.spec.ts`, `relay-gate.spec.ts`,
  `reveal-once.spec.ts` **değiştirilmeden** geçer. Bir düzen değişikliği bunlardan birini düzenlemeyi
  gerektiriyorsa: dur ve bildir.
- Bu konsol ileride org/proje kapsamlı SaaS yüzeyi olacak. Kabuk baştan çok kiracılı varsayar.
