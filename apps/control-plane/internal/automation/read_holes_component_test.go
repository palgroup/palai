//go:build component

// External-package (automation_test) component test for the E29 T1 read holes, over real PostgreSQL and
// main.go's OWN NewRouter seam list. It lives in package automation_test so it can import api + store,
// which is also what lets ONE file drive both families this task opens: schedules (internal/automation)
// and hooks (internal/extensions, reached through the store adapter that implements api.HookAPI).
//
// WHY THE SHAPE OF EVERY TEST HERE IS "CREATE IT, THROW THE ID AWAY, FIND IT AGAIN".
//
// A management surface with a create and a singular GET but no list is not a surface an operator can use;
// it is a surface an operator can use ONCE, on the day they read the 201 body. The id is the only handle
// and nothing in the system will ever hand it back. So each test below deliberately discards what the
// create returned before it starts looking, because keeping the id would make the test pass against a tree
// that has no read path at all — which is exactly the tree these tests were first run against.
//
// The one exception is the Location leg, which keeps the header rather than the id: following a 201's
// Location IS the reader's other way home, and E29 T2 removed that header from POST /v1/hooks precisely
// because it addressed a route nothing mounted. This file is what puts it back honestly.
package automation_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
)

// readHolesFixture is one booted spine + one router carrying BOTH families' seams, plus a client per
// tenant. The second tenant is not decoration: every list route this task opens is a new way to ask the
// database for rows, and a new way to ask is a new way to ask for someone else's.
type readHolesFixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	base    string
	a, b    *client
	orgA    string
	projA   string
	trigger string
}

func newReadHolesFixture(t *testing.T) *readHolesFixture {
	t.Helper()
	url := envOrSkip(t)
	ctx := context.Background()
	repo, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := repo.Spine().Pool()

	tokenA, tokenB := randID("tok"), randID("tok")
	orgA, projA := seedScopedTenant(t, pool, tokenA)
	seedScopedTenant(t, pool, tokenB)

	// main.go's own seam list, with the hooks seam wired (repo implements api.HookAPI through
	// internal/store/hooks.go). The schedule ticker is deliberately NOT started: this file tests the READ
	// surface, and a background loop that mutates rows mid-page would make a keyset assertion flaky for a
	// reason that has nothing to do with what is being proven.
	webhookStore := automation.NewWebhookStore(pool)
	triggerStore := automation.NewTriggerStore(pool).WithAdmitter(repo.Spine())
	scheduleStore := automation.NewScheduleStore(pool, triggerStore)
	router := api.NewRouter(repo, repo, repo, repo, repo, repo, webhookStore, triggerStore, scheduleStore, nil, nil, nil, repo, nil, nil, api.SSEConfig{}, nil, nil)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	f := &readHolesFixture{
		t:     t,
		pool:  pool,
		base:  srv.URL,
		a:     &client{t: t, base: srv.URL, token: tokenA},
		b:     &client{t: t, base: srv.URL, token: tokenB},
		orgA:  orgA,
		projA: projA,
	}
	f.trigger = f.a.createCronTrigger()
	return f
}

