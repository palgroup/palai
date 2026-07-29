# Cloud self-host smoke — ölçülen rapor (2026-07-29)

Bu rapor `docs/operations/install.md`'i **hiç görmemiş bir operatör gibi**, harfi harfine takip
etmenin sonucudur. Yol A (paketlenmiş `palai` binary'si, ağaç dışından) izlendi; stack production
overlay ile ayağa kaldırıldı; gerçek bir provider kimlik bilgisiyle gerçek run'lar çalıştırıldı;
backup → restore → verify döngüsü ve sertifika değişimi ölçüldü.

**Her iddia, aşağıda transcript'i bulunan gerçek çıktıya dayanır.** Kimlik bilgileri hiçbir yerde
argv'ye, log'a, evidence'a veya commit'e girmedi.

- Fork noktası: `cloud-smoke` @ `2356935`
- Host: macOS (darwin/arm64), Docker Engine 24.0.2, Compose 2.38.1, OpenSSL 3.6.3, Go 1.26.5
- Stack: `palai/{control-plane,runner,reference-engine}:smoke` (checkout'tan, **yalnız linux/arm64**)

---

## Karar (tek paragraf)

**Sahibi yarın bunu bir cloud VM'ine kurarsa: stack ayağa kalkar ve gerçekten çalışır — ama tek bir
şeyi yapmaya kalkarsa, gerçek alan adı için sertifikayı değiştirmeye kalkarsa, stack'i sessizce
kırar.** Adım 1–6 temiz: beş servis 27 saniyede healthy oluyor, TLS edge tek yayınlanan yüzey,
`/metrics` gerçekten dışarı kapalı, provisioning edge üzerinden çalışıyor ve **gerçek bir OpenAI
run'ı uçtan uca tamamlanıyor** (`gpt-4o-mini-2024-07-18` → "Ankara", "Paris", "Tokyo"). `palai backup
→ restore → restore verify` döngüsünün altı kontrolü de yeşil ve backup'tan önceki canlı run
restore'dan sonra edge üzerinden geri okunabiliyor — felaket kurtarma hikâyesi gerçek. Buna karşılık
üç şey kırık ve üçü de dokümanın kendi mutlu yolunda: (1) dokümanın önerdiği `/healthz` sağlık
kontrolü **vakum** — control-plane durdurulmuşken bile `200` ve `exit 0` döndürüyor, yani operatörün
smoke testi yalan söylüyor; (2) `PALAI_COMPOSE_PROJECT` operatörün seçtiği bir isim olarak
belgelenmiş ama `palai backup/restore/support-bundle` konteyner adlarını `config.json`'daki *başka*
bir isimden türetiyor — dokümanı takip eden bir kurulumda **backup komple ölü**, stack sapasağlam
görünürken; (3) adım 7'nin sertifika değişimi **uygulanabilir değil** — o sertifika runner
gateway'iyle paylaşılıyor ve runner tam olarak tek bir SAN'a pin'li, dolayısıyla edge'i memnun eden
her sertifika runner'ı kırıyor, üstelik **gecikmeli olarak**: değişimden hemen sonra her şey sağlıklı
görünüyor, kırılma bir sonraki control-plane restart'ında (yani `restart: always` ile ilk VM
reboot'unda) geliyor ve semptomu "run'lar sonsuza kadar queued kalıyor", hiçbir yerde "certificate"
yazmıyor. (1) ve (2) doküman düzeltmesiyle kapatıldı ve düzeltmeler bu branch'te. (3) bir tasarım
boşluğu; dokümana gerçeği yazdım, kodu değiştirmedim. Gerçek alan adı için bugün çalışan yol: TLS'i
Palai edge'inin **önündeki** bir proxy'de (cloud LB / nginx) sonlandırmak.

---

## install.md'den sapmalar — 8 adet

| # | Adım | Doküman ne diyor | Gerçekte ne oldu |
|---|---|---|---|
| S1 | Prerequisites | "The three stack images reachable by your Docker daemon" | İmajların **nasıl** üretileceği hiçbir yerde yazmıyor. `scripts/release/build.sh`'i kendim bulup flag'lerini okumak zorunda kaldım. Üstelik **varsayılanı amd64+arm64** — kısıtlamazsan tek mimarili bir host'ta gereksiz iş. *Düzeltildi.* |
| S2 | 1 | `export PALAI_HOME=/srv/palai` | macOS'te `/srv` yok ve root ister. `/private/tmp/palai-smoke-op/srv-palai` kullandım. **Ortamsal**, cloud'da bulgu değil. |
| S3 | 2 | "A hand-run compose must also create the GitHub App key slot" | `palai init` bu dosyayı **zaten** oluşturmuş (`secrets/github-app-key`). `touch` zararsız ama gereksiz. Önemsiz. |
| S4 | 4 | `docker compose ... config >/dev/null && echo OK` | `OK` yazdı ama önce **4 uyarı**: `PALAI_PG_PORT/S3_PORT/API_PORT/RUNNER_PORT is not set`. Zararsız (overlay onları `!reset` ediyor) ama doküman operatörü hazırlamıyor. |
| S5 | 6 | `curl .../healthz` | **Vakum.** `200`/0 byte döndürüyor; var olmayan bir path ile ayırt edilemiyor; control-plane **durdurulmuşken** bile geçiyor. Probe'u `/v1/capabilities` ile değiştirmek zorunda kaldım. *Düzeltildi.* |
| S6 | 6 | "admin CLI pointed at `https://control-plane:${PALAI_EDGE_PORT}`" | Çalışmıyor: `lookup control-plane: no such host`. IP koyunca `x509: ... doesn't contain any IP SANs`. `/etc/hosts` kaydı gerekiyor ama doküman söylemiyor. (Bu makinede sudo yok; container'da `--add-host` ile ispatladım.) *Düzeltildi.* |
| S7 | 3 | "set ... `PALAI_COMPOSE_PROJECT`" (örnek: `palai-prod`) | `palai backup` **çöktü**: `No such container: palai-21e1258e-postgres-1`. İsim `config.json`'dan geliyor. Stack'i indirip doğru proje adıyla yeniden kurmak zorunda kaldım. *Düzeltildi.* |
| S8 | 7 | sertifikayı değiştir, `up -d edge` | İki ayrı kırılma: `up -d edge` sertifikayı **hiç yüklemiyor** (no-op), ve değişim runner'ı bir sonraki restart'ta öldürüyor. *Doküman düzeltildi, tasarım boşluğu filed.* |

---

## Bulgular

| # | Ne kırıldı | Nerede | Aksiyon | Cloud için önem |
|---|---|---|---|---|
| **1** | Dokümanın sağlık kontrolü vakum: `/healthz` edge üzerinden her zaman `200`/exit 0, control-plane ölüyken bile | `docs/operations/install.md:144` (eski) · Caddyfile `deploy/compose/production.yml:97` | **Düzeltildi** — probe `/v1/capabilities`, gerekçesiyle | **Yüksek** — operatörün "stack sağlıklı" kanıtı yalan; ölü stack'i yeşil raporlar |
| **2** | `PALAI_COMPOSE_PROJECT` ile `config.json`'daki `project` bağlanmıyor → `backup`/`restore`/`support-bundle` yanlış konteyner adını arıyor | `cmd/cli/internal/stack/install_backup.go:418` · `docs/operations/install.md:94` | **Düzeltildi** — doküman artık iki ismi bağlıyor | **Yüksek** — backup, en kötü gün çalışmadığını öğreneceğin şey. Stack sağlıklı görünürken DR tamamen ölü |
| **3** | Edge sertifikası runner gateway ile **paylaşılıyor**; runner tam olarak tek SAN (`control-plane`) pin'liyor → gerçek alan adı sertifikası runner'ı kırıyor, **gecikmeli** | `deploy/compose/production.yml:70-71` (edge) + `deploy/compose/compose.yaml:61-62` (runner gw) · pin: `packages/runner/enrollment.go:142`, `session.go:302`, `renewal.go:101` | **Filed** — doküman gerçeği söylüyor, kod değişmedi | **Kritik** — "gerçek alan adı" tam da sahibin istediği şey. Kırılma reboot'a kadar gizli, semptom "run'lar queued kalıyor", hiçbir mesajda sertifika geçmiyor |
| **4** | `up -d edge` sertifikayı reload etmiyor (compose config değişmedi sanıyor); doğrusu `restart edge` | `docs/operations/install.md:170` (eski) | **Düzeltildi** | **Orta** — tek başına da bir tuzak: operatör sertifikayı değiştirir, eski sertifika servis edilmeye devam eder |
| **5** | Production stack için **sağlık komutu yok**. `palai local doctor` host portlarına bakıyor, overlay onları yayınlamıyor → 15 kontrolün 13'ü yanlış sebeple kırmızı | `cmd/cli/internal/stack/doctor.go:51` · `docs/operations/operability.md:78` | **Filed** — install.md'ye ceiling notu eklendi | **Orta** — `operability.md`'de dürüstçe kabul edilmiş ama install.md'den link yoktu; operatörün elinde `compose ps` + tek curl kalıyor |
| **6** | Kaynak alt sınırları (RAM/disk/core) **hiçbir dokümanda yazmıyor**; `doctor`'ın disk kontrolü **oran** tabanlı (`free/total < %10`), mutlak değil | `cmd/cli/internal/stack/doctor_v2.go:26` | **Filed** | **Orta** — 20 GB'lık bir VM'de %10 = 2 GB "yeşil"; 460 GB'lık bu host'ta 40.6 GB **kırmızı**. Oran, kaynak tabanı için yanlış şekil |
| **7** | Route A'nın gerektirdiği imajların nasıl üretileceği yazmıyor; `build.sh` varsayılanı çift mimari | `docs/operations/install.md:44` | **Düzeltildi** | **Düşük** — engelleyici değil ama ilk adımda operatörü durduruyor |
| **8** | Belgelenen `docker compose config` komutu 4 uyarı basıyor | `deploy/compose/production.env.example` | **Filed** (kozmetik) | **Düşük** — `!reset` edilmiş portlar; zararsız ama gürültü |

### Düzeltilmeyenlerin gerekçesi

Bulgu 3 ve 5 **tasarım boşluğu**, doküman hatası değil. Bulgu 3'ün doğru düzeltmesi production
overlay'e `PALAI_EDGE_CERT`/`PALAI_EDGE_KEY` çifti eklemek (varsayılan `${PALAI_HOME}/ca/server.*`),
böylece runner tam pin'ini korurken edge kendi kimliğini alır — ~4 satır, ama shipped bir production
posture'ı ve kendi guard test'lerini (`deploy/compose/production_guard_test.go`) etkiliyor. Bulgu 5
için "doctor'ı production'da yeşil yapmak" ancak kontrolleri zayıflatarak olur; yapılmadı.
`docs/operations/known-gaps-1.0.md`'ye satır **eklenmedi**: o dosya kendi ifadesiyle "decisions, not
observations" ve sahibinin aldığı kararların kaydı — bir smoke agent'ın gözlemi oraya yazılmaz.

---

## Transcript

### Ön koşullar

```
$ docker compose version
Docker Compose version 2.38.1
$ docker version --format '{{.Server.Version}} api={{.Server.APIVersion}} arch={{.Server.Arch}}'
24.0.2 api=1.43 arch=arm64
$ openssl version
OpenSSL 3.6.3 9 Jun 2026
```

### İmajlar (S1 — dokümanda olmayan adım)

```
$ scripts/release/build.sh --tag smoke --out /tmp/palai-smoke-rel \
    --platforms linux/arm64 --cli-targets darwin/arm64 --runner-archs arm64
...
build.sh: wrote /tmp/palai-smoke-rel/release-manifest.json (version 0.15.0, stamp 0.15.0+g2356935)
build.sh: wrote /tmp/palai-smoke-rel/release-index.json (SOURCE_DATE_EPOCH=1785330501)
   2.97s user 3.63s system 17% cpu 37.726 total
```

Üç imaj 38 saniyede, **yalnız arm64, qemu yok**.

### Adım 1–2

```
$ palai init
initialised /private/tmp/palai-smoke-op/srv-palai (project palai-21e1258e, api :62606)
```

`compose/` materyalize oldu → Yol A doğru çözümlendi. `runner-token` yok (doküman doğru),
`secrets/github-app-key` **zaten var** (doküman fazladan `touch` söylüyor — S3).

### Adım 4 — posture doğrulaması

```
$ palai config validate --env-file production.env
master_key         ok       master key present, 32-byte hex, not a dev-default
bootstrap_key      ok       bootstrap api-key present and not a placeholder
cert_pair          ok       edge TLS cert/key pair present and readable
dispatch_workers   ok       dispatch exec-path on (1 worker(s))
edge_only_surface  ok       edge is the only host-published surface; Caddyfile proxies only /v1/*
                            (/metrics + /healthz not edge-reachable)
env_contract       ok       6 required keys present, no unknown keys
config valid
```

> `edge_only_surface` satırı `/healthz`'in edge'den erişilemez olduğunu **zaten söylüyor** — yani repo
> kendi içinde çelişiyordu: validator doğruyu biliyor, install.md yanlışı öğretiyordu.

```
$ docker compose --env-file production.env -f .../compose.yaml -f .../production.yml config >/dev/null && echo OK
level=warning msg="The \"PALAI_PG_PORT\" variable is not set. Defaulting to a blank string."
level=warning msg="The \"PALAI_S3_PORT\" variable is not set. Defaulting to a blank string."
level=warning msg="The \"PALAI_API_PORT\" variable is not set. Defaulting to a blank string."
level=warning msg="The \"PALAI_RUNNER_PORT\" variable is not set. Defaulting to a blank string."
OK
```

### Adım 5 — ayağa kalkış (27 sn)

```
 Container palai-smoke-postgres-1      Healthy
 Container palai-smoke-object-store-1  Healthy
 Container palai-smoke-control-plane-1 Healthy
 Container palai-smoke-runner-1        Healthy
 Container palai-smoke-edge-1          Healthy
```

Yayınlanan yüzey — **yalnız edge**, overlay sözünü tutuyor:

```
palai-smoke-edge-1           0.0.0.0:443->443/tcp
palai-smoke-control-plane-1  (yok)
palai-smoke-postgres-1       5432/tcp        (yayınlanmamış)
palai-smoke-object-store-1   8333/tcp ...    (yayınlanmamış)
```

### Bulgu 1 — sağlık kontrolü vakum

Aynı komut şekliyle dört path:

```
$ curl --cacert ca.crt --resolve control-plane:443:127.0.0.1 https://control-plane:443/healthz
[code=200 bytes=0]
$ curl ... https://control-plane:443/this-endpoint-never-existed
[code=200 bytes=0]
$ curl ... https://control-plane:443/metrics
[code=200 bytes=0]                      <-- güvenlik özelliği DOĞRU çalışıyor
$ curl ... -H "Authorization: Bearer <bootstrap>" https://control-plane:443/v1/capabilities
{"object":"capabilities","maturity":"preview",...}
[code=200 bytes=355]
```

Gerçek `/healthz` compose ağı içinden:

```
$ docker run --rm --network palai-smoke_default curlimages/curl -sS http://control-plane:8080/healthz
ok
[code=200 bytes=2]
```

**Kesin kanıt — control-plane durdurulmuşken:**

```
$ docker compose ... stop control-plane
 Container palai-smoke-control-plane-1  Stopped

$ curl ... https://control-plane:443/healthz
[code=200 bytes=0]
curl exit: 0        <-- belgelenen sağlık kontrolü hâlâ BAŞARILI diyor

$ curl ... https://control-plane:443/v1/capabilities
[code=502 bytes=0]  <-- gerçek API doğru davranıyor
```

### Bulgu 6 (S6) — admin CLI edge'e ulaşamıyor

```
$ PALAI_BASE_URL="https://control-plane:443" palai org list --ca ca.crt
palai: GET /v1/organizations: ... dial tcp: lookup control-plane: no such host

$ PALAI_BASE_URL="https://127.0.0.1:443" palai org list --ca ca.crt
palai: ... tls: failed to verify certificate: x509: cannot validate certificate for 127.0.0.1
       because it doesn't contain any IP SANs
```

İsim çözümlenince aynı CLI çalışıyor (`--add-host` ile ispat):

```
$ docker run --rm --network palai-...  --add-host control-plane:172.20.0.6 ... /palai org create \
    --display-name "Cloud Smoke Org" --ca /ca.crt
{
  "id": "org_1710a4db95300d682c809eeb8efea21e",
  "default_project_id": "prj_8d92a0c945c8eab7582e691916e7d214",
  "admin_api_key": { "id": "key_...", "key": "sk_<REDACTED>" }
}
```

### Gerçek run — uçtan uca, edge üzerinden

Önce `PALAI_MODEL_PROVIDER=fake` (belgelenen varsayılan):

```
$ curl ... -H "Idempotency-Key: <uuid>" -d '{"input":"Say the word ORANGE and nothing else."}' \
       https://control-plane:443/v1/responses
{"id":"resp_d5bc...","run_id":"run_e351...","status":"queued",...}
[code=202]

$ curl ... https://control-plane:443/v1/responses/resp_d5bc...
{"id":"resp_d5bc...","model":"fake","output":[{"content":"ok","type":"message"}],"status":"completed"}
```

Runner gerçekten bir engine lease'i aldı — kısa devre değil:

```
runner-1  | received lease lease_att_e2e8e3db9e6a7be0 for run run_e351170756c7d3f23c5e033c19b7173b (fence 1)
runner-1  | engine completed for run run_e351170756c7d3f23c5e033c19b7173b: 1764 stdout bytes
```

Sonra **canlı provider** (`.env.local`'dan `OPENAI_API_KEY`, dosya-secret olarak, argv'ye
girmeden):

```
$ printf '%s' "$OPENAI_API_KEY" > ${PALAI_HOME}/secrets/provider-one   # değer echo edilmedi
provider-one secret bytes:      164
$ # production.env: PALAI_MODEL_PROVIDER=provider-one, PALAI_MODEL=gpt-4o-mini

$ curl ... -d '{"input":"Reply with exactly one word: the capital city of Turkey."}' .../v1/responses
[1] status=queued ... [8] status=completed
{
    "id": "resp_f5f3053436362c8de70eb92117026163",
    "model": "gpt-4o-mini-2024-07-18",
    "output": [ { "content": "Ankara", "type": "message" } ],
    "usage": { "input_tokens": 51, "output_tokens": 2, "total_tokens": 53 }
}
```

**Gerçek bir model çağrısı, gerçek token sayımıyla, TLS edge üzerinden.**

### Bulgu 2 — backup ölü

```
$ palai backup --out backup1.tar.gz
palai: read migration version: docker exec -i palai-21e1258e-postgres-1 sh -c ...
       Error response from daemon: No such container: palai-21e1258e-postgres-1
REAL EXIT CODE = 1                      (arşiv yazılmadı — hata dürüst, hedef yanlış)

config.json project  : palai-21e1258e
PALAI_COMPOSE_PROJECT: palai-smoke      <-- dokümanın operatöre seçtirdiği isim
container that exists: palai-smoke-postgres-1
```

`palai backup` için `--project` flag'i **yok**; iki ismi bağlayan tek bir doküman satırı da yoktu.

### Backup → restore → verify (isimler hizalandıktan sonra)

```
$ palai backup --out backup1.tar.gz
backup: dumping Postgres (pg_dump -Fc)…
backup: copying the object-store volume…
backup written: migration v44, 2 org(s), 65 object-file(s)          [23.5 sn, 51282 byte]

manifest: { "migration_version": 44, "sample_response_id": "resp_7bc35cc5c1038d06ac6be33b986f54e1",
            "db_dump_sha256": "8d6558fb...", "object_store_sha256": "f030c553..." }
```

Stack tamamen silindi (`down -v`), boş stack doğrulandı (eski key → `401 invalid_token`), sonra:

```
$ palai restore --archive backup1.tar.gz
restore: stopping writers (control-plane, runner)…
restore: loading Postgres (pg_restore --clean)…
restore: replacing the object-store volume…
restore: starting writers…
restore complete: migration v44, 2 org(s) loaded into palai-21e1258e     [52 sn]

$ palai restore verify --archive backup1.tar.gz
archive_checksum       ok    db+object-store members match manifest (65 object-file(s))
migration_version      ok    v44 matches manifest
tenant_ids             ok    all 2 manifest org id(s) present in restored data
run_retrieval          ok    response resp_7bc3... retrievable from restored data
rls_isolation          ok    85 org-bearing tables FORCE RLS, 85 tenant_isolation policies
secret_decrypt         ok    no secret_refs to verify
restore verify: all checks green
```

Bağımsız kanıt — backup'tan **önceki** canlı run, restore'dan sonra edge üzerinden geri okundu:

```
{ "id": "resp_7bc35cc5c1038d06ac6be33b986f54e1", "model": "gpt-4o-mini-2024-07-18",
  "output": [ { "content": "Paris", "type": "message" } ], "status": "completed" }
```

### Bulgu 3 — sertifika değişimi (kritik)

Başlangıç durumu:

```
$ openssl x509 -in ca/server.crt -noout -subject -ext subjectAltName
subject=CN=control-plane
X509v3 Subject Alternative Name:  DNS:control-plane          <-- TEK SAN

$ curl http://control-plane:8080/healthz/runner     (ağ içinden)
{"gateway":true,"identity":{...},"sessions":1}               <-- runner bağlı
```

**Adım 7 harfi harfine** — `palai.example.com` için sertifika (aynı CA, en lehte senaryo):

```
$ cp newdomain.crt ${PALAI_HOME}/ca/server.crt ; cp newdomain.key ${PALAI_HOME}/ca/server.key
$ docker compose ... up -d edge
 Container palai-21e1258e-control-plane-1  Healthy       <-- edge'e HİÇ dokunulmadı

$ curl --resolve palai.example.com:443:127.0.0.1 https://palai.example.com:443/v1/capabilities
curl: (60) SSL: no alternative certificate subject name matches target host name 'palai.example.com'

$ echo | openssl s_client -connect 127.0.0.1:443 -servername palai.example.com | openssl x509 -noout -subject
subject=CN=control-plane            <-- edge HÂLÂ ESKİ sertifikayı servis ediyor
$ openssl x509 -in ${PALAI_HOME}/ca/server.crt -noout -subject
subject=CN=palai.example.com        <-- diskteki yeni sertifika
```

→ **Bulgu 4: `up -d edge` bir no-op.** (`restart edge` çalışıyor — ayrıca ölçüldü.)

Değişimden hemen sonra runner **hâlâ sağlıklı** (control-plane eski sertifikayı bellekte tutuyor):

```
{"gateway":true,"identity":{...},"sessions":1}
```

Control-plane restart edildiğinde (= VM reboot, `restart: always`):

```
{"gateway":true,"sessions":0}

runner-1 | enroll: ... tls: failed to verify certificate:
           x509: certificate is valid for palai.example.com, not control-plane
```

Operatörün gördüğü semptom — API'de sertifikadan tek kelime yok:

```
run after the documented cert swap: resp_a27e69fa36b6dbd1c09cec92a1a4ac8f
[1] status=queued  [2] status=queued  ...  [7] status=queued
```

**Çift SAN'lı sertifika da reddediliyor** (denendi, çözüm değil):

```
$ openssl x509 -in both.crt -noout -ext subjectAltName
    DNS:control-plane, DNS:palai.example.com

runner-1 | enroll: ... controller certificate DNS identity is not exact
```

Kaynağı — `packages/runner/enrollment.go:142`:

```go
if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != config.ControllerDNS {
    return errors.New("controller certificate DNS identity is not exact")
}
```

Aynı kontrol `session.go:302` ve `renewal.go:101`'de de var. Bu, runner için **doğru** bir güvenlik
özelliği; kusur, edge'in aynı dosyayı paylaşması.

Teşhisin kesinliği — orijinal sertifika geri konunca her şey düzeliyor:

```
{"gateway":true,"identity":{...},"sessions":1}
[6] status=completed
{ "model": "gpt-4o-mini-2024-07-18", "output": [ { "content": "Tokyo" } ] }
```

### Bulgu 5 — production'da doctor

```
$ palai local doctor
host_quarantine    fail   connect Postgres: ... 127.0.0.1:62608: connection refused
api                fail   GET /v1/capabilities: ... 127.0.0.1:62606: connection refused
migration          fail   ... runner fail ... image_digests fail ... callback fail
disk               fail   data dir 8.8% free (40.6GiB of 460.4GiB) — under the 10% floor
retention_ttl      ok     store:false retention disabled (ttl=0)
provider           ok     no provider configured
...
palai: doctor: one or more checks are not green         (15 kontrolün 13'ü kırmızı)
```

Hepsi aynı sebepten: doctor `config.json`'daki **host portlarına** bakıyor, overlay onları
yayınlamıyor. `support-bundle` ise `docker exec` kullandığı için **çalışıyor**:

```
$ palai support-bundle --out bundle.tar.gz
support bundle written to /tmp/palai-smoke-op/bundle.tar.gz (5 parts, redacted)
```

### Kimlik bilgisi sızıntısı kontrolü (vakum değil)

```
$ grep -rlF "$OPENAI_API_KEY" bundle-x | wc -l
0                     support-bundle üyeleri (5 dosya, açılmış)
$ pg_restore -f - db.dump | wc -c
290119                backup'ın DB dump'ı, AÇILMIŞ (ham -Fc üzerinde grep vakum olurdu)
$ pg_restore -f - db.dump | grep -cF "$K"
0
$ grep -lF "$OPENAI_API_KEY" canary.txt
control OK: grep finds a planted copy      <-- taramanın vakum OLMADIĞININ kanıtı
```

### Ölçülen kaynak kullanımı (Bulgu 6 için veri)

Beş servis, boşta, birkaç run sonrası:

| Servis | CPU | Bellek |
|---|---|---|
| control-plane | 18.4% | 10.8 MiB |
| postgres | 4.5% | 81.2 MiB |
| object-store | 1.0% | 117.3 MiB |
| edge | 0.0% | 12.3 MiB |
| runner | 0.0% | 5.2 MiB |
| **toplam** | | **~227 MiB** |

İmajlar: control-plane 25.9 MB + runner 15.4 MB + reference-engine 144 MB + caddy 49.3 MB +
postgres:15 467 MB + seaweedfs 241 MB ≈ **943 MB**. Backup arşivi 2 org / 65 obje için 51 KB.

Bu sayılara dayanarak **1 GB RAM'lik bir VM boşta yeter ama run başına engine container'ı
hesaba katılmamıştır**; disk için asıl taban imajlar (~1 GB) + Postgres + obje deposudur. Hiçbir
doküman bir alt sınır yazmıyor ve `doctor`'ın **oran** tabanlı kontrolü küçük diskte kullanışsız:
20 GB'lık bir VM'de %10 = 2 GB ile "yeşil" der.

---

## Bu makineden yürüyemediğim ayaklar

Bunlar **test edilmedi**; rapor bunları kapsıyormuş gibi okunmamalı.

1. **Gerçek bir cloud VM.** Her şey macOS/arm64 üzerinde Docker Desktop'ta koştu. Linux kernel'a özgü
   davranış (cgroup, systemd birimi `deploy/systemd/palai-stack.service`, gerçek reboot) ölçülmedi.
2. **`linux/amd64` imajları.** Makine disiplini gereği qemu yasaktı; yalnız arm64 üretildi ve
   koşuldu. Çoğu cloud VM'i amd64'tür — imajların o mimaride koştuğu **bu smoke'ta doğrulanmadı**.
3. **Gerçek alan adı + gerçek ACME sertifikası.** Bulgu 3 aynı yerel CA'dan üretilmiş sertifikalarla
   ölçüldü. Public bir CA sertifikası **daha sert** başarısız olur (runner CA'yı da pin'liyor), ama
   bu ölçülmedi.
4. **Önerdiğim çözüm — edge'in önünde ikinci bir proxy.** Bunu **kurmadım**. Dayanağım dolaylı:
   Caddyfile `auto_https off` + açık `tls` çiftiyle her SNI'ye kendi sertifikasını sunuyor (SNI
   `palai.example.com` ile bağlanıp `CN=control-plane` sertifikası aldım). Mantıklı, ama **ölçülmüş
   değil** — bir cloud LB'nin arkasında doğrulanması gereken bir öneridir.
5. **`/etc/hosts` kaydı.** Bu makinede sudo yok; CLI'ı container içinde `--add-host` ile ispatladım.
   Kaydın kendisi denenmedi (davranışı aynıdır).
6. **Gerçek reboot.** `restart: always` semantiği `docker compose restart` ile simüle edildi, host
   reboot'u ile değil.
7. **Yük/dayanıklılık.** Tek eşzamanlı run'lar çalıştırıldı; eşzamanlılık, uzun süreli çalışma,
   disk dolması, queue backlog davranışı ölçülmedi.
8. **Port 443'ün ayrıcalıksız bind'i.** macOS'te Docker Desktop sorunsuz bind etti; Linux'ta
   `net.ipv4.ip_unprivileged_port_start` / `CAP_NET_BIND_SERVICE` bu smoke'ta test edilmedi.
9. **Kubernetes / çok düğüm** (`docs/operations/kubernetes.md`) hiç dokunulmadı.

---

## Makine durumu

Smoke sırasında host'ta bana ait olmayan bir stack zaten koşuyordu (`storage-*` compose projesi +
`iceberg-rest`, portlar 5432/5433/6453/6454/9000/9001/8181/50020). **Dokunulmadı**; production
overlay hiçbir port yayınlamadığı için çakışma da olmadı. Kendi stack'im tek seferde bir tane
çalıştırıldı ve rapor sonunda `down -v` ile kaldırıldı.
