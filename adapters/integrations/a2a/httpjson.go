package a2a

import (
	"encoding/json"
	"net/http"
)

// writeJSON renders a value as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr renders an A2A error. The HTTP+JSON binding uses HTTP status codes plus a small JSON error object;
// the code is a stable machine token, the message a human string. No config oracle: unknown/foreign ids all
// render the same generic not_found (the callers pass "no such ..." details that reveal nothing distinguishing).
func writeErr(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": detail},
	})
}

// writeSSE writes one Server-Sent Event frame carrying a JSON-encoded A2A stream event.
func writeSSE(w http.ResponseWriter, v any) {
	blob, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(blob)
	_, _ = w.Write([]byte("\n\n"))
}
