// Code generated from the canonical ToolSet schema; DO NOT EDIT.
package contracts

type ToolSet struct {
	CreatedAt      string         `json:"created_at,omitempty"`
	Digest         string         `json:"digest,omitempty"`
	ID             OpaqueID       `json:"id"`
	Object         string         `json:"object"`
	OrganizationID OrganizationID `json:"organization_id,omitempty"`
	ProjectID      ProjectID      `json:"project_id,omitempty"`
	RevisionNumber int            `json:"revision_number"`
	Set            string         `json:"set"`
}
