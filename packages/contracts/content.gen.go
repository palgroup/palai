// Code generated from the canonical ContentItem schema; DO NOT EDIT.
package contracts

// ContentItem is an open union: unknown fields and unknown type
// values survive a JSON round-trip (ADR-0002, spec API-009).
type ContentItem map[string]any

// Type returns the type discriminator.
func (c ContentItem) Type() string {
	v, _ := c["type"].(string)
	return v
}
