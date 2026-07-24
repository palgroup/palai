// Code generated from the canonical Usage schema; DO NOT EDIT.
package contracts

type Usage struct {
	CacheReadTokens  int            `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int            `json:"cache_write_tokens,omitempty"`
	Cost             map[string]any `json:"cost,omitempty"`
	InputTokens      int            `json:"input_tokens"`
	OutputTokens     int            `json:"output_tokens"`
	ToolCalls        int            `json:"tool_calls,omitempty"`
	TotalTokens      int            `json:"total_tokens,omitempty"`
}
