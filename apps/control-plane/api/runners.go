package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// The runner registry READ surface (E24 T1). Two routes, both GET, and the absence of a write route
// is deliberate rather than unfinished: cordoning, draining and revoking a runner is T5's work and
// enrolment is the runner plane's, so there is nothing here an operator could POST that would not be
// a second way to do something that does not exist yet. A surface that can only be read cannot be
// mis-used to change a fleet.
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
}

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
// the `provision` capability, the same as every other org-admin surface, and there is still no route
// that moves a machine (cordon/drain/revoke is T5's).
type RunnerRegistryAPI interface {
	ListRunners(ctx context.Context, org, project string, w RunnerListWindow) ([]RunnerItem, error)
	// GetRunner reports found=false for an id that is not in the caller's tenant, so the handler
	// answers 404 without ever learning whether it exists elsewhere.
	GetRunner(ctx context.Context, org, project, id string) (RunnerItem, bool, error)
	// ListRunnerPools pages this tenant's pools. It is on the SAME interface as the runner reads
	// rather than an interface of its own because there is one implementation and one mount, and a
	// second seam would be an abstraction bought before anything asked for it.
	ListRunnerPools(ctx context.Context, org, project string, w RunnerListWindow) ([]RunnerPoolItem, error)
	// MintRunnerPoolKey mints an enrolment key for one of this tenant's pools and returns its value
	// EXACTLY ONCE. found=false is a pool that is not the caller's (or does not exist), rendered 404.
	MintRunnerPoolKey(ctx context.Context, org, project, poolID string, expiresAt *time.Time) (RunnerPoolKeyItem, bool, error)
	// ListRunnerPoolKeys lists key metadata — never a value, never a digest.
	ListRunnerPoolKeys(ctx context.Context, org, project, poolID string) ([]RunnerPoolKeyItem, error)
	// RevokeRunnerPoolKey closes a key and reports the machines it already admitted, none of which is
	// stopped. Idempotent; found=false for an unknown or foreign id.
	RevokeRunnerPoolKey(ctx context.Context, org, project, keyID string) (RunnerPoolKeyItem, bool, error)
}

type runnerHandler struct{ runners RunnerRegistryAPI }

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
	items, err := h.runners.ListRunners(r.Context(), scope.Organization, scope.Project, window)
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
	items, err := h.runners.ListRunnerPools(r.Context(), scope.Organization, scope.Project, window)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	rows := make([]ListRow, 0, len(items))
	for _, it := range items {
		body, _ := json.Marshal(map[string]any{
			"id": it.ID, "object": "runner_pool", "name": it.Name, "posture": it.Posture,
			"os": it.OS, "arch": it.Arch, "strict_enrollment": it.StrictEnrollment,
			"created_at": it.CreatedAt,
		})
		rows = append(rows, ListRow{ID: it.ID, CreatedAt: it.CreatedAt, Body: body})
	}
	renderPage(w, r, "runner_pool", scope, rows, q.Limit)
}

// getRunner reads one enrolled machine (GET /v1/runners/{runner_id}).
func (h *runnerHandler) getRunner(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	item, found, err := h.runners.GetRunner(r.Context(), scope.Organization, scope.Project, r.PathValue("runner_id"))
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
	item, found, err := h.runners.MintRunnerPoolKey(r.Context(), scope.Organization, scope.Project, r.PathValue("pool_id"), body.ExpiresAt)
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
	items, err := h.runners.ListRunnerPoolKeys(r.Context(), scope.Organization, scope.Project, r.PathValue("pool_id"))
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
	item, found, err := h.runners.RevokeRunnerPoolKey(r.Context(), scope.Organization, scope.Project, r.PathValue("key_id"))
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

// runnerView is the ONE projection both routes render, so a field cannot appear on the list and go
// missing on the read. Zero timestamps are omitted rather than rendered as year 1: a runner that has
// never been seen has no last_seen_at, and "0001-01-01" is a worse answer than absence.
func runnerView(it RunnerItem) map[string]any {
	view := map[string]any{
		"id": it.ID, "object": "runner", "pool_id": it.PoolID, "label": it.Label,
		"runner_dns": it.DNS, "public_key_sha256": it.PublicKeySHA256, "state": it.State,
		"os": it.OS, "arch": it.Arch, "posture": it.Posture, "capacity": it.Capacity,
		"created_at": it.CreatedAt,
	}
	for key, at := range map[string]time.Time{
		"cert_not_after": it.CertNotAfter, "enrolled_at": it.EnrolledAt, "last_seen_at": it.LastSeenAt,
	} {
		if !at.IsZero() {
			view[key] = at
		}
	}
	return view
}
