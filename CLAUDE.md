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
