package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
)

// fakeScheduleAPI records what reached the store seam and scripts its outcomes.
type fakeScheduleAPI struct {
	createIn     *automation.ScheduleInput
	createErr    error
	reviseIn     *automation.ScheduleInput
	reviseFound  bool
	pausedTo     *bool
	pauseFound   bool
	deleteFound  bool
	getFound     bool
	occurrences  []automation.OccurrenceView
	schedules    []automation.ScheduleView
	listStatus   *string
	listWindow   *automation.ListWindow
	occWindow    *automation.ListWindow
	listCalledAt int
}

func (f *fakeScheduleAPI) CreateSchedule(_ context.Context, _, _ string, in automation.ScheduleInput) (string, error) {
	f.createIn = &in
	if f.createErr != nil {
		return "", f.createErr
	}
	return "sch_created", nil
}
func (f *fakeScheduleAPI) GetSchedule(context.Context, string, string) (automation.ScheduleView, bool, error) {
	return automation.ScheduleView{ID: "sch_1", Name: "nightly"}, f.getFound, nil
}
func (f *fakeScheduleAPI) ReviseSchedule(_ context.Context, _, _ string, in automation.ScheduleInput) (int, bool, error) {
	f.reviseIn = &in
	return 2, f.reviseFound, nil
}
func (f *fakeScheduleAPI) SetPaused(_ context.Context, _, _ string, paused bool) (bool, error) {
	f.pausedTo = &paused
	return f.pauseFound, nil
}
func (f *fakeScheduleAPI) DeleteSchedule(context.Context, string, string) (bool, error) {
	return f.deleteFound, nil
}
func (f *fakeScheduleAPI) ListSchedules(_ context.Context, _ string, w automation.ListWindow, status string) ([]automation.ScheduleView, error) {
	f.listWindow, f.listStatus = &w, &status
	f.listCalledAt++
	return f.schedules, nil
}
func (f *fakeScheduleAPI) ListOccurrences(_ context.Context, _, _ string, w automation.ListWindow) ([]automation.OccurrenceView, error) {
	f.occWindow = &w
	return f.occurrences, nil
}

