package providerone

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// THE MODELS LIST (E29 provider models). The owner's ask was "model isimlerini string koymak istemiyorum" —
// no model name typed into this tree — and this is the half that makes that possible for the OpenAI and
// OpenAI-compatible families: the provider's own list, fetched with the operator's own credential.
//
// IT IS THE PROBE'S REQUEST WITH THE BODY KEPT. probe.go already GETs exactly this URL and throws the body
// away on purpose; nothing else about the call changes. That is why there is ONE derivation of the URL
// (modelsListURL) and ONE classification of the status (classify) shared by both — a lister that derived
// its own URL could probe one endpoint and list another, and an operator would have no way to see it.

// maxListBytes bounds the body this reads. The size of an upstream response is the UPSTREAM's choice, and
// an unbounded read of somebody else's server is a memory budget somebody else controls. 133 OpenAI models
// measure ~15 KB (2026-08-01), so a megabyte is two orders of magnitude of headroom.
//
// AN OVERSIZED BODY REFUSES RATHER THAN TRUNCATES. A JSON array cut off mid-element is either a parse
// error or — far worse, if the cut lands on an element boundary — a SHORTER LIST THAT LOOKS COMPLETE.
const maxListBytes = 1 << 20

// listTimeout bounds a whole listing, including every page of a paginated one. probeTimeout bounds a single
// request; without a second bound a provider that paginates forever would hold an API request open for
// probeTimeout × the page bound.
const listTimeout = 30 * time.Second

// ListModels implements modelbroker.ModelLister for this family and for openai-compatible (the same wire at
// the operator's URL). baseURL empty means this adapter's own endpoint.
//
// THE LIST IS UNPAGINATED FOR THIS FAMILY and that is measured, not assumed: `GET /v1/models` on
// api.openai.com returns `{object, data}` and no cursor field at all, with all 133 models in one response
// (2026-08-01). An OpenAI-COMPATIBLE gateway is somebody else's server and could invent one, but it would
// have to invent the field names too, so there is nothing here to follow. See provider_two for the family
// that DOES paginate.
func (a Adapter) ListModels(ctx context.Context, baseURL, secret string) modelbroker.ModelListing {
	if baseURL == "" {
		baseURL = a.baseURL()
	}
	endpoint, ok := modelsListURL(baseURL)
	if !ok {
		return modelbroker.ModelListing{
			Outcome: modelbroker.ProbeUnsupported, Endpoint: RedactURL(baseURL),
			Detail: "the endpoint does not end in " + chatCompletionsSuffix + ", so its models list could not be " +
				"derived and NOTHING was asked — this is not an empty list, it is no list",
		}
	}
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	var page openAIModelPage
	probe := FetchJSON(ctx, a.client(), endpoint, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+secret) // the sole use of the credential
	}, &page)
	if probe.Outcome != modelbroker.ProbeAccepted {
		return listingFrom(probe, nil, true)
	}
	return listingFrom(probe, page.models(), true)
}

// openAIModelPage is the wire shape, measured 2026-08-01:
//
//	{"object":"list","data":[{"id":"gpt-4o-mini","object":"model","created":1721172741,"owned_by":"system"}]}
//
// Data is a POINTER so that a 200 carrying no `data` key at all — `{}`, or a gateway's own envelope — is a
// body this could not read rather than a list with nothing in it. Unknown fields are tolerated on purpose:
// a provider that adds one must not break the picker.
type openAIModelPage struct {
	Data *[]struct {
		ID string `json:"id"`
		// Created is a unix second count on this family. Anthropic's sibling uses an RFC 3339 string, which
		// is why each family decodes its own shape rather than sharing one struct.
		Created int64 `json:"created"`
	} `json:"data"`
}

func (p openAIModelPage) models() []modelbroker.ModelInfo {
	if p.Data == nil {
		return nil
	}
	out := make([]modelbroker.ModelInfo, 0, len(*p.Data))
	for _, m := range *p.Data {
		if m.ID == "" {
			// A nameless entry would render as a blank, SELECTABLE row in a picker, and selecting it writes
			// a route revision whose model id is "" — which resolves to the deployment default rather than
			// failing, so the operator's choice would silently become somebody else's.
			continue
		}
		info := modelbroker.ModelInfo{ID: m.ID}
		if m.Created > 0 {
			info.CreatedAt = time.Unix(m.Created, 0).UTC()
		}
		out = append(out, info)
	}
	return out
}

