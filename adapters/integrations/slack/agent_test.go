package slack

import (
	"context"
	"testing"
)

// setStatus is the FIRST visible win of the agent surface and it needs NO new scope (S3: chat:write, the
// scope the app already holds), so it runs on today's channel flow before any panel exists. The wire shape
// differs from chat.* on purpose and the difference is easy to get silently wrong: the argument is
// `channel_id`, not `channel`.
func TestSetStatusCarriesTheDocumentedArguments(t *testing.T) {
	peer := &recordingPeer{reply: `{"ok":true}`}
	if err := SetStatus(context.Background(), peer, "https://slack.test/api", []byte("xoxb-token"),
		"C1", "1700000000.000100", "is thinking…", []string{"reading the thread…", "asking the model…"}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if peer.urls[0] != "https://slack.test/api/assistant.threads.setStatus" {
		t.Fatalf("posted to %q, want assistant.threads.setStatus", peer.urls[0])
	}
	got := peer.decode(t, 0)
	if got["channel_id"] != "C1" || got["thread_ts"] != "1700000000.000100" || got["status"] != "is thinking…" {
		t.Fatalf("setStatus body = %q, want channel_id/thread_ts/status", peer.bodies[0])
	}
	if _, wrong := got["channel"]; wrong {
		t.Fatalf("setStatus sent `channel`; the documented argument is `channel_id`: %q", peer.bodies[0])
	}
	if loading, _ := got["loading_messages"].([]any); len(loading) != 2 {
		t.Fatalf("loading_messages = %v, want the two rotating messages", got["loading_messages"])
	}
}

// S15: "Maximum of 10 messages." Over the cap the call is TRIMMED rather than refused — a status indicator is
// decoration, and losing it because someone wrote eleven loading strings would be the wrong failure.
func TestSetStatusCapsLoadingMessagesAtTen(t *testing.T) {
	peer := &recordingPeer{reply: `{"ok":true}`}
	many := make([]string, 25)
	for i := range many {
		many[i] = "working…"
	}
	if err := SetStatus(context.Background(), peer, "https://slack.test/api", []byte("x"), "C1", "1.1", "is thinking…", many); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	loading, _ := peer.decode(t, 0)["loading_messages"].([]any)
	if len(loading) != MaxLoadingMessages {
		t.Fatalf("sent %d loading messages, want them capped at %d", len(loading), MaxLoadingMessages)
	}
}

// Clearing the status is the same call with an empty status, and it must still be made: a panel left saying
// "is thinking…" after the answer landed is a lie the next reader has no way to check.
func TestSetStatusClearsWithAnEmptyStatus(t *testing.T) {
	peer := &recordingPeer{reply: `{"ok":true}`}
	if err := SetStatus(context.Background(), peer, "https://slack.test/api", []byte("x"), "C1", "1.1", "", nil); err != nil {
		t.Fatalf("SetStatus(clear): %v", err)
	}
	got := peer.decode(t, 0)
	if status, ok := got["status"]; !ok || status != "" {
		t.Fatalf("clearing body = %q, want an explicit empty status", peer.bodies[0])
	}
	if _, ok := got["loading_messages"]; ok {
		t.Fatalf("clearing sent loading_messages: %q", peer.bodies[0])
	}
}
