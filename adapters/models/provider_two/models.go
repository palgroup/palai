package providertwo

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// THE MODELS LIST, Anthropic (E29 provider models). Same request the credential probe already makes, with
// the body kept — see provider_one/models.go for why the two share one classifier and one body reader.
//
// TWO THINGS DIFFER FROM THE SIBLING FAMILY, and both were measured against api.anthropic.com on
// 2026-08-01 rather than inferred from documentation:
//
//   - THE LIST CARRIES A DISPLAY NAME. `{"id":"claude-opus-5","display_name":"Claude Opus 5"}`. That label
//     is the provider's, so a picker can render "Claude Opus 5" without this tree ever typing it.
//   - THE LIST IS PAGINATED: `has_more` + `last_id`, continued with `?after_id=`. OpenAI's is not. Eleven
//     models fit in one page today, which is precisely why this is code and not a comment — a
//     first-page-only lister would be correct until the provider shipped its twenty-first model, and then
//     it would quietly shorten every picker with nothing to say it had.

// pageLimit is the page size asked for. Anthropic's default is 20 and its maximum is 1000; asking for the
// maximum means the pagination below is exercised by a provider catalogue that has grown twentyfold rather
// than by an ordinary Tuesday.
const pageLimit = 1000

// maxListPages bounds the pagination. A provider that answers has_more forever — a broken gateway, or one
// whose cursor this adapter is holding wrong — must not be able to make one API request loop.
//
// STOPPING IS REPORTED, NEVER HIDDEN: the listing comes back Complete=false, because "the first 20000
// models" and "the models" are different answers and a caller that cannot tell them apart will render the
// second when it has the first.
const maxListPages = 20

// listTimeout bounds the WHOLE listing, every page included. The per-request bound lives in
// provider_one.FetchJSON; without this second one, twenty slow pages would hold an API request open for
// twenty times the per-request timeout.
const listTimeout = 30 * time.Second

// ListModels implements modelbroker.ModelLister. baseURL empty means this adapter's own endpoint.
func (a Adapter) ListModels(ctx context.Context, baseURL, secret string) modelbroker.ModelListing {
	if baseURL == "" {
		baseURL = a.baseURL()
	}
	endpoint, ok := modelsListURL(baseURL)
	if !ok {
		return modelbroker.ModelListing{
			Outcome: modelbroker.ProbeUnsupported, Endpoint: providerone.RedactURL(baseURL),
			Detail: "the endpoint does not end in " + messagesSuffix + ", so its models list could not be derived " +
				"and NOTHING was asked — this is not an empty list, it is no list",
		}
	}
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	var models []modelbroker.ModelInfo
	after := ""
	for page := 0; page < maxListPages; page++ {
		var body anthropicModelPage
		probe := providerone.FetchJSON(ctx, a.client(), pageURL(endpoint, after), func(r *http.Request) {
			r.Header.Set("x-api-key", secret) // the sole use of the credential
			r.Header.Set("anthropic-version", anthropicVersion)
		}, &body)
		if probe.Outcome != modelbroker.ProbeAccepted {
			// A FAILURE ON PAGE TWO IS THE LISTING'S FAILURE, and everything read so far is DISCARDED. The
			// alternative — returning page one as though it were the catalogue — is the one outcome that
			// looks identical to success while being wrong, and a picker would render it without a mark.
			return listingFrom(probe, nil, false)
		}
		models = append(models, body.models()...)
		if !body.HasMore || body.LastID == "" {
			// The provider says there is no more, so this IS the list. A has_more with no cursor is the
			// same statement: there is nothing this adapter could ask for next.
			return listingFrom(probe, models, true)
		}
		after = body.LastID
		if page == maxListPages-1 {
			return listingFrom(probe, models, false)
		}
	}
	// Unreachable: the loop returns on every path. Kept so a future edit to the bound cannot fall through
	// into an implicit zero-value listing, which would read as "not_probed" on a call that did probe.
	return modelbroker.ModelListing{
		Outcome: modelbroker.ProbeUnsupported, Endpoint: providerone.RedactURL(endpoint),
		Detail: "the models list was not read", Complete: false,
	}
}

// pageURL adds the page cursor. after empty is the first page, which carries no after_id at all — the
// provider treats an empty one as a model id it cannot find.
func pageURL(endpoint, after string) string {
	q := url.Values{"limit": {strconv.Itoa(pageLimit)}}
	if after != "" {
		q.Set("after_id", after)
	}
	return endpoint + "?" + q.Encode()
}

// anthropicModelPage is the wire shape, measured 2026-08-01:
//
//	{"data":[{"type":"model","id":"claude-opus-5","display_name":"Claude Opus 5",
//	          "created_at":"2026-07-24T00:00:00Z"}],
//	 "has_more":false,"first_id":"claude-opus-5","last_id":"claude-opus-4-1-20250805"}
//
// Data is a POINTER for the same reason the sibling family's is: a 200 carrying no `data` key is a body
// that could not be read, not a page with nothing on it.
type anthropicModelPage struct {
	Data *[]struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		// CreatedAt is RFC 3339 here; the sibling family sends a unix integer. Each family decodes its own
		// shape rather than sharing one struct that would have to accept both.
		CreatedAt time.Time `json:"created_at"`
	} `json:"data"`
	HasMore bool   `json:"has_more"`
	LastID  string `json:"last_id"`
}

func (p anthropicModelPage) models() []modelbroker.ModelInfo {
	if p.Data == nil {
		return nil
	}
	out := make([]modelbroker.ModelInfo, 0, len(*p.Data))
	for _, m := range *p.Data {
		if m.ID == "" {
			continue // a nameless entry is a blank selectable row in a picker — see the sibling family
		}
		out = append(out, modelbroker.ModelInfo{
			ID: m.ID, DisplayName: m.DisplayName, CreatedAt: m.CreatedAt.UTC(),
		})
	}
	return out
}

// listingFrom folds a classified request into a listing. Models AND `complete` ride only on ProbeAccepted,
// so neither an empty list nor a "complete" can be how a failure is expressed. Identical to the sibling
// family's, deliberately: the two disagreed about `complete` in a live transcript, and one rule enforced in
// two places that are read side by side is how that stops recurring. See provider_one/models.go.
func listingFrom(p modelbroker.Probe, models []modelbroker.ModelInfo, complete bool) modelbroker.ModelListing {
	out := modelbroker.ModelListing{
		Outcome: p.Outcome, Status: p.Status, Endpoint: p.Endpoint, Detail: p.Detail,
	}
	if p.Outcome == modelbroker.ProbeAccepted {
		out.Models, out.Complete = models, complete
	}
	return out
}
