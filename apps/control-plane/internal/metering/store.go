// Package metering is the durable store behind the usage surface (spec §43, E13 Task 6,
// BIL-001/BIL-003/QUO-001): the read half of the append-only usage_ledger, and the write half of the
// budgets/quotas limits the ADMISSION transaction enforces. The settlement writes live where the facts
// are committed (packages/coordinator), not here — a metering package that could also write the ledger
// would be a second, unsynchronized path into a table whose whole value is being the single record.
//
// SCOPING: every method is bound to the identity the caller was verified as. Migration 000032 secures
// these tables at the ORGANIZATION level on purpose (so an org-wide limit can be summed from the
// project-narrowed connection that admits a run), so the intra-organization project narrowing is done
// explicitly in SQL here — an org-scoped key sees its whole organization, a project-scoped key sees the
// organization-wide limits plus its own project's.
//
// HONEST CEILING — METERING ONLY: no price revision, no invoice, no compensating adjustment, no
// BYOK platform/provider split, and no billing exporter (BIL-004/005/006 → E13-H/SaaS). What is here is
// enough that an external exporter can price the ledger by reading it.
package metering

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/storage"
)

// Store serves the metering surface over the durable spine's pool.
type Store struct {
	pool *pgxpool.Pool
}

// New builds a metering store over the durable spine's pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

