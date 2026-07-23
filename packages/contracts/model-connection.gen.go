// Code generated from the canonical ModelConnection schema; DO NOT EDIT.
package contracts

type ModelConnection struct {
	CreatedAt      string         `json:"created_at,omitempty"`
	ID             OpaqueID       `json:"id"`
	Object         string         `json:"object"`
	OrganizationID OrganizationID `json:"organization_id,omitempty"`
	ProjectID      ProjectID      `json:"project_id,omitempty"`
	Provider       string         `json:"provider"`
	SecretRef      string         `json:"secret_ref"`
}
