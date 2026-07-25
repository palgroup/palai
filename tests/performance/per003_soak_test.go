//go:build performance

// PER-003 — long-session soak: bounded memory, bounded journal, compaction, SSE reconnect (spec §64.14).
//
// One session is kept alive for a BOUNDED WINDOW (default 90s) with a client attached to its stream the
// whole time, while new responses chain onto it. Four things are measured on the running binary:
//
//   - MEMORY: the control-plane process's RSS is sampled through the window; total growth must stay
//     inside a budget. A soak whose only claim is "it did not crash" is not a measurement.
//   - JOURNAL: bytes per event must stay flat — growth linear in events, not in session length.
//   - COMPACTION: the REAL §8.3/§22.2 retention sweep reclaims the payload bytes of terminal
//     store=false responses in this session, and the rows survive. The journal is navigable AND bounded.
//   - RECONNECT: the stream is reconnected repeatedly with after_sequence; each resume must continue at
//     exactly last+1 — zero gaps, zero duplicates — and the long-lived reader must observe a contiguous
//     sequence for the whole window.
//
// HONEST NAMING, LOUDLY: "soak" here is a BOUNDED WINDOW OF MINUTES on a laptop. It is NOT the RC soak
// §64.15 asks for; that is E18 §6 leg 7 and this case makes no claim about it. "Compaction" here is the
// implemented RETENTION payload scrub (§8.3/§22.2) — it is NOT §25.14 context compaction, which is
// evaluation-gated and needs real model steps (E08 rule: no tools to a real provider, and dispatch is
// off in this tier), so §25.14 is NOT exercised and NOT claimed anywhere in this run.
package performance

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

const per003Ceiling = "macOS + Docker Desktop profile; NO SLO and NO reference-hardware claim. 'Soak' is a " +
	"BOUNDED WINDOW OF MINUTES — the real RC soak is E18 §6 leg 7 and is NOT claimed here. 'Compaction' is " +
	"the implemented §8.3/§22.2 RETENTION payload scrub, NOT §25.14 context compaction (evaluation-gated, " +
	"needs real model steps; dispatch is off per the E08 rule, so §25.14 is not exercised or claimed)."

const (
	per003AppendTick   = 100 * time.Millisecond // one chained response per tick
	per003RSSTick      = 2 * time.Second
	per003ReconnectGap = 10 * time.Second
	// The compaction cohort is sized under the reaper's 100-row batch so ONE sweep reclaims all of it;
	// a partially-swept cohort would make "reclaimed" a timing accident.
	per003PurgeCohort = 40
)

