// Package execution's orchestrator is the single response kernel: it drives a run
// through the canonical state machine and a live engine channel, committing every
// provider and tool result before it reaches the engine. It writes no second agent
// loop — the engine owns the loop; the orchestrator only correlates requests, commits
// state, and dispatches (spec §24.7, §25.10).
package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/coordinator/recovery"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	"github.com/palgroup/palai/packages/runner"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

const engineProtocol = "engine.v1"

// dialHandshakeDeadline bounds one attempt's engine dial + engine.ready handshake. It is
// shorter than the 30s worker lease the dispatcher grants (main.go startDispatch) so a
// stuck dial fails the attempt — routed through the existing retry / dead-letter path —
// well before the lease lapses, turning a silent hang into a classified, retryable failure.
const dialHandshakeDeadline = 20 * time.Second

// pauseDrainDeadline bounds the pause checkpoint drain (SES-009, brief fork-1(i)): the controller
// asks the engine for a checkpoint of the pause boundary and drains in-flight frames until the offer
// arrives. A wedged-but-live engine that never offers fails the attempt here rather than hanging
// until lease reclaim. Generous relative to a single-threaded engine's synchronous offer.
const pauseDrainDeadline = 10 * time.Second

// Orchestrator executes response run attempts through the common kernel.
type Orchestrator struct {
	store  *store.Store
	spine  *coordinator.Store
	dialer EngineDialer
	models *modelbroker.Broker
	tools  *toolbroker.Broker
	route  ModelRoute
	// shell runs argv commands for the workspace shell tool inside the sandbox (spec §28.8). Nil
	// when no sandbox driver is wired into this control plane — a shell tool call then fails
	// cleanly rather than escaping. main.go injects it via SetShellRunner where a driver exists.
	shell toolbroker.ShellRunner
	// tasks is the durable session-scoped task/todo registry the task/todo tools persist through
	// (spec §11). It is always the spine (the control plane owns the DB), so it is wired at
	// construction; a stack opts into the durable primitives by registering the task/todo tools.
	tasks toolbroker.TaskRegistry
	// publications is the durable publication registry the push/PR tools record a pending approval
	// through (spec §30.8). Like tasks it is always the spine (the control plane owns the DB), so it is
	// wired at construction; a stack opts in by registering the push/PR tools.
	publications toolbroker.PublicationRegistry
	// publisher executes approved publications (push branch / open PR) at a safe boundary (spec
	// §30.9-30.10). Nil disables the approval pump — a stack with no repository publication wired
	// (every non-publication test) simply never publishes. main.go injects it via SetPublisher.
	publisher Publisher
	// provisionRoot + provisionBroker drive the root run's workspace auto-provisioning (spec §29.7-30.3,
	// E09 Task 10): the host dir allocations are minted under, and the broker the clone's read credential
	// comes from. Both unset ⇒ no provisioning (a run with a binding gets no workspace, tools fail clean).
	// main.go injects them env-gated via SetWorkspaceProvisioner.
	provisionRoot   string
	provisionBroker repositories.Broker
	// artifacts is the object-store write-path the finalize changeset compile persists the patch +
	// test-log through (spec §30.6). Nil ⇒ no changeset is compiled (a stack with no artifact store
	// wired). main.go injects it via SetChangesetWriter.
	artifacts ArtifactWriter
	// checkpoints persists an engine checkpoint.offer as a durable recovery object (spec §26.1-26.2).
	// Nil ⇒ no object store wired (every non-S3 stack): a checkpoint offer is advisory and dropped,
	// no durable boundary is created. main.go injects it via SetCheckpointSink.
	checkpoints *CheckpointSink
	// snapshots cuts + restores the workspace byte-archive a pause-boundary checkpoint links (spec
	// §26.4, §29.10, SES-009). Nil ⇒ no snapshot is cut at a pause (a stack with no object store, or no
	// workspace): the checkpoint then declares no workspace dependency, unchanged from T4. main.go
	// injects it via SetSnapshotSink alongside the checkpoint sink.
	snapshots *SnapshotSink
	// reconstructionForbidden is the §26.3 policy knob: when set, an incompatible checkpoint fails the
	// run EXPLICITLY rather than falling to transcript reconstruction (spec §26.3 rung 4). ponytail:
	// a plain bool setter, model-route pattern; DB-backed recovery policy is another epic. Default
	// false — reconstruction is allowed.
	reconstructionForbidden bool
	// hooks fires the run's registered extension hooks at the five pinned dispatch points (spec §28.17,
	// E12 T8). Nil ⇒ no hooks fire (a stack with no hook registry wired, or every pre-T8 test): the
	// dispatch is bit-unchanged. main.go injects it via SetHookFirer.
	hooks HookFirer
	// DialHandshakeDeadline bounds the dial + engine.ready handshake per attempt. Zero uses
	// dialHandshakeDeadline; NewOrchestrator sets the default. Tests shorten it.
	DialHandshakeDeadline time.Duration
}

