// Command palai-control-plane serves the LP-0 HTTP surface over the durable spine.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
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
		Handler:           api.NewRouter(repo, repo),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("palai control-plane listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
