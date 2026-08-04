//go:build performance

// Shared plumbing for the PER cases that drive the RUNNING control-plane binary: a seeded tenant, an
// authenticated HTTP client, and the durable spine for the write-side/read-back steps. It mirrors
// tests/e2e/sse's harness — a separate build target cannot import another package's test helpers, so the
// small scaffolding is duplicated rather than shared.
//
// Everything a PER case measures goes through the real binary over real HTTP. The database handle exists
// only for the steps a client cannot perform (seeding a tenant, terminating a queued run so a gate
// opens) and for reading back what the API does not expose.
package performance

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// stack is the running control plane plus the durable spine behind it.
type stack struct {
	t      *testing.T
	base   string
	spine  *coordinator.Store
	tenant coordinator.Tenant
	token  string
	client *http.Client
}

// loadClient is the measuring client. http.DefaultClient keeps 2 idle connections per host, so a
// concurrent load shape would spend its time in Go's own connection pool and the harness would publish
// client-side queuing as server latency. The pool is sized above every load shape in this package.
func loadClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = 256
	return &http.Client{Transport: transport, Timeout: 60 * time.Second}
}

// requireStack skips when the tier's stack is absent: `make test-performance TEST=service` provides it.
// A skip is the honest outcome for a case whose seam is not running — never a silent pass.
func requireStack(t *testing.T) *stack {
	t.Helper()
	base := strings.TrimRight(os.Getenv("PALAI_PERF_BASE_URL"), "/")
	pgURL := os.Getenv("PALAI_PERF_POSTGRES_URL")
	if base == "" || pgURL == "" {
		t.Skip("PALAI_PERF_BASE_URL + PALAI_PERF_POSTGRES_URL are required; run make test-performance TEST=service")
	}
	ctx := context.Background()
	spine, err := coordinator.Open(ctx, pgURL)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(spine.Close)
	if err := spine.Migrate(ctx); err != nil { // idempotent; the binary already migrated
		t.Fatalf("Migrate() error = %v", err)
	}
	token := newID("perf-tok")
	return &stack{t: t, base: base, spine: spine, tenant: seedTenantWithKey(t, spine.Pool(), token),
		token: token, client: loadClient()}
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// seedTenantWithKey creates project -> principal -> api_key; the stored verifier is the hash of
// token, never token itself.
func seedTenantWithKey(t *testing.T, pool *pgxpool.Pool, token string) coordinator.Tenant {
	t.Helper()
	ctx := context.Background()
	tenant := coordinator.Tenant{Project: newID("prj")}
	principalID := newID("prin")
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("seed exec %q error = %v", sql, err)
		}
	}
	exec(`INSERT INTO projects (id) VALUES ($1)`, tenant.Project)
	exec(`INSERT INTO principals (id, project_id, kind) VALUES ($1, $2, 'service')`,
		principalID, tenant.Project)
	exec(`INSERT INTO api_keys (id, project_id, principal_id, key_hash) VALUES ($1, $2, $3, $4)`,
		newID("key"), tenant.Project, principalID, coordinator.HashAPIKey(token))
	return tenant
}

// do issues one authenticated request and returns the status, the decoded body and the elapsed wall
// time. The timing brackets the whole round trip, which is what a caller experiences.
func (s *stack) do(method, path, body string, headers map[string]string) (int, map[string]any, time.Duration) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.base+path, reader)
	if err != nil {
		s.t.Fatalf("build %s %s error = %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, time.Since(start)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded, elapsed
}

// mustPost fails the test unless the status matches want.
func (s *stack) mustPost(path, body string, want int, headers map[string]string) map[string]any {
	s.t.Helper()
	status, decoded, _ := s.do(http.MethodPost, path, body, headers)
	if status != want {
		s.t.Fatalf("POST %s status = %d, want %d (%v)", path, status, want, decoded)
	}
	return decoded
}

// exec runs one statement on the durable spine under system scope, for the steps a client cannot do.
func (s *stack) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.spine.Pool().Exec(storage.WithSystemScope(context.Background()), sql, args...); err != nil {
		s.t.Fatalf("exec %q error = %v", sql, err)
	}
}

// queryInt reads a single integer (a count, a byte total) under system scope.
func (s *stack) queryInt(sql string, args ...any) int64 {
	s.t.Helper()
	var v int64
	if err := s.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()), sql, args...).Scan(&v); err != nil {
		s.t.Fatalf("query %q error = %v", sql, err)
	}
	return v
}

