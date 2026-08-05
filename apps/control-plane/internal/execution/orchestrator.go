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
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/coordinator/recovery"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	"github.com/palgroup/palai/packages/outputcontract"
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
	// THERE IS NO `shell` FIELD, AND ITS ABSENCE IS THE POINT (A.3). A ShellRunner held here would be
	// one executor for the whole process, and every attempt got it: two runs on two machines were two
	// runs on the SAME executor, so "this one on a Mac, that one in a container" could not be expressed.
	// The runner is now derived per attempt from the connection that attempt holds — shellFor in
	// tool_dispatch.go, where the branch that yields none is also named.
	//
	// background is UNCHANGED and still process-wide, which is a real inconsistency rather than an
	// oversight: a background task outlives the attempt that started it, so the attempt's connection is
	// the wrong lifetime to hang it on. Moving it needs a machine-side task registry, which is E26 T5's
	// seam and not this epic's. Today a synchronous command runs on the attempt's machine while a
	// backgrounded one runs beside the control plane.
	//
	// background starts a shell command that OUTLIVES the attempt that started it (E26 T1). It answers a
	// different question from a ShellRunner: a ShellRunner returns a result, a BackgroundRunner returns a
	// handle. Nil when
	// the wired posture cannot detach — a background tool call then fails cleanly, and specifically does
	// NOT fall back to running the command synchronously: a model that asked for a background task and
	// got a blocking call is blocked in exactly the way the feature exists to prevent. main.go injects it
	// via SetBackgroundRunner where the wired shell runner can also detach.
	background toolbroker.BackgroundRunner
	// machineID names the machine THIS process's background runner reaches (A.3 T6). It is recorded on
	// every task this plane starts and compared before any later probe or signal, because a kernel
	// cannot tell "never mine" from "exited" — both are ESRCH — so asking the wrong one produces a wrong
	// answer rather than an error. Empty means this deployment has not named its machine; a task started
	// under it is written with an empty machine and is therefore never probed locally afterwards, which
	// is the same conservative reading rows predating the column get.
	machineID string
	// machines reaches a NAMED machine outside any lease (A.3 T7). It is what makes background
	// execution a property of the machine rather than of this process: StartBackground asks the
	// attempt's machine to spawn, and the reconciler asks the row's machine whether it finished.
	//
	// Nil is a deployment with no gateway — every component test that builds an Orchestrator by hand,
	// and the deterministic e2e tier. Those attempts have no machine, so a background call is refused
	// rather than silently running here, which is the same discipline shellFor applies to a synchronous
	// command.
	machines MachineCaller
	// bgSpawn serialises the CEILING CHECK and the start it guards (E26 T5, §0.3). It is all that is left
	// of a map from task id to handle: the durable background_tasks row replaced that map in T5, because a
	// map dies with the process that made it and the whole point of a background task is to outlive that
	// process. What remains needs a lock for a different reason — the concurrency count and the spawn are
	// two statements, and several attempts of several runs dispatch concurrently through one orchestrator.
	bgSpawn sync.Mutex
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
	provisionRoot string
	// sessionAccounts mints the uid an allocation's tools run under (macOS). Nil means none, which is
	// every deployment that has not opted in.
	sessionAccounts SessionAccounts
	provisionBroker repositories.Broker
	// provisionSecrets resolves a binding's connection_ref to that tenant's OWN Git credential (E13 Task
	// 9). Nil ⇒ no secret-ref store wired: every binding clones under provisionBroker, as before. main.go
	// injects it via SetConnectionSecrets.
	provisionSecrets SecretResolver
	// artifacts is the object-store write-path the finalize changeset compile persists the patch +
	// test-log through (spec §30.6). Nil ⇒ no changeset is compiled (a stack with no artifact store
	// wired). main.go injects it via SetChangesetWriter.
	artifacts ArtifactWriter
	// images is the object-store READ path an `image_ref` content item resolves through, so a run can see
	// an image (spec §25.10). Nil ⇒ every image_ref reads as a miss and the turn says so; a text-only run is
	// unaffected either way. main.go injects it via SetImageReader.
	images ImageReader
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
	// queueDeadline is the §20.12 admission queue deadline: a run that waits in the queue longer than
	// this before it is provisioned is timed out at dispatch, BEFORE any billable compute. Zero disables
	// the check (the pre-E13-T7 behaviour); main.go sets it from PALAI_QUEUE_DEADLINE.
	// ponytail: one global deadline for the basic tier; a per-project deadline (a config_policy field,
	// T2) is a later refinement, not a gate requirement.
	queueDeadline time.Duration
	// remoteAgents resolves a REGISTERED remote A2A agent (a2a_remote_agents, §38.5) inside the run's OWN
	// tenant; remoteChildren dispatches a delegated child to it (E19 T5). Either nil ⇒ a child.request
	// naming a remote agent is DENIED — never quietly run on the LOCAL path, which would execute the
	// delegation on our engine under our credentials, the exact substitution the remote branch exists to
	// prevent. main.go injects both via SetRemoteChildren.
	remoteAgents   RemoteAgents
	remoteChildren RemoteChildRunner
	// envSecrets resolves an ENVIRONMENT value from its derived secret_refs name (E25 T3). Nil ⇒ a run
	// whose pinned revision names an environment fails its tool call rather than running the command
	// without the credential it was configured with. main.go injects it via SetEnvironmentSecrets.
	envSecrets SecretResolver
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
	o := &Orchestrator{store: st, spine: st.Spine(), dialer: dialer, models: models, tools: tools, tasks: newTaskRegistry(st.Spine()), route: defaultModelRoute, DialHandshakeDeadline: dialHandshakeDeadline}
	// The publication registry is handed o.canPublish rather than the publisher itself, and the two-step
	// construction is what that costs. The registry runs when a push TOOL is called; the publisher is
	// injected later by SetPublisher, so a captured copy would be the nil one every time. A method value
	// reads o.publisher when the question is asked.
	o.publications = newPublicationRegistry(st.Spine(), o.canPublish)
	return o
}

