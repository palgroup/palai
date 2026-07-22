// Code generated from the canonical MCPConnection schema; DO NOT EDIT.
package contracts

type MCPConnection struct {
	CreatedAt      string         `json:"created_at,omitempty"`
	Disabled       bool           `json:"disabled,omitempty"`
	ID             OpaqueID       `json:"id"`
	Name           string         `json:"name"`
	Object         string         `json:"object"`
	OrganizationID OrganizationID `json:"organization_id,omitempty"`
	ProjectID      ProjectID      `json:"project_id,omitempty"`
	Transport      string         `json:"transport"`
	TrustLevel     string         `json:"trust_level,omitempty"`
}
