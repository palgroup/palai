// This file is the first time the bot's pieces meet in ONE process. Until Task 12.5 each of them was
// satisfied only by a fake inside its own package: relay.Deps and relay.InboundDeps had never been
// constructed side by side, StartStream's recipient fields had never been filled from a real Slack event,
// and nothing had ever turned an approval message's own button back into a decision.
//
// The seams below are fakes because a real Slack workspace and a real control plane are not what these
// assertions are about — the wire formats they stand in for are real, though: every payload is a
// documented Slack shape, and the approve click is built from the button THIS PROCESS just posted rather
// than from a literal a test author invented. That round trip is the point (see
// TestAnApprovalIsPostedAndItsOwnButtonDecidesIt): the button's value carries a private convention
// (approvalActionValue's `<approval id>|<request hash>`) that only these two functions share, so a test
// that typed its own value would prove the halves agree with the test instead of with each other.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/slack-bot/internal/relay"
	palai "github.com/palgroup/palai/sdks/go"
)

// ---------------------------------------------------------------------------------------------------
// the fakes
// ---------------------------------------------------------------------------------------------------

// fakePalai stands in for the SDK's Sessions/Responses groups. It records what a turn asked for, and
// hands back a fixed event sequence so the relay goroutine finishes on its own.
type fakePalai struct {
	mu        sync.Mutex
	sessions  int
	responses []palai.ResponseCreateRequest
	steers    []string
	// events is what SessionEvents replays, once, for the first stream opened.
	events []palai.Event
}

func (f *fakePalai) CreateSession(context.Context, palai.CreateSessionParams) (*palai.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions++
	return &palai.Session{ID: fmt.Sprintf("sess_%d", f.sessions)}, nil
}

func (f *fakePalai) Steer(_ context.Context, sessionID string, p palai.SteerParams) (*palai.Command, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steers = append(f.steers, sessionID+":"+p.Message)
	return &palai.Command{}, nil
}

func (f *fakePalai) CreateResponse(_ context.Context, req palai.ResponseCreateRequest) (*palai.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses = append(f.responses, req)
	return &palai.Response{ID: "resp_1"}, nil
}

func (f *fakePalai) SessionEvents(context.Context, string, palai.EventsParams) (relay.EventStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &fakeStream{events: append([]palai.Event(nil), f.events...)}, nil
}

func (f *fakePalai) createdResponses() []palai.ResponseCreateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]palai.ResponseCreateRequest(nil), f.responses...)
}

type fakeStream struct {
	events []palai.Event
	i      int
}

func (s *fakeStream) Next() (palai.Event, error) {
	if s.i >= len(s.events) {
		return palai.Event{}, io.EOF
	}
	e := s.events[s.i]
	s.i++
	return e, nil
}

func (s *fakeStream) Close() error { return nil }

// fakeStore is relay.ThreadStore over a map. The REAL store is what main.go passes; this one exists so
// these assertions need no Postgres.
type fakeStore struct {
	mu   sync.Mutex
	rows map[string]string
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[string]string{}} }

func (s *fakeStore) key(botID, teamID, channelID, threadTS string) string {
	return strings.Join([]string{botID, teamID, channelID, threadTS}, "|")
}

func (s *fakeStore) SessionForThread(_ context.Context, botID, teamID, channelID, threadTS string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.rows[s.key(botID, teamID, channelID, threadTS)]
	return v, ok, nil
}

func (s *fakeStore) BindThread(_ context.Context, botID, teamID, channelID, threadTS, sessionID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.key(botID, teamID, channelID, threadTS)
	if v, ok := s.rows[k]; ok {
		return v, nil
	}
	s.rows[k] = sessionID
	return sessionID, nil
}

func (s *fakeStore) RebindThread(_ context.Context, botID, teamID, channelID, threadTS, oldSessionID, newSessionID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.key(botID, teamID, channelID, threadTS)
	if s.rows[k] != oldSessionID {
		return false, nil
	}
	s.rows[k] = newSessionID
	return true, nil
}

