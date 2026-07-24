package queue

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemoryConfig shapes the in-process reference queue. Capacity bounds ready+in-flight messages so Publish
// applies backpressure (ErrQueueFull) instead of growing without bound. Visibility is the lease timeout:
// an in-flight message not acked within it becomes visible again (a redelivery — this is how a crash
// before ack recovers). MaxDeliveries is the dead-letter bound: a message delivered this many times
// without an Ack moves to the dead-letter view instead of redelivering forever.
type MemoryConfig struct {
	Capacity      int
	Visibility    time.Duration
	MaxDeliveries int
}

func (c MemoryConfig) withDefaults() MemoryConfig {
	if c.Capacity <= 0 {
		c.Capacity = 1024
	}
	if c.Visibility <= 0 {
		c.Visibility = 30 * time.Second
	}
	if c.MaxDeliveries <= 0 {
		c.MaxDeliveries = 20
	}
	return c
}

// Memory is the in-process reference InboundQueue: a bounded FIFO with lease/visibility redelivery and a
// dead-letter view. It proves the contract deterministically (no Docker) and is the shape a real broker
// implements durably. It is safe for concurrent producers/consumers (one mutex — this is a reference, not
// a throughput engine; the ceiling is a single lock).
//
// ponytail: single global lock, O(n) scan per Consume. Fine for a reference/contract queue; a real broker
// (or a per-key sharded lock) is the throughput path and is the operator leg.
type Memory struct {
	mu   sync.Mutex
	cfg  MemoryConfig
	now  func() time.Time
	seq  int64
	msgs []*memMsg // ready + in-flight, FIFO by enqueue order
	dead []*memMsg

	// ackFailures is a test hook: drop the next N acks (a lost ack — the effect committed but the ack
	// never reached the broker), so a redelivery of an ALREADY-effected message can be exercised. It is
	// unexported; only same-package tests set it.
	ackFailures int
}

type memMsg struct {
	handle     string
	key        string
	body       []byte
	attempt    int
	enqueuedAt time.Time
	leaseUntil time.Time // zero = ready; non-zero = leased until this instant
}

// NewMemory builds a reference queue. now may be nil (defaults to time.Now); tests inject a clock to drive
// visibility deterministically.
func NewMemory(cfg MemoryConfig, now func() time.Time) *Memory {
	if now == nil {
		now = time.Now
	}
	return &Memory{cfg: cfg.withDefaults(), now: now}
}

// Publish enqueues a message, applying backpressure: at capacity it returns ErrQueueFull rather than
// dropping the message. The body is copied so the caller cannot mutate an enqueued payload.
func (m *Memory) Publish(_ context.Context, idempotencyKey string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.msgs) >= m.cfg.Capacity {
		return ErrQueueFull
	}
	m.seq++
	m.msgs = append(m.msgs, &memMsg{
		handle:     fmt.Sprintf("mem-%d", m.seq),
		key:        idempotencyKey,
		body:       append([]byte(nil), body...),
		enqueuedAt: m.now(),
	})
	return nil
}

// Consume leases up to max ready (or lease-expired) messages, runs the Handler on each, and applies the
// returned Disposition. A message whose attempt count would exceed MaxDeliveries is dead-lettered instead
// of delivered. The ack happens only AFTER the Handler returns Ack, so a crash between the effect and the
// ack redelivers the message (at-least-once) — the ackFailures hook models exactly that.
func (m *Memory) Consume(ctx context.Context, max int, h Handler) (int, error) {
	handled := 0
	for handled < max {
		m.mu.Lock()
		msg := m.leaseOneLocked()
		if msg == nil {
			m.mu.Unlock()
			break
		}
		if msg.attempt > m.cfg.MaxDeliveries {
			m.toDeadLocked(msg)
			m.mu.Unlock()
			continue
		}
		lease := Message{Handle: msg.handle, IdempotencyKey: msg.key, Body: msg.body, Attempt: msg.attempt}
		m.mu.Unlock()

		disp, err := h(ctx, lease)

		m.mu.Lock()
		switch {
		case err == nil && disp == Ack:
			if m.ackFailures > 0 {
				// Lost ack: the effect committed but the ack was dropped. Make the message visible again
				// immediately (a redelivery), so an idempotent Handler is exercised on an already-effected key.
				m.ackFailures--
				msg.leaseUntil = time.Time{}
			} else {
				m.removeLocked(msg)
			}
		case disp == DeadLetter:
			m.toDeadLocked(msg)
		default: // Retry, or a Handler error (treated as transient): leave leased; it redelivers when the lease expires.
		}
		m.mu.Unlock()
		handled++
	}
	return handled, nil
}

// leaseOneLocked returns the next deliverable message (ready, or leased with an expired lease), marking it
// leased for Visibility and incrementing its attempt. Caller holds m.mu.
func (m *Memory) leaseOneLocked() *memMsg {
	now := m.now()
	for _, msg := range m.msgs {
		if msg.leaseUntil.IsZero() || !now.Before(msg.leaseUntil) {
			msg.attempt++
			msg.leaseUntil = now.Add(m.cfg.Visibility)
			return msg
		}
	}
	return nil
}

func (m *Memory) removeLocked(msg *memMsg) {
	for i, cur := range m.msgs {
		if cur == msg {
			m.msgs = append(m.msgs[:i], m.msgs[i+1:]...)
			return
		}
	}
}

func (m *Memory) toDeadLocked(msg *memMsg) {
	m.removeLocked(msg)
	m.dead = append(m.dead, msg)
}

// Depth reports the backlog gauge: ready messages, in-flight (leased, not-yet-expired) messages, the
// dead-letter count, and the age of the oldest ready message.
func (m *Memory) Depth(_ context.Context) (Depth, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var d Depth
	d.Dead = len(m.dead)
	var oldest time.Time
	for _, msg := range m.msgs {
		if msg.leaseUntil.IsZero() || !now.Before(msg.leaseUntil) {
			d.Ready++
			if oldest.IsZero() || msg.enqueuedAt.Before(oldest) {
				oldest = msg.enqueuedAt
			}
		} else {
			d.InFlight++
		}
	}
	if !oldest.IsZero() {
		d.OldestAge = now.Sub(oldest)
	}
	return d, nil
}
