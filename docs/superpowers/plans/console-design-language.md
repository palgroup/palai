# Palai Console — Tasarım Dili Şartnamesi

**Durum:** şartname (uygulanmadı). **Tarih:** 2026-07-30. **Kapsam:** `apps/web-console`.

Bu doküman bir tasarım denemesi değil, bir **şartname**. Amacı, uygulayan agent'ın tek bir estetik karar
vermek zorunda kalmaması. Her renk çifti ölçülmüş bir kontrast oranı taşıyor; "uygun bir şey seç" diye
bırakılmış hiçbir değer yok.

---

## 1. Karar

**Seçilen tasarım dili: "Operator" — Radix Colors'ın 12 adımlı ölçek mimarisi, WCAG 2.x formülüyle yeniden
doğrulanmış değerlerle, sıfır bağımlılıkla düz CSS custom properties olarak; hairline öncelikli yüzeyler,
tipografiyle kurulan hiyerarşi ve makine değerleri için monospace.**

Gerekçe tek cümlede: Radix Colors, "tasarımcı olmadan tutarlı bir palet" sorununun yayınlanmış en güçlü
cevabıdır ve adım başına tanımlı semantik rolleri sayesinde tek bir `@media (prefers-color-scheme: dark)`
bloğunda 30 satırla dark mode'u çözer — ama kontrast garantisi APCA (Lc) cinsindendir, bu konsol ise axe ile
WCAG 2.0/2.1/2.2 etiketleri altında test edilir, dolayısıyla **mimarisini alıp adım eşlemesini WCAG 2.x'e göre
yeniden ölçmek** doğru olan tek yaklaşımdır (§3.1'de bu ölçüm Radix'in kendi adım rollerinden ikisini
reddediyor).

---

## 2. Bugün ne var, neyi ölçtüm

`app/globals.css` = **214 satır** el yazımı CSS, 7 renk tokenı, `system-ui`, GitHub Primer paleti. Sıfır
styling bağımlılığı. Bileşenler: `Panel`, `ResourceForm`, `SecretField`, `ApprovalPanel`, `AgentDiff`,
`Status`, `Timeline`.

Kontrast ölçüm yöntemi: WCAG 2.2 *Understanding SC 1.4.3*'ün verdiği formül, Node ile uygulandı —
relatif luminans `L = 0.2126R + 0.7152G + 0.0722B` (kanal başına sRGB linearizasyonu, eşik 0.04045) ve oran
`(L1 + 0.05) / (L2 + 0.05)`. Kaynak: <https://www.w3.org/WAI/WCAG22/Understanding/contrast-minimum.html>
(fetch: 2026-07-30). Bütün sayılar 2 ondalığa yuvarlı.

### 2.1 Zaten bozuk olan üç şey (ölçülmüş, tahmin değil)

**BULGU-1 — `--border` her iki modda da SC 1.4.11 Non-text Contrast'ı ihlal ediyor.**

| Çift | Ölçüm | Gereken | Sonuç |
|---|---|---|---|
| `#d0d7de` on `#ffffff` (light, input/button kenarı, sayfa üstünde) | **1.45:1** | 3:1 | FAIL |
| `#d0d7de` on `#f6f8fa` (light, panel içinde) | **1.36:1** | 3:1 | FAIL |
| `#30363d` on `#0d1117` (dark, sayfa üstünde) | **1.55:1** | 3:1 | FAIL |
| `#30363d` on `#161b22` (dark, panel içinde) | **1.42:1** | 3:1 | FAIL |

`input`, `select`, `textarea` ve `button` arka planı `var(--bg)` — yani sayfa ile aynı. Kontrolün görünür
sınırını **yalnızca** bu kenar taşıyor, dolayısıyla SC 1.4.11'in "kullanıcı arayüzü bileşenlerini
tanımlamak için gereken görsel bilgi" tanımına tam olarak giriyor ve 3:1 zorunlu. axe bunu yakalamıyor:
axe-core'un kontrol kenarı kontrastı için bir kuralı yok (`color-contrast` yalnızca metin/arka plan çiftlerine
bakar). E25 T2 etiket setini genişletti ama eklediği üç kural `autocomplete-valid`, `avoid-inline-spacing`,
`target-size` — hiçbiri bunu görmüyor. Yani suite yeşil, kriter ihlal.

**BULGU-2 — dark mode'da skip link metni SC 1.4.3'ü ihlal ediyor, ve bu konsolun ilk Tab durağı.**

`.skip-link` `background: var(--accent)` + sabit `color: #fff` kullanıyor (`globals.css:56-57`). Dark blokta
`--accent` `#4aa3ff`'e dönüyor:

| Çift | Ölçüm | Gereken | Sonuç |
|---|---|---|---|
| `#ffffff` on `#0b5cad` (light) | 6.67:1 | 4.5:1 | PASS |
| `#ffffff` on `#4aa3ff` (**dark**) | **2.63:1** | 4.5:1 | **FAIL** |

Neden hiçbir test yakalamadı — iki bağımsız sebep, ikisi de doğrulandı:

1. **Hiçbir axe taraması dark mode'da çalışmıyor.** `playwright.config.ts`'te `colorScheme` hiçbir yerde
   set edilmiyor, ve kurulu `playwright-core@1.51.1`'in kendi tip tanımı bunu yazıyor: *"Passing `null`
   resets emulation to system defaults. **Defaults to `'light'`.**"*
   (`node_modules/.pnpm/playwright-core@1.51.1/.../types/types.d.ts:9785`). Yani **dark paletin tamamı
   otomatik erişilebilirlik kapsamının dışında.**
2. Skip link her taramada `left: -9999px` ile ekran dışında; axe hiçbir taramayı link focus'luyken yapmıyor
   (`tests/a11y.spec.ts:116`'daki klavye testi axe çağırmıyor).

**BULGU-3 — panel arka planı görsel olarak hiçbir iş yapmıyor.** `#f6f8fa` on `#ffffff` = **1.06:1**;
dark'ta `#161b22` on `#0d1117` = **1.09:1**. Bu bir WCAG ihlali değil (panel sınırını 1px kenar da
taşıyor) ama tasarımın dayandığı yüzey hiyerarşisi pratikte yok. Aşağıdaki şartname bunu bir kusur olarak
düzeltmiyor, bir **ilkeye** çeviriyor: ayrımı dolgu değil hairline + gölge taşır (§4.1).

### 2.2 Küçük ama gerçek

- **`.status` yorumu kodu yanlış anlatıyor.** `globals.css:160` diyor: *"color is a redundant hint"*. CSS'te
  `.status` için **hiç renk kuralı yok** — ne `[data-status]` ne `[data-lane]` seçicisi var (doğrulandı:
  `grep "data-status\|data-lane" globals.css` → 0 eşleşme). Davranış doğru (renk-tek-başına ihlali yok),
  yorum fazla söz veriyor. Şartname bunu yorumu düzeltmek yerine **renk ipucunu gerçekten ekleyerek**
  kapatıyor (§5.5) — çünkü metin+glif zaten yerinde olduğu için renk artık gerçekten yedek bir katman.
