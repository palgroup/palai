package extensions

import (
	"context"
	"errors"
	"testing"

	remotehttp "github.com/palgroup/palai/adapters/tools/http"
)

// TestHookSecurityCapabilityEscalationDenied is the E12 Task 8 capability-escalation corpus (spec §28.17, the
// master-plan §8/E12 exit-gate invariant): every hostile hook config or output is DENIED. It runs in-package
// (the extensions store + strict decoders are internal — the internal-import constraint keeps the corpus out
// of tests/, the T7 skills-security precedent). Selected by name in the security tier.
func TestHookSecurityCapabilityEscalationDenied(t *testing.T) {
	// 1. A transform patch can NEVER grant a capability: an added tool, a switched model, a widened budget,
	// or an injected secret is rejected by the strict decode (the field is not in the schema).
	for _, patch := range []string{
		`{"tools":["push"]}`,
		`{"arguments":{"ok":1},"tools":["push"]}`, // smuggled alongside a legit patch
		`{"model":"gpt-4o"}`,
		`{"budget":999999}`,
		`{"secret":"sk-live-xxx"}`,
		`{"capabilities":["admin"]}`,
	} {
		if _, err := decodeHookPatch([]byte(patch)); err == nil {
			t.Fatalf("transform patch granted a capability (accepted): %s", patch)
		}
	}

	// 2. Unknown-point SMUGGLING: a hook aimed at a point that does not exist (a phantom fire site) is a
	// create reject — dead/hidden config can never be stored.
	if _, err := DecodeHookInput([]byte(`{"name":"x","hook_point":"before_secret_read","category":"observer","executor":"platform_inline","config":{"handler":"h"}}`)); !errors.Is(err, ErrUnknownHookPoint) {
		t.Fatalf("unknown-point smuggling accepted, want ErrUnknownHookPoint: %v", err)
	}

	// 3. An INLINE credential is rejected — a secret is only ever a secret_ref handle. Both a top-level
	// `secret` (DisallowUnknownFields) and a credential-shaped config key (the allowlist) are refused.
	for _, body := range []string{
		`{"name":"x","hook_point":"before_tool","category":"policy","executor":"remote_http","config":{"url":"https://h/x"},"secret":"sk-live"}`,
		`{"name":"x","hook_point":"before_tool","category":"policy","executor":"remote_http","config":{"url":"https://h/x","token":"sk-live"},"secret_ref":"s"}`,
	} {
		if _, err := DecodeHookInput([]byte(body)); err == nil {
			t.Fatalf("inline credential accepted, want reject: %s", body)
		}
	}
}

// TestHookSecurityRemoteHookGetsNoPlatformAuthority is the hook confused-deputy invariant (the T4
// TestUpstreamTokenNeverForwarded sibling, spec §28.17): a remote hook worker receives ONLY the hook's OWN
// signing secret, resolved fresh per invoke — never a platform/tenant credential. The tool-http.v1 Invocation
// carries no platform-authority field at all, so a hostile hook worker cannot be handed a token to replay
// against the control plane.
func TestHookSecurityRemoteHookGetsNoPlatformAuthority(t *testing.T) {
	capture := &captureInvoker{resp: map[string]any{"decision": "allow"}}
	s := New(nil)
	// The resolver returns the HOOK's org-scoped secret. A platform bearer would be a DIFFERENT value the
	// binding must never thread; there is no field on the Invocation for one.
	s.SetRemoteInvoker(capture, func(org, ref string) ([]byte, error) {
		return []byte("hook-own-secret-" + ref), nil
	})
	hook := loadedHook{ID: "hook_x", Point: HookPointBeforeTool, Category: HookCategoryPolicy, Executor: HookExecutorRemote, URL: "https://hooks.example/x", SecretRef: "sref_hook"}
	ev := HookEvent{Org: "org_1", Project: "prj_1", RunID: "run_1", Point: HookPointBeforeTool, Payload: map[string]any{"tool_name": "push"}}

	if _, err := s.fireLoaded(context.Background(), ev, []loadedHook{hook}); err != nil {
		t.Fatalf("fireLoaded() error = %v", err)
	}
	// The invoke is signed with the HOOK's own secret — not a platform credential.
	if string(capture.got.Secret) != "hook-own-secret-sref_hook" {
		t.Fatalf("remote hook signed with %q, want the hook's own resolved secret", capture.got.Secret)
	}
	// The envelope carries only the hook's secret_ref handle; there is structurally no platform-token field.
	if capture.got.SecretRef != "sref_hook" {
		t.Fatalf("Invocation SecretRef = %q, want the hook's own handle", capture.got.SecretRef)
	}
	// The signed envelope is a tool-http.v1 invoke (the shared signer), never a platform-authenticated call.
	_ = remotehttp.Protocol
}
