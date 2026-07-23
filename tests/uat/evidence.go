// Package uat holds the local-live UAT case runner and the evidence verifier. The verifier
// (this file) is Docker-free pure logic so it rides make verify; the case runner
// (local_live_test.go) is behind the `uat` build tag and drives the real stack.
package uat

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/palgroup/palai/packages/coordinator/recovery"
	"github.com/palgroup/palai/tests/uat/dr"
)

// Finding is one reason an evidence bundle fails verification. Case is "" for a
// release-level finding.
type Finding struct {
	Case   string
	Kind   string // missing | invalid | secret
	Detail string
}

func (f Finding) String() string {
	if f.Case == "" {
		return fmt.Sprintf("[%s] %s", f.Kind, f.Detail)
	}
	return fmt.Sprintf("%s [%s] %s", f.Case, f.Kind, f.Detail)
}

// Summary is the verified state of one release bundle — the numbers make evidence-verify.
type Summary struct {
	Passed         int
	Failed         int
	Missing        int
	SecretFindings int
	Findings       []Finding
}

// OK reports a clean bundle: every case passed with no missing field and no leaked secret.
func (s Summary) OK() bool { return s.Failed == 0 && s.Missing == 0 && s.SecretFindings == 0 }

// String renders the operator summary line evidence-verify prints.
func (s Summary) String() string {
	return fmt.Sprintf("%d passed, %d failed, %d missing, %d secret findings",
		s.Passed, s.Failed, s.Missing, s.SecretFindings)
}

// evidenceManifest mirrors protocols/schemas/evidence/manifest.json. Missing required
// fields decode to the zero value, which the verifier reports rather than tolerating.
type evidenceManifest struct {
	Release    string `json:"release"`
	GitSHA     string `json:"git_sha"`
	APIVersion string `json:"api_version"`
	Migration  string `json:"migration"`
	CapturedAt string `json:"captured_at"`
	// Maturity is the release stage (e.g. "rc"); OperatorAttestation is the E14 §6 operator-leg note a
	// beyond-rc promote requires. Both are optional metadata VerifyManifest ignores; PromoteGate reads them.
	Maturity            string          `json:"maturity"`
	OperatorAttestation json.RawMessage `json:"operator_attestation"`
	Cases               []evidenceCase  `json:"cases"`
}

type evidenceCase struct {
	ID                string         `json:"id"`
	Status            string         `json:"status"`
	ProofClass        string         `json:"proof_class"`
	RunID             string         `json:"run_id"`
	ImageDigest       string         `json:"image_digest"`
	ProviderRequestID string         `json:"provider_request_id"`
	MTLSEnroll        string         `json:"mtls_enroll"`
	ExternalReceipt   string         `json:"external_receipt"`
	Terminal          evidenceTerm   `json:"terminal"`
	Usage             map[string]int `json:"usage"`
	DBAssertions      []string       `json:"db_assertions"`
	Checksum          string         `json:"checksum"`
	// RecoveryClaim is a non-empty "continued"/"resumed" marker when the case claims its run survived a
	// kill/pause and was recovered (REC-006, spec §26.12). RecoveryProof is the §26.12 evidence that
	// claim requires — a marker alone is NEVER proof.
	RecoveryClaim string                  `json:"recovery_claim"`
	RecoveryProof *recovery.RecoveryProof `json:"recovery_proof"`
	// The E11 automation claims (spec §20-21, §33) extend the RecoveryProof discipline — a marker alone is
	// NEVER proof — to the three automation invariants: a duplicated event produced a single linked action
	// (DedupeClaim), a scheduler fired a single canonical occurrence (OccurrenceClaim), and a callback was
	// delivered exactly once without disturbing the run terminal (CallbackClaim). Each requires its proof.
	DedupeClaim     string           `json:"dedupe_claim"`
	DedupeProof     *DedupeProof     `json:"dedupe_proof"`
	OccurrenceClaim string           `json:"occurrence_claim"`
	OccurrenceProof *OccurrenceProof `json:"occurrence_proof"`
	CallbackClaim   string           `json:"callback_claim"`
	CallbackProof   *CallbackProof   `json:"callback_proof"`
	// The E12 extensibility claims (spec §28) extend the same marker-alone-is-NEVER-proof discipline to the
	// three invariants this epic owns: the run's effective tool set was ADVERTISED to the provider
	// (AdvertisingClaim), an enabled skill rode the run pinned by digest + scan with NO authority
	// (SkillClaim), and an extension crash was ISOLATED — breaker + tool_unavailable, core stayed up, another
	// run flowed (CrashIsolationClaim, the EXT-005 exit gate). The remote-tool async callback reuses the
	// existing CallbackClaim/CallbackProof (a signed one-use callback fits its fields). Each requires proof.
	AdvertisingClaim    string               `json:"advertising_claim"`
	AdvertisingProof    *AdvertisingProof    `json:"advertising_proof"`
	SkillClaim          string               `json:"skill_claim"`
	SkillProof          *SkillProof          `json:"skill_proof"`
	CrashIsolationClaim string               `json:"crash_isolation_claim"`
	CrashIsolationProof *CrashIsolationProof `json:"crash_isolation_proof"`
	// The E13 managed-cloud claims (plan §T11, MCI-001..008) extend the same marker-alone-is-NEVER-proof
	// discipline to the eight invariants the managed-cloud EXIT journey owns, ONE per MCI case: a second
	// tenant was PROVISIONED via the API on the same restart-less process with its config_policy applied
	// (ProvisioningClaim, MCI-001 — also the journey's restart-less spine); a secret-ref was written and
	// RESOLVED by a run with no restart and the value never surfaced (SecretResolveClaim, MCI-002); a
	// cross-tenant read was DENIED — tenant B got a real 404/403 with zero rows for tenant A's resource
	// (IsolationClaim, MCI-003/004); an artifact DOWNLOADED with a re-derivable content digest that matched
	// the run's bytes (ArtifactClaim, MCI-004); an admission was REFUSED by a budget/rate limit before any
	// billable compute (RefusalClaim, MCI-005); two projects RESOLVED distinct model routes on one stack
	// (RouteClaim, MCI-006); a repository binding's connection_ref RESOLVED a binding-scoped credential
	// (BindingClaim, MCI-007); and a steer command DROVE the run through the SDK session surface
	// (SteerClaim, MCI-008). Each requires its proof — a "provisioned"/"isolated"/"refused" marker alone is
	// never evidence.
	ProvisioningClaim  string              `json:"provisioning_claim"`
	ProvisioningProof  *ProvisioningProof  `json:"provisioning_proof"`
	SecretResolveClaim string              `json:"secret_resolve_claim"`
	SecretResolveProof *SecretResolveProof `json:"secret_resolve_proof"`
	IsolationClaim     string              `json:"isolation_claim"`
	IsolationProof     *IsolationProof     `json:"isolation_proof"`
	ArtifactClaim      string              `json:"artifact_claim"`
	ArtifactProof      *ArtifactProof      `json:"artifact_proof"`
	RefusalClaim       string              `json:"refusal_claim"`
	RefusalProof       *RefusalProof       `json:"refusal_proof"`
	RouteClaim         string              `json:"route_claim"`
	RouteProof         *RouteProof         `json:"route_proof"`
	BindingClaim       string              `json:"binding_claim"`
	BindingProof       *BindingProof       `json:"binding_proof"`
	SteerClaim         string              `json:"steer_claim"`
	SteerProof         *SteerProof         `json:"steer_proof"`
	// The E14 self-host claims (plan §T7, OPS-002 + DR-002 + DR-004..006 — the E14 EXIT gate) extend the same
	// marker-alone-is-NEVER-proof discipline to the self-host single-node install journey: a clean production
	// install came up hardened and resolved the restart-less install SPINE ending in a REAL provider run
	// (InstallClaim, OPS-002 — also the journey's restart-less spine); an installation backup restored into a
	// SEPARATE clean stack (BackupClaim, DR-002); and `restore verify` matched the manifest across all six
	// checks — checksum, migration, tenant-ids, run-retrieval, RLS isolation, secret canary (RestoreVerifyClaim,
	// DR-004..006). Each requires its proof — an "installed"/"restored"/"verified" marker alone is never evidence.
	InstallClaim       string              `json:"install_claim"`
	InstallProof       *InstallProof       `json:"install_proof"`
	BackupClaim        string              `json:"backup_claim"`
	BackupProof        *BackupProof        `json:"backup_proof"`
	RestoreVerifyClaim string              `json:"restore_verify_claim"`
	RestoreVerifyProof *RestoreVerifyProof `json:"restore_verify_proof"`
	// The E15 SH-2 RC claims (plan §T6, OPS-003..008 + DR-001 + SAN-011 — the E15 EXIT gate) extend the same
	// marker-alone-is-NEVER-proof discipline to the five upgrade/DR/air-gap/helm invariants: an active run was
	// DRAINED before the N->N+1 control-plane recreate and SURVIVED on its pinned engine to completion (the T2
	// MF-3 with-active-run drain), and the app + engine-alias rollbacks then ran the same drain-before-recreate
	// ordering (UpgradeClaim, OPS-005/007 + SAN-011 — the journey's spine); the migration chain
	// RESUMED after an interruption to the right journal head with no data corruption (MigrationJournalClaim,
	// OPS-006); a DR drill produced a MEASURED RPO/RTO the verifier recomputes from raw timestamps
	// (DrillClaim, DR-001 + DR-002/004..006 — the measurement anti-fabrication anchor); a signed air-gap
	// bundle re-verified OFFLINE and rejected a tamper (AirgapClaim, OPS-004); and the restricted Helm chart
	// RENDERED with zero ClusterRole + the restricted policy asserts (HelmRenderClaim, OPS-003). Each requires
	// its proof — an "upgraded"/"resumed"/"drilled"/"verified"/"rendered" marker alone is never evidence.
	UpgradeClaim          string                 `json:"upgrade_claim"`
	UpgradeProof          *UpgradeProof          `json:"upgrade_proof"`
	MigrationJournalClaim string                 `json:"migration_journal_claim"`
	MigrationJournalProof *MigrationJournalProof `json:"migration_journal_proof"`
	DrillClaim            string                 `json:"drill_claim"`
	DrillProof            *DrillProof            `json:"drill_proof"`
	AirgapClaim           string                 `json:"airgap_claim"`
	AirgapProof           *AirgapProof           `json:"airgap_proof"`
	HelmRenderClaim       string                 `json:"helm_render_claim"`
	HelmRenderProof       *HelmRenderProof       `json:"helm_render_proof"`
}

