// Command slack-bot is a Palai bot process: one instance per registered row in the control plane's
// bot registry (POST/GET /v1/bots, apps/control-plane/api/bots.go). It reads FOUR environment
// variables and nothing else — PALAI_BOT_ID, PALAI_API_URL, PALAI_API_KEY, PALAI_BOT_DATABASE_URL —
// and fetches everything else about itself (its kind, the agent it speaks as, its Slack tokens'
// secret handles, its channels) from its own bot row at startup, via the SDK's Bots.Get. That is
// the point: an operator configures a bot in the admin panel, not in a file, and a Slack token is
// never this process's environment variable.
//
// This is the process SKELETON (2026-08-03 plan, Task 5): it loads its config, resolves its own bot
// row, refuses if that row is missing or disabled, logs what it is, and waits for a shutdown
// signal. The Socket Mode connection, the relay and the durable store are later tasks (6-10) — this
// file does not open PALAI_BOT_DATABASE_URL yet, it only carries it through config.Load.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	palai "github.com/palgroup/palai/sdks/go"

	"github.com/palgroup/palai/apps/slack-bot/internal/config"
)

func main() {
	log.SetFlags(0)

	// Signal handling starts before anything that can block, so an operator's Ctrl-C or a
	// container's SIGTERM interrupts a slow bot-row fetch too, not just the idle wait at the end.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("slack-bot: %v", err)
	}

	client, err := palai.New(palai.WithBaseURL(cfg.PalaiBaseURL), palai.WithAPIKey(cfg.PalaiAPIKey))
	if err != nil {
		log.Fatalf("slack-bot: construct SDK client: %v", err)
	}

	bot, err := client.Bots.Get(ctx, cfg.BotID)
	if err != nil {
		var apiErr *palai.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			log.Fatalf("slack-bot: PALAI_BOT_ID=%s has no row in the bot registry — register it first", cfg.BotID)
		}
		log.Fatalf("slack-bot: fetch bot row PALAI_BOT_ID=%s: %v", cfg.BotID, err)
	}
	// A disabled row is a refusal, not a warning: starting anyway would relay messages for a bot
	// an operator deliberately turned off.
	if bot.Disabled {
		log.Fatalf("slack-bot: bot %s (%q) is disabled in the registry — enable it in the admin panel before starting this process", bot.ID, bot.Name)
	}

	log.Printf("slack-bot: running as %s %q (kind=%s, agent_revision_id=%s, repository_binding_id=%s)",
		bot.ID, bot.Name, bot.Kind, bot.AgentRevisionID, bot.RepositoryBindingID)

	<-ctx.Done()
	log.Printf("slack-bot: shutting down")
}
