package providertwo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// THE MODELS LIST, provider-two / Anthropic (E29 provider models).
//
// Anthropic's list differs from provider-one's in two ways that both reach the operator's screen, so both
// are pinned here rather than assumed from the sibling family:
//
//   - IT CARRIES A DISPLAY NAME. Measured 2026-08-01:
//     curl -s https://api.anthropic.com/v1/models -H "x-api-key: $K" -H 'anthropic-version: 2023-06-01'
//     -> {"data":[{"type":"model","id":"claude-opus-5","display_name":"Claude Opus 5",
//     "created_at":"2026-07-24T00:00:00Z"}, …], "has_more":false, "first_id":…, "last_id":…}
//     -> 11 models
//   - IT IS PAGINATED (has_more / last_id / ?after_id=). provider-one's is not. A lister that read only
//     the first page would silently shorten the picker, and would do it MORE as the provider ships more
//     models — the failure would arrive with success.

func listedIDs(l modelbroker.ModelListing) []string {
	out := make([]string, 0, len(l.Models))
	for _, m := range l.Models {
		out = append(out, m.ID)
	}
	return out
}

// The happy path, against the wire shape api.anthropic.com actually serves, and the auth headers this
// family actually uses — `x-api-key` plus the required version header, never a bearer.
func TestListModelsReadsAnthropicsOwnList(t *testing.T) {
	var gotKey, gotVersion, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotVersion = r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[
			{"type":"model","id":"claude-opus-5","display_name":"Claude Opus 5","created_at":"2026-07-24T00:00:00Z"},
			{"type":"model","id":"claude-sonnet-5","display_name":"Claude Sonnet 5","created_at":"2026-06-29T00:00:00Z"}],
			"has_more":false,"first_id":"claude-opus-5","last_id":"claude-sonnet-5"}`))
	}))
	defer srv.Close()

	got := Adapter{}.ListModels(context.Background(), srv.URL+"/v1/messages", "sk-ant-the-operators-key")

	if gotPath != "/v1/models" {
		t.Fatalf("the lister hit %q, want the endpoint's models list", gotPath)
	}
	if gotKey != "sk-ant-the-operators-key" || gotVersion != anthropicVersion {
		t.Fatalf("headers = x-api-key %q / anthropic-version %q, want the connection's credential and %q",
			gotKey, gotVersion, anthropicVersion)
	}
	// A bearer here would be the sibling family's header on this family's endpoint — it would 401, and the
	// operator would be told their key is wrong when it is not.
	if gotAuth != "" {
		t.Fatalf("Authorization = %q — this family authenticates on x-api-key and must send nothing else", gotAuth)
	}
	if got.Outcome != modelbroker.ProbeAccepted {
		t.Fatalf("outcome = %q (detail %q), want %q", got.Outcome, got.Detail, modelbroker.ProbeAccepted)
	}
	if ids := listedIDs(got); len(ids) != 2 || ids[0] != "claude-opus-5" {
		t.Fatalf("models = %v, want the provider's own ids in its own order", ids)
	}
	// THE DISPLAY NAME IS THE PROVIDER'S, verbatim. It is the only reason a picker can show "Claude Opus 5"
	// without this tree typing that string anywhere.
	if got.Models[0].DisplayName != "Claude Opus 5" {
		t.Fatalf("display name = %q, want the provider's own label", got.Models[0].DisplayName)
	}
	if got.Models[0].CreatedAt.IsZero() {
		t.Fatal("created_at is zero — the provider's own timestamp was dropped")
	}
	if !got.Complete {
		t.Fatal("complete = false on a list whose last page said has_more:false")
	}
	rendered := got.Endpoint + "\x00" + got.Detail
	if strings.Contains(rendered, "sk-ant-the-operators-key") {
		t.Fatal("the listing's own result carries the credential")
	}
}

// PAGINATION IS FOLLOWED. Anthropic returns has_more + last_id and expects ?after_id=. Eleven models fit
// in one default page today, which is exactly why this must be a test and not an observation: the day it
// stops fitting, a first-page-only lister shortens every picker and nothing says so.
func TestListModelsFollowsAnthropicsPagination(t *testing.T) {
	var afterIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after := r.URL.Query().Get("after_id")
		afterIDs = append(afterIDs, after)
		if after == "" {
			_, _ = w.Write([]byte(`{"data":[{"type":"model","id":"a","display_name":"A"}],
				"has_more":true,"first_id":"a","last_id":"a"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"type":"model","id":"b","display_name":"B"}],
			"has_more":false,"first_id":"b","last_id":"b"}`))
	}))
	defer srv.Close()

	got := Adapter{}.ListModels(context.Background(), srv.URL+"/v1/messages", "sk-x")

	if len(afterIDs) != 2 || afterIDs[0] != "" || afterIDs[1] != "a" {
		t.Fatalf("after_id sequence = %v, want the first page unqualified then ?after_id=a", afterIDs)
	}
	if ids := listedIDs(got); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("models = %v, want BOTH pages", ids)
	}
	if !got.Complete {
		t.Fatal("complete = false after the provider said has_more:false")
	}
}

// A provider that never stops saying has_more is BOUNDED, and the bound is VISIBLE. Complete=false is the
// difference between "these are the models" and "these are the first N", and a caller that cannot tell
// them apart will render the second as the first.
func TestListModelsBoundsAnEndlessPagination(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		id := "m" + strconv.Itoa(pages)
		_, _ = w.Write([]byte(`{"data":[{"type":"model","id":"` + id + `"}],"has_more":true,"last_id":"` + id + `"}`))
	}))
	defer srv.Close()

	got := Adapter{}.ListModels(context.Background(), srv.URL+"/v1/messages", "sk-x")

	if pages != maxListPages {
		t.Fatalf("the lister fetched %d pages, want the bound of %d", pages, maxListPages)
	}
	if got.Complete {
		t.Fatal("complete = true on a list the lister stopped reading — the caller cannot tell it is partial")
	}
	if got.Outcome != modelbroker.ProbeAccepted {
		t.Fatalf("outcome = %q, want %q: what WAS read is real, it is merely incomplete", got.Outcome, modelbroker.ProbeAccepted)
	}
	if len(got.Models) != maxListPages {
		t.Fatalf("models = %d, want the %d that were actually read", len(got.Models), maxListPages)
	}
}

// A page that fails MID-PAGINATION discards the partial result rather than returning the pages it got as
// if they were the list. Half of Anthropic's catalogue rendered as all of it is a wrong answer that looks
// exactly like a right one.
func TestListModelsRefusesAPartialPaginationRatherThanShorteningIt(t *testing.T) {
	first := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first {
			first = false
			_, _ = w.Write([]byte(`{"data":[{"type":"model","id":"a"}],"has_more":true,"last_id":"a"}`))
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	got := Adapter{}.ListModels(context.Background(), srv.URL+"/v1/messages", "sk-x")
	if got.Outcome != modelbroker.ProbeTransient {
		t.Fatalf("outcome = %q, want %q — the second page's failure is the listing's failure", got.Outcome, modelbroker.ProbeTransient)
	}
	if len(got.Models) != 0 {
		t.Fatalf("models = %v — a partial pagination was returned as though it were the list", listedIDs(got))
	}
}

// The same failure vocabulary as the sibling family, because the two share ONE classifier. A family that
// answered 403 differently from its sibling would make an operator's mental model wrong at the worst
// moment.
func TestListModelsUsesTheSameFailureVocabularyAsProviderOne(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   modelbroker.ProbeOutcome
	}{
		{"the key is wrong or expired", http.StatusUnauthorized, modelbroker.ProbeRejected},
		{"rate limited", http.StatusTooManyRequests, modelbroker.ProbeTransient},
		{"no models list here", http.StatusNotFound, modelbroker.ProbeUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			got := Adapter{}.ListModels(context.Background(), srv.URL+"/v1/messages", "sk-x")
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", got.Outcome, tc.want)
			}
			if len(got.Models) != 0 {
				t.Fatalf("models = %v on a listing that reached no list", listedIDs(got))
			}
			// The endpoint travels into an API response, so it is the redacted form the sibling produces.
			if got.Endpoint != providerone.RedactURL(srv.URL+"/v1/models") {
				t.Fatalf("endpoint = %q, want the redacted models-list URL", got.Endpoint)
			}
		})
	}
}

// An endpoint this family cannot derive a list from: nothing leaves the process, and the answer says so.
func TestListModelsDeclinesAnEndpointItCannotDeriveAListFrom(t *testing.T) {
	dialled := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { dialled = true }))
	defer srv.Close()

	got := Adapter{}.ListModels(context.Background(), srv.URL+"/v1/chat/completions", "sk-x")
	if got.Outcome != modelbroker.ProbeUnsupported {
		t.Fatalf("outcome = %q, want %q", got.Outcome, modelbroker.ProbeUnsupported)
	}
	if dialled {
		t.Fatal("the lister dialled an endpoint it could not derive a models list from")
	}
}
