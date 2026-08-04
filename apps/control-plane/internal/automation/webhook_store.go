package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// The two typed refusals the delete path can produce (E29 T3). Both exist so a caller gets an answer they
// can act on instead of a 500, and both are sentinels rather than status codes because the store layer does
// not know what protocol it is being read over.
var (
	// ErrEndpointPinned is a delete refused because a trigger revision names this endpoint as its callback
	// target. It is the one referrer of webhook_endpoints that still blocks a delete, and it blocks it on
	// purpose: unlike a delivery — a record of something already sent — a trigger revision is live
	// configuration that will send to this endpoint the next time it fires, and a revision is immutable, so
	// there is no edit that unpins it. Removing the trigger, or publishing a revision that names no
	// callback, is what frees the endpoint.
	ErrEndpointPinned = errors.New("automation: webhook endpoint is pinned by a trigger revision's callback target")

	// ErrDeliveryEndpointDeleted is a redelivery refused because the endpoint the delivery was addressed to
	// has been unregistered. The delivery row is still there and still readable — that is the whole point of
	// keeping it — but there is nowhere left to send it, and the pump would never pick it up because
	// DueDeliveries joins webhook_endpoints. Answering "accepted" would be accepting work that cannot happen.
	ErrDeliveryEndpointDeleted = errors.New("automation: the endpoint this delivery was addressed to has been deleted")
)

// WebhookStore is the pgx-backed repository for the webhook pump and API (spec §21.4-21.6). It shares
// the durable spine's pool. The pump reads/writes system-wide (every project's endpoints); the API
// methods are tenant-scoped by the verified identity.
type WebhookStore struct {
	pool *pgxpool.Pool
}

// NewWebhookStore wraps a shared connection pool.
func NewWebhookStore(pool *pgxpool.Pool) *WebhookStore { return &WebhookStore{pool: pool} }

// --- pump-facing row types ---

type endpointCursor struct {
	ID          string
	Org         string
	Project     string
	Filter      []string
	APIRevision string
	Cursor      int64
}

type journalEvent struct {
	JournalID int64
	EventID   string
	SessionID string
	Type      string
	Payload   []byte
}

type deliveryInsert struct {
	ID         string
	Org        string
	Project    string
	EndpointID string
	SessionID  string
	EventID    string
	EventType  string
	Payload    []byte
}

type dueDelivery struct {
	ID                 string
	Org                string
	Project            string
	SessionID          string
	EndpointID         string
	EventID            string
	EventType          string
	Payload            []byte
	AttemptCount       int
	FirstAttemptAt     *time.Time
	URL                string
	AllowPrivate       bool
	TimeoutMS          int
	MaxAttempts        int
	RetryWindowSeconds int
	SecretRef          string
	SecretRefNext      string
	FixedHeaders       map[string]string
	APIRevision        string
}

type attemptRecord struct {
	DeliveryID string
	StatusCode int
	DurationMS int64
	Excerpt    string
	Error      string
}

