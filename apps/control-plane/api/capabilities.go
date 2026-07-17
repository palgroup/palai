package api

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// capabilitiesBody is the discovery surface GET /v1/capabilities publishes: the LP-0
// maturity and isolation posture, the configured store:false retention TTL, and the
// per-resource capability matrix. Clients (and `palai local doctor`) read it to learn
// what this deployment supports without probing each route (spec §20.9 configured
// retention through discovery; LP plan §2 maturity declaration).
type capabilitiesBody struct {
	Object       string            `json:"object"`
	Maturity     string            `json:"maturity"`
	Isolation    string            `json:"isolation"`
	Retention    retentionBody     `json:"retention"`
	Capabilities map[string]string `json:"capabilities"`
}

type retentionBody struct {
	StoreFalseTTLSeconds int `json:"store_false_ttl_seconds"`
}

// capabilities serves the discovery body. It reads the configured retention TTL from the
// same PALAI_RETENTION_STORE_FALSE_TTL the reaper honors — a single source of truth, so
// discovery never advertises a TTL the reaper is not enforcing (unset ⇒ 0 ⇒ disabled).
func capabilities(w http.ResponseWriter, r *http.Request) {
	if _, ok := middleware.ScopeFrom(r.Context()); !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	body := capabilitiesBody{
		Object:    "capabilities",
		Maturity:  "preview",
		Isolation: "development",
		Retention: retentionBody{StoreFalseTTLSeconds: int(configuredRetentionTTL().Seconds())},
		Capabilities: map[string]string{
			"responses":  "preview",
			"sessions":   "unavailable",
			"workspaces": "unavailable",
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

// configuredRetentionTTL parses the reaper's TTL env var, returning 0 (disabled) when
// unset or unparseable — the same resolution main.startRetention applies.
func configuredRetentionTTL() time.Duration {
	d, err := time.ParseDuration(os.Getenv("PALAI_RETENTION_STORE_FALSE_TTL"))
	if err != nil || d < 0 {
		return 0
	}
	return d
}