// HookFirer runs a run's registered hooks at a dispatch point and returns the verdict (spec §28.17, E12 T8).
// *extensions.Store satisfies it; a test fakes it. The orchestrator depends only on this narrow seam so it
// stays free of the registry's DB + transport mechanics.
type HookFirer interface {
	Fire(ctx context.Context, ev extensions.HookEvent) (extensions.HookOutcome, error)
}

// NewOrchestrator binds the durable store, the engine dialer, and the model and tool
// brokers into one kernel. The model route defaults to the deterministic fake provider;
// main.go overrides it for a live provider via SetModelRoute.
func NewOrchestrator(st *store.Store, dialer EngineDialer, models *modelbroker.Broker, tools *toolbroker.Broker) *Orchestrator {
	return &Orchestrator{store: st, spine: st.Spine(), dialer: dialer, models: models, tools: tools, tasks: newTaskRegistry(st.Spine()), publications: newPublicationRegistry(st.Spine()), route: defaultModelRoute, DialHandshakeDeadline: dialHandshakeDeadline}
}

// SetModelRoute points the kernel at a non-default provider/model/secret selected by the
// composition root (main.go) from the environment. ponytail: a setter, not a model_routes
// lookup — the DB-backed routing is the deferred E-series carve-out.
func (o *Orchestrator) SetModelRoute(r ModelRoute) { o.route = r }

// SetShellRunner injects the sandbox shell runner the workspace shell tool executes through. Left
// unset, a shell tool call fails cleanly (no runner) rather than escaping the sandbox.
func (o *Orchestrator) SetShellRunner(s toolbroker.ShellRunner) { o.shell = s }

// SetHookFirer injects the hook dispatcher the five pinned points fire through (spec §28.17, E12 T8). Left
// unset, no hook fires — the dispatch is bit-unchanged (the same discipline as SetShellRunner/SetPublisher).
// It also propagates the firer to the publication registry, so the before_repository_publish point fires
// from inside the publish tool's RequestPublication.
func (o *Orchestrator) SetHookFirer(h HookFirer) {
	o.hooks = h
	if pr, ok := o.publications.(*publicationRegistry); ok {
		pr.hooks = h
	}
}

// SetChangesetWriter injects the object-store write-path the finalize changeset compile persists the
// patch + test-log through (spec §30.6). Left unset, a terminated coding run compiles no changeset —
// the same discipline as SetPublisher.
func (o *Orchestrator) SetChangesetWriter(aw ArtifactWriter) { o.artifacts = aw }

// SetCheckpointSink injects the checkpoint persistence path (spec §26.1-26.2). Left unset, a
// checkpoint.offer is dropped (no durable boundary) — the same discipline as SetChangesetWriter.
func (o *Orchestrator) SetCheckpointSink(cs *CheckpointSink) { o.checkpoints = cs }

// SetSnapshotSink injects the workspace snapshot capture/restore path (spec §29.10, SES-009). Left
// unset, no boundary snapshot is cut at a pause — the checkpoint declares no workspace dependency, the
// T4 behaviour. Wired alongside SetCheckpointSink where an object store is configured.
func (o *Orchestrator) SetSnapshotSink(ss *SnapshotSink) { o.snapshots = ss }

// SetReconstructionForbidden sets the §26.3 policy: when true, an incompatible checkpoint fails the
// run explicitly rather than reconstructing from the transcript (spec §26.3 rung 4).
func (o *Orchestrator) SetReconstructionForbidden(forbidden bool) {
	o.reconstructionForbidden = forbidden
}

