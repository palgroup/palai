package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// The Docker-free half of the E23 T9 approval surface: what the EDGE refuses before anything reaches a
// store call. The end-to-end proof (a real park, a real key, a real wake) is the component tier's
// TestHTTPToolApproval* against a real spine; these are the claims that need no database and would
// otherwise never run in `make verify`.

// fakeApprovalAPI records what reached the seam. `decided` is the load-bearing field: several tests below
// assert that a refused request produced NO store call at all, which is a different claim from "the store
// was called and said no".
type fakeApprovalAPI struct {
	items   []PendingApproval
	decided *ApprovalDecision
	scope   middleware.Scope
	id      string
	out     ApprovalOutcome
	query   ListQuery
}

func (f *fakeApprovalAPI) ListPendingApprovals(_ context.Context, scope middleware.Scope, q ListQuery) ([]PendingApproval, error) {
	f.scope, f.query = scope, q
	return f.items, nil
}

func (f *fakeApprovalAPI) DecideApproval(_ context.Context, scope middleware.Scope, id string, d ApprovalDecision) (ApprovalOutcome, error) {
	f.scope, f.id = scope, id
	f.decided = &d
	return f.out, nil
}

func approvalTestServer(t *testing.T, s ApprovalAPI) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil,
		WithApprovals(s)))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestApprovalSurfaceIsUnmountedWithoutTheOption pins the §2 posture the rest of this router follows: the
