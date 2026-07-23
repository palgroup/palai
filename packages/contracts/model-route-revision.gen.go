// Code generated from the canonical ModelRouteRevision schema; DO NOT EDIT.
package contracts

type ModelRouteRevision struct {
	ConnectionID OpaqueID `json:"connection_id,omitempty"`
	CreatedAt    string   `json:"created_at,omitempty"`
	ID           OpaqueID `json:"id"`
	Model        string   `json:"model,omitempty"`
	Object       string   `json:"object"`
	Published    bool     `json:"published"`
	Revision     int      `json:"revision,omitempty"`
	RouteID      OpaqueID `json:"route_id"`
}
