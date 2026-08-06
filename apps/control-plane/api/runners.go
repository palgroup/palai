package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// The runner registry surface: the READ half (E24 T1), the pool-key half (E24 T3) and the LIFECYCLE half
// (E24 T5). T1 wrote that there would never be a write route here and gave the right reason for what it
// shipped — reading a fleet cannot move a fleet — and T5 is why there is one now: `Revoke()` was
// implemented, tested and catalogued as SAN-011's hard stop, and NOTHING CALLED IT (§3.6 D15). An operator
// with no way to say "decommission that Mac" has no hard stop.
//
// WHAT THIS ANSWERS, and it is the question §3.6 D13 says nobody could ask: which machines have
// enrolled, under which pool, and when each was last seen. Before the registry, the only observable
// runner fact outside the runner process was /healthz/runner's LAST certificate — one slot, last
// writer wins — so with two runners up an operator could see one of them and could not tell which.
//
// HONEST CEILING, restated where an operator meets it: last_seen_at records the last time the machine
// AUTHENTICATED (enrol, connect, renew). Nothing polls and nothing expires a row, so a stale stamp
// means "has not authenticated since", NOT "is down". The item deliberately carries no `healthy`
// field, because there is nothing behind one.

// RunnerItem is one enrolled machine's read projection. It carries the public halves of an identity —
// a certificate's DNS name and expiry, a public key's fingerprint — and no credential: the private key
// never leaves the runner and the pool key that admitted it is not named here (that is T3's).
type RunnerItem struct {
	ID              string
	PoolID          string
	Label           string
	DNS             string
	PublicKeySHA256 string
	State           string
	OS              string
	Arch            string
	Posture         string
	Capacity        int
	CertNotAfter    time.Time
	EnrolledAt      time.Time
	LastSeenAt      time.Time
	CreatedAt       time.Time
	AgentVersion    string
	IsolationModes  string
	Connections     *int64
	// ActiveLeases is how many leases this machine is serving RIGHT NOW, and it is the one field here that
	// is not a stored fact — the gateway holding the sessions is the only thing that knows (E24 T5). It is
	// on the single read rather than the listing because it is a live value: a page of them would be a page
	// of separate instants presented as one.
	//
	// It exists because a cordon leaves exactly one question open: an operator who has taken a Mac out of
	// service needs to know when it is safe to take AWAY, and before this there was nothing to ask.
	//
	// A POINTER, so that "serving nothing" and "nobody asked the gateway" are different answers. Zero is a
	// real and important value here — it is the one that means the Mac can be unplugged — and a plain int
	// would render it identically to a deployment that wired no gateway at all.
	ActiveLeases *int64
	// THE MACHINE'S OWN ANSWER about its configuration (migration 000060). Together they are what lets a
	// panel say "this Mac is running what you saved" instead of "saved" — the distinction the whole desired-
	// configuration surface exists to keep, carried the last hop to the screen.
	//
	// ConfigRevision is 0 when the machine has never reported, which is the honest state for every runner
	// enrolled before the settings poll existed. ConfigApplied is its verdict per setting and is nil in the
	// same case; an EMPTY map is the different, meaningful answer that the machine polled and the plane had
	// no document for it.
	ConfigRevision   int64
	ConfigApplied    map[string]string
	ConfigReportedAt time.Time
}

// RunnerListWindow is the keyset page window the registry read takes — declared here rather than
// imported for the reason SlackListWindow is: the api package is imported BY the stores, so it cannot
// import them back.
type RunnerListWindow struct {
	CreatedGTE     *time.Time
	CreatedLTE     *time.Time
	AfterCreatedAt *time.Time
	AfterID        string
	Limit          int
}

// RunnerPoolItem is one pool's read projection (E24 T2): the posture its machines have, the shape it
// expects, and whether enrolling into it needs a human. A pool has no credential to leak — its
// enrolment key is a separate row and T3's surface — so every field here is public by construction.
type RunnerPoolItem struct {
	ID               string
	Name             string
	Posture          string
	OS               string
	Arch             string
	StrictEnrollment bool
	CreatedAt        time.Time
	// Waiting is how many attempts are queued for this pool with no machine free to take them — the number
	// `RunnerGateway.Waiting(poolID)` has counted since E24 and that nothing read until E28 T1 (`FLT-P14`).
	// It answers the one question an operator of a fleet actually asks: why is nothing running in my Mac
	// pool.
	//
	// IT IS HERE AND NOT ON /healthz/runner, which is the gap row's own placement decision: the value is per
	// POOL and pool ids are TENANT-SCOPED NAMES, so publishing a set of them on an unauthenticated endpoint
	// — where its sibling `Connected()` lives — would be an information-disclosure choice nobody made.
	//
	// A POINTER, ActiveLeases's pattern applied verbatim: "nobody could ask the gateway" and "nothing is
	// waiting" are different answers, and zero is the one that means the pool is keeping up. A deployment
	// with no runner listener bound renders no field at all rather than a confident 0.
	Waiting *int64
}

