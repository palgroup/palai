# Slack botu bir SDK relay'i olarak — uygulama planı

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended)
> or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax
> for tracking.

**Goal:** Slack botunu control-plane'in içinden çıkarıp, panelden N tane üretilebilen, yalnız public
`/v1` API'yi (SDK üzerinden) kullanan ayrı bir servise dönüştürmek — ve `palai up`'ı Slack'ten
tamamen habersiz bırakmak.

**Architecture:** Üç parça. (1) `apps/slack-bot` — kendi Postgres şemasını taşıyan, Socket Mode'u
kendi açan, Palai'ı Go SDK üzerinden bir müşteri gibi tüketen bir relay. (2) Control plane'de
**generic** bir bot kaydı (`integration_bots`, `kind` + opaque `config`) — Slack'e ait tek bir sembol
CP'ye girmez. (3) Konsolda bot sihirbazı: bot yarat → agent seç → manifest al → token'ları gir → canlı
test. Mevcut `adapters/integrations/slack` (4026 satır) **hiçbir Palai paketi import etmiyor**, bu
yüzden yeniden yazılmaz — `git mv` ile taşınır.

**Tech Stack:** Go 1.26.0 (`go.mod:3`), Go SDK (`sdks/go`), Postgres, Next.js konsol
(`apps/web-console`), Slack Socket Mode + `agent_view` manifest.

---

## Global Constraints

- **Go sürümü `1.26.0`, toolchain `go1.26.4`** — `go.mod:3-4`. Bot aynı modülde yaşar.
- **CP'ye Slack'e ait tek bir sembol eklenmez.** Yeni CP yüzeyi `kind`-agnostiktir; `kind="slack"`
  yalnız bir dizedir ve CP onu yorumlamaz.
- **Bot yalnız public `/v1` API'yi kullanır.** CP'nin internal paketlerine import yok. Guard: T14.
- **`palai up` Slack'i bilmez.** Bitişte `grep -ci slack cmd/cli/internal/stack/up.go` → `0`.
- **Bot kendi şemasını taşır** (`slack_bot` Postgres şeması). Palai'ın tablolarına yazmaz.
- **Migration numaraları bitişiktir.** Yeni numara için: `ls storage/migrations/*.up.sql | tail -1`
  → bugün `000059_repository_binding_lifecycle` (2026-08-03), yani sıradaki **000060**. Merge
  sırasında çakışırsa integrator yeniden numaralar (files + `VALUES` marker + embed var).
- **Her yeni tenant tablosu `allTables`'a kaydedilir** ve `palai_apply_tenant_policy` çağırır.
- **`git stash` yok** — RED kanıtı commit olarak bırakılır (CLAUDE.md).
- **CUTOVER, paralel çalışma değil.** Taşınan kod taşındığı yerde **silinir**; eski yol bir bayrak
  arkasında yaşatılmaz. Ölü kod, kullanılmayan sembol, "ileride lazım olur" dosyası bırakılmaz.
  Guard'lar T14/T15'te yürüyüşle ölçer.
- **Özellik bayrağı YOK.** Bir davranışı açıp kapatan yeni env/flag eklenmez — bu ağaçta bayrağı
  olmayan bir yol, okuyanın tek yolu görmesi demektir.
- **Her görev CANLI doğrulanır.** Birim testi yeterli değildir: her görevin son adımı ayaktaki
  stack'e karşı koşar (`palai local doctor`, gerçek HTTP, gerçek konsol). Bir kontrolün var olması
  koştuğunun kanıtı değildir.
- **Kanal-agnostik tasarım.** `kind` bugün `slack`; yarın `whatsapp`, `telegram`, `x`. Hiçbir CP
  yüzeyi, hiçbir tablo ve hiçbir konsol bileşeni `slack`'i özel-durumlamaz — kanal, listeden seçilen
  bir değerdir.

---

## §3 Seam envanteri — ÖLÇÜLDÜ 2026-08-03

Her satır kendi komutuyla yazıldı. Task'ın ilk adımı bu komutları yeniden koşmaktır; değişen sayı
anında görünür.

| İddia | Komut | Sonuç (2026-08-03) |
|---|---|---|
| Adapter saf Slack kodudur, Palai bağı yok | `grep -h 'palai/' adapters/integrations/slack/*.go \| grep -v _test \| grep '"'` | **boş** — taşınabilir |
| Adapter boyutu | `ls adapters/integrations/slack/*.go \| grep -v _test \| xargs wc -l \| tail -1` | **4026** satır |
| CP api Slack kodu | `ls apps/control-plane/api/slack*.go \| grep -v _test \| xargs wc -l \| tail -1` | **1055** satır |
| CP extensions Slack kodu | `ls apps/control-plane/internal/extensions/slack*.go \| grep -v _test \| xargs wc -l \| tail -1` | **4945** satır |
| `up.go` içindeki Slack sembolleri | `rg -c '^func .*[Ss]lack\|^var slack\|^const slack' cmd/cli/internal/stack/up.go` | **25** |
| `up.go` toplam | `wc -l cmd/cli/internal/stack/up.go` | **2197** satır |
| Session olay akışı SSE'dir ve resumable'dır | `rg -n 'PollInterval\|journal is the source of truth' apps/control-plane/api/events.go` | `events.go:28,67` — journal tail, 500 ms |
| **Token streaming YAPILMIŞTIR** | `grep -rn 'model_step.delta' --include='*.go' . \| grep -v _test` | **6 üretim yazarı**: `model_delta_sink.go`, `model_dispatch.go:216`, `mcp_progress.go:52,92`, `events.go:26` |
| Go SDK'da SSE var | `ls sdks/go/stream.go` | var |
| Go SDK'da sessions/approvals **yok** | `ls sdks/go/*.go` | `webhook responses palai modelroutes stream types errors` — **sessions ve approvals YOK** |
| Sessions API'de metadata/filtre **yok** | `grep -n 'metadata\|r.URL.Query()' apps/control-plane/api/sessions.go` | **boş** — bot korelasyonu Palai'da tutulamaz, kendi store'u şart |
| Sıradaki migration | `ls storage/migrations/*.up.sql \| tail -1` | `000059` → sıradaki **000060** |

### Ağacın kendi yanlış inançları — bu plan tarafından düzeltilenler

1. **`adapters/integrations/slack/stream.go:22` bayattır.** *"Token seviyesinde streaming, engine
   tarafında `model_step.delta.v1` ister; ayrı bir epic."* — O epic **2026-08-02'de kapandı**
   (`docs/superpowers/specs/2026-08-02-session-isolation-device-config-and-live-view-design.md` §6
   item 2: *"DONE 2026-08-02"*), altı üretim yazarı var. Yorum T7'de düzeltilir. **Kalan gerçek
   tavan** granularity'dir: 500 ms journal tail + coalescing window; gerçek token akışı (option C)
   hâlâ yok — ve Slack için gerekmiyor, çünkü `chat.appendStream` Tier 4'tür (100+/dk ≈ 600 ms).
2. **Aynı spec'in item 0'ı (execution relay / `FLT-P15`) ÖLÜDÜR.** Owner 2026-08-03'te reddetti:
   Mac, Palai app olarak konumlanıyor, CP orada native koşuyor. Bu plan relay varsaymaz.

---

## Dosya yapısı

