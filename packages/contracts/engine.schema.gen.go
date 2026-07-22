// Code generated from the canonical EngineFrame schema; DO NOT EDIT.
package contracts

type EngineFrame struct {
	AttemptID AttemptID      `json:"attempt_id,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	ID        FrameID        `json:"id"`
	Protocol  string         `json:"protocol"`
	ReplyTo   *string        `json:"reply_to,omitempty"`
	RunID     RunID          `json:"run_id,omitempty"`
	Sequence  int            `json:"sequence"`
	Time      string         `json:"time"`
	Type      string         `json:"type"`
}

type ToolCall struct {
	Arguments map[string]any `json:"arguments"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name"`
}
