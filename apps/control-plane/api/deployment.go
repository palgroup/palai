package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// GET /v1/deployment — THE MACHINE'S OWN CONFIGURATION.
//
// Measured on main at cf0efd63 (2026-08-01):
//
//	grep -oE 'PALAI_[A-Z_]+' deploy/compose/compose.yaml | sort -u   -> 24 settings
//	of those, readable from any /v1 route                            -> 0
//
// Every knob that decides what this deployment DOES lived in container environment, and no client — not
// the console, not the SDK, not `palai` — could read one of them. The cost is not hypothetical: a stack
// brought up with `make local-up` takes PALAI_DISPATCH_WORKERS=0 from compose.yaml:82, accepts runs
// through POST /v1/responses, and executes none of them. The console shows the run parked at
// run.queued.v1 and says nothing, because nothing on the wire could have told it. The value is currently
// discoverable with `docker inspect`, which is not an operator interface.
//
// THIS IS NOT config_policy AND MUST NOT BE. `config_policy` is a JSONB column on the PROJECTS table
// (storage/migrations/000005_config_revisions.up.sql:16), written per project through
// PATCH /v1/projects/{id} and resolved as the §14 project layer. A dispatch worker count is not a
// property of a project — it is a property of the process, shared by every project on it, and putting a
// machine-wide switch behind a project picker would make it look per-project to the one reader who most
// needs to know it is not. The two surfaces answer different questions and neither can answer the other's.
//
// WHAT THIS SURFACE IS ALLOWED TO CLAIM. It reports THIS PROCESS's environment and nothing else. That is
// a real ceiling and it is why PALAI_RUNNER_CONCURRENCY is absent: the variable lives on the RUNNER
// container, and cmd/cli/internal/stack/upgrade.go says what happens to a reader that forgets —
// "reading a runner-scoped var off the control-plane container always misses it". A control plane
// reporting its own (unset) copy and labelling it the runner's would be a confident wrong answer, which
// is worse than the silence this surface replaces.

// The mutability vocabulary. It answers ONE question — what does an operator have to do for a new value
// to take effect — and it has exactly two answers because the code has exactly two.
//
// There is deliberately no "live" word. Every variable below is read from the PROCESS ENVIRONMENT, which
// is fixed at exec: even the handful read per-job rather than at boot (execute_run.go's ExecuteRun closes
// over PALAI_ENGINE_IMAGE when the handler is built) are reading a value nothing in this tree can change
// without replacing the process. A surface offering an "edit" control for any of them would be the exact
// defect this repository keeps finding — declared, and nothing happens.
const (
	// mutabilityBringUp: the value is read once from this process's environment; a new value needs the
	// process replaced with it (a compose recreate, `palai up`, or a native relaunch).
	mutabilityBringUp = "bring_up"
	// mutabilityDefaultOnly: the same, EXCEPT that the behaviour it governs has a shipped runtime
	// write-path, so the environment value only decides the fallback. Naming these apart from
	// mutabilityBringUp is the point of the distinction: an operator told "restart the stack" when a POST
	// would have done has been sent on an outage they did not need.
	mutabilityDefaultOnly = "bring_up_default_only"
)

// The value vocabulary. `path` exists so a reader can tell a filesystem handle from a setting: the
// credential rule in this tree is that a *_FILE variable's VALUE is a path and a path is not a secret.
const (
	kindValue = "value"
	kindPath  = "path"
)

// THE PLANE VOCABULARY — WHICH PROCESS reads the setting. It is a different question from mutability and
// from kind, and it is the axis this surface did not have until E29's scoping pass.
//
// THE QUESTION IT ANSWERS is "can this be configured per MACHINE?", and the answer is now three-of-thirty-
// nine rather than the none this paragraph used to report. It said "all thirty-five are read by the
// control-plane PROCESS"; both numbers had moved. Re-measured against the live stack 2026-08-03 — and the
// command is carried so the next reader re-runs it instead of trusting the sentence:
//
//	curl -s .../v1/deployment | jq -r '.settings[].plane' | sort | uniq -c
//	  36 control_plane
//	   3 runner_pool
//
// Measured per binary the same day, by grepping each variable in cmd/runner and packages/runner:
//
//	                          control plane          cmd/runner
//	PALAI_RUNNER_CONCURRENCY  —                      main.go:118  planeIntDefault(identity.Settings, …)
//	PALAI_SANDBOX_IMAGE       main.go:69, 926        —
//	PALAI_SHELL_NATIVE        main.go:69, 1026       —            (ZERO runner readers — see below)
//	PALAI_WORKSPACE_ROOT      main.go:677            main.go:121
//
// THE FIRST ROW HAS INVERTED SINCE THIS TABLE WAS WRITTEN, and the sentence under it inverted with it. It
// read: "why PALAI_RUNNER_CONCURRENCY is in unreportedSettings and not here: it is the ONE genuinely
// per-machine knob and this process holds no copy of it." The variable is now a catalogue ENTRY (the
// runner-plane one, `Observable: false`) and is NOT in unreportedSettings; what stayed true is the reason
// it was there — this process still holds no copy, which is what Observable=false reports.
//
// THE THIRD ROW IS THE ONE THAT DECIDES WHAT A MACHINE PLANE MAY CARRY. PALAI_SHELL_NATIVE has ZERO readers
// in cmd/runner and packages/runner:
//
//	grep -rn PALAI_SHELL_NATIVE --include='*.go' cmd/runner/ packages/runner/   ->  (no output, 2026-08-03)
//
// A runner does not run tools — the lease carries the engine and the shell tool runs control-plane-side —
// so putting the shell posture on either runner plane would write a value into a process that never reads
// it, which is exactly what readerOf() refuses. It is a control-plane setting on a machine that happens to
// host a control plane, and that is a different sentence from "a per-machine setting".
//
// THE LAST ROW IS THE SHARPEST and it is why the plane belongs in the KEY rather than in a document —
// PALAI_WORKSPACE_ROOT has two readers with two DIFFERENT jobs under one name (the control plane ALLOCATES
// workspaces under it; the runner refuses to bind-mount a leased path that does not sit under it, and
// "unset disables the check"). A flat document forces one string into both and, reaching only the control
// plane, would move the allocation root while leaving the check that guards it switched off.
const (
	// planeControlPlane: read by the control-plane process. Every catalogued setting is one of these, and
	// planeMatchesReader() is what makes that a test rather than a claim.
	planeControlPlane = "control_plane"
	// planeRunnerPool: read by a RUNNER's own process, configured per pool because a pool is the unit a
	// machine belongs to (`GET /v1/runner-pools` → 17 on the live stack; a runner row carries pool_id).
	// NOT os/arch: measured 2026-08-01, all three enrolled runners report os="" arch="" — cmd/runner never
	// sends them (runner_gateway.go's enrolRequest calls them "inventory (T4 may compare them)"), so a
	// design that selected machines by either would be selecting on a field nothing populates.
	//
	// THREE CATALOGUE ENTRIES CARRY IT, and this sentence replaces one that said none did. It read
	// "NOTHING IN THIS CATALOGUE CARRIES IT YET ... the write path refuses it BY NAME because the reader is
	// a second binary this task does not ship", and it was true when written; `ffb84f3b` gave the plane a
	// reader and three entries moved onto it while the paragraph justifying their absence stayed. Measured
	// against the live stack 2026-08-03, which is the form that cannot go stale silently:
	//
	//	curl -s .../v1/deployment | jq -r '.settings[] | select(.plane=="runner_pool") | .name'
	//	  PALAI_RUNNER_CONCURRENCY   (writable)
	//	  PALAI_RUNNER_POSTURE       (not writable — no DesiredValue grammar)
	//	  PALAI_RUNNER_POOL          (not writable — no DesiredValue grammar)
	planeRunnerPool = "runner_pool"
	// planeRunnerMachine: read by the SAME process as planeRunnerPool — a runner's own — and scoped to ONE
	// machine by runner id. The two planes differ in SCOPE, not in reader, and that conflation is inherited
	// rather than introduced: `plane` in this model has always answered both "which process reads it" and
	// "which scope does the document configure", and migration 000060's header records what a two-axis
	// model would have cost instead.
	//
	// IT EXISTS BECAUSE "THIS MAC" IS NOT A POOL. A rented Mac that serves four sessions and one that
	// serves one sit in the same pool — posture `unsandboxed-host` is what a pool carries — and before this
	// plane the only way to configure them apart was a pool per machine, which turns the unit that owns
	// POSTURE and ENROLMENT KEYS into a per-machine container it was not designed to be.
	//
	// A MACHINE DOCUMENT OVERLAYS ITS POOL'S, KEY BY KEY (store.DesiredSettingsForMachine). It is resolved
	// on read rather than flattened on write, so a later pool edit still reaches a machine that has been
	// individually configured — a flattened copy would freeze the pool's values at write time and leave the
	// pool edit silently not arriving, which is "declared, and nothing happens" one layer down.
	planeRunnerMachine = "runner_machine"
)

// PlaneRunnerPool is planeRunnerPool for the callers outside this package: the enrolment handler's store
// read (store.DesiredSettingsForPool) and the settings poll's overlay (store.DesiredSettingsForMachine),
// which answer a machine its own pool's document. Exported rather than re-spelled as a literal because
// migration 000053's CHECK constraint enforces the same two words, and a second spelling of a value a
// database constraint holds is a silent mismatch waiting for the first typo.
const PlaneRunnerPool = planeRunnerPool

// PlaneRunnerMachine is planeRunnerMachine for the same reason and the same callers — 000060's CHECK
// constraint carries the identical string, and the overlay in store.DesiredSettingsForMachine is the one
// production reader that keys on it.
const PlaneRunnerMachine = planeRunnerMachine