// complete writes the run's artifacts and reports where they landed, so a reader of the test log can
// find the profile behind every number.
func complete(t *testing.T, run *Run, caseID string) Summary {
	t.Helper()
	dir := OutputDir(caseID)
	sum, err := run.Complete(dir)
	if err != nil {
		t.Fatalf("%s gate FAILED (artifacts in %s): %v", caseID, dir, err)
	}
	p := run.Profile()
	t.Logf("%s PASS — artifacts=%s profile=%q %d cores / %.1f GiB / %s / load=%q",
		caseID, dir, p.Machine, p.Cores, float64(p.MemoryBytes)/(1<<30), p.Docker, p.LoadShape)
	// The ceiling goes where the numbers are READ. A log line of §54.3-shaped percentiles with the
	// qualification in a sibling file reads as a result; with the ceiling next to it, it reads as a
	// profile number.
	t.Logf("  CEILING: %s", p.Ceiling)
	for _, st := range sum.Stats {
		t.Logf("  %-32s n=%-4d p50=%-12s p95=%-12s p99=%-12s errors=%d", st.Metric, st.Count,
			format(st.P50, st.Unit), format(st.P95, st.Unit), format(st.P99, st.Unit), st.Errors)
	}
	return sum
}

// mustFailGate is the shape every RED fixture asserts: the run completes, writes its evidence, and the
// gate REFUSES it. A fixture that merely errors out proves nothing about the gate.
func mustFailGate(t *testing.T, run *Run, caseID, wantMetric string) {
	t.Helper()
	sum, err := run.Complete(OutputDir(caseID))
	if err == nil {
		t.Fatalf("%s: the deliberately-bad fixture PASSED the gate; the gate has no teeth", caseID)
	}
	if !errors.Is(err, ErrThreshold) { // the sentinel, not its message text
		t.Fatalf("%s: want a threshold-gate failure, got %v", caseID, err)
	}
	found := false
	for _, f := range sum.Failures {
		if strings.HasPrefix(f, wantMetric) {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s: gate failed but not on %s: %v", caseID, wantMetric, sum.Failures)
	}
	t.Logf("%s RED fixture correctly REFUSED: %s", caseID, strings.Join(sum.Failures, "; "))
}

func jsonBody(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("encode body: %v", err))
	}
	return string(raw)
}

// sseConn is a live SSE reader over the real stream endpoint. Cancel disconnects the client; the
// journal is unaffected.
type sseConn struct {
	cancel context.CancelFunc
	body   io.ReadCloser
	sc     *bufio.Scanner
}

// openStream issues the streaming GET and returns once the 200 headers are back, together with the
// time-to-headers. rawQuery carries the resume cursor (after_sequence=N) on a reconnect.
func (s *stack) openStream(sessionID, rawQuery string) (*sseConn, time.Duration, error) {
	target := s.base + "/v1/sessions/" + sessionID + "/events"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	start := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		cancel()
		return nil, time.Since(start), err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, time.Since(start), fmt.Errorf("GET events status = %d, want 200", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return &sseConn{cancel: cancel, body: resp.Body, sc: sc}, time.Since(start), nil
}

// next returns the next event's id and CloudEvents sequence, skipping heartbeat comments. ok is false
// at EOF.
func (c *sseConn) next() (id string, seq int, ok bool) {
	var dataLine string
	for c.sc.Scan() {
		line := c.sc.Text()
		switch {
		case line == "":
			if id != "" || dataLine != "" {
				var envelope struct {
					Sequence int `json:"sequence"`
				}
				_ = json.Unmarshal([]byte(dataLine), &envelope)
				return id, envelope.Sequence, true
			}
		case strings.HasPrefix(line, ":"): // heartbeat comment
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			dataLine = strings.TrimPrefix(line, "data: ")
		}
	}
	return "", 0, false
}

func (c *sseConn) close() {
	c.cancel()
	c.body.Close()
}

// firstEvent measures time-to-FIRST-EVENT, not time-to-headers: §54.3's "first SSE event p95 below 1 s"
// is about the event, and a server that answered 200 and then stalled would pass the weaker reading.
func (s *stack) firstEvent(sessionID string) (time.Duration, int, error) {
	start := time.Now()
	conn, _, err := s.openStream(sessionID, "")
	if err != nil {
		return time.Since(start), 0, err
	}
	defer conn.close()
	_, seq, ok := conn.next()
	if !ok {
		return time.Since(start), 0, errors.New("stream closed before the first event")
	}
	return time.Since(start), seq, nil
}

// residentBytes reads the control-plane process's RSS. The binary is a child of the tier script, which
// exports its pid; ps is the portable reading (KiB on both macOS and Linux).
func residentBytes(t *testing.T) float64 {
	t.Helper()
	pid := os.Getenv("PALAI_PERF_SERVER_PID")
	if pid == "" {
		t.Skip("PALAI_PERF_SERVER_PID is required; run make test-performance TEST=service")
	}
	out, err := exec.Command("ps", "-o", "rss=", "-p", pid).Output()
	if err != nil {
		t.Fatalf("read rss of pid %s: %v", pid, err)
	}
	kib, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("parse rss %q: %v", out, err)
	}
	return kib * 1024
}

// envInt reads a load-shape knob, so a machine under pressure can lower the shape without editing code
// (and the chosen value lands in the profile's load_shape string either way).
func envInt(name string, def int) int {
	if raw := os.Getenv(name); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	if raw := os.Getenv(name); raw != "" {
		if v, err := time.ParseDuration(raw); err == nil && v > 0 {
			return v
		}
	}
	return def
}
