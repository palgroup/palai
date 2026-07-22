package extensions

import (
	"context"
	"fmt"
	"time"

	"github.com/palgroup/palai/packages/contracts"
)

// Hook dispatch (spec §28.17, E12 Task 8, TOL-012). Fire runs a point's ENABLED hooks in registration order
// and returns a verdict. The fail-mode is category-driven and NON-invertible:
//   - policy    → sync fail-CLOSED: a deny (or any invoke error/timeout) blocks the guarded operation VISIBLY.
//   - transform → a schema-validated patch to before_tool.arguments / after_tool.result, also fail-CLOSED: an
//     invoke error/timeout or an out-of-schema patch blocks the operation (a hook can never grant capability).
//   - observer  → async fail-OPEN: it runs on its own goroutine with a recover, and its outcome (or crash)
//     can NEVER affect the operation — there is no result channel back into Fire.
//
// Fire holds NO shared lock across a hook's network/handler wait (the T4 MF3 lesson): each hook runs under its
// own bounded context, and the only shared state a remote hook touches is the per-hook breaker's short mutex.

// hook ceilings per category (overridable in tests for speed). policy/transform are small; observer generous.
// A hook's declared timeout_ms is CLAMPED to its category ceiling — a hook can shrink its wait but never
// exceed the ceiling, so it can never pin a dispatch slot longer than the platform allows.
var (
	policyHookCeiling    = 3 * time.Second
	transformHookCeiling = 3 * time.Second
	observerHookCeiling  = 15 * time.Second
)

// HookEvent is what fires at a point (the dispatch seam input). Payload carries the point's observable /
// mutable data: before_tool {tool_name, arguments}; after_tool {tool_name, result}; before_model {tool_count};
// on_terminal {outcome}; before_repository_publish {operation, branch, ...}. The org/project scope selects the
// project's hooks; run/session/response identify the journal a deny is recorded on.
type HookEvent struct {
	Org        string
	Project    string
	SessionID  string
	ResponseID string
	RunID      string
	Point      string
	Payload    map[string]any
}

// HookOutcome is Fire's verdict. Denied marks a fail-closed policy/transform outcome (the caller maps it to
// the point's visible effect — a deny tool.result, a failed model step, a rejected publication). HookID +
// Reason identify the deny for the audit event. Payload is the (possibly transform-patched) point data the
// caller proceeds with when NOT denied.
type HookOutcome struct {
	Denied  bool
	Reason  string
	HookID  string
	Payload map[string]any
}

// HookHandler is a platform-authored in-process hook: deterministic and network-less (discipline, not the
// type system — it receives ONLY the event). A policy handler sets Deny; a transform handler sets Patch; an
// observer handler returns the zero decision.
type HookHandler func(ctx context.Context, ev HookEvent) (HookDecision, error)

// HookDecision is a single handler's (or remote invoke's) verdict.
type HookDecision struct {
	Deny   bool
	Reason string
	Patch  *contracts.HookPatch
}

// SetHookHandlers injects the platform_inline handler table (E12 T8). Nil handlers leave every inline hook
// fail-closed (a deny), never a nil-call.
func (s *Store) SetHookHandlers(handlers map[string]HookHandler) { s.hookHandlers = handlers }

// Fire loads a point's enabled hooks (registration order) and dispatches them. A tenant with no hooks at the
// point is a no-op returning the input payload unchanged, so a run that configures no hooks is bit-unchanged.
func (s *Store) Fire(ctx context.Context, ev HookEvent) (HookOutcome, error) {
	hooks, err := s.loadHooks(ctx, ev.Org, ev.Project, ev.Point)
	if err != nil {
		return HookOutcome{}, err
	}
	return s.fireLoaded(ctx, ev, hooks)
}

// fireLoaded is the DB-free dispatch core (unit-testable with a fabricated hook list). It walks the ordered
// hooks, threading a transform's patched payload into the next hook, and returns on the first fail-closed
// policy/transform outcome.
func (s *Store) fireLoaded(ctx context.Context, ev HookEvent, hooks []loadedHook) (HookOutcome, error) {
	out := HookOutcome{Payload: ev.Payload}
	for _, h := range hooks {
		stepEvent := ev
		stepEvent.Payload = out.Payload
		switch h.Category {
		case HookCategoryObserver:
			// Async fail-OPEN: fire-and-forget on its own goroutine with a recover. No result channel — an
			// observer can never affect the operation, and its panic is contained (the run is unaffected).
			s.fireObserver(h, stepEvent)
		case HookCategoryPolicy:
			dec, err := s.invokeHook(ctx, h, stepEvent)
			if err != nil {
				// Fail-CLOSED: an invoke error/timeout is a DENY (a policy hook that cannot answer must not
				// let the operation through). This is the non-invertible half — never a fail-open fallthrough.
				return HookOutcome{Denied: true, Reason: fmt.Sprintf("policy hook %s failed closed: %v", h.ID, err), HookID: h.ID, Payload: out.Payload}, nil
			}
			if dec.Deny {
				return HookOutcome{Denied: true, Reason: denyReason(dec.Reason), HookID: h.ID, Payload: out.Payload}, nil
			}
		case HookCategoryTransform:
			dec, err := s.invokeHook(ctx, h, stepEvent)
			if err != nil {
				return HookOutcome{Denied: true, Reason: fmt.Sprintf("transform hook %s failed closed: %v", h.ID, err), HookID: h.ID, Payload: out.Payload}, nil
			}
			if dec.Patch != nil {
				patched, perr := applyTransformPatch(ev.Point, out.Payload, dec.Patch)
				if perr != nil {
					// An out-of-surface patch (e.g. a before_tool hook trying to touch result) is fail-closed —
					// a hook can never step outside its category's patch surface.
					return HookOutcome{Denied: true, Reason: fmt.Sprintf("transform hook %s rejected: %v", h.ID, perr), HookID: h.ID, Payload: out.Payload}, nil
				}
				out.Payload = patched
			}
		}
	}
	return out, nil
}