**Yeni:**
- `apps/slack-bot/main.go` — süreç girişi: config, sağlık, kapanış
- `apps/slack-bot/internal/config/config.go` — bot kimliği + Palai bağlantısı
- `apps/slack-bot/internal/store/` — thread↔session korelasyonu (kendi şeması)
- `apps/slack-bot/internal/relay/` — session olayları → Slack; Slack olayları → session
- `apps/slack-bot/internal/slack/` — **taşınan** `adapters/integrations/slack` (git mv)
- `apps/slack-bot/migrations/` — bot'un kendi şeması
- `sdks/go/sessions.go`, `sdks/go/approvals.go`, `sdks/go/sessionevents.go`
- `storage/migrations/000060_integration_bots.up.sql` / `.down.sql`
- `apps/control-plane/api/bots.go` — generic bot kaydı (kind-agnostik)
- `apps/web-console/app/bots/page.tsx`, `apps/web-console/app/bots/[id]/page.tsx`
- `apps/web-console/lib/botManifest.ts` — manifest üretici

**Değişen:**
- `cmd/cli/internal/stack/up.go` — 25 Slack sembolü sökülür (T13)
- `apps/control-plane/api/router.go` — Slack rotaları kaldırılır (T14)

**Silinen (T14):** `apps/control-plane/api/slack*.go`, `apps/control-plane/internal/extensions/slack*.go`

---

## Görev sırası ve gerekçesi

SDK önce (T1-T3), çünkü bot onsuz yazılamaz. Bot çekirdeği sonra (T4-T8), çünkü panel bir çalışan
bot'u konfigüre eder. Panel sonra (T9-T12). Sökme **en son** (T13-T14), çünkü çalışan Slack işlevi
kaybolmadan önce yerine geçen çalışıyor olmalı.

---

### Task 1: Go SDK — sessions kaynağı

**Files:**
- Create: `sdks/go/sessions.go`
- Test: `sdks/go/sessions_test.go`

**Interfaces:**
- Consumes: `sdks/go/palai.go` — mevcut `Client` ve istek yardımcıları
- Produces: `c.Sessions.Create(ctx, CreateSessionParams) (*Session, error)`,
  `c.Sessions.Steer(ctx, sessionID string, p SteerParams) (*Command, error)`,
  `type Session struct { ID, Object, Status string }`

**NESTED, like every other resource in this SDK** — `responses.go:26` (`Responses struct{client}`),
`modelroutes.go:13`, and the TS SDK's `Sessions` class all nest, and every method there carries a
variadic `opts ...CallOption` tail for per-call timeout/retry. A flat, non-variadic resource would be
the only one in the package and would permanently lack a per-call override. **CORRECTED 2026-08-03**:
this line first read `func (c *Client) CreateSession(...)`, which was the plan's error, not the
implementer's.

**MEASURED, and it overrides the sketch below:** `CreateSessionParams` carries **`Name` only**.
`packages/contracts/session-write.gen.go` types `SessionWrite{AutoApprovePublications,
AutoApproveTools, Name *string}` and `api/commands.go:260` decodes it with `DisallowUnknownFields`,
so `agent_revision_id` is a **400** — verified live on 2026-08-03. Steering is
`{command_id, kind, delivery, message}` (`api/commands.go:349-367`), never `{text}`.

- [ ] **Step 1: Mevcut kaynak desenini oku**

`sdks/go/responses.go` bu modülün desenidir — istek kurma, hata sarma, tip adlandırma. Yeni dosya
onu birebir izler. TS karşılığı `sdks/typescript/src/resources/sessions.ts` (steer/interrupt
imzaları oradan alınır).

- [ ] **Step 2: Başarısız testi yaz**

```go
func TestCreateSessionPostsToV1Sessions(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"sess_1","object":"session","status":"open"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "ak_test")
	s, err := c.CreateSession(context.Background(), CreateSessionParams{AgentRevisionID: "rev_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if gotPath != "/v1/sessions" {
		t.Fatalf("path = %q, want /v1/sessions", gotPath)
	}
	if !strings.Contains(gotBody, `"agent_revision_id":"rev_1"`) {
		t.Fatalf("body %q does not carry agent_revision_id", gotBody)
	}
	if s.ID != "sess_1" {
		t.Fatalf("id = %q, want sess_1", s.ID)
	}
}
```

- [ ] **Step 3: Testin başarısız olduğunu gör**

Run: `go test ./sdks/go/ -run TestCreateSessionPostsToV1Sessions -v`
Expected: FAIL — `undefined: CreateSessionParams`

- [ ] **Step 4: Minimal implementasyon**

```go
package palai

import "context"

type Session struct {
	ID     string `json:"id"`
	Object string `json:"object"`
	Status string `json:"status"`
}

type CreateSessionParams struct {
	AgentRevisionID     string `json:"agent_revision_id,omitempty"`
	PrincipalID         string `json:"principal_id,omitempty"`
	RepositoryBindingID string `json:"repository_binding_id,omitempty"`
}

type SteerParams struct {
	Text string `json:"text"`
}

type Command struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (c *Client) CreateSession(ctx context.Context, p CreateSessionParams) (*Session, error) {
	var out Session
	if err := c.do(ctx, "POST", "/v1/sessions", p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) SteerSession(ctx context.Context, sessionID string, p SteerParams) (*Command, error) {
	var out Command
	if err := c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(sessionID)+"/commands", p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
```

`c.do` adı `palai.go`'daki gerçek yardımcıyla eşleşmelidir — Step 1'de okunan desen ne diyorsa o.

- [ ] **Step 5: Testin geçtiğini gör**

Run: `go test ./sdks/go/ -run TestCreateSessionPostsToV1Sessions -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add sdks/go/sessions.go sdks/go/sessions_test.go
git commit -m "feat(sdk-go): sessions resource — create and steer"
```

---

### Task 2: Go SDK — approvals kaynağı

**Files:**
- Create: `sdks/go/approvals.go`
- Test: `sdks/go/approvals_test.go`

**Interfaces:**
- Consumes: T1'in `c.do` kullanımı
- Produces: `func (c *Client) ListApprovals(ctx, ListApprovalsParams) ([]Approval, error)`,
  `func (c *Client) ApproveApproval(ctx, id string, p DecisionParams) error`,
  `func (c *Client) DenyApproval(ctx, id string, p DecisionParams) error`

- [ ] **Step 1: Rotaların bugün var olduğunu doğrula**

