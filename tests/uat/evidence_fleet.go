package uat

// The E24 T8 EXIT-gate proof type (plan §T8) — the `runner-fleet-0.1.0` sign-off.
//
// It lives in its own file rather than growing evidence.go further (the E18 T10 / E21 T7 / E22 T7 / E23 T7
// precedent), but it is the SAME package and the SAME discipline: Complete() gates the structure a claim
// marker requires, and every counter that MATTERS is RECOMPUTED from bytes the proof carries rather than
// read off a declared number.
//
// WHAT THIS BUNDLE CLAIMS, and every clause is narrower than the sentence a reader will want it to be:
//
//   - a fleet is an INVENTORY: several machines exist as identified, tenant-scoped, pooled and revocable
//     rows, and the identity is the SERVER's to mint rather than the enrolling machine's to declare;
//   - WHICH pool a run executes in is a configuration, and placement into one is a REFUSAL rather than a
//     preference — a machine in the wrong pool is not "next best", it is unreachable;
//   - a run with no capacity PARKS instead of dying, and wakes when a machine joins its pool;
//   - revoking a pool KEY leaves the machines that enrolled with it RUNNING, and revoking a MACHINE
//     outlives the control-plane process that decided it.
//
// AND THE HONEST CEILING, WHICH IS THIS BUNDLE'S MOST IMPORTANT SENTENCE. E24 T7 — the execution relay —
// WAS DEFERRED BY THE OWNER AND IS NOT IN THIS EPIC. Every tool still executes in the CONTROL PLANE's own
// process: the shell through `orch.SetShellRunner(shellRunnerFromEnv())` and the file tool through
// `workspace.NewWorkspaceFS(env.WorkspaceRoot)`, both against the allocation the control plane holds. So a
// Mac pool routes a run's ENGINE to a Mac and `xcodebuild` still runs wherever the control plane is — which
// means, in one clause: A MAC IS ONLY A MAC WHEN THE CONTROL PLANE IS ON IT. The fleet is real; the remote
// EXECUTION is not. That is why there is no FLT-006, no counter (f), and no relay leg in the journey.
//
// WHAT IT DOES NOT CLAIM: that any of it ran on two physical machines. FleetPeer is STRUCTURALLY the literal
// "fake", `linux/amd64` has never been verified from this machine, and §6 leg 1 is OPEN and BIGGER.
// NO TIER ADVANCES.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// FleetBundle is the E24 EXIT bundle's release name.
const FleetBundle = "runner-fleet-0.1.0"

// FleetCaseIDs are the FIVE UAT ids E24 opens, and the `FLT-` prefix is a GATE decision rather than an
// aesthetic one — the same measured reasoning that gave E21 its `TLM-`, E22 its `CAS-` and E23 its `HIL-`:
//
//   - an `SLK-`/`WRK-`/`SAN-`/`OPS-` id added to expectedExtensionsCatalog regenerates the SHIPPED
//     extensions-0.1.0 bundle (that map IS its case list, and CapabilityClaims feeds a digest folded into
//     its every checksum);
//   - an id already inside AgentSurfaceCaseIDs / ToolsMemoryCaseIDs / CodeAndShipCaseIDs /
//     ToolApprovalCaseIDs matches an EARLIER family marker in PromoteGateFor and dispatches this release to
//     a WEAKER gate that knows nothing about the wrong-pool or cross-tenant sweeps — the
//     promote-gate-family-dispatch defect, reachable from a naming choice.
//
// `FLT-` collides with no prefix in the tree (counted 2026-07-30), and it is in extensionIDPrefixes so the
// orphan sweep still walks these directories. Ownership may live here; escaping the sweep may not.
//
// IT IS FIVE AND NOT SIX. The plan's FLT-006 ("a tool call runs on the runner's machine, and no credential
// crosses that boundary") describes T7, which the owner deferred. An id for a leg that did not run would be
// a green row for a claim nobody proved, so it is absent rather than authored — and the ceiling it would
// have carried is stated in this file's header instead.
var FleetCaseIDs = []string{"FLT-001", "FLT-002", "FLT-003", "FLT-004", "FLT-005"}

// FleetPeer is the ONE honest naming a FleetProof may carry. Every machine in this bundle is a fake runner
// built from the shipped enrollment package against the shipped gateway over a real mTLS wire and a real
// PostgreSQL — but on ONE box, in ONE process tree, on darwin/arm64. A bundle that cannot type the word
// "real" into this field cannot overclaim a fleet by accident.
const FleetPeer = "fake"

// fleetRunnerIDPrefix is the SERVER's mint prefix (execution.mintRunnerID). Group (g) re-derives that every
// id in the registry carries it, which is the mechanical form of "the id is not the machine's to choose".
const fleetRunnerIDPrefix = "rnr_"

// fleetRunnerDNSSuffix is the suffix execution.runnerDNS appends. Group (g)'s STRONG half is that the
// certificate's DNS is derived from the SERVER's id and not from the label the machine asked for — the
// defect T1 measured, where two ids were minted by two files that never met and every later lookup went
// through the SAN.
const fleetRunnerDNSSuffix = ".runners.palai.internal"

// --- (a)+(b) the offer ledger ----------------------------------------------------------------------------

// fleetOfferRow is one rendezvous decision: an attempt met a parked machine and was either OFFERED it or
// was not. Both tenants and both pools are carried on BOTH sides, so the gate can re-derive the two crown
// negatives without believing either number.
type fleetOfferRow struct {
	AttemptID    string `json:"attempt_id"`
	AttemptOrg   string `json:"attempt_organization_id"`
	AttemptProj  string `json:"attempt_project_id"`
	AttemptPool  string `json:"attempt_pool_id"`
	RunnerID     string `json:"runner_id"`
	RunnerOrg    string `json:"runner_organization_id"`
	RunnerProj   string `json:"runner_project_id"`
	RunnerPool   string `json:"runner_pool_id"`
	Offered      bool   `json:"offered"`
	LeaseGranted bool   `json:"lease_granted"`
}

