package extensions

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/evals"
	"github.com/palgroup/palai/tests/uat"
)

// The extensions-0.1.0 evidence bundle is GENERATED, not hand-maintained. It carries 39 entries (the 38 UAT
// cases the epic owns plus the release-level tier anchor); hand-editing that would drift the moment a case's
// proof list changed. Every field is DERIVED from a canonical source in the tree:
//
//	proof_class + db_assertions  <- expectedExtensionsCatalog (so a bundle assertion cannot describe a proof
//	                                the catalog gate does not resolve)
//	eval_gate_proof numbers      <- a REAL evals.RunAll held-out run under the shipped SafePolicy engine
//	capability_tier_proof tiers   <- uat.RecomputeCapabilityTiers over these very cases' outcomes
//	checksum                     <- hashParts(id, run_id, uat.CapabilityClaimsDigest())
//
// TestCommittedExtensionsBundleIsTheGeneratorOutput asserts the committed file EQUALS this generator byte for
// byte, so the bundle can never drift from the tree, and the gate below verifies it through the SHIPPED
// verifier. Regenerate with: PALAI_WRITE_EXTENSIONS_BUNDLE=1 go test ./tests/uat/extensions/
const extensionsRelease = "extensions-0.1.0"

// syntheticImageDigest is an obviously-unservable digest (the sdk-provider-parity-0.1.0 precedent). The E17
// journeys are COMPONENT-tier: they drive real PostgreSQL, not an engine container, so there is no real engine
// image to name. The shape verifier requires the field for a non-external-receipt case, so it carries a value
// that could never be a real registry digest — and every case's db_assertions says so explicitly rather than
// borrowing another release's real digest and implying a container run that did not happen.
var syntheticImageDigest = "sha256:" + strings.Repeat("e17", 21) + "e"

// hashParts reproduces the tests/uat construction (sha256 of each part followed by a NUL, hex, sha256:-prefixed)
// so this generator and the gate derive the same re-derivable values.
// ponytail: a 6-line copy, the same one the managed-cloud / self-host gates keep.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// tierAnchorCaseID is the release-level entry carrying the CapabilityTierProof. It deliberately has no
// tests/uat/cases directory: the tier table is not a behaviour one case asserts, it is the recomputation over
// ALL of them. The id is namespaced to E17 so it can never be mistaken for a UAT case.
const tierAnchorCaseID = "E17-TIER"

// axeReportSummary is the canonical axe-core outcome the console e2e records (UI-001): zero WCAG 2 A/AA
// violations on both surfaces. The ConsoleProof's axe_report_digest is sha256 of exactly this line, so it is
// re-derivable rather than an opaque number.
const axeReportSummary = "axe-core wcag2a,wcag2aa violations=0 surfaces=admin,live-run"

// consoleNetworkTrace is the request-path trace the playwright network intercept recorded: EVERY request the
// browser issued. uat.ConsoleProof.Complete() RECOMPUTES the off-/v1 count from this list, so a backchannel
// path here would fail the bundle.
var consoleNetworkTrace = []string{
	"/api/palai/v1/organizations",
	"/api/palai/v1/projects",
	"/api/palai/v1/api-keys",
	"/api/palai/v1/model-connections",
	"/api/palai/v1/responses",
	"/api/palai/v1/responses/resp_console/events",
	"/api/palai/v1/responses/resp_console/commands",
	"/api/palai/v1/artifacts/art_console/content",
}

