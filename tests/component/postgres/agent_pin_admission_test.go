//go:build component

package postgres

import (
	"context"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
)

// seedAgentRevision creates a profile + one revision (published if publish) and returns the revision id.
func seedAgentRevision(t *testing.T, tenant coordinator.Tenant, cs *coordinator.Store, publish bool) string {
	t.Helper()
	pool := cs.Pool()
	profileID, revID := newID("aprof"), newID("arev")
	exec(t, pool, `INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,$4)`,
		profileID, tenant.Organization, tenant.Project, newID("name"))
	pubExpr := "NULL"
	if publish {
		pubExpr = "clock_timestamp()"
	}
	exec(t, pool, `INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, tools, published_at)
	               VALUES ($1,$2,$3,$4,1,'model-pinned','["file"]', `+pubExpr+`)`,
		revID, tenant.Organization, tenant.Project, profileID)
	return revID
}

// TestUnpublishedRevisionCannotBePinnedOrRun proves the draft guard (spec §10, AGT-001): admitting a
// run pinned to a DRAFT revision is a typed reject that creates no run, and an unknown pin is a 404 —
// while a PUBLISHED revision admits and stamps the pin on the run row (so resolution can read it).
func TestUnpublishedRevisionCannotBePinnedOrRun(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, principalID := seedTenantWithKey(t, cs.Pool(), "tok")

	draftRev := seedAgentRevision(t, tenant, cs, false)
	in := admissionInput(principalID, newID("key"), "hash-draft", `{"id":"r","object":"response","status":"queued"}`)
	in.AgentRevisionID = draftRev
	out, err := cs.AdmitResponse(ctx, tenant, in)
	if err != nil {
		t.Fatalf("AdmitResponse(draft pin) error = %v", err)
	}
	if !out.PinnedRevisionNotPublished {
		t.Fatalf("draft pin admission = %+v, want PinnedRevisionNotPublished (409)", out)
	}
	assertNoRun(t, cs, in.RunID)

	// An unknown pin is a 404, likewise leaving no run.
	unknown := admissionInput(principalID, newID("key"), "hash-unknown", `{"id":"r"}`)
	unknown.AgentRevisionID = "arev_does_not_exist"
	out2, err := cs.AdmitResponse(ctx, tenant, unknown)
	if err != nil {
		t.Fatalf("AdmitResponse(unknown pin) error = %v", err)
	}
	if !out2.PinnedRevisionNotFound {
		t.Fatalf("unknown pin admission = %+v, want PinnedRevisionNotFound (404)", out2)
	}
	assertNoRun(t, cs, unknown.RunID)

	// A published revision admits and stamps the pin on the run row — resolution reads it back.
	pubRev := seedAgentRevision(t, tenant, cs, true)
	ok := admissionInput(principalID, newID("key"), "hash-ok", `{"id":"r","object":"response","status":"queued"}`)
	ok.AgentRevisionID = pubRev
	out3, err := cs.AdmitResponse(ctx, tenant, ok)
	if err != nil {
		t.Fatalf("AdmitResponse(published pin) error = %v", err)
	}
	if out3.Replayed || out3.Conflict || out3.PinnedRevisionNotFound || out3.PinnedRevisionNotPublished {
		t.Fatalf("published pin admission = %+v, want a fresh create", out3)
	}
	revID, model, tools, err := cs.PinnedExecConfig(ctx, tenant, ok.RunID)
	if err != nil {
		t.Fatalf("PinnedExecConfig() error = %v", err)
	}
	if revID != pubRev || model != "model-pinned" || len(tools) != 1 || tools[0] != "file" {
		t.Fatalf("resolved pin = %s/%s/%v, want %s/model-pinned/[file]", revID, model, tools, pubRev)
	}
}

// TestRunTemplateRevisionStartsProfileFreeRun proves the AGT-003 E11 face: a run started from a
// published run-template revision runs with NO AgentProfile involved — the pin resolves the template's
// executable config, and no agent_profiles row exists for this tenant.
func TestRunTemplateRevisionStartsProfileFreeRun(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, principalID := seedTenantWithKey(t, cs.Pool(), "tok")
	pool := cs.Pool()

	templateRev := newID("rtr")
	exec(t, pool, `INSERT INTO run_template_revisions (id, organization_id, project_id, template_name, revision_number, model, tools, published_at)
	               VALUES ($1,$2,$3,'nightly',1,'template-model','["shell"]', clock_timestamp())`,
		templateRev, tenant.Organization, tenant.Project)

	in := admissionInput(principalID, newID("key"), "hash-tmpl", `{"id":"r","object":"response","status":"queued"}`)
	in.RunTemplateRevisionID = templateRev
	out, err := cs.AdmitResponse(ctx, tenant, in)
	if err != nil {
		t.Fatalf("AdmitResponse(template) error = %v", err)
	}
	if out.PinnedRevisionNotFound || out.PinnedRevisionNotPublished || out.Conflict {
		t.Fatalf("template admission = %+v, want a fresh create", out)
	}

	// The run resolves the template's config, and NO agent profile exists for this tenant.
	revID, model, tools, err := cs.PinnedExecConfig(ctx, tenant, in.RunID)
	if err != nil {
		t.Fatalf("PinnedExecConfig() error = %v", err)
	}
	if revID != templateRev || model != "template-model" || len(tools) != 1 || tools[0] != "shell" {
		t.Fatalf("resolved template pin = %s/%s/%v, want %s/template-model/[shell]", revID, model, tools, templateRev)
	}
	var profiles int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_profiles WHERE organization_id=$1 AND project_id=$2`,
		tenant.Organization, tenant.Project).Scan(&profiles); err != nil {
		t.Fatalf("count agent_profiles: %v", err)
	}
	if profiles != 0 {
		t.Fatalf("agent_profiles rows = %d, want 0 (a template run is profile-free)", profiles)
	}
}

func assertNoRun(t *testing.T, cs *coordinator.Store, runID string) {
	t.Helper()
	var n int
	if err := cs.Pool().QueryRow(context.Background(), `SELECT count(*) FROM runs WHERE id=$1`, runID).Scan(&n); err != nil {
		t.Fatalf("count run %s: %v", runID, err)
	}
	if n != 0 {
		t.Fatalf("run %s exists (n=%d), want none — a rejected pin must leave no run", runID, n)
	}
}
