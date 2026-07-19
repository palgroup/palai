// Code generated from the canonical Command schema; DO NOT EDIT.
package contracts

type Command struct {
	AppliedSequence *int           `json:"applied_sequence,omitempty"`
	CreatedAt       string         `json:"created_at"`
	Delivery        string         `json:"delivery,omitempty"`
	ID              CommandID      `json:"id"`
	Kind            string         `json:"kind"`
	Object          string         `json:"object"`
	Result          map[string]any `json:"result,omitempty"`
	SessionID       SessionID      `json:"session_id"`
	Status          string         `json:"status"`
}