// RunnerPoolCreate is what `POST /v1/runner-pools` was asked to create (E28 T1). It is a struct rather than
// the decoded body so the store never sees a caller's JSON: the route validates the posture and the name,
// and the tenant comes from the verified scope and from nowhere else.
type RunnerPoolCreate struct {
	Name             string
	Posture          string
	OS               string
	Arch             string
	StrictEnrollment bool
}

// ErrRunnerPoolNameTaken is the runner_pools UNIQUE (project_id, name) index rendered as an answer rather
// than as a 500. It is declared HERE rather than in internal/fleet because the api package is imported BY
// the stores and cannot import them back — the same direction RunnerListWindow is declared in.
//
// The index was DECLARED as (organization_id, project_id, name) and rebuilt without the organization
// during the A.2 organization removal. The shape above is what a reader finds in pg_indexes, and that is
// where to check it rather than in any one migration — the index NAME (`runner_pools_name_key`) survives
// a chain rewrite, a migration number does not:
//
//	SELECT indexdef FROM pg_indexes WHERE indexname = 'runner_pools_name_key';
//
// The ERROR did not change meaning across that rebuild: the collision it names was always "this project
// already has a pool by that name", because a project belonged to exactly one organization — dropping the
// leading column narrowed the index's TEXT, not the set of rows it rejects.
var ErrRunnerPoolNameTaken = errors.New("api: a runner pool with that name already exists in this project")

// runnerPoolPostures is the pair migration 000045 declares in its CHECK. Validating on the ROUTE makes a
// typo a named 400 instead of a 23514 the handler could only render as "internal_error"; the CHECK stays
// the last defence rather than the first.
var runnerPoolPostures = map[string]bool{"sandboxed-linux": true, "unsandboxed-host": true}

// RunnerPoolKeyItem is one enrolment key's operator-facing projection (E24 T3). Value is set ONLY on
// the create response — the one time the value is shown — and is `omitempty` so a listing physically
// cannot carry it. Prefix is the value's first 8 characters: enough to tell two keys apart in a list,
// which is a listing's whole job, and not a credential.
type RunnerPoolKeyItem struct {
	ID         string
	PoolID     string
	Prefix     string
	Value      string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
	// EnrolledRunners is populated on a REVOKE and names the machines the key already admitted — none of
	// which is stopped. It is on the response because an operator who is not shown them reads "revoked"
	// as "removed" and believes one call decommissioned a fleet.
	EnrolledRunners []RunnerPoolKeyEnrollment
}

// RunnerPoolKeyEnrollment is one machine a revocation did NOT stop.
type RunnerPoolKeyEnrollment struct {
	ID         string
	Label      string
	DNS        string
	State      string
	PoolID     string
	EnrolledAt time.Time
}

