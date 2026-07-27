//go:build live

// The AGENT PANEL live smoke (E20 T2). Like its siblings it is written now and runs unchanged the moment the
// owner reinstalls the app with the new manifest — no code change, no second credential.
//
// WHAT ONLY A REAL WORKSPACE CAN SETTLE, and it is more than usual here because the panel is Slack's NEWEST
// surface (agent_view shipped 2026-07) and our fake is therefore the youngest fake in this repo:
//
//   - that a panel message really arrives as `message.im` carrying channel_type "im" (the field the DM scope
//     exemption turns on — if Slack ever sent something else, the exemption would simply never fire and the
//     panel would die under a configured allow-list, silently);
//   - that its follow-ups really carry `thread_ts`, i.e. that the panel really is ONE conversation
//     (developing-agents says so; a page can be stale);
//   - S16(e), the measurement this leg exists for: whether the panel renders our approval buttons at all.
//     NOTHING IS CLAIMED ABOUT THAT — the test prints what it saw. An unmeasured claim is worse than none.
//
// It SKIPS rather than fails when a credential is missing, naming the variable and the handover row.
package live

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLiveSlackPanelDMBirthsExactlyOneRun: the owner opens the Palai agent in Slack's sidebar and types. One
// run, in one session, correlated to the DM.
//
// SETUP the owner does once (E20 plan §0.1): paste the updated deploy/slack/app-manifest.yaml, REINSTALL the
// app (agent_view + im:history are new scopes, so the old install will not deliver message.im), and keep the
// control plane running with PALAI_SLACK_SOCKET_TEAM_ID set.
func TestLiveSlackPanelDMBirthsExactlyOneRun(t *testing.T) {
	dbURL := need(t, "PALAI_SLACK_LIVE_POSTGRES_URL", "the RUNNING control plane's Postgres URL (make compose-up prints it)")
	team := need(t, "SLACK_TEAM_ID", "§0.1 — workspace admin → about, or any event payload's team_id")

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to the running stack: %v", err)
	}
	defer pool.Close()

	start := time.Now().UTC()
	t.Logf("watching workspace %s for a PANEL-born run. Open Palai from the Slack sidebar (or DM the bot directly) and send a message now.", team)

	deadline := time.Now().Add(liveWindow(t))
	byEvent := map[string][]string{}
	for time.Now().Before(deadline) {
		byEvent = pollSlackBornRuns(t, ctx, pool, BornRunsByTeam, team, start)
		if len(byEvent) > 0 {
			// Settle before concluding: a redelivery (or an overlapping socket during a refresh) is exactly
			// when a second run would appear, and appearing late is how it would escape a single read.
			time.Sleep(10 * time.Second)
			byEvent = pollSlackBornRuns(t, ctx, pool, BornRunsByTeam, team, start)
			break
		}
		time.Sleep(2 * time.Second)
	}
	if len(byEvent) == 0 {
		t.Fatalf(`no panel-born run appeared in %s. In likelihood order:
  1. the app was not REINSTALLED after the manifest change — im:history and agent_view are new scopes, and
     without im:history Slack never delivers message.im at all (nothing logs this; the panel is just silent);
  2. message.im is not in the app's subscribed bot events;
  3. the control plane is not running with PALAI_SLACK_SOCKET_TEAM_ID=%s, or its connection's app_token_ref
     does not resolve (its log would say so at startup);
  4. the message went to a CHANNEL rather than the panel, and the connection has a non-empty allowed_channels
     that excludes it.`, liveWindow(t), team)
	}
	for eventID, responses := range byEvent {
		if len(responses) != 1 {
			t.Fatalf("panel source event %s produced %d responses (%v), want exactly 1 — the panel is a THIRD ENTRANCE into one admission bridge, not a second one", eventID, len(responses), responses)
		}
	}

	// The correlation, and it is the half that makes the panel a conversation rather than a series of
	// strangers: every panel run must sit in a thread↔session row, and the channel it names is REPORTED
	// rather than asserted — "D…" meaning DM is a convention no Slack page commits to, so it is a thing for
	// the operator to read, not a thing for a test to conclude.
	rows, err := pool.Query(ctx,
		`SELECT channel_id, thread_ts, session_id FROM slack_thread_sessions
		  WHERE team_id = $1 AND created_at > $2 ORDER BY created_at`, team, start)
	if err != nil {
		t.Fatalf("read the panel's thread correlations: %v", err)
	}
	defer rows.Close()
	correlations := 0
	sessions := map[string]bool{}
	for rows.Next() {
		var channel, thread, session string
		if err := rows.Scan(&channel, &thread, &session); err != nil {
			t.Fatalf("scan a correlation: %v", err)
		}
		correlations++
		sessions[session] = true
		t.Logf("panel correlation: channel=%s thread_ts=%s session=%s", channel, thread, session)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the panel's thread correlations: %v", err)
	}
	if correlations == 0 {
		t.Fatal("a panel run was born but no thread↔session row was written — the conversation has no session to carry its history, so every follow-up would start over")
	}
	t.Logf(`%d real panel event(s) each produced exactly one run across %d session(s).
S16(e) IS NOT ANSWERED BY THIS TEST: whether the agent panel renders our approval buttons is something to
LOOK AT in Slack, and nothing here claims it either way.`, len(byEvent), len(sessions))
}
