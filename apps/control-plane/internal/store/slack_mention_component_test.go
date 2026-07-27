//go:build component

package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// THE MENTION, WHERE IT SHIPS (E21 T3). The renderer's own tests prove the mint rule; this one proves the
// thing a renderer test cannot: that the identity SURVIVES THE JOURNEY. slack.Event.UserID exists for the
// length of one Admit call, and the worker that posts the answer runs later, from a database row, after a
// restart. Everything here is real — the shipped route, the real admission, the real terminal transaction
// through the coordinator, the real reply pump — and the only fake is Slack itself.
//
// TestSlack* is what puts this in scripts/test/component's -run allow-list; a name outside it is a test this
// tier never runs.
func TestSlackTerminalRunMentionsTheRequesterAndOnlyTheRequester(t *testing.T) {
	f := newSlackFixture(t)
	f.withStreaming(t, 4)
	ctx := context.Background()

	const asker = "U0ASKER"
	f.deliver(t, f.eventText(t, "EvM1", "app_mention", asker, "C91", "1700000091.000100", "",
		"<@"+f.botUser+"> publish it"), time.Now(), "", "").Body.Close()
	runID, responseID, sessionID := f.runAndResponse(t)
	f.terminate(t, runID, statemachines.RunCmdProvision, statemachines.RunCmdStart)
	f.commitStep(t, sessionID, responseID, runID)

	// The stream has to be open for the answer to close it with BLOCKS — that is the path the mention rides.
	f.upsertTask(t, sessionID, responseID, runID, "t1", "Publish the branch", "in_progress")
	f.awaitCalls(t, "/chat.startStream", 1)

	// The model asks its question the ONE way it can — a typed intent — and, in the same answer, tries the two
	// things it must never be able to do: name a different person, and write a live token itself.
	const answer = `[{"type":"mention","who":"requester","text":"which branch should I publish?"},` +
		`{"type":"mention","who":"U0SOMEONEELSE","text":"or ask them"},` +
		`{"type":"text","text":"cc <@U0SOMEONEELSE> and <!channel>"}]`
	f.finalizeWith(t, responseID, "completed", map[string]any{
		"output": []any{map[string]any{"type": "message", "content": answer}},
	})
	f.terminate(t, runID, statemachines.RunCmdComplete)

	// FROZEN AT ENQUEUE, and read here BEFORE any pump exists: the identity is on the delivery row because
	// the terminal transaction put it there, not because a poster still had the event in memory.
	var frozen string
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT requester_user_id FROM slack_reply_deliveries WHERE run_id=$1`, runID).Scan(&frozen); err != nil {
		t.Fatalf("read the frozen requester for run %s: %v", runID, err)
	}
	if frozen != asker {
		t.Fatalf("the delivery row froze requester %q, want the person who asked (%q)", frozen, asker)
	}

	// A pump built AFTER the run terminated is what a restarted control plane is: it has never seen the
	// event, and everything it knows comes off the row above.
	if posted, err := extensions.NewSlackReplyPump(f.bridge).Tick(ctx); err != nil || posted != 1 {
		t.Fatalf("the pump delivered %d answers (err %v), want 1", posted, err)
	}
	stops := f.callsTo("/chat.stopStream")
	if len(stops) != 1 {
		t.Fatalf("fake Slack saw %d chat.stopStream call(s), want exactly 1", len(stops))
	}
	stop := decodeSlackCall(t, stops[0])

	// The whole message as Slack will read it: the markdown and every string inside the blocks. Decoded, not
	// grepped — encoding/json escapes `<` and a raw substring search over the body could not fail.
	wire := []string{}
	if markdown, _ := stop["markdown_text"].(string); markdown != "" {
		wire = append(wire, markdown)
	}
	blocks, ok := stop["blocks"].([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("chat.stopStream carried no blocks; the mention never reached the wire: %s", stops[0].body)
	}
	wire = append(wire, decodedNodeStrings(blocks)...)
	whole := strings.Join(wire, "\n")

	if got := strings.Count(whole, "<@"); got != 1 {
		t.Fatalf("the closing message carries %d live mention(s), want exactly one: %q", got, whole)
	}
	if !strings.Contains(whole, "<@"+asker+"> which branch should I publish?") {
		t.Fatalf("the one mention is not the requester in front of the model's question: %q", whole)
	}
	if strings.Contains(whole, "<@U0SOMEONEELSE>") || strings.Contains(whole, "<!channel>") {
		t.Fatalf("the model reached a person it chose, or a broadcast: %q", whole)
	}
	// Not silently dropped either — a defused token is still shown to the reader.
	if !strings.Contains(whole, "U0SOMEONEELSE") {
		t.Fatalf("what the model wrote vanished instead of being defused: %q", whole)
	}
	if found := sweepActionableNodes("blocks", blocks); len(found) != 0 {
		t.Fatalf("the mention message carried %d actionable element(s): %v", len(found), found)
	}
}

// decodedNodeStrings collects every string VALUE out of an already-decoded block array. Same reasoning as the
// renderer's decodedStrings: assertions run after the JSON escaping is undone, never over the raw bytes.
func decodedNodeStrings(node any) []string {
	switch v := node.(type) {
	case string:
		return []string{v}
	case map[string]any:
		var out []string
		for _, value := range v {
			out = append(out, decodedNodeStrings(value)...)
		}
		return out
	case []any:
		var out []string
		for _, el := range v {
			out = append(out, decodedNodeStrings(el)...)
		}
		return out
	}
	return nil
}