// RunnerRegistryAPI is the registry seam; internal/fleet implements it through a thin adapter.
// Tiers that wire no registry pass nil and the routes stay unmounted — the same posture every other
// optional surface in this router takes.
//
// IT GAINED A WRITE HALF IN E24 T3, and the comment it replaces said there would never be one. That
// was true of what T1/T2 shipped — reading a fleet cannot move a fleet — and it stops being true the
// moment a fleet has a CREDENTIAL, because minting and revoking one is an operator action with no
// other home: the runner plane authenticates machines, not people. The three key routes are gated on
// the `provision` capability, the same as every other org-admin surface.
//
// AND IT GAINED A MACHINE-MOVING HALF IN E24 T5 (SetRunnerState), gated on the same capability for the
// same reason: taking a Mac out of service and decommissioning one are org administration.
// No organization parameter (A.2 Task 3): the request scope no longer resolves one, and the fleet store
// resolves it fresh from project where it still needs one internally.
type RunnerRegistryAPI interface {
	ListRunners(ctx context.Context, project string, w RunnerListWindow) ([]RunnerItem, error)
	// GetRunner reports found=false for an id that is not in the caller's tenant, so the handler
	// answers 404 without ever learning whether it exists elsewhere.
	GetRunner(ctx context.Context, project, id string) (RunnerItem, bool, error)
	// ListRunnerPools pages this tenant's pools. It is on the SAME interface as the runner reads
	// rather than an interface of its own because there is one implementation and one mount, and a
	// second seam would be an abstraction bought before anything asked for it.
	ListRunnerPools(ctx context.Context, project string, w RunnerListWindow) ([]RunnerPoolItem, error)
	// CreateRunnerPool writes ONE pool for this tenant (E28 T1) — the birth path E24 left absent, and the
	// reason this interface has it: before it, `InsertDefaultRunnerPool` was the only statement that wrote a
	// pool row and it wrote the name, the posture and the strict flag as LITERALS, so a rented-Mac pool could
	// not exist and `strict_enrollment` could not be switched on by anything outside a test file.
	//
	// It returns ErrRunnerPoolNameTaken for a name this project already uses, so a duplicate is a 409 rather
	// than a 500 — an operator who typed a name twice is not told their control plane is broken.
	CreateRunnerPool(ctx context.Context, project string, in RunnerPoolCreate) (RunnerPoolItem, error)
	// SetRunnerPoolStrictEnrollment opens or closes ONE pool's waiting room. It is the ONLY field a pool can
	// be patched through, and that is a correctness requirement rather than a limitation: a machine INHERITS
	// its pool's posture at enrolment, so moving a populated pool's posture would retroactively change what
	// the machines already in it ARE. found=false is an unknown or foreign id, rendered as the same
	// non-disclosing 404 the lifecycle verbs give.
	SetRunnerPoolStrictEnrollment(ctx context.Context, project, poolID string, strict bool) (RunnerPoolItem, bool, error)
	// MintRunnerPoolKey mints an enrolment key for one of this tenant's pools and returns its value
	// EXACTLY ONCE. found=false is a pool that is not the caller's (or does not exist), rendered 404.
	MintRunnerPoolKey(ctx context.Context, project, poolID string, expiresAt *time.Time) (RunnerPoolKeyItem, bool, error)
	// ListRunnerPoolKeys lists key metadata — never a value, never a digest.
	ListRunnerPoolKeys(ctx context.Context, project, poolID string) ([]RunnerPoolKeyItem, error)
	// RevokeRunnerPoolKey closes a key and reports the machines it already admitted, none of which is
	// stopped. Idempotent; found=false for an unknown or foreign id.
	RevokeRunnerPoolKey(ctx context.Context, project, keyID string) (RunnerPoolKeyItem, bool, error)
	// SetRunnerState cordons, resumes or revokes ONE machine (E24 T5). action is one of
	// "cordon"/"resume"/"revoke" and is bound at route registration, so an implementation never has to
	// validate a caller-supplied string. found=false is an unknown or foreign id — or a machine already
	// revoked and asked to move, since a revoke is irreversible — all rendered as the same non-disclosing
	// 404.
	//
	// IT IS THE FIRST WRITE ROUTE HERE THAT MOVES A MACHINE, which the comment above says did not exist,
	// and §3.6 D15 is why it does now: `Revoke()` was implemented, tested, catalogued as SAN-011's hard stop
	// and CALLED BY NOTHING. A security control with no operator surface is a security control that does
	// not exist.
	SetRunnerState(ctx context.Context, project, id, action string) (RunnerItem, bool, error)
	// ApproveRunner admits ONE machine out of a strict pool's waiting room (E24 T6). It takes the whole
	// verified Scope rather than org/project because the DECIDING PRINCIPAL is derived from the key id on it
	// — the ApprovalAPI posture, for the same reason: a decision carries an identity, and a caller that
	// could supply one could name somebody else. The outcome is api.ApprovalOutcome, reused rather than
	// restated, because the three facts are the same three: not found, not permitted, or applied.
	ApproveRunner(ctx context.Context, scope middleware.Scope, id string) (RunnerItem, ApprovalOutcome, error)
}

type runnerHandler struct {
	runners RunnerRegistryAPI
	// desired serves the per-pool and per-machine desired documents. Nil on a deployment with no durable
	// spine, in which case the two read routes are not registered at all — an absent route is a better
	// answer than one that always reports "no document", which would be indistinguishable from a
	// deployment nobody had configured.
	desired DesiredConfigAPI
	// occupancies serves ONE machine's session history. Nil on a deployment with no durable spine, in
	// which case the route is not registered — an absent route is a better answer than one that always
	// reports an empty history, which an operator would read as "this Mac has never held a session".
	occupancies MachineOccupancyAPI
}

// MachineOccupancyAPI is the read behind the machine-detail view: what this machine is holding now and
// what it held before, newest first (device plan T6, DoD 10).
//
// THE CURSOR IS THE PAIR, not the timestamp. Two holds can begin in the same clock tick, and a page
// ordered on time alone can drop or repeat a row between requests — this tree has had an unordered LIMIT
// decide a security outcome twice.
type MachineOccupancyAPI interface {
	MachineOccupancies(ctx context.Context, project, runnerID string, before time.Time, beforeID string, limit int) ([]MachineOccupancyItem, error)
}

// MachineOccupancyItem is one hold as the panel reads it.
type MachineOccupancyItem struct {
	ID             string
	SessionID      string
	StartedAt      time.Time
	LastActivityAt time.Time
	ReleasedAt     time.Time
	ReleaseReason  string
	BilledSeconds  float64
}

