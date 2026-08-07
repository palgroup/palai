package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// RunnerLogReader reads what one machine has said about itself.
type RunnerLogReader interface {
	Page(ctx context.Context, runnerID, sessionID string, limit int) ([]RunnerLogEntry, error)
}

// RunnerLogEntry is one line as the admin plane renders it.
type RunnerLogEntry struct {
	ID         string
	RunnerID   string
	At         time.Time
	ReceivedAt time.Time
	Level      string
	SessionID  string
	Message    string
}

// listRunnerLogs answers `GET /v1/runners/{runner_id}/logs` — the channel that did not exist.
//
// AN OPERATOR ASKING "WHAT WENT WRONG ON THAT MAC" HAD TO SSH INTO IT. The runner plane moved leases,
// identity and config and never a sentence the agent wrote, which is answerable on one machine and
// impossible on a hundred. This is the read half of that channel.
//
// BOTH CLOCKS ARE RENDERED, and the gap between them is the signal. `at` is when the machine wrote the
// line; `received_at` is when the plane could first have known. An agent that was offline for an hour
// ships a backlog whose `at` is old and whose `received_at` is now — and an operator who sees only one
// of the two cannot tell a slow machine from a machine that was gone.
//
// `?session_id=` narrows to what the machine said WHILE one session ran; absent, the answer includes
// everything between sessions, which is where an infrastructure question lives.
func (h *runnerHandler) listRunnerLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authorizeAdmin(w, r); !ok {
		return
	}
	if h.runnerLogs == nil {
		middleware.WriteProblem(w, r, http.StatusNotImplemented, "not_implemented",
			"this deployment does not collect machine logs")
		return
	}
	runnerID := strings.TrimSpace(r.PathValue("runner_id"))
	if runnerID == "" {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such machine in this project")
		return
	}
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
				"limit must be a positive integer")
			return
		}
		limit = n
	}
	entries, err := h.runnerLogs.Page(r.Context(), runnerID, strings.TrimSpace(r.URL.Query().Get("session_id")), limit)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	views := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		view := map[string]any{
			"object": "runner_log", "id": e.ID, "runner_id": e.RunnerID,
			"at": e.At, "received_at": e.ReceivedAt, "message": e.Message,
		}
		if e.Level != "" {
			view["level"] = e.Level
		}
		if e.SessionID != "" {
			view["session_id"] = e.SessionID
		}
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": views})
}
