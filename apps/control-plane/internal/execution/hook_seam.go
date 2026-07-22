package execution

import (
	"context"
	"encoding/json"

	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
)

// The hook dispatch seam (spec §28.17, E12 T8). Hooks fire INSIDE the single dispatch loop — there is NO new
// engine frame/command kind and NO second dispatch loop. Each of the five points calls fireHook; a fail-closed
// policy/transform DENY is journaled once as policy.denied.v1 (reusing the existing event kind — the payload
// carries the hook id + point) and mapped to the point's visible effect by its caller. A nil hook firer (no
// registry wired, every pre-T8 test) makes fireHook a pass-through, so the dispatch is bit-unchanged.

const eventPolicyDenied = "policy.denied.v1"

// fireHook runs the run's hooks at a point with the given payload and returns the verdict. A nil firer is a
// no-op that returns the payload unchanged (never denied). The scope + run/session/response identity are
// threaded from the attempt so a deny is journaled on this run.
func (o *Orchestrator) fireHook(ctx context.Context, st *attemptState, point string, payload map[string]any) (extensions.HookOutcome, error) {
	if o.hooks == nil {
		return extensions.HookOutcome{Payload: payload}, nil
	}
	return o.hooks.Fire(ctx, extensions.HookEvent{
		Org:        st.tenant.Organization,
		Project:    st.tenant.Project,
		SessionID:  st.sessionID,
		ResponseID: st.responseID,
		RunID:      string(st.attempt.RunID),
		Point:      point,
		Payload:    payload,
	})
}

// journalPolicyDenied records the fail-closed hook deny on the run's response journal (spec §28.17). It reuses
// the existing policy.denied.v1 event kind — the payload carries the deny's provenance (hook id, point, the
// tool_call/step it blocked, the reason). Guarded by the run being active, so a raced cancel returns
// ErrRunTerminal (abortIfTerminal maps it to a clean attempt end).
func (o *Orchestrator) journalPolicyDenied(ctx context.Context, st *attemptState, point, hookID, reason string, extra map[string]any) error {
	fields := map[string]any{
		"run_id":  st.attempt.RunID,
		"hook_id": hookID,
		"point":   point,
		"reason":  reason,
	}
	for k, v := range extra {
		fields[k] = v
	}
	payload, _ := json.Marshal(fields)
	return o.spine.JournalRunEvent(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), eventPolicyDenied, payload)
}
