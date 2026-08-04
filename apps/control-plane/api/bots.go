package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// The kind-agnostic bot registry (2026-08-03 plan, Task 4). A control plane that is about to be talked
// to by N separate relay processes — one per channel a project wires up — needs exactly one durable fact
// about each: a name, WHAT KIND of thing it is (a value from a list, never a special case), the agent it
// speaks as, the repository and principal it defaults to, and a document its channel adapter alone
// understands. This file is that registry's whole HTTP surface.
//
// THE ONE RULE EVERY LINE HERE OBEYS: the control plane never learns what a bot IS. `kind` is a bare
// string it never branches on; `config` is carried as json.RawMessage and never decoded, so a field one
// channel's adapter needs (a workspace id, a phone number, a webhook secret handle) never requires a
// change in this package. There is a guard test for exactly this in bots_test.go — it fails if this file
// so much as mentions a channel by name.

// BotRegistry is the store seam for the registry; internal/bots.Store implements it. Tiers that wire no
// bot store pass nil and the routes stay unmounted, the same posture every other optional surface takes.
type BotRegistry interface {
	CreateBot(ctx context.Context, scope middleware.Scope, req BotCreate) (BotResult, error)
	GetBot(ctx context.Context, scope middleware.Scope, id string) (BotResult, error)
	ListBots(ctx context.Context, scope middleware.Scope, q ListQuery) ([]ListRow, error)
	PatchBot(ctx context.Context, scope middleware.Scope, id string, patch BotPatch) (BotResult, error)
	DeleteBot(ctx context.Context, scope middleware.Scope, id string) (bool, error)
}

// ErrBotNameTaken is returned when this project already has a bot by that name (the registry's
// UNIQUE (project_id, name) on integration_bots — declared as (organization_id, project_id, name) and
// rebuilt without the organization during the A.2 organization removal) — a typed value rather than a raw
// SQL error so this package renders a 409 without parsing a SQLSTATE.
var ErrBotNameTaken = errors.New("api: a bot with that name already exists in this project")

// BotCreate is the resolved create body. The id is minted by the store, matching RepositoryBindingCreate.
type BotCreate struct {
	Name                string
	Kind                string
	AgentRevisionID     string
	RepositoryBindingID string
	PrincipalID         string
	Config              json.RawMessage
}

// BotResult is a bot projection. Invalid is a create missing a required field (400); NotFound is a read
// of a missing/foreign id (404); Body is the resource to return verbatim — matching BindingResult.
type BotResult struct {
	Body     []byte
	Invalid  bool
	NotFound bool
}

// BotPatch is a partial revision: a nil field is unchanged, Config nil means unchanged. `kind` has no
// field here — it is this row's identity, immutable the way every other registration surface in this
// tree keeps its own workspace/team identifier immutable: changing what a bot IS is a new registration,
// not a revision of this one.
type BotPatch struct {
	Name                *string
	AgentRevisionID     *string
	RepositoryBindingID *string
	PrincipalID         *string
	Config              json.RawMessage
	Disabled            *bool
}

type botHandler struct{ bots BotRegistry }

// botCreateBody is the edge's strict shape (spec plan Task 4's row shape). DisallowUnknownFields is what
// keeps `config`'s opacity real at the boundary too: a caller cannot smuggle a sibling top-level field
// past this decoder and have it silently dropped.
type botCreateBody struct {
	Name                string          `json:"name"`
	Kind                string          `json:"kind"`
	AgentRevisionID     string          `json:"agent_revision_id"`
	RepositoryBindingID string          `json:"repository_binding_id"`
	PrincipalID         string          `json:"principal_id"`
	Config              json.RawMessage `json:"config"`
}

// create registers a bot (POST /v1/bots). Durable config, server-minted id, no Idempotency-Key — the
// repository-binding posture: a rare operator action whose duplicate is operator-visible (and here,
// additionally, refused as a name conflict).
func (h *botHandler) create(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return
	}
	var body botCreateBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"the body accepts only name, kind, agent_revision_id, repository_binding_id, principal_id and config")
		return
	}

	out, err := h.bots.CreateBot(r.Context(), scope, BotCreate{
		Name: body.Name, Kind: body.Kind, AgentRevisionID: body.AgentRevisionID,
		RepositoryBindingID: body.RepositoryBindingID, PrincipalID: body.PrincipalID, Config: body.Config,
	})
	switch {
	case errors.Is(err, ErrBotNameTaken):
		middleware.WriteProblem(w, r, http.StatusConflict, "conflict", "a bot with that name already exists in this project")
		return
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if out.Invalid {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "name and kind are required")
		return
	}
	w.Header().Set("Location", "/v1/bots/"+botIDOf(out.Body))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(out.Body)
}

// get reads one bot within the verified scope (GET /v1/bots/{bot_id}). A missing or foreign id is a
// tenant-scoped 404, leaking no cross-tenant existence.
func (h *botHandler) get(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.bots.GetBot(r.Context(), scope, r.PathValue("bot_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if out.NotFound {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such bot in this project")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Body)
}

// list returns a tenant-scoped page of bots (GET /v1/bots), confined to the verified scope by RLS.
func (h *botHandler) list(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	q, ok := beginList(w, r, "bots", scope)
	if !ok {
		return
	}
	rows, err := h.bots.ListBots(r.Context(), scope, q)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	renderPage(w, r, "bots", scope, rows, q.Limit)
}

// botPatchBody is the edge's strict shape for a revision. Pointers distinguish "absent" from "set to
// empty" — clearing agent_revision_id is a real operation (unassigning a bot from an agent) and must not
// be confused with a PATCH that simply did not mention it. There is deliberately no `kind` field: see
// BotPatch.
type botPatchBody struct {
	Name                *string         `json:"name"`
	AgentRevisionID     *string         `json:"agent_revision_id"`
	RepositoryBindingID *string         `json:"repository_binding_id"`
	PrincipalID         *string         `json:"principal_id"`
	Config              json.RawMessage `json:"config"`
	Disabled            *bool           `json:"disabled"`
}

// patch repairs a bot (PATCH /v1/bots/{bot_id}).
func (h *botHandler) patch(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return
	}
	var body botPatchBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"a revision accepts only name, agent_revision_id, repository_binding_id, principal_id, config and disabled — kind is immutable")
		return
	}
	if body.Name != nil && *body.Name == "" {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "name cannot be cleared")
		return
	}
	out, err := h.bots.PatchBot(r.Context(), scope, r.PathValue("bot_id"), BotPatch{
		Name: body.Name, AgentRevisionID: body.AgentRevisionID, RepositoryBindingID: body.RepositoryBindingID,
		PrincipalID: body.PrincipalID, Config: body.Config, Disabled: body.Disabled,
	})
	switch {
	case errors.Is(err, ErrBotNameTaken):
		middleware.WriteProblem(w, r, http.StatusConflict, "conflict", "a bot with that name already exists in this project")
		return
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if out.NotFound {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such bot in this project")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Body)
}

// delete unregisters a bot (DELETE /v1/bots/{bot_id}). A hard delete, unlike repository bindings' archive:
// nothing in this tenant's schema holds a foreign key onto integration_bots (the plan's own rule — no
// provenance row is retracted by removing one), so there is no receipt for a delete to falsify.
func (h *botHandler) delete(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	found, err := h.bots.DeleteBot(r.Context(), scope, r.PathValue("bot_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !found {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such bot in this project")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// botIDOf reads the id from a bot projection for the Location header; "" if unparseable.
func botIDOf(body []byte) string {
	var probe struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.ID
}
