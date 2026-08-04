//go:build component

package store_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/palgroup/palai/storage"
)

// THE WALL THIS FILE EXISTS TO REMOVE (E22 T3, plan §3.6 D3): a Slack-born run was STRUCTURALLY non-coding.
// slack_admit.go's AdmitResponse call never set RepositoryBindingID/RepositoryRef — the only two setters in
// the tree were api/responses.go (the HTTP body's `repository` field) and store/postgres.go — and
// slackRunTarget carried nothing that could name a repo. So a Slack thread could not reach a repository
// however completely PALAI_WORKSPACE_ROOT was configured: no workspace was ever attached to its session, and
// RunPublicationTarget answered "the run prepared no repository".
//
// WHERE THE PROOF IS READ, and it is not the run row: the binding does not live on `runs`. Admission attaches
// a SESSION-SCOPED workspace (AttachSessionWorkspace, coordinator/store.go) carrying repository_binding_id +
// requested_ref, and that row is what provisionRootWorkspace later clones from. A run with no such row is a
// run that will provision nothing — which is exactly the pre-E22 Slack behaviour.

// bindRepository registers a repository binding for a tenant and returns its id. Written as a direct INSERT
// for the same reason scopeToChannels is an UPDATE: the registration write-path (POST /v1/repository-bindings)
// is proven in apps/control-plane/api and e2e; what is under test HERE is whether an admission carries a
// binding that already exists.
func (f *slackFixture) bindRepository(t *testing.T, project string) string {
	t.Helper()
	id := newID("repo")
	exec(t, f.pool, `INSERT INTO repository_bindings
	                   (id, project_id, provider, repository_identity, clone_url, default_branch)
	                 VALUES ($1,$2,'github','acme/widgets','https://github.com/acme/widgets.git','dev')`,
		id, project)
	return id
}

// useRepository rewrites the fixture connection's default_policy to name a binding, the way an operator (or
// `palai up`) who bound a repository to their workspace leaves it. The run target's other two fields are
// re-stated verbatim: default_policy is ONE document, and a partial write would refuse the admission for the
// wrong reason.
func (f *slackFixture) useRepository(t *testing.T, bindingID string) {
	t.Helper()
	exec(t, f.pool, `UPDATE slack_connections SET default_policy=$1::jsonb
	                  WHERE  project_id=$2`,
		fmt.Sprintf(`{"agent_revision_id":%q,"principal_id":%q,"repository_binding_id":%q}`,
			f.revision, f.principal, bindingID), f.project)
}

