// Code generated from the canonical Pagination schema; DO NOT EDIT.
package contracts

type Page struct {
	Data           []any   `json:"data"`
	HasMore        bool    `json:"has_more"`
	NextCursor     *string `json:"next_cursor,omitempty"`
	PreviousCursor *string `json:"previous_cursor,omitempty"`
}

type PageParams struct {
	After  string `json:"after,omitempty"`
	Before string `json:"before,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}
