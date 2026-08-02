# Terminoloji hizalaması — tasarım (2026-08-02)

Ağacın sözlüğünü küresel olarak yerleşik adlara hizalar. Kapsam **üç iş**; dördüncü aday
ölçüldü ve **düşürüldü** (§6).

Bu doküman CLAUDE.md'nin dört kuralına uyar: her sayı onu üreten komutla yazılır, her
runtime iddiası bir yazar gösterir, devralınan tavan tarihlidir. Task'ın ilk adımı §2'nin
komutlarını **yeniden koşmaktır** — değişen sayı anında görünür.

---

## 1. Karar

Ağacın omurgası zaten küresel: `Project`, `Response`, `Run`, `ToolCall`, `Artifact`,
`Runner`, `Sandbox`, `Approval`, `Event`, `Workspace`, `Lease`, `Attempt`. Bunlara
dokunulmaz.

**Değişenler:**

| # | Bugün | Sonra | Neden |
|---|---|---|---|
| T1 | `ModelStep` | `Turn` | Kavram var, adı yok. Küresel tanım: *"a single AI invocation, including any associated tool calls"* (OpenAI Agents SDK) |
| T2 | `state` \| `status` karışık | dışarıda `status`, içeride `state` | Çoğunluk zaten böyle; 6 aykırı var |
| T3 | 12 bağımsız terminal sözlüğü | kanonik sözlük + **gerekçeli** istisna listesi | "Başarıyla bitti" için 4 ayrı kelime kullanılıyor |

**Açıkça değişmeyenler ve nedenleri:**

- **`Session` → `Conversation` YAPILMAZ.** Ölçüldü: 94 tablonun ~40'ı ve tüm public
  rotalar. Sahibi bunu kapsam dışı bıraktı (2026-08-02). Bedeli değil, kararı budur.
- **`Message` → `Item` YAPILMAZ.** Aynı karar.
- **`Runner` → `Worker` YAPILMAZ.** `Runner` zaten küresel doğru (GitHub Actions,
  Buildkite). Buradaki gerçek karışıklık bir **isim** değil bir **sınır** sorunudur:
  runner tool çalıştırmaz — engine'i sandbox'ta barındırıp süpervize eder
  (`MASTER-SPEC.md:1743`), tool'lar control-plane tarafında koşar
  (`packages/tool-broker/`). Adı değiştirmek o karışıklığı çözmez, taşır. §7'de bunun
  yerine ne yapıldığı yazılıdır.
- **`tasks` → `todos` YAPILMAZ.** §6.

---

## 2. Ölçüm envanteri

Her satır kendi komutunu taşır. Task bunları **önce yeniden koşar**.

```bash
# T1 dokunma alanı
rg -cN --glob '!node_modules' --glob '!*.lock' 'model_step|ModelStep' . \
  | awk -F: '{s+=$2} END{print s+0}'        # → 226   (2026-08-02)

# T2 aykırıları — dahili depolamada 'status' kullanan tablolar
cat storage/migrations/*.up.sql | awk '
  /^CREATE TABLE/{t=$0; sub(/CREATE TABLE (IF NOT EXISTS )?/,"",t); sub(/ ?\(.*/,"",t)}
  /^[ \t]+status TEXT/{print t}' | sort -u
# → durable_jobs, idempotency_records, runners, schedules, tasks   (5)  (2026-08-02)

# T2 aykırısı — public şemada 'state' kullanan tek yer
rg -nN '"state"' protocols/schemas -g '*.json'
# → protocols/schemas/execution/skill.json   (1)  (2026-08-02)

# T2 çoğunluk (değişmeyen taraf)
cat storage/migrations/*.up.sql | grep -cE "^[ \t]+state TEXT"    # → 22  (2026-08-02)
rg -oN '"status"' protocols/schemas -g '*.json' | wc -l           # → 13  (2026-08-02)

# T3 — 'başarıyla bitti' için kullanılan kelimeler
cat storage/migrations/*.up.sql \
  | grep -ooE "'(completed|succeeded|exited|delivered)'" | sort | uniq -c
# → completed 7, delivered 7, succeeded 1, exited 1   (2026-08-02)

# T3 — bağımsız terminal sözlüğü sayısı
cat storage/migrations/*.up.sql \
  | grep -oE "CHECK \((state|status|outcome|entry_kind) IN \([^)]*\)" | sort -u | wc -l
# → 12   (2026-08-02)

# En son migration (T2 bunun ardına yazılır)
ls storage/migrations/*.up.sql | tail -1
# → 000055_run_instructions.up.sql   (2026-08-02)
```

