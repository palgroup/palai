//go:build component

package coordinator

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/storage"
)

// The session-list read side (E29, the Sessions screen). A list row must carry enough to RENDER the
// screen — label, the agent(s) the session's runs used, metered tokens, and the activity span — from ONE
// query, because a 50-row page that needs a follow-up per row is 50 requests.
//
// Every assertion here drives the SHIPPED Store.ListSessions against a real Postgres. Nothing is
// asserted about a projection built in the test: the four enrichments either come out of the SQL or
// they do not.

// TestSessionListRowCarriesLabelAgentTokensAndSpan is the whole enrichment in one seeded fixture,
// because the four columns share one query and a partial regression is exactly what a per-field test
// would miss. It seeds three sessions in one project and one decoy session in a SECOND project of the
// same organization, and asserts:
//
//   - NAME. An operator-supplied name wins and reports name_source=operator. A session with no name
//     falls back to the first non-retracted response's text, truncated, reporting name_source=derived.
//     A session with neither reports name_source=none and an empty name.
//   - RETRACTION. A session whose FIRST response was retracted derives from the second. A retracted
//     turn is words the human took back; SessionHistory already refuses to show them to the model
//     (storage/queries/sessions.sql), and a list screen is not a loophole around that.
//   - AGENT. The association is per-RUN (runs.agent_revision_id, migration 000019) — there is no
//     session→agent column anywhere — so the row carries the DISTINCT profile names of the session's
//     runs, sorted. A run pinned to a run_template_revision contributes NOTHING: 000019 states a
//     template must not impersonate an agent identity.
//   - TOKENS. Summed from usage_ledger (migration 000032), split input/output, and the decoy project's
//     rows must NOT be summed in: usage_ledger's RLS is ORGANIZATION-level with has_project=false, so
//     the project narrowing is the query's own job and a missing project predicate silently
//     over-counts every sibling project.
//   - SPAN. min(runs.created_at)/max(runs.updated_at) — both written by UpdateRunState
//     (storage/queries/responses.sql: `SET state = $4, updated_at = clock_timestamp()`). A session with
//     no runs has no span at all rather than a zero one.
func TestSessionListRowCarriesLabelAgentTokensAndSpan(t *testing.T) {
	cs, ctx := openSessionListStore(t)
	org := pinTestID("org")
	project, decoyProject := pinTestID("prj"), pinTestID("prj")
	mustExecPin(t, cs, `INSERT INTO organizations (id) VALUES ($1)`, org)
	mustExecPin(t, cs, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, project, org)
	mustExecPin(t, cs, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, decoyProject, org)

	profile, revision := pinTestID("agt"), pinTestID("arev")
	mustExecPin(t, cs, `INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,$4)`,
		profile, org, project, "Gece Doğrulama")
	mustExecPin(t, cs, `INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, published_at)
		VALUES ($1,$2,$3,$4,1,clock_timestamp())`, revision, org, project, profile)
	template := pinTestID("tmpl")
	mustExecPin(t, cs, `INSERT INTO run_template_revisions (id, organization_id, project_id, template_name, revision_number, published_at)
		VALUES ($1,$2,$3,$4,1,clock_timestamp())`, template, org, project, "nightly-template")

	base := time.Now().UTC().Add(-time.Hour)
	// named: an operator label, one agent-pinned run and one template-pinned run, metered tokens, a span.
	named := seedSession(t, cs, org, project, base, "Nightly verification")
	seedRun(t, cs, org, project, named, base, base.Add(90*time.Second), revision, "")
	seedRun(t, cs, org, project, named, base.Add(2*time.Minute), base.Add(3*time.Minute), "", template)
	seedLedger(t, cs, org, project, named, "model.input_tokens", 1200)
	seedLedger(t, cs, org, project, named, "model.output_tokens", 340)
	// The decoy project's rows carry the SAME session id on purpose: usage_ledger has no FK to sessions
	// (000032 says so — a settlement must outlive the run it settles), so a query that forgets the
	// project predicate sums these too.
	seedLedgerIn(t, cs, org, decoyProject, named, "model.input_tokens", 999999)

	// derived: no operator name; its first response was RETRACTED, so the label comes from the second.
	derived := seedSession(t, cs, org, project, base.Add(time.Minute), "")
	seedResponse(t, cs, org, project, derived, base.Add(time.Minute), `"the words the human took back"`, true)
	seedResponse(t, cs, org, project, derived, base.Add(2*time.Minute),
		`"`+strings.Repeat("uzun ", 40)+`"`, false)

	// bare: created and never used — no name, no response, no run, no meter.
	bare := seedSession(t, cs, org, project, base.Add(3*time.Minute), "")

	rows, err := cs.ListSessions(ctx, Tenant{Project: project}, ListParams{Limit: 10})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	byID := map[string]SessionView{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if len(byID) != 3 {
		t.Fatalf("page returned %d sessions, want 3 (ids=%v)", len(byID), byID)
	}

	got := byID[named]
	if got.Name != "Nightly verification" || got.NameSource != "operator" {
		t.Fatalf("named session label = %q/%q, want %q/operator", got.Name, got.NameSource, "Nightly verification")
	}
	if len(got.Agents) != 1 || got.Agents[0] != "Gece Doğrulama" {
		t.Fatalf("named session agents = %v, want exactly [Gece Doğrulama] — a template-pinned run must contribute no agent identity", got.Agents)
	}
	if got.InputTokens != 1200 || got.OutputTokens != 340 {
		t.Fatalf("named session tokens = in %d / out %d, want 1200/340 (a sibling project's 999999 must not be summed in)",
			got.InputTokens, got.OutputTokens)
	}
	if got.FirstActivityAt == nil || got.LastActivityAt == nil {
		t.Fatalf("named session span = %v..%v, want both set from its runs", got.FirstActivityAt, got.LastActivityAt)
	}
	if span := got.LastActivityAt.Sub(*got.FirstActivityAt); span != 3*time.Minute {
		t.Fatalf("named session span = %v, want 3m (first run created .. last run updated)", span)
	}

	got = byID[derived]
	if got.NameSource != "derived" {
		t.Fatalf("unnamed session name_source = %q, want derived", got.NameSource)
	}
	if strings.HasPrefix(got.Name, "the words") {
		t.Fatalf("unnamed session derived its label from a RETRACTED turn (%q); retraction is not undone by a list screen", got.Name)
	}
	if !strings.HasPrefix(got.Name, "uzun uzun") {
		t.Fatalf("unnamed session name = %q, want it derived from the first NON-retracted response", got.Name)
	}
	if n := len([]rune(got.Name)); n > derivedNameRunes {
		t.Fatalf("derived label is %d runes, want at most %d — a whole prompt is not a table cell", n, derivedNameRunes)
	}

	got = byID[bare]
	if got.Name != "" || got.NameSource != "none" {
		t.Fatalf("bare session label = %q/%q, want empty/none", got.Name, got.NameSource)
	}
	if len(got.Agents) != 0 {
		t.Fatalf("bare session agents = %v, want empty", got.Agents)
	}
	if got.FirstActivityAt != nil || got.LastActivityAt != nil {
		t.Fatalf("bare session span = %v..%v, want no span at all — a session that never ran has no duration, not a zero one",
			got.FirstActivityAt, got.LastActivityAt)
	}
}

// TestSessionListPagesTotallyOrderedAcrossAClockTie is the pagination half, and it is not decoration:
// the list orders by (created_at DESC, id DESC) and pages by that keyset, so a PARTIAL order at a page
// boundary skips or repeats rows. Three sessions share ONE created_at to the microsecond, and the two
// pages must together be the three rows, each exactly once.
//
// WHAT THIS TEST DOES NOT PROVE, stated because the tempting claim is false: it does not hold the
// statement's OUTER `ORDER BY p.created_at DESC, p.id DESC`. That line is a guard against a plan
// change, not an observed behaviour — every lateral is driven by a nested loop that preserves the
// CTE's order, so this test passes with the line deleted (measured 2026-07-31, three runs, all `ok`).
// The row-order assertion below is what CAN be checked; it would catch a reorder the planner actually
// performs, and nothing here can manufacture one.
func TestSessionListPagesTotallyOrderedAcrossAClockTie(t *testing.T) {
	cs, ctx := openSessionListStore(t)
	org, project := pinTestID("org"), pinTestID("prj")
	mustExecPin(t, cs, `INSERT INTO organizations (id) VALUES ($1)`, org)
	mustExecPin(t, cs, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, project, org)

	tie := time.Now().UTC().Truncate(time.Microsecond)
	want := map[string]bool{}
	for range 3 {
		want[seedSession(t, cs, org, project, tie, "")] = false
	}

	tenant := Tenant{Project: project}
	first, err := cs.ListSessions(ctx, tenant, ListParams{Limit: 2})
	if err != nil {
		t.Fatalf("ListSessions page 1: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("page 1 returned %d rows, want 2", len(first))
	}
	last := first[len(first)-1]
	second, err := cs.ListSessions(ctx, tenant, ListParams{Limit: 2, AfterCreatedAt: &last.CreatedAt, AfterID: last.ID})
	if err != nil {
		t.Fatalf("ListSessions page 2: %v", err)
	}
	assertDescending(t, first)
	assertDescending(t, second)
	for _, row := range append(first, second...) {
		seen, known := want[row.ID]
		if !known {
			t.Fatalf("paging returned an unknown session %q", row.ID)
		}
		if seen {
			t.Fatalf("session %q was returned on BOTH pages — the keyset order is not total", row.ID)
		}
		want[row.ID] = true
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("session %q was SKIPPED across the page boundary — the keyset order is not total", id)
		}
	}
}

// TestSessionRenameIsALabelNotAnIdentity proves the rename write path lands, and that two sessions may
// carry the SAME name. The reference screen shows several sessions sharing one label, so a unique index
// here would reject a legitimate rename; this test is what would fail if one were ever added.
func TestSessionRenameIsALabelNotAnIdentity(t *testing.T) {
	cs, ctx := openSessionListStore(t)
	org, project := pinTestID("org"), pinTestID("prj")
	mustExecPin(t, cs, `INSERT INTO organizations (id) VALUES ($1)`, org)
	mustExecPin(t, cs, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, project, org)

	now := time.Now().UTC()
	one := seedSession(t, cs, org, project, now, "")
	two := seedSession(t, cs, org, project, now.Add(time.Second), "")
	tenant := Tenant{Project: project}

	for _, id := range []string{one, two} {
		view, err := cs.RenameSession(ctx, tenant, id, "Gece Doğrulama")
		if err != nil {
			t.Fatalf("RenameSession(%s): %v", id, err)
		}
		if !view.Found || view.Name != "Gece Doğrulama" || view.NameSource != "operator" {
			t.Fatalf("RenameSession(%s) = %+v, want found with the operator label", id, view)
		}
	}

	// A foreign session id is a MISS, not an error and not a write: the same no-existence-disclosure
	// contract GetSession has.
	foreign := middlewareForeignSession(t, cs)
	view, err := cs.RenameSession(ctx, tenant, foreign, "stolen")
	if err != nil {
		t.Fatalf("RenameSession(foreign) error = %v, want a clean miss", err)
	}
	if view.Found {
		t.Fatal("RenameSession renamed a session belonging to another tenant")
	}
}

// assertDescending holds a page to the (created_at DESC, id DESC) order renderPage mints its cursor
// from — it takes the LAST row's position, so a page handed back out of order pages from the wrong
// place. See the test's own doc comment for what this can and cannot catch.
func assertDescending(t *testing.T, page []SessionView) {
	t.Helper()
	for i := 1; i < len(page); i++ {
		prev, cur := page[i-1], page[i]
		if cur.CreatedAt.After(prev.CreatedAt) || (cur.CreatedAt.Equal(prev.CreatedAt) && cur.ID >= prev.ID) {
			t.Fatalf("row %d (%s, %s) does not follow row %d (%s, %s) in (created_at DESC, id DESC)",
				i, cur.CreatedAt, cur.ID, i-1, prev.CreatedAt, prev.ID)
		}
	}
}

func openSessionListStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	cs, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return cs, ctx
}

