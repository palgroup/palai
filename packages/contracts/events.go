package contracts

// This file is a handwritten helper on the generated Event type. It is stdlib-only
// and defines no new exported types (the check gate compares only *.gen.go).

import (
	"bytes"
	"encoding/json"
)

// MarshalSSE renders the event as one Server-Sent Events frame (asyncapi
// x-sse-binding): the CloudEvents envelope as single-line JSON in the data field,
// the per-session event id as the SSE id (a client echoes it back via
// Last-Event-ID to resume), and the event type as the SSE event name.
//
// json.Marshal never emits a raw newline (newlines inside string values are
// escaped), so the envelope always occupies exactly one data line.
func (e Event) MarshalSSE() ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("id: ")
	b.WriteString(string(e.ID))
	b.WriteString("\nevent: ")
	b.WriteString(e.Type)
	b.WriteString("\ndata: ")
	b.Write(data)
	b.WriteString("\n\n")
	return b.Bytes(), nil
}
