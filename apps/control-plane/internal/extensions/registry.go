// Package extensions is the control-plane domain logic for the E12 extensibility registry. Task 2 opens
// it with the tool surface: durable Tool lineages, IMMUTABLE publishable ToolRevisions (one executor
// binding each), and named publishable ToolSetRevisions that pin exact published revisions (spec
// §28.2-28.4). It mirrors internal/automation/agents.go beat-for-beat: a revise always creates a NEW
// revision — nothing here rewrites a revision's config columns, so a published revision is immutable by
// discipline; publish is the one legitimate mutation (a once-only conditional flip). Resolution of a
// run's pinned tool set into a broker-loadable tool lives on the coordinator spine + the broker lookup;
// this package owns the management writes and reads.
//
// ponytail: the contract registers tool.revision.published.v1 + tool_set.revision.published.v1, but NO
// code emits them — publication's durable fact IS the published_at flip on the row (queryable,
// immutable-once-set). A project-scoped management action has no session journal to ride, so the events
// are declared-but-unemitted by design (the automation/agents.go precedent); add an emitter here if a
// downstream consumer ever needs the event.
package extensions

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

var (
	// ErrUnknownField is returned when a tool/set revision body carries a field outside the enforced
	// config subset (json.DisallowUnknownFields). Dead or unsupported config — including an inline
	// credential — is rejected, never silently stored (honest naming, spec §28.4).
	ErrUnknownField = errors.New("extensions: revision body carries an unsupported field")
	// ErrInvalidCanonicalName is returned when a canonical name is not exactly three non-empty ASCII
	// segments within the length bound (publisher.namespace.tool, spec §28.2).
	ErrInvalidCanonicalName = errors.New("extensions: canonical name must be publisher.namespace.tool (3 ASCII segments)")
	// ErrModelNameReserved is returned when a tool's deterministic model-visible short name collides with
	// a code-defined built-in (file/shell/commit/…) — a registered tool must never shadow a built-in.
	ErrModelNameReserved = errors.New("extensions: model-visible name collides with a built-in tool")
	// ErrNameCollision is returned when a canonical name or its deterministic model-visible short name is
	// already taken in the project (collision is a create REJECT, never an auto-suffix, spec §28.2).
	ErrNameCollision = errors.New("extensions: tool name already exists in this project")
	// ErrToolNotFound is returned when a revision is created against a tool absent from the scope.
	ErrToolNotFound = errors.New("extensions: tool not found in scope")
	// ErrUnknownToolRevision is returned when a set pins a tool revision absent from the scope.
	ErrUnknownToolRevision = errors.New("extensions: pinned tool revision not found in scope")
	// ErrRevisionNotPublished is returned when a set pins a DRAFT tool revision (only published revisions
	// may be pinned, spec §28.4).
	ErrRevisionNotPublished = errors.New("extensions: pinned tool revision is not published")
	// ErrOverrideNotStricter is returned when a set-pin override widens a declared limit rather than
	// tightening it (approval-only-stricter, spec §28.4).
	ErrOverrideNotStricter = errors.New("extensions: pin override may only tighten a declared limit")
)

// maxSegmentLen bounds each canonical-name segment (an app-level length ceiling, spec §28.2).
const maxSegmentLen = 128

// Store is the extensibility management store over the durable spine's pool. reserved holds the
// code-defined built-in model-visible names a registered tool must not shadow (injected from the flat
// broker registration list at composition — the registry owns no built-in knowledge itself).
type Store struct {
	pool     *pgxpool.Pool
	reserved map[string]bool
}

// New wraps a pgx pool as the extensions store, reserving the given built-in model-visible short names.
func New(pool *pgxpool.Pool, reserved ...string) *Store {
	set := make(map[string]bool, len(reserved))
	for _, name := range reserved {
		set[name] = true
	}
	return &Store{pool: pool, reserved: set}
}

// Tool is a created tool lineage's committed shape.
type Tool struct {
	ID               string
	CanonicalName    string
	ModelVisibleName string
}

// ToolRevisionInput is the enforced executor-config subset a tool revision carries (spec §28.4). Any
// field outside this struct — including a raw credential — is rejected by DecodeToolRevisionInput; a
// secret is a SecretRef HANDLE only. Executor names the binding kind (control_plane|remote_http|mcp);
// only control_plane has a binder in T2.
type ToolRevisionInput struct {
	Executor       string         `json:"executor"`
	Description    string         `json:"description"`
	InputSchema    map[string]any `json:"input_schema"`
	OutputSchema   map[string]any `json:"output_schema"`
	ReplayClass    string         `json:"replay_class"`
	TimeoutMS      *int           `json:"timeout_ms"`
	Limits         map[string]any `json:"limits"`
	ExecutorConfig map[string]any `json:"executor_config"`
	SecretRef      string         `json:"secret_ref"`
}