// listingFrom folds a classified request into a listing. Models ride ONLY on ProbeAccepted — see
// modelbroker.ModelListing for why an empty list must never be able to mean "we could not ask".
//
// AND `complete` RIDES ONLY ON ProbeAccepted TOO, which is a rule this function enforces rather than
// trusting its callers with. It was found in a live transcript: a rejected Anthropic key rendered
// `complete: false` and a dead OpenAI-compatible gateway rendered `complete: true`, because the paginating
// family had abandoned a loop and this one had made a single request with nothing further to fetch. Both
// mean "there is no list". `complete` qualifies a LIST; with no list it is not a smaller truth, it is a
// claim about something that does not exist — and a screen cannot know which family it is reading.
func listingFrom(p modelbroker.Probe, models []modelbroker.ModelInfo, complete bool) modelbroker.ModelListing {
	out := modelbroker.ModelListing{
		Outcome: p.Outcome, Status: p.Status, Endpoint: p.Endpoint, Detail: p.Detail,
	}
	if p.Outcome == modelbroker.ProbeAccepted {
		out.Models, out.Complete = models, complete
	}
	return out
}

// FetchJSON performs ONE GET, classifies the status exactly as the credential probe does, and on a 2xx
// decodes the bounded body into `into`. It is exported because provider-two's lister runs the identical
// request over different auth headers — one classifier and one body reader, so the two families cannot
// disagree about what a 403 means or about how large a body they will accept.
//
// A 2xx WHOSE BODY DOES NOT DECODE BECOMES ProbeUnsupported, NOT an accepted empty list, and the status is
// carried so the caller can still see the 200. The credential was accepted and there is still no list; the
// operator needs to know both, and "accepted with zero models" would tell them the opposite of the second.
//
// THE ERROR BODY IS STILL NEVER READ. On a non-2xx nothing is decoded and no upstream text enters Detail —
// the probe's rule, unchanged, because an upstream error body can echo request material and this string
// reaches an API response and a log.
func FetchJSON(ctx context.Context, client *http.Client, endpoint string, auth func(*http.Request), into any) modelbroker.Probe {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	redacted := RedactURL(endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return modelbroker.Probe{Outcome: modelbroker.ProbeUnsupported, Endpoint: redacted, Detail: "the endpoint is not a usable URL"}
	}
	auth(req)

	resp, err := client.Do(req)
	if err != nil {
		return unreachable(redacted)
	}
	defer resp.Body.Close()

	out := classify(resp.StatusCode, redacted)
	if out.Outcome != modelbroker.ProbeAccepted || into == nil {
		// Drain a little so the connection can be reused; the body itself is never inspected.
		_, _ = resp.Body.Read(make([]byte, 512))
		return out
	}

	// maxListBytes+1 so an over-limit body is DETECTED rather than silently cut at the boundary.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxListBytes+1))
	if err != nil {
		return modelbroker.Probe{
			Outcome: modelbroker.ProbeUnsupported, Status: resp.StatusCode, Endpoint: redacted,
			Detail: "the endpoint answered " + strconv.Itoa(resp.StatusCode) + " and then the connection failed " +
				"mid-body, so no list was read. The credential was NOT rejected.",
		}
	}
	if len(body) > maxListBytes {
		return modelbroker.Probe{
			Outcome: modelbroker.ProbeUnsupported, Status: resp.StatusCode, Endpoint: redacted,
			Detail: "the endpoint's models list is larger than " + strconv.Itoa(maxListBytes) + " bytes and was " +
				"NOT read — a truncated list would look like a short one. The credential was NOT rejected.",
		}
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(into); err != nil {
		return modelbroker.Probe{
			Outcome: modelbroker.ProbeUnsupported, Status: resp.StatusCode, Endpoint: redacted,
			Detail: "the endpoint answered " + strconv.Itoa(resp.StatusCode) + " and accepted this credential, but its " +
				"body is not a models list this adapter can read. NOTHING was listed — this is not an empty list.",
		}
	}
	return out
}
