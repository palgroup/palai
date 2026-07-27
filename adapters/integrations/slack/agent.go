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
// needs no reinstall and no new permission.
//
// WHAT IS NOT CONFIRMED, and it is the difference between "no new scope" and "works today": the reference
// documents channel_id + thread_ts and says nothing about the thread having to be an AGENT thread — but it
// does not say it works in an ordinary channel thread either, and the method lives in the assistant.threads.*
// namespace. So the scope claim is documented; the "already works in today's channel flow" claim is an
// INFERENCE, and TestLiveSlackStatusNeedsNoNewScope is what settles it against a real workspace. Every caller
// treats a failure here as cosmetic and carries on, so the inference being wrong costs the indicator and
// nothing else.

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
//
// THE TEXT IS DEFUSED (E21 T3, plan §3.6 D6). This call was one of the two outbound paths that never met
// NeutralizeBroadcasts. Every caller feeds it CONSTANTS today, so it was safe BY ARGUMENT — and a status
// line is exactly the field a later task hands a model's own words, at which point the safety would have
// expired silently. Making it safe by CONSTRUCTION costs one call and removes the question.
func SetStatus(ctx context.Context, doer Doer, apiBase string, token []byte, channelID, threadTS, status string, loadingMessages []string) error {
	payload := map[string]any{
		"channel_id": channelID,
		"thread_ts":  threadTS,
		"status":     NeutralizeBroadcasts(status),
	}
	if len(loadingMessages) > MaxLoadingMessages {
		loadingMessages = loadingMessages[:MaxLoadingMessages]
	}
	if len(loadingMessages) > 0 {
		defused := make([]string, 0, len(loadingMessages))
		for _, message := range loadingMessages {
			defused = append(defused, NeutralizeBroadcasts(message))
		}
		payload["loading_messages"] = defused
	}
	body, _ := json.Marshal(payload)
	_, err := PostMessage(ctx, doer, PostRequest{
		MethodURL: apiBase + "/assistant.threads.setStatus", Token: token, Body: body,
	}, PostOptions{})
	return err
}