// routes are derived from an actual mount, so a tier that wires no approval store 404s rather than 500ing
// on a nil seam.
func TestApprovalSurfaceIsUnmountedWithoutTheOption(t *testing.T) {
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/approvals"},
		{"POST", "/v1/approvals/apr_1/approve"},
		{"POST", "/v1/approvals/apr_1/deny"},
	} {
		resp := do(t, tc.method, srv.URL+tc.path, "", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s on a router with no approval store = %d, want 404", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// TestApprovalDecisionRequiresTheRequestHash is the structural half of the one-shot binding. The Slack
// button carries tool_calls.request_hash so that arguments changed after the approval leave no approval,
// and DecideToolApproval SKIPS the comparison when the field is empty — so a decision keyed only by an
// approval id would make this the one surface where the binding does not hold.
//
// It has to be refused HERE, before the store call, which is why the assertion is that nothing reached the
// seam rather than that the seam returned a refusal.
func TestApprovalDecisionRequiresTheRequestHash(t *testing.T) {
	for _, body := range []string{"", "{}", `{"reason":"looks fine"}`} {
		fake := &fakeApprovalAPI{out: ApprovalOutcome{Found: true, Applied: true}}
		base := approvalTestServer(t, fake)
		resp := do(t, "POST", base+"/v1/approvals/apr_1/approve", body, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("approve with body %q = %d, want 400 — an approval id alone authorizes nothing", body, resp.StatusCode)
		}
		if fake.decided != nil {
			t.Fatalf("a hash-less decision reached the store seam: %+v", fake.decided)
		}
	}
}

// TestApprovalDecisionBodyCannotNameItsOwnApprover is the trust boundary. The deciding principal is
// stamped from the VERIFIED key inside the store adapter; the body has no say, and the strict decode makes
// that structural rather than a promise — an attempt to name one is a 400, not a silently ignored field.
//
// The control is the last case: the same body without the extra field is ACCEPTED, so "everything is
// refused" cannot satisfy this test.
func TestApprovalDecisionBodyCannotNameItsOwnApprover(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"an approver field", `{"request_hash":"h1","approver":"key_the_real_approver"}`, http.StatusBadRequest},
		{"a decided_by field", `{"request_hash":"h1","decided_by":"slack:T1:Uboss"}`, http.StatusBadRequest},
		{"a tenant field", `{"request_hash":"h1","organization_id":"org_victim"}`, http.StatusBadRequest},
		{"the control: hash only", `{"request_hash":"h1"}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeApprovalAPI{out: ApprovalOutcome{Found: true, Applied: true}}
			base := approvalTestServer(t, fake)
			resp := do(t, "POST", base+"/v1/approvals/apr_1/approve", tc.body, nil)
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusBadRequest && fake.decided != nil {
				t.Fatalf("a body naming an identity reached the store seam: %+v", fake.decided)
			}
		})
	}
}

// TestApprovalListPassesItsCursorToTheStore is the pin on a bug this surface shipped for exactly one
// draft: the handler minted a next_cursor from the over-fetch and then never passed one back down, so a
// client that followed the cursor got page 1 again — forever, with has_more still true. A paginated list
// whose cursor reaches nothing is worse than an unpaginated one.
func TestApprovalListPassesItsCursorToTheStore(t *testing.T) {
	first := PendingApproval{ID: "apr_1", CreatedAt: time.Unix(1700000000, 0).UTC()}
	second := PendingApproval{ID: "apr_2", CreatedAt: time.Unix(1700000100, 0).UTC()}
	fake := &fakeApprovalAPI{items: []PendingApproval{first, second}} // limit=1 + the over-fetch row
	base := approvalTestServer(t, fake)

	resp := do(t, "GET", base+"/v1/approvals?limit=1", "", nil)
	var page struct {
		HasMore    bool    `json:"has_more"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	resp.Body.Close()
	if fake.query.After != nil {
		t.Fatalf("the first page carried a cursor %+v, want none", fake.query.After)
	}
	if fake.query.Limit != 2 {
		t.Fatalf("the store was asked for %d rows on limit=1, want 2 (the over-fetch renderPage reads as has_more)", fake.query.Limit)
	}
	if !page.HasMore || page.NextCursor == nil {
		t.Fatalf("page has_more=%v cursor=%v, want a further page", page.HasMore, page.NextCursor)
	}

	resp = do(t, "GET", base+"/v1/approvals?limit=1&after="+url.QueryEscape(*page.NextCursor), "", nil)
	resp.Body.Close()
	if fake.query.After == nil {
		t.Fatal("the followed cursor reached the store as nil — the client would be handed page 1 forever")
	}
	if fake.query.After.ID != first.ID || !fake.query.After.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("the store resumed from %+v, want the last row of page 1 (%s, %s)", fake.query.After, first.ID, first.CreatedAt)
	}
}

// TestApprovalOutcomesMapToDistinguishableStatuses: an operator scripting this surface has to be able to
// tell "you may not decide this" from "this is no longer decidable" from "it landed". Collapsing the first
// two would hide a misconfigured approver list behind what looks like an expired button.
//
// A not-found is a 404 rather than a 403 for the reason the whole tree renders a foreign id that way: a
// 403 would confirm the approval exists in somebody else's tenant.
func TestApprovalOutcomesMapToDistinguishableStatuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  ApprovalOutcome
		want int
	}{
		{"unknown or foreign", ApprovalOutcome{}, http.StatusNotFound},
		{"not an approver", ApprovalOutcome{Found: true, Unauthorized: true}, http.StatusForbidden},
		{"void: stale hash, expired, or already decided", ApprovalOutcome{Found: true}, http.StatusConflict},
		{"applied", ApprovalOutcome{Found: true, Applied: true}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := approvalTestServer(t, &fakeApprovalAPI{out: tc.out})
			resp := do(t, "POST", base+"/v1/approvals/apr_1/deny", `{"request_hash":"h1","reason":"no"}`, nil)
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// TestApprovalRoutesCarryTheVerifiedScopeAndTheRoutesDecision: the tenant and the deciding identity come
// from the credential (§39.2), and which way the decision goes comes from the URL — /approve and /deny are
// different routes precisely so the body cannot flip one into the other.
func TestApprovalRoutesCarryTheVerifiedScopeAndTheRoutesDecision(t *testing.T) {
	for _, tc := range []struct {
		path        string
		wantApprove bool
	}{
		{"/v1/approvals/apr_7/approve", true},
		{"/v1/approvals/apr_7/deny", false},
	} {
		fake := &fakeApprovalAPI{out: ApprovalOutcome{Found: true, Applied: true}}
		base := approvalTestServer(t, fake)
		resp := do(t, "POST", base+tc.path, `{"request_hash":"h1"}`, nil)
		resp.Body.Close()
		if fake.decided == nil {
			t.Fatalf("%s reached no store call", tc.path)
		}
		if fake.decided.Approve != tc.wantApprove {
			t.Fatalf("%s decided approve=%v, want %v", tc.path, fake.decided.Approve, tc.wantApprove)
		}
		if fake.id != "apr_7" {
			t.Fatalf("%s decided approval %q, want apr_7", tc.path, fake.id)
		}
		if fake.scope.Project != "prj_1" {
			t.Fatalf("%s ran in project %s, want the verified scope's prj_1",
				tc.path, fake.scope.Project)
		}
	}
}
