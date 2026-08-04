// Code generated from the canonical ModelRoute schema; DO NOT EDIT.
package contracts

type ModelRoute struct {
	ConsultedByDispatch bool      `json:"consulted_by_dispatch,omitempty"`
	CreatedAt           string    `json:"created_at,omitempty"`
	DispatchNote        string    `json:"dispatch_note,omitempty"`
	ID                  OpaqueID  `json:"id"`
	Name                string    `json:"name"`
	Object              string    `json:"object"`
	ProjectID           ProjectID `json:"project_id,omitempty"`
}
