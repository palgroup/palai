package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// SEARCHING THE WORKSPACE (E21 T5, spec §36).
//
// WHY THIS IS A WIRE AND NOT AN MCP CONNECTION, since Slack ships an official MCP server and we already
// speak its transport. The blocker is authentication and it is total: the MCP server requires confidential
// OAuth 2.0 with a USER token — "You'll need to use your app's client_id and client_secret for Slack OAuth…
// Users go through OAuth consent and authorize the app" — and Palai has no authorization-code flow for MCP
// connections at all. Their secret model is a static handle: resolve, put in a header, drop. There is no
// consent redirect, no refresh loop, no per-user token store, and building one is an epic rather than a
// task (jira-mcp-connection.md, J6). What makes the rejection cheap rather than sad is the next line: the
// SAME capability is reachable with the bot token we already hold. Slack's own reference says
// assistant.search.context accepts a bot token given `search:read.public`. So going through the MCP server
// would have meant opening an OAuth epic to reach a method we can call directly today.
//   https://docs.slack.dev/ai/slack-mcp-server/ (checked 2026-07-27) — the OAuth requirement
//   https://docs.slack.dev/reference/methods/assistant.search.context/ (checked 2026-07-27) — the bot token
//
// NOTHING RETRIEVED HERE MAY BE STORED, and that is the vendor's term rather than our preference. Verbatim:
// "You must not store or copy any of the data retrieved from this API. You may not use any of this data for
// training." So a result enters a prompt, the run ends, and it is gone. It is written to no table, no
// artifact, no embedding and no evidence bundle — which is also why `knowledge-vector` stays disabled and
// why "let's vectorise the workspace" is not a feature this epic declined but one the contract forbids.
//   https://docs.slack.dev/apis/web-api/real-time-search-api/ (checked 2026-07-27)
//
// THE RESULT IS UNTRUSTED DATA. Every message this returns was typed by a human who is not the person
// asking, and some of them are not in the conversation at all. It reaches the model as quoted, attributed
// text with a warning ahead of it, it can never become a fetch target, and it grants nothing. Same rule
// E20 T3 set for thread history and E17 T3 set for remote A2A results.
//
// CONTRACT: https://docs.slack.dev/reference/methods/assistant.search.context/ (checked 2026-07-27) —
// arguments `query` (required, "User prompt or search query"), `action_token`, `channel_types`
// (public_channel,private_channel,mpim,im), `content_types` (messages,files,channels,users),
// `include_context_messages` (default false), `limit` (max 20, default 20), `before`/`after`, `sort`/
// `sort_dir`, `highlight`; the answer is {ok, results:{messages,files,channels}, response_metadata:
// {next_cursor}}; a refusal is HTTP 200 carrying {"ok":false,"error":…}. A natural-language question
// triggers SEMANTIC search. Rate limit is the tightest thing this repo calls: "for most teams, the limit
// is 10+ requests per minute", plus "a user-level limit of 10 requests per minute with burst".

// MaxSearchLimit is the vendor's ceiling AND its default. There is no "scan everything" here, and a caller
// asking for more gets this — silently widening is not on the table, but neither is failing over a number.
const MaxSearchLimit = 20

// MaxSearchTextRunes bounds a single returned message before it reaches a prompt. Slack messages can be
// long, twenty of them can be very long, and a search result is the LEAST important thing competing for the
// model's context — the human's own question must not be squeezed out by what the search dragged in.
const MaxSearchTextRunes = 600

// SearchMessage is one message the search returned. Every field is UNTRUSTED: text and username are written
// by workspace members, and channel is where they wrote it.
type SearchMessage struct {
	Channel  string // channel NAME as the API rendered it; display only
	Username string // author's display name as the API rendered it; display only, never a principal
	Text     string // flattened and bounded
	// Context is the surrounding messages, requested because the owner's use case ("what did X say about
	// the release?") is meaningless without them: a matched line alone rarely carries its own answer.
	Context []string
}

