//go:build component

package store_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
)

// THE ONE SENTENCE THIS FILE PROVES: a Sessions screen can be rendered from ONE call to the shipped
// HTTP surface, and the columns it renders come from the real database rather than from a projection a
// test built.
//
// WHY IT EXISTS ON TOP OF THE COORDINATOR TESTS. Those drive Store.ListSessions directly, so they prove
// the SQL. They cannot prove the route: a field can be selected, scanned, and then quietly dropped by
// the marshaller between the store and the wire, which is a failure that looks like success from either
// side alone. Everything below the tenant seed here is an HTTP call to the shipped router —
//
//	POST /v1/sessions              {name}      (the "new session" affordance)
//	POST /v1/sessions                          (bodyless, the pre-existing call)
//	PATCH /v1/sessions/{id}        {name}      (the rename affordance — the one that matters, because
//	                                            most sessions are opened implicitly and never named)
//	GET  /v1/sessions?limit=…                  (the screen)
//	GET  /v1/sessions/{id}                     (the row's own link)
//
// — and the assertions are made on the response BODIES, which are the bytes a browser would render.
func TestSessionListJourneyRendersTheScreenFromOneCall(t *testing.T) {
	f := newSessionListFixture(t)

	// A labelled session, and the agent/tokens/span its runs and meters produce. The runs and ledger
	// rows are seeded in SQL because nothing in the public API mints a completed run with settled
	// meters synchronously; the LABEL and the RENAME below go through HTTP, which is what is new here.
	named := f.createSession(t, `{"name":"Nightly verification"}`)
	f.seedAgentRun(t, named, "Gece Doğrulama", 90*time.Second)
	f.seedTokens(t, named, 1200, 340)

	// An unlabelled session carrying a prompt — the shape every implicitly-opened session has.
	derived := f.createSession(t, ``)
	f.seedPrompt(t, derived, "  Release  checklist\nfor  the  night  ")

	// A session that exists and has done nothing.
	bare := f.createSession(t, ``)

	page := f.list(t, "?limit=10")
	rows := map[string]contracts.Session{}
	for _, raw := range page.Data {
		var s contracts.Session
		encoded, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("re-encode page row: %v", err)
		}
		if err := json.Unmarshal(encoded, &s); err != nil {
			t.Fatalf("decode page row: %v", err)
		}
		rows[string(s.ID)] = s
	}
	if len(rows) != 3 {
		t.Fatalf("page carried %d sessions, want the 3 this journey created", len(rows))
	}

	got := rows[named]
	if got.Name != "Nightly verification" || got.NameSource != "operator" {
		t.Fatalf("labelled row = %q/%q over HTTP, want it carried from the create body", got.Name, got.NameSource)
	}
	if len(got.Agents) != 1 || got.Agents[0] != "Gece Doğrulama" {
		t.Fatalf("labelled row agents = %v, want [Gece Doğrulama] — the per-run agent must survive the marshaller", got.Agents)
	}
	if got.InputTokens != 1200 || got.OutputTokens != 340 {
		t.Fatalf("labelled row tokens = %d/%d, want 1200/340", got.InputTokens, got.OutputTokens)
	}
	if got.DurationMs == nil || *got.DurationMs != 90_000 {
		t.Fatalf("labelled row duration_ms = %v, want 90000", got.DurationMs)
	}
	if got.FirstActivityAt == nil || got.LastActivityAt == nil {
		t.Fatal("labelled row carries no activity span")
	}

	got = rows[derived]
	if got.NameSource != "derived" {
		t.Fatalf("unlabelled row name_source = %q, want derived", got.NameSource)
	}
	// The whitespace fold is not cosmetic: the prompt contains a newline, and a table cell that renders
	// one silently loses the rest of the label.
	if got.Name != "Release checklist for the night" {
		t.Fatalf("derived label = %q, want the prompt folded to one line with single spaces", got.Name)
	}

	got = rows[bare]
	if got.Name != "" || got.NameSource != "none" {
		t.Fatalf("bare row = %q/%q, want empty/none", got.Name, got.NameSource)
	}
	if got.DurationMs != nil || got.FirstActivityAt != nil {
		t.Fatalf("bare row has a duration (%v); a session that never ran has none, not a zero one", got.DurationMs)
	}
	if got.Agents == nil {
		t.Fatal("bare row's agents is JSON null; it must be [] — 'no agents' and 'unknown' are not two facts here")
	}

	// The rename, over HTTP, on the session that was never labelled. This is the affordance the screen
	// needs, and its effect must be visible on the NEXT list read rather than only in its own response.
	renamed := f.rename(t, derived, `{"name":"Gece Doğrulama"}`)
	if renamed.Name != "Gece Doğrulama" || renamed.NameSource != "operator" {
		t.Fatalf("rename response = %q/%q, want the label just set", renamed.Name, renamed.NameSource)
	}
	after := f.list(t, "?limit=10")
	if !strings.Contains(pageJSON(t, after), `"Gece Doğrulama"`) {
		t.Fatalf("the renamed label is absent from the next page:\n%s", pageJSON(t, after))
	}

	// The row's own link resolves and agrees with the row. A list that disagrees with the resource it
	// links to is worse than one that omits the column.
	detail := f.get(t, derived)
	if detail.Name != renamed.Name || detail.NameSource != renamed.NameSource {
		t.Fatalf("detail read = %q/%q but the row said %q/%q", detail.Name, detail.NameSource, renamed.Name, renamed.NameSource)
	}
}

