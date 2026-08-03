//go:build component

// Real-PostgreSQL component tests for webhook-endpoint deletion and the two list orderings (E29 T3).
// They run under `make test-component TEST=postgres`, which starts a throwaway container and exports
// PALAI_COMPONENT_POSTGRES_URL — the automation leg of that tier carries NO -run allow-list, so a test
// added to this package runs there without a second registration.
//
// WHAT WAS WRONG. A webhook endpoint could not be deleted: no route, no store method, no SQL. The create's
// own comment justified taking no Idempotency-Key on the grounds that "a duplicate endpoint is
// operator-visible + deletable", and only the first half of that was true.
//
// AND WHAT THE PLAN GOT WRONG ABOUT IT. The plan said to verify there is no ON DELETE CASCADE. There was
// one — webhook_deliveries.endpoint_id has carried `ON DELETE CASCADE` since 000020 — and
// delivery_attempts.delivery_id cascades off THAT, so the delete this task opens would have taken the whole
// audit trail with it in two hops, silently. That is E28's runner_pool_keys lesson arriving a second time,
// and it is why TestNoForeignKeyCascadesOutOfAWebhookEndpoint reads the CATALOGUE rather than the migration
// text: the authority on what a delete takes with it is pg_constraint, not a file somebody remembers.
package automation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// ---- deletion --------------------------------------------------------------------------------------

// TestASurplusWebhookEndpointIsDeletedAndItsTwinSurvives is the shape the create comment describes: two
// identical endpoints exist because the create is deliberately not idempotent, and the operator removes the
// one they did not mean to make. Identical URLs are the point — nothing distinguishes the two rows but
// their ids, so the delete has to be addressed by id and has to take exactly one row.
func TestASurplusWebhookEndpointIsDeletedAndItsTwinSurvives(t *testing.T) {
	pool := componentPool(t)
	store := NewWebhookStore(pool)
	ctx := context.Background()
	_, project, _ := seedSession(t, pool)

	const url = "https://hooks.example/the-same-address"
	keep, err := store.CreateEndpoint(ctx, project, defaultEndpoint(url, "ref"))
	if err != nil {
		t.Fatalf("CreateEndpoint(keep) error = %v", err)
	}
	surplus, err := store.CreateEndpoint(ctx, project, defaultEndpoint(url, "ref"))
	if err != nil {
		t.Fatalf("CreateEndpoint(surplus) error = %v", err)
	}
	if keep == surplus {
		t.Fatal("the two creates returned one id; this test needs the non-idempotent create it is about")
	}

	deleted, err := store.DeleteEndpoint(ctx, project, surplus)
	if err != nil {
		t.Fatalf("DeleteEndpoint() error = %v", err)
	}
	if !deleted {
		t.Fatal("DeleteEndpoint reported no row; the surplus endpoint is still registered and still receiving")
	}

	// DELETE is idempotent: the second call changes nothing and says so.
	again, err := store.DeleteEndpoint(ctx, project, surplus)
	if err != nil {
		t.Fatalf("second DeleteEndpoint() error = %v", err)
	}
	if again {
		t.Fatal("the second delete claimed to remove a row that was already gone")
	}

	listed, err := store.ListEndpoints(ctx, project)
	if err != nil {
		t.Fatalf("ListEndpoints() error = %v", err)
	}
	if len(listed) != 1 || listed[0].ID != keep {
		t.Fatalf("after deleting the surplus the list is %v, want exactly [%s]", ids(listed), keep)
	}
	if _, found, err := store.GetEndpoint(ctx, project, surplus); err != nil || found {
		t.Fatalf("GetEndpoint(deleted) = found %v, err %v; want not-found", found, err)
	}
}

