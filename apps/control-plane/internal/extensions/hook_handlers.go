package extensions

import "context"

// Platform-authored hook handlers (spec §28.17, E12 T8). A platform_inline hook is a code-defined,
// deterministic, network-less function keyed by config.handler. It receives ONLY the event (never network,
// never a secret) — the type discipline that keeps tenant code out of the API process. These are the
// building blocks an admin opts into by registering a hook row that names one; none fire unless a hook is
// registered against it.

// PlatformHookHandlers returns the built-in platform_inline handler table wired at composition. Today it
// carries the deny-all policy fixture that proves the fail-closed deny path end to end (the approved live
// hook-deny-visible case): a real model spontaneously calls a tool, a before_tool policy hook denies it, and
// the model sees the structured denial. Honest ceiling: deny-all is a blunt platform control — richer
// platform policies (deny-irreversible-without-approval, rate-limit) are follow-on handlers registered here.
func PlatformHookHandlers() map[string]HookHandler {
	return map[string]HookHandler{
		"deny_all": denyAll,
	}
}

// denyAll is a policy handler that denies every call at its point. It is the deterministic fixture the live
// deny-visible case + the component deny leg register: an admin who registers a before_tool policy hook with
// handler "deny_all" blocks every tool the model spontaneously calls, and the model sees the structured deny.
func denyAll(_ context.Context, ev HookEvent) (HookDecision, error) {
	name, _ := ev.Payload["tool_name"].(string)
	reason := "blocked by the project before_tool policy hook"
	if name != "" {
		reason = "the tool " + name + " is blocked by the project before_tool policy hook"
	}
	return HookDecision{Deny: true, Reason: reason}, nil
}
