//go:build live

// The Slack Events API live smoke (E19 T1). Both tests are written NOW and run UNCHANGED the moment the
// owner supplies credentials — that is the phase's whole shape: correctness comes from the published
// contract, and the live leg exists to settle the things a document cannot.
//
// It settles exactly one thing no fixture can, and it is the reason this file exists:
//
//	D3 (plan §3.5) — our entire Slack dedupe assumes a redelivery repeats the SAME event_id, and the
//	official Events API page does NOT say so. It says event_id is globally unique across workspaces, and it
//	says an unacknowledged delivery is retried three times. It never joins those two sentences. Every local
//	proof in this repo REPEATS the id because our fixture chooses to, which is exactly the kind of
//	self-confirming fake E17 T10 got burned by. Only real Slack can answer it, and
//	TestLiveSlackRetryCarriesTheSameEventID asks it directly, by refusing to acknowledge a real delivery.
//
// Neither test fails when a credential is missing: it SKIPS, naming the env var and the §0 handover row
// that supplies it, so a partial handover reports partial-green instead of a red wall.
package live

import (
	"context"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/adapters/integrations/slack"
)

// need returns an env var's value or skips, naming where §0 says to get it.
func need(t *testing.T, name, where string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set — supply it from E19 plan §0 (%s) and re-run; no code changes are needed", name, where)
	}
	return v
}

// liveWindow is how long a test waits for the operator to act in Slack.
func liveWindow(t *testing.T) time.Duration {
	t.Helper()
	if raw := os.Getenv("PALAI_SLACK_LIVE_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("PALAI_SLACK_LIVE_TIMEOUT=%q is not a duration", raw)
		}
		return d
	}
	return 3 * time.Minute
}

// delivery is one real callback as this receiver saw it.
type delivery struct {
	eventID     string
	retryNum    string
	retryReason string
	at          time.Time
}

