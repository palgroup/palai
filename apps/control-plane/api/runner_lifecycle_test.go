package api

// THE MACHINE-LIFECYCLE ROUTES (E24 T5), against the real mux and the real provision gate.
//
// ‼️ AND SINCE 2026-08-07 THIS FILE IS THE ONLY PLACE THE REACHABILITY CLAIM LIVES. E24 T5 wrote
// cmd/cli/internal/admin/runner_lifecycle_test.go for §3.6 D15: `Revoke()` — SAN-011's hard stop — had
// been implemented, tested and catalogued while NOTHING IN THE TREE CALLED IT, and that file existed to
// keep a caller in front of it. The caller it kept was `palai admin runner`, which left with T1's other
// API duplicates.
//
// The claim did not leave with it, which is why the file could be deleted rather than moved: the routes
// below ARE the caller now, and they are driven here against the real mux — the verb reaches the state
// change, a key without `provision` is refused, another tenant's machine is a 404, and a verb the mux does
// not serve is refused. tests/uat/fleet/reachable_test.go names `Revoke` and `Resume` in its production-
// caller sweep besides. What is gone is the CLI's own rendering and argument parsing, which had no second
// home because they had no second surface.
//
// §3.6 D15: `Revoke()` — SAN-011's hard stop for a compromised runner — was implemented, tested,
// catalogued, and reachable from nothing. These are the routes that end that, so what they have to prove is
// not only that they answer but that they answer with the RIGHT verb about the RIGHT machine, refuse a key
// without `provision`, and refuse a verb the mux does not serve.

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestRunnerLifecycleRoutesCarryTheVerbAndTheMachine pins the three routes end to end through the mux: the
// action reaching the store is the one in the path, and the id is the one in the path. A handler that read
// the action out of a body — or defaulted it — would answer 200 here and pass, which is why the assertion
// is on what the store was ASKED rather than on the status.
func TestRunnerLifecycleRoutesCarryTheVerbAndTheMachine(t *testing.T) {
	for _, tc := range []struct{ path, wantAction, wantState string }{
		{"/v1/runners/rnr_mac/cordon", "cordon", "cordoned"},
		{"/v1/runners/rnr_mac/resume", "resume", "active"},
		{"/v1/runners/rnr_mac/revoke", "revoke", "revoked"},
	} {
		t.Run(tc.wantAction, func(t *testing.T) {
			registry := &fakeRunnerRegistry{runnerFound: true}
			ts := scopedRouter(t, registry, nil)
			status, body := doKeyRequest(t, ts, http.MethodPost, tc.path, "")
			if status != http.StatusOK {
				t.Fatalf("POST %s = %d, want 200: %s", tc.path, status, body)
			}
			if registry.lifecycleAction != tc.wantAction {
				t.Errorf("the store was asked for %q, want %q", registry.lifecycleAction, tc.wantAction)
			}
			if registry.lifecycleRunner != "rnr_mac" {
				t.Errorf("the store was given machine %q, want rnr_mac — a lifecycle call that names no machine is the whole-gateway bool this task removes", registry.lifecycleRunner)
			}
			var view map[string]any
			if err := json.Unmarshal(body, &view); err != nil {
				t.Fatalf("decode %s: %v", body, err)
			}
			if view["state"] != tc.wantState {
				t.Errorf("response state = %v, want %q — an operator handed a bare 200 has to guess what happened", view["state"], tc.wantState)
			}
		})
	}
}