// SweepFleetOffers decodes the offer ledger and returns the offers that crossed a POOL boundary and the
// offers that crossed a TENANT boundary — groups (a) and (b), re-derived from the carried rows rather than
// read off a declared count (the SweepActionableElements pattern).
//
// IT ALSO REPORTS THE THREE NON-VACUITY COUNTS, and they are the half that makes the zeros worth anything.
// A corpus in which no wrong-pool machine and no foreign-tenant machine was ever PRESENT satisfies both
// zeros trivially — and that is not a hypothetical shape, it is the shape the runner plane had before this
// epic, where a single unbuffered channel meant every parked machine satisfied every attempt. So the sweep
// insists on seeing a machine that was passed over for its pool, a machine that was passed over for its
// tenant, and a machine that was actually USED.
func SweepFleetOffers(ledger json.RawMessage) (wrongPool, crossTenant []string, matched, poolRefusals, tenantRefusals int, err error) {
	if len(ledger) == 0 {
		return nil, nil, 0, 0, 0, fmt.Errorf("no offer ledger to sweep: a placement count over nothing is vacuous")
	}
	var rows []fleetOfferRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("the carried offer ledger is not JSON, so \"no attempt was offered the wrong machine\" is unverifiable: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil, 0, 0, 0, fmt.Errorf("the offer ledger is EMPTY: a rendezvous that never met a machine cannot certify which machines it refuses")
	}
	for _, row := range rows {
		samePool := row.AttemptPool == row.RunnerPool
		sameTenant := row.AttemptOrg == row.RunnerOrg && row.AttemptProj == row.RunnerProj
		switch {
		case row.Offered && !samePool:
			wrongPool = append(wrongPool, fmt.Sprintf("%s(pool=%s) -> %s(pool=%s)", row.AttemptID, row.AttemptPool, row.RunnerID, row.RunnerPool))
		case row.Offered && !sameTenant:
			crossTenant = append(crossTenant, fmt.Sprintf("%s(%s/%s) -> %s(%s/%s)", row.AttemptID, row.AttemptOrg, row.AttemptProj, row.RunnerID, row.RunnerOrg, row.RunnerProj))
		case row.Offered && row.LeaseGranted:
			matched++
		case !row.Offered && !samePool:
			poolRefusals++
		case !row.Offered && !sameTenant:
			tenantRefusals++
		}
	}
	return wrongPool, crossTenant, matched, poolRefusals, tenantRefusals, nil
}

// --- (c) the run ledger ---------------------------------------------------------------------------------

// fleetRunRow is one placement outcome. CapacityPresent is whether the target pool held a usable machine
// when the run was placed; Woken records that a machine JOINING that pool is what moved it on.
type fleetRunRow struct {
	RunID           string `json:"run_id"`
	PoolID          string `json:"pool_id"`
	CapacityPresent bool   `json:"capacity_present"`
	State           string `json:"state"`
	Woken           bool   `json:"woken_by_a_machine_joining"`
}

// fleetDeadStates are the states a run cannot be in when the only thing wrong was that nobody was home.
var fleetDeadStates = map[string]bool{"dead_letter": true, "failed": true, "canceled": true, "cancelled": true}

// SweepFleetCapacityDeaths decodes the run ledger and returns the runs that DIED because their pool was
// empty — group (c). AWS documents a Mac host as taking "approximately 6 minutes to 20 minutes" to start
// and `Dial` gave it twenty seconds over five attempts, so before this epic a run placed in a pool waiting
// for a machine died four times before the machine booted. This counter is that repair, measured.
//
// The demonstrations matter as much as the zero: a run that actually PARKED (so parking is real rather than
// a state nothing reaches) and a run a machine joining WOKE and which then ran (so the park is not simply a
// nicer way to hang).
func SweepFleetCapacityDeaths(ledger json.RawMessage) (died []string, parked, woken int, err error) {
	if len(ledger) == 0 {
		return nil, 0, 0, fmt.Errorf("no run ledger to sweep: a capacity-death count over nothing is vacuous")
	}
	var rows []fleetRunRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return nil, 0, 0, fmt.Errorf("the carried run ledger is not JSON, so \"a run parks rather than dying\" is unverifiable: %w", err)
	}
	if len(rows) == 0 {
		return nil, 0, 0, fmt.Errorf("the run ledger is EMPTY: a corpus with no placed run cannot certify what happens to one")
	}
	for _, row := range rows {
		if !row.CapacityPresent && fleetDeadStates[row.State] {
			died = append(died, row.RunID+" (pool="+row.PoolID+", state="+row.State+")")
			continue
		}
		if !row.CapacityPresent && row.State == "waiting" {
			parked++
		}
		if row.Woken && !fleetDeadStates[row.State] && row.State != "waiting" {
			woken++
		}
	}
	return died, parked, woken, nil
}

// --- (d) the key-revocation fence, and it is this epic's cheapest security test -------------------------

// fleetCredentialRow is one presentation of a credential at the enrolment or renewal door. RevokedAt is the
// instant the KEY this machine came in on was revoked (empty for a key that was never revoked), so the
// sweep can partition every row by "before" and "after" without trusting a flag.
//
// At and RevokedAt are RFC3339 with a literal Z, which sorts lexicographically — the release index's own
// idiom, and the reason the comparison below is a string one.
type fleetCredentialRow struct {
	RunnerID    string `json:"runner_id"`
	KeyID       string `json:"enrolled_via_key_id"`
	Kind        string `json:"kind"` // enroll | renew
	At          string `json:"at"`
	RevokedAt   string `json:"key_revoked_at"`
	Outcome     string `json:"outcome"` // ok | refused
	LeaseIntact bool   `json:"lease_still_relaying"`
}

