package api

import (
	"context"
	"encoding/json"
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

// RunnerRegistryAPI is the read seam; internal/fleet.Store implements it through a thin adapter.
// Tiers that wire no registry pass nil and the routes stay unmounted — the same posture every other
// optional surface in this router takes.
type RunnerRegistryAPI interface {
	ListRunners(ctx context.Context, org, project string, w RunnerListWindow) ([]RunnerItem, error)
	// GetRunner reports found=false for an id that is not in the caller's tenant, so the handler
	// answers 404 without ever learning whether it exists elsewhere.
	GetRunner(ctx context.Context, org, project, id string) (RunnerItem, bool, error)
	// ListRunnerPools pages this tenant's pools. It is on the SAME interface as the runner reads
	// rather than an interface of its own because there is one implementation and one mount, and a
	// second seam would be an abstraction bought before anything asked for it.
	ListRunnerPools(ctx context.Context, org, project string, w RunnerListWindow) ([]RunnerPoolItem, error)
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
