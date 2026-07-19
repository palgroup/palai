package contracts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

// readProtocolSchema loads a JSONL protocol schema from protocols/<rel>. The
// engine and runner frame schemas live under their own protocol roots
// (protocols/engine, protocols/runner), outside the canonical schemas tree that
// readSchema serves.
func readProtocolSchema(t *testing.T, rel string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "protocols", rel))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// knownTypes reads a documented-open frame-type registry ($defs.<name>.enum) and
// returns its members sorted, so a test can assert the registry covers a spec
// table without depending on declaration order.
func knownTypes(t *testing.T, defs map[string]any, name string) []string {
	t.Helper()
	def, ok := defs[name].(map[string]any)
	if !ok {
		t.Fatalf("$defs.%s is missing", name)
	}
	types := schemaStrings(t, def["enum"])
	slices.Sort(types)
	return types
}

func TestEngineFrameRequiresProtocolIDTypeSequenceTime(t *testing.T) {
	schema := readProtocolSchema(t, "engine/engine.schema.json")
	got := schemaStrings(t, schema["required"])
	// The JSONL frame envelope always carries these five fields (spec §25.5).
	want := []string{"protocol", "id", "type", "sequence", "time"}
	if !slices.Equal(got, want) {
		t.Fatalf("engine.schema.json required\n got = %v\nwant = %v", got, want)
	}
	props, _ := schema["properties"].(map[string]any)
	protocol, _ := props["protocol"].(map[string]any)
	if protocol["const"] != "engine.v1" {
		t.Fatalf("protocol.const = %v, want \"engine.v1\"", protocol["const"])
	}
	seq, _ := props["sequence"].(map[string]any)
	if seq["type"] != "integer" || seq["minimum"] != float64(1) {
		t.Fatalf("sequence def = %v, want {integer, minimum 1}", seq)
	}

	// The generated envelope carries every required attribute and round-trips a
	// populated frame without losing them.
	f := contracts.EngineFrame{
		Protocol: "engine.v1",
		ID:       contracts.FrameID("frm_abc123"),
		Type:     "run.start",
		Sequence: 1,
		Time:     "2026-07-16T12:00:00Z",
		Data:     map[string]any{"k": "v"},
	}
	var round contracts.EngineFrame
	roundTrip(t, f, &round)
	if round.Protocol != f.Protocol || round.ID != f.ID || round.Type != f.Type ||
		round.Sequence != f.Sequence || round.Time != f.Time {
		t.Fatalf("round-trip lost required attributes: %+v", round)
	}
}

func TestEngineFrameKnownTypesCoverSpecTables(t *testing.T) {
	schema := readProtocolSchema(t, "engine/engine.schema.json")
	defs, ok := schema["$defs"].(map[string]any)
	if !ok {
		t.Fatal("engine.schema.json has no $defs")
	}

	// §25.7 controller-to-engine frames: the 12-row table carries 13 frame types
	// (model.result and model.delta share one row), plus supervisor.hello from the
	// §25.6 handshake — 14 documented controller types.
	controller := knownTypes(t, defs, "controller_types")
	wantController := []string{
		"approval.result", "checkpoint.request", "child.result", "config.change",
		"message.deliver", "model.delta", "model.result", "protocol.ack",
		"run.cancel", "run.pause", "run.restore", "run.start", "supervisor.hello",
		"tool.result",
	}
	if !slices.Equal(controller, wantController) {
		t.Fatalf("controller_types\n got = %v\nwant = %v", controller, wantController)
	}

	// §25.8 engine-to-controller frames: the 13-row table carries 16 frame types
	// (engine.ready/heartbeat, output.delta/item, and warning/protocol.error each
	// share a row).
	engine := knownTypes(t, defs, "engine_types")
	wantEngine := []string{
		"approval.request", "checkpoint.offer", "child.request", "context.compacted",
		"engine.heartbeat", "engine.ready", "model.request", "output.delta",
		"output.item", "progress", "protocol.ack", "protocol.error", "run.terminal",
		"run.waiting", "tool.request", "warning",
	}
	if !slices.Equal(engine, wantEngine) {
		t.Fatalf("engine_types\n got = %v\nwant = %v", engine, wantEngine)
	}

	// type stays an open string: an unsupported type is preserved (it produces a
	// protocol.error at runtime), never coerced or rejected by the envelope
	// (API-009, spec §25.5).
	props, _ := schema["properties"].(map[string]any)
	typeProp, _ := props["type"].(map[string]any)
	if typeProp["type"] != "string" {
		t.Fatalf("type.type = %v, want string", typeProp["type"])
	}
	if _, closed := typeProp["enum"]; closed {
		t.Fatal("type property carries an exclusive enum; frame types must stay open")
	}
	f := contracts.EngineFrame{
		Protocol: "engine.v1",
		ID:       contracts.FrameID("frm_abc123"),
		Type:     "holo.custom.frame",
		Sequence: 1,
		Time:     "2026-07-16T12:00:00Z",
	}
	var round contracts.EngineFrame
	roundTrip(t, f, &round)
	if round.Type != "holo.custom.frame" {
		t.Fatalf("unknown frame type not preserved: %q", round.Type)
	}
}