// fakeStreamSlack is relay.Slack, and it records the RECIPIENT it was built for — the fields
// relay.NewChannelSlackStreamer closes over, which this task fills from a real event for the first time.
type fakeStreamSlack struct {
	mu                               sync.Mutex
	recipientUserID, recipientTeamID string
	appended                         []string
	tasks                            []slack.Task
	stopped                          int
	// failStop makes the closing chat.stopStream refuse. It is the cheapest way to make relay.Run return
	// a non-nil error from inside the background goroutine — the value this bot once discarded.
	failStop bool
}

func (f *fakeStreamSlack) StartStream(context.Context, string, string, string) (string, error) {
	return "ts_1", nil
}

func (f *fakeStreamSlack) AppendStream(_ context.Context, _, _, markdownText string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appended = append(f.appended, markdownText)
	return nil
}

func (f *fakeStreamSlack) StopStream(context.Context, string, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped++
	if f.failStop {
		return errors.New("slack: chat post failed: invalid_arguments")
	}
	return nil
}

// UpdateTask records the task cards the relay drew. They are kept SEPARATE from appended: a card is a
// task_update chunk, not body text, and folding the two together here would let a test claiming "the answer
// contains no status prose" pass while prose was still being written.
func (f *fakeStreamSlack) UpdateTask(_ context.Context, _, _ string, task slack.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks = append(f.tasks, task)
	return nil
}

// fakeApprovalsPalai is relay.ApprovalsPalai: one open approval to be found by request hash, and a record
// of what was decided.
type fakeApprovalsPalai struct {
	mu       sync.Mutex
	open     []palai.Approval
	approved []string
	denied   []string
	hashes   []string
}

func (f *fakeApprovalsPalai) ListApprovals(context.Context, palai.ListApprovalsParams) (*palai.Page[palai.Approval], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &palai.Page[palai.Approval]{Data: append([]palai.Approval(nil), f.open...)}, nil
}

func (f *fakeApprovalsPalai) ApproveApproval(_ context.Context, id string, p palai.DecisionParams) (*palai.ApprovalDecisionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approved = append(f.approved, id)
	f.hashes = append(f.hashes, p.RequestHash)
	return &palai.ApprovalDecisionResult{Decision: "approved"}, nil
}

func (f *fakeApprovalsPalai) DenyApproval(_ context.Context, id string, p palai.DecisionParams) (*palai.ApprovalDecisionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denied = append(f.denied, id)
	f.hashes = append(f.hashes, p.RequestHash)
	return &palai.ApprovalDecisionResult{Decision: "denied"}, nil
}

// fakeApprovalSlack is relay.ApprovalSlack: it keeps the message bodies so a test can click the very
// button that was posted.
type fakeApprovalSlack struct {
	mu      sync.Mutex
	posted  [][]byte
	updated [][]byte
}

func (f *fakeApprovalSlack) PostMessage(_ context.Context, body []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posted = append(f.posted, body)
	return "ts_approval", nil
}

func (f *fakeApprovalSlack) UpdateMessage(_ context.Context, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated = append(f.updated, body)
	return nil
}

// ---------------------------------------------------------------------------------------------------
// the wiring under test
// ---------------------------------------------------------------------------------------------------

