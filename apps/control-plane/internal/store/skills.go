package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/egress"
)

// The E12 skills management surface (spec §20.2, §28.15-28.16, TOL-011). These methods adapt the
// tenant-scoped api.SkillRegistryAPI contract to the extensions store: scope → (organization, project),
// the typed rejects → api.SkillResult flags, and a committed row → its JSON projection. Install and
// enable are admin actions — there is no model-facing install surface.

// CreateSkill registers a named skill lineage. A name collision is a Conflict (409).
func (s *Store) CreateSkill(ctx context.Context, scope middleware.Scope, body []byte) (api.SkillResult, error) {
	var req struct {
		Name string `json:"name"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return api.SkillResult{BadField: true}, nil
	}
	skill, err := s.tools.CreateSkill(ctx, scope.Organization, scope.Project, req.Name)
	if res, mapped := skillReject(err); mapped {
		return res, nil
	}
	if err != nil {
		return api.SkillResult{}, err
	}
	return api.SkillResult{Body: mustJSON(map[string]any{"id": skill.ID, "object": "skill", "name": skill.Name})}, nil
}

// InstallSkillRevision installs a revision by URL: fetch (hardened egress) → quarantine → digest →
// metadata. An unsafe archive / denied source is a BadField (400); an unknown skill is a NotFound (404).
func (s *Store) InstallSkillRevision(ctx context.Context, scope middleware.Scope, skillID string, body []byte) (api.SkillResult, error) {
	var req struct {
		SourceURL string `json:"source_url"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return api.SkillResult{BadField: true}, nil
	}
	rev, err := s.tools.InstallSkillRevisionFromURL(ctx, scope.Organization, scope.Project, skillID, req.SourceURL)
	if res, mapped := skillReject(err); mapped {
		return res, nil
	}
	if err != nil {
		return api.SkillResult{}, err
	}
	return api.SkillResult{Body: mustJSON(map[string]any{
		"id": rev.ID, "object": "skill_revision", "skill_id": rev.SkillID,
		"revision_number": rev.RevisionNumber, "digest": rev.Digest, "state": rev.State,
		"scan_findings": rev.Findings,
	})}, nil
}

// EnableSkillRevision enables an approved revision. Scan findings → Conflict (409); unknown → NotFound.
func (s *Store) EnableSkillRevision(ctx context.Context, scope middleware.Scope, skillID, revisionID string) (api.SkillResult, error) {
	exists, err := s.tools.EnableSkillRevision(ctx, scope.Organization, scope.Project, revisionID)
	if res, mapped := skillReject(err); mapped {
		return res, nil
	}
	if err != nil {
		return api.SkillResult{}, err
	}
	if !exists {
		return api.SkillResult{NotFound: true}, nil
	}
	return api.SkillResult{Body: mustJSON(map[string]any{"id": revisionID, "object": "skill_revision", "state": "enabled"})}, nil
}

// ListSkills lists a project's skill lineages.
func (s *Store) ListSkills(ctx context.Context, scope middleware.Scope) (api.SkillResult, error) {
	skills, err := s.tools.ListSkills(ctx, scope.Organization, scope.Project)
	if err != nil {
		return api.SkillResult{}, err
	}
	data := make([]any, 0, len(skills))
	for _, sk := range skills {
		data = append(data, map[string]any{"id": sk.ID, "object": "skill", "name": sk.Name})
	}
	return api.SkillResult{Body: mustJSON(map[string]any{"object": "list", "data": data})}, nil
}

// skillReject maps a typed domain error to its api.SkillResult reject flag: a bad source/archive → 400,
// a name/state conflict → 409, an absent skill/revision → 404. A nil or unrecognised error is not mapped
// (a transient/DB error surfaces as a 500 through the caller).
func skillReject(err error) (api.SkillResult, bool) {
	switch {
	case err == nil:
		return api.SkillResult{}, false
	case errors.Is(err, extensions.ErrUnsafeArchive),
		errors.Is(err, extensions.ErrSkillMetadataMissing),
		errors.Is(err, egress.ErrDenied):
		return api.SkillResult{BadField: true}, true
	case errors.Is(err, extensions.ErrSkillNameCollision),
		errors.Is(err, extensions.ErrScanFindingsBlockEnable):
		return api.SkillResult{Conflict: true}, true
	case errors.Is(err, extensions.ErrSkillNotFound):
		return api.SkillResult{NotFound: true}, true
	default:
		return api.SkillResult{}, false
	}
}
