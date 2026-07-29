//go:build live

// DOES THIS MAC RUN MORE THAN ONE SESSION AT A TIME? That is the whole product requirement for
// renting Macs and running Palai natively on each one, and until this file existed nobody had ever
// demonstrated it — every green suite in the tree proves runs COMPLETE, and a serial stack completes
// runs too.
//
// So the assertion here is deliberately not "N runs finished". It is:
//
//	two runs were IN FLIGHT AT THE SAME INSTANT, on TWO DIFFERENT dispatch workers.
//
// Both halves matter. Overlapping intervals alone could be one worker interleaving; distinct owners
// alone could be two workers taking turns. Together they are concurrency, and a stack that regresses
// to serial fails this test rather than quietly taking twice as long.
//
// WHERE THE EVIDENCE COMES FROM. Not from log lines and not from this test's own stopwatch — from the
// control plane's durable ledger. `job_attempts.owner` is the dispatch worker that claimed the job
// (`control-plane-<pid>-<i>`, main.go startDispatch), `job_attempts.started_at` is when it claimed it,
// and `durable_jobs.updated_at` is when the job reached its terminal state. Postgres timestamps, one
// clock, no reporter in between.
//
// MEASURED ON A NATIVE MAC, 2026-07-29 (M2 Pro, 12 core, 16 GiB), against `palai up --native`:
//
//	PALAI_DISPATCH_WORKERS=1  two runs submitted together ran BACK TO BACK — the second attempt started
//	                          43ms after the first job finished. 1 distinct worker, 0 overlapping pairs.
//	PALAI_DISPATCH_WORKERS=2  the same two runs OVERLAPPED for 11.92s on control-plane-38340-0 and
//	                          control-plane-38340-1, and `docker ps` showed two engine containers alive
//	                          at once.
//
// CONCURRENCY IS TWO NUMBERS and the stack is as parallel as the smaller: PALAI_DISPATCH_WORKERS (how
// many runs the control plane dispatches) and PALAI_RUNNER_CONCURRENCY (how many engine leases one
// runner parks). This test reads BOTH off the running stack and says so when it skips, because a stack
// configured serial should report "not configured for concurrency", not "concurrency is broken".
//
// It SKIPS with no stack: it needs a real one, brought up by `palai up --native`. Point PALAI_STACK_DIR
// at that stack's .palai directory (default: ./.palai at the repo root).
package concurrency

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// stack is what `palai up` leaves on disk: the API to submit to and the Postgres to read the ledger
// from. Deriving both from the stack directory is what keeps this test a one-liner to run — there is
// no second set of env vars to keep in sync with the bring-up.
type stack struct {
	baseURL string
	apiKey  string
	pool    *pgxpool.Pool
}

