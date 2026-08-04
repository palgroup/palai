package stack

import (
	"reflect"
	"testing"

	"github.com/palgroup/palai/packages/toolset"
)

// TestBringUpGrantsTheCanonicalDefaultSet pins the ONE fact this task exists for: the names the CLI
// writes into prj_local's policy are the canonical list, not a copy of it. A copy is what commit
// 1e5fc63e deleted and what left the guard hunting a symbol that no longer existed.
func TestBringUpGrantsTheCanonicalDefaultSet(t *testing.T) {
	granted := bootstrapDefaultTools()
	want := toolset.Default()
	if !reflect.DeepEqual(granted, want) {
		t.Fatalf("bring-up grants %v, canonical Default() is %v", granted, want)
	}
}

// TestTheGrantPreservesEveryOtherPolicyKey is the guard against the trap named in Step 1: the policy
// write REPLACES the document. A bring-up that sent only default_tools would delete approvers, pool,
// and allowed_models on its SECOND run — and approvers is an allow-list an operator deliberately
// closed. The first run is not where this bites, which is exactly why it needs a test.
func TestTheGrantPreservesEveryOtherPolicyKey(t *testing.T) {
	existing := map[string]any{
		"approvers":      []string{"key:apik_1"},
		"pool":           "mac-mini",
		"allowed_models": []string{"m1"},
	}
	merged := policyWithDefaultTools(existing, toolset.Default())

	for key, want := range existing {
		got, present := merged[key]
		if !present {
			t.Errorf("the write dropped %q: a re-run would clear it from the live policy", key)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the write changed %q: got %v, want %v", key, got, want)
		}
	}
	if !reflect.DeepEqual(merged["default_tools"], toolset.Default()) {
		t.Errorf("default_tools = %v, want the canonical set", merged["default_tools"])
	}
}

// TestTheGrantDoesNotMutateTheCallersPolicy — the caller holds the document it just read from the
// server; a merge that wrote through would make the "before" and "after" the same object and hide a
// diff the caller may want to log or skip on.
func TestTheGrantDoesNotMutateTheCallersPolicy(t *testing.T) {
	existing := map[string]any{"pool": "mac-mini"}
	_ = policyWithDefaultTools(existing, toolset.Default())
	if _, leaked := existing["default_tools"]; leaked {
		t.Fatal("policyWithDefaultTools wrote into the caller's map")
	}
}

// TestTheGrantAcceptsAnAbsentPolicy — a freshly provisioned project has no config_policy at all, and
// a nil map is where a merge written for the happy path panics.
func TestTheGrantAcceptsAnAbsentPolicy(t *testing.T) {
	merged := policyWithDefaultTools(nil, toolset.Default())
	if !reflect.DeepEqual(merged["default_tools"], toolset.Default()) {
		t.Fatalf("default_tools = %v on a nil policy, want the canonical set", merged["default_tools"])
	}
}

// TestTheGrantSkipsAPolicyThatAlreadyGrantsTools drives the predicate over THE SHAPES A JSON DECODE
// ACTUALLY PRODUCES, which is the whole reason it is worth a test.
//
// The skip exists so a re-run cannot overwrite a baseline an operator chose. What makes it fragile is
// that the value is read out of a `map[string]any` decoded from the wire, so a list of names arrives as
// `[]any` of `string` and NEVER as `[]string` — a predicate written against the Go type would assert
// false on every populated policy, skip nothing, and overwrite the operator's list on every bring-up.
// The failure is invisible in the direction that matters: the tool set stays plausible, it is simply
// no longer the one that was set.
//
// The null case is the second shape: `configPolicyInput` marshals its slices WITHOUT omitempty
// (apps/control-plane/internal/identity/store.go:559-565), so a project whose policy was written for
// `pool` alone stores `"default_tools":null` — the key is PRESENT and grants nothing, so a presence
// check would skip the write on exactly the project that needs it.
func TestTheGrantSkipsAPolicyThatAlreadyGrantsTools(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy map[string]any
		want   bool
	}{
		{"a decoded list of names", map[string]any{"default_tools": []any{"palai.workspace.file"}}, true},
		{"an empty decoded list", map[string]any{"default_tools": []any{}}, false},
		{"the key present and null", map[string]any{"default_tools": nil, "pool": "mac-mini"}, false},
		{"a policy that names other keys", map[string]any{"pool": "mac-mini"}, false},
		{"no policy at all", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := policyAlreadyGrantsTools(tc.policy); got != tc.want {
				t.Fatalf("policyAlreadyGrantsTools(%v) = %v, want %v", tc.policy, got, tc.want)
			}
		})
	}
}
