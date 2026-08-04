package automation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/adapters/integrations/queue"
	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// The queue bridges (E19 T6, spec §34.1-34.5). Two directions, one file:
//
//	inbound   PGQueue.Consume -> normalize -> admit -> RecordEffect -> Ack
//	outbound  run terminal tx -> queue_deliveries row -> DeliverDue pump -> Sink
//
// ─────────────────────────────────────────────────────────────────────────────────────────────────────
// WHY THIS DOES NOT CALL IngestInbound — the core decision of this task, written where a reader meets it.
//
// IngestInbound (inbound.go) requires a PER-MESSAGE HMAC: webhook.ParseInbound verifies a signature over
// the raw body before anything is persisted, and the trigger's secret_ref is what authenticates the
// SENDER of that one message. That is the correct auth model for an HTTP receiver, where every request
// arrives from an unauthenticated network with no prior relationship.
//
// A broker is not that. In a broker the authentication boundary IS THE CONNECTION: the platform
// authenticated to SQS/PubSub/Kafka (or, in the reference adapter, to its own Postgres) when it opened the
// consumer, and the messages that arrive on it carry no MAC of their own — there is nothing to verify
// per message, and inventing a MAC would be verifying a signature we would also have to have produced.
//
// So the queue bridge admits through the Admitter DIRECTLY, and it is a SECOND ENTRYPOINT rather than
// IngestInbound bent by a flag. That is deliberate: a single entrypoint with a `skipSignature bool`
// parameter is one wrong call site away from an HTTP receiver that accepts unsigned bodies, and the
// difference between the two auth models would live in a caller's argument instead of in the type system.
// Two authentication models deserve two admission entrypoints. The three things that make this entrypoint
// safe are structural, not filters that a later edit could forget:
//
//  1. THE TENANT COMES FROM THE CONNECTION ROW, never the payload. Nothing in the message body
//     participates in choosing org/project — not `source_tenant`, not anything else. The bridge does not
//     read a tenant out of the payload and then validate it; there is no code path by which it could.
//  2. THE RUN TARGET COMES FROM THE CONNECTION'S config JSONB (agent_revision_id + principal_id), which
//     000037 defines as exactly that. A payload cannot pin a revision, choose a principal, or name a
//     session.
//  3. ITS OWN IDEMPOTENCY NAMESPACE. queueAdmitRoute is distinct from createRoute, a2aAdmitRoute and
//     slackAdmitRoute, so a broker's idempotency_key can never collide with a native Idempotency-Key, an
//     A2A messageId or a Slack event_id of the same value.
//
// And the registration question the sibling Slack path taught us to ask — WHO MAY CLAIM THE KEY THE
// LOOKUP SELECTS BY, AND WHAT IF TWO ROWS MATCH? — has a structurally different answer here, which is the
// reason this surface does not need the ORDER BY/LIMIT 2 ambiguity refusal that a team-id lookup needs:
// the bridge never resolves a connection from outsider-supplied input at all. It SWEEPS its own catalogue
// (SweepQueueConnections) and each row carries its own tenant; the admin surface mints the connection id
// server-side inside the caller's verified scope. There is no key an outsider supplies, hence no key an
// outsider can squat, and no second row to disambiguate.
//
// HONEST CEILING: there is no real broker PRODUCT anywhere in this tree (E17 §6 leg 5) — no NATS, SQS,
// Pub/Sub or Kafka. What these bridges prove is the BRIDGE, not broker semantics: the reference adapter's
// tables ARE the broker, so redelivery, visibility timeouts and dead-lettering are OUR implementation of
// those semantics being exercised, not a vendor's. `queues` therefore stays preview and this task does not
// touch the tier.

// queueAdmitRoute is the idempotency route the queue admission keys on (the a2aAdmitRoute / slackAdmitRoute
// precedent). The key under it is the MESSAGE's own idempotency_key, which the queue contract defines as
// stable across redeliveries of the same logical message — so a lost-ack redelivery presents the same key
// and REPLAYS onto the one run.
const queueAdmitRoute = "/v1/queue/messages"

var (
	// ErrQueueNoRunTarget is a connection whose config names no usable run target. Fail-closed: a binding
	// that has not been told WHAT to run, or AS WHOM, admits nothing — it is skipped entirely, so its
	// backlog is preserved rather than burned through the dead-letter bound while an operator fixes it.
	ErrQueueNoRunTarget = errors.New("automation: queue connection config names no run target")
	// ErrQueueForeignPrincipal is a config naming a principal outside the connection's own tenant.
	ErrQueueForeignPrincipal = errors.New("automation: queue connection principal is outside the connection's tenant")
)

