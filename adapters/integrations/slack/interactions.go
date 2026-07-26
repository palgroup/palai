package slack

import (
	"encoding/json"
	"errors"
	"net/url"
)

// The interactivity HTTP transport (E19 T2, spec §36, plan §3.5 row D8). Slack's interactivity Request URL
// receives a DIFFERENT body shape from the Events API, and the difference is a security boundary rather than
// a formatting detail.
//
// CONTRACT: https://docs.slack.dev/interactivity/handling-user-interaction/ (checked 2026-07-26) — payloads
// arrive "in the form application/x-www-form-urlencoded" and "The body of the request will contain a payload
// parameter; your app should parse this payload parameter as JSON." A 200 is required "within 3 seconds of
// receiving the payload".
//
// THE INFERENCE, WRITTEN DOWN WITH ITS SOURCE so a later reader does not have to re-derive it: the
// interactivity page above does NOT say that the v0 signature must be verified over the raw form body before
// the payload is deserialized. It never mentions verification at all. That requirement comes from the OTHER
// page — https://docs.slack.dev/authentication/verifying-requests-from-slack/ (checked 2026-07-26), which
// says the base string is built from "the raw request body, before it has been deserialized". Composing the
// two: the bytes Slack MACs are `payload=%7B%22type%22...`, NOT the JSON inside. So a receiver that
// url-decodes first and verifies the extracted JSON is verifying a string Slack never signed — and since no
// signature over it can ever match, the only way such a receiver "works" is by not checking at all. The order
// in the route (verify the RAW body, then extract) is forced by this composition, not by preference.

// ErrNotFormEncoded is a body that is not the documented single url-encoded `payload` parameter: a raw JSON
// body, an empty body, a form without the parameter, or a form carrying it more than once.
var ErrNotFormEncoded = errors.New("slack: interactivity body is not a single url-encoded payload parameter")

// ExtractInteractionPayload url-decodes the interactivity form body and returns the single `payload`
// parameter's JSON. It MUST run AFTER VerifySignature over the same raw bytes (see the inference above).
//
// More than one `payload` parameter is refused rather than resolved by taking the first: the MAC covers the
// WHOLE body, so a body carrying a decoy the receiver never reads is signed just as validly as one that does
// not — and "which of the two did we act on" is not a question a signature can answer afterwards.
func ExtractInteractionPayload(rawFormBody []byte) ([]byte, error) {
	values, err := url.ParseQuery(string(rawFormBody))
	if err != nil {
		return nil, ErrNotFormEncoded
	}
	payloads := values["payload"]
	if len(payloads) != 1 || payloads[0] == "" {
		return nil, ErrNotFormEncoded
	}
	return []byte(payloads[0]), nil
}

// ParseInteractionTeam reads the workspace id out of an interactivity payload so the receiver can resolve
// WHICH connection's signing secret to verify against. It is the ParseTeam sibling and carries the same
// warning: this runs BEFORE authentication, so the value is a LOOKUP KEY and never an identity. What makes it
// safe is that the resolved connection's secret must then MAC this very body.
//
// CONTRACT: https://docs.slack.dev/reference/interaction-payloads/block_actions-payload/ (checked
// 2026-07-26) — the payload carries `team: {id, domain}` and `user: {id, username, team_id}`.
//
// CEILING: enterpriseID is always empty. Enterprise Grid / org-wide install is out of scope for this phase
// (a Grid payload carries `enterprise` and `is_enterprise_install`), so a connection registered with an
// enterprise id does not resolve on this route. Stated rather than half-implemented.
func ParseInteractionTeam(payload []byte) (teamID, enterpriseID string, ok bool) {
	var probe struct {
		Team struct {
			ID string `json:"id"`
		} `json:"team"`
		User struct {
			TeamID string `json:"team_id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return "", "", false
	}
	team := probe.Team.ID
	if team == "" {
		team = probe.User.TeamID
	}
	if team == "" {
		return "", "", false
	}
	return team, "", true
}

// ParseAppRateLimited reports whether a body is Slack telling us our app has exceeded its Events API delivery
// budget, and returns the workspace + the minute the throttling started. It is an OUTER type like
// url_verification — not an event_callback — so MapEvent refuses it, and without this branch the route would
// answer 400 + x-slack-no-retry and discard the one signal that says we are being throttled.
//
// CONTRACT: https://docs.slack.dev/apis/web-api/rate-limits/ (checked 2026-07-26) — Events API delivery is
// capped at "30,000 per workspace/team per app per 60 minutes"; past that the app receives an
// `app_rate_limited` event with token, type, team_id, minute_rate_limited and api_app_id.
func ParseAppRateLimited(body []byte) (teamID string, minuteRateLimited int64, ok bool) {
	var probe struct {
		Type              string `json:"type"`
		TeamID            string `json:"team_id"`
		MinuteRateLimited int64  `json:"minute_rate_limited"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.Type != "app_rate_limited" {
		return "", 0, false
	}
	return probe.TeamID, probe.MinuteRateLimited, true
}

// ApprovalMessage builds the chat.postMessage body for an approval request: the authoritative detail of the
// operation plus the two minted buttons, each carrying the one-shot request hash in its `value` (which is
// what makes the resulting decision EXACT — see MapInteractiveApproval).
//
// HONEST CEILING, stated where a reader meets it: this is the MINIMUM Block Kit surface — one section of
// text and two buttons. A rich approval UI (diffs, arguments, reviewer lists, modals) is the console's job,
// not the transport's. Nothing here degrades if that is built later; it is simply not built here.
func ApprovalMessage(channel, threadTS, detail, requestHash string) []byte {
	body := map[string]any{
		"channel": channel,
		// text is the notification/fallback string, so the detail is legible without Block Kit rendering.
		"text": "Approval requested: " + detail,
		"blocks": []any{
			map[string]any{
				"type": "section",
				"text": map[string]any{"type": "mrkdwn", "text": "*Approval requested*\n" + detail},
			},
			map[string]any{
				"type": "actions",
				"elements": []any{
					map[string]any{
						"type":      "button",
						"action_id": ActionApprove,
						"text":      map[string]any{"type": "plain_text", "text": "Approve"},
						"style":     "primary",
						"value":     requestHash,
					},
					map[string]any{
						"type":      "button",
						"action_id": ActionDeny,
						"text":      map[string]any{"type": "plain_text", "text": "Deny"},
						"style":     "danger",
						"value":     requestHash,
					},
				},
			},
		},
	}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}
	raw, _ := json.Marshal(body)
	return raw
}

// UpdateMessage builds the chat.update body that REPAIRS an already-visible message in place: same channel,
// the message's own ts, new text and no buttons (the decision is made; the buttons must not invite a second
// click). Editing the visible message is the SLK-006 repair — one message per thread, never a second post.
func UpdateMessage(channel, messageTS, text string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"channel": channel,
		"ts":      messageTS,
		"text":    text,
		"blocks": []any{
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}},
		},
	})
	return raw
}