- **Statü glifleri emoji'ye dönüşebilir.** `Status.tsx` `✔` (U+2714) ve `✖` (U+2716) kullanıyor. Unicode'un
  kendi `emoji-data.txt`'sinde ikisi de `Emoji` ve `Extended_Pictographic` taşıyor, `Emoji_Presentation`
  listesinde **yok** (fetch: <https://unicode.org/Public/UNIDATA/emoji/emoji-data.txt>, 2026-07-30). Yani
  varsayılan sunum metin, ama platform renkli emoji glifi seçebilir — bu hem renk-tek-başına kuralına
  sızma hem de tablo hizasını bozan bir genişlik. `○` (U+25CB), `⊘` (U+2298), `•` (U+2022) emoji değil.
  Çözüm §5.5'te: `✔`/`✖` sonrası **U+FE0E (VS15)**.
- **`button:disabled { opacity: 0.5 }`** panel üstünde metni `#88898a` on `#fbfcfd` = **3.41:1**'e düşürüyor
  (dark: 4.80:1). WCAG 1.4.3 devre dışı kontrolleri açıkça muaf tutuyor, dolayısıyla ihlal değil — ama
  `Panel.tsx`'in "Load more" butonu **meşgulken** de disabled oluyor, yani aktif bir işlemin göstergesi
  yarım kontrastta okunuyor. §5.1'de ayrı bir busy durumu var.
- `:focus-visible` `border-radius: 2px` sabitliyor; §4.4'te tokena bağlanıyor.

---

## 3. Araştırma — hangi sistem neye yarıyor, nerede bu vakaya yaramıyor

Hepsi fetch edildi, tarih 2026-07-30. Hafızadan hiçbir iddia yok.

### 3.1 Radix Colors — seçilen mimari, ama adım eşlemesi düzeltilerek

<https://www.radix-ui.com/colors/docs/palette-composition/understanding-the-scale>

12 adım, adım başına tanımlı rol: 1 app background, 2 subtle background, 3 UI element background,
4 hovered UI element background, 5 active/selected UI element background, 6 subtle borders and separators,
7 UI element border and focus rings, 8 hovered UI element border, 9 solid backgrounds, 10 hovered solid
backgrounds, 11 low-contrast text, 12 high-contrast text.

Yayınlanmış garanti, kelimesi kelimesine: *"Steps 11 and 12 — which are designed for text — are guaranteed
to Lc 60 and Lc 90 APCA contrast ratio on top of a step 2 background from the same scale."*

**Bu garanti APCA. Bu konsol WCAG 2.x ile test ediliyor. İkisi aynı şey değil ve fark ölçülebilir.**
Radix'in kendi adım rollerini WCAG 2.x formülüyle ölçtüm (değerler `radix-ui/colors` `src/light.ts` ve
`src/dark.ts` kaynağından, MIT):

| Radix'in dediği | Ölçülen WCAG 2.x | Karar |
|---|---|---|
| Adım 7 = "UI element border and focus rings" | `slate7` on `slate1`: **1.53:1** (light), **2.04:1** (dark) | **Reddedildi** — 3:1'in çok altında |
| Adım 8 = "hovered UI element border" | `slate8` on `slate1`: **1.86:1** (light), **3.01:1** (dark); panel üstünde dark **2.81:1** | **Reddedildi** |
| — | `slate9` on `slate1`: 3.22 / `slate3` üstünde **2.90** | Yetersiz (inset yüzeyde düşüyor) |
| — | **`slate10`**: page 3.69 / subtle 3.60 / inset **3.33** (light); dark 4.45 / 4.15 / **3.75** | **Kabul: kontrol kenarı = adım 10** |
| Adım 11 = "low-contrast text", Lc 60 garantili | `amber11` on `amber2`: **4.43:1** — WCAG AA'yı **geçmiyor** | **amber11 metin olarak yasak** |
| Adım 11, diğer ölçekler | slate 5.65, red 4.94, grass 4.82, blue **4.53** (adım 2 üstünde) | Geçiyor; blue'nun payı 0.03 — dikkat |

İki sonuç, ikisi de bu şartnamenin omurgası:

1. **WCAG 2.x'te bir interaktif kontrol kenarı için ilk kullanılabilir nötr adım 10'dur, 7 veya 8 değil.**
   Mevcut `#d0d7de` kabaca bir adım-6/7 değeri — BULGU-1'in tam sebebi bu. Radix'in adım rolünü olduğu gibi
   kopyalamak aynı hatayı tekrarlardı.
2. **Radix'in Lc 60 garantisi WCAG AA'ya çevrilmiyor.** amber bunu ölçülebilir biçimde gösteriyor:
   `#ab6400` hiçbir yüzeyde 4.5:1'i geçmiyor — `slate1` üstünde tam **4.50** (sıfır pay), `slate2` üstünde
   **4.38**, `slate3` üstünde **4.05**, `amber3` üstünde **4.25**. Bu yüzden şartnamede amber'in metin tokenı
   adım **12** (`#4f3422`, `amber3` üstünde 10.47:1) ve amber11 hiç emit edilmiyor.

Dark mode: Radix her ölçek için ayrı bir dark ölçek yayınlıyor, aynı adım numaralarıyla. Bu yüzden dark blok
**yalnızca 27 raw adımı** yeniden tanımlıyor, tek bir semantik alias'a dokunmuyor. Şartnamenin iki katmanlı
olmasının tek sebebi bu (§4.1).

### 3.2 Diğerleri — ne aldım, ne almadım

