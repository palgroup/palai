// Code generated from the canonical Tool schema; DO NOT EDIT.
package contracts

type Tool struct {
	CanonicalName    string         `json:"canonical_name"`
	CreatedAt        string         `json:"created_at,omitempty"`
	ID               OpaqueID       `json:"id"`
	ModelVisibleName string         `json:"model_visible_name"`
	Object           string         `json:"object"`
	OrganizationID   OrganizationID `json:"organization_id,omitempty"`
	ProjectID        ProjectID      `json:"project_id,omitempty"`
}