// testBot builds the dispatcher the way main.go does — same constructors, same hook — over the fakes. The
// only difference from production is RunInBackground: it runs synchronously, so every assertion after
// OnEventsAPI returns already reflects the run's whole stream.
func testBot(t *testing.T, approvers []string, streamEvents []palai.Event) (*dispatcher, *fakePalai, *fakeStreamSlack, *fakeApprovalsPalai, *fakeApprovalSlack) {
	t.Helper()
	fp := &fakePalai{events: streamEvents}
	fs := newFakeStore()
	streamSlack := &fakeStreamSlack{}
	ap := &fakeApprovalsPalai{}
	as := &fakeApprovalSlack{}

	d := &dispatcher{
		approvals: relay.ApprovalDeps{Palai: ap, Slack: as, AllowedApprovers: approvers},
		botUserID: "U_BOT",
		logf:      func(string, ...any) {},
	}
	d.inbound = relay.NewInboundDeps(
		fs, fp,
		func(recipientUserID, recipientTeamID string) relay.Slack {
			streamSlack.mu.Lock()
			streamSlack.recipientUserID, streamSlack.recipientTeamID = recipientUserID, recipientTeamID
			streamSlack.mu.Unlock()
			return streamSlack
		},
		func(f func()) { f() },
		d.onApprovalRequested,
		d.onRunFailed,
		"bot_1", "U_BOT", "rev_1", "rbd_1",
	)
	return d, fp, streamSlack, ap, as
}