Run: `grep -n 'v1/approvals' apps/control-plane/api/router.go`
Expected: `GET /v1/approvals`, `POST /v1/approvals/{approval_id}/approve`,
`POST /v1/approvals/{approval_id}/deny` (2026-08-03'te `router.go:367-369`).

- [ ] **Step 2: Başarısız testi yaz**

```go
func TestApproveHitsTheApproveRoute(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "ak_test")
	if err := c.ApproveApproval(context.Background(), "apr_1", DecisionParams{Reason: "ok"}); err != nil {
		t.Fatalf("ApproveApproval: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/v1/approvals/apr_1/approve" {
		t.Fatalf("%s %s, want POST /v1/approvals/apr_1/approve", gotMethod, gotPath)
	}
}

func TestApprovalIDIsPathEscaped(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "ak_test")
	_ = c.DenyApproval(context.Background(), "apr/../../secret", DecisionParams{})
	if strings.Contains(gotPath, "..") {
		t.Fatalf("path %q carries an unescaped traversal", gotPath)
	}
}
```

İkinci test bu ağacın kayıtlı kusur ailesindendir (yol/üyelik karşılaştırmaları yenilmiş halde
gönderilmiştir); id'yi kaçırmayan bir istemci aynı sınıfın yeni örneğidir.

- [ ] **Step 3: Testlerin başarısız olduğunu gör**

Run: `go test ./sdks/go/ -run 'TestApprove|TestApprovalID' -v`
Expected: FAIL — `undefined: DecisionParams`

- [ ] **Step 4: Minimal implementasyon**

```go
package palai

import (
	"context"
	"net/url"
)

type Approval struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Operation string `json:"operation"`
	SessionID string `json:"session_id"`
}

type ListApprovalsParams struct {
	SessionID string
	Status    string
}

type DecisionParams struct {
	Reason string `json:"reason,omitempty"`
}

func (c *Client) ApproveApproval(ctx context.Context, id string, p DecisionParams) error {
	return c.do(ctx, "POST", "/v1/approvals/"+url.PathEscape(id)+"/approve", p, nil)
}

func (c *Client) DenyApproval(ctx context.Context, id string, p DecisionParams) error {
	return c.do(ctx, "POST", "/v1/approvals/"+url.PathEscape(id)+"/deny", p, nil)
}
```

`ListApprovals` sorgu parametrelerini `url.Values` ile kurar; alan adları
`apps/control-plane/api/approvals.go`'nun `list` handler'ının okuduğu adlarla eşleşmelidir —
implementer o handler'ı okur, tahmin etmez.

- [ ] **Step 5: Testlerin geçtiğini gör**

Run: `go test ./sdks/go/ -run 'TestApprove|TestApprovalID' -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add sdks/go/approvals.go sdks/go/approvals_test.go
git commit -m "feat(sdk-go): approvals resource — list, approve, deny"
```

---

### Task 3: Go SDK — session olay akışı (resumable SSE)

**Files:**
- Create: `sdks/go/sessionevents.go`
- Test: `sdks/go/sessionevents_test.go`

**Interfaces:**
- Consumes: `sdks/go/stream.go` — mevcut SSE çerçeveleyici
- Produces: `func (c *Client) SessionEvents(ctx, sessionID string, from Cursor) (*EventStream, error)`,
  `func (s *EventStream) Next() (Event, error)`, `type Event struct { Seq int64; Type string; Data json.RawMessage }`

- [ ] **Step 1: Sunucunun ne yaydığını oku**

Run: `rg -n 'PollInterval|terminal|resume' apps/control-plane/api/events.go | head -20`

Uç nokta bir **journal tail**'idir: resume cursor'ından replay eder, terminal olayda temiz kapanır.
İstemci bu yüzden cursor tutar ve reconnect'te kaldığı yerden devam eder — bot'un durum tutmadan
hayatta kalmasının tek sebebi budur.

- [ ] **Step 2: Başarısız testi yaz**

```go
func TestSessionEventsResumesFromTheLastSeq(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: model_step.delta.v1\ndata: {\"seq\":7,\"text\":\"hi\"}\n\n")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "ak_test")
	st, err := c.SessionEvents(context.Background(), "sess_1", Cursor{Seq: 6})
	if err != nil {
		t.Fatalf("SessionEvents: %v", err)
	}
	defer st.Close()
	if !strings.Contains(gotQuery, "6") {
		t.Fatalf("query %q does not carry the resume cursor", gotQuery)
	}
	ev, err := st.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Type != "model_step.delta.v1" {
		t.Fatalf("type = %q", ev.Type)
	}
}
```

Cursor parametresinin **adı** `events.go`'nun okuduğu addır — implementer onu oradan alır.

- [ ] **Step 3: Testin başarısız olduğunu gör**

Run: `go test ./sdks/go/ -run TestSessionEventsResumes -v`
Expected: FAIL — `undefined: Cursor`

- [ ] **Step 4: Implementasyon**

`sdks/go/stream.go`'daki mevcut çerçeveleyiciyi sarar; yeni SSE ayrıştırıcı **yazılmaz**.
`Next()` terminal olayda `io.EOF` döner (`isTerminalEvent` karşılığı — TS'te
`sdks/typescript/src/stream.ts` bu ismi taşır).

- [ ] **Step 5: Testin geçtiğini gör**

Run: `go test ./sdks/go/ -run TestSessionEventsResumes -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add sdks/go/sessionevents.go sdks/go/sessionevents_test.go
git commit -m "feat(sdk-go): resumable session event stream"
```

---

### Task 4: Generic bot kaydı — migration + API (CP'de, Slack'e ait sıfır sembol)

**Files:**
- Create: `storage/migrations/000060_integration_bots.up.sql`, `.down.sql`
- Create: `apps/control-plane/api/bots.go`
- Modify: `apps/control-plane/api/router.go`
- Test: `apps/control-plane/api/bots_test.go`

**Interfaces:**
- Produces: `POST/GET /v1/bots`, `GET/PATCH/DELETE /v1/bots/{bot_id}`; satır şekli
  `{id, name, kind, agent_revision_id, repository_binding_id, config, disabled}`

- [ ] **Step 1: Migration numarasını yeniden ölç**

Run: `ls storage/migrations/*.up.sql | tail -1`
Beklenen: `000059_repository_binding_lifecycle.up.sql` → yeni dosya **000060**. Farklıysa
en yüksek + 1 kullanılır.

- [ ] **Step 2: Migration'ı yaz**

```sql
-- integration_bots — a KIND-AGNOSTIC bot registry. The control plane stores a bot's identity, the
-- agent it speaks as, and an OPAQUE config document; it never parses `config` and never learns what
-- a `kind` means. A Slack bot is one row with kind='slack'; nothing here mentions Slack.
CREATE TABLE IF NOT EXISTS integration_bots (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    project_id TEXT NOT NULL REFERENCES projects (id),
    name TEXT NOT NULL,
    kind TEXT NOT NULL,
    agent_revision_id TEXT NOT NULL DEFAULT '',
    repository_binding_id TEXT NOT NULL DEFAULT '',
    principal_id TEXT NOT NULL DEFAULT '',
    config JSONB NOT NULL DEFAULT '{}'::jsonb,
    disabled BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (organization_id, project_id, name)
);

CALL palai_apply_tenant_policy('integration_bots', 'organization_id', true);
GRANT SELECT, INSERT, UPDATE, DELETE ON integration_bots TO palai_app;

INSERT INTO schema_migrations (version) VALUES (60) ON CONFLICT DO NOTHING;
```

- [ ] **Step 3: Tabloyu `allTables`'a kaydet**

Run: `rg -n 'allTables' --glob '!*_test.go' | head -3`
Bulunan listeye `integration_bots` eklenir. Kayıtsız bir tenant tablosu tenancy corpus'unu
kırmızıya çevirir (bu ağacın kayıtlı kuralı).

- [ ] **Step 4: Başarısız testi yaz**

```go
func TestBotsAPIStoresConfigOpaquely(t *testing.T) {
	// Bir bot, CP'nin hiç tanımadığı alanlar taşıyan bir config ile yaratılır ve
	// AYNEN geri okunur. CP'nin config'i yorumlamadığının kanıtı budur.
	body := `{"name":"ios-bot","kind":"slack","config":{"team_id":"T1","channels":["C1"],"anything":42}}`
	rec := doRequest(t, "POST", "/v1/bots", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	var got struct {
		ID     string         `json:"id"`
		Config map[string]any `json:"config"`
	}
	mustDecode(t, rec, &got)
	if got.Config["anything"] != float64(42) {
		t.Fatalf("config was reshaped: %v", got.Config)
	}
}

func TestBotsAPICarriesNoSlackSymbol(t *testing.T) {
	// Bu dosyada 'slack' geçmemelidir — kind bir dizedir, bir özellik değil.
	src, err := os.ReadFile("bots.go")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(src), []byte("slack")) {
		t.Fatal("bots.go mentions slack; the registry must stay kind-agnostic")
	}
}
```

- [ ] **Step 5: Testlerin başarısız olduğunu gör**

Run: `go test ./apps/control-plane/api/ -run TestBotsAPI -v`
Expected: FAIL — rota yok (404)

- [ ] **Step 6: Handler'ı ve rotaları yaz**

`apps/control-plane/api/repository_bindings.go` bu ağacın CRUD desenidir (tenant scope, sayfalama,
`DisallowUnknownFields`). `bots.go` onu izler. `config` **`json.RawMessage`** olarak taşınır —
çözümlenmez.

**Dikkat (kayıtlı kusur):** liste sorgusu `ORDER BY` taşımalıdır. Sırasız bir `LIMIT` bu ağaçta
iki güvenlik kararına ve bir yanlış-kırmızı gate'e karar verdi.

- [ ] **Step 7: Testlerin geçtiğini gör**

Run: `go test ./apps/control-plane/api/ -run TestBotsAPI -v`
Expected: PASS

- [ ] **Step 8: Tenancy corpus'unu koştur**

Run: `TEST=tenancy scripts/test/security`
Expected: PASS. (`go test ./tests/security/tenancy/...` **yanlıştır** — `matched no packages`
yazıp exit 0 döner.)

- [ ] **Step 9: Commit**

```bash
git add storage/migrations/000060_integration_bots.*.sql apps/control-plane/api/bots.go \
        apps/control-plane/api/bots_test.go apps/control-plane/api/router.go
git commit -m "feat(control-plane): kind-agnostic integration_bots registry"
```

---

### Task 5: Bot iskeleti — `apps/slack-bot`

**Files:**
- Create: `apps/slack-bot/main.go`, `apps/slack-bot/internal/config/config.go`
- Test: `apps/slack-bot/internal/config/config_test.go`

**Interfaces:**
- Consumes: T1-T3 SDK
- Produces: `config.Load() (Config, error)`; `Config{BotID, PalaiBaseURL, PalaiAPIKey, DatabaseURL}`

- [ ] **Step 1: Başarısız testi yaz**

```go
func TestConfigRefusesAMissingBotID(t *testing.T) {
	t.Setenv("PALAI_BOT_ID", "")
	t.Setenv("PALAI_API_URL", "https://cp.example")
	t.Setenv("PALAI_API_KEY", "ak_1")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted an empty PALAI_BOT_ID; a bot with no registry row has nothing to be")
	}
}

func TestConfigNamesTheVariableThatIsWrong(t *testing.T) {
	t.Setenv("PALAI_BOT_ID", "bot_1")
	t.Setenv("PALAI_API_URL", "")
	t.Setenv("PALAI_API_KEY", "ak_1")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "PALAI_API_URL") {
		t.Fatalf("error %v does not name the variable that is wrong", err)
	}
}
```

- [ ] **Step 2: Testlerin başarısız olduğunu gör**

Run: `go test ./apps/slack-bot/internal/config/ -v`
Expected: FAIL — paket yok

- [ ] **Step 3: Config'i yaz**

Bot **dört** değişken okur ve başkasını okumaz: `PALAI_BOT_ID`, `PALAI_API_URL`, `PALAI_API_KEY`,
`PALAI_BOT_DATABASE_URL`. Slack token'ları **değişkenden gelmez** — bot kaydının `config`'inden ve
`/v1/secret-refs`'ten gelir (T7). Bu, `.env.local`'in emekli olmasının nedenidir.

- [ ] **Step 4: Testlerin geçtiğini gör**

Run: `go test ./apps/slack-bot/internal/config/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add apps/slack-bot/
git commit -m "feat(slack-bot): process skeleton and four-variable config"
```

---

### Task 6: Slack protokol kodunun taşınması

**Files:**
- Move: `adapters/integrations/slack/*.go` → `apps/slack-bot/internal/slack/`

- [ ] **Step 1: Bağımsızlığı yeniden ölç**

Run: `grep -h 'palai/' adapters/integrations/slack/*.go | grep -v _test | grep '"'`
Expected: **boş**. Boş değilse bu görev durur ve bağ önce kesilir — plan bu ölçüme dayanıyor.

- [ ] **Step 2: Taşı**

```bash
git mv adapters/integrations/slack apps/slack-bot/internal/slack
```

- [ ] **Step 3: Paket yolunu düzelt**

Run: `rg -l 'adapters/integrations/slack' --glob '*.go'`
Her import `apps/slack-bot/internal/slack`'e çevrilir. **CP tarafındaki dosyalar T14'e kadar bu
importu taşımaya devam eder** — o yüzden bu adımda CP hâlâ derlenir.

- [ ] **Step 4: Derle ve testleri koştur**

Run: `go build ./... && go test ./apps/slack-bot/...`
Expected: PASS

- [ ] **Step 5: Etiketli derlemeyi de doğrula**

Run: `go vet -tags="component live" ./...`
Expected: temiz. (Düz `vet` etiketli testlerdeki bayat çağıranları kaçırır — bu ağacın kayıtlı
kuralı.)

- [ ] **Step 6: Commit**

```bash
git commit -am "refactor(slack): move the pure Slack protocol code into the bot"
```

---

### Task 7: Relay çekirdeği — session olayları → Slack

**Files:**
- Create: `apps/slack-bot/internal/relay/relay.go`
- Modify: `apps/slack-bot/internal/slack/stream.go:17-22` (bayat yorum)
- Test: `apps/slack-bot/internal/relay/relay_test.go`

**Interfaces:**
- Consumes: T3 `SessionEvents`, T6 `slack.StartStream/AppendStream/StopStream`
- Produces: `func Run(ctx, deps Deps, sessionID, channel, threadTS string) error`

- [ ] **Step 1: Bayat yorumu düzelt**

`stream.go:22` *"Token seviyesinde streaming ayrı bir epic"* der; o epic **2026-08-02'de kapandı**.
Yeni metin hem kapanışı hem **kalan** tavanı söyler:

```go
// STREAMING GRANULARITY. model_step.delta.v1 has production writers since 2026-08-02
// (execution/model_delta_sink.go). What arrives here is a COALESCED window, not a token: the SSE
// endpoint is a journal tail at 500 ms (api/events.go:28) and the sink matches that window on
// purpose. True token-level streaming (a live channel that bypasses the journal) is still absent
// and is NOT needed here — chat.appendStream is Tier 4, ~600 ms between calls.
```

**Bir yorumu düzeltirken hangi dalların doğru olduğunu say** — bu cümle hem yazarın varlığını hem
granularity tavanını taşır; "artık doğru" diye yazılmaz.

- [ ] **Step 2: Başarısız testi yaz**

```go
func TestDeltasBecomeOneAppendPerWindow(t *testing.T) {
	events := []palai.Event{
		{Type: "model_step.delta.v1", Data: []byte(`{"text":"Hel"}`)},
		{Type: "model_step.delta.v1", Data: []byte(`{"text":"lo"}`)},
		{Type: "run.completed.v1", Data: []byte(`{}`)},
	}
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.started != 1 || fake.stopped != 1 {
		t.Fatalf("start=%d stop=%d, want 1/1", fake.started, fake.stopped)
	}
	if got := strings.Join(fake.appended, ""); got != "Hello" {
		t.Fatalf("appended %q, want Hello", got)
	}
}

func TestATerminalEventAlwaysStopsTheStream(t *testing.T) {
	// Slack'te kapatılmayan bir akış kalıcı olarak "streaming" görünür (SLK-P2).
	for _, term := range []string{"run.failed.v1", "run.canceled.v1", "run.timed_out.v1"} {
		fake := &fakeSlack{}
		_ = Run(context.Background(), Deps{
			Events: staticStream([]palai.Event{{Type: term, Data: []byte(`{}`)}}),
			Slack:  fake,
		}, "sess_1", "C1", "1.1")
		if fake.stopped != 1 {
			t.Fatalf("%s left the stream open", term)
		}
	}
}
```

İkinci test bir **reddi** değil bir **garantiyi** iddia eder ve her terminal dalını tek tek gezer —
bir dalın unutulması bu ağaçta tekrar eden kusur ailesidir.

- [ ] **Step 3: Testlerin başarısız olduğunu gör**

Run: `go test ./apps/slack-bot/internal/relay/ -v`
Expected: FAIL — `undefined: Run`

- [ ] **Step 4: Relay'i yaz**

Döngü: `SessionEvents` → tip anahtarı → `model_step.delta.v1` biriktir ve `AppendStream`;
`tool_call.*` ilerleme satırı; terminal olay → `StopStream`. `defer` ile terminal garanti altına
alınır — panik veya erken dönüş de akışı kapatmalıdır.

- [ ] **Step 5: Testlerin geçtiğini gör**

Run: `go test ./apps/slack-bot/internal/relay/ -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add apps/slack-bot/internal/relay/ apps/slack-bot/internal/slack/stream.go
git commit -m "feat(slack-bot): relay session events into a Slack stream"
```

---

### Task 8: Thread↔session korelasyonu (bot'un kendi şeması)

**Files:**
- Create: `apps/slack-bot/migrations/0001_thread_sessions.sql`
- Create: `apps/slack-bot/internal/store/threads.go`
- Test: `apps/slack-bot/internal/store/threads_test.go`

**Interfaces:**
- Produces: `func (s *Store) SessionForThread(ctx, botID, teamID, channelID, threadTS string) (string, bool, error)`,
  `func (s *Store) BindThread(ctx, ...) (string, error)`

- [ ] **Step 1: Neden bot'ta olduğunu doğrula**

Run: `grep -n 'metadata\|r.URL.Query()' apps/control-plane/api/sessions.go`
Expected: **boş** — Sessions API'de dış korelasyon anahtarı ile arama yoktur, bu yüzden eşleme
Palai'da tutulamaz. Bu çıktı boş değilse bu görev yeniden değerlendirilir.

- [ ] **Step 2: Şemayı yaz**

`storage/migrations/000035_slack.up.sql:40-54`'ün şekli birebir devralınır — **özellikle
`UNIQUE (bot_id, team_id, channel_id, thread_ts)`**: aynı thread'e gelen ikinci olay AYNI session'ı
çözer ve eşzamanlı yarış veritabanında (23505) çöker, iki session doğmaz.

- [ ] **Step 3: Başarısız testi yaz**

```go
func TestASecondEventInTheSameThreadReusesTheSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	first, err := s.BindThread(ctx, "bot_1", "T1", "C1", "111.1", "sess_1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.BindThread(ctx, "bot_1", "T1", "C1", "111.1", "sess_2")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("thread bound twice: %q then %q", first, second)
	}
}

func TestTwoBotsInOneThreadDoNotShareASession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a, _ := s.BindThread(ctx, "bot_ios", "T1", "C1", "111.1", "sess_a")
	b, _ := s.BindThread(ctx, "bot_android", "T1", "C1", "111.1", "sess_b")
	if a == b {
		t.Fatal("two bots collapsed onto one session; bot_id is part of the key")
	}
}
```

İkinci test bu planın çok-bot gereksinimini fence'ler: eski tablo `bot_id` taşımıyordu.

- [ ] **Step 4: Testlerin başarısız olduğunu gör**

Run: `go test ./apps/slack-bot/internal/store/ -v`
Expected: FAIL

- [ ] **Step 5: Store'u yaz**

`INSERT ... ON CONFLICT DO NOTHING RETURNING`, ardından boş dönerse `SELECT`. Her sorgu `ORDER BY`
taşır.

- [ ] **Step 6: Testlerin geçtiğini gör**

Run: `go test ./apps/slack-bot/internal/store/ -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add apps/slack-bot/migrations/ apps/slack-bot/internal/store/
git commit -m "feat(slack-bot): thread-session correlation keyed by bot"
```

---

### Task 9: Inbound — Socket Mode → session

**Files:**
- Create: `apps/slack-bot/internal/relay/inbound.go`
- Test: `apps/slack-bot/internal/relay/inbound_test.go`

**Interfaces:**
- Consumes: T6 `slack.Socket`, T8 store, T1 `CreateSession`/`SteerSession`
- Produces: `func HandleEvent(ctx, deps Deps, ev slack.InboundEvent) error`

- [ ] **Step 1: Başarısız testi yaz**

```go
func TestATopLevelMentionIsDeliveredOnce(t *testing.T) {
	// Slack aynı mesaj için hem app_mention hem message.channels gönderir; her birinin kendi
	// event_id'si vardır, bu yüzden event_id ile dedupe onları BİRLEŞTİRMEZ (SLK-P5).
	deps := newTestDeps(t)
	mention := slack.InboundEvent{Type: "app_mention", TeamID: "T1", ChannelID: "C1",
		ThreadTS: "1.1", EventID: "Ev1", Text: "<@U1> hi"}
	twin := slack.InboundEvent{Type: "message.channels", TeamID: "T1", ChannelID: "C1",
		ThreadTS: "1.1", EventID: "Ev2", Text: "<@U1> hi"}
	if err := HandleEvent(context.Background(), deps, mention); err != nil {
		t.Fatal(err)
	}
	if err := HandleEvent(context.Background(), deps, twin); err != nil {
		t.Fatal(err)
	}
	if deps.Palai.(*fakePalai).steers != 1 {
		t.Fatalf("steers = %d, want 1: the twin was delivered as a second turn",
			deps.Palai.(*fakePalai).steers)
	}
}

func TestTheBotIgnoresItsOwnMessages(t *testing.T) {
	deps := newTestDeps(t)
	self := slack.InboundEvent{Type: "message.channels", TeamID: "T1", ChannelID: "C1",
		ThreadTS: "1.1", EventID: "Ev3", UserID: deps.BotUserID, Text: "an answer"}
	if err := HandleEvent(context.Background(), deps, self); err != nil {
		t.Fatal(err)
	}
	if deps.Palai.(*fakePalai).steers != 0 {
		t.Fatal("the bot answered itself; the self-loop guard is the bot_user_id")
	}
}
```

- [ ] **Step 2: Testlerin başarısız olduğunu gör**

Run: `go test ./apps/slack-bot/internal/relay/ -run 'TestATopLevel|TestTheBotIgnores' -v`
Expected: FAIL

- [ ] **Step 3: Implementasyon**

Dedupe anahtarı `(team, channel, thread_ts, text-hash)` olur — `event_id` **değil**, çünkü ikiz
olayların event_id'leri farklıdır (SLK-P5). Self-loop guard `bot_user_id`'dir.

- [ ] **Step 4: Testlerin geçtiğini gör**

Run: `go test ./apps/slack-bot/internal/relay/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add apps/slack-bot/internal/relay/inbound.go apps/slack-bot/internal/relay/inbound_test.go
git commit -m "feat(slack-bot): inbound events open or steer a session"
```

---

### Task 10: Onay köprüsü

**Files:**
- Create: `apps/slack-bot/internal/relay/approvals.go`
- Test: `apps/slack-bot/internal/relay/approvals_test.go`

**Interfaces:**
- Consumes: T2 approvals SDK, T6 `slack.Blocks`, `slack.Interactions`
- Produces: `func OnApprovalRequested(ctx, deps, ev palai.Event) error`,
  `func OnButton(ctx, deps, payload slack.Interaction) error`

- [ ] **Step 1: Başarısız testi yaz**

```go
func TestAnUnlistedClickerCannotDecide(t *testing.T) {
	deps := newTestDeps(t)
	deps.AllowedApprovers = []string{"U_allowed"}
	err := OnButton(context.Background(), deps, slack.Interaction{
		UserID: "U_stranger", Action: "approve", ApprovalID: "apr_1"})
	if err == nil {
		t.Fatal("an unlisted user decided an approval")
	}
	if deps.Palai.(*fakePalai).approvals != 0 {
		t.Fatal("the decision reached the API before the allow-list was checked")
	}
}
```

Sıra önemlidir: allow-list kontrolü API çağrısından **önce** olmalıdır, yoksa test iddia ettiği
şeyden farklı bir sebeple geçebilir.

- [ ] **Step 2: Testin başarısız olduğunu gör**

Run: `go test ./apps/slack-bot/internal/relay/ -run TestAnUnlistedClicker -v`
Expected: FAIL

- [ ] **Step 3: Implementasyon**

`approval.requested.v1` → `slack.BuildApprovalBlocks` (T6'da taşınan `approval_display.go`) →
`chat.postMessage`. Buton → allow-list → `ApproveApproval`/`DenyApproval` → `chat.update`.

- [ ] **Step 4: Testin geçtiğini gör**

Run: `go test ./apps/slack-bot/internal/relay/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add apps/slack-bot/internal/relay/approvals.go apps/slack-bot/internal/relay/approvals_test.go
git commit -m "feat(slack-bot): approval bridge with an allow-list gate"
```

---

### Task 11: Konsol — bot listesi ve yaratma

**Files:**
- Create: `apps/web-console/app/bots/page.tsx`
- Test: `apps/web-console/tests/bots.spec.ts`

- [ ] **Step 1: Ekran desenini oku**

`apps/web-console/app/repositories/page.tsx` (538 satır) bu konsolun desenidir: `/api/palai/v1`
relay'i, `Panel`/`Status` bileşenleri, `FormDialog`.

- [ ] **Step 2: Başarısız testi yaz**

```ts
test("bot yaratma FORMU sürülür, endpoint değil", async ({ page }) => {
  await page.goto("/bots");
  await page.getByRole("button", { name: "New bot" }).click();
  // Kanal seçimi ilk adımdır: bugün Slack, yarın WhatsApp/Telegram/X.
  await page.getByRole("option", { name: "Slack" }).click();
  await page.getByLabel("Name").fill("ios-bot");
  await page.getByLabel("Agent").selectOption({ index: 1 });
  await page.getByRole("button", { name: "Create" }).click();
  await expect(page.getByText("ios-bot")).toBeVisible();
});

test("kanal listesi yakında gelecekleri KAPALI gösterir, gizlemez", async ({ page }) => {
  await page.goto("/bots");
  await page.getByRole("button", { name: "New bot" }).click();
  const slack = page.getByRole("option", { name: "Slack" });
  await expect(slack).toBeEnabled();
  // Yakında gelecek kanallar görünür ama seçilemez — bir yol haritası, bir yalan değil.
  for (const soon of ["WhatsApp", "Telegram"]) {
    await expect(page.getByRole("option", { name: soon })).toBeDisabled();
  }
});
```

Bu ağacın kayıtlı kuralı: bir yüzey için yazılan test **o yüzeyi sürmelidir** — `fetch` ile
endpoint'e gitmek formun `method`'unun eksik olduğunu göremez.

**Kanal listesi tek bir yerden gelir** (`apps/web-console/lib/channels.ts`): `{id, label, enabled}`.
Slack `enabled: true`, diğerleri `false`. Yeni bir kanal eklemek bu listeye bir satırdır — ekranda
`if (kind === "slack")` **hiçbir yerde geçmez**.

- [ ] **Step 3: Testin başarısız olduğunu gör**

Run: `cd apps/web-console && pnpm test -- bots.spec.ts`
Expected: FAIL — `/bots` yok

- [ ] **Step 4: Ekranı yaz**

Liste + `FormDialog`. Agent seçici `GET /v1/agents`'tan doldurulur; repository seçici
`GET /v1/repository-bindings`'ten. **Form bir `method` taşır** — JS bağlanmadığı anda alanlar
URL'e düşmemelidir.

- [ ] **Step 5: Testin geçtiğini gör**

Run: `cd apps/web-console && pnpm test -- bots.spec.ts`
Expected: PASS

- [ ] **Step 6: Erişilebilirlik taramasını dialog AÇIKKEN koştur**

Dialog kapalıyken tarama kontrolleri kaçırır (kayıtlı kusur: 265→409). `expectAxeClean` dialog
açıkken çağrılır.

- [ ] **Step 7: Commit**

```bash
git add apps/web-console/app/bots/ apps/web-console/tests/bots.spec.ts
git commit -m "feat(console): bot registry screen"
```

---

### Task 12: Konsol — manifest sihirbazı

**Files:**
- Create: `apps/web-console/lib/botManifest.ts`
- Create: `apps/web-console/app/bots/[id]/page.tsx`
- Test: `apps/web-console/lib/botManifest.test.ts`

**Interfaces:**
- Produces: `buildManifest(bot: Bot): string` — YAML

- [ ] **Step 1: Kaynağı belirle**

`deploy/slack/app-manifest.yaml` (179 satır) **şablondur**. Bot başına değişen: `display_information.name`,
`features.bot_user.display_name`, `features.agent_view.agent_description`, `suggested_prompts`.
Sabit kalan: 9 scope, 5 event, `socket_mode_enabled: true`.

- [ ] **Step 2: Başarısız testi yaz**

```ts
test("üretilen manifest shipped şablonun scope kümesini korur", () => {
  const yaml = buildManifest({ name: "iOS Bot", description: "iOS işlerini yürütür", prompts: [] });
  for (const scope of ["app_mentions:read", "assistant:write", "channels:history",
                       "chat:write", "files:read", "files:write", "im:history",
                       "search:read.public", "users:read"]) {
    expect(yaml).toContain(scope);
  }
  expect(yaml).toContain("socket_mode_enabled: true");
  expect(yaml).toContain("iOS Bot");
});

test("agent_description 300 karakter sınırında kesilir", () => {
  const yaml = buildManifest({ name: "b", description: "x".repeat(400), prompts: [] });
  const desc = yaml.match(/agent_description: >-\n\s+(.*)/)?.[1] ?? "";
  expect(desc.length).toBeLessThanOrEqual(300);
});
```

300 sınırı manifest referansının kendi kuralıdır ve şablonun yorumunda yazılıdır.

- [ ] **Step 3: Testlerin başarısız olduğunu gör**

Run: `cd apps/web-console && pnpm test -- botManifest.test.ts`
Expected: FAIL

- [ ] **Step 4: Üreticiyi yaz**

Scope/event listeleri şablondan **kopyalanır ve sabitlenir** — kullanıcı düzenleyemez; bir scope
eklemek kod değişikliğidir.

- [ ] **Step 5: Detay sayfasını yaz**

Üç adım: (1) manifest'i göster + kopyala, (2) token alanları → `POST /v1/secret-refs` → handle'lar
bot'un `config`'ine `PATCH /v1/bots/{id}` ile yazılır, (3) "Test et".

**Token'lar asla `config`'e değer olarak yazılmaz** — yalnız handle. Bu ağacın secret modeli budur.

- [ ] **Step 6: Testlerin geçtiğini gör**

Run: `cd apps/web-console && pnpm test -- botManifest.test.ts`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add apps/web-console/lib/botManifest.ts apps/web-console/app/bots/\[id\]/ \
        apps/web-console/lib/botManifest.test.ts
git commit -m "feat(console): manifest wizard and sealed token entry"
```

---

### Task 13: Canlı test düğmesi

**Files:**
- Create: `apps/slack-bot/internal/relay/selftest.go`
- Modify: `apps/web-console/app/bots/[id]/page.tsx`
- Test: `apps/slack-bot/internal/relay/selftest_test.go`

**Interfaces:**
- Produces: `func SelfTest(ctx, deps) (Report, error)`; `Report{AuthOK, SocketOK, PostOK, Detail}`

- [ ] **Step 1: Başarısız testi yaz**

```go
func TestSelfTestReportsWhichLegFailed(t *testing.T) {
	deps := newTestDeps(t)
	deps.Slack.(*fakeSlack).authErr = errors.New("invalid_auth")
	r, _ := SelfTest(context.Background(), deps)
	if r.AuthOK {
		t.Fatal("AuthOK true with an invalid token")
	}
	if !strings.Contains(r.Detail, "invalid_auth") {
		t.Fatalf("detail %q does not carry Slack's own message", r.Detail)
	}
}
```

Slack'in kendi hata dizesi taşınır — yeniden adlandırılmış bir hata operatöre yanlış dosyayı
işaret ettirir.

- [ ] **Step 2: Testin başarısız olduğunu gör**

Run: `go test ./apps/slack-bot/internal/relay/ -run TestSelfTest -v`
Expected: FAIL

- [ ] **Step 3: Implementasyon**

Üç bacak, sırayla: `auth.test` → Socket Mode bağlantısı → yapılandırılmış kanala bir test mesajı.
Her bacak kendi sonucunu taşır; ilk hatada durur ve **hangi bacağın** düştüğünü söyler.

- [ ] **Step 4: Testin geçtiğini gör**

Run: `go test ./apps/slack-bot/internal/relay/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add apps/slack-bot/internal/relay/selftest.go apps/slack-bot/internal/relay/selftest_test.go \
        apps/web-console/app/bots/\[id\]/page.tsx
git commit -m "feat(slack-bot): three-leg self test surfaced in the console"
```

---

### Task 14: `palai up`'tan Slack'in sökülmesi

**Files:**
- Modify: `cmd/cli/internal/stack/up.go`
- Delete: `cmd/cli/internal/stack/up_slack_test.go`
- Modify: `cmd/cli/internal/stack/up_repository_test.go`

- [ ] **Step 1: Sökülecekleri yeniden say**

Run: `rg -n '^func .*[Ss]lack|^var slack|^const slack' cmd/cli/internal/stack/up.go | wc -l`
Expected: **25** (2026-08-03). Farklıysa liste yeniden çıkarılır.

- [ ] **Step 2: Guard testini yaz (RED)**

```go
func TestBringUpKnowsNothingAboutSlack(t *testing.T) {
	src, err := os.ReadFile("up.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(bytes.ToLower(src), []byte("slack")); n != 0 {
		t.Fatalf("up.go mentions slack %d times; bring-up prepares a stack and nothing else", n)
	}
}
```

- [ ] **Step 3: RED'i gör ve commit'le**

Run: `go test ./cmd/cli/internal/stack/ -run TestBringUpKnowsNothing -v`
Expected: FAIL — sayı sıfır değil

```bash
git add cmd/cli/internal/stack/up_no_slack_test.go
git commit -m "test(cli): RED — bring-up still carries Slack wiring"
```

RED bir commit'tir, stash değil (CLAUDE.md: bu depoda `stash pop` yok).

- [ ] **Step 4: Sök**

25 sembol ve çağıranları kaldırılır. `wireSlack` çağrısı `Up`/`UpNative` akışından çıkar.
`applySlackEnv`'in **`PALAI_SECRET_MASTER_KEY_FILE` ile ilgisi yoktur** (yorumu bunu açıkça söyler)
— master key mantığı `ensureSecretSlots`'ta kalır ve **dokunulmaz**.

- [ ] **Step 5: YEŞİL'i gör**

Run: `go test ./cmd/cli/internal/stack/ -v`
Expected: PASS — guard dahil

- [ ] **Step 6: Bring-up'ı gerçekten koştur**

Run: `palai local down && palai up --native`
Expected: stack ayağa kalkar, Slack'e dair tek satır yok. **Bu adım atlanamaz** — betiği okumak
bir bring-up'ın koştuğunun kanıtı değildir.

- [ ] **Step 7: Commit**

```bash
git add -A cmd/cli/internal/stack/
git commit -m "refactor(cli): bring-up prepares a stack and nothing else"
```

---

### Task 15: CP'den Slack'in emekliye ayrılması

**Files:**
- Delete: `apps/control-plane/api/slack*.go` (1055 satır), `apps/control-plane/internal/extensions/slack*.go` (4945 satır)
- Modify: `apps/control-plane/api/router.go`
- Create: `storage/migrations/000061_retire_slack_tables.up.sql`

- [ ] **Step 1: Yeni botun çalıştığını kanıtla — SÖKMEDEN ÖNCE**

Gerçek bir workspace'te T13'ün üç bacağı yeşil olmalı ve bir thread'de bir run tamamlanmalıdır.
**Bu görev o kanıt olmadan başlamaz.** Bir işlevi, yerine geçen çalıştığı görülmeden silmek bu
ağacın kayıtlı en pahalı hatasıdır.

- [ ] **Step 2: Guard testini yaz (RED)**

```go
func TestControlPlaneCarriesNoSlackCode(t *testing.T) {
	var found []string
	for _, dir := range []string{"apps/control-plane/api", "apps/control-plane/internal/extensions"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.Name()), "slack") {
				found = append(found, filepath.Join(dir, e.Name()))
			}
		}
	}
	if len(found) != 0 {
		t.Fatalf("the control plane still carries Slack files: %v", found)
	}
}
```

Bu tarama **yürüyüştür** — dışa dönük boşluk için doğru otoritedir (var olan ama sahipsiz dosyayı
bulur). İçe dönük eksikler için kanonik liste gerekir; burada gereken bu değildir.

- [ ] **Step 3: RED'i gör ve commit'le**

Run: `go test ./tests/docs/ -run TestControlPlaneCarriesNoSlack -v`
Expected: FAIL — 31 dosya

```bash
git commit -am "test(docs): RED — the control plane still carries Slack"
```

- [ ] **Step 4: Sil ve rotaları kaldır**

Run: `git rm apps/control-plane/api/slack*.go apps/control-plane/internal/extensions/slack*.go`
`router.go`'daki Slack rotaları (`:437-443` ve interactions) kaldırılır.

- [ ] **Step 5: Tabloları emekliye ayır**

`slack_connections`, `slack_thread_sessions`, `slack_reply_deliveries`, `slack_message_turns`
DROP edilir. `usage_events`'i migration `000034` ile düşürmek bu ağaçta bir tabloyu **doğru**
emekliye ayırmanın örneğidir — aynı biçim izlenir.

- [ ] **Step 6: Tam doğrulama**

Run: `go build ./... && go vet -tags="component live" ./... && make verify`
Expected: PASS

Run: `TEST=tenancy scripts/test/security`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git commit -am "refactor(control-plane): retire the in-process Slack integration"
```