type evidenceTerm struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// DedupeProof is the evidence a dedupe_claim requires (spec §20.x, AUT-001): a duplicated event produced
// exactly ONE canonical action and the duplicate row links back to the original (original linkage). Unlike
// recovery.RecoveryProof, these three proof types have no orchestrator emitter — they are evidence-domain
// data assembled from the run's real DB rows — so they live here in tests/uat (deliberate).
type DedupeProof struct {
	OriginalDeliveryID   string `json:"original_delivery_id"`
	DuplicateDeliveryID  string `json:"duplicate_delivery_id"`
	CanonicalActionCount int    `json:"canonical_action_count"`
}

// Complete reports distinct original/duplicate ids (the linkage) and exactly one canonical action — a
// duplicated event that fanned out to two actions, or a duplicate that does not link a distinct original,
// is not proof.
func (p DedupeProof) Complete() bool {
	return p.OriginalDeliveryID != "" && p.DuplicateDeliveryID != "" &&
		p.OriginalDeliveryID != p.DuplicateDeliveryID && p.CanonicalActionCount == 1
}

// OccurrenceProof is the evidence an occurrence_claim requires (spec §33, AUT-007): competing scheduler
// replicas produced exactly ONE canonical occurrence, carrying its planned/admitted instants (lateness).
type OccurrenceProof struct {
	OccurrenceID   string `json:"occurrence_id"`
	PlannedAt      string `json:"planned_at"`
	AdmittedAt     string `json:"admitted_at"`
	CanonicalCount int    `json:"canonical_count"`
}

// Complete reports the occurrence carries its identity + both instants and a single canonical count — two
// replicas racing to two occurrence rows for the same (schedule,revision,planned_at) is not proof.
func (p OccurrenceProof) Complete() bool {
	return p.OccurrenceID != "" && p.PlannedAt != "" && p.AdmittedAt != "" && p.CanonicalCount == 1
}

// CallbackProof is the evidence a callback_claim requires (spec §21.x, AUT-011/013): a run-terminal
// callback was delivered exactly once (the receiver deduped a signed retry to a single semantic receipt)
// and the callback delivery did NOT disturb the run's terminal result.
type CallbackProof struct {
	DeliveryID           string `json:"delivery_id"`
	WebhookDeliveryID    string `json:"webhook_delivery_id"`
	Attempts             int    `json:"attempts"`
	ReceiverReceiptCount int    `json:"receiver_receipt_count"`
	RunTerminalIntact    bool   `json:"run_terminal_intact"`
}

// Complete reports the callback carries both ids, at least one delivery attempt, exactly one semantic
// receipt at the receiver, and a run terminal left intact — a callback counted twice, or one that mutated
// the run's terminal, is not proof.
func (p CallbackProof) Complete() bool {
	return p.DeliveryID != "" && p.WebhookDeliveryID != "" && p.Attempts >= 1 &&
		p.ReceiverReceiptCount == 1 && p.RunTerminalIntact
}

// AdvertisingProof is the evidence an advertising_claim requires (spec §28.5, EXT-001/002): the run's
// EFFECTIVE tool set was advertised to the provider — the schema list the provider request actually carried,
// hashed (AdvertisedSchemaHash), with the model-visible tool names. Mode records HOW the tool was selected:
// "spontaneous" (the model chose it with NO tool_choice forcing) or "forced" (a pre-advertising broker-seam
// forced call). A "forced" proof is HONESTLY named "forced" and is never described in spontaneous language —
// the manifest cannot overclaim spontaneity, an empty/other Mode fails the completeness gate.
type AdvertisingProof struct {
	AdvertisedSchemaHash string   `json:"advertised_schema_hash"`
	ToolNames            []string `json:"tool_names"`
	Mode                 string   `json:"mode"`
}

// Complete reports a hashed advertised schema list, at least one advertised tool name, and an honest
// selection mode ("spontaneous" or "forced"). An empty hash, no tool names, or an unnamed/other mode is not
// proof — a case that advertised nothing, or that hides whether the call was forced, does not pass.
func (p AdvertisingProof) Complete() bool {
	return p.AdvertisedSchemaHash != "" && len(p.ToolNames) >= 1 &&
		(p.Mode == "spontaneous" || p.Mode == "forced")
}

// SkillProof is the evidence a skill_claim requires (spec §28.15-28.16, TOL-011): an enabled skill rode the
// run pinned by an EXACT digest with a recorded quarantine scan result. A skill grants NO authority, so the
// load-bearing proof is the digest pin + scan outcome (never the skill body). A "loaded" marker with no
// digest, or a skill enabled without a scan result, is not proof.
type SkillProof struct {
	Digest     string `json:"digest"`
	ScanResult string `json:"scan_result"`
}

// Complete reports the skill carries both a non-empty pinned digest and a non-empty scan result — a skill
// that recorded no digest (so the run could drift to "latest") or no scan outcome is not proof.
func (p SkillProof) Complete() bool {
	return p.Digest != "" && p.ScanResult != ""
}

// CrashIsolationProof is the evidence a crash_isolation_claim requires (spec §28.21, EXT-005 — the E12 EXIT
// gate): an extension crash (an MCP server SIGKILL / a remote tool down / a hook worker down) tripped the
// per-connection circuit BREAKER, surfaced tool_unavailable VISIBLY to the run, left the control-plane
// process STABLE (it did not fall), and a SEPARATE run still FLOWED afterward. All four must hold — a crash
// that took the core down, or one the run never saw, is the opposite of isolation and is not proof.
type CrashIsolationProof struct {
	BreakerTripped         bool `json:"breaker_tripped"`
	ToolUnavailableVisible bool `json:"tool_unavailable_visible"`
	ControlPlaneStable     bool `json:"control_plane_stable"`
	OtherRunFlowed         bool `json:"other_run_flowed"`
}

// Complete reports all four isolation facts hold. A false on ANY of them — the breaker never tripped, the
// run never saw tool_unavailable, the control-plane fell, or no other run flowed — is not crash isolation,
// so the EXT-005 release gate cannot be marker-passed.
func (p CrashIsolationProof) Complete() bool {
	return p.BreakerTripped && p.ToolUnavailableVisible && p.ControlPlaneStable && p.OtherRunFlowed
}

