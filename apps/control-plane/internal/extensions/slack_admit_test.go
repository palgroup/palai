package extensions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/slack"
)

// The run input IS the prompt (apps/control-plane/internal/execution/model_dispatch.go asJSONString hands a
// non-string straight to the provider as compact JSON), so anything but the human's own words shows up in
// the conversation. This is the regression that shipped: a real mention answered "It looks like you have
// shared a JSON object that represents a message event from Slack…".
func TestSlackRunInputIsTheHumanMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   slack.Event
		want string
	}{
		{"a plain message", slack.Event{Kind: slack.KindMessage, Text: "merhaba"}, "merhaba"},
		{"an edit is marked, not replayed as new", slack.Event{Kind: slack.KindCorrection, Text: "fixed typo"}, "(edited) fixed typo"},
		{"a file share with a comment", slack.Event{Kind: slack.KindFileShare, Text: "review this"}, "review this"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := slackRunInput(tc.ev); got != tc.want {
				t.Fatalf("slackRunInput = %q, want %q", got, tc.want)
			}
		})
	}
}

// The input must be a JSON STRING once marshalled — not an object. An object is what the engine forwards
// verbatim and the model then describes back to the user instead of answering it.
func TestSlackRunInputMarshalsToAJSONString(t *testing.T) {
	raw, err := json.Marshal(slackRunInput(slack.Event{Kind: slack.KindMessage, Text: "merhaba"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `"merhaba"` {
		t.Fatalf("stored input = %s, want the bare JSON string \"merhaba\"", raw)
	}
}

// SCOPE IS NOT CONVERSATION. The channel, the workspace, the Slack user and the connection are enforced
// server-side (the allow-lists, the resolved tenant, the connection's principal) and must not travel in the
// prompt: a model that reads "principal_id" or "channel_id" as part of the user's turn has been handed a
// string that LOOKS like authority, and a user who types those words gets the same string.
func TestSlackRunInputCarriesNoScopeOrEnvelope(t *testing.T) {
	ev := slack.Event{
		Kind: slack.KindMessage, Text: "merhaba",
		TeamID: "T1", ChannelID: "C1", UserID: "U9", SourceEventID: "Ev1",
		Data: json.RawMessage(`{"type":"app_mention","user":"U9","text":"<@Ubot> merhaba"}`),
	}
	got := slackRunInput(ev)
	for _, leaked := range []string{"T1", "C1", "U9", "Ev1", "app_mention", "principal", "connection", "{"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("run input %q carries %q — scope and the raw envelope must never reach the prompt", got, leaked)
		}
	}
}

// A deletion RETRACTS. Echoing the removed words back as if the user had just said them is the opposite of
// what SLK-005 classifies a tombstone as, so the deleted text must not be in the input at all.
func TestSlackRunInputDoesNotReplayDeletedText(t *testing.T) {
	got := slackRunInput(slack.Event{Kind: slack.KindTombstone, Text: "delete the production database"})
	if strings.Contains(got, "production") {
		t.Fatalf("tombstone input = %q, want no trace of the retracted message", got)
	}
	if got == "" {
		t.Fatal("tombstone input is empty; a run with an empty prompt spends a model call on nothing")
	}
}

// A textless event still births a run today (admission is unchanged), so it must not hand the model an
// empty prompt.
func TestSlackRunInputNeverEmpty(t *testing.T) {
	for _, kind := range []slack.Kind{slack.KindMessage, slack.KindCorrection, slack.KindTombstone, slack.KindFileShare, slack.KindOther} {
		if got := slackRunInput(slack.Event{Kind: kind}); got == "" {
			t.Fatalf("kind %q with no text produced an empty input", kind)
		}
	}
}