// SweepFleetKeyRevocation is group (d), AND IT IS THE ANSWER TO A QUESTION THE NEXT READER WILL ASK.
//
// When somebody says "if we revoked the key we should cut the connection too", this function is the reply:
// cutting would DELETE the exact property that makes easy enrolment safe. A pool key is meant to be pasted
// onto ten machines, which is only tolerable because revoking it costs an operator nothing — it stops new
// machines and stops no running work. Make revocation also a kill switch and every revoke becomes an
// outage, so nobody revokes, so the key stays live forever.
//
// So the proof does not merely assert "zero machines dropped". It COUNTS the renewals that happened AFTER
// the revocation instant and RE-DERIVES that every one of them SUCCEEDED. The structural reason it can:
// `handleRenew` authenticates with the certificate the machine already holds and the credential chain is
// not on that path at all.
//
// Returns: dropped — renewals after the revocation that were refused (MUST be empty); admitted — enrolments
// after the revocation that succeeded (MUST be empty, or the revocation did nothing); renewals — how many
// renewals happened after it (MUST be non-zero, or "all of them succeeded" is a statement about no rows);
// refused — how many enrolments it turned away (MUST be non-zero, same reason); leasesIntact — how many of
// those post-revocation renewals still held a relaying lease.
func SweepFleetKeyRevocation(ledger json.RawMessage) (dropped, admitted []string, renewals, refused, leasesIntact int, err error) {
	if len(ledger) == 0 {
		return nil, nil, 0, 0, 0, fmt.Errorf("no credential ledger to sweep: a revocation count over nothing is vacuous")
	}
	var rows []fleetCredentialRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("the carried credential ledger is not JSON, so \"revoking a key stops nobody who is already in\" is unverifiable: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil, 0, 0, 0, fmt.Errorf("the credential ledger is EMPTY: a corpus where no credential was ever presented cannot certify what revoking one does")
	}
	for _, row := range rows {
		// A row whose key was never revoked says nothing about revocation, and neither does one that
		// happened BEFORE it. Both are skipped rather than counted, so the two zeros below are statements
		// about the window that matters.
		if row.RevokedAt == "" || row.At <= row.RevokedAt {
			continue
		}
		switch row.Kind {
		case "renew":
			renewals++
			if row.Outcome != "ok" {
				dropped = append(dropped, row.RunnerID+" (renew at "+row.At+" -> "+row.Outcome+", key "+row.KeyID+" revoked at "+row.RevokedAt+")")
			}
			if row.LeaseIntact {
				leasesIntact++
			}
		case "enroll":
			if row.Outcome == "ok" {
				admitted = append(admitted, row.RunnerID+" (enrol at "+row.At+" on revoked key "+row.KeyID+")")
			} else {
				refused++
			}
		}
	}
	return dropped, admitted, renewals, refused, leasesIntact, nil
}

// --- (e) the lifecycle ledger ---------------------------------------------------------------------------

// fleetLifecycleRow is one machine's standing at one gateway GENERATION. Generation 1 is the process that
// took the decision; generation 2 is the process after a restart, which is the whole point: `revoked` was a
// process-local atomic.Bool, so a restart erased the revocation.
type fleetLifecycleRow struct {
	RunnerID    string `json:"runner_id"`
	Action      string `json:"action"` // revoke | cordon | resume | (empty: untouched)
	Generation  int    `json:"gateway_generation"`
	Reconnected bool   `json:"reconnected"`
	TookALease  bool   `json:"took_a_lease"`
}

// SweepFleetRevocationSurvival is group (e): a machine an operator REVOKED must not come back after the
// control plane restarts. It returns the revoked machines that did — and the two demonstrations without
// which the zero is free.
//
// refusedAfterRestart: a revoked machine that was actually TURNED AWAY by the second generation. Without
// one, nothing ever tried, and "zero came back" is a statement about an experiment nobody ran.
//
// servedAfterRestart: a machine that was NOT revoked and DID take a lease from that same second generation.
// Without one, a gateway that refused everybody would look identical to a gateway that refused the right
// machine — which is the difference between a revocation and an outage.
func SweepFleetRevocationSurvival(ledger json.RawMessage) (returned []string, refusedAfterRestart, servedAfterRestart int, err error) {
	if len(ledger) == 0 {
		return nil, 0, 0, fmt.Errorf("no lifecycle ledger to sweep: a survival count over nothing is vacuous")
	}
	var rows []fleetLifecycleRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return nil, 0, 0, fmt.Errorf("the carried lifecycle ledger is not JSON, so \"a revocation outlives the process\" is unverifiable: %w", err)
	}
	if len(rows) == 0 {
		return nil, 0, 0, fmt.Errorf("the lifecycle ledger is EMPTY: a corpus with no revoked machine cannot certify that a revocation survives anything")
	}
	for _, row := range rows {
		if row.Generation < 2 {
			continue // the deciding process proves nothing about durability
		}
		switch {
		case row.Action == "revoke" && (row.Reconnected || row.TookALease):
			returned = append(returned, fmt.Sprintf("%s (revoked, generation %d, reconnected=%t, lease=%t)", row.RunnerID, row.Generation, row.Reconnected, row.TookALease))
		case row.Action == "revoke":
			refusedAfterRestart++
		case row.TookALease:
			servedAfterRestart++
		}
	}
	return returned, refusedAfterRestart, servedAfterRestart, nil
}

// --- (g) the registry -----------------------------------------------------------------------------------

// fleetRegistryRow is one row of the runner registry as an operator reads it. RequestedLabel is what the
// enrolling machine ASKED to be called, which is the field that used to decide everything.
type fleetRegistryRow struct {
	RunnerID       string `json:"runner_id"`
	RequestedLabel string `json:"requested_label"`
	CertificateDNS string `json:"certificate_dns"`
	PoolID         string `json:"pool_id"`
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id"`
}

// SweepFleetRegistry is group (g): how many DISTINCT machines the registry carries, and that every id in it
// was minted by the SERVER. It returns the rows that fail either half.
//
// THE STRONG FORM IS THE COLLISION COUNT, NOT THE DISTINCT COUNT. Two machines with two different labels
// getting two rows proves nothing — that was true before this epic. What was NOT true is that two machines
// asking for the SAME name get two identities, and `deploy/compose/runner-entrypoint.sh` hardcodes
// `PALAI_RUNNER_ID=runner-local`, so `--scale runner=3` was three machines holding one certificate identity
// and `identity atomic.Pointer` was a single slot where the last writer won. So the sweep requires at least
// one label that TWO distinct ids came in under.
//
// And the DNS check is the T1 verification pass turned into a gate: two id minters that never met would
// satisfy "the id starts with rnr_" while the certificate named a different machine than the row did, and
// since every later lookup goes through the SAN, last_seen_at could never advance for anybody.
func SweepFleetRegistry(ledger json.RawMessage) (clientChosen []string, distinct, collidingLabels int, err error) {
	if len(ledger) == 0 {
		return nil, 0, 0, fmt.Errorf("no registry ledger to sweep: an inventory count over nothing is vacuous")
	}
	var rows []fleetRegistryRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return nil, 0, 0, fmt.Errorf("the carried registry ledger is not JSON, so \"the server mints every id\" is unverifiable: %w", err)
	}
	if len(rows) == 0 {
		return nil, 0, 0, fmt.Errorf("the registry ledger is EMPTY: an inventory of nothing is not a fleet")
	}
	ids := map[string]bool{}
	byLabel := map[string]map[string]bool{}
	for _, row := range rows {
		switch {
		case !strings.HasPrefix(row.RunnerID, fleetRunnerIDPrefix):
			clientChosen = append(clientChosen, row.RunnerID+" (id does not carry the server's "+fleetRunnerIDPrefix+" mint prefix)")
		case row.CertificateDNS != row.RunnerID+fleetRunnerDNSSuffix:
			clientChosen = append(clientChosen, row.RunnerID+" (certificate names "+row.CertificateDNS+", which is not derived from the row's id)")
		}
		ids[row.RunnerID] = true
		if byLabel[row.RequestedLabel] == nil {
			byLabel[row.RequestedLabel] = map[string]bool{}
		}
		byLabel[row.RequestedLabel][row.RunnerID] = true
	}
	for _, seen := range byLabel {
		if len(seen) > 1 {
			collidingLabels++
		}
	}
	return clientChosen, len(ids), collidingLabels, nil
}

