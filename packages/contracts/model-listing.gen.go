// Code generated from the canonical ModelListing schema; DO NOT EDIT.
package contracts

type ModelListing struct {
	Complete     bool             `json:"complete"`
	ConnectionID OpaqueID         `json:"connection_id"`
	Data         []map[string]any `json:"data,omitempty"`
	Detail       string           `json:"detail"`
	Endpoint     string           `json:"endpoint,omitempty"`
	FetchedAt    string           `json:"fetched_at"`
	Listed       string           `json:"listed"`
	Object       string           `json:"object"`
	Outcome      string           `json:"outcome"`
	Provider     string           `json:"provider"`
	Status       int              `json:"status,omitempty"`
}