// attemptState is the per-attempt working set threaded through the dispatch handlers.
type attemptState struct {
	attempt        AttemptDescriptor
	tenant         coordinator.Tenant
	sessionID      string
	responseID     string
	ch             EngineChannel
	ledger         *runner.FrameLedger
	seq            int // controller frame sequence (engine ignores it; kept envelope-valid)
	lastInboundSeq int // last accepted engine frame sequence; the intake requires the next to be +1
	output         []contracts.ContentItem
	usage          contracts.Usage
	model          string // the actually-used model from the latest committed model result
	// Delegation state (spec §25.18-19). depth is this run's depth (a child's is parent+1);
	// childModel/childBudget route a ChildRun's own model call; budget/budgetBounded is the
	// parent budget children intersect against; childReserved is the effective budget already
	// handed to dispatched children (so the next child intersects the depleting remainder);
	// childRunIDs are the children this attempt dispatched (fan-out count + final-output linkage).
	depth         int
	childModel    string
	childBudget   int
	budget        int
	budgetBounded bool
	childReserved int
	childRunIDs   []string
	// Workspace provisioning state (spec §29.7-29.8, E09 Task 10): the logical workspace the root run
	// provisioned and its writer lease, released at attempt end. Empty on a run with no attached binding.
	workspaceID      string
	workspaceLeaseID string
	// Engine handshake identity, captured from engine.ready — the §26.2 checkpoint provenance the
	// engine's opaque offer does not carry.
	engineVersion   string
	protocolVersion string
	// Recovery state (spec §26.3-26.9, E10 T4). restored marks an attempt started from a compatible
	// checkpoint (run.restore, not run.start): it resumed PAST every prior step, so every boundary is
	// live. committedStepWatermark is M — the committed model steps at attempt start; on a
	// reconstruction the engine re-walks steps 1..M as replays, so a fresh effect must wait for the
	// boundary preceding step M+1 (the first LIVE step). modelStepIndex counts the model.requests
	// this attempt has dispatched (== the engine step number on the run.start path).
	restored               bool
	committedStepWatermark int
	modelStepIndex         int
	attemptStart           time.Time
	// lastModelRequestID is the model_request_id of the step whose model.result produced the tool calls
	// this attempt is now dispatching — the commit_boundary a side-effecting tool's durable pre-write
	// records (spec §26.6, E12 T4), so an async-callback ledger row is keyed to the boundary it belongs to.
	lastModelRequestID string
}

// budgetRemaining reports the parent budget a child may still intersect against: the total less
// this run's own model spend and the budget already reserved to earlier children. Meaningful only
// when bounded; an unbounded parent passes its children's requests through untouched.
func (st *attemptState) budgetRemaining() (int, bool) {
	if !st.budgetBounded {
		return 0, false
	}
	return st.budget - st.usage.TotalTokens - st.childReserved, true
}