// --- (h) the canonical vendor ledger --------------------------------------------------------------------

// FleetContracts is the CANONICAL ledger of the published vendor requirements and on-machine measurements
// E24 acted on — the ToolApprovalContracts / CodeAndShipContracts discipline. A proof's contracts must
// EQUAL this table, so a bundle cannot build a surface while quietly dropping the row that named its gap.
//
// THE TWO UNCONFIRMED ROWS ARE DELIBERATELY ABSENT AND THEIR ABSENCE IS THE HONEST ANSWER. §3.5 P12
// (Scaleway's claimed sub-minute Apple-silicon start and its automatic-delete option) and P13 (the
// concurrency and ordering semantics of Anthropic's Environments Work endpoints) could not be confirmed on
// any published page: the FAQ returned navigation only, and the work endpoints document no queue order. A
// ledger of "published requirements we implement" is the wrong home for two things nobody could read, and
// neither entered the code as an assumption — T4's park carries NO duration constant and T2 chose its own
// order explicitly. They live in docs/operations/known-gaps-1.0.md and are measured by §6 legs 3 and 4.
// TestTheFleetLedgerRefusesTheUnconfirmedRows pins it.
var FleetContracts = []ContractRequirement{
	{
		Divergence: "P1",
		SourceURL:  "https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes (fetched 2026-07-29)",
		Requirement: "⭐⭐ THE DEFINING DECISION OF THIS EPIC IS TAKEN FROM THE VENDOR'S OWN ARCHITECTURE, VERBATIM: " +
			"\"The `self_hosted` environment acts as a work queue: when a session is assigned to it, Anthropic " +
			"enqueues the session as a work item. Your worker claims work items from that queue, spawns an " +
			"execution context for each one, downloads the agent's skills, runs the tool calls, and posts the " +
			"results back.\" The whole page was fetched and searched and there is NOT ONE SENTENCE about routing " +
			"INSIDE an environment — the placement primitive IS the environment. So a pool is a QUEUE and a LABEL " +
			"and there is no routing in it, which is what E24 built on the runner plane and why it did not try to " +
			"revive WorkerSpec.PoolLabel on the capability-worker plane, where no run passes at all",
	},
	{
		Divergence: "P2",
		SourceURL:  "https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes (fetched 2026-07-29)",
		Requirement: "\"Work items are claimed by polling the environment's queue: either by an always-on worker that " +
			"polls continuously, or a webhook-triggered handler that wakes on `session.status_run_started` and " +
			"starts polling.\" OURS DOES NOT POLL AND THAT DIVERGENCE IS KEPT DELIBERATELY: the runner opens an " +
			"outbound WebSocket and PARKS, and the control plane pushes `lease.offer` to it, so queue latency is " +
			"not bounded by a poll interval and there is no losing herd from a poll with no SKIP LOCKED. E24 " +
			"turned the single unbuffered `available` channel into one channel per (tenant, pool) and kept the " +
			"push model rather than adopting the vendor's",
	},
	{
		Divergence: "P3",
		SourceURL:  "https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes (fetched 2026-07-29)",
		Requirement: "⭐ TWO CREDENTIALS, VERBATIM: \"an environment key (generated in the Console in the steps that " +
			"follow) authenticates the worker to its queue; your Claude API key creates sessions and reads queue " +
			"stats from outside the worker host. Key generation is Console-only.\" The separation is taken and is " +
			"STRONGER here: their environment key is the worker's CONTINUING identity, so a leaked one is " +
			"open-ended queue access; a leaked Palai pool key is ONE ENROLMENT and is revocable, because the key " +
			"only ever mints a certificate and is never presented again. \"Console-only\" is CLI-only for us — the " +
			"admin console has no authentication at all, so minting a pool key behind it would have invalidated " +
			"everything this epic built (§5, E25)",
	},
	{
		Divergence: "P4",
		SourceURL:  "https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes (fetched 2026-07-29)",
		Requirement: "The vendor's own warning: setting `ANTHROPIC_API_KEY` on the worker host \"exposes an " +
			"organization-scoped credential to agent tool calls\". THIS ROW IS ALSO WHY THIS PROOF HAS NO " +
			"CREDENTIAL-BYTES COUNTER. It is the requirement E24 T7's relay would have been measured against, and " +
			"T7 WAS DEFERRED BY THE OWNER — there is no relay, so there are no relay frames to sweep and a number " +
			"here would be fabricated. The credential boundary is unmoved: brokering stays control-plane-side and " +
			"no machine in this fleet is handed a credential, because no machine in this fleet runs a tool",
	},
	{
		Divergence: "P5",
		SourceURL:  "https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes (fetched 2026-07-29)",
		Requirement: "\"Then write a spawn script that forwards session details into a fresh sandbox\", with ten " +
			"platform integrations named (AWS Lambda MicroVMs, Blaxel, Cloudflare, Daytona, E2B, GKE Agent " +
			"Sandbox, Modal, Namespace, Superserve, Vercel). ALL TEN ARE ONE SPAWN SEAM and none of them is in " +
			"the vendor's core either. E24 does NOT open that seam, and the reason is the ordering rather than " +
			"the effort: spawning a machine is meaningless until a run can WAIT for it, and a run could not — it " +
			"died in about two and a half minutes. T4 fixed that, so the scaler is written ON TOP of this epic " +
			"(E26) rather than beside it",
	},
	{
		Divergence: "P6",
		SourceURL:  "https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes (fetched 2026-07-29)",
		Requirement: "Worker host requirements, verbatim: \"A Linux host with `/bin/bash` at that exact path. The " +
			"worker's bash tool invokes it directly, without consulting `PATH`.\" THE COMPETITOR'S SELF-HOSTED " +
			"WORKER IS LINUX-ONLY BY ITS OWN DOCUMENT, which is exactly the gap a Mac pool aims at. AND THE HONEST " +
			"HALF, WHICH THIS BUNDLE STATES RATHER THAN IMPLIES: the differentiator is NOT delivered here either. " +
			"E24 routes a run's ENGINE to a pool; every tool still executes in the control plane's process, so a " +
			"Mac is only a Mac when the control plane is ON it. That was T7's job and T7 was deferred",
	},
	{
		Divergence: "P7",
		SourceURL:  "https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes (fetched 2026-07-29)",
		Requirement: "Two-phase stopping, verbatim: \"Use `work.stop` to ask the worker handling a specific session " +
			"to shut it down. By default the work item moves to `stopping`: the worker notices on its next lease " +
			"heartbeat, cancels the session's in-flight tool call, and confirms the shutdown, at which point the " +
			"work item becomes `stopped`. Pass `force: true` … to mark the work item `stopped` immediately.\" The " +
			"gentle/forced pair is T5's state machine: `Cordon` leaves the machine connected and takes it out of " +
			"the rendezvous, `Revoke` is the hard stop. Both existed and NEITHER HAD A PRODUCTION CALLER — " +
			"`Revoke` and `Resume` had none anywhere and `Cordon`'s only caller was `Drain`'s own first line, " +
			"whose only caller was SIGTERM",
	},
	{
		Divergence: "P8",
		SourceURL:  "https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes (fetched 2026-07-29)",
		Requirement: "`reclaim_older_than_ms`: \"re-claim work items that were claimed but never acknowledged within " +
			"this many milliseconds\" (2000 in the SDK examples). E24 WRITES NO NEW RECLAIM. The equivalent is " +
			"already in this tree twice — the capability plane's RedispatchForRetry plus lease fence, and the E10 " +
			"recovery layer the gateway's drain hands off to — so T5 keyed the existing one to a runner id and " +
			"added a heartbeat reaper rather than a third mechanism. Two reclaim paths would have meant two " +
			"double-execution bugs",
	},
	{
		Divergence: "P9",
		SourceURL:  "https://aws.amazon.com/ec2/instance-types/mac/faqs/ (fetched 2026-07-29)",
		Requirement: "⭐ THE 24-HOUR FLOOR IS APPLE'S LICENCE AND NOT A VENDOR'S PRICING CHOICE, verbatim: \"Billing " +
			"is per second, with a 24-hour minimum allocation period for the Dedicated Host to comply with the " +
			"Apple macOS Software License Agreement\", and \"At the end of the 24-hour minimum allocation period, " +
			"the host can be released at any time with no further commitment.\" So a Mac pool has a FLOOR measured " +
			"in a day rather than a minute. Migration 000045 carries `runner_pools.min_size` for it and NOTHING " +
			"READS THAT COLUMN — a floor needs a loop that enforces it, and that loop is E26's",
	},
	{
		Divergence: "P10",
		SourceURL:  "https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-mac-instances.html (fetched 2026-07-29)",
		Requirement: "⭐ THE MEASUREMENT THAT MADE T4 EXIST, verbatim: \"For an AWS vended AMI with a x86 Mac instance " +
			"or a Apple silicon Mac instance, the launch time can range from approximately 6 minutes to 20 " +
			"minutes.\" Also: \"Mac instances are available only as bare metal instances on Dedicated Hosts, with " +
			"a minimum allocation period of 24 hours before you can release the Dedicated Host. You can launch " +
			"one Mac instance per Dedicated Host.\" A START THAT TAKES 18 TO 60 TIMES THE 20-SECOND DIAL BUDGET " +
			"means \"bring a Mac up when load arrives\" was not an expensive choice but an UNREACHABLE one: with " +
			"five attempts the run dead-lettered in about two and a half minutes, four deaths before the machine " +
			"booted. The fix is not a bigger timeout, it is a run that PARKS — E23 T1's choreography, used a " +
			"second time",
	},
	{
		Divergence: "P11",
		SourceURL:  "https://www.scaleway.com/en/developers/api/apple-silicon (fetched 2026-07-29)",
		Requirement: "The floor is READABLE AS A FIELD rather than computable: `\"deletable_at\": " +
			"\"2022-03-22T12:34:56.123456Z\"`, and \"Apple silicon-as-a-Service comes with a minimum allocation " +
			"period of 24 hours.\" A scaler must READ `deletable_at` instead of computing `now + 24h`, because " +
			"clock skew and per-provider differences break the arithmetic. E24 carries no start-time and no " +
			"floor-time constant at all — the park is INDEFINITE and rests on no duration — which is what leaves " +
			"this row implementable in E26 without unpicking anything here",
	},
	{
		Divergence: "P14",
		SourceURL:  "MEASURED (E22 §3.5, inherited unchanged): docs/research/macos-isolation-without-accounts.md §6, re-read 2026-07-30",
		Requirement: "`simctl --set` is an ARGV flag, argv belongs to the model, and per-session device separation is " +
			"therefore ADVISORY rather than enforced. E24 DOES NOT CHANGE THIS AND DOES NOT CLAIM TO: a Mac pool " +
			"is now tenant-scoped by RLS, so tenant A's attempt is never offered tenant B's machine — but the " +
			"INSIDE of one Mac is still a single uid, so two customers sharing one Mac share an account. MAC-P6 " +
			"stays open, verbatim, and the per-machine question is the owner's to answer",
	},
}

