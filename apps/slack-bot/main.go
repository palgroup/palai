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
			Posts:            st,
			BotID:            bot.ID,
		},
		botUserID: slackCfg.BotUserID,
	}
	d.inbound = relay.NewInboundDeps(
		st,
		relay.NewPalaiClient(client),
		relay.NewChannelSlackStreamer(http.DefaultClient, slackAPIBase, creds.botToken),
		runInBackground,
		d.onApprovalRequested,
		d.onRunFailed,
		bot.ID, slackCfg.BotUserID, bot.AgentRevisionID, bot.RepositoryBindingID,
	).WithImages(&relay.ImageLeg{
		// A SEPARATE Doer FROM THE STREAMING CLIENT'S IS NOT NEEDED — this one talks to files.slack.com
		// rather than the Web API, but http.DefaultClient serves both and neither holds per-host state.
		Doer:      http.DefaultClient,
		Token:     creds.botToken,
		Artifacts: relay.NewArtifactCreator(client),
		// The thread read that finds the pictures shared BEFORE the message that births a run. It is mounted
		// unconditionally alongside the leg it feeds: it needs no credential of its own (the same bot token,
		// the same client), and what it finds is worth nothing without a leg to attach it — so a deployment
		// carrying one and not the other would be a configuration with no meaning.
	}, log.Printf).WithThreadHistory(relay.NewThreadHistory(http.DefaultClient, slackAPIBase, creds.botToken))

	// SAID AT BOOT, ONCE, because the alternative is how this capability died the last time it was built:
	// mounted behind a nil check in a deployment that never configured it, with the only evidence anywhere
	// sitting inside a run's own prompt — an operator had to read a model's input to find out that shared
	// screenshots were being dropped. A line per message would bury a permanent configuration fact under
	// traffic; a line at boot is where an operator already looks.
	if d.inbound.Images.Ready() {
		log.Printf("slack-bot: image leg ready — a screenshot shared with a message is fetched and attached to the turn, and so are the images shared earlier in a thread this bot is invited into")
	} else {
		log.Printf("slack-bot: image leg NOT ready — a screenshot shared with a message will be relayed as a note saying it could not be attached")
	}

	// BEFORE THE SOCKET, and that ordering is the whole reason this call is here rather than alongside the
	// sweep below. These are answers this process's predecessor was in the middle of writing when it died;
	// finishing them cannot wait on a WebSocket handshake, and a thread that has been silent since the last
	// restart should not have to wait for the next inbound message either. It hands each recovery off to a
	// goroutine and returns, so a run that is genuinely still working does not hold the socket shut.
	//
	// A FAILURE HERE DOES NOT STOP THE BOT. Recovery is about the PREVIOUS process's unfinished business;
	// refusing to serve the next message because the last one could not be cleaned up would turn a partial
	// outage into a total one. The record stays pending either way, so the next start tries again.
	if err := relay.RecoverPendingRuns(ctx, d.inbound); err != nil {
		log.Printf("slack-bot: could not finish the answers left by the previous process (they are still recorded and the next start will retry): %v", err)
	}

	// The approval sweep runs for the whole life of the process, not only at boot. Its boot pass is the one
	// that matters most — a run parked on a human while this bot was down was never asked, and nothing else
	// will ever ask — but the same loop also covers a live process whose event stream dropped, and that
	// second case would be invisible in every test a boot-only scan could pass.
	sweepCtx, stopSweep := context.WithCancel(ctx)
	defer stopSweep()
	sweepDone := startApprovalSweep(sweepCtx, relay.SweepApprovalDeps{
		Approvals: d.approvals,
		Threads:   st,
		Logf:      log.Printf,
	})

	log.Printf("slack-bot: opening Socket Mode")
	socketErr := socket.Run(ctx, socket.Config{
		AppToken: creds.appToken,
		APIBase:  slackAPIBase,
		Doer:     http.DefaultClient,
	}, d)
	stopSweep()
	<-sweepDone

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

// approvalSweepInterval is how often this process re-asks the control plane whether any run of its own is
// parked on a human who has not been shown the buttons.
//
// IT IS A CONSTANT AND NOT A SETTING, deliberately. The cost is one GET /v1/approvals per tick against a
// route that answers an empty page in the ordinary case, and the benefit — a parked run being noticed
// within a few seconds instead of never — is not something an operator should have to opt into. A knob here
// would mostly be a way to turn the guarantee off.
const approvalSweepInterval = 15 * time.Second

// startApprovalSweep runs SweepApprovals on a ticker until ctx ends, and returns a channel that closes once
// the loop has stopped — so shutdown can wait for a sweep that is mid-post rather than exiting underneath
// one and leaving a claim taken for a message that never went out.
//
// THE FIRST PASS IS IMMEDIATE, before the first tick, because the case it exists for is already true at
// boot: a run parked while this process was down has been waiting since then, and making it wait one more
// interval is the one delay this whole change is meant to remove.
func startApprovalSweep(ctx context.Context, deps relay.SweepApprovalDeps) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(approvalSweepInterval)
		defer ticker.Stop()
		for {
			if err := relay.SweepApprovals(ctx, deps); err != nil && ctx.Err() == nil {
				// Logged, never fatal: this is a background repair, and a control plane that is briefly
				// unreachable must not take the bot's live path down with it.
				log.Printf("slack-bot: the approval sweep could not run this time (a parked run may still be waiting to be asked about): %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
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
