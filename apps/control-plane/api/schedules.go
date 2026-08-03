package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
)

// ScheduleAPI is the store seam for the schedule management surface (spec §33, E11 Task 3). The automation
// ScheduleStore implements it; production wires it, and tiers that do not touch schedules pass nil so the
// routes stay unmounted. Every method is scoped by the verified identity, never a request-body field
// (§39.2). A firing admits AS the creating principal.
//
// No organization parameter (A.2 Task 3): the request scope no longer resolves one, and ScheduleStore
// resolves it fresh from project where it still needs one internally.
type ScheduleAPI interface {
	CreateSchedule(ctx context.Context, project, principal string, in automation.ScheduleInput) (string, error)
	GetSchedule(ctx context.Context, project, id string) (automation.ScheduleView, bool, error)
	ReviseSchedule(ctx context.Context, project, id string, in automation.ScheduleInput) (int, bool, error)
	SetPaused(ctx context.Context, project, id string, paused bool) (bool, error)
	DeleteSchedule(ctx context.Context, project, id string) (bool, error)
	// ListSchedules is the E29 T1 read side. Until it landed, a schedule could be found again only by an id
	// its creator kept — and a schedule is the one object in this tree that fires on a wall clock with
	// nobody watching, so the id outliving the terminal that printed it is the normal case, not the edge.
	// status filters the lifecycle column; empty is unfiltered.
	ListSchedules(ctx context.Context, project string, w automation.ListWindow, status string) ([]automation.ScheduleView, error)
	// ListOccurrences takes a WINDOW rather than a bare limit (E29 T1). The bare limit was always passed as
	// 0, the store clamped 0 to 100, and the response envelope had no has_more — so the hundred-and-first
	// occurrence of a per-minute schedule was indistinguishable from no hundred-and-first occurrence.
	ListOccurrences(ctx context.Context, project, id string, w automation.ListWindow) ([]automation.OccurrenceView, error)
}

type scheduleHandler struct {
	schedules ScheduleAPI
}

