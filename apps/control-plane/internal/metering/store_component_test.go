//go:build component

// Package metering_test holds the real-PostgreSQL component tests for the metering store (E13 Task 6,
// BIL-001/BIL-003/QUO-001). They run only under `make test-component TEST=postgres`; the build tag keeps
// them out of the credential-free unit tier. The handler tests upstream use a fake seam, so THIS is where
// the SQL itself is proven — the prefix matching, the org/project narrowing, and the keyset page.
package metering_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/metering"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

func openHarness(t *testing.T) *coordinator.Store {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	cs, err := coordinator.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return cs
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// tenant seeds an organization with two projects, so every test can prove the intra-organization
// narrowing the SQL (not RLS) is responsible for.
func tenant(t *testing.T, cs *coordinator.Store) (projectA, projectB string) {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	projectA, projectB = newID("prj"), newID("prj")
	stmts := [][]any{
		{`INSERT INTO projects (id) VALUES ($1)`, projectA},
		{`INSERT INTO projects (id) VALUES ($1)`, projectB},
	}
	for _, stmt := range stmts {
		if _, err := cs.Pool().Exec(ctx, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	return projectA, projectB
}

func settle(t *testing.T, cs *coordinator.Store, project, meter, unit string, quantity float64) {
	t.Helper()
	if _, err := cs.Pool().Exec(storage.WithSystemScope(context.Background()),
		`INSERT INTO usage_ledger (id, project_id, run_id, meter, quantity, unit, dedupe_key)
		 VALUES ($1, $2, $3, $4, $5, $6, $1)`,
		newID("use"), project, newID("run"), meter, quantity, unit); err != nil {
		t.Fatalf("settle %s: %v", meter, err)
	}
}

// TestSetLimitIsAnUpsertScopedToTheCaller proves a limit belongs to the scope of the key that set it and
// that re-POSTing the same meter prefix RESTATES it rather than minting a rival row — two rows for one
// prefix would make "which limit binds?" a race.
func TestSetLimitIsAnUpsertScopedToTheCaller(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	store := metering.New(cs.Pool())
	projectA, _ := tenant(t, cs)
	scope := middleware.Scope{Project: projectA}

	first, err := store.SetBudget(ctx, scope, []byte(`{"meter_prefix":"model.","limit_quantity":1000}`))
	if err != nil {
		t.Fatalf("SetBudget error = %v", err)
	}
	second, err := store.SetBudget(ctx, scope, []byte(`{"meter_prefix":"model.","limit_quantity":2500}`))
	if err != nil {
		t.Fatalf("SetBudget(restate) error = %v", err)
	}
	var a, b struct {
		ID            string  `json:"id"`
		ProjectID     string  `json:"project_id"`
		LimitQuantity float64 `json:"limit_quantity"`
	}
	mustDecode(t, first.Body, &a)
	mustDecode(t, second.Body, &b)
	if a.ID != b.ID {
		t.Fatalf("re-setting the same meter prefix minted a second budget (%s then %s)", a.ID, b.ID)
	}
	if b.LimitQuantity != 2500 {
		t.Fatalf("restated limit = %v, want 2500", b.LimitQuantity)
	}
	if b.ProjectID != projectA {
		t.Fatalf("budget project = %q, want the caller's own %q (the scope is never a body field)", b.ProjectID, projectA)
	}

	// An unknown field is rejected outright: a misspelled limit must not silently store no cap.
	if out, _ := store.SetBudget(ctx, scope, []byte(`{"meter_prefix":"model.","limit":1}`)); !out.BadField {
		t.Fatalf("unknown-field budget = %+v, want a strict-decode reject", out)
	}
	if out, _ := store.SetQuota(ctx, scope, []byte(`{"meter_prefix":"run.","limit_quantity":5}`)); out.MissingField != "window_seconds" {
		t.Fatalf("quota without a window = %+v, want a missing window_seconds reject", out)
	}
}

// TestUsageSummaryTotalsTheCallersScope proves the summary reports the caller's own consumption: a
// project-scoped key sees its project's meters, never its sibling's, and sees the limits its totals are
// measured against. The second half this sentence used to promise — "an org-scoped key sees the whole
// organization" — describes a caller no credential can construct any more, and the comment inside the
// test says so at the point where that half used to be asserted.
func TestUsageSummaryTotalsTheCallersScope(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	store := metering.New(cs.Pool())
	projectA, projectB := tenant(t, cs)

	settle(t, cs, projectA, "model.input_tokens", "token", 30)
	settle(t, cs, projectA, "model.output_tokens", "token", 12)
	settle(t, cs, projectB, "model.output_tokens", "token", 500)
	if _, err := store.SetBudget(ctx, middleware.Scope{Project: projectA},
		[]byte(`{"meter_prefix":"model.","limit_quantity":1000}`)); err != nil {
		t.Fatalf("SetBudget error = %v", err)
	}

	projectTotal, budgets := summaryTotals(t, store, middleware.Scope{Project: projectA})
	if projectTotal != 42 {
		t.Fatalf("project-A summary total = %v, want 42 (its own two meters, not project-B's 500)", projectTotal)
	}
	if budgets != 1 {
		t.Fatalf("project-A summary carried %d budget(s), want the 1 that binds it", budgets)
	}
	// THE ORG-WIDE HALF THIS TEST ONCE ASSERTED HERE IS GONE, AND NOT BY THIS TASK'S CHOICE. It drove
	// middleware.Scope{Project: ""} directly into the store, bypassing HTTP — but
	// storage.OpenPool's PrepareConn has refused a non-orgOnly, empty-project acquisition since A.2 Task 1
	// (ErrProjectRequired, storage/tenant.go), and coordinator.Store.VerifyAPIKey has rejected a
	// projectless key even earlier ("A key with no project is rejected: the LP-0 surface only admits
	// project-scoped keys"). So the scenario this half asserted — an org-scoped key with no project
	// reading both projects' usage — was already unreachable through any real credential before this task
	// touched the file; A.2 Task 3 only removed the Scope field that let the test keep CONSTRUCTING it by
	// hand. What changed since: usage_ledger's POLICY is keyed on the installation —
	// `palai_apply_installation_policy('usage_ledger')`, the authority to grep for, not a chain number —
	// and narrowing it would make every installation-wide budget under-count. So the DATABASE no longer stands
	// in the way of an installation-wide usage view; what is still missing is a SEAM that asks for one. The
	// narrowing this test pins is the query's, not the policy's, and that is now the only narrowing there
	// is: delete the predicate and project-A's summary silently swallows project-B's 500.
}

// TestLedgerPageIsKeysetOrderedAndScoped proves the raw entry page an exporter reads: newest first, no
// row repeated or skipped across the keyset boundary, and confined to the caller's project.
func TestLedgerPageIsKeysetOrderedAndScoped(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	store := metering.New(cs.Pool())
	projectA, projectB := tenant(t, cs)
	for i := range 5 {
		settle(t, cs, projectA, "model.output_tokens", "token", float64(i+1))
	}
	settle(t, cs, projectB, "model.output_tokens", "token", 999)
	scope := middleware.Scope{Project: projectA}

	// Page one asks for 3 (2 + the has_more over-fetch the handler adds).
	first, err := store.ListUsageLedger(ctx, scope, api.ListQuery{Limit: 3})
	if err != nil {
		t.Fatalf("ListUsageLedger error = %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("first page returned %d rows, want the 3 requested", len(first))
	}
	if !first[0].CreatedAt.After(first[2].CreatedAt) {
		t.Fatalf("page is not newest-first: %v then %v", first[0].CreatedAt, first[2].CreatedAt)
	}

	// The page's last row is the keyset position; the next page continues strictly before it.
	boundary := first[1]
	second, err := store.ListUsageLedger(ctx, scope, api.ListQuery{
		Limit: 10, After: &api.ListCursor{CreatedAt: boundary.CreatedAt, ID: boundary.ID},
	})
	if err != nil {
		t.Fatalf("ListUsageLedger(after) error = %v", err)
	}
	if len(second) != 3 {
		t.Fatalf("continuation returned %d rows, want the 3 remaining of project-A's 5", len(second))
	}
	for _, row := range second {
		if row.ID == first[0].ID || row.ID == first[1].ID {
			t.Fatalf("continuation repeated row %s across the keyset boundary", row.ID)
		}
	}
	// Project-B's row never appears in project-A's page, and every row names project-A.
	for _, row := range append(first, second...) {
		var entry struct {
			ProjectID string `json:"project_id"`
			Object    string `json:"object"`
		}
		mustDecode(t, row.Body, &entry)
		if entry.ProjectID != projectA {
			t.Fatalf("page carried a row of project %q, want only %q", entry.ProjectID, projectA)
		}
		if entry.Object != "usage_ledger_entry" {
			t.Fatalf("row object = %q, want usage_ledger_entry", entry.Object)
		}
	}

	// A created_after filter narrows the page the same way every other list's does.
	future := time.Now().UTC().Add(time.Hour)
	empty, err := store.ListUsageLedger(ctx, scope, api.ListQuery{Limit: 10, CreatedGTE: &future})
	if err != nil {
		t.Fatalf("ListUsageLedger(filtered) error = %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("created_after in the future returned %d rows, want 0", len(empty))
	}
}

func summaryTotals(t *testing.T, store *metering.Store, scope middleware.Scope) (total float64, budgets int) {
	t.Helper()
	out, err := store.UsageSummary(context.Background(), scope)
	if err != nil {
		t.Fatalf("UsageSummary error = %v", err)
	}
	var body struct {
		Meters []struct {
			Quantity float64 `json:"quantity"`
		} `json:"meters"`
		Budgets []json.RawMessage `json:"budgets"`
	}
	mustDecode(t, out.Body, &body)
	for _, m := range body.Meters {
		total += m.Quantity
	}
	return total, len(body.Budgets)
}

func mustDecode(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

// settleForSession is settle with a session attached. The plain helper leaves session_id NULL, which is
// exactly the row a session filter must NOT return — so both shapes have to exist in the fixture for the
// filter to be shown to discriminate rather than merely to run.
func settleForSession(t *testing.T, cs *coordinator.Store, project, session, meter, unit string, quantity float64) {
	t.Helper()
	if _, err := cs.Pool().Exec(storage.WithSystemScope(context.Background()),
		`INSERT INTO usage_ledger (id, project_id, session_id, run_id, meter, quantity, unit, dedupe_key)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $1)`,
		newID("use"), project, session, newID("run"), meter, quantity, unit); err != nil {
		t.Fatalf("settle %s for session: %v", meter, err)
	}
}

// A DASHBOARD ASKS "WHAT DID THIS SESSION COST", AND THE ANSWER USED TO BE EVERY SESSION'S ROWS.
//
// Measured against the live stack on 2026-07-31 before this change:
// `/v1/usage/ledger?limit=20&session_id=ses_YOK` → 20 rows, for a session that does not exist. The row has
// carried session_id since the table was created and the shared keyset parse has always read
// created_after/created_before — so the WINDOW worked. What did not exist was the narrowing, and an
// accepted-and-ignored filter is worse than an absent one: it returns other sessions' rows under a heading
// that names one session, and the operator reads the heading.
//
// This runs against a real Postgres deliberately. The handler test proves the parameter reaches the store;
// only the database can prove the predicate discriminates — and the two cases that matter are a row
// belonging to ANOTHER session and a row belonging to NO session (session_id NULL), because `= $8` excludes
// the NULL row silently and that is correct here but must be shown rather than assumed.
func TestLedgerNarrowsBySessionAndMeterAndLeavesTheUnfilteredPageWhole(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	store := metering.New(cs.Pool())
	project, _ := tenant(t, cs)
	scope := middleware.Scope{Project: project}

	wanted, other := newID("ses"), newID("ses")
	settleForSession(t, cs, project, wanted, "model.input_tokens", "token", 10)
	settleForSession(t, cs, project, wanted, "model.output_tokens", "token", 20)
	settleForSession(t, cs, project, other, "model.input_tokens", "token", 30)
	settle(t, cs, project, "run.admitted", "run", 1) // no session at all — session_id NULL

	all, err := store.ListUsageLedger(ctx, scope, api.ListQuery{Limit: 50})
	if err != nil {
		t.Fatalf("unfiltered ListUsageLedger error = %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("unfiltered page returned %d rows, want all 4 — an empty filter must be NO predicate, not a match for the empty value", len(all))
	}

	bySession, err := store.ListUsageLedger(ctx, scope, api.ListQuery{Limit: 50, SessionID: wanted})
	if err != nil {
		t.Fatalf("session-filtered ListUsageLedger error = %v", err)
	}
	if len(bySession) != 2 {
		t.Fatalf("session filter returned %d rows, want the 2 belonging to %s (the other session's row and the session-less row must both be excluded)", len(bySession), wanted)
	}

	byMeter, err := store.ListUsageLedger(ctx, scope, api.ListQuery{Limit: 50, Meter: "model.input_tokens"})
	if err != nil {
		t.Fatalf("meter-filtered ListUsageLedger error = %v", err)
	}
	if len(byMeter) != 2 {
		t.Fatalf("meter filter returned %d rows, want the 2 input-token entries", len(byMeter))
	}

	both, err := store.ListUsageLedger(ctx, scope, api.ListQuery{Limit: 50, SessionID: wanted, Meter: "model.input_tokens"})
	if err != nil {
		t.Fatalf("both-filtered ListUsageLedger error = %v", err)
	}
	if len(both) != 1 {
		t.Fatalf("session+meter returned %d rows, want the single intersection — two filters must compose, not replace one another", len(both))
	}

	// A filter for a session that does not exist returns NOTHING. This is the assertion the live stack
	// failed: it answered with a full page.
	none, err := store.ListUsageLedger(ctx, scope, api.ListQuery{Limit: 50, SessionID: "ses_does_not_exist"})
	if err != nil {
		t.Fatalf("absent-session ListUsageLedger error = %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("a filter for a session that does not exist returned %d rows, want 0 — it was returning other sessions' rows under that session's name", len(none))
	}
}

// settleAt is `settle` with an explicit occurred_at. The bucketing tests need to place rows inside
// KNOWN buckets, which clock_timestamp() cannot do — every row would land in the current hour and the
// series would be one bucket wide no matter what the SQL did.
func settleAt(t *testing.T, cs *coordinator.Store, project, meter, unit string, quantity float64, at time.Time) {
	t.Helper()
	if _, err := cs.Pool().Exec(storage.WithSystemScope(context.Background()),
		`INSERT INTO usage_ledger (id, project_id, run_id, meter, quantity, unit, dedupe_key, occurred_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $1, $7)`,
		newID("use"), project, newID("run"), meter, quantity, unit, at); err != nil {
		t.Fatalf("settle %s at %s: %v", meter, at, err)
	}
}

// seriesPoint mirrors the store's projection for assertion. It is declared in the TEST because the
// production type is unexported: a test that reached into the package's own struct would pass on a
// projection whose JSON never rendered, and the JSON is the thing the dashboard consumes.
type seriesPoint struct {
	Object      string    `json:"object"`
	BucketStart time.Time `json:"bucket_start"`
	Meter       string    `json:"meter"`
	Unit        string    `json:"unit"`
	Quantity    float64   `json:"quantity"`
	Entries     int64     `json:"entries"`
}

type seriesEnvelope struct {
	Object string        `json:"object"`
	Bucket string        `json:"bucket"`
	Points []seriesPoint `json:"points"`
}

// TestUsageSeriesBucketsZeroFillsAndOrdersTotally is the whole contract of the chart source in one
// window, and every clause of it was a way the series could be wrong while looking right:
//
//   - BUCKETING: two rows ten minutes apart fall in ONE hourly bucket and are summed. A series that
//     returned them as two points would draw the same total as two half-height columns.
//   - ZERO-FILL: the 11:00 bucket has no rows at all and must still appear, at zero, for every meter.
//     A chart that omits it does not show a gap — it slides 12:00 left and draws continuous spend
//     across an hour where there was none.
//   - TOTAL ORDER: three meters share every bucket, so `ORDER BY bucket_start` alone is a PARTIAL order
//     and the points within a bucket could arrive in any order. The assertion below is the exact
//     sequence, so an untied ordering is a failure rather than a coin flip.
//   - SCOPE: a sibling project's row inside the same window must not reach a project-scoped caller.
//     usage_ledger is secured at the INSTALLATION level — `palai_apply_installation_policy('usage_ledger')`
//     in 000002_row_level_security.up.sql, deliberately WIDER than the project so an installation-wide
//     budget cannot under-count — so the project narrowing is the QUERY's own job and RLS will not catch
//     its absence. The "and MUST reach an org-scoped one" half of this line is gone with organizations;
//     no credential can construct that caller (see TestUsageSummaryTotalsTheCallersScope).
func TestUsageSeriesBucketsZeroFillsAndOrdersTotally(t *testing.T) {
	cs := openHarness(t)
	store := metering.New(cs.Pool())
	projectA, projectB := tenant(t, cs)

	// A fixed instant, not time.Now(): the buckets a series returns must be the same on every run and at
	// every hour of the day, and a relative seed would make this test's expectations drift with the clock.
	base := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	settleAt(t, cs, projectA, "model.input_tokens", "token", 100, base)
	settleAt(t, cs, projectA, "model.input_tokens", "token", 50, base.Add(10*time.Minute))
	settleAt(t, cs, projectA, "model.output_tokens", "token", 7, base.Add(10*time.Minute))
	settleAt(t, cs, projectA, "run.admitted", "run", 1, base.Add(10*time.Minute))
	// 11:00 is deliberately EMPTY — the zero-fill is what has to invent it.
	settleAt(t, cs, projectA, "model.input_tokens", "token", 20, base.Add(2*time.Hour))
	// A sibling project, inside the window, on a meter project A also uses.
	settleAt(t, cs, projectB, "model.input_tokens", "token", 9999, base)

	q := api.UsageSeriesQuery{Bucket: "hour", Start: base, End: base.Add(2 * time.Hour)}
	got := readSeries(t, store, middleware.Scope{Project: projectA}, q)

	want := []seriesPoint{
		{BucketStart: base, Meter: "model.input_tokens", Unit: "token", Quantity: 150, Entries: 2},
		{BucketStart: base, Meter: "model.output_tokens", Unit: "token", Quantity: 7, Entries: 1},
		{BucketStart: base, Meter: "run.admitted", Unit: "run", Quantity: 1, Entries: 1},
		{BucketStart: base.Add(time.Hour), Meter: "model.input_tokens", Unit: "token", Quantity: 0, Entries: 0},
		{BucketStart: base.Add(time.Hour), Meter: "model.output_tokens", Unit: "token", Quantity: 0, Entries: 0},
		{BucketStart: base.Add(time.Hour), Meter: "run.admitted", Unit: "run", Quantity: 0, Entries: 0},
		{BucketStart: base.Add(2 * time.Hour), Meter: "model.input_tokens", Unit: "token", Quantity: 20, Entries: 1},
		{BucketStart: base.Add(2 * time.Hour), Meter: "model.output_tokens", Unit: "token", Quantity: 0, Entries: 0},
		{BucketStart: base.Add(2 * time.Hour), Meter: "run.admitted", Unit: "run", Quantity: 0, Entries: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("series returned %d points, want %d (3 buckets x 3 meters, zero-filled): %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if !g.BucketStart.UTC().Equal(w.BucketStart) || g.Meter != w.Meter || g.Unit != w.Unit ||
			g.Quantity != w.Quantity || g.Entries != w.Entries {
			t.Fatalf("point %d = %s %s/%s q=%v n=%d, want %s %s/%s q=%v n=%d",
				i, g.BucketStart.UTC().Format(time.RFC3339), g.Meter, g.Unit, g.Quantity, g.Entries,
				w.BucketStart.Format(time.RFC3339), w.Meter, w.Unit, w.Quantity, w.Entries)
		}
		if g.Object != "usage_series_point" {
			t.Fatalf("point %d object = %q, want usage_series_point", i, g.Object)
		}
	}

	// THE ORG-WIDE HALF ONCE ASSERTED HERE IS GONE — see TestUsageSummaryTotalsTheCallersScope's comment:
	// an org-scoped, projectless middleware.Scope was already unreachable through any real credential
	// since A.2 Task 1 (storage.OpenPool's PrepareConn refuses it; VerifyAPIKey rejects a projectless key
	// even earlier), so this half tested a scenario Task 3 could no longer even construct, not one it
	// broke.

	// A meter narrowing collapses the series to one line per bucket and must NOT disturb the buckets:
	// the same three, still zero-filled.
	q.Meter = "model.output_tokens"
	filtered := readSeries(t, store, middleware.Scope{Project: projectA}, q)
	if len(filtered) != 3 {
		t.Fatalf("meter-filtered series returned %d points, want 3 (one meter x three buckets): %+v", len(filtered), filtered)
	}
	for _, p := range filtered {
		if p.Meter != "model.output_tokens" {
			t.Fatalf("meter-filtered series carried %q — a filter that is accepted and ignored returns other meters under this one's name", p.Meter)
		}
	}
}

// TestUsageSeriesEmptyWindowIsAnEmptyChartNotAZeroChart pins the one case the zero-fill deliberately
// does NOT cover. Buckets are invented for meters PRESENT in the window; a window with no rows has no
// meters, so it returns no points. The alternative — emitting a zero line for every meter ever seen —
// would require this table to hold an authoritative meter vocabulary, and it holds only what has been
// settled. `points` must still be [] rather than null, so a client draws an empty chart rather than
// crashing on a nil.
func TestUsageSeriesEmptyWindowIsAnEmptyChartNotAZeroChart(t *testing.T) {
	cs := openHarness(t)
	store := metering.New(cs.Pool())
	projectA, _ := tenant(t, cs)

	base := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	settleAt(t, cs, projectA, "model.input_tokens", "token", 100, base)

	// A window that ends before the only row exists.
	out, err := store.UsageSeries(context.Background(), middleware.Scope{Project: projectA},
		api.UsageSeriesQuery{Bucket: "hour", Start: base.Add(-5 * time.Hour), End: base.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("UsageSeries(empty window) error = %v", err)
	}
	var env seriesEnvelope
	mustDecode(t, out.Body, &env)
	if len(env.Points) != 0 {
		t.Fatalf("empty window returned %d points, want 0", len(env.Points))
	}
	if !bytes.Contains(out.Body, []byte(`"points":[]`)) {
		t.Fatalf("empty series rendered %s, want an empty ARRAY — a null points field makes every client special-case it", out.Body)
	}
}

// TestUsageSeriesDayBucketsTruncateInUTCNotTheSessionTimezone is the reason UsageSeries calls the
// three-argument date_trunc. The two-argument form truncates a timestamptz in the SESSION's TimeZone,
// so a connection that inherited a non-UTC timezone would put the SAME instant in a different day —
// silently re-slicing every chart, with no error and no wrong-looking number to notice.
//
// The test sets the session timezone to one far from UTC and asserts the day boundary does not move.
// It is the shape this repository keeps finding: the behaviour depends on ambient configuration, so
// reading the query proves nothing and only running it under the other configuration does.
func TestUsageSeriesDayBucketsTruncateInUTCNotTheSessionTimezone(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	store := metering.New(cs.Pool())
	projectA, _ := tenant(t, cs)

	// 22:30 UTC on the 30th is already the 31st in Asia/Kathmandu (+05:45) and still the 30th in UTC.
	at := time.Date(2026, 7, 30, 22, 30, 0, 0, time.UTC)
	settleAt(t, cs, projectA, "model.input_tokens", "token", 5, at)

	if _, err := cs.Pool().Exec(storage.WithSystemScope(ctx), `SET TIME ZONE 'Asia/Kathmandu'`); err != nil {
		t.Fatalf("SET TIME ZONE: %v", err)
	}
	t.Cleanup(func() { _, _ = cs.Pool().Exec(storage.WithSystemScope(ctx), `SET TIME ZONE 'UTC'`) })

	got := readSeries(t, store, middleware.Scope{Project: projectA},
		api.UsageSeriesQuery{Bucket: "day", Start: at.Add(-24 * time.Hour), End: at.Add(time.Hour)})
	for _, p := range got {
		if p.Quantity == 0 {
			continue
		}
		if want := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC); !p.BucketStart.UTC().Equal(want) {
			t.Fatalf("22:30Z landed in the %s bucket, want %s — date_trunc followed the session timezone",
				p.BucketStart.UTC().Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}
}

func readSeries(t *testing.T, store *metering.Store, scope middleware.Scope, q api.UsageSeriesQuery) []seriesPoint {
	t.Helper()
	out, err := store.UsageSeries(context.Background(), scope, q)
	if err != nil {
		t.Fatalf("UsageSeries error = %v", err)
	}
	var env seriesEnvelope
	mustDecode(t, out.Body, &env)
	if env.Object != "usage_series" {
		t.Fatalf("series object = %q, want usage_series", env.Object)
	}
	return env.Points
}

// TestUsageSeriesOrdersTotallyWithinABucket is the test that PINS the (meter, unit) tiebreaker, and it
// exists as its own case because the obvious version of it does not work.
//
// `ORDER BY bucket_start` alone is a PARTIAL order here: grouping by meter puts one row per meter in
// every bucket, so the sort key repeats and the rows within a bucket may arrive in any order at all. A
// chart drawn from that interleaves its series.
//
// THE TRAP, MEASURED (2026-07-31), because the obvious test does NOT catch this. Deleting
// `, p.meter, p.unit` from the ORDER BY and running the whole suite left every case GREEN — first with
// the three meters the bucketing test seeds, then again with the eight below. The rows come back sorted
// anyway, and the reason is the PLAN, not the query: the LEFT JOIN's key is (bucket_start, meter, unit),
// so when the planner chooses a merge join it sorts both inputs on exactly that triple and the output is
// incidentally ordered. Take that plan away and the accident disappears:
//
//	-- same untied query, 8 meters in one bucket
//	ORDER BY bucket_start                      -> a.alpha | b.beta | m.mid | model.input_tokens | ...
//	SET enable_mergejoin=off; ORDER BY bucket_start
//	                                           -> b.beta | model.input_tokens | ... | z.omega | a.alpha
//	SET enable_mergejoin=off; ORDER BY bucket_start, meter, unit
//	                                           -> a.alpha | b.beta | m.mid | model.input_tokens | ...
//
// So the untied form is correct only for as long as the planner keeps choosing a sorting join — which
// it decides from row-count estimates that change as a tenant's ledger grows. That is precisely a
// partial order that looks total in the small. This test therefore asserts the sequence under BOTH
// plans: the default one, and one where the sorting join is unavailable. The second leg is what makes
// the mutation fail, and disabling a join strategy is not a fabricated configuration — it is one of the
// plans the planner is free to pick on its own.
func TestUsageSeriesOrdersTotallyWithinABucket(t *testing.T) {
	cs := openHarness(t)
	store := metering.New(cs.Pool())
	projectA, _ := tenant(t, cs)

	base := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	// Eight meters in ONE bucket. The names are deliberately not the four the coordinator settles: the
	// meter column is free text and this test is about the ORDER of whatever is there, not the
	// vocabulary. They are chosen so hash order and sorted order visibly disagree (see above).
	seeded := []struct{ meter, unit string }{
		{"z.omega", "x"}, {"model.output_tokens", "token"}, {"a.alpha", "x"}, {"run.admitted", "run"},
		{"m.mid", "x"}, {"model.input_tokens", "token"}, {"step.interrupted", "step"}, {"b.beta", "x"},
	}
	for i, s := range seeded {
		settleAt(t, cs, projectA, s.meter, s.unit, float64(i+1), base.Add(time.Duration(i)*time.Minute))
	}

	q := api.UsageSeriesQuery{Bucket: "hour", Start: base, End: base.Add(30 * time.Minute)}
	want := []string{
		"a.alpha", "b.beta", "m.mid", "model.input_tokens",
		"model.output_tokens", "run.admitted", "step.interrupted", "z.omega",
	}
	assertOrder := func(plan string) {
		got := readSeries(t, store, middleware.Scope{Project: projectA}, q)
		if len(got) != len(want) {
			t.Fatalf("[%s] one bucket returned %d points, want %d (eight meters in a single bucket)", plan, len(got), len(want))
		}
		for i := range want {
			if got[i].Meter != want[i] {
				order := make([]string, len(got))
				for j, p := range got {
					order[j] = p.Meter
				}
				t.Fatalf("[%s] point %d = %q, want %q — the within-bucket order is untied.\n got: %v\nwant: %v",
					plan, i, got[i].Meter, want[i], order, want)
			}
		}
	}
	assertOrder("default plan")

	// Now take away the sorting join, so the ordering can only come from the ORDER BY itself.
	ctx := storage.WithSystemScope(context.Background())
	if _, err := cs.Pool().Exec(ctx, `SET enable_mergejoin = off`); err != nil {
		t.Fatalf("SET enable_mergejoin: %v", err)
	}
	t.Cleanup(func() { _, _ = cs.Pool().Exec(ctx, `SET enable_mergejoin = on`) })
	assertOrder("merge join disabled")
}
