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
			// knowledge-vector stays DISABLED: the adapter interface exists but no vector store is wired
			// (§6 leg 4), and discovery never claims a store it lacks. A "disabled" word is a NEGATIVE claim —
			// it names a surface this deployment does NOT serve — so unlike the tiers below it needs no mount
			// to derive from and is honest exactly because it is static.
			"knowledge-vector": "disabled",
			// apple-build stays DISABLED, and the reason is STRUCTURAL rather than an inventory: no
			// apple-build operation is typed in workers.Catalog and KnownCapability("apple-build") is false,
			// so an operation absent from the catalog is refused at dispatch, at claim and at submit —
			// WRK-006 proves the capability is ABSENT rather than merely unadvertised, and
			// uat.CodeAndShipProof re-derives it from the catalog's own SOURCE rather than trusting this
			// comment (§6 leg 3).
			//
			// D7, CORRECTED BY E23 T7: this comment used to read "there is no signing certificate,
			// provisioning profile or store credential anywhere" — the same false sentence E22 T7 corrected
			// in workers/types.go and left standing in two other files, of which this one is the MOST READ,
			// because it is the discovery surface. Measured on the development machine 2026-07-28:
			// `security find-identity -v -p codesigning` reports FOUR valid identities and five provisioning
			// profiles are installed. The old claim was an inventory claim about one machine, and
			// inventories change; the claim above cannot be falsified by a keychain.
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
		// The Slack integration (E17 T1 adapter + E19 T1 Events API route): advertised ONLY where WithSlack
		// actually mounted the receiver, so a binary that serves no Slack surface does not claim `slack` while
		// POST /v1/slack/events 404s. Until E19 T1 this was a static string — the same §2 defect D14 named for
		// capability-workers, one tier quieter.
		//
		// It enters as PREVIEW and the wiring does NOT move it: every SLK claim is green, but the stable flip
		// awaits §6 leg 1 — an external receipt from a REAL Slack workspace. The local proof is a FAKE peer
		// built to the published contract, and uat.CapabilityOperatorLegs caps the tier mechanically, so the
		// T11 CapabilityTierProof recompute — not this line — decides the word.
		if cfg.slack != nil {
			caps["slack"] = "preview"
		}
		// The knowledge spine (E17 T4): the FTS ingestion/index/retrieval core, advertised ONLY when
		// WithKnowledge actually mounted the routes. Until E19 T8 this was a static "stable" — the SAME shape
		// D14 named for capability-workers and at the SAME strongest tier: a router built without WithKnowledge
		// 404s every /v1/knowledge-bases route while discovery told a client the contract was stable. Production
		// mounts it unconditionally (main.go), so no deployment's answer changes; what changes is that the
		// claim can no longer outlive the mount through the exported NewRouter.
		//
		// The word is the T11 recompute's, not this line's: all eight KNO claims closed green and the
		// capability has no §6 leg its core depends on, which is what flipped it to stable — asserted bit-equal
		// by TestServedCapabilityTiersEqualTheRecompute. Mounting does not raise it and never could.
		if cfg.knowledge != nil {
			caps["knowledge"] = "stable"
		}
		// The queue adapter (E17 T7 store + E19 T6 bridges): a durable SQS/PubSub/Kafka-class consumer +
		// outbound result-delivery outbox, proven by the Postgres-durable REFERENCE adapter. Advertised ONLY
		// when WithQueueConnections mounted the admin surface — the same static-string defect as `knowledge`,
		// one tier quieter, and E19 T6 is what made a mount exist to derive from at all.
		//
		// It stays PREVIEW and the wiring does NOT move it: AUT-009/010 are green, but NO broker PRODUCT was
		// ever run — there is no NATS, SQS, Pub/Sub or Kafka anywhere in this tree, so the T7 plan's own
		// stable-candidacy condition is UNMET. §6 leg 5 (EXTENDED to any broker product) is the operator work
		// that flips it, and uat.CapabilityOperatorLegs caps it mechanically. The unwritten cloud adapters are
		// separately NOT advertised: an unwritten adapter is never discoverable.
		if cfg.queues != nil {
			caps["queues"] = "preview"
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
