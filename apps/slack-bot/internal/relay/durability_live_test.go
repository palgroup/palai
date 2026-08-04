//go:build live

// THE KILL HALF of the durability proof — the half a unit test cannot give.
//
// Everything in resume_test.go is a claim about what this package does with a fake store and a fake Slack.
// The claim an operator actually cares about is different in kind: "an answer survives the process being
// killed". That one is only provable by killing a process, so this file is the process that dies.
//
// It wires InboundDeps through the SAME constructors apps/slack-bot/main.go uses — NewPalaiClient,
// NewChannelSlackStreamer, NewInboundDeps — against the running control plane, the real bot database and
// the real Slack workspace, drives one real turn through relay.HandleEvent, waits until the durable record
// shows the answer being written, and then calls os.Exit. THE EXIT IS THE POINT: no defer runs, no stream
// is closed, no record is cleared — exactly the state `pkill` leaves, which is the state the bot has been
// left in dozens of times while this tree was worked on.
//
// The RECOVERY half is deliberately NOT here. It is performed by the shipped binary, started the ordinary
// way, so what finishes the answer is the product an operator runs rather than a test that happens to
// contain the same code. See the report for the transcript of the two halves together.
package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/slack-bot/internal/store"
	palai "github.com/palgroup/palai/sdks/go"
)