// createNamedSchedule POSTs a schedule and returns ONLY its name. The id is read and dropped on purpose —
// see the file comment. A caller that needs the id has to say so.
func (f *readHolesFixture) createNamedSchedule(c *client, name string) string {
	f.t.Helper()
	c.createSchedule(`{"name":"` + name + `","trigger_id":"` + f.trigger + `","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	return name
}

// getJSON performs an authenticated GET and returns the status plus the decoded body. Unlike client.get it
// never fails the test on a non-2xx: every test in this file has a status it is specifically asserting.
func getJSON(t *testing.T, c *client, path string) (int, map[string]any, string) {
	t.Helper()
	resp := c.get(path)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

// pageRows reads the shared contracts.Page envelope's data array as objects, and FAILS if the response is
// not that envelope. That failure is the point on the truncation leg: a body with no `data` and no
// `has_more` cannot say it was cut, which is the whole defect §3.6 D12 names.
func pageRows(t *testing.T, body map[string]any, path string, raw string) ([]map[string]any, bool, string) {
	t.Helper()
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("GET %s returned no `data` array — the body is %s.\n"+
			"A list that answers outside the shared page envelope cannot carry has_more or next_cursor, "+
			"so a client has no way to learn it was truncated: the hundred-and-first row is indistinguishable "+
			"from no hundred-and-first row.", path, truncate(raw))
	}
	rows := make([]map[string]any, 0, len(data))
	for _, d := range data {
		row, _ := d.(map[string]any)
		rows = append(rows, row)
	}
	hasMore, _ := body["has_more"].(bool)
	cursor, _ := body["next_cursor"].(string)
	return rows, hasMore, cursor
}

func truncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

// findByField scans page rows for one whose `field` equals want.
func findByField(rows []map[string]any, field, want string) (map[string]any, bool) {
	for _, row := range rows {
		if v, _ := row[field].(string); v == want {
			return row, true
		}
	}
	return nil, false
}

// TestAScheduleIsFindableWithoutTheIDTheCreateReturned is the headline read hole. A schedule is created
// through the public API alone, its id is DISCARDED, and the test tries to find it again.
//
// Before this task that was impossible: the family mounted POST, GET-by-id, PATCH, pause, resume, DELETE
// and the occurrence log, and no route that enumerates. A schedule fires on a wall clock with nobody
// watching, so "the operator lost the id" is not a hypothetical — it is the state every deployment reaches
// the first time a schedule outlives the terminal that created it.
func TestAScheduleIsFindableWithoutTheIDTheCreateReturned(t *testing.T) {
	f := newReadHolesFixture(t)
	name := f.createNamedSchedule(f.a, randID("nightly"))

	status, body, raw := getJSON(t, f.a, "/v1/schedules?limit=100")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/schedules status = %d (body %s), want 200.\n"+
			"A schedule created through the public API can be found again ONLY by an id the caller kept: "+
			"the family mounts a create and a singular read and nothing that enumerates.", status, truncate(raw))
	}
	rows, _, _ := pageRows(t, body, "/v1/schedules", raw)
	row, found := findByField(rows, "name", name)
	if !found {
		t.Fatalf("the schedule %q is not in the %d row(s) GET /v1/schedules returned", name, len(rows))
	}

	// THE LIST ROW AND THE SINGULAR READ ARE THE SAME SHAPE, field for field. A list row that carried a
	// different projection would make every screen code against two shapes and would make the singular GET
	// the only place some field can be learned — which is the hole this task is closing, re-opened one
	// field at a time.
	id, _ := row["id"].(string)
	if id == "" {
		t.Fatalf("list row carries no id: %v", row)
	}
	singularStatus, singular, singularRaw := getJSON(t, f.a, "/v1/schedules/"+id)
	if singularStatus != http.StatusOK {
		t.Fatalf("GET /v1/schedules/%s status = %d (%s), want 200", id, singularStatus, truncate(singularRaw))
	}
	assertSameShape(t, "schedule", row, singular)
}

// assertSameShape fails when a list row and the singular read of the same resource disagree on ANY key or
// value. It compares both directions: a key the list omits is a field only the singular read can teach,
// and a key only the list carries is a field the singular read cannot confirm.
func assertSameShape(t *testing.T, kind string, row, singular map[string]any) {
	t.Helper()
	rowJSON, _ := json.Marshal(row)
	singularJSON, _ := json.Marshal(singular)
	for k, want := range singular {
		got, present := row[k]
		if !present {
			t.Fatalf("%s list row is MISSING %q, which the singular read returns.\n  list:     %s\n  singular: %s\n"+
				"A list projection that differs from the singular one makes the screen code against two shapes.",
				kind, k, rowJSON, singularJSON)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s list row %q = %v, singular read = %v", kind, k, got, want)
		}
	}
	for k := range row {
		if _, present := singular[k]; !present {
			t.Fatalf("%s list row carries %q, which the singular read does not.\n  list:     %s\n  singular: %s",
				kind, k, rowJSON, singularJSON)
		}
	}
}

// TestAHookIsFindableWithoutTheIDTheCreateReturned is the same hole, one family over and worse: hooks
// mounted POST and POST-disable and NOT ONE GET. A registered hook fires inside every run's dispatch loop
// and could be neither enumerated nor read back — `disable` was a write whose only confirmation was its own
// 200.
//
// It also follows the 201's Location. E29 T2 deleted that header from this handler because it named
// `/v1/hooks/{id}`, an address nothing served; the comment it left says the header comes back when the GET
// lands. This is the leg that holds it to that: the header is followed, not read.
func TestAHookIsFindableWithoutTheIDTheCreateReturned(t *testing.T) {
	f := newReadHolesFixture(t)
	name := randID("guard")

	resp := postRaw(t, f.a, "/v1/hooks", `{"name":"`+name+`","hook_point":"before_tool","category":"policy","executor":"platform_inline","config":{"handler":"deny_tool"}}`)
	if resp.status != http.StatusCreated {
		t.Fatalf("POST /v1/hooks status = %d (%s), want 201", resp.status, truncate(resp.raw))
	}
	location := resp.header.Get("Location")
	if location == "" {
		t.Fatalf("POST /v1/hooks wrote no Location.\n" +
			"E29 T2 removed it because `/v1/hooks/{id}` was an address nothing mounted, and left this in the " +
			"handler: \"When GET /v1/hooks/{id} lands, this header comes back with it and the guard accepts it.\" " +
			"The GET is this task; the header is owed with it.")
	}

	// FOLLOW the header. Reading it and matching it against the route table is what this repository has
	// shipped before and it is how seven dangling addresses survived: a route table is a string until
	// something dispatches on it.
	status, singular, singularRaw := getJSON(t, f.a, location)
	if status != http.StatusOK {
		t.Fatalf("following the create's own Location %q gave %d (%s), want 200 — a 201 that names an address "+
			"a client cannot dereference has told the client nothing.", location, status, truncate(singularRaw))
	}
	if got, _ := singular["name"].(string); got != name {
		t.Fatalf("Location %q resolved to a hook named %q, want %q", location, got, name)
	}

	// And now the list, with the id discarded.
	listStatus, body, raw := getJSON(t, f.a, "/v1/hooks?limit=100")
	if listStatus != http.StatusOK {
		t.Fatalf("GET /v1/hooks status = %d (%s), want 200.\n"+
			"The hooks family mounts POST and POST-disable and not one enumerating route, so a hook that fires "+
			"inside every run of this project cannot be listed by the operator who registered it.", listStatus, truncate(raw))
	}
	rows, _, _ := pageRows(t, body, "/v1/hooks", raw)
	row, found := findByField(rows, "name", name)
	if !found {
		t.Fatalf("the hook %q is not in the %d row(s) GET /v1/hooks returned", name, len(rows))
	}
	assertSameShape(t, "hook", row, singular)
}

// TestADisabledHookIsStillListedAndSaysSo is why HooksForPoint could not have been the list. That query
// takes a point and returns only ENABLED hooks — it is the dispatch loop's read. A management list that
// inherited it would make `disable` a write with NO read-back at all: the hook would simply vanish, which
// is indistinguishable from a hook that was deleted, and hooks cannot be deleted.
func TestADisabledHookIsStillListedAndSaysSo(t *testing.T) {
	f := newReadHolesFixture(t)
	name := randID("guard")
	created := f.a.postJSON("/v1/hooks", `{"name":"`+name+`","hook_point":"before_tool","category":"policy","executor":"platform_inline","config":{"handler":"deny_tool"}}`, "")
	id, _ := created["id"].(string)
	f.a.postJSON("/v1/hooks/"+id+"/disable", ``, "")

	_, body, raw := getJSON(t, f.a, "/v1/hooks?limit=100")
	rows, _, _ := pageRows(t, body, "/v1/hooks", raw)
	row, found := findByField(rows, "name", name)
	if !found {
		t.Fatalf("a DISABLED hook %q vanished from GET /v1/hooks (%d row(s)).\n"+
			"Disable is the only write on this family and the list is its only read-back; a list that hides "+
			"disabled rows makes the kill-switch unobservable.", name, len(rows))
	}
	if disabled, _ := row["disabled"].(bool); !disabled {
		t.Fatalf("hook %q lists as disabled=%v after POST /v1/hooks/{id}/disable returned 200: %v", name, row["disabled"], row)
	}
	if at, _ := row["disabled_at"].(string); at == "" {
		t.Fatalf("hook %q carries no disabled_at: %v", name, row)
	}
}

// TestASoftDeletedScheduleIsGoneFromBothTheListAndTheSingularRead is §3.6 D15. DeleteSchedule is a SOFT
// delete — the row stays so its occurrences and deliveries survive retention — and the easiest possible
// bug in a brand-new list query is to read the table and resurrect every tombstone. GetSchedule has
// filtered `deleted_at IS NULL` since E11; if the two disagree the deployment holds two different truths
// about whether a schedule exists, and the one the screen shows is the wrong one.
func TestASoftDeletedScheduleIsGoneFromBothTheListAndTheSingularRead(t *testing.T) {
	f := newReadHolesFixture(t)
	name := randID("doomed")
	id := f.a.createSchedule(`{"name":"` + name + `","trigger_id":"` + f.trigger + `","cron_expr":"0 2 * * *","timezone":"UTC"}`)

	if req := deleteReq(t, f.a, "/v1/schedules/"+id); req != http.StatusNoContent {
		t.Fatalf("DELETE /v1/schedules/%s status = %d, want 204", id, req)
	}

	singular, _, _ := getJSON(t, f.a, "/v1/schedules/"+id)
	if singular != http.StatusNotFound {
		t.Fatalf("GET /v1/schedules/%s after DELETE = %d, want 404", id, singular)
	}
	_, body, raw := getJSON(t, f.a, "/v1/schedules?limit=100")
	rows, _, _ := pageRows(t, body, "/v1/schedules", raw)
	if _, found := findByField(rows, "name", name); found {
		t.Fatalf("the soft-deleted schedule %q is STILL in GET /v1/schedules.\n"+
			"The singular read answers 404 for it, so the list and the read disagree about whether it exists — "+
			"and a list that reads the table without `deleted_at IS NULL` resurrects every tombstone.", name)
	}
}

// TestTheOccurrencesEnvelopeSaysWhenItTruncated is §3.6 D12. The route passed limit=0, the store clamped
// that to 100, and the handler wrote `{"occurrences":[…]}` — no data, no has_more, no next_cursor. A
// schedule firing every minute crosses a hundred occurrences in under two hours, and from the hundred-and-
// first onward the API answered as if the schedule's history simply stopped.
func TestTheOccurrencesEnvelopeSaysWhenItTruncated(t *testing.T) {
	f := newReadHolesFixture(t)
	id := f.a.createSchedule(`{"name":"` + randID("busy") + `","trigger_id":"` + f.trigger + `","cron_expr":"* * * * *","timezone":"UTC"}`)
	seedOccurrences(t, f.pool, id, 101)

	// The DEFAULT page: whatever the shared parse decides, the answer must SAY it was cut.
	status, body, raw := getJSON(t, f.a, "/v1/schedules/"+id+"/occurrences")
	if status != http.StatusOK {
		t.Fatalf("GET occurrences status = %d (%s), want 200", status, truncate(raw))
	}
	rows, hasMore, cursor := pageRows(t, body, "/v1/schedules/"+id+"/occurrences", raw)
	if !hasMore {
		t.Fatalf("101 occurrences returned %d row(s) with has_more=false.\n"+
			"The hundred-and-first is then indistinguishable from nothing, and a screen reading this body "+
			"cannot tell the operator the history was cut.", len(rows))
	}
	if cursor == "" {
		t.Fatal("has_more is true and next_cursor is empty: the caller is told there is more and given no way to reach it")
	}

	// PAGE THROUGH ALL 101 AND COUNT. has_more is only worth something if the cursor it comes with actually
	// advances; a cursor that returns the same page forever satisfies every assertion above.
	seen := map[string]bool{}
	pages := 0
	path := "/v1/schedules/" + id + "/occurrences?limit=25"
	for {
		pages++
		if pages > 20 {
			t.Fatalf("paging did not terminate after %d pages (%d distinct occurrences seen)", pages, len(seen))
		}
		st, pageBody, pageRaw := getJSON(t, f.a, path)
		if st != http.StatusOK {
			t.Fatalf("page %d status = %d (%s)", pages, st, truncate(pageRaw))
		}
		pageOf, more, next := pageRows(t, pageBody, path, pageRaw)
		for _, row := range pageOf {
			occID, _ := row["occurrence_id"].(string)
			if seen[occID] {
				t.Fatalf("occurrence %q appeared on two pages — the keyset is not a total order", occID)
			}
			seen[occID] = true
		}
		if !more {
			break
		}
		path = "/v1/schedules/" + id + "/occurrences?limit=25&after=" + url.QueryEscape(next)
	}
	if len(seen) != 101 {
		t.Fatalf("paging the occurrence log saw %d distinct rows across %d pages, want 101", len(seen), pages)
	}
}

// TestTheOccurrenceKeysetHoldsAcrossAPlannedAtTie is the §2 total-ordering requirement, driven rather than
// read. Two occurrences of the SAME schedule at the same planned instant are reachable — the uniqueness
// constraint is (schedule, revision, planned_at), so two revisions can plan the same minute — and an
// `ORDER BY planned_at DESC` with no tiebreaker leaves the row at a page boundary free to be skipped or
// repeated. This repository has had an unordered LIMIT decide a security outcome twice.
func TestTheOccurrenceKeysetHoldsAcrossAPlannedAtTie(t *testing.T) {
	f := newReadHolesFixture(t)
	id := f.a.createSchedule(`{"name":"` + randID("tied") + `","trigger_id":"` + f.trigger + `","cron_expr":"* * * * *","timezone":"UTC"}`)

	// Six occurrences, all at ONE instant, across six revisions: every page boundary is a tie.
	instant := time.Now().UTC().Truncate(time.Second)
	for rev := 1; rev <= 6; rev++ {
		mustExec(t, f.pool,
			`INSERT INTO schedule_occurrences (occurrence_id, schedule_id, schedule_revision, planned_at, state)
			 VALUES ($1, $2, $3, $4, 'pending')`,
			fmt.Sprintf("occ_tie_%s_%d", id, rev), id, rev, instant)
	}

	seen := map[string]bool{}
	path := "/v1/schedules/" + id + "/occurrences?limit=2"
	for i := 0; i < 10; i++ {
		st, body, raw := getJSON(t, f.a, path)
		if st != http.StatusOK {
			t.Fatalf("tie page status = %d (%s)", st, truncate(raw))
		}
		rows, more, next := pageRows(t, body, path, raw)
		for _, row := range rows {
			occID, _ := row["occurrence_id"].(string)
			if seen[occID] {
				t.Fatalf("occurrence %q repeated across a page boundary where every row shares planned_at — "+
					"the ORDER BY has no total tiebreaker", occID)
			}
			seen[occID] = true
		}
		if !more {
			break
		}
		path = "/v1/schedules/" + id + "/occurrences?limit=2&after=" + url.QueryEscape(next)
	}
	if len(seen) != 6 {
		t.Fatalf("paging six occurrences that share one planned_at saw %d distinct rows, want 6 — "+
			"a partial order at a page boundary SKIPS rows as readily as it repeats them", len(seen))
	}
}

// TestTheScheduleListPagesTotallyOrderedAcrossAClockTie is the same claim for the schedule list itself:
// created_at is a clock_timestamp() and two creates inside one microsecond tie.
func TestTheScheduleListPagesTotallyOrderedAcrossAClockTie(t *testing.T) {
	f := newReadHolesFixture(t)
	tied := time.Now().UTC()
	for i := 0; i < 6; i++ {
		id := f.a.createSchedule(`{"name":"` + randID("tied-sch") + `","trigger_id":"` + f.trigger + `","cron_expr":"0 2 * * *","timezone":"UTC"}`)
		mustExec(t, f.pool, `UPDATE schedules SET created_at = $2 WHERE id = $1`, id, tied)
	}

	seen := map[string]bool{}
	path := "/v1/schedules?limit=2"
	for i := 0; i < 10; i++ {
		st, body, raw := getJSON(t, f.a, path)
		if st != http.StatusOK {
			t.Fatalf("tie page status = %d (%s)", st, truncate(raw))
		}
		rows, more, next := pageRows(t, body, path, raw)
		for _, row := range rows {
			rowID, _ := row["id"].(string)
			if seen[rowID] {
				t.Fatalf("schedule %q repeated across a page boundary where every row shares created_at", rowID)
			}
			seen[rowID] = true
		}
		if !more {
			break
		}
		path = "/v1/schedules?limit=2&after=" + url.QueryEscape(next)
	}
	if len(seen) != 6 {
		t.Fatalf("paging six schedules created at ONE instant saw %d distinct rows, want 6", len(seen))
	}
}

// TestTheScheduleListFiltersOnStatusAndRefusesAnUnknownOne is §3.6 D14. statusFilterKinds' own defining
// sentence is "the list kinds that carry a lifecycle-state column ?status= can filter on", and schedules
// carries one (000022:48, CHECK (status IN ('active','paused','failed'))) — so the map's rule included
// schedules while its membership did not. A paused schedule is the one thing an operator filters for.
//
// The negative half matters more than the positive: an unknown status must be refused BY THE ROUTE. If the
// route passes it to SQL, `?status=banana` is an empty 200 — a client that believes it filtered acting on
// a page that means "no such state", which reads identically to "none in that state".
func TestTheScheduleListFiltersOnStatusAndRefusesAnUnknownOne(t *testing.T) {
	f := newReadHolesFixture(t)
	activeName := randID("active-sch")
	pausedName := randID("paused-sch")
	f.createNamedSchedule(f.a, activeName)
	pausedID := f.a.createSchedule(`{"name":"` + pausedName + `","trigger_id":"` + f.trigger + `","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	f.a.postJSON("/v1/schedules/"+pausedID+"/pause", ``, "")

	status, body, raw := getJSON(t, f.a, "/v1/schedules?status=paused&limit=100")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/schedules?status=paused = %d (%s), want 200.\n"+
			"statusFilterKinds rejects ?status= for every kind not in it, and schedules was not in it — "+
			"while the map's own defining sentence describes exactly the column schedules carries.", status, truncate(raw))
	}
	rows, _, _ := pageRows(t, body, "/v1/schedules?status=paused", raw)
	if _, found := findByField(rows, "name", pausedName); !found {
		t.Fatalf("?status=paused did not return the paused schedule %q (%d row(s))", pausedName, len(rows))
	}
	if _, found := findByField(rows, "name", activeName); found {
		t.Fatalf("?status=paused ALSO returned the active schedule %q — the filter was accepted and ignored, "+
			"which is worse than refusing it", activeName)
	}

	// An unknown value is a 400 written by the route.
	badStatus, badBody, badRaw := getJSON(t, f.a, "/v1/schedules?status=banana")
	if badStatus != http.StatusBadRequest {
		t.Fatalf("GET /v1/schedules?status=banana = %d (%s), want 400.\n"+
			"A status outside the column's CHECK must be refused at the edge; answering 200 with an empty page "+
			"tells a client 'none are banana' when the truth is 'banana is not a state'.", badStatus, truncate(badRaw))
	}
	if code, _ := badBody["code"].(string); code != "invalid_request" {
		t.Fatalf("?status=banana problem code = %q, want invalid_request (body %s)", code, truncate(badRaw))
	}
}