func TestEngineFrameMaxLineBytesDocumented(t *testing.T) {
	schema := readProtocolSchema(t, "engine/engine.schema.json")
	defs, _ := schema["$defs"].(map[string]any)
	limits, ok := defs["limits"].(map[string]any)
	if !ok {
		t.Fatal("engine.schema.json $defs.limits is missing")
	}
	// Maximum line size is one MiB by default (spec §25.5); larger content uses
	// artifact references instead of inline frame payloads.
	if limits["max_line_bytes"] != float64(1048576) {
		t.Fatalf("max_line_bytes = %v, want 1048576", limits["max_line_bytes"])
	}

	// The documented ceiling is a real budget: a minimal frame marshals well
	// within it through the generated envelope.
	f := contracts.EngineFrame{
		Protocol: "engine.v1",
		ID:       contracts.FrameID("frm_abc123"),
		Type:     "engine.heartbeat",
		Sequence: 1,
		Time:     "2026-07-16T12:00:00Z",
	}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= 1048576 {
		t.Fatalf("minimal frame is %d bytes, exceeds max_line_bytes", len(data))
	}
}

// engineBranchData returns the then.properties.data schema of the allOf branch that gates
// on the given frame type, reading the shape straight from the canonical schema.
func engineBranchData(t *testing.T, schema map[string]any, typ string) map[string]any {
	t.Helper()
	allOf, _ := schema["allOf"].([]any)
	for _, raw := range allOf {
		entry, _ := raw.(map[string]any)
		ifObj, _ := entry["if"].(map[string]any)
		ifProps, _ := ifObj["properties"].(map[string]any)
		typeGate, _ := ifProps["type"].(map[string]any)
		if !gateMatches(typeGate, typ) {
			continue
		}
		then, _ := entry["then"].(map[string]any)
		props, _ := then["properties"].(map[string]any)
		data, ok := props["data"].(map[string]any)
		if !ok {
			t.Fatalf("%q branch has no then.properties.data", typ)
		}
		return data
	}
	t.Fatalf("no allOf branch gates on %q", typ)
	return nil
}

// TestRunStartCarriesInputAndHistory pins the run.start data shape: it requires input and
// documents the assembled prior-response history channel (messages), so session chaining
// cannot silently drift the frame shape (spec §9, §22.2; LP Task 9 schema-pin pattern).
func TestRunStartCarriesInputAndHistory(t *testing.T) {
	schema := readProtocolSchema(t, "engine/engine.schema.json")
	data := engineBranchData(t, schema, "run.start")

	if req := schemaStrings(t, data["required"]); !slices.Contains(req, "input") {
		t.Fatalf("run.start data.required = %v, want it to require input", req)
	}
	props, _ := data["properties"].(map[string]any)
	messages, ok := props["messages"].(map[string]any)
	if !ok {
		t.Fatal("run.start data.properties is missing the messages (history) field")
	}
	if messages["type"] != "array" {
		t.Fatalf("run.start messages type = %v, want array", messages["type"])
	}

	// The generated envelope round-trips a chained run.start (input + history) without loss.
	f := contracts.EngineFrame{
		Protocol: "engine.v1",
		ID:       contracts.FrameID("frm_abc123"),
		Type:     "run.start",
		Sequence: 2,
		Time:     "2026-07-16T12:00:00Z",
		Data: map[string]any{
			"input":    "second turn",
			"messages": []any{map[string]any{"role": "assistant", "content": "prior output"}},
		},
	}
	var round contracts.EngineFrame
	roundTrip(t, f, &round)
	if round.Data["input"] != "second turn" {
		t.Fatalf("round-trip lost run.start input: %+v", round.Data)
	}
	if _, ok := round.Data["messages"].([]any); !ok {
		t.Fatalf("round-trip lost run.start history: %+v", round.Data)
	}
}

