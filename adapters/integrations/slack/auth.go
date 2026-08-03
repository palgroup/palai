package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ASKING SLACK WHO THIS TOKEN IS (2026-08-03 plan, Task 13).
//
// WHY IT EXISTS. Every other call in this package does something — posts, streams, reads a thread — and
// learns whether the token works only as a side effect of the thing failing. A bot's self-test needs the
// question asked directly and FIRST, because "invalid_auth" arriving from chat.postMessage is
// indistinguishable at the call site from "that channel does not exist": the operator is sent to the wrong
// screen. auth.test answers with no side effect at all, so a self-test can separate "the token is wrong"
// from "the token is right and something else is".
//
// AND IT ANSWERS THE SECOND HALF OF THE QUESTION TOO: which workspace this token actually belongs to. A
// token pasted from the wrong app is a valid token — it passes every "is it well-formed" check a console
// can make — and the only thing that catches it is Slack naming the workspace back. That is why Identity
// carries the team as well as the ok.
//
// CONTRACT: https://docs.slack.dev/reference/methods/auth.test/ (checked 2026-08-03 against the live
// workspace, transcript in .superpowers/sdd/2026-08-03-slack-bot-as-sdk-relay/task-13-report.md) — HTTP
// POST; NO required parameters and no scope (it is the one method every token may call); the answer is
// {ok, url, team, user, team_id, user_id, bot_id, is_enterprise_install}; a refusal is HTTP 200 carrying
// {"ok":false,"error":"invalid_auth"|"not_authed"|"account_inactive"|"token_revoked"|…}.

// Identity is who Slack says a token is. Every field is Slack's own word for it.
//
// UserID is the load-bearing one for a bot token and it is worth saying why: it is the `U…` the app speaks
// as, which is BOTH the self-loop guard the relay drops its own messages by (relay.InboundDeps.BotUserID)
// and the recipient id chat.startStream requires. A bot row that carries no bot_user_id can therefore learn
// its own from here rather than from an operator copying it out of the Slack UI.
type Identity struct {
	UserID string
	// User is the app's handle (`t125-live-bot`), not a human's name.
	User   string
	TeamID string
	Team   string
	// URL is the workspace's own address (https://<workspace>.slack.com/), which is what makes "the wrong
	// workspace" legible to an operator who knows the name and not the `T…`.
	URL string
	// BotID is the `B…` app id. It is EMPTY for a user token, which is the one structural difference
	// between the two kinds of token this tree ever holds, so it is carried rather than dropped.
	BotID string
}

// AuthTest asks Slack who the token is. A refusal comes back as a typed *APIError carrying Slack's own
// code, never a renamed one — the code is what sends an operator to the right screen (`invalid_auth` is a
// token to re-paste, `account_inactive` is a workspace to look at), and a message this package invented
// would send them to this file instead.
//
// IT IS NOT A SENDER, so like ThreadReplies and DisplayName it deliberately does not ride PostMessage: no
// retry budget and no {ok,ts} decoding. A self-test that is rate-limited has learned something real and
// should say so rather than wait.
func AuthTest(ctx context.Context, doer Doer, apiBase string, token []byte) (Identity, error) {
	if len(token) == 0 {
		return Identity{}, fmt.Errorf("slack: auth.test needs a token — it is the only thing the call carries")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/auth.test", nil)
	if err != nil {
		return Identity{}, fmt.Errorf("slack: build auth.test: %w", err)
	}
	// The header and nothing else, as everywhere in this package. The call takes no parameters; Slack's Web
	// API expects a form content type regardless (the same shape socket.dial presents to
	// apps.connections.open).
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := doer.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("slack: auth.test: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("slack: auth.test answered HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Identity{}, fmt.Errorf("slack: read auth.test body: %w", err)
	}
	var env struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		URL    string `json:"url"`
		Team   string `json:"team"`
		User   string `json:"user"`
		TeamID string `json:"team_id"`
		UserID string `json:"user_id"`
		BotID  string `json:"bot_id"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Identity{}, fmt.Errorf("slack: decode auth.test: %w", err)
	}
	if !env.OK {
		if env.Error == "" {
			env.Error = "unknown"
		}
		return Identity{}, &APIError{Code: env.Error}
	}
	return Identity{
		UserID: env.UserID, User: env.User,
		TeamID: env.TeamID, Team: env.Team,
		URL: env.URL, BotID: env.BotID,
	}, nil
}
