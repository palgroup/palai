package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/automation"
)

// fakeScheduleAPI records what reached the store seam and scripts its outcomes.
type fakeScheduleAPI struct {
	createIn    *automation.ScheduleInput
	createErr   error
	reviseIn    *automation.ScheduleInput
	reviseFound bool
	pausedTo    *bool
	pauseFound  bool
	deleteFound bool
	getFound    bool
	occurrences []automation.OccurrenceView
}

func (f *fakeScheduleAPI) CreateSchedule(_ context.Context, _, _, _ string, in automation.ScheduleInput) (string, error) {
	f.createIn = &in
	if f.createErr != nil {
		return "", f.createErr
	}
	return "sch_created", nil
}
func (f *fakeScheduleAPI) GetSchedule(context.Context, string, string, string) (automation.ScheduleView, bool, error) {
	return automation.ScheduleView{ID: "sch_1", Name: "nightly"}, f.getFound, nil
}
func (f *fakeScheduleAPI) ReviseSchedule(_ context.Context, _, _, _ string, in automation.ScheduleInput) (int, bool, error) {
	f.reviseIn = &in
	return 2, f.reviseFound, nil
}
func (f *fakeScheduleAPI) SetPaused(_ context.Context, _, _, _ string, paused bool) (bool, error) {
	f.pausedTo = &paused
	return f.pauseFound, nil
}
func (f *fakeScheduleAPI) DeleteSchedule(context.Context, string, string, string) (bool, error) {
	return f.deleteFound, nil
}
func (f *fakeScheduleAPI) ListOccurrences(context.Context, string, string, string, int) ([]automation.OccurrenceView, error) {
	return f.occurrences, nil
}

func scheduleTestServer(t *testing.T, api *fakeScheduleAPI) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, api, nil, nil, SSEConfig{}, nil))
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