// TestLiveSlackRetryCarriesTheSameEventID is the D3 assertion, and it is deliberately hostile to our own
// assumption: it stands up a receiver, lets REAL Slack deliver a REAL event, and then REFUSES to acknowledge
// it — no 2xx, no x-slack-no-retry — so Slack does what its documentation says and redelivers.
//
// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-07-25) — a delivery without a 2xx inside
// three seconds is treated as failed and retried three times (immediately, +1 minute, +5 minutes), carrying
// x-slack-retry-num and x-slack-retry-reason.
//
// It then asserts three things at once, none of which any local fixture can establish:
//
//  1. D3: the retry's event_id EQUALS the original's. If this fails, the follow-up named in the plan opens —
//     a composite dedupe key (event_id + event_time + team_id). Nothing is built for that today (YAGNI), and
//     this test is the evidence that would justify it.
//  2. D2: the reason on a timed-out ack really is http_timeout, i.e. the counter we added measures what we
//     claim it measures.
//  3. D9: the v0 signature our verifier computes matches the one REAL Slack sends, over a real body.
//
// SETUP the operator does once: point the app's Request URL at this receiver (Socket Mode OFF for this leg —
// Socket Mode carries no signature and no HTTP retry, which is exactly what is under test here), then send
// one message the app is subscribed to. Expect it to take ~1 minute: the second retry is delivered a minute
// after the first.
func TestLiveSlackRetryCarriesTheSameEventID(t *testing.T) {
	secret := []byte(need(t, "SLACK_SIGNING_SECRET", "§0.1 — App → Basic Information → App Credentials → Signing Secret"))
	addr := os.Getenv("PALAI_SLACK_LIVE_LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8099"
	}

	var (
		mu         sync.Mutex
		seen       []delivery
		enough     = make(chan struct{})
		closedOnce sync.Once
	)
	srv := &http.Server{Addr: addr, ReadHeaderTimeout: 5 * time.Second}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/slack/events", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		// The handshake still has to work or Slack will not accept the Request URL at all.
		if challenge, ok := slack.ParseChallenge(body); ok {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(challenge))
			return
		}
		// REAL Slack, REAL signature, OUR verifier. A failure here is a D9 finding, not a flake.
		if err := slack.VerifySignature(secret, r.Header.Get(slack.HeaderTimestamp), r.Header.Get(slack.HeaderSignature),
			body, time.Now(), slack.DefaultTolerance); err != nil {
			t.Errorf("a REAL Slack request failed OUR v0 verification (%v) — the signing scheme in adapters/integrations/slack does not match what Slack sends", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		ev, err := slack.MapEvent(body, "", false)
		if err != nil {
			w.WriteHeader(http.StatusOK) // an ignored/other event: ack it and keep waiting
			return
		}
		mu.Lock()
		seen = append(seen, delivery{
			eventID:     ev.SourceEventID,
			retryNum:    r.Header.Get(slack.HeaderRetryNum),
			retryReason: slack.RetryReason(r.Header.Get(slack.HeaderRetryReason)),
			at:          time.Now(),
		})
		n := len(seen)
		mu.Unlock()
		t.Logf("delivery %d: event_id=%s retry_num=%q retry_reason=%q", n, ev.SourceEventID,
			r.Header.Get(slack.HeaderRetryNum), r.Header.Get(slack.HeaderRetryReason))

		if n < 2 {
			// STALL past the documented three-second budget and then fail WITHOUT the suppress header, so
			// Slack redelivers. This is the only way to observe a real retry.
			time.Sleep(4 * time.Second)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// We have the retry: ack so Slack stops, and stop waiting.
		w.WriteHeader(http.StatusOK)
		closedOnce.Do(func() { close(enough) })
	})
	srv.Handler = mux

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			t.Errorf("live receiver: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	t.Logf("listening on http://%s/v1/slack/events — point the Slack app's Request URL here (through your tunnel) and send one subscribed message. The first delivery will be STALLED on purpose; the retry follows within ~1 minute.", addr)
	select {
	case <-enough:
	case <-time.After(liveWindow(t)):
		mu.Lock()
		got := len(seen)
		mu.Unlock()
		t.Fatalf("saw %d deliveries in %s, want at least 2 (an original and its retry) — if the FIRST never arrived the Request URL is not reaching this receiver; if only the first arrived, Slack did not retry a stalled non-2xx and D1's premise needs re-checking", got, liveWindow(t))
	}

	mu.Lock()
	defer mu.Unlock()
	first, retry := seen[0], seen[1]
	if retry.retryNum == "" {
		t.Fatalf("the second delivery carried no %s header; it is a different event, not a redelivery", slack.HeaderRetryNum)
	}
	// (1) D3 — the assumption our whole dedupe rests on.
	if retry.eventID != first.eventID {
		t.Fatalf("D3 IS FALSE: the retry carried event_id %q but the original was %q. Slack's docs never promised this, and our dedupe (idempotency key = team_id + event_id) therefore does NOT collapse a redelivery. OPEN THE FOLLOW-UP the plan names: a composite key of event_id + event_time + team_id.",
			retry.eventID, first.eventID)
	}
	t.Logf("D3 CONFIRMED against a real workspace: the retry repeated event_id %s (%s after the original). The assumption labelled in adapters/integrations/slack now has a receipt.",
		retry.eventID, retry.at.Sub(first.at).Round(time.Second))
	// (2) D2 — a stalled ack must be reported as a timeout, or the counter measures nothing.
	if retry.retryReason != slack.RetryReasonHTTPTimeout {
		t.Errorf("we stalled past three seconds and Slack called it %q, want %q — the http_timeout counter is supposed to be the ack-budget signal (plan §3.5 D2)",
			retry.retryReason, slack.RetryReasonHTTPTimeout)
	}
}

// TestLiveSlackMentionBirthsExactlyOneRun is the wiring half of §6 leg 1 (slack — a REAL workspace external
// receipt; leg 2 is the foreign A2A peer, see CapabilityOperatorLegs in tests/uat/evidence.go): with the
// Request URL pointed at a RUNNING control plane (not at this test), one real @-mention must produce exactly
// one run — including through any redelivery Slack decides to send.
//
// It observes the running stack's DATABASE rather than standing up its own server, because the thing under
// test is the deployed route, not a copy of it. The operator registers the workspace through the normal
// admin API first (a signing_secret_ref handle, never an inline secret — the store refuses one).
func TestLiveSlackMentionBirthsExactlyOneRun(t *testing.T) {
	dbURL := need(t, "PALAI_SLACK_LIVE_POSTGRES_URL", "the RUNNING control plane's Postgres URL (make compose-up prints it)")
	team := need(t, "SLACK_TEAM_ID", "§0.1 — workspace admin → about, or any event payload's team_id")

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to the running stack: %v", err)
	}
	defer pool.Close()

	// Everything born from here on. Slack-sourced responses are identified by the input projection the
	// admission bridge writes (source=slack + the workspace), so this cannot count someone else's traffic.
	const bornSince = `SELECT COALESCE(input->>'event_id',''), id
	                     FROM responses
	                    WHERE input->>'source' = 'slack' AND input->>'team_id' = $1 AND created_at > $2`
	start := time.Now().UTC()
	t.Logf("watching for a Slack-born run in workspace %s — @-mention the bot in a channel it has been invited to", team)

	deadline := time.Now().Add(liveWindow(t))
	byEvent := map[string][]string{}
	for time.Now().Before(deadline) {
		byEvent = pollSlackBornRuns(t, ctx, pool, bornSince, team, start)
		if len(byEvent) > 0 {
			// Give any redelivery its first window (Slack's immediate retry) before concluding, then re-read
			// ONCE and stop. Polling on after a hit would burn the whole three-minute window on a clean pass.
			time.Sleep(10 * time.Second)
			byEvent = pollSlackBornRuns(t, ctx, pool, bornSince, team, start)
			break
		}
		time.Sleep(2 * time.Second)
	}
	if len(byEvent) == 0 {
		t.Fatalf("no Slack-born run appeared in %s — check that the Request URL reaches this stack, the app is subscribed to app_mention, and the workspace is registered with a resolvable signing_secret_ref", liveWindow(t))
	}
	for eventID, responses := range byEvent {
		if len(responses) != 1 {
			t.Fatalf("source event %s produced %d responses (%v), want exactly 1 — one effect per source event is SLK-001/002, and a real workspace is where it counts", eventID, len(responses), responses)
		}
	}
	t.Logf("%d real Slack event(s) each produced exactly one run. §6 leg 1's HTTP-transport half has a receipt; the remaining Slack legs (interactivity, Socket Mode) belong to T2/T3.", len(byEvent))
}

// pollSlackBornRuns reads the Slack-born responses since `start`, grouped by source event id.
func pollSlackBornRuns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query, team string, start time.Time) map[string][]string {
	t.Helper()
	rows, err := pool.Query(ctx, query, team, start)
	if err != nil {
		t.Fatalf("poll for Slack-born runs: %v", err)
	}
	defer rows.Close()
	byEvent := map[string][]string{}
	for rows.Next() {
		var eventID, respID string
		if err := rows.Scan(&eventID, &respID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		byEvent[eventID] = append(byEvent[eventID], respID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("poll for Slack-born runs: %v", err)
	}
	return byEvent
}