// ExecuteAttempt drives one run attempt to a terminal outcome. It provisions and
// starts the run through canonical transitions, opens the engine channel, and runs
// the frame-intake loop: every frame is validated and deduped before any dispatch,
// and every provider/tool result is committed before it is delivered to the engine.
func (o *Orchestrator) ExecuteAttempt(ctx context.Context, attempt AttemptDescriptor) error {
	tenant, sessionID, responseID, state, input, err := o.spine.RunContext(ctx, string(attempt.RunID))
	if err != nil {
		return err
	}
	// RunContext established the tenant; from here the whole attempt runs under it, so every
	// orchestrator write is gated by migration 000029's policies. In the worker path the context is
	// already this tenant's; when the orchestrator is driven directly (recovery, tests) this is what
	// narrows the system-scoped read back to the run's own tenant.
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)

	// A waiting run was pre-empted by a pause (spec §22.3, SES-009); a job redelivered in the ms
	// window between PauseRun's commit and the paused attempt settling must not drive it. Provision
	// and Start would both skip on ErrInvalidState (waiting is non-terminal), so without this guard
	// the doomed attempt delivers the pre-empted message and finalizes an illegal waiting→completed,
	// dead-lettering the job and FAILING a resumable run. Bail cleanly — resume opens a fresh attempt
	// to continue it. Only waiting bails; a running/provisioning reclaim proceeds as before.
	if statemachines.RunState(state) == statemachines.RunWaiting {
		return nil
	}

	// Move the run into execution using canonical transitions only. A run already
	// advanced past a step (redelivery) is skipped; a run already terminal is left
	// alone (spec §22.3).
	for _, cmd := range []statemachines.RunCommand{statemachines.RunCmdProvision, statemachines.RunCmdStart} {
		switch _, err := o.spine.ApplyRunTransition(ctx, tenant, string(attempt.RunID), cmd); {
		case errors.Is(err, coordinator.ErrRunTerminal):
			return nil
		case errors.Is(err, statemachines.ErrInvalidState):
			// already applied by an earlier attempt; resume idempotently
		case err != nil:
			return err
		}
	}

	// Recovery ladder rung 1 — exact (spec §26.3, E10 T4): BEFORE dialing or touching the checkpoint,
	// stand down if the ORIGINAL attempt is still driving the run (a live response.run lease other than
	// this attempt's own claimed job). The original continues untouched; this attempt records the rung
	// and exits without dialing (ENG-008 — no dial, no checkpoint read). Only engages when a live
	// sibling exists, so a fresh first attempt (its own job excluded) never stands down.
	switch live, err := o.spine.RunHasLiveResponseJob(ctx, tenant, string(attempt.RunID), attempt.JobID); {
	case err != nil:
		return err
	case live:
		return o.recordExactStandDown(ctx, tenant, sessionID, responseID, attempt)
	}

	// Record the durable attempt row before anything mid-run can offer a checkpoint (spec §26.1): the
	// checkpoint / transcript-boundary / workspace-snapshot FKs all reference attempts(id). Idempotent,
	// so a reclaim re-recording is a no-op. This attempt is proceeding (exact stood down above).
	if err := o.spine.RecordAttempt(ctx, tenant, string(attempt.RunID), string(attempt.AttemptID)); err != nil {
		return err
	}

	// Freeze the run's skill pins ONCE at run-start (spec §28.16, E12 Task 7): resolve the pinned
	// revision's requested skills to their enabled digests + metadata and record them on the run row, so a
	// mid-run enable of a new revision never changes what THIS run sees (never "latest"). Idempotent — a
	// resumed attempt sees the pins already frozen and skips. An unknown/not-enabled skill fails the run
	// here, VISIBLY. A skill-less run writes nothing, so its config + provider request stay bit-identical.
	if err := o.store.PinRunSkills(ctx, tenant, string(attempt.RunID)); err != nil {
		return fmt.Errorf("pin run skills: %w", err)
	}

	// Read this run's delegation context (spec §25.18): its depth, the required delegations a root
	// run seeds into run.start, its parent budget children intersect against, and — for a ChildRun
	// — its own model and budget. A plain run carries none and behaves exactly as before. Read here
	// (before the dial) because the ROOT-run workspace provisioning below is depth-gated.
	depth, delegationRaw, err := o.spine.RunDelegation(ctx, string(attempt.RunID))
	if err != nil {
		return fmt.Errorf("read run delegation: %w", err)
	}
	deleg := parseRunDelegation(delegationRaw)

	// Auto-provision the coding workspace for the ROOT run when the session has an attached binding
	// (spec §29.7-30.3, E09 Task 10): resolve the binding, allocate the host dir, clone @ the ref under
	// a brokered credential, acquire the single writer lease, and set the mount BEFORE the engine dials
	// (the tools and the runner bind-mount need it known at dial time; the lease spans the whole run).
	// Only the root run (depth 0) provisions + leases — a child (depth>0) already carries the workspace
	// dispatchChild resolved for it (read-only snapshot / isolated worktree, no second writer lease).
	// A run with no attachment, or no provisioner wired, gets no workspace — the pre-E09 behaviour.
	var workspaceID, workspaceLeaseID string
	if depth == 0 && attempt.WorkspaceHostPath == "" && o.provisionRoot != "" && o.provisionBroker != nil {
		hostPath, leaseID, wsID, perr := o.provisionRootWorkspace(ctx, tenant, sessionID, string(attempt.RunID), attempt.JobID, attempt.Fence)
		if perr != nil {
			return fmt.Errorf("provision workspace: %w", perr)
		}
		attempt.WorkspaceHostPath, workspaceLeaseID, workspaceID = hostPath, leaseID, wsID
	}
	// Release the writer lease + return the workspace to ready on EVERY exit (terminal, error, pause):
	// a fresh attempt (resume) re-leases the same allocation, and edits persist across runs.
	defer o.releaseWorkspace(tenant, workspaceID, workspaceLeaseID)

	// Bound the dial + engine.ready handshake with an attempt-scoped deadline: a runner that
	// connects but whose handshake wedges (or a gateway with no available runner) must fail
	// the attempt — routed through retry / dead-letter — not hang it silently. The deadline
	// covers only Dial and the ready receive below; the run loop that follows uses the parent
	// ctx, so a long-running response is never cut off at the deadline.
	dialCtx, cancelDial := context.WithTimeout(ctx, o.DialHandshakeDeadline)
	defer cancelDial()

	ch, err := o.dialer.Dial(dialCtx, attempt)
	if err != nil {
		return fmt.Errorf("dial engine: %w", err)
	}
	defer func() { _ = ch.Close() }()

	st := &attemptState{
		attempt: attempt, tenant: tenant, sessionID: sessionID, responseID: responseID,
		ch: ch, ledger: runner.NewFrameLedger(),
		workspaceID: workspaceID, workspaceLeaseID: workspaceLeaseID,
		attemptStart: time.Now(),
	}

	st.depth = depth
	if deleg.Spec != nil {
		st.childModel = deleg.Spec.Model
		st.childBudget = deleg.Spec.Budget
	}
	if deleg.Budget > 0 {
		st.budget, st.budgetBounded = deleg.Budget, true
	} else if deleg.Spec != nil && deleg.Spec.Budget > 0 {
		st.budget, st.budgetBounded = deleg.Spec.Budget, true
	}

	ready, err := ch.Receive(dialCtx)
	if err != nil {
		return fmt.Errorf("receive engine.ready: %w", err)
	}
	if _, err := st.ledger.Admit(ready); err != nil {
		return fmt.Errorf("engine.ready: %w", err)
	}
	if ready.Type != "engine.ready" {
		return fmt.Errorf("first frame type = %q, want engine.ready", ready.Type)
	}
	st.lastInboundSeq = ready.Sequence
	// Capture the engine handshake identity for checkpoint provenance (spec §26.2): the selected
	// protocol and the engine version. The pinned image digest rides the attempt descriptor.
	st.protocolVersion, _ = ready.Data["selected_protocol"].(string)
	if engine, ok := ready.Data["engine"].(map[string]any); ok {
		st.engineVersion, _ = engine["version"].(string)
	}

	var inputValue any
	if len(input) > 0 {
		_ = json.Unmarshal(input, &inputValue)
	}
	// Carry the session's prior responses as conversation history so a chained response
	// continues the session (spec §9, §22.2). A first response has no prior — messages is
	// omitted and run.start is exactly the LP-0 single-shot shape.
	prior, err := o.spine.SessionHistory(ctx, tenant, sessionID, responseID)
	if err != nil {
		return fmt.Errorf("assemble session history: %w", err)
	}
	runStart := map[string]any{"input": inputValue}
	if messages := historyMessages(prior); len(messages) > 0 {
		runStart["messages"] = messages
	}
	// Seed required delegations (spec §25.18): the engine emits one child.request per spec at the
	// safe boundary after its first model step. Config-driven, so a real single-step run delegates.
	if delegations := deleg.emitFrames(); len(delegations) > 0 {
		runStart["delegations"] = delegations
	}

	// Recovery ladder rungs 2-4 (spec §26.3-26.4, E10 T4): with a durable checkpoint present, weigh
	// its compatibility and either RESTORE a fresh process (run.restore, rung 2), reconstruct from the
	// transcript (run.start + committed-step replay, rung 3, recording WHY the checkpoint was
	// rejected), or fail explicitly (rung 4). With no checkpoint the ladder does not engage and this
	// is the ordinary run.start path (fork 7 — a fresh first attempt is bit-unchanged). The
	// committed-step watermark is captured either way for the replayed-boundary drain gate (§26.9).
	plan, err := o.consultCheckpointLadder(ctx, st, ready)
	if err != nil {
		return err
	}
	st.committedStepWatermark = plan.committedSteps

	// Intent hook (spec §26.9, §22.3, E10 T7 ENG-012 fork 2): a CANCELLATION intent accepted during the
	// outage is already processed before compute opens — ExecuteAttempt's ApplyRunTransition(Provision,
	// Start) above returns ErrRunTerminal for a canceled run and this attempt returns without dialing (the
	// terminal check IS the pre-dial cancel hook). A PAUSE is deliberately NOT pre-empted here: a pause is
	// a cooperative stop that must go through the boundary pump so it captures its SES-009 checkpoint
	// (checkpointBeforePause); applying it at run.start would skip the checkpoint. Any queue/steer/
	// interrupt message accepted in the outage stays queued for the pump to deliver in canonical
	// (creation/applied_sequence) order once the run continues — never spliced into a reconstructed step.

	switch plan.decision.Level {
	case recovery.LevelExplicitFailure:
		return o.failRecovery(ctx, st, plan)
	case recovery.LevelCompatibleCheckpoint:
		st.restored = true
		if err := ch.Send(ctx, o.frame(st, "run.restore", plan.restoreData(), string(ready.ID))); err != nil {
			return fmt.Errorf("send run.restore: %w", err)
		}
		// A compatible restore resumed past every completed step, so the config it ran under is the
		// checkpoint's (a config change would have failed the §26.4 compatibility decision). No
		// pending-session-config apply here — that is the run.start path's cross-run carry.
		if err := o.recordCompatibleRecovery(ctx, st, plan); err != nil {
			return err
		}
	default:
		if err := ch.Send(ctx, o.frame(st, "run.start", runStart, string(ready.ID))); err != nil {
			return fmt.Errorf("send run.start: %w", err)
		}
		// Apply any config switch accepted for this session that had no boundary in its own run — an
		// idle-session change_config, or a single-step run — so this run's first model step routes
		// under it (spec §9.3, the cross-run config carry). Runs before the first model.request; a
		// switch aimed at a mid-run boundary is untouched (it is applied by the pump/watcher instead).
		if err := o.applyPendingSessionConfig(ctx, st); err != nil {
			return abortIfTerminal(err)
		}
		// Carry any send_message that survived a prior run's terminal (E10 T7 ENG-012 fork 3): re-scope
		// it to this run so the ordinary boundary pump delivers it at this run's first input boundary. A
		// no-op when none carried. Only on run.start — a restore resumed past the boundary.
		if _, err := o.spine.CarrySessionSendMessages(ctx, st.tenant, st.sessionID, string(st.attempt.RunID)); err != nil {
			return abortIfTerminal(err)
		}
		// A checkpoint that existed but was rejected fell to transcript reconstruction: record the
		// rejection reason + the chosen rung (spec §26.3-26.4). No checkpoint => not engaged (fork 7).
		if plan.present {
			if err := o.recordTranscriptRecovery(ctx, st, plan); err != nil {
				return err
			}
		}
	}

	for {
		frame, err := o.receiveEngineFrame(ctx, st)
		if err != nil {
			return err
		}

		switch frame.Type {
		case "model.request":
			st.modelStepIndex++
			continues, err := o.dispatchModel(ctx, st, frame)
			if err != nil {
				return abortIfTerminal(err)
			}
			// After a model result is committed and delivered, this is a safe boundary
			// (spec §25.11). When the run continues to another step, drain any queued/steered
			// messages here so they fold into the NEXT model request — the input boundary
			// (spec §9.2). A final result has no next step, so nothing is delivered. The
			// just-completed step's model_request_id keys this boundary durably, so a reclaimed
			// attempt redelivers a message recorded here at the SAME boundary (spec §26.9, T2).
			if continues {
				boundaryRequestID, _ := frame.Data["model_request_id"].(string)
				// Replayed-boundary gate (spec §26.9, E10 T4 fork 4): a boundary whose NEXT step is a
				// replay must NOT run fresh effects — a fresh message folded there would rewrite a step
				// LookupModelResult replays by id without a hash check (silent divergence). At a
				// replayed boundary only prior-attempt durable deliveries refold; pause-read,
				// fresh-drain, config/approval and publication ALL wait for the first LIVE boundary.
				if o.boundaryIsLive(st) {
					switch err := o.pumpCommands(ctx, st, boundaryRequestID); {
					case errors.Is(err, errRunPaused):
						// A pause landed at this boundary: the run is waiting, the attempt ends cleanly
						// and releases its compute, and resume re-opens a fresh attempt (spec §22.3).
						return nil
					case err != nil:
						return abortIfTerminal(err)
					}
				} else if err := o.redeliverBoundaryMessages(ctx, st, boundaryRequestID); err != nil {
					return abortIfTerminal(err)
				}
			}
		case "tool.request":
			switch err := o.dispatchTool(ctx, st, frame); {
			case errors.Is(err, errToolUncertainWait):
				// An uncertain tool blocks continuation (spec §26.7): end the attempt cleanly — no
				// tool.result was sent, so the engine subprocess closes without hanging, and the reconcile
				// job resolves the row and re-enqueues the run. Not a failure, like a pause.
				return nil
			case err != nil:
				return abortIfTerminal(err)
			}
		case "child.request":
			switch err := o.dispatchChild(ctx, st, frame); {
			case errors.Is(err, errRunReleased):
				// A detached child was enqueued and the parent released its compute (spec §26.5, E10
				// T8): the run is waiting, this attempt ends cleanly, and the child terminal reopens a
				// fresh attempt to fold the result — exactly as a pause ends and resume reopens.
				return nil
			case err != nil:
				return abortIfTerminal(err)
			}
		case "output.item":
			st.output = append(st.output, contracts.ContentItem(frame.Data))
		case "checkpoint.offer":
			// Persist the engine's checkpoint at this safe boundary (spec §26.1-26.2). The bytes ride
			// the offer; the control plane stores + checksums them opaquely. A checkpoint failure does
			// not always fail the run (§26.5), but a hard persist error here surfaces rather than
			// silently dropping a boundary a later recovery would rely on.
			// A mid-loop checkpoint.offer links no workspace snapshot (the boundary snapshot is cut only
			// at a pause, SES-009). It declares no workspace dependency, unchanged from T4.
			if err := o.persistCheckpoint(ctx, st, frame, ""); err != nil {
				return abortIfTerminal(err)
			}
		case "run.terminal":
			return o.finalize(ctx, st, frame)
		case "protocol.error":
			return fmt.Errorf("engine protocol error: %v", frame.Data)
		default:
			// progress, warning, heartbeat, run.waiting: nothing to commit or dispatch.
		}
	}
}

