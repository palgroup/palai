// Code generated from the canonical Event schema; DO NOT EDIT.
package contracts

type Event struct {
	AttemptID       AttemptID      `json:"attempt_id,omitempty"`
	Data            map[string]any `json:"data"`
	Datacontenttype string         `json:"datacontenttype,omitempty"`
	ID              EventID        `json:"id"`
	ProjectID       ProjectID      `json:"project_id,omitempty"`
	RunID           RunID          `json:"run_id,omitempty"`
	Sequence        int            `json:"sequence"`
	SessionID       SessionID      `json:"session_id,omitempty"`
	Source          string         `json:"source"`
	Specversion     string         `json:"specversion"`
	Subject         string         `json:"subject,omitempty"`
	Time            string         `json:"time"`
	Type            string         `json:"type"`
}
