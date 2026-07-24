package palai

import (
	"encoding/json"
	"reflect"
	"strings"
)

// The typed models below are LOSSLESS forward-compatible wrappers (spec API-009,
// ADR-0002): a struct decode keeps its typed fields for ergonomics AND preserves
// every field this SDK version does not know, so a decode→encode round-trip never
// silently drops an unknown field a newer server added. This is the deliberate
// "struct-based decoder that a naive impl would break" the shared conformance corpus
// (tests/conformance/sdk) exists to catch: a plain `json.Unmarshal` into a fixed
// struct strips unknowns, and the corpus fails that. The forward* helpers are the
// reusable core; each model is a struct + an Extra catch-all delegating to them.

// forwardUnmarshal decodes data into the typed struct pointed to by typed AND captures every
// field the struct does not name into extra. A field the typed struct owns is decoded normally;
// everything else survives verbatim in extra as raw JSON, so the round-trip is lossless.
func forwardUnmarshal(data []byte, typed any, extra *map[string]json.RawMessage) error {
	if err := json.Unmarshal(data, typed); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	for _, name := range jsonFieldNames(typed) {
		delete(all, name)
	}
	if len(all) > 0 {
		*extra = all
	}
	return nil
}

// forwardMarshal serializes the typed struct and merges the preserved unknown fields back in.
// Extra keys are disjoint from the typed keys by construction (forwardUnmarshal removed them), so
// the merge never collides.
func forwardMarshal(typed any, extra map[string]json.RawMessage) ([]byte, error) {
	b, err := json.Marshal(typed)
	if err != nil {
		return nil, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(b, &merged); err != nil {
		return nil, err
	}
	for k, v := range extra {
		merged[k] = v
	}
	return json.Marshal(merged)
}

// jsonFieldNames returns the wire names of every json-tagged field on a struct (pointer), so the
// forward-compat split knows which keys are "known". A field tagged `json:"-"` (the Extra catch-all)
// is intentionally excluded so it never counts as a wire key.
//
// ponytail: this returns the exact tag name (e.g. "id"), but encoding/json matches field names
// CASE-INSENSITIVELY on decode — so an input key "Id" fills the typed ID field AND, because "Id" !=
// "id", stays in Extra, and a round-trip emits both "Id" and "id". TS would instead leave the typed
// field unset and keep only "Id". A cross-language divergence the corpus doesn't cover (its keys are
// canonical snake_case), inherent to encoding/json v1. Not worth a strict case-exact decoder now;
// revisit only if a real server ever emits mixed-case keys.
//
// ponytail: a known field that is BOTH omitempty AND zero in the input is dropped on round-trip
// (its key lands in neither the typed marshal nor Extra). No shipped payload carries an
// information-bearing zero for a known field, and a reader never depends on one; upgrade to a
// present-keys diff only if a caller measurably needs a zero-valued known field preserved.
func jsonFieldNames(structPtr any) []string {
	t := reflect.TypeOf(structPtr)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	names := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			name = t.Field(i).Name
		}
		names = append(names, name)
	}
	return names
}

// --- Response --------------------------------------------------------------------------------

// Response is a run's canonical result (spec §22). Known fields are typed for ergonomics; any
// field a newer server adds is preserved in Extra and re-emitted on marshal.
type Response struct {
	ID        string                     `json:"id"`
	Object    string                     `json:"object"`
	Status    string                     `json:"status"`
	Model     string                     `json:"model,omitempty"`
	CreatedAt string                     `json:"created_at,omitempty"`
	UpdatedAt string                     `json:"updated_at,omitempty"`
	SessionID string                     `json:"session_id,omitempty"`
	RunID     string                     `json:"run_id,omitempty"`
	Output    []ContentItem              `json:"output,omitempty"`
	Usage     *Usage                     `json:"usage,omitempty"`
	Error     json.RawMessage            `json:"error,omitempty"`
	Metadata  map[string]any             `json:"metadata,omitempty"`
	Extra     map[string]json.RawMessage `json:"-"`
}

