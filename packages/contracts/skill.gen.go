// Code generated from the canonical Skill schema; DO NOT EDIT.
package contracts

type Skill struct {
	CreatedAt      string           `json:"created_at,omitempty"`
	Digest         string           `json:"digest,omitempty"`
	ID             OpaqueID         `json:"id"`
	Name           string           `json:"name,omitempty"`
	Object         string           `json:"object"`
	OrganizationID OrganizationID   `json:"organization_id,omitempty"`
	ProjectID      ProjectID        `json:"project_id,omitempty"`
	RevisionNumber int              `json:"revision_number,omitempty"`
	ScanFindings   []map[string]any `json:"scan_findings,omitempty"`
	SkillID        OpaqueID         `json:"skill_id,omitempty"`
	SourceUrl      string           `json:"source_url,omitempty"`
	State          string           `json:"state,omitempty"`
}