func TestPER003LongSessionSoak(t *testing.T) {
	s := requireStack(t)
	window := envDuration("PALAI_PERF_SOAK_WINDOW", 90*time.Second)

	run := mustRun(t, "PER-003", fmt.Sprintf(
		"bounded soak window %s: one session with an attached SSE reader for the whole window, one chained "+
			"response every %s, RSS sampled every %s, stream reconnected every %s, %d store=false responses "+
			"left for the real retention sweep",
		window, per003AppendTick, per003RSSTick, per003ReconnectGap, per003PurgeCohort), per003Ceiling)

	run.Gate("soak_append_accept", 95, float64(500*time.Millisecond), "ns", "spec §54.3 (accepted mutation p95 < 500ms)")
	run.Gate("sse_reconnect_resume", 95, float64(time.Second), "ns",
		"case budget mirroring §54.3's first-SSE-event objective (§54.2 lists SSE reconnect as an availability SLI, not a latency one)")
	run.Gate("sse_reconnect_gaps", 100, 0, "events", "invariant (resume at exactly last+1: no gap, no duplicate)")
	// Observed on this profile: a fraction of a MiB over the window. 64 MiB is far above the noise and
	// far below a real leak — a budget that only a genuine regression can breach.
	run.Gate("control_plane_rss_growth_bytes", 100, 64*1024*1024, "bytes", "case budget (bounded growth over the window)")
	run.Gate("journal_bytes_per_event", 100, 4096, "bytes", "case budget (growth linear in events, not in session length)")
	run.MaxErrorRate(0)

	sessionID, _ := s.openSoakSession(t)

	// The compaction cohort: store=false responses chained onto the SAME session, so the sweep has to
	// scrub per-response and leave the rest of this session's journal alone (§22.2).
	cohort := make([]string, 0, per003PurgeCohort)
	for i := 0; i < per003PurgeCohort; i++ {
		id, elapsed, ok := s.appendToSession(sessionID, false)
		run.Latency("soak_append_accept", "purge-cohort", elapsed, ok)
		if !ok {
			t.Fatalf("could not seed the compaction cohort at %d", i)
		}
		cohort = append(cohort, id)
	}

	// A client attached for the whole window: this is the "long session" half, and its contiguity check
	// is what makes a silently-dropping stream a failure instead of a quiet pass.
	reader := s.attachReader(t, sessionID)

	rssStart := residentBytes(t)
	run.Observe("control_plane_rss_bytes", "start", rssStart, "bytes", true)

	deadline := time.Now().Add(window)
	nextRSS := time.Now().Add(per003RSSTick)
	nextReconnect := time.Now().Add(per003ReconnectGap)
	appends := 0
	for time.Now().Before(deadline) {
		_, elapsed, ok := s.appendToSession(sessionID, true)
		run.Latency("soak_append_accept", "soak", elapsed, ok)
		appends++

		if time.Now().After(nextRSS) {
			run.Observe("control_plane_rss_bytes", "soak", residentBytes(t), "bytes", true)
			nextRSS = time.Now().Add(per003RSSTick)
		}
		if time.Now().After(nextReconnect) {
			s.measureReconnect(t, run, sessionID)
			nextReconnect = time.Now().Add(per003ReconnectGap)
		}
		time.Sleep(per003AppendTick)
	}
	// One last reconnect after the appends stop, so the window always contains at least one even when a
	// caller shortens it below the reconnect interval.
	s.measureReconnect(t, run, sessionID)

	rssEnd := residentBytes(t)
	run.Observe("control_plane_rss_bytes", "end", rssEnd, "bytes", true)
	growth := rssEnd - rssStart
	if growth < 0 {
		growth = 0 // the process gave memory back; that is not growth
	}
	run.Observe("control_plane_rss_growth_bytes", "window", growth, "bytes", true)

	seen := reader.stop()
	if seen.gaps > 0 {
		t.Fatalf("the attached reader saw %d sequence gaps over the window; the stream dropped events", seen.gaps)
	}
	if seen.events == 0 {
		t.Fatal("the attached reader saw no events at all; the soak measured an idle stream")
	}
	t.Logf("PER-003 attached reader: %d events, contiguous, over %s (%d chained appends)", seen.events, window, appends)

	// Journal growth, before compaction.
	rowsBefore := s.queryInt(`SELECT count(*) FROM events WHERE session_id=$1`, sessionID)
	bytesBefore := s.queryInt(`SELECT COALESCE(sum(pg_column_size(payload)), 0) FROM events WHERE session_id=$1`, sessionID)
	if rowsBefore == 0 {
		t.Fatal("the session journal is empty; nothing was measured")
	}
	run.Observe("journal_bytes_per_event", "pre-compaction", float64(bytesBefore)/float64(rowsBefore), "bytes", true)
	run.Observe("journal_events", "pre-compaction", float64(rowsBefore), "events", true)

	s.compactCohort(t, run, sessionID, cohort, rowsBefore, bytesBefore)
	complete(t, run, "PER-003")
}

// openSoakSession admits the first response and returns the session it minted.
func (s *stack) openSoakSession(t *testing.T) (sessionID, responseID string) {
	t.Helper()
	body := s.mustPost("/v1/responses", `{"input":"soak open"}`, 202,
		map[string]string{"Idempotency-Key": newID("idem")})
	sessionID, _ = body["session_id"].(string)
	responseID, _ = body["id"].(string)
	if sessionID == "" || responseID == "" {
		t.Fatalf("soak session open carried no ids: %v", body)
	}
	return sessionID, responseID
}

// appendToSession chains one more response onto the session, growing its journal.
//
// A session admits ONE active root run at a time (409 active_run_exists), and dispatch is off in this
// tier, so the harness closes the previous turn itself before opening the next. That is exactly the
// shape of a long session — a sequence of completed turns — with the engine out of the loop per the E08
// rule. The close is outside the timed section, so it never flatters the admission latency.
func (s *stack) appendToSession(sessionID string, store bool) (responseID string, elapsed time.Duration, ok bool) {
	s.exec(`UPDATE runs SET state='completed' WHERE session_id=$1 AND state <> 'completed'`, sessionID)
	payload := jsonBody(map[string]any{"input": "soak append", "session_id": sessionID, "store": store})
	status, body, elapsed := s.do("POST", "/v1/responses", payload, map[string]string{"Idempotency-Key": newID("idem")})
	responseID, _ = body["id"].(string)
	if status != 202 || responseID == "" {
		s.t.Logf("chained append rejected: status=%d body=%v", status, body)
	}
	return responseID, elapsed, status == 202 && responseID != ""
}

// soakReader is the long-lived attached client.
type soakReader struct {
	conn   *sseConn
	done   chan struct{}
	mu     sync.Mutex
	events int
	gaps   int
}