func (r *Response) UnmarshalJSON(b []byte) error {
	type alias Response
	var a alias
	if err := forwardUnmarshal(b, &a, &a.Extra); err != nil {
		return err
	}
	*r = Response(a)
	return nil
}

func (r Response) MarshalJSON() ([]byte, error) {
	type alias Response
	return forwardMarshal(alias(r), r.Extra)
}

// Usage is the token/cost accounting attached to a Response (spec §22.5).
type Usage struct {
	InputTokens  int            `json:"input_tokens"`
	OutputTokens int            `json:"output_tokens"`
	TotalTokens  int            `json:"total_tokens,omitempty"`
	ToolCalls    int            `json:"tool_calls,omitempty"`
	Cost         map[string]any `json:"cost,omitempty"`
}

// ContentItem is an open union (matches the server's contracts.ContentItem): unknown fields and
// unknown `type` values survive a JSON round-trip. A plain map is lossless by construction.
type ContentItem map[string]any

// Type returns the content item's discriminator.
func (c ContentItem) Type() string {
	v, _ := c["type"].(string)
	return v
}

// --- Event -----------------------------------------------------------------------------------

// Event is one CloudEvents-shaped run event (spec §24). Unknown event types are delivered, not
// dropped (API-009): Type is a plain string and unknown fields ride along in Extra.
type Event struct {
	Specversion string                     `json:"specversion"`
	ID          string                     `json:"id"`
	Source      string                     `json:"source"`
	Type        string                     `json:"type"`
	Time        string                     `json:"time"`
	Sequence    int                        `json:"sequence"`
	Data        map[string]any             `json:"data"`
	SessionID   string                     `json:"session_id,omitempty"`
	RunID       string                     `json:"run_id,omitempty"`
	AttemptID   string                     `json:"attempt_id,omitempty"`
	Subject     string                     `json:"subject,omitempty"`
	Extra       map[string]json.RawMessage `json:"-"`
}

func (e *Event) UnmarshalJSON(b []byte) error {
	type alias Event
	var a alias
	if err := forwardUnmarshal(b, &a, &a.Extra); err != nil {
		return err
	}
	*e = Event(a)
	return nil
}

func (e Event) MarshalJSON() ([]byte, error) {
	type alias Event
	return forwardMarshal(alias(e), e.Extra)
}

// --- Model routing (E16 T1 read-back) --------------------------------------------------------

// ModelConnection binds a provider family to a secret REF (never a value). It is a lossless
// wrapper so a newer server's added fields survive.
type ModelConnection struct {
	ID             string                     `json:"id"`
	Object         string                     `json:"object"`
	Provider       string                     `json:"provider"`
	SecretRef      string                     `json:"secret_ref"`
	ProjectID      string                     `json:"project_id,omitempty"`
	OrganizationID string                     `json:"organization_id,omitempty"`
	CreatedAt      string                     `json:"created_at,omitempty"`
	Extra          map[string]json.RawMessage `json:"-"`
}

func (m *ModelConnection) UnmarshalJSON(b []byte) error {
	type alias ModelConnection
	var a alias
	if err := forwardUnmarshal(b, &a, &a.Extra); err != nil {
		return err
	}
	*m = ModelConnection(a)
	return nil
}

func (m ModelConnection) MarshalJSON() ([]byte, error) {
	type alias ModelConnection
	return forwardMarshal(alias(m), m.Extra)
}

// ModelRoute is a named route alias for the project.
type ModelRoute struct {
	ID        string                     `json:"id"`
	Object    string                     `json:"object"`
	Name      string                     `json:"name"`
	ProjectID string                     `json:"project_id,omitempty"`
	CreatedAt string                     `json:"created_at,omitempty"`
	Extra     map[string]json.RawMessage `json:"-"`
}

func (m *ModelRoute) UnmarshalJSON(b []byte) error {
	type alias ModelRoute
	var a alias
	if err := forwardUnmarshal(b, &a, &a.Extra); err != nil {
		return err
	}
	*m = ModelRoute(a)
	return nil
}