// The DESIRED-value grammar. Every writable setting declares one, and it names the STANDARD LIBRARY CALL
// this binary's own reader makes — not a shape somebody thought looked right.
//
// THAT IS THE WHOLE POINT OF THE GRAMMAR AND IT IS NOT A STYLE CHOICE. Every reader of every catalogued
// setting COERCES SILENTLY on a value it cannot parse, and each one coerces to something different:
//
//	DispatchWorkers        (deployment.go)  strconv.Atoi        -> 1  on error
//	envInt                 (main.go:1994)   strconv.Atoi        -> 0  on error
//	envFloat               (main.go:1978)   strconv.ParseFloat  -> 0  on error
//	envDuration            (main.go:1986)   time.ParseDuration  -> 0  on error
//	envDurationOr          (main.go)        time.ParseDuration  -> the named default on error
//
// So `PALAI_SANDBOX_WALL_TIME=10min` is 10 minutes to a human, an unparseable duration to Go, and the
// DEFAULT to this binary — main.go:996 already writes that down. A write surface that stored "10min"
// would show an operator a wall time the process is not running, which is the defect this whole file was
// built to expose, shipped INTO it. So a value the reader would coerce is REFUSED at the door, and
// TestEveryDesiredValueThisBinaryAcceptsIsParsedByItsOwnReader (in the composition root's test, where the
// real helpers live) drives every accepted value through the real reader to prove the two agree.
const (
	desiredInt      = "integer"  // strconv.Atoi, and non-negative
	desiredRate     = "rate"     // strconv.ParseFloat, finite and non-negative
	desiredDuration = "duration" // time.ParseDuration, non-negative
	desiredToken    = "token"    // a bare identifier: see desiredTokenPattern
)

const (
	warnDispatchOff = "dispatch_workers_zero"
	warnModelFake   = "model_provider_fake"
	// warnDesiredPending: an operator saved a configuration this process is not running. ADVISORY rather
	// than blocking, and the distinction is the honest one — the machine is working, it is working with the
	// PREVIOUS configuration, and the remedy is a bring-up somebody has to choose to run. Calling it
	// blocking would put a red banner on a healthy deployment for as long as one setting was pending.
	warnDesiredPending = "desired_config_pending"
	// warnWorkspaceRootPlane: this deployment provisions workspaces, and the SAME variable name has a second
	// reader on a plane this process cannot see. Advisory, and it clears the moment workspaces are off.
	warnWorkspaceRootPlane = "workspace_root_runner_plane"

	severityBlocking = "blocking"
	severityAdvisory = "advisory"
)

// deploymentSetting is one row of the effective configuration.
type deploymentSetting struct {
	Name  string `json:"name"`
	Group string `json:"group"`
	// Value is what this process holds, empty when the variable is unset. A URL's userinfo is stripped
	// (see reportedValue) — no catalogued variable is supposed to carry one, and "supposed to" is not a
	// property a response body should depend on.
	Value string `json:"value"`
	Set   bool   `json:"set"`
	// Default is what the process uses when the variable is unset, in the words of the code that applies
	// it. "unset" on its own tells a reader nothing about what is running.
	Default    string `json:"default"`
	Kind       string `json:"kind"`
	Effect     string `json:"effect"`
	Mutability string `json:"mutability"`
	ChangeWith string `json:"change_with"`
	// ReaderFile/ReaderFunc name the code that reads this variable. They are a FUNCTION rather than a
	// line so an unrelated edit above the reader does not redden a citation nobody re-read, and
	// TestEveryCatalogueCitationResolvesToARealReader parses the file and asserts that function's source
	// actually mentions the variable — so the mutability claim above is anchored to code rather than to
	// somebody's recollection of it.
	ReaderFile string `json:"reader_file"`
	ReaderFunc string `json:"reader_func"`
	// Writable reports whether the panel may write a DESIRED value for this setting. It is served rather
	// than derived by each client, so a console and a CLI cannot disagree about which
	// controls exist — and a client written against an older deployment gets `false` and renders nothing,
	// which is the safe direction.
	Writable bool `json:"writable"`
	// NotWritableBecause is the sentence from nonDesiredReason, empty when Writable. It is served for the
	// same reason the effect prose is: an operator who cannot change something on this screen is entitled
	// to the reason without reading Go source, and most of the catalogue is in that position.
	NotWritableBecause string `json:"not_writable_because,omitempty"`
	// Desired is what the desired document asks for, and DesiredSet distinguishes "the operator decided
	// this" from "no opinion" — an empty Desired with DesiredSet false is the second, and the two are
	// different facts about the same machine.
	Desired    string `json:"desired"`
	DesiredSet bool   `json:"desired_set"`
	// Plane names WHICH PROCESS reads this setting: `control_plane` is this one, `runner_pool` is a machine's
	// own runner. It is on the wire because a row whose value this process cannot observe must be
	// DISTINGUISHABLE from one that is genuinely unset — those are opposite facts wearing the same empty
	// string, and this surface exists because opposite facts wearing one word cost an evening.
	Plane string `json:"plane"`
	// Observable is false when the setting is read on ANOTHER plane. The value/set/drift fields are then
	// empty because this process holds no copy — not because the setting is unset on the machine that does
	// read it. deployment.go's header calls the alternative "a confident wrong answer, which is worse than
	// the silence this surface replaces".
	Observable bool `json:"observable"`
	// ValueGrammar names the shape a written value must have — the STANDARD LIBRARY CALL this binary's own
	// reader makes, so a console can tell an operator "a Go duration" instead of leaving them to discover
	// that `10min` is not one by watching it not take effect. Empty when the setting is not writable.
	ValueGrammar string `json:"value_grammar,omitempty"`
	// Drift is DesiredSet && Desired != Value: this process is not running what the document asks for, so
	// a bring-up is pending. It compares the raw environment STRINGS, which is what the next bring-up will
	// export — see desiredDrift for why comparing behaviour instead gets PALAI_DISPATCH_WORKERS backwards.
	Drift bool `json:"drift"`
}

// deploymentWarning is a configured value that changes what the product DOES, in a way a screen showing
// the product would otherwise misrepresent.
//
// IT NAMES NO CONSOLE PATH, on purpose. Which screen would lie is the console's own question — it is the
// only thing that knows what its screens claim — and a control plane carrying a list of `/runs` and
// `/history` would be a server with an opinion about a client it never serves. The `code` is the join.
type deploymentWarning struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Headline string `json:"headline"`
	Detail   string `json:"detail"`
	Remedy   string `json:"remedy"`
	// Settings names the rows this warning was derived from, so a screen can link the banner to the table.
	Settings []string `json:"settings"`
}

type deploymentBody struct {
	Object   string              `json:"object"`
	Settings []deploymentSetting `json:"settings"`
	Warnings []deploymentWarning `json:"warnings"`
	// Desired is the desired document's own metadata, or NULL when no operator has ever written one — which
	// is a fact a screen must be able to state plainly. A machine with no desired document is running on its
	// compose file's defaults, and rendering an empty object here would let a screen imply the panel is in
	// control when the compose file still is.
	Desired *desiredView `json:"desired"`
}

// catalogueEntry is one declared setting. The catalogue is an ALLOW-LIST and that is the whole of the
// credential rule: a variable nobody has decided about is INVISIBLE rather than published, so the failure
// mode of adding PALAI_SECRET_PROVIDER_THREE tomorrow is a missing row, not a leaked key.
type catalogueEntry struct {
	Name       string
	Group      string
	Kind       string
	Default    string
	Effect     string
	Mutability string
	ChangeWith string
	ReaderFile string
	ReaderFunc string
	// Plane names WHICH PROCESS reads this setting, and it is not free prose: planeMatchesReader() derives
	// the plane from ReaderFile and fails when the two disagree. So the plane inherits the proof
	// TestEveryCatalogueCitationResolvesToARealReader already provides — that file, that function, and that
	// function's source really does mention the variable — instead of being a third thing to keep in step
	// by hand. The zero value is planeControlPlane, which is what every entry in this catalogue is.
	Plane string
	// DesiredValue names the grammar a written value must satisfy, and its EMPTINESS is what makes the
	// setting read-only: desiredWritable() admits a name only if this is non-empty AND Kind != kindPath.
	// There is no separate boolean, because two fields that must agree are a pair that can disagree.
	DesiredValue string
}

// planeOf reads an entry's plane, defaulting to the control plane. A default rather than a required field
// on every entry, because every one of them IS a control-plane setting and a field repeated identically
// on each of them is a field nobody reads — the guard below is what keeps it honest.
func planeOf(entry catalogueEntry) string {
	if entry.Plane == "" {
		return planeControlPlane
	}
	return entry.Plane
}

// readerOf collapses a plane to the BINARY that reads it, and it exists because `plane` answers two
// questions at once — which process reads the setting, and which scope the document configures.
//
// `runner_pool` and `runner_machine` answer the first identically and the second differently. Every
// comparison that means "would this value reach a process that reads it?" must therefore go through here
// rather than comparing plane strings, or a machine document is refused the very setting it exists to
// carry. Every comparison that means "which scope is this?" must NOT: those still compare planes.
//
// The two runner planes returning one string is the whole of the function, and the exhaustive switch is
// what makes a fourth plane a compile-time-visible decision rather than a silent default: a new plane that
// nobody classifies lands in the default arm and gets a reader that matches nothing, so it can carry no
// setting at all until somebody says which binary reads it. That is the safe direction — the same one
// planeMatchesReader takes when a reader file matches no prefix list.
func readerOf(plane string) string {
	switch plane {
	case planeControlPlane:
		return "control-plane process"
	case planeRunnerPool, planeRunnerMachine:
		return "runner process"
	default:
		return "unclassified plane " + plane
	}
}

// controlPlaneReaderFiles are the paths whose code runs INSIDE the control-plane process. A citation into
// one of them means the setting is read there, whatever the entry claims.
//
// IT IS A PREFIX LIST AND THIS TREE SAYS PREFIX COMPARISONS ARE GUILTY — four shipped defeated in E18
// alone. What makes this one safe is the direction it fails in: a path that matches NOTHING here yields no
// derived plane and planeMatchesReader REFUSES rather than passing. A new reader location is a red test
// asking somebody to decide, never a silent control_plane.
// adapters/sandboxes/posture/ IS IN THIS LIST AS A DECISION, AND IT IS THE FIRST ENTRY THAT IS NOT
// WHOLLY TRUE (A.3). That file is read by BOTH binaries: the control plane builds its background runner
// from it, and the runner builds the executor a synchronous command actually runs on. This catalogue has
// one plane per setting, so one of the two readers cannot be named.
//
// It is placed here because the claim the plane makes — "this process reads it, and this process's copy
// is reportable" — is TRUE of the control plane, and moving it to the runner list would make the surface
// report observable:false for three variables this process demonstrably reads. What the placement does
// NOT mean is that setting it here is sufficient: since A.3 a shell command runs on the machine that
// holds the lease, so the MACHINE's copy is what decides whether `palai.workspace.shell` can run
// anything at all. Each entry's Effect says so, because the plane field cannot.
var controlPlaneReaderFiles = []string{
	"apps/control-plane/",
	"packages/version/",
	"adapters/sandboxes/posture/",
}

