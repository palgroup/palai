package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// ScheduleStore is the pgx-backed store for the schedule management surface + the ticker's due-scan /
// occurrence-claim / handoff-sweep (spec §33, E11 Task 3). It shares the durable spine's pool and holds a
// TriggerStore so a firing admits through the SAME §20.2.2 delivery pipeline a manual/API POST takes (the
// agent pin lives in the trigger revision — a schedule only decides WHEN).
type ScheduleStore struct {
	pool     *pgxpool.Pool
	triggers *TriggerStore
}

// NewScheduleStore wraps the shared pool and the trigger store the ticker fires through.
func NewScheduleStore(pool *pgxpool.Pool, triggers *TriggerStore) *ScheduleStore {
	return &ScheduleStore{pool: pool, triggers: triggers}
}

var (
	// ErrScheduleNotFound is returned when a schedule is absent from the scope (or soft-deleted).
	ErrScheduleNotFound = errors.New("automation: schedule not found in scope")
	// ErrInvalidTimezone rejects a schedule whose timezone is not a resolvable IANA name (a 400). The
	// binary imports time/tzdata so resolution is independent of container zoneinfo.
	ErrInvalidTimezone = errors.New("automation: invalid timezone (must be a resolvable IANA name)")
	// ErrScheduleInvalid rejects a schedule whose shape is inconsistent (a cron without an expr, a one_time
	// without an instant, a name/trigger missing, or a cron that never fires in the 5-year lookahead).
	ErrScheduleInvalid = errors.New("automation: invalid schedule")
)

// ScheduleInput is the create/revise payload (the firing-relevant config). Zero StartsAt/EndsAt/OneTimeAt
// mean unset.
type ScheduleInput struct {
	Name                string
	TriggerID           string
	Kind                string // "cron" (default) | "one_time"
	CronExpr            string
	Timezone            string
	OneTimeAt           time.Time
	MisfirePolicy       string
	MisfireGraceSeconds int
	MaxCatchUp          int
	JitterSeconds       int
	StartsAt            time.Time
	EndsAt              time.Time
}

