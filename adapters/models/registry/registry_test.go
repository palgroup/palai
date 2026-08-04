package registry

import (
	"context"
	"testing"

	fake "github.com/palgroup/palai/adapters/models/fake"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// THE ANTI-DRIFT GUARD, and it is the whole reason the family list and the adapter map live in two files
// that can see each other.
//
// A family declared in modelbroker.Families() but registered in no adapter is a provider the API accepts,
// the console offers, and the broker cannot dial — the exact defect this work exists to close, reintroduced
// one list at a time. A family registered here but absent from Families() is the mirror: an adapter no
// operator can ever select. The sets must be EQUAL, and this test is the only thing that makes that a
// measurement rather than a convention.
//
// It compares SETS, in both directions. A one-directional check (every family has an adapter) is the
// sweep that only looks one way, and this tree has paid for that twice.
func TestEveryCanonicalFamilyHasAnAdapterAndViceVersa(t *testing.T) {
	adapters := Adapters(Options{})
	// The fake is registered but is deliberately NOT a selectable family — see FakeFamily.
	if _, ok := adapters[FakeFamily]; !ok {
		t.Fatalf("the deterministic %q adapter is unregistered: a stack that configured no provider can run nothing", FakeFamily)
	}
	delete(adapters, FakeFamily)

	declared := map[string]bool{}
	for _, f := range modelbroker.Families() {
		declared[f.Name] = true
		if _, ok := adapters[f.Name]; !ok {
			t.Errorf("family %q (%s) is declared, offered on the API and pickable in the console, but NO adapter "+
				"is registered for it: a connection naming it fails at the first model step with unknown_provider", f.Name, f.Label)
		}
	}
	for name := range adapters {
		if !declared[name] {
			t.Errorf("adapter %q is registered but is not a declared family: no operator can ever select it", name)
		}
	}
}

// A family that REQUIRES a base URL must be one that ACCEPTS one, or the store's validation asks for a
// field it will then refuse. Cheap, and it pins the pair the store branches on.
func TestFamilyBaseURLFlagsAreCoherent(t *testing.T) {
	for _, f := range modelbroker.Families() {
		if f.RequiresBaseURL && !f.AcceptsBaseURL {
			t.Errorf("family %q requires a base URL it does not accept", f.Name)
		}
	}
}

// Every family the operator can select must be INSPECTABLE — both halves, in ONE map. A family with no
// prober has a verify button that can only answer "not supported"; a family with no lister has a model
// picker with nothing in it, which is worse, because an empty picker reads as "this key sees no models".
//
// The two halves are one interface and one registration for the reason this file already argues about
// families and adapters: two maps that can disagree WILL, and the disagreement shows up as a control that
// is present and dead rather than as a build failure.
func TestEverySelectableFamilyCanBeInspected(t *testing.T) {
	inspectors := Inspectors()
	for _, f := range modelbroker.Families() {
		if _, ok := inspectors[f.Name]; !ok {
			t.Errorf("family %q has no connection inspector: an operator can neither verify its key nor see "+
				"which models it can reach", f.Name)
		}
	}
	for name := range inspectors {
		if _, ok := modelbroker.LookupFamily(name); !ok {
			t.Errorf("inspector %q is registered for no declared family: no operator can ever reach it", name)
		}
	}
}

// THE FAKE HAS NO INSPECTOR, AND THAT IS SAFE ONLY BECAUSE NO CONNECTION CAN NAME IT. The brief for this
// work asked what the fake family's models list should answer, and the honest answer is that the question
// cannot arise: the models list hangs off a CONNECTION, and modelbroker.LookupFamily is what
// CreateModelConnection validates against — so `{"provider":"fake"}` is a 400 before a row exists.
//
// Registering a fabricated list for it would be adding a code path nothing can reach, which is the defect
// this tree keeps finding rather than a defence against it. This test is what makes the absence a decision:
// the day `fake` becomes selectable, it fails.
func TestTheFakeFamilyIsUnreachableFromAConnectionAndThereforeNeedsNoInspector(t *testing.T) {
	if _, ok := modelbroker.LookupFamily(FakeFamily); ok {
		t.Fatalf("%q is now a selectable family, so a connection can name it — it needs an inspector, or the "+
			"models list and the verify button on its row will both answer 'not supported'", FakeFamily)
	}
	if _, ok := Inspectors()[FakeFamily]; ok {
		t.Fatalf("%q has an inspector but no connection can name it: a code path nothing can reach", FakeFamily)
	}
}

// AN UNROUTED DEPLOYMENT ANSWERS EXACTLY WHAT IT ALWAYS DID, and this asserts it by DRIVING the
// adapter the map actually holds rather than by comparing the struct the map was built from. Every
// deployment that exists is this one — no PALAI_FAKE_SCRIPT_FILE, so Options.FakeScript is nil — and
// a great many proofs in this tree read `fake-local` and `ok` back out of a committed model step.
func TestAnUnroutedDeploymentReplaysTheBuiltInScript(t *testing.T) {
	adapter, ok := Adapters(Options{})[FakeFamily]
	if !ok {
		t.Fatalf("no %q adapter is registered", FakeFamily)
	}
	res, err := adapter.Execute(context.Background(), modelbroker.Request{ModelRequestID: "mreq_default"}, "unused", nil)
	if err != nil {
		t.Fatalf("Execute = %v, want the built-in script", err)
	}
	if res.Output != "ok" || res.ProviderRequestID != "fake-local" || res.Model != "fake" {
		t.Fatalf("built-in answer moved: output=%q provider_request_id=%q model=%q, want %q/%q/%q",
			res.Output, res.ProviderRequestID, res.Model, "ok", "fake-local", "fake")
	}
	if len(res.ToolCalls) != 0 || res.FinishReason != "stop" {
		t.Fatalf("built-in answer now calls %d tool(s) and finishes %q: a stack that configured nothing "+
			"must still answer without reaching a tool", len(res.ToolCalls), res.FinishReason)
	}
}

// A ROUTED SCRIPT REACHES THE ADAPTER THE BROKER DISPATCHES THROUGH — the whole point of the Options
// field, and the half that would be a dead setting if the map kept building the built-in script.
func TestARoutedScriptIsWhatTheFakeAdapterReplays(t *testing.T) {
	routed := fake.Script{
		ProviderRequestID: "scripted-uname", Model: "fake",
		ToolCalls: []modelbroker.ToolCall{{ID: "call_uname", Name: "palai.workspace.shell", Arguments: `{"command":"uname"}`}},
		Then:      []fake.Script{{ProviderRequestID: "scripted-answer", Model: "fake", Output: "Darwin"}},
	}
	adapter := Adapters(Options{FakeScript: &routed})[FakeFamily]

	first, err := adapter.Execute(context.Background(), modelbroker.Request{ModelRequestID: "mreq_1"}, "unused", nil)
	if err != nil {
		t.Fatalf("Execute = %v, want the routed script's first turn", err)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != "palai.workspace.shell" {
		t.Fatalf("routed first turn called %v; a deployment that routed a script still got no tool call", first.ToolCalls)
	}

	second, err := adapter.Execute(context.Background(), modelbroker.Request{
		ModelRequestID: "mreq_2",
		Messages:       []modelbroker.Message{{Role: "assistant", ToolCalls: first.ToolCalls}, {Role: "tool", ToolCallID: "call_uname", Content: "Darwin"}},
	}, "unused", nil)
	if err != nil {
		t.Fatalf("Execute = %v, want the routed script's second turn", err)
	}
	if second.Output != "Darwin" {
		t.Fatalf("routed second turn output = %q, want the scripted answer", second.Output)
	}

	// And the built-in script is untouched by any of it: the routed one is a parameter, not a mutation.
	if FakeScript.Output != "ok" || FakeScript.ProviderRequestID != "fake-local" || len(FakeScript.ToolCalls) != 0 {
		t.Fatalf("the package-level FakeScript was mutated by routing one in: %+v", FakeScript)
	}
}
