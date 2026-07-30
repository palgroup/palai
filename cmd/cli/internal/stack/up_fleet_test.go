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

// statefulFleetAPI serves the two reads the fleet summary makes: machines with STATES, and pools. `status`
// applies to the runner read, which is the one whose failure has to be said out loud.
func statefulFleetAPI(t *testing.T, states []string, pools int, status int) *apiClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/runner-pools"):
			rows := make([]map[string]string, 0, pools)
			for i := 0; i < pools; i++ {
				rows = append(rows, map[string]string{"id": fmt.Sprintf("pool_%d", i)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": rows})
		case strings.HasPrefix(r.URL.Path, "/v1/runners"):
			if status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			rows := make([]map[string]string, 0, len(states))
			for i, state := range states {
				rows = append(rows, map[string]string{"id": fmt.Sprintf("rnr_%d", i), "state": state})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": rows})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &apiClient{baseURL: srv.URL, key: "test", http: &http.Client{Timeout: 5 * time.Second}}
}

// TestTheFleetLineNamesEveryMachineWaitingForAHuman is E24 T6's operator claim, and the load-bearing case is
// the third: a machine held in a strict pool's waiting room must be COUNTED and the command that admits it
// must be NAMED. E21 T2's lesson is the reason — a state nothing prints is a state an operator reads as a
// broken machine, and here the silence would last until somebody noticed a Mac had been idle for a day.
func TestTheFleetLineNamesEveryMachineWaitingForAHuman(t *testing.T) {
	for _, tc := range []struct {
		name     string
		states   []string
		pools    int
		want     []string
		wantNone []string
	}{
		{
			name:     "a queued-only stack has no fleet and says so without alarming anybody",
			pools:    0,
			want:     []string{"no pools and no enrolled machines"},
			wantNone: []string{"pending approval"},
		},
		{
			name:   "machines that are all serving are counted and nothing is demanded",
			states: []string{"active", "active"}, pools: 1,
			want:     []string{"1 pool(s), 2 active runner(s), 0 pending approval"},
			wantNone: []string{"PENDING machine", "palai admin runner approve"},
		},
		{
			name:   "a machine waiting for a human is named, and so is the command that admits it",
			states: []string{"active", "pending", "pending"}, pools: 2,
			want: []string{"2 pool(s), 1 active runner(s), 2 pending approval",
				"holds a certificate and takes NO work", "palai admin runner approve"},
		},
		{
			name:   "a cordoned machine is neither active nor waiting, and is not counted as either",
			states: []string{"cordoned", "revoked"}, pools: 1,
			want:     []string{"1 pool(s), 0 active runner(s), 0 pending approval"},
			wantNone: []string{"palai admin runner approve"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fleetLine(statefulFleetAPI(t, tc.states, tc.pools, http.StatusOK).fleet())
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("fleet line = %q, want it to contain %q", got, want)
				}
			}
			for _, unwanted := range tc.wantNone {
				if strings.Contains(got, unwanted) {
					t.Errorf("fleet line = %q, want it NOT to contain %q", got, unwanted)
				}
			}
		})
	}
}

// TestAnUnreadableFleetSaysSoOnTheReport is the half a silent summary would hide, and it is the same
// argument dispatchWorkerFleetWarning's read failure makes: "nothing is waiting" and "nobody could ask" must
// not render identically, because the second is the state in which a waiting machine goes unmentioned.
func TestAnUnreadableFleetSaysSoOnTheReport(t *testing.T) {
	got := fleetLine(statefulFleetAPI(t, nil, 0, http.StatusInternalServerError).fleet())
	if !strings.Contains(got, "could not be read") {
		t.Fatalf("fleet line = %q, want it to report the failed read", got)
	}
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
