package statemachines

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The registry and AsyncAPI live at the repo root; go test runs each package's
// test binary with the package directory as its working directory.
var (
	registryPath = filepath.Join("..", "..", "protocols", "schemas", "execution", "event-types.json")
	asyncAPIPath = filepath.Join("..", "..", "protocols", "asyncapi", "asyncapi-3.1.yaml")
)

// registryEvents reads the ordered event list from event-types.json.
func registryEvents(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(registryPath)
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

// TestEveryTableEventExistsInRegistry checks the subset direction only: every
// event a table can emit is a registered name. The reverse does not hold — the
// registry also carries genesis events (born-into-state, no table row) and events
// owned by later epics.
func TestEveryTableEventExistsInRegistry(t *testing.T) {
	registry := map[string]bool{}
	for _, e := range registryEvents(t) {
		registry[e] = true
	}
	for _, spec := range allTables() {
		for _, r := range spec.rows {
			if !registry[r.event] {
				t.Errorf("%s: event %q missing from registry", spec.name, r.event)
			}
		}
	}
}

// TestRegistryMatchesAsyncAPIEventTypes enforces the Task 5 obligation: the JSON
// registry and the AsyncAPI x-event-types list are identical in order and
// content, so the two sources of truth cannot drift.
func TestRegistryMatchesAsyncAPIEventTypes(t *testing.T) {
	registry := registryEvents(t)
	async := asyncAPIEventTypes(t)
	if len(registry) != len(async) {
		t.Fatalf("count mismatch: registry has %d, asyncapi has %d", len(registry), len(async))
	}
	for i := range registry {
		if registry[i] != async[i] {
			t.Errorf("index %d: registry %q != asyncapi %q", i, registry[i], async[i])
		}
	}
}

// asyncAPIEventTypes extracts the x-event-types string list. ponytail: the block
// is a flat YAML list of scalars, so a targeted line scan reads it without a YAML
// dependency (packages/state-machines is stdlib-only).
func asyncAPIEventTypes(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(asyncAPIPath)
	if err != nil {
		t.Fatalf("read asyncapi: %v", err)
	}
	var out []string
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !inBlock {
			if trimmed == "x-event-types:" {
				inBlock = true
			}
			continue
		}
		item, ok := strings.CutPrefix(trimmed, "- ")
		if !ok {
			break // the first non-item line ends the block
		}
		out = append(out, strings.TrimSpace(item))
	}
	if len(out) == 0 {
		t.Fatal("no x-event-types entries found in asyncapi")
	}
	return out
}