// machineOccupancyView renders one hold. An OPEN hold has no released_at and no reason, and both are
// omitted rather than rendered empty: "still running" and "released for a reason nobody recorded" are
// different answers, and a blank string would collapse them.
func machineOccupancyView(it MachineOccupancyItem) map[string]any {
	view := map[string]any{
		"object": "machine_occupancy", "id": it.ID, "session_id": it.SessionID,
		"started_at": it.StartedAt, "billed_seconds": it.BilledSeconds,
		// Rendered even at zero: a hold that has never seen activity is a real state, and an absent field
		// would read as "not measured".
		"last_activity_at": it.LastActivityAt,
	}
	if !it.ReleasedAt.IsZero() {
		view["released_at"] = it.ReleasedAt
	}
	if it.ReleaseReason != "" {
		view["release_reason"] = it.ReleaseReason
	}
	return view
}

// listRunners returns a tenant-scoped page of enrolled machines (GET /v1/runners).
func (h *runnerHandler) listRunners(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	q, ok := beginList(w, r, "runner", scope)
	if !ok {
		return
	}
	window := RunnerListWindow{
		CreatedGTE: q.CreatedGTE, CreatedLTE: q.CreatedLTE, Limit: q.Limit + 1, // +1 over-fetch: renderPage reads it as has_more
	}
	if q.After != nil {
		window.AfterCreatedAt, window.AfterID = &q.After.CreatedAt, q.After.ID
	}
	items, err := h.runners.ListRunners(r.Context(), scope.Project, window)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	rows := make([]ListRow, 0, len(items))
	for _, it := range items {
		body, _ := json.Marshal(runnerView(it))
		rows = append(rows, ListRow{ID: it.ID, CreatedAt: it.CreatedAt, Body: body})
	}
	renderPage(w, r, "runner", scope, rows, q.Limit)
}

// listRunnerPools returns a tenant-scoped page of pools (GET /v1/runner-pools). It answers the
// question the runner list raises and cannot settle on its own — a runner's pool_id is an opaque id
// until something says what that pool IS — and it is the surface an operator reads before deciding
// which pool a project's `config_policy.pool` should name.
func (h *runnerHandler) listRunnerPools(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	q, ok := beginList(w, r, "runner_pool", scope)
	if !ok {
		return
	}
	window := RunnerListWindow{
		CreatedGTE: q.CreatedGTE, CreatedLTE: q.CreatedLTE, Limit: q.Limit + 1, // +1 over-fetch: renderPage reads it as has_more
	}
	if q.After != nil {
		window.AfterCreatedAt, window.AfterID = &q.After.CreatedAt, q.After.ID
	}
	items, err := h.runners.ListRunnerPools(r.Context(), scope.Project, window)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	rows := make([]ListRow, 0, len(items))
	for _, it := range items {
		body, _ := json.Marshal(runnerPoolView(it))
		rows = append(rows, ListRow{ID: it.ID, CreatedAt: it.CreatedAt, Body: body})
	}
	renderPage(w, r, "runner_pool", scope, rows, q.Limit)
}

// THE POOL WRITE SURFACE (E28 T1), and it is two routes because a pool has exactly two operator decisions:
// what it IS, taken once at creation, and whether enrolling into it needs a human, taken whenever.
//
// WHAT WAS MEASURED BEFORE THEM. `POST /v1/runner-pools` was not in this router; `grep -rn "UPDATE
// runner_pools" … | grep -v _test` answered 0; and `InsertDefaultRunnerPool` wrote `'default'`,
// `'sandboxed-linux'` and `false` as literals. So every tenant had exactly one pool, forever, and
// `approveRunner` below — correctly written, correctly gated — decided a state no operator could produce.

