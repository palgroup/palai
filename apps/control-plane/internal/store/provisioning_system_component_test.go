//go:build component

package store

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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