// TestDeletingAWebhookEndpointLeavesEveryDeliveryAndAttemptOfItIntact is the task's central decision, and
// the one the plan was wrong about the schema of.
//
// A delivery row is an AUDIT TRAIL: it records that this deployment sent this event to this address at this
// time and got this back. Removing the endpoint is a statement about the FUTURE — stop sending — and it does
// not entitle anybody to erase what was already sent. So the endpoint row goes, the deliveries stay, and
// their endpoint_id no longer resolves. That last part is deliberate and visible rather than tidied away:
// there is no ON DELETE SET NULL here either, because the id is evidence of where the payload went.
//
// The pump stops serving those rows on its own, with no new code: DueDeliveries INNER JOINs
// webhook_endpoints, so an orphaned pending delivery drops out of the due scan. That is the enforcement of
// "stop sending", and it is asserted here rather than assumed.
func TestDeletingAWebhookEndpointLeavesEveryDeliveryAndAttemptOfItIntact(t *testing.T) {
	pool := componentPool(t)
	store := NewWebhookStore(pool)
	ctx := context.Background()
	org, project, session := seedSession(t, pool)

	endpoint, err := store.CreateEndpoint(ctx, project, defaultEndpoint("https://hooks.example/audit", "ref"))
	if err != nil {
		t.Fatalf("CreateEndpoint() error = %v", err)
	}

	// Three deliveries in the three states a real trail carries, each with an attempt row beneath it, so the
	// second cascade hop (delivery_attempts.delivery_id) is covered too.
	var deliveryIDs []string
	for i, state := range []string{"delivered", "dead", "pending"} {
		id := randID("whd")
		deliveryIDs = append(deliveryIDs, id)
		mustExec(t, pool, `INSERT INTO webhook_deliveries (id, organization_id, project_id, endpoint_id, session_id, event_id, event_type, state)
			VALUES ($1,$2,$3,$4,$5,$6,'run.completed.v1',$7)`,
			id, org, project, endpoint, session, randID("evt"), state)
		mustExec(t, pool, `INSERT INTO delivery_attempts (delivery_id, attempt_number, status_code) VALUES ($1,$2,$3)`,
			id, i+1, 200)
	}
	deliveriesBefore := countRows(t, pool, `SELECT count(*) FROM webhook_deliveries WHERE endpoint_id = $1`, endpoint)
	attemptsBefore := countRows(t, pool,
		`SELECT count(*) FROM delivery_attempts a JOIN webhook_deliveries d ON d.id = a.delivery_id WHERE d.endpoint_id = $1`, endpoint)
	if deliveriesBefore != 3 || attemptsBefore != 3 {
		t.Fatalf("fixture is %d deliveries / %d attempts, want 3/3", deliveriesBefore, attemptsBefore)
	}

	if _, err := store.DeleteEndpoint(ctx, project, endpoint); err != nil {
		t.Fatalf("DeleteEndpoint() error = %v", err)
	}

	deliveriesAfter := countRows(t, pool, `SELECT count(*) FROM webhook_deliveries WHERE endpoint_id = $1`, endpoint)
	if deliveriesAfter != deliveriesBefore {
		t.Fatalf("deleting the endpoint took %d of its %d delivery rows with it. A delivery is an audit record of "+
			"something this deployment ALREADY SENT; removing the endpoint stops future sends and erases no history. "+
			"Check for an ON DELETE CASCADE on webhook_deliveries.endpoint_id.",
			deliveriesBefore-deliveriesAfter, deliveriesBefore)
	}
	attemptsAfter := countRows(t, pool,
		`SELECT count(*) FROM delivery_attempts a JOIN webhook_deliveries d ON d.id = a.delivery_id WHERE d.endpoint_id = $1`, endpoint)
	if attemptsAfter != attemptsBefore {
		t.Fatalf("deleting the endpoint took %d of its %d attempt rows with it (the SECOND cascade hop, "+
			"delivery_attempts.delivery_id)", attemptsBefore-attemptsAfter, attemptsBefore)
	}

	// The trail is still READABLE through the shipped route's store method, not merely present in the table.
	listed, err := store.ListDeliveries(ctx, project, "", 100)
	if err != nil {
		t.Fatalf("ListDeliveries() error = %v", err)
	}
	surviving := map[string]bool{}
	for _, d := range listed {
		surviving[d.ID] = true
		if d.EndpointID != endpoint {
			t.Fatalf("delivery %s reports endpoint_id %q, want the deleted endpoint's id verbatim: the id is "+
				"evidence of where the payload went and it is shown as it is, not blanked", d.ID, d.EndpointID)
		}
	}
	for _, id := range deliveryIDs {
		if !surviving[id] {
			t.Fatalf("delivery %s survived in the table but is not readable through ListDeliveries", id)
		}
	}

	// And the pump stops on its own: an orphaned PENDING delivery is not due, because DueDeliveries joins
	// the endpoint. This is what makes "the endpoint is deleted" mean "nothing more is sent".
	//
	// DueDeliveries is the pump's own cross-tenant sweep (webhook_pump.go's Tick applies WithSystemScope
	// before calling it, for the same reason: it scans every tenant's due rows at once, by construction,
	// so no single-tenant scope could ever be the right one). This call needs the same scope Tick gives
	// it — a bare ctx has never carried one, and PrepareConn refuses to acquire a connection under no
	// scope at all rather than silently seeing nothing (A.2 Task 1, ErrProjectRequired).
	due, err := store.DueDeliveries(storage.WithSystemScope(ctx), 100)
	if err != nil {
		t.Fatalf("DueDeliveries() error = %v", err)
	}
	for _, d := range due {
		if d.EndpointID == endpoint {
			t.Fatalf("delivery %s to the DELETED endpoint is still due; deleting an endpoint must stop future "+
				"sends, and the inner join to webhook_endpoints is what does that", d.ID)
		}
	}
}

