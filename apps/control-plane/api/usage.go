package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// UsageAPI is the store seam for the metering surface (spec §43, E13 Task 6): the durable budget/quota
// limits admission enforces, and the tenant-scoped visibility into what has actually been settled. The
// Postgres-backed internal/metering Store implements it; production wires it via WithUsage.
//
// Every method is scoped by the verified identity, never a body field — a limit is created for the
// caller's OWN scope, so a `system` key sets an installation-wide limit and a project-scoped key sets
// one for its project, and no request can name someone else's tenant.
//
// HONEST CEILING — METERING ONLY: no invoice, no price, no adjustment entry, no BYOK platform/provider
// split, no exporter (BIL-004/005/006 → E13-H/SaaS). This surface reports consumption and caps it.
type UsageAPI interface {
	// SetBudget and SetQuota are UPSERTS on (scope, meter prefix): a limit is durable config with the
	// meter prefix as its identity, so re-POSTing one restates it rather than minting a rival row.
	SetBudget(ctx context.Context, scope middleware.Scope, body []byte) (ProvisionResult, error)
	ListBudgets(ctx context.Context, scope middleware.Scope) (ProvisionResult, error)
	SetQuota(ctx context.Context, scope middleware.Scope, body []byte) (ProvisionResult, error)
	ListQuotas(ctx context.Context, scope middleware.Scope) (ProvisionResult, error)
	// UsageSummary totals the settled ledger per meter for the caller's scope, alongside the limits
	// those totals are measured against.
	UsageSummary(ctx context.Context, scope middleware.Scope) (ProvisionResult, error)
	// ListUsageLedger returns a keyset page of raw settled entries — the rows an external billing
	// exporter would read. It fetches Limit+1 so the handler detects a further page, exactly like every
	// other list on this surface.
	ListUsageLedger(ctx context.Context, scope middleware.Scope, q ListQuery) ([]ListRow, error)
	// UsageSeries totals the settled ledger per meter per TIME BUCKET for the caller's scope. It is the
	// chart source UsageSummary is not: the summary is a lifetime total with no window at all, and the
	// only other way to draw a line was to page the ledger client-side, which is a request loop whose
	// length grows with the tenant's spend. The handler has already validated and defaulted the query.
	UsageSeries(ctx context.Context, scope middleware.Scope, q UsageSeriesQuery) (ProvisionResult, error)
}

// UsageSeriesQuery is a resolved, already-validated bucketed-usage request. Every field is settled by
// the handler before the store sees it: Bucket is one of the two supported units (never caller text),
// Start/End are a bounded window with defaults applied, and Meter is "" for no predicate — never a
// value to match.
type UsageSeriesQuery struct {
	// Bucket is "hour" or "day". The store interpolates it into an interval literal, so it MUST NOT
	// reach the store unvalidated; parseSeriesQuery is the only thing that constructs it.
	Bucket string
	Start  time.Time
	End    time.Time
	Meter  string
}

// The bucket vocabulary. It is a closed set of two rather than an arbitrary interval, and the reason is
// not that parsing "15 minutes" is hard — it is that every other decision on this route is SIZED to the
// bucket. The window ceiling below is expressed as a bucket count, the zero-fill emits one point per
// bucket per meter, and migration 000049's measurement covers exactly these two shapes. An arbitrary
// interval would make the response size, the query cost and the row ceiling all functions of a string
// the caller chose. "hour" and "day" answer the two questions the dashboard actually asks ("spent in
// the last hour", "spent today"); a caller wanting weeks sums days client-side, which is exact because
// day buckets nest inside weeks. Anything else is a 400 naming both accepted values.
const (
	seriesBucketHour = "hour"
	seriesBucketDay  = "day"
)