---

### Task 16: Bot başına gözlemlenebilirlik — session'lar, metrikler, loglar

**Files:**
- Modify: `apps/control-plane/api/sessions.go` (filtre), `storage/queries/sessions.sql`
- Modify: `apps/web-console/app/bots/[id]/page.tsx`
- Test: `apps/control-plane/api/sessions_filter_test.go`

**Interfaces:**
- Produces: `GET /v1/sessions?bot_id=<id>` — bot'un açtığı session'lar

Bu görev owner'ın *"botların metriklerini, loglarını, session'larını admin tarafında görmek
istiyorum"* isteğidir. Diğer 15 görev onu karşılamıyor: bot session'ı **kendi** store'unda
korele ediyor (T8) ve konsol bot'a değil Palai'a bağlanıyor, yani bağ CP'de olmalı.

- [ ] **Step 1: Bugünkü filtreleri ölç — tasarım buna bağlı**

Run: `grep -n 'r.URL.Query()' apps/control-plane/api/sessions.go`
Run: `grep -n 'name: ListSessions' -A 15 storage/queries/sessions.sql`

2026-08-03'te birinci komut **boş** döndü. Boş kalırsa Step 3'teki alan eklenir; bugün bir filtre
zaten varsa `bot_id` onun yanına aynı biçimde eklenir — yeni bir desen icat edilmez.

