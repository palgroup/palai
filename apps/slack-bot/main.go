// Command slack-bot is a Palai bot process: one instance per registered row in the control plane's
// bot registry (POST/GET /v1/bots, apps/control-plane/api/bots.go). It reads FOUR environment
// variables and nothing else — PALAI_BOT_ID, PALAI_API_URL, PALAI_API_KEY, PALAI_BOT_DATABASE_URL —
// and fetches everything else about itself (its kind, the agent it speaks as, its Slack tokens'
// secret handles, its channels) from its own bot row at startup, via the SDK's Bots.Get. That is
// the point: an operator configures a bot in the admin panel, not in a file, and a Slack token is
// never this process's environment variable.
//
// THE SHAPE OF A RUNNING BOT, in the order this file builds it:
//
//	config.Load             the four variables
//	Bots.Get                its own row: what it is, and the NAMES its credentials are sealed under
//	parseSlackConfig        the row's `config` document (the keys apps/web-console/lib/channels.ts writes)
//	store.Open + Migrate    its own database: which Palai session a Slack thread is talking to
//	redeemSlackCredentials  the handles → the token bytes, over GET /v1/bots/{bot_id}/credentials
//	socket.Run              the Socket Mode connection, held open and reconnected until shutdown
//
// Everything a misconfigured ROW would fail on is answered first, for free, before a Postgres pool or a
// WebSocket is opened to discover it. The credentials come last, immediately before the only two things
// that need them, so nothing earlier in this function is ever holding a live token.
//
// HOW THIS PROCESS COMES TO HOLD A SLACK TOKEN, stated here because a reader deserves it at the top rather
// than three files in. The console seals the tokens through POST /v1/secret-refs and stores only the handle
// NAMES in this row; that has not changed and is the point of the design. Neither has the property under
// it: a sealed secret has no GENERAL read-back path, and GET /v1/secret-refs/{name} answers metadata with
// no parameter that adds the value. What exists is one narrow route — GET /v1/bots/{bot_id}/credentials,
// which this process calls with its OWN id at the step above (credentials.go) — and two of its properties
// belong up here. The caller names a BOT and never a SECRET: there is no body, query or header a secret
// name could ride in, so what gets opened is the `_ref` keys of the row named and nothing else, and no
// input can widen that. And it is gated on a capability of its own, `bots.credentials`, deliberately not
// `provision`: a key minted for a bot can be scoped to redeeming the handles of bots in its own project and
// to nothing else, so the most exposed credential in a deployment is not also a provisioning key.
//
// Reading SLACK_BOT_TOKEN out of the environment instead is still not the arrangement. It is no longer a
// workaround for anything either — but it is still one line, so it is enforced rather than promised:
// TestNoSlackCredentialComesFromTheEnvironment (wiring_test.go) fails the build if any shipped file under
// apps/slack-bot reads the environment outside internal/config.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	palai "github.com/palgroup/palai/sdks/go"

	"github.com/palgroup/palai/apps/slack-bot/internal/config"
	"github.com/palgroup/palai/apps/slack-bot/internal/relay"
	"github.com/palgroup/palai/apps/slack-bot/internal/socket"
	"github.com/palgroup/palai/apps/slack-bot/internal/store"
)

// slackAPIBase is where every outbound Slack call goes. It is a constant and not a variable because this
// process reads FOUR environment variables and nothing else (internal/config's own rule); the control
// plane's PALAI_SLACK_API_BASE_URL override exists for a staging deployment of a service that already
// reads dozens of variables, and adding a fifth here to match would be the first crack in that rule.
const slackAPIBase = "https://slack.com/api"

// shutdownGrace bounds how long a shutdown waits for relays that are still draining a run into Slack.
// They do not stop when the socket does — a relay detaches from the socket's context on purpose, so the
// message it opened gets closed rather than left "streaming" forever — so this is a wait, not a kill.
// When it runs out, the process says how many were still in flight instead of exiting quietly.
const shutdownGrace = 30 * time.Second

func main() {
	log.SetFlags(0)

	// Signal handling starts before anything that can block, so an operator's Ctrl-C or a container's
	// SIGTERM interrupts a slow bot-row fetch too, not just the socket at the end.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := dispatch(ctx, os.Args[1:]); err != nil {
		log.Fatalf("slack-bot: %v", err)
	}
}

// dispatch picks what this invocation is: the relay, or the one-shot self-test (selftest.go).
//
// A SUBCOMMAND AND NOT A VARIABLE, for the reason stated at the top of this file: this process reads four
// environment variables and nothing else, and a fifth one selecting a mode would be the first crack in that
// rule. Everything else about both modes still comes from the row.
//
// Anything unrecognised is a refusal carrying the usage, rather than a silent fall-through to the relay: a
// mistyped subcommand that started a long-lived Socket Mode connection instead would look, to the operator
// who typed it, exactly like a self-test that hung.
func dispatch(ctx context.Context, args []string) error {
	switch {
	case len(args) == 0:
		if err := run(ctx); err != nil {
			return err
		}
		log.Printf("slack-bot: stopped")
		return nil
	case args[0] == "selftest" && len(args) == 2:
		return runSelfTest(ctx, args[1])
	}
	return fmt.Errorf("usage:\n  slack-bot                        hold Socket Mode open and relay (what a deployment runs)\n  slack-bot selftest <channel-id>  run the four-leg live test once and exit, where <channel-id> is %s",
		selfTestChannelHelp)
}