// receiveEngineFrame reads the next NEW engine frame with the full intake discipline (spec §25.5,
// ENG-002): envelope + identity validation, frame-id dedup (a same-hash retransmit is skipped, a
// different-hash repeat is a violation), and sequence monotonicity. Shared by the main run loop and
// the pause drain (SES-009): both admit + seq-track frames identically, so a drained-but-discarded
// frame still rides the same ledger and ordered sequence stream as a dispatched one.
func (o *Orchestrator) receiveEngineFrame(ctx context.Context, st *attemptState) (contracts.EngineFrame, error) {
	for {
		frame, err := st.ch.Receive(ctx)
		if errors.Is(err, io.EOF) {
			return frame, fmt.Errorf("engine closed the channel before a terminal frame: %w", err)
		}
		if err != nil {
			return frame, fmt.Errorf("receive frame: %w", err)
		}
		if err := validateFrame(frame, st.attempt); err != nil {
			return frame, err
		}
		// Frame-ID dedup (ENG-002 controller half): a repeat with the same hash is an idempotent
		// retransmit, a repeat with a different hash is a protocol violation.
		duplicate, err := st.ledger.Admit(frame)
		if err != nil {
			return frame, fmt.Errorf("frame ledger: %w", err)
		}
		if duplicate {
			continue
		}
		// Intake sequence monotonicity: after dedup, every accepted engine frame must carry the next
		// sequence. A gap or reorder is a protocol violation that fails the attempt before any
		// dispatch. A dropped retransmit above does not advance the sequence.
		if frame.Sequence != st.lastInboundSeq+1 {
			return frame, fmt.Errorf("engine frame %s sequence %d is not %d (non-monotonic)", frame.ID, frame.Sequence, st.lastInboundSeq+1)
		}
		st.lastInboundSeq = frame.Sequence
		return frame, nil
	}
}

