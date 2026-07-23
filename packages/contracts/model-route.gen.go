// Code generated from the canonical ModelRoute schema; DO NOT EDIT.
package contracts

type ModelRoute struct {
	CreatedAt      string         `json:"created_at,omitempty"`
	ID             OpaqueID       `json:"id"`
	Name           string         `json:"name"`
	Object         string         `json:"object"`
	OrganizationID OrganizationID `json:"organization_id,omitempty"`
	ProjectID      ProjectID      `json:"project_id,omitempty"`
}