---

## 3. T1 · `ModelStep` → `Turn`

### 3.1 Değişen yüzeyler

```
Go tipleri / alanlar   ModelStep*  →  Turn*
Public event tipleri   model_step.created.v1      →  turn.created.v1
                       model_step.delta.v1        →  turn.delta.v1
                       model_step.completed.v1    →  turn.completed.v1
                       model_step.failed.v1       →  turn.failed.v1
                       model_step.interrupted.v1  →  turn.interrupted.v1
Şema alanı             max_tool_calls  →  max_turns   (+ §3.3)
```

Event listesi ölçümle alındı, sayılmadı:
```bash
rg -oN 'model_step[a-z._0-9]*' protocols/asyncapi/asyncapi-3.1.yaml | sort -u   # → 5  (2026-08-02)
```

### 3.2 Dört dil, tek rename

T1 yalnızca Go değildir. Ölçüm:
```bash
rg -cN 'model_step|ModelStep|max_tool_calls|MaxToolCalls' \
  sdks/ apps/web-console/app apps/web-console/tests examples/
```
2026-08-02'de dokunan yerler: `sdks/go/types.go`, `sdks/typescript/src/generated/types.ts`,
`sdks/python/tests/test_sse.py`, `apps/web-console/app/runs/page.tsx`,
`apps/web-console/app/sessions/[id]/page.tsx`,
`apps/web-console/app/api/palai/stream/route.ts`, `examples/nextjs-sdk/` (5 dosya, en
yoğunu `tests/fake-control-plane.mjs` — 8 eşleşme).

TypeScript tarafı **üretilmiş** (`src/generated/types.ts`) — elle düzenlenmez, üreteç
koşturulur.

### 3.3 `max_tool_calls` bir rename DEĞİL, bir boşluk

Bu alan bugün yayınlanmış, iki SDK'da tiplenmiş ve **hiçbir şeyi sınırlamıyor**. Ağaç bunu
zaten kaydetmiş:

```
docs/operations/tool-errors.md:
  grep -rn 'MaxToolCalls' --include='*.go' . | wc -l   ->  2   (2026-08-01)
  `max_tool_calls` today is setting nothing.
```

Yeniden ölçüm (task bunu koşar):
```bash
rg -nN 'MaxToolCalls' --type go -g '!vendor' . | wc -l
```

**Kural:** `max_turns` adını almak yeterli değildir — o ad daha küresel olduğu için yalanı
daha inandırıcı yapar. Alan bir **okuyucu** kazanmalıdır: turn sayacı, limiti aşınca
run'ın terminal durumu, ve bunu kanıtlayan bir test. Rename bu bağlama olmadan
yapılmaz.

**Yazar iddiası (CLAUDE.md kural 3):** bugün `max_tool_calls`'ı yazan kod yok — yani
iddia şudur: *"bunu hiçbir şey okumuyor."* T1 sonunda okuyanı gösteren satır bu spec'e
eklenmelidir.

### 3.4 Evidence bundle'ları ve UAT case'leri

Event adları **yayınlanmış manifest'lerde düz metin olarak** geçiyor:

```bash
rg -lN 'model_step' tests/uat/ evidence/
# → evidence/releases/interactive-0.1.0/manifest.json
#   evidence/releases/local-live-0.1.0-command-spine/manifest.json
#   tests/uat/cases/UI-002/case.yaml
#   tests/uat/command_spine_test.go
#   tests/uat/interactive_journey_test.go        (2026-08-02)
```

`interactive-0.1.0/manifest.json` içindeki iki iddia:
- `"runs.state=completed after a real in-flight abort (model_step.interrupted.v1 journaled)"`
- `"command.applied.v1 applied_sequence=22 lands BETWEEN two real model_step.created.v1 events"`

Bu ağaçta case checksum'ları yeniden hesaplanır ve bundle'lar commit'lidir. **T1
tamamlanmadan önce bundle regen adımı koşulmalı ve checksum'lar yeniden üretilmelidir.**
Aksi hâlde RC kırmızıya döner ve sebebi rename'e değil checksum'a benzer görünür — yani
başarısızlık yanlış dosyayı işaret eder (CLAUDE.md, 2026-08-01 imzası).

### 3.5 Console fixture ledger'ı

