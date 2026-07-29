package stack

// THE SECOND MAC THAT NOTHING CAN REACH (§3.6 D11, E24 T4).
//
// Concurrency is min(PALAI_DISPATCH_WORKERS, the fleet's lease slots) and the first of those defaults to
// 1, so an operator who enrols a second machine adds a lease slot no dispatch worker will ever take. It
// was measured, it was written into the plan, and nothing anywhere told the operator — which is what
// makes this a correction rather than a feature: the fleet does not get faster, and now it says why.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fleetAPI serves GET /v1/runners with `count` machines, or the status given.
func fleetAPI(t *testing.T, count, status int) *apiClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/runners") {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		rows := make([]map[string]string, 0, count)
		for i := 0; i < count; i++ {
			rows = append(rows, map[string]string{"id": fmt.Sprintf("rnr_%d", i)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": rows})
	}))
	t.Cleanup(srv.Close)
	return &apiClient{baseURL: srv.URL, key: "test", http: &http.Client{Timeout: 5 * time.Second}}
}

// TestASecondRunnerWithOneDispatchWorkerIsSaidOutLoud pins the warning's exact trigger and its silence.
// Both halves matter: a warning that fired on a single-runner stack would be noise on every install in
// existence, and one that stayed silent on a two-machine stack is the measured lie this closes.
func TestASecondRunnerWithOneDispatchWorkerIsSaidOutLoud(t *testing.T) {
	for _, tc := range []struct {
		name     string
		workers  string
		runners  int
		wantWarn bool
	}{
		{"two machines and one dispatch worker is the whole bug", "1", 2, true},
		{"unset means one, because that is what applyConcurrencyEnv exports", "", 3, true},
		{"one machine is not a fleet and needs no warning", "1", 1, false},
		{"no machine at all is a queued-only stack", "1", 0, false},
		{"two dispatch workers can reach two machines", "2", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := fleetAPI(t, tc.runners, http.StatusOK)
			got := dispatchWorkerFleetWarning(api, envGetter(map[string]string{"PALAI_DISPATCH_WORKERS": tc.workers}))
			if tc.wantWarn != (got != "") {
				t.Fatalf("dispatchWorkerFleetWarning = %q, want a warning: %v", got, tc.wantWarn)
			}
			if !tc.wantWarn {
				return
			}
			// The sentence the plan asks for, verbatim in its load-bearing half: the bound is named, and
			// WHERE it lives is named, because "concurrency is limited" without "by the control plane" sends
			// an operator to buy a third Mac.
			want := fmt.Sprintf("%d runners are enrolled but PALAI_DISPATCH_WORKERS=1 — concurrent runs are bounded by the control plane, not the fleet", tc.runners)
			if !strings.Contains(got, want) {
				t.Fatalf("warning = %q, want it to contain %q", got, want)
			}
		})
	}
}

// TestAnUnreadableRunnerListSaysSoRatherThanGoingQuiet is the half a "no warning" result would hide. A
// read that failed and a fleet that is fine look identical from the outside, and the operator-facing
// consequence of confusing them is that a real ceiling goes unmentioned on the one stack that has it.
func TestAnUnreadableRunnerListSaysSoRatherThanGoingQuiet(t *testing.T) {
	api := fleetAPI(t, 0, http.StatusInternalServerError)
	got := dispatchWorkerFleetWarning(api, envGetter(map[string]string{"PALAI_DISPATCH_WORKERS": "1"}))
	if !strings.Contains(got, "could not read the enrolled runner count") {
		t.Fatalf("warning = %q, want it to report the failed read", got)
	}
}

// TestAStackWithNoRunnerGatewayIsNotWarnedAt keeps the queued-only posture silent. The route is only
// mounted when a registry is wired, so a 404 is "there is no fleet here" and not a failure — and a
// warning on every queued-only stack would be the kind of noise that teaches operators to ignore
// warnings.
func TestAStackWithNoRunnerGatewayIsNotWarnedAt(t *testing.T) {
	api := fleetAPI(t, 0, http.StatusNotFound)
	if got := dispatchWorkerFleetWarning(api, envGetter(map[string]string{"PALAI_DISPATCH_WORKERS": "1"})); got != "" {
		t.Fatalf("dispatchWorkerFleetWarning = %q on a stack with no runner route, want silence", got)
	}
}
