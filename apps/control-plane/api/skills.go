package api

import (
	"context"
	"io"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// SkillRegistryAPI is the store seam for the E12 skills management surface (spec §20.2, §28.15-28.16,
// TOL-011): skill lineages, install-by-URL of an immutable QUARANTINE-sanitized revision, and the
// enable transition. Install and enable are ADMIN actions — there is deliberately NO model-facing
// install path (a skill is untrusted content, not a tool). The Postgres store implements it; tiers that
// never touch skills pass nil so the routes stay unmounted. Scoped by the verified identity (§39.2).
type SkillRegistryAPI interface {
	CreateSkill(ctx context.Context, scope middleware.Scope, body []byte) (SkillResult, error)
	InstallSkillRevision(ctx context.Context, scope middleware.Scope, skillID string, body []byte) (SkillResult, error)
	EnableSkillRevision(ctx context.Context, scope middleware.Scope, skillID, revisionID string) (SkillResult, error)
	ListSkills(ctx context.Context, scope middleware.Scope) (SkillResult, error)
}

// SkillResult is a management projection. Exactly one outcome is set: Body carries the created/installed/
// enabled/listed resource (2xx); BadField marks a malformed body or a fetch/quarantine rejection (400);
// Conflict marks a name collision or an enable blocked by scan findings (409); NotFound marks an absent
// skill or revision (404).
type SkillResult struct {
	Body     []byte
	BadField bool
	Conflict bool
	NotFound bool
}

type skillHandler struct {
	skills SkillRegistryAPI
}

// createSkill registers a named skill lineage (POST /v1/skills). Durable config, server-minted id.
func (h *skillHandler) createSkill(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.skills.CreateSkill(r.Context(), scope, raw)
	// No Location: `/v1/skills/<id>` is not mounted — the family has a LIST and no singular read (E29 T2).
	// Mounting one was considered and deferred rather than done here, because it would serve the list's
	// three-field projection ({id, object, name}: no revision id, no enabled bit, no digest) and a singular
	// read that answers none of the questions a caller has is a second thing to fix later. The projection and
	// the route belong in one change. The minted id is in the 201 body.
	h.write(w, r, out, err, http.StatusCreated, "")
}

// installRevision installs a revision by URL (POST /v1/skills/{skill_id}/revisions). The body carries
// the source_url; the store fetches over the hardened egress path, quarantines, and stores the sanitized
// archive + digest. An unsafe archive or a denied fetch is a 400; an unknown skill is a 404.
func (h *skillHandler) installRevision(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.skills.InstallSkillRevision(r.Context(), scope, r.PathValue("skill_id"), raw)
	// No Location: `/v1/skill-revisions/` is the exact twin of the `/v1/tool-revisions/` case router.go
	// describes — a prefix NO epic ever mounted, named in a 201 since the family shipped (E29 T2). The
	// revision id this returns is load-bearing (enable takes it) and the 201 body is the only place it has
	// ever existed; that is a real read gap, and it is not fixed by pointing at an address that 404s.
	h.write(w, r, out, err, http.StatusCreated, "")
}

// enableRevision enables an approved revision (POST /v1/skills/{skill_id}/revisions/{revision_id}/enable).
// A revision with scan findings is a 409 (stuck at quarantined); an unknown revision is a 404.
func (h *skillHandler) enableRevision(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.skills.EnableSkillRevision(r.Context(), scope, r.PathValue("skill_id"), r.PathValue("revision_id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// listSkills lists a project's skill lineages (GET /v1/skills).
func (h *skillHandler) listSkills(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.skills.ListSkills(r.Context(), scope)
	h.write(w, r, out, err, http.StatusOK, "")
}

// begin authenticates and reads the bounded body (the toolHandler twin).
func (h *skillHandler) begin(w http.ResponseWriter, r *http.Request) (middleware.Scope, []byte, bool) {
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

// write renders a management outcome: the typed rejects first, then 2xx with the resource.
func (h *skillHandler) write(w http.ResponseWriter, r *http.Request, out SkillResult, err error, okStatus int, locationPrefix string) {
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	switch {
	case out.BadField:
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request carries an unsupported field, an unsafe archive, or a denied source")
		return
	case out.Conflict:
		middleware.WriteProblem(w, r, http.StatusConflict, "conflict", "the skill name is already taken, or the revision has scan findings and cannot be enabled")
		return
	case out.NotFound:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such skill or skill revision in this project")
		return
	}
	if locationPrefix != "" {
		w.Header().Set("Location", locationPrefix+resourceIDOf(out.Body))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(okStatus)
	_, _ = w.Write(out.Body)
}
