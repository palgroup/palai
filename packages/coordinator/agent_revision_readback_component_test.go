//go:build component

package coordinator

import (
	"context"
	"testing"
)

// THE REVISION THAT ANSWERED HAS TO LEAVE THE PLANE.
//
// ‼️ response-create.json PROMISES IT AND NOTHING READ IT BACK. Its `agent_id` description: the server
// "resolves it to that agent's highest-numbered PUBLISHED revision at admission and pins the run to it,
// so the run is reproducible from its recorded agent_revision_id even though the request did not name
// one". coordinator/store.go repeats the claim. 000019 put the column on `runs`, the resolution happens,
// and — measured on a live plane 2026-08-07 — a run driven by an agent answered `agent_revision_id:
// null` from GET /v1/responses/{id}. No route, no event and no projection carried it.
//
// Reproducing a run means naming the revision that answered. A promise whose only witness is a database
// column the customer cannot query is not a promise, and this is the read that keeps it.

func TestAResponseReportsTheRevisionThatAnsweredIt(t *testing.T) {
	cs := failureReasonFixture(t)
	ctx := context.Background()

	project := pinTestID("prj")
	session, response, runID := pinTestID("ses"), pinTestID("resp"), pinTestID("run")
	profile, revision := pinTestID("aprof"), pinTestID("arev")
	mustExecPin(t, cs, `INSERT INTO projects (id) VALUES ($1)`, project)
	mustExecPin(t, cs, `INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, session, project)
	mustExecPin(t, cs, `INSERT INTO responses (id, project_id, session_id, state, input)
	                     VALUES ($1,$2,$3,'completed','"hi"'::jsonb)`, response, project, session)
	// ‼️ A REAL PROFILE AND A REAL REVISION, because `runs.agent_revision_id` carries a FOREIGN KEY and
	// the first writing of this fixture invented an id: the insert was refused with
	// `runs_agent_revision_id_fkey`. That refusal is the product being right — a run cannot claim to have
	// resolved a revision that does not exist — and a fixture that worked around it (dropping the
	// constraint, or inserting a bare row) would have measured a world the plane does not allow.
	mustExecPin(t, cs, `INSERT INTO agent_profiles (id, project_id, name) VALUES ($1,$2,'readback')`,
		profile, project)
	mustExecPin(t, cs, `INSERT INTO agent_revisions (id, project_id, profile_id, revision_number, model, tools, instructions)
	                     VALUES ($1,$2,$3,1,'model-x','[]','answer')`, revision, project, profile)
	mustExecPin(t, cs, `INSERT INTO runs (id, project_id, session_id, response_id, state, agent_revision_id)
	                     VALUES ($1,$2,$3,$4,'completed',$5)`, runID, project, session, response, revision)

	view, err := cs.GetResponse(ctx, Tenant{Project: project}, response)
	if err != nil {
		t.Fatalf("GetResponse error = %v", err)
	}
	if !view.Found {
		t.Fatal("the seeded response was not found — nothing below is a statement about the read")
	}
	if view.AgentRevisionID != revision {
		t.Fatalf("the response reports agent revision %q, want %q: the run recorded it and the read "+
			"dropped it, which is the state every surface was in until this read existed",
			view.AgentRevisionID, revision)
	}
}

// A RESPONSE NO AGENT STEERED REPORTS NOTHING, WHICH IS A DIFFERENT ANSWER FROM AN EMPTY STRING BY
// ACCIDENT. Without this half the read could return the zero value for every response and still satisfy
// the test above only because the fixture happened to set a revision — and a caller would have no way to
// tell "no agent" from "an agent whose id we lost".
func TestAResponseWithNoAgentReportsNoRevision(t *testing.T) {
	cs := failureReasonFixture(t)
	ctx := context.Background()

	project := pinTestID("prj")
	session, response, runID := pinTestID("ses"), pinTestID("resp"), pinTestID("run")
	mustExecPin(t, cs, `INSERT INTO projects (id) VALUES ($1)`, project)
	mustExecPin(t, cs, `INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, session, project)
	mustExecPin(t, cs, `INSERT INTO responses (id, project_id, session_id, state, input)
	                     VALUES ($1,$2,$3,'completed','"hi"'::jsonb)`, response, project, session)
	mustExecPin(t, cs, `INSERT INTO runs (id, project_id, session_id, response_id, state)
	                     VALUES ($1,$2,$3,$4,'completed')`, runID, project, session, response)

	view, err := cs.GetResponse(ctx, Tenant{Project: project}, response)
	if err != nil {
		t.Fatalf("GetResponse error = %v", err)
	}
	if view.AgentRevisionID != "" {
		t.Fatalf("a response no agent steered reports revision %q, want none", view.AgentRevisionID)
	}
}
