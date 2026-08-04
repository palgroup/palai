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
// NOT METERED YET, and named so each gap is a decision rather than a surprise. Every one is additive —
// a new meter string through the same seam, no schema change:
//
//   - tool-call, sandbox vCPU/memory-second, workspace/artifact GiB-time, network egress (§43.2
//     dimensions). None is user-triggerable spend against the model budget.
//   - the MCP sampling path, which routes the broker directly and commits no model_requests row. It
//     bounds itself with a §25.18 Reservation, but that spend does NOT reach the ledger, so it does not
//     count against a budget.
//   - THE TOKENS OF AN INTERRUPTED STEP. An interrupt aborts the provider call mid-stream; the provider
//     bills the prompt and the partial completion, but its counts ride the final stream chunk a canceled
//     stream never receives, so no honest token number exists at that seam. CONSEQUENCE, stated plainly
//     because interrupting is user-triggerable: a tenant can spend real provider tokens that the budget
//     gate never sees, by interrupting steps. meterInterruptedStep records the step itself so the
//     behaviour is visible and cappable by a `step.` quota, but it is a COUNT of steps, not the tokens —
//     the token gap is real until the adapters surface partial usage on cancel.
//   - a CHILD run's admission is metered but not GATED (see CreateChildRun): a parent already past the
//     gate can spawn children that spend beyond an exhausted limit until it terminates.
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
	// The prompt-cache meters. TWO of them, for the same reason input and output are two: a cache READ
	// and a cache WRITE are priced differently by the providers that report both (a write costs MORE
	// than an uncached input token, a read costs a fraction of one), and a single collapsed
	// `model.cache_tokens` would fuse a premium into a discount irrecoverably — no reader could
	// un-sum it.
	//
	// READ THIS BEFORE DERIVING ANYTHING FROM THESE NUMBERS. The quantity settled is the provider's
	// OWN count, and the two provider families do not agree on what it is counted BESIDE:
	//
	//   provider-one (OpenAI), adapters/models/provider_one/adapter.go:141-147
	//       prompt_tokens INCLUDES prompt_tokens_details.cached_tokens, so model.cache_read_tokens is
	//       a SUBSET of model.input_tokens — the same tokens, counted on both meters. There is no
	//       cache-write counter on the wire at all (the provider caches on its own), so
	//       model.cache_write_tokens never has a row for this family.
	//   provider-two (Anthropic), adapters/models/provider_two/adapter.go:243-251
	//       input_tokens EXCLUDES both cache counters, so model.cache_read_tokens and
	//       model.cache_write_tokens are DISJOINT from model.input_tokens — additive to it.
	//
	// CONSEQUENCE, stated as the arithmetic because that is how a dashboard gets it wrong:
	// `model.input_tokens + model.cache_read_tokens` is the total prompt size for provider-two and
	// DOUBLE-COUNTS the cached prefix for provider-one. A cross-provider sum of that expression means
	// nothing. What IS well-defined for every family, and what a cache-savings figure actually needs,
	// is model.cache_read_tokens ALONE: tokens the provider served from cache and billed at its cache
	// rate. Price it, do not add it.
	//
	// Settlement deliberately does NOT normalize the two families onto one invariant, which it could
	// only do by re-basing model.input_tokens — the meter the durable budget gate reads. Subtracting
	// the cached prefix for provider-one would silently loosen every budget already configured, and
	// folding it in for provider-two would silently tighten them. The asymmetry is the providers', and
	// it is reported rather than laundered. A reader that needs to resolve it recovers the family the
	// same way it must already recover it to apply a PRICE at all — via model_request_id (000050) to
	// the step's own model.
	meterCacheReadTokens  = "model.cache_read_tokens"
	meterCacheWriteTokens = "model.cache_write_tokens"
	// meterInterruptedStep counts model steps aborted mid-flight by an interrupt. The provider bills the
	// prompt and the partial completion of an aborted streaming call, but its token counts arrive only in
	// the final stream chunk, which a canceled stream never reaches — so the tokens are genuinely unknown
	// here and this meter does NOT claim to be them. It records the fact that IS known, so interrupted
	// spend is visible in the ledger and cappable by a `step.` quota rather than invisible and unbounded.
	//
	// Its own prefix and its own unit are load-bearing: folding a step COUNT into a `model.` meter would
	// corrupt the token total the budget gate reads.
	meterInterruptedStep = "step.interrupted"

	unitToken = "token"
	unitRun   = "run"
	unitStep  = "step"
)

