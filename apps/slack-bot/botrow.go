package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The bot row's `config` document, as the CONSOLE writes it. Every key below is a contract with another
// program: apps/web-console/lib/channels.ts declares them (its own words: "the keys below are a contract
// with a reader in another program, which is why each one names its reader"), and this file is that
// reader. The control plane stores the document verbatim and looks inside it for nothing — `kind` is a
// bare string and `config` is opaque by design — so the two ends agree here or nowhere.
//
// A KEY THIS FILE DOES NOT DECODE IS A KEY NOBODY READS. json.Unmarshal ignores unknown fields, which is
// the right behaviour for a forward-compatible document, but it also means a console that starts writing
// `foo_ref` and a bot that never reads it look identical from both sides. That is why every field the
// console declares appears below, including the one this process cannot use (see SigningSecretRef).
type slackConfig struct {
	// TeamID is the workspace, `T…`. The console derives every sealed handle's NAME from it; this process
	// uses it for nothing at runtime — an inbound event carries its own team_id — so it is decoded to be
	// logged and to make the row's own shape checkable, not to route with.
	TeamID string `json:"team_id"`
	// BotUserID is the self-loop guard (relay.InboundDeps.BotUserID) AND what strips the `<@U…>` prefix
	// out of a mention's text (slack.MapEvent's stripMention). It is NOT required: MapEvent's own guard
	// also drops anything carrying a bot_id, which is what the app's own posts carry, so an empty value
	// costs a tidier prompt rather than a message loop.
	BotUserID string `json:"bot_user_id"`
	// AllowedApprovers is the ONLY per-human authorization the approval path has
	// (relay.ApprovalDeps.AllowedApprovers): the control plane sees ONE deciding principal — this bot's
	// API key — for every click, so it cannot tell one clicker from another. An empty list denies
	// everyone, which is a safe default and a silent one, so startup says so out loud.
	AllowedApprovers []string `json:"allowed_approvers"`
	// SigningSecretRef verifies the v0 signature on an Events/interactivity HTTP callback. THIS PROCESS
	// NEVER RECEIVES ONE: it speaks Socket Mode, whose documented contract is that the pre-authenticated
	// WebSocket IS the authentication ("there's no need to verify or validate inbound events" —
	// docs.slack.dev/apis/events-api/using-socket-mode). So this handle is decoded, reported at startup,
	// and used by nothing. Its VALUE does arrive here even so: the credentials route walks every `_ref` key
	// generically, so the redemption carries this secret back alongside the two tokens and credentials.go
	// simply never reads that key out of the map. It is not dead configuration: the console asks for it
	// because a Slack app has one, and a future HTTP transport would need it.
	SigningSecretRef string `json:"signing_secret_ref"`
	// BotTokenRef is the xoxb- token every outbound call presents: the stream this bot opens in a thread,
	// and the approval message a human clicks.
	BotTokenRef string `json:"bot_token_ref"`
	// AppTokenRef is the xapp- app-level token, Socket Mode's ONLY authentication.
	AppTokenRef string `json:"app_token_ref"`
}

// parseSlackConfig decodes a bot row's `config` and refuses a document that cannot produce a running bot.
//
// IT REFUSES ON THE HANDLES AND NOT ON THE OTHER KEYS, and the split is what each one costs when absent:
// without app_token_ref there is nothing to present at connect and no event can ever arrive; without
// bot_token_ref the bot connects and then cannot say a word, which is worse than not starting because it
// looks like it is working. team_id and bot_user_id cost accuracy, not function, so they are reported
// rather than refused.
//
// DisallowUnknownFields is deliberately NOT used. The console may add a key before this process learns to
// read it — that is an ordinary deployment order, not an error — and refusing here would take the bot down
// for a field it does not need.
func parseSlackConfig(raw json.RawMessage) (slackConfig, error) {
	if len(raw) == 0 {
		return slackConfig{}, errors.New("the bot row carries no config document; configure this bot in the admin panel (Bots → this bot → Credentials)")
	}
	var cfg slackConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return slackConfig{}, fmt.Errorf("the bot row's config is not a JSON object this process can read: %w", err)
	}
	var missing []string
	if cfg.AppTokenRef == "" {
		missing = append(missing, "app_token_ref (the xapp- app-level token; Socket Mode's only authentication)")
	}
	if cfg.BotTokenRef == "" {
		missing = append(missing, "bot_token_ref (the xoxb- bot token; without it this bot connects and can say nothing)")
	}
	if len(missing) > 0 {
		return slackConfig{}, fmt.Errorf("the bot row's config names no %s — seal the values in the admin panel (Bots → this bot → Credentials), which stores the handle and never the token",
			strings.Join(missing, " and no "))
	}
	return cfg, nil
}

// describe is the one line startup logs about how this bot is configured. It names every handle by NAME —
// a secret ref name is not a secret — and no value, and it says out loud the two things that are legal,
// silent, and surprising: an empty approver list denies every click, and an unknown bot user id leaves a
// mention's `<@U…>` prefix in the text the model reads.
func (c slackConfig) describe() string {
	parts := []string{
		"team=" + orNone(c.TeamID),
		"bot_user=" + orNone(c.BotUserID),
		fmt.Sprintf("approvers=%d", len(c.AllowedApprovers)),
		"app_token_ref=" + c.AppTokenRef,
		"bot_token_ref=" + c.BotTokenRef,
		"signing_secret_ref=" + orNone(c.SigningSecretRef),
	}
	line := strings.Join(parts, " ")
	if len(c.AllowedApprovers) == 0 {
		line += " — NO approvers are configured, so every approve/deny click will be refused"
	}
	if c.BotUserID == "" {
		line += " — no bot_user_id, so a mention's own <@…> prefix stays in the text the agent reads"
	}
	return line
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
