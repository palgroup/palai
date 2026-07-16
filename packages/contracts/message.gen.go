// Code generated from the canonical Message schema; DO NOT EDIT.
package contracts

type Message struct {
	Content    []ContentItem `json:"content"`
	CreatedAt  string        `json:"created_at"`
	Delivery   string        `json:"delivery,omitempty"`
	ID         MessageID     `json:"id"`
	Role       string        `json:"role"`
	SourceRef  OpaqueID      `json:"source_ref,omitempty"`
	Visibility string        `json:"visibility,omitempty"`
}