// QueueBridgeConfig carries the admission caps and the loop cadence. MaxConcurrentRuns/MaxQueuedRuns are
// the §20.12 per-project caps the bridge passes through to the SAME admission the API edge uses — a queue
// flood is therefore shed by the same gate a POST flood is, not by a second policy.
type QueueBridgeConfig struct {
	MaxConcurrentRuns int
	MaxQueuedRuns     int
	// Batch bounds how many messages one connection yields per tick (the bounded-buffer half that is not
	// the queue's own capacity ceiling).
	Batch int
	Tick  time.Duration
	// DeliveryBackoff is the outbox pump's retry delay for a failed outbound delivery.
	DeliveryBackoff time.Duration
}

func (c QueueBridgeConfig) withDefaults() QueueBridgeConfig {
	if c.Batch <= 0 {
		c.Batch = 32
	}
	if c.Tick <= 0 {
		c.Tick = time.Second
	}
	if c.DeliveryBackoff <= 0 {
		c.DeliveryBackoff = 30 * time.Second
	}
	return c
}

// QueueBridge is the supervised consume→admit loop over every enabled inbound queue connection.
type QueueBridge struct {
	store    *QueueStore
	admitter RunAdmitter
	cfg      QueueBridgeConfig
	log      func(string, ...any)
}

// NewQueueBridge wires the inbound bridge. admitter is the SAME coordinator spine POST /v1/responses and
// the trigger pipeline admit through — a queued message is born as the same durable object, under the same
// caps, budgets and published-revision pin (§34.1: the adapter invents no run identity). log may be nil.
func NewQueueBridge(store *QueueStore, admitter RunAdmitter, cfg QueueBridgeConfig, log func(string, ...any)) *QueueBridge {
	return &QueueBridge{store: store, admitter: admitter, cfg: cfg.withDefaults(), log: log}
}