type readerResult struct{ events, gaps int }

// attachReader opens a stream and drains it for the whole window, counting events and any sequence
// discontinuity. A dropped or duplicated event is a gap.
func (s *stack) attachReader(t *testing.T, sessionID string) *soakReader {
	t.Helper()
	conn, _, err := s.openStream(sessionID, "")
	if err != nil {
		t.Fatalf("attach long-lived reader: %v", err)
	}
	r := &soakReader{conn: conn, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		last := 0
		for {
			_, seq, ok := r.conn.next()
			if !ok {
				return
			}
			r.mu.Lock()
			r.events++
			if last != 0 && seq != last+1 {
				r.gaps++
			}
			last = seq
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *soakReader) stop() readerResult {
	r.conn.close()
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return readerResult{events: r.events, gaps: r.gaps}
}

// measureReconnect resumes the stream at after_sequence=head-1 and requires the next event to be
// exactly head: a resume that skips or repeats is a gap, recorded as a gated sample rather than only a
// log line.
func (s *stack) measureReconnect(t *testing.T, run *Run, sessionID string) {
	t.Helper()
	head := s.queryInt(`SELECT COALESCE(max(seq), 0) FROM events WHERE session_id=$1`, sessionID)
	if head < 1 {
		t.Fatal("cannot measure a reconnect against an empty journal")
	}
	start := time.Now()
	conn, _, err := s.openStream(sessionID, fmt.Sprintf("after_sequence=%d", head-1))
	if err != nil {
		run.Latency("sse_reconnect_resume", "resume", time.Since(start), false)
		t.Fatalf("reconnect: %v", err)
	}
	defer conn.close()
	_, seq, ok := conn.next()
	elapsed := time.Since(start)
	run.Latency("sse_reconnect_resume", "resume", elapsed, ok)
	if !ok {
		t.Fatal("reconnect delivered no event")
	}
	gap := seq - int(head)
	if gap < 0 {
		gap = -gap
	}
	run.Observe("sse_reconnect_gaps", "resume", float64(gap), "events", true)
}

// compactCohort drives the REAL retention sweep: the cohort is made terminal and aged past the TTL the
// binary was started with, and the supervised reaper is then given time to scrub. The proof is that the
// payload bytes fall while the ROWS remain — a bounded journal that is still navigable.
func (s *stack) compactCohort(t *testing.T, run *Run, sessionID string, cohort []string, rowsBefore, bytesBefore int64) {
	t.Helper()
	cohortBytes := s.queryInt(
		`SELECT COALESCE(sum(pg_column_size(payload)), 0) FROM events WHERE response_id = ANY($1)`, cohort)
	if cohortBytes == 0 {
		t.Fatal("the compaction cohort carries no journal payload; the sweep would have nothing to reclaim")
	}
	// Terminal and aged: the reaper only touches store=false responses past their TTL. Dispatch is off in
	// this tier, so nothing else will ever move these rows out of queued.
	s.exec(`UPDATE responses SET state='completed', updated_at = clock_timestamp() - interval '1 hour'
	        WHERE id = ANY($1)`, cohort)

	start := time.Now()
	deadline := start.Add(90 * time.Second) // the supervised reaper sweeps on a 30s ticker
	var bytesAfter int64
	for {
		bytesAfter = s.queryInt(`SELECT COALESCE(sum(pg_column_size(payload)), 0) FROM events WHERE session_id=$1`, sessionID)
		if bytesAfter < bytesBefore {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the retention sweep reclaimed nothing in %s (journal still %d bytes); "+
				"is PALAI_RETENTION_STORE_FALSE_TTL set on the binary?", time.Since(start), bytesAfter)
		}
		time.Sleep(time.Second)
	}
	run.Latency("journal_compaction_sweep", "retention", time.Since(start), true)

	rowsAfter := s.queryInt(`SELECT count(*) FROM events WHERE session_id=$1`, sessionID)
	if rowsAfter != rowsBefore {
		t.Fatalf("compaction changed the row count %d -> %d; retention scrubs payloads, it does not "+
			"delete journal rows (§22.2)", rowsBefore, rowsAfter)
	}
	reclaimed := bytesBefore - bytesAfter
	run.Observe("journal_payload_bytes_reclaimed", "retention", float64(reclaimed), "bytes", true)
	run.Observe("journal_bytes_per_event", "post-compaction", float64(bytesAfter)/float64(rowsAfter), "bytes", true)
	t.Logf("PER-003 compaction (§8.3/§22.2 retention scrub, NOT §25.14 context compaction): %d bytes "+
		"reclaimed of %d, %d journal rows intact", reclaimed, bytesBefore, rowsAfter)
}