// SearchContext runs ONE Real-time Search query with the bot token.
//
// channel_types and content_types are DELIBERATELY NOT PARAMETERS. They are pinned to public_channel and
// messages, because a parameter is a way to ask for a scope, and the scopes we do not want (private
// channels, DMs, group DMs) are exactly the ones an argument would let a caller — or a model that reached
// this far — request. They are not granted to a bot token, so today the request would simply fail; pinning
// them means it stays that way if a scope is ever added for another reason. Fail-closed by construction
// rather than by a check somebody could delete.
//
// actionToken is REQUIRED for a bot token, in the API's own words: "All API calls made using a bot token
// require an action_token. API calls made using a user token do not require an action_token." It is a
// CREDENTIAL — it is never logged, never written to a table, never put in argv, and never enters evidence.
// It also comes from the event that started the run, so this search is bound to that conversation rather
// than being a standing power the app holds.
func SearchContext(ctx context.Context, doer Doer, apiBase string, token []byte, actionToken, query string, limit int) ([]SearchMessage, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("slack: a search needs a query")
	}
	if actionToken == "" {
		// Not an empty result: a caller that reaches here without a token has a bug, and answering "no
		// matches" would hide it behind something indistinguishable from a quiet workspace.
		return nil, fmt.Errorf("slack: a bot-token search needs an action_token")
	}
	if limit <= 0 || limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}
	form := url.Values{
		"query":                    {query},
		"action_token":             {actionToken},
		"channel_types":            {"public_channel"}, // pinned; see the doc comment
		"content_types":            {"messages"},       // pinned; see the doc comment
		"include_context_messages": {"true"},
		"limit":                    {strconv.Itoa(limit)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/assistant.search.context",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("slack: build search: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if len(token) > 0 {
		req.Header.Set("Authorization", "Bearer "+string(token))
	}
	resp, err := doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack: search: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Includes 429. Not retried here: ratelimit.go owns Slack's 429 for the SENDING path, and this
		// caller's own pacer is what keeps the 10/min ceiling — a second retry layer would stack on both.
		return nil, fmt.Errorf("slack: search answered HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("slack: read search body: %w", err)
	}
	// UNCONFIRMED (plan §3.5 M20): the reference names the results envelope but does not print a message
	// object's field list, so the per-message names below are decoded PERMISSIVELY — an absent field yields
	// an empty string and the message still renders with whatever it did carry. The live leg is what turns
	// this from a reading into a measurement. Decoding permissively is the fail-soft choice on purpose: a
	// strict decode would turn one unexpected field into a search that always errors.
	var env struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Results struct {
			Messages []struct {
				ChannelName string `json:"channel_name"`
				Channel     struct {
					Name string `json:"name"`
				} `json:"channel"`
				Username string `json:"username"`
				User     string `json:"user"`
				Text     string `json:"text"`
				Context  []struct {
					Text string `json:"text"`
				} `json:"context_messages"`
			} `json:"messages"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("slack: decode search: %w", err)
	}
	if !env.OK {
		if env.Error == "" {
			env.Error = "unknown"
		}
		// Typed so a caller can tell `missing_scope` (search:read.public was never granted) from
		// `invalid_auth` (a stale action_token) without matching on substrings.
		return nil, &APIError{Code: env.Error}
	}
	out := make([]SearchMessage, 0, len(env.Results.Messages))
	for _, m := range env.Results.Messages {
		channel := m.ChannelName
		if channel == "" {
			channel = m.Channel.Name
		}
		who := m.Username
		if who == "" {
			who = m.User
		}
		msg := SearchMessage{
			Channel:  flattenName(channel),
			Username: flattenName(who),
			Text:     boundedText(m.Text),
		}
		for _, c := range m.Context {
			if t := boundedText(c.Text); t != "" {
				msg.Context = append(msg.Context, t)
			}
		}
		if msg.Text == "" && len(msg.Context) == 0 {
			continue // nothing to show; an empty quote in a prompt is noise
		}
		out = append(out, msg)
	}
	return out, nil
}

// boundedText flattens and bounds one message. Flattening matters for the same reason it does in users.go:
// the caller renders "name: text" lines into a prompt, so a newline inside the text forges a speaker.
func boundedText(s string) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	runes := []rune(s)
	if len(runes) > MaxSearchTextRunes {
		// Visible, never silent — the same rule the renderer follows.
		return string(runes[:MaxSearchTextRunes]) + "…"
	}
	return s
}