// runnerReaderFiles are the paths whose code runs inside a RUNNER process. THREE catalogue entries cite
// into it — PALAI_RUNNER_CONCURRENCY, PALAI_RUNNER_POSTURE, PALAI_RUNNER_POOL — which is a change from the
// state this comment used to describe ("empty of catalogue entries today, and listed anyway so the guard
// can tell the two apart"). It is listed for the original reason as well as the current one: with a single
// list the guard would be asserting that everything is what everything is.
//
// It backs BOTH runner planes. readerOf collapses planeRunnerPool and planeRunnerMachine onto one binary,
// and this list is the file-level statement of the same fact — a machine-scoped setting is read by the
// same cmd/runner a pool-scoped one is, so there is no second list to add and adding one would make the
// two planes derivable to different readers.
var runnerReaderFiles = []string{
	"cmd/runner/",
}

// planeMatchesReader derives the plane from the citation and reports whether the entry agrees. ok=false
// with an empty derived plane means the citation points somewhere neither list knows about.
func planeMatchesReader(entry catalogueEntry) (derived string, ok bool) {
	for _, prefix := range controlPlaneReaderFiles {
		if strings.HasPrefix(entry.ReaderFile, prefix) {
			return planeControlPlane, planeOf(entry) == planeControlPlane
		}
	}
	for _, prefix := range runnerReaderFiles {
		if strings.HasPrefix(entry.ReaderFile, prefix) {
			return planeRunnerPool, planeOf(entry) == planeRunnerPool
		}
	}
	return "", false
}

const (
	cpMain = "apps/control-plane/cmd/palai-control-plane/main.go"
	// shellPosture is where the three shell settings are READ since A.3. They moved out of cpMain
	// because the RUNNER reads them too now — it is what executes the command — and a citation that
	// still pointed at the control plane would name a file that no longer decides the answer.
	shellPosture = "adapters/sandboxes/posture/posture.go"
	changeCP     = "recreate the control-plane with the new value (`palai up`, or `docker compose up -d --force-recreate control-plane`)"
	changeAll    = "recreate the whole stack with the new value (`palai up`)"
	// changeDesired is what a WRITABLE setting's remedy became, and the second sentence is a CORRECTION to
	// what this catalogue used to tell every operator.
	//
	// changeCP names two commands as equivalent and for a desired-configuration setting they are not.
	// `palai up` reads the desired document off the machine and exports it (cmd/cli/internal/stack/desired.go);
	// `docker compose up -d --force-recreate control-plane` interpolates ${PALAI_X} from the SHELL THAT
	// INVOKES IT, which knows nothing about the document. An operator who saved a value in the panel and then
	// ran the compose command would recreate the process with the value it already had and see the pending
	// banner survive a restart, with nothing on any screen able to say why.
	changeDesired = "save it here (it is written to this machine's desired configuration) and then run `palai up` on the machine — " +
		"the bring-up reads that document and exports it into the environment it starts the control plane with. " +
		"`docker compose up -d --force-recreate control-plane` does NOT apply it: compose interpolates from the shell that runs it, not from the document"
)