func seedSession(t *testing.T, cs *Store, org, project string, createdAt time.Time, name string) string {
	t.Helper()
	id := pinTestID("ses")
	mustExecPin(t, cs, `INSERT INTO sessions (id, organization_id, project_id, name, created_at) VALUES ($1,$2,$3,$4,$5)`,
		id, org, project, name, createdAt)
	return id
}

func seedRun(t *testing.T, cs *Store, org, project, session string, createdAt, updatedAt time.Time, revision, template string) {
	t.Helper()
	var rev, tmpl *string
	if revision != "" {
		rev = &revision
	}
	if template != "" {
		tmpl = &template
	}
	mustExecPin(t, cs, `INSERT INTO runs (id, organization_id, project_id, session_id, state, created_at, updated_at, agent_revision_id, run_template_revision_id)
		VALUES ($1,$2,$3,$4,'completed',$5,$6,$7,$8)`,
		pinTestID("run"), org, project, session, createdAt, updatedAt, rev, tmpl)
}

func seedResponse(t *testing.T, cs *Store, org, project, session string, createdAt time.Time, inputJSON string, retracted bool) {
	t.Helper()
	var retractedAt *time.Time
	if retracted {
		at := createdAt.Add(time.Second)
		retractedAt = &at
	}
	mustExecPin(t, cs, `INSERT INTO responses (id, organization_id, project_id, session_id, state, input, created_at, retracted_at)
		VALUES ($1,$2,$3,$4,'completed',$5::jsonb,$6,$7)`,
		pinTestID("resp"), org, project, session, inputJSON, createdAt, retractedAt)
}

func seedLedger(t *testing.T, cs *Store, org, project, session, meter string, quantity int64) {
	t.Helper()
	seedLedgerIn(t, cs, org, project, session, meter, quantity)
}

func seedLedgerIn(t *testing.T, cs *Store, org, project, session, meter string, quantity int64) {
	t.Helper()
	mustExecPin(t, cs, `INSERT INTO usage_ledger (id, organization_id, project_id, session_id, meter, quantity, unit, dedupe_key)
		VALUES ($1,$2,$3,$4,$5,$6,'token',$7)`,
		pinTestID("use"), org, project, session, meter, quantity, pinTestID("dk"))
}

// middlewareForeignSession seeds a session in a whole second organization, so a rename addressed at it
// from the first tenant's scope is refused by RLS rather than by an id-shape check.
func middlewareForeignSession(t *testing.T, cs *Store) string {
	t.Helper()
	org, project := pinTestID("org"), pinTestID("prj")
	mustExecPin(t, cs, `INSERT INTO organizations (id) VALUES ($1)`, org)
	mustExecPin(t, cs, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, project, org)
	return seedSession(t, cs, org, project, time.Now().UTC(), "")
}

var _ = storage.WithSystemScope
