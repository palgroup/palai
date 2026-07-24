// Package queue is the queue-adapter contract (E17 Task 7, spec §34.1-34.5): a SQS/PubSub/Kafka-class
// durable consumer + an outbound result-delivery outbox, both following the SAME durable-delivery
// discipline the automation webhook/inbound seam already enforces (durable-record-before-ack, dedupe on
// an idempotency key, bounded-buffer backpressure, dead-letter after N failures).
//
// The contract lives here as pure Go (no storage import — like adapters/integrations/webhook). Two
// REFERENCE adapters prove it: Memory (in-process, deterministic contract tests, this package) and a
// Postgres-durable one (apps/control-plane/internal/automation, migration 000037) that survives a crash.
// A real broker (SQS/PubSub/Kafka) plugs in behind the SAME interface and is the OPERATOR leg (§6) —
// unwritten, so discovery never advertises it.
//
// INVARIANT (§34.1): a consumed message NORMALIZES to the existing InboundEvent and admits through the
// existing automation pipeline — the adapter invents NO new run identity. A broker offset / receipt
// handle is a transport handle, never the canonical run id.
package queue

import (
	"context"
	"errors"
	"time"
)

// Message is one leased queue message handed to a Handler. Handle is the broker's receipt/lease handle
// (an SQS ReceiptHandle, a PubSub ackId, a queue_messages row id) — a transport token the adapter acks
// against, NOT a canonical run identity (§34.1). IdempotencyKey is the dedupe key: a redelivery of the
// SAME logical message carries the SAME key, so an idempotent effect runs ONCE across redeliveries.
// Body is the opaque payload the InboundEvent normalize consumes. Attempt is 1 on first delivery and
// increments on each redelivery, so a consumer can dead-letter after N.
type Message struct {
	Handle         string
	IdempotencyKey string
	Body           []byte
	Attempt        int
}

// Disposition is the effect's verdict, which the consumer turns into an ack, a redelivery, or a
// dead-letter. It is what a Handler returns after running (or de-duplicating) the effect.
type Disposition int

const (
	// Ack: the effect committed (or was a de-duplicated no-op). The consumer acks — the message leaves
	// the queue. Because the ack happens ONLY after this is returned, a crash before it redelivers the
	// message (at-least-once), never loses it (§34.2).
	Ack Disposition = iota
	// Retry: a transient failure. The message stays un-acked and redelivers after the visibility timeout,
	// counting toward the dead-letter bound.
	Retry
	// DeadLetter: poison — the effect can never succeed (an unmappable payload, §34.3). The message moves
	// to the dead-letter view immediately and never redelivers.
	DeadLetter
)

// Handler runs the effect for one message and returns its Disposition. The effect MUST be idempotent on
// m.IdempotencyKey: the queue is at-least-once, so a lost ack redelivers a message whose effect already
// committed, and the Handler must recognise the duplicate and NOT run the effect twice (Ack it instead).
// A returned error is treated as a Retry (a transient failure the visibility timeout redelivers).
type Handler func(ctx context.Context, m Message) (Disposition, error)

// ErrQueueFull is the backpressure signal a bounded producer returns when the queue is at capacity: the
// producer WAITS or sheds (a caller retries the Publish), and the queue never silently DROPS an enqueued
// message (§34.4). Backpressure is applied, not data loss.
var ErrQueueFull = errors.New("queue: full (backpressure)")

// Depth is the backpressure/observability gauge (§34.4): the ready backlog, the in-flight (leased,
// not-yet-acked) count, the dead-letter count, and the age of the oldest ready message. A producer or an
// operator reads it to decide whether to back off, exactly as the inbound backlog gauge does (AUT-010).
type Depth struct {
	Ready     int
	InFlight  int
	Dead      int
	OldestAge time.Duration
}

// InboundQueue is the consume side of the adapter contract. Publish enqueues a message (applying
// backpressure, never dropping); Consume leases up to max ready messages, runs the Handler on each, and
// acks only those the Handler Acks; Depth reports the backlog gauge. The reference impls are Memory (this
// package) and the Postgres-durable one (automation); a real broker is the operator leg (§6).
type InboundQueue interface {
	Publish(ctx context.Context, idempotencyKey string, body []byte) error
	Consume(ctx context.Context, max int, h Handler) (handled int, err error)
	Depth(ctx context.Context) (Depth, error)
}

// Sink is the destination an outbound run-result is delivered TO — a real SQS/PubSub/Kafka Publish (the
// operator leg), or a local recording sink in tests. destKey is the destination idempotency key: a Sink
// that receives the same destKey twice (a retry after a lost delivery-ack) MUST collapse it to ONE effect,
// so the outbox's at-least-once retries never duplicate a delivered result (§34.5). A returned error keeps
// the outbox delivery pending (durable, loss-less) for the next tick.
type Sink interface {
	Deliver(ctx context.Context, destKey string, body []byte) error
}
