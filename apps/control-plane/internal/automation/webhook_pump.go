// Package automation runs the outbound-webhook delivery pump (spec §21.4-21.6, E11 Task 4). The pump
// fans the event journal out to each registered endpoint, signs and delivers each event over the
// egress-safe sender, and drives the retry / dead-letter / redelivery state machine. It is a
// supervised background loop, independent of the run path — a delivery never blocks run completion
// (AUT-011 delivery half).
package automation

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
)

// PumpConfig carries the platform retry bounds (spec §21.4: "retry policy within platform bounds") and
// the loop cadence. BaseBackoff/MaxBackoff shape the jittered exponential curve; per-endpoint
// MaxAttempts and RetryWindow come from the endpoint row.
type PumpConfig struct {
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Tick        time.Duration
	BatchSize   int
}

func (c PumpConfig) withDefaults() PumpConfig {
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 30 * time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = time.Hour
	}
	if c.Tick <= 0 {
		c.Tick = time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	return c
}

// SecretResolver resolves an endpoint's SecretRef handle to the signing secret bytes at delivery time
// (the LP file-secret bridge in production; a static map in tests). The bytes never touch a log or the
// delivery row. A func type, not an interface — the two callers are a closure each.
type SecretResolver func(ref string) ([]byte, error)

// WebhookPump is the supervised delivery loop.
type WebhookPump struct {
	store   *WebhookStore
	sender  *webhook.Sender
	secrets SecretResolver
	cfg     PumpConfig
	now     func() time.Time
	log     func(string, ...any)
}

// NewWebhookPump wires the pump. secrets may be nil (an endpoint whose secret cannot be resolved then
// fails its attempt and retries — never an unsigned delivery). log may be nil.
func NewWebhookPump(store *WebhookStore, sender *webhook.Sender, secrets SecretResolver, cfg PumpConfig, log func(string, ...any)) *WebhookPump {
	return &WebhookPump{store: store, sender: sender, secrets: secrets, cfg: cfg.withDefaults(), now: time.Now, log: log}
}

