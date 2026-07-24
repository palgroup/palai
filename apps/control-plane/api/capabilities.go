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
			"responses": "preview",
			"sessions":  "unavailable",
			// Coding workspaces are reachable end to end (a session attaches a repository binding, the root
			// run auto-provisions — E09 T10) only where the deployment configured a host allocation root, so
			// discovery derives it from PALAI_WORKSPACE_ROOT the same way it reads the retention TTL — a
			// deployment with no coding stack never advertises a capability it cannot serve.
			"workspaces": workspacesCapability(),
			// Knowledge spine (E17 Task 4): the FTS ingestion/index/retrieval core. It enters as "preview"
			// like every new capability — no task writes its own "stable"; the E17 exit gate (T11)
			// RECOMPUTES the tier from the KNO claim outcomes and flips it to stable only if all are green
			// (CapabilityTierProof). The vector strategy is a defined-but-disabled adapter (pgvector not
			// wired — §6 operator leg), so it is advertised disabled and never claims a store it lacks.
			"knowledge":        "preview",
			"knowledge-vector": "disabled",
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

// workspacesCapability reports whether this deployment can serve coding workspaces: "available" when
// PALAI_WORKSPACE_ROOT is configured (the same knob main.go gates SetWorkspaceProvisioner on), else
// "unavailable" — so a control plane with no coding stack does not advertise workspaces it cannot mount.
func workspacesCapability() string {
	if os.Getenv("PALAI_WORKSPACE_ROOT") != "" {
		return "available"
	}
	return "unavailable"
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