// ManagedCloudStepIDs is the ordered restart-less SPINE the managed-cloud EXIT journey resolves on ONE
// process (plan §T11): provision a tenant over the public API (org, project, api-key), write its config_policy,
// run a REAL provider completion, steer it, list the run history, and deny the cross-tenant read. These are
// the steps ONE process actually resolves — NOT the full MCI-001..008 catalog (MCI-002/004/005/006/007 are
// separate live smokes in their own processes; see scripts/uat/managed-cloud). JourneyDigest in a
// ProvisioningProof is the hash of exactly this canonical list; the anti-fabrication gate
// (tests/uat/managed-cloud) recomputes hashParts(ManagedCloudStepIDs...), asserts the committed step_ids
// EQUAL this canonical list, and fails if either the digest or the list does not reproduce — a fabricated
// spine is caught the way the E11 advertised_schema_hash was.
var ManagedCloudStepIDs = []string{
	"provision-org", "provision-project", "provision-api-key", "config-policy",
	"real-run", "steer", "list-history", "cross-tenant-deny",
}

// hashParts is the shared checksum primitive (sha256 of each part followed by a NUL, hex-encoded, sha256:
// prefixed) — the same construction as tests/uat hashBundle and the extensibility gate's hashOf. The
// managed-cloud JourneyDigest is hashParts over the ordered step ids, so it is re-derivable from the
// manifest's own step list and cannot be fabricated independently.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// ProvisioningProof is the evidence a provisioning_claim requires (plan §T11 T2, MCI-001 — and the journey's
// restart-less spine): a SECOND tenant was created through the public API (POST /v1/organizations, /v1/projects,
// /v1/api-keys) on the SAME running process, its config_policy was written and observed by the resolver, and
// the restart-less SPINE steps resolved on that one process with NO restart. OrgID/ProjectID/APIKeyID are the
// created tenant's ids; ConfigPolicyApplied records the PATCH /v1/projects config_policy took on the resolver;
// StepIDs is the ordered spine the process resolved (ManagedCloudStepIDs — the API-provision + run + steer +
// list + cross-tenant-deny spine, NOT the finer MCI smokes) and JourneyDigest is hashParts(StepIDs...) —
// re-derivable, so a fabricated digest is caught. RestartCount is the number of restarts across the spine
// (must be 0 — the live journey proves it via pg_postmaster_start_time identical start-to-end; the
// in-process control-plane cannot restart mid-journey, so the database boot time is the concrete measure). A
// "provisioned" marker with no ids, an unapplied policy, a fabricated digest, or any restart is not proof.
type ProvisioningProof struct {
	OrgID               string   `json:"org_id"`
	ProjectID           string   `json:"project_id"`
	APIKeyID            string   `json:"api_key_id"`
	ConfigPolicyApplied bool     `json:"config_policy_applied"`
	StepIDs             []string `json:"step_ids"`
	JourneyDigest       string   `json:"journey_digest"`
	RestartCount        int      `json:"restart_count"`
}

// Complete reports the created tenant's three ids, an applied config_policy, a full ordered spine, a well-
// formed journey digest, and zero restarts. It does NOT recompute the digest (that is the anti-fabrication
// gate's job, mirroring AdvertisingProof) — but an empty or malformed digest, a short spine, or a restart
// fails here so the restart-less spine can never be marker-passed.
func (p ProvisioningProof) Complete() bool {
	return p.OrgID != "" && p.ProjectID != "" && p.APIKeyID != "" && p.ConfigPolicyApplied &&
		len(p.StepIDs) >= len(ManagedCloudStepIDs) && checksumPattern.MatchString(p.JourneyDigest) &&
		p.RestartCount == 0
}

// SecretResolveProof is the evidence a secret_resolve_claim requires (plan §T11 T3, MCI-002): a secret-ref
// was written through the API and RESOLVED by a real run without a restart, and its plaintext value NEVER
// surfaced in a response, log, or event. Ref/Version identify the written secret; ResolvedInRun is the run
// that consumed it; RestartCount must be 0 (rotation/resolution without restart is the whole point);
// ValueSurfaced must be false. A "rotated" marker, a resolution that needed a restart, or a value that
// leaked is not proof.
type SecretResolveProof struct {
	Ref           string `json:"ref"`
	Version       string `json:"version"`
	ResolvedInRun string `json:"resolved_in_run"`
	RestartCount  int    `json:"restart_count"`
	ValueSurfaced bool   `json:"value_surfaced"`
}

// Complete reports the ref, its version, the run that resolved it, zero restarts, and a value that never
// surfaced. A missing ref/version/run, any restart, or a surfaced value is not proof.
func (p SecretResolveProof) Complete() bool {
	return p.Ref != "" && p.Version != "" && p.ResolvedInRun != "" && p.RestartCount == 0 && !p.ValueSurfaced
}

// IsolationProof is the evidence an isolation_claim requires (plan §T11 T1/T4/T5, MCI-003/004, TEN-001/002 —
// the brief's load-bearing cross-tenant invariant): tenant B's request for tenant A's resource returned a
// REAL deny (a 404 not-found or a 403 RLS-deny), disclosing ZERO of tenant A's rows — not a log line saying
// "isolated". OwnerTenant/RequesterTenant are distinct; Resource names what was reached for (a run, an
// artifact, a secret, a list cursor); ObservedStatus is the deny code; LeakedRows is how many of the owner's
// rows the requester saw (must be 0). Same tenant on both sides, a 2xx, or any leaked row is the opposite of
// isolation and is not proof.
type IsolationProof struct {
	OwnerTenant     string `json:"owner_tenant"`
	RequesterTenant string `json:"requester_tenant"`
	Resource        string `json:"resource"`
	ObservedStatus  int    `json:"observed_status"`
	LeakedRows      int    `json:"leaked_rows"`
}

// Complete reports two DISTINCT tenants, a named resource, a deny status (404 or 403), and zero leaked rows.
// A self-isolation (same tenant), an allow status, or any leaked row fails — cross-tenant isolation cannot
// be marker-passed.
func (p IsolationProof) Complete() bool {
	return p.OwnerTenant != "" && p.RequesterTenant != "" && p.OwnerTenant != p.RequesterTenant &&
		p.Resource != "" && (p.ObservedStatus == 404 || p.ObservedStatus == 403) && p.LeakedRows == 0
}

// ArtifactProof is the evidence an artifact_claim requires (plan §T11 T5, MCI-004): a run-produced artifact
// was DOWNLOADED over the authenticated read-path and its bytes matched the run's output. ContentDigest is
// the sha256 the API's Content-Digest header carried; ByteLen is the downloaded length; DigestMatches records
// that the digest equalled sha256 of the ACTUAL downloaded bytes (and, in the live tier, the workspace file
// bit-for-bit). The digest is re-derivable from the artifact bytes, so the anti-fabrication gate recomputes
// it — a made-up digest, a zero-length body, or a digest that did not match the bytes is not proof.
type ArtifactProof struct {
	ArtifactID    string `json:"artifact_id"`
	ContentDigest string `json:"content_digest"`
	ByteLen       int    `json:"byte_len"`
	DigestMatches bool   `json:"digest_matches"`
}

// Complete reports the artifact id, a well-formed sha256 content digest, a non-empty body, and a digest that
// matched the downloaded bytes. A missing id, a malformed digest, an empty body, or an unmatched digest fails.
func (p ArtifactProof) Complete() bool {
	return p.ArtifactID != "" && checksumPattern.MatchString(p.ContentDigest) && p.ByteLen > 0 && p.DigestMatches
}

// RefusalProof is the evidence a refusal_claim requires (plan §T11 T6/T7, MCI-005, BIL-001/QUO-001): an
// admission was REFUSED by a durable budget or an edge rate limit, and the refused run NEVER started billable
// compute (§20.12 — the run is rejected before compute, so it is not charged). LimitKind is "budget" or
// "rate"; ObservedStatus is the deny code (402 for a budget/quota exhaustion, 429 for a rate/concurrency
// cap); BillableComputeStarted must be false. A "refused" marker, an unknown limit kind, a non-deny status,
// or a refusal that still burned compute is not proof.
type RefusalProof struct {
	LimitKind              string `json:"limit_kind"`
	ObservedStatus         int    `json:"observed_status"`
	BillableComputeStarted bool   `json:"billable_compute_started"`
}

