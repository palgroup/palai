package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// fakeUsage scripts each seam outcome so the metering handler contract is exercised without a database.
// The recorded scope/body/query let a test assert the provision gate ran on the WRITE routes only and
// that the shared list parse reached the store.
type fakeUsage struct {
	write     ProvisionResult
	read      ProvisionResult
	rows      []ListRow
	lastScope middleware.Scope
	lastBody  []byte
	lastQuery ListQuery
	// lastSeries and seriesCalls prove the REJECTS never reach the store: a 400 that still ran the query
	// would be a validation message printed over a real result.
	lastSeries  UsageSeriesQuery
	seriesCalls int
}

func (f *fakeUsage) SetBudget(_ context.Context, s middleware.Scope, b []byte) (ProvisionResult, error) {
	f.lastScope, f.lastBody = s, b
	return f.write, nil
}
func (f *fakeUsage) ListBudgets(_ context.Context, s middleware.Scope) (ProvisionResult, error) {
	f.lastScope = s
	return f.read, nil
}
func (f *fakeUsage) SetQuota(_ context.Context, s middleware.Scope, b []byte) (ProvisionResult, error) {
	f.lastScope, f.lastBody = s, b
	return f.write, nil
}
func (f *fakeUsage) ListQuotas(_ context.Context, s middleware.Scope) (ProvisionResult, error) {
	f.lastScope = s
	return f.read, nil
}
func (f *fakeUsage) UsageSummary(_ context.Context, s middleware.Scope) (ProvisionResult, error) {
	f.lastScope = s
	return f.read, nil
}
func (f *fakeUsage) ListUsageLedger(_ context.Context, s middleware.Scope, q ListQuery) ([]ListRow, error) {
	f.lastScope, f.lastQuery = s, q
	return f.rows, nil
}
func (f *fakeUsage) UsageSeries(_ context.Context, s middleware.Scope, q UsageSeriesQuery) (ProvisionResult, error) {
	f.lastScope, f.lastSeries, f.seriesCalls = s, q, f.seriesCalls+1
	return f.read, nil
}

