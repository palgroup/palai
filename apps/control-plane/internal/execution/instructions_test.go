package execution

import (
	"strings"
	"testing"

	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// TestThePlatformTextReachesTheModelRequest is the end this layer exists for. Every other test here
// proves the layer RESOLVES; only this one proves the bytes land in the messages a provider is
// called with. The distinction is the one this tree keeps paying for — a route existing is not a
// field being accepted — and the seam it drives is the whole production chain from
// model_dispatch.go:178: resolveInstructionLayers feeding applyInstructionLayers.
//
// IT ALSO PINS THE ORDER ON THE WIRE. The engine's kernel turn stays first because
// applyInstructionLayers inserts after any leading system messages; platform comes next; tenant text
// after that. A revision string arriving ahead of the platform text would be a caller-supplied
// instruction sitting where platform guidance should be.
func TestThePlatformTextReachesTheModelRequest(t *testing.T) {
	const revisionFixture = "the pinned revision's own instruction"
	engineAssembled := []modelbroker.Message{
		{Role: "system", Content: "the engine's kernel turn"},
		{Role: "user", Content: "do the thing"},
	}

	out := applyInstructionLayers(engineAssembled, resolveInstructionLayers(revisionFixture, ""))

	var systemTurns []string
	for _, m := range out {
		if m.Role == "system" {
			systemTurns = append(systemTurns, m.Content)
		}
	}
	if len(systemTurns) < 3 {
		t.Fatalf("got %d system turns, want the kernel turn + platform + revision", len(systemTurns))
	}

	platformAt, revisionAt := -1, -1
	for i, turn := range systemTurns {
		switch {
		case turn == platformInstructions:
			platformAt = i
		case strings.Contains(turn, revisionFixture):
			revisionAt = i
		}
	}
	if platformAt < 0 {
		t.Fatal("the platform text is in no system turn: the layer resolves but nothing sends it")
	}
	if systemTurns[0] != "the engine's kernel turn" {
		t.Errorf("system turn 0 is %q, want the engine's own turn to stay first", systemTurns[0])
	}
	if revisionAt >= 0 && revisionAt < platformAt {
		t.Errorf("the revision's instruction is at %d and platform at %d: tenant text must not lead",
			revisionAt, platformAt)
	}
	// The user turn must survive, and stay after every system turn.
	if last := out[len(out)-1]; last.Role != "user" || last.Content != "do the thing" {
		t.Errorf("the user turn was displaced: last message is %+v", last)
	}
}

// TestThePlatformLayerLeadsAndTenantTextFollows pins the ONE ordering property that matters for
// authority: platform discipline is stated first, and a revision or run may narrow it by speaking
// after it. Reversed, a tenant-supplied string would sit in the position that overrides platform
// text — the same untrusted-claim shape the tool-description rule exists to prevent.
func TestThePlatformLayerLeadsAndTenantTextFollows(t *testing.T) {
	layers := resolveInstructionLayers("the revision's own instruction", "the run's own instruction")
	if len(layers) != 3 {
		t.Fatalf("got %d layers, want platform + revision + run", len(layers))
	}
	if layers[0].Layer != layerInstructionsPlatform {
		t.Errorf("layer 0 is %q, want the platform layer to lead", layers[0].Layer)
	}
	if layers[0].Text == "" {
		t.Error("the platform layer resolved to empty text; a layer that says nothing is not a layer")
	}
	if layers[1].Layer != layerInstructionsRevision || layers[2].Layer != layerInstructionsRun {
		t.Errorf("tenant layers are %q,%q — want revision then run, both after platform",
			layers[1].Layer, layers[2].Layer)
	}
	for i, l := range layers[1:] {
		if l.Layer == layerInstructionsPlatform {
			t.Errorf("the platform layer appears again at position %d", i+1)
		}
	}
}

// TestThePlatformLayerIsPresentWithNoTenantText — the platform layer is UNCONDITIONAL. A run with no
// revision instruction and no run instruction still carries it, because working discipline is not
// something a tenant opts into. Before this layer existed, such a run reached the model with nothing
// but the engine's own 27-word protocol turn.
func TestThePlatformLayerIsPresentWithNoTenantText(t *testing.T) {
	layers := resolveInstructionLayers("", "")
	if len(layers) != 1 {
		t.Fatalf("got %d layers, want exactly the platform layer", len(layers))
	}
	if layers[0].Layer != layerInstructionsPlatform {
		t.Fatalf("the single layer is %q, want the platform layer", layers[0].Layer)
	}
}

// TestTenantTextStillComposesInOrder guards the behaviour that already existed: a revision-only run
// and a run-only run each carry their one tenant layer, after platform. This is here so the new
// leading layer cannot quietly drop one of them.
func TestTenantTextStillComposesInOrder(t *testing.T) {
	revisionOnly := resolveInstructionLayers("revision", "")
	if len(revisionOnly) != 2 || revisionOnly[1].Layer != layerInstructionsRevision {
		t.Errorf("revision-only run resolved to %v", revisionOnly)
	}
	runOnly := resolveInstructionLayers("", "run")
	if len(runOnly) != 2 || runOnly[1].Layer != layerInstructionsRun {
		t.Errorf("run-only run resolved to %v", runOnly)
	}
}
