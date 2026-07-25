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
// discovery never advertises a TTL the reaper is not enforcing (unset ⇒ 0 ⇒ disabled). It
// closes over the router config so a capability whose backing surface is optional (a2a) is
// advertised ONLY when that surface is actually mounted (§2: discovery never claims what the
// deployment cannot serve — the workspacesCapability posture).
func capabilities(cfg routerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := middleware.ScopeFrom(r.Context()); !ok {
			middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
			return
		}
		caps := map[string]string{
			"responses": "preview",
			"sessions":  "unavailable",
			// Coding workspaces are reachable end to end (a session attaches a repository binding, the root
			// run auto-provisions — E09 T10) only where the deployment configured a host allocation root, so
			// discovery derives it from PALAI_WORKSPACE_ROOT the same way it reads the retention TTL — a
			// deployment with no coding stack never advertises a capability it cannot serve.
			"workspaces": workspacesCapability(),
			// The Slack integration (E17 T1) is PREVIEW and the T11 exit gate KEPT it there: every SLK-001..008
			// claim is green, but the stable flip awaits §6 leg 1 — an external receipt from a REAL Slack
			// workspace. The local proof is a FAKE peer, so preview is the honest tier, and no amount of local
			// evidence moves it (uat.CapabilityOperatorLegs caps it mechanically).
			"slack": "preview",
			// Knowledge spine (E17 Task 4): the FTS ingestion/index/retrieval core. FLIPPED to stable by the
			// T11 exit gate — all eight KNO claims closed green and the capability has no §6 leg its core
			// depends on. The flip came THROUGH the verification: the tier below must equal
			// uat.RecomputeCapabilityTiers over the extensions-0.1.0 per-case outcomes, asserted by
			// TestServedCapabilityTiersEqualTheRecompute. The vector strategy stays DISABLED — the adapter
			// interface exists but no vector store is wired (§6 leg 4), and discovery never claims a store
			// it lacks.
			"knowledge":        "stable",
			"knowledge-vector": "disabled",
			// The queue adapter (E17 T7): a durable SQS/PubSub/Kafka-class consumer + outbound result-delivery
			// outbox, proven by the Postgres-durable REFERENCE adapter. FLIPPED to stable by the T11 exit gate
			// (AUT-009/010 green on a real durable adapter). §6 leg 5 EXTENDS this capability with real
			// SQS/PubSub brokers rather than substituting for it, which is why it does not cap the tier — but
			// those unwritten adapters are still deliberately NOT advertised here: an unwritten adapter is
			// never discoverable.
			"queues": "stable",
			// The CapabilityWorker contract (E17 T9): the outbound-enrolled, lease/fenced typed-operation
			// surface for out-of-process capability jobs. FLIPPED to stable by the T11 exit gate (WRK-001..007
			// green — the CONTRACT is what is stable). apple-build stays DISABLED: there is no signing
			// certificate, provisioning profile or store credential anywhere (§6 leg 3), so discovery never
			// claims a macOS/iOS BUILD this deployment cannot serve — WRK-006 proves the capability is ABSENT
			// from the worker catalog rather than merely unadvertised.
			"capability-workers": "stable",
			"apple-build":        "disabled",
			// The open-core console (E17 T10): the public-API-only admin + live-run surface (apps/web-console).
			// FLIPPED to stable by the T11 exit gate (UI-001/UI-002 green: axe-clean, keyboard-operable, the
			// authoritative approval detail, and a network trace entirely on the /v1 relay). §6 leg 8 (a manual
			// VoiceOver/screen-reader pass over a DEPLOYED console) sits ABOVE the automated accessibility
			// ceiling — it extends the evidence rather than substituting for it, so it does not cap the tier;
			// UI-001's case text states that ceiling explicitly.
			"console": "stable",
		}
		// The A2A 1.0 server projection (E17 T2): advertised ONLY when WithA2A actually mounted the surface,
		// so a binary that wires no A2A store does not claim `a2a` while every A2A route 404s. When mounted it
		// enters as "preview" and NEVER writes its own tier — the T11 exit gate recomputes it from the A2A
		// claim outcomes (CapabilityTierProof). The local proof is a fake/loopback generic client; a FOREIGN
		// A2A peer is the §6 operator leg (loopback != interop), and JWS/JCS card signing is a v0-OUT
		// hardening item — neither is claimed here.
		if cfg.a2a != nil {
			caps["a2a"] = "preview"
		}
		body := capabilitiesBody{
			Object:       "capabilities",
			Maturity:     "preview",
			Isolation:    "development",
			Retention:    retentionBody{StoreFalseTTLSeconds: int(configuredRetentionTTL().Seconds())},
			Capabilities: caps,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(body)
	}
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