// sessionWorkspace reads the coding workspace the admission attached to this tenant's session, if any.
// Tenant-scoped in its own predicate: a component database is shared by every test in the package, and an
// unscoped count would sweep another fixture's rows and pass for the wrong reason.
func (f *slackFixture) sessionWorkspace(t *testing.T) (bindingID, ref string, found bool) {
	t.Helper()
	rows, err := f.pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT repository_binding_id, requested_ref FROM workspaces
		  WHERE  project_id=$1`, f.project)
	if err != nil {
		t.Fatalf("read the session workspace: %v", err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		n++
		if err := rows.Scan(&bindingID, &ref); err != nil {
			t.Fatalf("scan the session workspace: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the session workspace: %v", err)
	}
	if n > 1 {
		t.Fatalf("%d workspaces attached in this tenant, want at most 1 — a session keeps ONE bound workspace", n)
	}
	return bindingID, ref, n == 1
}

// TestSlackRunCarriesTheConnectionsRepositoryBinding is E22 T3's whole point, stated as the assertion we
// WANT: an event on a connection whose default_policy names a repository births a run whose session has that
// repository attached. Before T3 this failed — no workspace row existed at all.
func TestSlackRunCarriesTheConnectionsRepositoryBinding(t *testing.T) {
	f := newSlackFixture(t)
	binding := f.bindRepository(t, f.project)
	f.useRepository(t, binding)

	resp := f.deliver(t, f.event("EvRepo1", "app_mention", "Umapped", "C40", "1700000600.000100", ""), time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("an event on a repository-bound connection = %d, want a 2xx ack", resp.StatusCode)
	}
	if n := f.runCount(t); n != 1 {
		t.Fatalf("the event birthed %d runs, want 1", n)
	}
	got, ref, found := f.sessionWorkspace(t)
	if !found {
		t.Fatal("the Slack-born run's session has NO coding workspace: the connection's repository binding was " +
			"not carried into the admission, so the agent can never clone, read or write a file — " +
			"RunPublicationTarget would answer 'the run prepared no repository' (plan §3.6 D3)")
	}
	if got != binding {
		t.Fatalf("the session is bound to repository %q, want the CONNECTION's %q — a run target is operator "+
			"configuration and no payload may move it", got, binding)
	}
	// requested_ref is deliberately empty: adapters/repositories/prepare.go falls back to the binding's
	// default_branch, which is the SAME value the publication target uses as the PR base. A ref invented here
	// would silently split the two.
	if ref != "" {
		t.Fatalf("requested_ref = %q, want empty — the binding's default_branch is the single source of the "+
			"checked-out ref and the publication base", ref)
	}
}

// TestSlackConnectionWithoutARepositoryIsBitUnchanged is the other half, and it is the half that keeps this
// change cheap: a workspace that bound no repository must behave EXACTLY as it did before E22. Not "mostly" —
// no workspace row, and the run still born.
func TestSlackConnectionWithoutARepositoryIsBitUnchanged(t *testing.T) {
	f := newSlackFixture(t) // registered with the two-field default_policy, no repository

	resp := f.deliver(t, f.event("EvRepo2", "app_mention", "Umapped", "C40", "1700000601.000100", ""), time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("an event on a repository-LESS connection = %d, want a 2xx ack — the optional field must not "+
			"become a requirement", resp.StatusCode)
	}
	if n := f.runCount(t); n != 1 {
		t.Fatalf("the event birthed %d runs, want 1", n)
	}
	if _, _, found := f.sessionWorkspace(t); found {
		t.Fatal("a connection that bound NO repository attached a coding workspace anyway — a run that " +
			"provisions an allocation nobody asked for is a behaviour change, not a default")
	}
}

// TestSlackRefusesAnotherTenantsRepositoryBinding is the security half, and the refusal has to happen at
// ADMISSION rather than at clone time. The store's RepositoryBindingExists check is tenant-scoped and returns
// no row for a foreign id, so the answer is the SAME one an unknown id gets — the cross-tenant existence of
// the binding is never disclosed, and no run, no session and no workspace is born to fail later.
//
// TERMINAL, with the suppress header: no redelivery can move a binding into another tenant, so Slack must not
// pull it three more times.
func TestSlackRefusesAnotherTenantsRepositoryBinding(t *testing.T) {
	f := newSlackFixture(t)
	foreignProject := newID("prj")
	exec(t, f.pool, `INSERT INTO projects (id) VALUES ($1)`, foreignProject)
	foreign := f.bindRepository(t, foreignProject)
	f.useRepository(t, foreign)

	resp := f.deliver(t, f.event("EvRepo3", "app_mention", "Umapped", "C40", "1700000602.000100", ""), time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("an event naming ANOTHER tenant's repository binding = %d, want 422 — the refusal belongs at "+
			"admission, not at the clone", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Slack-No-Retry"); got != "1" {
		t.Fatalf("the refusal answered X-Slack-No-Retry=%q, want \"1\": no redelivery can move a binding into "+
			"this tenant", got)
	}
	if n := f.runCount(t); n != 0 {
		t.Fatalf("%d runs after a foreign binding was named, want 0 — a bad binding must birth NOTHING", n)
	}
	if _, _, found := f.sessionWorkspace(t); found {
		t.Fatal("a foreign repository binding attached a workspace in THIS tenant")
	}
	var attached int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM workspaces WHERE repository_binding_id=$1`, foreign).Scan(&attached); err != nil {
		t.Fatalf("count workspaces on the foreign binding: %v", err)
	}
	if attached != 0 {
		t.Fatalf("%d workspaces attached to the FOREIGN binding, want 0", attached)
	}
}