// Complete reports a known limit kind, a deny status matching that kind (429 for rate, 402 for budget), and
// no billable compute. Any other combination — a rate limit that returned 402, a budget that burned compute —
// is not proof.
func (p RefusalProof) Complete() bool {
	if p.BillableComputeStarted {
		return false
	}
	switch p.LimitKind {
	case "rate":
		return p.ObservedStatus == 429
	case "budget":
		return p.ObservedStatus == 402
	default:
		return false
	}
}

// RouteProof is the evidence a route_claim requires (plan §T11 T8, MCI-006): two projects on ONE stack
// resolved DISTINCT model routes — a different model id AND a distinct model connection each — so the
// DB-backed per-project router (not a global env default) chose the model+credential. ProjectAModel and
// ProjectBModel are the resolved model ids (must differ); DistinctConnections records that the two routes
// pointed at different model_connections (distinct credentials). Honest ceiling: one provider FAMILY
// (provider-one) — this proves per-project model+credential selection, not a second adapter. Identical
// models or a shared connection is not proof that per-project routing took effect.
type RouteProof struct {
	ProjectAModel       string `json:"project_a_model"`
	ProjectBModel       string `json:"project_b_model"`
	DistinctConnections bool   `json:"distinct_connections"`
}

// Complete reports two non-empty, DISTINCT resolved model ids and distinct connections. Equal models or a
// shared connection means per-project routing was not proven.
func (p RouteProof) Complete() bool {
	return p.ProjectAModel != "" && p.ProjectBModel != "" && p.ProjectAModel != p.ProjectBModel &&
		p.DistinctConnections
}

// BindingProof is the evidence a binding_claim requires (plan §T11 T9, MCI-007): a repository binding whose
// connection_ref was set resolved a BINDING-SCOPED credential through the secret-ref path, not the global
// GitHub App fallback. BindingID identifies the binding; ConnectionRef is the non-empty ref it carried;
// ResolvedViaRef records that the credential resolver took the ref path. Honest ceiling: this proves the
// connection_ref resolver SEAM (plan §T9) — a per-tenant GitHub App onboarding surface is out of scope. An
// empty ref or a resolution that fell through to the global App is not proof of the seam.
type BindingProof struct {
	BindingID      string `json:"binding_id"`
	ConnectionRef  string `json:"connection_ref"`
	ResolvedViaRef bool   `json:"resolved_via_ref"`
}

// Complete reports a binding id, a non-empty connection ref, and a resolution that took the ref path. A
// missing ref or a global-App fallback is not proof.
func (p BindingProof) Complete() bool {
	return p.BindingID != "" && p.ConnectionRef != "" && p.ResolvedViaRef
}

// SteerProof is the evidence a steer_claim requires (plan §T11 T10, MCI-008): a steer command driven through
// the @palai/sdk session surface took effect on the run — the E08 command spine reached from the SDK for the
// first time. SessionID/CommandID identify the durable command; CommandKind is what was steered (e.g.
// send_message, change_config, interrupt); Applied records that the command was accepted and observed on the
// run (queued/applied, not rejected). A steer that was never accepted, or a marker with no command id, is not
// proof.
type SteerProof struct {
	SessionID   string `json:"session_id"`
	CommandID   string `json:"command_id"`
	CommandKind string `json:"command_kind"`
	Applied     bool   `json:"applied"`
}

// Complete reports the session, the durable command id, its kind, and that it was applied. A missing id/kind
// or an unapplied command is not proof.
func (p SteerProof) Complete() bool {
	return p.SessionID != "" && p.CommandID != "" && p.CommandKind != "" && p.Applied
}

// SelfHostStepIDs is the ordered restart-less install SPINE the self-host EXIT journey resolves on ONE
// production-compose stack (plan §T7, the E14 EXIT gate): a clean install, the production bring-up, the
// CA-verified TLS edge, config-validate + doctor v2 green, a tenant provisioned through the admin CLI over
// the edge, a REAL provider run through the edge, the metrics probe, an installation backup, and a
// support-bundle. These are the steps ONE stack actually resolves with NO restart — NOT the full OPS-002 +
// DR-002 + DR-004..006 catalog: the restore into a SEPARATE clean stack is a SECOND stack (BackupProof /
// RestoreVerifyProof), the same way ManagedCloudStepIDs excluded the finer MCI smokes. JourneyDigest in an
// InstallProof is hashParts of exactly this canonical list; the anti-fabrication gate (tests/uat/self-host)
// recomputes hashParts(SelfHostStepIDs...), asserts the committed step_ids EQUAL this canonical list, and
// fails if either the digest or the list does not reproduce — a fabricated spine is caught the way the E13
// journey_digest was.
var SelfHostStepIDs = []string{
	"clean-install", "production-bring-up", "tls-edge-verified", "config-validate", "doctor-v2",
	"provision-tenant", "real-run", "metrics-probe", "backup", "support-bundle",
}

// InstallProof is the evidence an install_claim requires (plan §T7, OPS-002 — and the journey's restart-less
// spine): a clean production-profile install came up HARDENED and resolved the restart-less install SPINE
// ending in a REAL provider run. MasterKeyNonDev records the fail-closed boot guard admitted a real (not
// dev-default) master key; RegistrationClosed that there is no public self-registration surface (provisioning
// is bootstrap-key + the admin CLI only); EdgeVerified that the admin CLI + the run reached the control-plane
// through the self-signed TLS edge with CA verification (not the loopback API); ConfigValid / DoctorGreen that
// `palai config validate` and doctor v2 were green; StepIDs is the ordered spine the stack resolved
// (SelfHostStepIDs) and JourneyDigest is hashParts(StepIDs...) — re-derivable, so a fabricated digest is
// caught. RestartCount is the number of control-plane restarts across the spine (must be 0 — the live journey
// proves it via pg_postmaster_start_time identical start-to-end, the E13 measure). A "installed" marker with
// an unhardened posture, an unverified edge, a red doctor, a fabricated digest, or any restart is not proof.
type InstallProof struct {
	MasterKeyNonDev    bool     `json:"master_key_non_dev"`
	RegistrationClosed bool     `json:"registration_closed"`
	EdgeVerified       bool     `json:"edge_verified"`
	ConfigValid        bool     `json:"config_valid"`
	DoctorGreen        bool     `json:"doctor_green"`
	StepIDs            []string `json:"step_ids"`
	JourneyDigest      string   `json:"journey_digest"`
	RestartCount       int      `json:"restart_count"`
}

// Complete reports a hardened posture (real master key, closed registration, CA-verified edge), a green
// config-validate + doctor, a full ordered spine, a well-formed journey digest, and zero restarts. It does
// NOT recompute the digest (that is the anti-fabrication gate's job, mirroring ProvisioningProof) — but an
// empty/malformed digest, a short spine, an unverified edge, or a restart fails here so the restart-less
// install spine can never be marker-passed.
func (p InstallProof) Complete() bool {
	return p.MasterKeyNonDev && p.RegistrationClosed && p.EdgeVerified && p.ConfigValid && p.DoctorGreen &&
		len(p.StepIDs) >= len(SelfHostStepIDs) && checksumPattern.MatchString(p.JourneyDigest) && p.RestartCount == 0
}

// BackupProof is the evidence a backup_claim requires (plan §T7 T4, DR-002): an installation backup captured
// from a running stack restored into a SEPARATE, empty clean stack — the "restore into a separate clean
// install" invariant. SourceProject / TargetProject are the two distinct compose projects (a restore into the
// same stack proves nothing); ManifestDigest is a re-derivable hash over the backup manifest's identity +
// checksums (the anti-fabrication gate recomputes it from the fixture manifest, mirroring the artifact digest);
// MigrationVersion is the schema version the backup captured (> 0); TargetWasEmpty records the no-clobber gate
// refused nothing because the target held no tenant data; Restored records the load completed. Honest ceiling
// (plan §6): the two stacks are two isolated production-compose stacks on one host — a separate PHYSICAL host
// is the operator leg. A same-stack restore, a fabricated digest, or a non-empty target is not proof.
type BackupProof struct {
	SourceProject    string `json:"source_project"`
	TargetProject    string `json:"target_project"`
	ManifestDigest   string `json:"manifest_digest"`
	MigrationVersion int    `json:"migration_version"`
	TargetWasEmpty   bool   `json:"target_was_empty"`
	Restored         bool   `json:"restored"`
}

