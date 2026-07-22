package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/storage"
)

// Usage metering (spec §43, E13 Task 6): the settlement half of the append-only usage_ledger and the
// durable budget/quota gate admission reads against it (migration 000032).
//
// HONEST CEILING — METERING ONLY. This records WHAT was consumed. It does not price it: there is no
// price revision, no invoice, no compensating adjustment, and no billing exporter here (BIL-004/005/006
// are E13-H/SaaS). The ledger is deliberately self-sufficient enough that an external exporter can price
// it by reading the table alone, and the core stays ignorant of every billing concept.
//
// NOT METERED YET, and named so the gap is a decision rather than a surprise: tool-call, sandbox
// vCPU/memory-second, workspace/artifact GiB-time, and network egress (all §43.2 dimensions), plus the
// MCP sampling path, which routes the broker directly and commits no model_requests row. Each is
// additive — a new meter string through the same seam, no schema change.
const (
	// meterRunAdmitted counts runs at ADMISSION. It is the reservation: the row lands inside the
	// admission transaction, before the run executes, so a run-count quota counts runs that have been
	// admitted rather than runs that have already finished paying.
	meterRunAdmitted = "run.admitted"
	// The model meters, split by direction because an exporter prices input and output differently —
	// that split is the whole reason a ledger is more than a token counter. A total is the SUM of these
	// two and is deliberately NOT a third row: a roll-up row would double-count any prefix that covers
	// both, which is exactly what budgets/quotas do.
	meterInputTokens  = "model.input_tokens"
	meterOutputTokens = "model.output_tokens"

	unitToken = "token"
	unitRun   = "run"
)

// usageEntry is one settled meter fact bound for the ledger. dedupeKey is derived by the caller from
// the settling operation's own identity (the model request, the run), never from a clock or a random —
// that determinism is what makes a redelivery settle exactly once (BIL-001).
type usageEntry struct {
	sessionID string
	runID     string
	meter     string
	unit      string
	dedupeKey string
	quantity  int64
}

// ledgerID derives a ledger row's stable identity from the tenant and the dedupe key, so the SAME fact
// re-settled produces the SAME primary key. The tenant is folded in because dedupe keys are only unique
// within a tenant, and the id must be unique across the installation.
func ledgerID(tenant Tenant, dedupeKey string) string {
	sum := sha256.Sum256([]byte(tenant.Organization + "\x00" + tenant.Project + "\x00" + dedupeKey))
	return "use_" + hex.EncodeToString(sum[:12])
}

// settleUsage appends entries to the ledger inside the caller's transaction, so a meter is durable
// exactly when the fact it meters is. A zero-quantity entry is skipped rather than stored: an empty row
// prices to nothing and only dilutes the ledger.
func settleUsage(ctx context.Context, tx pgx.Tx, tenant Tenant, entries ...usageEntry) error {
	for _, e := range entries {
		if e.quantity <= 0 {
			continue
		}
		if _, err := tx.Exec(ctx, storage.Query("SettleUsage"),
			ledgerID(tenant, e.dedupeKey), tenant.Organization, tenant.Project,
			e.sessionID, e.runID, e.meter, e.quantity, e.dedupeKey, e.unit); err != nil {
			return fmt.Errorf("settle usage %s: %w", e.meter, err)
		}
	}
	return nil
}

// modelUsageEntries turns one model step's provider usage into its settled ledger entries. The dedupe
// key is the model request's own id, which is stable across attempts and redeliveries (the same id the
// provider idempotency key is derived from), so re-committing a step settles nothing new.
func modelUsageEntries(sessionID, runID, requestID string, usage contracts.Usage) []usageEntry {
	entry := func(meter string, quantity int) usageEntry {
		return usageEntry{
			sessionID: sessionID, runID: runID, meter: meter, unit: unitToken,
			dedupeKey: "mreq:" + requestID + ":" + meter, quantity: int64(quantity),
		}
	}
	return []usageEntry{entry(meterInputTokens, usage.InputTokens), entry(meterOutputTokens, usage.OutputTokens)}
}

// LimitExceeded describes the durable limit that refused an admission, in the terms the caller needs to
// remediate: which limit, how it is denominated, what it allows, what has been used, and — for a quota,
// whose window releases capacity on its own — when that next happens. A budget has no ResetAt because a
// budget does not reset: it is raised, or the period is moved.
type LimitExceeded struct {
	// Kind is "budget" (cumulative since a period start) or "quota" (a rolling window).
	Kind        string
	MeterPrefix string
	Limit       float64
	Used        float64
	ResetAt     *time.Time
}

// checkDurableLimits reports the first budget or quota the caller has already exhausted, or nil when
// every configured limit still has headroom (including the common case of none configured at all: both
// queries return no row against empty tables).
//
// ponytail: two small aggregate reads per fresh admission, under ReadCommitted with no row lock. Two
// admissions racing the exact limit boundary can therefore BOTH pass — the ledger stays exact, but the
// gate is accurate to ±the runs in flight. That is the documented variance BIL-003 allows, and it is the
// honest one for a token budget in any case: a run's token spend is unknown until it settles, so a
// single in-flight run can always overshoot by its own usage. Upgrade path when a hard boundary is
// required: SELECT the matching limit rows FOR UPDATE first, which serializes admissions per limit.
func checkDurableLimits(ctx context.Context, tx pgx.Tx, tenant Tenant) (*LimitExceeded, error) {
	out := LimitExceeded{Kind: "budget"}
	var periodStart time.Time
	switch err := tx.QueryRow(ctx, storage.Query("ExhaustedBudget"), tenant.Organization, tenant.Project).
		Scan(&out.MeterPrefix, &out.Limit, &out.Used, &periodStart); {
	case err == nil:
		return &out, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("read exhausted budget: %w", err)
	}

	out = LimitExceeded{Kind: "quota"}
	var windowSeconds int64
	var oldest *time.Time
	switch err := tx.QueryRow(ctx, storage.Query("ExhaustedQuota"), tenant.Organization, tenant.Project).
		Scan(&out.MeterPrefix, &out.Limit, &out.Used, &windowSeconds, &oldest); {
	case err == nil:
		// The oldest in-window row is the first to age out, so that is when capacity next releases. It
		// is always present on an exhausted quota (a quota can only be exhausted by rows in its window).
		if oldest != nil {
			reset := oldest.Add(time.Duration(windowSeconds) * time.Second)
			out.ResetAt = &reset
		}
		return &out, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("read exhausted quota: %w", err)
	}
	return nil, nil
}
