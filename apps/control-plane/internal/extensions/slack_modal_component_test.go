//go:build component

package extensions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// E23 T4's modal, against REAL Postgres and a fake Slack peer. Two claims, and neither is checkable in a
// unit test:
//
//	1. the modal is derived from a REAL parked ledger row — every argument the broker will send is in the
//	   view, read back out of the database rather than handed to the renderer by the test; and
//	2. THE MODAL PATH WRITES NOTHING. A trigger_id dies three seconds after Slack sends it and the ack is
//	   owed in the same three seconds, so a write here is a lock here, and a lock here is a dead button.
//	   Asserted by counting every row in every table before and after, not by reading the code.

// recordingDoer captures the outbound Slack call the modal path makes and answers ok.
type recordingDoer struct {
	urls   []string
	bodies [][]byte
}

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	body := []byte{}
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	d.urls = append(d.urls, req.URL.String())
	d.bodies = append(d.bodies, body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true,"view":{"id":"V1"}}`)),
	}, nil
}

// readOnlySpine opens a SECOND connection pool whose transactions Postgres itself refuses to let write,
// and hands the modal path that one.
//
// THE FIRST VERSION OF THIS PROOF COULD NOT FAIL, and the correction is worth recording where the next
// reader meets it. It counted every row of every table before and after; injecting a real write into the
// modal path did not turn it red, because the write was an UPDATE and an UPDATE changes no row count.
// A no-write assertion that only sees INSERTs is a no-write assertion that passes while the thing it
// forbids happens.
//
// default_transaction_read_only makes the DATABASE the judge instead: any INSERT, UPDATE or DELETE on this
// pool raises 25006 and the modal path fails loudly. Nothing has to be enumerated, so nothing can be
// forgotten. The caller self-checks the setting actually took — a read-only pool that is not read-only
// would make every assertion below vacuous.
func readOnlySpine(t *testing.T) (*coordinator.Store, *Store) {
	t.Helper()
	url := poolURL(t) + "&options=" + neturl.QueryEscape("-c default_transaction_read_only=on")
	cs, err := coordinator.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("open the read-only spine: %v", err)
	}
	t.Cleanup(cs.Close)
	return cs, New(cs.Pool(), "file")
}

func TestSlackApprovalModalReadsTheLedgerAndWritesNothing(t *testing.T) {
	cs, store, _, pool := journeySpine(t)
	ctx := context.Background()

	team := testID("T2344")
	const (
		channel  = "C2344"
		threadTS = "1700002344.000100"
		mapped   = "Umapped2344"
		unmapped = "Uunmapped2344"
	)

	project := seedOrgProject(t, store)
	tenant := coordinator.Tenant{Project: project}
	sessionID := seedSession(t, store, project)
	respID, runID := testID("resp"), testID("run")
	mustSystemExec(t, pool, `UPDATE sessions SET state='active' WHERE id=$1`, sessionID)
	mustSystemExec(t, pool, `INSERT INTO responses (id, project_id, session_id, state) VALUES ($1,$2,$3,'in_progress')`,
		respID, project, sessionID)
	mustSystemExec(t, pool, `INSERT INTO runs (id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,'running')`,
		runID, project, sessionID, respID)

	conn, err := store.CreateSlackConnection(ctx, project, []byte(`{
		"team_id":"`+team+`","bot_user_id":"Ubot2344",
		"signing_secret_ref":"slack/modal/signing","bot_token_ref":"slack/modal/bot",
		"scopes":"app_mentions:read chat:write","allowed_users":["`+mapped+`"]}`))
	if err != nil {
		t.Fatalf("install the connection: %v", err)
	}
	// The thread correlation the click will resolve through. Seeded directly — what this test proves is the
	// modal, not SLK-003, which slack_component_test.go already proves against this same schema.
	mustSystemExec(t, pool, `INSERT INTO slack_thread_sessions (id, project_id, connection_id, team_id, channel_id, thread_ts, session_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, testID("slkt"), project, conn.ID, team, channel, threadTS, sessionID)

	// A REAL parked call, through T1's own production entry point. The arguments carry a nested object, a
	// number and a broadcast token, so the assertions below are about a screen a human might actually meet.
	arguments := []byte(`{"issue":"PAL-42","transition":"Done","retries":3,` +
		`"comment":"<!channel> shipping","fields":{"assignee":"U9","labels":["ops","urgent"]}}`)
	const requestHash = "hash2344"
	approvalID, callID := testID("apr"), testID("tc")
	if err := cs.RequestToolApproval(ctx, tenant, coordinator.ToolApprovalRequest{
		ApprovalID: approvalID, ToolCallID: callID, SessionID: sessionID, ResponseID: respID, RunID: runID,
		Fence: 1, ToolName: "jira.transitionIssue", Arguments: arguments, ReplayClass: "irreversible",
		RequestHash: requestHash, ExpiresAt: time.Now().Add(30 * time.Minute), Boundary: testID("mr"),
	}); err != nil {
		t.Fatalf("park the call: %v", err)
	}

	// EVERYTHING ABOVE WAS SET UP ON THE WRITABLE POOL. From here the modal path runs on a pool Postgres
	// will not let write at all.
	roCS, roStore := readOnlySpine(t)
	if _, err := roCS.Pool().Exec(storage.WithSystemScope(ctx),
		`UPDATE slack_thread_sessions SET last_bot_message_ts = 'x' WHERE session_id = $1`, sessionID); err == nil {
		t.Fatal("the read-only pool accepted a write, so the no-write proof below cannot fail and certifies nothing")
	}

	doer := &recordingDoer{}
	// The bot token is a HANDLE resolved at call time, exactly as the decision path resolves it; the
	// resolver is the fixture's, so no credential is anywhere near the outbound body it produces.
	secrets := func(ref string) ([]byte, error) { return []byte("xoxb-fixture-not-a-credential-" + ref), nil }
	admitter := NewSlackAdmitter(roStore, nil, secrets, api.AdmissionLimits{}).
		WithDecisions(roCS, doer, "https://slack.invalid/api")
	connRef := api.SlackConnectionRef{
		ID: conn.ID, Project: project, TeamID: team, BotTokenRef: "slack/modal/bot",
	}
	intent := slack.ShowArgumentsIntent{
		TeamID: team, UserID: mapped, RequestHash: requestHash, TriggerID: "tr.2344",
		ChannelID: channel, ThreadTS: threadTS, MessageTS: threadTS,
	}

	// ---- THE NO-WRITE PROOF: the whole path runs, and it runs where a write is impossible -------------
	rejected, err := admitter.OpenApprovalArguments(ctx, connRef, intent)
	if err != nil {
		t.Fatalf("the modal path failed on a read-only connection — it writes somewhere, and a write inside a three-second trigger is a dead button: %v", err)
	}
	if rejected != "" {
		t.Fatalf("an authorized click was refused: %s", rejected)
	}

	// ---- the view is the ledger's own row ------------------------------------------------------------
	if len(doer.urls) != 1 || !strings.HasSuffix(doer.urls[0], "/views.open") {
		t.Fatalf("outbound calls = %v, want exactly one views.open", doer.urls)
	}
	var open struct {
		TriggerID string `json:"trigger_id"`
		View      struct {
			PrivateMetadata string           `json:"private_metadata"`
			Blocks          []map[string]any `json:"blocks"`
		} `json:"view"`
	}
	if err := json.Unmarshal(doer.bodies[0], &open); err != nil {
		t.Fatalf("decode the views.open body: %v (%s)", err, doer.bodies[0])
	}
	if open.TriggerID != "tr.2344" {
		t.Fatalf("views.open trigger_id = %q, want the click's own", open.TriggerID)
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(open.View.PrivateMetadata), &meta); err != nil {
		t.Fatalf("private_metadata: %v", err)
	}
	if meta["approval_id"] != approvalID || meta["request_hash"] != requestHash || len(meta) != 2 {
		t.Fatalf("private_metadata = %v, want exactly the ledger's binding", meta)
	}

	// Every leaf of the parked arguments, read back out of Postgres and rendered — decoded, never grepped
	// over the marshalled bytes (encoding/json escapes the very characters the defence produces).
	cells := map[string]bool{}
	for _, block := range open.View.Blocks {
		if block["type"] != "table" {
			continue
		}
		rows, _ := block["rows"].([]any)
		for _, row := range rows {
			for _, c := range row.([]any) {
				cell, _ := c.(map[string]any)
				text, _ := cell["text"].(string)
				cells[text] = true
			}
		}
	}
	for _, want := range []string{"issue", "transition", "retries", "comment",
		"fields.assignee", "fields.labels[0]", "fields.labels[1]", `"PAL-42"`, `"Done"`, "3", `"U9"`, `"ops"`, `"urgent"`} {
		if !cells[want] {
			t.Fatalf("the modal is missing %q; a parked argument that is not on the screen is an argument nobody authorized. cells = %v", want, cells)
		}
	}
	for cell := range cells {
		if strings.Contains(cell, "<!channel") {
			t.Fatalf("a live broadcast token survived the round trip through Postgres: %q", cell)
		}
	}

	// ---- and the same click from an unmapped user opens NOTHING --------------------------------------
	doer.urls, doer.bodies = nil, nil
	intent.UserID = unmapped
	rejected, err = admitter.OpenApprovalArguments(ctx, connRef, intent)
	if err != nil {
		t.Fatalf("unmapped click: %v", err)
	}
	if rejected == "" {
		t.Fatal("an unmapped user opened the argument modal; the read path must run the SAME deny-by-default chain the decision does")
	}
	if len(doer.urls) != 0 {
		t.Fatalf("a refused click still called Slack: %v", doer.urls)
	}

	// ---- and a hash that pins nothing open shows nothing ---------------------------------------------
	intent.UserID, intent.RequestHash = mapped, "hash-that-matches-nothing"
	if rejected, err = admitter.OpenApprovalArguments(ctx, connRef, intent); err != nil || rejected == "" {
		t.Fatalf("a stale hash opened a modal (rejected=%q err=%v)", rejected, err)
	}
	if len(doer.urls) != 0 {
		t.Fatalf("a stale hash still called Slack: %v", doer.urls)
	}
}