// Complete reports two DISTINCT non-empty projects, a well-formed manifest digest, a captured migration
// version, an empty restore target, and a completed restore. Equal projects, a malformed digest, or a
// non-empty target means the separate-clean-install restore was not proven.
func (p BackupProof) Complete() bool {
	return p.SourceProject != "" && p.TargetProject != "" && p.SourceProject != p.TargetProject &&
		checksumPattern.MatchString(p.ManifestDigest) && p.MigrationVersion > 0 && p.TargetWasEmpty && p.Restored
}

// RestoreVerifyProof is the evidence a restore_verify_claim requires (plan §T7 T4, DR-004..006): `palai
// restore verify` matched the restored target against its backup manifest across ALL SIX checks the shipped
// command runs (install_backup.go InstallRestoreVerify). ArchiveChecksum: the db + object-store members
// re-hashed to the manifest; MigrationVersion / TenantIDs: the live schema version and org ids match;
// RunRetrieval: the sample response is queryable from the restored data; RLSIsolation: FORCE row-level
// security + the tenant_isolation policies survived the restore (DR-005 — a silent cross-tenant breach a
// superuser read would never notice); SecretDecrypt: a stored secret still decrypts under the target master
// key (DR-006 — the canary against a restore that did not carry the source key). A "verified" marker with ANY
// check false is not proof — a restore that landed with RLS off or with dead secrets is exactly what this
// gate must catch.
type RestoreVerifyProof struct {
	ArchiveChecksum  bool `json:"archive_checksum"`
	MigrationVersion bool `json:"migration_version"`
	TenantIDs        bool `json:"tenant_ids"`
	RunRetrieval     bool `json:"run_retrieval"`
	RLSIsolation     bool `json:"rls_isolation"`
	SecretDecrypt    bool `json:"secret_decrypt"`
}

// Complete reports all six restore-verify checks green. A false on ANY of them — a checksum mismatch, a
// migration/tenant-id drift, an unretrievable run, RLS disabled on the restored data, or a secret that no
// longer decrypts — is not a verified restore, so DR-004..006 cannot be marker-passed.
func (p RestoreVerifyProof) Complete() bool {
	return p.ArchiveChecksum && p.MigrationVersion && p.TenantIDs && p.RunRetrieval && p.RLSIsolation && p.SecretDecrypt
}

// UpgradeStepIDs is the ordered upgrade-journey spine the SH-2 EXIT journey resolves (plan §T6). Unlike the
// install/managed-cloud spines this is NOT restart-less — an N->N+1 upgrade RECREATES the control-plane by
// design; the load-bearing invariant is that the ACTIVE run SURVIVES that recreate on its pinned engine and
// the event stream stays continuous. JourneyDigest in an UpgradeProof is hashParts of exactly this list; the
// anti-fabrication gate (tests/uat/upgrade) recomputes hashParts(UpgradeStepIDs...), asserts the committed
// step_ids EQUAL this canonical list, and fails if either does not reproduce — the E14 spine-anchor discipline.
var UpgradeStepIDs = []string{
	"clean-install-n", "provision", "real-run-n", "active-run-start", "backup",
	"upgrade-n-to-n1", "active-run-survived", "real-run-n1",
	"app-rollback", "engine-alias-rollback", "dr-drill", "airgap-offline-verify", "helm-render",
}

// HelmPolicyAsserts is the canonical restricted-chart policy-assert set a HelmRenderProof carries (plan §T3/§T6).
// AssertsDigest is hashParts of exactly this list, so the anti-fabrication gate recomputes it — a bundle that
// quietly drops an assert (e.g. no-cluster-role) cannot keep a matching digest. Keep in lockstep with the
// render-assert suite (tests/uat/kubernetes/render_assert_test.go).
var HelmPolicyAsserts = []string{
	"no-cluster-role", "run-as-non-root", "no-privileged", "network-policy-default-deny",
	"pod-disruption-budget", "migration-job-pre-install-hook", "external-pg-s3-only",
}

// UpgradeProof is the evidence an upgrade_claim requires (plan §T6, OPS-005/007 + SAN-011): an active run was
// DRAINED before the N->N+1 control-plane recreate and SURVIVED on its pinned engine to completion
// (SurvivingRunCompleted) — the T2 MF-3 with-active-run drain (RollbackDrained records that drain-before-recreate
// path took, not a silent migration). The event stream it emitted stayed continuous across the recreate
// (EventContinuityDigest = hashParts of the ordered ContinuityEventIDs — re-derivable; the live journey proves
// the survivor's session events are GAPLESS at the DB and the anchor canons the created→terminal endpoints).
// BOTH rollbacks then ran the same ordering: the app image rolled back to N with the schema still expanded
// (AppRollback) and the new-run engine alias rolled back to engine_n while the survivor stayed pinned
// (EngineAliasRollback). FromVersion/ToVersion are the two build stamps (must differ — same binaries, different
// -ldflags stamp). StepIDs is the ordered journey spine (UpgradeStepIDs) and JourneyDigest is hashParts(StepIDs...).
// An "upgraded" marker with a run that did not survive, a fabricated continuity/spine digest, a drain that did
// not take, or equal version stamps is not proof.
type UpgradeProof struct {
	FromVersion           string   `json:"from_version"`
	ToVersion             string   `json:"to_version"`
	SurvivingRunID        string   `json:"surviving_run_id"`
	SurvivingRunCompleted bool     `json:"surviving_run_completed"`
	ContinuityEventIDs    []string `json:"continuity_event_ids"`
	EventContinuityDigest string   `json:"event_continuity_digest"`
	AppRollback           bool     `json:"app_rollback"`
	EngineAliasRollback   bool     `json:"engine_alias_rollback"`
	RollbackDrained       bool     `json:"rollback_drained"`
	StepIDs               []string `json:"step_ids"`
	JourneyDigest         string   `json:"journey_digest"`
}

// Complete reports two DISTINCT version stamps, a surviving+completed run, a continuity digest re-derivable from
// the event list, both rollbacks with the drain-before-recreate invariant, and the CANONICAL upgrade spine +
// its digest. Unlike InstallProof, this recomputes the spine anchor IN the gate (SF-4): step_ids must equal
// UpgradeStepIDs and journey_digest must be hashParts of that canonical list, so a shape-consistent fabricated
// spine/digest is rejected by VerifyManifest/PromoteGate, not only the anchor test. A run that did not complete,
// equal stamps, a rollback that did not drain, or a non-canonical spine/digest is not proof.
func (p UpgradeProof) Complete() bool {
	return p.FromVersion != "" && p.ToVersion != "" && p.FromVersion != p.ToVersion &&
		p.SurvivingRunID != "" && p.SurvivingRunCompleted && len(p.ContinuityEventIDs) >= 2 &&
		p.EventContinuityDigest == hashParts(p.ContinuityEventIDs...) &&
		p.AppRollback && p.EngineAliasRollback && p.RollbackDrained &&
		slices.Equal(p.StepIDs, UpgradeStepIDs) && p.JourneyDigest == hashParts(UpgradeStepIDs...)
}

// MigrationJournalProof is the evidence a migration_journal_claim requires (plan §T6, OPS-006): the boot
// migration chain was INTERRUPTED mid-run (a test-only fault killed the control-plane) and RESUMED on restart
// to the correct journal head with NO data corruption. JournalHead is the head migration the schema_revisions
// journal reports after resume; InterruptedAt is the migration the fault hit; Resumed records the chain
// completed; RowChecksumMatch records the pre/post row-checksum was identical (no corruption). A "resumed"
// marker with no head, an unfinished chain, or a checksum drift is not proof.
type MigrationJournalProof struct {
	JournalHead      string `json:"journal_head"`
	InterruptedAt    string `json:"interrupted_at"`
	Resumed          bool   `json:"resumed"`
	RowChecksumMatch bool   `json:"row_checksum_match"`
}

// Complete reports a journal head, the interruption point, a resumed+completed chain, and a matching pre/post
// row checksum. A missing head/interruption, an unfinished chain, or a checksum mismatch is not proof.
func (p MigrationJournalProof) Complete() bool {
	return p.JournalHead != "" && p.InterruptedAt != "" && p.Resumed && p.RowChecksumMatch
}

