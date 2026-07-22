// Package automation is the control-plane domain logic for the E11 automation layer. Task 1 opens it
// with the agent surface: AgentProfile lineages, IMMUTABLE publishable AgentRevisions, and profile-free
// RunTemplateRevisions (spec §10, §32.2). A revise always creates a NEW draft revision — nothing here
// ever rewrites a revision's config columns, so a published revision is immutable by discipline; publish
// is the one legitimate mutation (a once-only conditional flip). Resolution of a run's pinned revision
// into its ExecutionSpec lives on the coordinator spine (execution reads it there); this package owns the
// management writes and reads.
//
// ponytail: the contract registers agent.revision.published.v1, but NO code emits it — publication's
// durable fact IS the published_at flip on the revision row (queryable, immutable-once-set). A
// project-scoped management action has no session journal to ride, so the event is declared-but-
// unemitted by design; add an audit/journal emitter here if a downstream consumer ever needs the event.
package automation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// ErrUnknownField is returned when a revision body carries a field outside the accepted config subset —
// knowledge (E17, opened by that epic under the same pattern) or, for a template, an identity/delegation
// field a template must never carry. As of E12 Task 2 the four extension fields (tool_sets/mcp_connections/
// skills/hooks) are ACCEPTED (see RevisionInput). Dead or unsupported config is still rejected, never
// silently stored (honest naming, spec §2).
var ErrUnknownField = errors.New("automation: revision body carries an unsupported field")

// ErrProfileNotFound is returned when a revision is created against a profile absent from the scope.
var ErrProfileNotFound = errors.New("automation: agent profile not found in scope")

// Store is the automation management store over the durable spine's pool.
type Store struct{ pool *pgxpool.Pool }

// New wraps a pgx pool as the automation store.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// RevisionInput is the enforced executable-config subset a revision (agent or template) carries (spec
// §10, §2). Model "" inherits the deployment default; Tools nil imposes no capability ceiling (a non-nil
// set — even empty — is the ceiling the resolver intersects). Any field outside this struct is rejected
// by DecodeRevisionInput.
//
// The four E12 extension fields (ToolSets/MCPConnections/Skills/Hooks) are accepted here as of E12 Task 2
// (the deliberate reversal of E11's unknown-field reject — phase-11 §7 devir 3). This package OPENS the
// schema for all four so the wave-2 tasks (T5 mcp, T7 skills, T8 hooks) never touch agents.go again (the
// conflict shield). T2 CONSUMES only ToolSets (a list of published ToolSetRevision ids the resolver
// unions into the effective set); MCPConnections/Skills/Hooks ride OPAQUE — persisted but validated and
// consumed by their owning task, never here.
//
// Reference validation (that a ToolSets id names an existing, published, in-tenant ToolSetRevision) is
// DEFERRED to consumption, not enforced at create: the resolver (PinnedRunConfig) and the broker lookup
// both filter on published_at + tenant, so a typo'd/draft/foreign id fails CLOSED — it grants no
// capability and leaks nothing. This is deliberate: validating references at create would force each
// wave-2 task (T5/T7/T8) to add its own cross-table check here, re-coupling the very seam the conflict
// shield protects. A future loud-at-create validation is a documented, non-blocking follow-up.
type RevisionInput struct {
	Model          string   `json:"model"`
	Tools          []string `json:"tools"`
	Instructions   string   `json:"instructions"`
	ToolSets       []string `json:"tool_sets"`
	MCPConnections []string `json:"mcp_connections"`
	Skills         []string `json:"skills"`
	Hooks          []string `json:"hooks"`
}

// Revision is a stored revision's committed shape (management GET + the immutability check). ToolSets is
// the E12 extension T2 consumes (the pinned published ToolSetRevision ids); it is populated at create
// from the decoded input. The opaque MCPConnections/Skills/Hooks are persisted but not surfaced here —
// their owning task reads its own field.
type Revision struct {
	ID             string
	RevisionNumber int
	Model          string
	Tools          []string
	Instructions   string
	ToolSets       []string
	Published      bool
}

// DecodeRevisionInput strictly decodes the executable-config subset, REJECTING any unknown field via
// json.DisallowUnknownFields — the stdlib guard is enough (ponytail). It backs both agent revisions
// and templates: a template naming an identity/delegation field fails here because the struct has no
// such field, so a template can never impersonate an agent identity.
func DecodeRevisionInput(raw []byte) (RevisionInput, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in RevisionInput
	if err := dec.Decode(&in); err != nil {
		return RevisionInput{}, fmt.Errorf("%w: %v", ErrUnknownField, err)
	}
	return in, nil
}

// CreateProfile inserts a named agent-profile lineage and returns its id.
func (s *Store) CreateProfile(ctx context.Context, org, project, name string) (string, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	id := newID("aprof")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertAgentProfile"), id, org, project, name); err != nil {
		return "", fmt.Errorf("insert agent profile: %w", err)
	}
	return id, nil
}

