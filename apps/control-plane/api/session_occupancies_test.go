package api

// A SESSION'S MACHINE BILL, over the surface a customer can actually reach.
//
// ‼️ THE STORE METHOD EXISTED AND NOTHING CALLED IT. coordinator.Store.SessionOccupancies has carried the
// comment "This IS the session's machine bill" since A.4 T4, and internal/execution/machine_occupancy.go's
// own header records that it was "called from exactly zero non-test files". So machine minutes were
// metered, settled into usage_ledger and billed — and the only occupancy route on the surface was
// /v1/runners/{id}/occupancies, which is systemOnly and asks about a MACHINE. On a shared fleet a
// machine's history contains other tenants' sessions, so that route is not merely the wrong question for
// a customer: it is one they must never be answered.
//
// Measured on this Mac 2026-08-06: a session that held a Mac for 330s of wall clock and was released
// `idle` bills 10.08 seconds, because the idle tail is excluded by the billed-interval CASE. That
// difference is the reason the route renders `billed_seconds` and not a subtraction a caller could do
// itself from started_at and released_at.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeOccupancies answers both halves of MachineOccupancyAPI and records which project was asked, so a
// route that leaked another tenant's scope is visible rather than merely absent from the output.
type fakeOccupancies struct {
	askedProject string
	askedSession string
	rows         []MachineOccupancyItem
}

func (f *fakeOccupancies) MachineOccupancies(_ context.Context, project, _ string, _ time.Time, _ string, _ int) ([]MachineOccupancyItem, error) {
	f.askedProject = project
	return f.rows, nil
}

func (f *fakeOccupancies) SessionOccupancies(_ context.Context, project, sessionID string) ([]MachineOccupancyItem, error) {
	f.askedProject, f.askedSession = project, sessionID
	return f.rows, nil
}

// occupancyRouter serves the shipped router with the occupancy surface mounted and a TENANT scope — no
// `system` capability, which is the whole claim: a customer's own key must reach this.
func occupancyRouter(t *testing.T, occ MachineOccupancyAPI) *httptest.Server {
	t.Helper()
	verifier := scopedVerifier{middleware.Scope{Project: "prj_1", Principal: "prin_1", Scopes: []string{"responses.write"}}}
	ts := httptest.NewServer(NewRouter(verifier, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		SSEConfig{}, nil, nil, WithRunners(&fakeRunnerRegistry{}), WithMachineOccupancies(occ)))
	t.Cleanup(ts.Close)
	return ts
}

// TestASessionsMachineBillIsReadableByItsOwnTenant is the route's reason for existing: a project-scoped
// key, with no `system` capability, reads the machine time its own session used.
func TestASessionsMachineBillIsReadableByItsOwnTenant(t *testing.T) {
	released := time.Unix(1_700_000_330, 0).UTC()
	occ := &fakeOccupancies{rows: []MachineOccupancyItem{{
		ID:             "lse_one",
		SessionID:      "ses_1",
		StartedAt:      time.Unix(1_700_000_000, 0).UTC(),
		LastActivityAt: time.Unix(1_700_000_010, 0).UTC(),
		ReleasedAt:     released,
		ReleaseReason:  "idle",
		// 330 seconds of wall clock, 10 of them billed: the idle tail is not chargeable, and this is the
		// number a caller cannot derive from the timestamps.
		BilledSeconds: 10.08,
	}}}
	ts := occupancyRouter(t, occ)

	status, body := doKeyRequest(t, ts, http.MethodGet, "/v1/sessions/ses_1/occupancies", "")
	if status != http.StatusOK {
		t.Fatalf("GET a session's occupancies = %d, want 200 for the tenant that owns it: %s", status, body)
	}
	if occ.askedProject != "prj_1" {
		t.Fatalf("the handler asked for project %q, want the verified bearer's prj_1", occ.askedProject)
	}
	if occ.askedSession != "ses_1" {
		t.Fatalf("the handler asked for session %q, want the path's ses_1", occ.askedSession)
	}
	var page struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(page.Data) != 1 {
		t.Fatalf("page carries %d row(s), want 1: %s", len(page.Data), body)
	}
	row := page.Data[0]
	if row["billed_seconds"] != 10.08 {
		t.Fatalf("billed_seconds = %v, want 10.08 — the billed interval is the one number a caller cannot "+
			"compute from started_at and released_at, which is why the route renders it", row["billed_seconds"])
	}
	if row["release_reason"] != "idle" {
		t.Fatalf("release_reason = %v, want idle: it is what the billed interval branches on", row["release_reason"])
	}
}

// TestTheMachineHistoryStaysOutOfATenantsReach is the other half, and without it the test above would be
// an argument for opening the machine route too. A machine on a shared fleet carries OTHER tenants'
// sessions, so its history is the plane's to read and a tenant key must be refused — by the capability,
// before any row is touched.
func TestTheMachineHistoryStaysOutOfATenantsReach(t *testing.T) {
	occ := &fakeOccupancies{}
	ts := occupancyRouter(t, occ)

	status, body := doKeyRequest(t, ts, http.MethodGet, "/v1/runners/rnr_1/occupancies", "")
	if status != http.StatusForbidden {
		t.Fatalf("GET a MACHINE's occupancies with a tenant key = %d, want 403: a shared Mac's history "+
			"names every tenant that has run on it (%s)", status, body)
	}
	if occ.askedProject != "" {
		t.Fatalf("the refused request still reached the store (asked for %q)", occ.askedProject)
	}
}
