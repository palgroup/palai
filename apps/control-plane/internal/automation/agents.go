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

// ErrProfileNameTaken is a profile name that already exists in this project. It is the caller's to fix
// — a different name, or the existing profile's id — so it must never reach a client as a 500.
var ErrProfileNameTaken = errors.New("agent profile name already exists")

// ErrEnvironmentNotFound is returned when a revision names an `environment` that does not exist in the
// caller's organization (E25 T3), at create and again at publish.
//
// IT DEPARTS FROM THIS FILE'S OWN "validate at consumption" RULE, AND THE REASON IS THE DIFFERENCE BETWEEN
// A CAPABILITY AND A CREDENTIAL. RevisionInput's comment defers tool_sets/mcp/skills/hooks reference checks
// to consumption because a typo'd id there fails CLOSED and harmlessly: it grants no capability and leaks
// nothing. An environment fails closed too — an unknown id resolves to zero keys — but "harmlessly" does
// not follow. The operator's belief is that the agent HAS the credentials, and a run that silently receives
// none does not stop: `curl` succeeds anonymously, `gh` reads the public repository, the deploy script
// writes to the default target. The failure is a wrong answer that looks like a right one, which is exactly
// the class of thing worth 5 lines and one query to refuse loudly.
var ErrEnvironmentNotFound = errors.New("automation: environment not found in scope")

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
	// Environment is the id of the environment whose key→value pairs this agent's shell commands receive
	// (E25 T3, the `environment` column on agent_revisions). Empty means no environment, which is every existing revision. Unlike the
	// four fields above this one IS reference-checked at create and at publish — see ErrEnvironmentNotFound
	// for why a credential is not a capability.
	Environment string `json:"environment"`
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
	Environment    string
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
func (s *Store) CreateProfile(ctx context.Context, project, name string) (string, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	id := newID("aprof")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertAgentProfile"), id, project, name); err != nil {
		// A NAME ALREADY IN USE IS THE CALLER'S ANSWER, NOT THE SERVER'S FAULT. Measured 2026-08-02 against
		// the live control plane: re-registering an existing profile name served
		// `500 internal_error retryable:true` with NO log line — so the request_id in the body led
		// nowhere, and `retryable` told the client to do the one thing that can never work. `palai up`'s
		// Slack step and the iOS live smoke both read that 500 as a broken control plane on every run
		// after the first.
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
			return "", fmt.Errorf("%w: %q", ErrProfileNameTaken, name)
		}
		return "", fmt.Errorf("insert agent profile: %w", err)
	}
	return id, nil
}

// CreateRevision inserts a DRAFT revision under a profile from a raw body (strictly decoded). It verifies
// the profile is in scope first, so a revision never attaches to a foreign/unknown profile. A revise is
// just another CreateRevision — the config columns of earlier revisions are never touched.
func (s *Store) CreateRevision(ctx context.Context, project, profileID string, raw []byte) (Revision, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	in, err := DecodeRevisionInput(raw)
	if err != nil {
		return Revision{}, err
	}
	switch err := s.pool.QueryRow(ctx, storage.Query("AgentProfileExists"), profileID, project).Scan(new(int)); {
	case errors.Is(err, pgx.ErrNoRows):
		return Revision{}, ErrProfileNotFound
	case err != nil:
		return Revision{}, fmt.Errorf("verify agent profile: %w", err)
	}
	if err := s.verifyEnvironment(ctx, in.Environment); err != nil {
		return Revision{}, err
	}
	id := newID("arev")
	var number int
	// ponytail: revision_number is MAX+1 in-statement, so two concurrent CreateRevision on ONE profile
	// can pick the same number and one loses the UNIQUE(profile_id, revision_number) (a 23505 → 500).
	// Benign at the expected authoring cadence (a human editing a profile); add a retry-on-23505 loop
	// if concurrent revise throughput ever matters.
	if err := s.pool.QueryRow(ctx, storage.Query("InsertAgentRevision"),
		id, project, profileID, in.Model, marshalTools(in.Tools), in.Instructions,
		marshalTools(in.ToolSets), marshalTools(in.MCPConnections), marshalTools(in.Skills), marshalTools(in.Hooks),
		in.Environment).Scan(&number); err != nil {
		return Revision{}, fmt.Errorf("insert agent revision: %w", err)
	}
	return Revision{ID: id, RevisionNumber: number, Model: in.Model, Tools: in.Tools, Instructions: in.Instructions,
		ToolSets: in.ToolSets, Environment: in.Environment}, nil
}

