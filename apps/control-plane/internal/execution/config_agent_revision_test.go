package execution

import (
	"encoding/json"
	"testing"
)

// TestRevisionModelPinsBetweenDeploymentAndSession proves the pinned-agent-revision layer sits
// between the deployment default and the session/run override (spec §14.3 "profile+overrides"):
// a revision's model wins over the deployment default but a session override still wins over the
// revision, and the effective value's provenance names the layer that set it.
func TestRevisionModelPinsBetweenDeploymentAndSession(t *testing.T) {
	base := ResolveInput{
		DeploymentModel:    "model-alpha",
		DeploymentSecret:   "openai_api_key",
		ProjectTools:       []string{"a", "b"},
		AgentRevisionID:    "arev_1",
		AgentRevisionModel: "model-pinned",
	}

	// The revision pins the model over the deployment default; provenance is the revision layer,
	// and the snapshot carries the pinned revision id in provenance (AGT-001).
	snap := Resolve(base)
	if snap.Model != "model-pinned" || snap.Provenance["model"] != layerAgentRevision {
		t.Fatalf("revision pin: model = %q prov = %q, want model-pinned from %q", snap.Model, snap.Provenance["model"], layerAgentRevision)
	}
	if snap.Provenance["agent_revision"] != "arev_1" {
		t.Fatalf("provenance agent_revision = %q, want the pinned revision id arev_1", snap.Provenance["agent_revision"])
	}

	// A session override still wins over the pinned revision (overrides modify the revision baseline).
	over := base
	over.SessionModel = "model-session"
	snap2 := Resolve(over)
	if snap2.Model != "model-session" || snap2.Provenance["model"] != layerSession {
		t.Fatalf("session over revision: model = %q prov = %q, want model-session from session", snap2.Model, snap2.Provenance["model"])
	}

	// A different revision that pins a different model re-addresses the content hash, so the
	// checkpoint config hash reflects a revision change (checkpoint.go effectiveConfigHash).
	other := base
	other.AgentRevisionID, other.AgentRevisionModel = "arev_2", "model-other"
	if Resolve(other).Hash == snap.Hash {
		t.Fatal("a revision that pins a different model must change the content address")
	}
}

// TestRevisionToolCeilingIntersectsRunTools proves the revision's tool set is a capability CEILING:
// the effective tool list is the intersection of the resolved tools with the revision's declared
// set, so a tool the revision does not declare NEVER enters the effective registry — even when the
// project baseline or a session override carries it (63.4 "capability never expands"). The cut stays
// inside the pure resolver.
func TestRevisionToolCeilingIntersectsRunTools(t *testing.T) {
	in := ResolveInput{
		DeploymentModel:    "m",
		ProjectTools:       []string{"file", "shell", "web"},
		AgentRevisionID:    "arev_1",
		AgentRevisionTools: []string{"file", "shell"}, // the ceiling — no "web"
	}
	snap := Resolve(in)

	got := map[string]bool{}
	for _, tool := range snap.Tools {
		got[tool] = true
	}
	if !got["file"] || !got["shell"] {
		t.Fatalf("effective tools = %v, want the ceiling ∩ project baseline {file, shell}", snap.Tools)
	}
	if got["web"] {
		t.Fatalf("effective tools = %v, want 'web' excluded — it is outside the revision ceiling", snap.Tools)
	}
	if snap.Provenance["tools"] != layerAgentRevision {
		t.Fatalf("tools provenance = %q, want %q (the revision bounded the set)", snap.Provenance["tools"], layerAgentRevision)
	}

	// A session override that names a tool outside the ceiling is still capped: the override picks a
	// subset, the revision ceiling remains the maximum.
	over := in
	over.SessionTools = []string{"file", "web"}
	snapOver := Resolve(over)
	for _, tool := range snapOver.Tools {
		if tool == "web" {
			t.Fatalf("session override escaped the ceiling: %v still contains 'web'", snapOver.Tools)
		}
	}

	// The cap is reflected in the content hash: the same run WITHOUT the ceiling addresses differently.
	uncapped := in
	uncapped.AgentRevisionID, uncapped.AgentRevisionTools = "", nil
	if Resolve(uncapped).Hash == snap.Hash {
		t.Fatal("a tool ceiling must change the content address relative to the uncapped resolution")
	}

	// Redaction/shape sanity: the snapshot is still plain JSON with no surprise fields.
	if _, err := json.Marshal(snap); err != nil {
		t.Fatalf("snapshot marshal error = %v", err)
	}
}