type budgetView struct {
	ID            string    `json:"id"`
	Object        string    `json:"object"`
	ProjectID     string    `json:"project_id"`
	MeterPrefix   string    `json:"meter_prefix"`
	LimitQuantity float64   `json:"limit_quantity"`
	PeriodStart   time.Time `json:"period_start"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type quotaView struct {
	ID            string    `json:"id"`
	Object        string    `json:"object"`
	ProjectID     string    `json:"project_id"`
	MeterPrefix   string    `json:"meter_prefix"`
	LimitQuantity float64   `json:"limit_quantity"`
	WindowSeconds int64     `json:"window_seconds"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// meterTotal is one line of the summary: what a meter has accumulated in the caller's scope.
type meterTotal struct {
	Meter    string  `json:"meter"`
	Unit     string  `json:"unit"`
	Quantity float64 `json:"quantity"`
	Entries  int64   `json:"entries"`
}

// summaryView answers "how much have I used, and how close am I to my limits?" in one read — the totals
// and the limits they are measured against, so a client needs no second call to interpret the first.
type summaryView struct {
	Object         string       `json:"object"`
	OrganizationID string       `json:"organization_id"`
	ProjectID      string       `json:"project_id"`
	Meters         []meterTotal `json:"meters"`
	Budgets        []budgetView `json:"budgets"`
	Quotas         []quotaView  `json:"quotas"`
}

// ledgerEntryView is one settled row as the ledger page renders it — the shape an external billing
// exporter reads. schema_version travels WITH the row (not just in the docs) so an exporter can tell
// which field contract produced an entry it did not write.
type ledgerEntryView struct {
	ID            string    `json:"id"`
	Object        string    `json:"object"`
	SchemaVersion int       `json:"schema_version"`
	ProjectID     string    `json:"project_id"`
	SessionID     string    `json:"session_id,omitempty"`
	RunID         string    `json:"run_id,omitempty"`
	Meter         string    `json:"meter"`
	Quantity      float64   `json:"quantity"`
	Unit          string    `json:"unit"`
	OccurredAt    time.Time `json:"occurred_at"`
}

type listView struct {
	Object string `json:"object"`
	Data   any    `json:"data"`
}

// SetBudget upserts a cumulative spend cap for the caller's own scope. The scope is NOT a body field: an
// org-scoped key sets an organization-wide limit and a project-scoped key sets one for its project, so a
// request can never cap someone else's tenant.
func (s *Store) SetBudget(ctx context.Context, scope middleware.Scope, body []byte) (api.ProvisionResult, error) {
	var in struct {
		MeterPrefix   *string  `json:"meter_prefix"`
		LimitQuantity *float64 `json:"limit_quantity"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.MeterPrefix == nil {
		return api.ProvisionResult{MissingField: "meter_prefix"}, nil
	}
	if in.LimitQuantity == nil || *in.LimitQuantity <= 0 {
		return api.ProvisionResult{MissingField: "limit_quantity"}, nil
	}
	ctx = s.scoped(ctx, scope)
	v := budgetView{Object: "budget"}
	if err := s.pool.QueryRow(ctx, storage.Query("UpsertBudget"),
		middleware.NewID("bdg"), scope.Organization, scope.Project, *in.MeterPrefix, *in.LimitQuantity).
		Scan(&v.ID, &v.ProjectID, &v.MeterPrefix, &v.LimitQuantity, &v.PeriodStart, &v.UpdatedAt); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("upsert budget: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(v)}, nil
}

// SetQuota upserts a rolling-window cap for the caller's own scope. The window is part of the limit, so
// re-POSTing restates both it and the quantity.
func (s *Store) SetQuota(ctx context.Context, scope middleware.Scope, body []byte) (api.ProvisionResult, error) {
	var in struct {
		MeterPrefix   *string  `json:"meter_prefix"`
		LimitQuantity *float64 `json:"limit_quantity"`
		WindowSeconds *int64   `json:"window_seconds"`
	}
	if err := strictDecode(body, &in); err != nil {
		return api.ProvisionResult{BadField: true}, nil
	}
	if in.MeterPrefix == nil {
		return api.ProvisionResult{MissingField: "meter_prefix"}, nil
	}
	if in.LimitQuantity == nil || *in.LimitQuantity <= 0 {
		return api.ProvisionResult{MissingField: "limit_quantity"}, nil
	}
	if in.WindowSeconds == nil || *in.WindowSeconds <= 0 {
		return api.ProvisionResult{MissingField: "window_seconds"}, nil
	}
	ctx = s.scoped(ctx, scope)
	v := quotaView{Object: "quota"}
	if err := s.pool.QueryRow(ctx, storage.Query("UpsertQuota"),
		middleware.NewID("quo"), scope.Organization, scope.Project, *in.MeterPrefix, *in.LimitQuantity, *in.WindowSeconds).
		Scan(&v.ID, &v.ProjectID, &v.MeterPrefix, &v.LimitQuantity, &v.WindowSeconds, &v.UpdatedAt); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("upsert quota: %w", err)
	}
	return api.ProvisionResult{Body: mustJSON(v)}, nil
}

// ListBudgets returns every budget that binds the caller.
func (s *Store) ListBudgets(ctx context.Context, scope middleware.Scope) (api.ProvisionResult, error) {
	data, err := s.readBudgets(s.scoped(ctx, scope), scope)
	if err != nil {
		return api.ProvisionResult{}, err
	}
	return api.ProvisionResult{Body: mustJSON(listView{Object: "list", Data: data})}, nil
}

// ListQuotas returns every quota that binds the caller.
func (s *Store) ListQuotas(ctx context.Context, scope middleware.Scope) (api.ProvisionResult, error) {
	data, err := s.readQuotas(s.scoped(ctx, scope), scope)
	if err != nil {
		return api.ProvisionResult{}, err
	}
	return api.ProvisionResult{Body: mustJSON(listView{Object: "list", Data: data})}, nil
}

// UsageSummary totals the settled ledger per meter for the caller's scope and reports it alongside the
// limits those totals are measured against.
func (s *Store) UsageSummary(ctx context.Context, scope middleware.Scope) (api.ProvisionResult, error) {
	ctx = s.scoped(ctx, scope)
	rows, err := s.pool.Query(ctx, storage.Query("UsageTotals"), scope.Organization, scope.Project)
	if err != nil {
		return api.ProvisionResult{}, fmt.Errorf("read usage totals: %w", err)
	}
	defer rows.Close()
	out := summaryView{
		Object: "usage_summary", OrganizationID: scope.Organization, ProjectID: scope.Project,
		Meters: []meterTotal{},
	}
	for rows.Next() {
		var m meterTotal
		if err := rows.Scan(&m.Meter, &m.Unit, &m.Quantity, &m.Entries); err != nil {
			return api.ProvisionResult{}, fmt.Errorf("scan usage total: %w", err)
		}
		out.Meters = append(out.Meters, m)
	}
	if err := rows.Err(); err != nil {
		return api.ProvisionResult{}, fmt.Errorf("iterate usage totals: %w", err)
	}
	if out.Budgets, err = s.readBudgets(ctx, scope); err != nil {
		return api.ProvisionResult{}, err
	}
	if out.Quotas, err = s.readQuotas(ctx, scope); err != nil {
		return api.ProvisionResult{}, err
	}
	return api.ProvisionResult{Body: mustJSON(out)}, nil
}

// ListUsageLedger returns a keyset page of settled entries. The store fetches exactly the Limit the
// handler asked for (already the page size + 1), so has_more is decided without a second round trip.
func (s *Store) ListUsageLedger(ctx context.Context, scope middleware.Scope, q api.ListQuery) ([]api.ListRow, error) {
	ctx = s.scoped(ctx, scope)
	var afterTime *time.Time
	afterID := ""
	if q.After != nil {
		afterTime, afterID = &q.After.CreatedAt, q.After.ID
	}
	rows, err := s.pool.Query(ctx, storage.Query("ListUsageLedger"),
		scope.Organization, scope.Project, q.CreatedGTE, q.CreatedLTE, afterTime, afterID, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("list usage ledger: %w", err)
	}
	defer rows.Close()
	out := []api.ListRow{}
	for rows.Next() {
		v := ledgerEntryView{Object: "usage_ledger_entry"}
		var session, run *string
		if err := rows.Scan(&v.ID, &v.SchemaVersion, &v.ProjectID, &session, &run,
			&v.Meter, &v.Quantity, &v.Unit, &v.OccurredAt); err != nil {
			return nil, fmt.Errorf("scan usage ledger entry: %w", err)
		}
		if session != nil {
			v.SessionID = *session
		}
		if run != nil {
			v.RunID = *run
		}
		// The keyset coordinates are the ledger's own (occurred_at, id) — the same pair the shared
		// cursor carries for every other list.
		out = append(out, api.ListRow{ID: v.ID, CreatedAt: v.OccurredAt, Body: mustJSON(v)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage ledger: %w", err)
	}
	return out, nil
}

func (s *Store) readBudgets(ctx context.Context, scope middleware.Scope) ([]budgetView, error) {
	rows, err := s.pool.Query(ctx, storage.Query("ListBudgets"), scope.Organization, scope.Project)
	if err != nil {
		return nil, fmt.Errorf("list budgets: %w", err)
	}
	defer rows.Close()
	out := []budgetView{}
	for rows.Next() {
		v := budgetView{Object: "budget"}
		if err := rows.Scan(&v.ID, &v.ProjectID, &v.MeterPrefix, &v.LimitQuantity, &v.PeriodStart, &v.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan budget: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate budgets: %w", err)
	}
	return out, nil
}

func (s *Store) readQuotas(ctx context.Context, scope middleware.Scope) ([]quotaView, error) {
	rows, err := s.pool.Query(ctx, storage.Query("ListQuotas"), scope.Organization, scope.Project)
	if err != nil {
		return nil, fmt.Errorf("list quotas: %w", err)
	}
	defer rows.Close()
	out := []quotaView{}
	for rows.Next() {
		v := quotaView{Object: "quota"}
		if err := rows.Scan(&v.ID, &v.ProjectID, &v.MeterPrefix, &v.LimitQuantity, &v.WindowSeconds, &v.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan quota: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate quotas: %w", err)
	}
	return out, nil
}

// scoped binds the request to the verified tenant. On an HTTP request the auth middleware has already
// published that scope and it wins (ScopeToTenant yields to it), so this only matters for an internal
// caller driving the store directly — which then runs under the policies rather than around them.
func (s *Store) scoped(ctx context.Context, scope middleware.Scope) context.Context {
	return storage.ScopeToTenant(ctx, scope.Organization, scope.Project)
}

// strictDecode decodes body into v rejecting unknown fields (the E11 T1 pattern), so a misspelled limit
// field is a 400 rather than a silently-unset cap.
func strictDecode(body []byte, v any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("metering: marshal projection: %v", err))
	}
	return b
}

// compile-time proof the store serves the surface the router mounts.
var _ api.UsageAPI = (*Store)(nil)