// TestNoForeignKeyCascadesOutOfAWebhookEndpoint is the structural half, and it is a SWEEP rather than a
// check of the one constraint this task knew about. Every foreign key POINTING AT webhook_endpoints is
// enumerated from pg_constraint and each one's delete action is judged; a table added next year that
// references this one with ON DELETE CASCADE fails here, which is the only way "a delete does not silently
// take a second thing with it" can stay true after the person who wrote the delete has moved on.
//
// It reads the live catalogue, not the migration file. A migration is what was INTENDED; pg_constraint is
// what the database will DO, and those differ whenever a later migration alters a constraint (which is
// exactly what this task's own migration does).
func TestNoForeignKeyCascadesOutOfAWebhookEndpoint(t *testing.T) {
	pool := componentPool(t)
	ctx := storage.WithSystemScope(context.Background())

	rows, err := pool.Query(ctx, `
		SELECT c.conname, c.conrelid::regclass::text, c.confdeltype
		FROM pg_constraint c
		WHERE c.contype = 'f' AND c.confrelid = 'webhook_endpoints'::regclass
		ORDER BY c.conname`)
	if err != nil {
		t.Fatalf("read pg_constraint: %v", err)
	}
	defer rows.Close()

	// The confdeltype codes Postgres stores: a = NO ACTION, r = RESTRICT, c = CASCADE, n = SET NULL,
	// d = SET DEFAULT. Only the first two leave the referencing row alone, and they are the only two a
	// referrer of this table may carry.
	refusing := map[string]string{"a": "NO ACTION", "r": "RESTRICT"}
	destructive := map[string]string{"c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT"}

	seen := 0
	for rows.Next() {
		var name, table, action string
		if err := rows.Scan(&name, &table, &action); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if _, ok := refusing[action]; ok {
			t.Logf("%s on %s: %s — a delete does not reach through it", name, table, refusing[action])
			continue
		}
		what, known := destructive[action]
		if !known {
			what = "an unrecognised confdeltype " + action
		}
		t.Fatalf("%s: deleting a webhook_endpoints row performs %s on %s. Deleting an endpoint must take "+
			"exactly one row. %s is where the delivery audit trail lives, and a cascade through it erases "+
			"what this deployment already sent — the same shape as E28's runner_pool_keys.",
			name, what, table, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if seen == 0 {
		t.Fatal("no foreign key references webhook_endpoints at all. Two do (webhook_deliveries.endpoint_id and " +
			"trigger_revisions.callback_endpoint_id), so finding none means this query stopped matching the " +
			"schema and the sweep is vacuous.")
	}
}

// TestAWebhookEndpointPinnedByATriggerRevisionIsRefusedRatherThanOrphaningIt covers the second referring
// key, which the plan did not mention at all. trigger_revisions.callback_endpoint_id points at this table
// with no delete action, i.e. the delete is REFUSED by the database.
//
// That refusal is kept rather than worked around, and the difference from the delivery case is the
// difference between the past and the future: a delivery records something already sent, but a trigger
// revision is LIVE CONFIGURATION that will send something to this endpoint the next time it fires. Deleting
// out from under it would leave a trigger pointed at nothing. So the answer is a typed refusal naming the
// reason, not a 500 from a leaked constraint violation and not a silent orphan.
//
// callback.go's comment already leaned on this FK ("the endpoint cannot be deleted while a pinned revision
// references it"). That was vacuously true while nothing could delete an endpoint at all; this test is what
// makes it true on purpose.
func TestAWebhookEndpointPinnedByATriggerRevisionIsRefusedRatherThanOrphaningIt(t *testing.T) {
	pool := componentPool(t)
	store := NewWebhookStore(pool)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)

	endpoint, err := store.CreateEndpoint(ctx, project, defaultEndpoint("https://hooks.example/cb", "ref"))
	if err != nil {
		t.Fatalf("CreateEndpoint() error = %v", err)
	}
	trigger, revision := randID("trg"), randID("trv")
	mustExec(t, pool, `INSERT INTO triggers (id, organization_id, project_id, name, type) VALUES ($1,$2,$3,'nightly','cron')`,
		trigger, org, project)
	mustExec(t, pool, `INSERT INTO trigger_revisions (id, organization_id, project_id, trigger_id, revision_number, callback_endpoint_id)
		VALUES ($1,$2,$3,$4,1,$5)`, revision, org, project, trigger, endpoint)

	deleted, err := store.DeleteEndpoint(ctx, project, endpoint)
	if !errors.Is(err, ErrEndpointPinned) {
		t.Fatalf("DeleteEndpoint(pinned) = (%v, %v), want ErrEndpointPinned. A trigger revision is immutable and "+
			"live: deleting the endpoint it names either fails as a raw constraint violation (a 500 an operator "+
			"cannot act on) or orphans a trigger. It has to be a typed refusal that names the reason.",
			deleted, err)
	}
	if deleted {
		t.Fatal("DeleteEndpoint reported a deletion alongside the refusal")
	}
	if _, found, err := store.GetEndpoint(ctx, project, endpoint); err != nil || !found {
		t.Fatalf("the refused delete removed the endpoint anyway: found %v, err %v", found, err)
	}
}

// TestRedeliveringToADeletedEndpointIsRefusedRatherThanQueuedForever closes a hole this task OPENS. Before
// it, no endpoint could be deleted, so no delivery could be orphaned and redeliver never lied. After it, an
// orphaned delivery can be re-queued — the UPDATE matches, the row goes back to `pending`, the route answers
// 202 — and the pump's inner join means it will never be attempted. Answering "accepted" for work that
// cannot happen is exactly the shape this tree keeps finding, so it is refused at the source instead of
// filed as a gap.
func TestRedeliveringToADeletedEndpointIsRefusedRatherThanQueuedForever(t *testing.T) {
	pool := componentPool(t)
	store := NewWebhookStore(pool)
	ctx := context.Background()
	org, project, session := seedSession(t, pool)

	endpoint, err := store.CreateEndpoint(ctx, project, defaultEndpoint("https://hooks.example/gone", "ref"))
	if err != nil {
		t.Fatalf("CreateEndpoint() error = %v", err)
	}
	delivery := randID("whd")
	mustExec(t, pool, `INSERT INTO webhook_deliveries (id, organization_id, project_id, endpoint_id, session_id, event_id, event_type, state)
		VALUES ($1,$2,$3,$4,$5,$6,'run.completed.v1','dead')`,
		delivery, org, project, endpoint, session, randID("evt"))

	// While the endpoint exists, redelivery is exactly as it was — the non-vacuity half, so a blanket
	// refusal could not pass this test.
	found, err := store.Redeliver(ctx, project, delivery)
	if err != nil || !found {
		t.Fatalf("Redeliver(live endpoint) = (%v, %v), want (true, nil)", found, err)
	}
	mustExec(t, pool, `UPDATE webhook_deliveries SET state = 'dead' WHERE id = $1`, delivery)

	if _, err := store.DeleteEndpoint(ctx, project, endpoint); err != nil {
		t.Fatalf("DeleteEndpoint() error = %v", err)
	}

	found, err = store.Redeliver(ctx, project, delivery)
	if !errors.Is(err, ErrDeliveryEndpointDeleted) {
		t.Fatalf("Redeliver(orphaned) = (%v, %v), want ErrDeliveryEndpointDeleted. The UPDATE matches and the row "+
			"goes back to `pending`, but DueDeliveries joins webhook_endpoints, so nothing will ever attempt it: "+
			"a 202 here is an acceptance of work that cannot happen.", found, err)
	}
	var state string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM webhook_deliveries WHERE id = $1`, delivery).Scan(&state); err != nil {
		t.Fatalf("re-read the delivery: %v", err)
	}
	if state != "dead" {
		t.Fatalf("the refused redelivery still moved the delivery to %q; a refusal writes nothing", state)
	}
}

// TestAForeignTenantsWebhookEndpointIsNeitherReadNorDeleted is the cross-tenant leg for the two new
// routes. Both are addressed by an id the caller supplies, which is the shape this tree has shipped
// defeated before, so both are checked — a read that leaks and a delete that destroys are different
// severities of the same missing WHERE clause.
func TestAForeignTenantsWebhookEndpointIsNeitherReadNorDeleted(t *testing.T) {
	pool := componentPool(t)
	store := NewWebhookStore(pool)
	ctx := context.Background()
	_, project, _ := seedSession(t, pool)
	_, otherProject, _ := seedSession(t, pool)

	victim, err := store.CreateEndpoint(ctx, otherProject, defaultEndpoint("https://hooks.example/theirs", "ref"))
	if err != nil {
		t.Fatalf("CreateEndpoint(foreign) error = %v", err)
	}

	if _, found, err := store.GetEndpoint(ctx, project, victim); err != nil || found {
		t.Fatalf("GetEndpoint across tenants = found %v, err %v; another tenant's receiver URL and secret handle "+
			"are readable by id", found, err)
	}
	deleted, err := store.DeleteEndpoint(ctx, project, victim)
	if err != nil {
		t.Fatalf("DeleteEndpoint across tenants error = %v", err)
	}
	if deleted {
		t.Fatal("one tenant deleted another tenant's webhook endpoint: their notifications stop and nothing tells them")
	}
	if _, found, err := store.GetEndpoint(ctx, otherProject, victim); err != nil || !found {
		t.Fatalf("the foreign endpoint is gone from its OWNER's scope: found %v, err %v", found, err)
	}
}

// ---- the projection --------------------------------------------------------------------------------

// TestAWebhookEndpointsFixedHeadersReachNoReader scans for a value that is genuinely in the database. That
// is the whole reason this test is here and not beside its sibling in the api package: with no
// fixed_headers field on EndpointView, a scan of an api-level response could never fail, and a scan that
// cannot fail proves nothing. Here the row really carries the header — asserted first, so the fixture
// itself cannot rot into vacuity — and then every shipped read of that endpoint is scanned as raw JSON.
func TestAWebhookEndpointsFixedHeadersReachNoReader(t *testing.T) {
	pool := componentPool(t)
	store := NewWebhookStore(pool)
	ctx := context.Background()
	_, project, _ := seedSession(t, pool)

	const secretHeader = "Bearer sk-live-not-a-real-credential"
	create := defaultEndpoint("https://hooks.example/headers", "ref")
	create.FixedHeaders = map[string]string{"Authorization": secretHeader}
	endpoint, err := store.CreateEndpoint(ctx, project, create)
	if err != nil {
		t.Fatalf("CreateEndpoint() error = %v", err)
	}

	// NON-VACUITY: the value really is stored, so a scan that finds nothing found nothing for the right reason.
	var stored string
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT fixed_headers::text FROM webhook_endpoints WHERE id = $1`, endpoint).Scan(&stored); err != nil {
		t.Fatalf("read the stored fixed_headers: %v", err)
	}
	if !strings.Contains(stored, secretHeader) {
		t.Fatalf("the fixture never stored the header (%s); this scan would pass no matter what the readers did", stored)
	}

	single, found, err := store.GetEndpoint(ctx, project, endpoint)
	if err != nil || !found {
		t.Fatalf("GetEndpoint() = (%v, %v)", found, err)
	}
	listed, err := store.ListEndpoints(ctx, project)
	if err != nil {
		t.Fatalf("ListEndpoints() error = %v", err)
	}
	for name, view := range map[string]any{"GetEndpoint": single, "ListEndpoints": listed} {
		raw, err := json.Marshal(view)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		for _, forbidden := range []string{secretHeader, "sk-live", "Authorization", "fixed_headers"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("%s renders %q. fixed_headers is a free map the operator wrote and it holds a credential "+
					"for the receiver; a read route that returns it is a credential read.\n%s", name, forbidden, raw)
			}
		}
	}
}