// checkpointBeforePause captures a durable checkpoint of the pause boundary BEFORE the run's compute
// is released (spec §26.5, SES-009). The engine — single-threaded — has already written this turn's
// in-flight tool.requests to the pipe; the controller asks for a checkpoint, DRAINS those in-flight
// frames with the same intake discipline as the main loop but WITHOUT dispatching them (no external
// effect runs; the resume re-derives them from the checkpoint), and persists the offered checkpoint.
// A persist failure is returned so the caller fails the attempt rather than pausing with no
// recoverable boundary (§26.5 last sentence) — never a silent checkpoint-less pause.
func (o *Orchestrator) checkpointBeforePause(ctx context.Context, st *attemptState) error {
	// Bound the drain (brief fork-1(i)): a wedged-but-live engine that never emits the offer must fail
	// the attempt on this deadline rather than hang until lease reclaim — §26.5 forbids a silent
	// checkpoint-less pause, so a failed drain fails the attempt (retry/reclaim), never pauses.
	drainCtx, cancel := context.WithTimeout(ctx, pauseDrainDeadline)
	defer cancel()
	// Cut the boundary WORKSPACE snapshot BEFORE asking for the checkpoint (SES-009), so the checkpoint
	// links a durable tree the resume restores. A snapshot failure fails the attempt here, exactly like a
	// checkpoint failure — §26.5 forbids pausing with no recoverable boundary, so it never pauses
	// snapshot-less silently. Empty when no sink/workspace is wired (the T4 no-dependency case).
	snapshotID, err := o.captureBoundarySnapshot(ctx, st)
	if err != nil {
		return fmt.Errorf("cut pause boundary snapshot: %w", err)
	}
	if err := st.ch.Send(drainCtx, o.frame(st, "checkpoint.request", map[string]any{}, "")); err != nil {
		return err
	}
	for {
		frame, err := o.receiveEngineFrame(drainCtx, st)
		if err != nil {
			return fmt.Errorf("drain for pause checkpoint: %w", err)
		}
		switch frame.Type {
		case "checkpoint.offer":
			return o.persistCheckpoint(ctx, st, frame, snapshotID)
		case "protocol.error":
			// Surface it, never discard: a malformed frame during the drain is a real engine fault that
			// must fail the attempt, not be swallowed while the pause proceeds checkpoint-less.
			return fmt.Errorf("engine protocol error during pause drain: %v", frame.Data)
		default:
			// An in-flight tool.request/child.request for the pausing turn: admitted + seq-tracked by
			// receiveEngineFrame, discarded here WITHOUT dispatch. No tool runs, no commit — the resume's
			// fresh process re-derives it from the restored checkpoint (SES-009).
		}
	}
}