// Run ticks the pump until ctx is cancelled. It returns a non-nil error only on a genuine failure, so
// the supervisor restarts it; a cancelled context is a clean shutdown. Each tick fans the journal out
// and drains the due deliveries, so one dead endpoint never starves another (per-row independence).
func (p *WebhookPump) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.cfg.Tick)
	defer ticker.Stop()
	for {
		if err := p.Tick(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Tick runs one fan-out + delivery pass. Exported so the component suite drives it deterministically.
func (p *WebhookPump) Tick(ctx context.Context) error {
	if err := p.fanOut(ctx); err != nil {
		return err
	}
	return p.deliverDue(ctx)
}

// fanOut materializes deliveries for every enabled endpoint from the journal past its cursor, then
// advances the cursor. ON CONFLICT dedupe makes it safe to re-run (a crash between insert and advance
// re-emits nothing).
func (p *WebhookPump) fanOut(ctx context.Context) error {
	endpoints, err := p.store.FanOutEndpoints(ctx)
	if err != nil {
		return fmt.Errorf("fan-out endpoints: %w", err)
	}
	for _, ep := range endpoints {
		events, err := p.store.ReadJournalForEndpoint(ctx, ep.Org, ep.Project, ep.Cursor, ep.Filter, p.cfg.BatchSize)
		if err != nil {
			return fmt.Errorf("read journal for endpoint %s: %w", ep.ID, err)
		}
		var high int64
		for _, ev := range events {
			body := buildEnvelope(ev, ep)
			if err := p.store.InsertDelivery(ctx, deliveryInsert{
				ID: newID("whd"), Org: ep.Org, Project: ep.Project, EndpointID: ep.ID,
				SessionID: ev.SessionID, EventID: ev.EventID, EventType: ev.Type, Payload: body,
			}); err != nil {
				return fmt.Errorf("insert delivery: %w", err)
			}
			high = ev.JournalID
		}
		if high > ep.Cursor {
			if err := p.store.AdvanceEndpointCursor(ctx, ep.ID, high); err != nil {
				return fmt.Errorf("advance cursor for endpoint %s: %w", ep.ID, err)
			}
		}
	}
	return nil
}

// deliverDue attempts every due delivery once. Each delivery is independent: a failure reschedules or
// dead-letters that one row and the loop moves on, so a permanently-down endpoint cannot block the
// deliveries queued for a healthy one (no head-of-line blocking).
func (p *WebhookPump) deliverDue(ctx context.Context) error {
	due, err := p.store.DueDeliveries(ctx, p.cfg.BatchSize)
	if err != nil {
		return fmt.Errorf("due deliveries: %w", err)
	}
	for _, d := range due {
		if err := p.attempt(ctx, d); err != nil {
			// A per-delivery attempt error (e.g. a store write) is logged and skipped, not fatal —
			// one bad row must not stall the whole queue. The row stays pending for the next tick.
			if p.log != nil {
				p.log("webhook delivery %s attempt error: %v", d.ID, err)
			}
		}
	}
	return nil
}

// attempt makes one signed HTTP attempt for a delivery and records its outcome. An unresolvable secret
// is treated as a retryable failure (the attempt records a transport error), never an unsigned send.
func (p *WebhookPump) attempt(ctx context.Context, d dueDelivery) error {
	attemptNo := d.AttemptCount + 1
	ts := p.now()

	var res webhook.Result
	sig, sendErr := p.sign(d, ts, attemptNo)
	if sendErr != nil {
		// Secret resolution failed: record a transport-style attempt and reschedule/dead-letter it.
		res = webhook.Result{Err: sendErr}
	} else {
		res = p.sender.Deliver(ctx, sig.dst, sig.body)
	}

	if err := p.store.RecordAttempt(ctx, attemptRecord{
		DeliveryID: d.ID, AttemptNumber: attemptNo,
		StatusCode: res.StatusCode, DurationMS: res.DurationMS,
		Excerpt: res.Excerpt, Error: errString(res.Err),
	}); err != nil {
		return err
	}

	switch classify(res) {
	case outcomeComplete:
		if err := p.store.MarkDelivered(ctx, d.ID, attemptNo); err != nil {
			return err
		}
		p.emit(ctx, d, "webhook.delivery.succeeded.v1", attemptNo, res.StatusCode)
	case outcomeDead:
		if err := p.store.MarkDead(ctx, d.ID, attemptNo); err != nil {
			return err
		}
		p.emit(ctx, d, "webhook.delivery.dead_lettered.v1", attemptNo, res.StatusCode)
	default: // outcomeRetry
		policy := deliveryPolicy{MaxAttempts: d.MaxAttempts, RetryWindow: time.Duration(d.RetryWindowSeconds) * time.Second}
		if retryExhausted(attemptNo, orNow(d.FirstAttemptAt, ts), ts, policy) {
			if err := p.store.MarkDead(ctx, d.ID, attemptNo); err != nil {
				return err
			}
			p.emit(ctx, d, "webhook.delivery.dead_lettered.v1", attemptNo, res.StatusCode)
			return nil
		}
		next := ts.Add(nextBackoff(attemptNo, p.cfg.BaseBackoff, p.cfg.MaxBackoff))
		return p.store.Reschedule(ctx, d.ID, attemptNo, next)
	}
	return nil
}

// signed bundles a sender destination with the body it signs, so attempt can branch on secret errors.
type signed struct {
	dst  webhook.Destination
	body []byte
}

// sign resolves the endpoint's active secret(s) and produces the destination + signed headers for one
// attempt. Rotation: both the primary and the (optional) next ref are resolved so the attempt carries
// a signature per active secret (§21.5).
func (p *WebhookPump) sign(d dueDelivery, ts time.Time, attempt int) (signed, error) {
	if p.secrets == nil {
		return signed{}, fmt.Errorf("no secret resolver configured")
	}
	var secrets [][]byte
	for _, ref := range []string{d.SecretRef, d.SecretRefNext} {
		if ref == "" {
			continue
		}
		s, err := p.secrets(ref)
		if err != nil {
			return signed{}, fmt.Errorf("resolve signing secret: %w", err)
		}
		secrets = append(secrets, s)
	}
	if len(secrets) == 0 {
		return signed{}, fmt.Errorf("endpoint %s has no signing secret", d.EndpointID)
	}
	headers := webhook.NewSigner(secrets...).Headers(d.ID, ts, attempt, d.Payload)
	for k, v := range d.FixedHeaders {
		headers[k] = v // fixed headers (resolved secret-ref values) ride alongside the signature
	}
	return signed{
		dst: webhook.Destination{
			URL:          d.URL,
			AllowPrivate: d.AllowPrivate,
			TimeoutMS:    d.TimeoutMS,
			Headers:      headers,
		},
		body: d.Payload,
	}, nil
}

// emit best-effort journals a terminal delivery outcome into the source session's stream (spec §21.6
// visibility). It never fails the delivery — the durable delivery/attempt rows are the source of
// truth. The fan-out query excludes webhook.* types, so this can never loop back into a new delivery.
func (p *WebhookPump) emit(ctx context.Context, d dueDelivery, eventType string, attempt, status int) {
	payload, _ := json.Marshal(map[string]any{
		"delivery_id": d.ID, "endpoint_id": d.EndpointID, "event_id": d.EventID,
		"attempt": attempt, "status_code": status,
	})
	if err := p.store.EmitDeliveryEvent(ctx, d.Org, d.Project, d.SessionID, eventType, payload); err != nil && p.log != nil {
		p.log("webhook delivery %s: journal emit failed: %v", d.ID, err)
	}
}

// --- pure decision functions (unit-tested, no I/O) ---

type outcome int

const (
	outcomeComplete outcome = iota
	outcomeRetry
	outcomeDead
)

// classify maps one attempt's Result to the delivery outcome (spec §21.6): 2xx completes; a terminal
// egress/redirect deny is dead; network errors, 408/409/425/429, and 5xx retry; every other 4xx is
// terminal.
func classify(res webhook.Result) outcome {
	if res.Terminal {
		return outcomeDead
	}
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return outcomeComplete
	case res.StatusCode == 0: // no HTTP response — a transport error
		return outcomeRetry
	case res.StatusCode == 408, res.StatusCode == 409, res.StatusCode == 425, res.StatusCode == 429:
		return outcomeRetry
	case res.StatusCode >= 500:
		return outcomeRetry
	default: // other 4xx
		return outcomeDead
	}
}

// backoffCeiling is the deterministic upper bound for a given attempt: base * 2^(attempt-1), capped at
// max. Exposed so the schedule is testable without observing jitter.
func backoffCeiling(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	ceil := base
	for i := 1; i < attempt; i++ {
		ceil *= 2
		if ceil >= max || ceil <= 0 { // cap, and guard the doubling overflow
			return max
		}
	}
	if ceil > max {
		return max
	}
	return ceil
}

// nextBackoff is the jittered delay before the next attempt: a full-jitter sample in [0, ceiling],
// which decorrelates a thundering herd of retries against one recovering receiver.
func nextBackoff(attempt int, base, max time.Duration) time.Duration {
	ceil := backoffCeiling(attempt, base, max)
	if ceil <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(ceil) + 1))
}