// PublishRevision flips a draft revision to published exactly once. published is true only when THIS
// call did the flip; exists distinguishes an unknown revision (false) from one already published
// (true) — so the caller can 404 an unknown id while treating a re-publish as an idempotent success.
func (s *Store) PublishRevision(ctx context.Context, project, revisionID string) (published, exists bool, err error) {
	ctx = storage.ScopeToTenant(ctx, project)
	// THE ENVIRONMENT IS RE-CHECKED AT PUBLISH, and this is not belt-and-braces about the create check —
	// it is the check that matters. Publish is what makes a revision runnable, so it is the last moment at
	// which "this agent has the production credentials" can still be refused instead of discovered by a run
	// that quietly had none. It is also the moment that survives a future create path (the console's, a
	// CLI's, an import) forgetting to validate: publish is the single throat every revision passes through.
	var environment string
	switch e := s.pool.QueryRow(ctx, storage.Query("AgentRevisionEnvironment"), revisionID, project).Scan(&environment); {
	case errors.Is(e, pgx.ErrNoRows):
		return false, false, nil // unknown revision: the caller renders a 404, unchanged
	case e != nil:
		return false, false, fmt.Errorf("read revision environment: %w", e)
	}
	if err := s.verifyEnvironment(ctx, environment); err != nil {
		// exists=true so the caller does not report this as an unknown revision: the revision is real and
		// its environment is not.
		return false, true, err
	}
	return s.publish(ctx, "PublishAgentRevision", "AgentRevisionPublished", revisionID, project)
}

// verifyEnvironment refuses an `environment` that names no row THE CALLER CAN SEE. An EMPTY environment is
// the no-environment case and always passes — that is every revision written before environments existed,
// and the column's DEFAULT empty string is what makes it representable without a nullable FK.
//
// IT RAN UNDER storage.WithInstallationScope AND THE OVERRIDE IS NOW DELETED, which is the whole change
// here. Both callers scope ctx to the project on their first line (ScopeToTenant), so the override was
// actively WIDENING a context that already carried the right answer. Its own doc said so plainly, and was
// right for its phase: "every environment id in the installation is visible here, and this check answers
// 'does it exist' rather than 'does it exist in your tenant'". 000006 gives environments a project_id, so
// those are the same question again and the inherited scope asks it.
//
// WHAT THAT BUYS, precisely: a revision can no longer PIN another project's environment id. That was
// reachable — an operator who learned an id could publish a revision naming it — and the run would then
// have resolved that environment's keys. It is refused as absent, and absent is also what an id that does
// not exist returns, so the error discloses nothing either way.
//
// It still passes an environment whose project_id is the pre-000006 empty string, because that policy arm
// admits every scope. That is the migration's stated residue, not a hole opened here.
func (s *Store) verifyEnvironment(ctx context.Context, environment string) error {
	if environment == "" {
		return nil
	}
	switch err := s.pool.QueryRow(ctx, storage.Query("EnvironmentExists"), environment).Scan(new(int)); {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %q", ErrEnvironmentNotFound, environment)
	case err != nil:
		return fmt.Errorf("verify environment: %w", err)
	}
	return nil
}

// GetRevision reads a revision's committed shape, or found=false when it is absent from the scope.
func (s *Store) GetRevision(ctx context.Context, project, revisionID string) (Revision, bool, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	var (
		rev       Revision
		toolsJSON []byte
		published *any
	)
	rev.ID = revisionID
	err := s.pool.QueryRow(ctx, storage.Query("GetAgentRevision"), revisionID, project).
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
func (s *Store) CreateTemplateRevision(ctx context.Context, project, templateName string, raw []byte) (Revision, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	in, err := DecodeRevisionInput(raw)
	if err != nil {
		return Revision{}, err
	}
	id := newID("rtr")
	var number int
	if err := s.pool.QueryRow(ctx, storage.Query("InsertRunTemplateRevision"),
		id, project, templateName, in.Model, marshalTools(in.Tools), in.Instructions,
		marshalTools(in.ToolSets), marshalTools(in.MCPConnections), marshalTools(in.Skills), marshalTools(in.Hooks)).Scan(&number); err != nil {
		return Revision{}, fmt.Errorf("insert run template revision: %w", err)
	}
	return Revision{ID: id, RevisionNumber: number, Model: in.Model, Tools: in.Tools, Instructions: in.Instructions, ToolSets: in.ToolSets}, nil
}

// PublishTemplateRevision flips a draft template revision to published exactly once (see PublishRevision).
func (s *Store) PublishTemplateRevision(ctx context.Context, project, revisionID string) (published, exists bool, err error) {
	ctx = storage.ScopeToTenant(ctx, project)
	return s.publish(ctx, "PublishRunTemplateRevision", "RunTemplateRevisionPublished", revisionID, project)
}

// publish is the shared once-only flip: try the conditional UPDATE, and on no flip disambiguate an
// unknown revision from an already-published one via the publish-state read (both agent and template).
func (s *Store) publish(ctx context.Context, flipQuery, stateQuery, revisionID, project string) (published, exists bool, err error) {
	switch e := s.pool.QueryRow(ctx, storage.Query(flipQuery), revisionID, project).Scan(new(string)); {
	case e == nil:
		return true, true, nil
	case !errors.Is(e, pgx.ErrNoRows):
		return false, false, fmt.Errorf("publish revision: %w", e)
	}
	// No flip: the revision is unknown or already published. The state read tells them apart.
	switch e := s.pool.QueryRow(ctx, storage.Query(stateQuery), revisionID, project).Scan(new(bool)); {
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
