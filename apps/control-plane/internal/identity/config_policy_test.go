package identity

import (
	"encoding/json"
	"testing"
)

// E23 T2 — the write path for the approver list, and why this test exists at all.
//
// The plan recorded the approver list as needing no new plumbing because "the write path already ships
// (PATCH /v1/projects/{id})". The endpoint ships; the FIELD did not. UpdateProjectPolicy decodes into
// configPolicyInput through strictDecode, which sets DisallowUnknownFields — so a body carrying
// {"config_policy":{"approvers":[…]}} was rejected as a 400 and there was no way to configure the list at
// all. Half of "no migration needed" was true and the other half was measured false.
//
// This is a decode test rather than a round-trip one deliberately: the strictness IS the write path's
// behaviour, and it is what a schema addition can silently regress.

// TestConfigPolicyInputAcceptsAnApproverList is the fix, stated as a fact: the field decodes, and it
// decodes into the same shape coordinator.ConfigPolicy reads back out of the JSONB column.
func TestConfigPolicyInputAcceptsAnApproverList(t *testing.T) {
	var in struct {
		ConfigPolicy *configPolicyInput `json:"config_policy"`
	}
	body := []byte(`{"config_policy":{"approvers":["slack:T0123ABCD:U0123ABCD","key:key_9f2c"]}}`)
	if err := strictDecode(body, &in); err != nil {
		t.Fatalf("strictDecode(approvers) error = %v — the approver list has no way into config_policy", err)
	}
	if in.ConfigPolicy == nil {
		t.Fatal("config_policy decoded to nil")
	}
	if got := in.ConfigPolicy.Approvers; len(got) != 2 || got[0] != "slack:T0123ABCD:U0123ABCD" || got[1] != "key:key_9f2c" {
		t.Fatalf("approvers = %q, want the two principals verbatim", got)
	}

	// And what is stored is what the coordinator reads: UpdateProjectPolicy marshals this struct straight
	// into the column, so the key has to survive the round trip under the name ConfigPolicy expects.
	stored, err := json.Marshal(in.ConfigPolicy)
	if err != nil {
		t.Fatalf("marshal config policy: %v", err)
	}
	var back struct {
		Approvers []string `json:"approvers"`
	}
	if err := json.Unmarshal(stored, &back); err != nil {
		t.Fatalf("unmarshal the stored policy: %v", err)
	}
	if len(back.Approvers) != 2 {
		t.Fatalf("the stored policy carries %d approvers, want 2 — the column and the reader disagree on the key", len(back.Approvers))
	}
}

// TestConfigPolicyInputStillRejectsAnUnknownField is the guard on the guard. Adding a field to a
// strict-decode struct is exactly the change that can turn strictness off by accident, and the strictness
// is load bearing: it is what turns a typo'd `approvrs` into a 400 instead of a silently dropped write
// that leaves an operator believing they configured an approver list.
func TestConfigPolicyInputStillRejectsAnUnknownField(t *testing.T) {
	var in struct {
		ConfigPolicy *configPolicyInput `json:"config_policy"`
	}
	if err := strictDecode([]byte(`{"config_policy":{"approvrs":["key:key_9f2c"]}}`), &in); err == nil {
		t.Fatal("a mis-spelled approvers key decoded without error; a typo must be a 400, not a silent no-op")
	}
}
