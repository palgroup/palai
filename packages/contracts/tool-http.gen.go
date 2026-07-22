// Code generated from the canonical ToolHTTPInvoke schema; DO NOT EDIT.
package contracts

type ToolHTTPInvoke struct {
	Arguments    map[string]any `json:"arguments"`
	AttemptID    string         `json:"attempt_id"`
	Callback     map[string]any `json:"callback"`
	Deadline     string         `json:"deadline"`
	Protocol     string         `json:"protocol"`
	RequestHash  string         `json:"request_hash"`
	RunID        string         `json:"run_id"`
	ToolCallID   string         `json:"tool_call_id"`
	ToolRevision string         `json:"tool_revision"`
}
