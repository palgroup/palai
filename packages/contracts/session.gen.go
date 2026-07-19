// Code generated from the canonical Session schema; DO NOT EDIT.
package contracts

type Session struct {
	CreatedAt      string         `json:"created_at"`
	ID             SessionID      `json:"id"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	Object         string         `json:"object"`
	OrganizationID OrganizationID `json:"organization_id,omitempty"`
	ProjectID      ProjectID      `json:"project_id,omitempty"`
	Status         string         `json:"status"`
}
