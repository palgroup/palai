package slack

import (
	"context"
	"encoding/json"
)

// The AGENT PANEL wire (E20 T1/T2, spec §36) — today just the one call that costs nothing to have.
//
// WHY THIS IS THE FIRST THING TO SHIP: assistant.threads.setStatus needs `chat:write`, NOT `assistant:write`
// (S3). The guidance everyone repeats — "the agent methods need assistant:write" — is an INCOMPLETE truth: it
// holds for setSuggestedPrompts and setTitle, and not for status or for any of the streaming calls. So this
// runs on the app's EXISTING scopes, in today's channel threads, before `agent_view` is enabled and before
// anyone reinstalls anything.

// MaxLoadingMessages caps the rotating strings a status can carry.
//
// CONTRACT: https://docs.slack.dev/reference/methods/assistant.threads.setStatus/ (checked 2026-07-27) —
// required `channel_id`, `thread_ts`, `status`; optional `loading_messages` with a "Maximum of 10 messages";
// scope `chat:write`; 600 requests per minute per app per team; success is {"ok":true}.
//
// 600/min is far above anything else this integration does, which is why status updates are NOT run through
// the ChannelPacer: pacing them would hold them against a budget they are nowhere near.
const MaxLoadingMessages = 10

// SetStatus sets (or, with an empty status, clears) the thread's working indicator.
//
// The argument is `channel_id`, NOT `channel`. The chat.* family uses `channel` and the assistant.threads.*
// family uses `channel_id`; sending the wrong one is an invalid_arguments a reader would spend a while
// staring at.
//
// Over-long loading_messages are TRIMMED rather than refused: a status indicator is decoration, and dropping
// it because someone wrote eleven strings would be the wrong failure.
func SetStatus(ctx context.Context, doer Doer, apiBase string, token []byte, channelID, threadTS, status string, loadingMessages []string) error {
	payload := map[string]any{
		"channel_id": channelID,
		"thread_ts":  threadTS,
		"status":     status,
	}
	if len(loadingMessages) > MaxLoadingMessages {
		loadingMessages = loadingMessages[:MaxLoadingMessages]
	}
	if len(loadingMessages) > 0 {
		payload["loading_messages"] = loadingMessages
	}
	body, _ := json.Marshal(payload)
	_, err := PostMessage(ctx, doer, PostRequest{
		MethodURL: apiBase + "/assistant.threads.setStatus", Token: token, Body: body,
	}, PostOptions{})
	return err
}
