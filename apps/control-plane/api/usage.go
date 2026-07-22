package api

import (
	"context"
	"io"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// UsageAPI is the store seam for the metering surface (spec §43, E13 Task 6): the durable budget/quota
// limits admission enforces, and the tenant-scoped visibility into what has actually been settled. The
// Postgres-backed internal/metering Store implements it; production wires it via WithUsage.
//
// Every method is scoped by the verified identity, never a body field — a limit is created for the
// caller's OWN scope, so an org-scoped key sets an organization-wide limit and a project-scoped key sets
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
}

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
	rows, err := h.usage.ListUsageLedger(r.Context(), scope, ListQuery{
		After: q.After, Limit: q.Limit + 1, CreatedGTE: q.CreatedGTE, CreatedLTE: q.CreatedLTE,
	})
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	renderPage(w, r, usageLedgerKind, scope, rows, q.Limit)
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