// TestRunnerLifecycleRefusesAKeyWithoutProvision is the authorization claim. Taking a Mac out of service
// and decommissioning one are org administration, so a run-only key must reach the store NOT AT ALL — the
// assertion is on the store having been asked nothing, because a 403 written after the write would be a
// 403 that changed the fleet.
func TestRunnerLifecycleRefusesAKeyWithoutProvision(t *testing.T) {
	for _, action := range []string{"cordon", "resume", "revoke"} {
		registry := &fakeRunnerRegistry{runnerFound: true}
		ts := scopedRouter(t, registry, []string{"run"})
		status, body := doKeyRequest(t, ts, http.MethodPost, "/v1/runners/rnr_mac/"+action, "")
		if status != http.StatusForbidden {
			t.Fatalf("POST %s with a run-only key = %d, want 403: %s", action, status, body)
		}
		if registry.lifecycleAction != "" {
			t.Fatalf("an unauthorized key reached the store with %q", registry.lifecycleAction)
		}
	}
}

// TestRunnerLifecycleAnswers404ForAMachineThatIsNotThisTenants pins the non-disclosing answer. "Not yours",
// "not there" and "already decommissioned" are one answer on purpose: which of the three it was is not
// something a caller should be able to probe an id space for.
func TestRunnerLifecycleAnswers404ForAMachineThatIsNotThisTenants(t *testing.T) {
	registry := &fakeRunnerRegistry{runnerFound: false}
	ts := scopedRouter(t, registry, nil)
	status, body := doKeyRequest(t, ts, http.MethodPost, "/v1/runners/rnr_someone_elses/revoke", "")
	if status != http.StatusNotFound {
		t.Fatalf("revoking another tenant's machine = %d, want 404: %s", status, body)
	}
}

// TestRunnerLifecycleServesNoOtherVerb is the route surface's own boundary, and it is what lets the handler
// carry no validation code at all: the three actions are bound at registration, so `.../decommission` is a
// 404 from the mux and there is no string anywhere that could be turned into a `runners.state` value.
func TestRunnerLifecycleServesNoOtherVerb(t *testing.T) {
	registry := &fakeRunnerRegistry{runnerFound: true}
	ts := scopedRouter(t, registry, nil)
	for _, path := range []string{
		"/v1/runners/rnr_mac/decommission",
		"/v1/runners/rnr_mac/revoked",
		"/v1/runners/rnr_mac/pending",
	} {
		status, _ := doKeyRequest(t, ts, http.MethodPost, path, "")
		if status != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", path, status)
		}
		if registry.lifecycleAction != "" {
			t.Fatalf("POST %s reached the store with action %q", path, registry.lifecycleAction)
		}
	}
}

// TestRunnerReadRendersItsLiveLeaseCountIncludingZero is the per-runner lease counter's operator-facing
// half. After cordoning a Mac the only question left is whether it can be taken AWAY, and zero is the
// answer that means yes — so zero has to be RENDERED rather than omitted, while a deployment that wired no
// gateway must carry no such field at all. Both directions are asserted, because a field that is always
// present and always zero would satisfy either one alone.
func TestRunnerReadRendersItsLiveLeaseCountIncludingZero(t *testing.T) {
	none := int64(0)
	registry := &fakeRunnerRegistry{runnerFound: true, activeLeases: &none}
	ts := scopedRouter(t, registry, nil)
	_, body := doKeyRequest(t, ts, http.MethodPost, "/v1/runners/rnr_mac/cordon", "")
	var view map[string]any
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	leases, ok := view["active_leases"]
	if !ok {
		t.Fatal("the cordon response carries no active_leases: an operator cannot tell whether the Mac can be unplugged")
	}
	if leases != float64(0) {
		t.Fatalf("active_leases = %v, want 0", leases)
	}

	withoutGateway := &fakeRunnerRegistry{runnerFound: true}
	tsNoGateway := scopedRouter(t, withoutGateway, nil)
	_, quiet := doKeyRequest(t, tsNoGateway, http.MethodPost, "/v1/runners/rnr_mac/cordon", "")
	var quietView map[string]any
	if err := json.Unmarshal(quiet, &quietView); err != nil {
		t.Fatalf("decode %s: %v", quiet, err)
	}
	if _, present := quietView["active_leases"]; present {
		t.Fatal("a deployment with no gateway rendered active_leases: 'serving nothing' and 'nobody asked' must not look the same")
	}
}