// Run ticks the bridge until ctx is cancelled (the webhook-pump shape: a returned error restarts under the
// supervisor, a cancelled context is a clean shutdown).
func (b *QueueBridge) Run(ctx context.Context) error {
	ticker := time.NewTicker(b.cfg.Tick)
	defer ticker.Stop()
	for {
		if err := b.Tick(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Tick consumes one batch from every ENABLED inbound connection. Exported so the component suite drives it
// deterministically (no sleeps).
//
// The run target is resolved ONCE per connection, BEFORE anything is leased. A connection whose config
// names no target is skipped entirely rather than consumed and dead-lettered: a misconfiguration is the
// operator's to fix and the messages are innocent, so burning them through the dead-letter bound would
// destroy a backlog that becomes deliverable the moment the config is corrected.
func (b *QueueBridge) Tick(ctx context.Context) error {
	conns, err := b.store.sweepConnections(ctx, "inbound")
	if err != nil {
		return err
	}
	for _, c := range conns {
		target, terr := b.runTarget(ctx, c)
		if terr != nil {
			b.logf("queue: connection %s admits nothing: %v", c.id, terr)
			continue
		}
		if _, err := b.store.inboundQueueFor(c).Consume(ctx, b.cfg.Batch, b.handler(c, target)); err != nil {
			return err
		}
	}
	return nil
}

// handler is the queue.Handler for one connection: normalize → admit → RecordEffect → Ack. Consume acks
// ONLY on the returned Ack, and only AFTER this function returns — so the durable admission commit and the
// receipt both precede the ack (§34.2), and a crash anywhere before the ack redelivers.
func (b *QueueBridge) handler(c queueConn, target queueRunTarget) queue.Handler {
	return func(ctx context.Context, m queue.Message) (queue.Disposition, error) {
		input, err := queueRunInput(c, m)
		if err != nil {
			// POISON (§34.3): an unmappable payload can never succeed, however many times it is
			// redelivered. Dead-letter it immediately so it does not occupy the stream, and so the
			// operator has the message to inspect rather than a silent drop.
			b.logf("queue: dead-lettering unmappable message on connection %s: %v", c.id, err)
			return queue.DeadLetter, nil
		}
		disp, err := b.admit(ctx, c, target, m, input)
		if err != nil || disp != queue.Ack {
			return disp, err
		}
		// The append-only receipt (§34.2). It is written AFTER the admission, and the ORDER is
		// load-bearing in the opposite direction from the E17 T7 reference handler's — that handler
		// writes the receipt in the SAME transaction as its effect, which this bridge cannot do because
		// the effect (AdmitResponse) owns its own transaction inside the coordinator.
		//
		// Writing the receipt FIRST would be the lost-run bug: receipt committed, process dies before the
		// admission, redelivery sees a receipt and acks — the message is gone and no run was ever born.
		// Writing it AFTER is safe because the ADMISSION ITSELF is the idempotency anchor and a strictly
		// stronger one: it is keyed on the same message key under queueAdmitRoute and commits atomically
		// with the run, so a redelivery whose receipt was lost REPLAYS onto the same response instead of
		// creating a second. The receipt is the auditable ledger of "this message's effect committed",
		// not the thing that makes the effect single.
		if _, err := b.store.RecordEffect(ctx, b.store.pool, c.project, c.id, m.IdempotencyKey); err != nil {
			// The run is already durable. Refusing to ack earns a redelivery, which replays onto the same
			// response and re-attempts the receipt — never a second run.
			return queue.Retry, err
		}
		return queue.Ack, nil
	}
}

// admit runs the real §20.9 admission for one message. Every identity input is read from the CONNECTION;
// the only thing taken from the message is its body (as opaque run input) and its idempotency key.
func (b *QueueBridge) admit(ctx context.Context, c queueConn, target queueRunTarget, m queue.Message, input []byte) (queue.Disposition, error) {
	if b.admitter == nil {
		return queue.Retry, errors.New("automation: queue admitter is not wired")
	}
	responseID, runID, sessionID := newID("resp"), newID("run"), newID("ses")
	body, err := json.Marshal(contracts.Response{
		ID:        contracts.ResponseID(responseID),
		Object:    "response",
		Status:    "queued",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Output:    []contracts.ContentItem{},
		Usage:     contracts.Usage{},
		SessionID: contracts.SessionID(sessionID),
		RunID:     contracts.RunID(runID),
		ProjectID: contracts.ProjectID(c.project),
	})
	if err != nil {
		return queue.Retry, fmt.Errorf("marshal queue projection: %w", err)
	}
	sum := sha256.Sum256(input)
	adm, err := b.admitter.AdmitResponse(ctx,
		// The tenant. It is the swept connection row's own org/project and nothing else has ever been
		// consulted — see the file header, invariant 1.
		coordinator.Tenant{Project: c.project},
		coordinator.AdmissionInput{
			Principal:      target.principal,
			IdempotencyKey: m.IdempotencyKey,
			Method:         "POST",
			Route:          queueAdmitRoute,
			RequestHash:    hex.EncodeToString(sum[:]),
			ResponseID:     responseID,
			RunID:          runID,
			SessionID:      sessionID,
			Input:          input,
			Body:           body,
			Store:          true,
			// The pinned revision, from the CONNECTION's config — never the payload.
			AgentRevisionID:   target.agentRevisionID,
			MaxConcurrentRuns: b.cfg.MaxConcurrentRuns,
			MaxQueuedRuns:     b.cfg.MaxQueuedRuns,
		})
	if err != nil {
		return queue.Retry, fmt.Errorf("admit queued message: %w", err)
	}
	// A REPLAY is a success: the redelivery found its own original reservation and no second run was
	// created. That is the lost-ack story, and acking here is what finally retires the message.
	if adm.Replayed {
		return queue.Ack, nil
	}
	switch {
	case adm.ConcurrencyLimited, adm.QueueDepthExceeded, adm.LimitExceeded != nil:
		// Load, not configuration: the message is fine and capacity frees on its own. Retry keeps it
		// leased and the visibility timeout redelivers it — the SINGLE retry owner (§35.2) is the queue,
		// so the bridge adds no retry of its own and there is no multiplication.
		return queue.Retry, nil
	case adm.Conflict:
		// The same idempotency key with a DIFFERENT request hash: two distinct messages published under
		// one key. No redelivery can fix that, and re-attempting forever would wedge the stream.
		b.logf("queue: dead-lettering message whose idempotency key was reused with a different body (connection %s)", c.id)
		return queue.DeadLetter, nil
	case adm.PinnedRevisionNotFound, adm.PinnedRevisionNotPublished, adm.RepositoryBindingNotFound,
		adm.SessionNotFound, adm.SessionConflict, adm.Purged:
		// Configuration refusals. Retry rather than dead-letter: they are all repairable by an operator
		// (publish the revision, fix the pin), and the dead-letter bound still stops an infinite loop.
		b.logf("queue: admission refused for connection %s (repairable configuration); message redelivers", c.id)
		return queue.Retry, nil
	}
	return queue.Ack, nil
}

func (b *QueueBridge) logf(format string, args ...any) {
	if b.log != nil {
		b.log(format, args...)
	}
}

// queueRunTarget is what a connection's config says to run and as whom. Both come from the CONNECTION row;
// neither can be influenced by a message.
type queueRunTarget struct {
	agentRevisionID string
	principal       string
}

// runTarget decodes and validates the connection's config JSONB. Fail-closed on every branch, the
// SlackAdmitter.runTarget twin: no principal, no revision, or a principal belonging to another tenant all
// refuse — the connection then admits nothing at all rather than guessing a default.
func (b *QueueBridge) runTarget(ctx context.Context, c queueConn) (queueRunTarget, error) {
	var cfg struct {
		AgentRevisionID string `json:"agent_revision_id"`
		PrincipalID     string `json:"principal_id"`
	}
	if len(c.config) > 0 {
		if err := json.Unmarshal(c.config, &cfg); err != nil {
			return queueRunTarget{}, fmt.Errorf("%w: config is not an object", ErrQueueNoRunTarget)
		}
	}
	if cfg.PrincipalID == "" {
		return queueRunTarget{}, fmt.Errorf("%w: config.principal_id is required", ErrQueueNoRunTarget)
	}
	if cfg.AgentRevisionID == "" {
		return queueRunTarget{}, fmt.Errorf("%w: config.agent_revision_id is required", ErrQueueNoRunTarget)
	}
	// The principal must live in the connection's OWN tenant. Without this a connection could name any
	// principal id in the deployment and run as it — the confused deputy the Slack path closes the same way.
	switch err := b.store.pool.QueryRow(storage.ScopeToTenant(ctx, c.project),
		storage.Query("QueueRunPrincipalInScope"), cfg.PrincipalID, c.project).Scan(new(int)); {
	case errors.Is(err, pgx.ErrNoRows):
		return queueRunTarget{}, ErrQueueForeignPrincipal
	case err != nil:
		return queueRunTarget{}, fmt.Errorf("resolve queue run principal: %w", err)
	}
	return queueRunTarget{agentRevisionID: cfg.AgentRevisionID, principal: cfg.PrincipalID}, nil
}

// queueRunInput normalizes a message body into the run's input. The queue contract (§34.1) says a consumed
// message normalizes to the existing webhook.InboundEvent envelope, so the same strict decode applies —
// minus the MAC, which a broker message does not have (see the file header).
//
// A body that is not that envelope is POISON and returns an error, which the handler turns into an
// immediate dead-letter rather than an endless redelivery.
//
// NOTE the two fields that are deliberately INERT here, because they are exactly the fields a payload
// would use to escape its connection if this bridge trusted them:
//
//   - source_tenant travels as DATA. It is a string the PRODUCER wrote; it names nothing this bridge acts
//     on. The tenant is the connection's, always.
//   - source_event_id is likewise data. The dedupe key is the MESSAGE's idempotency_key (the broker's own
//     redelivery identity), not a field the producer can vary to force a second run under one message.
func queueRunInput(c queueConn, m queue.Message) ([]byte, error) {
	var ev webhook.InboundEvent
	if err := json.Unmarshal(m.Body, &ev); err != nil {
		return nil, fmt.Errorf("queue message body is not a §21.7 envelope: %w", err)
	}
	if ev.Source == "" {
		return nil, errors.New("queue message envelope has no source")
	}
	// The input is a pure function of the MESSAGE — nothing about the delivery ATTEMPT may appear, or a
	// redelivery would hash differently and the reservation would report a CONFLICT instead of a replay
	// (the Slack path's identical trap). m.Attempt and m.Handle are therefore absent by construction.
	return json.Marshal(map[string]any{
		"source":          "queue",
		"connection_id":   c.id,
		"idempotency_key": m.IdempotencyKey,
		"event_source":    ev.Source,
		"source_tenant":   ev.SourceTenant,
		"source_event_id": ev.SourceEventID,
		"data":            ev.Data,
	})
}

// --- outbound: the DeliverDue pump ---

// QueueOutboxPump drains queue_deliveries for every enabled outbound connection (§34.5). The rows it
// delivers were committed by EnqueueTerminalQueueDeliveries INSIDE the run's terminal transaction, so this
// pump can be down, restarted, or absent for an arbitrary window without a result being lost — it is a
// delivery mechanism, never the durability mechanism.
type QueueOutboxPump struct {
	store *QueueStore
	sinks QueueSinkResolver
	cfg   QueueBridgeConfig
	log   func(string, ...any)
}

// QueueSinkResolver turns an outbound connection's config JSONB into the destination its results are
// delivered to. QueueHTTPSinks is the production resolver; a component test injects a recording sink.
type QueueSinkResolver func(rawConfig []byte) (queue.Sink, error)

// NewQueueOutboxPump wires the pump. sinks resolves a connection to its destination — QueueHTTPSinks is the
// production resolver; a component test injects a recording sink. A connection whose sink cannot be
// resolved is SKIPPED, leaving its deliveries pending (durable) rather than dead-lettering them.
func NewQueueOutboxPump(store *QueueStore, sinks QueueSinkResolver, cfg QueueBridgeConfig, log func(string, ...any)) *QueueOutboxPump {
	return &QueueOutboxPump{store: store, sinks: sinks, cfg: cfg.withDefaults(), log: log}
}

func (p *QueueOutboxPump) Run(ctx context.Context) error {
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

// Tick attempts every due delivery once, per outbound connection. Exported for the component suite.
func (p *QueueOutboxPump) Tick(ctx context.Context) error {
	conns, err := p.store.sweepConnections(ctx, "outbound")
	if err != nil {
		return err
	}
	for _, c := range conns {
		sink, err := p.sinks(c.config)
		if err != nil {
			if p.log != nil {
				p.log("queue: outbound connection %s has no usable destination: %v", c.id, err)
			}
			continue
		}
		if _, err := p.store.outboxFor(c).DeliverDue(ctx, sink, p.cfg.Batch, p.cfg.DeliveryBackoff); err != nil {
			return err
		}
	}
	return nil
}

// QueueHTTPSinks is the production sink resolver: an outbound connection's config names a
// `destination_url`, and the result is POSTed there over the SAME egress-safe sender the webhook pump uses
// (https-only unless the self-host flag is set, private/loopback ranges denied, DNS re-resolved and the
// connection pinned per attempt, redirects refused). No new egress code, and no new SSRF surface class:
// naming an outbound URL is what the §21.4 webhook endpoint surface already lets a tenant do.
//
// CEILING, and it is the reason this is not a substitute for webhook endpoints: this sink does NOT SIGN.
// The receiver cannot tell our POST from anyone else's, so a deployment that needs authenticity uses the
// §21.4 webhook endpoint surface, which signs. Upgrade path: resolve a secret_ref off the connection config
// and reuse webhook.NewSigner, exactly as WebhookPump.sign does.
//
// ponytail: a permanent 4xx burns the connection's max_deliveries before dead-lettering, because
// PGOutbox.DeliverDue's seam is `error`, not a Disposition — it cannot distinguish "never going to work"
// from "try later". Widening that seam is worth it only once a real broker client needs the distinction.
func QueueHTTPSinks(sender *webhook.Sender) QueueSinkResolver {
	return func(rawConfig []byte) (queue.Sink, error) {
		var cfg struct {
			DestinationURL string `json:"destination_url"`
			AllowPrivate   bool   `json:"allow_private"`
			TimeoutMS      int    `json:"timeout_ms"`
		}
		if len(rawConfig) > 0 {
			if err := json.Unmarshal(rawConfig, &cfg); err != nil {
				return nil, fmt.Errorf("config is not an object: %w", err)
			}
		}
		if cfg.DestinationURL == "" {
			return nil, errors.New("config.destination_url is required")
		}
		return queueHTTPSink{sender: sender, url: cfg.DestinationURL, allowPrivate: cfg.AllowPrivate, timeoutMS: cfg.TimeoutMS}, nil
	}
}

type queueHTTPSink struct {
	sender       *webhook.Sender
	url          string
	allowPrivate bool
	timeoutMS    int
}

// Deliver POSTs one result. destKey rides in a header so the receiver can apply its own destination
// idempotency — the §34.5 contract's requirement on the SINK side, which we cannot enforce for it but must
// give it the means to satisfy.
func (s queueHTTPSink) Deliver(ctx context.Context, destKey string, body []byte) error {
	res := s.sender.Deliver(ctx, webhook.Destination{
		URL:          s.url,
		AllowPrivate: s.allowPrivate,
		TimeoutMS:    s.timeoutMS,
		Headers:      map[string]string{"Content-Type": "application/json", "Palai-Destination-Key": destKey},
	}, body)
	if webhook.Classify(res) == webhook.OutcomeComplete {
		return nil
	}
	if res.Err != nil {
		return res.Err
	}
	return fmt.Errorf("queue sink: destination answered %d", res.StatusCode)
}