// fleetContractParts flattens the canonical ledger into hashParts input, so the digest is re-derivable from
// the CODE table alone and a bundle cannot present a self-consistent digest over an edited ledger.
func fleetContractParts() []string {
	parts := make([]string, 0, 3*len(FleetContracts))
	for _, req := range FleetContracts {
		parts = append(parts, req.Divergence, req.SourceURL, req.Requirement)
	}
	return parts
}

// FleetContractsDigest is hashParts over the CANONICAL contract ledger — the E24 bundle's checksum anchor. A
// dropped or reworded §3.5 row moves every checksum in the release.
func FleetContractsDigest() string { return hashParts(fleetContractParts()...) }

// --- the proof -------------------------------------------------------------------------------------------

// FleetProof is the evidence a fleet_claim requires (plan §T8 — the E24 EXIT anchor). Its groups are the
// plan's (a)..(h) with ONE deliberate hole:
//
//	(a) OfferLedger / OffersToTheWrongPool (MUST be zero, RE-DERIVED) — and the ledger must show a machine
//	    that was PASSED OVER for its pool, or the zero is a statement about a corpus with no wrong machine
//	    in it;
//	(b) OffersAcrossTenants (MUST be zero, RE-DERIVED from the same ledger) — same non-vacuity rule;
//	(c) RunLedger / RunsDeadLetteredForAbsentCapacity (MUST be zero, RE-DERIVED) — plus a run that really
//	    parked and a run a joining machine really woke;
//	(d) CredentialLedger / EnrolledRunnersDroppedByAKeyRevocation (MUST be zero, RE-DERIVED) — with the
//	    fence described on SweepFleetKeyRevocation: the renewals AFTER the revocation are counted and every
//	    one must have SUCCEEDED;
//	(e) LifecycleLedger / RevokedRunnersThatCameBack (MUST be zero, RE-DERIVED) — after a control-plane
//	    RESTART, with a machine that was refused and a machine that was served by that same new process;
//	(f) ABSENT, and its absence is the honest answer rather than an omission. The plan's (f) counted
//	    credential bytes in the execution relay's frames. THE OWNER DEFERRED T7, so there is no relay and
//	    there are no frames; a number here would be measuring a leg that never ran. See §3.5 P4.
//	(g) RegistryLedger / DistinctRunners / every id SERVER-MINTED (RE-DERIVED) — and the strong form: two
//	    machines that asked for the SAME label hold two identities;
//	(h) Contracts — every vendor requirement and on-machine measurement with its source, fetch date and
//	    §3.5 divergence id.
//
// HONEST CEILING, MECHANICALLY ENFORCED: Peer must be the literal "fake". This bundle is STRUCTURALLY
// incapable of claiming two physical machines, and it cannot claim remote EXECUTION at all — see the file
// header, which is where the sentence that matters most lives.
type FleetProof struct {
	Peer string `json:"peer"`

	// (a)+(b) Placement is a refusal, and the tenant is on the runner plane at last.
	OfferLedger          json.RawMessage `json:"offer_ledger"`
	OffersToTheWrongPool int             `json:"offers_to_the_wrong_pool"`
	OffersAcrossTenants  int             `json:"offers_across_tenants"`

	// (c) Capacity absence parks a run; it does not kill one.
	RunLedger                         json.RawMessage `json:"run_ledger"`
	RunsDeadLetteredForAbsentCapacity int             `json:"runs_dead_lettered_for_absent_capacity"`

	// (d) Revoking a KEY stops new machines and stops nobody who is already in.
	CredentialLedger                       json.RawMessage `json:"credential_ledger"`
	EnrolledRunnersDroppedByAKeyRevocation int             `json:"enrolled_runners_dropped_by_a_key_revocation"`
	RenewalsAfterTheRevocation             int             `json:"renewals_after_the_revocation"`

	// (e) Revoking a MACHINE outlives the process that decided it.
	LifecycleLedger            json.RawMessage `json:"lifecycle_ledger"`
	RevokedRunnersThatCameBack int             `json:"revoked_runners_that_came_back"`

	// (g) The inventory, and who names the machines in it.
	RegistryLedger  json.RawMessage `json:"registry_ledger"`
	DistinctRunners int             `json:"distinct_runners"`

	// (h) The published contracts and measurements, anchored to the code table.
	Contracts       []ContractRequirement `json:"contracts"`
	ContractsDigest string                `json:"contracts_digest"`
}

