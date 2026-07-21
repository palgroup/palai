package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
)

// ScheduleAPI is the store seam for the schedule management surface (spec §33, E11 Task 3). The automation
// ScheduleStore implements it; production wires it, and tiers that do not touch schedules pass nil so the
// routes stay unmounted. Every method is scoped by the verified identity, never a request-body field
// (§39.2). A firing admits AS the creating principal.
type ScheduleAPI interface {
	CreateSchedule(ctx context.Context, org, project, principal string, in automation.ScheduleInput) (string, error)
	GetSchedule(ctx context.Context, org, project, id string) (automation.ScheduleView, bool, error)
	ReviseSchedule(ctx context.Context, org, project, id string, in automation.ScheduleInput) (int, bool, error)
	SetPaused(ctx context.Context, org, project, id string, paused bool) (bool, error)
	DeleteSchedule(ctx context.Context, org, project, id string) (bool, error)
	ListOccurrences(ctx context.Context, org, project, id string, limit int) ([]automation.OccurrenceView, error)
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
	body, in, ok := decodeScheduleInput(w, r, raw)
	if !ok {
		return
	}
	_ = body
	id, err := h.schedules.CreateSchedule(r.Context(), scope.Organization, scope.Project, scope.Principal, in)
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
	view, found, err := h.schedules.GetSchedule(r.Context(), scope.Organization, scope.Project, r.PathValue("schedule_id"))
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
	_, in, ok := decodeScheduleInput(w, r, raw)
	if !ok {
		return
	}
	revision, found, err := h.schedules.ReviseSchedule(r.Context(), scope.Organization, scope.Project, r.PathValue("schedule_id"), in)
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
func (h *scheduleHandler) pauseSchedule(w http.ResponseWriter, r *http.Request)  { h.setPaused(w, r, true) }
func (h *scheduleHandler) resumeSchedule(w http.ResponseWriter, r *http.Request) { h.setPaused(w, r, false) }

func (h *scheduleHandler) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	found, err := h.schedules.SetPaused(r.Context(), scope.Organization, scope.Project, r.PathValue("schedule_id"), paused)
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
	found, err := h.schedules.DeleteSchedule(r.Context(), scope.Organization, scope.Project, r.PathValue("schedule_id"))
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

// listOccurrences returns a schedule's occurrences newest-first (GET /v1/schedules/{schedule_id}/occurrences).
func (h *scheduleHandler) listOccurrences(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	occs, err := h.schedules.ListOccurrences(r.Context(), scope.Organization, scope.Project, r.PathValue("schedule_id"), 0)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if occs == nil {
		occs = []automation.OccurrenceView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"occurrences": occs})
}

// decodeScheduleInput parses the request body into an automation.ScheduleInput, turning malformed JSON or
// an unparseable RFC3339 time into a 400.
func decodeScheduleInput(w http.ResponseWriter, r *http.Request, raw []byte) (scheduleBody, automation.ScheduleInput, bool) {
	var body scheduleBody
	if err := json.Unmarshal(raw, &body); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON")
		return scheduleBody{}, automation.ScheduleInput{}, false
	}
	oneTime, ok := parseOptionalTime(w, r, body.OneTimeAt, "one_time_at")
	if !ok {
		return scheduleBody{}, automation.ScheduleInput{}, false
	}
	startsAt, ok := parseOptionalTime(w, r, body.StartsAt, "starts_at")
	if !ok {
		return scheduleBody{}, automation.ScheduleInput{}, false
	}
	endsAt, ok := parseOptionalTime(w, r, body.EndsAt, "ends_at")
	if !ok {
		return scheduleBody{}, automation.ScheduleInput{}, false
	}
	return body, automation.ScheduleInput{
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
