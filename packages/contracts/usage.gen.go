// Code generated from the canonical Usage schema; DO NOT EDIT.
package contracts

type Usage struct {
	Cost         map[string]any `json:"cost,omitempty"`
	InputTokens  int            `json:"input_tokens"`
	OutputTokens int            `json:"output_tokens"`
	ToolCalls    int            `json:"tool_calls,omitempty"`
	TotalTokens  int            `json:"total_tokens,omitempty"`
}