// canPublish asks the WIRED publisher whether a publication naming this connection ref has a credential
// path on this deployment — BEFORE a pending approval is recorded and a human is asked about it.
//
// A publication that cannot be published must not become a question. The alternative is the shape this
// tree calls silent-skip in its most expensive form: a human reads a push, presses Approve, is told the
// run woke, and no write ever happens.
//
// IT IS INERT WITH NO PUBLISHER WIRED, and that ceiling is stated rather than hidden. A deployment with no
// publisher publishes nothing either — but PRODUCTION ALWAYS WIRES ONE (main.go calls SetPublisher
// unconditionally; TestABindingsOwnCredentialPublishesWithNoGitHubApp pins that the constructor never
// returns nil), so the inert case is the deterministic tiers, where an orchestrator with no publisher is
// how every non-publication test is built and parking an approval nobody will pump is the point.
func (o *Orchestrator) canPublish(connectionRef string) error {
	prechecker, ok := o.publisher.(PublicationPrechecker)
	if !ok {
		return nil
	}
	return prechecker.CanPublish(connectionRef)
}

// SetModelRoute sets the DEPLOYMENT-DEFAULT provider/model/secret the composition root (main.go) selects
// from the environment. Since E13 T8 it is the FALLBACK layer: a project with a published model route
// dispatches through that route instead (effectiveRoute), and a project without one runs on this.
func (o *Orchestrator) SetModelRoute(r ModelRoute) { o.route = r }

// SetBackgroundRunner injects the detached shell runner a background task is started through (E26 T1).
// Left unset — which is every deployment before E26 and every posture that cannot detach — the dispatch
// is bit-unchanged: the same SetHookFirer/SetPublisher discipline, where an unwired seam means the
// feature is simply absent rather than half-present.
//
// IT ALSO WIRES THE CANCELLATION KILLER, IN THE SAME CALL AND DELIBERATELY (E26 T5). A deployment that
// can START a background task and cannot END one when its run is cancelled is precisely the orphan this
// epic exists to prevent — and E26 T2 already found one instance of that shape by omission, where every
// deployment granting the shell tool could begin a build and none could stop it because the kill tool sat
// on a different conditional. Two setters would be two chances to wire one and forget the other; one
// setter makes the broken state unrepresentable.
func (o *Orchestrator) SetBackgroundRunner(b toolbroker.BackgroundRunner) {
	o.background = b
	o.rewireBackgroundKiller()
}

// rewireBackgroundKiller re-derives the cancellation killer from what this orchestrator can currently
// reach. IT IS CALLED BY BOTH SETTERS BECAUSE SINCE A.3 T7 THE ABILITY TO KILL IS A PROPERTY OF REACHING
// THE MACHINE, not of holding a local executor: BackgroundKiller() returns nil while o.machines is nil,
// so a composition root that called SetBackgroundRunner FIRST and SetMachineCaller second shipped a
// deployment whose cancellation killed nothing — silently, because both wires look present and the value
// captured between them is the one that was nil.
//
// main.go happens to call them in the safe order today; that ordering is not something the type system
// says, and "correct because of the line number it sits on" is the shape this tree has paid for before.
// Deriving it from both ends makes the order irrelevant.
func (o *Orchestrator) rewireBackgroundKiller() {
	if o.spine != nil {
		o.spine.SetBackgroundKiller(o.BackgroundKiller())
	}
}

// SetBackgroundMachine names the machine this process's background runner reaches (A.3 T6). It is
// recorded on every task started here and compared before any later probe or signal.
//
// IT IS SEPARATE FROM SetBackgroundRunner BECAUSE THE TWO ANSWER DIFFERENT QUESTIONS, and folding them
// would make the identity look like a property of the executor. It is a property of the HOST: a control
// plane that restarts, or is replaced by a second process on the same box, reaches the same kernel and
// must still settle what it started — which is the crash-restart property the wake suite proves. Left
// unset, every task this plane starts is written with an unknown machine and is therefore never probed
// locally again, which is the same conservative reading rows predating the column get.
func (o *Orchestrator) SetBackgroundMachine(id string) { o.machineID = id }

// MachineCaller reaches a named machine with no lease in hand. *RunnerGateway satisfies it; a
// deployment without one leaves it nil and every background call is refused.
type MachineCaller interface {
	CallMachine(ctx context.Context, machineID, verb string, payload map[string]any) (map[string]any, error)
}

// SetMachineCaller wires the surface background execution travels on (A.3 T7). It is separate from the
// EngineDialer even though the gateway satisfies both, because the two answer different questions: a
// dialer hands out WHICHEVER machine a pool has free for an attempt, and this one addresses the machine
// a task ALREADY runs on — by name, long after that attempt ended.
func (o *Orchestrator) SetMachineCaller(m MachineCaller) {
	o.machines = m
	// The killer is re-derived here for the reason rewireBackgroundKiller names: reaching a machine is
	// what grants the ability to stop a task, so the grant has to be taken after this assignment and not
	// only after the other setter's.
	o.rewireBackgroundKiller()
}

// callMachine is the one place a background verb crosses to a machine, so the refusals are written once.
// An unreachable machine is ErrMachineUnreachable, which every caller turns into `lost` — never
// `exited`, because the process may still be running and this control plane cannot prove it is ours.
func (o *Orchestrator) callMachine(ctx context.Context, machineID, verb string, payload map[string]any) (map[string]any, error) {
	if o.machines == nil {
		return nil, fmt.Errorf("%w: this deployment has no runner gateway", ErrMachineUnreachable)
	}
	// AN EMPTY MACHINE IS UNREACHABLE AND NOT "HERE" (T6's rule, kept). A row written before the column
	// existed says nothing about where it ran, and resolving that to some machine would be the wrong
	// answer this whole seam exists to stop.
	if machineID == "" {
		return nil, fmt.Errorf("%w: the row names no machine", ErrMachineUnreachable)
	}
	return o.machines.CallMachine(ctx, machineID, verb, payload)
}

// THERE IS NO BackgroundRunner() ACCESSOR, AND ITS ABSENCE IS A DELETION. T5 shipped one whose doc comment
// said it existed "so a COMPOSITION-ROOT test can ask what production actually wired" — and the E26 T7
// reachability sweep measured that NOTHING ASKED, not production and not one test. That is the same sentence
// E25 T9 found on host.Executor.WallTime and filed as CON-P9, one epic later and word for word. WallTime was
// filed rather than deleted because the number it reports means something to an operator (zero means
// UNBOUNDED); this returned an interface value that means nothing to anybody. The composition root's wiring
// is asserted where it is decided instead — backgroundRunnerFor, in main_test.go, on the value the posture
// actually binds.

// SetHookFirer injects the hook dispatcher the five pinned points fire through (spec §28.17, E12 T8). Left
// unset, no hook fires — the dispatch is bit-unchanged (the same discipline as SetBackgroundRunner/SetPublisher).
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

