# Palai — plan ve iddia disiplini

## Bir plan iddiası ÇALIŞTIRILABİLİR olmalıdır

Bu ağaçta yazılan planlar (`docs/superpowers/plans/`) ağaç hakkında iddialarda bulunur — §3 seam
envanteri, §3.5 sapma tablosu, §3.6 "ağacın kendi yanlış inançları". **2026-07-30 gecesinde bu
iddialardan sekizi yanlış çıktı ve sekizinin de şekli aynıydı: "bir şey beyan edilmiş / adlandırılmış /
rotalanmış" ifadesi, "bir şey OLUYOR" diye okundu.**

Bu, aynı ağaçta kodda tekrar tekrar bulunan hatanın aynısıdır — beyan edilmiş ama bağlanmamış semboller,
ateşlenemeyen guard'lar, hiç koşmayan testler. **Planlar, kod hakkında, kodun kendisi hakkında yaptığı
hatanın aynısını yapıyor.**

### Dört kural

**1. Her sayı, onu üreten komutla yazılır.**
`"ağaçta dört kopya var"` değil → `` `grep -rn '<desen>' | wc -l` → 4 (2026-07-29) ``.
Task o komutu yeniden koşar; değişen sayı anında görünür.
*Gerekçe:* "one-use" inancı dört sanılıyordu, gerçekte **on**du ve exit gate **yirmi altı** buldu — plan
üç literal aramıştı, MASTER-SPEC `single-use` yazıyordu ve hiçbirine eşleşmiyordu. Sayı yanlış değildi,
**arama yanlıştı ve arama görünmüyordu.**

**2. "Bugün çalışıyor" iddiası bir TRANSCRIPT ister, bir atıf değil — VE AYNISI "bugün açık" için de geçerlidir.**
Bir rotanın var olması, bir alanın kabul edildiği anlamına gelmez. Ve bir maruziyetin *adlandırılmış*
olması, o mekanizmanın hedef platformda **işlediği** anlamına gelmez.
*Gerekçe (yetenek):* `config_policy.pool` için plan rota tablosuna baktı ve "yazma yolu shipped" dedi;
`DisallowUnknownFields` o alanı **400**'lüyordu. Aynı hata E23 T2'de `approvers` için de yapılmıştı.
*Gerekçe (tavan):* plan "aynı uid `ps -E` / `/proc/<pid>/environ` ile değeri okuyabilir" dedi; macOS
26.3'te `ps -E` **hiçbir ortam ifşa etmiyor** ve `/proc` yok — ve bu epic'in hedefi native bir Mac.
**Bir tavanı abartmak, onu küçümsemek kadar yanlıştır**: operatöre koşturunca çalışmayan bir gösterim
bırakır ve gerçek riskin yerini kaydırır.

**3. Çalışma zamanı durumu hakkındaki her iddia bir YAZAR gösterir.**
Bir durumdan/alandan bahsediyorsan, onu **yazan** kodu göster. Gösteremiyorsan iddian şudur:
*"bunu hiçbir şey yazmıyor."*
*Gerekçe:* plan "canlı yanıt `in_progress` okuyor" dedi; `queued` okuyordu, ve yanıt durum makinesinin
**ortasını üç yıldır hiçbir şey yazmamıştı** — yayımlanmış şema o durumları ilan ederken.

**4. Devralınan tavanlar TARİHLİ olur.**
Başka bir epic'ten alınan bir tavan yeniden ölçülmeden taşınmaz.
*Gerekçe:* "yeni bir bundle adı shipped RC'yi kırmızıya çevirir" iddiası doğruydu ve **arada
kapanmıştı**; ödenmeyecek bir fiyat plana yazılıydı.

### Ve bir sonuç

Kusursuz plan diye bir şey yoktur — salı yazılan bir plan çarşamba değişen bir ağaca karşı kayar.
Hedef mükemmel plan değil, **ucuz yeniden doğrulama**: task'ın ilk adımı planın sayılarını yeniden
ölçmektir, ve plan bunu mümkün kılacak şekilde yazılır.

**Planların yanlış çıkması bir başarısızlık değildir** — o gece sekizini de gate'ler ve RED'ler yakaladı,
hiçbiri `main`'e yanlış inmedi. Maliyet tespit maliyetiydi. Bu dört kural onu düşürür, sıfırlamaz.