// TestARealTurnIsAbandonedMidAnswer starts a real run into a real Slack thread and then kills this
// process while the answer is still being written.
//
// IT ALWAYS "FAILS" BY EXITING — there is no passing path, because a process that returned normally would
// have run relay.Run's deferred stop() and closed the very thing this proof needs left open. The exit code
// is 97 so the caller can tell "killed as designed" from "the fixture broke".
func TestARealTurnIsAbandonedMidAnswer(t *testing.T) {
	apiURL := liveEnv(t, "PALAI_API_URL")
	apiKey := liveEnv(t, "PALAI_API_KEY")
	dbURL := liveEnv(t, "PALAI_BOT_DATABASE_URL")
	botID := liveEnv(t, "PALAI_BOT_ID")
	token := []byte(liveEnv(t, "SLACK_BOT_TOKEN"))
	channel := liveEnv(t, "SLACK_TEST_CHANNEL")
	teamID := liveEnv(t, "SLACK_TEAM_ID")
	userID := liveEnv(t, "SLACK_RECIPIENT_USER_ID")
	botUserID := liveEnv(t, "SLACK_BOT_USER_ID")
	agentRevision := liveEnv(t, "PALAI_AGENT_REVISION_ID")
	bindingID := os.Getenv("PALAI_REPOSITORY_BINDING_ID")
	prompt := os.Getenv("PALAI_LIVE_PROMPT")
	if prompt == "" {
		prompt = "Bu projede kaç Swift dosyası var? Say ve tek cümlede söyle."
	}

	ctx := context.Background()

	// A REAL THREAD. The root is posted with the bot's own token because nothing here can speak as a human;
	// what matters for this proof is that the thread, the channel and the recipient are real, so the stream
	// the relay opens is a stream Slack actually renders for the operator.
	root, err := postRoot(ctx, token, channel, "durability proof — the bot will be killed while answering this")
	if err != nil {
		t.Fatalf("post the thread root: %v", err)
	}
	t.Logf("PROOF thread=%s/%s", channel, root)

	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open the bot store: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	client, err := palai.New(palai.WithBaseURL(apiURL), palai.WithAPIKey(apiKey))
	if err != nil {
		t.Fatalf("palai client: %v", err)
	}

	deps := NewInboundDeps(
		st,
		NewPalaiClient(client),
		NewChannelSlackStreamer(http.DefaultClient, "https://slack.com/api", token),
		func(f func()) { go f() },
		func(context.Context, string, string, palai.Event) {},
		func(err error, sessionID, ch, thread string) { t.Logf("run failed: %v", err) },
		botID, botUserID, agentRevision, bindingID,
	)
	deps.Logf = t.Logf

	ev := slack.Event{
		Type: "app_mention", Kind: slack.KindMessage,
		TeamID: teamID, ChannelID: channel, ThreadTS: root, MessageTS: root,
		UserID: userID, Text: prompt,
	}
	if err := HandleEvent(ctx, deps, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// WAIT FOR THE RUN TO BE GENUINELY UNDERWAY before killing it, or the proof is about a different
	// window. "Underway" is read from the DURABLE record rather than from this process's memory: the stream
	// has a ts (chat.startStream came back) and the cursor has moved (Slack has taken at least one event).
	// That is precisely the state whose recovery is being claimed.
	pending, err := waitForDelivery(ctx, st, botID, teamID, channel, root)
	if err != nil {
		t.Fatalf("the run never reached a recoverable state: %v", err)
	}

	fmt.Printf("KILLED session=%s stream_ts=%s last_sequence=%d channel=%s thread=%s\n",
		pending.SessionID, pending.StreamTS, pending.LastSequence, channel, root)
	// THE KILL. os.Exit runs no deferred function anywhere in this process — relay.Run's stop() never
	// fires, the stream is left open, the record is left pending. There is deliberately no clean exit path
	// from this test.
	os.Exit(97)
}

// waitForDelivery polls the durable record until the thread is mid-answer: a stream is open and at least
// one event has been confirmed delivered.
func waitForDelivery(ctx context.Context, st *store.Store, botID, teamID, channel, thread string) (store.PendingRun, error) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := st.PendingDeliveries(ctx, botID)
		if err != nil {
			return store.PendingRun{}, err
		}
		for _, p := range rows {
			if p.TeamID == teamID && p.ChannelID == channel && p.ThreadTS == thread &&
				p.StreamTS != "" && p.LastSequence > 0 {
				return p, nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return store.PendingRun{}, fmt.Errorf("no pending delivery with an open stream and a moved cursor appeared within 90s")
}

// postRoot posts the plain message whose ts becomes the proof thread.
func postRoot(ctx context.Context, token []byte, channel, text string) (string, error) {
	res, err := slack.PostMessage(ctx, http.DefaultClient, slack.PostRequest{
		MethodURL: "https://slack.com/api/chat.postMessage", Token: token,
		Body: mustJSON(map[string]any{"channel": channel, "text": text}),
	}, slack.PostOptions{})
	if err != nil {
		return "", err
	}
	return res.MessageTS, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestTheAbandonedThreadIsStillOwedAnAnswer is the assertion made BETWEEN the two halves: after the kill
// and before the shipped binary starts, the database still names the thread, the stream and the cursor.
//
// It is separate from the kill because it must run in a process that did NOT die, and it is the fact the
// whole design rests on — if it were false, no recovery could exist however well written.
func TestTheAbandonedThreadIsStillOwedAnAnswer(t *testing.T) {
	dbURL := liveEnv(t, "PALAI_BOT_DATABASE_URL")
	botID := liveEnv(t, "PALAI_BOT_ID")
	thread := liveEnv(t, "PALAI_PROOF_THREAD_TS")

	ctx := context.Background()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open the bot store: %v", err)
	}
	defer st.Close()

	rows, err := st.PendingDeliveries(ctx, botID)
	if err != nil {
		t.Fatalf("PendingDeliveries: %v", err)
	}
	for _, p := range rows {
		if p.ThreadTS != thread {
			continue
		}
		if p.StreamTS == "" || p.SessionID == "" || p.RecipientUserID == "" {
			t.Fatalf("the record names %+v — a recovery needs the session, the open message and the recipient", p)
		}
		t.Logf("STILL OWED session=%s stream_ts=%s last_sequence=%d recipient=%s", p.SessionID, p.StreamTS, p.LastSequence, p.RecipientUserID)
		return
	}
	t.Fatalf("thread %s is not marked pending — nothing would ever finish its answer; pending rows: %+v", thread, rows)
}

// TestTheRecoveredThreadIsSettled is the assertion made AFTER the shipped binary has recovered: the debt
// is cleared and the cursor has passed the point the dead process reached.
func TestTheRecoveredThreadIsSettled(t *testing.T) {
	dbURL := liveEnv(t, "PALAI_BOT_DATABASE_URL")
	botID := liveEnv(t, "PALAI_BOT_ID")
	thread := liveEnv(t, "PALAI_PROOF_THREAD_TS")

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var runPending bool
	var lastSequence int64
	var streamTS, sessionID string
	err = pool.QueryRow(ctx,
		`SELECT run_pending, last_sequence, stream_ts, session_id FROM thread_sessions
		 WHERE bot_id=$1 AND thread_ts=$2`, botID, thread).Scan(&runPending, &lastSequence, &streamTS, &sessionID)
	if err != nil {
		t.Fatalf("read the recovered row: %v", err)
	}
	if runPending {
		t.Fatalf("thread %s is still marked pending after recovery (cursor %d)", thread, lastSequence)
	}
	if streamTS != "" {
		t.Fatalf("thread %s still names an open stream %s after recovery", thread, streamTS)
	}
	t.Logf("SETTLED session=%s last_sequence=%d", sessionID, lastSequence)
}

// TestTheThreadCarriesExactlyOneAnswer reads the thread back and checks the recovered half landed in the
// message the dead process opened rather than in a second one.
//
// IT READS THE THREAD, NOT THE RELAY'S OWN SEND LOG, because the property is about what a human sees. The
// blocks of a LIVE stream are not readable through conversations.replies (measured: a message mid-stream
// comes back with no blocks at all), which is exactly why this leg runs after the stream is closed.
func TestTheThreadCarriesExactlyOneAnswer(t *testing.T) {
	token := []byte(liveEnv(t, "SLACK_BOT_TOKEN"))
	channel := liveEnv(t, "SLACK_TEST_CHANNEL")
	thread := liveEnv(t, "PALAI_PROOF_THREAD_TS")

	messages, _, err := slack.ThreadReplies(context.Background(), http.DefaultClient, "https://slack.com/api", token,
		channel, thread, 50)
	if err != nil {
		t.Fatalf("conversations.replies: %v", err)
	}
	var botMessages int
	for _, m := range messages {
		if m.TS == thread {
			continue // the root this fixture posted
		}
		botMessages++
		t.Logf("MESSAGE ts=%s text=%q", m.TS, truncateForLog(m.Text))
	}
	if botMessages != 1 {
		t.Fatalf("the thread carries %d bot message(s), want exactly 1 — a recovery that posted a second one leaves the first stuck at Working…", botMessages)
	}
}

func truncateForLog(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