// SetImageReader injects the read side of the object store, which is what lets a run SEE an image: an
// `image_ref` content item in the run's input names an artifact, and the bytes are joined to the provider
// request here in the control plane (spec §24 — the object-store credential never reaches the engine, and
// the 1 MiB engine frame could not carry a screenshot regardless).
//
// Left unset, every image_ref resolves as a miss and the turn carries the "no longer available" marker —
// which is the truth for a stack with no object store, and leaves a text-only run bit-unchanged.
func (o *Orchestrator) SetImageReader(ir ImageReader) { o.images = ir }

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

// SetRemoteChildren wires remote child-run dispatch (E19 T5, §38.5): the registered-agent lookup and the
// A2A client that dials it. Left unset (either nil), a delegation naming a remote agent is DENIED rather
// than dispatched — the same fail-closed discipline as SetPublisher, except the fallback here would be a
// SECURITY substitution (our engine, our credentials) rather than a no-op, so it is refused explicitly.
func (o *Orchestrator) SetRemoteChildren(agents RemoteAgents, runner RemoteChildRunner) {
	o.remoteAgents, o.remoteChildren = agents, runner
}

// SetQueueDeadline sets the §20.12 admission queue deadline (see the field doc). Left unset (zero),
// a run is never timed out for queue age — the pre-E13-T7 behaviour, so every existing tier is
// bit-unchanged. main.go injects it from PALAI_QUEUE_DEADLINE.
func (o *Orchestrator) SetQueueDeadline(d time.Duration) { o.queueDeadline = d }

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
	// route is the attempt's effective ModelRoute — the project's DB-backed route when it has one, else
	// the deployment default (E13 T8). Resolved once by effectiveRoute and cached here so every boundary
	// of one attempt agrees on the same target; routeResolved distinguishes "not yet read" from "read,
	// and the project has no route".
	route         ModelRoute
	routeResolved bool
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
	// outputContract is the §22.7 contract this run's answer must satisfy, or the zero value when the
	// request named no format — which is every run that does not opt in, and every run that existed
	// before migration 000052. Read ONCE per attempt (RunContextFor) and used twice: model_dispatch
	// turns it into the provider's own decoding constraint, and finalize checks the produced answer
	// against it before the run may be called completed. Both readers share these bytes deliberately;
	// two independent re-derivations are how "what we asked for" and "what we checked" drift apart.
	outputContract outputcontract.Contract
	// runInstructions is the RUN-SPECIFIC instruction layer (spec §25.12 layer 5): the request's own
	// `instructions` string, "" when it named none. Read ONCE per attempt (RunContextFor) beside the
	// output contract, for the same reason: the value is immutable for the life of the run, and one
	// read means two model steps of one run cannot be told different things.
	//
	// The PINNED REVISION's instructions (layer 3) are deliberately NOT cached here. They are read per
	// step from PinnedExecConfig, alongside the model and tool ceiling that already resolve that way,
	// so all of one revision's config comes from one read and cannot go half-stale.
	runInstructions string
	// remoteChildren counts the delegations this attempt sent to REMOTE agents (E19 T5). A remote child
	// takes no local ChildRun, so childRunIDs cannot count it — and an uncounted child escapes the fan-out
	// bound entirely, which is why the gate reads fanoutUsed() rather than len(childRunIDs).
	remoteChildren int
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
	// envKeys are this attempt's environment key NAMES and their derived secret_refs names, resolved ONCE
	// at attempt start from the run's PINNED revision (E25 T3). NAMES ONLY — no value is ever stored here,
	// on the orchestrator, or anywhere else that outlives one Execute call. Nil for every run whose
	// revision names no environment, which is every run in every deployment before E25.
	envKeys []envKey
	// toolAnswerErrors counts the tool REFUSALS this attempt has handed the model — a tool error delivered
	// as a result rather than raised as a fault (tool_answer.go). It is what bounds a model that keeps
	// asking for a file that is not there. It counts REPLAYED refusals too, so a reclaimed attempt
	// rebuilding the same transcript arrives at the same number rather than starting over.
	//
	// HONEST CEILING, AND IT IS THE WHOLE OF WHAT THIS FIELD CLAIMS: it is per ATTEMPT, in memory. A run
	// whose attempts are torn down and reopened by something OTHER than a replay of the same transcript —
	// a park, a pause, a resume — starts a fresh count. Making it per-run needs the count to be durable,
	// which is a column and a migration; the refusals themselves already are (every one is a committed
	// tool_calls row), so that upgrade is a query away when a deployment needs it.
	toolAnswerErrors int
}

