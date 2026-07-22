//go:build e2e

package responses

// E13 Task 4: the run-history LIST (GET /v1/responses) over the REAL store. It proves the durable
// keyset page + cursor against Postgres and RLS, and the TEN-001 cursor-fuzz contract end to end:
// a second tenant presenting the first tenant's cursor is an EXPLICIT 400 invalid_cursor, not a
// silently-empty page, and a second tenant's own list never sees the first tenant's runs (RLS).

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

// listResponses issues GET /v1/responses with the given query string and bearer token.
func (h *harness) listResponses(token, query string) *http.Response {
	h.t.Helper()
	u := h.base + "/v1/responses"
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		h.t.Fatalf("build GET error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("GET /v1/responses error = %v", err)
	}
	return resp
}

func decodeListPage(t *testing.T, resp *http.Response) contracts.Page {
	t.Helper()
	defer resp.Body.Close()
	var p contracts.Page
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode page error = %v", err)
	}
	return p
}

func TestListResponsesPagesRealRunHistory(t *testing.T) {
	h := newHarness(t)

	// Three admitted runs in this tenant's history.
	for i := 0; i < 3; i++ {
		h.admit()
	}

	first := h.listResponses(h.token, "limit=2")
	if first.StatusCode != http.StatusOK {
		first.Body.Close()
		t.Fatalf("list status = %d, want 200", first.StatusCode)
	}
	page := decodeListPage(t, first)
	if len(page.Data) != 2 || !page.HasMore || page.NextCursor == nil {
		t.Fatalf("first page: len=%d has_more=%v cursor=%v, want 2 rows + a further page", len(page.Data), page.HasMore, page.NextCursor)
	}

	second := decodeListPage(t, h.listResponses(h.token, "limit=2&after="+url.QueryEscape(*page.NextCursor)))
	if len(second.Data) != 1 || second.HasMore {
		t.Fatalf("second page: len=%d has_more=%v, want the 1 remaining row and no further page", len(second.Data), second.HasMore)
	}

	// Every row is a canonical, tenant-stamped Response projection.
	for _, raw := range page.Data {
		blob, _ := json.Marshal(raw)
		var r contracts.Response
		if err := json.Unmarshal(blob, &r); err != nil {
			t.Fatalf("decode list row error = %v (row=%s)", err, blob)
		}
		if !r.ID.Valid() || r.Object != "response" {
			t.Fatalf("list row is not a canonical response: %+v", r)
		}
		if r.OrganizationID != contracts.OrganizationID(h.tenant.Organization) {
			t.Fatalf("list row org = %q, want the verified %q", r.OrganizationID, h.tenant.Organization)
		}
	}
}

func TestListResponsesRejectsCrossTenantCursor(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 2; i++ {
		h.admit()
	}
	page := decodeListPage(t, h.listResponses(h.token, "limit=1"))
	if page.NextCursor == nil {
		t.Fatal("expected a next_cursor to replay across tenants")
	}

	// A SECOND tenant on the same stack, provisioned with its own key.
	otherToken := newID("e2e-tok")
	seedTenantWithKey(t, h.spine.Pool(), otherToken)

	// The second tenant presenting the first tenant's cursor is an EXPLICIT reject (400
	// invalid_cursor), not a silently-empty 200 — the TEN-001 cursor-fuzz contract.
	resp := h.listResponses(otherToken, "limit=1&after="+url.QueryEscape(*page.NextCursor))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("cross-tenant cursor status = %d, want 400", resp.StatusCode)
	}
	var prob contracts.Problem
	if err := json.NewDecoder(resp.Body).Decode(&prob); err != nil {
		t.Fatalf("decode problem error = %v", err)
	}
	if prob.Code != "invalid_cursor" {
		t.Fatalf("code = %q, want invalid_cursor", prob.Code)
	}

	// And the second tenant's OWN list never sees the first tenant's runs (RLS).
	own := decodeListPage(t, h.listResponses(otherToken, ""))
	if len(own.Data) != 0 {
		t.Fatalf("second tenant sees %d row(s) of history, want 0 (RLS confines the list)", len(own.Data))
	}
}