// invokeHook runs one policy/transform hook under a category-clamped bounded context, in a goroutine so a
// hanging inline handler is ABANDONED on timeout (fail-closed) rather than wedging the dispatch. A panic is
// recovered into an error (fail-closed for policy/transform). ponytail: a handler that ignores its ctx and
// hangs forever leaks its goroutine — a named ceiling; a platform handler must not hang, and the remote path
// carries the transport's own timeout.
func (s *Store) invokeHook(ctx context.Context, h loadedHook, ev HookEvent) (HookDecision, error) {
	tctx, cancel := context.WithTimeout(ctx, hookTimeout(h.Category, h.TimeoutMS))
	defer cancel()
	type result struct {
		dec HookDecision
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: fmt.Errorf("hook panicked: %v", r)}
			}
		}()
		dec, err := s.runHook(tctx, h, ev)
		done <- result{dec: dec, err: err}
	}()
	select {
	case <-tctx.Done():
		return HookDecision{}, fmt.Errorf("timed out after %s: %w", hookTimeout(h.Category, h.TimeoutMS), tctx.Err())
	case r := <-done:
		return r.dec, r.err
	}
}

// fireObserver runs an observer hook async, fail-OPEN. A crash/timeout is swallowed (recover + a bounded
// context that just cancels the goroutine) — the operation is NEVER blocked or affected by an observer.
func (s *Store) fireObserver(h loadedHook, ev HookEvent) {
	go func() {
		defer func() { _ = recover() }() // fail-OPEN: an observer panic never propagates
		octx, cancel := context.WithTimeout(context.Background(), hookTimeout(h.Category, h.TimeoutMS))
		defer cancel()
		_, _ = s.runHook(octx, h, ev) // result intentionally discarded — an observer cannot affect the run
	}()
}

// runHook dispatches to the executor: a platform_inline handler (code-defined, deterministic) or the T4
// signed remote transport (implemented in the remote step). An inline hook naming no registered handler is
// fail-closed (an error), never a nil-call.
func (s *Store) runHook(ctx context.Context, h loadedHook, ev HookEvent) (HookDecision, error) {
	switch h.Executor {
	case HookExecutorInline:
		fn := s.hookHandlers[h.Handler]
		if fn == nil {
			return HookDecision{}, fmt.Errorf("no platform hook handler %q registered", h.Handler)
		}
		return fn(ctx, ev)
	case HookExecutorRemote:
		return s.runRemoteHook(ctx, h, ev)
	default:
		return HookDecision{}, fmt.Errorf("unknown hook executor %q", h.Executor)
	}
}

// runRemoteHook invokes a remote_http hook over the T4 signed transport (implemented in the remote step,
// hook_remote.go). Until the transport is wired a remote hook is fail-closed — a remote policy/transform hook
// whose transport is unavailable must NEVER be treated as an allow.
func (s *Store) runRemoteHook(ctx context.Context, h loadedHook, ev HookEvent) (HookDecision, error) {
	return HookDecision{}, fmt.Errorf("remote hook transport not wired")
}

// applyTransformPatch applies a transform hook's patch to the ONLY surface its point allows: before_tool may
// replace arguments, after_tool may replace result. A patch touching any other surface is an error (fail-
// closed). The patch schema (contracts.HookPatch) carries NO capability field, so a hook can never grant one.
func applyTransformPatch(point string, payload map[string]any, patch *contracts.HookPatch) (map[string]any, error) {
	out := clonePayload(payload)
	switch point {
	case HookPointBeforeTool:
		if patch.Result != nil {
			return nil, fmt.Errorf("a before_tool transform may only patch arguments, not result")
		}
		if patch.Arguments != nil {
			out["arguments"] = patch.Arguments
		}
	case HookPointAfterTool:
		if patch.Arguments != nil {
			return nil, fmt.Errorf("an after_tool transform may only patch result, not arguments")
		}
		if patch.Result != nil {
			out["result"] = patch.Result
		}
	default:
		return nil, fmt.Errorf("point %q is not transformable", point)
	}
	return out, nil
}

// clonePayload shallow-copies a payload map so a transform's replacement never mutates the caller's map.
func clonePayload(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// hookTimeout clamps a hook's declared timeout_ms to its category ceiling: a hook can shrink its wait but
// never exceed the ceiling (so it can never pin a dispatch slot beyond the platform's bound).
func hookTimeout(category string, timeoutMS *int) time.Duration {
	ceiling := policyHookCeiling
	switch category {
	case HookCategoryObserver:
		ceiling = observerHookCeiling
	case HookCategoryTransform:
		ceiling = transformHookCeiling
	}
	if timeoutMS != nil && *timeoutMS > 0 {
		if want := time.Duration(*timeoutMS) * time.Millisecond; want < ceiling {
			return want
		}
	}
	return ceiling
}

// denyReason returns a non-empty deny reason (a hook that denies without a reason still surfaces a clear one).
func denyReason(reason string) string {
	if reason == "" {
		return "denied by a policy hook"
	}
	return reason
}
