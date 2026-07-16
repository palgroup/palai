// Code generated from the canonical RunnerMessage schema; DO NOT EDIT.
package contracts

type RunnerMessage struct {
	AttemptID AttemptID      `json:"attempt_id,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	Fence     int            `json:"fence,omitempty"`
	LeaseID   string         `json:"lease_id,omitempty"`
	Protocol  string         `json:"protocol"`
	RunID     RunID          `json:"run_id,omitempty"`
	Time      string         `json:"time"`
	Type      string         `json:"type"`
}