// ToolRevision is a stored tool revision's committed shape (management + the immutability check).
type ToolRevision struct {
	ID             string
	RevisionNumber int
	Executor       string
	Digest         string
	Published      bool
}

// ToolPinInput is one pin in a set revision: an exact published tool revision plus an optional
// only-tightening override.
type ToolPinInput struct {
	ToolRevisionID string         `json:"tool_revision_id"`
	Overrides      map[string]any `json:"overrides"`
}

// ToolSetRevisionInput is the strict-decoded set-revision body: the exact pins the set names.
type ToolSetRevisionInput struct {
	Tools []ToolPinInput `json:"tools"`
}

// ToolSetRevision is a stored set revision's committed shape.
type ToolSetRevision struct {
	ID             string
	RevisionNumber int
	Digest         string
	Published      bool
}

// DecodeToolRevisionInput strictly decodes the executor-config subset, REJECTING any unknown field via
// json.DisallowUnknownFields (the stdlib guard is enough — ponytail). A raw credential field therefore
// never decodes, so a secret can only enter as a secret_ref handle.
func DecodeToolRevisionInput(raw []byte) (ToolRevisionInput, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in ToolRevisionInput
	if err := dec.Decode(&in); err != nil {
		return ToolRevisionInput{}, fmt.Errorf("%w: %v", ErrUnknownField, err)
	}
	return in, nil
}

// CreateTool inserts a named tool lineage, deriving the deterministic model-visible short name from the
// canonical name's last segment. A malformed canonical name, a built-in collision, or a duplicate name
// in the project is a typed reject BEFORE any partial write.
func (s *Store) CreateTool(ctx context.Context, org, project, canonicalName string) (Tool, error) {
	short, err := validateCanonicalName(canonicalName)
	if err != nil {
		return Tool{}, err
	}
	if s.reserved[short] {
		return Tool{}, ErrModelNameReserved
	}
	id := newID("tool")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertTool"), id, org, project, canonicalName, short); err != nil {
		if isUniqueViolation(err) {
			return Tool{}, ErrNameCollision
		}
		return Tool{}, fmt.Errorf("insert tool: %w", err)
	}
	return Tool{ID: id, CanonicalName: canonicalName, ModelVisibleName: short}, nil
}

// CreateToolRevision inserts a DRAFT revision under a tool from a raw body (strictly decoded). It verifies
// the tool is in scope first, then digest-addresses the config. A revise is just another CreateToolRevision
// — earlier revisions' config columns are never touched.
func (s *Store) CreateToolRevision(ctx context.Context, org, project, toolID string, raw []byte) (ToolRevision, error) {
	in, err := DecodeToolRevisionInput(raw)
	if err != nil {
		return ToolRevision{}, err
	}
	switch err := s.pool.QueryRow(ctx, storage.Query("ToolExists"), toolID, org, project).Scan(new(int)); {
	case errors.Is(err, pgx.ErrNoRows):
		return ToolRevision{}, ErrToolNotFound
	case err != nil:
		return ToolRevision{}, fmt.Errorf("verify tool: %w", err)
	}
	digest := revisionDigest(in)
	id := newID("trev")
	var number int
	if err := s.pool.QueryRow(ctx, storage.Query("InsertToolRevision"),
		id, org, project, toolID, in.Executor, in.Description, marshalJSON(in.InputSchema), marshalJSONOrNil(in.OutputSchema),
		replayClassOrDefault(in.ReplayClass), in.TimeoutMS, marshalJSONOrNil(in.Limits), marshalJSONOrNil(in.ExecutorConfig),
		nullableText(in.SecretRef), digest).Scan(&number); err != nil {
		return ToolRevision{}, fmt.Errorf("insert tool revision: %w", err)
	}
	return ToolRevision{ID: id, RevisionNumber: number, Executor: in.Executor, Digest: digest}, nil
}

// PublishToolRevision flips a draft revision to published exactly once (see automation.PublishRevision).
func (s *Store) PublishToolRevision(ctx context.Context, org, project, revisionID string) (published, exists bool, err error) {
	return s.publish(ctx, "PublishToolRevision", "ToolRevisionPublished", revisionID, org, project)
}