// DrillProof is the evidence a drill_claim requires (plan §T6, DR-001 + DR-002/004..006 — the measurement
// anti-fabrication anchor): a DR drill ran on the two-stack seam and produced a MEASURED RPO/RTO the verifier
// recomputes from the RAW timestamps. It REUSES the T5 dr.Measure format verbatim (the same raw timestamps +
// computed seconds T5's dr.Verify writes), and Complete() recomputes with the SAME dr.ComputeRPO/RTO T5 uses —
// so a hand-edited rpo_seconds/rto_seconds fails HERE (the shape verifier), not only in the dedicated anchor
// test. Measure is nil for detection-only drills (DR-004 object corruption, DR-005 key recovery) that prove
// fail-closed detection, not a timed recovery. A "drilled" marker with no id/scenario, a failed drill, or a
// measurement the raw timestamps do not support is not proof.
type DrillProof struct {
	DrillID  string      `json:"drill_id"`
	Scenario string      `json:"scenario"`
	Passed   bool        `json:"passed"`
	Measure  *dr.Measure `json:"measure,omitempty"`
}

// Complete reports a named drill that passed and, when it carries a Measure, an RPO/RTO DERIVABLE from its raw
// timestamps (recomputed with dr.ComputeRPO/RTO, the exact primitives T5's dr.Verify uses) and non-negative. A
// detection-only drill (Measure nil) passes on the id/scenario/passed triple. A fabricated measurement — a
// hand-edited seconds value the timestamps do not reproduce, or an unparseable/negative window — fails.
func (p DrillProof) Complete() bool {
	if p.DrillID == "" || p.Scenario == "" || !p.Passed {
		return false
	}
	if p.Measure == nil {
		return true // detection-only drill: fail-closed detection, no timed recovery to measure
	}
	lw, err1 := time.Parse(time.RFC3339Nano, p.Measure.LastMarkerWrittenAt)
	lb, err2 := time.Parse(time.RFC3339Nano, p.Measure.LastMarkerInBackupAt)
	da, err3 := time.Parse(time.RFC3339Nano, p.Measure.DisasterAt)
	ra, err4 := time.Parse(time.RFC3339Nano, p.Measure.RecoveredAt)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return false
	}
	const eps = 1e-6
	if math.Abs(dr.ComputeRPO(lw, lb)-p.Measure.RPOSeconds) > eps ||
		math.Abs(dr.ComputeRTO(da, ra)-p.Measure.RTOSeconds) > eps {
		return false
	}
	return p.Measure.RPOSeconds >= 0 && p.Measure.RTOSeconds >= 0
}

// AirgapProof is the evidence an airgap_claim requires (plan §T6, OPS-004): a signed offline air-gap bundle
// re-verified with NO network and rejected a tamper. ManifestDigest is the bundle's signed-root sha256sums
// digest; SignatureVerified records the openssl P-256 detached signature (the E14 T5 tool, verbatim) verified;
// OfflineNetworkNone records the verify ran inside `docker run --network none` (egress topologically
// impossible, not a log line); TamperRejected records a 1-byte flip made verify FAIL (the negative half). A
// "verified" marker without the offline-network-none proof, or without the tamper rejection, is not proof.
type AirgapProof struct {
	ManifestDigest     string `json:"manifest_digest"`
	SignatureVerified  bool   `json:"signature_verified"`
	OfflineNetworkNone bool   `json:"offline_network_none"`
	TamperRejected     bool   `json:"tamper_rejected"`
}

// Complete reports a well-formed manifest digest, a verified signature, an offline (--network none)
// verification, and a rejected tamper. A malformed digest or any false is not proof.
func (p AirgapProof) Complete() bool {
	return checksumPattern.MatchString(p.ManifestDigest) && p.SignatureVerified &&
		p.OfflineNetworkNone && p.TamperRejected
}

// HelmRenderProof is the evidence a helm_render_claim requires (plan §T6, OPS-003): the restricted Helm chart
// rendered and passed the policy asserts. RenderHash is sha256 of the `helm template` output (environment-
// captured, not re-derivable across hosts, so only well-formedness is gated here). PolicyAsserts is the set of
// restricted asserts that passed; AssertsDigest is hashParts(PolicyAsserts...) — RE-DERIVABLE, so the anti-
// fabrication gate recomputes it against HelmPolicyAsserts and a bundle that drops an assert cannot keep a
// matching digest. NoClusterRole is the load-bearing restricted invariant (no ongoing cluster-admin). A render
// with a fabricated asserts digest, fewer than the canonical asserts, or a ClusterRole present is not proof.
type HelmRenderProof struct {
	RenderHash    string   `json:"render_hash"`
	PolicyAsserts []string `json:"policy_asserts"`
	AssertsDigest string   `json:"asserts_digest"`
	NoClusterRole bool     `json:"no_cluster_role"`
}

// Complete reports a well-formed render hash, the CANONICAL restricted policy asserts + their digest, and the
// no-ClusterRole invariant. Like UpgradeProof (SF-4) the asserts anchor is recomputed IN the gate: policy_asserts
// must equal HelmPolicyAsserts and asserts_digest must be hashParts of that canonical list, so a bundle that
// quietly drops an assert (e.g. no-cluster-role) with a self-consistent digest is rejected by VerifyManifest/
// PromoteGate. A malformed render hash, a non-canonical assert list/digest, or a ClusterRole present fails.
func (p HelmRenderProof) Complete() bool {
	return checksumPattern.MatchString(p.RenderHash) &&
		slices.Equal(p.PolicyAsserts, HelmPolicyAsserts) && p.AssertsDigest == hashParts(HelmPolicyAsserts...) &&
		p.NoClusterRole
}

// secretPattern matches a credential-shaped token (an OpenAI-style sk- key), so a plaintext
// credential fails the redaction scan even when the exact value is not supplied as a needle.
var secretPattern = regexp.MustCompile(`sk-[A-Za-z0-9_-]{12,}`)

// gitCredentialPatterns catch a leaked Git credential the coding release mints and pushes with (spec
// §30.2, E09 exit-gate credential-absence scan): a classic/fine-grained PAT, a GitHub App user/
// installation/refresh token (gho_/ghu_/ghs_/ghr_), and an App private-key PEM header. A plaintext hit
// fails the bundle by construction, the ^chatcmpl-/needle discipline extended to the repository tier.
var gitCredentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),  // fine-grained PAT
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),    // ghp_ PAT, gho_/ghu_ OAuth, ghs_ installation, ghr_ refresh
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY`), // a GitHub App private key committed in the clear
}

// remoteSigningSecretPattern catches a leaked webhook/callback signing secret (the whsec_ prefix, spec
// §21.5). The E11 outbound callback AND the E12 remote-tool + hook signed transports all sign with the SAME
// webhook signer (adapters/integrations/webhook, Webhook-Signature), so a plaintext whsec_ in the manifest
// fails the bundle by construction — the same discipline scripts/verify/e01.sh applies to spike artifacts,
// now enforced in the evidence tier too (E12 T10; whsec_ was previously in e01.sh only). Opaque MCP
// connection bearers carry no distinctive prefix, so they are caught by-value as needles (the strongest,
// shape-independent redaction), never a made-up regex.
var remoteSigningSecretPattern = regexp.MustCompile(`whsec_[A-Za-z0-9_-]{6,}`)

// checksumPattern is the required checksum shape (sha256:<64 hex>).
var checksumPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// liveProviderIDPattern is the provider request-id shape a live-provider case must carry.
// Today the only live adapter is provider-one (OpenAI Chat Completions, ids "chatcmpl-...");
// widen the alternation when a second live adapter lands.
var liveProviderIDPattern = regexp.MustCompile(`^chatcmpl-[A-Za-z0-9_-]+$`)

// externalReceiptPattern is the real remote-ref/PR receipt shape an external-receipt case must carry
// (spec §30.9-30.10, REP-006/008) — parallel to liveProviderIDPattern's ^chatcmpl- for live-provider.
// A push receipt is the remote's own commit sha (40 hex); a pull-request receipt is the provider PR id
// (GitHub node id "PR_..."/numeric) or its https URL. A fake/local placeholder matches none of these, so
// an external-receipt case cannot pass with a fake remote — the whole point of the class.
var externalReceiptPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64}|PR_[A-Za-z0-9]+|https://[^\s]+/pull/[0-9]+)$`)

