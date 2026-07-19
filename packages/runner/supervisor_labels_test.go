package runner

import (
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

// TestBuildSpecTagsEngineWithComposeProject proves H4's runner half: every engine sandbox
// carries the compose-project label (io.palai.project) taken from PALAI_COMPOSE_PROJECT, so
// `palai local down` can force-remove exactly this stack's orphaned engines. Outside compose
// (env unset) only the base sandbox label is applied, so the fault/security tiers that count
// io.palai.sandbox=engine are unaffected.
func TestBuildSpecTagsEngineWithComposeProject(t *testing.T) {
	t.Setenv("PALAI_COMPOSE_PROJECT", "palai-h4test")
	spec := buildSpec(EngineRequest{RunID: contracts.RunID("run_x"), AttemptID: contracts.AttemptID("att_x")})

	if got := spec.Labels[sandboxLabelKey]; got != "engine" {
		t.Fatalf("sandbox label = %q, want engine", got)
	}
	if got := spec.Labels[composeProjectLabelKey]; got != "palai-h4test" {
		t.Fatalf("compose-project label = %q, want palai-h4test", got)
	}
}

// TestBuildSpecOmitsProjectLabelOutsideCompose proves the label is absent when the runner is
// not part of a compose stack, so a bare or test invocation adds no stray project label.
func TestBuildSpecOmitsProjectLabelOutsideCompose(t *testing.T) {
	t.Setenv("PALAI_COMPOSE_PROJECT", "")
	spec := buildSpec(EngineRequest{RunID: contracts.RunID("run_x"), AttemptID: contracts.AttemptID("att_x")})

	if _, ok := spec.Labels[composeProjectLabelKey]; ok {
		t.Fatalf("compose-project label present with PALAI_COMPOSE_PROJECT unset: %v", spec.Labels)
	}
}