// TestTheEndpointProjectionCarriesTheDeliveryPolicy is the other half of the six-field decision: the four
// that DO join. They are behavioural configuration — what this deployment will do when it sends — and an
// operator who cannot read them back has no way to know what a deployment does except to remember what was
// typed. signing_secret_ref joins as a HANDLE; the value behind it stays unreadable and this asserts that
// too, so "the handle is readable" is never quietly widened into "the secret is readable".
func TestTheEndpointProjectionCarriesTheDeliveryPolicy(t *testing.T) {
	pool := componentPool(t)
	store := NewWebhookStore(pool)
	ctx := context.Background()
	_, project, _ := seedSession(t, pool)

	create := EndpointCreate{
		URL: "https://hooks.example/policy", EventFilter: []string{"run.completed.v1"},
		SigningSecretRef: "secret_ref_live", SigningSecretRefNext: "secret_ref_next",
		TimeoutMS: 4321, MaxAttempts: 9, RetryWindowSeconds: 12345, AllowPrivateDestination: true,
	}
	endpoint, err := store.CreateEndpoint(ctx, project, create)
	if err != nil {
		t.Fatalf("CreateEndpoint() error = %v", err)
	}
	view, found, err := store.GetEndpoint(ctx, project, endpoint)
	if err != nil || !found {
		t.Fatalf("GetEndpoint() = (%v, %v)", found, err)
	}

	if view.SigningSecretRef != create.SigningSecretRef || view.SigningSecretRefNext != create.SigningSecretRefNext {
		t.Fatalf("secret refs read back as (%q, %q), want (%q, %q)",
			view.SigningSecretRef, view.SigningSecretRefNext, create.SigningSecretRef, create.SigningSecretRefNext)
	}
	if view.TimeoutMS != create.TimeoutMS || view.MaxAttempts != create.MaxAttempts || view.RetryWindowSeconds != create.RetryWindowSeconds {
		t.Fatalf("delivery policy reads back as (%d, %d, %d), want (%d, %d, %d)",
			view.TimeoutMS, view.MaxAttempts, view.RetryWindowSeconds,
			create.TimeoutMS, create.MaxAttempts, create.RetryWindowSeconds)
	}

	// The singular read and the list are the SAME projection. A caller that got a different shape from two
	// routes would be looking at two resources.
	listed, err := store.ListEndpoints(ctx, project)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListEndpoints() = %v, %v", listed, err)
	}
	single, _ := json.Marshal(view)
	fromList, _ := json.Marshal(&listed[0])
	if string(single) != string(fromList) {
		t.Fatalf("the singular read and the list render different bytes for one endpoint:\n  single: %s\n  list:   %s",
			single, fromList)
	}
}