func openStack(t *testing.T) *stack {
	t.Helper()
	dir := strings.TrimSpace(os.Getenv("PALAI_STACK_DIR"))
	if dir == "" {
		dir = filepath.Join("..", "..", "..", ".palai")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolve PALAI_STACK_DIR: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(abs, "config.json"))
	if err != nil {
		t.Skipf("no stack at %s (%v) — bring one up with `palai up --native` or set PALAI_STACK_DIR", abs, err)
	}
	var cfg struct {
		BaseURL string `json:"base_url"`
		PGPort  int    `json:"pg_port"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s/config.json: %v", abs, err)
	}
	key, err := os.ReadFile(filepath.Join(abs, "api-key"))
	if err != nil {
		t.Fatalf("read api key: %v", err)
	}
	pw, err := os.ReadFile(filepath.Join(abs, "secrets", "pg-password"))
	if err != nil {
		t.Fatalf("read pg password: %v", err)
	}
	// The credential goes straight into the DSN string and never into argv, a log line or a t.Logf.
	dsn := fmt.Sprintf("postgres://palai:%s@127.0.0.1:%d/palai", strings.TrimSpace(string(pw)), cfg.PGPort)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("stack at %s is not reachable on Postgres :%d (%v) — is it up?", abs, cfg.PGPort, err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("stack at %s has no live Postgres on :%d (%v) — is it up?", abs, cfg.PGPort, err)
	}
	t.Cleanup(pool.Close)
	return &stack{baseURL: strings.TrimSpace(cfg.BaseURL), apiKey: strings.TrimSpace(string(key)), pool: pool}
}

// submit starts one run and returns as soon as the API has accepted it (202). It deliberately does NOT
// wait: the point of this test is what happens while several are in flight.
func (s *stack) submit(t *testing.T, i int) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"input": fmt.Sprintf("Reply with the single word: session%d", i)})
	req, err := http.NewRequest(http.MethodPost, s.baseURL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	// POST /v1/responses is idempotency-gated; without this header it answers 400.
	req.Header.Set("Idempotency-Key", fmt.Sprintf("live-concurrency-%d-%d", time.Now().UnixNano(), i))
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST /v1/responses: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v1/responses = %d, want 202", resp.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return created.ID
}

// TestLiveNativeMacRunsSessionsConcurrently is the regression fence: it goes red the day this stack
// starts running submitted sessions one after another.
//
// IT DOES NOT SKIP A SERIAL STACK, and that is deliberate. An earlier draft read the worker count back
// out of the ledger and skipped when it looked like 1, so that "configured serial" would report as a
// skip rather than a failure. That guard cannot tell the two interesting cases apart: PALAI_DISPATCH_WORKERS=1
// (nobody asked for concurrency) and PALAI_DISPATCH_WORKERS=4 with PALAI_RUNNER_CONCURRENCY=1 (four
// workers queueing behind one engine lease) both show ONE owner doing all the work. The second is the
// exact defect this fence exists to catch, and skipping on it would report green. So a serial stack
// FAILS here, and the message names both knobs.
func TestLiveNativeMacRunsSessionsConcurrently(t *testing.T) {
	s := openStack(t)

	n := 2
	if v := strings.TrimSpace(os.Getenv("PALAI_CONCURRENCY_N")); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 2 {
			t.Fatalf("PALAI_CONCURRENCY_N=%q must be an integer >= 2", v)
		}
		n = parsed
	}

	// The cutoff: only runs this test starts are evidence. Taken from the DATABASE's clock, because
	// every timestamp compared below is the database's — a laptop clock a few hundred ms off would
	// otherwise silently include or exclude rows.
	var since time.Time
	if err := s.pool.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&since); err != nil {
		t.Fatalf("read database clock: %v", err)
	}

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); s.submit(t, i) }()
	}
	wg.Wait()

	// Wait for all of them to leave the queue, then read the ledger. A generous ceiling: a real model
	// round trip on a loaded Mac took 12s in the measurement above, and a retry chain can take longer.
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var running int
		if err := s.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM durable_jobs
			  WHERE kind='response.run' AND status IN ('queued','running') AND created_at > $1`, since).Scan(&running); err != nil {
			t.Fatalf("poll job status: %v", err)
		}
		if running == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d runs were still queued/running after 5m — the stack did not finish the work", running, n)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// THE PROOF. One interval per RUN — the attempt holding the highest fence, because a retried job has
	// several job_attempts rows and counting those as separate runs would manufacture overlap out of
	// retries (it read 2 submitted runs as 10 concurrent on the first draft of this query).
	const ledger = `
		WITH iv AS (
		  SELECT DISTINCT ON (dj.id) dj.id, ja.owner, ja.started_at AS s, dj.updated_at AS e
		  FROM durable_jobs dj JOIN job_attempts ja ON ja.job_id = dj.id
		  WHERE dj.kind='response.run' AND ja.started_at > $1
		  ORDER BY dj.id, ja.fence DESC
		)
		SELECT (SELECT count(*) FROM iv),
		       (SELECT count(DISTINCT owner) FROM iv),
		       (SELECT count(*) FROM iv a JOIN iv b
		          ON a.id < b.id AND a.s < b.e AND b.s < a.e AND a.owner <> b.owner),
		       COALESCE((SELECT max(extract(epoch FROM LEAST(a.e,b.e) - GREATEST(a.s,b.s))) FROM iv a JOIN iv b
		          ON a.id < b.id AND a.s < b.e AND b.s < a.e AND a.owner <> b.owner), 0)`
	var runs, owners, overlappingPairs int
	var longestOverlap float64
	if err := s.pool.QueryRow(context.Background(), ledger, since).Scan(&runs, &owners, &overlappingPairs, &longestOverlap); err != nil {
		t.Fatalf("read the run ledger: %v", err)
	}
	t.Logf("ledger: %d runs, %d distinct dispatch workers, %d overlapping pair(s), longest overlap %.2fs",
		runs, owners, overlappingPairs, longestOverlap)

	if runs < n {
		t.Fatalf("the ledger has %d dispatched runs, want %d — some submission never reached a dispatch worker", runs, n)
	}
	if owners < 2 {
		t.Fatalf("all %d runs were claimed by %d dispatch worker — this stack executed them SERIALLY. Bring it up with "+
			"PALAI_DISPATCH_WORKERS=2 AND PALAI_RUNNER_CONCURRENCY=2: the stack is only as parallel as the smaller of "+
			"the two, so dispatch alone leaves every worker but one blocked on the single engine lease", runs, owners)
	}
	if overlappingPairs < 1 {
		t.Fatalf("%d runs across %d dispatch workers, but NO TWO WERE EVER IN FLIGHT AT THE SAME TIME — the workers "+
			"took turns, which is serial execution with extra steps. The usual cause is PALAI_RUNNER_CONCURRENCY=1: "+
			"one engine lease slot, so every dispatch worker but one blocks in Dial", runs, owners)
	}
}
