package registry

import (
	"testing"

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
