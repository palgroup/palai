package execution

import "testing"

// TestResearchRequiresNetworkCapabilityAndNeverPublishCapability pins the journey 63.4 pass condition
// for the research tool: "network capability" is simply the tool NAME being in the effective set, and a
// grant NEVER expands — a revision that ceilings research in does not thereby admit a publish tool, and
// a revision that omits research can never surface it (default DENY). No new capability machine: this is
// the existing intersectTools invariant, pinned with the research + publish tool names.
func TestResearchRequiresNetworkCapabilityAndNeverPublishCapability(t *testing.T) {
	const (
		research = "palai.research.fetch"
		publish  = "palai.publish.push"
	)

	// The revision ceilings research IN; the project baseline ALSO offers a publish tool. The effective
	// set admits research (the network capability) but NOT publish — capability does not expand.
	snap := Resolve(ResolveInput{
		DeploymentModel:    "m",
		ProjectTools:       []string{research, publish},
		AgentRevisionID:    "arev_research",
		AgentRevisionTools: []string{research}, // ceiling: research only
	})
	got := map[string]bool{}
	for _, tool := range snap.Tools {
		got[tool] = true
	}
	if !got[research] {
		t.Fatalf("effective tools = %v, want the network capability %q present", snap.Tools, research)
	}
	if got[publish] {
		t.Fatalf("effective tools = %v, want %q EXCLUDED — a research grant never expands to publish", snap.Tools, publish)
	}

	// A revision whose ceiling omits research can never surface it, even when the project baseline (or a
	// session override) names it — the ceiling is the maximum and research is outside it (default DENY).
	capped := Resolve(ResolveInput{
		DeploymentModel:    "m",
		ProjectTools:       []string{research, publish},
		AgentRevisionID:    "arev_no_research",
		AgentRevisionTools: []string{publish}, // ceiling excludes research
		SessionTools:       []string{research}, // an override that tries to re-add it
	})
	for _, tool := range capped.Tools {
		if tool == research {
			t.Fatalf("effective tools = %v; research escaped a ceiling that excludes it", capped.Tools)
		}
	}
}