// deploymentCatalogue is the ordered, declared configuration of this process. ORDER IS THE SCREEN'S: the
// two settings that silently change what the product does come first, because an alphabetical dump puts
// PALAI_DISPATCH_WORKERS between the CA paths and the engine image, which is where it was already hiding.
//
// Every ReaderFile/ReaderFunc pair below was measured by parsing the cited file and reading which
// function's body mentions the variable; the guard re-runs that parse on every test run.
var deploymentCatalogue = []catalogueEntry{
	// --- what this deployment actually DOES with a submitted run -------------------------------------
	{
		Name: "PALAI_DISPATCH_WORKERS", Group: "execution", Kind: kindValue, Default: "1",
		Effect:     "How many durable dispatch workers run. ZERO IS NOT 'SLOWER', IT IS OFF: startDispatch returns before it builds anything, so the deployment admits runs through POST /v1/responses and executes none — and with it go the reconciler, the dead-letter sweep, approval expiry, capacity-park expiry and the background-exit notification.",
		Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		// The reader is this file's own DispatchWorkers, and main.dispatchWorkerCount delegates to it, so
		// the number this surface reports and the number startDispatch gates on are the same number.
		ReaderFile: "apps/control-plane/api/deployment.go", ReaderFunc: "DispatchWorkers",
		DesiredValue: desiredInt,
	},
	{
		// THE FIRST RUNNER-PLANE ENTRY, and it is here because the plane finally has a reader. Every comment
		// above saying PALAI_RUNNER_CONCURRENCY is absent from this catalogue was correct when written: the
		// variable lives on the RUNNER, this process holds no copy, and a surface reporting its own blank as
		// somebody else's setting would be a confident wrong answer. What changed is not that this process can
		// now READ it — it still cannot — but that it can now SEND it: RunnerGateway.handleEnroll answers a
		// machine its pool's document and cmd/runner takes this value from it.
		//
		// Plane is what keeps the old comment's point intact. The row builder already marks a runner-plane
		// entry `Observable: false`, so this reports as a setting that EXISTS and is decided elsewhere, rather
		// than as an effective value this process invented. That is why it leaves unreportedSettings: it is no
		// longer an unreported variable, it is a catalogued one whose reader is the other binary.
		Name: "PALAI_RUNNER_CONCURRENCY", Group: "execution", Kind: kindValue, Default: "1",
		Plane: planeRunnerPool,
		// THE LAST SENTENCE WAS TRUE WHEN IT WAS WRITTEN AND STOPPED BEING TRUE IN THE COMMIT BELOW IT. It
		// read: "The runner reads it at enrolment, so a change reaches a machine when it next enrols rather
		// than at once." Counting which halves survived, rather than writing "now it works":
		//   * "the runner reads it at enrolment" — STILL TRUE. RunnerGateway.handleEnroll answers an
		//     enrolling machine its pool's document and cmd/runner takes its lease count from that answer.
		//   * "a change reaches a machine when it next ENROLS" — NO LONGER TRUE, and it was the operative
		//     half. Migration 000060 added a settings POLL: packages/runner/serve.go's settingsLoop asks
		//     every defaultSettingsInterval (30s unless PALAI_SETTINGS_INTERVAL says otherwise), and
		//     applySettings RESIZES the lease pool live for this setting specifically. So a machine that has
		//     been running for a week takes a new value without re-enrolling and without a restart.
		// This sentence is rendered on /deployment and in the /fleet configuration forms, which is why it is
		// worth the paragraph: it is what a reader accepts instead of reading cmd/runner.
		Effect:     "How many leases ONE MACHINE in this pool serves at once — the fleet's parallelism knob. A Mac that takes four sessions is this set to 4 on that Mac's pool. A running machine asks for its configuration about every 30 seconds and resizes its lease pool live, so a change reaches it without a restart and without re-enrolling; shrinking is cooperative, so a loop in flight finishes its lease first.",
		Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: "cmd/runner/main.go", ReaderFunc: "main",
		DesiredValue: desiredInt,
	},
	{
		Name: "PALAI_MODEL_PROVIDER", Group: "model", Kind: kindValue, Default: "fake",
		Effect:     "The DEPLOYMENT-DEFAULT model route. Exactly one value — `provider-one` — selects a live provider; every other value, including a provider name this binary can otherwise speak to and including a typo, falls through to the deterministic fake adapter, whose answers are fabricated and render exactly like real ones.",
		Mutability: mutabilityDefaultOnly,
		ChangeWith: "for ONE project, publish a model route (POST /v1/model-routes) — it is resolved per attempt and overrides this with no restart; to change the fallback every project without a route gets, " + changeDesired,
		ReaderFile: cpMain, ReaderFunc: "modelBrokerFromEnv",
		DesiredValue: desiredToken,
	},
	{
		Name: "PALAI_MODEL", Group: "model", Kind: kindValue, Default: "gpt-4o-mini (only when PALAI_MODEL_PROVIDER=provider-one; otherwise the fake adapter's `fake`)",
		Effect:     "The model id the deployment-default route names. Read only on the `provider-one` branch — with any other provider it is inert.",
		Mutability: mutabilityDefaultOnly,
		ChangeWith: "a published project model route names its own model; otherwise " + changeDesired,
		ReaderFile: cpMain, ReaderFunc: "modelBrokerFromEnv",
		DesiredValue: desiredToken,
	},
	{
		Name: "PALAI_OPENAI_COMPATIBLE_BASE_URL", Group: "model", Kind: kindValue, Default: "none — the custom family then has no deployment-wide endpoint",
		Effect:     "The deployment-wide endpoint for the OpenAI-compatible provider family. A model connection carrying its own base URL (migration 000049) wins over it, per request.",
		Mutability: mutabilityDefaultOnly,
		ChangeWith: "give the model connection its own endpoint (POST /v1/model-connections); otherwise " + changeCP,
		ReaderFile: cpMain, ReaderFunc: "modelBrokerFromEnv",
	},
	{
		// It is a PATH and it changes what a run ANSWERS, which is why it is reported rather than exempted.
		// `f24b430e` added it to compose.yaml and to no list; the walk-vs-list guard found it, which is the
		// direction that guard exists for. The runner-scoped exemptions in unreportedSettings say "this
		// process holds no copy" — untrue here: modelBrokerFromEnv reads it in THIS process, at boot.
		Name: "PALAI_FAKE_SCRIPT_FILE", Group: "model", Kind: kindPath,
		Default:    "unset — the deterministic adapter replays the built-in registry.FakeScript and nothing about a run changes",
		Effect:     "A JSON file holding the exchange the DETERMINISTIC adapter replays, so a stack with no provider credential can drive a run that calls a tool and reads its result. It is reachable only as the deployment default: no model connection can name the fake family, so with PALAI_MODEL_PROVIDER=provider-one this file is REFUSED at boot rather than loaded and never consulted. A set-but-unreadable file is fatal at boot for the same reason — it is the only input that can make modelBrokerFromEnv fail.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "modelBrokerFromEnv",
	},
	{
		Name: "PALAI_ENGINE_IMAGE", Group: "execution", Kind: kindValue, Default: "none — a lease pins no image and a dispatched run cannot start an engine",
		Effect:     "The engine image digest the control plane pins into every lease. The runner refuses a mutable tag, so this is an immutable sha256 reference.",
		Mutability: mutabilityBringUp, ChangeWith: changeAll,
		ReaderFile: "apps/control-plane/internal/execution/execute_run.go", ReaderFunc: "ExecuteRun",
	},
	{
		Name: "PALAI_QUEUE_DEADLINE", Group: "execution", Kind: kindValue, Default: "disabled — a run never expires on queue age",
		Effect:     "How long an admitted run may wait in the queue before dispatch times it out, ahead of any billable compute (§20.12).",
		Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "startDispatch",
		DesiredValue: desiredDuration,
	},
	{
		Name: "PALAI_FLEET_PARK_TTL", Group: "execution", Kind: kindValue,
		Default: "disabled — a run parked for want of a machine waits forever",
		// THE SECOND SENTENCE IS THE ROW'S REAL WORK. Unlike every other setting in this group, this one
		// does nothing at all on a stack that dispatches nothing: the reconciler that runs the sweep is
		// constructed BELOW startDispatch's early return, so PALAI_DISPATCH_WORKERS=0 builds no reconciler,
		// and a control plane that refuses to dispatch (no runner listener bound) takes the same exit. A
		// panel that accepted a TTL there, reported success and expired nothing would be a form that lies.
		Effect: "How long a run parked for want of a MACHINE may wait before it ends as a `timed_out` " +
			"response naming the reason, instead of waiting forever (E24 T5, FLT-P7). Unset means never, " +
			"deliberately — a rented Mac takes six to twenty minutes to start, so any default would be a " +
			"guess about somebody else's fleet. Read only where dispatch runs: with PALAI_DISPATCH_WORKERS=0, " +
			"or on a control plane with no runner listener bound, no reconciler is built and this expires nothing.",
		Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "startDispatch",
		DesiredValue: desiredDuration,
	},
	{
		Name: "PALAI_TOOL_ERROR_BUDGET", Group: "execution", Kind: kindValue, Default: "16",
		Effect: "How many tool REFUSALS one attempt may hand a model before the run ends with a named " +
			"terminal failure (`tool_error_budget_exhausted`). A tool error is delivered to the model AS A " +
			"RESULT so it can correct itself — which is what stopped a guessed filename, and a correctly " +
			"refused traversal, from wedging a run forever — and this is the bound that replaced the " +
			"accidental stop that wedge used to be. It counts refusals, not tool calls: a run making " +
			"progress is not bounded by it. `0` is unbounded and has to be written; a value this binary " +
			"cannot parse falls back to 16 rather than to infinity. docs/operations/tool-errors.md.",
		Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: "apps/control-plane/internal/execution/tool_answer.go", ReaderFunc: "toolAnswerErrorBudget",
		DesiredValue: desiredInt,
	},
	{
		Name: "PALAI_WORKSPACE_ROOT", Group: "execution", Kind: kindPath, Default: "none — no workspace is provisioned and GET /v1/capabilities reports workspaces `unavailable`",
		Effect:     "The host directory coding workspaces are allocated under. Its presence is what makes this deployment advertise the `workspaces` capability at all.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startDispatch",
	},
	{
		Name: "PALAI_WORKSPACE_IDLE_TTL", Group: "execution", Kind: kindValue, Default: "5m",
		Effect: "How long a session's workspace is held before its machine is handed back: the allocation is " +
			"archived, its directory and account are deleted, and the next message restores it onto whichever " +
			"machine is free. A knob, not a switch — there is no value that turns the sweep off, because \"off\" " +
			"is the state that left 107 allocation directories and 97 uid slots held by sessions idle for days. " +
			"The idle tail is NOT billed: an occupancy closed by this sweep is billed to its last activity.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startIdleRelease",
	},

	// --- what this deployment may DIAL, which is a boundary rather than a feature --------------------
	{
		Name: "PALAI_MCP_ALLOW_PRIVATE", Group: "egress", Kind: kindValue, Default: "unset — an MCP server must be a public address; loopback and RFC1918 are refused",
		Effect: "Set to `1`, an MCP server may live on a private address: loopback, RFC1918, link-local. " +
			"A self-hosted deployment's MCP server is frequently on its own network — a Jira behind a VPN, an " +
			"internal wiki — so refusing them outright is the wrong default in the other direction. It WIDENS " +
			"what `egress.VetURL` admits and removes nothing: the pinned dialer stays the connect-time " +
			"authority, so a name that resolves elsewhere after the vet is still refused. One derivation " +
			"serves both the create-time vet and the dial (`mcpAllowsPrivateEgress`).",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "mcpAllowsPrivateEgress",
	},
	{
		Name: "PALAI_A2A_PUSH_ALLOW_PRIVATE", Group: "egress", Kind: kindValue, Default: "unset — a push destination must be a public https address",
		Effect: "Set to `1`, an A2A push notification may be delivered to a private address, for a self-host " +
			"receiver on the same network. Same shape as the MCP flag: it widens `egress.VetURL` and the pinned " +
			"dialer still decides at connect time.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "a2aPusherFromEnv",
	},

	// --- where a shell command runs, which is a security posture rather than a feature ---------------
	{
		Name: "PALAI_SANDBOX_IMAGE", Group: "shell", Kind: kindValue, Default: "none — there is no shell tool; a shell call fails cleanly rather than escaping",
		Effect:     "The pinned command image the workspace shell tool runs inside. Mutually exclusive with PALAI_SHELL_NATIVE — a machine runs its shell commands in the sandbox or on the host, never both. SET IT ON THE RUNNER: since A.3 a shell command runs on the machine that holds the attempt's lease, so this process's copy governs only detached background tasks and a run's shell tool reads the machine's.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: shellPosture, ReaderFunc: "RunnerFromEnv",
	},
	{
		Name: "PALAI_SHELL_NATIVE", Group: "shell", Kind: kindValue, Default: "unset — the sandboxed posture, which is how every existing deployment runs",
		Effect:     "Set to the exact words `unsandboxed-host`, shell commands run on the machine as its uid with NO container boundary, no network denial and no resource bound. The value is a sentence rather than a `1` because deleting a security boundary should not be reachable by the reflex that switches a feature on. SET IT ON THE RUNNER: since A.3 that machine is the one holding the attempt's lease, and this process's copy governs only detached background tasks.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: shellPosture, ReaderFunc: "Resolve",
	},
	{
		Name: "PALAI_SANDBOX_WALL_TIME", Group: "shell", Kind: kindValue, Default: "10m",
		Effect:     "The wall time ONE shell call runs under, in both postures and on both binaries — one derivation, so a machine cannot bound a command differently from the control plane that asked for it. The container posture refuses a non-positive bound.",
		Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: shellPosture, ReaderFunc: "WallTime",
		DesiredValue: desiredDuration,
	},

	// --- what the deployment keeps, and for how long -------------------------------------------------
	{
		Name: "PALAI_S3_ENDPOINT", Group: "storage", Kind: kindValue, Default: "none — NO object store: artifact retrieval, the checkpoint and snapshot sinks, retention's byte deletion and the orphan collector are all off",
		Effect:     "The object store the control plane writes artifacts to. Its credential is a separate variable this surface never reports.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "artifactStoreFromEnv",
	},
	{
		Name: "PALAI_S3_BUCKET", Group: "storage", Kind: kindValue, Default: "palai-artifacts",
		Effect:     "The bucket artifacts are written to.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "artifactStoreFromEnv",
	},
	{
		Name: "PALAI_RETENTION_STORE_FALSE_TTL", Group: "storage", Kind: kindValue, Default: "disabled — nothing is reaped, and GET /v1/capabilities advertises a TTL of 0",
		Effect:     "How long a `store:false` response survives before the retention reaper deletes it. Unset starts no reaper at all, so no arbitrary production default is imposed.",
		Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "startRetention",
		DesiredValue: desiredDuration,
	},

	// --- how this process is reached ------------------------------------------------------------------
	{
		Name: "PALAI_LISTEN_ADDR", Group: "network", Kind: kindValue, Default: ":8080",
		Effect:     "The public API listener.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "main",
	},
	{
		Name: "PALAI_RUNNER_LISTEN_ADDR", Group: "network", Kind: kindValue, Default: "none — the runner gateway is NOT bound, so no machine can enrol and no run reaches an engine",
		Effect:     "The mutually-authenticated listener enrolled runners dial. Setting it makes the CA and server-certificate variables below required.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "main",
	},
	{
		Name: "PALAI_PUBLIC_BASE_URL", Group: "network", Kind: kindValue, Default: "none — an A2A push target is not addressable from outside",
		Effect:     "This deployment's externally reachable base URL, used where a peer must be handed an address to call back on.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "main",
	},
	{
		Name: "PALAI_TOOL_CALLBACK_BASE_URL", Group: "network", Kind: kindValue, Default: "none — a remote tool can then only answer synchronously",
		Effect:     "The base URL a remote tool's asynchronous 202 result is posted back to.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startDispatch",
	},

	// --- the edge's bounds ----------------------------------------------------------------------------
	{
		Name: "PALAI_REQUEST_RATE_PER_SEC", Group: "edge", Kind: kindValue, Default: "unbounded",
		Effect: "Sustained request rate the API edge admits per key.", Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "edgeLimitsFromEnv",
		DesiredValue: desiredRate,
	},
	{
		Name: "PALAI_REQUEST_BURST", Group: "edge", Kind: kindValue, Default: "unbounded",
		Effect: "Burst allowance above the sustained rate.", Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "edgeLimitsFromEnv",
		DesiredValue: desiredInt,
	},
	{
		Name: "PALAI_MAX_CONCURRENT_RUNS", Group: "edge", Kind: kindValue, Default: "unbounded",
		Effect: "How many runs one tenant may have in flight before admission refuses.", Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "edgeLimitsFromEnv",
		DesiredValue: desiredInt,
	},
	{
		Name: "PALAI_MAX_QUEUED_RUNS", Group: "edge", Kind: kindValue, Default: "unbounded",
		Effect: "How many runs one tenant may have queued before admission refuses.", Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "edgeLimitsFromEnv",
		DesiredValue: desiredInt,
	},

	// --- the handles this process holds. PATHS, NEVER VALUES ------------------------------------------
	{
		Name: "PALAI_SECRET_MASTER_KEY_FILE", Group: "identity", Kind: kindPath, Default: "unset — the DB-backed secret store is DISABLED, the secret-ref routes stay unmounted, and every resolver is env-file-only",
		Effect:     "The file holding the 32-byte hex master key the envelope-encrypted secret store redeems through. A set-but-unreadable file is fatal at boot rather than a silent downgrade. This surface reports the PATH; the key never leaves the process.",
		Mutability: mutabilityBringUp,
		ChangeWith: "a stored secret is rotated live through POST /v1/secret-refs and needs no restart; changing the MASTER KEY's location needs " + changeCP,
		ReaderFile: cpMain, ReaderFunc: "main",
	},
	{
		Name: "PALAI_BOOTSTRAP_API_KEY_FILE", Group: "identity", Kind: kindPath, Default: "unset — no bootstrap identity is seeded",
		Effect:     "The file holding the first admin API key, seeded once at boot. The value is a path; the key itself is never read back by any route.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "main",
	},
	{
		Name: "PALAI_RUNNER_CA_CERT", Group: "fleet", Kind: kindPath, Default: "required once the runner listener is bound; the process refuses to start without it",
		Effect: "The CA certificate the runner gateway presents and verifies enrolled machines against.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startRunnerGateway",
	},
	{
		Name: "PALAI_RUNNER_CA_KEY", Group: "fleet", Kind: kindPath, Default: "required once the runner listener is bound; the process refuses to start without it",
		Effect: "The CA private key runner certificates are issued from. A PATH — the key material is never returned by any route.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startRunnerGateway",
	},
	{
		Name: "PALAI_RUNNER_SERVER_CERT", Group: "fleet", Kind: kindPath, Default: "required once the runner listener is bound",
		Effect: "The gateway listener's own certificate. Its SANs decide which addresses a runner may dial.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startRunnerGateway",
	},
	{
		Name: "PALAI_RUNNER_SERVER_KEY", Group: "fleet", Kind: kindPath, Default: "required once the runner listener is bound",
		Effect: "The gateway listener's private key. A PATH, never the key.", Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startRunnerGateway",
	},
	{
		Name: "PALAI_ENROLLMENT_TOKEN_FILE", Group: "fleet", Kind: kindPath, Default: "required once the runner listener is bound",
		Effect:     "The bootstrap enrolment token a machine spends to join, and re-presents only when its identity expired before renewal could roll it forward.",
		Mutability: mutabilityBringUp, ChangeWith: changeCP,
		ReaderFile: cpMain, ReaderFunc: "startRunnerGateway",
	},
	{
		Name: "PALAI_RUNNER_CERT_TTL", Group: "fleet", Kind: kindValue, Default: "5m",
		Effect: "The lifetime of an issued runner certificate; a short value makes a runner renew mid-session.", Mutability: mutabilityBringUp, ChangeWith: changeDesired,
		ReaderFile: cpMain, ReaderFunc: "startRunnerGateway",
		DesiredValue: desiredDuration,
	},

	// --- what this deployment is connected to ---------------------------------------------------------
	// PALAI_SLACK_SOCKET_TEAM_ID WAS CATALOGUED HERE AND IS GONE (cutover, 2026-08-05). It named the one
	// Slack workspace the control plane held an outbound Socket Mode connection to, read by
	// main.startSlackSocket. That function no longer exists: apps/slack-bot holds the connection, one
	// process per registered bot row, so there is no single-workspace selector to report and no reader to
	// cite. TestEveryCatalogueCitationResolvesToARealReader is what caught the stale citation.

	// --- what this process says it is -----------------------------------------------------------------
	// --- THE RUNNER PLANE. Read by cmd/runner, NOT by this process ------------------------------------
	//
	// TWO ENTRIES, AND THE REASON THEY ARE HERE IS THE REASON THEY WERE NOT. The compose walk
	// (TestEveryComposeSettingIsCataloguedOrDeclaredUnreported) can only see what compose SETS, and
	// compose.yaml's runner block sets neither of these — measured 2026-08-01, `docker inspect` on the live
	// runner found 0 of 3 for WORKSPACE_ROOT/POSTURE/POOL. So a one-directional sweep reported a complete
	// catalogue while two variables the runner binary reads were in it nowhere. That is the shape CLAUDE.md
	// names: a walk finds what exists and only a LIST finds what does not.
	//
	// THEY CARRY value="" ON EVERY DEPLOYMENT AND THAT IS THE POINT, not a defect in the rows. This process
	// holds no copy of a runner-scoped variable — which is exactly why PALAI_RUNNER_CONCURRENCY stays in
	// unreportedSettings rather than becoming a third entry here: it is a NUMBER an operator would read as
	// this deployment's concurrency, and reporting an unset copy of it would be the confident wrong answer
	// deployment.go's header refuses. These two are different: their VALUE is not what a reader wants, their
	// EXISTENCE is. `Effect` says what the runner does with each and `Default` says what a runner that was
	// given nothing falls back to, which is the question an operator debugging an unplaced run actually has.
	//
	// NEITHER IS WRITABLE, AND THE REASON IS NOW THE ONLY ONE. This paragraph used to give two — "they are
	// on the runner plane, so desiredWritable() never admits them" and "the runner plane has no writer at
	// all yet" — and BOTH have stopped being true, while the conclusion they supported is still correct.
	// The runner plane has two writers today (a pool document and a machine document), and the plane is not
	// what excludes these: readerOf() admits any runner-plane setting into either runner document.
	//
	// What excludes them is that neither declares a DesiredValue grammar, which is the single gate
	// desiredWritable() applies. That is a DECISION rather than an omission, and it is the same one for
	// both: each is a value the MACHINE declares about itself at enrolment (its posture, its pool), read by
	// loadConfig before the machine has an identity to be sent a document for. The control plane learns
	// them; it cannot hand them back. Writing either into a desired document would produce a value whose
	// only reader has already run.
	//
	// Catalogued anyway, because a catalogue that omits what nothing sets is a catalogue that will keep
	// omitting it.
	{
		Name: "PALAI_RUNNER_POSTURE", Group: "fleet", Kind: kindValue, Plane: planeRunnerPool,
		Default: "unset — the machine declares no posture and the registry places it on its pool's own (fleet/store.go)",
		Effect: "What KIND of machine this runner is, declared by the machine at enrolment: `sandboxed-linux` is a " +
			"container runner, `unsandboxed-host` is a rented Mac. The registry REFUSES an enrolment whose declared posture " +
			"disagrees with its pool's, so this is the one runner-reported field that can turn a bring-up into a refusal.",
		Mutability: mutabilityBringUp,
		ChangeWith: "set it on the RUNNER and restart that machine's runner; the pool's own posture is changed through " +
			"POST/PATCH /v1/runner-pools, and the two must agree",
		ReaderFile: "cmd/runner/main.go", ReaderFunc: "loadConfig",
	},
	{
		Name: "PALAI_RUNNER_POOL", Group: "fleet", Kind: kindValue, Plane: planeRunnerPool,
		Default: "unset — the machine names no pool and the registry places it on the deployment default",
		Effect: "WHICH POOL this machine enrols into, and therefore which pool's posture and strict-enrolment rules " +
			"apply to it. It is the only per-machine configuration axis this product has: a runner's os and arch are " +
			"reported (and, measured 2026-08-01, reported EMPTY — cmd/runner sends neither), so a pool is how machines are " +
			"told apart.",
		Mutability: mutabilityBringUp,
		ChangeWith: "set it on the RUNNER and restart that machine's runner; an already-enrolled machine keeps the pool " +
			"it joined with",
		ReaderFile: "cmd/runner/main.go", ReaderFunc: "loadConfig",
	},

	{
		Name: "PALAI_VERSION", Group: "version", Kind: kindValue, Default: "the version stamp baked into this image at build time",
		Effect:     "Overrides the baked release stamp, which decides the §48.2 support window and what an upgrade records as applied_by.",
		Mutability: mutabilityBringUp, ChangeWith: changeAll,
		ReaderFile: "packages/version/version.go", ReaderFunc: "Resolve",
	},
}

