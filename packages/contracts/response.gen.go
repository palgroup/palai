// Code generated from the canonical Response schema; DO NOT EDIT.
package contracts

type Response struct {
	CreatedAt      string            `json:"created_at"`
	Error          any               `json:"error,omitempty"`
	ID             ResponseID        `json:"id"`
	Labels         map[string]string `json:"labels,omitempty"`
	Metadata       map[string]any    `json:"metadata,omitempty"`
	Model          string            `json:"model"`
	Object         string            `json:"object"`
	OrganizationID OrganizationID    `json:"organization_id,omitempty"`
	Output         []ContentItem     `json:"output"`
	ProjectID      ProjectID         `json:"project_id,omitempty"`
	Revision       int               `json:"revision,omitempty"`
	RunID          RunID             `json:"run_id,omitempty"`
	SessionID      SessionID         `json:"session_id,omitempty"`
	Status         string            `json:"status"`
	UpdatedAt      string            `json:"updated_at,omitempty"`
	Usage          Usage             `json:"usage"`
}
