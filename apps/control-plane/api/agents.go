package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// AgentRegistry is the store seam for the automation-agent management surface (spec §20.2.1, §10):
// AgentProfile lineages, immutable publishable AgentRevisions, and profile-free RunTemplateRevisions.
// The Postgres store implements it; production wires it, and tiers that never touch agents pass nil so
// the routes stay unmounted. It is scoped by the verified identity, never a request-body field (§39.2).
type AgentRegistry interface {
	CreateAgentProfile(ctx context.Context, scope middleware.Scope, name string) (AgentResult, error)
	CreateAgentRevision(ctx context.Context, scope middleware.Scope, profileID string, body []byte) (AgentResult, error)
	PublishAgentRevision(ctx context.Context, scope middleware.Scope, revisionID string) (AgentResult, error)
	CreateRunTemplateRevision(ctx context.Context, scope middleware.Scope, templateName string, body []byte) (AgentResult, error)
	PublishRunTemplateRevision(ctx context.Context, scope middleware.Scope, revisionID string) (AgentResult, error)
	// GetAgentProfile + ListAgentProfiles + ListAgentRevisions are the E13 T4 read side, RLS-scoped.
	GetAgentProfile(ctx context.Context, scope middleware.Scope, id string) (AgentResult, error)
	ListAgentProfiles(ctx context.Context, scope middleware.Scope, q ListQuery) ([]ListRow, error)
	ListAgentRevisions(ctx context.Context, scope middleware.Scope, profileID string, q ListQuery) ([]ListRow, error)
}

// AgentResult is a management projection. Exactly one outcome is set: Body carries the created/published
// resource (2xx); BadField marks a body outside the enforced executable-config subset (400 — an E12 or
// identity/delegation field); NotFound marks an absent profile or revision (404); MissingName marks a
// profile create with no name (400).
type AgentResult struct {
	Body        []byte
	BadField    bool
	NotFound    bool
	MissingName bool
	// NameTaken is a profile name already registered in this project: a 409, not a 500.
	NameTaken bool
}

type agentHandler struct {
	agents AgentRegistry
}

// createProfile registers a named agent-profile lineage (POST /v1/agents). Durable config, not an
// idempotent operation, so no idempotency key — the API mints the id server-side.
func (h *agentHandler) createProfile(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON")
		return
	}
	out, err := h.agents.CreateAgentProfile(r.Context(), scope, body.Name)
	h.write(w, r, out, err, http.StatusCreated, "/v1/agents/")
}

// createRevision creates a DRAFT agent revision from the executable-config body (POST
// /v1/agents/{agent_id}/revisions). An unsupported field is a 400; an unknown profile is a 404.
func (h *agentHandler) createRevision(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.agents.CreateAgentRevision(r.Context(), scope, r.PathValue("agent_id"), raw)
	// No Location: `/v1/agent-revisions/<id>` was never a mounted prefix, so following it 404'd (E29 T2;
	// the router.go note above the tool-revision routes describes the same class). What is missing here is
	// not a capability — a revision IS readable, at GET /v1/agents/{agent_id}/revisions — but an ADDRESS: a
	// Location declares a collection member, this collection is /v1/agents/{id}/revisions, and its singular
	// member is not mounted. Writing the correct-looking wrong address was the other option and it is worse
	// than writing none. The id is in the 201 body either way.
	h.write(w, r, out, err, http.StatusCreated, "")
}

// publishRevision publishes a draft agent revision (POST /v1/agents/{agent_id}/revisions/{revision_id}/publish).
func (h *agentHandler) publishRevision(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.agents.PublishAgentRevision(r.Context(), scope, r.PathValue("revision_id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// createTemplateRevision creates a DRAFT run-template revision (POST /v1/run-templates/{template}/revisions).
func (h *agentHandler) createTemplateRevision(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.agents.CreateRunTemplateRevision(r.Context(), scope, r.PathValue("template"), raw)
	// No Location: `/v1/run-template-revisions/<id>` was never mounted (E29 T2). Unlike an agent revision
	// this one has no correct address to point at either — run_template_revisions is read by NO layer: no
	// route, and the only SELECT over the table is a published-or-not boolean. A caller can pass this id to
	// POST /v1/responses and can never read it back; that is a gap for a later epic to close with a query, a
	// projection and a route. Until then the honest header is no header.
	h.write(w, r, out, err, http.StatusCreated, "")
}

// publishTemplateRevision publishes a draft run-template revision.
func (h *agentHandler) publishTemplateRevision(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.agents.PublishRunTemplateRevision(r.Context(), scope, r.PathValue("revision_id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// getProfile reads one agent-profile lineage (GET /v1/agents/{agent_id}).
func (h *agentHandler) getProfile(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.agents.GetAgentProfile(r.Context(), scope, r.PathValue("agent_id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// listProfiles returns a tenant-scoped page of agent-profile lineages (GET /v1/agents).
func (h *agentHandler) listProfiles(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	q, ok := beginList(w, r, "agents", scope)
	if !ok {
		return
	}
	rows, err := h.agents.ListAgentProfiles(r.Context(), scope, q)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	renderPage(w, r, "agents", scope, rows, q.Limit)
}

// listRevisions returns a tenant-scoped page of one profile's revisions (GET /v1/agents/{id}/revisions).
func (h *agentHandler) listRevisions(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	// The cursor kind carries the profile id: a revisions list is scoped to ONE profile, so a cursor
	// minted on profile A must NOT MAC-validate on profile B's revisions (which would silently skip B's
	// rows past A's keyset position). Both beginList and renderPage use the same profile-scoped kind.
	kind := "agent-revisions:" + r.PathValue("agent_id")
	q, ok := beginList(w, r, kind, scope)
	if !ok {
		return
	}
	rows, err := h.agents.ListAgentRevisions(r.Context(), scope, r.PathValue("agent_id"), q)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	renderPage(w, r, kind, scope, rows, q.Limit)
}

// begin authenticates and reads the bounded body, shared by the create handlers.
func (h *agentHandler) begin(w http.ResponseWriter, r *http.Request) (middleware.Scope, []byte, bool) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return middleware.Scope{}, nil, false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return middleware.Scope{}, nil, false
	}
	return scope, raw, true
}

// write renders a management outcome: the typed rejects first, then 2xx with the resource (and a
// Location header for a create).
func (h *agentHandler) write(w http.ResponseWriter, r *http.Request, out AgentResult, err error, okStatus int, locationPrefix string) {
	if err != nil {
		// A 500 WITH NO LOG LINE IS AN UNDIAGNOSABLE ONE. The problem body carries a request_id so an
		// operator can find the cause; before this line the cause was never written anywhere, so the id
		// led nowhere and every agent-surface failure looked identical from both ends.
		log.Printf("agents: %s %s: %v", r.Method, r.URL.Path, err)
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	switch {
	case out.NameTaken:
		middleware.WriteProblem(w, r, http.StatusConflict, "conflict",
			"an agent with this name already exists in this project; use its id or choose another name")
		return
	case out.MissingName:
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "name is required")
		return
	case out.BadField:
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the revision carries an unsupported field (accepted: model, tools, instructions, tool_sets, mcp_connections, skills, hooks)")
		return
	case out.NotFound:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such agent, revision, or template in this project")
		return
	}
	if locationPrefix != "" {
		w.Header().Set("Location", locationPrefix+resourceIDOf(out.Body))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(okStatus)
	_, _ = w.Write(out.Body)
}

// resourceIDOf reads the id from a management projection for the Location header; "" if unparseable.
func resourceIDOf(body []byte) string {
	var probe struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.ID
}
