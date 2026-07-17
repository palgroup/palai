// Command palai-control-plane serves the LP-0 HTTP surface over the durable spine.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
)

func main() {
	ctx := context.Background()

	databaseURL := os.Getenv("PALAI_DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("PALAI_DATABASE_URL is required")
	}
	repo, err := store.Open(ctx, databaseURL)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		log.Fatalf("apply migration: %v", err)
	}

	addr := os.Getenv("PALAI_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewRouter(repo, repo, repo, sseConfigFromEnv()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("palai control-plane listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}

// sseConfigFromEnv reads the event-stream timers from the environment. Unset values
// stay zero and take production defaults in api.NewRouter; operators (and the e2e
// tier) shorten them without a rebuild.
func sseConfigFromEnv() api.SSEConfig {
	return api.SSEConfig{
		Heartbeat:    envDuration("PALAI_SSE_HEARTBEAT"),
		PollInterval: envDuration("PALAI_SSE_POLL_INTERVAL"),
		WriteTimeout: envDuration("PALAI_SSE_WRITE_TIMEOUT"),
		BatchLimit:   envInt("PALAI_SSE_BATCH_LIMIT"),
	}
}

func envDuration(name string) time.Duration {
	d, err := time.ParseDuration(os.Getenv(name))
	if err != nil {
		return 0
	}
	return d
}

func envInt(name string) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return 0
	}
	return n
}
