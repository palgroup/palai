// Code generated from the canonical Hook schema; DO NOT EDIT.
package contracts

type Hook struct {
	Category       string         `json:"category"`
	CreatedAt      string         `json:"created_at,omitempty"`
	Disabled       bool           `json:"disabled,omitempty"`
	Executor       string         `json:"executor"`
	HookPoint      string         `json:"hook_point"`
	ID             OpaqueID       `json:"id"`
	Name           string         `json:"name"`
	Object         string         `json:"object"`
	OrganizationID OrganizationID `json:"organization_id,omitempty"`
	ProjectID      ProjectID      `json:"project_id,omitempty"`
}