// GetToolRevision reads a revision's committed shape (management + the immutability check).
func (s *Store) GetToolRevision(ctx context.Context, org, project, revisionID string) (ToolRevision, bool, error) {
	var (
		rev       ToolRevision
		published *any
	)
	rev.ID = revisionID
	err := s.pool.QueryRow(ctx, storage.Query("GetToolRevision"), revisionID, org, project).
		Scan(new(string), &rev.RevisionNumber, &rev.Executor, &rev.Digest, &published)
	if errors.Is(err, pgx.ErrNoRows) {
		return ToolRevision{}, false, nil
	}
	if err != nil {
		return ToolRevision{}, false, fmt.Errorf("read tool revision: %w", err)
	}
	rev.Published = published != nil
	return rev, true, nil
}

// CreateToolSetRevision inserts a DRAFT set revision naming exact pins. Each pin must reference a PUBLISHED
// tool revision in scope (an unknown or draft revision is a typed reject), and any per-pin override may
// only tighten a declared limit — so the whole set is validated BEFORE it is written.
func (s *Store) CreateToolSetRevision(ctx context.Context, org, project, setName string, raw []byte) (ToolSetRevision, error) {
	in, err := decodeToolSetRevisionInput(raw)
	if err != nil {
		return ToolSetRevision{}, err
	}
	for _, pin := range in.Tools {
		published, timeoutMS, err := s.pinTarget(ctx, org, project, pin.ToolRevisionID)
		if err != nil {
			return ToolSetRevision{}, err
		}
		if !published {
			return ToolSetRevision{}, ErrRevisionNotPublished
		}
		if err := checkOverrideStricter(pin.Overrides, timeoutMS); err != nil {
			return ToolSetRevision{}, err
		}
	}
	digest := setDigest(in)
	id := newID("tsrev")
	var number int
	if err := s.pool.QueryRow(ctx, storage.Query("InsertToolSetRevision"),
		id, org, project, setName, marshalJSON(in.Tools), digest).Scan(&number); err != nil {
		return ToolSetRevision{}, fmt.Errorf("insert tool set revision: %w", err)
	}
	return ToolSetRevision{ID: id, RevisionNumber: number, Digest: digest}, nil
}

// PublishToolSetRevision flips a draft set revision to published exactly once.
func (s *Store) PublishToolSetRevision(ctx context.Context, org, project, revisionID string) (published, exists bool, err error) {
	return s.publish(ctx, "PublishToolSetRevision", "ToolSetRevisionPublished", revisionID, org, project)
}

// pinTarget reads a pinned tool revision's publish state + declared timeout, distinguishing an unknown
// revision (ErrUnknownToolRevision) from a known one.
func (s *Store) pinTarget(ctx context.Context, org, project, revisionID string) (published bool, timeoutMS *int, err error) {
	err = s.pool.QueryRow(ctx, storage.Query("ToolRevisionForPin"), revisionID, org, project).Scan(&published, &timeoutMS)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, ErrUnknownToolRevision
	}
	if err != nil {
		return false, nil, fmt.Errorf("read pinned tool revision: %w", err)
	}
	return published, timeoutMS, nil
}

// publish is the shared once-only flip (the automation.Store.publish twin): try the conditional UPDATE,
// and on no flip disambiguate an unknown revision from an already-published one via the state read.
func (s *Store) publish(ctx context.Context, flipQuery, stateQuery, revisionID, org, project string) (published, exists bool, err error) {
	switch e := s.pool.QueryRow(ctx, storage.Query(flipQuery), revisionID, org, project).Scan(new(string)); {
	case e == nil:
		return true, true, nil
	case !errors.Is(e, pgx.ErrNoRows):
		return false, false, fmt.Errorf("publish revision: %w", e)
	}
	switch e := s.pool.QueryRow(ctx, storage.Query(stateQuery), revisionID, org, project).Scan(new(bool)); {
	case errors.Is(e, pgx.ErrNoRows):
		return false, false, nil
	case e != nil:
		return false, false, fmt.Errorf("read revision publish state: %w", e)
	}
	return false, true, nil
}