// TestAForeignTenantReadsNeitherListNorCursor is the tenancy half at the surface an operator uses. Three
// legs, and the third is the one that has cost this tree before: a cursor is an OPAQUE token, so a client
// holding one from another tenant must be REFUSED rather than handed a silently-empty page (the TEN-001
// cursor-fuzz contract, api/pagination.go:178). An empty 200 reads as "that tenant has nothing".
func TestAForeignTenantReadsNeitherListNorCursor(t *testing.T) {
	f := newReadHolesFixture(t)
	scheduleName := randID("private-sch")
	hookName := randID("private-hook")
	f.createNamedSchedule(f.a, scheduleName)
	created := f.a.postJSON("/v1/hooks", `{"name":"`+hookName+`","hook_point":"before_tool","category":"policy","executor":"platform_inline","config":{"handler":"deny_tool"}}`, "")
	hookID, _ := created["id"].(string)
	// A SECOND row in each family, because a cursor only exists on a page that was truncated. Without it
	// mintCursor returns "" and the fuzz leg below would skip itself while the test still reported PASS.
	f.createNamedSchedule(f.a, randID("private-sch"))
	f.a.postJSON("/v1/hooks", `{"name":"`+randID("private-hook")+`","hook_point":"before_tool","category":"policy","executor":"platform_inline","config":{"handler":"deny_tool"}}`, "")

	// Tenant B's own lists must not carry tenant A's rows.
	_, body, raw := getJSON(t, f.b, "/v1/schedules?limit=100")
	rows, _, _ := pageRows(t, body, "/v1/schedules", raw)
	if _, found := findByField(rows, "name", scheduleName); found {
		t.Fatalf("tenant B's GET /v1/schedules returned tenant A's schedule %q", scheduleName)
	}
	_, hookBody, hookRaw := getJSON(t, f.b, "/v1/hooks?limit=100")
	hookRows, _, _ := pageRows(t, hookBody, "/v1/hooks", hookRaw)
	if _, found := findByField(hookRows, "name", hookName); found {
		t.Fatalf("tenant B's GET /v1/hooks returned tenant A's hook %q", hookName)
	}

	// The singular reads are 404, not 403: an existence disclosure is a disclosure.
	if st, _, _ := getJSON(t, f.b, "/v1/hooks/"+hookID); st != http.StatusNotFound {
		t.Fatalf("tenant B GET /v1/hooks/%s = %d, want 404", hookID, st)
	}

	// A cursor minted for tenant A, presented by tenant B, on each of the three new list kinds.
	scheduleCursor := mintCursor(t, f.a, "/v1/schedules?limit=1")
	hookCursor := mintCursor(t, f.a, "/v1/hooks?limit=1")
	for _, tc := range []struct{ path, cursor string }{
		{"/v1/schedules", scheduleCursor},
		{"/v1/hooks", hookCursor},
	} {
		if tc.cursor == "" {
			t.Fatalf("tenant A's %s returned no next_cursor to fuzz with — seed more rows", tc.path)
		}
		st, probe, probeRaw := getJSON(t, f.b, tc.path+"?after="+url.QueryEscape(tc.cursor))
		if st != http.StatusBadRequest {
			t.Fatalf("tenant B presenting tenant A's %s cursor = %d (%s), want 400 — a foreign cursor is an "+
				"EXPLICIT reject, never a silently-empty page.", tc.path, st, truncate(probeRaw))
		}
		if code, _ := probe["code"].(string); code != "invalid_cursor" {
			t.Fatalf("%s foreign-cursor problem code = %q, want invalid_cursor", tc.path, code)
		}
	}
}

