//go:build e2e

// Package sse holds the end-to-end proof for the resumable event stream. It runs
// only under `make test-e2e TEST=sse`, which starts a throwaway PostgreSQL
// container AND the real palai-control-plane binary, then exports
// PALAI_E2E_BASE_URL (the server) and PALAI_E2E_POSTGRES_URL (the database). The
// build tag keeps it out of the credential-free, Docker-free unit tier.
//
// Every test is black-box over HTTP against the running control plane: the write
// side advances runs and seeds the journal through packages/coordinator on the
// shared database, the read side streams the journal over Server-Sent Events. The
// journal and the SSE handler live in apps/control-plane/internal, so they are
// exercised through the binary, never imported here.
package sse

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"

	"github.com/palgroup/palai/storage"
)

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is required; run make test-e2e TEST=sse", name)
	}
	return v
}

// serverWriteTimeout is the write deadline the running server was configured with
// (the e2e script sets it for both the binary and this test). A stalled-consumer
// test waits a safe multiple of it rather than guessing a sleep.
func serverWriteTimeout() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("PALAI_SSE_WRITE_TIMEOUT")); err == nil && d > 0 {
		return d
	}
	return 200 * time.Millisecond
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// harness talks to the running control plane over HTTP and drives runs / seeds the
// journal directly on the shared database.
type harness struct {
	t      *testing.T
	base   string
	spine  *coordinator.Store
	tenant coordinator.Tenant
	token  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	base := strings.TrimRight(requireEnv(t, "PALAI_E2E_BASE_URL"), "/")
	ctx := context.Background()

	spine, err := coordinator.Open(ctx, requireEnv(t, "PALAI_E2E_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(spine.Close)
	if err := spine.Migrate(ctx); err != nil { // idempotent; the binary already migrated
		t.Fatalf("Migrate() error = %v", err)
	}

	token := newID("e2e-tok")
	tenant := seedTenantWithKey(t, spine.Pool(), token)
	return &harness{t: t, base: base, spine: spine, tenant: tenant, token: token}
}

// seedTenantWithKey creates org -> project -> principal -> api_key; the stored
// verifier is the hash of token, never token itself.
func seedTenantWithKey(t *testing.T, pool *pgxpool.Pool, token string) coordinator.Tenant {
	t.Helper()
	ctx := context.Background()
	tenant := coordinator.Tenant{Organization: newID("org"), Project: newID("prj")}
	principalID := newID("prin")
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("seed exec %q error = %v", sql, err)
		}
	}
	exec(`INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	exec(`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	exec(`INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1, $2, $3, 'service')`,
		principalID, tenant.Organization, tenant.Project)
	exec(`INSERT INTO api_keys (id, organization_id, project_id, principal_id, key_hash) VALUES ($1, $2, $3, $4, $5)`,
		newID("key"), tenant.Organization, tenant.Project, principalID, coordinator.HashAPIKey(token))
	return tenant
}

// seedSession inserts a bare session in the harness tenant (no admission), for
// read-path tests that populate the journal directly.
func (h *harness) seedSession() string {
	h.t.Helper()
	sessionID := newID("ses")
	if _, err := h.spine.Pool().Exec(storage.WithSystemScope(context.Background()),
		`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`,
		sessionID, h.tenant.Organization, h.tenant.Project); err != nil {
		h.t.Fatalf("seed session error = %v", err)
	}
	return sessionID
}

// seedEvent appends a single journal event with an arbitrary type and payload and
// advances the sequence allocator. It returns the event id.
func (h *harness) seedEvent(sessionID string, seq int, typ, payloadJSON string) string {
	h.t.Helper()
	ctx := context.Background()
	id := newID("evt")
	if _, err := h.spine.Pool().Exec(storage.WithSystemScope(ctx),
		`INSERT INTO events (id, organization_id, project_id, session_id, seq, type, payload)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`,
		id, h.tenant.Organization, h.tenant.Project, sessionID, seq, typ, payloadJSON); err != nil {
		h.t.Fatalf("seed event error = %v", err)
	}
	if _, err := h.spine.Pool().Exec(storage.WithSystemScope(ctx),
		`INSERT INTO session_sequences (session_id, last_seq) VALUES ($1, $2)
		 ON CONFLICT (session_id) DO UPDATE SET last_seq = GREATEST(session_sequences.last_seq, EXCLUDED.last_seq)`,
		sessionID, seq); err != nil {
		h.t.Fatalf("seed sequence error = %v", err)
	}
	return id
}

// seedBulkEvents appends count non-terminal events (each padded to ~payloadBytes)
// at seq 1..count, then one terminal event at seq count+1, and advances the
// sequence allocator. It floods the journal without thousands of round-trips; the
// event ids are globally unique so repeated (-count) runs never collide on the PK.
func (h *harness) seedBulkEvents(sessionID string, count, payloadBytes int) {
	h.t.Helper()
	ctx := context.Background()
	tag := newID("bulk")
	if _, err := h.spine.Pool().Exec(storage.WithSystemScope(ctx), `
		INSERT INTO events (id, organization_id, project_id, session_id, seq, type, payload)
		SELECT 'evt_' || $6 || '_' || lpad(g::text, 9, '0'), $1, $2, $3, g, 'run.running.v1',
		       jsonb_build_object('pad', repeat('x', $5))
		FROM generate_series(1, $4) AS g`,
		h.tenant.Organization, h.tenant.Project, sessionID, count, payloadBytes, tag); err != nil {
		h.t.Fatalf("seed bulk events error = %v", err)
	}
	if _, err := h.spine.Pool().Exec(storage.WithSystemScope(ctx), `
		INSERT INTO events (id, organization_id, project_id, session_id, seq, type, payload)
		VALUES ('evt_' || $5 || '_terminal', $1, $2, $3, $4, 'run.completed.v1', '{}'::jsonb)`,
		h.tenant.Organization, h.tenant.Project, sessionID, count+1, tag); err != nil {
		h.t.Fatalf("seed terminal event error = %v", err)
	}
	if _, err := h.spine.Pool().Exec(storage.WithSystemScope(ctx),
		`INSERT INTO session_sequences (session_id, last_seq) VALUES ($1, $2)
		 ON CONFLICT (session_id) DO UPDATE SET last_seq = EXCLUDED.last_seq`,
		sessionID, count+1); err != nil {
		h.t.Fatalf("seed sequence error = %v", err)
	}
}

// createSession admits a response over real HTTP, minting a session + root run and
// the run.queued.v1 birth event (seq 1). It returns the session and run ids.
func (h *harness) createSession() (sessionID, runID string) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.base+"/v1/responses", strings.NewReader(`{"input":"do the work"}`))
	if err != nil {
		h.t.Fatalf("build POST error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Idempotency-Key", newID("idem"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST /v1/responses error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("POST status = %d, want 202", resp.StatusCode)
	}
	var r contracts.Response
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		h.t.Fatalf("decode response error = %v", err)
	}
	if r.SessionID == "" || r.RunID == "" {
		h.t.Fatalf("response missing ids: %+v", r)
	}
	return string(r.SessionID), string(r.RunID)
}