// The window ceiling, per bucket. An unbounded created_after over a large ledger is a full scan, and
// the zero-fill makes the RESPONSE grow with the window too, so the span is capped in both directions
// by the same constant. The caps are chosen so neither shape exceeds ~200 points per meter, which is
// also the widest window migration 000049 measured (7-day hour: 7.602 ms; 92-day day: 4.173 ms).
//
// Exceeding the cap is a 400 that names the limit, in this surface's existing idiom — the same shape as
// "limit must be an integer between 1 and 100". It is deliberately NOT a silent clamp to the maximum: a
// clamped window returns a chart that is not the chart the caller asked for, under a heading that says
// it is, and this repository has paid for accepted-and-ignored parameters more than once.
const (
	maxSeriesSpanHour = 7 * 24 * time.Hour  // 168 hourly buckets
	maxSeriesSpanDay  = 92 * 24 * time.Hour // 92 daily buckets
	// The default span when created_after is omitted. Deliberately NARROWER than the ceiling: the
	// default request should not be the most expensive one the route allows.
	defaultSeriesSpanHour = 24 * time.Hour
	defaultSeriesSpanDay  = 30 * 24 * time.Hour
)

// usageLedgerKind names the ledger list for the shared tenant-bound cursor, so a cursor minted here can
// never be replayed on another list (and vice versa).
const usageLedgerKind = "usage-ledger"

// usageHandler renders the metering routes. Setting a limit is gated on the same `provision` capability
// as tenancy and secret administration; READING usage deliberately is not — a tenant's own key must be
// able to see what that tenant has spent, or the metering is invisible to the party paying for it.
type usageHandler struct {
	usage UsageAPI
}

func (h *usageHandler) setBudget(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.beginWrite(w, r)
	if !ok {
		return
	}
	out, err := h.usage.SetBudget(r.Context(), scope, raw)
	h.write(w, r, out, err)
}

func (h *usageHandler) listBudgets(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		unauthenticated(w, r)
		return
	}
	out, err := h.usage.ListBudgets(r.Context(), scope)
	h.write(w, r, out, err)
}

func (h *usageHandler) setQuota(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.beginWrite(w, r)
	if !ok {
		return
	}
	out, err := h.usage.SetQuota(r.Context(), scope, raw)
	h.write(w, r, out, err)
}

func (h *usageHandler) listQuotas(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		unauthenticated(w, r)
		return
	}
	out, err := h.usage.ListQuotas(r.Context(), scope)
	h.write(w, r, out, err)
}

// summary is the metering-visibility read: per-meter totals for the caller's scope plus the limits they
// are measured against, so a client can answer "how much have I used, and how close am I?" in one call.
func (h *usageHandler) summary(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		unauthenticated(w, r)
		return
	}
	out, err := h.usage.UsageSummary(r.Context(), scope)
	h.write(w, r, out, err)
}

// ledger is the raw settled-entry page. It reuses the shared keyset parse/render, so it pages, filters,
// and rejects a foreign cursor identically to every other list on this surface.
func (h *usageHandler) ledger(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		unauthenticated(w, r)
		return
	}
	q, ok := beginList(w, r, usageLedgerKind, scope)
	if !ok {
		return
	}
	// session_id and meter are parsed HERE rather than in beginList, and the placement is the point: the
	// shared parse serves every list on this surface, so a filter read there would be accepted on all of
	// them and honoured by one. A filter that is accepted and ignored is worse than an absent one — it does
	// not return nothing, it returns other rows under a heading naming one session, and the operator reads
	// the heading. Empty means unfiltered; the query treats '' as "no predicate", never as a match.
	rows, err := h.usage.ListUsageLedger(r.Context(), scope, ListQuery{
		After: q.After, Limit: q.Limit + 1, CreatedGTE: q.CreatedGTE, CreatedLTE: q.CreatedLTE,
		SessionID: r.URL.Query().Get("session_id"), Meter: r.URL.Query().Get("meter"),
	})
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	renderPage(w, r, usageLedgerKind, scope, rows, q.Limit)
}

// series is the bucketed chart read: quantity per meter per bucket over a bounded window. It is its own
// route rather than a `bucket=` mode on GET /v1/usage, and the choice is recorded here because the
// alternative was cheaper to write and worse to live with. /v1/usage returns ONE published object,
// `usage_summary`, carrying meters/budgets/quotas; adding buckets to it would make the shape of an
// already-shipped response depend on a query parameter — one object with two meanings, a union in all
// three SDKs, and a client that forgets the parameter silently reading lifetime totals as if they were
// a window. A separate route costs one more path and leaves every existing caller's response identical.
func (h *usageHandler) series(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		unauthenticated(w, r)
		return
	}
	q, ok := parseSeriesQuery(w, r)
	if !ok {
		return
	}
	out, err := h.usage.UsageSeries(r.Context(), scope, q)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Body)
}