// createRunnerPool creates one pool (POST /v1/runner-pools).
//
// THE VALIDATION IS HERE AND THE DATABASE'S CHECK IS THE LAST DEFENCE, NOT THE FIRST. A posture outside the
// two 000045 declares would come back as a 23514 this handler could only render as `internal_error`, which
// tells an operator their control plane is broken when what happened is that they typed `macos`.
func (h *runnerHandler) createRunnerPool(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorizeAdmin(w, r)
	if !ok {
		return
	}
	if scope.Project == "" {
		// A pool with no project is a pool nothing can enrol into: fleet.Store.Register refuses one, because
		// 000045's tenant policy for `runners` narrows on a project. Refusing here keeps a created pool
		// USABLE rather than merely written.
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"a runner pool belongs to a project; use an API key scoped to one")
		return
	}
	var body struct {
		Name             string `json:"name"`
		Posture          string `json:"posture"`
		OS               string `json:"os"`
		Arch             string `json:"arch"`
		StrictEnrollment bool   `json:"strict_enrollment"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	// A field this route does not know is a knob a caller believes exists. Refusing beats ignoring, which is
	// the rule identity.configPolicyInput's comment records two epics learning the hard way.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "name is required and must not be blank")
		return
	}
	if !runnerPoolPostures[body.Posture] {
		// A POOL IS A POSTURE, so there is no default to fall back on: a pool created without one would place
		// runs somewhere nobody described.
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"posture must be one of sandboxed-linux, unsandboxed-host")
		return
	}
	item, err := h.runners.CreateRunnerPool(r.Context(), scope.Project, RunnerPoolCreate{
		Name: strings.TrimSpace(body.Name), Posture: body.Posture, OS: body.OS, Arch: body.Arch,
		StrictEnrollment: body.StrictEnrollment,
	})
	switch {
	case errors.Is(err, ErrRunnerPoolNameTaken):
		middleware.WriteProblem(w, r, http.StatusConflict, "already_exists",
			"this project already has a runner pool with that name")
		return
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	// THE SAME PROJECTION THE LISTING RENDERS. A create that answered in a different shape would make every
	// consumer code against two shapes, and the first one to diverge would do so silently.
	writeJSON(w, http.StatusCreated, runnerPoolView(item))
}

// patchRunnerPool switches one pool's waiting room (PATCH /v1/runner-pools/{pool_id}).
//
// ONE FIELD, AND `posture` IS DELIBERATELY NOT AMONG THEM — a correctness requirement rather than a
// limitation. internal/fleet's Register makes an enrolling machine INHERIT the pool's posture ("the machine
// inherits the pool's posture: having agreed (or said nothing), what it IS is what the pool is"), so moving
// a populated pool's posture would retroactively change what the machines already in it ARE, and the rows
// recording what they were would silently disagree with the pool they are in. A pool's posture is decided
// once, at creation. Renaming is absent for a smaller reason: nothing asked for it.
func (h *runnerHandler) patchRunnerPool(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorizeAdmin(w, r)
	if !ok {
		return
	}
	var body struct {
		// A POINTER, so "not named" and "named false" are different requests: a PATCH that silently did
		// nothing would read to an operator exactly like a PATCH that worked.
		StrictEnrollment *bool `json:"strict_enrollment"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"the request body could not be read; strict_enrollment is the only field this route accepts")
		return
	}
	if body.StrictEnrollment == nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"strict_enrollment is required; a pool's posture is fixed at creation because its machines inherit it")
		return
	}
	item, found, err := h.runners.SetRunnerPoolStrictEnrollment(r.Context(), scope.Project,
		r.PathValue("pool_id"), *body.StrictEnrollment)
	switch {
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	case !found:
		// The same answer for "not yours" and "not there", the posture every other route on this surface takes.
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such runner pool in this project")
		return
	}
	writeJSON(w, http.StatusOK, runnerPoolView(item))
}

// getRunner reads one enrolled machine (GET /v1/runners/{runner_id}).
func (h *runnerHandler) getRunner(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	item, found, err := h.runners.GetRunner(r.Context(), scope.Project, r.PathValue("runner_id"))
	switch {
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	case !found:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such runner in this project")
		return
	}
	writeJSON(w, http.StatusOK, runnerView(item))
}

