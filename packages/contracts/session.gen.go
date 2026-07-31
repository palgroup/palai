// Code generated from the canonical Session schema; DO NOT EDIT.
package contracts

type Session struct {
	Agents          []string       `json:"agents"`
	CreatedAt       string         `json:"created_at"`
	DurationMs      *int           `json:"duration_ms,omitempty"`
	FirstActivityAt *string        `json:"first_activity_at,omitempty"`
	ID              SessionID      `json:"id"`
	InputTokens     int            `json:"input_tokens"`
	LastActivityAt  *string        `json:"last_activity_at,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	Name            string         `json:"name"`
	NameSource      string         `json:"name_source"`
	Object          string         `json:"object"`
	OrganizationID  OrganizationID `json:"organization_id,omitempty"`
	OutputTokens    int            `json:"output_tokens"`
	ProjectID       ProjectID      `json:"project_id,omitempty"`
	Status          string         `json:"status"`
}
