package extensions

import (
	"context"
	"reflect"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

// TestTransformPatchSchemaValidatedNoCapabilityFields proves the transform-patch shape carries ONLY
// arguments/result and NOTHING else (spec §28.17, TOL-012): the generated contracts.HookPatch type has
// exactly those two fields — there is no tools/model/budget/secret capability field IN THE SCHEMA — and a
// strict decode (DisallowUnknownFields) accepts an arguments/result patch while rejecting anything else.
func TestTransformPatchSchemaValidatedNoCapabilityFields(t *testing.T) {
	// The schema itself carries no capability field: reflect the generated type's fields.
	got := map[string]bool{}
	rt := reflect.TypeOf(contracts.HookPatch{})
	for i := 0; i < rt.NumField(); i++ {
		got[rt.Field(i).Name] = true
	}
	want := map[string]bool{"Arguments": true, "Result": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HookPatch fields = %v, want exactly %v (no capability field may exist in the schema)", got, want)
	}

	// A strict decode accepts an in-schema patch.
	if _, err := decodeHookPatch([]byte(`{"arguments":{"path":"/safe"}}`)); err != nil {
		t.Fatalf("in-schema arguments patch rejected: %v", err)
	}
	if _, err := decodeHookPatch([]byte(`{"result":{"redacted":true}}`)); err != nil {
		t.Fatalf("in-schema result patch rejected: %v", err)
	}
}

// TestHookCannotGrantCapability proves a transform patch can never grant a capability (the exit-gate
// invariant, master plan §8/E12): an "add push tool" patch, or one naming a model/budget/secret, is REJECTED
// by the strict decode — the capability field is not in the schema, so DisallowUnknownFields fails it closed.
func TestHookCannotGrantCapability(t *testing.T) {
	for _, body := range []string{
		`{"tools":["push"]}`,                     // add a tool
		`{"arguments":{"x":1},"tools":["push"]}`, // smuggle a tool alongside a legit patch
		`{"model":"gpt-4o"}`,                     // switch the model
		`{"budget":999999}`,                      // widen the budget
		`{"secret":"sk-live-xxx"}`,               // inject a secret
		`{"capabilities":["admin"]}`,             // grant a capability set
	} {
		if _, err := decodeHookPatch([]byte(body)); err == nil {
			t.Fatalf("capability-granting patch accepted, want reject: %s", body)
		}
	}
}

// TestTransformPatchThreadsArguments proves a before_tool transform hook's arguments patch replaces the args
// the tool runs with (threaded into the returned payload), while a before_tool transform that tries to patch
// result is fail-closed (out of its category's patch surface).
func TestTransformPatchThreadsArguments(t *testing.T) {
	s := &Store{hookHandlers: map[string]HookHandler{
		"rewrite_args": func(_ context.Context, ev HookEvent) (HookDecision, error) {
			return HookDecision{Patch: &contracts.HookPatch{Arguments: map[string]any{"path": "/redacted"}}}, nil
		},
		"patch_result_at_before_tool": func(_ context.Context, ev HookEvent) (HookDecision, error) {
			return HookDecision{Patch: &contracts.HookPatch{Result: map[string]any{"leak": true}}}, nil
		},
	}}

	hooks := []loadedHook{{ID: "hook_t", Point: HookPointBeforeTool, Category: HookCategoryTransform, Executor: HookExecutorInline, Handler: "rewrite_args"}}
	out, err := s.fireLoaded(context.Background(), HookEvent{Point: HookPointBeforeTool, Payload: map[string]any{"tool_name": "file", "arguments": map[string]any{"path": "/etc/secret"}}}, hooks)
	if err != nil || out.Denied {
		t.Fatalf("transform arguments patch = (%+v, %v), want applied not denied", out, err)
	}
	patched, _ := out.Payload["arguments"].(map[string]any)
	if patched["path"] != "/redacted" {
		t.Fatalf("arguments not patched: %v", out.Payload["arguments"])
	}

	// A before_tool transform touching result is out of surface → fail-closed (deny).
	badHooks := []loadedHook{{ID: "hook_b", Point: HookPointBeforeTool, Category: HookCategoryTransform, Executor: HookExecutorInline, Handler: "patch_result_at_before_tool"}}
	denied, err := s.fireLoaded(context.Background(), HookEvent{Point: HookPointBeforeTool, Payload: map[string]any{"tool_name": "file", "arguments": map[string]any{}}}, badHooks)
	if err != nil || !denied.Denied {
		t.Fatalf("out-of-surface before_tool result patch = (%+v, %v), want fail-closed deny", denied, err)
	}
}
