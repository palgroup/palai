package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// The remote_http hook binding (spec §28.17, E12 T8). A tenant hook runs OFF the API process — it reuses the
// SAME T4 signed transport the remote-tool executor uses (the Store's remoteInvoker + remoteSecret, the
// lookup.go remoteExec idiom): the signing secret is resolved FRESH per invoke from the org-scoped bridge and
// never held in a closure, the egress SSRF vet runs inside Invoke for free, and the tool-http.v1 envelope is
// signed by the shared webhook signer. Policy/transform accept ONLY a 200-inline result; a transport error or
// a non-inline (202/timeout/rejection) answer is fail-CLOSED. An observer's error is tolerated (fail-open) —
// fireObserver discards runHook's return entirely.

// runRemoteHook invokes a remote_http hook over the T4 signed transport and interprets the inline result by
// category. A remote hook that cannot be signed/reached is fail-closed (an error), never a silent allow.
//
// ponytail: a hook fire reuses the tool Executor, which opens a durable remote_tool_operations row per invoke
// (keyed on a synthetic hook-fire id) and, on a 202, awaits a callback that never comes and times out at the
// hook ceiling → fail-closed. A dedicated sync-only hook transport (no operation row, instant 202 reject) is
// the upgrade path if hook-fire row volume ever matters; the reuse keeps ONE signed transport + egress layer.
func (s *Store) runRemoteHook(ctx context.Context, h loadedHook, ev HookEvent) (HookDecision, error) {
	if s.remoteInvoker == nil || s.remoteSecret == nil {
		return HookDecision{}, fmt.Errorf("remote hook %s: transport not wired", h.ID)
	}
	if h.SecretRef == "" {
		return HookDecision{}, fmt.Errorf("remote hook %s: no secret_ref (a signed transport needs a secret)", h.ID)
	}
	// Resolve the signing secret FRESH per invoke (org-scoped), never captured in a closure (the remoteExec
	// idiom): a hook binding holds only non-secret wiring (url, secret_ref handle).
	secret, err := s.remoteSecret(ev.Org, h.SecretRef)
	if err != nil {
		return HookDecision{}, fmt.Errorf("resolve remote hook secret for %s: %w", h.ID, err)
	}
	// A hook fire is not a durable tool call: the synthetic id only keys the signed envelope + the transport
	// idempotency, never a durable hook operation.
	invokeID := newID("hookfire")
	resp, err := s.remoteInvoker.Invoke(ctx, remotehttp.Invocation{
		URL:          h.URL,
		AllowPrivate: h.AllowPrivate,
		Secret:       secret,
		ToolCallID:   invokeID,
		ToolRevision: "hook@" + h.ID,
		RunID:        ev.RunID,
		// A hook fire has no attempt row; the run id is a stable attempt-less identity for the envelope.
		AttemptID:   ev.RunID,
		RequestHash: toolbroker.RequestHash(ev.Point, ev.Payload),
		Arguments:   ev.Payload,
		Org:         ev.Org,
		Project:     ev.Project,
		SecretRef:   h.SecretRef,
		TimeoutMS:   int(hookTimeout(h.Category, h.TimeoutMS) / time.Millisecond),
	})
	if err != nil {
		// Fail-CLOSED for policy/transform; the observer path discards this (fail-open).
		return HookDecision{}, err
	}
	return interpretRemoteHookResponse(h.Category, resp)
}

// interpretRemoteHookResponse maps a remote hook's inline result to a decision by category. A policy hook must
// answer decision:"allow"|"deny" EXPLICITLY — an ambiguous/missing decision is fail-closed. A transform hook's
// body IS the patch, strict-decoded (so a capability field is rejected). An observer's body is ignored.
func interpretRemoteHookResponse(category string, resp map[string]any) (HookDecision, error) {
	switch category {
	case HookCategoryPolicy:
		switch decision, _ := resp["decision"].(string); decision {
		case "allow":
			return HookDecision{}, nil
		case "deny":
			reason, _ := resp["reason"].(string)
			return HookDecision{Deny: true, Reason: reason}, nil
		default:
			return HookDecision{}, fmt.Errorf("remote policy hook returned no explicit allow/deny decision")
		}
	case HookCategoryTransform:
		// The response body IS the patch; strict-decode it so an out-of-schema (capability) field fails closed.
		raw, _ := json.Marshal(resp)
		patch, err := decodeHookPatch(raw)
		if err != nil {
			return HookDecision{}, err
		}
		return HookDecision{Patch: patch}, nil
	default:
		// Observer: the caller discards this, but return cleanly.
		return HookDecision{}, nil
	}
}