// fanoutUsed is the number of children this attempt has already dispatched: the local ChildRuns plus the
// remote delegations, which have no run row to be counted by (E19 T5). The fan-out gate reads this, never
// len(childRunIDs) — an uncounted child is an unbounded one.
func (st *attemptState) fanoutUsed() int { return len(st.childRunIDs) + st.remoteChildren }

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
	ctx = storage.ScopeToTenant(ctx, tenant.Project)

	// A waiting run was pre-empted by a pause (spec §22.3, SES-009); a job redelivered in the ms
	// window between PauseRun's commit and the paused attempt settling must not drive it. Provision
	// and Start would both skip on ErrInvalidState (waiting is non-terminal), so without this guard
	// the doomed attempt delivers the pre-empted message and finalizes an illegal waiting→completed,
	// dead-lettering the job and FAILING a resumable run. Bail cleanly — resume opens a fresh attempt
	// to continue it. Only waiting bails; a running/provisioning reclaim proceeds as before.
	if statemachines.RunState(state) == statemachines.RunWaiting {
		return nil
	}

	// §20.12 queue-deadline: a run that waited in the admission queue past its deadline is timed out
	// HERE — before the Provision/Start transitions and the engine dial below — so no billable compute
	// starts. A no-op when the deadline is unset, the run has already left the queue, or it is within
	// the deadline; on a timeout the run reaches its timed_out terminal and its response is finalized,
	// so the attempt is complete. This is the guaranteeing check the reconciler's reaper cannot give:
	// it runs at the exact instant the worker would otherwise provision the run.
	if o.queueDeadline > 0 {
		switch timedOut, err := o.spine.TimeoutQueuedIfExpired(ctx, tenant, string(attempt.RunID), responseID, o.queueDeadline, queueTimeoutProjection); {
		case err != nil:
			return err
		case timedOut:
			return nil
		}
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

	// THE RESPONSE'S OWN LIFECYCLE (E26 T3, §3.6 D3), driven through the same skip-on-ErrInvalidState
	// ladder the run above uses, and for the same reason: a redelivered or resumed attempt re-applies
	// whichever command is legal from where the response actually is, and the others are no-ops.
	//
	// IT IS HERE BECAUSE OF WHAT WAS MEASURED, not because the plan asked for it. §3.6 D3 said a live
	// response reads `in_progress`; it read `queued`, for the whole life of every run this tree has ever
	// executed — InsertResponse wrote 'queued' and the terminal UpdateResponse wrote the end, and nothing
	// in between existed. So ResponseTable had no production caller at all, its `provisioning` and
	// `in_progress` states were unreachable, and `waiting_for_tool` — which the published schema has
	// advertised since §8.3 was written and which E26's park must produce — is legal ONLY from
	// `in_progress`. Binding the park without binding the entry would have made every park's response
	// transition illegal, which is a fix that reports success and changes nothing.
	//
	// resume is the third rung and it is what a WOKEN run needs: an attempt re-entered after a park finds
	// its response in waiting_for_tool, where provision and start are both illegal, and resume is the one
	// command that returns it to in_progress. Ordered after them so a fresh run never touches it.
	//
	// ponytail: three rungs is three primary-key reads and at most two writes per attempt, on a path that
	// already makes about fifteen round trips before the engine says anything. If that ever matters, the
	// upgrade is one read + one write: read the state once and walk ResponseTable in memory, keeping the
	// state read as the UPDATE's predicate exactly as AdvanceResponse already does.
	for _, cmd := range []statemachines.ResponseCommand{
		statemachines.ResponseCmdProvision, statemachines.ResponseCmdStart, statemachines.ResponseCmdResume,
	} {
		if err := o.advanceResponse(ctx, tenant, responseID, cmd); err != nil {
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
	// It also carries the §22.7 output contract, read HERE rather than at each use so the document
	// asked of the model and the document checked at finalize are provably the same bytes.
	runCtx, err := o.spine.RunContextFor(ctx, string(attempt.RunID))
	if err != nil {
		return fmt.Errorf("read run context: %w", err)
	}
	depth, delegationRaw := runCtx.Depth, runCtx.Delegation
	deleg := parseRunDelegation(delegationRaw)
	outputContract, err := parseOutputContract(runCtx.OutputContract)
	if err != nil {
		// The row was written by resolveOutputContract from a contract that had already passed
		// outputcontract.Parse, so a decode failure here means the stored bytes are not what this
		// server wrote. Failing the attempt is the only honest response: proceeding would run the
		// model with NO constraint while the caller's request said otherwise.
		return fmt.Errorf("read run output contract: %w", err)
	}

	// Auto-provision the coding workspace for the ROOT run when the session has an attached binding
	// (spec §29.7-30.3, E09 Task 10). Only the root run (depth 0) provisions + leases — a child (depth>0)
	// already carries the workspace dispatchChild resolved for it (read-only snapshot / isolated
	// worktree, no second writer lease). A run with no attachment, or no provisioner wired, gets no
	// workspace — the pre-E09 behaviour.
	//
	// IT IS TWO HALVES ACROSS THE DIAL NOW (A.3 T5), and the split is not a refactor: the bytes belong on
	// the machine that will run the commands, and which machine that is is not known until the dial
	// returns. This half only READS and MINTS A NAME — the allocation id and its path — because the lease
	// offer carries that path and the runner both bind-mounts it and, since this task, CREATES it. The
	// half that puts anything on a disk runs below, on the connection the dial handed back.
	var workspaceID, workspaceLeaseID string
	// machineOccupancyID names this session's hold on the machine this attempt lands on, so the hold's
	// last-activity stamp — which is where an idle-closed bill STOPS — can be moved as the attempt ends.
	// Empty whenever there is no machine to occupy, which is every deterministic tier.
	var machineOccupancyID string
	var wsPlan workspacePlan
	var wsPlanned bool
	if depth == 0 && attempt.WorkspaceHostPath == "" && o.provisionRoot != "" && o.provisionBroker != nil {
		plan, planned, perr := o.planRootWorkspace(ctx, tenant, sessionID)
		if perr != nil {
			log.Printf("run %s: workspace planning failed: %v", attempt.RunID, perr)
			return o.failProvisioning(ctx, tenant, attempt, responseID, perr)
		}
		if planned {
			wsPlan, wsPlanned = plan, true
			attempt.WorkspaceHostPath = plan.hostPath
		}
	}
	// Release the writer lease + return the workspace to ready on EVERY exit (terminal, error, pause):
	// a fresh attempt (resume) re-leases the same allocation, and edits persist across runs.
	//
	// THE CLOSURE IS LOAD-BEARING AND IT IS A REPAIR (A.4 T4). A deferred call evaluates its ARGUMENTS at
	// the `defer` statement, not at the exit — so `defer o.releaseWorkspace(tenant, workspaceID,
	// workspaceLeaseID)` passed the two EMPTY strings these variables hold here, and releaseWorkspace
	// returns immediately on an empty lease id. It was correct when it was written (E09 T10 assigned both
	// four lines ABOVE the defer) and stopped being correct when A.3 T5 moved provisioning after the dial
	// — the assignment now happens ~80 lines BELOW. Nothing failed: the workspace simply never went
	// leased→ready, and the next attempt's acquireWriterLease reclaimed the dangling lease and carried on.
	//
	// MEASURED ON THE RUNNING STACK, 2026-08-05, which is how it was found at all:
	//
	//	select state, max(created_at), count(*) from workspaces group by 1;
	//	  ready  34, newest 2026-08-04 07:21Z   <- all of them predate c025c9aa (2026-08-04 10:38Z)
	//	  leased 17, newest 2026-08-04 22:12Z   <- every workspace created since, and none has a live run
	//
	// WHAT IT COST IS THIS WHOLE PHASE. `IdleWorkspacesForRelease` selects `state = 'ready'` AND no active
	// writer lease, so a build where nothing releases has NO idle candidate, ever: the releaser T2/T3
	// shipped could not fire, no machine was ever handed back, and the occupancy this task settles could
	// never close. TestExecuteAttemptDefersReadTheirArgumentsAtExit is the fence, and it reads the
	// syntax rather than the behaviour because the behaviour is a no-op that logs nothing.
	defer func() { o.releaseWorkspace(tenant, workspaceID, workspaceLeaseID) }()
	// The occupancy's last-activity stamp moves at the attempt's END as well as at its start, and without
	// this the bill is short by the length of every run: an idle-closed hold is billed to
	// `last_activity_at`, so a two-hour run whose only stamp was written when it STARTED would have its
	// machine time thrown away the moment the reaper closed the hold. Same closure discipline as the line
	// above, and for the same reason — the id is assigned below.
	defer func() { o.keepMachineHeld(tenant, machineOccupancyID) }()

	// WHERE this attempt runs, decided here and recorded (E24 T4). It resolves the pool, carries the
	// tenant onto the descriptor — the runner plane had no tenant on it at all (§3.6 D8) — and orders the
	// attempt by its RUN's queued-at. Placed immediately before the dial because the dial is what the
	// decision is FOR: an attempt that stood down above never dialed and records no placement, which is
	// right, because the attempt that does dial will record it.
	// A pool this tenant cannot be served by is a configuration mistake, and it ends the run with a
	// reason rather than parking it somewhere its own wake cannot look (see errPoolNotServable). It is
	// checked HERE, before the dial, for the reason failProvisioning is: the fault is deterministic, so
	// the retry ladder would spend five attempts reproducing it and hide it behind the generic terminal.
	if err := o.place(ctx, tenant, &attempt); err != nil {
		if errors.Is(err, errPoolNotServable) {
			return o.failPlacement(ctx, tenant, attempt, responseID)
		}
		return err
	}

	// Bound the dial + engine.ready handshake with an attempt-scoped deadline: a runner that
	// connects but whose handshake wedges (or a gateway with no available runner) must fail
	// the attempt — routed through retry / dead-letter — not hang it silently. The deadline
	// covers only Dial and the ready receive below; the run loop that follows uses the parent
	// ctx, so a long-running response is never cut off at the deadline.
	dialCtx, cancelDial := context.WithTimeout(ctx, o.DialHandshakeDeadline)
	defer cancelDial()

	ch, err := o.dialer.Dial(dialCtx, attempt)
	switch {
	case errors.Is(err, ErrPoolHasNoRunner):
		// The pool held NO machine of this tenant for the whole dial budget (E24 T4, §3.6 D12). The run
		// PARKS instead of failing: this attempt ends cleanly, the dispatch worker is freed, and the next
		// machine to join that pool re-enters the run. Failing here spent five attempts and dead-lettered
		// in ~2.5 minutes, while AWS documents a Mac host taking 6 to 20 minutes to start — so the
		// behaviour the whole feature needs was not slow, it was unreachable. Exactly as a pause ends and
		// a resume reopens.
		if perr := o.parkForCapacity(ctx, tenant, attempt); !errors.Is(perr, errRunAwaitingCapacity) {
			return perr
		}
		return nil
	case err != nil:
		return fmt.Errorf("dial engine: %w", err)
	}
	defer func() { _ = ch.Close() }()

	// THE HALF OF PROVISIONING THAT TOUCHES A DISK (A.3 T5), and it runs here because here is the first
	// line that knows WHICH disk. It lays the allocation out, clones @ the requested ref, materializes
	// the run's skills, and takes the single writer lease — all of it through the connection the dial
	// returned, so an attempt placed on a Mac prepares its workspace on that Mac.
	//
	// IT IS BEFORE THE ENGINE IS SPOKEN TO, WHICH IS THE ORDERING THAT MATTERS. The runner started the
	// engine when it admitted the lease, but the engine says nothing until it is asked and no tool call
	// can arrive before this returns — so the repository is on disk before anything could look for it.
	if wsPlanned {
		hostPath, leaseID, wsID, perr := o.realizeRootWorkspace(ctx, ch, tenant, wsPlan, string(attempt.RunID), attempt.JobID, attempt.Fence)
		if perr != nil {
			// A PROVISIONING FAILURE IS THE PLATFORM'S OWN, AND IT MUST SAY SO. Before this it returned an
			// error into the retry ladder and the run dead-lettered into the generic terminal — "the run
			// failed during execution" — with the reason recorded NOWHERE an operator can reach. Measured
			// 2026-08-02 on a live stack: the runs row has no error column, the attempts row carries only
			// state, run.failed.v1's payload is {state, run_id}, and the control-plane log says nothing. So
			// a bad clone URL, a missing credential and a model refusing to answer were one sentence.
			//
			// The sanitized-problem stance still holds. terminalProblems is a fixed line because a failure
			// must never carry raw PROVIDER or ENGINE text; a clone failure is neither — it is this
			// deployment's own git invocation against a repository the operator configured.
			//
			// It fails the run rather than retrying, for the reason failContextOverflow does: a missing
			// credential and a wrong ref are DETERMINISTIC, so the ladder would spend eight attempts
			// reproducing the same error and hide it behind the generic terminal anyway.
			log.Printf("run %s: workspace provisioning failed: %v", attempt.RunID, perr)
			return o.failProvisioning(ctx, tenant, attempt, responseID, perr)
		}
		attempt.WorkspaceHostPath, workspaceLeaseID, workspaceID = hostPath, leaseID, wsID
		// THE MACHINE OCCUPANCY OPENS HERE (A.4 T4) — the first line that knows both WHICH machine and
		// that the session's allocation is really on it. Before the realize there is no machine's disk
		// involved yet, and after it the hold is a durable fact somebody is paying for.
		//
		// It is INSIDE the `wsPlanned` arm because the occupancy is the ALLOCATION's hold: the idle
		// releaser is what ends it, and the releaser is driven by workspaces. Opening one for a run with
		// no workspace would open a hold nothing can ever close (machine_occupancy.go names that ceiling).
		//
		// A MACHINE AT ITS DECLARED CAPACITY PARKS THE RUN (A.4 T6), which is the second of the two
		// conditions parkForCapacity answers and the later of them: the first is a pool with nobody in it,
		// this one is a machine that took the dial and then said it has no room. Running anyway — what this
		// line did between T5 and T6 — puts a session on a Mac that declared it would not take one, and
		// opens no row, so the overrun is invisible on the bill as well as over the ceiling.
		//
		// THE REALIZE ABOVE HAS ALREADY HAPPENED, AND THAT COST IS PAID RATHER THAN DESIGNED AWAY. The
		// refusal is only knowable once a machine is chosen, and the one place that judges it without a
		// race is the acquire's own INSERT (leases.sql) — a cheaper pre-check after the dial would be a
		// second ceiling, weaker than the real one and disagreeing with it under contention.
		//
		// SO A PARKED ATTEMPT LEAVES ITS ALLOCATION ON THE MACHINE THAT REFUSED IT, and what that costs is
		// stated rather than assumed: the deferred releaseWorkspace drives the workspace back to `ready`,
		// and `ready` is planRootWorkspace's REUSE arm — the woken attempt is planned against that SAME
		// allocation. Landing on the same machine, it reuses it. Landing elsewhere, it meets the
		// workspace-affinity ceiling A.3 T5 already named and reuseAllocation already fails loudly on
		// (provision.go: a reused allocation whose machine is not the one this attempt dialed has no
		// repository to read). That ceiling is not new here — every retry and every resume has had it
		// since provisioning moved after the dial — but a capacity park is a new way to MEET it, because
		// the wake is keyed on the pool and not on the machine whose slot freed.
		occupancyID, herr := o.holdMachine(ctx, tenant, sessionID, ch)
		if errors.Is(herr, coordinator.ErrMachineAtCapacity) {
			if perr := o.parkForCapacity(ctx, tenant, attempt); !errors.Is(perr, errRunAwaitingCapacity) {
				return perr
			}
			return nil
		}
		machineOccupancyID = occupancyID
	}

	st := &attemptState{
		attempt: attempt, tenant: tenant, sessionID: sessionID, responseID: responseID,
		ch: ch, ledger: runner.NewFrameLedger(),
		workspaceID: workspaceID, workspaceLeaseID: workspaceLeaseID,
		attemptStart: time.Now(),
	}

	// This attempt's environment key NAMES, from the run's PINNED revision (E25 T3). Read here, at attempt
	// start, so the set is fixed before the engine says anything — the scope half of the worker
	// secret-handle pattern. The VALUES are resolved immediately before each exec call, never here.
	if st.envKeys, err = o.resolveEnvKeys(ctx, st); err != nil {
		return err
	}

	st.depth = depth
	st.outputContract = outputContract
	st.runInstructions = runCtx.Instructions
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
	// Compaction budget (E21 T1). The budget IS a model's context window, and the run's resolved
	// route is where such a window would come from — except that measured against this tree on
	// 2026-07-27 nothing stores one: not model_routes, not the broker, not either provider adapter.
	// So there is no per-model window to resolve, every run folds at the conservative default, and
	// calling effectiveRoute here to read a field that does not exist would be theatre. This is the
	// line that reads it the day a route revision carries one; until then the default IS the
	// fail-closed answer.
	if messages := historyMessages(prior, defaultHistoryBudgetChars); len(messages) > 0 {
		runStart["messages"] = messages
	}
	// Seed required delegations (spec §25.18): the engine emits one child.request per spec at the
	// safe boundary after its first model step. Config-driven, so a real single-step run delegates.
	if delegations := deleg.emitFrames(); len(delegations) > 0 {
		runStart["delegations"] = delegations
	}
	// Seed the §22.7 output contract. THIS IS THE WRITER for engine.schema.json's run.start
	// data.output_contract, and engines/reference/src/palai_engine/loop.py:_finish is the reader:
	// it validates the final answer against this schema and ends the run failed with
	// schema_validation_failed rather than completed. Omitted entirely for a run that demands
	// nothing, so a text run's run.start frame is BIT-IDENTICAL to the pre-§22.7 one.
	if frame := outputContractFrame(st.outputContract); frame != nil {
		runStart["output_contract"] = frame
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
			if errors.Is(err, errContextOverflow) {
				// Named, not retried: the history that did not fit will not fit on attempt two.
				return o.failContextOverflow(ctx, st)
			}
			if errors.Is(err, errProviderPermanent) {
				// Named, not retried: the provider has already answered this exact request. The full
				// sanitized failure — including the provider's own explanation — is logged below by the
				// generic branch's sibling; here the run is settled so it stops saying "Working…".
				log.Printf("run %s step %d: provider rejected the request permanently: %v", st.attempt.RunID, st.modelStepIndex, err)
				return o.failProviderRejected(ctx, st)
			}
			if err != nil {
				// THE CAUSE, WHERE AN OPERATOR CAN REACH IT. Measured 2026-08-02 driving the iOS live chain:
				// an agent-driven run parked at `safe_boundary before_model, step 1`, the engine exited 0
				// having never called the model, the runner re-leased it five times, and the whole event was
				// `run.failed.v1 {state, run_id}` with an EMPTY control-plane log. The error was in this
				// variable the entire time.
				//
				// It is logged rather than surfaced in the terminal problem for the reason terminalProblems
				// exists: a model dispatch failure can carry raw PROVIDER text, and that must not reach a
				// caller. The log is this deployment's own, and its operator is exactly who needs it.
				log.Printf("run %s step %d: model dispatch failed: %v", st.attempt.RunID, st.modelStepIndex, err)
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
			case errors.Is(err, errRunParked):
				// A gated tool call is waiting on a human (spec §22.4, E23 T1): the run is WAITING, this
				// attempt ends cleanly and releases its compute — the worker is freed and no engine process
				// is held while somebody reads — and the decision (or the expiry reaper) opens a fresh
				// attempt through the one wake. Exactly as a pause ends and resume reopens.
				return nil
			case errors.Is(err, errToolAnswerBudget):
				// The model has been handed more tool refusals than the budget allows (tool_answer.go).
				// NAMED, NOT RETRIED — the same shape as errContextOverflow above: a conversation that made
				// sixteen refused calls replays into the same sixteen, so the retry ladder would spend its
				// attempts reproducing them before dead-lettering into the generic failure this avoids.
				return o.failToolAnswerBudget(ctx, st)
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
			switch err := o.finalize(ctx, st, frame); {
			case errors.Is(err, errRunParked):
				// The model finished its turn while a background task it started is still running (E26
				// T3): the run is WAITING, this attempt ends cleanly and releases its compute, and the
				// task's exit re-enters it through the one wake. Exactly as a pause ends and resume
				// reopens — and exactly as the approval arm above, which is the point of there being one
				// sentinel rather than two.
				return nil
			default:
				return err
			}
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

// errContextOverflow marks a provider failure caused by the request not fitting the model's context
// window. It is a SENTINEL, not a message: model_dispatch wraps the sanitized provider failure with
// it and the run loop acts on it, so the two ends agree without parsing each other's text.
var errContextOverflow = errors.New("context_overflow")

// contextOverflowCodes are the provider error codes that mean "this did not fit". The classification
// keys on the CODE and only the code.
//
// THAT WAS ONCE FORCED AND IS NOW A CHOICE, and the difference is worth stating because the old reason is
// still written in half the tree: both adapters used to discard the provider's free text outright, so
// there was no message to match against, by construction. As of 2026-08-04 provider-two's sanitizeError
// carries the provider's explanation with the credential redacted out of it, so a message IS available
// for that family. The classification still does not read it — matching on a provider's free-form
// sentence is how a wording change silently reclassifies a failure — but the text is now there for the
// operator in the log, which is what it is for.
//
// CONTRACT: OpenAI API errors — https://developers.openai.com/api/docs/guides/error-codes
// (fetched 2026-07-27). UNCONFIRMED IN THE DOCUMENT: that page enumerates only status-level
// conditions (401/403/429/500/503) and does not publish a table of `code` values at all, so
// `context_length_exceeded` is recorded here as the code the API returns in practice rather than as
// a documented guarantee. provider_one's sanitizeError takes error.code when present, else
// error.type, so this is the string that reaches us.
//
// CONTRACT: Anthropic Messages API errors — https://platform.claude.com/docs/en/api/errors
// (fetched 2026-07-27). `request_too_large` (413) IS documented: "Request exceeds the maximum
// allowed number of bytes", 32 MB on the Messages API. That is the oversized-conversation failure
// for provider-two and it is classified here.
//
// WHAT IS DELIBERATELY NOT CLASSIFIED, and this is the honest ceiling of the whole bullet: a prompt
// over the CONTEXT WINDOW (as opposed to the byte ceiling) is, on Anthropic, a 400
// `invalid_request_error` — the same documented type as a malformed tool block, an unsupported
// thinking parameter, or a prefilled assistant message. provider_two's sanitizeError reads
// error.type, so every one of those arrives as the same code. Classifying it would fail every
// provider-two 400 as a context overflow, which is a worse lie than not naming it at all. On
// provider-two the byte budget in history.go is the defence and this classifier is not.
var contextOverflowCodes = map[string]bool{
	"context_length_exceeded": true, // provider-one
	"request_too_large":       true, // provider-two
}

// isContextOverflow reports whether a sanitized provider failure is an oversized request.
func isContextOverflow(e *modelbroker.SanitizedError) bool {
	return e != nil && contextOverflowCodes[e.Code]
}

// errProviderPermanent marks a provider failure the SAME request can never recover from. It is a
// sentinel in errContextOverflow's shape, for the same reason: model_dispatch wraps the sanitized failure
// and the run loop acts on it, so neither end parses the other's text.
//
// Named apart from model_dispatch's errProviderRejected on purpose — that one is the METRICS
// classification for "the result carried an error at all" and is never surfaced; this one is a statement
// that retrying is futile.
var errProviderPermanent = errors.New("provider_rejected_permanently")

// isPermanentProviderRejection reports whether retrying this exact request is pointless.
//
// THE COST OF NOT HAVING THIS, measured 2026-08-04 on the live deployment: a malformed request drew
// `400 invalid_request_error`, the attempt failed into the retry ladder, and the ladder re-sent the
// BYTE-IDENTICAL body once a second. The provider answered 400 every time, because a 400 is a statement
// about the request and the request never changed. The run sat in `in_progress` and the operator watched
// "Working…" forever — the run never reached a terminal state that could say why.
//
// The classification is by STATUS CLASS, not by code, and that is deliberate: provider-two returns the
// same `invalid_request_error` type for a malformed image block, an unsupported parameter and an
// oversized prompt, so a code table would have to enumerate every provider's vocabulary to say the one
// thing the status already says. 4xx means the caller must change something; 5xx and 429 mean the server
// might answer differently in a moment, and those stay on the ladder.
//
//   - 408 and 429 are EXCLUDED: both explicitly invite a retry.
//   - 401/403 are INCLUDED. A rejected credential does not become accepted by asking again a second
//     later; the operator has to rotate or re-scope it, and a run hanging in `in_progress` hides that
//     from them exactly as a 400 did.
func isPermanentProviderRejection(e *modelbroker.SanitizedError) bool {
	if e == nil || e.Status < 400 || e.Status >= 500 {
		return false
	}
	return e.Status != 408 && e.Status != 429 // Request Timeout / Too Many Requests: both invite a retry
}

// providerRejectedProblem is the terminal Response error a permanently-rejected run projects.
//
// IT CARRIES NO PROVIDER TEXT, and that is the terminalProblems boundary rather than an oversight: the
// provider's own sentence names request internals and reaches a TENANT here, while the operator who
// needs it already has it in this deployment's log (the dispatch failure is logged with the full
// sanitized message). The title still does the job Slack needs — renderSlackReply appends it, so a person
// is told the request was rejected rather than being told nothing at all.
func providerRejectedProblem() contracts.Problem {
	return contracts.Problem{
		Type:   problemTypePrefix + "provider_rejected",
		Code:   "provider_rejected",
		Title:  "The model provider rejected this request",
		Status: 502,
		Detail: "the model provider refused the request and retrying it unchanged cannot succeed; " +
			"the deployment's control-plane log carries the provider's own explanation",
	}
}

// contextOverflowProblem is the terminal Response error an overflowed run projects. It is separate
// from terminalProblems' generic "Internal error" for one reason: Slack's failure line appends the
// problem TITLE (renderSlackReply), so this title is the difference between a person being told the
// conversation got too long and a person being told nothing. Status 400 — the request was too big,
// which is a caller-side fact, and it is NOT retryable: the same history retried is the same size.
func contextOverflowProblem() contracts.Problem {
	return contracts.Problem{
		Type:   problemTypePrefix + "context_length_exceeded",
		Code:   "context_length_exceeded",
		Title:  "The conversation is too long for the model",
		Status: 400,
		Detail: "the request exceeded the model's context window even after older turns were dropped; " +
			"start a new thread, or send a clear command to reset this session's history",
	}
}

// failContextOverflow drives the run to an explicit, NAMED terminal failure. It mirrors
// failRecovery's shape deliberately — one terminal transition, one projection — and it returns nil
// so the attempt ends cleanly instead of failing the job: an oversized request is DETERMINISTIC, so
// the retry ladder would spend eight attempts and every one of them would send the same too-large
// history to the same provider before dead-lettering into the generic failure this exists to avoid.
// failProvisioning ends a run whose workspace could not be prepared, CARRYING THE REASON. It is
// failContextOverflow's shape — one terminal transition, one projection, nil returned so the attempt
// ends cleanly rather than entering the retry ladder.
func (o *Orchestrator) failProvisioning(ctx context.Context, tenant coordinator.Tenant, attempt AttemptDescriptor, responseID string, cause error) error {
	switch _, err := o.spine.ApplyRunTransition(ctx, tenant, string(attempt.RunID), statemachines.RunCmdFail); {
	case errors.Is(err, coordinator.ErrRunTerminal), errors.Is(err, statemachines.ErrInvalidState):
		return nil // another path already settled this run; its projection stands
	case err != nil:
		return err
	}
	problem := contracts.Problem{
		Code:   "workspace_provisioning_failed",
		Title:  "Workspace could not be prepared",
		Status: 500,
		Detail: "the run's repository workspace could not be prepared: " + sanitizeProvisioningCause(cause) +
			". Check the binding's clone_url and default branch, and whether a private repository needs a " +
			"connection_ref naming a token in this deployment's secret store.",
	}
	problem.Type = problemTypePrefix + problem.Code
	projection, _ := json.Marshal(map[string]any{"error": problem})
	return o.spine.FinalizeResponse(ctx, tenant, responseID, "failed", projection)
}

// failPlacement ends a run whose resolved pool is not one its tenant can be served by, with a Response
// body that says which pool and what to do about it. It is failProvisioning's shape for the same reason:
// the fault is the deployment's own configuration, not a provider or an engine, so naming it carries no
// untrusted text — and the person waiting gets an error instead of a run that is `waiting` forever.
func (o *Orchestrator) failPlacement(ctx context.Context, tenant coordinator.Tenant, attempt AttemptDescriptor, responseID string) error {
	switch _, err := o.spine.ApplyRunTransition(ctx, tenant, string(attempt.RunID), statemachines.RunCmdFail); {
	case errors.Is(err, coordinator.ErrRunTerminal), errors.Is(err, statemachines.ErrInvalidState):
		return nil // another path already settled this run; its projection stands
	case err != nil:
		return err
	}
	problem := contracts.Problem{
		Code:   "runner_pool_not_available",
		Title:  "No runner pool serves this project",
		Status: 503,
		Detail: "the run resolved to runner pool " + strconv.Quote(attempt.PoolID) + ", which this project " +
			"cannot be served by, so no machine of yours can ever take it. Create a pool named 'default' for " +
			"this project, or name an existing one in the project's config_policy pool field.",
	}
	problem.Type = problemTypePrefix + problem.Code
	projection, _ := json.Marshal(map[string]any{"error": problem})
	return o.spine.FinalizeResponse(ctx, tenant, responseID, "failed", projection)
}

// maxProvisioningCause bounds the reason. git's stderr on a bad fetch is a handful of lines; anything
// longer is a surprise and is truncated rather than pasted whole into a projection an API returns.
const maxProvisioningCause = 400

// sanitizeProvisioningCause flattens the cause to one bounded line, because the detail lands in a JSON
// problem document read in a terminal and a browser, and a multi-line detail renders as a run-on line in
// both — collapsing it deliberately beats letting the renderer do it accidentally.
func sanitizeProvisioningCause(cause error) string {
	msg := strings.Join(strings.Fields(cause.Error()), " ")
	if len(msg) > maxProvisioningCause {
		msg = msg[:maxProvisioningCause] + "…"
	}
	return msg
}

func (o *Orchestrator) failContextOverflow(ctx context.Context, st *attemptState) error {
	switch _, err := o.spine.ApplyRunTransition(ctx, st.tenant, string(st.attempt.RunID), statemachines.RunCmdFail); {
	case errors.Is(err, coordinator.ErrRunTerminal), errors.Is(err, statemachines.ErrInvalidState):
		return nil // another path already settled this run; its projection stands
	case err != nil:
		return err
	}
	problem := contextOverflowProblem()
	projection, _ := json.Marshal(map[string]any{
		"output": st.output,
		"usage":  st.usage,
		"model":  st.model,
		"error":  problem,
	})
	return o.spine.FinalizeResponse(ctx, st.tenant, st.responseID, "failed", projection)
}

// failProviderRejected drives the run to an explicit, NAMED terminal failure when the provider refused
// the request in a way retrying cannot fix. failContextOverflow's shape exactly: one terminal transition,
// one projection, and nil returned so the attempt ends cleanly instead of feeding the retry ladder a
// request the provider has already answered.
func (o *Orchestrator) failProviderRejected(ctx context.Context, st *attemptState) error {
	switch _, err := o.spine.ApplyRunTransition(ctx, st.tenant, string(st.attempt.RunID), statemachines.RunCmdFail); {
	case errors.Is(err, coordinator.ErrRunTerminal), errors.Is(err, statemachines.ErrInvalidState):
		return nil // another path already settled this run; its projection stands
	case err != nil:
		return err
	}
	problem := providerRejectedProblem()
	projection, _ := json.Marshal(map[string]any{
		"output": st.output,
		"usage":  st.usage,
		"model":  st.model,
		"error":  problem,
	})
	return o.spine.FinalizeResponse(ctx, st.tenant, st.responseID, "failed", projection)
}

// failToolAnswerBudget drives the run to an explicit, NAMED terminal failure when its model has been
// handed more tool refusals than the deployment's budget allows (tool_answer.go). It is
// failContextOverflow's shape, deliberately: one terminal transition, one projection, and nil returned
// so the attempt ends cleanly instead of failing the job into the retry ladder.
//
// The refusals themselves are already durable — every one of them was COMMITTED to its tool_calls row
// before this was reached — so the operator this failure is addressed to can read exactly which call
// kept being refused, which is the thing a generic "internal error" never told anybody.
func (o *Orchestrator) failToolAnswerBudget(ctx context.Context, st *attemptState) error {
	switch _, err := o.spine.ApplyRunTransition(ctx, st.tenant, string(st.attempt.RunID), statemachines.RunCmdFail); {
	case errors.Is(err, coordinator.ErrRunTerminal), errors.Is(err, statemachines.ErrInvalidState):
		return nil // another path already settled this run; its projection stands
	case err != nil:
		return err
	}
	projection, _ := json.Marshal(map[string]any{
		"output": st.output,
		"usage":  st.usage,
		"model":  st.model,
		"error":  toolAnswerBudgetProblem(st.toolAnswerErrors),
	})
	return o.spine.FinalizeResponse(ctx, st.tenant, st.responseID, "failed", projection)
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