// beginScope authenticates and reads the bounded body, shared by the mutating schedule handlers (the
// triggerHandler.begin shape, as a free helper).
func beginScope(w http.ResponseWriter, r *http.Request) (middleware.Scope, []byte, bool) {
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

// scheduleBody is the create/revise request shape. Times are RFC3339 strings (empty ⇒ unset).
type scheduleBody struct {
	Name                string `json:"name"`
	TriggerID           string `json:"trigger_id"`
	Kind                string `json:"kind"`
	CronExpr            string `json:"cron_expr"`
	Timezone            string `json:"timezone"`
	OneTimeAt           string `json:"one_time_at"`
	MisfirePolicy       string `json:"misfire_policy"`
	MisfireGraceSeconds int    `json:"misfire_grace_seconds"`
	MaxCatchUp          int    `json:"max_catch_up"`
	JitterSeconds       int    `json:"jitter_seconds"`
	StartsAt            string `json:"starts_at"`
	EndsAt              string `json:"ends_at"`
}

// createSchedule registers a schedule (POST /v1/schedules). The cron expr + IANA timezone are validated at
// create (an unknown timezone or malformed cron is a 400, never a stored row). Durable config, not an
// idempotent operation, so no Idempotency-Key — the API mints the id server-side.
func (h *scheduleHandler) createSchedule(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := beginScope(w, r)
	if !ok {
		return
	}
	in, ok := decodeScheduleInput(w, r, raw)
	if !ok {
		return
	}
	id, err := h.schedules.CreateSchedule(r.Context(), scope.Project, scope.Principal, in)
	if bad := scheduleProblem(w, r, err); bad {
		return
	}
	w.Header().Set("Location", "/v1/schedules/"+id)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// getSchedule returns a schedule's management projection (GET /v1/schedules/{schedule_id}).
func (h *scheduleHandler) getSchedule(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	view, found, err := h.schedules.GetSchedule(r.Context(), scope.Project, r.PathValue("schedule_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !found {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such schedule in this project")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// reviseSchedule applies a firing-relevant edit (PATCH /v1/schedules/{schedule_id}), bumping the revision.
// Name/trigger are immutable; only the firing config is edited. A malformed cron/timezone is a 400.
func (h *scheduleHandler) reviseSchedule(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := beginScope(w, r)
	if !ok {
		return
	}
	in, ok := decodeScheduleInput(w, r, raw)
	if !ok {
		return
	}
	revision, found, err := h.schedules.ReviseSchedule(r.Context(), scope.Project, r.PathValue("schedule_id"), in)
	if bad := scheduleProblem(w, r, err); bad {
		return
	}
	if !found {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such schedule in this project")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": revision})
}

// pauseSchedule / resumeSchedule stop and restart the schedule's admission (POST .../pause, .../resume).
func (h *scheduleHandler) pauseSchedule(w http.ResponseWriter, r *http.Request) {
	h.setPaused(w, r, true)
}
func (h *scheduleHandler) resumeSchedule(w http.ResponseWriter, r *http.Request) {
	h.setPaused(w, r, false)
}

func (h *scheduleHandler) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	found, err := h.schedules.SetPaused(r.Context(), scope.Project, r.PathValue("schedule_id"), paused)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !found {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such schedule in this project")
		return
	}
	status := "active"
	if paused {
		status = "paused"
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}

// deleteSchedule soft-deletes a schedule (DELETE /v1/schedules/{schedule_id}); its occurrence rows persist
// under retention.
func (h *scheduleHandler) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	found, err := h.schedules.DeleteSchedule(r.Context(), scope.Project, r.PathValue("schedule_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !found {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such schedule in this project")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listSchedules returns a tenant-scoped page of schedules (GET /v1/schedules), confined by RLS, in the
// shared page envelope. ?status= filters the lifecycle column; cursor + created_at bounds otherwise.
//
// IT IS NOT A ListView. The sibling automation family (triggers) is already on beginList/renderPage in this
// same package, and a schedule collection grows without bound — one per cadence an operator ever wanted —
// whereas ListView's un-paginated envelope is for the small fixed collections (model routes) it was written
// for. An unbounded collection in an envelope that cannot say "there is more" is exactly the defect the
// occurrence log below carried.
func (h *scheduleHandler) listSchedules(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	q, ok := beginList(w, r, "schedules", scope)
	if !ok {
		return
	}
	// The VALUE of ?status= is validated here, at the edge. beginList only decides whether this KIND may
	// carry the parameter at all; whether `banana` is a state is this route's question, and answering it
	// with an empty 200 would tell a client "none are banana" when the truth is "banana is not a state".
	// The column's CHECK constraint stays the last line of defence rather than the first.
	if q.Status != "" && !automation.KnownScheduleStatus(q.Status) {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"status must be one of "+strings.Join(automation.ScheduleStatuses, ", "))
		return
	}
	views, err := h.schedules.ListSchedules(r.Context(), scope.Project, listWindow(q), q.Status)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	rows := make([]ListRow, 0, len(views))
	for _, view := range views {
		// The row body is ScheduleView itself — the SAME projection GET /v1/schedules/{id} returns. Building
		// a leaner map here is how a list row and a singular read start disagreeing, and a screen that has
		// to code against two shapes reads one of them wrong.
		body, _ := json.Marshal(view)
		rows = append(rows, ListRow{ID: view.ID, CreatedAt: view.CreatedAt, Body: body})
	}
	renderPage(w, r, "schedules", scope, rows, q.Limit)
}

// listOccurrences returns a page of a schedule's occurrences newest-first
// (GET /v1/schedules/{schedule_id}/occurrences), in the shared page envelope.
//
// IT USED TO ANSWER OUTSIDE THAT ENVELOPE: `{"occurrences": [...]}`, with the store's limit hard-coded to 0
// and clamped to 100. No data, no has_more, no next_cursor — so a schedule firing every minute became
// unreadable after 100 minutes and said nothing about it. A truncation a client cannot detect is a lie the
// client repeats.
//
// The keyset column is planned_at rather than created_at (the occurrence's own order is the order it was
// PLANNED in), so the shared cursor's position carries the planned instant. The created_at bounds still
// mean created_at — see the query.
// IT IS NOT `provision`-GATED, AND THE TWO NEW LISTS BESIDE IT ARE. That asymmetry is not an oversight and
// it cost a revert to get right: this route SHIPPED in E11 with no capability gate, and adding one now is a
// contract change — a key with a narrowed scope set that reads this log today would start receiving 403.
// The gate belongs on routes that did not exist yesterday, which is exactly the line the E25 T7 precedent
// draws and which listSchedules' own comment cites. "In practice almost every key has an empty scope set
// and would not notice" is the reasoning that makes a silent contract change, so it is not the reasoning
// used. Pinned by TestTheShippedOccurrenceLogIsNotRetroGated.
func (h *scheduleHandler) listOccurrences(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	q, ok := beginList(w, r, "schedule_occurrences", scope)
	if !ok {
		return
	}
	occs, err := h.schedules.ListOccurrences(r.Context(), scope.Project, r.PathValue("schedule_id"), listWindow(q))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	rows := make([]ListRow, 0, len(occs))
	for _, occ := range occs {
		body, _ := json.Marshal(occ)
		rows = append(rows, ListRow{ID: occ.OccurrenceID, CreatedAt: occ.PlannedAt, Body: body})
	}
	renderPage(w, r, "schedule_occurrences", scope, rows, q.Limit)
}

// listWindow maps the shared parse onto the automation store's window, over-fetching by one so renderPage
// can detect a further page without a second round trip (api/pagination.go's contract).
func listWindow(q ListQuery) automation.ListWindow {
	window := automation.ListWindow{CreatedGTE: q.CreatedGTE, CreatedLTE: q.CreatedLTE, Limit: q.Limit + 1}
	if q.After != nil {
		window.AfterCreatedAt = &q.After.CreatedAt
		window.AfterID = q.After.ID
	}
	return window
}

// authorize resolves the verified scope and enforces the `provision` capability for the ONE route E29 T1
// added to this family: GET /v1/schedules.
//
// THE ASYMMETRY WITH EVERY SHIPPED ROUTE IS DELIBERATE, for the reason hookHandler.authorize spells out and
// the E25 T7 tool-revision reads set the precedent for: a new read answering what an operator provisioned
// sits with the provisioned surfaces, and retro-gating create/pause/resume/delete/get-by-id — or the
// occurrence log — would be a contract change rather than a read route.
// HONEST CEILING: a key with an EMPTY scope set holds `provision` implicitly, so this separates a narrowed
// key from a broad one and never an operator from an operator.
func (h *scheduleHandler) authorize(w http.ResponseWriter, r *http.Request) (middleware.Scope, bool) {
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

// decodeScheduleInput parses the request body into an automation.ScheduleInput, turning malformed JSON or
// an unparseable RFC3339 time into a 400.
func decodeScheduleInput(w http.ResponseWriter, r *http.Request, raw []byte) (automation.ScheduleInput, bool) {
	var body scheduleBody
	if err := json.Unmarshal(raw, &body); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON")
		return automation.ScheduleInput{}, false
	}
	oneTime, ok := parseOptionalTime(w, r, body.OneTimeAt, "one_time_at")
	if !ok {
		return automation.ScheduleInput{}, false
	}
	startsAt, ok := parseOptionalTime(w, r, body.StartsAt, "starts_at")
	if !ok {
		return automation.ScheduleInput{}, false
	}
	endsAt, ok := parseOptionalTime(w, r, body.EndsAt, "ends_at")
	if !ok {
		return automation.ScheduleInput{}, false
	}
	return automation.ScheduleInput{
		Name: body.Name, TriggerID: body.TriggerID, Kind: body.Kind, CronExpr: body.CronExpr,
		Timezone: body.Timezone, OneTimeAt: oneTime, MisfirePolicy: body.MisfirePolicy,
		MisfireGraceSeconds: body.MisfireGraceSeconds, MaxCatchUp: body.MaxCatchUp, JitterSeconds: body.JitterSeconds,
		StartsAt: startsAt, EndsAt: endsAt,
	}, true
}

// parseOptionalTime parses an optional RFC3339 field; empty ⇒ zero time; malformed ⇒ a 400.
func parseOptionalTime(w http.ResponseWriter, r *http.Request, value, field string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", field+" must be an RFC3339 timestamp")
		return time.Time{}, false
	}
	return t, true
}

// scheduleProblem maps a create/revise store error to an HTTP problem, returning true when it wrote one. A
// bad cron/timezone/shape or an unknown trigger reference is a 400 (client-fixable); anything else is a 500.
func scheduleProblem(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, automation.ErrInvalidCron),
		errors.Is(err, automation.ErrInvalidTimezone),
		errors.Is(err, automation.ErrScheduleInvalid),
		errors.Is(err, automation.ErrTriggerNotFound):
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the schedule config is invalid: "+err.Error())
	default:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
	}
	return true
}