// mintCursor asks for a one-row page and returns the next_cursor it hands back. It creates a second row
// first so has_more is reachable.
func mintCursor(t *testing.T, c *client, path string) string {
	t.Helper()
	_, body, raw := getJSON(t, c, path)
	_, _, cursor := pageRows(t, body, path, raw)
	return cursor
}

// seedOccurrences drops n distinct occurrences onto a schedule, one per minute going back from now. They
// are seeded rather than ticked because what is under test is the READ envelope, not the ticker.
func seedOccurrences(t *testing.T, pool *pgxpool.Pool, scheduleID string, n int) {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Minute)
	for i := 0; i < n; i++ {
		mustExec(t, pool,
			`INSERT INTO schedule_occurrences (occurrence_id, schedule_id, schedule_revision, planned_at, state)
			 VALUES ($1, $2, 1, $3, 'pending')`,
			fmt.Sprintf("occ_%s_%03d", scheduleID, i), scheduleID, base.Add(-time.Duration(i)*time.Minute))
	}
}

// rawResponse is one authenticated request's status, headers and body — the shape a Location leg needs,
// which the map-returning helpers throw away.
type rawResponse struct {
	status int
	header http.Header
	raw    string
}

func postRaw(t *testing.T, c *client, path, body string) rawResponse {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s error = %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return rawResponse{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
}

func deleteReq(t *testing.T, c *client, path string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, c.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s error = %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
