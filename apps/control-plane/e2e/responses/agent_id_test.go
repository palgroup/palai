//go:build e2e

package responses

import (
	"context"
	"net/http"
	"testing"

	"github.com/palgroup/palai/storage"
)

// runAgentRevision reads the revision a run was actually pinned to. It is the ONLY assertion that
// distinguishes this feature from what shipped before: `{"agent_id":"agt_..."}` returned 202 both
// before and after, and the run completed both times. What differed is that the row carried NULL and
// the run executed the project's own configuration while the caller believed they had run an agent.
func (h *harness) runAgentRevision(runID string) string {
	h.t.Helper()
	var revision *string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT agent_revision_id FROM runs WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		runID, h.tenant.Project).Scan(&revision); err != nil {
		h.t.Fatalf("read run agent_revision_id error = %v", err)
	}
	if revision == nil {
		return ""
	}
	return *revision
}

// TestAgentIDResolvesToThePublishedRevision proves the whole of what `agent_id` now means: it names
// an AGENT, admission resolves it to that agent's highest-numbered published revision, and the run
// records THAT revision — so the run is reproducible even though the request named no revision.
//
// MEASURED BEFORE THIS CHANGE (live stack, 2026-08-01): POST {"agent_id":"agt_01","input":"..."}
// returned 202/queued, the run completed, and no agent field appeared anywhere in the response.
// `additionalProperties: true` accepted the field and nothing read it.
func TestAgentIDResolvesToThePublishedRevision(t *testing.T) {
	h := newHarness(t)

	st, profile := h.postAgent("/v1/agents", `{"name":"resolver"}`)
	if st != http.StatusCreated {
		t.Fatalf("create profile status = %d, want 201", st)
	}
	profileID, _ := profile["id"].(string)

	// An agent with NO published revision is a 409 that says so — not a silent fall-through to the
	// project's own config, which is exactly what the dropped field used to do.
	if st := h.postResponseStatus(`{"input":"go","agent_id":"` + profileID + `"}`); st != http.StatusConflict {
		t.Fatalf("agent_id with no published revision status = %d, want 409", st)
	}

	// Publish revision 1.
	_, rev1 := h.postAgent("/v1/agents/"+profileID+"/revisions", `{"model":"model-one"}`)
	rev1ID, _ := rev1["id"].(string)
	if st, _ := h.postAgent("/v1/agents/"+profileID+"/revisions/"+rev1ID+"/publish", ``); st != http.StatusOK {
		t.Fatalf("publish rev1 failed")
	}

	responseID, _, runID := h.admitWith(`{"input":"go","agent_id":"`+profileID+`"}`, newID("idem"))
	if got := h.runAgentRevision(runID); got != rev1ID {
		t.Fatalf("run pinned agent_revision_id = %q, want %q — the request named an agent and the run "+
			"must record the revision it resolved to, or nothing about it is reproducible", got, rev1ID)
	}
	_ = responseID

	// Publish revision 2. `agent_id` now means revision 2 — that is the point of naming the agent
	// rather than a revision — while an explicit agent_revision_id still pins revision 1.
	_, rev2 := h.postAgent("/v1/agents/"+profileID+"/revisions", `{"model":"model-two"}`)
	rev2ID, _ := rev2["id"].(string)
	if st, _ := h.postAgent("/v1/agents/"+profileID+"/revisions/"+rev2ID+"/publish", ``); st != http.StatusOK {
		t.Fatalf("publish rev2 failed")
	}

	_, _, runID2 := h.admitWith(`{"input":"go","agent_id":"`+profileID+`"}`, newID("idem"))
	if got := h.runAgentRevision(runID2); got != rev2ID {
		t.Fatalf("after publishing a second revision, agent_id resolved to %q, want the newest published %q", got, rev2ID)
	}

	_, _, runID3 := h.admitWith(`{"input":"go","agent_revision_id":"`+rev1ID+`"}`, newID("idem"))
	if got := h.runAgentRevision(runID3); got != rev1ID {
		t.Fatalf("an explicit agent_revision_id resolved to %q, want the exact pin %q", got, rev1ID)
	}
}

// TestUnknownAgentIDIsRefusedNotDropped is the regression that matters most: the failure mode being
// fixed is a field that was ACCEPTED and ignored, so the case to pin is that a wrong value now
// stops the request instead of producing a cheerful 202 and an unrelated run.
func TestUnknownAgentIDIsRefusedNotDropped(t *testing.T) {
	h := newHarness(t)
	if st := h.postResponseStatus(`{"input":"go","agent_id":"agt_01"}`); st != http.StatusNotFound {
		t.Fatalf("unknown agent_id status = %d, want 404 — this is the exact request that returned "+
			"202 and ran the project default on 2026-08-01", st)
	}
}

// TestAgentIDAndAgentRevisionIDAreMutuallyExclusive — one names an agent, the other an exact
// revision. A request carrying both has no obviously-right reading, so it is refused rather than
// silently resolved in one of the two directions.
func TestAgentIDAndAgentRevisionIDAreMutuallyExclusive(t *testing.T) {
	h := newHarness(t)
	if st := h.postResponseStatus(`{"input":"go","agent_id":"agt_1","agent_revision_id":"arev_1"}`); st != http.StatusBadRequest {
		t.Fatalf("agent_id + agent_revision_id status = %d, want 400", st)
	}
	if st := h.postResponseStatus(`{"input":"go","agent_id":"agt_1","run_template_revision_id":"trev_1"}`); st != http.StatusBadRequest {
		t.Fatalf("agent_id + run_template_revision_id status = %d, want 400", st)
	}
}

// TestDelegationsStillAdmit is the measurement that decided AGAINST DisallowUnknownFields on this
// route, kept as an executable guard rather than a note in a commit message.
//
// `delegations` and `delegation_budget` are read from the RAW body (api/responses.go
// resolveDelegations) and appear in NEITHER the published schema NOR the generated contract — I
// diffed both sets on 2026-08-01 and each had 27 entries with an empty symmetric difference, so
// these two fields are in neither. Switching the create decoder to DisallowUnknownFields, the
// obvious way to make `agent_id` stop being silently dropped, would therefore 400 every delegated
// run: this case, the SUB-* UAT cases, and seven bodies across child_dispatch_test.go and
// detached_child_test.go. That is why agent_id became a real FIELD instead.
func TestDelegationsStillAdmit(t *testing.T) {
	h := newHarness(t)
	body := `{"input":"do it","delegation_budget":100,"delegations":[{"role":"r","objective":"o","model":"fake-child","required":true}]}`
	if st := h.postResponseStatus(body); st != http.StatusAccepted {
		t.Fatalf("a delegated create status = %d, want 202. These two fields are in no published "+
			"schema and no generated contract, so a body-level DisallowUnknownFields would refuse "+
			"every delegated run in the product.", st)
	}
}