// unreportedSettings names every variable deploy/compose/compose.yaml SETS that this surface deliberately
// does not report, with the reason. Adding a name here is the auditable act; the DEFAULT for a new compose
// variable is that TestEveryComposeSettingIsCataloguedOrDeclaredUnreported fails until somebody decides.
//
// The two reasons are the only two there are: the value is a credential, or the value is not this
// process's to report.
var unreportedSettings = map[string]string{
	"PALAI_CONTROLLER_URL": "runner-scoped: the address the RUNNER dials this control plane on. This process holds no copy.",
	"PALAI_CONTROLLER_DNS": "runner-scoped: the SAN the RUNNER pins its gateway connection to. This process holds no copy.",
	// The AGENT's trust anchor, and it is not the same thing as PALAI_RUNNER_CA_CERT one group up — that
	// one is the CA this process SIGNS machine certificates with and is catalogued because this process
	// reads it. This is the file an agent verifies the gateway against, on the agent's own disk, and the
	// plane holds no copy of it. Naming them alike in compose.yaml is what this rename removed.
	"PALAI_CONTROLLER_CA":   "runner-scoped: the trust anchor the AGENT verifies this gateway with, on the agent's disk. This process holds no copy.",
	"PALAI_COMPOSE_PROJECT": "runner-scoped: the compose project label the RUNNER tags engine sandboxes with. This process holds no copy.",
	// IT IS NOT A CATALOGUE ENTRY, AND THE DIFFERENCE FROM ITS SIBLING IS THE WHOLE REASON. The runner-plane
	// row above (PALAI_RUNNER_CONCURRENCY) earned its place when the plane gained a way to SEND the value:
	// handleEnroll answers a machine its pool's document and cmd/runner takes the number from it. That path
	// cannot carry this one. The document arrives in the enrolment RESPONSE, and this value has to be in the
	// enrolment REQUEST — a ceiling delivered by the answer cannot bound the question that asked for it. So
	// cmd/runner reads it from the machine's own environment, and a `runner_pool` row here would tell an
	// operator to type it into a panel where it would do nothing.
	"PALAI_RUNNER_CAPACITY": "runner-scoped: how many sessions the MACHINE declares it can hold at once. " +
		"This process holds no copy, and the pool document cannot carry it either — it is read from the machine's " +
		"own environment at ENROLMENT, before any document reaches it. The number a machine actually declared is " +
		"on its runners row (GET /v1/runners, `capacity`), which is where an operator reads it back.",
}

