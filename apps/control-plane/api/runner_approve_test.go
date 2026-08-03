package api

// THE WAITING ROOM'S DOOR (E24 T6), against the real mux and the real capability gate.
//
// What this route has to get right is not its status codes but its GATE and its IDENTITY. The gate is
// `approve` and not `provision`, which is api/approvals.go's argument applied verbatim: PATCH
// /v1/projects/{id} is the config_policy write path, it is gated on `provision`, and config_policy is where
// `approvers` lives — so a `provision` key could add itself to the list it is about to be checked against.
// The identity is the verified SCOPE, passed down whole, because the deciding principal is derived from the
// key id on it and a route that dropped it would admit machines as nobody.

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestRunnerApproveRequiresTheApproveCapabilityAndNotProvision is the gate, stated in the shape that makes
// it a test of the gate rather than of a blanket refusal: a `provision` key — which can cordon, revoke and
// mint pool keys on these very routes — is REFUSED here, an `approve` key is admitted, and the unrestricted
// admin key is admitted. The refusal is asserted to have reached the store NOT AT ALL, because a 403 written
// after the write would be a 403 that admitted the machine.
func TestRunnerApproveRequiresTheApproveCapabilityAndNotProvision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scopes []string
		want   int
	}{
		{"a provision key cannot approve, because it can rewrite the approver list", []string{"provision"}, http.StatusForbidden},
		{"a run-only key cannot approve", []string{"run"}, http.StatusForbidden},
		{"an approve key can", []string{"approve"}, http.StatusOK},
		{"the unrestricted admin key can", nil, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := &fakeRunnerRegistry{runnerFound: true}
			ts := scopedRouter(t, registry, tc.scopes)
			status, body := doKeyRequest(t, ts, http.MethodPost, "/v1/runners/rnr_mac/approve", "")
			if status != tc.want {
				t.Fatalf("POST approve with scopes %v = %d, want %d: %s", tc.scopes, status, tc.want, body)
			}
			if tc.want == http.StatusForbidden && registry.approvedRunner != "" {
				t.Fatalf("the store was asked to approve %q by a key the gate refused: a 403 written after the write is a 403 that admitted the machine", registry.approvedRunner)
			}
		})
	}
}

// TestRunnerApproveCarriesTheVerifiedScopeAndTheMachine pins what reaches the store. The PRINCIPAL is
// derived from the scope's key id one layer down, so a route that passed only the tenant would approve as an
// unidentified caller — which `ConfigPolicy.ApproverAllowed` refuses against any configured list, meaning
// the failure would present as "approval is broken" on exactly the deployments that configured a list.
func TestRunnerApproveCarriesTheVerifiedScopeAndTheMachine(t *testing.T) {
	registry := &fakeRunnerRegistry{runnerFound: true}
	ts := scopedRouter(t, registry, []string{"approve"})
	status, body := doKeyRequest(t, ts, http.MethodPost, "/v1/runners/rnr_mac/approve", "")
	if status != http.StatusOK {
		t.Fatalf("POST approve = %d, want 200: %s", status, body)
	}
	if registry.approvedRunner != "rnr_mac" {
		t.Errorf("the store was asked to approve %q, want rnr_mac", registry.approvedRunner)
	}
	if registry.approvedScope.Project != "prj_1" {
		t.Errorf("the store was handed project %q, want the verified bearer's prj_1",
			registry.approvedScope.Project)
	}
	if registry.approvedScope.Principal == "" {
		t.Error("the store was handed a scope with no principal: the approver policy is checked against an identity, and a decision by nobody is refused by every configured list")
	}
	var view map[string]any
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if view["state"] != "active" {
		t.Errorf("response state = %v, want active — an operator handed a bare 200 has to guess whether the machine can now take work", view["state"])
	}
}

// TestRunnerApproveSeparatesNotPermittedFromNotThere is the pair of refusals an operator has to be able to
// tell apart. A misconfigured approver list and a mistyped machine id are different problems, and collapsing
// them would hide the first behind the second — the ApprovalOutcome split, for the reason it exists there.
func TestRunnerApproveSeparatesNotPermittedFromNotThere(t *testing.T) {
	refused := &fakeRunnerRegistry{runnerFound: true, approveRefused: true}
	status, body := doKeyRequest(t, scopedRouter(t, refused, []string{"approve"}), http.MethodPost, "/v1/runners/rnr_mac/approve", "")
	if status != http.StatusForbidden {
		t.Fatalf("POST approve with a principal outside the approver list = %d, want 403: %s", status, body)
	}
	var problem map[string]any
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if problem["type"] == nil || !jsonContains(problem, "approver_not_authorized") {
		t.Fatalf("the 403 does not name the approver list as the reason: %s", body)
	}

	missing := &fakeRunnerRegistry{runnerFound: false}
	if status, body := doKeyRequest(t, scopedRouter(t, missing, []string{"approve"}), http.MethodPost, "/v1/runners/rnr_ghost/approve", ""); status != http.StatusNotFound {
		t.Fatalf("POST approve on a machine that is not this tenant's = %d, want 404: %s", status, body)
	}
}

// jsonContains reports whether any string value in the problem document carries `want`. The problem
// renderer's field names are middleware's, and this assertion is about the REASON reaching the operator
// rather than about which field carries it.
func jsonContains(doc map[string]any, want string) bool {
	for _, v := range doc {
		if s, ok := v.(string); ok && s == want {
			return true
		}
	}
	return false
}
