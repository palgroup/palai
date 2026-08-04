//go:build component

package execution

import (
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
)

// The REGISTERED gated-tool fixture: a published tool revision carrying `approval_required` plus a run
// granted it — the shape an operator actually produces, and the shape HIL-P8 names.
//
// IT WAS EXTRACTED FROM slack_tool_approval_component_test.go ON 2026-08-05, when that file went with the
// in-process Slack bridge. The fixture is not Slack's and never was: it seeds a tool, a revision, a tool
// set, an agent revision and a run. Its other caller is http_tool_approval_component_test.go — the GENERIC
// decision surface — which would have lost its seed with the transport that happened to house it.

// registeredGatedToolLabel is the operator's own sentence — written at publish time, beside the per-tool
// approval decision they already make. It is the ONLY human sentence on the approval screen, and the whole
// reason 000044 R3 gave it a column of its own instead of reusing a discovered revision's `description`:
// the description is written by the SERVER being authorized.
const registeredGatedToolLabel = "the shared service account may move tickets, and only in PAL"

// seedRegisteredGatedRun seeds a REGISTERED, PUBLISHED, gated tool revision and a run granted it. It
// returns the session and run.
func seedRegisteredGatedRun(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, modelName string) (sessionID, runID string) {
	t.Helper()
	pool := cs.Pool()
	project := tenant.Project
	toolID, trevID := redeliveryID("tool"), redeliveryID("trev")
	setID, profileID, arevID := redeliveryID("tsrev"), redeliveryID("aprof"), redeliveryID("arev")
	sessionID, runID = redeliveryID("ses"), redeliveryID("run")

	execSQL(t, pool, `INSERT INTO tools (id, project_id, canonical_name, model_visible_name)
	                  VALUES ($1,$2,$3,$4)`, toolID, project, "reg."+modelName, modelName)
	// approval_required + approval_label are 000044 R3's columns, set exactly where the operator sets them.
	execSQL(t, pool, `INSERT INTO tool_revisions (id, project_id, tool_id, revision_number, executor, description, input_schema, replay_class, digest, published_at, approval_required, approval_label)
	                  VALUES ($1,$2,$3,1,'control_plane',$4,'{"type":"object"}'::jsonb,'irreversible',$5,clock_timestamp(),true,$6)`,
		trevID, project, toolID, "moves a ticket", "sha256:"+trevID, registeredGatedToolLabel)
	execSQL(t, pool, `INSERT INTO tool_set_revisions (id, project_id, set_name, revision_number, tool_pins, digest, published_at)
	                  VALUES ($1,$2,$3,1,$4::jsonb,'d',clock_timestamp())`,
		setID, project, "set-"+modelName, `[{"tool_revision_id":"`+trevID+`"}]`)
	execSQL(t, pool, `INSERT INTO agent_profiles (id, project_id, name) VALUES ($1,$2,$3)`,
		profileID, project, profileID)
	execSQL(t, pool, `INSERT INTO agent_revisions (id, project_id, profile_id, revision_number, model, published_at, tool_sets, mcp_connections)
	                  VALUES ($1,$2,$3,1,'model-x',clock_timestamp(),$4::jsonb,'[]'::jsonb)`,
		arevID, project, profileID, `["`+setID+`"]`)
	execSQL(t, pool, `INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, sessionID, project)
	execSQL(t, pool, `INSERT INTO runs (id, project_id, session_id, agent_revision_id, state)
	                  VALUES ($1,$2,$3,$4,'running')`, runID, project, sessionID, arevID)
	return sessionID, runID
}