// VerifyManifest checks one evidence manifest against the required-field and redaction
// contract. It returns every finding; an empty slice is a clean pass. secrets are extra
// literal needles (e.g. a run's real credential) that must never appear in the manifest.
func VerifyManifest(raw []byte, secrets []string) []Finding {
	var findings []Finding

	// Redaction is a hard gate regardless of structure: a leaked credential fails the bundle.
	for _, needle := range secrets {
		if needle != "" && bytes.Contains(raw, []byte(needle)) {
			findings = append(findings, Finding{Kind: "secret", Detail: "manifest contains a supplied credential value"})
		}
	}
	if secretPattern.Match(raw) {
		findings = append(findings, Finding{Kind: "secret", Detail: "manifest contains a credential-shaped token (sk-...)"})
	}
	for _, pat := range gitCredentialPatterns {
		if pat.Match(raw) {
			findings = append(findings, Finding{Kind: "secret", Detail: "manifest contains a Git-credential-shaped token (PAT/App key/installation token)"})
			break
		}
	}
	if remoteSigningSecretPattern.Match(raw) {
		findings = append(findings, Finding{Kind: "secret", Detail: "manifest contains a webhook/remote-tool signing secret (whsec_...)"})
	}

	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return append(findings, Finding{Kind: "invalid", Detail: "manifest is not valid JSON: " + err.Error()})
	}

	miss := func(cond bool, field, c string) {
		if cond {
			findings = append(findings, Finding{Case: c, Kind: "missing", Detail: field})
		}
	}
	miss(m.Release == "", "release", "")
	miss(m.GitSHA == "", "git_sha", "")
	miss(m.APIVersion == "", "api_version", "")
	miss(m.Migration == "", "migration", "")
	miss(len(m.Cases) == 0, "cases", "")

	for _, c := range m.Cases {
		// Every case, regardless of tier, carries an id, the run that produced it, its db assertions,
		// and a well-formed checksum over the captured surface.
		miss(c.ID == "", "id", c.ID)
		miss(c.RunID == "", "run_id", c.ID)
		miss(len(c.DBAssertions) == 0, "db_assertions", c.ID)
		miss(c.Checksum == "", "checksum", c.ID)
		if c.Checksum != "" && !checksumPattern.MatchString(c.Checksum) {
			findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "checksum is not sha256:<64 hex>"})
		}

		// REC-006 (spec §26.12): a case that CLAIMS recovery (a "continued"/"resumed" marker) must carry
		// a COMPLETE RecoveryProof — the marker alone is never evidence. A missing proof is a "missing"
		// finding; a proof missing any of the eight §26.12 field groups is "invalid". Reuses
		// recovery.RecoveryProof.Complete, the same completeness gate the orchestrator emits under.
		if c.RecoveryClaim != "" {
			switch {
			case c.RecoveryProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "recovery_proof (a recovery claim requires a §26.12 RecoveryProof; a 'continued'/'resumed' marker is not proof)"})
			case !c.RecoveryProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "recovery_proof is incomplete — a §26.12 field group is missing (REC-006)"})
			}
		}

		// The E11 automation claims mirror the RecoveryProof rule exactly: a non-empty marker with no
		// proof is a "missing" finding; a proof that fails its Complete() invariant is "invalid".
		if c.DedupeClaim != "" {
			switch {
			case c.DedupeProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "dedupe_proof (a dedupe claim requires original-linkage proof; a 'deduplicated' marker is not proof)"})
			case !c.DedupeProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "dedupe_proof is incomplete: original/duplicate linkage or the single-canonical-action count is missing (AUT-001)"})
			}
		}
		if c.OccurrenceClaim != "" {
			switch {
			case c.OccurrenceProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "occurrence_proof (an occurrence claim requires single-canonical proof; a marker is not proof)"})
			case !c.OccurrenceProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "occurrence_proof is incomplete: occurrence id, planned/admitted instants, or the single-canonical count is missing (AUT-007)"})
			}
		}
		if c.CallbackClaim != "" {
			switch {
			case c.CallbackProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "callback_proof (a callback claim requires single-semantic-delivery proof; a marker is not proof)"})
			case !c.CallbackProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "callback_proof is incomplete: delivery ids, attempts, the single receiver receipt, or run-terminal-intact is missing (AUT-011/013)"})
			}
		}

		// The E12 extensibility claims mirror the rule exactly: a non-empty marker with no proof is "missing";
		// a proof that fails its Complete() invariant is "invalid".
		if c.AdvertisingClaim != "" {
			switch {
			case c.AdvertisingProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "advertising_proof (an advertising claim requires the advertised schema hash + tool names + selection mode; a marker is not proof)"})
			case !c.AdvertisingProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "advertising_proof is incomplete: the advertised schema hash, the tool names, or an honest selection mode (spontaneous/forced) is missing (EXT-001/002)"})
			}
		}
		if c.SkillClaim != "" {
			switch {
			case c.SkillProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "skill_proof (a skill claim requires a pinned digest + scan result; a 'loaded' marker is not proof)"})
			case !c.SkillProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "skill_proof is incomplete: the exact digest pin or the quarantine scan result is missing (TOL-011)"})
			}
		}
		if c.CrashIsolationClaim != "" {
			switch {
			case c.CrashIsolationProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "crash_isolation_proof (a crash-isolation claim requires breaker + tool_unavailable + control-plane-stable + other-run-flowed; a marker is not proof)"})
			case !c.CrashIsolationProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "crash_isolation_proof is incomplete: the breaker did not trip, tool_unavailable was not visible, the control-plane was not stable, or no other run flowed (EXT-005)"})
			}
		}

		// The E13 managed-cloud claims mirror the rule exactly: a non-empty marker with no proof is "missing";
		// a proof that fails its Complete() invariant is "invalid" (plan §T11, MCI-001..008).
		if c.ProvisioningClaim != "" {
			switch {
			case c.ProvisioningProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "provisioning_proof (a provisioning claim requires the created tenant's org/project/key ids + an applied config_policy + the restart-less journey spine; a 'provisioned' marker is not proof)"})
			case !c.ProvisioningProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "provisioning_proof is incomplete: an org/project/key id, the applied config_policy, the ordered journey spine + digest, or the zero-restart invariant is missing (MCI-001)"})
			}
		}
		if c.SecretResolveClaim != "" {
			switch {
			case c.SecretResolveProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "secret_resolve_proof (a secret-resolve claim requires the ref+version resolved by a run with no restart and the value never surfaced; a marker is not proof)"})
			case !c.SecretResolveProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "secret_resolve_proof is incomplete: the ref/version, the resolving run, the zero-restart invariant, or value-never-surfaced is missing (MCI-002)"})
			}
		}
		if c.IsolationClaim != "" {
			switch {
			case c.IsolationProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "isolation_proof (an isolation claim requires two distinct tenants + a real 404/403 deny + zero leaked rows; a 'isolated' marker is not proof)"})
			case !c.IsolationProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "isolation_proof is incomplete: the tenants are not distinct, the status was not a 404/403 deny, or a tenant-A row leaked to tenant B (MCI-003/004, TEN-001/002)"})
			}
		}
		if c.ArtifactClaim != "" {
			switch {
			case c.ArtifactProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "artifact_proof (an artifact claim requires the artifact id + a re-derivable content digest that matched the run's bytes; a marker is not proof)"})
			case !c.ArtifactProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "artifact_proof is incomplete: the artifact id, a well-formed sha256 content digest, a non-empty body, or the digest-matched-bytes invariant is missing (MCI-004)"})
			}
		}
		if c.RefusalClaim != "" {
			switch {
			case c.RefusalProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "refusal_proof (a refusal claim requires the limit kind + a deny status + no billable compute; a 'refused' marker is not proof)"})
			case !c.RefusalProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "refusal_proof is incomplete: an unknown limit kind, a status that does not match the kind (429 for rate, 402 for budget), or compute that started anyway (MCI-005)"})
			}
		}
		if c.RouteClaim != "" {
			switch {
			case c.RouteProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "route_proof (a route claim requires two projects' DISTINCT resolved model ids + distinct connections; a marker is not proof)"})
			case !c.RouteProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "route_proof is incomplete: the two model ids are not both present-and-distinct, or the connections were not distinct — per-project routing was not proven (MCI-006)"})
			}
		}
		if c.BindingClaim != "" {
			switch {
			case c.BindingProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "binding_proof (a binding claim requires the binding id + a non-empty connection_ref resolved via the ref path; a marker is not proof)"})
			case !c.BindingProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "binding_proof is incomplete: the binding id, the connection_ref, or the resolved-via-ref invariant is missing (MCI-007)"})
			}
		}
		if c.SteerClaim != "" {
			switch {
			case c.SteerProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "steer_proof (a steer claim requires the session + durable command id + kind + applied; a marker is not proof)"})
			case !c.SteerProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "steer_proof is incomplete: the session, the durable command id, its kind, or the applied invariant is missing (MCI-008)"})
			}
		}

		// The E14 self-host claims mirror the rule exactly: a non-empty marker with no proof is "missing";
		// a proof that fails its Complete() invariant is "invalid" (plan §T7, OPS-002 + DR-002 + DR-004..006).
		if c.InstallClaim != "" {
			switch {
			case c.InstallProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "install_proof (an install claim requires the hardened posture + CA-verified edge + green config-validate/doctor + the restart-less install spine; an 'installed' marker is not proof)"})
			case !c.InstallProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "install_proof is incomplete: the non-dev master key, closed registration, CA-verified edge, green config-validate/doctor, the ordered install spine + digest, or the zero-restart invariant is missing (OPS-002)"})
			}
		}
		if c.BackupClaim != "" {
			switch {
			case c.BackupProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "backup_proof (a backup claim requires two distinct stacks + a re-derivable manifest digest + an empty restore target; a 'restored' marker is not proof)"})
			case !c.BackupProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "backup_proof is incomplete: the source/target projects are not distinct, the manifest digest is malformed, the target was not empty, or the restore did not complete (DR-002)"})
			}
		}
		if c.RestoreVerifyClaim != "" {
			switch {
			case c.RestoreVerifyProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "restore_verify_proof (a restore-verify claim requires all six checks — checksum, migration, tenant-ids, run-retrieval, RLS isolation, secret canary; a 'verified' marker is not proof)"})
			case !c.RestoreVerifyProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "restore_verify_proof is incomplete: a checksum/migration/tenant-id/run-retrieval mismatch, RLS disabled on the restored data, or a secret that no longer decrypts under the target key (DR-004..006)"})
			}
		}

		// The E15 SH-2 RC claims mirror the rule exactly: a non-empty marker with no proof is "missing"; a
		// proof that fails its Complete() invariant is "invalid" (plan §T6, OPS-003..008 + DR-001 + SAN-011).
		if c.UpgradeClaim != "" {
			switch {
			case c.UpgradeProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "upgrade_proof (an upgrade claim requires the two version stamps + a surviving-and-completed run + a re-derivable continuity digest + both rollbacks draining the run; an 'upgraded' marker is not proof)"})
			case !c.UpgradeProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "upgrade_proof is incomplete: equal/missing version stamps, a run that did not survive-and-complete, a malformed continuity/journey digest, or a rollback that did not drain the active run (OPS-005/007, SAN-011, MF-3)"})
			}
		}
		if c.MigrationJournalClaim != "" {
			switch {
			case c.MigrationJournalProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "migration_journal_proof (a migration-journal claim requires the journal head + the interruption point + a resumed chain with a matching row checksum; a 'resumed' marker is not proof)"})
			case !c.MigrationJournalProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "migration_journal_proof is incomplete: a missing journal head/interruption point, an unfinished chain, or a pre/post row-checksum drift (OPS-006)"})
			}
		}
		if c.DrillClaim != "" {
			switch {
			case c.DrillProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "drill_proof (a drill claim requires the drill id + scenario + pass, and for a timed drill a RPO/RTO derivable from raw timestamps; a 'drilled' marker is not proof)"})
			case !c.DrillProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "drill_proof is incomplete: a missing id/scenario, a failed drill, or a MEASURED rpo/rto the raw timestamps do not reproduce — a fabricated measurement (DR-001, DR-002/004..006)"})
			}
		}
		if c.AirgapClaim != "" {
			switch {
			case c.AirgapProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "airgap_proof (an airgap claim requires the manifest digest + an offline (--network none) signature re-verify + a rejected tamper; a 'verified' marker is not proof)"})
			case !c.AirgapProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "airgap_proof is incomplete: a malformed manifest digest, a signature that did not verify, a verify that was not offline, or a tamper that was not rejected (OPS-004)"})
			}
		}
		if c.HelmRenderClaim != "" {
			switch {
			case c.HelmRenderProof == nil:
				findings = append(findings, Finding{Case: c.ID, Kind: "missing", Detail: "helm_render_proof (a helm-render claim requires the render hash + the restricted policy asserts + a re-derivable asserts digest + no-ClusterRole; a 'rendered' marker is not proof)"})
			case !c.HelmRenderProof.Complete():
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: "helm_render_proof is incomplete: a malformed render/asserts digest, fewer than the canonical policy asserts, or a ClusterRole present in the render (OPS-003)"})
			}
		}

		if c.ProofClass == "external-receipt" {
			// A publication (push/PR) is not a model run: it carries a REAL remote-ref/PR receipt instead
			// of a provider request id, image digest, mTLS enroll, or a run terminal. The receipt is the
			// load-bearing proof, so it must be present and genuinely remote-shaped — a fake never passes.
			miss(c.ExternalReceipt == "", "external_receipt", c.ID)
			if c.ExternalReceipt != "" && !externalReceiptPattern.MatchString(c.ExternalReceipt) {
				findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: fmt.Sprintf("external_receipt %q is not a real remote-ref/PR receipt (want a git sha, provider PR id, or PR URL) for proof_class=external-receipt", c.ExternalReceipt)})
			}
			continue
		}

		// A model-run case (live-provider, e2e-deterministic, component-real): the engine-run receipt
		// shape — image digest, provider request id, mTLS enroll, and a single terminal.
		miss(c.ImageDigest == "", "image_digest", c.ID)
		miss(c.ProviderRequestID == "", "provider_request_id", c.ID)
		miss(c.MTLSEnroll == "", "mtls_enroll", c.ID)
		if c.ProofClass == "live-provider" && c.ProviderRequestID != "" && !liveProviderIDPattern.MatchString(c.ProviderRequestID) {
			findings = append(findings, Finding{Case: c.ID, Kind: "invalid", Detail: fmt.Sprintf("provider_request_id %q is not provider-shaped (want chatcmpl-...) for proof_class=live-provider", c.ProviderRequestID)})
		}
		if c.Terminal.Count != 1 {
			findings = append(findings, Finding{Case: c.ID, Kind: "invalid",
				Detail: fmt.Sprintf("terminal count = %d, want exactly 1", c.Terminal.Count)})
		}
	}
	return findings
}

