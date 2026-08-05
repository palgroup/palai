# MCP: panelden tanımla, agent'a ata, makinede çalışsın

**Tarih:** 2026-08-05 · **Hedef (sahibinin ifadesi):** *"bizim bunları admin panelden ayarlayıp agent'a
atayıp agent mac/linux'ta ayağa kalktığında doğal olarak erişimi olması gerekiyor"* — ve *"leak
olmayacak hiçbir şekilde."*

---

## §1 — Zincir tek yerden kopuk, ve bu planın tamamı o halka

Ölçüm (2026-08-05, canlı stack + ağaç):

| Halka | Durum | Kanıt |
|---|---|---|
| 1. Panelden MCP **tanımla** | **YOK** — API var, ekran yok | `POST /v1/mcp-connections` + `/discover` router.go:174-177'de; `apps/web-console/app/` altında `mcp` dizini yok |
| 2. Agent'a **ata** | **VAR** | `app/agents/[id]/page.tsx` `/mcp-connections`'ı çekiyor, `revision-mcp-connection` alanıyla revision'a yazıyor |
| 3. Makinede **erişim** | **VAR** | `extensions/lookup.go:190` — araç ancak run'ın pinlenmiş AgentRevision'ının `mcp_connections`'ı bağlantıyı adlandırırsa çözülür; bearer `secret_ref`'ten **istek anında** alınır (`manager.go:214`) |

**Canlıda 0 bağlantı var**, ve sebebi 2 veya 3 değil: operatörün bağlantı yaratacak bir ekranı yok, tek
yol elle API çağrısı. Panel bağlantı listesini çekiyor ve boş liste gösteriyor.

**Bunun anlamı:** M1 tek başına zinciri kapatır. M2 (OAuth) onu genişletir, M3 (katalog) rahatlatır.

---

## §2 — Endüstri ne yapıyor (deep research, 2026-08-05, 16 doğrulanmış bulgu)

- **İki katman, her iki satıcıda da:** tek-kullanıcı disk JSON'u (`.cursor/mcp.json`, `.mcp.json` /
  `claude mcp add`) + üstüne org katmanı (Cursor Team Settings allowlist, Anthropic `managed-mcp.json`
  ve `allowedMcpServers`/`deniedMcpServers`).
- **Keşif diskten çıktı:** `cursor.com/marketplace`, `claude.ai/directory` — küratörlü web katalogları.
- **Transport yakınsadı:** remote **streamable HTTP** önerilen, SSE **deprecated**, stdio yalnız local.
  *(Bizde zaten Streamable HTTP var — `mcp/http.go`.)*
- **OAuth tam belirtilmiş ve uygulanmış** (Claude Code): RFC 9728 Protected Resource Metadata
  (`/.well-known/oauth-protected-resource`) → RFC 8414 fallback → `WWW-Authenticate` başlığı; **DCR**,
  yoksa **CIMD** otomatik keşfi, yoksa operatörün verdiği `client_id`/`client_secret`; secret **OS
  keychain'de, config'te asla**; `oauth.scopes` ile kapsam sabitleme ve AS destekliyorsa otomatik
  `offline_access`; 401'de **bir kez** refresh-reconnect-retry.
- **VE ASIL BULGU:** *"Neither vendor's admin layer is multi-tenant infrastructure: both govern a FLEET
  OF SINGLE-USER INSTALLS with client-side enforcement, per-user credentials, and no server-side token
  broker."* Anthropic'in kendi cümlesi: *"Claude Code doesn't have a built-in MCP server registry"*, ve
  allow/deny listeleri *"aren't a registry"*. Cursor: *"Allowlisting approves an MCP configuration
  without distributing or installing the server."*

**Palai bu eksende ZATEN doğru tarafta:** bağlantı sunucu tarafında yaşıyor, credential `secret_ref` ile
adlandırılıyor, değer hiçbir zaman istemciye gitmiyor, ve kiracıya göre yalıtılmış. Yani inşa edilecek
şey rakibin kopyası değil — rakibin **yapamadığı** şeyin yüzeyi.

---

## §3 — Cursor'un deeplink'i: REDDEDİLDİ

`cursor://anysphere.cursor-deeplink/mcp/install?name=…&config=<base64(JSON)>` — sunucu yapılandırmasının
**tamamı URL'de**, tek bir onay modalının arkasında.