// frame builds a controller frame with the attempt identity and a monotonic sequence.
func (o *Orchestrator) frame(st *attemptState, typ string, data map[string]any, replyTo string) contracts.EngineFrame {
	st.seq++
	f := contracts.EngineFrame{
		Protocol:  engineProtocol,
		ID:        newFrameID(),
		Type:      typ,
		Sequence:  st.seq,
		Time:      time.Now().UTC().Format(time.RFC3339),
		RunID:     st.attempt.RunID,
		AttemptID: st.attempt.AttemptID,
		Data:      data,
	}
	if replyTo != "" {
		rt := replyTo
		f.ReplyTo = &rt
	}
	return f
}

// validateFrame enforces the engine envelope and the run/attempt identity before any
// transaction, so a malformed or misrouted frame never reaches a dispatch.
func validateFrame(f contracts.EngineFrame, a AttemptDescriptor) error {
	if f.Protocol != engineProtocol || !f.ID.Valid() || f.Type == "" {
		return fmt.Errorf("frame violates the engine envelope")
	}
	if _, err := time.Parse(time.RFC3339, f.Time); err != nil {
		return fmt.Errorf("frame %s has no valid timestamp", f.ID)
	}
	if f.RunID != "" && f.RunID != a.RunID {
		return fmt.Errorf("frame %s run identity mismatch", f.ID)
	}
	if f.AttemptID != "" && f.AttemptID != a.AttemptID {
		return fmt.Errorf("frame %s attempt identity mismatch", f.ID)
	}
	return nil
}

// abortIfTerminal maps a mid-attempt terminal-run rejection to a clean attempt end. When a
// run is canceled while an attempt is in flight, its next commit is rejected with
// ErrRunTerminal (the commit-after-terminal guard); the attempt then has nothing left to do
// — the run is already terminal — so it ends without error and the durable job settles
// instead of dead-lettering. Any other error still fails the attempt.
func abortIfTerminal(err error) error {
	if errors.Is(err, coordinator.ErrRunTerminal) {
		return nil
	}
	return err
}

func newFrameID() contracts.FrameID {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return contracts.FrameID("frm_" + hex.EncodeToString(raw[:]))
}
