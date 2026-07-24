package api

import (
	"context"
	"io"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// KnowledgeAPI is the store seam for the E17 Task 4 knowledge spine (17b): knowledge bases, their ingest
// sources, the immutable ingest -> index build, ranked FTS retrieval, and the append-only index-revision
// history. The Postgres-backed internal/knowledge Store implements it; production wires it via WithKnowledge
// so a tier that never serves knowledge leaves the routes unmounted. Every method is scoped by the verified
// identity (org+project), never a body field.
type KnowledgeAPI interface {
	CreateKnowledgeBase(ctx context.Context, scope middleware.Scope, body []byte) (ProvisionResult, error)
	ListKnowledgeBases(ctx context.Context, scope middleware.Scope) (ProvisionResult, error)
	CreateSource(ctx context.Context, scope middleware.Scope, kbID string, body []byte) (ProvisionResult, error)
	ListSources(ctx context.Context, scope middleware.Scope, kbID string) (ProvisionResult, error)
	DeleteSource(ctx context.Context, scope middleware.Scope, sourceID string) (ProvisionResult, error)
	Ingest(ctx context.Context, scope middleware.Scope, kbID, sourceID string, body []byte) (ProvisionResult, error)
	Retrieve(ctx context.Context, scope middleware.Scope, kbID string, body []byte) (ProvisionResult, error)
	ListIndexRevisions(ctx context.Context, scope middleware.Scope, kbID string) (ProvisionResult, error)
}

// knowledgeHandler renders the knowledge routes. It reuses ProvisionResult (the generic management-write
// projection) and the `provision` capability gate — knowledge management is an org-admin operation, like
// tenancy provisioning and secret refs. The auth/render plumbing is kept local rather than shared so this
// surface stays decoupled from the other tested handlers.
//
// ACL-FIRST HOOK (T5 hardens): retrieval applies the principal's ACL grants AT THE QUERY LEVEL. In this T4
// spine the grants arrive in the request body (`acl_grants`) as the mechanism; T5 REPLACES body-supplied
// grants with grants derived from the authenticated principal — a request body is never trusted for
// authorization. The query-level predicate is the seam that makes that hardening a WHERE-clause change,
// not a post-fetch filter (which §25.15.4 forbids).
type knowledgeHandler struct {
	knowledge KnowledgeAPI
}

func (h *knowledgeHandler) createBase(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.knowledge.CreateKnowledgeBase(r.Context(), scope, raw)
	h.write(w, r, out, err, http.StatusCreated)
}

func (h *knowledgeHandler) listBases(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.knowledge.ListKnowledgeBases(r.Context(), scope)
	h.write(w, r, out, err, http.StatusOK)
}

func (h *knowledgeHandler) createSource(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.knowledge.CreateSource(r.Context(), scope, r.PathValue("kb_id"), raw)
	h.write(w, r, out, err, http.StatusCreated)
}

func (h *knowledgeHandler) listSources(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.knowledge.ListSources(r.Context(), scope, r.PathValue("kb_id"))
	h.write(w, r, out, err, http.StatusOK)
}

func (h *knowledgeHandler) deleteSource(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.knowledge.DeleteSource(r.Context(), scope, r.PathValue("source_id"))
	h.write(w, r, out, err, http.StatusOK)
}

func (h *knowledgeHandler) ingest(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.knowledge.Ingest(r.Context(), scope, r.PathValue("kb_id"), r.PathValue("source_id"), raw)
	h.write(w, r, out, err, http.StatusOK)
}

func (h *knowledgeHandler) query(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.knowledge.Retrieve(r.Context(), scope, r.PathValue("kb_id"), raw)
	h.write(w, r, out, err, http.StatusOK)
}

func (h *knowledgeHandler) listIndexRevisions(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.knowledge.ListIndexRevisions(r.Context(), scope, r.PathValue("kb_id"))
	h.write(w, r, out, err, http.StatusOK)
}

// authorize resolves the verified scope and enforces the provision capability.
func (h *knowledgeHandler) authorize(w http.ResponseWriter, r *http.Request) (middleware.Scope, bool) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return middleware.Scope{}, false
	}
	if !scope.HasScope(provisionScope) {
		middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope", "this API key lacks the provision capability")
		return middleware.Scope{}, false
	}
	return scope, true
}

func (h *knowledgeHandler) begin(w http.ResponseWriter, r *http.Request) (middleware.Scope, []byte, bool) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return middleware.Scope{}, nil, false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return middleware.Scope{}, nil, false
	}
	return scope, raw, true
}

// write renders a knowledge outcome: the typed rejects first, then 2xx with the projection.
func (h *knowledgeHandler) write(w http.ResponseWriter, r *http.Request, out ProvisionResult, err error, okStatus int) {
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	switch {
	case out.MissingField != "":
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", out.MissingField+" is required")
		return
	case out.BadField:
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body carries an unsupported or invalid field")
		return
	case out.NotFound:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such knowledge resource in this scope")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(okStatus)
	_, _ = w.Write(out.Body)
}