// caseCeilings are per-case honesty clauses the generic assertion lines cannot express: what a case's proof
// does NOT reach. They are recorded IN the bundle (not only in a code comment) because the bundle is what a
// reader outside this repo sees, and the E17 T11 review found each of these implied more than it proved.
var caseCeilings = map[string][]string{
	"SLK-001": {
		// E19 T2 retired the "UNWIRED DECISION PATH" clause that stood here. It said the allow-list had no
		// non-test caller and no Slack HTTP route existed, so the journey hand-composed the inbound leg. Both
		// halves are now false: POST /v1/slack/interactions is mounted (api/slack_interactions.go) and it drives
		// SlackAuthorizationPolicyFor -> ApproverAuthorized -> AcceptCommand -> ApplyApprovalDecision in
		// production code (extensions/slack_decision.go), asserted end to end over real Postgres by
		// apps/control-plane/internal/store/slack_interactions_component_test.go — an unauthorized click leaves
		// ZERO commands. What remains unproven is stated below, and it is a different claim.
		// E19 T3 narrowed the clause below by one word. It used to end "no socket to slack.com exists in this
		// repo's tests", and that is no longer true: tests/live/slack/socket_live_test.go dials a REAL Socket
		// Mode connection — but only when SLACK_APP_TOKEN is present, and it SKIPS otherwise. The honest form is
		// therefore about what runs WITHOUT credentials, not about what exists.
		"NO REAL WORKSPACE: every Slack proof this case COUNTS runs against a FAKE peer — a fixture that signs the way the verification page documents, a local HTTP server standing in for slack.com/api, and a fake WSS server built from the Socket Mode page. The shipped routes (events + interactivity), the Socket Mode connect loop, and the decision chain are exercised end to end over real Postgres, but nothing here is a receipt from a real workspace: no test that runs WITHOUT credentials opens a socket to slack.com. That receipt is §6 leg 1 and is what caps `slack` at preview — and Socket Mode is the cheapest way to collect it, since it needs no public URL",
		"ONE SOCKET MODE CONNECTION, ONE WORKSPACE: the connect loop is proven with a SINGLE connection to the single workspace PALAI_SLACK_SOCKET_TEAM_ID names. Multi-workspace fan-out (N connections = N sockets) is NOT built — a listing query, per-connection supervision and add/remove on registration are connection-pool management, i.e. scale work. Two consequences a reader should not have to infer: a deployment serving several workspaces runs Socket Mode for at most one of them, and a workspace registered AFTER the control plane boots is not picked up until it restarts",
		"AN ACKNOWLEDGED ENVELOPE WHOSE ADMISSION THEN FAILS IS LOST, and this is an asymmetry with the HTTP transport rather than a shared property: the HTTP route acknowledges AFTER the reservation commits, so a failure there earns a Slack redelivery. Socket Mode publishes no acknowledgement budget at all (the 3-second figure is documented for the HTTP callback and for interactivity, not for an envelope), so the loop answers FIRST — which satisfies any budget that may exist, at the cost that Slack will not redeliver an envelope it has seen acknowledged. A failing admission is therefore logged as a named loss and produces no run",
		"NO RESPONSE PAYLOAD IS EVER SENT ON AN ACKNOWLEDGEMENT: accepts_response_payload is decoded and slack.SocketAck refuses a payload on an envelope declaring false — the documented rule, enforced as a typed error rather than a silent drop — but nothing in-process has anything to say back inside an acknowledgement (an admitted run answers later, in the thread). The accepting branch is therefore exercised by the rule's own unit test, not by a real sender",
		"THE APPROVAL MESSAGE IS NOT POSTED BY ANYTHING IN-PROCESS: the decision path REPAIRS the message a click arrived on (chat.update, bot token from bot_token_ref, per-channel Special-Tier pacing, one bounded 429 repair) and records its ts as the thread's repair handle. What posts an approval message with its two minted buttons in the first place is the credential-gated live leg (tests/live/slack) and, later, whatever composes approval UX — there is no in-process worker that fans approval messages out. Block Kit construction here is deliberately minimal (two buttons + the authoritative detail); a rich approval UI is the console's job. Enterprise Grid / org-wide install is out of scope: the interactivity route never resolves an enterprise id",
		"NO FEEDBACK TO AN UNAUTHORIZED CLICKER: a refused decision answers Slack 200 with nothing done and is recorded control-plane-side only, so the clicking user sees no reply at all. Telling them would need an ephemeral message (response_url, whose 30-minute/5-use window is the wrong tool for approval repair, or chat.postEphemeral); that is deliberately not built",
		"TERMINAL-SUMMARY MEASUREMENT: the proof's terminal_summary_posts counts the terminal SURFACE posts (exactly one repaired chat.update, never duplicated). The §63.3 fan-out form — one terminal summary per delivery id, with two canonical deliveries — is NOT asserted: there is no shipped Slack outbound worker to fan out, so a second post would be fixture theatre rather than evidence",
		"JOURNEY STEP 8 IS COMMAND-ACCEPTANCE: the change_config route switch is asserted DURABLE (the row reads back queued on the session, carrying the new model), not APPLIED — applying a boundary config change needs a running engine, which this journey does not start, so that is an E08 execution-tier proof (apps/control-plane/internal/execution). Step 9's cancellation IS asserted to a real terminal: the §9.2 interrupt-delivery message reads back queued for the pump AND the run is driven to `canceled` through the production CancelRunReconciled path (§26.10/SES-010), with the run row re-read. A DEFECT this strengthening caught: the old step 9 posted Kind:\"interrupt\", which is not a command kind at all, so the coordinator durably REJECTED it on every run while the old assertion (\"AcceptCommand returned no error\") passed",
	},
	"UI-001": {
		"FAKE UPSTREAM, MECHANICALLY LABELLED: every console proof drives the built console against tests/fake-control-plane.mjs, never a real control plane (console_proof.upstream is structurally \"fake\"). E17 T10 itself proved a fake upstream CAN diverge from the real contract — its fixture had invented an approval event the real approval.requested.v1 does not carry — so green against a fake is not evidence about the real API. A DEPLOYED console against a real /v1 is §6 leg 8 and caps `console` at preview",
	},
	"A2A-003": {
		// E19 T4 added real push DELIVERY to this case. The word "spec-compliant" is deliberately absent:
		// the token header is our own choice, so calling the delivery spec-compliant would be the exact
		// overclaim the honest-ceiling discipline exists to stop.
		"TOKEN HEADER IS OUR CHOICE, NOT THE SPEC'S: the A2A specification defines PushNotificationConfig.token as a field but names NO HTTP header to carry it (https://a2a-protocol.org/latest/topics/streaming-and-async/, fetched 2026-07-26). What is delivered is an A2A StreamResponse payload with an APPLICATION-SPECIFIC token header (X-A2A-Notification-Token) — it is NOT \"spec-compliant push\", and a foreign peer expecting a different header simply will not see the token. Every delivery proof in this case ran against a LOOPBACK sink this repo wrote; a loopback sink is not interop, so a foreign peer accepting these deliveries remains §6 leg 2 and `a2a` stays preview",
		"D12 PARTIALLY CLOSED — OWNERSHIP VERIFICATION IS ABSENT: the contract names domain allowlisting, ownership verification AND an egress firewall. The allowlist (normalized whole-host equality) and the firewall (packages/egress + the pinned dialer, with redirects REVALIDATED per hop) are implemented and RED-proven; nothing verifies that the client registering a webhook target actually controls it. A deployment exposed to untrusted registrants must set PushPolicy.AllowedHosts — when it is empty the egress policy alone governs, and any public https host a client names will be POSTed to",
		"DELIVERY IS NOT DURABLE AND NOT OBSERVER-DRIVEN: the pending set is IN-PROCESS, so a crash between accepting a notification and delivering it drops that notification — the retry/dead-letter discipline is the webhook pump's, but the queue behind it is not. Delivery fires only on the state-producing operations the server itself serves (message:send, message:stream, tasks:subscribe, tasks:cancel); there is NO asynchronous observer of run terminality, so a run that reaches a terminal state with no A2A request in flight notifies nobody. Both gaps need durable per-target delivery state and therefore a migration, which E19 excludes by construction",
	},
	"AUT-009": {
		// The word "stable" is deliberately absent: the honest-ceiling guard forbids it in a preview
		// capability's evidence, so the unmet condition is named without borrowing the tier word.
		"NO BROKER PRODUCT: queue_delivery_proof.broker is \"postgres-durable-reference\" — one of the seams that actually ran. No NATS, SQS, Pub/Sub or Kafka exists anywhere in this tree, so the plan §T7 tier-promotion condition for `queues` (a real broker container, NATS JetStream) is UNMET and §6 leg 5 (EXTENDED to cover any broker PRODUCT) caps the capability at preview",
	},
}