// Complete reports the groups hold on a FAKE peer AND re-derives (a), (b), (c), (d), (e) and (g) from the
// bytes the proof carries. A proof that declares zero wrong-pool offers over a ledger containing one, or a
// registry of "two machines" that is one row twice, fails HERE — in the shape verifier — rather than in a
// dedicated test somebody could forget to run.
func (p FleetProof) Complete() bool {
	if p.Peer != FleetPeer || p.ContractsDigest != FleetContractsDigest() ||
		!slices.Equal(p.Contracts, FleetContracts) {
		return false
	}
	// (a)+(b) Placement refused the wrong pool and the wrong tenant, and had the chance to do otherwise.
	wrongPool, crossTenant, matched, poolRefusals, tenantRefusals, err := SweepFleetOffers(p.OfferLedger)
	if err != nil || len(wrongPool) != 0 || len(crossTenant) != 0 ||
		p.OffersToTheWrongPool != 0 || p.OffersAcrossTenants != 0 {
		return false
	}
	if matched < 1 || poolRefusals < 1 || tenantRefusals < 1 {
		return false // a rendezvous that never had a wrong machine to refuse, or never used a right one
	}
	// (c) No run died of an empty pool, one parked, and one woke.
	died, parked, woken, err := SweepFleetCapacityDeaths(p.RunLedger)
	if err != nil || len(died) != 0 || p.RunsDeadLetteredForAbsentCapacity != 0 {
		return false
	}
	if parked < 1 || woken < 1 {
		return false // the zero is free over a corpus where nothing ever waited or nothing was ever woken
	}
	// (d) THE FENCE. Every renewal after the revocation succeeded, and the revocation still refused an
	// enrolment — the two halves that make "revoking a key is cheap" true rather than merely claimed.
	dropped, admitted, renewals, refused, leasesIntact, err := SweepFleetKeyRevocation(p.CredentialLedger)
	if err != nil || len(dropped) != 0 || len(admitted) != 0 ||
		p.EnrolledRunnersDroppedByAKeyRevocation != 0 {
		return false
	}
	if renewals < 1 || refused < 1 || leasesIntact < 1 || p.RenewalsAfterTheRevocation != renewals {
		return false
	}
	// (e) A revoked machine stayed out across a restart, and an unrevoked one still got served.
	returned, refusedAfterRestart, servedAfterRestart, err := SweepFleetRevocationSurvival(p.LifecycleLedger)
	if err != nil || len(returned) != 0 || p.RevokedRunnersThatCameBack != 0 {
		return false
	}
	if refusedAfterRestart < 1 || servedAfterRestart < 1 {
		return false
	}
	// (g) The inventory is more than one machine, every id is the server's, and a shared label still
	// produced two identities.
	clientChosen, distinct, collidingLabels, err := SweepFleetRegistry(p.RegistryLedger)
	if err != nil || len(clientChosen) != 0 || distinct < 2 || collidingLabels < 1 ||
		p.DistinctRunners != distinct {
		return false
	}
	return true
}

// carriesE24FleetCase reports whether a case is one of the five ids E24 OPENED — the FAMILY marker, shared
// by the manifest verifier and PromoteGateFor so the two can never disagree about what an E24 release is.
//
// THE FAMILY IS RECOGNIZED BY THE CASE IDS, NEVER BY THE fleet_claim THE GATE ENFORCES. Dispatching on the
// claim marker is precisely how a release DROPS it, reroutes to a weaker family and passes — the defect the
// E17 dispatch comment describes and this repository has shipped once already.
func carriesE24FleetCase(c evidenceCase) bool {
	return slices.Contains(FleetCaseIDs, c.ID)
}