// interruptedStepEntry is the ledger record for a model step aborted mid-flight. It is keyed on the
// aborted step's own model_request_id, so a redelivered interrupt records it exactly once. Without that
// id there is no deterministic key, so the entry is left empty (quantity 0) and settleUsage skips it —
// recording an unkeyed row would double-count on every redelivery, which is worse than not recording it.
func interruptedStepEntry(sessionID, runID, requestID string) usageEntry {
	if requestID == "" {
		return usageEntry{}
	}
	return usageEntry{
		sessionID: sessionID, runID: runID, meter: meterInterruptedStep, unit: unitStep,
		dedupeKey: "mreq:" + requestID + ":" + meterInterruptedStep, quantity: 1,
		modelRequestID: requestID,
	}
}

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
	// modelRequestID names the TURN this settlement is attributed to, and is empty for the meters that
	// do not describe one (run.admitted is the admission reservation, settled before any model call
	// exists). It is not new information — the dedupe keys below have always been built from this same
	// id — it is that information promoted to a column, so a reader can join a cost to a turn without
	// string-parsing an idempotency detail. Because the identity was ALREADY per-step, storing it
	// changes nothing about which rows collide (migration 000050).
	modelRequestID string
}

// ledgerID derives a ledger row's stable identity from the tenant and the dedupe key, so the SAME fact
// re-settled produces the SAME primary key. The tenant is folded in because dedupe keys are only unique
// within a tenant, and the id must be unique across the installation.
//
// A.2 Task 6 dropped the organization from the hash, which changes the id a given fact derives. That is
// not a double-count risk: re-settlement is refused by SettleUsage's `ON CONFLICT (project_id,
// dedupe_key) DO NOTHING`, not by this id — the id only has to be unique, and rows written before the
// change keep the id they were written with.
func ledgerID(tenant Tenant, dedupeKey string) string {
	sum := sha256.Sum256([]byte(tenant.Project + "\x00" + dedupeKey))
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
			ledgerID(tenant, e.dedupeKey), tenant.Project,
			e.sessionID, e.runID, e.meter, e.quantity, e.dedupeKey, e.unit, e.modelRequestID); err != nil {
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
			modelRequestID: requestID,
		}
	}
	// The cache entries ride the same per-(step, meter) identity as the token entries, so they settle
	// exactly once on a redelivery like everything else. A provider that reports neither counter — or a
	// step that simply cached nothing — settles NO cache row: settleUsage skips a zero quantity, and an
	// absent meter is the honest record here. See the meter constants for what absent does NOT mean.
	return []usageEntry{
		entry(meterInputTokens, usage.InputTokens),
		entry(meterOutputTokens, usage.OutputTokens),
		entry(meterCacheReadTokens, usage.CacheReadTokens),
		entry(meterCacheWriteTokens, usage.CacheWriteTokens),
	}
}

// LimitExceeded describes the durable limit that refused an admission, in the terms the caller needs to
// remediate: which limit, how it is denominated, what it allows, what has been used, and — for a quota,
// whose window releases capacity on its own — when that next happens.
//
// A budget has no ResetAt because a budget never resets, and in THIS phase it never rolls over either:
// there is no delete route and no way to advance period_start, so a budget is effectively a LIFETIME cap
// whose only remediation is raising limit_quantity (which is what the 429 detail tells the caller, and
// all it tells them). Billing-period rollover and budget removal are E13-H — the period_start column is
// already there to carry them.
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
// WHEN SEVERAL ARE EXHAUSTED AT ONCE, THE NARROWEST ONE IS NAMED — the project limit ahead of the
// organization-wide one. That is the queries' ORDER BY, not this function's, and the reasoning lives
// beside it in storage/queries/usage.sql; the short of it is that a 429's remediation body must name a
// limit its reader can act on, and a project operator cannot raise their organization's budget. Before
// E29 T7 the ordering was not total and the reported row was whichever drew the smaller random id.
//
// It is a REPORTING guarantee, not an enforcement one. Every configured limit is enforced either way:
// each query's HAVING catches every exhausted row and any one of them refuses the admission. What is
// determined here is which one the caller is told about.
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
	switch err := tx.QueryRow(ctx, storage.Query("ExhaustedBudget"), tenant.Project).
		Scan(&out.MeterPrefix, &out.Limit, &out.Used, &periodStart); {
	case err == nil:
		return &out, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("read exhausted budget: %w", err)
	}

	out = LimitExceeded{Kind: "quota"}
	var windowSeconds int64
	var oldest *time.Time
	switch err := tx.QueryRow(ctx, storage.Query("ExhaustedQuota"), tenant.Project).
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
