package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// decodeResponse decodes a 202 body into the canonical Response contract.
func decodeResponse(t *testing.T, body []byte) contracts.Response {
	t.Helper()
	var r contracts.Response
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	return r
}

func TestResponseAcceptedReturns202WithLocation(t *testing.T) {
	srv := newTestServer(t)

	resp := postResponses(t, srv, authedHeaders("key-accept"), `{"input":"investigate the issue"}`)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	r := decodeResponse(t, body)
	if !r.ID.Valid() {
		t.Fatalf("response id %q is not a canonical response_id", r.ID)
	}
	if r.Object != "response" {
		t.Fatalf("object = %q, want response", r.Object)
	}
	if r.Status != "queued" {
		t.Fatalf("status = %q, want queued", r.Status)
	}

	// The resource is bound to the verified tenant, not to anything the client can
	// choose: the response carries the authenticated org/project (spec §39.2).
	if r.OrganizationID != contracts.OrganizationID(testScope.Organization) {
		t.Fatalf("organization_id = %q, want verified %q", r.OrganizationID, testScope.Organization)
	}
	if r.ProjectID != contracts.ProjectID(testScope.Project) {
		t.Fatalf("project_id = %q, want verified %q", r.ProjectID, testScope.Project)
	}

	// Location points at the canonical resource for the created response.
	if got, want := resp.Header.Get("Location"), "/v1/responses/"+string(r.ID); got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

func TestResponseIgnoresBodyScopeOverride(t *testing.T) {
	srv := newTestServer(t)

	// The body smuggles a foreign project/organization as raw JSON so the unknown
	// fields actually travel over the wire. Scope must come from the verified key,
	// never the body — otherwise a caller could write into another tenant (spec §39.2).
	body := `{"input":"x","project_id":"prj_evil","organization_id":"org_evil"}`
	r := decodeResponse(t, readBody(t, postResponses(t, srv, authedHeaders("key-scope"), body)))

	if r.ProjectID != contracts.ProjectID(testScope.Project) {
		t.Fatalf("project_id = %q, want verified %q (body override leaked)", r.ProjectID, testScope.Project)
	}
	if r.OrganizationID != contracts.OrganizationID(testScope.Organization) {
		t.Fatalf("organization_id = %q, want verified %q (body override leaked)", r.OrganizationID, testScope.Organization)
	}
	if string(r.ProjectID) == "prj_evil" || string(r.OrganizationID) == "org_evil" {
		t.Fatalf("injected scope leaked into the response: %+v", r)
	}
}

func TestResponseCarriesRequestAndVersionHeaders(t *testing.T) {
	srv := newTestServer(t)

	resp := postResponses(t, srv, authedHeaders("key-headers"), `{"input":"hello"}`)
	defer resp.Body.Close()

	reqID := resp.Header.Get("Request-Id")
	if !contracts.RequestID(reqID).Valid() {
		t.Fatalf("Request-Id = %q, want a canonical request_id", reqID)
	}
	if got := resp.Header.Get("API-Version"); got != middleware.APIVersion {
		t.Fatalf("API-Version = %q, want %q", got, middleware.APIVersion)
	}
}

func TestResponseReplaySameKeySameBodyReturnsSameResource(t *testing.T) {
	srv := newTestServer(t)
	headers := authedHeaders("key-replay")
	const body = `{"input":"do the work once"}`

	first := decodeResponse(t, readBody(t, postResponses(t, srv, headers, body)))
	resp := postResponses(t, srv, headers, body)
	second := decodeResponse(t, readBody(t, resp))

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("replay status = %d, want 202", resp.StatusCode)
	}
	// A duplicate create returns the original resource: one response, one run.
	if first.ID != second.ID {
		t.Fatalf("replay response id = %q, want original %q", second.ID, first.ID)
	}
	if first.RunID != second.RunID {
		t.Fatalf("replay run id = %q, want original %q", second.RunID, first.RunID)
	}
}

func TestRetrieveReturnsTerminalProjection(t *testing.T) {
	srv := newTestServer(t)
	headers := authedHeaders("key-retrieve")

	created := decodeResponse(t, readBody(t, postResponses(t, srv, headers, `{"input":"do the work"}`)))

	resp := getResponse(t, srv, string(created.ID))
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retrieve status = %d, want 200 (body=%s)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	// The retrieved projection is the committed resource, field-for-field: same id,
	// same status, same output and usage as the admission returned.
	got := decodeResponse(t, body)
	if got.ID != created.ID {
		t.Fatalf("retrieved id = %q, want %q", got.ID, created.ID)
	}
	if got.Status != created.Status {
		t.Fatalf("retrieved status = %q, want %q", got.Status, created.Status)
	}
	if len(got.Output) != len(created.Output) {
		t.Fatalf("retrieved output len = %d, want %d", len(got.Output), len(created.Output))
	}
	if got.Usage.TotalTokens != created.Usage.TotalTokens || got.Usage.InputTokens != created.Usage.InputTokens {
		t.Fatalf("retrieved usage = %+v, want %+v", got.Usage, created.Usage)
	}
}

func TestRetrieveUnknownIDReturns404(t *testing.T) {
	srv := newTestServer(t)

	resp := getResponse(t, srv, "resp_does_not_exist")
	problem := decodeProblem(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("retrieve status = %d, want 404", resp.StatusCode)
	}
	if problem.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", problem.Code)
	}
}

func TestCancelUnknownIDReturns404(t *testing.T) {
	srv := newTestServer(t)

	resp := cancelResponse(t, srv, "resp_does_not_exist")
	problem := decodeProblem(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cancel status = %d, want 404", resp.StatusCode)
	}
	if problem.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", problem.Code)
	}
}

func TestCancelRequiresAuth(t *testing.T) {
	srv := newTestServer(t)

	// No Authorization header: cancel is an authenticated, tenant-scoped mutation, so an
	// unauthenticated request never reaches the handler.
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses/resp_x/cancel", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	problem := decodeProblem(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cancel status = %d, want 401", resp.StatusCode)
	}
	if problem.Code != "authentication_required" {
		t.Fatalf("code = %q, want authentication_required", problem.Code)
	}
}

func TestResponseConflictSameKeyDifferentBodyReturns409(t *testing.T) {
	srv := newTestServer(t)
	headers := authedHeaders("key-conflict")

	first := postResponses(t, srv, headers, `{"input":"first request"}`)
	first.Body.Close()
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", first.StatusCode)
	}

	resp := postResponses(t, srv, headers, `{"input":"a different request"}`)
	problem := decodeProblem(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflict status = %d, want 409", resp.StatusCode)
	}
	if problem.Code != "idempotency_mismatch" {
		t.Fatalf("code = %q, want idempotency_mismatch", problem.Code)
	}
}
