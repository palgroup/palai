package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// bodyCapturingAdmitter records the projection the handler asked to persist. It resolves a real
// organization on purpose: the value the create path used to store is still reachable, so what the
// test observes is the handler declining to write it, not a fixture with nothing to write.
type bodyCapturingAdmitter struct{ body []byte }

func (a *bodyCapturingAdmitter) AdmitResponse(_ context.Context, req AdmitRequest) (AdmitResult, error) {
	a.body = append([]byte(nil), req.Body...)
	return AdmitResult{ResponseID: req.ResponseID, Body: req.Body}, nil
}
func (a *bodyCapturingAdmitter) GetResponse(context.Context, middleware.Scope, string) (RetrieveResult, error) {
	return RetrieveResult{}, nil
}
func (a *bodyCapturingAdmitter) CancelResponse(context.Context, middleware.Scope, string) (RetrieveResult, error) {
	return RetrieveResult{}, nil
}
func (a *bodyCapturingAdmitter) ListResponses(context.Context, middleware.Scope, ListQuery) ([]ListRow, error) {
	return nil, nil
}
func (a *bodyCapturingAdmitter) ResolveOrganization(context.Context, string) (string, error) {
	return "org_1", nil
}

// TestTheStoredProjectionDropsOrganizationAndStillReadsTheOnesThatCarriedIt pins the SHAPE this server
// persists, which is a different claim from the shape it renders.
//
// The "queued" projection a create request builds is marshalled once and used twice: returned to the
// caller, and written to idempotency_records.response_body, where a replay returns it verbatim months
// later (storage/queries/responses.sql, ReserveIdempotency / GetIdempotency). So removing
// organization_id from it is a change to DURABLE data, and the rows written before the change keep the
// field forever — nothing rewrites them, and the retention sweep only NULLs the body wholesale.
//
// Both directions are asserted because only one of them is about today's code. That a new body omits
// the field is a property of this package. That an OLD body still decodes is a property of
// encoding/json and the contract's tag, and it is the one a reader would otherwise take on faith:
// `omitempty` governs WRITING, and it is `json.Unmarshal` ignoring an absent field that makes the read
// safe. They are different mechanisms with similar-looking spellings, so the test drives both rather
// than reasoning from the struct tag.
func TestTheStoredProjectionDropsOrganizationAndStillReadsTheOnesThatCarriedIt(t *testing.T) {
	// 1. What this server writes now — captured off the REAL handler, not rebuilt here. A test that
	// marshals its own contracts.Response would assert about a struct literal in this file: put the
	// field back in responses.go and it would still pass. What is stored is whatever createResponse
	// puts in AdmitRequest.Body, so that is what this reads. Its admitter deliberately still resolves
	// an organization (scriptedAdmitter returns "org_1"), so the value is AVAILABLE to be written and
	// the assertion below measures a choice rather than an absence of data.
	capture := &bodyCapturingAdmitter{}
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, capture,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	defer srv.Close()
	resp := postResponse(t, srv.URL)
	defer resp.Body.Close()

	fresh := capture.body
	if len(fresh) == 0 {
		t.Fatal("the handler admitted nothing, so this test read no stored body at all — it is VACUOUS")
	}
	if strings.Contains(string(fresh), "organization_id") {
		t.Errorf("a newly stored response body still names organization_id: %s", fresh)
	}
	// Not a tautology via the same struct: the field this test cares about is gone while its NEIGHBOUR
	// on the wire is still there, so a projection that lost both would fail here rather than pass.
	if !strings.Contains(string(fresh), `"project_id":`) {
		t.Errorf("the stored body lost project_id too, which is the tenant key that REPLACED the "+
			"organization: %s", fresh)
	}

	// 2. What a row written before this change looks like, byte for byte as it sits in Postgres today.
	legacy := []byte(`{"id":"resp_0","object":"response","status":"queued","output":[],` +
		`"usage":{},"session_id":"ses_0","run_id":"run_0",` +
		`"organization_id":"org_0","project_id":"proj_0"}`)
	var replayed contracts.Response
	if err := json.Unmarshal(legacy, &replayed); err != nil {
		t.Fatalf("a body stored before A.2 Task 5 no longer decodes, so every replay of it fails: %v", err)
	}
	if replayed.ProjectID != "proj_0" || replayed.ID != "resp_0" {
		t.Errorf("legacy body decoded to the wrong resource: id=%q project=%q", replayed.ID, replayed.ProjectID)
	}

	// 3. And the absence itself decodes, which is what every body written from now on will look like.
	// contracts.Response no longer DECLARES an organization_id (A.2 Task 6 removed it from the schema),
	// so the assertion moved from "the field reads as the zero value" to "an old body still decodes and
	// the unknown key is dropped" — checked at step 2 above, on the byte-for-byte legacy body.
	var current contracts.Response
	if err := json.Unmarshal(fresh, &current); err != nil {
		t.Fatalf("decode a body written without organization_id: %v", err)
	}
}
