package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// ListResponseToolCalls renders a response's tool calls from the `tool_calls` ledger (E30 T2).
//
// AN EMPTY LIST IS NOT A 404. A response that ran no tools has no tool calls, and a response that does
// not exist has no tool calls, and those are different facts — so the response's own existence is
// checked first and only a genuinely absent (or foreign) id is NotFound. Answering 404 for "this run
// used no tools" would make a perfectly ordinary chat turn look like a broken id.
func (s *Store) ListResponseToolCalls(ctx context.Context, scope middleware.Scope, responseID string) (api.ToolCallsResult, error) {
	tenant, err := s.tenantOf(ctx, scope)
	if err != nil {
		return api.ToolCallsResult{}, err
	}
	view, err := s.spine.GetResponse(ctx, tenant, responseID)
	if err != nil {
		return api.ToolCallsResult{}, err
	}
	if !view.Found {
		return api.ToolCallsResult{NotFound: true}, nil
	}

	views, err := s.spine.ToolCallsForResponse(ctx, tenant, responseID)
	if err != nil {
		return api.ToolCallsResult{}, err
	}

	data := make([]map[string]any, 0, len(views))
	for _, v := range views {
		row := map[string]any{
			"id":           v.ID,
			"object":       "tool_call",
			"name":         v.Name,
			"state":        v.State,
			"replay_class": v.ReplayClass,
			"created_at":   v.CreatedAt.UTC().Format(time.RFC3339Nano),
			"updated_at":   v.UpdatedAt.UTC().Format(time.RFC3339Nano),
			"arguments":    rawOrNull(v.Arguments),
		}
		// RESULT IS OMITTED RATHER THAN NULLED WHEN THERE IS NONE, and the difference is the whole point
		// of the field. A call still parked on an approval, still executing, or belonging to an
		// UNRETAINED tool has no result to show — and a renderer that saw `"result": null` would draw an
		// empty result, which reads as "the tool returned nothing" rather than "nothing is known yet".
		if len(v.Result) > 0 {
			row["result"] = json.RawMessage(v.Result)
		}
		data = append(data, row)
	}

	body, err := json.Marshal(map[string]any{"object": "list", "data": data})
	if err != nil {
		return api.ToolCallsResult{}, err
	}
	return api.ToolCallsResult{Body: body}, nil
}

// rawOrNull passes ledger JSON through untouched, defaulting an empty column to an empty object. The
// bytes are NOT re-encoded: a renderer that wants to read a shell command's exact argument string must
// see what the ledger holds, not a round trip through a Go map that reorders keys.
func rawOrNull(raw []byte) any {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(raw)
}
