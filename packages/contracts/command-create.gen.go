// Code generated from the canonical CommandCreateRequest schema; DO NOT EDIT.
package contracts

type CommandCreateRequest struct {
	CommandID CommandID `json:"command_id"`
	Delivery  string    `json:"delivery,omitempty"`
	Immediate bool      `json:"immediate,omitempty"`
	Kind      string    `json:"kind"`
	Message   string    `json:"message,omitempty"`
	Model     string    `json:"model,omitempty"`
	Tools     []string  `json:"tools,omitempty"`
}
