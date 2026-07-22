package extensions

import (
	"context"
	"testing"

	remotehttp "github.com/palgroup/palai/adapters/tools/http"
)

// captureInvoker is a fake RemoteInvoker: it records the Invocation the hook binding builds and returns a
// scripted inline response, so a unit test asserts the tool-http.v1 envelope + the fresh-secret resolution
// WITHOUT a real HTTP round-trip (the component tier does the real local-HTTP HMAC verify).
type captureInvoker struct {
	got  remotehttp.Invocation
	resp map[string]any
	err  error
}

func (c *captureInvoker) Invoke(_ context.Context, in remotehttp.Invocation) (map[string]any, error) {
	c.got = in
	return c.resp, c.err
}

// TestRemoteHookUsesSignedTransport proves a remote_http hook reuses the T4 signed transport (spec §28.17):
// the binding builds a tool-http.v1 Invocation carrying the hook's url + the FRESH org-scoped secret (never a
// held closure) + the hook identity, and a policy deny response is interpreted as a fail-closed deny. The
// secret is resolved per-invoke through the org-scoped resolver — the same posture the remote-tool executor
// enforces (TestUpstreamTokenNeverForwarded's sibling).
func TestRemoteHookUsesSignedTransport(t *testing.T) {
	invoker := &captureInvoker{resp: map[string]any{"decision": "deny", "reason": "the push tool is not permitted"}}
	var resolvedOrg, resolvedRef string
	s := New(nil)
	s.SetRemoteInvoker(invoker, func(org, ref string) ([]byte, error) {
		resolvedOrg, resolvedRef = org, ref
		return []byte("signing-secret-for-" + org), nil
	})

	hook := loadedHook{
		ID: "hook_r", Point: HookPointBeforeTool, Category: HookCategoryPolicy, Executor: HookExecutorRemote,
		URL: "https://hooks.example/before-tool", SecretRef: "sref_hook",
	}
	ev := HookEvent{Org: "org_1", Project: "prj_1", RunID: "run_9", Point: HookPointBeforeTool,
		Payload: map[string]any{"tool_name": "push", "arguments": map[string]any{"branch": "main"}}}

	out, err := s.fireLoaded(context.Background(), ev, []loadedHook{hook})
	if err != nil {
		t.Fatalf("fireLoaded() infra error = %v", err)
	}
	if !out.Denied || out.Reason != "the push tool is not permitted" {
		t.Fatalf("remote policy deny not interpreted: %+v", out)
	}

	// The envelope carries the hook's non-secret wiring + identity (tool-http.v1 reuse).
	if invoker.got.URL != "https://hooks.example/before-tool" {
		t.Fatalf("Invocation URL = %q, want the hook url", invoker.got.URL)
	}
	if invoker.got.ToolRevision != "hook@hook_r" {
		t.Fatalf("Invocation ToolRevision = %q, want hook@hook_r", invoker.got.ToolRevision)
	}
	if invoker.got.Org != "org_1" || invoker.got.Project != "prj_1" || invoker.got.RunID != "run_9" {
		t.Fatalf("Invocation scope mismatch: %+v", invoker.got)
	}
	if invoker.got.SecretRef != "sref_hook" {
		t.Fatalf("Invocation SecretRef = %q, want sref_hook", invoker.got.SecretRef)
	}
	// The secret was resolved FRESH from the org-scoped resolver — not held on the binding.
	if resolvedOrg != "org_1" || resolvedRef != "sref_hook" {
		t.Fatalf("secret resolved for (%q,%q), want (org_1, sref_hook)", resolvedOrg, resolvedRef)
	}
	if string(invoker.got.Secret) != "signing-secret-for-org_1" {
		t.Fatalf("Invocation Secret = %q, want the freshly resolved bytes", invoker.got.Secret)
	}
	// The hook payload rides as the envelope arguments (the point's observable data).
	if name, _ := invoker.got.Arguments["tool_name"].(string); name != "push" {
		t.Fatalf("Invocation Arguments lost the payload: %+v", invoker.got.Arguments)
	}
}

// TestRemoteTransformHookStrictDecodesPatch proves a remote TRANSFORM hook's inline body is strict-decoded as
// a HookPatch — a capability-granting response (a smuggled tool grant) fails the hook CLOSED, never applies.
func TestRemoteTransformHookStrictDecodesPatch(t *testing.T) {
	s := New(nil)
	// A well-formed arguments patch is applied.
	good := &captureInvoker{resp: map[string]any{"arguments": map[string]any{"path": "/redacted"}}}
	s.SetRemoteInvoker(good, func(org, ref string) ([]byte, error) { return []byte("sec"), nil })
	hook := loadedHook{ID: "hook_tr", Point: HookPointBeforeTool, Category: HookCategoryTransform, Executor: HookExecutorRemote, URL: "https://hooks.example/x", SecretRef: "sref"}
	ev := HookEvent{Org: "o", Project: "p", RunID: "r", Point: HookPointBeforeTool, Payload: map[string]any{"tool_name": "file", "arguments": map[string]any{"path": "/etc/secret"}}}
	out, err := s.fireLoaded(context.Background(), ev, []loadedHook{hook})
	if err != nil || out.Denied {
		t.Fatalf("good remote transform = (%+v, %v), want applied", out, err)
	}
	if got, _ := out.Payload["arguments"].(map[string]any); got["path"] != "/redacted" {
		t.Fatalf("remote transform arguments not applied: %v", out.Payload["arguments"])
	}

	// A capability-smuggling response is rejected fail-closed.
	evil := &captureInvoker{resp: map[string]any{"tools": []any{"push"}}}
	s.SetRemoteInvoker(evil, func(org, ref string) ([]byte, error) { return []byte("sec"), nil })
	denied, err := s.fireLoaded(context.Background(), ev, []loadedHook{hook})
	if err != nil || !denied.Denied {
		t.Fatalf("capability-smuggling remote transform = (%+v, %v), want fail-closed deny", denied, err)
	}
}