// decodeToolSetRevisionInput strictly decodes the set-revision body, rejecting unknown fields.
func decodeToolSetRevisionInput(raw []byte) (ToolSetRevisionInput, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in ToolSetRevisionInput
	if err := dec.Decode(&in); err != nil {
		return ToolSetRevisionInput{}, fmt.Errorf("%w: %v", ErrUnknownField, err)
	}
	return in, nil
}

// validateCanonicalName enforces the publisher.namespace.tool shape (exactly three non-empty ASCII
// segments, each within the length bound) and returns the deterministic model-visible short name (the
// last segment). A malformed name is ErrInvalidCanonicalName.
func validateCanonicalName(canonical string) (string, error) {
	segments := splitDots(canonical)
	if len(segments) != 3 {
		return "", fmt.Errorf("%w: got %d segments", ErrInvalidCanonicalName, len(segments))
	}
	for _, seg := range segments {
		if seg == "" || len(seg) > maxSegmentLen || !isASCIIName(seg) {
			return "", fmt.Errorf("%w: bad segment %q", ErrInvalidCanonicalName, seg)
		}
	}
	return segments[2], nil
}

// splitDots splits on '.' without allocating a regexp (a canonical name is a short dotted string).
func splitDots(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// isASCIIName reports whether every byte is a printable ASCII name character (letters, digits, _ or -).
func isASCIIName(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// checkOverrideStricter rejects a pin override that widens a declared limit. Today only timeout_ms is a
// declared limit: an override above the declared ceiling is rejected; equal/below — or bounding a
// previously-unbounded (nil) declaration — is stricter and accepted.
func checkOverrideStricter(overrides map[string]any, declaredTimeoutMS *int) error {
	raw, ok := overrides["timeout_ms"]
	if !ok {
		return nil
	}
	override, ok := numberAsInt(raw)
	if !ok {
		return fmt.Errorf("%w: timeout_ms override is not a number", ErrOverrideNotStricter)
	}
	if declaredTimeoutMS != nil && override > *declaredTimeoutMS {
		return fmt.Errorf("%w: timeout_ms %d > declared %d", ErrOverrideNotStricter, override, *declaredTimeoutMS)
	}
	return nil
}

// numberAsInt reads a JSON number (float64) as an int; also accepts an int for programmatic callers.
func numberAsInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

// revisionDigest is the canonical content address of a tool revision's config (the config.go pattern):
// sha256 over the sorted-key JSON of the enforced subset. secret_ref is a handle, so hashing it is safe.
func revisionDigest(in ToolRevisionInput) string {
	canonical, _ := json.Marshal(struct {
		Executor       string         `json:"executor"`
		Description    string         `json:"description"`
		InputSchema    map[string]any `json:"input_schema"`
		OutputSchema   map[string]any `json:"output_schema"`
		ReplayClass    string         `json:"replay_class"`
		TimeoutMS      *int           `json:"timeout_ms"`
		Limits         map[string]any `json:"limits"`
		ExecutorConfig map[string]any `json:"executor_config"`
		SecretRef      string         `json:"secret_ref"`
	}{in.Executor, in.Description, in.InputSchema, in.OutputSchema, replayClassOrDefault(in.ReplayClass), in.TimeoutMS, in.Limits, in.ExecutorConfig, in.SecretRef})
	return "sha256:" + hex.EncodeToString(sha256Sum(canonical))
}

// setDigest is the canonical content address of a set revision's pins.
func setDigest(in ToolSetRevisionInput) string {
	canonical, _ := json.Marshal(in.Tools)
	return "sha256:" + hex.EncodeToString(sha256Sum(canonical))
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// replayClassOrDefault defaults an unset replay class to 'pure' (the broker's safe default).
func replayClassOrDefault(class string) string {
	if class == "" {
		return "pure"
	}
	return class
}

// marshalJSON renders a value to JSONB bytes (a non-nil map always stores an object).
func marshalJSON(v any) []byte {
	out, _ := json.Marshal(v)
	return out
}

// marshalJSONOrNil keeps a nil map/value as SQL NULL rather than a JSON "null".
func marshalJSONOrNil(v map[string]any) any {
	if v == nil {
		return nil
	}
	out, _ := json.Marshal(v)
	return out
}

// nullableText maps an empty string to SQL NULL (a secret ref that was never set stays NULL).
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// isUniqueViolation reports whether err is a PostgreSQL unique_violation (SQLSTATE 23505), so a name
// collision surfaces as a typed reject rather than an opaque 500.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// newID mints an opaque, globally unique id with the given prefix (the automation.newID twin).
func newID(prefix string) string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}