// ---- the two partial orderings ---------------------------------------------------------------------

// TestTwoWebhookEndpointsSharingACreatedAtListInAnOrderTheQueryDecides is the ordering RED, and it is built
// so that the ONLY difference between the two halves is the physical order the rows were written in. No
// planner hint, no ORDER BY override, no LIMIT trick: two independent tenants, the same pair of endpoints,
// inserted in opposite orders, tied on created_at.
//
// `ORDER BY created_at DESC` with no tiebreaker is a PARTIAL order, so both answers are correct and Postgres
// is free to return either. The test states that as what it is — a query whose result is a function of
// something the caller cannot see — and the fix makes the result a function of the query alone.
//
// The tie is CONSTRUCTED, and that is legitimate rather than contrived: created_at is a microsecond
// timestamptz with nothing in the schema forbidding two rows from sharing one, and two creates in one
// operator script land close enough that the collision is a matter of luck rather than of impossibility.
// Constructing it makes a reachable condition reproducible; it does not invent one.
func TestTwoWebhookEndpointsSharingACreatedAtListInAnOrderTheQueryDecides(t *testing.T) {
	pool := componentPool(t)
	store := NewWebhookStore(pool)
	ctx := context.Background()
	tied := time.Now().UTC().Truncate(time.Microsecond)

	// Two scopes, one pair of endpoints each, created in OPPOSITE orders.
	forward := newTiedEndpointPair(t, pool, store, tied, false)
	reverse := newTiedEndpointPair(t, pool, store, tied, true)

	listOf := func(p tiedPair) []string {
		t.Helper()
		listed, err := store.ListEndpoints(ctx, p.project)
		if err != nil {
			t.Fatalf("ListEndpoints() error = %v", err)
		}
		if len(listed) != 2 {
			t.Fatalf("ListEndpoints() returned %d rows, want the 2 this fixture created", len(listed))
		}
		return ids(listed)
	}

	// Leg 1 — the empirical one. Each scope's pair is labelled by ROLE (the id that sorts higher, the id that
	// sorts lower), so the two scopes are compared on the same footing despite holding different random ids.
	forwardRoles, reverseRoles := roleOrder(forward, listOf(forward)), roleOrder(reverse, listOf(reverse))
	if !equal(forwardRoles, reverseRoles) {
		t.Fatalf("one query returned two different orders for two identically-shaped pairs:\n"+
			"  inserted high-then-low: %v\n  inserted low-then-high:  %v\n"+
			"`ORDER BY created_at DESC` with no tiebreaker is a PARTIAL order, so the answer is decided by the "+
			"physical order of the rows rather than by the query. A caller paging this list cannot see that.",
			forwardRoles, reverseRoles)
	}

	// Leg 2 — the structural one, which is what generalises. Under a TOTAL order no two adjacent rows may
	// compare equal on the full key, so the returned sequence must be strictly decreasing. Leg 1 can only
	// catch a tie the fixture happened to produce; this catches the property itself.
	for _, p := range []tiedPair{forward, reverse} {
		listed, err := store.ListEndpoints(ctx, p.project)
		if err != nil {
			t.Fatalf("ListEndpoints() error = %v", err)
		}
		assertStrictlyDescending(t, "ListWebhookEndpoints", endpointKeys(listed))
	}
}