// listMachineOccupancies reads ONE machine's session history (GET /v1/runners/{runner_id}/occupancies).
//
// ‼️ THE MACHINE IS RESOLVED FIRST AND THAT IS THE TENANCY BOUNDARY. An id in another project answers
// 404 from GetRunner, so this route cannot be used to learn that a machine exists elsewhere by the shape
// of its answer — and the occupancy query carries the project in its WHERE as well, so neither layer is
// the only one holding it.
func (h *runnerHandler) listMachineOccupancies(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	runnerID := r.PathValue("runner_id")
	if _, found, err := h.runners.GetRunner(r.Context(), scope.Project, runnerID); err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	} else if !found {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such runner in this project")
		return
	}
	// The SAME limit parse every list route uses, so a page of holds cannot be bounded differently from
	// a page of anything else. Its cursor half is not used here: this route's cursor is the
	// (started_at, id) pair below, which the opaque list cursor cannot express.
	q, ok := beginList(w, r, "machine_occupancy", scope)
	if !ok {
		return
	}
	before, beforeID, err := occupancyCursor(r)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	items, err := h.occupancies.MachineOccupancies(r.Context(), scope.Project, runnerID, before, beforeID, q.Limit)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	views := make([]map[string]any, 0, len(items))
	for _, it := range items {
		views = append(views, machineOccupancyView(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": views})
}

// occupancyCursor reads the (started_at, id) pair a caller continues a page from. BOTH or NEITHER: a
// timestamp with no id cannot break a tie, which is the whole reason the cursor is a pair, and accepting
// it would hand back a page that silently drops rows.
func occupancyCursor(r *http.Request) (time.Time, string, error) {
	rawTime := strings.TrimSpace(r.URL.Query().Get("starting_before"))
	rawID := strings.TrimSpace(r.URL.Query().Get("starting_before_id"))
	switch {
	case rawTime == "" && rawID == "":
		return time.Time{}, "", nil
	case rawTime == "" || rawID == "":
		return time.Time{}, "", errors.New("starting_before and starting_before_id are a pair: a timestamp alone cannot break a tie between two holds that began in the same tick")
	}
	at, err := time.Parse(time.RFC3339Nano, rawTime)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("starting_before must be an RFC3339 timestamp: %w", err)
	}
	return at, rawID, nil
}

// THE KEY SURFACE (E24 T3). Three routes, all gated on `provision`, and the value of a key exists on
// exactly one of them: the create response. There is no route that reads a key back — nothing stores
// the value to read.

// mintPoolKey mints an enrolment key for a pool (POST /v1/runner-pools/{pool_id}/keys). The value is in
// the response body and nowhere else, so a caller that loses it mints another and revokes this one.
func (h *runnerHandler) mintPoolKey(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorizeAdmin(w, r)
	if !ok {
		return
	}
	var body struct {
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if r.Body != nil {
		// A body is optional (a key with no expiry is the default), so a decode failure on an empty body
		// is not an error. A malformed one is.
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
			return
		}
	}
	item, found, err := h.runners.MintRunnerPoolKey(r.Context(), scope.Project, r.PathValue("pool_id"), body.ExpiresAt)
	switch {
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	case !found:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such runner pool in this project")
		return
	}
	writeJSON(w, http.StatusCreated, poolKeyView(item, true))
}

// listPoolKeys lists a pool's keys (GET /v1/runner-pools/{pool_id}/keys) as metadata.
func (h *runnerHandler) listPoolKeys(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorizeAdmin(w, r)
	if !ok {
		return
	}
	items, err := h.runners.ListRunnerPoolKeys(r.Context(), scope.Project, r.PathValue("pool_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	views := make([]map[string]any, 0, len(items))
	for _, item := range items {
		views = append(views, poolKeyView(item, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": views})
}

// revokePoolKey closes a key (POST /v1/runner-pool-keys/{key_id}/revoke) and answers with the machines
// it already admitted. The key id is in the PATH and the value is nowhere: a revoke that took the value
// would put a credential in an access log.
func (h *runnerHandler) revokePoolKey(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorizeAdmin(w, r)
	if !ok {
		return
	}
	item, found, err := h.runners.RevokeRunnerPoolKey(r.Context(), scope.Project, r.PathValue("key_id"))
	switch {
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	case !found:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such runner pool key in this project")
		return
	}
	writeJSON(w, http.StatusOK, poolKeyView(item, false))
}

// setRunnerState cordons, resumes or revokes one machine (POST /v1/runners/{runner_id}/<action>).
//
// THE ACTION IS BOUND AT ROUTE-REGISTRATION TIME, not read out of the path, and that is worth a sentence
// because it removes a whole class of code: there are three mux patterns and one closure each, so an
// unknown verb is a 404 from the mux and there is no string to validate, no body to decode, and no field a
// caller could smuggle a `runners.state` value through. The action reaching the store is one of exactly
// three literals written in this file.
//
// REVOKE IS IRREVERSIBLE, which is today's in-memory semantics made durable rather than a new rule, and
// the response says so by carrying the machine's new state: an operator handed a bare 200 has to guess.
func (h *runnerHandler) setRunnerState(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := h.authorizeAdmin(w, r)
		if !ok {
			return
		}
		item, found, err := h.runners.SetRunnerState(r.Context(), scope.Project,
			r.PathValue("runner_id"), action)
		switch {
		case err != nil:
			middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
			return
		case !found:
			// The same answer for "not yours", "not there" and "already decommissioned": which of the three it
			// was is not something a caller should be able to probe for.
			middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such runner in this project")
			return
		}
		writeJSON(w, http.StatusOK, runnerView(item))
	}
}

// approveRunner admits one machine out of a strict pool's waiting room
// (POST /v1/runners/{runner_id}/approve, E24 T6).
//
// WHY THIS IS NOT ROUTED THROUGH E23'S THROAT, AND WHY THAT IS NOT A VIOLATION OF E23'S RULE. E23 requires
// that a decision about a gated operation go through ONE function, `coordinator.DecideToolApproval`. That
// function's subject is a TOOL CALL or a PUBLICATION: it is keyed by a tool call id and bound to a
// `request_hash`, so that arguments changed after a human looked leave no approval. A MACHINE ENROLMENT HAS
// NO REQUEST HASH TO BIND TO — there are no arguments, no parked tool call, and the certificate was issued
// before anybody was asked. Routing it there would mean fabricating a tool call and a hash per Mac that
// boots, and the binding would then bind nothing. A separate PATH is correct here; what is NOT separate is
// the POLICY, because WHO MAY DECIDE is still `config_policy.approvers` evaluated by the one function that
// evaluates it (`coordinator.ConfigPolicy.ApproverAllowed`, applied in fleet/strict.go). The longer form of
// this reasoning is at the top of that file, where the rule is applied.
//
// IT IS GATED ON `approve` AND NOT ON `provision`, which is api/approvals.go's argument applied verbatim
// rather than a taxonomy preference: PATCH /v1/projects/{id} is the config_policy write path and is gated on
// `provision`, and config_policy is where `approvers` lives — so a `provision` key could add ITSELF to the
// list it is about to be checked against. The three lifecycle verbs above stay on `provision` because
// cordoning a Mac is administration; admitting one is a decision, and the two capabilities stay
// independent.
func (h *runnerHandler) approveRunner(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	if !scope.HasScope(approveScope) {
		middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope", "this API key lacks the approve capability")
		return
	}
	item, outcome, err := h.runners.ApproveRunner(r.Context(), scope, r.PathValue("runner_id"))
	switch {
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	case outcome.Unauthorized:
		// "You may not decide this" is a different fact from "there is no such machine", and an operator whose
		// approver list is misconfigured has to be able to tell them apart — the ApprovalOutcome split, for the
		// reason it exists there.
		middleware.WriteProblem(w, r, http.StatusForbidden, "approver_not_authorized",
			"this project's approver list does not include the principal this key resolves to")
		return
	case !outcome.Found:
		// The same answer for "not yours", "not there" and "cordoned or revoked, so not admissible": which of
		// the three it was is not something a caller should be able to probe for.
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such runner awaiting approval in this project")
		return
	}
	writeJSON(w, http.StatusOK, runnerView(item))
}

// authorizeAdmin resolves the verified scope and enforces the `provision` capability — the same gate
// every other org-admin surface uses (api-keys, secret-refs, model-routes, limits).
func (h *runnerHandler) authorizeAdmin(w http.ResponseWriter, r *http.Request) (middleware.Scope, bool) {
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

// poolKeyView renders a key. withValue is true on exactly one call site — the create response — and the
// field is omitted rather than emptied everywhere else, so a listing has no key to accidentally carry.
func poolKeyView(item RunnerPoolKeyItem, withValue bool) map[string]any {
	view := map[string]any{
		"id": item.ID, "object": "runner_pool_key", "pool_id": item.PoolID,
		"key_prefix": item.Prefix, "created_at": item.CreatedAt,
	}
	if withValue && item.Value != "" {
		// THE ONE TIME. The store does not keep the value, so this response is the only thing that has it.
		view["key"] = item.Value
	}
	for key, at := range map[string]*time.Time{
		"expires_at": item.ExpiresAt, "revoked_at": item.RevokedAt, "last_used_at": item.LastUsedAt,
	} {
		if at != nil {
			view[key] = *at
		}
	}
	if item.EnrolledRunners != nil {
		machines := make([]map[string]any, 0, len(item.EnrolledRunners))
		for _, m := range item.EnrolledRunners {
			machines = append(machines, map[string]any{
				"id": m.ID, "label": m.Label, "runner_dns": m.DNS, "state": m.State,
				"pool_id": m.PoolID, "enrolled_at": m.EnrolledAt,
			})
		}
		// Named for what it MEANS, not for what it lists: these machines keep running, and the field name
		// is where an operator learns that without reading a runbook.
		view["enrolled_runners_still_running"] = machines
	}
	return view
}

// runnerPoolView is the ONE projection the list, the create and the patch render, so a create cannot
// answer in a shape the read will not repeat. `waiting` is rendered only when the gateway could be asked —
// see RunnerPoolItem.Waiting for why absence and zero are different answers.
func runnerPoolView(it RunnerPoolItem) map[string]any {
	view := map[string]any{
		"id": it.ID, "object": "runner_pool", "name": it.Name, "posture": it.Posture,
		"os": it.OS, "arch": it.Arch, "strict_enrollment": it.StrictEnrollment,
		"created_at": it.CreatedAt,
	}
	if it.Waiting != nil {
		view["waiting"] = *it.Waiting
	}
	return view
}

// runnerView is the ONE projection both routes render, so a field cannot appear on the list and go
// missing on the read. Zero timestamps are omitted rather than rendered as year 1: a runner that has
// never been seen has no last_seen_at, and "0001-01-01" is a worse answer than absence.
func runnerView(it RunnerItem) map[string]any {
	view := map[string]any{
		"id": it.ID, "object": "runner", "pool_id": it.PoolID, "label": it.Label,
		"runner_dns": it.DNS, "public_key_sha256": it.PublicKeySHA256, "state": it.State,
		"os": it.OS, "arch": it.Arch, "posture": it.Posture, "capacity": it.Capacity,
		"created_at": it.CreatedAt,
		// WHAT THE MACHINE REPORTED, rendered unconditionally including empty. 000007 added both columns
		// and the enrolment writes them; until now nothing read them, so the panel could not say which
		// build a machine runs or whether it can isolate a session while the answer sat in the row — the
		// defect this tree already paid for once with runners.capacity. Empty is a real answer: a machine
		// enrolled before these existed reported neither, and an omitted field would read as "not asked".
		"agent_version": it.AgentVersion, "isolation_modes": it.IsolationModes,
	}
	for key, at := range map[string]time.Time{
		"cert_not_after": it.CertNotAfter, "enrolled_at": it.EnrolledAt, "last_seen_at": it.LastSeenAt,
		"config_reported_at": it.ConfigReportedAt,
	} {
		if !at.IsZero() {
			view[key] = at
		}
	}
	// The live lease count, on the reads that have one. Rendered UNCONDITIONALLY on those, including at
	// zero: "this machine is serving nothing" and "nobody asked the gateway" are different answers, and an
	// omitted field would make them look the same to an operator deciding whether to unplug a Mac. The
	// listing does not set it — see RunnerItem.ActiveLeases — so a page carries no such field at all.
	if it.ActiveLeases != nil {
		view["active_leases"] = *it.ActiveLeases
	}
	// CONNECTION STATE, FROM THE GATEWAY AND NOT FROM A TIMESTAMP. last_seen_at answers when a machine
	// last spoke; this answers whether it is there now, and on a Mac unplugged four minutes ago the two
	// disagree in the direction that decides whether an operator sends it work. Rendered only on the
	// reads that consulted the gateway — a listing that did not ask must not render "offline", which an
	// operator would read as an answer.
	if it.Connections != nil {
		state := "offline"
		if *it.Connections > 0 {
			state = "online"
		}
		view["connection_state"] = state
		// Beside the state because they are different facts: one machine parking two loops is ONE online
		// machine with room for two leases.
		view["connections"] = *it.Connections
	}
	// THE CONFIGURATION REPORT IS RENDERED ONLY WHEN THE MACHINE HAS MADE ONE, and the absence is the
	// message: a machine that has never reported is one this control plane has never confirmed is running
	// what the panel says. A screen that showed `config_revision: 0` would render that as a number, and an
	// operator reads a number as an answer. Absence they have to ask about.
	if !it.ConfigReportedAt.IsZero() {
		view["config_revision"] = it.ConfigRevision
		// Rendered even when empty, which is a REAL answer distinct from the absence above: the machine
		// polled and this deployment had no document for it, so it is running its own defaults and knows it.
		applied := it.ConfigApplied
		if applied == nil {
			applied = map[string]string{}
		}
		view["config_applied"] = applied
	}
	return view
}

// desiredForScope serves the desired document for ONE pool or ONE machine.
//
// WHY IT IS HERE AND NOT ON /v1/deployment. That route's own comment states the rule this follows: "GET
// /v1/deployment reports THIS PROCESS's environment — a machine's document belongs to whatever surface
// reports that machine, and serving it here would put a pool's configuration under a heading that says
// 'this deployment'". The surface that reports machines is this one, so this is where their configuration
// is read.
//
// IT IS A READ AND THERE IS NO MATCHING WRITE ROUTE, deliberately: the write is PUT /v1/deployment/desired
// carrying `plane` and `scope_id`, which is ONE write path for all three scopes. A second one here would be
// a second validator of the same document — the exact defect deployment_desired.go's header says the whole
// surface was built to avoid.
//
// THE SCOPE COMES FROM THE PATH, and the tenant boundary comes from RLS on the machine lookup rather than
// from this document (which has no organization_id and cannot have one). That is why the pool/runner id is
// NOT verified to belong to the caller here: the `provision` capability is the authority for the whole
// desired surface, and a provision key that could read one scope of a system-scoped document can read them
// all. Naming a scope that does not exist reads as no document, which is also what an unwritten one reads
// as — a distinction this route deliberately does not draw, because drawing it would turn a configuration
// read into an existence oracle for ids the caller did not mint.
func (h *runnerHandler) desiredForScope(plane, pathValue string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := middleware.ScopeFrom(r.Context())
		if !ok {
			middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
			return
		}
		if !scope.HasScope(provisionScope) {
			middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope", "this API key lacks the provision capability")
			return
		}
		scopeID := r.PathValue(pathValue)
		if scopeID == "" {
			middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the path carries no "+pathValue)
			return
		}
		doc, err := h.desired.GetDesiredConfig(r.Context(), scope, plane, scopeID)
		if err != nil {
			middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "the desired configuration could not be read")
			return
		}
		// NIL IS SERVED AS AN EXPLICIT NULL rather than as a 404, and the two say different things. A 404
		// would mean "there is no such pool", which this route cannot know and must not imply; a null
		// document means "nobody has decided anything for this scope", which is the true and useful answer
		// and the one a form needs in order to render empty fields rather than an error.
		body := map[string]any{"object": "deployment_desired", "plane": plane, "scope_id": scopeID, "desired": nil}
		if doc != nil {
			body["desired"] = doc
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}
