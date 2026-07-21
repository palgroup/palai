// Package automation is the control-plane domain logic for the E11 automation layer. Task 1 opens it
// with the agent surface: AgentProfile lineages, IMMUTABLE publishable AgentRevisions, and profile-free
// RunTemplateRevisions (spec §10, §32.2). A revise always creates a NEW draft revision — nothing here
// ever rewrites a revision's config columns, so a published revision is immutable by discipline; publish
// is the one legitimate mutation (a once-only conditional flip). Resolution of a run's pinned revision
// into its ExecutionSpec lives on the coordinator spine (execution reads it there); this package owns the
// management writes and reads.
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

// ErrUnknownField is returned when a revision body carries a field outside the enforced executable-config
// subset — an E12 field (mcp/skills/hooks/knowledge) or, for a template, an identity/delegation field.
// Dead or unsupported config is rejected, never silently stored (honest naming, spec §2 E11 include list).
var ErrUnknownField = errors.New("automation: revision body carries an unsupported field")

// ErrProfileNotFound is returned when a revision is created against a profile absent from the scope.
var ErrProfileNotFound = errors.New("automation: agent profile not found in scope")

// Store is the automation management store over the durable spine's pool.
type Store struct{ pool *pgxpool.Pool }

// New wraps a pgx pool as the automation store.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// RevisionInput is the enforced executable-config subset a revision (agent or template) carries in this
// slice (spec §10, §2). Model "" inherits the deployment default; Tools nil imposes no capability
// ceiling (a non-nil set — even empty — is the ceiling the resolver intersects). Any field outside this
// struct is rejected by DecodeRevisionInput.
type RevisionInput struct {
	Model        string   `json:"model"`
	Tools        []string `json:"tools"`
	Instructions string   `json:"instructions"`
}

// Revision is a stored revision's committed shape (management GET + the immutability check).
type Revision struct {
	ID             string
	RevisionNumber int
	Model          string
	Tools          []string
	Instructions   string
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
	if err := s.pool.QueryRow(ctx, storage.Query("InsertAgentRevision"),
		id, org, project, profileID, in.Model, marshalTools(in.Tools), in.Instructions).Scan(&number); err != nil {
		return Revision{}, fmt.Errorf("insert agent revision: %w", err)
	}
	return Revision{ID: id, RevisionNumber: number, Model: in.Model, Tools: in.Tools, Instructions: in.Instructions}, nil
}

// PublishRevision flips a draft revision to published exactly once. It reports whether this call did the
// publish (false = unknown revision or already published), so publish stays idempotent and irreversible.
func (s *Store) PublishRevision(ctx context.Context, org, project, revisionID string) (bool, error) {
	switch err := s.pool.QueryRow(ctx, storage.Query("PublishAgentRevision"), revisionID, org, project).Scan(new(string)); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("publish agent revision: %w", err)
	}
	return true, nil
}

// GetRevision reads a revision's committed shape, or found=false when it is absent from the scope.
func (s *Store) GetRevision(ctx context.Context, org, project, revisionID string) (Revision, bool, error) {
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
	in, err := DecodeRevisionInput(raw)
	if err != nil {
		return Revision{}, err
	}
	id := newID("rtr")
	var number int
	if err := s.pool.QueryRow(ctx, storage.Query("InsertRunTemplateRevision"),
		id, org, project, templateName, in.Model, marshalTools(in.Tools), in.Instructions).Scan(&number); err != nil {
		return Revision{}, fmt.Errorf("insert run template revision: %w", err)
	}
	return Revision{ID: id, RevisionNumber: number, Model: in.Model, Tools: in.Tools, Instructions: in.Instructions}, nil
}

// PublishTemplateRevision flips a draft template revision to published exactly once (see PublishRevision).
func (s *Store) PublishTemplateRevision(ctx context.Context, org, project, revisionID string) (bool, error) {
	switch err := s.pool.QueryRow(ctx, storage.Query("PublishRunTemplateRevision"), revisionID, org, project).Scan(new(string)); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("publish run template revision: %w", err)
	}
	return true, nil
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