// mentionEnvelope is a documented events_api payload: the app_mention Slack delivers for a top-level
// mention (https://docs.slack.dev/reference/events/app_mention).
func mentionEnvelope(text string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"event_callback","team_id":"T1","event_id":"Ev1",
		"event":{"type":"app_mention","user":"U_HUMAN","text":%q,"ts":"100.001","channel":"C1","channel_type":"channel"}}`, text))
}

// A Slack message becomes a Palai turn: the event is mapped, a session is opened and bound, a response is
// created against the bot's own agent, and the run's events are streamed into the thread. This is the
// path that had no caller at all before this task.
func TestAnInboundMessageBecomesATurn(t *testing.T) {
	events := []palai.Event{
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "on it"}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	d, fp, streamSlack, _, _ := testBot(t, nil, events)

	d.OnEventsAPI(context.Background(), mentionEnvelope("<@U_BOT> build the app"))

	reqs := fp.createdResponses()
	if len(reqs) != 1 {
		t.Fatalf("Responses.Create was called %d time(s), want 1", len(reqs))
	}
	// The mention's own <@U_BOT> is stripped by MapEvent — the model should read the instruction, not the
	// addressing.
	if got := reqs[0].Input; got != "build the app" {
		t.Fatalf("the run's input is %q, want the message with its own mention stripped", got)
	}
	if reqs[0].AgentRevisionID == nil || *reqs[0].AgentRevisionID != "rev_1" {
		t.Fatalf("the run named agent revision %v, want the bot row's own rev_1", reqs[0].AgentRevisionID)
	}
	// StartStream's recipient fields (S9) are filled from the EVENT for the first time here: stream.go
	// refuses before ever calling Slack when either is empty, so an invented value is not an option and a
	// missing one is a stream that never opens.
	streamSlack.mu.Lock()
	defer streamSlack.mu.Unlock()
	if streamSlack.recipientUserID != "U_HUMAN" || streamSlack.recipientTeamID != "T1" {
		t.Fatalf("the stream was opened for recipient %q/%q, want the event's own U_HUMAN/T1",
			streamSlack.recipientUserID, streamSlack.recipientTeamID)
	}
	if strings.Join(streamSlack.appended, "") != "on it" {
		t.Fatalf("the thread received %v, want the run's own text", streamSlack.appended)
	}
	if streamSlack.stopped != 1 {
		t.Fatalf("the Slack stream was closed %d time(s), want exactly 1 — an unclosed stream renders as permanently streaming", streamSlack.stopped)
	}
}

// A RELAY THAT FAILS AFTER THE TURN WAS ACCEPTED IS THE ONE FAILURE THIS PROCESS CANNOT RETURN, so it has
// to be the one it says loudest. OnEventsAPI already returned nil to the socket loop by the time the relay
// goroutine dies; the envelope was acknowledged; the run may have completed on the control plane with its
// answer journalled. Everything a monitor could look at is green and the thread is stuck.
//
// This measures the log line and not just the hook because the hook alone can be wired to a function that
// writes nothing — which is precisely what `_ = Run(...)` was.
func TestARelayThatFailsAfterTheTurnIsAcceptedIsLogged(t *testing.T) {
	events := []palai.Event{
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "the answer"}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	d, fp, streamSlack, _, _ := testBot(t, nil, events)
	streamSlack.failStop = true

	var logged []string
	d.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	d.OnEventsAPI(context.Background(), mentionEnvelope("<@U_BOT> how many Swift files are there?"))

	// The turn itself succeeded — that is the whole trap. Nothing about the Palai side of this episode
	// looks wrong.
	if n := len(fp.createdResponses()); n != 1 {
		t.Fatalf("Responses.Create was called %d time(s), want 1", n)
	}
	if len(logged) != 1 {
		t.Fatalf("the process logged %d line(s) for a relay that failed, want 1: %q", len(logged), logged)
	}
	line := logged[0]
	sessionID := fp.createdResponses()[0].SessionID
	if sessionID == nil {
		t.Fatal("the run was created with no session id")
	}
	// Each of these is something the operator needs and cannot recover from the others: the session to
	// read the run back, the channel and thread to find the message that stopped, and the error to know
	// what to do about it.
	for _, want := range []string{*sessionID, "channel=C1", "thread=100.001", "invalid_arguments"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the log line does not carry %q, so an operator cannot act on it: %s", want, line)
		}
	}
}

// The loop guard: this app's own post carries a bot_id, and it must never become a turn — otherwise the
// bot answers itself, forever.
func TestTheBotsOwnMessageIsNotATurn(t *testing.T) {
	d, fp, _, _, _ := testBot(t, nil, nil)

	d.OnEventsAPI(context.Background(), json.RawMessage(`{"type":"event_callback","team_id":"T1","event_id":"Ev2",
		"event":{"type":"message","bot_id":"B_SELF","user":"U_BOT","text":"my own reply","ts":"100.002","channel":"C1"}}`))

	if n := len(fp.createdResponses()); n != 0 {
		t.Fatalf("the bot's own message opened %d run(s); the loop guard is the difference between a bot and a loop", n)
	}
}

// THE ROUND TRIP, and it is the assertion this whole file exists for: a parked run posts an approval
// message, and THAT message's own button — its value built by one half of the bridge — is what decides
// it. The button carries a private `<approval id>|<request hash>` convention shared by exactly two
// functions; a test that wrote its own value would never see them disagree.
func TestAnApprovalIsPostedAndItsOwnButtonDecidesIt(t *testing.T) {
	events := []palai.Event{
		{Type: relay.ApprovalRequestedEventType, SessionID: "sess_1",
			Data: map[string]any{"request_hash": "rh_live", "tool_name": "xcodebuild"}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	d, _, _, ap, as := testBot(t, []string{"U_HUMAN"}, events)
	ap.open = []palai.Approval{{
		ID: "apr_1", RequestHash: "rh_live", Identity: "xcodebuild",
		OperatorLabel: "build the iOS app", Arguments: `{"scheme":"App"}`,
	}}

	d.OnEventsAPI(context.Background(), mentionEnvelope("<@U_BOT> ship it"))

	as.mu.Lock()
	posted := append([][]byte(nil), as.posted...)
	as.mu.Unlock()
	if len(posted) != 1 {
		t.Fatalf("%d approval message(s) were posted, want 1 — a parked run nobody is asked about is a hang", len(posted))
	}

	value := approveButtonValue(t, posted[0])
	d.OnInteractive(context.Background(), blockActionsPayload("U_HUMAN", slack.ActionApprove, value))

	ap.mu.Lock()
	defer ap.mu.Unlock()
	if len(ap.approved) != 1 || ap.approved[0] != "apr_1" {
		t.Fatalf("approved %v, want the approval the posted button named", ap.approved)
	}
	// The request hash is a MANDATORY body field the control plane refuses empty ("an approval id alone
	// authorizes nothing"), and it has to survive the trip out to Slack and back inside one button value.
	if len(ap.hashes) != 1 || ap.hashes[0] != "rh_live" {
		t.Fatalf("decided with request hash %v, want rh_live carried back out of the button", ap.hashes)
	}
}

// An unlisted clicker is refused BEFORE the control plane is called. That ordering is the property, not
// just the outcome: AllowedApprovers is the only per-human boundary this path has, because the control
// plane sees one principal — this bot's key — for every click it forwards.
func TestAnUnlistedClickerDecidesNothing(t *testing.T) {
	d, _, _, ap, _ := testBot(t, []string{"U_ALLOWED"}, nil)

	d.OnInteractive(context.Background(), blockActionsPayload("U_STRANGER", slack.ActionApprove, "apr_1|rh_1"))

	ap.mu.Lock()
	defer ap.mu.Unlock()
	if len(ap.approved) != 0 || len(ap.denied) != 0 {
		t.Fatalf("an unlisted click reached the control plane: approved=%v denied=%v", ap.approved, ap.denied)
	}
}

// The third button ToolApprovalMessage mints — "Show arguments" — opens a modal this bot does not wire.
// It must decide nothing rather than fall through into a decision branch.
func TestTheShowArgumentsButtonDecidesNothing(t *testing.T) {
	d, _, _, ap, _ := testBot(t, []string{"U_HUMAN"}, nil)

	d.OnInteractive(context.Background(), blockActionsPayload("U_HUMAN", slack.ActionShowArguments, "apr_1|rh_1"))

	ap.mu.Lock()
	defer ap.mu.Unlock()
	if len(ap.approved) != 0 || len(ap.denied) != 0 {
		t.Fatalf("the show-arguments button decided something: approved=%v denied=%v", ap.approved, ap.denied)
	}
}

// approveButtonValue digs the Approve button's value out of a chat.postMessage body this process built.
func approveButtonValue(t *testing.T, body []byte) string {
	t.Helper()
	var msg struct {
		Blocks []struct {
			Type     string `json:"type"`
			Elements []struct {
				ActionID string `json:"action_id"`
				Value    string `json:"value"`
			} `json:"elements"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("the posted approval message is not decodable: %v", err)
	}
	for _, b := range msg.Blocks {
		for _, e := range b.Elements {
			if e.ActionID == slack.ActionApprove {
				return e.Value
			}
		}
	}
	t.Fatalf("the posted approval message carries no %s button: %s", slack.ActionApprove, body)
	return ""
}