---

## Ölçüm disiplini (koda da geçerli)

- **Bir kontrolün var olması, koştuğunun kanıtı değildir.** `scripts/test/component`'in `-run`
  allow-list'inde adı olmayan bir test hiç koşmaz ve gate yeşil görünür. Çalışan tek yöntem:
  **shipped selector'ı koştur, `--- PASS`'i gate'in iddia ettiği bacaklarla diff'le, string'i asla okuma.**
  Bu imza bu ağaçta **yedi kez** çıktı ve en net hâli şudur: bir test **untagged** olabilir, yani
  `make verify`'a biner ve **ağaçtan hiç eksik değildir** — eksik olduğu şey **onu iddia eden
  invocation'dır, ki yokluğu önemli olan tek invocation odur.** Betiği okumak da bacak listesini okumak
  da bulamaz; **ikisi de tam görünür.**
- **Bir mekanizmayı kanıtlamak, insanın kullandığı YÜZEYİ kanıtlamak değildir. 2026-07-31'de beş kusur
  çıktı ve beşi de bu şekildi.** Konsolun auth süiti dokuz kolla kimliksiz erişimi kanıtlıyordu, saldırıyı
  önce gösteriyordu, relay'in her export'unu AST ile sayıyordu — ve **hepsi `/api/console/login`'e `fetch`
  ile gidiyordu.** Hiçbiri formu sürmedi, ve formun `method`'u yoktu: JS bağlanmadığı her anda parola
  **URL'e** düşüyordu. On formdan sekizi değer taşıyordu (parola, environment secret'ı, politika dokümanı,
  token gömülebilen clone URL'i).
  Aynı gün, aynı şekil, dört kez daha:
  **(a)** `PALAI_CONSOLE_PASSWORD_HASH` `webServer.env` ile enjekte ediliyordu ve worktree'de `.env.local`
  hiç yok (gitignore) — yani **dokümanın operatöre tarif ettiği tek yol, hiçbir testin sürmediği yoldu**;
  o yolda dotenv `$`'ları yiyip hash'i bozuyordu.
  **(b)** `next dev` hiç hydrate olmuyordu ve süit yalnız `next start` servis ediyordu.
  **(c)** `journey.spec.ts:66,108` `toContainText("fake")` diyor, profil dallanması yok — **real profilde
  hiç geçemez**, ama yorumu "runs on BOTH profiles deliberately" diyor.
  **(d)** `/runs`'taki `Agent (optional)` seçicisi hiçbir şey göndermiyor (`response-create.json` `agent_id`
  kabul etmiyor); 61 run'ın 54'ünde agent bağı yok.
  **Kural:** bir yüzey için yazılan her test, **o yüzeyi sürmelidir** — endpoint'i değil formu, harness'ın
  enjekte ettiğini değil dosyanın taşıdığını, prod build'i değil operatörün koştuğu modu. Bir testin
  "X çalışıyor" demesi, **X'e giden insan yolunun** çalıştığı anlamına gelmez.
