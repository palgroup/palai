package store

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// THE CONNECTION'S MODELS LIST (E29 provider models). The list hangs off a CONNECTION because the
// CREDENTIAL is what the provider answers for — see ListConnectionModels for the argument.

// A real request, carrying the connection's real credential, to the connection's own endpoint. The
// inspector wired here is the PRODUCTION one, so what is measured is the shipped behaviour.
func TestListConnectionModelsMakesARealRequestWithTheConnectionsOwnCredential(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"llama-3.3-70b","object":"model","created":1730000000}]}`))
	}))
	defer srv.Close()

	rec := coordinator.ModelConnectionRecord{
		ID: "mconn_1", Provider: "openai-compatible", SecretRef: "vllm",
		BaseURL: srv.URL + "/v1/chat/completions",
	}
	s := (&Store{}).WithModelConnectionInspectors(
		map[string]ConnectionInspector{"openai-compatible": providerone.Adapter{}},
		func(org, ref string) ([]byte, bool, error) { return []byte("sk-the-operators-key"), true, nil },
	)

	got := s.listConnectionModels(context.Background(), coordinator.Tenant{Organization: "org_1", Project: "prj_1"}, rec)

	if gotPath != "/v1/models" {
		t.Fatalf("the lister hit %q, want the connection endpoint's models list", gotPath)
	}
	if gotAuth != "Bearer sk-the-operators-key" {
		t.Fatalf("Authorization = %q — the list was not fetched with THIS connection's credential", gotAuth)
	}
	if got.Outcome != modelbroker.ProbeAccepted || len(got.Models) != 1 || got.Models[0].ID != "llama-3.3-70b" {
		t.Fatalf("listing = %+v, want the endpoint's own single model", got)
	}
}

// Every path that reached NO list answers `not_probed`, never an empty list. This is the same rule the
// verify surface holds and it matters MORE here: `not_probed` at least reads as an absence, while an empty
// `data` array reads as a measurement — "your credential can see no models" — which is a sentence nobody
// measured.
func TestListConnectionModelsNeverAnswersAnEmptyListForAQuestionItDidNotAsk(t *testing.T) {
	tenant := coordinator.Tenant{Organization: "org_1", Project: "prj_1"}
	rec := coordinator.ModelConnectionRecord{ID: "mconn_1", Provider: "provider-one", SecretRef: "k"}
	resolves := func(org, ref string) ([]byte, bool, error) { return []byte("sk-x"), true, nil }

	cases := map[string]*Store{
		"no inspector wired for the family": (&Store{}).WithModelConnectionInspectors(map[string]ConnectionInspector{}, resolves),
		"no secret store wired": (&Store{}).WithModelConnectionInspectors(
			map[string]ConnectionInspector{"provider-one": providerone.Adapter{}}, nil),
		"the secret ref does not resolve": (&Store{}).WithModelConnectionInspectors(
			map[string]ConnectionInspector{"provider-one": providerone.Adapter{}},
			func(org, ref string) ([]byte, bool, error) { return nil, false, nil }),
	}
	for name, s := range cases {
		got := s.listConnectionModels(context.Background(), tenant, rec)
		if got.Outcome != modelbroker.ProbeUnsupported {
			t.Errorf("%s: outcome = %q, want %q", name, got.Outcome, modelbroker.ProbeUnsupported)
		}
		if len(got.Models) != 0 {
			t.Errorf("%s: %d models came back from a call that asked nothing", name, len(got.Models))
		}
		if got.Detail == "" {
			t.Errorf("%s: the refusal names no reason, so an operator cannot tell what to fix", name)
		}
	}
}

// THE PROJECTION IS WHERE THE RULE BECOMES VISIBLE TO A SCREEN, so it is asserted on the rendered bytes
// rather than on the struct: `data` is present ONLY when the provider actually answered with a list.
//
// A console that renders `data` without branching on `outcome` first will therefore crash or render
// nothing, which is the correct failure. The wrong one — the one this pins against — is a tidy empty
// table under a heading that says "Models".
func TestTheModelListingProjectionOmitsDataUnlessTheProviderAnswered(t *testing.T) {
	rec := coordinator.ModelConnectionRecord{ID: "mconn_1", Provider: "provider-two"}
	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	t.Run("a refusal carries no data key at all", func(t *testing.T) {
		raw := modelListingView(rec, modelbroker.ModelListing{
			Outcome: modelbroker.ProbeRejected, Status: 401, Endpoint: "https://api.anthropic.com/v1/models",
			Detail: "the endpoint REJECTED this credential",
		}, at)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("projection is not JSON: %v", err)
		}
		if _, present := out["data"]; present {
			t.Fatalf("a rejected credential rendered a `data` key: %s", raw)
		}
		if out["outcome"] != string(modelbroker.ProbeRejected) {
			t.Fatalf("outcome = %v, want the probe's own vocabulary", out["outcome"])
		}
		if out["detail"] == "" || out["detail"] == nil {
			t.Fatal("the refusal renders no reason for the operator to read")
		}
	})

	t.Run("an answered list carries data, even when the answer is none", func(t *testing.T) {
		raw := modelListingView(rec, modelbroker.ModelListing{
			Outcome: modelbroker.ProbeAccepted, Status: 200, Complete: true,
			Endpoint: "https://api.anthropic.com/v1/models", Detail: "accepted",
		}, at)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("projection is not JSON: %v", err)
		}
		// THIS empty array is a measurement: the provider was asked and named no models. It is the one
		// case where `data: []` is the truth, and it is only reachable because every other case omits it.
		data, present := out["data"]
		if !present {
			t.Fatalf("an accepted listing omitted `data`: %s", raw)
		}
		if got, ok := data.([]any); !ok || len(got) != 0 {
			t.Fatalf("data = %v, want an empty array", data)
		}
		if out["fetched_at"] != at.Format(time.RFC3339Nano) {
			t.Fatalf("fetched_at = %v, want the moment the list was fetched — a list with no age is a list "+
				"an operator cannot tell is stale", out["fetched_at"])
		}
		if out["listed"] != modelbroker.ListedScope {
			t.Fatalf("listed = %v, want the scope sentence: a picker must not present a provider's whole "+
				"catalogue as though it were this family's chat models", out["listed"])
		}
	})

	t.Run("a model renders the provider's own id and label", func(t *testing.T) {
		raw := modelListingView(rec, modelbroker.ModelListing{
			Outcome: modelbroker.ProbeAccepted, Status: 200, Complete: true,
			Models: []modelbroker.ModelInfo{
				{ID: "claude-opus-5", DisplayName: "Claude Opus 5", CreatedAt: at},
				{ID: "gpt-4o-mini"}, // the family that names no label
			},
		}, at)
		var out struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("projection is not JSON: %v", err)
		}
		if len(out.Data) != 2 {
			t.Fatalf("data = %d entries, want 2", len(out.Data))
		}
		if out.Data[0]["id"] != "claude-opus-5" || out.Data[0]["display_name"] != "Claude Opus 5" {
			t.Fatalf("entry = %v, want the provider's own id and label", out.Data[0])
		}
		if out.Data[0]["object"] != "model" {
			t.Fatalf("object = %v, want \"model\"", out.Data[0]["object"])
		}
		// The provider named no label, so the key is ABSENT rather than "". A console that reads
		// `display_name || id` gets the id; a console that renders `display_name` gets nothing to render,
		// which is visibly wrong rather than quietly wrong.
		if _, present := out.Data[1]["display_name"]; present {
			t.Fatalf("an unlabelled model rendered a display_name key: %v", out.Data[1])
		}
	})

	t.Run("an incomplete list says so", func(t *testing.T) {
		raw := modelListingView(rec, modelbroker.ModelListing{
			Outcome: modelbroker.ProbeAccepted, Status: 200, Complete: false,
			Models: []modelbroker.ModelInfo{{ID: "a"}},
		}, at)
		if !strings.Contains(string(raw), `"complete":false`) {
			t.Fatalf("a truncated list did not say it was truncated: %s", raw)
		}
	})
}

// The read is per-project like every other model-routing read: an ORG-granular provision key is a 400
// naming what is missing, before any query and before anything leaves the process.
func TestListConnectionModelsRejectsAnOrgGranularKey(t *testing.T) {
	s := &Store{}
	out, err := s.ListConnectionModels(context.Background(), middleware.Scope{Organization: "org_1"}, "mconn_1")
	if err != nil || out.MissingField == "" {
		t.Fatalf("ListConnectionModels(org-granular key) = (%+v, %v), want a MissingField reject (400)", out, err)
	}
	var _ api.ProvisionResult = out
}