// ScheduleView is a schedule's management projection (GET /v1/schedules/{id}).
type ScheduleView struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	TriggerID           string     `json:"trigger_id"`
	Kind                string     `json:"kind"`
	CronExpr            string     `json:"cron_expr,omitempty"`
	Timezone            string     `json:"timezone"`
	MisfirePolicy       string     `json:"misfire_policy"`
	MisfireGraceSeconds int        `json:"misfire_grace_seconds"`
	MaxCatchUp          int        `json:"max_catch_up"`
	JitterSeconds       int        `json:"jitter_seconds"`
	Status              string     `json:"status"`
	StatusReason        string     `json:"status_reason,omitempty"`
	Revision            int        `json:"revision"`
	NextFireAt          *time.Time `json:"next_fire_at,omitempty"`
	OneTimeAt           *time.Time `json:"one_time_at,omitempty"`
	StartsAt            *time.Time `json:"starts_at,omitempty"`
	EndsAt              *time.Time `json:"ends_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// OccurrenceView is one occurrence's projection (GET /v1/schedules/{id}/occurrences). planned_at vs
// admitted_at makes lateness + jitter visible (§33.5).
type OccurrenceView struct {
	OccurrenceID string     `json:"occurrence_id"`
	Revision     int        `json:"schedule_revision"`
	PlannedAt    time.Time  `json:"planned_at"`
	AdmittedAt   *time.Time `json:"admitted_at,omitempty"`
	State        string     `json:"state"`
	DeliveryID   string     `json:"delivery_id,omitempty"`
	Reason       string     `json:"reason,omitempty"`
}

// CreateSchedule validates the cron/timezone at create (an unknown IANA name is a 400, never a stored
// row), computes the initial next_fire_at, and inserts the schedule. It verifies the target trigger is in
// the tenant's scope (a schedule can never fire a foreign trigger). created_by records the principal the
// firing admits AS.
func (s *ScheduleStore) CreateSchedule(ctx context.Context, org, project, principal string, in ScheduleInput) (string, error) {
	spec, err := s.validate(ctx, org, project, in)
	if err != nil {
		return "", err
	}
	next, hasNext := spec.firstFireAt(createBase(in, time.Now()))
	if !hasNext {
		return "", fmt.Errorf("%w: no occurrence in the 5-year lookahead", ErrScheduleInvalid)
	}
	id := newID("sch")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertSchedule"),
		id, org, project, in.Name, in.TriggerID, principal, kindOrDefault(in.Kind), in.CronExpr, in.Timezone,
		nullableTime(in.OneTimeAt), policyOrDefault(in.MisfirePolicy), graceOrDefault(in.MisfireGraceSeconds),
		in.MaxCatchUp, in.JitterSeconds, nullableTime(in.StartsAt), nullableTime(in.EndsAt), next); err != nil {
		return "", fmt.Errorf("insert schedule: %w", err)
	}
	return id, nil
}

// ReviseSchedule applies a firing-relevant edit in place, bumping revision and recomputing next_fire_at
// (the no-schedule_revisions-table decision — occurrences pin the revision they fired under). A revise
// re-activates a paused/failed schedule. Returns found=false when the schedule is absent from scope.
func (s *ScheduleStore) ReviseSchedule(ctx context.Context, org, project, id string, in ScheduleInput) (int, bool, error) {
	// A revise edits only the firing config — name/trigger are immutable and the schedule already exists in
	// scope, so only the firing shape (cron/tz/kind) is re-validated (not name/trigger/scope).
	spec, err := validateFiring(in)
	if err != nil {
		return 0, false, err
	}
	next, hasNext := spec.firstFireAt(createBase(in, time.Now()))
	if !hasNext {
		return 0, false, fmt.Errorf("%w: no occurrence in the 5-year lookahead", ErrScheduleInvalid)
	}
	var revision int
	switch err := s.pool.QueryRow(ctx, storage.Query("ReviseSchedule"),
		id, org, project, in.CronExpr, in.Timezone, nullableTime(in.OneTimeAt), policyOrDefault(in.MisfirePolicy),
		graceOrDefault(in.MisfireGraceSeconds), in.MaxCatchUp, in.JitterSeconds,
		nullableTime(in.StartsAt), nullableTime(in.EndsAt), next).Scan(&revision); {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("revise schedule: %w", err)
	}
	return revision, true, nil
}

// SetPaused pauses or resumes a schedule (the due-scan stops/starts admitting new occurrences; an
// in-flight run is untouched). A pause leaves next_fire_at as-is (it never fires while paused); a RESUME
// recomputes next_fire_at from now — a resumed schedule fires fresh, never replaying its stale missed
// window (which for policy=fail would re-enter the misfire machine and re-fail — review #4). Returns
// found=false when absent from scope.
func (s *ScheduleStore) SetPaused(ctx context.Context, org, project, id string, paused bool) (bool, error) {
	if paused {
		switch err := s.pool.QueryRow(ctx, storage.Query("PauseSchedule"), id, org, project).Scan(new(string)); {
		case errors.Is(err, pgx.ErrNoRows):
			return false, nil
		case err != nil:
			return false, fmt.Errorf("pause schedule: %w", err)
		}
		return true, nil
	}

	// Resume: recompute next_fire_at from now off the schedule's stored firing config.
	view, found, err := s.GetSchedule(ctx, org, project, id)
	if err != nil || !found {
		return false, err
	}
	spec, err := validateFiring(scheduleInputFromView(view))
	if err != nil {
		return false, err // a stored schedule is always valid; defensive
	}
	next, _ := spec.firstFireAt(createBase(scheduleInputFromView(view), time.Now()))
	switch err := s.pool.QueryRow(ctx, storage.Query("ResumeSchedule"), id, org, project, nullableTime(next)).Scan(new(string)); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("resume schedule: %w", err)
	}
	return true, nil
}

// scheduleInputFromView rebuilds the firing config of a stored schedule (for a resume's next_fire_at
// recompute).
func scheduleInputFromView(v ScheduleView) ScheduleInput {
	in := ScheduleInput{
		Kind: v.Kind, CronExpr: v.CronExpr, Timezone: v.Timezone, MisfirePolicy: v.MisfirePolicy,
		MisfireGraceSeconds: v.MisfireGraceSeconds, MaxCatchUp: v.MaxCatchUp, JitterSeconds: v.JitterSeconds,
	}
	if v.OneTimeAt != nil {
		in.OneTimeAt = *v.OneTimeAt
	}
	if v.StartsAt != nil {
		in.StartsAt = *v.StartsAt
	}
	if v.EndsAt != nil {
		in.EndsAt = *v.EndsAt
	}
	return in
}

// DeleteSchedule soft-deletes a schedule (deleted_at set): the due-scan skips it while its occurrence rows
// + linked deliveries stay queryable under retention (B9). Returns found=false when absent.
func (s *ScheduleStore) DeleteSchedule(ctx context.Context, org, project, id string) (bool, error) {
	switch err := s.pool.QueryRow(ctx, storage.Query("SoftDeleteSchedule"), id, org, project).Scan(new(string)); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("soft-delete schedule: %w", err)
	}
	return true, nil
}

// GetSchedule reads a schedule's management projection, or found=false when absent (or soft-deleted).
func (s *ScheduleStore) GetSchedule(ctx context.Context, org, project, id string) (ScheduleView, bool, error) {
	var v ScheduleView
	switch err := s.pool.QueryRow(ctx, storage.Query("GetSchedule"), id, org, project).Scan(
		&v.ID, &v.Name, &v.TriggerID, &v.Kind, &v.CronExpr, &v.Timezone, &v.MisfirePolicy, &v.MisfireGraceSeconds,
		&v.MaxCatchUp, &v.JitterSeconds, &v.Status, &v.StatusReason, &v.Revision, &v.NextFireAt, &v.OneTimeAt,
		&v.StartsAt, &v.EndsAt, &v.CreatedAt, &v.UpdatedAt); {
	case errors.Is(err, pgx.ErrNoRows):
		return ScheduleView{}, false, nil
	case err != nil:
		return ScheduleView{}, false, fmt.Errorf("read schedule: %w", err)
	}
	return v, true, nil
}

// ListOccurrences reads a schedule's occurrences newest-first (tenant-scoped through the parent schedule).
func (s *ScheduleStore) ListOccurrences(ctx context.Context, org, project, id string, limit int) ([]OccurrenceView, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, storage.Query("ListScheduleOccurrences"), id, org, project, limit)
	if err != nil {
		return nil, fmt.Errorf("list occurrences: %w", err)
	}
	defer rows.Close()
	var out []OccurrenceView
	for rows.Next() {
		var o OccurrenceView
		if err := rows.Scan(&o.OccurrenceID, &o.Revision, &o.PlannedAt, &o.AdmittedAt, &o.State, &o.DeliveryID, &o.Reason); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// validate is the create gate: it verifies the name/trigger are present and the trigger is in the tenant's
// scope (a schedule can never fire a foreign trigger), then the firing shape (validateFiring). It is
// fail-closed.
func (s *ScheduleStore) validate(ctx context.Context, org, project string, in ScheduleInput) (scheduleSpec, error) {
	if in.Name == "" || in.TriggerID == "" {
		return scheduleSpec{}, fmt.Errorf("%w: name and trigger_id are required", ErrScheduleInvalid)
	}
	if _, err := s.triggers.triggerEnabled(ctx, org, project, in.TriggerID); err != nil {
		return scheduleSpec{}, err // ErrTriggerNotFound (foreign/unknown) — a schedule cannot fire it
	}
	return validateFiring(in)
}

// validateFiring parses + bounds a schedule's FIRING config into a scheduleSpec: the timezone
// (LoadLocation — an unknown IANA name is a 400, never a stored row), and the cron expr (kind=cron) or
// one-time instant (kind=one_time). Shared by create and revise (revise never touches name/trigger/scope).
func validateFiring(in ScheduleInput) (scheduleSpec, error) {
	// Bound the numeric knobs app-side so an out-of-range value is a 400, not a DB-CHECK 500 (m-api). The
	// DB CHECKs remain the last line of defense.
	if in.MaxCatchUp < 0 || in.MaxCatchUp > 100 {
		return scheduleSpec{}, fmt.Errorf("%w: max_catch_up must be 0..100", ErrScheduleInvalid)
	}
	if in.JitterSeconds < 0 || in.JitterSeconds > 3600 {
		return scheduleSpec{}, fmt.Errorf("%w: jitter_seconds must be 0..3600", ErrScheduleInvalid)
	}
	if in.MisfireGraceSeconds < 0 {
		return scheduleSpec{}, fmt.Errorf("%w: misfire_grace_seconds must be non-negative", ErrScheduleInvalid)
	}
	loc, err := time.LoadLocation(in.Timezone)
	if err != nil {
		return scheduleSpec{}, fmt.Errorf("%w: %q", ErrInvalidTimezone, in.Timezone)
	}
	spec := scheduleSpec{
		kind: kindOrDefault(in.Kind), loc: loc,
		misfirePolicy: policyOrDefault(in.MisfirePolicy),
		grace:         time.Duration(graceOrDefault(in.MisfireGraceSeconds)) * time.Second,
		maxCatchUp:    in.MaxCatchUp,
	}
	if !in.StartsAt.IsZero() {
		spec.startsAt = in.StartsAt.UTC()
	}
	if !in.EndsAt.IsZero() {
		spec.endsAt = in.EndsAt.UTC()
	}
	switch spec.kind {
	case "one_time":
		if in.OneTimeAt.IsZero() {
			return scheduleSpec{}, fmt.Errorf("%w: one_time schedule needs one_time_at", ErrScheduleInvalid)
		}
		spec.oneTimeAt = in.OneTimeAt.UTC()
	default: // cron
		c, err := ParseCron(in.CronExpr)
		if err != nil {
			return scheduleSpec{}, err
		}
		spec.cron = c
	}
	return spec, nil
}

// resolveSpec rebuilds a scheduleSpec from a due-scan row (the ticker path). A bad row (unparseable
// timezone/cron — impossible for a create-validated schedule, but defensive) surfaces as an error the
// caller logs + skips.
func resolveSpec(kind, cronExpr, timezone, misfirePolicy string, graceSeconds, maxCatchUp int, endsAt *time.Time) (scheduleSpec, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return scheduleSpec{}, fmt.Errorf("%w: %q", ErrInvalidTimezone, timezone)
	}
	spec := scheduleSpec{
		kind: kind, loc: loc, misfirePolicy: misfirePolicy,
		grace: time.Duration(graceSeconds) * time.Second, maxCatchUp: maxCatchUp,
	}
	if endsAt != nil {
		spec.endsAt = endsAt.UTC()
	}
	if kind == "cron" {
		c, err := ParseCron(cronExpr)
		if err != nil {
			return scheduleSpec{}, err
		}
		spec.cron = c
	}
	return spec, nil
}

// dueSchedule is one row of the ticker's due-scan.
type dueSchedule struct {
	id, org, project, triggerID, createdBy, kind, cronExpr, timezone, misfirePolicy string
	graceSeconds, maxCatchUp, jitterSeconds, revision                               int
	endsAt                                                                          *time.Time
	nextFireAt                                                                      time.Time
}

// fireDueSchedules is the ticker's due-scan phase: it claims occurrences (durably committed 'pending')
// and advances next_fire_at per the misfire policy, for every schedule due at `now`. Admission is a
// SEPARATE phase (sweepPendingOccurrences), so the occurrence is durable BEFORE any run is born (§5). A
// poison schedule (bad row / claim error) is logged and SKIPPED, never returned — one bad schedule must
// not wedge the whole sweep behind a supervisor restart loop (the delivery-reconciler discipline).
//
// ponytail: the due-scan takes NO row lock (no FOR UPDATE SKIP LOCKED). Correctness is the occurrence
// UNIQUE(schedule_id, revision, planned_at) index — two replicas that both process a due schedule target
// the same deterministic occurrence_id, and ON CONFLICT DO NOTHING collapses them to one row; redundant
// compute at trigger cadence is cheap. Add SKIP LOCKED as a contention optimization only if the due-scan
// ever profiles hot.
func (s *ScheduleStore) fireDueSchedules(ctx context.Context, now time.Time, limit int, log func(string, ...any)) error {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, storage.Query("DueSchedules"), now, limit)
	if err != nil {
		return fmt.Errorf("due-scan: %w", err)
	}
	var due []dueSchedule
	for rows.Next() {
		var d dueSchedule
		if err := rows.Scan(&d.id, &d.org, &d.project, &d.triggerID, &d.createdBy, &d.kind, &d.cronExpr,
			&d.timezone, &d.misfirePolicy, &d.graceSeconds, &d.maxCatchUp, &d.jitterSeconds, &d.endsAt,
			&d.revision, &d.nextFireAt); err != nil {
			rows.Close()
			return err
		}
		due = append(due, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range due {
		if err := s.fireOne(ctx, d, now); err != nil {
			log("schedule-ticker: fire schedule %s: %v", d.id, err)
		}
	}
	return nil
}

// fireOne applies one due schedule's plan in a single guarded transaction: claim the firing occurrences
// (ON CONFLICT DO NOTHING), record the ONE windowed-skip row if any, then advance next_fire_at (or fail
// the schedule) guarded on (revision, the read next_fire_at) so only the first replica to advance wins.
func (s *ScheduleStore) fireOne(ctx context.Context, d dueSchedule, now time.Time) error {
	spec, err := resolveSpec(d.kind, d.cronExpr, d.timezone, d.misfirePolicy, d.graceSeconds, d.maxCatchUp, d.endsAt)
	if err != nil {
		return err
	}
	plan := planTick(spec, d.nextFireAt, now)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, t := range plan.fire {
		occID := OccurrenceID(d.id, d.revision, t)
		if _, err := tx.Exec(ctx, storage.Query("ClaimOccurrence"), occID, d.id, d.revision, t.UTC()); err != nil {
			return fmt.Errorf("claim occurrence: %w", err)
		}
	}
	if plan.skipCount > 0 {
		occID := OccurrenceID(d.id, d.revision, plan.skipTo)
		reason := skipReason(plan.skipFrom, plan.skipTo, plan.skipCount)
		if _, err := tx.Exec(ctx, storage.Query("RecordSkipWindow"), occID, d.id, d.revision, plan.skipTo.UTC(), reason); err != nil {
			return fmt.Errorf("record skip window: %w", err)
		}
	}
	if plan.fail {
		if _, err := tx.Exec(ctx, storage.Query("FailSchedule"), d.id, d.org, d.project, plan.failReason, d.nextFireAt); err != nil {
			return fmt.Errorf("fail schedule: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, storage.Query("AdvanceNextFireAt"), d.id, d.org, d.project, d.revision, d.nextFireAt, nullableTime(plan.nextFireAt)); err != nil {
			return fmt.Errorf("advance next_fire_at: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// pendingOccurrence is one row of the handoff sweep.
type pendingOccurrence struct {
	occurrenceID, scheduleID, org, project, triggerID, createdBy string
	revision, jitterSeconds                                      int
	plannedAt                                                    time.Time
	endsAt                                                       *time.Time
}

// sweepPendingOccurrences is the ticker's handoff phase: it admits every occurrence durably committed
// 'pending' (a fresh claim, a crash between claim-commit and admission, or a jitter-delayed one) through
// CreateScheduledDelivery — the SAME §20.2.2 pipeline, with the occurrence_id as the dedupe_key, so a
// double handoff collapses to one run. A jitter-gated occurrence whose admit-after instant is still future
// is left pending for a later tick. A poison occurrence is logged and SKIPPED.
func (s *ScheduleStore) sweepPendingOccurrences(ctx context.Context, now time.Time, limit int, log func(string, ...any)) error {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, storage.Query("PendingOccurrences"), limit)
	if err != nil {
		return fmt.Errorf("pending sweep: %w", err)
	}
	var pending []pendingOccurrence
	for rows.Next() {
		var p pendingOccurrence
		if err := rows.Scan(&p.occurrenceID, &p.scheduleID, &p.revision, &p.plannedAt,
			&p.org, &p.project, &p.triggerID, &p.createdBy, &p.jitterSeconds, &p.endsAt); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range pending {
		if err := s.admitOccurrence(ctx, p, now); err != nil {
			log("schedule-ticker: admit occurrence %s: %v", p.occurrenceID, err)
		}
	}
	return nil
}

// admitOccurrence hands one pending occurrence to the delivery pipeline, then records the outcome. It is a
// no-op until the occurrence's jitter admit-after instant is reached, so admission is jittered + never
// lands beyond ends_at (§33.5). A trigger-config failure (disabled/no-revision) fails the occurrence; a
// handed-off delivery (run/deferred/duplicate/skip) marks it admitted with the canonical delivery id.
func (s *ScheduleStore) admitOccurrence(ctx context.Context, p pendingOccurrence, now time.Time) error {
	var endsAt time.Time
	if p.endsAt != nil {
		endsAt = p.endsAt.UTC()
	}
	if now.Before(admitAfter(p.occurrenceID, p.plannedAt, p.jitterSeconds, endsAt)) {
		return nil // jitter window not yet open — leave pending for a later tick
	}
	payload := scheduledPayload(p)
	del, err := s.triggers.CreateScheduledDelivery(ctx, p.org, p.project, p.createdBy, p.triggerID, p.occurrenceID, payload)
	switch {
	case errors.Is(err, ErrTriggerDisabled), errors.Is(err, ErrNoActiveRevision), errors.Is(err, ErrTriggerNotFound):
		_, e := s.pool.Exec(ctx, storage.Query("MarkOccurrenceFailed"), p.occurrenceID, err.Error())
		return e
	case err != nil:
		return err // transient — leave pending, retry next sweep
	}
	if del.State == "failed" || del.State == "rejected" {
		_, e := s.pool.Exec(ctx, storage.Query("MarkOccurrenceFailed"), p.occurrenceID, del.Reason)
		return e
	}
	deliveryID := del.ID
	if del.State == "duplicate" && del.DuplicateOf != "" {
		deliveryID = del.DuplicateOf // the canonical delivery the occurrence already handed off to
	}
	_, e := s.pool.Exec(ctx, storage.Query("MarkOccurrenceAdmitted"), p.occurrenceID, deliveryID)
	return e
}

// scheduledPayload is the synthetic source object a scheduled firing carries — the occurrence context the
// trigger's input mapping may select from (planned_at, occurrence_id, revision).
func scheduledPayload(p pendingOccurrence) []byte {
	body, _ := json.Marshal(map[string]any{
		"schedule_id":   p.scheduleID,
		"occurrence_id": p.occurrenceID,
		"planned_at":    p.plannedAt.UTC().Format(time.RFC3339),
		"revision":      p.revision,
	})
	return body
}

// skipReason encodes a windowed skip as {from,to,count} JSON (the ONE record a misfire skip materializes).
func skipReason(from, to time.Time, count int) string {
	body, _ := json.Marshal(map[string]any{
		"from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339), "count": count,
	})
	return string(body)
}

// createBase is the "fire strictly after" instant a fresh/revised schedule computes its first fire from:
// max(now, starts_at), nudged back a tick so an instant landing exactly on the boundary still fires.
func createBase(in ScheduleInput, now time.Time) time.Time {
	base := now
	if !in.StartsAt.IsZero() && in.StartsAt.After(now) {
		base = in.StartsAt
	}
	return base.Add(-time.Nanosecond)
}

func kindOrDefault(kind string) string {
	if kind == "" {
		return "cron"
	}
	return kind
}

func policyOrDefault(policy string) string {
	if policy == "" {
		return "fire_once_now"
	}
	return policy
}

func graceOrDefault(seconds int) int {
	if seconds <= 0 {
		return 300
	}
	return seconds
}

// nullableTime keeps a zero time NULL and a set time a stored value (the nullable TIMESTAMPTZ columns).
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
