//go:build performance

// PER-001 — single-shot and SSE load: percentiles and error rate under a configured load (spec §64.14,
// §54.3). Three §54.3 latency objectives are measured against the RUNNING control-plane binary over real
// HTTP, each with the spec's own target as the (configurable) threshold:
//
//	API metadata read      p95 < 300ms   GET /v1/responses/{id}
//	accepted mutation      p95 < 500ms   POST /v1/responses (202 accepted)
//	first SSE event        p95 < 1s      GET /v1/sessions/{id}/events, time to the FIRST EVENT
//
// HONEST CEILING: macOS + Docker Desktop, one laptop shared with other work — NO SLO and NO
// reference-hardware claim; §54.3's targets are product goals whose real measurement is E18 §6 leg 3,
// and §54.3 also requires segmentation by region/tier/environment, which a single local stack cannot
// provide. Dispatch is disabled (E08 rule: the engine opens no tool to a real provider), so this
// measures the API + journal spine: admission, read-back and stream start — never model or engine time.
// The load shape is a configured concurrency, not a saturation search: "under target load" here means
// the shape stamped in profile.json, and nothing beyond it is claimed.
package performance

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

const per001Ceiling = "macOS + Docker Desktop profile on a shared laptop; NO SLO and NO reference-hardware " +
	"claim (§54.3 targets are product goals — real measurement is E18 §6 leg 3, which also owns the " +
	"region/tier/environment segmentation §54.3 requires). Dispatch is OFF (E08 rule): these are API + " +
	"journal spine latencies, never model or engine time. The load shape is the configured one below, " +
	"not a saturation point."

func TestPER001SingleShotAndSSELoad(t *testing.T) {
	s := requireStack(t)
	concurrency := envInt("PALAI_PERF_CONCURRENCY", 8)
	iterations := envInt("PALAI_PERF_ITERATIONS", 25)

	run := mustRun(t, "PER-001", fmt.Sprintf(
		"%d concurrent clients x %d iterations = %d single-shot admissions, %d metadata reads and %d SSE "+
			"stream starts against one control-plane process + one Postgres container",
		concurrency, iterations, concurrency*iterations, concurrency*iterations, concurrency*iterations),
		per001Ceiling)

	run.Gate("api_metadata_read", 95, float64(300*time.Millisecond), "ns", "spec §54.3 (API metadata read p95 < 300ms)")
	run.Gate("mutation_accept", 95, float64(500*time.Millisecond), "ns", "spec §54.3 (accepted mutation p95 < 500ms)")
	run.Gate("sse_first_event", 95, float64(time.Second), "ns", "spec §54.3 (first SSE event p95 < 1s)")
	// §54.3 is a latency clause; the error rate is PER-001's own requirement ("percentiles/error rates
	// under target load"). Zero: a 5xx under this shape is a defect, not a percentile.
	run.MaxErrorRate(0)

	var wg sync.WaitGroup
	for c := 0; c < concurrency; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				status, body, elapsed := s.do("POST", "/v1/responses", `{"input":"perf single-shot"}`,
					map[string]string{"Idempotency-Key": newID("idem")})
				run.Latency("mutation_accept", "single-shot", elapsed, status == 202)
				if status != 202 {
					continue
				}
				responseID, _ := body["id"].(string)
				sessionID, _ := body["session_id"].(string)
				if responseID == "" || sessionID == "" {
					t.Errorf("202 carried no ids: %v", body)
					return
				}

				readStatus, _, readElapsed := s.do("GET", "/v1/responses/"+responseID, "", nil)
				run.Latency("api_metadata_read", "single-shot", readElapsed, readStatus == 200)

				// The stream's first event is the run.queued.v1 birth event the admission just wrote, so
				// this is genuinely "time to first event" and not a wait for future work.
				firstEvent, seq, err := s.firstEvent(sessionID)
				run.Latency("sse_first_event", "cold-open", firstEvent, err == nil)
				if err != nil {
					t.Errorf("first SSE event: %v", err)
					return
				}
				if seq != 1 {
					t.Errorf("first event sequence = %d, want the birth event at 1", seq)
					return
				}
			}
		}()
	}
	wg.Wait()

	complete(t, run, "PER-001")
}