// VerifyRelease verifies the manifest.json in a release directory and rolls the findings
// into the operator summary. A missing manifest is a failed bundle, not a crash.
func VerifyRelease(releaseDir string, secrets []string) (Summary, error) {
	raw, err := os.ReadFile(filepath.Join(releaseDir, "manifest.json"))
	if err != nil {
		return Summary{}, fmt.Errorf("read evidence manifest: %w", err)
	}
	findings := VerifyManifest(raw, secrets)

	var m evidenceManifest
	_ = json.Unmarshal(raw, &m)

	// A case is failed if it carries any finding or its recorded status is not PASS.
	failedCases := map[string]bool{}
	summary := Summary{Findings: findings}
	for _, f := range findings {
		switch f.Kind {
		case "missing":
			summary.Missing++
		case "secret":
			summary.SecretFindings++
		}
		if f.Case != "" {
			failedCases[f.Case] = true
		}
	}
	for _, c := range m.Cases {
		if failedCases[c.ID] || c.Status != "PASS" {
			summary.Failed++
			continue
		}
		summary.Passed++
	}
	// A release-level finding (bad git_sha, leaked secret) fails the whole bundle even when
	// every case looks clean, so a zero-case pass is never reported as OK.
	if summary.Passed > 0 && (summary.SecretFindings > 0 || releaseLevelMissing(findings)) {
		summary.Failed += summary.Passed
		summary.Passed = 0
	}
	return summary, nil
}

// releaseLevelMissing reports whether any finding is a release-level (case-less) problem.
func releaseLevelMissing(findings []Finding) bool {
	for _, f := range findings {
		if f.Case == "" && (f.Kind == "missing" || f.Kind == "invalid") {
			return true
		}
	}
	return false
}
