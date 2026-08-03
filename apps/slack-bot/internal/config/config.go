// Package config loads the slack-bot process's environment. It is deliberately small: the process
// reads FOUR variables and nothing else. Everything a bot needs beyond these — its kind, the agent
// it speaks as, its Slack tokens' secret handles, its channels — comes from its own row in the
// control plane's bot registry (fetched by main.go via the SDK's Bots.Get), not from the
// environment. A Slack token is never a variable of this process: that is the whole point of the
// registry existing (2026-08-03 plan, Task 5).
package config

import (
	"fmt"
	"net/url"
	"os"
)

// Config is the slack-bot process's environment.
type Config struct {
	// BotID is this process's own row in the control plane's bot registry (PALAI_BOT_ID). A bot
	// with no registry row has nothing to be — there is no default.
	BotID string
	// PalaiBaseURL is the control plane's base URL (PALAI_API_URL), the SDK's WithBaseURL.
	PalaiBaseURL string
	// PalaiAPIKey authenticates every SDK call this process makes (PALAI_API_KEY).
	PalaiAPIKey string
	// DatabaseURL is this bot's own durable store (PALAI_BOT_DATABASE_URL) — a later task's
	// concern (Tasks 6-10); carried here so this process reads its full environment in one place.
	DatabaseURL string
}

// Load reads the four variables this process accepts, in the order a caller would fix them: a
// missing PALAI_BOT_ID is refused before PALAI_API_URL is even looked at, so the first error a
// misconfigured deployment sees is always the first thing actually wrong, not whichever variable
// happened to be checked last. Every refusal names the variable that is wrong.
func Load() (Config, error) {
	botID, err := requireEnv("PALAI_BOT_ID")
	if err != nil {
		return Config{}, err
	}
	baseURL, err := requireEnv("PALAI_API_URL")
	if err != nil {
		return Config{}, err
	}
	apiKey, err := requireEnv("PALAI_API_KEY")
	if err != nil {
		return Config{}, err
	}
	databaseURL, err := requireEnv("PALAI_BOT_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	if err := validatePostgresURL(databaseURL); err != nil {
		return Config{}, err
	}
	return Config{
		BotID:        botID,
		PalaiBaseURL: baseURL,
		PalaiAPIKey:  apiKey,
		DatabaseURL:  databaseURL,
	}, nil
}

// requireEnv reads name, refusing by NAME when it is unset or empty — a bare "configuration error"
// stopping the process is a support ticket, not a diagnosis.
func requireEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return v, nil
}

// validatePostgresURL checks that PALAI_BOT_DATABASE_URL is at least URL-shaped with a postgres
// scheme, so a fat-fingered value is refused here rather than several tasks later when the store
// (Task 8) first dials it, from a different component. It parses and checks the scheme only —
// opening a connection stays Task 8's job, not this skeleton's.
func validatePostgresURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("PALAI_BOT_DATABASE_URL is not a valid URL: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fmt.Errorf("PALAI_BOT_DATABASE_URL must be a postgres:// or postgresql:// URL, got %q", raw)
	}
	return nil
}