// buildExtensionsManifest assembles the bundle. Everything is derived; nothing is typed twice.
func buildExtensionsManifest(t *testing.T) []byte {
	t.Helper()

	ids := make([]string, 0, len(expectedExtensionsCatalog))
	for id := range expectedExtensionsCatalog {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	claimsDigest := uat.CapabilityClaimsDigest()
	newCase := func(id string) map[string]any {
		runID := "run_e17_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		entry := expectedExtensionsCatalog[id]
		assertions := []string{
			fmt.Sprintf("%s: proven by %d in-tree proof(s) at the %s tier — %s",
				id, len(entry.proofs), entry.class, strings.Join(entry.proofs, "; ")),
			"the case text NAMES its local seam (fake/loopback/deterministic/live/component-real) AND its §6 operator leg; TestExtensionsCatalogMaterialized enforces both mechanically",
			"AUTHORED BUNDLE, component tier: the E17 journeys drive real PostgreSQL, NOT an engine container — image_digest/provider_request_id/mtls_enroll are the shape verifier's required receipt fields carrying deliberately unservable placeholder values, never a claim that a container ran",
		}
		assertions = append(assertions, caseCeilings[id]...)
		if id == tierAnchorCaseID {
			assertions = []string{
				"RELEASE-LEVEL entry, not a UAT case (it has no tests/uat/cases directory): a capability's maturity tier is not a behaviour one case asserts, it is the RECOMPUTATION over all of them",
				"the verifier RECOMPUTES every tier from the canonical code tables + this bundle's own per-case outcomes and refuses any declared tier or /v1/capabilities snapshot that disagrees — the manifest's own tier copy is never an input (plan §T11)",
				"PER-CASE STATUS IS AUTHORED DETERMINISTIC DATA, and what backs it is the CO-RUN in scripts/uat/extensions: the same invocation that verifies this bundle also runs the knowledge, worker and automation-queue component suites, the Slack/A2A package suites and the console playwright specs against the throwaway Postgres it stood up. A red backing suite fails the gate rather than being reported around it (proven by tests/uat/extensions TestARedBackingSuiteFailsTheGate); the shipped scripts/evidence/verify alone does NOT re-run them",
				"HONEST CEILING — four capabilities cap at PREVIEW for ONE reason, that the counterpart system was never contacted: slack (no real workspace, §6 leg 1), a2a (no foreign peer, §6 leg 2), queues (no broker PRODUCT was ever run — the plan §T7 NATS-JetStream-container condition is UNMET and the durable proof is the Postgres reference adapter, §6 leg 5 EXTENDED) and console (every console proof ran against a FAKE /v1 upstream, never a real control plane, §6 leg 8). knowledge-vector and apple-build are DISABLED because no vector store and no Apple signing material exist anywhere. Only knowledge and capability-workers close STABLE",
			}
		}
		return map[string]any{
			"id": id, "status": "PASS", "proof_class": entry.class,
			"run_id": runID, "image_digest": syntheticImageDigest,
			"provider_request_id": "prov_e17_deterministic", "mtls_enroll": "component-tier: no runner enrollment",
			"terminal":      map[string]any{"type": "response.completed", "count": 1},
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
			"db_assertions": assertions,
			"checksum":      hashParts(id, runID, claimsDigest),
		}
	}

	cases := make([]map[string]any, 0, len(ids)+1)
	outcomes := make(map[string]string, len(ids))
	for _, id := range ids {
		cases = append(cases, newCase(id))
		outcomes[id] = "PASS"
	}

	// ---- the six E17 area proofs, each attached to the case whose invariant it certifies ------------
	attach := func(id string, fields map[string]any) {
		for _, c := range cases {
			if c["id"] == id {
				for k, v := range fields {
					c[k] = v
				}
				return
			}
		}
		t.Fatalf("cannot attach a proof to %s: it is not in the bundle", id)
	}

	attach("SLK-001", map[string]any{
		"slack_mapping_claim": "one-canonical-session-one-effect-per-event-on-a-FAKE-peer",
		"slack_mapping_proof": uat.SlackMappingProof{
			Peer: "fake", TeamID: "T63300", SessionID: "ses_slack_journey",
			CanonicalSessions: 1,
			SourceEventIDs:    []string{"Ev63301", "Ev63306"},
			DeliveredEvents:   3, CanonicalEffects: 2,
			PostReceipts:                 []string{"901.000100", "902.000100"},
			TerminalSummaryPosts:         1, RateLimitRepairs: 1,
			UnauthorizedApprovalRejected: true, CanonicalResultIntact: true,
		},
	})

	transcript := []string{
		"client GET agent-card.json", "client POST message:send", "server 200 Task",
		"client GET tasks/{id}", "server 200 completed",
	}
	outcomesByEndpoint := make(map[string]string, len(uat.A2AEndpoints))
	for _, ep := range uat.A2AEndpoints {
		outcomesByEndpoint[ep] = "pass"
	}
	attach("A2A-002", map[string]any{
		"a2a_conformance_claim": "twelve-endpoint-matrix-green-over-a-LOOPBACK-exchange",
		"a2a_conformance_proof": uat.A2AConformanceProof{
			Endpoints: uat.A2AEndpoints, FixtureOutcomes: outcomesByEndpoint,
			Peer: "loopback", LoopbackTranscript: transcript,
			TranscriptDigest: hashParts(transcript...), CardLeakedInternalDetail: false,
		},
	})

	// The citation's chunk bytes ARE the fixture document; the verifier recomputes ChunkBytes[start:end].
	const chunkBytes = "Deployment runbook: the rollout gate blocks a release without a restore proof."
	attach("KNO-003", map[string]any{
		"knowledge_acl_claim": "acl-first-zero-unauthorized-results-with-re-derivable-citation-offsets",
		"knowledge_acl_proof": uat.KnowledgeACLProof{
			AuthorizedResults: 1, UnauthorizedResults: 0,
			RankingShiftedByUnauthorized: false, PostFilterTopK: false,
			Citations: []uat.KnowledgeCitation{{
				ChunkID: "kchk_journey_1", ChunkBytes: chunkBytes,
				StartOffset: 0, EndOffset: len(chunkBytes), Quote: chunkBytes,
			}},
			SourceDeletePropagated: true,
		},
	})

	// Broker is one of uat.QueueBrokerSeams — a seam that ACTUALLY RAN. No broker product exists in this tree,
	// which is why `queues` caps at preview (the plan §T7 NATS-container condition is unmet).
	attach("AUT-009", map[string]any{
		"queue_delivery_claim": "lost-ack-redelivery-single-effect-dead-letter-and-loss-less-outbound",
		"queue_delivery_proof": uat.QueueDeliveryProof{
			Broker: "postgres-durable-reference",
			DistinctMessages: 3, Consumed: 4, Redelivered: 1, CanonicalEffects: 3,
			DeadLettered: 1, Dropped: 0, OutboundDeliveredOnce: true,
		},
	})

	attach("WRK-003", map[string]any{
		"worker_fence_claim": "stale-fence-rejected-no-tunnel-refused-secret-handle-expired",
		"worker_fence_proof": uat.WorkerFenceProof{
			WorkerID: "wrk_journey_1", Capability: "swift-toolchain/swift.build-check",
			StaleFenceRejected:        true,
			NoTunnelRefusedOperations: []string{"tunnel.connect", "shell.exec", "socks.proxy"},
			TunnelSucceeded:           false,
			SecretHandleScope:         "job", SecretHandleExpired: true, SecretValueInJournal: false,
			AppleBuildAdvertised: false,
		},
	})

	// Upstream is structurally "fake": the playwright specs drive the built console against
	// tests/fake-control-plane.mjs, never a real control plane. That is why `console` caps at preview.
	attach("UI-001", map[string]any{
		"console_claim": "axe-clean-keyboard-operable-and-every-request-on-the-v1-relay-over-a-FAKE-upstream",
		"console_proof": uat.ConsoleProof{
			Upstream:      "fake",
			AxeViolations: 0, AxeReportDigest: digestOf(axeReportSummary),
			NetworkTrace: consoleNetworkTrace,
			KeyboardOperable: true, SkipLinkFirst: true,
			ApprovalDetailAuthoritative: true, APIKeyReachedBrowser: false,
		},
	})

	// ---- QUA-004's eval-gate proof: a REAL held-out run, not authored numbers ----------------------
	attach("QUA-004", map[string]any{
		"eval_gate_claim": "held-out-thresholds-met-gate-mechanics-only-not-model-quality",
		"eval_gate_proof": heldOutEvalProof(t),
	})

	// ---- the tier anchor: tiers RECOMPUTED from the outcomes above ---------------------------------
	outcomes[tierAnchorCaseID] = "PASS"
	recomputed := uat.RecomputeCapabilityTiers(outcomes)
	declarations := make([]uat.CapabilityTierDeclaration, 0, len(uat.CapabilityTierOrder))
	snapshot := make(map[string]string, len(uat.CapabilityTierOrder))
	for _, capability := range uat.CapabilityTierOrder {
		declarations = append(declarations, uat.CapabilityTierDeclaration{
			Capability: capability, DeclaredTier: recomputed[capability],
			ClaimCaseIDs: uat.CapabilityClaims[capability],
		})
		snapshot[capability] = recomputed[capability]
	}
	anchor := newCase(tierAnchorCaseID)
	anchor["proof_class"] = "unit"
	anchor["capability_tier_claim"] = "tiers-recomputed-from-per-case-outcomes"
	anchor["capability_tier_proof"] = uat.CapabilityTierProof{
		Capabilities: declarations, Snapshot: snapshot,
		SnapshotSource: "GET /v1/capabilities served by the real api.NewRouter (asserted bit-equal by apps/control-plane/api TestServedCapabilityTiersEqualTheRecompute)",
		ClaimsDigest:   uat.CapabilityClaimsDigest(),
	}
	cases = append(cases, anchor)

	manifest := map[string]any{
		"release":     extensionsRelease,
		"git_sha":     "211b69f",
		"api_version": "v1",
		"migration":   "000040_capability_workers",
		"captured_at": "2026-07-25T00:00:00Z",
		"maturity":    "rc",
		"cases":       cases,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return buf.Bytes()
}

// bundleDir is the committed release directory.
func bundleDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "evidence", "releases", extensionsRelease)
}

