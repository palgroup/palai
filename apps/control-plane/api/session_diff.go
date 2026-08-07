package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/coordinator"
)

// SessionWorkspaces resolves the directory a session's work lives in. It is an interface so the route
// can be mounted against the real spine and driven in a test without one.
type SessionWorkspaces interface {
	WorkspaceHostPath(ctx context.Context, project, sessionID string) (string, error)
}

// sessionDiffTimeout bounds the `git diff`. It is generous because a first diff on a large checkout
// walks the whole tree, and short enough that a wedged repository answers rather than hanging a request.
const sessionDiffTimeout = 30 * time.Second

// sessionDiff answers `GET /v1/sessions/{session_id}/diff` with the working-tree patch, as text.
//
// THIS IS THE SURFACE THAT DID NOT EXIST, and its absence broke the flow rather than inconveniencing it:
// an agent writes code on a Mac, waits for approval, and the human who has to approve had no way to see
// what changed. Measured 2026-08-07 — zero routes matching diff|patch|changes under /v1.
//
// IT RUNS `git diff` AND NOTHING ELSE. Not `git status`, not a file listing, not a shell: one command
// with a fixed argv against a directory the caller's own tenant scope resolved. There is no place for a
// caller-supplied string to reach the process, which is what keeps a read surface from becoming an
// execution surface.
//
//   - `--no-color` because the reader is a machine and an SDK should not have to strip ANSI.
//   - `HEAD` is NOT named. The unstaged working tree against the index plus the index against HEAD is
//     what "what has the agent changed" means while it is still working; naming a commit would hide
//     everything not yet staged, which on an agent's tree is most of it.
//   - untracked files are NOT in it, and that is git's behaviour rather than a choice made here. A new
//     file the agent created shows once it is added. Saying so is cheaper than a reader concluding the
//     agent wrote nothing.
func (h *sessionDiffHandler) sessionDiff(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	if sessionID == "" {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such session in this project")
		return
	}

	dir, err := h.workspaces.WorkspaceHostPath(r.Context(), scope.Project, sessionID)
	switch {
	case errors.Is(err, coordinator.ErrNoWorkspace):
		// 404 AND NOT AN EMPTY DIFF. "This session has no workspace" and "this session has changed
		// nothing" are different facts, and a caller polling for the agent's work has to be able to tell
		// them apart — an empty 200 would read as "it finished and changed nothing".
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found",
			"this session has no workspace yet: it has not been placed on a machine, so there is nothing to diff")
		return
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}

	patch, err := gitDiff(r.Context(), dir)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":     "session.diff",
		"session_id": sessionID,
		"patch":      patch,
	})
}

// sessionDiffHandler holds the one dependency this route has.
type sessionDiffHandler struct{ workspaces SessionWorkspaces }

// gitDiff runs the one command, in the one directory, with a fixed argv.
//
// A NON-ZERO EXIT IS NOT ALWAYS A FAILURE and this is the distinction that matters: `git diff` exits 0
// with an empty patch on a clean tree, and exits non-zero when the directory is not a repository at all.
// The second is worth reporting; the first is the common case and reporting it as an error would make a
// clean workspace look broken. So stderr decides, not the code — an empty stderr with any exit means the
// tree simply had nothing to say.
func gitDiff(ctx context.Context, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionDiffTimeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "diff", "--no-color")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil && strings.TrimSpace(stderr.String()) != "" {
		return "", errors.New("git diff in the session workspace: " + strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
