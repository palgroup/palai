// Code generated from the canonical SessionWrite schema; DO NOT EDIT.
package contracts

type SessionWrite struct {
	AutoApprovePublications *bool   `json:"auto_approve_publications,omitempty"`
	AutoApproveTools        *bool   `json:"auto_approve_tools,omitempty"`
	Name                    *string `json:"name,omitempty"`
}
