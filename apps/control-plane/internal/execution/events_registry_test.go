package execution

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestEmittedOrchestratorEventsAreInCanonicalRegistry checks the subset direction: every
// event type the execution package journals is a name the canonical registry already
// carries (protocols/schemas/execution/event-types.json, derived from spec §13.3/§21.3).
// It guards the Step 7 fold — a future ad-hoc event name outside the registry fails here
// rather than reaching the journal. The reverse does not hold: the registry also carries
// genesis and later-epic events with no execution emitter.
func TestEmittedOrchestratorEventsAreInCanonicalRegistry(t *testing.T) {
	registry := map[string]bool{}
	for _, e := range registryEvents(t) {
		registry[e] = true
	}
	if len(emittedEventTypes) == 0 {
		t.Fatal("execution package emits no event types")
	}
	for _, e := range emittedEventTypes {
		if !registry[e] {
			t.Errorf("emitted event %q is not in the canonical registry", e)
		}
	}
}

// registryEvents reads the ordered event list from event-types.json. The registry lives
// at the repo root; go test runs with the package directory as its working directory, so
// the path climbs from apps/control-plane/internal/execution to the root.
func registryEvents(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "protocols", "schemas", "execution", "event-types.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	var doc struct {
		Events []string `json:"events"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse registry: %v", err)
	}
	if len(doc.Events) == 0 {
		t.Fatal("registry lists no events")
	}
	return doc.Events
}
