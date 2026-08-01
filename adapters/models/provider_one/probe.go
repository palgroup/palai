package providerone

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// THE CREDENTIAL PROBE (E29 provider wiring). An operator who pastes a key wants to know it works before an
// agent needs it at 3am, and the only check worth having is one that leaves the process.
//
// IT IS A GET ON THE MODELS LIST, NOT A COMPLETION, and both halves of that are deliberate:
//
//   - A LIST, because it is unambiguous. A minimal chat completion would have to read a verdict out of a
//     400 ("your key is fine, your body is not"), and that inference is only sound on servers that check
//     auth before shape. On one that does not — and an OpenAI-COMPATIBLE endpoint is by definition
//     somebody else's server — a bad key would probe green. A false green is the one outcome this button
//     must never produce.
//   - A LIST, also, because it is free. This action sits on a form an operator will press repeatedly while
//     getting a URL right, and a button that quietly bills a completion each time is a button that gets
//     pressed once and then distrusted.
//
// EVERY OpenAI-compatible server serves it: OpenAI itself, vLLM, Ollama, LM Studio, llama.cpp and LiteLLM
// all expose GET /v1/models with the same bearer auth.
//
// WHAT IT DOES NOT CLAIM, and the API response repeats this to the operator: that the MODEL id on the route
// exists, that a quota remains, or that a tool-calling run will succeed. It claims the endpoint answered and
// did not reject this credential. The capability question is separately and actively answered for the custom
// family by openai_compatible's Prober, at run admission.

// probeTimeout bounds the probe so a dead endpoint cannot hold an API request open.
const probeTimeout = 10 * time.Second

// chatCompletionsSuffix is the path this adapter POSTs a run to. The models list is its sibling.
const chatCompletionsSuffix = "/chat/completions"

// modelsListURL derives the models-list URL from the configured chat-completions URL. ok=false when the
// configured endpoint does not carry the suffix this adapter posts to — the honest answer is then that no
// probe was performed, never a guess.
func modelsListURL(chatURL string) (string, bool) {
	u, err := url.Parse(chatURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	if !strings.HasSuffix(u.Path, chatCompletionsSuffix) {
		return "", false
	}
	u.Path = strings.TrimSuffix(u.Path, chatCompletionsSuffix) + "/models"
	return u.String(), true
}

// ProbeCredential implements modelbroker.CredentialProber. baseURL empty means this adapter's own endpoint.
func (a Adapter) ProbeCredential(ctx context.Context, baseURL, secret string) modelbroker.Probe {
	if baseURL == "" {
		baseURL = a.baseURL()
	}
	endpoint, ok := modelsListURL(baseURL)
	if !ok {
		return modelbroker.Probe{
			Outcome: modelbroker.ProbeUnsupported, Endpoint: RedactURL(baseURL),
			Detail: "the endpoint does not end in " + chatCompletionsSuffix + ", so its models list could not be " +
				"derived and NOTHING was checked — the credential may or may not work",
		}
	}
	return ClassifyProbe(ctx, a.client(), http.MethodGet, endpoint, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+secret) // the sole use of the credential
	})
}

// ClassifyProbe performs one probe request and reads the outcome from the status. It is exported because
// provider-two runs the identical classification over different auth headers — one classifier, so the two
// families cannot disagree about what a 403 means.
//
// The response BODY is never read into the result: an upstream error body can echo request material, and
// this string reaches an API response and a log.
func ClassifyProbe(ctx context.Context, client *http.Client, method, endpoint string, auth func(*http.Request)) modelbroker.Probe {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	redacted := RedactURL(endpoint)
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return modelbroker.Probe{Outcome: modelbroker.ProbeUnsupported, Endpoint: redacted, Detail: "the endpoint is not a usable URL"}
	}
	auth(req)

	resp, err := client.Do(req)
	if err != nil {
		return unreachable(redacted)
	}
	defer resp.Body.Close()
	// Drain a little so the connection can be reused; the body itself is never inspected.
	_, _ = resp.Body.Read(make([]byte, 512))

	return classify(resp.StatusCode, redacted)
}

// unreachable is the no-HTTP-answer verdict. The transport error text is NOT quoted: it can carry the URL
// verbatim, userinfo included, and this detail is rendered to a console and written to a log.
func unreachable(redacted string) modelbroker.Probe {
	return modelbroker.Probe{
		Outcome: modelbroker.ProbeUnreachable, Endpoint: redacted,
		Detail: "the control plane could not reach this endpoint (name resolution, connection, TLS or timeout)",
	}
}

// classify maps ONE status onto the outcome vocabulary. It is the single switch both families' probe AND
// both families' models list read their verdict from, so no two of those four surfaces can come to
// different conclusions about the same 403.
func classify(status int, redacted string) modelbroker.Probe {
	switch {
	case status >= 200 && status < 300:
		return modelbroker.Probe{
			Outcome: modelbroker.ProbeAccepted, Status: status, Endpoint: redacted,
			Detail: "the endpoint answered and accepted this credential. It was NOT asked whether the route's " +
				"model id exists or whether a quota remains.",
		}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return modelbroker.Probe{
			Outcome: modelbroker.ProbeRejected, Status: status, Endpoint: redacted,
			Detail: "the endpoint REJECTED this credential — it is wrong, expired or revoked. Write a new one.",
		}
	case status == http.StatusTooManyRequests || status >= 500:
		return modelbroker.Probe{
			Outcome: modelbroker.ProbeTransient, Status: status, Endpoint: redacted,
			Detail: "the endpoint answered with a rate limit or a server error, which says nothing about the " +
				"credential either way. Try again.",
		}
	default:
		// 404 on a server that serves completions but no models list, 400 from a gateway with its own
		// contract. The credential was NOT rejected, but neither was it accepted, and saying "ok" here is
		// exactly the false green this probe exists to avoid.
		return modelbroker.Probe{
			Outcome: modelbroker.ProbeUnsupported, Status: status, Endpoint: redacted,
			Detail: "the endpoint answered but did not serve a models list, so the credential was NOT checked. " +
				"It was not rejected either — this is an inconclusive result, not a failure.",
		}
	}
}

// RedactURL strips userinfo and query before an endpoint is put in a response, an error or a log. A gateway
// URL can carry a token in either, and this string leaves the process.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<endpoint>"
	}
	u.RawQuery = ""
	u.User = nil
	return u.String()
}
