package providerone

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// THE MODELS LIST, provider-one / openai-compatible (E29 provider models).
//
// The probe already GETs this exact URL and deliberately throws the body away. These tests pin the half
// that reads it, and every one of them exists because an operator picking a model from a screen must never
// be shown a list that is a guess: an empty picker that means "we could not ask" is the defect this whole
// surface is built to avoid.

// listedIDs is a small helper so a failure prints the ids rather than a struct dump.
func listedIDs(l modelbroker.ModelListing) []string {
	out := make([]string, 0, len(l.Models))
	for _, m := range l.Models {
		out = append(out, m.ID)
	}
	return out
}

// The happy path, against the wire shape api.openai.com actually serves — MEASURED 2026-08-01:
//
//	curl -s https://api.openai.com/v1/models -H "Authorization: Bearer $K" | python3 -c '…'
//	  -> keys: ['object', 'data'];  n= 133
//	  -> {"id": "gpt-4o-mini", "object": "model", "created": 1721172741, "owned_by": "system"}
//
// There is NO display name and NO pagination in that shape, and both absences are asserted rather than
// assumed: a lister that invented a label would be putting words in the provider's mouth.
func TestListModelsReadsTheProvidersOwnList(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"gpt-4o-mini","object":"model","created":1721172741,"owned_by":"system"},
			{"id":"gpt-5.2","object":"model","created":1765411200,"owned_by":"system"}]}`))
	}))
	defer srv.Close()

	a := Adapter{}
	got := a.ListModels(context.Background(), srv.URL+"/v1/chat/completions", "sk-the-operators-key")

	if gotMethod != http.MethodGet || gotPath != "/v1/models" {
		t.Fatalf("the lister sent %s %q, want GET of the endpoint's models list", gotMethod, gotPath)
	}
	if gotAuth != "Bearer sk-the-operators-key" {
		t.Fatalf("Authorization = %q — the lister did not send the connection's own credential", gotAuth)
	}
	if got.Outcome != modelbroker.ProbeAccepted {
		t.Fatalf("outcome = %q (detail %q), want %q", got.Outcome, got.Detail, modelbroker.ProbeAccepted)
	}
	if ids := listedIDs(got); len(ids) != 2 || ids[0] != "gpt-4o-mini" || ids[1] != "gpt-5.2" {
		t.Fatalf("models = %v, want the provider's own two ids IN THE PROVIDER'S ORDER", ids)
	}
	// The provider names no label, so neither does this. An invented one ("GPT 4o Mini") would be a
	// display string no operator could match against OpenAI's own documentation.
	if got.Models[0].DisplayName != "" {
		t.Fatalf("display name = %q, want empty — this family's list carries none and inventing one is a claim",
			got.Models[0].DisplayName)
	}
	if got.Models[0].CreatedAt.IsZero() || got.Models[0].CreatedAt.Unix() != 1721172741 {
		t.Fatalf("created_at = %v, want the provider's own unix `created` decoded", got.Models[0].CreatedAt)
	}
	// One page is the whole list for this family — nothing was left behind.
	if !got.Complete {
		t.Fatal("complete = false on a family whose list is not paginated")
	}
	// THE CREDENTIAL IS IN NOTHING THE LISTING RETURNS.
	rendered := got.Endpoint + "\x00" + got.Detail + "\x00" + strings.Join(listedIDs(got), ",")
	if strings.Contains(rendered, "sk-the-operators-key") {
		t.Fatal("the listing's own result carries the credential")
	}
}

// EVERY WAY THIS CAN FAIL IS ITS OWN ANSWER, and none of them is an empty list. This is the whole point of
// the surface: a screen that renders `data: []` cannot tell "your key sees no models" from "we never
// asked", and the second is the one that sends an operator hunting the wrong thing.
func TestListModelsDistinguishesEveryFailure(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    modelbroker.ProbeOutcome
		wantSts int
	}{
		// A gateway that serves completions but no models list. probe.go:123 already records that these
		// exist; the listing must NOT read that as "no models".
		{"gateway 404s its models list", http.StatusNotFound, `{"error":"nope"}`, modelbroker.ProbeUnsupported, http.StatusNotFound},
		{"gateway 400s its models list", http.StatusBadRequest, `{"error":"nope"}`, modelbroker.ProbeUnsupported, http.StatusBadRequest},
		{"the key is wrong or expired", http.StatusUnauthorized, `{}`, modelbroker.ProbeRejected, http.StatusUnauthorized},
		{"the key is forbidden here", http.StatusForbidden, `{}`, modelbroker.ProbeRejected, http.StatusForbidden},
		{"rate limited", http.StatusTooManyRequests, `{}`, modelbroker.ProbeTransient, http.StatusTooManyRequests},
		{"upstream is broken", http.StatusBadGateway, `{}`, modelbroker.ProbeTransient, http.StatusBadGateway},
		// A 200 whose body is not a models list. The credential WAS accepted and there is still no list,
		// and calling that "accepted with zero models" would be the false green in its purest form.
		{"200 that is not a models list", http.StatusOK, `<html>hello</html>`, modelbroker.ProbeUnsupported, http.StatusOK},
		{"200 whose data is not an array", http.StatusOK, `{"object":"list","data":"soon"}`, modelbroker.ProbeUnsupported, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			got := Adapter{}.ListModels(context.Background(), srv.URL+"/v1/chat/completions", "sk-x")
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q (detail %q)", got.Outcome, tc.want, got.Detail)
			}
			if got.Status != tc.wantSts {
				t.Fatalf("status = %d, want %d — the caller loses the upstream's own answer", got.Status, tc.wantSts)
			}
			if len(got.Models) != 0 {
				t.Fatalf("models = %v on a listing that reached no list", listedIDs(got))
			}
			if got.Detail == "" {
				t.Fatal("the failure names no reason, so an operator cannot tell what to fix")
			}
		})
	}
}

// No HTTP answer at all is its own outcome, distinct from every status above.
func TestListModelsReportsAnUnreachableEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + "/v1/chat/completions"
	srv.Close() // nothing is listening now

	got := Adapter{}.ListModels(context.Background(), url, "sk-x")
	if got.Outcome != modelbroker.ProbeUnreachable {
		t.Fatalf("outcome = %q, want %q", got.Outcome, modelbroker.ProbeUnreachable)
	}
	if got.Status != 0 {
		t.Fatalf("status = %d on a request that got no HTTP answer", got.Status)
	}
}

// An endpoint whose shape yields no models list: the lister DECLINES rather than guessing at a URL, and
// nothing leaves the process. Same rule the probe already holds — one derivation, so the two cannot drift.
func TestListModelsDeclinesAnEndpointItCannotDeriveAListFrom(t *testing.T) {
	dialled := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { dialled = true }))
	defer srv.Close()

	got := Adapter{}.ListModels(context.Background(), srv.URL+"/completions", "sk-x")
	if got.Outcome != modelbroker.ProbeUnsupported {
		t.Fatalf("outcome = %q, want %q", got.Outcome, modelbroker.ProbeUnsupported)
	}
	if dialled {
		t.Fatal("the lister dialled an endpoint it could not derive a models list from")
	}
}

// A body the caller did not bound is a body an upstream chooses the size of. The read is capped, and a
// truncated parse is a REFUSAL rather than a short list — a half-decoded array would be the most
// convincing wrong answer this surface could give.
func TestListModelsBoundsTheBodyItReads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[`))
		entry := []byte(`{"id":"m","object":"model","created":1},`)
		for written := 0; written < maxListBytes+len(entry); written += len(entry) {
			if _, err := w.Write(entry); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte(`{"id":"last","object":"model","created":1}]}`))
	}))
	defer srv.Close()

	got := Adapter{}.ListModels(context.Background(), srv.URL+"/v1/chat/completions", "sk-x")
	if got.Outcome != modelbroker.ProbeUnsupported {
		t.Fatalf("outcome = %q, want %q — an oversized body must refuse, never truncate into a short list",
			got.Outcome, modelbroker.ProbeUnsupported)
	}
	if len(got.Models) != 0 {
		t.Fatalf("models = %d entries from a body that was cut off mid-array", len(got.Models))
	}
}

// An entry with no id is DROPPED rather than listed as "". A blank row in a model picker is a row an
// operator can select, and selecting it writes a route that dies at the first run.
func TestListModelsDropsAnEntryWithNoID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"","object":"model"},{"id":"gpt-4o-mini"}]}`))
	}))
	defer srv.Close()

	got := Adapter{}.ListModels(context.Background(), srv.URL+"/v1/chat/completions", "sk-x")
	if ids := listedIDs(got); len(ids) != 1 || ids[0] != "gpt-4o-mini" {
		t.Fatalf("models = %v, want the nameless entry dropped", ids)
	}
}
