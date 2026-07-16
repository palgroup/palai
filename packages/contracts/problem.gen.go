// Code generated from the canonical Problem schema; DO NOT EDIT.
package contracts

type Problem struct {
	Code        string           `json:"code"`
	Context     map[string]any   `json:"context,omitempty"`
	Detail      string           `json:"detail,omitempty"`
	FieldErrors []map[string]any `json:"field_errors,omitempty"`
	Instance    string           `json:"instance,omitempty"`
	RequestID   RequestID        `json:"request_id"`
	Retryable   bool             `json:"retryable,omitempty"`
	Status      int              `json:"status"`
	Title       string           `json:"title"`
	Type        string           `json:"type"`
}