// FanOutEndpoints returns every enabled endpoint and its durable cursor (system-wide, not tenant-scoped).
func (s *WebhookStore) FanOutEndpoints(ctx context.Context) ([]endpointCursor, error) {
	rows, err := s.pool.Query(ctx, storage.Query("FanOutEndpoints"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []endpointCursor
	for rows.Next() {
		var e endpointCursor
		if err := rows.Scan(&e.ID, &e.Org, &e.Project, &e.Filter, &e.APIRevision, &e.Cursor); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReadJournalForEndpoint reads the matching journal slice past the cursor (loop-guarded, ordered by
// the global journal_id), tenant-scoped to the endpoint's own org+project so a delivery never carries
// another tenant's journal (§39.2).
func (s *WebhookStore) ReadJournalForEndpoint(ctx context.Context, project string, cursor int64, filter []string, limit int) ([]journalEvent, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	if filter == nil {
		filter = []string{}
	}
	rows, err := s.pool.Query(ctx, storage.Query("ReadJournalForEndpoint"), project, cursor, filter, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []journalEvent
	for rows.Next() {
		var e journalEvent
		if err := rows.Scan(&e.JournalID, &e.EventID, &e.SessionID, &e.Type, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// InsertDelivery materializes a delivery (idempotent on (endpoint, event)).
func (s *WebhookStore) InsertDelivery(ctx context.Context, d deliveryInsert) error {
	_, err := s.pool.Exec(ctx, storage.Query("InsertDelivery"),
		d.ID, d.Org, d.Project, d.EndpointID, d.SessionID, d.EventID, d.EventType, d.Payload)
	return err
}

// AdvanceEndpointCursor moves an endpoint's fan-out high-water mark forward (monotonic).
func (s *WebhookStore) AdvanceEndpointCursor(ctx context.Context, endpointID string, cursor int64) error {
	_, err := s.pool.Exec(ctx, storage.Query("AdvanceEndpointCursor"), endpointID, cursor)
	return err
}

// DueDeliveries returns pending deliveries whose backoff clock has elapsed, joined to their endpoint.
func (s *WebhookStore) DueDeliveries(ctx context.Context, limit int) ([]dueDelivery, error) {
	rows, err := s.pool.Query(ctx, storage.Query("DueDeliveries"), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dueDelivery
	for rows.Next() {
		var d dueDelivery
		var fixedHeaders []byte
		if err := rows.Scan(
			&d.ID, &d.Org, &d.Project, &d.SessionID, &d.EndpointID, &d.EventID, &d.EventType,
			&d.Payload, &d.AttemptCount, &d.FirstAttemptAt,
			&d.URL, &d.AllowPrivate, &d.TimeoutMS, &d.MaxAttempts, &d.RetryWindowSeconds,
			&d.SecretRef, &d.SecretRefNext, &fixedHeaders, &d.APIRevision,
		); err != nil {
			return nil, err
		}
		if len(fixedHeaders) > 0 {
			if err := json.Unmarshal(fixedHeaders, &d.FixedHeaders); err != nil {
				return nil, fmt.Errorf("decode fixed headers: %w", err)
			}
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RecordAttempt appends one sanitized attempt row with a monotonic attempt_number (max+1, computed in
// SQL) so a redelivery's attempts never collide with the original cycle's (F6).
func (s *WebhookStore) RecordAttempt(ctx context.Context, a attemptRecord) error {
	_, err := s.pool.Exec(ctx, storage.Query("RecordDeliveryAttempt"),
		a.DeliveryID, a.StatusCode, a.DurationMS, a.Excerpt, a.Error)
	return err
}

// MarkDelivered terminalizes a delivery as delivered.
func (s *WebhookStore) MarkDelivered(ctx context.Context, id string, attempts int) error {
	_, err := s.pool.Exec(ctx, storage.Query("MarkDeliveryDelivered"), id, attempts)
	return err
}

// Reschedule keeps a delivery pending with a new backoff clock.
func (s *WebhookStore) Reschedule(ctx context.Context, id string, attempts int, nextAt time.Time) error {
	_, err := s.pool.Exec(ctx, storage.Query("RescheduleDelivery"), id, attempts, nextAt)
	return err
}

// MarkDead moves a delivery to the dead-letter state.
func (s *WebhookStore) MarkDead(ctx context.Context, id string, attempts int) error {
	_, err := s.pool.Exec(ctx, storage.Query("MarkDeliveryDead"), id, attempts)
	return err
}

// EmitDeliveryEvent appends a webhook.delivery.* observability event to the source session's journal
// (spec §21.6 stream visibility). It allocates a session sequence and inserts the event in one
// transaction — the same seq-then-append shape the coordinator uses — with a NULL response_id
// (session-scoped metadata the per-response retention purge leaves untouched). Best-effort: the
// durable delivery/attempt rows are the source of truth, so a failed emit does not fail the delivery.
func (s *WebhookStore) EmitDeliveryEvent(ctx context.Context, project, sessionID, eventType string, payload []byte) error {
	ctx = storage.ScopeToTenant(ctx, project)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var seq int64
	if err := tx.QueryRow(ctx, storage.Query("AllocateSequence"), sessionID).Scan(&seq); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, storage.Query("AppendEvent"),
		newID("evt"), project, sessionID, nil, seq, eventType, payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- API-facing (tenant-scoped) methods ---

// EndpointCreate is the resolved create body for a webhook endpoint (spec §21.4).
type EndpointCreate struct {
	URL                     string
	EventFilter             []string
	APIRevision             string
	SigningSecretRef        string
	SigningSecretRefNext    string
	FixedHeaders            map[string]string
	TimeoutMS               int
	MaxAttempts             int
	RetryWindowSeconds      int
	AllowPrivateDestination bool
}

// CreateEndpoint registers an endpoint in the verified scope and returns its server-minted id.
func (s *WebhookStore) CreateEndpoint(ctx context.Context, project string, c EndpointCreate) (string, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	id := newID("whe")
	fixed := c.FixedHeaders
	if fixed == nil {
		fixed = map[string]string{}
	}
	fixedJSON, err := json.Marshal(fixed)
	if err != nil {
		return "", err
	}
	filter := c.EventFilter
	if filter == nil {
		filter = []string{}
	}
	var out string
	err = s.pool.QueryRow(ctx, storage.Query("CreateWebhookEndpoint"),
		id, project, c.URL, true, filter, c.APIRevision,
		c.SigningSecretRef, c.SigningSecretRefNext, fixedJSON,
		c.TimeoutMS, c.MaxAttempts, c.RetryWindowSeconds, c.AllowPrivateDestination, journalLag,
	).Scan(&out)
	return out, err
}

// EndpointView is the endpoint projection — one shape, rendered by both the list and the singular read.
//
// It carries no secret material and it carries the DELIVERY POLICY, which are two separate decisions about
// the ten fields EndpointCreate accepts. Four of them joined here in E29 T3 because they are behavioural
// configuration an operator must be able to read back: what this deployment will do when it sends. The
// secret refs joined on the same grounds plus one more — a *_ref is a HANDLE, and the value behind it is
// readable through no route, which is E25's environment-value rule applied to this family.
//
// fixed_headers is the one field of the six that stayed OUT, and not because it is uninteresting. It is a
// free map the operator writes, an Authorization header for the receiver is an ordinary thing to put in it,
// and reflecting it back would make a read route a credential read. There is deliberately no field for it
// here: the exclusion is structural, so it cannot be re-added by filling something in.
type EndpointView struct {
	ID                   string    `json:"id"`
	URL                  string    `json:"url"`
	Enabled              bool      `json:"enabled"`
	EventFilter          []string  `json:"event_filter"`
	APIRevision          string    `json:"api_revision,omitempty"`
	SigningSecretRef     string    `json:"signing_secret_ref"`
	SigningSecretRefNext string    `json:"signing_secret_ref_next"`
	TimeoutMS            int       `json:"timeout_ms"`
	MaxAttempts          int       `json:"max_attempts"`
	RetryWindowSeconds   int       `json:"retry_window_seconds"`
	AllowPrivate         bool      `json:"allow_private_destination"`
	CreatedAt            time.Time `json:"created_at"`
}

// ListEndpoints returns the scope's endpoints, newest first and then by id — a TOTAL order, so two reads of
// an unchanged table return one sequence. See ListWebhookEndpoints in webhooks.sql for what the tiebreaker
// is doing there.
func (s *WebhookStore) ListEndpoints(ctx context.Context, project string) ([]EndpointView, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListWebhookEndpoints"), project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EndpointView{}
	for rows.Next() {
		var e EndpointView
		if err := rows.Scan(&e.ID, &e.URL, &e.Enabled, &e.EventFilter, &e.APIRevision,
			&e.SigningSecretRef, &e.SigningSecretRefNext,
			&e.TimeoutMS, &e.MaxAttempts, &e.RetryWindowSeconds,
			&e.AllowPrivate, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeliveryView is a listed delivery projection.
type DeliveryView struct {
	ID            string    `json:"id"`
	EndpointID    string    `json:"endpoint_id"`
	EventID       string    `json:"event_id"`
	EventType     string    `json:"event_type"`
	State         string    `json:"state"`
	AttemptCount  int       `json:"attempt_count"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ListDeliveries returns the scope's deliveries, optionally filtered by state (state="" = all).
func (s *WebhookStore) ListDeliveries(ctx context.Context, project, state string, limit int) ([]DeliveryView, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, storage.Query("ListWebhookDeliveries"), project, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDeliveries(rows)
}

// GetDelivery returns a single delivery in scope, or (nil, false) if not found.
func (s *WebhookStore) GetDelivery(ctx context.Context, project, id string) (*DeliveryView, bool, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	rows, err := s.pool.Query(ctx, storage.Query("GetWebhookDelivery"), id, project)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	views, err := scanDeliveries(rows)
	if err != nil || len(views) == 0 {
		return nil, false, err
	}
	return &views[0], true, nil
}

func scanDeliveries(rows pgx.Rows) ([]DeliveryView, error) {
	out := []DeliveryView{}
	for rows.Next() {
		var d DeliveryView
		if err := rows.Scan(&d.ID, &d.EndpointID, &d.EventID, &d.EventType, &d.State, &d.AttemptCount, &d.NextAttemptAt, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// AttemptView is one sanitized attempt (spec §21.6): status, duration, excerpt, error — no secret.
type AttemptView struct {
	AttemptNumber int       `json:"attempt_number"`
	StatusCode    int       `json:"status_code"`
	DurationMS    int64     `json:"duration_ms"`
	Excerpt       string    `json:"response_excerpt"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// ListAttempts returns the sanitized attempt view for a delivery in scope.
func (s *WebhookStore) ListAttempts(ctx context.Context, project, deliveryID string) ([]AttemptView, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListDeliveryAttempts"), deliveryID, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AttemptView{}
	for rows.Next() {
		var a AttemptView
		if err := rows.Scan(&a.AttemptNumber, &a.StatusCode, &a.DurationMS, &a.Excerpt, &a.Error, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Redeliver revives a dead/pending delivery with the same id + payload (spec §21.6). Returns false if no
// such delivery exists in scope, and ErrDeliveryEndpointDeleted if the delivery is there but the endpoint it
// was addressed to has been unregistered — see that error's comment for why those are different answers.
func (s *WebhookStore) Redeliver(ctx context.Context, project, id string) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	var out string
	err := s.pool.QueryRow(ctx, storage.Query("RedeliverDelivery"), id, project).Scan(&out)
	if err == pgx.ErrNoRows {
		// The UPDATE matched nothing for one of two reasons and they deserve different answers. Ask which.
		var exists int
		switch probeErr := s.pool.QueryRow(ctx, storage.Query("DeliveryExists"), id, project).Scan(&exists); {
		case probeErr == pgx.ErrNoRows:
			return false, nil
		case probeErr != nil:
			return false, probeErr
		default:
			return false, ErrDeliveryEndpointDeleted
		}
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// GetEndpoint reads one endpoint in the verified scope. The projection is the list's, character for
// character (GetWebhookEndpoint in webhooks.sql sits next to ListWebhookEndpoints for that reason): a caller
// that got a different shape from the two routes would be looking at two resources.
func (s *WebhookStore) GetEndpoint(ctx context.Context, project, id string) (*EndpointView, bool, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	var e EndpointView
	err := s.pool.QueryRow(ctx, storage.Query("GetWebhookEndpoint"), id, project).Scan(
		&e.ID, &e.URL, &e.Enabled, &e.EventFilter, &e.APIRevision,
		&e.SigningSecretRef, &e.SigningSecretRefNext,
		&e.TimeoutMS, &e.MaxAttempts, &e.RetryWindowSeconds,
		&e.AllowPrivate, &e.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &e, true, nil
}

// DeleteEndpoint unregisters an endpoint in the verified scope and reports whether a row went. It is the
// capability the create's own comment has claimed since E11 ("a duplicate endpoint is operator-visible +
// deletable") and which nothing in the tree provided until E29 T3.
//
// It is IDEMPOTENT in the sense that matters: calling it a second time produces no further effect. It
// reports false rather than true on that second call, and the route above turns that into a 404 — the same
// posture DELETE /v1/schedules/{id} and DELETE /v1/slack-connections/{id} already take, so an operator meets
// one answer across the three destructive routes rather than two.
//
// WHAT IT DOES NOT TAKE WITH IT. Migration 000052 dropped the ON DELETE CASCADE that would have carried
// every delivery and every attempt away with the endpoint. A delivery is an audit record of a payload this
// deployment actually sent, and unregistering the address does not un-send it; the surviving rows keep the
// deleted id, unresolvable on purpose. Future sending stops because DueDeliveries inner-joins this table.
func (s *WebhookStore) DeleteEndpoint(ctx context.Context, project, id string) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	var out string
	err := s.pool.QueryRow(ctx, storage.Query("DeleteWebhookEndpoint"), id, project).Scan(&out)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	// 23503 here can only be trigger_revisions.callback_endpoint_id, the one referrer this table keeps that
	// refuses a delete. Rendering it as a typed refusal is the difference between telling an operator which
	// configuration is in the way and handing them a 500. It is classified by SQLSTATE rather than by
	// matching the constraint name, because a name is a string a migration can rename.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return false, ErrEndpointPinned
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