**Gerekçe (sahibinin ifadesi):** *"cursor'ın deeplink'e ihtiyaç var mı? zaten bizim direkt admin panelde
şak diye ekleyebiliyoruz iyi bir ui/ux ile."* Doğru: deeplink bir **IDE çözümü** — diskteki JSON'u elle
düzenletmek zor olduğu için icat edilmiş. Bizde tanımın yaşadığı yer zaten panel.

**Ve güvenlik gerekçesi, araştırmadan:** o modal *"a UX gate, not admission control"* — argüman önizlemesi
ekran dışına kırpılıyor, 2.0 öncesi markası taklit edilebiliyordu, ve tek aldatıcı tıklama kullanıcının
hesabı altında sandbox'sız çalıştırmaya dönüşüyor.

**Eğer ileride bir "tek tık" istersek**, taşınan şey bir **katalog kaydına referans** olacak
(`?server=jira`), yapılandırmanın kendisi değil. Sunucunun ne olduğunu URL'i gönderen değil, katalog
söyler.

---

## §4 — Sızıntı bütçesi: "leak olmayacak" ne demek, madde madde

Bu bölüm bir dilek listesi değil; her satır ölçülmüş bir yüzey ve bir kabul kriteri.

### 4.1 Bugün KAPALI olanlar — bozulmayacak

| Yüzey | Durum | Kanıt |
|---|---|---|
| Connection okuma yanıtı | Secret dönmüyor | `api/mcp_connections.go:19` — *"non-secret metadata only, RLS-scoped"* |
| Credential saklama | Değer connection satırında değil; `secret_ref` handle | `manager.go:37` — *"secret_ref below is a HANDLE redeemed at request time"* |
| Kiracı yalıtımı | secret_ref'ler `project_id`'ye anahtarlı (mig 000006) | `manager.go:74` |
| İnline credential | Şema reddediyor | `auth.go:95` — `oauth` anahtar allow-list'i, *"a credential must be a secret_ref, never inline"* |
| SSRF | Dial öncesi doğrulama | `VetHTTPURL`, `VetAudience`, egress resolver |
| Yetki tavanı | Rider'da adı geçmeyen bağlantı çözülmez | `extensions/lookup.go:190` |

### 4.2 Bugün AÇIK olan, ve bu planın kapatması gereken

**(a) MCP yanıtları redaksiyondan geçmiyor.** `grep RedactSecrets|RedactValues` → `mcp/` ve
`extensions/` altında **sıfır**. Shell çıktısı iki redaktörden geçiyor (`host/exec.go:170`), MCP aracının
döndürdüğü gövde geçmiyor — ve o gövde modele gidiyor **ve ledger'a yazılıyor**. Bir MCP sunucusunun
yanıtında kendi token'ı, bir Authorization başlığı yankısı ya da bir DSN bulunabilir.
→ **Kabul kriteri:** MCP tool sonucu, shell sonucuyla AYNI redaktör zincirinden geçer, ve bunu
executor'ı süren bir test kanıtlar (bu hafta bir perturbasyon, redaktörü kanıtlamanın çağıranı
kanıtlamadığını gösterdi).

**(b) OAuth akışının hiçbir parçası yok.** `authorization_code`, `refresh_token`,
`client_registration`, `protected_resource`, `WWW-Authenticate` → **hepsi 0 referans**.
`ValidateOAuthMetadata` yalnız bir **şema doğrulayıcı**: `oauth` alanına ne konabileceğini denetliyor,
hiçbir akış çalıştırmıyor. Beyan edilmiş, uygulanmamış.

**(c) OAuth'un kendisi yeni sızıntı yüzeyleri getirir** ve M2 onları baştan kapatmalı:
- `code_verifier` ve `state` **sunucuda** tutulur, tarayıcıya asla gitmez; tek kullanımlık, süreli.
- `client_secret` ve token **yalnız** envelope-encrypted secret store'a yazılır — panele geri dönmez,
  log'a düşmez, event'e girmez, `docker inspect`'te görünmez. (Claude Code bunu OS keychain'e koyuyor;
  bizim eşdeğerimiz secret store, ve bizimki **per-tenant**.)
- Callback rotası `state`'i doğrulamadan token istemez; eşleşmeyen `state` **sessizce değil, sesli**
  reddedilir.
- Redirect URI kiracı tarafından seçilemez — deployment'ın kendi `PALAI_PUBLIC_BASE_URL`'inden türetilir.
- Token yenileme **ledger'a değer yazmaz**; yalnız "yenilendi" olgusu.