// getEvents issues the streaming GET with the harness credential and returns the
// raw response, for status assertions (e.g. the tenant-boundary 404). The caller
// closes the body.
func (h *harness) getEvents(sessionID string, headers map[string]string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.base+"/v1/sessions/"+sessionID+"/events", nil)
	if err != nil {
		h.t.Fatalf("build GET error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("GET events error = %v", err)
	}
	return resp
}

// apply advances a run one transition, appending its public event to the journal.
func (h *harness) apply(runID string, cmd statemachines.RunCommand) {
	h.t.Helper()
	if _, err := h.spine.ApplyRunTransition(context.Background(), h.tenant, runID, cmd); err != nil {
		h.t.Fatalf("ApplyRunTransition(%s) error = %v", cmd, err)
	}
}

// runState reads the authoritative run state straight from the database, so a test
// can prove a client disconnect did not move it.
func (h *harness) runState(runID string) string {
	h.t.Helper()
	var state string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		runID, h.tenant.Organization, h.tenant.Project).Scan(&state); err != nil {
		h.t.Fatalf("read run state error = %v", err)
	}
	return state
}

// sseEvent is a parsed SSE frame. seq and data come from the CloudEvents envelope.
type sseEvent struct {
	id    string
	event string
	seq   int
	data  map[string]any
}