| Sistem | İyi olduğu şey | Bu vaka için neden yetmiyor |
|---|---|---|
| **Vercel Geist** (<https://vercel.com/geist/colors>) | 10 ölçek × 10 adım, rol bantları net: 100–300 component bg, 400–600 border, 700–800 high-contrast bg, 900–1000 text/icon. Tokenlar `var(--ds-gray-400)` biçiminde. | **Kontrast oranı veya WCAG uyum iddiası yayınlanmıyor** — doküman "9–10 accessible text için tasarlandı" diyor, sayı vermiyor. Dark mode bu sayfada hiç ele alınmıyor. Ölçemediğim bir garantiyi devralamam. |
| **GitHub Primer** (<https://primer.style/foundations/color/overview>) | Üç katmanlı token hiyerarşisi (Base → Functional → Component) ve `bgColor-default` gibi fonksiyonel isimlendirme — bu şartnamenin iki katmanlı yapısı doğrudan bu fikrin sadeleştirilmişi. Dark, nötr ölçeği **ters çevirerek** üretiliyor, ayrı palet yok. | Kritik cümle: *"Step 8 is considered the minimum contrast value for interactive control borders."* Ölçtüm: `slate8` light'ta **1.86:1**. Primer'in eşiği bu vakada yetersiz. Ayrıca mevcut palet zaten Primer'in — BULGU-1'i miras aldığımız yer burası. Yüksek kontrast teması için verdiği 7:1 hedefi ise iyi bir gelecek referansı. |
| **Shopify Polaris** (<https://polaris-react.shopify.com/components/tables/index-table>) | Tek net ve doğrudan alınabilir kural: *"Numeric cells and titles should be right aligned."* Statü için Badge deseni. | Truncation ve satır yoğunluğu hakkında **hiçbir şey** yazmıyor (doğrulandı). Bileşen kütüphanesi React + kendi runtime'ı; alınacak olan tek cümleydi, alındı (§5.1). |
| **IBM Carbon** | Yoğunluk kademelerini yayınlayan sistem. | `carbondesignsystem.com`'un üç sayfası da fetch'te truncate oldu, GitHub kaynağından da satır yüksekliği token'larına ulaşamadım. **Bu yüzden Carbon'dan hiçbir sayı alıntılamıyorum.** §4.6'daki satır yükseklikleri benim şartnamem, bir alıntı değil — ve öyle etiketlendi. |
| **Linear** | Görünüş referansı olarak en yakın hedef: neredeyse akromatik yüzeyler + tek bir aksan, makine değerleri için özel monospace. | **Yayınlanmış bir tasarım sistemi dokümantasyonu yok.** Arama, yalnızca üçüncü taraf reverse-engineering çıkarımları buluyor (fontofweb, copycats, designmd, shadcn.io — hepsi resmî değil). Spec kaynağı olarak kullanılamaz; ilham olarak kullanıldı, alıntı olarak kullanılmadı. Ayrıca Inter Variable ve Linear Mono'ya dayanıyor — ikisi de bizim reddettiğimiz webfont maliyeti (§6). |
| **Stripe** | Aynı durum: dashboard'un tasarım sistemi yayınlanmış değil. | Spec kaynağı değil. |
| **Radix Themes** (<https://www.radix-ui.com/themes/docs/theme/typography>) | 9 adımlı tip ölçeği, adım başına font-size + line-height + letter-spacing üçlüsü — §4.3'ün yapısı bundan alındı (12/14/16/18/20/24/28/35/60px, negatif tracking büyük adımlarda). | Ölçeğin üst ucu marketing için (60px); data-dense admin için alt uç eksik. §4.3 ölçeği **11px'ten** başlatıp üst ucu 36px'te kesiyor. Paket olarak ise 4 bağımlılık + kendi CSS reset'i (§6). |
| **Tailwind Plus / Tailwind UI** (<https://tailwindcss.com/plus/ui-blocks/documentation>) | Hazır application-UI blokları, HTML/React/Vue. | **Ücretli ürün** ve Tailwind CSS v4.2 gerektiriyor, yani yeni bir build adımı. Fetch'te lisans metni ve fiyat sayfada görünmüyor — yani devralacağımız şartları **okumadan** karar vermemiz gerekirdi. Bir admin konsolu için bu tek başına yeterli bir red. |
| **Uber Base Web** | Olgun bileşen seti. | npm'den ölçüldü: `baseui@18.2.0`, **30 doğrudan bağımlılık** ve peer olarak `styletron-react>=6` — yani bir CSS-in-JS **runtime**'ı. `productionBrowserSourceMaps: true` olan, public-API-only relay duruşundaki bir konsola runtime style enjeksiyonu eklemek en pahalı seçenek. Red. |
| **Atlassian Design System** | Token isimlendirmesi (`color.text.subtlest`) iyi bir referans. | Fetch ettiğim Table sayfası deprecation uyarısı taşıyor (*"This package was an experiment, and is currently deprioritized. It is not recommended for use in production."*) ve yoğunluk/hizalama/truncation hakkında bir şey yazmıyor. Bu vakaya katkısı olmadı. |

### 3.3 Data-dense okunabilirlik için aldığım kararlar

Yayınlanmış kaynaklardan alınabilen tek kesin kural Polaris'in sayısal sağa hizalamasıydı. Geri kalanı bu
şartnamenin kendi kararı ve öyle etiketlendi:

- **Dolgu değil hairline.** BULGU-3 ölçümü, yüzey dolgusunun bu palette 1.06–1.09:1 taşıdığını, yani
  görünmediğini gösteriyor. Panel ayrımı 1px kenar + `--shadow-1` ile kurulur; dolgu yalnızca **bant**
  görevlerinde (thead, code bloğu, kv etiket kolonu) kullanılır.
- **Zebra striping yok.** Hairline satır ayırıcısı zaten var; ikisi birlikte gereksiz gürültü.
- **Kolon başlığı tipografiyle taşınır**, dolguyla değil: `--font-size-1` + uppercase + `--letter-spacing-1`
  + `--text-muted`. Bu, konsolu "devlet formu"ndan çıkaran en tek ucuz hamle; mevcut CSS'te h1 1.05rem ve
  h2 1.15rem, yani **hiç hiyerarşi yok** ve her şey ~16px.
- **Makine değerleri monospace.** id, hash, cursor, digest, branch → `--font-mono`. Sayısal kolonlar
  `font-variant-numeric: tabular-nums` + sağa hizalı (Polaris).
- **Truncation asla sessiz olmaz** — `Panel.tsx` bunu satır sayısı için zaten metinle söylüyor; §5.1 aynı
  kuralı **hücre** seviyesine taşıyor.

---

## 4. Token seti

124 token adı, `:root`'ta 124 declaration, dark blokta **30** override (27 raw renk adımı + `--text-on-solid`
+ 2 gölge). Sayılar ölçüldü, tahmin değil. Renk değerleri `radix-ui/colors` (MIT, `src/light.ts` /
`src/dark.ts`) kaynağından programatik olarak çıkarıldı — elle kopyalanmadı.

### 4.1 Neden iki katman

Katman 1 ham ölçek adımları (`--slate-10`), katman 2 semantik alias (`--border-control`). Kural: **kural
gövdelerinde yalnızca katman 2 kullanılır.** Sebep tek ve ölçülebilir: Radix light/dark ölçekleri aynı adım
numaralarını taşıdığı için dark blok sadece 27 ham adımı yeniden tanımlar ve 34 semantik alias'a hiç
dokunmaz. Tek katmanla dark blok 60+ satır olur ve her semantik değer iki yerde bakım isterdi.

Emit edilen adımlar bilinçli olarak eksik — kullanılmayan adım yazılmıyor: `slate` 1-6,9-12 (7 ve 8
§3.1'de reddedildi), `blue` 3,6,9,10,11,12, `red`/`grass` 3,6,11,12, `amber` 3,6,12 (11 ölçümle yasaklandı).
**Yeni bir adım gerektiğinde eklenir — ve o anda ölçülür.**

### 4.2 Renk — light

```css
:root {
  color-scheme: light dark;

  /* Katman 1 — ham ölçek adımları. Kural gövdelerinde KULLANILMAZ. */
  --slate-1: #fcfcfd; --slate-2: #f9f9fb; --slate-3: #f0f0f3; --slate-4: #e8e8ec;
  --slate-5: #e0e1e6; --slate-6: #d9d9e0; --slate-9: #8b8d98; --slate-10: #80838d;
  --slate-11: #60646c; --slate-12: #1c2024;
  --blue-3: #e6f4fe; --blue-6: #acd8fc; --blue-9: #0090ff; --blue-10: #0588f0;
  --blue-11: #0d74ce; --blue-12: #113264;
  --red-3: #feebec; --red-6: #fdbdbe; --red-11: #ce2c31; --red-12: #641723;
  --grass-3: #e9f6e9; --grass-6: #b2ddb5; --grass-11: #2a7e3b; --grass-12: #203c25;
  --amber-3: #fff7c2; --amber-6: #f3d673; --amber-12: #4f3422;

  /* Katman 2 — yüzeyler */
  --bg-page: var(--slate-1);    /* sayfa VE panel: aynı renk, ayrımı hairline yapar */
  --bg-subtle: var(--slate-2);  /* sticky thead bandı */
  --bg-inset: var(--slate-3);   /* code bloğu, kv etiket kolonu, disabled kontrol */
  --bg-hover: var(--slate-4);   /* satır / menü öğesi hover */
  --bg-active: var(--slate-5);  /* basılı */

  /* Katman 2 — metin */
  --text: var(--slate-12);
  --text-muted: var(--slate-11);
  --text-on-solid: #ffffff;     /* dark blokta ters çevrilen TEK semantik token */

  /* Katman 2 — kenarlar */
  --border-hairline: var(--slate-6);        /* dekoratif: satır ayırıcı, panel çizgisi */
  --border-control: var(--slate-10);        /* interaktif kontrol kenarı — SC 1.4.11 taşıyıcısı */
  --border-control-hover: var(--slate-11);

  /* Katman 2 — aksan */
  --accent-text: var(--blue-11);
  --accent-bg: var(--blue-3);
  --accent-border: var(--blue-6);
  --accent-solid: var(--blue-11);
  --accent-solid-hover: var(--blue-12);
  --focus-ring: var(--blue-11);

  /* Katman 2 — statü (5 durum × 3, + 2 inline metin) */
  --ok-bg: var(--grass-3);      --ok-border: var(--grass-6);      --ok-text: var(--grass-12);
  --ok-text-inline: var(--grass-11);
  --warn-bg: var(--amber-3);    --warn-border: var(--amber-6);    --warn-text: var(--amber-12);
  /* --warn-text-inline YOK: amber11 ölçümle hiçbir yüzeyde 4.5:1'i geçmiyor (§3.1). */
  --danger-bg: var(--red-3);    --danger-border: var(--red-6);    --danger-text: var(--red-12);
  --danger-text-inline: var(--red-11);
  --info-bg: var(--blue-3);     --info-border: var(--blue-6);     --info-text: var(--blue-12);
  --neutral-bg: var(--slate-3); --neutral-border: var(--slate-6); --neutral-text: var(--slate-12);
}
```

### 4.3 Renk — dark (30 override, hepsi bu blokta)

> **SIRALAMA KURALI — sessizce kırılır.** Bu blok, dosyada **bütün `:root` declaration'larından sonra**
> gelmelidir; §4.5 ve §4.6'nın tokenları bu blokta değil `:root`'ta tanımlı olmasına rağmen bu blok onları
> (`--shadow-1`, `--shadow-2`) override ediyor. Aynı specificity'de sonra gelen kazandığı için, `:root`
> içindeki light `--shadow-1` bu media bloğundan **sonra** yazılırsa dark modda da light gölge uygulanır ve
> hiçbir test bunu söylemez. Dosya sırası: tüm `:root` → sonra bu blok (§7'deki bölüm sırası buna uyuyor).

```css
@media (prefers-color-scheme: dark) {
  :root {
    --slate-1: #111113; --slate-2: #18191b; --slate-3: #212225; --slate-4: #272a2d;
    --slate-5: #2e3135; --slate-6: #363a3f; --slate-9: #696e77; --slate-10: #777b84;
    --slate-11: #b0b4ba; --slate-12: #edeef0;
    --blue-3: #0d2847; --blue-6: #104d87; --blue-9: #0090ff; --blue-10: #3b9eff;
    --blue-11: #70b8ff; --blue-12: #c2e6ff;
    --red-3: #3b1219; --red-6: #72232d; --red-11: #ff9592; --red-12: #ffd1d9;
    --grass-3: #1b2a1e; --grass-6: #2d5736; --grass-11: #71d083; --grass-12: #c2f0c2;
    --amber-3: #302008; --amber-6: #5c3d05; --amber-12: #ffe7b3;

    /* Solid dolgu dark'ta AÇIK olduğu için üstündeki metin koyulaşır. Bu ters çevrilme
       BULGU-2'nin yapısal düzeltmesi: skip link artık `#fff` sabitlemez. */
    --text-on-solid: var(--slate-1);

    /* Gölge dark yüzeyde görünmez; kart ayrımını hairline + üst iç highlight taşır. */
    --shadow-1: 0 1px 2px rgb(0 0 0 / 40%), inset 0 1px 0 rgb(255 255 255 / 3%);
    --shadow-2: 0 8px 24px rgb(0 0 0 / 60%), 0 2px 6px rgb(0 0 0 / 40%);
  }
}
```

### 4.4 Ölçülmüş kontrast tablosu — normatif

Uygulayan agent bu tabloyu değiştiremez. Her satır yukarıdaki WCAG 2.2 formülüyle hesaplandı.

**Metin — SC 1.4.3 AA, gereken 4.5:1** (bu konsolda 18pt+ metin yok, dolayısıyla 3:1 large-text istisnası
hiçbir yerde geçerli değil):

| Çift | Light | Dark |
|---|---|---|
| `--text` on `--bg-page` | 15.98 | 16.25 |
| `--text` on `--bg-inset` | 14.41 | 13.70 |
| `--text` on `--bg-hover` | 13.41 | 12.43 |
| `--text` on `--bg-active` | 12.55 | 11.26 |
| `--text-muted` on `--bg-hover` | 4.86 | 6.93 |
| `--text-muted` on `--bg-page` | 5.79 | 9.06 |
| `--text-muted` on `--bg-subtle` | 5.65 | 8.45 |
| `--text-muted` on `--bg-inset` | 5.22 | 7.64 |
| `--accent-text` on `--bg-page` | 4.65 | 8.97 |
| `--accent-text` on `--bg-subtle` | 4.53 | 8.37 |
| `--text-on-solid` on `--accent-solid` | 4.77 | 6.75 |
| `--text-on-solid` on `--accent-solid-hover` | 12.62 | 8.97 |
| `--ok-text` on `--ok-bg` | 10.85 | 11.84 |
| `--warn-text` on `--warn-bg` | 10.47 | 12.98 |
| `--danger-text` on `--danger-bg` | 10.84 | 11.95 |
| `--info-text` on `--info-bg` | 11.26 | 11.37 |
| `--neutral-text` on `--neutral-bg` | 14.41 | 13.70 |
| `--ok-text-inline` on `--bg-page` | 4.94 | 9.93 |
| `--danger-text-inline` on `--bg-page` | 5.08 | 8.95 |
| `--text` on `--ok-bg` (diff + satırı) | 14.69 | 12.95 |
| `--text` on `--danger-bg` (diff − satırı) | 14.29 | 14.06 |
| `--text` on `--accent-bg` (seçili satır) | 14.62 | 12.81 |
| `--text-muted` on `--accent-bg` | 5.30 | 7.14 |

En düşük değer **4.53** (`--accent-text` on `--bg-subtle`). Payı 0.03. **Kural: `--accent-text` `--bg-inset`
veya daha koyu bir yüzeyde kullanılmaz** — orada 4.5'in altına düşer.

**Non-text — SC 1.4.11 AA, gereken 3:1:**

| Çift | Light | Dark |
|---|---|---|
| `--border-control` on `--bg-page` | 3.69 | 4.45 |
| `--border-control` on `--bg-subtle` | 3.60 | 4.15 |
| `--border-control` on `--bg-inset` | **3.33** | 3.75 |
| `--border-control` on `--bg-hover` (hover'lı satırdaki kontrol) | **3.10** | 3.40 |
| `--border-control-hover` on `--bg-subtle` | 5.65 | 8.45 |
| `--focus-ring` on `--bg-page` | 4.65 | 6.75 |
| `--focus-ring` on `--bg-subtle` | 4.53 | 6.30 |
| `--accent-solid` on `--bg-page` (buton dolgusu) | 4.65 | 6.75 |

**Kritik uygulama kuralı — `--ring-offset` yük taşıyıcıdır.** `--focus-ring` ile `--border-control` arasındaki
oran yalnızca **1.44:1** (light) / **1.84:1** (dark). Bu bir ihlal değil, çünkü `outline-offset: 2px`
halkayı kontrol kenarından ayırıp **yüzeyin** yanına koyar — ve halkanın yüzeye karşı oranı 4.65/6.75.
`outline-offset`'i kaldırmak veya 0 yapmak halkayı non-conforming hale getirir. Bu satır silinmemeli.

**Dekoratif, SC uygulanmaz:** `--border-hairline` on `--bg-page` = 1.37 (light) / 1.65 (dark). Tablo satır
ayırıcısı ve panel çizgisi bir UI bileşeninin sınırı veya durumu değil, yapısal süslemedir; SC 1.4.11
"bileşen tanımlamak için gereken" testini geçmez. **Koşul:** hiçbir bilgi yalnızca hairline'ın varlığıyla
aktarılmaz. Statü çipi kenarları (`--*-border`) da aynı kategoride — çip interaktif değil, metni
(`--*-text`) 10:1+ taşıyor.

### 4.5 Tipografi

```css
:root {
  --font-sans: system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
  --font-mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace;

  --font-size-1: 0.6875rem; --font-size-2: 0.75rem;   --font-size-3: 0.8125rem;
  --font-size-4: 0.875rem;  --font-size-5: 1rem;      --font-size-6: 1.125rem;
  --font-size-7: 1.375rem;  --font-size-8: 1.75rem;   --font-size-9: 2.25rem;

  --line-height-1: 1rem;     --line-height-2: 1.125rem; --line-height-3: 1.25rem;
  --line-height-4: 1.375rem; --line-height-5: 1.5rem;   --line-height-6: 1.625rem;
  --line-height-7: 1.75rem;  --line-height-8: 2.125rem; --line-height-9: 2.5rem;

  --letter-spacing-1: 0.06em;  --letter-spacing-2: 0.01em;   --letter-spacing-3: 0em;
  --letter-spacing-4: 0em;     --letter-spacing-5: 0em;      --letter-spacing-6: -0.005em;
  --letter-spacing-7: -0.01em; --letter-spacing-8: -0.015em; --letter-spacing-9: -0.02em;

  --font-weight-regular: 400; --font-weight-medium: 500;
  --font-weight-semibold: 600; --font-weight-bold: 700;
}
```

Adım → kullanım eşlemesi (normatif):

| Adım | px | Kullanım |
|---|---|---|
| 1 | 11 | mikro etiket: tablo kolon başlığı, eyebrow, lane etiketi — **uppercase + `--letter-spacing-1`** |
| 2 | 12 | meta: zaman damgası, yardım metni, truncation notu |
| 3 | 13 | yoğun tablo hücresi, timeline satırı, kod |
| 4 | 14 | **gövde varsayılanı**, form kontrolleri, buton |
| 5 | 16 | düzyazı, bölüm giriş cümlesi |
| 6 | 18 | panel başlığı (`h3`) |
| 7 | 22 | bölüm başlığı (`h2`) |
| 8 | 28 | sayfa başlığı (`h1`) |
| 9 | 36 | login ekranı, boş-durum başlığı |

**`rem` kuralı yük taşıyıcıdır:** kök font-size'a `px` atanmaz. Değerler 16px kökten türetildi ama kullanıcının
tarayıcı font tercihiyle ölçeklenir — SC 1.4.4 Resize Text bunu gerektirir.

**SC 1.4.12 Text Spacing (AA) uyarısı:** `line-height`, `letter-spacing`, `word-spacing` üzerinde
`!important` kullanılamaz. Mevcut `prefers-reduced-motion` bloğunun `!important` kullanımı yalnızca
animation/transition üzerinde ve doğru; oraya spacing özelliği eklenmemeli. axe'ın
`avoid-inline-spacing` kuralı bunu inline stillerde kontrol ediyor.

`system-ui` **korunuyor.** Gerekçe: sıfır network isteği (CSP'de `font-src` girdisi yok, self-host edilecek
byte yok, FOUT/CLS yok) ve public-API-only relay duruşunda taşınacak ek yüzey yok. **Vazgeçilen:** konsol
işletim sistemleri arasında tipografik olarak aynı görünmez, ve 500 ağırlığı SF/Segoe'de gerçek, bazı Linux
fontconfig sonuçlarında sentezlenir. Kabul: tasarım dili ölçek, boşluk, renk ve mono/proportional
karşıtlığına dayanıyor, belirli bir yazı tipine değil.

### 4.6 Boşluk, yoğunluk, radius, kenar, gölge, hareket

```css
:root {
  --space-1: 0.25rem; --space-2: 0.375rem; --space-3: 0.5rem;
  --space-4: 0.75rem; --space-5: 1rem;     --space-6: 1.5rem;
  --space-7: 2rem;    --space-8: 3rem;     --space-9: 4rem;

  --row-h-compact: 1.75rem;  /* 28px — timeline / log satırı */
  --row-h: 2.25rem;          /* 36px — varsayılan tablo satırı */
  --row-h-roomy: 2.75rem;    /* 44px — içinde kontrol olan satır */
  --control-h: 2rem;         /* 32px — input, select, buton */
  --control-h-sm: 1.5rem;    /* 24px — SC 2.5.8 tabanı, ikon buton */

  --radius-1: 3px; --radius-2: 5px; --radius-3: 8px; --radius-4: 12px; --radius-full: 9999px;

  --border-w: 1px; --border-w-strong: 2px; --ring-w: 2px; --ring-offset: 2px;

  --shadow-1: 0 1px 2px rgb(0 0 0 / 4%), 0 1px 1px rgb(0 0 0 / 3%);
  --shadow-2: 0 8px 24px rgb(0 0 0 / 10%), 0 2px 6px rgb(0 0 0 / 6%);

  --duration-1: 90ms; --duration-2: 150ms; --duration-3: 240ms;
  --ease-out: cubic-bezier(0.2, 0, 0, 1);
  --ease-in-out: cubic-bezier(0.4, 0, 0.2, 1);
}
```

Bu satır yükseklikleri **bu şartnamenin kararı**, alıntı değil (§3.2'de Carbon'a ulaşamadığım not edildi).

**SC 2.5.8 Target Size (Minimum), AA — sert kısıt.** Hiçbir pointer hedefi 24×24 CSS px altına inmez.
`--control-h-sm` tam olarak bu taban. Bu, E25 T2'nin etiket genişletmesinin **gerçekten eklediği** kural
(`target-size`, `wcag22aa`), yani suite bunu yakalar — ölçeği küçültürken ilk kırılacak yer burası.

**Ring genişliği 3px → 2px'e iniyor.** Mevcut `outline: 3px` de uygundu; 2px + 2px offset, SC 2.4.13 Focus
Appearance'ın (AAA, zorunlu değil) 2 CSS px çevre tabanını hâlâ karşılıyor ve halka debug outline'ı gibi
durmuyor. Offset **değişmiyor** — §4.4'teki sebep.

Hareket tokenlarının hepsi mevcut `prefers-reduced-motion` bloğunun kapsamında; o blok **aynen korunur** ve
token bloklarından **sonra** gelmeye devam eder.

---

## 5. Bileşen desenleri

Her desen: yapı + token. Prose yok.

### 5.1 Tablo / liste — görünür truncation

```
section.panel
  > header.panel-head   : h3(--font-size-6) + p.muted(--font-size-2) + actions
  > div.table-scroll    : overflow-x auto; overflow-y visible
    > table
      > thead > tr > th : --font-size-1, uppercase, --letter-spacing-1, --text-muted,
                          --font-weight-semibold, background --bg-subtle, position sticky, top 0
      > tbody > tr > td : --font-size-3, height --row-h, border-bottom --border-hairline
  > p.table-more        : --font-size-2, --text-muted  (Panel.tsx'in mevcut metni)
  > button.load-more
```

| Konu | Kural |
|---|---|
| Satır yüksekliği | `--row-h`; kontrol içeren satır `--row-h-roomy` |
| Hücre dolgusu | `padding: 0 var(--space-4)`; ilk/son hücre `--space-5` |
| Ayırıcı | `border-bottom: var(--border-w) solid var(--border-hairline)`; **zebra yok** |
| Sayısal kolon | `text-align: right; font-variant-numeric: tabular-nums` (Polaris) |
| Makine değeri kolonu | `font-family: var(--font-mono); --font-size-2` |
| Hover | `tbody tr:hover { background: var(--bg-hover) }` |
| Seçili | `background: var(--accent-bg)`; **ayrıca `aria-selected`** — renk tek başına değil |
| Hücre truncation | `max-width` + `overflow: hidden; text-overflow: ellipsis; white-space: nowrap` **ve** `title` attribute **ve** `tabindex="0"` ile odaklanabilir — kesilen metne klavyeyle ulaşılabilmeli. Ellipsis tek başına sessiz bir kesmedir. |
| Liste truncation | `Panel.tsx` zaten metinle söylüyor; korunur |

**Sticky thead — iki zorunlu detay:**

1. `border-collapse: collapse` ile sticky `th` kenarları kayar. `border-collapse: separate; border-spacing: 0`
   kullan ve başlık çizgisini `box-shadow: inset 0 -1px 0 var(--border-hairline)` ile ver. Mevcut CSS
   `collapse` kullanıyor — değişmesi gerekiyor.
2. **SC 2.4.11 Focus Not Obscured (Minimum), AA:** *"When a user interface component receives keyboard focus,
   the component is not entirely hidden due to author-created content."* Failure technique **F110** doğrudan
   sticky header/footer'ı adlandırıyor. Sticky thead + sticky app header, klavyeyle gezilen bir satır
   kontrolünü gizleyebilir. Zorunlu karşı önlem: `html { scroll-padding-top: calc(var(--row-h) + var(--space-7)) }`.
   Kaynak: <https://www.w3.org/WAI/WCAG22/Understanding/focus-not-obscured-minimum.html> (fetch: 2026-07-30).
   `--bg-subtle` **opak** olmalı; yarı saydam thead altındaki satırları gösterir.

### 5.2 Form alanı

```
div.field
  > label[for]          : --font-size-2, --font-weight-medium, --text
  > input|select|textarea
  > p.field-hint[id]    : --font-size-2, --text-muted   (aria-describedby)
```

| Token | Değer |
|---|---|
| yükseklik | `--control-h` (textarea hariç) |
| dolgu | `0 var(--space-3)` |
| kenar | `var(--border-w) solid var(--border-control)` — **§4.4, BULGU-1'in düzeltmesi** |
| hover | `--border-control-hover` |
| radius | `--radius-2` |
| font | `--font-size-4`, `--font-family: inherit` |
| arka plan | `--bg-page` |
| disabled | `background: var(--bg-inset)`; `opacity` **kullanma** — dolgu değişimi kontrastı korur |
| alan aralığı | `margin-bottom: var(--space-5)` |
| label→kontrol | `margin-bottom: var(--space-2)` |

`ResourceForm.tsx`'in dört disiplini (programatik label, `role="alert"`, metinle statü, klavyeyle
erişilebilirlik) **dokunulmaz**. Bu tablo yalnızca görsel katman.

### 5.3 Secret alanı

`SecretField.tsx` yapısı **değişmez** (uncontrolled, `type="password"`, `autocomplete="new-password"`,
`onPaste` yok). Görsel:

| Token | Değer |
|---|---|
| font | `--font-mono`, `--font-size-4` — girilen değerin makine değeri olduğunu gösterir |
| kenar | `--border-control` |
| hint | `--font-size-2`, `--text-muted`, `--space-2` üstten |

Kilit/uyarı için renkli bir çerçeve **eklenmez** — hassasiyet `label` metniyle ve hint'le anlatılır.

### 5.4 Approval satırı

```
section.panel.panel-approval
  > h3                        : --font-size-6
  > fieldset.kv               : --bg-inset, --radius-2, --space-4
    > dl > dt                 : --font-size-1, uppercase, --letter-spacing-1, --text-muted
         > dd                 : --font-size-3, --font-mono (operation/branch/request_hash makine değeri)
  > p.proposal-summary        : --font-size-2, --text-muted, sol kenar --border-w-strong solid --neutral-border
  > div.actions               : --space-3 gap
    > button.primary          : --accent-solid + --text-on-solid, --control-h
    > button                  : varsayılan buton
```

Panel kenarı `--accent-border`, arka planı `--bg-page` (dolgu yok). `ApprovalPanel.tsx`'in "authoritative
detail asla display string ile değiştirilemez" kuralı görsel olarak da korunur: kv grid `--bg-inset` bandında
ve mono, proposal summary muted ve daha küçük.

### 5.5 Statü göstergesi

`Status.tsx` yapısı korunur (glif + kelime, glif `aria-hidden`). İki değişiklik:

1. **Glif metin sunumuna sabitlenir:** `✔` → `✔︎`, `✖` → `✖︎` (§2.2 ölçümü). `○ ⊘ •` emoji değil,
   dokunulmaz.
2. **Renk gerçekten yedek katman olarak eklenir** — `[data-status]` seçicileriyle çip formunda:

```css
.status { display: inline-flex; align-items: center; gap: var(--space-2);
  font-size: var(--font-size-2); font-weight: var(--font-weight-medium);
  padding: 0 var(--space-2); height: var(--control-h-sm);
  border-radius: var(--radius-full); border: var(--border-w) solid var(--neutral-border);
  background: var(--neutral-bg); color: var(--neutral-text); }
```

Durum eşlemesi — `Status.tsx`'in `glyphFor()` sınıflandırması **aynen** kullanılır, ikinci bir eşleme yazılmaz:

| glyphFor sonucu | bg | border | text |
|---|---|---|---|
| `✔` (complete/approved/succeed/restored) | `--ok-bg` | `--ok-border` | `--ok-text` |
| `✖` (fail/denied/error/lost) | `--danger-bg` | `--danger-border` | `--danger-text` |
| `○` (wait/pending/recover/stream) | `--info-bg` | `--info-border` | `--info-text` |
| `⊘` (cancel/expired/timed) | `--warn-bg` | `--warn-border` | `--warn-text` |
| `•` (diğer) | `--neutral-bg` | `--neutral-border` | `--neutral-text` |

Hepsi 10.47:1 ve üzeri (§4.4). Kelime ve glif kaldırılırsa çip anlamını kaybeder — renk hâlâ tek başına
taşıyıcı değil.

### 5.6 Timeline

```
ol.timeline > li.lane[data-lane][data-type]
  > span.lane-tag : --font-size-1, uppercase, --letter-spacing-1, --text-muted, --font-mono
  > span.event    : --font-size-3
```

| Token | Değer |
|---|---|
| satır yüksekliği | `--row-h-compact` |
| sol kılavuz | `border-left: var(--border-w-strong) solid var(--border-hairline)`, `padding-left: var(--space-4)` |
| satır aralığı | `--space-1` |
| lane etiketi genişliği | sabit `7rem`, `tabular-nums` ile hizalı — lane'ler kolon gibi okunur |

Lane **renkle kodlanmaz**: 6 lane için 6 ayırt edilebilir renk, renk körlüğünde çalışmaz ve mevcut kod
lane'i zaten metin olarak yazıyor. Ayrım tipografi ve sabit kolon hizasıyla kurulur.

### 5.7 Diff

```
pre.diff > code
  > span.diff-add : background --ok-bg,     color --text
  > span.diff-del : background --danger-bg, color --text
  > span.diff-ctx : color --text-muted
```

| Kural | Değer |
|---|---|
| font | `--font-mono`, `--font-size-3`, `--line-height-3` |
| `+`/`−` karakteri | **satırda kalır** ve `--text` rengindedir — renk yedek katman, taşıyıcı değil |
| dolgu | `0 var(--space-4)`, satır başına full-bleed |
| kapsayıcı | `--bg-inset`, `--radius-2`, `overflow-x: auto` |

Ölçüm: `--text` on `--ok-bg` 14.69/12.95, on `--danger-bg` 14.29/14.06. Gutter işaretine ayrı renk
verilmiyor — `--ok-text-inline`/`--danger-text-inline` orada 4.54:1'e düşüyordu, `--text` 14:1 veriyor.

### 5.8 Boş durum

```
div.empty
  > p.empty-title  : --font-size-5, --font-weight-medium, --text
  > p.empty-body   : --font-size-3, --text-muted
  > (opsiyonel) button.primary
```

`padding: var(--space-8) var(--space-5)`, `text-align: center`, kapsayıcı `--bg-page` + hairline. Mevcut
"None yet." metni `p.empty-title` olur. İkon veya illüstrasyon **yok** (§8).

### 5.9 Hata durumu

`role="alert"` ve metin-glif yapısı `ResourceForm.tsx`/`Panel.tsx`'ten **aynen** korunur. Görsel:

```css
.form-error { display: flex; gap: var(--space-2);
  font-size: var(--font-size-3); color: var(--text);
  background: var(--danger-bg); border: var(--border-w) solid var(--danger-border);
  border-radius: var(--radius-2); padding: var(--space-3) var(--space-4); }
.form-error .glyph { color: var(--danger-text); }
```

Mevcut sabit `#b3261e`/`#ff7b72` çifti **silinir** — tokena bağlanır. `--danger-text` on `--danger-bg` =
10.84/11.95. Satır içi (çip olmayan) hata metni için `--danger-text-inline` (`--bg-page` üstünde 5.08/8.95).

### 5.10 Buton

| Varyant | bg | text | border |
|---|---|---|---|
| primary | `--accent-solid` → hover `--accent-solid-hover` | `--text-on-solid` | yok |
| default | `--bg-page` → hover `--bg-hover` | `--text` | `--border-control` → `--border-control-hover` |
| danger | `--danger-bg` | `--danger-text` | `--danger-border` |
| busy | default + `aria-busy="true"` + metin `"…"` | `--text` | `--border-control` |

Ortak: `height: var(--control-h)`, `padding: 0 var(--space-4)`, `--radius-2`, `--font-size-4`,
`--font-weight-medium`, `transition: background var(--duration-1) var(--ease-out)`.

**Busy ≠ disabled** (§2.2). `Panel.tsx`'in "Load more" butonu yükleniyorken `aria-busy` kullanır ve
kontrastını korur; gerçekten devre dışı kontroller `--bg-inset` dolgusuyla gösterilir, `opacity` ile değil.

### 5.11 Sayfa iskeleti

```
header.app-header : --bg-page, border-bottom hairline, height --row-h-roomy, --space-5 yatay dolgu
  > h1            : --font-size-6 (marka: küçük kalır)
  > nav a         : --font-size-4, --text-muted; aktif olan --text + --font-weight-medium
main              : max-width 90rem, --space-6 dolgu, --space-6 panel arası
h1.page-title     : --font-size-8, --letter-spacing-8
h2                : --font-size-7, --letter-spacing-7
h3                : --font-size-6, --letter-spacing-6
```

`max-width` **72rem → 90rem**: data-dense tabloların 72rem'de gereksiz yatay kaydırması var.
Nav'daki aktif durum **yalnızca renkle** gösterilmez — `aria-current="page"` + ağırlık farkı.

---

## 6. Bağımlılık kararı

**Karar: yeni bağımlılık yok. El yazımı CSS + token sistemi devam eder.**

Ölçülmüş gerekçe:

| Aday | Ölçüm | Karar |
|---|---|---|
| `@radix-ui/colors@3.0.0` | **0 bağımlılık, MIT** — sadece sabitlerden oluşuyor | **Gereksiz.** Paket sadece renk değeri; 5 ölçeğin kullanılan 27 adımını iki modda `globals.css`'e yazmak aynı değerleri sıfır pakete indirir. MIT lisansı bu kopyalamayı zaten kapsıyor. Değerler kaynaktan programatik çıkarıldı, elle yazılmadı. |
| `@radix-ui/themes@3.3.0` | 4 bağımlılık + kendi CSS reset'i + bileşen katmanı | Red — bileşenlerimiz zaten var ve erişilebilirlik disiplinleri bize özel (`SecretField`'ın uncontrolled olması, `ApprovalPanel`'ın authoritative-detail kuralı). |
| Tailwind CSS + Tailwind Plus | Tailwind v4.2 gerektiriyor (build adımı); Plus **ücretli** ve lisans metni fetch'te görünmüyor | Red |
| `baseui@18.2.0` | **30 doğrudan bağımlılık** + peer `styletron-react>=6` (CSS-in-JS runtime) | Red |

Ek maliyet kalemleri, dürüstçe: `productionBrowserSourceMaps: true` olduğu için gönderilen her şey okunabilir
— eklenen her styling paketi **denetim yüzeyi** olur, üstelik public-API-only relay iddiasının network
kanıtında yeni bir origin veya yeni bir `font-src`/`style-src` gevşemesi gerektirebilir. El yazımı CSS bu
kalemlerin hepsinde sıfır.

**Vazgeçilenler, açıkça:**

- Hazır erişilebilir primitive'ler: focus trap'li **modal/dialog**, **combobox/autocomplete**, **tooltip**,
  **dropdown menu**, **date picker**, tarih/sayı yerelleştirme. Bunlar doğru yazılması zor bileşenler
  (klavye modeli, `aria-activedescendant`, inert). Konsolun bugün hiçbirine ihtiyacı yok. **Bir tanesi
  gerektiğinde bunu yeniden tartışın** — o zaman `@radix-ui/react-*` primitive'i (headless, stil yok) doğru
  bir tek-amaçlı ekleme olur, tam kütüphane değil.
- Utility-class hızı: her yeni desen `globals.css`'e el yazımı kural olarak eklenir.
- Otomatik dead-CSS budama. Karşılığı: `globals.css` tek dosya olarak okunabilir kalır.

---

## 7. Mevcut 214 satır ne olacak, hangi sırayla

**Karar: yeniden yapılandır (restructure) — ne genişlet ne sıfırdan yaz.** Erişilebilirlik bloğu
(`skip-link`, `:focus-visible`, `prefers-reduced-motion`, `.status`/`.form-error` metin kuralları) testlerle
kanıtlanmış ve **aynen** korunur; tek dosya olarak devam eder (üç dosyaya bölmek iki ekstra import'tan başka
bir şey kazandırmaz). Tahmini son boyut ~450-520 satır, sabit bölüm sırası:

```
1. Token blokları (:root, sonra @media dark)     — yeni, §4
2. Reset + taban (box-sizing, body, a, ::selection)
3. Erişilebilirlik (skip-link, focus-visible, reduced-motion)  — MEVCUT, korunur
4. Tipografi (h1-h3, .muted, code/mono, mikro etiket)
5. Layout (app-header, main, panel, panel-head)
6. Tablo (+ sticky thead, hücre truncation)
7. Form (field, label, input, secret, fieldset/kv)
8. Kontroller (button varyantları, focus)
9. Statü + geri bildirim (.status çipleri, .form-error, .empty)
10. Alan-özel (timeline/lane, diff, pre.code)
```

Uygulama sırası — her adım kendi başına yeşil bırakır:

| # | Adım | Neden bu sırada |
|---|---|---|
| **0** | `playwright.config.ts`'e **dark colorScheme projesi** ekle; a11y taramaları hem light hem dark koşsun | **İlk sıra, çünkü BULGU-2'yi yakalayan tek şey bu.** Bu adım kırmızı başlar (dark skip link 2.63:1) — düzeltmeyi adım 1 yapar. Test önce, düzeltme sonra. |
| **1** | Token blokları eklenir; eski 7 token **alias** olur (`--fg: var(--text)` vb.). `--border` → `--border-control`. `.skip-link` `color: #fff` → `var(--text-on-solid)`. | **Her iki ölçülmüş ihlali bu adım kapatır** (BULGU-1 ve BULGU-2). Görsel değişim yalnızca palet; hiçbir selector taşınmaz. Adım 0'ın dark taraması yeşile döner. |
| 2 | Tipografi: ölçek + `h1/h2/h3` + mikro etiket + `--font-mono` makine değerlerinde | En büyük görsel kazanç, sıfır a11y riski. axe (`avoid-inline-spacing`, `target-size`) yeniden koşar. |
| 3 | Layout + yoğunluk: `max-width` 90rem, panel yapısı, satır yükseklikleri, sticky thead + `scroll-padding-top` + `border-collapse: separate` | SC 2.4.11 riski burada doğuyor, karşı önlemiyle birlikte iniyor. |
| 4 | Bileşen desenleri: statü çipleri (+ U+FE0E glif sabitlemesi), approval kv grid, diff tint, empty/error, buton varyantları, busy≠disabled | Yapı değişmiyor, yalnızca sınıf ve token. |
| 5 | Eski alias'ları sil (`--fg`, `--bg`, `--muted`, `--panel`, `--accent`, `--focus`, `--border`) | Son adım: geriye dönük kaçış yolu kapanır. |
| 6 | `globals.css`'in açılış yorumunu güncelle: `.status` artık gerçekten renk ipucu taşıyor (§2.2) | Yorum koda eşitlenir. |

Her adımdan sonra: `pnpm typecheck`, `pnpm test:e2e` (light **ve** dark), `pnpm sweep`.

**Adım 1 tamamlandığında bir kontrast regresyon testi eklenmeli** — §4.4 tablosunun makinede yeniden
hesaplanması. Bu tabloyu elle bakımda tutmak, palet değiştiğinde sessizce yanlışa dönmesinin yoludur; axe
kontrol kenarlarını görmediği için (§2.1) tek gerçek koruma budur.

---

## 8. Bunun veremeyeceği şey

Token sistemi uygulayan bir agent **tutarlı bir araç** üretir, **tasarlanmış bir ürün** üretmez. Aradaki fark:

- **Bilgi mimarisi.** Bu şartname 20 panelden hangisinin önemli olduğunu, ana ekranın ne göstermesi
  gerektiğini, hangi kolonun ilk kolon olduğunu, neyin silinmesi gerektiğini söylemiyor. Bir konsolun
  "monoton" olmasının asıl sebebi genellikle palet değil, her şeyin eşit ağırlıkta gösterilmesidir.
  **Bir tasarımcının ilk yapacağı şey ekranlardan bir şeyler çıkarmak olurdu; bir token sistemi hiçbir şey
  çıkarmaz.**
- **Görsel kimlik.** Logo yok, marka yazı tipi yok, illüstrasyon yok, kendine ait bir renk yok — palet MIT
  lisanslı ve Radix kullanan her üründe aynı. Konsol "temiz" görünecek, "Palai" görünmeyecek.
- **İkonografi.** Bugün Unicode glifleri (`✔ ✖ ○ ⊘ •`) kullanılıyor; §2.2 bunların platforma göre emoji'ye
  dönebildiğini ölçtü ve U+FE0E ile sabitliyor — ama bu bir çözüm değil, bir yamadır. Gerçek bir ikon seti
  (tutarlı ağırlık, optik boyut, hizalama) bu dokümanın kapsamı dışında ve bağımlılık kararı gereği
  eklenmedi.
- **Hareket koreografisi.** Üç süre ve iki easing var; neyin animasyonlanacağı, neyin sırayla gireceği,
  bir stream'in canlılığının nasıl hissettirileceği yok.
- **Boş durum ve hata metinlerinin sesi.** "None yet." dilbilgisel olarak doğru ve tamamen bilgisiz.
  Operatöre ne yapması gerektiğini söyleyen metinler yazılmadı.
- **Responsive / mobil.** Data-dense tablolar için yatay kaydırma dışında bir strateji yok. 90rem'in altında
  ne olacağı belirtilmedi.
- **Kanıtlanan erişilebilirlik ≠ erişilebilirlik.** `tests/a11y.spec.ts`'in kendi yorumundaki sayı geçerli
  kalıyor: axe'ın yayınlanmış oranı *"on average 57% of WCAG issues"*. Bu doküman iki ölçülmüş ihlali
  buluyor ve düzeltiyor; **dağıtılmış bir konsol üzerinde bir ekran okuyucu ile manuel geçiş hâlâ
  yapılmadı** (§6 operator leg 8) ve bu şartname onun yerine geçmez.
- **Ve en dürüst olanı:** buradaki hiçbir şeye bir tasarımcı bakmadı. Ölçümler doğru, oranlar geçiyor,
  ölçek tutarlı — ama "iyi görünüyor mu" sorusunun cevabı ölçülemedi.

---

## Ek — kaynaklar (hepsi 2026-07-30 tarihinde fetch edildi)

| Kaynak | URL |
|---|---|
| WCAG 2.2 Understanding SC 1.4.3 (formül, eşikler) | <https://www.w3.org/WAI/WCAG22/Understanding/contrast-minimum.html> |
| WCAG 2.2 Understanding SC 2.4.11 (F110, sticky header) | <https://www.w3.org/WAI/WCAG22/Understanding/focus-not-obscured-minimum.html> |
| Radix Colors — 12 adım ve rolleri, APCA garantisi | <https://www.radix-ui.com/colors/docs/palette-composition/understanding-the-scale> |
| Radix Colors kaynak değerleri (MIT) | `github.com/radix-ui/colors` — `src/light.ts`, `src/dark.ts`, `LICENSE` |
| Radix Themes tip ölçeği | <https://www.radix-ui.com/themes/docs/theme/typography> |
| Vercel Geist renk ölçeği | <https://vercel.com/geist/colors> |
| GitHub Primer renk temelleri | <https://primer.style/foundations/color/overview> |
| Shopify Polaris IndexTable | <https://polaris-react.shopify.com/components/tables/index-table> |
| Tailwind Plus dokümantasyonu | <https://tailwindcss.com/plus/ui-blocks/documentation> |
| Atlassian Table (deprecated uyarısı) | <https://atlassian.design/components/table/examples> |
| Unicode emoji-data.txt (U+2714/U+2716) | <https://unicode.org/Public/UNIDATA/emoji/emoji-data.txt> |
| npm registry — baseui, @radix-ui/themes, @radix-ui/colors metadata | `registry.npmjs.org` |
| Playwright `colorScheme` varsayılanı | kurulu `playwright-core@1.51.1` `types/types.d.ts:9785` |

Linear ve Stripe için yayınlanmış tasarım sistemi dokümantasyonu **bulunamadı**; arama yalnızca üçüncü taraf
reverse-engineering kaynakları döndürdü, bu yüzden ikisinden de değer alıntılanmadı.