// verifyE24FleetPresence stops the re-derivations from being OPTIONAL: a manifest carrying ANY of the five
// E24 cases MUST carry EXACTLY ONE fleet_claim with its proof. "Exactly one" because FleetPromoteGate
// judges the first while this verifier checks all of them, so a second fabricated proof could ride behind
// an honest one.
func verifyE24FleetPresence(m evidenceManifest) []Finding {
	family, claims, withProof := false, 0, 0
	for _, c := range m.Cases {
		if carriesE24FleetCase(c) {
			family = true
		}
		if c.FleetClaim != "" {
			claims++
			if c.FleetProof != nil {
				withProof++
			}
		}
	}
	if !family {
		return nil
	}
	switch {
	case claims == 0:
		return []Finding{{Kind: "missing", Detail: fmt.Sprintf(
			"fleet_claim (this manifest carries E24 case(s) from %v, so it must carry the anchor that judges them — without it \"no attempt was offered the wrong machine\", \"no run died of an empty pool\" and \"a revocation outlives the process\" ship unverified behind five green rows; plan §T8)",
			FleetCaseIDs)}}
	case claims > 1:
		return []Finding{{Kind: "invalid", Detail: fmt.Sprintf(
			"%d fleet_claims in one manifest (want exactly 1): FleetPromoteGate judges the FIRST fleet proof, so a second could ride behind an honest one — one release, one re-derivation (plan §T8)", claims)}}
	case withProof != claims:
		return []Finding{{Kind: "missing", Detail: "fleet_proof (the fleet claim carries no proof body)"}}
	}
	return nil
}

// --- the canonical bytes the proof carries ---------------------------------------------------------------
//
// THE LEDGERS ARE AUTHORED, AND WHAT MAKES THEM HONEST IS THE CO-RUN RATHER THAN THIS FILE. Each row below
// records an outcome one of the SHIPPED component suites produces against a real PostgreSQL, a real mTLS
// wire and the real gateway — the suites `scripts/uat/fleet` runs in the SAME invocation that verifies this
// bundle. The gate's job is not to believe the rows: it is to RE-DERIVE every counter from them, so a row
// edited to flatter the release moves a count and the tag is refused. Each row names the test that produced
// it, because a ledger with untraceable provenance is a number with a story attached.
//
// The machine ids are the shape middleware.NewID("rnr") produces (prefix, underscore, 32 hex), and every
// certificate DNS is that id plus runnerDNSSuffix — which SweepFleetRegistry RE-DERIVES rather than
// pattern-matches, because T1's defect was two id minters that agreed on the shape and not the value.
// THE IDS BELOW ARE INLINE RATHER THAN NAMED CONSTANTS, and the reason is that a constant would be a second
// copy: `certificate_dns` in the registry ledger repeats its row's id on purpose, so that SweepFleetRegistry
// can RE-DERIVE the derivation rather than pattern-match it. Two names for one value is how those two stop
// agreeing quietly. The machines are: `rnr_4c1f…` and `rnr_9e3b…`, which BOTH asked to be called
// `runner-local` (deploy/compose/runner-entrypoint.sh hardcodes that name, and `--scale runner=3` is how an
// operator actually arrives at a fleet — they are the collision the registry sweep insists on seeing);
// `rnr_2a7d…`, the Mac-pool machine whose pool key is later revoked; `rnr_bd41…`, the machine an operator
// REVOKED; and `rnr_71c6…`, the machine that tries to enrol on the revoked key afterwards. Tenant beta exists
// for ONE reason: before this epic the runner plane had no tenant on it at all, so tenant beta's attempt
// taking tenant alpha's machine was not a bug but the definition.

// FleetOfferLedger is group (a)+(b)'s carried bytes: five rendezvous decisions.
//
// ROW 4 IS THE ONE TO READ AND ITS ENCODING IS DELIBERATE. Both sides carry the pool as an OPERATOR names it,
// so tenant B's attempt and tenant A's machine both read `mac-pool` — which means pool identity alone WOULD
// have matched them and the TENANT is what refused. Encoding it the other way (two different pool ids) would
// have let the pool key take credit for the tenant's refusal, and the tenant is the half that did not exist
// on this plane at all.
const FleetOfferLedger = `[
  {"attempt_id":"att_mac_01","attempt_organization_id":"org_alpha","attempt_project_id":"proj_alpha","attempt_pool_id":"mac-pool","runner_id":"rnr_2a7d5c0e918b46f3ac52d7b1e60498fa","runner_organization_id":"org_alpha","runner_project_id":"proj_alpha","runner_pool_id":"mac-pool","offered":true,"lease_granted":true},
  {"attempt_id":"att_mac_01","attempt_organization_id":"org_alpha","attempt_project_id":"proj_alpha","attempt_pool_id":"mac-pool","runner_id":"rnr_4c1f9a7d2b8e6053a1d4f70c92be5837","runner_organization_id":"org_alpha","runner_project_id":"proj_alpha","runner_pool_id":"linux-pool","offered":false,"lease_granted":false},
  {"attempt_id":"att_linux_02","attempt_organization_id":"org_alpha","attempt_project_id":"proj_alpha","attempt_pool_id":"linux-pool","runner_id":"rnr_4c1f9a7d2b8e6053a1d4f70c92be5837","runner_organization_id":"org_alpha","runner_project_id":"proj_alpha","runner_pool_id":"linux-pool","offered":true,"lease_granted":true},
  {"attempt_id":"att_beta_03","attempt_organization_id":"org_beta","attempt_project_id":"proj_beta","attempt_pool_id":"mac-pool","runner_id":"rnr_2a7d5c0e918b46f3ac52d7b1e60498fa","runner_organization_id":"org_alpha","runner_project_id":"proj_alpha","runner_pool_id":"mac-pool","offered":false,"lease_granted":false},
  {"attempt_id":"att_default_04","attempt_organization_id":"org_alpha","attempt_project_id":"proj_alpha","attempt_pool_id":"pool_default","runner_id":"rnr_9e3b60a4d7c25f18b0a6e34c71df8925","runner_organization_id":"org_alpha","runner_project_id":"proj_alpha","runner_pool_id":"pool_default","offered":true,"lease_granted":true}
]`

