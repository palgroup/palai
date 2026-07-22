package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
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
func (s *WebhookStore) ReadJournalForEndpoint(ctx context.Context, org, project string, cursor int64, filter []string, limit int) ([]journalEvent, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	if filter == nil {
		filter = []string{}
	}
	rows, err := s.pool.Query(ctx, storage.Query("ReadJournalForEndpoint"), org, project, cursor, filter, limit)
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
func (s *WebhookStore) EmitDeliveryEvent(ctx context.Context, org, project, sessionID, eventType string, payload []byte) error {
	ctx = storage.ScopeToTenant(ctx, org, project)
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
		newID("evt"), org, project, sessionID, nil, seq, eventType, payload); err != nil {
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
func (s *WebhookStore) CreateEndpoint(ctx context.Context, org, project string, c EndpointCreate) (string, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
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
		id, org, project, c.URL, true, filter, c.APIRevision,
		c.SigningSecretRef, c.SigningSecretRefNext, fixedJSON,
		c.TimeoutMS, c.MaxAttempts, c.RetryWindowSeconds, c.AllowPrivateDestination, journalLag,
	).Scan(&out)
	return out, err
}

// EndpointView is a listed endpoint projection (no secret material).
type EndpointView struct {
	ID           string            `json:"id"`
	URL          string            `json:"url"`
	Enabled      bool              `json:"enabled"`
	EventFilter  []string          `json:"event_filter"`
	APIRevision  string            `json:"api_revision,omitempty"`
	AllowPrivate bool              `json:"allow_private_destination"`
	CreatedAt    time.Time         `json:"created_at"`
	Extra        map[string]string `json:"-"`
}

// ListEndpoints returns the scope's endpoints, newest first.
func (s *WebhookStore) ListEndpoints(ctx context.Context, org, project string) ([]EndpointView, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListWebhookEndpoints"), org, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EndpointView{}
	for rows.Next() {
		var e EndpointView
		if err := rows.Scan(&e.ID, &e.URL, &e.Enabled, &e.EventFilter, &e.APIRevision, &e.AllowPrivate, &e.CreatedAt); err != nil {
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
func (s *WebhookStore) ListDeliveries(ctx context.Context, org, project, state string, limit int) ([]DeliveryView, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, storage.Query("ListWebhookDeliveries"), org, project, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDeliveries(rows)
}

// GetDelivery returns a single delivery in scope, or (nil, false) if not found.
func (s *WebhookStore) GetDelivery(ctx context.Context, org, project, id string) (*DeliveryView, bool, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	rows, err := s.pool.Query(ctx, storage.Query("GetWebhookDelivery"), id, org, project)
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
func (s *WebhookStore) ListAttempts(ctx context.Context, org, project, deliveryID string) ([]AttemptView, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListDeliveryAttempts"), deliveryID, org, project)
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

// Redeliver revives a dead/pending delivery with the same id + payload (spec §21.6). Returns false if
// no such delivery exists in scope.
func (s *WebhookStore) Redeliver(ctx context.Context, org, project, id string) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	var out string
	err := s.pool.QueryRow(ctx, storage.Query("RedeliverDelivery"), id, org, project).Scan(&out)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
