package execution

import "testing"

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