func usageTestServer(t *testing.T, verifier middleware.Verifier, usage UsageAPI) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(verifier, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil, WithUsage(usage)))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestUsageSurface pins the routing + rendering contract: setting a limit is 200 (it is an upsert, not a
// creation — a re-POST restates the same resource), the lists and the summary are 200, and a
// strict-decode reject is a 400.
func TestUsageSurface(t *testing.T) {
	admin := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1"}}
	fake := &fakeUsage{
		write: ProvisionResult{Body: []byte(`{"object":"budget","meter_prefix":"model.","limit_quantity":1000}`)},
		read:  ProvisionResult{Body: []byte(`{"object":"list","data":[]}`)},
	}
	base := usageTestServer(t, admin, fake)

	cases := []struct {
		method, path, body string
		wantStatus         int
	}{
		{"POST", "/v1/budgets", `{"meter_prefix":"model.","limit_quantity":1000}`, http.StatusOK},
		{"GET", "/v1/budgets", ``, http.StatusOK},
		{"POST", "/v1/quotas", `{"meter_prefix":"run.","limit_quantity":50,"window_seconds":3600}`, http.StatusOK},
		{"GET", "/v1/quotas", ``, http.StatusOK},
		{"GET", "/v1/usage", ``, http.StatusOK},
	}
	for _, c := range cases {
		resp := do(t, c.method, base+c.path, c.body, nil)
		if resp.StatusCode != c.wantStatus {
			t.Fatalf("%s %s status = %d, want %d", c.method, c.path, resp.StatusCode, c.wantStatus)
		}
		resp.Body.Close()
	}

	fake.write = ProvisionResult{BadField: true}
	if resp := do(t, "POST", base+"/v1/budgets", `{"nope":1}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown-field budget status = %d, want 400", resp.StatusCode)
	}
	fake.write = ProvisionResult{MissingField: "limit_quantity"}
	if resp := do(t, "POST", base+"/v1/quotas", `{"meter_prefix":"run."}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing-field quota status = %d, want 400", resp.StatusCode)
	}
}

// TestUsageWritesRequireProvisionScopeButReadsDoNot pins the split the surface makes on purpose: setting
// a limit is an administrative act, but SEEING what a tenant has spent is ordinary metering visibility —
// gating the read behind the provision capability would hide a tenant's own usage from its own key.
func TestUsageWritesRequireProvisionScopeButReadsDoNot(t *testing.T) {
	limited := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1", Scopes: []string{"responses"}}}
	fake := &fakeUsage{read: ProvisionResult{Body: []byte(`{"object":"list","data":[]}`)}}
	base := usageTestServer(t, limited, fake)

	for _, path := range []string{"/v1/budgets", "/v1/quotas"} {
		resp := do(t, "POST", base+path, `{"meter_prefix":"model.","limit_quantity":1}`, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s without the provision capability status = %d, want 403", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	for _, path := range []string{"/v1/usage", "/v1/budgets", "/v1/quotas"} {
		resp := do(t, "GET", base+path, ``, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s without the provision capability status = %d, want 200 (a tenant may always read its own metering)", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestUsageLedgerPageReusesTheSharedCursor proves the ledger page is the SAME keyset surface every other
// list uses — one over-fetch, one tenant-bound cursor, one rejection for a foreign one — rather than a
// second pagination dialect. The ledger carries no lifecycle state, so ?status= is an explicit 400.
func TestUsageLedgerPageReusesTheSharedCursor(t *testing.T) {
	admin := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1"}}
	fake := &fakeUsage{}
	base := usageTestServer(t, admin, fake)

	now := time.Now().UTC()
	for i := range 3 {
		fake.rows = append(fake.rows, ListRow{
			ID:        "use_" + string(rune('a'+i)),
			CreatedAt: now.Add(-time.Duration(i) * time.Minute),
			Body:      json.RawMessage(`{"object":"usage_ledger_entry"}`),
		})
	}

	page := getUsagePage(t, base, "limit=2")
	// The store is asked for one row MORE than the page: that over-fetch is how has_more is decided
	// without a second round trip, and it is the same contract every other list on this surface uses.
	if fake.lastQuery.Limit != 3 {
		t.Fatalf("store saw limit = %d, want 3 (the requested 2 plus the has_more over-fetch)", fake.lastQuery.Limit)
	}
	if len(page.Data) != 2 || !page.HasMore || page.NextCursor == nil {
		t.Fatalf("page = %d rows has_more=%v cursor=%v, want 2 rows + a further page", len(page.Data), page.HasMore, page.NextCursor)
	}

	// The minted cursor round-trips for its own tenant...
	getUsagePage(t, base, "limit=2&after="+*page.NextCursor)
	if fake.lastQuery.After == nil {
		t.Fatal("store saw no keyset position after a cursor was presented")
	}
	// ...and a cursor minted for a DIFFERENT resource kind is an explicit 400, never a silent page.
	foreign := encodeCursor(cursorKey(), "responses", middleware.Scope{Organization: "org_1", Project: "prj_1"},
		ListCursor{CreatedAt: now, ID: "resp_1"})
	resp := do(t, "GET", base+"/v1/usage/ledger?after="+foreign, ``, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("foreign-kind cursor status = %d, want 400", resp.StatusCode)
	}

	if resp := do(t, "GET", base+"/v1/usage/ledger?status=settled", ``, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("?status= on the ledger status = %d, want 400 (the ledger has no lifecycle state to filter)", resp.StatusCode)
	}
}

func getUsagePage(t *testing.T, base, query string) contracts.Page {
	t.Helper()
	resp := do(t, "GET", base+"/v1/usage/ledger?"+query, ``, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /v1/usage/ledger?%s status = %d (body=%s)", query, resp.StatusCode, body)
	}
	var page contracts.Page
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	return page
}

// A DASHBOARD ASKS "WHAT DID THIS SESSION COST", AND UNTIL THIS TEST THE ANSWER WAS EVERY SESSION'S ROWS.
//
// The ledger row has carried session_id and meter since it was created, and the shared keyset parse reads
// created_after/created_before — so the time window works and has always worked. What did NOT exist is the
// narrowing an operator actually asks for: a filter by session, or by meter. `?session_id=` on a session
// that does not exist returned a full page of OTHER sessions' rows, measured against the live stack on
// 2026-07-31: `/v1/usage/ledger?limit=20&session_id=ses_YOK` → 20 rows.
//
// That shape matters more than a missing feature. A filter that is accepted and ignored does not return
// nothing, it returns SOMEBODY ELSE'S rows under a heading that names one session — and the operator reads
// the heading. These two are deliberately parsed by the usage handler rather than by beginList: adding them
// to the shared parse would give every list on this surface two parameters it silently ignores, which is
// the very defect being fixed here.
func TestUsageLedgerNarrowsBySessionAndMeter(t *testing.T) {
	admin := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1"}}
	fake := &fakeUsage{}
	base := usageTestServer(t, admin, fake)

	getUsagePage(t, base, "limit=20&session_id=ses_wanted&meter=model.input_tokens")
	if fake.lastQuery.SessionID != "ses_wanted" {
		t.Fatalf("store saw session_id = %q, want %q — the filter reached the request and stopped there", fake.lastQuery.SessionID, "ses_wanted")
	}
	if fake.lastQuery.Meter != "model.input_tokens" {
		t.Fatalf("store saw meter = %q, want %q", fake.lastQuery.Meter, "model.input_tokens")
	}

	// Absent means unfiltered, and it must be the empty string rather than a zero value that reads as a
	// filter for the empty session — a store that translates "" into `session_id = ''` would return nothing
	// on every unfiltered page.
	getUsagePage(t, base, "limit=20")
	if fake.lastQuery.SessionID != "" || fake.lastQuery.Meter != "" {
		t.Fatalf("an unfiltered page carried session_id=%q meter=%q, want both empty", fake.lastQuery.SessionID, fake.lastQuery.Meter)
	}
}

// TestUsageSeriesRejectsBeforeItQueries pins the four ways a series request is refused, and pins that
// each refusal happens BEFORE the store is called. That second half is the point: a 400 rendered after
// the query ran would still have paid for the scan the ceiling exists to prevent, and a ceiling that
// costs what it forbids is not a ceiling.
//
// The window cap is a 400 rather than a silent clamp to the maximum. A clamped window answers a
// DIFFERENT question than the one asked, under a heading that says otherwise — this surface already
// refuses `limit=101` for the same reason instead of quietly serving 100.
func TestUsageSeriesRejectsBeforeItQueries(t *testing.T) {
	admin := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1"}}
	fake := &fakeUsage{read: ProvisionResult{Body: []byte(`{"object":"usage_series","points":[]}`)}}
	base := usageTestServer(t, admin, fake)

	for _, c := range []struct{ name, query, wants string }{
		// bucket is REQUIRED. It is the only parameter on this surface that cannot be defaulted: `limit`
		// changes how many rows come back, bucket changes what every number MEANS, and a caller who
		// omitted it would get a correct-looking chart at a granularity they never chose.
		{"bucket absent", "", "bucket is required"},
		{"bucket unknown", "bucket=week", "bucket must be one of"},
		{"bucket arbitrary interval", "bucket=15m", "bucket must be one of"},
		{"window inverted", "bucket=hour&created_after=2026-07-30T12:00:00Z&created_before=2026-07-30T10:00:00Z", "must be earlier than"},
		{"window empty", "bucket=hour&created_after=2026-07-30T10:00:00Z&created_before=2026-07-30T10:00:00Z", "must be earlier than"},
		{"hour window over 7 days", "bucket=hour&created_after=2026-07-01T00:00:00Z&created_before=2026-07-30T00:00:00Z", "must not exceed 7 days when bucket is hour"},
		{"day window over 92 days", "bucket=day&created_after=2026-01-01T00:00:00Z&created_before=2026-07-30T00:00:00Z", "must not exceed 92 days when bucket is day"},
		{"created_after unparseable", "bucket=hour&created_after=yesterday", "created_after must be an RFC3339"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := fake.seriesCalls
			res := do(t, "GET", base+"/v1/usage/series?"+c.query, ``, nil)
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", res.StatusCode, body)
			}
			if !strings.Contains(string(body), c.wants) {
				t.Fatalf("problem detail = %s, want it to contain %q — an error that does not name the limit cannot be acted on", body, c.wants)
			}
			if fake.seriesCalls != before {
				t.Fatalf("a rejected request still reached the store (%d -> %d)", before, fake.seriesCalls)
			}
		})
	}
}

// TestUsageSeriesResolvesTheWindowBeforeItReachesTheStore proves the defaults the store is entitled to
// assume. The store interpolates the bucket into a SQL interval literal, so "the handler validated it"
// is load-bearing rather than tidy: this test is what makes that claim true at the seam.
func TestUsageSeriesResolvesTheWindowBeforeItReachesTheStore(t *testing.T) {
	admin := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1"}}
	fake := &fakeUsage{read: ProvisionResult{Body: []byte(`{"object":"usage_series","points":[]}`)}}
	base := usageTestServer(t, admin, fake)

	// An explicit window arrives verbatim, in UTC.
	getSeries(t, base, "bucket=hour&created_after=2026-07-30T10:00:00Z&created_before=2026-07-30T12:00:00Z&meter=model.input_tokens")
	q := fake.lastSeries
	if q.Bucket != "hour" || q.Meter != "model.input_tokens" {
		t.Fatalf("store saw bucket=%q meter=%q, want hour / model.input_tokens", q.Bucket, q.Meter)
	}
	if !q.Start.Equal(time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)) || !q.End.Equal(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("store saw window %s..%s, want the requested one verbatim", q.Start, q.End)
	}

	// created_after omitted: the default span opens BEFORE created_before, not before now(). A default
	// anchored to the clock would answer a window containing none of the instant the caller named.
	getSeries(t, base, "bucket=day&created_before=2026-07-30T00:00:00Z")
	q = fake.lastSeries
	if !q.End.Equal(time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("default-start request ended at %s, want the requested created_before", q.End)
	}
	if got := q.End.Sub(q.Start); got != 30*24*time.Hour {
		t.Fatalf("default day span = %s, want 720h (30 days) ending at created_before", got)
	}

	// Neither bound given: the window ends now and opens one default span before it, and it is inside
	// the ceiling — the default request must never be the most expensive one the route allows.
	getSeries(t, base, "bucket=hour")
	q = fake.lastSeries
	if got := q.End.Sub(q.Start); got != 24*time.Hour {
		t.Fatalf("default hour span = %s, want 24h", got)
	}
	if q.End.Sub(q.Start) > maxSeriesSpanHour {
		t.Fatalf("the default hour window (%s) exceeds the ceiling (%s)", q.End.Sub(q.Start), maxSeriesSpanHour)
	}
}

// TestUsageSeriesReadNeedsNoProvisionCapability keeps the series on the same side of the gate as the
// rest of the metering READS: a tenant's own key must be able to see what that tenant spent, or the
// metering is invisible to the party paying for it. Only the limit-SETTING routes need `provision`.
func TestUsageSeriesReadNeedsNoProvisionCapability(t *testing.T) {
	plain := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1"}}
	fake := &fakeUsage{read: ProvisionResult{Body: []byte(`{"object":"usage_series","points":[]}`)}}
	base := usageTestServer(t, plain, fake)

	res := getSeries(t, base, "bucket=hour")
	if res != http.StatusOK {
		t.Fatalf("series status = %d, want 200 without the provision capability", res)
	}
	if fake.lastScope.Organization != "org_1" || fake.lastScope.Project != "prj_1" {
		t.Fatalf("store saw scope %+v, want the verified identity — the scope is never a query parameter", fake.lastScope)
	}
}

func getSeries(t *testing.T, base, query string) int {
	t.Helper()
	res := do(t, "GET", base+"/v1/usage/series?"+query, ``, nil)
	defer res.Body.Close()
	_, _ = io.ReadAll(res.Body)
	return res.StatusCode
}
