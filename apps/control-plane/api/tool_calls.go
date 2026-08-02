package api

import (
	"context"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// THE TOOL-CALL READ SURFACE (E30 T2, spec §26.7).
//
// WHY IT EXISTS, measured rather than argued. A client watching a run could see a tool call start and
// finish and could not say WHAT ran: `tool_call.executing.v1` carried {run_id, tool_call_id,
// replay_class} and `tool_call.completed.v1` carried {run_id, tool_call_id}. The name, the arguments and
// the result were on the `tool_calls` ledger with no route to them at all — the router had no
// tool-calls path of any kind. A chat that wants to render an `xcodebuild` failure with its file and
// line therefore had nothing to render from, and the demo's adapter said so in the only honest way
// available to it, by drawing the call with `nameUnavailable: true`.
//
// WHY IT IS A READ RATHER THAN A WIDER EVENT. The name now rides the frame, because it is short and
// comes from the closed set of tools the deployment registered. The arguments and result do not, and
// the reason is what an event payload becomes downstream: automation/webhook_pump.go:328 puts the whole
// payload in the body it POSTs to every registered endpoint and stores that envelope immutably so a
// redelivery replays it byte-for-byte, and the audit chain hashes it. Nothing in the coordinator bounds
// an event payload, and a trivial `xcodebuild` build measures 51,422 bytes on the machine this was
// written on. Widening the frame would ship model-authored megabytes off-box, once per endpoint,
// permanently — to buy a field a caller can simply ask for.
//
// AND IT IS NOT A SECOND COPY OF THE TRUTH, which is the objection that matters most in this tree. It
// projects `tool_calls` directly: no new table, no cached projection, no denormalised column, nothing
// to keep in step. There is one copy of a tool call and this reads it. The cost that is real, and is
// the whole cost: one round trip per detail render.

// ToolCallAPI is the store seam for the tool-call read. The Postgres store implements it; tiers that
// never run tools leave it nil and the route stays unmounted, the WithModelRoutes discipline.
type ToolCallAPI interface {
	// ListResponseToolCalls returns a response's tool calls in commit order, scoped to the verified
	// identity. A response belonging to another tenant is answered exactly like one that never existed.
	ListResponseToolCalls(ctx context.Context, scope middleware.Scope, responseID string) (ToolCallsResult, error)
}

// ToolCallsResult is the rendered list. NotFound is a response id that is unknown OR foreign — the same
// answer for both, so the 404 discloses no cross-tenant existence (§39.2).
type ToolCallsResult struct {
	Body     []byte
	NotFound bool
}

type toolCallHandler struct {
	toolCalls ToolCallAPI
}

// listForResponse serves GET /v1/responses/{response_id}/tool-calls.
//
// It is a GET and it writes nothing — the distinction the sibling verify/models pair draws is about the
// STAMP and the egress, and this has neither: it reads rows this deployment already wrote.
func (h *toolCallHandler) listForResponse(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.toolCalls.ListResponseToolCalls(r.Context(), scope, r.PathValue("response_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if out.NotFound {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such response in this project")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Body)
}