// parseSeriesQuery validates and defaults the series window. It is the ONLY constructor of a
// UsageSeriesQuery reaching the store, which is what lets the store interpolate the bucket into an
// interval literal: the value there can only ever be one of two constants this function wrote.
//
// The window params are `created_after`/`created_before` — the same names, and the same RFC3339
// parser, the ledger page already uses. A dashboard drawing a chart and then drilling into the rows
// behind it must not have to rename its window between the two calls.
func parseSeriesQuery(w http.ResponseWriter, r *http.Request) (UsageSeriesQuery, bool) {
	bad := func(detail string) (UsageSeriesQuery, bool) {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", detail)
		return UsageSeriesQuery{}, false
	}

	params := r.URL.Query()

	// bucket is REQUIRED rather than defaulted, and it is the one parameter on this surface that is.
	// `limit` may default because it changes how MANY rows come back; bucket changes what every number
	// MEANS. A caller who omits it and silently receives daily points has a chart that is wrong rather
	// than short, and nothing in the response would tell them.
	q := UsageSeriesQuery{Bucket: params.Get("bucket"), Meter: params.Get("meter")}
	var maxSpan, defaultSpan time.Duration
	switch q.Bucket {
	case seriesBucketHour:
		maxSpan, defaultSpan = maxSeriesSpanHour, defaultSeriesSpanHour
	case seriesBucketDay:
		maxSpan, defaultSpan = maxSeriesSpanDay, defaultSeriesSpanDay
	case "":
		return bad("bucket is required and must be one of hour, day")
	default:
		return bad("bucket must be one of hour, day")
	}

	after, err := parseTimeParam(r, "created_after")
	if err != nil {
		return bad("created_after must be an RFC3339 timestamp")
	}
	before, err := parseTimeParam(r, "created_before")
	if err != nil {
		return bad("created_before must be an RFC3339 timestamp")
	}
	// The window closes at created_before (default now) and opens defaultSpan before it. Resolving the
	// END first is what makes the default window relative to the window the caller actually asked about
	// rather than to the clock — `created_before=<last month>` with no created_after reads the span
	// BEFORE that instant, not a span ending today that contains none of it.
	q.End = time.Now().UTC()
	if before != nil {
		q.End = before.UTC()
	}
	q.Start = q.End.Add(-defaultSpan)
	if after != nil {
		q.Start = after.UTC()
	}

	if !q.Start.Before(q.End) {
		return bad("created_after must be earlier than created_before")
	}
	if q.End.Sub(q.Start) > maxSpan {
		return bad(fmt.Sprintf("the window between created_after and created_before must not exceed %d days when bucket is %s",
			int(maxSpan/(24*time.Hour)), q.Bucket))
	}
	return q, true
}

// beginWrite resolves the verified scope, enforces the provision capability, and reads the body. Only
// the limit-SETTING routes go through it.
func (h *usageHandler) beginWrite(w http.ResponseWriter, r *http.Request) (middleware.Scope, []byte, bool) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		unauthenticated(w, r)
		return middleware.Scope{}, nil, false
	}
	if !scope.HasScope(provisionScope) {
		middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope", "this API key lacks the provision capability")
		return middleware.Scope{}, nil, false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return middleware.Scope{}, nil, false
	}
	return scope, raw, true
}

// write renders a metering outcome: the typed rejects first, then 200 with the projection. Setting a
// limit answers 200, not 201: the POST is an upsert on a resource identified by its meter prefix, so a
// re-POST addresses the SAME resource — claiming a creation would be a lie half the time.
func (h *usageHandler) write(w http.ResponseWriter, r *http.Request, out ProvisionResult, err error) {
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	switch {
	case out.MissingField != "":
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", out.MissingField+" is required")
		return
	case out.BadField:
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body carries an unsupported field")
		return
	case out.NotFound:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such limit in this scope")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Body)
}

func unauthenticated(w http.ResponseWriter, r *http.Request) {
	middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
}