- **Bir süpürme yalnız BİR yönde bakar, ve iki yönde iki farklı hata vardır.** Dizinleri yürüyen bir
  tarama, **var olan ama sahipsiz** bir dizini bulur; **hiç var olmayan** bir dizini bulamaz. İçe dönük
  boşluklar için otorite **kanonik liste**, dışa dönükler için **yürüyüş**. (E26 T7: `BGT-` hiçbir
  prefix listesinde değildi *ve* BGT-003'ün dizini hiç yoktu — biri yürüyüşle, diğeri listeyle bulundu.)
- **Bir taramanın yeşil olması, taradığının kanıtı değildir.** `go test ./tests/security/tenancy/...`
  `matched no packages` yazıp **exit 0** döner (paket `//go:build security`); doğrusu
  `TEST=tenancy scripts/test/security`.
- **Sıkıştırılmış/kodlanmış çıktı üzerinde ham byte taraması hiçbir zaman başarısız olamaz.**
  Taramadan önce **decode et**.
- **Sıralanmamış bir SQL sonucu deterministik değildir** — `LIMIT`, `string_agg`, `array_agg`,
  `DISTINCT ON`, hepsi `ORDER BY`'sız. Bu ağaçta üç sonuca karar verdi: iki güvenlik kararı ve bir
  yanlış-kırmızı gate.
- **Production'ı aynalayan bir fake, production'ın hatasını da aynalar**; production'dan cömert bir fake
  var olmayan bir şekle karşı kod yazdırır. Bir değer bir sınırı geçip **sonra o şeyi geri aramak için**
  kullanılıyorsa, gidiş-dönüşü gerçek bir bağımlılığa karşı doğrula.
- **Kendi konfigürasyonunu kuran bir test, production'ın kurduğu konfigürasyonu hiç görmez.** Kanıtları
  production kablolamasından geçir.

- **Bir testin YEŞİLİ, harness'ın bir özelliği olabilir — ürünün değil.** 2026-07-31 gecesinde konsolun
  dört kanıtı, CI'ın ve git worktree'sinin sağladığı ama gerçek bir kurulumun sağlamadığı bir koşul yüzünden
  geçiyordu: `apps/web-console/.env.local`'in **yokluğu** — yani dokümanın her operatöre yarattırdığı dosya.
  En keskini: *"parolası olmayan bir konsol hiçbir şey servis etmez"* testi, sunucusunu değişkeni **atlayarak**
  başlatıyordu; `next start` uygulama dizininden `.env.local` okuyunca o konsol **yapılandırılmış** hâle
  geliyor ve 503 yerine 401 dönüyordu. **Ürünün reddettiğini kanıtlayan test, servis eden bir konsolu
  ölçüyordu.** Aynı gece bu sınıfın dördüncü örneği erişilebilirlikteydi: hiçbir axe taraması **açık bir
  dialog** ile koşmamıştı, çünkü döngü rotayı yüklenirken tarıyor ve `expectAxeClean` dialog kapandıktan
  sonra oturuyordu — yani bir form dialog'a taşınınca kanıttan sessizce çıkıyor ve **süpürme kapsam
  daralırken daha temiz bir sayı raporluyordu.**
  *Kural:* bir test bir **reddi** iddia ediyorsa, reddedilen koşulu **kimin sağladığını** sor. Harness onu
  **yoklukla** sağlıyorsa, operatörün makinesi **varlıkla** sağlayacaktır. Koşulunu **sahiplenen** bir test
  yaz (kendi dosyasını yazsın, kendi boş değerini açıkça geçsin), bir yokluğu devralan değil. Ve süiti,
  dokümanın tarif ettiği gibi kurulmuş bir makinede **en az bir kez** koştur: bu dördünün üçü worktree'de
  görünmezdi, ki her agent orada çalışır.

- **Bir iddia, İDDİA ETTİĞİ ŞEYLE İLGİSİZ bir sebeple geçebilir ya da kalabilir — 2026-08-01'de bu beş kez
  çıktı, üçü testi yazanın kendi eliyle.** Kararsızlık değil; şekil daha dar ve daha sinsi: **başarısızlığı
  yanlış dosyayı işaret eden bir iddia.**
  **(a)** Bir traversal testi sızıntı iddiasından ÖNCE cevap kodunu denetliyordu; iki kapsama guard'ı da
  silinince hata *"answer code = not_found, want refused"* diye düştü — bir **isimlendirme sorunu** gibi
  okunur, oysa olan **başkasının diskinden okumaktı**.
  **(b)** `$`-ayraçlı bir hash'in loader'dan sağ çıkamayacağını iddia eden test **rastgele bir tuz**
  kullanıyordu; `dotenv-expand` yalnız geçerli değişken adlarını genişlettiği için bir base64url segmentin
  harfle başlaması **yazı-turaydı**: izolede PASS, PASS, FAIL. Altındaki ürün gerçeği iddiadan değerliydi ve
  iddia onu gizliyordu — **eski biçimin hayatta kalması operatörün tuzuna göre kurulumdan kuruluma değişir.**
  **(c)** Bir spec fixture'ın son isteğini **bir UI durumunu bekledikten sonra tek seferde** okuyordu.
  "idle" olmaktan çıkmış bir durum konsolun **başladığını** söyler, sunucu-tarafı POST'unun **ulaştığını**
  değil; arada okuyan bir önceki testin gövdesini görür. **İddia tel hakkındaysa telde beklemeli.**
  **(d) FIXTURE'IN ÖZELLİĞİ ULAŞILAMAZ KILDIĞI HÂL, en sinsisi.** Bir yardımcı yolu `EvalSymlinks`'ten
  **önceden** geçirdiği için `/var` → `/private/var` vakası hiç doğamıyordu: test **kendi kurulumunun
  imkânsız kıldığı bir dünyayı** ölçüyordu ve yakalamak için var olduğu perturbasyon altında **yeşil kaldı**.
  **(e)** Aynı testin ikinci yazımı `!Contains(msg, root) && !Contains(msg, real)` diyordu ve **yine yeşil
  kaldı**, çünkü `/private/var/x`, `/var/x`'i **içerir**. Bu ağaç yenilmiş yol/üyelik karşılaştırmalarını bir
  DOĞRULAYICIDA zaten kaydediyor; bir TESTTE aynı kusur daha sessizdir: **yenilmiş bir doğrulayıcı eninde
  sonunda bir şeyi geçirir, yenilmiş bir iddia hiçbir zaman hiçbir şey söylemez.** Doğrusu özelliği iddia
  etmekti: *mesajdaki hiçbir jeton `/` ile başlamaz*.
  **VE TERSİ, ki bu olmadan kural ZARAR VERİR.** Aynı gün on perturbasyondan biri yeşil kaldı ve **test
  doğruydu**: `WorkspaceFS.resolve`'un `..` kontrolü silindi ama `assertNoSymlinkEscape` kapsamayı bağımsız
  denetleyip yolu yine reddetti — **iki guard var**. Özellik kırılmamıştı; kırılan perturbasyondu. *"Yeşil
  perturbasyon = zayıf test"* diye yazılsaydı bu kural insanları zaten doğru testleri güçlendirmeye, daha
  kötüsü **perturbasyonları ısırsın diye derinlemesine-savunma guard'larını zayıflatmaya** yollardı.
  **Kural: bir perturbasyon yeşil kaldığında önce ÖZELLİĞİN gerçekten kırılıp kırılmadığını sor. Test ancak
  kırıldıysa suçludur.**

- **Bir YORUM, kodun sahip olmadığı bir özelliği, okuyucunun onu denetleyeceği tam satırda iddia edebilir —
  ve 2026-08-01'de bunun üç örneği tek bir dalın alanından çıktı.**
  `publish.go:316` *"owner/repo binding'den gelir, modelden değil"* diyordu; `main.go` bir env değişkeni
  kullanıyordu, yani **bir stack kaç binding'e hizmet ederse etsin tam olarak BİR repoya PR açabiliyordu.**
  `publication.go:320` *"HTTP'nin kuyruğa aldığı komut burada boundary pump ile uygulanır"* diyordu; park
  etmiş bir run'da o pompayı **hiçbir şey** koşturmuyordu. `orchestrator.go:724` *"karar yeni bir attempt
  açar"* diyordu; yalnız **Slack** buna uyuyordu.
  Pahalı olmalarının sebebi yorumun dekorasyon olmaması: **okuyucunun çağıranı okumak yerine kabul ettiği
  kanıttır.** Ve düzeltirken **niteliksiz temiz cümleye** kaçmak, aynı kusuru bir katman öteye taşır — o
  daldaki üçüncü örnek, kusuru düzelten commit'in İÇİNDE bulundu: cümle `connection_ref` taşıyan binding
  için doğru olmuş, taşımayan için hâlâ yanlış kalmıştı. **Bir yorumu düzeltirken hangi dalların doğru
  olduğunu say; "artık doğru" diye yazma.**

- **Her yerel kontrolün "bitti", kalıcı kaydın "değil" dediği bir durum.** `git merge --no-commit` tam bunu
  bırakır: staged bir dosya yığını bitmiş gibi okunur, dalın ref'i kımıldamamıştır — ve o merge'ü doğrulamak
  için koşan süit **bayat bir ağacı** ölçer. Aynı aile: her katmanı tasarlandığı gibi davranırken `running`de
  kalan bir run, ve bir zamanlayıcıyla kendini silen bir onay. **Bir durumu "bitti" okumadan önce onu KALICI
  kaydeden şeye sor** — dala, satıra, ref'e; çalışma ağacına değil.
