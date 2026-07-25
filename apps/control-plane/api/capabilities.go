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
			// outbox, proven by the Postgres-durable REFERENCE adapter. It stays PREVIEW and the T11 exit gate
			// KEPT it there: AUT-009/010 are green, but NO broker PRODUCT was ever run — there is no NATS, SQS,
			// Pub/Sub or Kafka anywhere in this tree, so the T7 plan's own stable-candidacy condition (a real
			// broker container, NATS JetStream) is UNMET. §6 leg 5, EXTENDED to cover any broker product, is the
			// operator work that flips it (uat.CapabilityOperatorLegs caps it mechanically). The unwritten cloud
			// adapters are separately NOT advertised here: an unwritten adapter is never discoverable.
			"queues": "preview",
			// apple-build stays DISABLED: there is no signing certificate, provisioning profile or store
			// credential anywhere (§6 leg 3), so discovery never claims a macOS/iOS BUILD this deployment
			// cannot serve — WRK-006 proves the capability is ABSENT from the worker catalog rather than
			// merely unadvertised.
			"apple-build": "disabled",
			// The open-core console (E17 T10): the public-API-only admin + live-run surface (apps/web-console).
			// It stays PREVIEW and the T11 exit gate KEPT it there: UI-001/UI-002 are green (axe-clean,
			// keyboard-operable, the authoritative approval detail, a network trace entirely on the /v1 relay)
			// but EVERY one of those proofs ran against a FAKE /v1 upstream, never a real control plane — and
			// E17 T10 itself proved a fake upstream can DIVERGE from the real contract (its fixture had invented
			// an approval event the real approval.requested.v1 does not carry). Green against a fake is not
			// evidence about the real API, so §6 leg 8 (a DEPLOYED console against a real /v1, plus the manual
			// VoiceOver/screen-reader pass) caps it — the same class of ceiling that keeps `slack` at preview.
			"console": "preview",
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
		// The CapabilityWorker contract (E17 T9): the outbound-enrolled, lease/fenced typed-operation surface
		// for out-of-process capability jobs. Advertised ONLY when this binary actually BOUND the gateway
		// (WithCapabilityWorkers, set from main.startCapabilityWorkerGateway) — until E19 T8a it was a static
		// "stable", the heaviest form of the §2 discovery lie: the control-plane binary did not so much as
		// import internal/workers, so every deployment claimed at the STRONGEST tier a contract that answered
		// nowhere. The tier below is the T11 recompute over the WRK-001..007 outcomes ("the CONTRACT is what
		// is stable") and mounting does NOT raise it — the mount only makes the claim advertisABLE.
		if cfg.capabilityWorkers {
			caps["capability-workers"] = "stable"
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