`apps/web-console/tests/divergences.mjs` (4 eşleşme) fixture'ın event sözlüğünü **koşan
gerçek router'dan** toplanan yüzeye karşı diff'ler ve fark **kayıtlı** olmalıdır. Ledger
kuralı iki yönlüdür: fark kaydı olmayan bir fark testi kırar, **ve bir fark olmaktan
çıkan bir kayıt da testi kırar**. Event adı değişince hem `fake-control-plane.mjs` hem
`divergences.mjs` aynı commit'te güncellenir.

### 3.6 Başarı ölçütü

```bash
rg -cN --glob '!node_modules' --glob '!*.lock' 'model_step|ModelStep' . \
  | awk -F: '{s+=$2} END{print s+0}'            # → 0
rg -nN 'MaxToolCalls|max_tool_calls' --glob '!node_modules' .   # → 0
```
artı:
- asyncapi drift testi yeşil;
- `max_turns` aşımını kanıtlayan **yeni** bir test, bağlamadan önce RED, bağladıktan
  sonra yeşil (yeşil-önce yazılmaz);
- iki evidence bundle'ı yeniden üretildi, checksum'lar eşleşiyor;
- console conformance + ledger yeşil.

---

## 4. T2 · `state` / `status` tek kural

### 4.1 Kural

> **Dışarıda `status`, içeride `state`.**

Public yüzey (`protocols/schemas`, OpenAPI, SDK tipleri) `status` der — OpenAI Responses
uyumu bunu zaten gerektiriyor. Dahili depolama (`storage/migrations`, Go store tipleri)
`state` der.

Bu kural seçildi çünkü ağacın çoğunluğu **zaten** böyle: dahilide 22 `state`, public'te 13
`status` (§2). Yani bu bir yeniden adlandırma değil, **6 aykırının hizalanmasıdır.**

### 4.2 Düzeltilecek 6 yer

| Yer | Bugün | Sonra |
|---|---|---|
| `durable_jobs.status` | `status` | `state` |
| `idempotency_records.status` | `status` | `state` |
| `runners.status` | `status` | `state` |
| `schedules.status` | `status` | `state` |
| `tasks.status` | `status` | `state` |
| `protocols/schemas/execution/skill.json` | `"state"` | `"status"` |

`tasks.status` satırı §6 ile çelişmez: §6 **tablo adının** değişmediğini söyler
(`tasks` → `todos` düşürüldü), buradaki ise **sütun adıdır**. Tablo `tasks` kalır,
sütunu `state` olur.

Migration `000056_state_status_alignment.up.sql` olarak yazılır (§2'de ölçülen en son
numara 000055). **Paralel task uyarısı:** bu ağaçta paralel task'lar bir sonraki
*bitişik* numaraya yazar ve entegratör merge'de yeniden numaralar — numara çakışırsa
yeniden numaralandırma entegratörün işidir.

### 4.3 Başarı ölçütü

§2'deki iki aykırı-sayma komutu **boş** döner. Artı:
- migration'lar yeşil;
- migration'a dokunulduğu için **tenancy ve catalog korpusları koşulur** — bu ağaçta
  hedefli `-run` seçicileri bu iki korpusu iki kez kaçırdı;
- `go vet -tags="component live" ./...` — düz `vet` tag'li testlerdeki bayat çağıranları
  görmez.

---

## 5. T3 · Terminal durum sözlüğü

### 5.1 Kanonik sözlük

```
queued → running → completed | failed | canceled
```

### 5.2 İstisnalar meşrudur — ama gerekçelenir

12 state machine'i tek sözlüğe zorlamak **yanlış** olur. Farklı alanların gerçekten farklı
terminal semantiği vardır:

- `background_tasks`: `exited | killed | expired | lost` — bu **süreç** semantiğidir; bir
  süreç "completed" olmaz, bir çıkış kodu ile *exit* eder;
- `queue_deliveries` / `webhook_deliveries`: `delivered | dead` — bu **teslimat**
  semantiğidir;
- `approvals`: `approved | denied | expired` — bu **karar** semantiğidir.

Yapılacak iş: kanonik sözlüğü normatif olarak yaz, her istisnayı **tek cümlelik
gerekçesiyle** kaydet, gerekçesi olmayanı hizala. 2026-08-02'de gerekçesiz görünen tek
aday: `ingestion_jobs`'ın `succeeded`'ı — aynı şeyi `durable_jobs` `completed` diye
adlandırıyor.

### 5.3 Sözlüğü koruyan test

Bir test yazılır: her CHECK kısıtı ya kanonik sözlükten gelir ya da **adıyla** istisna
listesindedir. Yeni bir terminal kelime, listeye gerekçesiyle eklenmeden geçemez.