// nonDesiredReason names every catalogued setting the panel may NOT write, with the reason. It is the
// mirror of unreportedSettings and it exists for the same reason: adding a name is the auditable act, and
// TestEverySettingIsEitherWritableOrHasAStatedRefusal fails until somebody decides.
//
// THE RULE THE TWENTY-FOUR REFUSALS ARE INSTANCES OF: the panel writes OPERATIONAL BOUNDS and ROUTING
// DEFAULTS. It does not write IDENTITY, IMAGES, the ADDRESS this process is reached on, or a DESTINATION a
// credential is sent to. Every entry below is one of those four, plus the paths — which are refused
// STRUCTURALLY rather than by this map (desiredWritable drops Kind == kindPath before it ever reads a name)
// and are listed anyway, because a reader asking "why can I not set the master key path here" deserves the
// sentence rather than an inference from a missing row.
// secretFilePrefixes are read as PREFIXES rather than as variables: the full name carries the secret's
// own reference (`PALAI_MCP_SECRET_FILE_<REF>`), so there is no fixed name a catalogue could hold. They
// are also credential-bearing, which TestNoCredentialBearingVariableIsCatalogued keeps out on purpose.
var secretFilePrefixes = []string{
	"PALAI_A2A_REMOTE_SECRET_FILE_",
	"PALAI_INBOUND_SECRET_FILE_",
	"PALAI_MCP_SECRET_FILE_",
	"PALAI_REMOTE_TOOL_SECRET_FILE_",
	"PALAI_WEBHOOK_SECRET_FILE_",
}

// uncataloguedSettings is a LEDGER OF DEBT, not an exemption list, and the difference is what it is for.
//
// ‼️ EVERY GUARD IN THIS FILE LOOKED ONE WAY UNTIL 2026-08-06: compose -> catalogue, and catalogue ->
// compose for the writable entries. A variable the BINARY READS that no compose file names was invisible
// to both, and `PALAI_WORKSPACE_IDLE_TTL` — which decides when EVERY session's machine is handed back —
// sat in exactly that blind spot with a default, so nothing was red while no operator surface named it.
//
// Measured the same day: the composition root reads 64 `PALAI_` names and 42 of them were neither
// catalogued nor declared unreported. Inventing 34 catalogue entries in one sitting would have produced
// 34 sentences about defaults and effects that nobody verified — the catalogue's value is that its prose
// is TRUE, and filling it fast is how that stops being so. So they are written down instead, with the
// reason they are a group, and TestEverySettingTheCompositionRootReadsIsAccountedFor makes the list a
// CEILING: a name added to main.go and not to the catalogue fails, which is the hole this closes. The
// list shrinks as entries are catalogued properly; it must never grow.
var uncataloguedSettings = func() map[string]string {
	groups := []struct {
		reason string
		names  []string
	}{
		{"SSE tuning for the events stream. Operator-visible behaviour (how fast a client sees output, when a slow reader is dropped), so these belong in the catalogue.",
			[]string{"PALAI_SSE_BATCH_LIMIT", "PALAI_SSE_HEARTBEAT", "PALAI_SSE_POLL_INTERVAL", "PALAI_SSE_WRITE_TIMEOUT"}},
		{"Outbound webhook delivery pacing and retry envelope.",
			[]string{"PALAI_WEBHOOK_BACKOFF_BASE", "PALAI_WEBHOOK_BACKOFF_MAX", "PALAI_WEBHOOK_TICK"}},
		{"Trigger reconcile loop: how often it runs, how much it takes, and how long a mapped trigger is held.",
			[]string{"PALAI_TRIGGER_MAPPED_GRACE", "PALAI_TRIGGER_RECONCILE_BATCH", "PALAI_TRIGGER_RECONCILE_TICK"}},
		{"MCP client sweep and dial timeout. The boundary of this family — PALAI_MCP_ALLOW_PRIVATE — was catalogued on 2026-08-06 and left this list; what remains is pacing.",
			[]string{"PALAI_MCP_SWEEP_GRACE", "PALAI_MCP_SWEEP_INTERVAL", "PALAI_MCP_TIMEOUT"}},
		{"Inbound intake bounds: backlog, in-flight ceiling, raw retention and clock tolerance.",
			[]string{"PALAI_INBOUND_BACKLOG_MAX", "PALAI_INBOUND_MAX_INFLIGHT", "PALAI_INBOUND_RAW_TTL", "PALAI_INBOUND_TOLERANCE"}},
		{"The A2A push allow-list of destination hosts. Its sibling boundary PALAI_A2A_PUSH_ALLOW_PRIVATE was catalogued on 2026-08-06; this one is next, and it is a list rather than a switch.",
			[]string{"PALAI_A2A_PUSH_ALLOWED_HOSTS"}},
		{"Artifact garbage collection cadence and the grace an artifact gets before it is eligible.",
			[]string{"PALAI_ARTIFACT_GC_GRACE", "PALAI_ARTIFACT_GC_INTERVAL"}},
		{"The capability worker's own listener and the TTL of the job-scoped identity it hands out.",
			[]string{"PALAI_CAPABILITY_WORKER_IDENTITY_TTL", "PALAI_CAPABILITY_WORKER_LISTEN_ADDR"}},
		{"Durable queue and schedule pacing.",
			[]string{"PALAI_QUEUE_DELIVERY_BACKOFF", "PALAI_QUEUE_TICK", "PALAI_SCHEDULE_BATCH", "PALAI_SCHEDULE_TICK"}},
		{"Object-store region, metrics disk path, drain timeout, abandoned-lease grace, the session-account helper and the Slack API base. Unrelated to each other; grouped only by having no family.",
			[]string{"PALAI_ABANDONED_LEASE_GRACE", "PALAI_DRAIN_TIMEOUT", "PALAI_METRICS_DISK_PATH", "PALAI_S3_REGION", "PALAI_SESSION_ACCOUNT_HELPER", "PALAI_SLACK_API_BASE_URL"}},
	}
	out := map[string]string{}
	for _, g := range groups {
		for _, name := range g.names {
			out[name] = g.reason
		}
	}
	return out
}()

// credentialBearingSettings never enter the catalogue: their VALUE is the secret, so a row naming them —
// even with an empty value — tells a reader the shape of the deployment's credentials.
// TestNoCredentialBearingVariableIsCatalogued is the rule; this names the composition root's own three so
// the accounting guard can tell "deliberately absent" from "forgotten".
var credentialBearingSettings = []string{
	"PALAI_DATABASE_URL",
	"PALAI_S3_ACCESS_KEY",
	"PALAI_S3_SECRET_KEY",
}

