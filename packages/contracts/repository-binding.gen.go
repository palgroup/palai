// Code generated from the canonical RepositoryBinding schema; DO NOT EDIT.
package contracts

type RepositoryBinding struct {
	AllowedOperations  []string            `json:"allowed_operations,omitempty"`
	CloneUrl           string              `json:"clone_url"`
	ConnectionRef      string              `json:"connection_ref,omitempty"`
	CreatedAt          string              `json:"created_at,omitempty"`
	DataClassification string              `json:"data_classification,omitempty"`
	DefaultBranch      string              `json:"default_branch"`
	ID                 RepositoryBindingID `json:"id"`
	Object             string              `json:"object"`
	OrganizationID     OrganizationID      `json:"organization_id,omitempty"`
	Policy             map[string]any      `json:"policy,omitempty"`
	ProjectID          ProjectID           `json:"project_id,omitempty"`
	Provider           string              `json:"provider"`
	RegionConstraint   string              `json:"region_constraint,omitempty"`
	RepositoryIdentity string              `json:"repository_identity"`
}