func scheduleTestServer(t *testing.T, api *fakeScheduleAPI) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, api, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestScheduleManagementSurface pins the create/revise/pause/resume/delete/get-occurrences routes (B10):
// a create validates the cron + IANA timezone at the edge (a bad one is a 400, and it never reaches a
// stored row), a firing-relevant PATCH bumps the revision, pause/resume/delete route, and the occurrence
// log GETs.
func TestScheduleManagementSurface(t *testing.T) {
	fake := &fakeScheduleAPI{}
	base := scheduleTestServer(t, fake)

	// A create with a valid cron + timezone is a 201.
	if resp := do(t, "POST", base+"/v1/schedules", `{"name":"nightly","trigger_id":"trg_1","cron_expr":"0 2 * * *","timezone":"America/New_York"}`, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("valid create status = %d, want 201", resp.StatusCode)
	}

	// An unresolvable IANA timezone is a 400 (fail-closed), and it reached the store which returned the
	// typed error — the handler maps it to a 400, never a 500.
	fake.createErr = automation.ErrInvalidTimezone
	if resp := do(t, "POST", base+"/v1/schedules", `{"name":"x","trigger_id":"trg_1","cron_expr":"0 2 * * *","timezone":"Mars/Phobos"}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-timezone create status = %d, want 400", resp.StatusCode)
	}

	// A malformed cron is a 400.
	fake.createErr = automation.ErrInvalidCron
	if resp := do(t, "POST", base+"/v1/schedules", `{"name":"x","trigger_id":"trg_1","cron_expr":"@daily","timezone":"UTC"}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-cron create status = %d, want 400", resp.StatusCode)
	}

	// A malformed RFC3339 time is a 400 (parsed at the edge, never reaches the store).
	fake.createErr = nil
	if resp := do(t, "POST", base+"/v1/schedules", `{"name":"x","trigger_id":"trg_1","kind":"one_time","one_time_at":"not-a-time","timezone":"UTC"}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-time create status = %d, want 400", resp.StatusCode)
	}

	// A firing-relevant PATCH bumps the revision (200 with the new revision).
	fake.reviseFound = true
	if resp := do(t, "PATCH", base+"/v1/schedules/sch_1", `{"cron_expr":"*/5 * * * *","timezone":"UTC"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("revise status = %d, want 200", resp.StatusCode)
	}
	if fake.reviseIn == nil || fake.reviseIn.CronExpr != "*/5 * * * *" {
		t.Fatalf("revise input = %+v, want the edited cron reaching the store", fake.reviseIn)
	}
	// A revise of an unknown schedule is a 404.
	fake.reviseFound = false
	if resp := do(t, "PATCH", base+"/v1/schedules/sch_missing", `{"cron_expr":"*/5 * * * *","timezone":"UTC"}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revise unknown schedule status = %d, want 404", resp.StatusCode)
	}

	// Pause / resume route and pass the intended flag through.
	fake.pauseFound = true
	if resp := do(t, "POST", base+"/v1/schedules/sch_1/pause", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("pause status = %d, want 200", resp.StatusCode)
	}
	if fake.pausedTo == nil || !*fake.pausedTo {
		t.Fatal("pause did not pass paused=true to the store")
	}
	if resp := do(t, "POST", base+"/v1/schedules/sch_1/resume", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("resume status = %d, want 200", resp.StatusCode)
	}
	if fake.pausedTo == nil || *fake.pausedTo {
		t.Fatal("resume did not pass paused=false to the store")
	}

	// DELETE soft-deletes → 204; an unknown one → 404.
	fake.deleteFound = true
	if resp := do(t, "DELETE", base+"/v1/schedules/sch_1", ``, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}

	// The occurrence log GETs.
	fake.occurrences = []automation.OccurrenceView{{OccurrenceID: "occ_1", State: "admitted"}}
	if resp := do(t, "GET", base+"/v1/schedules/sch_1/occurrences", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("list occurrences status = %d, want 200", resp.StatusCode)
	}
}

// TestScheduleListRoutes pins the two E29 T1 reads at the handler boundary: the page envelope, the +1
// over-fetch, the ?status= admission and — the half that matters — the REFUSAL of a status the column
// cannot hold, written by the route rather than left to SQL.
func TestScheduleListRoutes(t *testing.T) {
	fake := &fakeScheduleAPI{}
	base := scheduleTestServer(t, fake)

	fake.schedules = []automation.ScheduleView{{ID: "sch_1", Name: "nightly", Status: "active"}}
	resp := do(t, "GET", base+"/v1/schedules?limit=5", ``, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/schedules status = %d, want 200", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, `"data"`) || !strings.Contains(body, `"sch_1"`) {
		t.Fatalf("list body = %s, want the shared page envelope carrying the row", body)
	}
	// The handler over-fetches by one so renderPage can answer has_more without a second query. Forwarding
	// the bare limit would report has_more=false on a page that is exactly full — a truncation nothing says.
	if fake.listWindow == nil || fake.listWindow.Limit != 6 {
		t.Fatalf("list reached the store with window %+v, want Limit 6 (the +1 over-fetch of ?limit=5)", fake.listWindow)
	}

	// ?status= is admitted for this kind and reaches the store as a filter.
	if resp := do(t, "GET", base+"/v1/schedules?status=paused", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("?status=paused status = %d, want 200", resp.StatusCode)
	}
	if fake.listStatus == nil || *fake.listStatus != "paused" {
		t.Fatalf("?status=paused reached the store as %v, want \"paused\"", fake.listStatus)
	}

	// AND A STATUS THE COLUMN CANNOT HOLD IS REFUSED HERE, BEFORE ANY STORE CALL. The call count is what
	// proves "before": a 400 written after the query still ran the query, and a route that answers 200 with
	// an empty page tells a client "none are in that state" when the truth is "that state does not exist".
	calls := fake.listCalledAt
	bad := do(t, "GET", base+"/v1/schedules?status=banana", ``, nil)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("?status=banana status = %d, want 400", bad.StatusCode)
	}
	if fake.listCalledAt != calls {
		t.Fatal("?status=banana reached the store; an unknown lifecycle state must be refused at the edge")
	}
	if body := readBody(t, bad); !strings.Contains(body, "active") || !strings.Contains(body, "paused") || !strings.Contains(body, "failed") {
		t.Fatalf("?status=banana problem = %s, want it to NAME the three states the column admits", body)
	}

	// The occurrence log rides the same envelope and the same over-fetch.
	fake.occurrences = []automation.OccurrenceView{{OccurrenceID: "occ_1", State: "admitted"}}
	occ := do(t, "GET", base+"/v1/schedules/sch_1/occurrences?limit=5", ``, nil)
	if occ.StatusCode != http.StatusOK {
		t.Fatalf("occurrences status = %d, want 200", occ.StatusCode)
	}
	if body := readBody(t, occ); !strings.Contains(body, `"data"`) || !strings.Contains(body, `"occ_1"`) {
		t.Fatalf("occurrences body = %s, want the shared page envelope", body)
	}
	if fake.occWindow == nil || fake.occWindow.Limit != 6 {
		t.Fatalf("occurrences reached the store with window %+v, want Limit 6", fake.occWindow)
	}
	// The occurrence log carries no lifecycle-state column either, so ?status= is refused on it — schedules
	// opened, schedule_occurrences did not, and the two kinds are deliberately separate.
	if bad := do(t, "GET", base+"/v1/schedules/sch_1/occurrences?status=admitted", ``, nil); bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("occurrences ?status=admitted = %d, want 400", bad.StatusCode)
	}
}

// TestTheShippedOccurrenceLogIsNotRetroGated draws the line E29 T1 nearly crossed. The NEW list carries the
// `provision` gate (it answers what an operator provisioned, the E25 T7 precedent); the occurrence log,
// which shipped in E11 with no gate, does NOT — adding one is a contract change, and a key with a narrowed
// scope set that reads that log today would start receiving 403 for an epic that promised to open reads.
//
// It is written as a test rather than left to the comment because the mistake is invisible in review: both
// handlers begin with an authorize-shaped line, the difference is one identifier, and the fleet of keys
// this would break all carry an EMPTY scope set in development — which HasScope treats as holding
// everything. The failure would have appeared only on a deployment that narrowed its keys, which is the
// deployment that took the security advice.
func TestTheShippedOccurrenceLogIsNotRetroGated(t *testing.T) {
	fake := &fakeScheduleAPI{occurrences: []automation.OccurrenceView{{OccurrenceID: "occ_1", State: "admitted"}}}
	narrow := scopedVerifier{middleware.Scope{Project: "prj_1", Principal: "prin_1", Scopes: []string{"responses"}}}
	srv := httptest.NewServer(NewRouter(narrow, nil, nil, nil, nil, nil, nil, nil, fake, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)

	if resp := do(t, "GET", srv.URL+"/v1/schedules/sch_1/occurrences", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("a key WITHOUT `provision` reading the shipped occurrence log = %d, want 200.\n"+
			"That route shipped in E11 ungated; narrowing it now is a contract change, not a read route.", resp.StatusCode)
	}
	// The other side of the same line: the route this task ADDED does carry the gate.
	if resp := do(t, "GET", srv.URL+"/v1/schedules", ``, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a key without `provision` reading the NEW list = %d, want 403", resp.StatusCode)
	}
}
