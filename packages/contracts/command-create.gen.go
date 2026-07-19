// Code generated from the canonical CommandCreateRequest schema; DO NOT EDIT.
package contracts

type CommandCreateRequest struct {
	CommandID CommandID `json:"command_id"`
	Delivery  string    `json:"delivery,omitempty"`
	Kind      string    `json:"kind"`
	Message   string    `json:"message,omitempty"`
}