// FleetRunLedger is group (c)'s carried bytes: four placements.
//
// Row 2 is the park (TestPlacementParksARunWhosePoolHasNoRunner) and row 3 is the wake
// (TestPlacementWakesAParkedRunWhenAMachineJoinsItsPool). Row 4 is the one an operator will want: a run
// waiting for some OTHER reason, untouched, which is the guard that makes waking safe to attempt after every
// single connect (TestPlacementWakeLeavesEveryOtherWaitingRunAlone).
const FleetRunLedger = `[
  {"run_id":"run_flt_placed_mac","pool_id":"mac-pool","capacity_present":true,"state":"completed","woken_by_a_machine_joining":false},
  {"run_id":"run_flt_parked_empty_pool","pool_id":"mac-pool","capacity_present":false,"state":"waiting","woken_by_a_machine_joining":false},
  {"run_id":"run_flt_woken_by_a_machine","pool_id":"mac-pool","capacity_present":false,"state":"completed","woken_by_a_machine_joining":true},
  {"run_id":"run_flt_waiting_on_an_approval","pool_id":"pool_default","capacity_present":true,"state":"waiting","woken_by_a_machine_joining":false}
]`

// FleetCredentialLedger is group (d)'s carried bytes, AND IT IS THIS EPIC'S CHEAPEST SECURITY TEST.
//
// The key `key_mac_01` is revoked at 12:00:00Z. Rows 2 and 3 are the machine that came in ON that key
// renewing TWICE afterwards, both successfully, both with the in-flight lease still relaying
// (TestPoolKeyRevocationLeavesAnEnrolledRunnerRenewing, TestPoolKeyRevocationDoesNotCutAnInFlightLease). Row
// 4 is the enrolment the revoked key was refused (TestPoolKeyRevocationRefusesANewEnrolment). Row 5 carries a
// key that was NEVER revoked, so the sweep's "skip everything outside the window" branch has something to
// skip — a partition rule that is never exercised is a partition rule nobody has seen work.
const FleetCredentialLedger = `[
  {"runner_id":"rnr_2a7d5c0e918b46f3ac52d7b1e60498fa","enrolled_via_key_id":"key_mac_01","kind":"enroll","at":"2026-07-30T11:00:00Z","key_revoked_at":"2026-07-30T12:00:00Z","outcome":"ok","lease_still_relaying":false},
  {"runner_id":"rnr_2a7d5c0e918b46f3ac52d7b1e60498fa","enrolled_via_key_id":"key_mac_01","kind":"renew","at":"2026-07-30T12:05:00Z","key_revoked_at":"2026-07-30T12:00:00Z","outcome":"ok","lease_still_relaying":true},
  {"runner_id":"rnr_2a7d5c0e918b46f3ac52d7b1e60498fa","enrolled_via_key_id":"key_mac_01","kind":"renew","at":"2026-07-30T12:10:00Z","key_revoked_at":"2026-07-30T12:00:00Z","outcome":"ok","lease_still_relaying":true},
  {"runner_id":"rnr_71c6d802fa4b39e5c18d05a7b2439ef4","enrolled_via_key_id":"key_mac_01","kind":"enroll","at":"2026-07-30T12:15:00Z","key_revoked_at":"2026-07-30T12:00:00Z","outcome":"refused","lease_still_relaying":false},
  {"runner_id":"rnr_4c1f9a7d2b8e6053a1d4f70c92be5837","enrolled_via_key_id":"key_linux_02","kind":"renew","at":"2026-07-30T12:20:00Z","key_revoked_at":"","outcome":"ok","lease_still_relaying":true}
]`

// FleetLifecycleLedger is group (e)'s carried bytes, across two gateway GENERATIONS on the same CA and the
// same database — which is what a control-plane restart is.
//
// Row 1 is generation 1, where the revoked machine was still connected when the decision landed; the sweep
// SKIPS it, because the deciding process proves nothing about durability. Rows 2 and 3 are the pair that
// discriminates (TestLifecycleRevocationSurvivesAControlPlaneRestart): the revoked machine is refused by the
// new process AND the unrevoked one still takes a lease through it. Rows 4 and 5 are the cordon half
// (TestLifecycleCordonSurvivesAControlPlaneRestartAndResumeClearsIt) — connected, taking nothing, then put
// back by a resume.
const FleetLifecycleLedger = `[
  {"runner_id":"rnr_bd41ef07c3925a68d0fb47e21c8a3d56","action":"revoke","gateway_generation":1,"reconnected":true,"took_a_lease":false},
  {"runner_id":"rnr_bd41ef07c3925a68d0fb47e21c8a3d56","action":"revoke","gateway_generation":2,"reconnected":false,"took_a_lease":false},
  {"runner_id":"rnr_4c1f9a7d2b8e6053a1d4f70c92be5837","action":"","gateway_generation":2,"reconnected":true,"took_a_lease":true},
  {"runner_id":"rnr_9e3b60a4d7c25f18b0a6e34c71df8925","action":"cordon","gateway_generation":2,"reconnected":true,"took_a_lease":false},
  {"runner_id":"rnr_9e3b60a4d7c25f18b0a6e34c71df8925","action":"resume","gateway_generation":2,"reconnected":true,"took_a_lease":true}
]`

// FleetRegistryLedger is group (g)'s carried bytes: three machines, two of which asked for the SAME name.
//
// THE COLLISION IS THE CLAIM. Two machines with two labels getting two rows was already true before this
// epic; what was not is that two machines asking for `runner-local` — the name compose hardcodes — hold two
// identities, and that each certificate's DNS is derived from the SERVER's id rather than from the label the
// machine sent (TestTwoMachinesClaimingOneRunnerIDGetSeparateIdentities,
// TestTheEnrolledRowIsTheOneTheCertificateFindsAgain).
const FleetRegistryLedger = `[
  {"runner_id":"rnr_4c1f9a7d2b8e6053a1d4f70c92be5837","requested_label":"runner-local","certificate_dns":"rnr_4c1f9a7d2b8e6053a1d4f70c92be5837.runners.palai.internal","pool_id":"linux-pool","organization_id":"org_alpha","project_id":"proj_alpha"},
  {"runner_id":"rnr_9e3b60a4d7c25f18b0a6e34c71df8925","requested_label":"runner-local","certificate_dns":"rnr_9e3b60a4d7c25f18b0a6e34c71df8925.runners.palai.internal","pool_id":"pool_default","organization_id":"org_alpha","project_id":"proj_alpha"},
  {"runner_id":"rnr_2a7d5c0e918b46f3ac52d7b1e60498fa","requested_label":"mac-mini-studio-1","certificate_dns":"rnr_2a7d5c0e918b46f3ac52d7b1e60498fa.runners.palai.internal","pool_id":"mac-pool","organization_id":"org_alpha","project_id":"proj_alpha"}
]`