// sseConn is a live SSE reader. Cancel disconnects the client (the server sees the
// request context fire); the journal is unaffected.
type sseConn struct {
	cancel context.CancelFunc
	body   io.ReadCloser
	sc     *bufio.Scanner
}

// openStream issues the streaming GET and returns after the 200 headers arrive.
func (h *harness) openStream(sessionID string, headers map[string]string) *sseConn {
	return h.openStreamQuery(sessionID, "", headers)
}

// openStreamQuery is openStream with an explicit URL query (e.g. after_sequence).
func (h *harness) openStreamQuery(sessionID, rawQuery string, headers map[string]string) *sseConn {
	h.t.Helper()
	target := h.base + "/v1/sessions/" + sessionID + "/events"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		h.t.Fatalf("build GET error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		h.t.Fatalf("GET events error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		h.t.Fatalf("GET events status = %d, want 200", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return &sseConn{cancel: cancel, body: resp.Body, sc: sc}
}

// next returns the next event frame, skipping heartbeat comment lines. ok is false
// at EOF (the server closed the stream, e.g. after a terminal event).
func (c *sseConn) next(t *testing.T) (sseEvent, bool) {
	t.Helper()
	var ev sseEvent
	var dataLine string
	got := false
	for c.sc.Scan() {
		line := c.sc.Text()
		switch {
		case line == "":
			if got {
				env := decodeEnvelope(t, dataLine)
				if env.Type != ev.event {
					t.Fatalf("event line %q disagrees with envelope type %q", ev.event, env.Type)
				}
				ev.seq = env.Sequence
				ev.data = env.Data
				return ev, true
			}
		case strings.HasPrefix(line, ":"):
			// heartbeat comment — ignore
		case strings.HasPrefix(line, "id: "):
			ev.id = strings.TrimPrefix(line, "id: ")
			got = true
		case strings.HasPrefix(line, "event: "):
			ev.event = strings.TrimPrefix(line, "event: ")
			got = true
		case strings.HasPrefix(line, "data: "):
			dataLine = strings.TrimPrefix(line, "data: ")
			got = true
		}
	}
	return sseEvent{}, false
}

// nextRawLine returns the next stream line verbatim (heartbeat comments included),
// for tests that assert on keep-alive framing rather than parsed events.
func (c *sseConn) nextRawLine() (string, bool) {
	if c.sc.Scan() {
		return c.sc.Text(), true
	}
	return "", false
}

func (c *sseConn) close() {
	c.cancel()
	c.body.Close()
}

func decodeEnvelope(t *testing.T, data string) contracts.Event {
	t.Helper()
	var envelope contracts.Event
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		t.Fatalf("decode event data %q error = %v", data, err)
	}
	if envelope.Specversion != "1.0" {
		t.Fatalf("event specversion = %q, want 1.0", envelope.Specversion)
	}
	return envelope
}

// assertContiguous checks the collected sequences are exactly first..first+len-1
// with no gaps or duplicates, and that every event id is unique.
func assertContiguous(t *testing.T, events []sseEvent, first int) {
	t.Helper()
	seenID := map[string]bool{}
	for i, e := range events {
		if want := first + i; e.seq != want {
			t.Fatalf("event %d sequence = %d, want %d (not contiguous)", i, e.seq, want)
		}
		if seenID[e.id] {
			t.Fatalf("duplicate event id %q", e.id)
		}
		seenID[e.id] = true
	}
}

// dialStalled opens the SSE endpoint over a raw socket with a tiny read buffer and
// then reads nothing, simulating a consumer too slow to drain the stream.
func (h *harness) dialStalled(sessionID string) net.Conn {
	h.t.Helper()
	u, err := url.Parse(h.base)
	if err != nil {
		h.t.Fatalf("parse base url error = %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		h.t.Fatalf("dial error = %v", err)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(4096) // shrink the receive window so the server blocks sooner
	}
	req := fmt.Sprintf("GET /v1/sessions/%s/events HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n",
		sessionID, u.Host, h.token)
	if _, err := conn.Write([]byte(req)); err != nil {
		h.t.Fatalf("write request error = %v", err)
	}
	return conn
}