// TestTwoWebhookDeliveriesSharingACreatedAtPageInAnOrderTheQueryDecides is the same defect one table over,
// and it is worse there because ListWebhookDeliveries carries a LIMIT. Under a partial order a row sitting
// on the page boundary can be returned twice across two pages or skipped entirely — the reader loses a
// delivery record and nothing reports it. This tree has had an unordered LIMIT decide a security outcome
// twice; this is the third generation of that shape.
func TestTwoWebhookDeliveriesSharingACreatedAtPageInAnOrderTheQueryDecides(t *testing.T) {
	pool := componentPool(t)
	store := NewWebhookStore(pool)
	ctx := context.Background()
	tied := time.Now().UTC().Truncate(time.Microsecond)

	forward := newTiedDeliveryTriple(t, pool, store, tied, false)
	reverse := newTiedDeliveryTriple(t, pool, store, tied, true)

	// The page is DELIBERATELY smaller than the fixture: a total order makes the first page a stable prefix,
	// a partial one makes its contents depend on the write order.
	const pageSize = 2
	pageOf := func(p tiedPair) []string {
		t.Helper()
		listed, err := store.ListDeliveries(ctx, p.project, "", pageSize)
		if err != nil {
			t.Fatalf("ListDeliveries() error = %v", err)
		}
		if len(listed) != pageSize {
			t.Fatalf("ListDeliveries(limit %d) returned %d rows", pageSize, len(listed))
		}
		out := make([]string, 0, len(listed))
		for _, d := range listed {
			out = append(out, d.ID)
		}
		return out
	}

	forwardRoles, reverseRoles := roleOrder(forward, pageOf(forward)), roleOrder(reverse, pageOf(reverse))
	if !equal(forwardRoles, reverseRoles) {
		t.Fatalf("one LIMITed query returned two different pages for two identically-shaped triples:\n"+
			"  inserted high-first: %v\n  inserted low-first:  %v\n"+
			"`ORDER BY created_at DESC LIMIT n` with no tiebreaker means the page BOUNDARY moves: a row can be "+
			"served on both pages or on neither, and the reader loses a delivery record silently.",
			forwardRoles, reverseRoles)
	}

	for _, p := range []tiedPair{forward, reverse} {
		listed, err := store.ListDeliveries(ctx, p.project, "", 100)
		if err != nil {
			t.Fatalf("ListDeliveries() error = %v", err)
		}
		keys := make([]sortKey, 0, len(listed))
		for _, d := range listed {
			keys = append(keys, sortKey{at: d.CreatedAt, id: d.ID})
		}
		assertStrictlyDescending(t, "ListWebhookDeliveries", keys)
	}
}

