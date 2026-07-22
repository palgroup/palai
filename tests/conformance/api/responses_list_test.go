package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

// listResponses issues GET /v1/responses with the given query string and a valid bearer token.
func listResponses(t *testing.T, srv *httptest.Server, query string) *http.Response {
	t.Helper()
	u := srv.URL + "/v1/responses"
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// decodePage decodes a list body into the canonical Page contract.
func decodePage(t *testing.T, body []byte) contracts.Page {
	t.Helper()
	var p contracts.Page
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode page: %v (body=%s)", err, body)
	}
	return p
}

// TestListResponsesPagesWithCursor admits three responses and pages them two-at-a-time, proving
// the Page envelope, has_more, and that the next_cursor advances to the remaining rows.
func TestListResponsesPagesWithCursor(t *testing.T) {
	srv := newTestServer(t)
	for _, key := range []string{"a", "b", "c"} {
		resp := postResponses(t, srv, authedHeaders("idem-"+key), `{"input":"`+key+`"}`)
		resp.Body.Close()
	}

	first := listResponses(t, srv, "limit=2")
	page := decodePage(t, readBody(t, first))
	if first.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", first.StatusCode)
	}
	if len(page.Data) != 2 {
		t.Fatalf("first page len = %d, want 2", len(page.Data))
	}
	if !page.HasMore || page.NextCursor == nil {
		t.Fatalf("first page has_more=%v next_cursor=%v, want a further page", page.HasMore, page.NextCursor)
	}

	second := listResponses(t, srv, "limit=2&after="+url.QueryEscape(*page.NextCursor))
	page2 := decodePage(t, readBody(t, second))
	if len(page2.Data) != 1 {
		t.Fatalf("second page len = %d, want the 1 remaining row", len(page2.Data))
	}
	if page2.HasMore {
		t.Fatalf("second page has_more=true, want false (no rows left)")
	}
}

// TestListResponsesAcceptsStatusFilter guards that responses (a status-capable list) still accepts
// ?status= with a 200 — the honest-filter reject (review SHOULD 1) is scoped to lists WITHOUT a status
// column, never responses/sessions.
func TestListResponsesAcceptsStatusFilter(t *testing.T) {
	srv := newTestServer(t)
	resp := postResponses(t, srv, authedHeaders("idem-s"), `{"input":"x"}`)
	resp.Body.Close()
	got := listResponses(t, srv, "status=queued&limit=10")
	if got.StatusCode != http.StatusOK {
		t.Fatalf("responses ?status= status = %d, want 200 (status filtering IS supported here)", got.StatusCode)
	}
	got.Body.Close()
}

// TestListResponsesRejectsForeignCursor is the TEN-001 cursor-fuzz half at the HTTP edge: a
// tampered/garbage cursor is a 400 invalid_cursor, never a silently-empty 200.
func TestListResponsesRejectsForeignCursor(t *testing.T) {
	srv := newTestServer(t)
	resp := listResponses(t, srv, "after=not-a-real-cursor")
	prob := decodeProblem(t, resp)
	if prob.Status != http.StatusBadRequest {
		t.Fatalf("foreign cursor status = %d, want 400", prob.Status)
	}
	if prob.Code != "invalid_cursor" {
		t.Fatalf("code = %q, want invalid_cursor", prob.Code)
	}
}

// TestListResponsesRequiresAuth proves the list is behind the bearer gate like every other route.
func TestListResponsesRequiresAuth(t *testing.T) {
	srv := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/responses", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list status = %d, want 401", resp.StatusCode)
	}
}
