package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// fakeUsage scripts each seam outcome so the metering handler contract is exercised without a database.
// The recorded scope/body/query let a test assert the provision gate ran on the WRITE routes only and
// that the shared list parse reached the store.
type fakeUsage struct {
	write     ProvisionResult
	read      ProvisionResult
	rows      []ListRow
	lastScope middleware.Scope
	lastBody  []byte
	lastQuery ListQuery
}

func (f *fakeUsage) SetBudget(_ context.Context, s middleware.Scope, b []byte) (ProvisionResult, error) {
	f.lastScope, f.lastBody = s, b
	return f.write, nil
}
func (f *fakeUsage) ListBudgets(_ context.Context, s middleware.Scope) (ProvisionResult, error) {
	f.lastScope = s
	return f.read, nil
}
func (f *fakeUsage) SetQuota(_ context.Context, s middleware.Scope, b []byte) (ProvisionResult, error) {
	f.lastScope, f.lastBody = s, b
	return f.write, nil
}
func (f *fakeUsage) ListQuotas(_ context.Context, s middleware.Scope) (ProvisionResult, error) {
	f.lastScope = s
	return f.read, nil
}
func (f *fakeUsage) UsageSummary(_ context.Context, s middleware.Scope) (ProvisionResult, error) {
	f.lastScope = s
	return f.read, nil
}
func (f *fakeUsage) ListUsageLedger(_ context.Context, s middleware.Scope, q ListQuery) ([]ListRow, error) {
	f.lastScope, f.lastQuery = s, q
	return f.rows, nil
}

func usageTestServer(t *testing.T, verifier middleware.Verifier, usage UsageAPI) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(verifier, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil, WithUsage(usage)))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestUsageSurface pins the routing + rendering contract: setting a limit is 200 (it is an upsert, not a
// creation — a re-POST restates the same resource), the lists and the summary are 200, and a
// strict-decode reject is a 400.
func TestUsageSurface(t *testing.T) {
	admin := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1"}}
	fake := &fakeUsage{
		write: ProvisionResult{Body: []byte(`{"object":"budget","meter_prefix":"model.","limit_quantity":1000}`)},
		read:  ProvisionResult{Body: []byte(`{"object":"list","data":[]}`)},
	}
	base := usageTestServer(t, admin, fake)

	cases := []struct {
		method, path, body string
		wantStatus         int
	}{
		{"POST", "/v1/budgets", `{"meter_prefix":"model.","limit_quantity":1000}`, http.StatusOK},
		{"GET", "/v1/budgets", ``, http.StatusOK},
		{"POST", "/v1/quotas", `{"meter_prefix":"run.","limit_quantity":50,"window_seconds":3600}`, http.StatusOK},
		{"GET", "/v1/quotas", ``, http.StatusOK},
		{"GET", "/v1/usage", ``, http.StatusOK},
	}
	for _, c := range cases {
		resp := do(t, c.method, base+c.path, c.body, nil)
		if resp.StatusCode != c.wantStatus {
			t.Fatalf("%s %s status = %d, want %d", c.method, c.path, resp.StatusCode, c.wantStatus)
		}
		resp.Body.Close()
	}

	fake.write = ProvisionResult{BadField: true}
	if resp := do(t, "POST", base+"/v1/budgets", `{"nope":1}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown-field budget status = %d, want 400", resp.StatusCode)
	}
	fake.write = ProvisionResult{MissingField: "limit_quantity"}
	if resp := do(t, "POST", base+"/v1/quotas", `{"meter_prefix":"run."}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing-field quota status = %d, want 400", resp.StatusCode)
	}
}

// TestUsageWritesRequireProvisionScopeButReadsDoNot pins the split the surface makes on purpose: setting
// a limit is an administrative act, but SEEING what a tenant has spent is ordinary metering visibility —
// gating the read behind the provision capability would hide a tenant's own usage from its own key.
func TestUsageWritesRequireProvisionScopeButReadsDoNot(t *testing.T) {
	limited := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1", Scopes: []string{"responses"}}}
	fake := &fakeUsage{read: ProvisionResult{Body: []byte(`{"object":"list","data":[]}`)}}
	base := usageTestServer(t, limited, fake)

	for _, path := range []string{"/v1/budgets", "/v1/quotas"} {
		resp := do(t, "POST", base+path, `{"meter_prefix":"model.","limit_quantity":1}`, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s without the provision capability status = %d, want 403", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	for _, path := range []string{"/v1/usage", "/v1/budgets", "/v1/quotas"} {
		resp := do(t, "GET", base+path, ``, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s without the provision capability status = %d, want 200 (a tenant may always read its own metering)", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestUsageLedgerPageReusesTheSharedCursor proves the ledger page is the SAME keyset surface every other
// list uses — one over-fetch, one tenant-bound cursor, one rejection for a foreign one — rather than a
// second pagination dialect. The ledger carries no lifecycle state, so ?status= is an explicit 400.
func TestUsageLedgerPageReusesTheSharedCursor(t *testing.T) {
	admin := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1"}}
	fake := &fakeUsage{}
	base := usageTestServer(t, admin, fake)

	now := time.Now().UTC()
	for i := range 3 {
		fake.rows = append(fake.rows, ListRow{
			ID:        "use_" + string(rune('a'+i)),
			CreatedAt: now.Add(-time.Duration(i) * time.Minute),
			Body:      json.RawMessage(`{"object":"usage_ledger_entry"}`),
		})
	}

	page := getUsagePage(t, base, "limit=2")
	// The store is asked for one row MORE than the page: that over-fetch is how has_more is decided
	// without a second round trip, and it is the same contract every other list on this surface uses.
	if fake.lastQuery.Limit != 3 {
		t.Fatalf("store saw limit = %d, want 3 (the requested 2 plus the has_more over-fetch)", fake.lastQuery.Limit)
	}
	if len(page.Data) != 2 || !page.HasMore || page.NextCursor == nil {
		t.Fatalf("page = %d rows has_more=%v cursor=%v, want 2 rows + a further page", len(page.Data), page.HasMore, page.NextCursor)
	}

	// The minted cursor round-trips for its own tenant...
	getUsagePage(t, base, "limit=2&after="+*page.NextCursor)
	if fake.lastQuery.After == nil {
		t.Fatal("store saw no keyset position after a cursor was presented")
	}
	// ...and a cursor minted for a DIFFERENT resource kind is an explicit 400, never a silent page.
	foreign := encodeCursor(cursorKey(), "responses", middleware.Scope{Organization: "org_1", Project: "prj_1"},
		ListCursor{CreatedAt: now, ID: "resp_1"})
	resp := do(t, "GET", base+"/v1/usage/ledger?after="+foreign, ``, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("foreign-kind cursor status = %d, want 400", resp.StatusCode)
	}

	if resp := do(t, "GET", base+"/v1/usage/ledger?status=settled", ``, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("?status= on the ledger status = %d, want 400 (the ledger has no lifecycle state to filter)", resp.StatusCode)
	}
}

func getUsagePage(t *testing.T, base, query string) contracts.Page {
	t.Helper()
	resp := do(t, "GET", base+"/v1/usage/ledger?"+query, ``, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /v1/usage/ledger?%s status = %d (body=%s)", query, resp.StatusCode, body)
	}
	var page contracts.Page
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	return page
}