**(d) Bir bağlantının araçları modele TANIMIYLA gider.** Bir MCP sunucusunun araç açıklaması düşman
girdisidir (bu ağaçta zaten kayıtlı: harici araç çıktısı güvenilmez olarak işaretleniyor). Katalog
eklendiğinde açıklamalar da aynı muameleyi görmeli.

---

## §5 — M1: `/mcp` konsol ekranı (zinciri kapatan iş)

**Amaç:** operatör panelden bir MCP sunucusu tanımlar, araçlarını görür, ve `/agents/[id]`'de zaten var
olan seçiciden agent'a atar.

| # | İş | Not |
|---|---|---|
| M1.1 | `app/mcp/page.tsx`: bağlantı listesi (ad, URL, transport, etkin mi, son keşif) | `GET /v1/mcp-connections` hazır; `Panel` bileşeni ve kolon deseni `deployment/page.tsx`'te |
| M1.2 | Ekleme formu: ad, URL, transport, credential | Credential **`SecretField`** ile — konsolun React state'ine girmeyen bileşen (registry ekranı bunu zaten yapıyor: önce `POST /v1/secret-refs` mühürler, sonra bağlantı **ref'i adlandırır**) |
| M1.3 | "Araçları keşfet" düğmesi → `POST /v1/mcp-connections/{id}/discover`, dönen araçları listele | Operatör atamadan ÖNCE ne verdiğini görür |
| M1.4 | Boş durum: "hiç bağlantı yok" + ne işe yaradığı | Bugün agents ekranı boş bir seçici gösteriyor ve sebebini söylemiyor |

**Kabul:** panelden oluşturulan bir bağlantı, `/agents/[id]` seçicisinde görünür; atanan bir agent'ın
run'ı o sunucunun aracını çağırabilir. **Uçtan uca canlı bir run ile kanıtlanır** — bu hafta öğrenildiği
gibi, mekanizmayı kanıtlamak yüzeyi kanıtlamaz.

---

## §6 — M2: OAuth ile bağlan (kendi epic'i)

**Amaç:** operatör panelde "Bağlan" der, tarayıcıda yetkilendirir, token **sunucu tarafında** mühürlenir.

Keşif ve akış sırası araştırmanın verdiği kanonik hâliyle:

1. `/.well-known/oauth-protected-resource` (RFC 9728) → yoksa `/.well-known/oauth-authorization-server`
   (RFC 8414) → 401'in `WWW-Authenticate` başlığı.
2. İstemci sağlama: **DCR** → yoksa **CIMD** → yoksa operatörün verdiği `client_id`/`client_secret`.
3. Yetkilendirme: PKCE **S256** (şemamız zaten bunu şart koşuyor), `state` sunucuda, callback
   deployment'ın kendi base URL'inde.
4. Kapsam: `oauth.scopes` ile sabitlenebilir (*"the supported way to restrict an MCP server to a
   security-team-approved subset"*), AS `offline_access` reklam ediyorsa eklenir.
5. Yenileme: 401'de **bir kez** refresh-reconnect-retry; başarısızsa bağlantı bozuk işaretlenir ve
   panelde görünür.

**Bizim farkımız, ve M2'nin var olma sebebi:** Claude Code token'ı **kullanıcının** keychain'ine koyar.
Biz **kiracının** secret store'una koyarız. Aynı akış, farklı sahiplik — ve çok kiracılı olan bu.

---

## §7 — M3: küratörlü katalog (sonra)

Admin'in onayladığı sunucu listesi; operatör listeden seçer, elle URL yazmaz. Anthropic'in bunu
yapmamasının sebebi mimarisi (*"no built-in registry"*), bizde ise doğal: katalog zaten sunucu tarafında.

Ertelenmesinin sebebi: M1 olmadan kataloğun ekleyeceği bir şey yok, ve katalog kayıtları da §4.2(d)
gereği düşman metin muamelesi ister.

---

## §8 — Bu plan NE YAPMIYOR

- **Deeplink yok** (§3).
- **stdio yerel sunucu yönetimi yok.** Ekosistem remote HTTP'ye yakınsadı ve bizim modelimiz sunucu
  tarafı; makinede süreç doğuran bir yerel MCP, bu planın kapsamadığı ayrı bir tehdit yüzeyi.
- **Kullanıcı başına bağlantı yok.** Bağlantı kiracıya aittir, agent revision'ı onu adlandırır. Bir
  kullanıcının kendi hesabıyla bağlandığı model (claude.ai connectors) farklı bir üründür.