// run is main with an error return, so every failure below leaves through one place and every deferred
// close actually runs — log.Fatal in the middle of this function would skip them.
func run(ctx context.Context) error {
	cfg, client, bot, slackCfg, err := loadBot(ctx)
	if err != nil {
		return err
	}

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	// Task 8 left Migrate deliberately unwired ("a caller, in whichever later task first needs a live
	// *Store, is expected to call Migrate once, right after Open, at process boot"). This is that caller.
	if err := st.Migrate(ctx); err != nil {
		return err
	}

	// The credentials are redeemed LAST, immediately before the two things that need them — the outbound
	// Slack clients and the dial. Nothing above this line can use a token, so nothing above it should be
	// holding one.
	creds, err := redeemSlackCredentials(ctx, client, bot.ID, slackCfg)
	if err != nil {
		return err
	}

	// Every relay.Run this process starts is tracked, so shutdown can wait for the ones still draining a
	// run into Slack rather than dropping them mid-message.
	var inflight sync.WaitGroup
	runInBackground := func(f func()) {
		inflight.Add(1)
		go func() {
			defer inflight.Done()
			f()
		}()
	}

	d := &dispatcher{
		approvals: relay.ApprovalDeps{
			Palai:            relay.NewApprovalsPalaiClient(client),
			Slack:            relay.NewApprovalSlack(http.DefaultClient, slackAPIBase, creds.botToken),
			AllowedApprovers: slackCfg.AllowedApprovers,
		},
		botUserID: slackCfg.BotUserID,
	}
	d.inbound = relay.NewInboundDeps(
		st,
		relay.NewPalaiClient(client),
		relay.NewChannelSlackStreamer(http.DefaultClient, slackAPIBase, creds.botToken),
		runInBackground,
		d.onApprovalRequested,
		bot.ID, slackCfg.BotUserID, bot.AgentRevisionID, bot.RepositoryBindingID,
	)

	log.Printf("slack-bot: opening Socket Mode")
	socketErr := socket.Run(ctx, socket.Config{
		AppToken: creds.appToken,
		APIBase:  slackAPIBase,
		Doer:     http.DefaultClient,
	}, d)

	// The socket is closed. Relays still draining a run are NOT cancelled with it — the Slack message each
	// one opened has to be closed, or it renders as permanently streaming — so this waits for them, and
	// says so if the wait runs out.
	drained := make(chan struct{})
	go func() {
		inflight.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(shutdownGrace):
		log.Printf("slack-bot: some runs were still streaming into Slack after %s; exiting anyway — their messages may stay open until Slack closes them", shutdownGrace)
	}
	return socketErr
}

// loadBot is everything BOTH modes do before they diverge: the four variables, this bot's own row, the
// checks that a row can produce a working bot at all, and its `config` document. It stops short of
// redeeming the credentials, so nothing here is ever holding a token — and the self-test redeems them at
// exactly the same point in its own sequence, for the same reason.
func loadBot(ctx context.Context) (config.Config, *palai.Client, *palai.Bot, slackConfig, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, nil, nil, slackConfig{}, err
	}

	client, err := palai.New(palai.WithBaseURL(cfg.PalaiBaseURL), palai.WithAPIKey(cfg.PalaiAPIKey))
	if err != nil {
		return cfg, nil, nil, slackConfig{}, fmt.Errorf("construct SDK client: %w", err)
	}

	bot, err := client.Bots.Get(ctx, cfg.BotID)
	if err != nil {
		var apiErr *palai.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return cfg, nil, nil, slackConfig{}, fmt.Errorf("PALAI_BOT_ID=%s has no row in the bot registry — register it first", cfg.BotID)
		}
		return cfg, nil, nil, slackConfig{}, fmt.Errorf("fetch bot row PALAI_BOT_ID=%s: %w", cfg.BotID, err)
	}
	// A disabled row is a refusal, not a warning: starting anyway would relay messages for a bot an
	// operator deliberately turned off. It refuses the SELF-TEST too, and that is deliberate rather than
	// incidental — a green self-test on a disabled bot is a report that the thing works, about a thing that
	// is switched off.
	if bot.Disabled {
		return cfg, nil, nil, slackConfig{}, fmt.Errorf("bot %s (%q) is disabled in the registry — enable it in the admin panel before starting this process", bot.ID, bot.Name)
	}
	// The KIND is checked because this binary is one channel's relay, not a generic one. The registry
	// accepts any kind by design (a bare string nothing branches on) precisely so tomorrow's WhatsApp row
	// costs no control-plane change — which means a row pointed at the wrong binary is a real mistake an
	// operator can make, and it must be named here rather than surface as a Slack API call that fails for
	// an unrelated-looking reason.
	if bot.Kind != "slack" {
		return cfg, nil, nil, slackConfig{}, fmt.Errorf("bot %s (%q) has kind %q; this process relays Slack and nothing else", bot.ID, bot.Name, bot.Kind)
	}

	slackCfg, err := parseSlackConfig(bot.Config)
	if err != nil {
		return cfg, nil, nil, slackConfig{}, fmt.Errorf("bot %s (%q): %w", bot.ID, bot.Name, err)
	}
	log.Printf("slack-bot: running as %s %q (kind=%s agent_revision_id=%s repository_binding_id=%s)",
		bot.ID, bot.Name, bot.Kind, orNone(bot.AgentRevisionID), orNone(bot.RepositoryBindingID))
	log.Printf("slack-bot: %s", slackCfg.describe())
	return cfg, client, bot, slackCfg, nil
}