// sessionListFixture is a migrated spine plus the shipped router with ONLY the session family mounted:
// a router that mounts nothing else cannot answer one of these calls by accident.
type sessionListFixture struct {
	url          string
	repo         *store.Store
	org, project string
}

func newSessionListFixture(t *testing.T) *sessionListFixture {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	repo, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	f := &sessionListFixture{repo: repo, org: newID("org"), project: newID("prj")}
	pool := repo.Spine().Pool()
	exec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, f.org)
	exec(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, f.project, f.org)

	scope := middleware.Scope{Organization: f.org, Project: f.project, Principal: newID("prin")}
	ts := httptest.NewServer(api.NewRouter(
		scopedVerifier{scope}, nil, nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		api.SSEConfig{}, nil, nil))
	t.Cleanup(ts.Close)
	f.url = ts.URL
	return f
}

func (f *sessionListFixture) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer component-key-not-a-credential")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, raw
}

func (f *sessionListFixture) createSession(t *testing.T, body string) string {
	t.Helper()
	status, raw := f.do(t, "POST", "/v1/sessions", body)
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/sessions = %d, want 201 (body=%s)", status, raw)
	}
	var s contracts.Session
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode created session: %v", err)
	}
	return string(s.ID)
}

func (f *sessionListFixture) rename(t *testing.T, id, body string) contracts.Session {
	t.Helper()
	status, raw := f.do(t, "PATCH", "/v1/sessions/"+id, body)
	if status != http.StatusOK {
		t.Fatalf("PATCH /v1/sessions/%s = %d, want 200 (body=%s)", id, status, raw)
	}
	var s contracts.Session
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode renamed session: %v", err)
	}
	return s
}

func (f *sessionListFixture) get(t *testing.T, id string) contracts.Session {
	t.Helper()
	status, raw := f.do(t, "GET", "/v1/sessions/"+id, "")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/sessions/%s = %d, want 200 (body=%s)", id, status, raw)
	}
	var s contracts.Session
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode session detail: %v", err)
	}
	return s
}

func (f *sessionListFixture) list(t *testing.T, query string) contracts.Page {
	t.Helper()
	status, raw := f.do(t, "GET", "/v1/sessions"+query, "")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/sessions%s = %d, want 200 (body=%s)", query, status, raw)
	}
	var page contracts.Page
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	return page
}

// seedAgentRun gives a session one completed run pinned to a published revision of a profile named
// `agent`, spanning `span` between its creation and its last transition.
func (f *sessionListFixture) seedAgentRun(t *testing.T, session, agent string, span time.Duration) {
	t.Helper()
	pool := f.repo.Spine().Pool()
	profile, revision := newID("aprof"), newID("arev")
	exec(t, pool, `INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,$4)`,
		profile, f.org, f.project, agent)
	exec(t, pool, `INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, published_at)
	               VALUES ($1,$2,$3,$4,1,clock_timestamp())`, revision, f.org, f.project, profile)
	start := time.Now().UTC().Add(-time.Hour)
	exec(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, state, created_at, updated_at, agent_revision_id)
	               VALUES ($1,$2,$3,$4,'completed',$5,$6,$7)`,
		newID("run"), f.org, f.project, session, start, start.Add(span), revision)
}

func (f *sessionListFixture) seedTokens(t *testing.T, session string, in, out int64) {
	t.Helper()
	pool := f.repo.Spine().Pool()
	for meter, quantity := range map[string]int64{"model.input_tokens": in, "model.output_tokens": out} {
		exec(t, pool, `INSERT INTO usage_ledger (id, organization_id, project_id, session_id, meter, quantity, unit, dedupe_key)
		               VALUES ($1,$2,$3,$4,$5,$6,'token',$7)`,
			newID("use"), f.org, f.project, session, meter, quantity, newID("dk"))
	}
}

func (f *sessionListFixture) seedPrompt(t *testing.T, session, prompt string) {
	t.Helper()
	encoded, err := json.Marshal(prompt)
	if err != nil {
		t.Fatalf("encode prompt: %v", err)
	}
	exec(t, f.repo.Spine().Pool(), `INSERT INTO responses (id, organization_id, project_id, session_id, state, input)
	     VALUES ($1,$2,$3,$4,'completed',$5::jsonb)`, newID("resp"), f.org, f.project, session, string(encoded))
}

func pageJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
