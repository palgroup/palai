package palai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestApproveHitsTheApproveRoute(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return jsonResp(200, `{"id":"apr_1","object":"approval.decision","decision":"approved"}`), nil
	})
	c := testClient(t, rt)

	out, err := c.Approvals.Approve(context.Background(), "apr_1", DecisionParams{RequestHash: "h1"})
	if err != nil {
		t.Fatalf("Approvals.Approve: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/v1/approvals/apr_1/approve" {
		t.Fatalf("%s %s, want POST /v1/approvals/apr_1/approve", gotMethod, gotPath)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, gotBody)
	}
	if body["request_hash"] != "h1" {
		t.Fatalf("body %s does not carry request_hash", gotBody)
	}
	if _, present := body["approve"]; present {
		t.Fatalf("body %s must not carry an approve field — the server's strict decode 400s on it", gotBody)
	}
	if out.ID != "apr_1" || out.Object != "approval.decision" || out.Decision != "approved" {
		t.Fatalf("result = %+v, want id=apr_1 object=approval.decision decision=approved", out)
	}
}

func TestDenyHitsTheDenyRouteAndCarriesReason(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return jsonResp(200, `{"id":"apr_1","object":"approval.decision","decision":"denied"}`), nil
	})
	c := testClient(t, rt)

	out, err := c.Approvals.Deny(context.Background(), "apr_1", DecisionParams{RequestHash: "h1", Reason: "not now"})
	if err != nil {
		t.Fatalf("Approvals.Deny: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/v1/approvals/apr_1/deny" {
		t.Fatalf("%s %s, want POST /v1/approvals/apr_1/deny", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"reason":"not now"`) {
		t.Fatalf("body %q does not carry reason", gotBody)
	}
	if out.Decision != "denied" {
		t.Fatalf("decision = %q, want denied", out.Decision)
	}
}

// TestApprovalIDIsPathEscaped guards the tree's recorded path-handling defect family: an id
// containing a traversal segment must not be spliced unescaped into the URL path. A bare
// strings.Contains(path, "..") is itself a defeated comparison here — "." is unreserved and stays
// literal even when correctly escaped, so that check would fail on a CORRECTLY escaped path too.
// What actually matters is whether the id's "/" landed inside one route segment: split on "/" and
// assert the exact segment count and values, so an unescaped id (which inserts extra segments) is
// caught structurally.
func TestApprovalIDIsPathEscaped(t *testing.T) {
	var gotPath string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.EscapedPath()
		return jsonResp(200, `{"id":"x","object":"approval.decision","decision":"denied"}`), nil
	})
	c := testClient(t, rt)

	_, _ = c.Approvals.Deny(context.Background(), "apr/../../secret", DecisionParams{RequestHash: "h1"})
	got := strings.Split(gotPath, "/")
	want := []string{"", "v1", "approvals", "apr%2F..%2F..%2Fsecret", "deny"}
	if len(got) != len(want) {
		t.Fatalf("path %q has %d segments, want %d (%q) — the id's \"/\" was not escaped into one segment", gotPath, len(got), len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("path %q segment %d = %q, want %q", gotPath, i, got[i], want[i])
		}
	}
}

// TestApproveRetriesNetworkFailure locks Approve/Deny as safe to retry after a torn connection: the
// server's decision throat treats a repeat decision as a 409 conflict rather than a second apply, so
// unlike Sessions.Create this is marked idempotent for the transport's own retry.
func TestApproveRetriesNetworkFailure(t *testing.T) {
	var mu sync.Mutex
	attempt := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		attempt++
		n := attempt
		mu.Unlock()
		if n == 1 {
			return nil, errors.New("network unreachable")
		}
		return jsonResp(200, `{"id":"apr_1","object":"approval.decision","decision":"approved"}`), nil
	})
	c := testClient(t, rt)

	if _, err := c.Approvals.Approve(context.Background(), "apr_1", DecisionParams{RequestHash: "h1"}); err != nil {
		t.Fatalf("Approvals.Approve: %v", err)
	}
	if attempt != 2 {
		t.Fatalf("want one network failure then a retried success (2 attempts), got %d", attempt)
	}
}

func TestListApprovalsBuildsOnlyServerHonoredQuery(t *testing.T) {
	var gotPath string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.RequestURI()
		return jsonResp(200, `{"data":[{"id":"apr_1","object":"approval","kind":"tool","tool_call_id":"tc_1",`+
			`"run_id":"run_1","session_id":"sess_1","request_hash":"h1","created_at":"2026-08-03T00:00:00Z",`+
			`"identity":"repo.write","operator_label":"Write to repo","arguments":"{}","truncated":false}],`+
			`"has_more":false}`), nil
	})
	c := testClient(t, rt)

	page, err := c.Approvals.List(context.Background(), ListApprovalsParams{
		Limit: 10, After: "cur_1", CreatedAfter: "2026-08-01T00:00:00Z", CreatedBefore: "2026-08-03T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("Approvals.List: %v", err)
	}
	if gotPath != "/v1/approvals?after=cur_1&limit=10&created_after=2026-08-01T00%3A00%3A00Z&created_before=2026-08-03T00%3A00%3A00Z" {
		t.Fatalf("query = %q, unexpected shape", gotPath)
	}
	if len(page.Data) != 1 {
		t.Fatalf("data len = %d, want 1", len(page.Data))
	}
	got := page.Data[0]
	if got.ID != "apr_1" || got.Kind != "tool" || got.ToolCallID != "tc_1" || got.SessionID != "sess_1" {
		t.Fatalf("approval = %+v, want id=apr_1 kind=tool tool_call_id=tc_1 session_id=sess_1", got)
	}
	if page.HasMore {
		t.Fatalf("has_more = true, want false")
	}
}

func TestListApprovalsWithNoParamsHitsBarePath(t *testing.T) {
	var gotPath string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.RequestURI()
		return jsonResp(200, `{"data":[],"has_more":false}`), nil
	})
	c := testClient(t, rt)

	if _, err := c.Approvals.List(context.Background(), ListApprovalsParams{}); err != nil {
		t.Fatalf("Approvals.List: %v", err)
	}
	if gotPath != "/v1/approvals" {
		t.Fatalf("path = %q, want /v1/approvals with no query", gotPath)
	}
}
