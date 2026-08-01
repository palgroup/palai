// Package registry builds the broker's adapter map from the canonical family list.
//
// IT EXISTS BECAUSE THE MAP USED TO BE BUILT INSIDE AN `if` (E29 provider wiring). The control plane's
// composition root registered provider-one, provider-two and openai-compatible only when
// PALAI_MODEL_PROVIDER was exactly "provider-one"; every other value — including the unset one a fresh
// self-host has — registered a single adapter named `fake`. The comment above that function said the env
// selection was "the FALLBACK, not the whole story", and for the ROUTE it was: a published model route did
// override the model and the credential. But the ADAPTER map was not a fallback, it was a gate, so on the
// only deployment the model-connections feature exists for — one whose operator has typed no key into
// .env.local — a connection created through POST /v1/model-connections resolved onto a route and then died
// at Broker.Route with `unknown_provider`.
//
// So the env var now chooses ONE thing and says so: the deployment DEFAULT ROUTE. Which providers this
// binary can speak to is not a deployment setting at all — it is a property of the binary, and it is this
// map.
package registry

import (
	"net/http"

	fake "github.com/palgroup/palai/adapters/models/fake"
	openaicompatible "github.com/palgroup/palai/adapters/models/openai_compatible"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	providertwo "github.com/palgroup/palai/adapters/models/provider_two"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// FakeFamily is the deterministic in-process adapter. It is NOT a modelbroker.Family: an operator cannot
// bind a connection to it (it dials nothing and answers a fixed script), and offering it in a provider
// picker would be offering a deployment a way to look configured while reaching no model at all. It is
// registered because it is the deployment default for a stack that has configured no provider.
const FakeFamily = "fake"

// FakeScript is the script the deterministic adapter answers with. It is the needle every shipped-binary
// wiring proof looks for: a terminal projection carrying model "fake" means the run never left the process.
var FakeScript = fake.Script{ProviderRequestID: "fake-local", Model: "fake", Output: "ok"}

// Options parametrizes the families whose endpoint a DEPLOYMENT may still pin. It is the compatibility
// seam for PALAI_OPENAI_COMPATIBLE_BASE_URL, which was the ONLY way to reach a custom endpoint before a
// connection could carry its own — a deployment that set it keeps working unchanged, and a connection that
// names its own base URL wins over it (modelbroker.Request.BaseURL, applied in the adapters).
type Options struct {
	// OpenAICompatibleBaseURL is the deployment-wide fallback endpoint for the custom family.
	OpenAICompatibleBaseURL string
	// Client, when set, replaces the adapters' HTTP client. Only a test sets it.
	Client *http.Client
}

// Adapters builds the map Broker.Route dispatches through: every canonical family, plus the fake. It takes
// no environment and reads no environment — everything variable arrives on Options — so a test builds the
// same map production does.
func Adapters(opts Options) map[string]modelbroker.ModelAdapter {
	one := providerone.Adapter{Client: opts.Client}
	two := providertwo.Adapter{Client: opts.Client}
	return map[string]modelbroker.ModelAdapter{
		"provider-one": one,
		"provider-two": two,
		"openai-compatible": openaicompatible.Adapter{
			Adapter: providerone.Adapter{BaseURL: opts.OpenAICompatibleBaseURL, Client: opts.Client},
		},
		FakeFamily: fake.Adapter{Script: FakeScript},
	}
}

// Inspectors returns what each family's endpoint can be ASKED, keyed by family: the `verify` action's
// credential probe and the model picker's models list. A family absent from this map can be asked neither,
// and both surfaces say so in those words rather than inventing a green or an empty list.
//
// IT IS ONE MAP FOR TWO CAPABILITIES ON PURPOSE (E29 provider models). The probe and the list are the SAME
// GET at the SAME URL with the SAME credential — probing is listing with the body discarded — so a family
// that could do one and not the other would be a family whose two management actions disagree about what
// its endpoint is. One map is also one place to register a third provider: adding it here lights up the
// verify button and the model picker together, and forgetting it fails
// TestEverySelectableFamilyCanBeInspected rather than shipping a dead control.
//
// THE FAKE IS ABSENT, and that is safe because no connection can name it — see FakeFamily and
// TestTheFakeFamilyIsUnreachableFromAConnectionAndThereforeNeedsNoInspector. Giving it a fabricated list
// would be code no request can reach.
func Inspectors() map[string]modelbroker.ConnectionInspector {
	return map[string]modelbroker.ConnectionInspector{
		"provider-one":      providerone.Adapter{},
		"provider-two":      providertwo.Adapter{},
		"openai-compatible": providerone.Adapter{}, // the same ChatCompletions wire, at the operator's URL
	}
}
