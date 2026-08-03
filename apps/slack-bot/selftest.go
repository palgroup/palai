package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/slack-bot/internal/relay"
	"github.com/palgroup/palai/apps/slack-bot/internal/socket"
)

// `slack-bot selftest <channel-id>` — the live test an operator presses after installing their Slack app
// (2026-08-03 plan, Task 13). It is a SUBCOMMAND rather than a variable because this process reads four
// environment variables and nothing else (main.go's rule): a fifth one to select a mode would be the first
// crack in it, and an argument is what a mode selection is anyway.
//
// IT REDEEMS THE SAME CREDENTIALS THE RELAY DOES, over the same route, from the same row. That is most of
// why it is worth running at all: a self-test that read tokens from somewhere else would be testing
// somewhere else. What it adds to the relay's own startup is that it drives all four things a relay needs
// and then STOPS, so the answer arrives as an exit code and four lines rather than as a process that either
// stays up or does not.
//
// THE CHANNEL IS AN ARGUMENT, and it has to be. The bot's row carries no channel — the relay answers
// wherever it is mentioned — and this app cannot discover one either: `channels:read` is not among the nine
// scopes the manifest asks for (apps/web-console/lib/botManifest.ts), measured 2026-08-03 against the live
// workspace, where both conversations.list and users.conversations answered
// `missing_scope: channels:read,groups:read,mpim:read,im:read`. So there is nothing to default to and
// nothing to guess: an operator names the channel they invited the app to.

// selfTestChannelHelp is what an operator is told when the channel is missing. It names where the id is
// found, because "a channel id" and "a channel name" look equally plausible to someone who has one.
const selfTestChannelHelp = "a channel ID (Slack → right-click the channel → View channel details → the `C…` at the bottom), which must be a channel this app has been invited to"

// runSelfTest drives the four legs and prints one line per leg.
//
// It returns an error only when the test could not be RUN — a row that cannot be read, credentials that
// cannot be redeemed. A leg that failed is a completed test with an answer, so it is printed like the
// others and turned into a non-zero exit at the end, never into a Go error that would bury three green legs
// under one sentence.
func runSelfTest(ctx context.Context, channel string) error {
	if channel == "" {
		return fmt.Errorf("this self-test needs %s", selfTestChannelHelp)
	}

	// The database URL is loaded and deliberately unused here: the self-test opens no store. A thread's
	// session mapping is the RELAY's state, and a test that migrated a schema to answer "can this token
	// post" would be doing durable work on the operator's behalf without being asked.
	_, client, bot, slackCfg, err := loadBot(ctx)
	if err != nil {
		return err
	}
	creds, err := redeemSlackCredentials(ctx, client, bot.ID, slackCfg)
	if err != nil {
		return err
	}

	log.Printf("slack-bot: self-testing %s %q against channel %s", bot.ID, bot.Name, channel)
	report, err := relay.SelfTest(ctx, relay.SelfTestDeps{
		Slack: selfTestSlack{
			doer:     http.DefaultClient,
			apiBase:  slackAPIBase,
			botToken: creds.botToken,
			appToken: creds.appToken,
		},
		// The relay's own stream constructor, so leg 4 goes through the code a real run streams through.
		NewStream: relay.NewChannelSlackStreamer(http.DefaultClient, slackAPIBase, creds.botToken),
		Channel:   channel,
	})
	if err != nil {
		return err
	}

	// A LEG THAT NEVER RAN IS NOT A LEG THAT FAILED, and the difference is the whole value of reporting
	// four of them: the legs run in order and stop at the first failure, so everything after it is
	// UNKNOWN. Printing those as FAIL would send an operator to fix three things when one is broken —
	// and, worse, would let them "fix" a leg that was never in question.
	failed := false
	for _, leg := range []struct {
		ok   bool
		name string
	}{
		{report.AuthOK, "1. auth.test — the bot token is valid and names its workspace"},
		{report.SocketOK, "2. Socket Mode — a connection opens and closes"},
		{report.PostOK, "3. chat.postMessage — the app can speak in the channel"},
		{report.StreamOK, "4. the stream trio — startStream, two appends, stopStream, read back"},
	} {
		mark := "ok"
		switch {
		case leg.ok:
		case failed:
			mark = "—"
		default:
			mark = "FAIL"
			failed = true
		}
		log.Printf("  [%4s] %s", mark, leg.name)
	}
	if failed {
		log.Printf("  [%4s] means the leg was not reached: the legs run in order and stop at the first failure", "—")
	}
	log.Printf("slack-bot: %s", report.Detail)
	if report.StopStream != "" {
		// PRINTED WHETHER OR NOT LEG 4 PASSED, because the measurement is the point: a run that observed
		// `replaces` failed the leg AND learned the thing the leg exists to learn.
		log.Printf("slack-bot: MEASUREMENT — chat.stopStream %s its markdown_text; the workspace kept %q", report.StopStream, report.Streamed)
	}
	if !report.OK() {
		return fmt.Errorf("the self-test did not pass")
	}
	return nil
}