func TestRunnerLeaseMessagesRequireFence(t *testing.T) {
	schema := readProtocolSchema(t, "runner/runner.schema.json")

	props, _ := schema["properties"].(map[string]any)
	protocol, _ := props["protocol"].(map[string]any)
	if protocol["const"] != "runner.v1" {
		t.Fatalf("protocol.const = %v, want \"runner.v1\"", protocol["const"])
	}
	fence, _ := props["fence"].(map[string]any)
	if fence["type"] != "integer" || fence["minimum"] != float64(1) {
		t.Fatalf("fence def = %v, want {integer, minimum 1}", fence)
	}

	// lease.accept/renew/complete mutate lease state and must carry a fencing
	// token so a recovered stale holder is rejected; lease.offer/revoke carry the
	// lease identity triple but no fence (spec §25 fencing; runner.v1 leasing).
	for _, typ := range []string{"lease.accept", "lease.renew", "lease.complete"} {
		req := leaseRequired(t, schema, typ)
		for _, field := range []string{"lease_id", "run_id", "attempt_id", "fence"} {
			if !slices.Contains(req, field) {
				t.Fatalf("%s required %v is missing %q", typ, req, field)
			}
		}
	}
	for _, typ := range []string{"lease.offer", "lease.revoke"} {
		req := leaseRequired(t, schema, typ)
		for _, field := range []string{"lease_id", "run_id", "attempt_id"} {
			if !slices.Contains(req, field) {
				t.Fatalf("%s required %v is missing %q", typ, req, field)
			}
		}
		if slices.Contains(req, "fence") {
			t.Fatalf("%s must not require a fence, got %v", typ, req)
		}
	}

	// The generated message round-trips a fenced lease.accept without losing the
	// fence or the lease identity triple.
	m := contracts.RunnerMessage{
		Protocol:  "runner.v1",
		Type:      "lease.accept",
		Time:      "2026-07-16T12:00:00Z",
		LeaseID:   "lease_abc123",
		RunID:     contracts.RunID("run_abc123"),
		AttemptID: contracts.AttemptID("att_abc123"),
		Fence:     7,
	}
	var round contracts.RunnerMessage
	roundTrip(t, m, &round)
	if round.Fence != 7 || round.LeaseID != m.LeaseID || round.RunID != m.RunID || round.AttemptID != m.AttemptID {
		t.Fatalf("round-trip lost lease identity/fence: %+v", round)
	}
}

// leaseRequired unions the then.required fields of every allOf branch whose
// if-gate matches typ, whether the gate uses a type const or an enum. It reads
// the requirement straight from the canonical schema rather than a copied list.
func leaseRequired(t *testing.T, schema map[string]any, typ string) []string {
	t.Helper()
	allOf, ok := schema["allOf"].([]any)
	if !ok || len(allOf) == 0 {
		t.Fatal("runner.schema.json has no allOf branches")
	}
	var required []string
	for _, raw := range allOf {
		entry, _ := raw.(map[string]any)
		ifObj, _ := entry["if"].(map[string]any)
		ifProps, _ := ifObj["properties"].(map[string]any)
		typeGate, _ := ifProps["type"].(map[string]any)
		if !gateMatches(typeGate, typ) {
			continue
		}
		then, _ := entry["then"].(map[string]any)
		reqRaw, ok := then["required"]
		if !ok {
			continue // a payload-shape branch (e.g. lease.offer data) sets no required
		}
		for _, field := range schemaStrings(t, reqRaw) {
			if !slices.Contains(required, field) {
				required = append(required, field)
			}
		}
	}
	if len(required) == 0 {
		t.Fatalf("no lease branch gates on %q", typ)
	}
	return required
}

// gateMatches reports whether an if-gate on the type property selects typ,
// supporting both a single const and an enum of types.
func gateMatches(typeGate map[string]any, typ string) bool {
	if typeGate["const"] == typ {
		return true
	}
	list, ok := typeGate["enum"].([]any)
	if !ok {
		return false
	}
	return slices.Contains(list, any(typ))
}
