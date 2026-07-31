package modelbroker

import "sort"

// THE PROVIDER FAMILIES AN OPERATOR MAY BIND A CONNECTION TO, declared ONCE (E29 provider wiring).
//
// WHY THIS FILE EXISTS. Before it, "which providers can this deployment reach" was answered in three
// places that could not see each other: a map literal in the control plane's composition root, no check at
// all on POST /v1/model-connections, and nothing in the console. So `{"provider":"openai"}` — the name the
// product's own documentation uses — was accepted with a 201, stored, published onto a route, and then
// failed at the first model step of the first run with `unknown_provider: openai`. The operator learns at
// 3am that a form said yes to a value nothing could dial.
//
// The list is here, in the package that owns the Route call, so the three consumers all read the same
// bytes: adapters/models/registry builds the adapter map FROM it (and a test in that package asserts the
// two sets are equal, so a family declared here and registered nowhere is a failing test rather than a
// runtime surprise), the store validates a create against it, and the console renders its picker from the
// projection the API serves.
//
// THE NAMES ARE THE WIRE NAMES AND THEY DO NOT CHANGE. `provider-one` / `provider-two` are what
// model_connections rows on every existing deployment already carry, and renaming them to `openai` /
// `anthropic` would orphan those rows. Label is what a human reads; Name is what the broker dials.

// Family is one dialable provider family: the wire name, the human label, and whether the operator must
// supply the endpoint.
type Family struct {
	// Name is the value stored in model_connections.provider and passed to Broker.Route.
	Name string
	// Label is the human name — what the console's picker shows and what an error message quotes.
	Label string
	// RequiresBaseURL marks a family that has NO default endpoint: the operator's own URL is the whole
	// point of it, and a blank one would silently dial someone else's API with their key.
	RequiresBaseURL bool
	// AcceptsBaseURL marks a family whose endpoint may be overridden. Only the custom family accepts one:
	// a base URL on a family named "OpenAI" is a custom endpoint wearing a disguise, and the operator who
	// wants one has a family named for it.
	AcceptsBaseURL bool
}

// families is the canonical set, keyed by wire name.
var families = map[string]Family{
	"provider-one": {Name: "provider-one", Label: "OpenAI"},
	"provider-two": {Name: "provider-two", Label: "Anthropic"},
	"openai-compatible": {
		Name: "openai-compatible", Label: "Custom (OpenAI-compatible)",
		RequiresBaseURL: true, AcceptsBaseURL: true,
	},
}

// Families returns the canonical families in a stable order (by wire name), so a projection built from it
// is byte-identical between processes and a golden test over it means something.
func Families() []Family {
	out := make([]Family, 0, len(families))
	for _, f := range families {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LookupFamily reports the family with this wire name. ok=false is an unknown family — the answer that
// turns a 3am `unknown_provider` into a 400 at the moment the operator submits the form.
func LookupFamily(name string) (Family, bool) {
	f, ok := families[name]
	return f, ok
}

// FamilyNames is the wire-name list, for an error message that tells the operator what IS accepted rather
// than only that their value was not.
func FamilyNames() []string {
	out := make([]string, 0, len(families))
	for _, f := range Families() {
		out = append(out, f.Name)
	}
	return out
}