// blockActionsPayload is a documented block_actions interaction payload
// (https://docs.slack.dev/reference/interaction-payloads/block_actions-payload).
func blockActionsPayload(userID, actionID, value string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"block_actions",
		"team":{"id":"T1"},"user":{"id":%q,"team_id":"T1"},
		"channel":{"id":"C1"},"message":{"ts":"ts_approval","thread_ts":"100.001"},
		"actions":[{"action_id":%q,"value":%q}]}`, userID, actionID, value))
}

// ---------------------------------------------------------------------------------------------------
// the guard
// ---------------------------------------------------------------------------------------------------

// THE ARCHITECTURE'S ONE RULE, enforced rather than promised: a Slack credential never comes from this
// process's environment. It is the reason the bot registry exists — "an operator configures a bot in the
// admin panel, not in a file" — and it outlived the pressure that used to threaten it. When this process
// could not redeem its own handles at all, reading SLACK_BOT_TOKEN was one line away from a working bot;
// GET /v1/bots/{bot_id}/credentials closed that gap (see credentials.go), so the shortcut now buys nothing.
// It is still one line, which is why the rule is enforced here rather than left to the reason for it.
//
// THE SCAN IS OVER THE SHIPPED SOURCE ONLY, and both halves of that are deliberate. It walks the whole
// binary's tree rather than this one file, because the line that breaks the rule is as likely to appear
// in internal/relay as here. And it skips _test.go, because a credential-gated LIVE test legitimately
// reads SLACK_APP_TOKEN from the environment of the machine running it — what may not happen is the
// PROCESS an operator deploys doing so. internal/config is the one production exception: it is the
// declared reader of the four variables, and it has its own test naming each one.
func TestNoSlackCredentialComesFromTheEnvironment(t *testing.T) {
	var offenders []string
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		slashed := filepath.ToSlash(path)
		if entry.IsDir() {
			if slashed == "internal/config" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "os.Getenv") || strings.Contains(line, "os.LookupEnv") {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", slashed, i+1, strings.TrimSpace(line)))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the bot's source: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("this process reads its environment outside internal/config:\n  %s\n"+
			"A Slack token is never this process's environment variable — it comes from the bot's registry row, "+
			"redeemed over GET /v1/bots/{bot_id}/credentials (credentials.go). If that redemption is what failed, "+
			"the fix is the row's handles or this key's bots.credentials capability, not this line.",
			strings.Join(offenders, "\n  "))
	}
}

// credentialServer stands in for the control plane's GET /v1/bots/{bot_id}/credentials, answering the
// body the route renders. It is an httptest server rather than a stubbed method because the assertions
// below are about what this process asks for and what it does with the answer — including the PATH, which
// is the one part a fake method would let drift silently.
func credentialServer(t *testing.T, body string, status int) (*palai.Client, *string) {
	t.Helper()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	client, err := palai.New(palai.WithBaseURL(srv.URL), palai.WithAPIKey("sk-test"))
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return client, &gotPath
}

// The happy path, which is the whole of Task 14.5 on this side: the handles the row names come back as
// token BYTES, read out of the map by the CONFIG KEY rather than by the secret's name.
func TestCredentialRedemptionReadsTheValuesByConfigKey(t *testing.T) {
	client, gotPath := credentialServer(t, `{"bot_id":"bot_1","object":"bot_credentials",`+
		`"credentials":{"app_token_ref":"xapp-live","bot_token_ref":"xoxb-live","signing_secret_ref":"sig"},`+
		`"unresolved":[]}`, 200)

	creds, err := redeemSlackCredentials(context.Background(), client, "bot_1", slackConfig{
		AppTokenRef: "slack-app-T1", BotTokenRef: "slack-bot-T1",
	})
	if err != nil {
		t.Fatalf("redeemSlackCredentials: %v", err)
	}
	if *gotPath != "/v1/bots/bot_1/credentials" {
		t.Fatalf("path = %q, want /v1/bots/bot_1/credentials", *gotPath)
	}
	if string(creds.appToken) != "xapp-live" || string(creds.botToken) != "xoxb-live" {
		t.Fatalf("creds = %q/%q, want the values keyed by app_token_ref/bot_token_ref", creds.appToken, creds.botToken)
	}
}

// A handle the control plane could not resolve is a REFUSAL, not an empty token. This is the assertion
// that would fail if the caller stopped reading `unresolved`: the map lookup alone yields "" and a bot
// built on it dials Slack and then cannot say a word, which looks like it is working.
func TestAnUnresolvedHandleRefusesAndNamesIt(t *testing.T) {
	client, _ := credentialServer(t, `{"bot_id":"bot_1","object":"bot_credentials",`+
		`"credentials":{"app_token_ref":"xapp-live"},"unresolved":["bot_token_ref"]}`, 200)

	_, err := redeemSlackCredentials(context.Background(), client, "bot_1", slackConfig{
		AppTokenRef: "slack-app-T1", BotTokenRef: "slack-bot-T1",
	})
	if err == nil {
		t.Fatal("redeemSlackCredentials succeeded with bot_token_ref unresolved")
	}
	for _, want := range []string{"bot_token_ref", "slack-bot-T1", "seal it"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not mention %q: %v", want, err)
		}
	}
	// The refusal names the HANDLE and never a redeemed value, including the one that DID resolve.
	if strings.Contains(err.Error(), "xapp-live") {
		t.Fatalf("the refusal echoed a redeemed token: %v", err)
	}
}

// THE SECOND GUARD, and it needs its own test because the first one hides it. A control plane that
// reports a key as RESOLVED and hands back an empty string is a different failure from one that reports
// it unresolved — `unresolved` is empty here — and only the value check catches it. Perturbing that check
// while driving TestAnUnresolvedHandleRefusesAndNamesIt leaves that test green, because the handle is
// listed there and the first arm answers first.
func TestAnEmptyRedeemedTokenIsRefused(t *testing.T) {
	client, _ := credentialServer(t, `{"bot_id":"bot_1","object":"bot_credentials",`+
		`"credentials":{"app_token_ref":"xapp-live","bot_token_ref":""},"unresolved":[]}`, 200)

	_, err := redeemSlackCredentials(context.Background(), client, "bot_1", slackConfig{
		AppTokenRef: "slack-app-T1", BotTokenRef: "slack-bot-T1",
	})
	if err == nil {
		t.Fatal("redeemSlackCredentials accepted an empty bot token")
	}
	if !strings.Contains(err.Error(), "bot_token_ref") {
		t.Fatalf("the refusal does not name the empty handle: %v", err)
	}
}

// A control plane that does not serve the route — an older one, or one wired with no master key — must
// refuse in a way an operator can act on rather than fail somewhere downstream.
func TestAMissingCredentialsRouteRefusesAndSaysWhy(t *testing.T) {
	client, _ := credentialServer(t, `{"type":"not_found","title":"not_found"}`, 404)

	_, err := redeemSlackCredentials(context.Background(), client, "bot_1", slackConfig{
		AppTokenRef: "slack-app-T1", BotTokenRef: "slack-bot-T1",
	})
	if err == nil {
		t.Fatal("redeemSlackCredentials succeeded against a control plane with no such route")
	}
	for _, want := range []string{"/v1/bots/bot_1/credentials", "bots.credentials"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// A row with no credential handles is refused before any durable resource is touched, and the refusal
// names BOTH missing handles rather than only the first — an operator fixing one at a time round-trips
// through a deployment for each.
func TestAConfigWithNoHandlesIsRefused(t *testing.T) {
	_, err := parseSlackConfig(json.RawMessage(`{"team_id":"T1"}`))
	if err == nil {
		t.Fatal("parseSlackConfig accepted a config with no token handles")
	}
	for _, want := range []string{"app_token_ref", "bot_token_ref"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q: %v", want, err)
		}
	}
	if _, err := parseSlackConfig(nil); err == nil {
		t.Fatal("parseSlackConfig accepted an absent config document")
	}
}

// A config the CONSOLE would write parses, and an unknown key does not take the bot down: the console may
// add a field before this process learns to read it, which is an ordinary deployment order.
func TestTheConsolesOwnConfigShapeParses(t *testing.T) {
	cfg, err := parseSlackConfig(json.RawMessage(`{
		"team_id":"T0AMPM5JX8U","bot_user_id":"U_BOT","allowed_approvers":["U1","U2"],
		"signing_secret_ref":"slack-signing-T0AMPM5JX8U","bot_token_ref":"slack-bot-T0AMPM5JX8U",
		"app_token_ref":"slack-app-T0AMPM5JX8U","some_future_key":{"a":1}}`))
	if err != nil {
		t.Fatalf("the shape apps/web-console/lib/channels.ts writes was refused: %v", err)
	}
	if cfg.TeamID != "T0AMPM5JX8U" || cfg.BotUserID != "U_BOT" || len(cfg.AllowedApprovers) != 2 {
		t.Fatalf("parsed %+v, want every declared key read", cfg)
	}
	if cfg.AppTokenRef != "slack-app-T0AMPM5JX8U" || cfg.BotTokenRef != "slack-bot-T0AMPM5JX8U" {
		t.Fatalf("parsed %+v, want both credential handles", cfg)
	}
	// An empty approver list denies every click, which is safe and silent — startup has to say it.
	empty := slackConfig{AppTokenRef: "a", BotTokenRef: "b"}
	if !strings.Contains(empty.describe(), "NO approvers") {
		t.Fatalf("the startup line does not warn that no approver is configured: %s", empty.describe())
	}
}
