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

	// Location points at the canonical resource for the created response.
	if got, want := resp.Header.Get("Location"), "/v1/responses/"+string(r.ID); got != want {
		t.Fatalf("Location = %q, want %q", got, want)
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
