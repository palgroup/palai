package execution

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestConfigSnapshotContentAddressedWithProvenance proves the resolver is content-addressed
// (same input → same hash) and records the layer that set each effective value (spec §14;
// SES-008 snapshot half). It also proves redaction: the credential ref stays a reference, so
// no secret value ever enters the snapshot (LP secret-hygiene pattern).
func TestConfigSnapshotContentAddressedWithProvenance(t *testing.T) {
	deployment := ResolveInput{
		DeploymentModel:  "model-alpha",
		DeploymentSecret: "openai_api_key", // a ref NAME, never the value
		ProjectTools:     []string{"palai.conformance.math.add"},
	}

	// No session override: model comes from the deployment, tools from the project baseline.
	base := Resolve(deployment)
	if base.Model != "model-alpha" {
		t.Fatalf("effective model = %q, want the deployment default model-alpha", base.Model)
	}
	if base.Provenance["model"] != layerDeployment {
		t.Fatalf("model provenance = %q, want %q", base.Provenance["model"], layerDeployment)
	}
	if base.Provenance["tools"] != layerProject {
		t.Fatalf("tools provenance = %q, want %q", base.Provenance["tools"], layerProject)
	}

	// Content addressing: the identical input resolves to the identical hash.
	if again := Resolve(deployment); again.Hash != base.Hash {
		t.Fatalf("same input produced different hashes: %q vs %q", base.Hash, again.Hash)
	}
	if !strings.HasPrefix(base.Hash, "sha256:") {
		t.Fatalf("hash = %q, want a sha256: content address", base.Hash)
	}

	// A session model override flips only the model's provenance to session and re-addresses.
	switched := deployment
	switched.SessionModel = "model-beta"
	snap := Resolve(switched)
	if snap.Model != "model-beta" || snap.Provenance["model"] != layerSession {
		t.Fatalf("session override: model = %q prov = %q, want model-beta from session", snap.Model, snap.Provenance["model"])
	}
	if snap.Provenance["tools"] != layerProject {
		t.Fatalf("tools provenance after a model-only override = %q, want it to stay project", snap.Provenance["tools"])
	}
	if snap.Hash == base.Hash {
		t.Fatal("a model switch must change the content address, but the hash was unchanged")
	}

	// Redaction: the ref name is carried, but the snapshot JSON holds no credential value.
	blob, _ := json.Marshal(snap)
	if snap.SecretRef != "openai_api_key" {
		t.Fatalf("secret ref = %q, want the reference name preserved", snap.SecretRef)
	}
	if strings.Contains(string(blob), "sk-") || strings.Contains(string(blob), "secret-value") {
		t.Fatalf("snapshot leaked a credential value: %s", blob)
	}
}

// TestResolveUnionsToolSetGrantsThenCeiling proves the E12 effective-set semantics: a pinned revision's
// tool_sets GRANT their short names onto the baseline (provenance agent_revision), and the AgentRevisionTools
// CEILING still intersects LAST — so a set-granted tool outside the ceiling is dropped. With no tool_sets
// and no ceiling, resolution is bit-identical to before (deterministic regression).
func TestResolveUnionsToolSetGrantsThenCeiling(t *testing.T) {
	base := ResolveInput{DeploymentModel: "m", ProjectTools: []string{"file"}}

	// Empty tool_sets + no ceiling: bit-identical to the un-extended resolve.
	before := Resolve(base)
	base.AgentRevisionToolSetTools = nil
	if got := Resolve(base); got.Hash != before.Hash {
		t.Fatalf("empty tool_sets changed the hash: %q vs %q (must be bit-identical)", got.Hash, before.Hash)
	}

	// A pinned revision granting "fetch" via a set unions it onto the baseline with agent_revision provenance.
	granted := Resolve(ResolveInput{
		DeploymentModel:           "m",
		ProjectTools:              []string{"file"},
		AgentRevisionID:           "arev_1",
		AgentRevisionToolSetTools: []string{"fetch"},
	})
	if !hasTool(granted.Tools, "fetch") || !hasTool(granted.Tools, "file") {
		t.Fatalf("effective tools = %v, want the baseline file + the granted fetch", granted.Tools)
	}
	if granted.Provenance["tools"] != layerAgentRevision {
		t.Fatalf("tools provenance = %q, want %q after a set grant", granted.Provenance["tools"], layerAgentRevision)
	}

	// The ceiling intersects LAST: a set granting "fetch" but a ceiling of only {file} drops fetch.
	ceilinged := Resolve(ResolveInput{
		DeploymentModel:           "m",
		ProjectTools:              []string{"file"},
		AgentRevisionID:           "arev_1",
		AgentRevisionToolSetTools: []string{"fetch"},
		AgentRevisionTools:        []string{"file"},
	})
	if hasTool(ceilinged.Tools, "fetch") {
		t.Fatalf("effective tools = %v, want fetch dropped by the {file} ceiling (ceiling intersects last)", ceilinged.Tools)
	}
}