var nonDesiredReason = map[string]string{
	// --- filesystem handles. desiredWritable REFUSES THESE BY KIND; the sentences are for the reader. ----
	"PALAI_WORKSPACE_ROOT": "a path. Naming the host directory every coding workspace is allocated under, from a web form, " +
		"is a filesystem write primitive wearing a settings control.",
	"PALAI_SECRET_MASTER_KEY_FILE": "a path, and the sharpest one: it names the file the ENTIRE secret store redeems through. " +
		"Moving it from a form points the store at a file the operator chose and the process reads at boot with no further question.",
	"PALAI_BOOTSTRAP_API_KEY_FILE": "a path, and the file it names holds the first admin key. It is seeded once at boot; " +
		"re-pointing it is minting an identity, not configuring a machine.",
	"PALAI_RUNNER_CA_CERT":        "a path to the fleet's trust root.",
	"PALAI_RUNNER_CA_KEY":         "a path to the private key every runner certificate is issued from.",
	"PALAI_RUNNER_SERVER_CERT":    "a path to the gateway listener's certificate; its SANs decide which addresses a runner may dial.",
	"PALAI_RUNNER_SERVER_KEY":     "a path to the gateway listener's private key.",
	"PALAI_ENROLLMENT_TOKEN_FILE": "a path to the credential a machine spends to join the fleet.",
	"PALAI_FAKE_SCRIPT_FILE": "a path, and the file it names decides what a run's model APPEARS to say. " +
		"A form that wrote it would let a reader of the panel author a fabricated exchange every credential-less " +
		"run then replays as if it were an answer.",

	// --- read once, at bring-up, by a sweep that then holds the value ---------------------------------
	"PALAI_WORKSPACE_IDLE_TTL": "read ONCE, by startIdleRelease, and held by the sweep for the process's life. " +
		"A form that wrote it would report a value no running sweep is using until the control plane restarts — " +
		"a settings screen answering with a number that is not in force, which is worse than one that refuses. " +
		"It is also plane-wide rather than per-machine: the desired document is a MACHINE's, and this decides " +
		"when EVERY session's machine is handed back. Change it where it is read, then restart.",

	// --- images: what CODE runs on this machine ------------------------------------------------------
	"PALAI_ENGINE_IMAGE": "an IMAGE REFERENCE. The control plane pins it into every lease and the runner starts it — so a form " +
		"that wrote it would be arbitrary container execution on the fleet, reached through a settings screen. It is pinned by the " +
		"release the operator installed and it changes when they upgrade.",
	"PALAI_SANDBOX_IMAGE": "an IMAGE REFERENCE, for the same reason: it is the container every workspace shell call runs inside, " +
		"with the workspace mounted. Choosing it is a supply-chain decision made at install time, not a setting.",

	// --- what this deployment may DIAL ---------------------------------------------------------------
	"PALAI_MCP_ALLOW_PRIVATE": "it widens what this deployment may DIAL. Set from a form, a reader of the " +
		"panel could point a tool at loopback and at everything on the deployment's own network — the " +
		"metadata service, an unauthenticated admin port, a database — and the request would leave this " +
		"process with this process's network position. It is a bring-up decision about where the deployment " +
		"SITS, made by whoever put it there.",
	"PALAI_A2A_PUSH_ALLOW_PRIVATE": "the same boundary on the push path, and it is worse in one respect: a " +
		"push destination is supplied per TASK, so a form that opened private addresses would let a caller " +
		"choose the internal address afterwards rather than at configuration time.",

	// --- a security boundary -------------------------------------------------------------------------
	// THE REFUSAL STANDS, AND WHAT IS NEW IS THE PRICE OF LIFTING IT, measured 2026-08-03 and written here
	// so the next person to ask does not re-derive it. The question is live: the owner's requirement is that
	// a Mac added as a runner takes its whole configuration from the panel, and this is the one setting that
	// makes Xcode reachable.
	//
	// THREE THINGS WERE ESTABLISHED AND THEY ALL FAVOUR LIFTING IT.
	//   1. It cannot go on either RUNNER plane, ever, and that is structural rather than policy:
	//      `grep -rn PALAI_SHELL_NATIVE --include='*.go' cmd/runner/ packages/runner/` finds NOTHING. A
	//      runner does not run the shell tool. So "configure this Mac's posture" is a control-plane
	//      question wearing machine clothes.
	//   2. On a Mac it IS that machine's own control plane. `palai up --native`
	//      (cmd/cli/internal/stack/native.go:15) runs the control-plane binary natively on the Mac and
	//      leaves the runner in Docker, precisely so it can reach xcodebuild and xcrun simctl.
	//   3. The delivery chain already works end to end and was traced: up.go:121 applyDesiredEnv exports the
	//      document into os.Environ, up.go:148 then starts the native plane, native.go:213 merges os.Environ
	//      wholesale, and native.go:252 forwards this variable by name. Nothing is missing between a panel
	//      write and a native Mac reading it.
	// The refusal's own argument — that a form hands back "the reflex that switches a feature on" — is
	// answerable: an `exact` grammar accepting ONLY the literal `unsandboxed-host` keeps the friction the
	// environment variable had, and deployment_desired.written_by already records WHO armed it from the
	// verified credential, which the environment variable never did.
	//
	// WHAT STOPPED IT, and it is one decision rather than a difficulty. Making it writable turns five guards
	// red, four of them trivially (a compose passthrough, a refusal test that asserts today's behaviour, an
	// accept fixture, a reader round-trip). The fifth is not trivial and is not this file's to settle:
	// TestEveryWritableSettingIsPassedByTheShippedComposeFile requires the variable be passed to the
	// CONTAINERISED control plane too, which widens the posture to every compose deployment — where the
	// container IS the boundary being removed. That is a different risk from a Mac deliberately brought up
	// native, and whoever lifts this must decide it deliberately rather than inherit it from a guard.
	"PALAI_SHELL_NATIVE": "it DELETES a security boundary — the exact words `unsandboxed-host` run every shell command on this " +
		"machine as this uid with no container, no network denial and no resource bound. deployment.go's own entry says the value is a " +
		"sentence rather than a `1` so that removing the boundary is not reachable by the reflex that switches a feature on; putting it " +
		"behind a form would hand that reflex back. On a Mac brought up with `palai up --native` this is that MACHINE's own control " +
		"plane and the panel serving it is that machine's own, so lifting this is a live question — see the comment above this entry " +
		"for the three facts that favour it and the one decision that has not been made.",

	// --- destinations a credential is sent to --------------------------------------------------------
	"PALAI_S3_ENDPOINT": "a destination this process sends its OBJECT-STORE CREDENTIAL to. PALAI_S3_ACCESS_KEY/SECRET_KEY are " +
		"variables this surface never reports and never writes, but they are sent to whatever this names — so a written endpoint is " +
		"credential exfiltration with the artifacts as a bonus.",
	"PALAI_S3_BUCKET": "the identity of the store every artifact already written lives in. Changing it does not move them; it makes " +
		"them unreachable while the surface reports success.",

	// --- the address this process is reached on ------------------------------------------------------
	"PALAI_LISTEN_ADDR": "the address this API is reached on — including by the panel doing the writing. A machine that changes " +
		"where it listens, from the surface that reaches it, cannot be reached to change it back.",
	"PALAI_RUNNER_LISTEN_ADDR": "the same, for the fleet: unbinding it strands every enrolled machine, and binding it makes four " +
		"path-valued variables suddenly required — a coupling a single-field form cannot express.",

	// --- an address handed to a peer to call back on --------------------------------------------------
	"PALAI_PUBLIC_BASE_URL": "the address peers are handed to call this deployment back on. A written value redirects a callback " +
		"leg rather than configuring one.",
	"PALAI_TOOL_CALLBACK_BASE_URL": "the address a remote tool POSTs its asynchronous result to. Same shape: writing it moves " +
		"where results land.",

	// --- half of a credential triple -----------------------------------------------------------------

	// --- it has a strictly better live write-path ------------------------------------------------------
	"PALAI_OPENAI_COMPATIBLE_BASE_URL": "superseded by a LIVE write-path that is strictly better: migration 000051 gave " +
		"model_connections its own base_url, vetted through packages/egress and resolved per request, so an endpoint is a property of a " +
		"connection rather than of the machine. Writing the deployment-wide one here would re-create the limitation 000051 removed — one " +
		"custom endpoint per stack — and cost a bring-up to change.",

	// --- read on ANOTHER plane, so this surface has no write path to offer at all --------------------
	"PALAI_RUNNER_POSTURE": "read by a RUNNER, not by this process. The desired document is keyed by plane and " +
		"the runner plane has no reader: cmd/runner takes its environment at exec and nothing hands it a document. A pool's " +
		"posture — which the registry DOES enforce against a machine's declaration — is set through POST/PATCH /v1/runner-pools.",
	"PALAI_RUNNER_POOL": "read by a RUNNER, not by this process. See PALAI_RUNNER_POSTURE; a machine names its pool at " +
		"enrolment and an already-enrolled machine keeps the pool it joined with.",

	// --- what this process says it is ------------------------------------------------------------------
	"PALAI_VERSION": "the release stamp, which decides the §48.2 support window and what an upgrade records as applied_by. It is " +
		"baked into the image at build time; a form that overrode it would let a machine misreport which build is running, and the " +
		"support window is computed from the claim.",
}