**Bu testin tuzağı (CLAUDE.md, 2026-08-01):** test, ölçtüğü şeyi kendi kurulumuyla
ulaşılamaz kılmamalıdır. Migration'ları **shipped yoldan** okumalı (`storage/embed.go`),
kendi fixture kopyasından değil — yoksa gerçek migration'lar sözlüğü ihlal ederken test
yeşil kalır.

### 5.4 Başarı ölçütü

- Kanonik sözlük + gerekçeli istisna listesi MASTER-SPEC'te normatif bölüm olarak var;
- koruyucu test, **listeye eklenmemiş** yeni bir terminal kelime enjekte edildiğinde
  KIRMIZI olur (perturbasyonla kanıtlanır);
- perturbasyon yeşil kalırsa: önce **özelliğin** gerçekten kırılıp kırılmadığı sorulur.
  Başka bir guard bağımsız olarak yakalıyorsa test suçlu değildir.

---

## 6. Düşürülen aday: `tasks` → `todos`

Öneri ölçüldü ve **yanlış çıktı.**

`tasks` tablosu tek bir tool'a değil, **iki tool'a** hizmet ediyor:

```go
// apps/control-plane/internal/execution/tools/task.go
func TaskTool() toolbroker.Tool { return registryTool("palai.task", "task") }
// TodoTool ... the same durable primitive with kind "todo"
```

Tablo `kind IN ('task','todo')` taşır. `todos` adı `task` kind'ını **dışlar** — tabloyu
yarısından eder.

Ayrıca giderilmek istenen çakışma **zaten giderilmiş**: `a2a_task_refs` ve
`background_tasks` niteleyici prefix taşıyor; çıplak `tasks` modelin kendi kalıcı kayıt
defteridir. Kırık olmayan bir şey refactor edilmez.

Bu bölüm silinmez — bir sonraki okuyucu aynı öneriyi yeniden üretmesin diye durur.

---

## 7. Etkilenmeyenler (kanıtla)

**Engine protokolü etkilenmiyor.**
```bash
rg -oN 'model_step|ModelStep' protocols/engine/engine.schema.json protocols/runner/runner.schema.json
# → (boş)   (2026-08-02)
```
Runner ↔ control-plane protokolü `model_step` taşımıyor, dolayısıyla engine major/minor
sürüm anlaşması (`MASTER-SPEC.md` §25.5) kırılmaz. Bu, T1'in en pahalı olabilecek
bacağının **kapalı** olduğu anlamına gelir.

**`Runner` adı korunur, ama sınırı yazılır.** T1–T3'ün yanında, akış dokümantasyonuna şu
üç şeyin ayrı olduğu açıkça yazılır:

```
Runner   = Mac/VM'de koşan uzun ömürlü daemon (outbound mTLS, enrollment token)
Sandbox  = izolasyon sınırı (unprivileged, CPU/bellek/süre limitli)
Engine   = agent döngüsünün kendisi (model ↔ tool)
```
ve şu cümle: **runner tool çalıştırmaz.** Runner engine'i barındırır ve süpervize eder
(`MASTER-SPEC.md:1743`); tool'lar control-plane tarafında paylaşılan workspace kökü
üzerinde koşar (`packages/tool-broker/`).

---

## 8. Sıra ve bağımsızlık

Üçü bağımsızdır; ayrı task olarak, paralel de gidebilir.

| | Kazanç | Maliyet | Not |
|---|---|---|---|
| T1 | yüksek | orta-yüksek | 4 dil + bundle regen + ledger |
| T2 | orta | **düşük** | 6 yer, 1 migration |
| T3 | orta | orta | asıl iş düşünmek, kod değil |

Önerilen sıra: **T2 → T1 → T3.** T2 en ucuz ve T1'in dokunacağı dosyalarla çakışmaz; T3
en sona kalır çünkü T1'in getirdiği `turn.*` event durumları kanonik sözlüğe girecektir.

---

## 9. Bu spec'in kendi tavanı

- Sayılar 2026-08-02'de ölçüldü. Task'ın **ilk adımı** §2'yi yeniden koşmaktır.
- `max_tool_calls`'ın sıfır okuyucusu olduğu iddiası `docs/operations/tool-errors.md`'den
  **devralındı (2026-08-01)** ve burada yeniden ölçülmesi için komutu yazıldı (§3.3) —
  devralınan tavan yeniden ölçülmeden taşınmaz.
- Bu spec bir **rename** tasarımıdır, bir davranış tasarımı değildir; tek istisnası
  §3.3'tür ve orası açıkça "rename değil, boşluk" diye işaretlidir.
