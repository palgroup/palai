// Code generated from the canonical ResourceEnvelope schema; DO NOT EDIT.
package contracts

type ResourceEnvelope struct {
	CreatedAt      string            `json:"created_at"`
	ID             OpaqueID          `json:"id"`
	Labels         map[string]string `json:"labels,omitempty"`
	Metadata       map[string]any    `json:"metadata,omitempty"`
	Object         string            `json:"object"`
	OrganizationID OrganizationID    `json:"organization_id,omitempty"`
	ProjectID      ProjectID         `json:"project_id,omitempty"`
	Revision       int               `json:"revision,omitempty"`
	UpdatedAt      string            `json:"updated_at,omitempty"`
}
