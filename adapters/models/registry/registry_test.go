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

// Every family the operator can select must have a probe, or the verify button on its row is a control
// that can only answer "not supported". If a family ever legitimately has no probe, this test is where
// that decision gets written down.
func TestEverySelectableFamilyCanBeProbed(t *testing.T) {
	probers := Probers()
	for _, f := range modelbroker.Families() {
		if _, ok := probers[f.Name]; !ok {
			t.Errorf("family %q has no credential prober: an operator cannot verify a key before an agent needs it", f.Name)
		}
	}
}