// CreateRevision inserts a DRAFT revision under a profile from a raw body (strictly decoded). It verifies
// the profile is in scope first, so a revision never attaches to a foreign/unknown profile. A revise is
// just another CreateRevision — the config columns of earlier revisions are never touched.
func (s *Store) CreateRevision(ctx context.Context, org, project, profileID string, raw []byte) (Revision, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	in, err := DecodeRevisionInput(raw)
	if err != nil {
		return Revision{}, err
	}
	switch err := s.pool.QueryRow(ctx, storage.Query("AgentProfileExists"), profileID, org, project).Scan(new(int)); {
	case errors.Is(err, pgx.ErrNoRows):
		return Revision{}, ErrProfileNotFound
	case err != nil:
		return Revision{}, fmt.Errorf("verify agent profile: %w", err)
	}
	id := newID("arev")
	var number int
	// ponytail: revision_number is MAX+1 in-statement, so two concurrent CreateRevision on ONE profile
	// can pick the same number and one loses the UNIQUE(profile_id, revision_number) (a 23505 → 500).
	// Benign at the expected authoring cadence (a human editing a profile); add a retry-on-23505 loop
	// if concurrent revise throughput ever matters.
	if err := s.pool.QueryRow(ctx, storage.Query("InsertAgentRevision"),
		id, org, project, profileID, in.Model, marshalTools(in.Tools), in.Instructions,
		marshalTools(in.ToolSets), marshalTools(in.MCPConnections), marshalTools(in.Skills), marshalTools(in.Hooks)).Scan(&number); err != nil {
		return Revision{}, fmt.Errorf("insert agent revision: %w", err)
	}
	return Revision{ID: id, RevisionNumber: number, Model: in.Model, Tools: in.Tools, Instructions: in.Instructions, ToolSets: in.ToolSets}, nil
}

// PublishRevision flips a draft revision to published exactly once. published is true only when THIS
// call did the flip; exists distinguishes an unknown revision (false) from one already published
// (true) — so the caller can 404 an unknown id while treating a re-publish as an idempotent success.
func (s *Store) PublishRevision(ctx context.Context, org, project, revisionID string) (published, exists bool, err error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	return s.publish(ctx, "PublishAgentRevision", "AgentRevisionPublished", revisionID, org, project)
}

// GetRevision reads a revision's committed shape, or found=false when it is absent from the scope.
func (s *Store) GetRevision(ctx context.Context, org, project, revisionID string) (Revision, bool, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	var (
		rev       Revision
		toolsJSON []byte
		published *any
	)
	rev.ID = revisionID
	err := s.pool.QueryRow(ctx, storage.Query("GetAgentRevision"), revisionID, org, project).
		Scan(new(string), &rev.RevisionNumber, &rev.Model, &toolsJSON, &rev.Instructions, &published, new(any))
	if errors.Is(err, pgx.ErrNoRows) {
		return Revision{}, false, nil
	}
	if err != nil {
		return Revision{}, false, fmt.Errorf("read agent revision: %w", err)
	}
	rev.Published = published != nil
	rev.Tools = unmarshalTools(toolsJSON)
	return rev, true, nil
}

// CreateTemplateRevision inserts a DRAFT run-template revision (profile-free, identity/delegation
// rejected by the strict decode) under a template name and returns it.
func (s *Store) CreateTemplateRevision(ctx context.Context, org, project, templateName string, raw []byte) (Revision, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	in, err := DecodeRevisionInput(raw)
	if err != nil {
		return Revision{}, err
	}
	id := newID("rtr")
	var number int
	if err := s.pool.QueryRow(ctx, storage.Query("InsertRunTemplateRevision"),
		id, org, project, templateName, in.Model, marshalTools(in.Tools), in.Instructions,
		marshalTools(in.ToolSets), marshalTools(in.MCPConnections), marshalTools(in.Skills), marshalTools(in.Hooks)).Scan(&number); err != nil {
		return Revision{}, fmt.Errorf("insert run template revision: %w", err)
	}
	return Revision{ID: id, RevisionNumber: number, Model: in.Model, Tools: in.Tools, Instructions: in.Instructions, ToolSets: in.ToolSets}, nil
}

// PublishTemplateRevision flips a draft template revision to published exactly once (see PublishRevision).
func (s *Store) PublishTemplateRevision(ctx context.Context, org, project, revisionID string) (published, exists bool, err error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	return s.publish(ctx, "PublishRunTemplateRevision", "RunTemplateRevisionPublished", revisionID, org, project)
}

// publish is the shared once-only flip: try the conditional UPDATE, and on no flip disambiguate an
// unknown revision from an already-published one via the publish-state read (both agent and template).
func (s *Store) publish(ctx context.Context, flipQuery, stateQuery, revisionID, org, project string) (published, exists bool, err error) {
	switch e := s.pool.QueryRow(ctx, storage.Query(flipQuery), revisionID, org, project).Scan(new(string)); {
	case e == nil:
		return true, true, nil
	case !errors.Is(e, pgx.ErrNoRows):
		return false, false, fmt.Errorf("publish revision: %w", e)
	}
	// No flip: the revision is unknown or already published. The state read tells them apart.
	switch e := s.pool.QueryRow(ctx, storage.Query(stateQuery), revisionID, org, project).Scan(new(bool)); {
	case errors.Is(e, pgx.ErrNoRows):
		return false, false, nil
	case e != nil:
		return false, false, fmt.Errorf("read revision publish state: %w", e)
	}
	return false, true, nil
}

// marshalTools keeps a nil ceiling NULL (no ceiling) and a non-nil set — even empty — a stored ceiling.
func marshalTools(tools []string) any {
	if tools == nil {
		return nil
	}
	out, _ := json.Marshal(tools)
	return out
}

func unmarshalTools(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var tools []string
	_ = json.Unmarshal(raw, &tools)
	return tools
}

// newID mints an opaque, globally unique id with the given prefix (the config-revision id pattern).
func newID(prefix string) string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}
