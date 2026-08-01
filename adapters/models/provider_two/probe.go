package providertwo

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// The Anthropic credential probe (E29 provider wiring). Same shape and same classifier as provider-one's —
// GET the models list, read the verdict off the status — over Anthropic's own auth headers, which are
// `x-api-key` plus the required version header rather than a bearer.
//
// ONE CLASSIFIER, deliberately: the two families must not be able to disagree about what a 403 means, and a
// second copy of that switch is how they would. See provider_one/probe.go for why the probe is a models
// list rather than a completion, and for what it declines to claim.

// messagesSuffix is the path this adapter POSTs a run to; the models list is its sibling.
const messagesSuffix = "/messages"

// modelsListURL derives the models-list URL from the configured Messages URL.
func modelsListURL(messagesURL string) (string, bool) {
	u, err := url.Parse(messagesURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	if !strings.HasSuffix(u.Path, messagesSuffix) {
		return "", false
	}
	u.Path = strings.TrimSuffix(u.Path, messagesSuffix) + "/models"
	return u.String(), true
}

// ProbeCredential implements modelbroker.CredentialProber.
func (a Adapter) ProbeCredential(ctx context.Context, baseURL, secret string) modelbroker.Probe {
	if baseURL == "" {
		baseURL = a.baseURL()
	}
	endpoint, ok := modelsListURL(baseURL)
	if !ok {
		return modelbroker.Probe{
			Outcome: modelbroker.ProbeUnsupported, Endpoint: providerone.RedactURL(baseURL),
			Detail: "the endpoint does not end in " + messagesSuffix + ", so its models list could not be derived " +
				"and NOTHING was checked — the credential may or may not work",
		}
	}
	return providerone.ClassifyProbe(ctx, a.client(), http.MethodGet, endpoint, func(r *http.Request) {
		r.Header.Set("x-api-key", secret) // the sole use of the credential
		r.Header.Set("anthropic-version", anthropicVersion)
	})
}