- [ ] **Step 2: Başarısız testi yaz**

```go
func TestSessionsCanBeListedByBot(t *testing.T) {
	// İki bot, iki session. Her biri yalnız kendisininkini görür.
	mustCreateSession(t, map[string]any{"bot_id": "bot_ios"})
	mustCreateSession(t, map[string]any{"bot_id": "bot_android"})

	rec := doRequest(t, "GET", "/v1/sessions?bot_id=bot_ios", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Data []struct {
			BotID string `json:"bot_id"`
		} `json:"data"`
	}
	mustDecode(t, rec, &got)
	if len(got.Data) != 1 || got.Data[0].BotID != "bot_ios" {
		t.Fatalf("filter returned %d rows: %+v", len(got.Data), got.Data)
	}
}

func TestAnUnknownBotFilterReturnsEmptyNotEverything(t *testing.T) {
	// Tanınmayan bir filtre SESSIZCE düşerse liste her şeyi döndürür — bir filtrenin
	// yok sayılması, bir tenant'ın başka bir botun trafiğini görmesidir.
	mustCreateSession(t, map[string]any{"bot_id": "bot_ios"})
	rec := doRequest(t, "GET", "/v1/sessions?bot_id=bot_nonexistent", "")
	var got struct {
		Data []json.RawMessage `json:"data"`
	}
	mustDecode(t, rec, &got)
	if len(got.Data) != 0 {
		t.Fatalf("an unknown bot_id returned %d rows; the filter was dropped", len(got.Data))
	}
}
```

