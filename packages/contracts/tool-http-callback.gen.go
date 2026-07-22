// Code generated from the canonical ToolHTTPCallback schema; DO NOT EDIT.
package contracts

type ToolHTTPCallback struct {
	OperationID string         `json:"operation_id"`
	Problem     map[string]any `json:"problem,omitempty"`
	Protocol    string         `json:"protocol"`
	Result      map[string]any `json:"result,omitempty"`
	ToolCallID  string         `json:"tool_call_id"`
}