// TestCommittedExtensionsBundleIsTheGeneratorOutput pins the committed bundle to the tree: it must be BYTE-
// identical to the generator's output, so a proof-list change, a threshold change, or a tier change cannot
// leave a stale bundle verifying green. Set PALAI_WRITE_EXTENSIONS_BUNDLE=1 to regenerate.
func TestCommittedExtensionsBundleIsTheGeneratorOutput(t *testing.T) {
	want := buildExtensionsManifest(t)
	path := filepath.Join(bundleDir(t), "manifest.json")

	if os.Getenv("PALAI_WRITE_EXTENSIONS_BUNDLE") == "1" {
		if err := os.MkdirAll(bundleDir(t), 0o755); err != nil {
			t.Fatalf("create release dir: %v", err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(want))
		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the committed %s bundle: %v (regenerate with PALAI_WRITE_EXTENSIONS_BUNDLE=1)", extensionsRelease, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the committed %s manifest is NOT the generator's output (%d vs %d bytes) — it has drifted from the tree; regenerate with PALAI_WRITE_EXTENSIONS_BUNDLE=1",
			extensionsRelease, len(got), len(want))
	}
}

// TestExtensionsReleaseVerifiesClean is the deterministic mirror of `make evidence-verify
// RELEASE=extensions-0.1.0`: the committed bundle must verify clean (0 failed, 0 missing, 0 secret findings)
// with EVERY E17 rule ACTIVE on real release data — the fake-peer Slack mapping, the loopback A2A matrix, the
// ACL-first knowledge citations whose offsets the verifier re-derives, the queue redelivery counters, the
// worker fence/no-tunnel/secret-handle facts, the console axe + /v1-only trace, the held-out eval gate, and
// the capability tier recomputation. A bundle missing any rule would silently not test that invariant.
func TestExtensionsReleaseVerifiesClean(t *testing.T) {
	summary, err := uat.VerifyRelease(bundleDir(t), nil)
	if err != nil {
		t.Fatalf("verify %s: %v", extensionsRelease, err)
	}
	if !summary.OK() {
		t.Fatalf("%s did not verify clean: %s\n%v", extensionsRelease, summary.String(), summary.Findings)
	}
	if want := len(expectedExtensionsCatalog) + 1; summary.Passed != want {
		t.Fatalf("%s verified %d cases, want %d (the %d UAT cases + the tier anchor)",
			extensionsRelease, summary.Passed, want, len(expectedExtensionsCatalog))
	}

	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var parsed struct {
		Cases []map[string]json.RawMessage `json:"cases"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	for _, claim := range []string{
		"slack_mapping_claim", "a2a_conformance_claim", "knowledge_acl_claim",
		"queue_delivery_claim", "worker_fence_claim", "console_claim",
		"eval_gate_claim", "capability_tier_claim",
	} {
		proof := strings.TrimSuffix(claim, "_claim") + "_proof"
		exercised := false
		for _, c := range parsed.Cases {
			if len(c[claim]) > 0 && len(c[proof]) > 0 {
				exercised = true
				break
			}
		}
		if !exercised {
			t.Errorf("%s carries no case with %s + %s — that E17 rule is not exercised on real release data", extensionsRelease, claim, proof)
		}
	}

	// HONEST-CEILING GUARD (plan §T11): no PREVIEW or DISABLED capability's evidence may carry the word
	// "stable". A bundle that described slack's fake-peer proof in stable language would be the prose half of
	// the overclaim the tier recompute blocks mechanically. The word is matched on WORD BOUNDARIES so a test
	// name like TestMapEventRedeliveryIsIdentityStable is not a false hit, and the tier-anchor entry is
	// exempt: it IS the table, so it necessarily carries "stable" for the capabilities that earned it — and
	// every tier in it is recomputed, never authored.
	tiers, err := uat.CapabilityTiersFromBundle(raw)
	if err != nil {
		t.Fatalf("recompute tiers: %v", err)
	}
	stableWord := regexp.MustCompile(`(?i)\bstable\b`)
	for capability, tier := range tiers {
		if tier == "stable" {
			continue
		}
		for _, c := range parsed.Cases {
			var id string
			_ = json.Unmarshal(c["id"], &id)
			if id == tierAnchorCaseID {
				continue
			}
			body := string(mustMarshal(t, c))
			if strings.Contains(body, capability) && stableWord.MatchString(body) {
				t.Errorf("case %s mentions the %s capability (%s) AND the word \"stable\" — a preview/disabled capability's evidence must never carry it (plan §T11 honest ceiling)", id, capability, tier)
			}
		}
	}

	// The bundle must NOT claim any of the three things this epic cannot prove locally.
	forbidden := []string{"real Slack workspace", "foreign A2A peer", "signed Apple build", "signed macOS build"}
	lower := strings.ToLower(string(raw))
	for _, phrase := range forbidden {
		idx := strings.Index(lower, strings.ToLower(phrase))
		if idx < 0 {
			continue
		}
		// The phrase is allowed ONLY inside an explicit negation naming the §6 leg.
		window := lower[max(0, idx-160):min(len(lower), idx+160)]
		if !strings.Contains(window, "§6") && !strings.Contains(window, "not claimed") {
			t.Errorf("the bundle mentions %q outside an explicit §6/not-claimed negation — this release does not run against any of them", phrase)
		}
	}
}

// TestExtensionsPromoteGateRefusesAndPasses is the plan §T11 promote sentence: the committed bundle promotes to
// rc, a bundle WITHOUT the tier table is refused, a bundle whose QUA-003 eval security suite is not green is
// refused, and a promote BEYOND rc awaits the §6 operator legs (never auto-claimed).
func TestExtensionsPromoteGateRefusesAndPasses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) != 0 {
		t.Fatalf("the committed %s bundle must promote to rc, got %v", extensionsRelease, refusals)
	}
	if refusals := uat.PromoteGateFor(raw, "stable"); len(refusals) == 0 {
		t.Fatal("a promote BEYOND rc must be REFUSED without the §6 operator-leg attestation (it is never auto-claimed)")
	}

	// Without the tier table: refused.
	noTier := stripField(t, raw, "capability_tier_claim")
	if refusals := uat.PromoteGateFor(noTier, "rc"); len(refusals) == 0 {
		t.Fatal("a tag WITHOUT the capability tier table must be REFUSED (plan §T11, §7)")
	}

	// With QUA-003 red: refused, and the refusal must name the precondition.
	redSecurity := setCaseStatus(t, raw, uat.CapabilitySecurityPrecondition, "FAIL")
	refusals := uat.PromoteGateFor(redSecurity, "rc")
	if len(refusals) == 0 {
		t.Fatalf("a tag whose %s eval security suite is red must be REFUSED (plan §T11)", uat.CapabilitySecurityPrecondition)
	}
	joined := make([]string, 0, len(refusals))
	for _, r := range refusals {
		joined = append(joined, r.Detail)
	}
	if !strings.Contains(strings.Join(joined, " | "), uat.CapabilitySecurityPrecondition) {
		t.Errorf("the refusal must name %s as the outstanding precondition, got: %v", uat.CapabilitySecurityPrecondition, joined)
	}
}

// TestBundleEvalNumbersAreTheCanonicalHeldOutRun pins QUA-004's numbers to a REAL run: the committed
// eval_gate_proof must equal a fresh evals.RunAll over the canonical held-out fixtures. EvalPromoteGate
// already refuses a mismatch; this says so directly so a reader knows the numbers are not authored.
func TestBundleEvalNumbersAreTheCanonicalHeldOutRun(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var parsed struct {
		Cases []struct {
			ID            string             `json:"id"`
			EvalGateProof *uat.EvalGateProof `json:"eval_gate_proof"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	reports, err := evals.RunAll(filepath.Join("..", "..", "evals", "testdata"), evals.HeldOut, evals.SafePolicy)
	if err != nil {
		t.Fatalf("recompute the held-out suites: %v", err)
	}
	found := false
	for _, c := range parsed.Cases {
		if c.EvalGateProof == nil {
			continue
		}
		found = true
		for _, s := range c.EvalGateProof.Suites {
			rep, ok := reports[s.Suite]
			if !ok {
				t.Errorf("%s: suite %q is not in the recomputed reports", c.ID, s.Suite)
				continue
			}
			if s.DatasetDigest != rep.Digest || s.HeldOutScore != rep.Score || s.SecurityRegressions != rep.SecurityFailures {
				t.Errorf("%s suite %q: committed (%q, %.3f, %d) != recomputed (%q, %.3f, %d) — the bundle's eval numbers must be a REAL held-out run",
					c.ID, s.Suite, s.DatasetDigest, s.HeldOutScore, s.SecurityRegressions, rep.Digest, rep.Score, rep.SecurityFailures)
			}
		}
	}
	if !found {
		t.Fatal("the bundle carries no eval_gate_proof")
	}
}

// TestTheTwoUnlabelledSeamsCannotBeFabricated is MUST-FIX 2's mechanical half. The two proofs backing the
// capabilities that were CANDIDATES for stable are the only ones that had no structurally-required seam label,
// unlike SlackMappingProof.Peer=="fake" and A2AConformanceProof.Peer=="loopback":
//
//	ConsoleProof carried NO upstream field though every console proof ran against a FAKE /v1 upstream;
//	QueueDeliveryProof.Broker only had to be non-empty, so a bundle could write "AWS SQS us-east-1".
//
// Both now carry the Peer discipline: a value naming a seam that was never run fails Complete(), so a future
// real-stack run must CHANGE the value and that change is visible in the bundle.
func TestTheTwoUnlabelledSeamsCannotBeFabricated(t *testing.T) {
	console := uat.ConsoleProof{
		Upstream: "fake", AxeViolations: 0, AxeReportDigest: digestOf(axeReportSummary),
		NetworkTrace: consoleNetworkTrace, KeyboardOperable: true, SkipLinkFirst: true,
		ApprovalDetailAuthoritative: true, APIKeyReachedBrowser: false,
	}
	if !console.Complete() {
		t.Fatalf("baseline: the fake-upstream console proof must be complete, got %+v", console)
	}
	for _, upstream := range []string{"", "real", "local stack", "https://api.palai.example/v1"} {
		fabricated := console
		fabricated.Upstream = upstream
		if fabricated.Complete() {
			t.Errorf("a ConsoleProof claiming upstream %q was ACCEPTED — every console proof ran against a FAKE /v1 upstream, and E17 T10 itself proved a fake can diverge from the real contract, so the seam label must be structurally \"fake\"", upstream)
		}
	}

	queue := uat.QueueDeliveryProof{
		Broker: "postgres-durable-reference", DistinctMessages: 3, Consumed: 4, Redelivered: 1,
		CanonicalEffects: 3, DeadLettered: 1, Dropped: 0, OutboundDeliveredOnce: true,
	}
	if !queue.Complete() {
		t.Fatalf("baseline: the reference-adapter queue proof must be complete, got %+v", queue)
	}
	for _, broker := range []string{"", "AWS SQS us-east-1", "NATS JetStream", "Google Pub/Sub", "kafka"} {
		fabricated := queue
		fabricated.Broker = broker
		if fabricated.Complete() {
			t.Errorf("a QueueDeliveryProof naming broker %q was ACCEPTED — no broker PRODUCT was ever run in this tree (there is no NATS anywhere), so Broker must be one of the seams that actually ran", broker)
		}
	}
}

// stripField removes a claim field from every case, so a gate can be shown to refuse its absence.
func stripField(t *testing.T, raw []byte, field string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, c := range m["cases"].([]any) {
		delete(c.(map[string]any), field)
	}
	return mustMarshal(t, m)
}

// setCaseStatus rewrites one case's status, modelling a red claim in an otherwise clean bundle.
func setCaseStatus(t *testing.T, raw []byte, id, status string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, c := range m["cases"].([]any) {
		if c.(map[string]any)["id"] == id {
			c.(map[string]any)["status"] = status
		}
	}
	return mustMarshal(t, m)
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