İkinci test bu ağacın kayıtlı kusur ailesindendir: yok sayılan bir parametre, geçtiğini sandığın
bir filtreyi vakum hâline getirir.

- [ ] **Step 3: Testlerin başarısız olduğunu gör**

Run: `go test ./apps/control-plane/api/ -run 'TestSessionsCanBeListedByBot|TestAnUnknownBot' -v`
Expected: FAIL

- [ ] **Step 4: `bot_id`'yi session'a taşı**

`sessions` tablosuna `bot_id TEXT NOT NULL DEFAULT ''` eklenir (migration numarası için
`ls storage/migrations/*.up.sql | tail -1` yeniden koşulur). `POST /v1/sessions` alanı kabul eder,
`GET /v1/sessions` ona göre filtreler.

**`ORDER BY` zorunludur** — sırasız bir `LIMIT` bu ağaçta iki güvenlik kararına karar verdi.
Alan CP için **opaktır**: `integration_bots`'a foreign key **konmaz**, çünkü CP bot'un ne olduğunu
bilmez (T4'ün kuralı).

- [ ] **Step 5: Testlerin geçtiğini gör**

Run: `go test ./apps/control-plane/api/ -run 'TestSessionsCanBeListedByBot|TestAnUnknownBot' -v`
Expected: PASS

- [ ] **Step 6: Bot'u alanı doldurur hâle getir**

T9'daki `CreateSession` çağrısı `BotID: cfg.BotID` taşır. Bu olmadan filtre her zaman boş döner —
yazarı olmayan bir alan, bu ağacın en sık kaydettiği kusurdur.

- [ ] **Step 7: Konsol sekmesini ekle**

Bot detay sayfasına üç sayı ve bir liste: session sayısı, son 24 saatteki run sayısı, açık onay
sayısı; altında son 20 session (`GET /v1/sessions?bot_id=…`), her satır `/sessions/{id}`'ye
bağlanır — run olayları ve tool çağrıları zaten orada render ediliyor.

- [ ] **Step 8: Testleri koştur ve commit'le**

Run: `go test ./apps/control-plane/api/ -v && cd apps/web-console && pnpm test -- bots.spec.ts`
Expected: PASS

```bash
git add apps/control-plane/api/sessions.go storage/queries/sessions.sql \
        storage/migrations/*_session_bot_id.*.sql apps/web-console/app/bots/\[id\]/
git commit -m "feat: sessions carry their bot, and the console reads them back"
```

---

## Kapanış doğrulaması

- [ ] `grep -ci slack cmd/cli/internal/stack/up.go` → **0**
- [ ] `ls apps/control-plane/**/slack*.go` → **boş**
- [ ] `grep -rn 'control-plane/internal' apps/slack-bot/` → **boş** (bot yalnız public API)
- [ ] `make verify` → yeşil
- [ ] `TEST=tenancy scripts/test/security` → yeşil
- [ ] Canlı: iki bot (ios, android) aynı workspace'te, iki ayrı thread, iki ayrı repo, ikisi de yanıt veriyor
- [ ] Konsolda her botun kendi session listesi görünüyor ve **birbirininkini göstermiyor** (T16)

---

## Bu planın iddia ETMEDİĞİ şeyler

- **Gerçek token-level streaming.** Granularity 500 ms journal tail + coalescing window'dur. Option C
  (journal'ı atlayan canlı kanal) hâlâ yoktur ve bu plan onu yapmaz.
- **OAuth "Add to Slack" akışı.** Manifest + elle token yolu kurulur. Authorization-code flow bu
  ağaçta hiç yoktur (`evidence_tools_memory.go`: *"bağlanmak bir epic, task değil"*) ve ayrı bir iştir.
- **Enterprise Grid / org-wide install.** Şablon `org_deploy_enabled: false` taşır.
- **Botlar arası konuşma.** Owner'ın ileriye dönük isteği: *"iOS bot'a 'android de yapmış' dediğimde
  android bot'u tagleyip ona soru sorabilsin."* Bu plan onu **kurmaz** ama **engellemez**: botlar tek
  bir `integration_bots` tablosunda aynı tenant içinde yaşar, her biri kendi `bot_id`'siyle session
  açar (T8) ve her session bot'unu taşır (T16) — yani "hangi botlar var, hangi işe bakıyorlar"
  sorusu bir sorgudur. Eksik olan tek şey bir botun başka bir botun oturumuna mesaj yazabilmesidir,
  ki o `POST /v1/sessions/{id}/commands`'in zaten yaptığı iştir. **Bu plan boyunca hiçbir tasarım
  kararı bunu imkânsız kılmamalıdır** — özellikle `bot_id` anahtarı ve kanal-agnostik `kind`.
- **Execution relay.** Reddedildi (2026-08-03). Mac, CP'nin üzerinde koştuğu makinedir.
