// Code generated from the canonical PreparationReceipt schema; DO NOT EDIT.
package contracts

type PreparationReceipt struct {
	BaseCommit   string `json:"base_commit"`
	Branch       string `json:"branch,omitempty"`
	PreparedAt   string `json:"prepared_at,omitempty"`
	RequestedRef string `json:"requested_ref,omitempty"`
	TreeHash     string `json:"tree_hash"`
}