// desiredTokenPattern is the accepted shape of a `token`-grammar value: the characters a provider selector
// or a model id is made of, and NOTHING that could act on the way to the process.
//
// IT IS AN ALLOW-LIST OF CHARACTERS, and that is deliberate rather than a denylist of dangerous ones. The
// value's journey is: JSON body -> a JSONB document -> `palai up` -> os.Setenv -> a `docker compose`
// process environment -> ${VAR} interpolation into a YAML scalar in compose.yaml. A newline in that last
// step is a YAML structure edit; a `$` is a second round of interpolation. Neither is expressible below,
// and neither is expressible in an integer or a duration either — which is why nine of the eleven writable
// settings have no free text in them at all.
var desiredTokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$`)

// desiredWritable is THE allow-list, computed from the catalogue, and it is the structural half of
// requirement 3.
//
// TWO CONDITIONS, AND THE SECOND ONE IS NOT REDUNDANT. A setting is writable when it declares a value
// grammar AND its kind is not a path. The kind test cannot be satisfied by editing an entry: flipping
// PALAI_SECRET_MASTER_KEY_FILE to `DesiredValue: desiredToken` still yields a read-only setting, because the
// filter drops it by KIND and never consults the grammar. That is what "structural rather than a validation
// string" means here — the refusal is in the code that builds the set, not in a message printed after
// something reached the set.
//
// The list is rebuilt on every call rather than cached in a package var, because a cached copy is a second
// source of truth for exactly the question this function exists to answer.
func desiredWritable() map[string]catalogueEntry {
	out := map[string]catalogueEntry{}
	for _, entry := range deploymentCatalogue {
		if entry.Kind == kindPath || entry.DesiredValue == "" {
			continue
		}
		out[entry.Name] = entry
	}
	return out
}

// DesiredWritableSettings is the writable set's NAMES, for the guards that live outside this package —
// deploy/compose's "does the shipped compose file actually pass this variable?" walk, and the composition
// root's round-trip through the real readers. It is exported for those two and has no production caller:
// production reads desiredWritable() above, which carries the entries.
// DesiredWritableSettingsFor is DesiredWritableSettings narrowed to ONE plane.
//
// It exists because "writable" and "readable by this process" stopped being the same set the day the
// runner plane got a reader. A guard in the control-plane binary that round-trips every writable setting
// through its own reader can only do that for settings this binary reads; a runner-plane setting's reader
// is cmd/runner, and asserting it here would either fail honestly or pass by measuring the wrong process.
func DesiredWritableSettingsFor(plane string) []string {
	names := make([]string, 0, len(deploymentCatalogue))
	writable := desiredWritable()
	for _, entry := range deploymentCatalogue {
		if _, ok := writable[entry.Name]; ok && planeOf(entry) == plane {
			names = append(names, entry.Name)
		}
	}
	return names
}

// ControlPlaneName is planeControlPlane for guards outside this package that must name the plane whose
// readers live in the control-plane binary.
const ControlPlaneName = planeControlPlane

func DesiredWritableSettings() []string {
	names := make([]string, 0, len(deploymentCatalogue))
	for _, entry := range deploymentCatalogue {
		if _, ok := desiredWritable()[entry.Name]; ok {
			names = append(names, entry.Name)
		}
	}
	return names
}

// DispatchWorkers is the ONE reading of PALAI_DISPATCH_WORKERS in this binary.
//
// main.dispatchWorkerCount delegates to it, and startDispatch's early return is `<= 0`, so the number
// reported by GET /v1/deployment and the number that decides whether anything executes cannot drift apart.
// A second copy of the parsing rules here would be the defect that made this surface necessary, one layer
// up: a screen confidently reporting a default the code does not use.
func DispatchWorkers() int {
	raw := os.Getenv("PALAI_DISPATCH_WORKERS")
	if raw == "" {
		return 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 1
	}
	return n
}

// deployment serves the effective configuration. It is gated on the `provision` capability — the same
// authority the tenancy surface requires — because this is an OPERATOR read: it names the object store,
// the sandbox image, the GitHub App and every path the process holds open. A narrow project-scoped key
// handed to a run has no business enumerating the machine it is running on.
func deployment(desired DesiredConfigAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := middleware.ScopeFrom(r.Context())
		if !ok {
			middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
			return
		}
		if !scope.HasScope(provisionScope) {
			middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope", "this API key lacks the provision capability")
			return
		}

		// The desired document, when this deployment has somewhere to keep one. A read failure is FATAL to
		// the response rather than degraded to "no document": the difference between "nobody has written
		// one" and "we could not read the one that exists" is the difference between a machine running on
		// its compose defaults and a machine whose operator's decision is invisible, and a screen shown the
		// first when the second is true would report no pending bring-up while one is pending.
		var doc *DesiredDocument
		if desired != nil {
			var err error
			// THE CONTROL PLANE'S OWN document, and this route reads no other. GET /v1/deployment reports
			// THIS PROCESS's environment — a machine's document belongs to whatever surface reports that
			// machine, and serving it here would put a pool's configuration under a heading that says
			// "this deployment", which is the misreading the plane exists to prevent.
			if doc, err = desired.GetDesiredConfig(r.Context(), scope, planeControlPlane, ""); err != nil {
				middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error",
					"the effective configuration was read but the desired configuration could not be, so this response cannot say whether a bring-up is pending")
				return
			}
		}
		writable := desiredWritable()

		body := deploymentBody{Object: "deployment", Settings: make([]deploymentSetting, 0, len(deploymentCatalogue))}
		for _, entry := range deploymentCatalogue {
			_, isWritable := writable[entry.Name]
			row := deploymentSetting{
				Name: entry.Name, Group: entry.Group,
				Default: entry.Default, Kind: entry.Kind, Effect: entry.Effect,
				Mutability: entry.Mutability, ChangeWith: entry.ChangeWith,
				ReaderFile: entry.ReaderFile, ReaderFunc: entry.ReaderFunc,
				Writable: isWritable, Plane: planeOf(entry),
				Observable: planeOf(entry) == planeControlPlane,
			}
			// THE ENV IS READ ONLY FOR THIS PROCESS'S OWN PLANE, and the guard is here rather than on the
			// value it produces. A runner-plane variable that happens to be exported in the CONTROL PLANE's
			// shell would otherwise be read, reported, and taken for the machine's — this process reporting
			// its own copy and labelling it somebody else's, which is the exact thing PALAI_RUNNER_CONCURRENCY
			// is left out of the catalogue to avoid. Skipping the lookup makes that impossible rather than
			// unlikely.
			if row.Observable {
				raw, set := os.LookupEnv(entry.Name)
				row.Value, row.Set = reportedValue(raw), set && raw != ""
			}
			if isWritable {
				row.ValueGrammar = entry.DesiredValue
			} else {
				row.NotWritableBecause = nonDesiredReason[entry.Name]
			}
			if doc != nil && row.Observable {
				// The desired value is compared against the RAW environment, not against `row.Value` — which
				// is reportedValue()'s redacted rendering. Comparing the redaction would report drift on any
				// value carrying userinfo forever, because the redacted form can never equal what was written.
				//
				// AND ONLY ON THE OBSERVABLE PLANE. Drift is "this process is not running what was saved";
				// for a setting this process does not read, there is nothing to compare and a computed
				// answer would be a claim about a machine.
				raw := os.Getenv(entry.Name)
				row.Desired, row.DesiredSet = doc.Settings[entry.Name]
				row.Drift = row.DesiredSet && row.Desired != raw
			}
			body.Settings = append(body.Settings, row)
		}
		body.Warnings = deploymentWarnings()
		if doc != nil {
			drifted := desiredDrift(doc, os.Getenv)
			body.Desired = &desiredView{
				Plane: planeControlPlane, Scope: "",
				Revision: doc.Revision, WrittenAt: doc.WrittenAt, WrittenBy: doc.WrittenBy,
				Pending: len(drifted) > 0, Drifted: drifted,
			}
			if len(drifted) > 0 {
				body.Warnings = append(body.Warnings, deploymentWarning{
					Code: warnDesiredPending, Severity: severityAdvisory,
					Headline: "This machine is not running the configuration that was saved for it.",
					Detail: "Desired revision " + strconv.FormatInt(doc.Revision, 10) + " differs from what this process holds for " +
						strconv.Itoa(len(drifted)) + " setting(s): " + strings.Join(drifted, ", ") + ". Every one of them is read from the " +
						"process environment, which is fixed at exec, so the saved value takes effect when the process is replaced with it — not before.",
					Remedy:   "Run `palai up` on the machine. The bring-up reads this document and exports it into the environment it starts the control plane with.",
					Settings: drifted,
				})
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(body)
	}
}

// deploymentWarnings reports the configured values that change what the product DOES in a way a screen
// showing the product would otherwise misrepresent. It is derived from the SAME readers production uses,
// never from the catalogue's prose.
func deploymentWarnings() []deploymentWarning {
	out := []deploymentWarning{}
	if DispatchWorkers() <= 0 {
		out = append(out, deploymentWarning{
			Code: warnDispatchOff, Severity: severityBlocking,
			Headline: "This deployment has no dispatcher — submitted runs are accepted and never executed.",
			Detail: "PALAI_DISPATCH_WORKERS is " + strconv.Itoa(DispatchWorkers()) + ", so startDispatch returns before it builds a worker. " +
				"POST /v1/responses still returns 201 and the run stays at run.queued.v1 forever. The reconciler, the dead-letter sweep, " +
				"approval expiry and capacity-park expiry are off with it, so nothing will time the run out either.",
			Remedy: "Recreate the control-plane with PALAI_DISPATCH_WORKERS=1 or higher. `make local-up` and deploy/compose/compose.yaml " +
				"default it to 0; the production overlay defaults it to 1.",
			Settings: []string{"PALAI_DISPATCH_WORKERS"},
		})
	}
	// THE VARIABLE WITH TWO READERS. PALAI_WORKSPACE_ROOT means "allocate workspaces here" to this process
	// (main.go:677, and it is what makes GET /v1/capabilities advertise `workspaces`) and "refuse to
	// bind-mount a leased path outside here" to a RUNNER (cmd/runner/main.go:120). One name, two planes,
	// two jobs — which is why the desired document is keyed by plane and why this warning exists.
	//
	// IT CLAIMS ONLY WHAT THIS PROCESS KNOWS, and that is the whole care in it. It does NOT say the runner's
	// copy is missing: this process holds no copy of a runner-scoped variable and deployment.go's header
	// records what a reader that forgets produces — "a confident wrong answer, which is worse than the
	// silence this surface replaces". What it says is that turning workspaces on HERE created a requirement
	// THERE, and names both so the operator can go and look.
	//
	// MEASURED 2026-08-01: not one shipped file put PALAI_WORKSPACE_ROOT in a runner's environment block
	// (compose.yaml, production.yml, native-control-plane.yml, airgap.yml — all NO; `docker inspect` on the
	// live runner → 0), and an unset root used to DISABLE the runner's check entirely. It now refuses, so
	// the failure is a refused lease rather than an arbitrary host mount — which is the safe direction and
	// still a stack that does not work until somebody is told.
	if os.Getenv("PALAI_WORKSPACE_ROOT") != "" {
		out = append(out, deploymentWarning{
			Code: warnWorkspaceRootPlane, Severity: severityAdvisory,
			Headline: "Workspaces are provisioned here, and every machine that mounts one needs the same variable set on ITS side.",
			Detail: "PALAI_WORKSPACE_ROOT is set on this control plane, so runs get workspaces allocated under " +
				quotedOrUnset(os.Getenv("PALAI_WORKSPACE_ROOT")) + ". The same variable name is read a SECOND time by each runner " +
				"(cmd/runner/main.go), where it does a different job: the runner refuses to bind-mount a leased workspace that does " +
				"not sit under it — the boundary that stops a control plane naming an arbitrary host path. This process holds no copy " +
				"of a runner's environment and does not claim yours is missing; what it can say is that switching workspaces on here " +
				"created a requirement there.",
			Remedy: "Give each runner PALAI_WORKSPACE_ROOT with the SAME host path this control plane allocates under. " +
				"deploy/compose/native-control-plane.yml sets it beside the bind it already had; a hand-rolled runner may not, and " +
				"a runner without it now REFUSES a workspace-bearing lease rather than mounting it.",
			Settings: []string{"PALAI_WORKSPACE_ROOT"},
		})
	}
	// WHAT AN APPROVED PUBLICATION CAN AND CANNOT DO HERE, said on the screen rather than in a boot log
	// nobody tails.
	//
	// THE COST OF THE SILENCE THIS REPLACES is the highest in this file, because a human is inside it: an
	// operator who presses Approve and is told the run woke has been told the write happened. Before this,
	// a deployment with none of the three variables set built no publisher at all and said so NOWHERE —
	// not at boot (the log line fired only for a HALF-configured App), not on any screen — and that is the
	// configuration a single-tenant stack with a connection_ref binding actually has. Measured on the live
	// native stack 2026-08-02: App id and installation id unset, publisher nil, no warning anywhere.
	//
	// NO PUBLISH WARNING IS EMITTED HERE ANY MORE. The two that were — a half-configured GitHub App and an
	// absent one — described a deployment-global App removed 2026-08-05, and the question that outlived it
	// ("which bindings cannot publish?") cannot be answered from this function: it reads the process
	// environment, and the answer is in the repository_bindings rows.
	//
	// It is asked where the rows are, by `palai up`'s missingPublisherNotice, which names the bindings
	// carrying no connection_ref. Reproducing it here from environment variables is what produced the
	// warnings this replaced — both were derived from variables rather than from the thing they claimed
	// to be about.
	if !liveModelProviderConfigured() {
		out = append(out, deploymentWarning{
			Code: warnModelFake, Severity: severityAdvisory,
			Headline: "Every run without a published model route is answered by the deterministic fake adapter.",
			Detail: "The deployment default route is chosen by an exact match on `provider-one`; PALAI_MODEL_PROVIDER is " +
				quotedOrUnset(os.Getenv("PALAI_MODEL_PROVIDER")) + ", so the fallback is the fake adapter. Its output is fabricated and " +
				"renders exactly like a real answer — a misspelled provider name lands here silently.",
			Remedy: "A project that publishes a model route (POST /v1/model-routes) dispatches through that instead, with no restart. " +
				"To change the fallback, recreate the control-plane with PALAI_MODEL_PROVIDER=provider-one and a credential configured.",
			Settings: []string{"PALAI_MODEL_PROVIDER", "PALAI_MODEL"},
		})
	}
	return out
}

// liveModelProviderConfigured mirrors modelBrokerFromEnv's branch EXACTLY, including the fact that it is an
// equality rather than a non-emptiness test: any value other than `provider-one` returns the fake route.
func liveModelProviderConfigured() bool {
	return os.Getenv("PALAI_MODEL_PROVIDER") == "provider-one"
}

func quotedOrUnset(v string) string {
	if v == "" {
		return "unset"
	}
	return strconv.Quote(v)
}

// reportedValue strips a URL's userinfo before the value is published. No catalogued variable is supposed
// to carry a credential — PALAI_S3_ENDPOINT's key is a separate variable this surface never reports — but
// "supposed to" is not a property a response body should rest on, and an operator who pasted
// `https://user:pass@…` into an endpoint should not learn about it from a console screenshot.
func reportedValue(raw string) string {
	if !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("redacted")
	return u.String()
}