// ---- fixtures --------------------------------------------------------------------------------------

// tiedPair is one tenant holding rows that share a created_at, labelled by the role each id plays under a
// TOTAL order (`high` sorts first under id DESC). Comparing two scopes by role rather than by id is what
// lets two fixtures with different random ids be compared at all.
type tiedPair struct {
	org, project string
	high, low    string
	mid          string // the third row, deliveries only
}

func newTiedEndpointPair(t *testing.T, pool *pgxpool.Pool, store *WebhookStore, at time.Time, reversed bool) tiedPair {
	t.Helper()
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)

	first, err := store.CreateEndpoint(ctx, project, defaultEndpoint("https://hooks.example/a", "ref"))
	if err != nil {
		t.Fatalf("CreateEndpoint() error = %v", err)
	}
	second, err := store.CreateEndpoint(ctx, project, defaultEndpoint("https://hooks.example/b", "ref"))
	if err != nil {
		t.Fatalf("CreateEndpoint() error = %v", err)
	}
	// The tie is written after both rows exist, in ONE statement, so neither row's physical position is
	// disturbed relative to the other by the fixture itself.
	mustExec(t, pool, `UPDATE webhook_endpoints SET created_at = $1 WHERE id = ANY($2)`, at, []string{first, second})

	pair := tiedPair{org: org, project: project, high: max2(first, second), low: min2(first, second)}
	if reversed {
		// Rewrite the pair so the id that sorts HIGH was inserted SECOND. Deleting and re-creating would mint
		// new ids, so the physical order is changed by rewriting the row that has to move to the end.
		mustExec(t, pool, `UPDATE webhook_endpoints SET url = url || '#moved' WHERE id = $1`, pair.high)
	} else {
		mustExec(t, pool, `UPDATE webhook_endpoints SET url = url || '#moved' WHERE id = $1`, pair.low)
	}
	return pair
}