// TestResolveGrantsToolSetsOnANullProjectBaseline answers the DIV-UI-001 blocker (2) question for the MCP
// path specifically: "a fresh project's config_policy is NULL so NO tools are advertised". That is true of a
// run that relies on the PROJECT baseline — Store.ProjectConfig maps a NULL config_policy to the zero
// ConfigPolicy, so ProjectTools is nil and a baseline-only run resolves an empty set. It is NOT true of a run
// that pins an agent revision with tool_sets, which is the ONLY way an MCP tool ever reaches a model: the set
// grant UNIONS onto the empty baseline, and with no declared ceiling (AgentRevisionTools nil — PinnedExecConfig
// leaves it nil for a NULL tools column) nothing intersects it away.
//
// So an operator does NOT have to write a config_policy to connect Jira. This is a regression guard on that:
// if the union were ever reordered behind the baseline, or an empty baseline started short-circuiting, an MCP
// connection would go silently unadvertised — the failure mode is a model that simply never calls the tool.
func TestResolveGrantsToolSetsOnANullProjectBaseline(t *testing.T) {
	// Exactly the shape a fresh project + an MCP-granting agent revision produces: no project baseline, no
	// declared ceiling, one set-granted MCP tool.
	snap := Resolve(ResolveInput{
		DeploymentModel:           "m",
		ProjectTools:              nil, // NULL config_policy
		AgentRevisionID:           "arev_jira",
		AgentRevisionToolSetTools: []string{"jira__getJiraIssue"},
		AgentRevisionTools:        nil, // no declared ceiling
	})
	if !hasTool(snap.Tools, "jira__getJiraIssue") {
		t.Fatalf("effective tools = %v, want the set-granted jira__getJiraIssue on a NULL project baseline "+
			"(a fresh project must not need a config_policy to advertise an MCP tool)", snap.Tools)
	}

	// The complement, so this test cannot pass for the wrong reason: with NO set grant the same fresh project
	// really does resolve nothing — the half of DIV-UI-001 (2) that IS true.
	bare := Resolve(ResolveInput{DeploymentModel: "m", ProjectTools: nil, AgentRevisionID: "arev_jira"})
	if len(bare.Tools) != 0 {
		t.Fatalf("effective tools = %v on a NULL baseline with no grant, want none", bare.Tools)
	}
}

func hasTool(tools []string, name string) bool {
	for _, t := range tools {
		if t == name {
			return true
		}
	}
	return false
}

// TestResolveSkillsFoldIntoHashOnlyWhenPresent pins the progressive-loading + bit-compat discipline
// (spec §28.16, T1 regression): a run with no skills resolves BIT-IDENTICALLY to the pre-skills path
// (nil SkillPinsJSON never touches the hash), while a run with skill pins folds them into both the
// snapshot's Skills rider AND the content hash — so a mid-run enable that changes the pin changes the
// hash, and the checkpoint stays coherent.
func TestResolveSkillsFoldIntoHashOnlyWhenPresent(t *testing.T) {
	base := ResolveInput{
		DeploymentModel:  "model-alpha",
		DeploymentSecret: "openai_api_key",
		ProjectTools:     []string{"palai.conformance.math.add"},
	}
	baseline := Resolve(base)

	// nil skills: bit-identical to the pre-skills resolution.
	base.SkillPinsJSON = nil
	if got := Resolve(base); got.Hash != baseline.Hash || len(got.Skills) != 0 {
		t.Fatalf("nil skills changed resolution: hash %q vs %q, skills %v (must be bit-identical)", got.Hash, baseline.Hash, got.Skills)
	}
	// empty JSON array: still no skills, still bit-identical (an explicit empty pin is a skill-less run).
	base.SkillPinsJSON = []byte(`[]`)
	if got := Resolve(base); got.Hash != baseline.Hash || len(got.Skills) != 0 {
		t.Fatalf("empty skill pins changed the hash: %q vs %q", got.Hash, baseline.Hash)
	}

	// A real pin: Skills populated (name/description/digest/path), hash diverges from the skill-less run.
	base.SkillPinsJSON = []byte(`[{"name":"commit-convention","description":"write commits","digest":"sha256:abc","path":".palai/skills/commit-convention/SKILL.md"}]`)
	withSkill := Resolve(base)
	if len(withSkill.Skills) != 1 || withSkill.Skills[0].Name != "commit-convention" || withSkill.Skills[0].Digest != "sha256:abc" {
		t.Fatalf("skills = %+v, want the parsed pin", withSkill.Skills)
	}
	if withSkill.Hash == baseline.Hash {
		t.Fatalf("a skill pin did not change the hash (%q) — skills must fold into the content address", withSkill.Hash)
	}
}

// TestSkillInstructionsGrantNoCapability is the no-authority core (spec §28.15, TOL-011): a skill's
// requested tools NEVER expand the effective set. Even with a skill pinned that "asks for" push (and a
// project baseline that offers push), the run's effective tools stay the revision-ceiling INTERSECTION —
// push is absent because the ceiling excludes it. The skill pin (SkillPinsJSON) carries no tool-granting
// field at all, so it structurally cannot feed the tool resolution; a hand-crafted pin with an extra
// required_tools key is ignored (SkillRef has no such field). The broker then rejects a tool.request for
// a tool absent from the effective (advertised) set — the skill's instructions grant nothing.
func TestSkillInstructionsGrantNoCapability(t *testing.T) {
	in := ResolveInput{
		DeploymentModel:    "m",
		DeploymentSecret:   "model",
		ProjectTools:       []string{"file", "push"}, // the baseline OFFERS push...
		AgentRevisionID:    "arev_1",
		AgentRevisionTools: []string{"file"}, // ...but the revision ceiling EXCLUDES it
		// A skill pin that names push in a (non-standard) required_tools field — it must be ignored.
		SkillPinsJSON: []byte(`[{"name":"pusher","description":"use the push tool","digest":"sha256:x","path":".palai/skills/pusher/SKILL.md","required_tools":["push"]}]`),
	}
	snap := Resolve(in)
	for _, tool := range snap.Tools {
		if tool == "push" {
			t.Fatalf("effective tools = %v, want NO push — a skill must never grant a tool the ceiling excludes", snap.Tools)
		}
	}
	if len(snap.Skills) != 1 || snap.Skills[0].Name != "pusher" {
		t.Fatalf("skills = %+v, want the pinned skill present (as context, not authority)", snap.Skills)
	}
}