// deliveryPolicy is a delivery's dead-letter bound (spec §21.6: 72h / 20 attempts by default).
type deliveryPolicy struct {
	MaxAttempts int
	RetryWindow time.Duration
}

// retryExhausted reports whether a delivery has hit its dead-letter cutoff: the attempt count reached
// the cap, or the elapsed time since the first attempt exceeded the retry window.
func retryExhausted(attemptCount int, firstAt, now time.Time, policy deliveryPolicy) bool {
	if policy.MaxAttempts > 0 && attemptCount >= policy.MaxAttempts {
		return true
	}
	if policy.RetryWindow > 0 && now.Sub(firstAt) >= policy.RetryWindow {
		return true
	}
	return false
}

// buildEnvelope produces the exact body a delivery signs and sends — a minimal CloudEvents-compatible
// envelope (spec §20) captured at fan-out and stored immutably, so a redelivery replays it byte-for-byte.
func buildEnvelope(ev journalEvent, ep endpointCursor) []byte {
	var data any
	if len(ev.Payload) > 0 {
		_ = json.Unmarshal(ev.Payload, &data)
	}
	envelope := map[string]any{
		"specversion": "1.0",
		"id":          ev.EventID,
		"type":        ev.Type,
		"source":      "/v1/sessions/" + ev.SessionID,
		"data":        data,
	}
	if ep.APIRevision != "" {
		envelope["api_revision"] = ep.APIRevision
	}
	body, _ := json.Marshal(envelope)
	return body
}

func orNow(t *time.Time, fallback time.Time) time.Time {
	if t != nil {
		return *t
	}
	return fallback
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// newID mints a prefixed random id (whd_/whe_), matching the store's TEXT id convention.
func newID(prefix string) string {
	var raw [12]byte
	_, _ = cryptorand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}