func newTiedDeliveryTriple(t *testing.T, pool *pgxpool.Pool, store *WebhookStore, at time.Time, reversed bool) tiedPair {
	t.Helper()
	ctx := context.Background()
	org, project, session := seedSession(t, pool)
	endpoint, err := store.CreateEndpoint(ctx, project, defaultEndpoint("https://hooks.example/deliveries", "ref"))
	if err != nil {
		t.Fatalf("CreateEndpoint() error = %v", err)
	}

	var made []string
	for range 3 {
		id := randID("whd")
		made = append(made, id)
		mustExec(t, pool, `INSERT INTO webhook_deliveries (id, organization_id, project_id, endpoint_id, session_id, event_id, event_type, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,'run.completed.v1',$7)`,
			id, org, project, endpoint, session, randID("evt"), at)
	}
	sortDesc(made)
	p := tiedPair{org: org, project: project, high: made[0], mid: made[1], low: made[2]}
	if reversed {
		mustExec(t, pool, `UPDATE webhook_deliveries SET event_type = event_type WHERE id = ANY($1)`,
			[]string{p.high, p.mid})
	} else {
		mustExec(t, pool, `UPDATE webhook_deliveries SET event_type = event_type WHERE id = ANY($1)`,
			[]string{p.low, p.mid})
	}
	return p
}

// roleOrder relabels a list of ids by the role each plays in its fixture, so two scopes holding different
// random ids can be compared.
func roleOrder(p tiedPair, got []string) []string {
	role := map[string]string{p.high: "high", p.mid: "mid", p.low: "low"}
	out := make([]string, 0, len(got))
	for _, id := range got {
		name, ok := role[id]
		if !ok {
			name = "unknown(" + id + ")"
		}
		out = append(out, name)
	}
	return out
}

// assertStrictlyDescending is the structural half of both ordering tests, and it is what generalises: the
// empirical leg can only catch a tie the fixture happened to produce, while this states the property. Under
// `ORDER BY created_at DESC, id DESC` every adjacent pair must satisfy prev > cur on the WHOLE key. A pair
// that compares equal means the key does not distinguish two rows, which is exactly the condition under
// which both orders are correct answers and the one returned is the storage layer's choice.
func assertStrictlyDescending(t *testing.T, query string, keys []sortKey) {
	t.Helper()
	for i := 1; i < len(keys); i++ {
		prev, cur := keys[i-1], keys[i]
		switch {
		case prev.at.After(cur.at):
			continue // strictly newer: distinguished by created_at alone
		case prev.at.Equal(cur.at) && prev.id > cur.id:
			continue // tied on created_at, distinguished by the id tiebreaker
		case prev.at.Equal(cur.at) && prev.id == cur.id:
			t.Fatalf("%s returned two rows with an identical sort key (%s, %s); id is the PRIMARY KEY, so this "+
				"cannot happen and finding it means the fixture or the projection is wrong",
				query, cur.at.Format(time.RFC3339Nano), cur.id)
		case prev.at.Equal(cur.at):
			t.Fatalf("%s: rows %d and %d share created_at %s and their ids run the wrong way (%q then %q). "+
				"The tiebreaker is `id DESC`.",
				query, i-1, i, cur.at.Format(time.RFC3339Nano), prev.id, cur.id)
		default:
			t.Fatalf("%s: rows %d and %d are not in descending created_at order (%s then %s)",
				query, i-1, i, prev.at.Format(time.RFC3339Nano), cur.at.Format(time.RFC3339Nano))
		}
	}
}

// sortKey is one row's full ordering key. created_at is kept as a time.Time rather than a formatted string
// on purpose: RFC3339Nano trims trailing zeros from the fraction, so ".1Z" and ".12Z" compare in the wrong
// order as text, and an ordering guard that compares its keys wrongly is worth less than no guard.
type sortKey struct {
	at time.Time
	id string
}

func endpointKeys(views []EndpointView) []sortKey {
	out := make([]sortKey, 0, len(views))
	for _, v := range views {
		out = append(out, sortKey{at: v.CreatedAt, id: v.ID})
	}
	return out
}

func ids(views []EndpointView) []string {
	out := make([]string, 0, len(views))
	for _, v := range views {
		out = append(out, v.ID)
	}
	return out
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func max2(a, b string) string {
	if a > b {
		return a
	}
	return b
}

func min2(a, b string) string {
	if a < b {
		return a
	}
	return b
}

func sortDesc(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] > s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}
