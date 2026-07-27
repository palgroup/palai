package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// READING A THREAD THE APP WAS INVITED INTO LATE (spec §36).
//
// WHY IT EXISTS. Every other read in this package is a write's answer; this one is the only place the app
// asks Slack what was said. It exists because of one live observation: the owner invited the bot into an
// existing #centauri thread carrying a long technical discussion and wrote "özetle" (summarise). The agent
// answered "please share the text you want summarised" — honest, and useless, because the thread predated the
// bot and nothing in the session carried it. A thread the app has been in from the start needs none of this
// (SLK-003 chains it onto one session and run.start replays that session); a thread it JOINED does.
//
// THE AUTHORITY BOUNDARY, and it is the whole reason this is one narrow function rather than a client:
// `channels:history` is granted, so a fetch keyed on an id somebody else chose IS an arbitrary read of the
// workspace. This function therefore takes the channel and the thread as ARGUMENTS and resolves nothing: no
// lookup, no name, no listing, no search. What may legitimately be passed is decided one layer up
// (slack_thread.go), where the admitted event is — and only the admitted event's own coordinates ever are.
//
// IT IS NOT A SENDER, so it deliberately does NOT ride PostMessage: that function owns this package's single
// 429 retry budget for the VISIBLE message, and it decodes {ok,ts}. A read has neither a ts nor a message to
// repair, and it runs inside the 2-second Slack ack budget (api/slack.go slackAckBudget), so a retry here
// would spend the budget that has to birth the run. A refusal or a 429 answers the caller, which then admits
// the run with no thread context — visibly less useful, never a lost message.
//
// CONTRACT: https://docs.slack.dev/reference/methods/conversations.replies/ (checked 2026-07-27) — required
// `channel` + `ts`; optional `limit`, `cursor`, `oldest`, `latest`, `inclusive`; HTTP GET;
// application/x-www-form-urlencoded or application/json; bot scopes channels:history / groups:history /
// im:history / mpim:history; Tier 3 (50+ per minute); the answer is
// {ok, messages:[…], has_more, response_metadata:{next_cursor}}.
//
// TWO PUBLISHED FACTS THAT SHAPE THE CALLER, so it does not have to rediscover them:
//
//  1. "The earliest messages in the time range are returned first" — the page's own words. So one page is
//     the START of a thread, never its tail, and paging is FORWARD through next_cursor.
//  2. `limit` DEFAULTS TO 1000. An unset limit is therefore an unbounded prompt written by whoever is loudest
//     in the thread; the caller must pass one.
type ThreadMessage struct {
	// UserID is the author. It is carried so the caller can tell the app's OWN prior turns from other
	// people's — it is never rendered into a prompt (see slackThreadNote).
	UserID string
	TS     string
	Text   string
}

// ThreadReplies reads one page of a thread's messages. channel and ts must be the coordinates of a message
// the app has already ADMITTED — this function cannot check that, which is exactly why its only caller is the
// admission path.
//
// WHAT IS FILTERED HERE, because it is protocol knowledge rather than prompt shaping:
//
//   - A message with a SUBTYPE is dropped. This is fail-closed and it is what keeps a RETRACTION retracted:
//     Slack replaces a deleted thread parent with a `tombstone` subtype, and joins/leaves/file-shares carry
//     their own. No page enumerates the subtype set, so an allow-list of "" is the only rule that cannot be
//     out-of-date. Cost: a file share's comment text is not read (deliberate — a file is not this task's, and
//     the app's own replies carry no subtype, so its turns survive).
//   - An EMPTY text contributes nothing.
//
// A deleted REPLY needs no rule at all: Slack simply stops returning it.
//
// hasMore is the page's own `has_more`, passed up so the caller can say so rather than implying the thread
// ended where the page did.
func ThreadReplies(ctx context.Context, doer Doer, apiBase string, token []byte, channel, ts string, limit int) (msgs []ThreadMessage, hasMore bool, err error) {
	if channel == "" || ts == "" || limit <= 0 {
		return nil, false, fmt.Errorf("slack: a thread read needs a channel, a thread ts and a limit")
	}
	query := url.Values{
		"channel": {channel},
		"ts":      {ts},
		"limit":   {strconv.Itoa(limit)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/conversations.replies?"+query.Encode(), nil)
	if err != nil {
		return nil, false, fmt.Errorf("slack: build thread read: %w", err)
	}
	if len(token) > 0 {
		// The token rides the header and nothing else — never the query string, which lands in every proxy
		// log between here and Slack.
		req.Header.Set("Authorization", "Bearer "+string(token))
	}
	resp, err := doer.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("slack: thread read: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Includes 429. A read is not repaired (see the header comment): the caller admits without context.
		return nil, false, fmt.Errorf("slack: thread read answered HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, false, fmt.Errorf("slack: read thread body: %w", err)
	}
	var env struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		HasMore  bool   `json:"has_more"`
		Messages []struct {
			Subtype string `json:"subtype"`
			User    string `json:"user"`
			TS      string `json:"ts"`
			Text    string `json:"text"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, false, fmt.Errorf("slack: decode thread read: %w", err)
	}
	if !env.OK {
		if env.Error == "" {
			env.Error = "unknown"
		}
		// Typed, so a caller can tell `missing_scope` (a private channel this app was never granted) from
		// `thread_not_found` (the thread is gone) without substring matching.
		return nil, false, &APIError{Code: env.Error}
	}
	for _, m := range env.Messages {
		if m.Subtype != "" || m.Text == "" {
			continue
		}
		msgs = append(msgs, ThreadMessage{UserID: m.User, TS: m.TS, Text: m.Text})
	}
	return msgs, env.HasMore, nil
}
