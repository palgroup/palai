//go:build component

package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/storage"
)

// keyedVerifier resolves each bearer token to whatever fixed scope the test registered under it, so one
// server can be handed two callers holding two different capabilities in the same test.
type keyedVerifier map[string]middleware.Scope

func (v keyedVerifier) VerifyAPIKey(_ context.Context, token string) (middleware.Scope, error) {
	scope, ok := v[token]
	if !ok {
		return middleware.Scope{}, middleware.ErrInvalidToken
	}
	return scope, nil
}

// TestTenantAdminKeyCannotOpenAnotherTenant proves opening a tenant is the platform's job, not the
// customer plane's. A tenant admin key (empty Scopes, unrestricted under HasScope) is exactly the key
// that must NOT reach this route: this is not a capability check, it is a BOUNDARY — opening a new
// tenant is not an operation on the caller's own tenant, it never belonged on the customer plane at
// all. Only a key carrying `system` may.
//
// IT DROVE POST /v1/organizations UNTIL A.2 TASK 6 AND WAS LEFT BEHIND BY IT — measured RED on
// 2026-08-04, and the way it failed is the reason this note exists. The route was unmounted, so the
// tenant-admin leg got 404 rather than 403 and reported "tenant admin anahtarı organizasyon açabildi:
// 404" — a message accusing the tenant key of opening a tenant, when nothing had been opened at all and
// the boundary under test was never exercised. The leak check below would then have errored on a dropped
// table. Both legs now drive POST /v1/projects, which authorizeSystem gates and which api/provisioning.go
// records as the ONLY tenant-opening route; the seam under test (authorizeSystem) is unchanged.
func TestTenantAdminKeyCannotOpenAnotherTenant(t *testing.T) {
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	repo, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	idstore := identity.New(repo.Spine().Pool())

	verifier := keyedVerifier{
		"tenant-admin": {Project: "prj_existing", Principal: "prin_existing"},
		"platform":     {Principal: "prin_platform", Scopes: []string{middleware.ScopeSystem}},
	}
	router := api.NewRouter(verifier, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, idstore, nil,
		api.SSEConfig{}, nil, nil)
	ts := httptest.NewServer(router)
	defer ts.Close()

	resp := postProject(t, ts.URL, "tenant-admin", `{"display_name":"squatted"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tenant admin anahtarı başka bir tenant açabildi: %d (beklenen 403)", resp.StatusCode)
	}

	resp = postProject(t, ts.URL, "platform", `{"display_name":"legit"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("platform anahtarı tenant açamadı: %d", resp.StatusCode)
	}

	// VE SIZINTI KONTROLÜ, cevap kodundan SONRA: reddedilen çağrı hiçbir satır yazmamalı. Bu ağaç bir kez
	// sızıntı iddiasından ÖNCE cevap kodunu denetleyen bir test yazdı ve gerçek kusur "isimlendirme
	// sorunu" gibi okundu; bu sıra bilerek korunuyor.
	var n int
	if err := repo.Spine().Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM projects WHERE display_name = 'squatted'`).Scan(&n); err != nil {
		t.Fatalf("count squatted projects: %v", err)
	}
	if n != 0 {
		t.Fatalf("403 döndü ama %d proje satırı yazıldı", n)
	}
}

func postProject(t *testing.T, base, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/projects", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST /v1/projects: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/projects: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestAKeyCannotMintAKeyMoreCapableThanItself drives the WIRE, not the predicate.
//
// middleware's own test pins Scope.CanGrant's logic. This one exists because a correct predicate that
// nothing calls is the defect this tree keeps finding: the check lives on the WRITE path (identity's
// CreateAPIKey), and the route in front of it is gated on `provision` — a gate that answers "may this
// caller create a key", never "what may go IN it". So the capability travels in the BODY, past the gate,
// and only this path can refuse it.
//
// BOTH DIRECTIONS RUN. A test that only proves the refusal is satisfied by a store that refuses
// everything, which would break the platform's ability to mint its second key.
func TestAKeyCannotMintAKeyMoreCapableThanItself(t *testing.T) {
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	repo, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	idstore := identity.New(repo.Spine().Pool())

	// The platform opens a tenant, so the key below is minted against a project that really exists —
	// otherwise a 404 could be mistaken for the refusal this test is about.
	// THE PLATFORM KEY CARRIES BOTH, and that is not fixture padding — it is what
	// identity.ProvisionFirstTenant seeds (`system`, `provision`, `approve`). The route is gated on
	// `provision` and the GRANT is checked against `system`: two different questions on one request. A
	// first draft gave this key `system` alone and leg (3) failed 403 at the ROUTE, before the grant check
	// ran at all — which would have "proved" the refusal while never reaching the code under test.
	verifier := keyedVerifier{
		"platform":     {Principal: "prin_platform", Scopes: []string{middleware.ScopeSystem, "provision"}},
		"tenant-admin": {Principal: "prin_t", Scopes: []string{"provision"}},
	}
	router := api.NewRouter(verifier, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, idstore, nil,
		api.SSEConfig{}, nil, nil)
	ts := httptest.NewServer(router)
	defer ts.Close()

	var opened struct {
		ID string `json:"id"`
	}
	resp := postProject(t, ts.URL, "platform", `{"display_name":"mint-scope"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("platform tenant açamadı: %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&opened); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	verifier["tenant-admin"] = middleware.Scope{Project: opened.ID, Principal: "prin_t", Scopes: []string{"provision"}}

	body := `{"project_id":"` + opened.ID + `","scopes":["` + middleware.ScopeSystem + `"]}`

	// (1) THE REFUSAL. A `provision` key asking for `system` is 403 — not 400: the body is well formed
	// and the capability is real; what is missing is the caller's authority to hand it out.
	refused := postJSON(t, ts.URL, "tenant-admin", "/v1/api-keys", body)
	if refused.StatusCode != http.StatusForbidden {
		t.Fatalf("provision anahtarı system mintleyebildi: %d (beklenen 403)", refused.StatusCode)
	}

	// (2) AND NO ROW WAS WRITTEN — checked AFTER the status, the order this file already argues for: a
	// refusal that still commits is the failure worth catching, and it hides behind a correct 403.
	var leaked int
	if err := repo.Spine().Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM api_keys WHERE project_id = $1 AND $2 = ANY(scopes)`,
		opened.ID, middleware.ScopeSystem).Scan(&leaked); err != nil {
		t.Fatalf("count system keys: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("403 döndü ama system taşıyan %d anahtar yazıldı", leaked)
	}

	// (3) THE GRANT STILL WORKS. Without this the rule could be satisfied by refusing everyone.
	allowed := postJSON(t, ts.URL, "platform", "/v1/api-keys", body)
	if allowed.StatusCode != http.StatusCreated {
		t.Fatalf("system taşıyan anahtar system veremedi: %d (beklenen 201)", allowed.StatusCode)
	}
	var minted struct {
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(allowed.Body).Decode(&minted); err != nil {
		t.Fatalf("decode minted key: %v", err)
	}
	if len(minted.Scopes) != 1 || minted.Scopes[0] != middleware.ScopeSystem {
		t.Fatalf("mintlenen anahtarın kapsamı %v, beklenen [%s]", minted.Scopes, middleware.ScopeSystem)
	}
}

// postJSON posts an arbitrary body to path with token's bearer, for the two mint attempts above.
func postJSON(t *testing.T, base, token, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST %s: %v", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestAnInstallationSeededBeforeTheScopeChangeIsRepairedOnBoot drives the UPGRADE path.
//
// A stack bootstrapped before 4155709a has `key_local` with an EMPTY scope set. Empty satisfies
// HasScope for every ordinary capability but never HasSystem, so that operator cannot reach any platform
// surface — and once a key stopped being able to grant a capability it does not hold, the mint-yourself-
// one workaround closed too. Bootstrap's `count > 0` early return meant nothing ever repaired it.
//
// The old shape is RECREATED here rather than assumed, so this test measures the transition instead of
// whatever state the database happened to be in.
func TestAnInstallationSeededBeforeTheScopeChangeIsRepairedOnBoot(t *testing.T) {
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	repo, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := repo.Bootstrap(ctx, "component-bootstrap-key"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	sys := storage.WithSystemScope(ctx)

	// Restore whatever key_local had, so this test does not decide another test's fixture.
	var original []string
	err = repo.Spine().Pool().QueryRow(sys, `SELECT scopes FROM api_keys WHERE id = 'key_local'`).Scan(&original)
	if errors.Is(err, pgx.ErrNoRows) {
		// OWN THE FIXTURE RATHER THAN INHERIT ONE. Bootstrap above only seeds key_local through
		// ProvisionFirstTenant when api_keys is GLOBALLY empty; it takes the reconcileBootstrapScopes
		// branch instead the moment ANY row exists, and that branch only UPDATEs a key_local row that is
		// already there — it never creates one. This package runs every component-real test against ONE
		// shared database in ONE process, and another test in this file (TestTenantAdminKeyCannotOpenAnotherTenant,
		// TestAKeyCannotMintAKeyMoreCapableThanItself) opens its own tenant first when the whole package
		// runs without a -run filter, so api_keys already holds rows by the time this test's Bootstrap call
		// above ran and key_local was never born. This test's claim is about the UPGRADE PATH, not about
		// running before every other test in the package, so it provisions the fixture itself instead of
		// depending on table-wide emptiness it does not control.
		if err := identity.New(repo.Spine().Pool()).ProvisionFirstTenant(ctx, "component-bootstrap-key"); err != nil {
			t.Fatalf("provision the key_local fixture this test owns: %v", err)
		}
		err = repo.Spine().Pool().QueryRow(sys, `SELECT scopes FROM api_keys WHERE id = 'key_local'`).Scan(&original)
	}
	if err != nil {
		t.Fatalf("read key_local: %v", err)
	}
	t.Cleanup(func() {
		_, _ = repo.Spine().Pool().Exec(context.Background(),
			`UPDATE api_keys SET scopes = $1 WHERE id = 'key_local'`, original)
	})

	// THE PRE-4155709a SHAPE.
	if _, err := repo.Spine().Pool().Exec(sys,
		`UPDATE api_keys SET scopes = '{}' WHERE id = 'key_local'`); err != nil {
		t.Fatalf("recreate the old empty-scope seed: %v", err)
	}

	// A boot against that stack. count > 0, so no tenant is provisioned — the reconciliation is the only
	// thing that may act.
	if err := repo.Bootstrap(ctx, "component-bootstrap-key"); err != nil {
		t.Fatalf("re-boot: %v", err)
	}

	var got []string
	if err := repo.Spine().Pool().QueryRow(sys, `SELECT scopes FROM api_keys WHERE id = 'key_local'`).Scan(&got); err != nil {
		t.Fatalf("read key_local after boot: %v", err)
	}
	want := identity.FirstTenantScopes()
	if len(got) != len(want) {
		t.Fatalf("key_local kapsamı %v, beklenen %v — eski kurulum onarılmadı", got, want)
	}
	repaired := middleware.Scope{Scopes: got}
	if !repaired.HasSystem() {
		t.Fatalf("onarımdan sonra bile HasSystem false: %v — bu kurulum platform yüzeylerine hâlâ ulaşamıyor", got)
	}

	// AND IT DOES NOT TOUCH A DELIBERATE SET. A non-empty scope list was chosen by somebody; re-widening
	// it would be this reconciliation quietly granting the platform capability to a narrowed key.
	if _, err := repo.Spine().Pool().Exec(sys,
		`UPDATE api_keys SET scopes = '{provision}' WHERE id = 'key_local'`); err != nil {
		t.Fatalf("set a deliberate narrow scope: %v", err)
	}
	if err := repo.Bootstrap(ctx, "component-bootstrap-key"); err != nil {
		t.Fatalf("re-boot over a narrowed key: %v", err)
	}
	if err := repo.Spine().Pool().QueryRow(sys, `SELECT scopes FROM api_keys WHERE id = 'key_local'`).Scan(&got); err != nil {
		t.Fatalf("read key_local: %v", err)
	}
	if len(got) != 1 || got[0] != "provision" {
		t.Fatalf("kasıtlı olarak daraltılmış kapsam değiştirildi: %v — yalnız BOŞ küme onarılmalı", got)
	}
}