// selfTestSlack is relay.SelfTestSlack against the real Slack API — the three legs that are single calls,
// each one delegating to the surface that already owns it: adapters/integrations/slack for the Web API, and
// this bot's OWN socket package for the dial, so leg 2 proves the loop the relay runs rather than a second
// one written here.
type selfTestSlack struct {
	doer     slack.Doer
	apiBase  string
	botToken []byte
	// appToken is Socket Mode's only authentication and is used by leg 2 alone. It is a DIFFERENT token
	// from botToken, which is why leg 1 passing says nothing about leg 2.
	appToken []byte
}

func (s selfTestSlack) AuthTest(ctx context.Context) (slack.Identity, error) {
	return slack.AuthTest(ctx, s.doer, s.apiBase, s.botToken)
}

func (s selfTestSlack) OpenSocket(ctx context.Context) (int, error) {
	return socket.Probe(ctx, socket.Config{
		AppToken: s.appToken, APIBase: s.apiBase, Doer: s.doer,
		// A self-test narrates its own legs; the dial's retry chatter would interleave with them and the
		// dial is not retried here anyway (Probe makes one attempt).
		Logf: func(string, ...any) {},
	})
}

// PostMessage is leg 3, over the package's single retry owner (slack.PostMessage) — the same sender the
// relay's approval message and every other outbound post ride, so a rate-limited workspace behaves here the
// way it behaves in production.
func (s selfTestSlack) PostMessage(ctx context.Context, channel, text string) (string, error) {
	body, err := json.Marshal(map[string]string{"channel": channel, "text": text})
	if err != nil {
		return "", fmt.Errorf("build the test message: %w", err)
	}
	out, err := slack.PostMessage(ctx, s.doer, slack.PostRequest{
		MethodURL: s.apiBase + "/chat.postMessage",
		Token:     s.botToken,
		Body:      body,
	}, slack.PostOptions{})
	if err != nil {
		return "", err
	}
	return out.MessageTS, nil
}

// selfTestThreadLimit bounds leg 4's read-back. The thread it reads is one this test made moments earlier
// and holds two messages; the bound is against a channel where somebody is replying while it runs.
const selfTestThreadLimit = 50

// MessageText reads ONE message back out of the thread, through the package's shipped thread reader.
//
// IT RIDES ThreadReplies RATHER THAN A SECOND conversations.replies CALLER, and the one thing that reader
// does which matters here is stated rather than assumed: it DROPS any message carrying a subtype. A
// streamed message carries none — measured 2026-08-03 against the live workspace, where the completed
// stream came back with `subtype` absent and `streaming_state: "completed"` — so the message this leg is
// looking for survives that filter. If it ever stopped surviving it, the failure below says so by name
// instead of reporting an empty message.
func (s selfTestSlack) MessageText(ctx context.Context, channel, threadTS, messageTS string) (string, error) {
	msgs, _, err := slack.ThreadReplies(ctx, s.doer, s.apiBase, s.botToken, channel, threadTS, selfTestThreadLimit)
	if err != nil {
		return "", err
	}
	for _, m := range msgs {
		if m.TS == messageTS {
			return m.Text, nil
		}
	}
	return "", fmt.Errorf("the streamed message %s was not among the %d messages read back from thread %s — it was accepted by Slack and is not in the thread, which is what a message carrying a subtype looks like from here (slack.ThreadReplies drops those)", messageTS, len(msgs), threadTS)
}