func (m ModelRoute) MarshalJSON() ([]byte, error) {
	type alias ModelRoute
	return forwardMarshal(alias(m), m.Extra)
}

// ModelRouteRevision is one revision of a route, carrying its derived `published` flag.
type ModelRouteRevision struct {
	ID           string                     `json:"id"`
	Object       string                     `json:"object"`
	RouteID      string                     `json:"route_id"`
	Model        string                     `json:"model,omitempty"`
	ConnectionID string                     `json:"connection_id,omitempty"`
	Published    bool                       `json:"published"`
	Revision     int                        `json:"revision,omitempty"`
	CreatedAt    string                     `json:"created_at,omitempty"`
	Extra        map[string]json.RawMessage `json:"-"`
}

func (m *ModelRouteRevision) UnmarshalJSON(b []byte) error {
	type alias ModelRouteRevision
	var a alias
	if err := forwardUnmarshal(b, &a, &a.Extra); err != nil {
		return err
	}
	*m = ModelRouteRevision(a)
	return nil
}

func (m ModelRouteRevision) MarshalJSON() ([]byte, error) {
	type alias ModelRouteRevision
	return forwardMarshal(alias(m), m.Extra)
}

// --- List envelopes (the T1 Page/ListView distinction) ---------------------------------------

// Page is the cursor-paginated data-plane envelope (contracts.Page): a slice plus the opaque,
// server-minted cursor the SDK passes straight back as the next `after` — it never parses it.
type Page[T any] struct {
	Data           []T     `json:"data"`
	HasMore        bool    `json:"has_more"`
	NextCursor     *string `json:"next_cursor,omitempty"`
	PreviousCursor *string `json:"previous_cursor,omitempty"`
}

// ListView is the un-paginated `{object:"list", data:[...]}` admin envelope (a full, small,
// tenant-scoped set — no cursor). The model-routing read-back returns this family.
type ListView[T any] struct {
	Object string `json:"object"`
	Data   []T    `json:"data"`
}

// ResponseCreateRequest is the create body (spec §22.1). Mirrors the server's
// contracts.ResponseCreateRequest; Input is required, everything else omitempty. Unknown-forward is
// not needed on a request the SDK constructs.
type ResponseCreateRequest struct {
	Input              any              `json:"input"`
	Model              string           `json:"model,omitempty"`
	Instructions       string           `json:"instructions,omitempty"`
	Metadata           map[string]any   `json:"metadata,omitempty"`
	MaxOutputTokens    int              `json:"max_output_tokens,omitempty"`
	MaxToolCalls       int              `json:"max_tool_calls,omitempty"`
	ToolChoice         string           `json:"tool_choice,omitempty"`
	Tools              []map[string]any `json:"tools,omitempty"`
	ToolSets           []string         `json:"tool_sets,omitempty"`
	Skills             []string         `json:"skills,omitempty"`
	Capabilities       []string         `json:"capabilities,omitempty"`
	SessionID          *string          `json:"session_id,omitempty"`
	PreviousResponseID *string          `json:"previous_response_id,omitempty"`
	Engine             *string          `json:"engine,omitempty"`
	Background         bool             `json:"background,omitempty"`
	Store              bool             `json:"store,omitempty"`
	Stream             bool             `json:"stream,omitempty"`
	ParallelToolCalls  bool             `json:"parallel_tool_calls,omitempty"`
	Budget             map[string]any   `json:"budget,omitempty"`
	Callback           map[string]any   `json:"callback,omitempty"`
	Context            map[string]any   `json:"context,omitempty"`
	Output             map[string]any   `json:"output,omitempty"`
}

// ListParams are the shared pagination + basic filters the whole read/LIST surface accepts. Camel
// at the edge, snake on the wire (created_after/created_before). `status` is honored only on the
// responses/sessions lists; it is passed through, never faked.
type ListParams struct {
	After         string
	Limit         int
	Status        string
	CreatedAfter  string
	CreatedBefore string
}
