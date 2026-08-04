package uat

import (
	"encoding/json"
	"slices"
)

// ToolExecutionCaseIDs are the UAT ids Faz A.3 opens, and the `EXE-` prefix is a GATE decision rather than a
// naming preference — the FLC-/BGT-/CON-/FLT-/HIL-/CAS-/TLM- reasoning, arrived at for the ninth time. Every
// one of those prefixes is already inside extensionIDPrefixes with its own owner list and its own promote
// gate, so an `FLT-006` would match carriesE24FleetCase in PromoteGateFor and dispatch to FleetPromoteGate —
// a gate that knows nothing about this phase's placement ledger, its no-fallback half, its background
// addressing, its outstanding `uname` legs or its SUPERSEDED-ceiling record, and would pass a bundle that
// dropped all five. That is the promote-gate-family-dispatch defect, reachable from a naming choice.
//
// `EXE-` collides with no prefix in the tree. Counted 2026-08-04:
//
//	ls tests/uat/cases | sed -E 's/^([^-]+)-.*/\1-/' | sort -u
//	# A2A- AGT- API- APV- AUT- BGT- CAS- CON- DEL- DET- DR- ENG- EXT- FLC- FLT- HIL- KNO- LP- MCI-
//	# MOD- OPS- PER- QUA- REC- REG- REP- SAN- SEC- SES- SLK- SUB- TLM- TOL- UI- WRK-      (35, no EXE-)
//
// AND THE OTHER DIRECTION IS CLOSED IN THE SAME BREATH, because E28 paid for leaving it open: `EXE-` goes
// into extensionIDPrefixes (tests/uat/extensions/catalog_test.go), which is the ONLY sweep in the tree that
// walks the cases DIRECTORY. A prefix left outside it is a family whose directories nothing checks — the
// quiet way of piercing a guard that still reports green, and the way FLC-001/002/003 escaped proof
// resolution for three whole tasks while every UAT package reported ok.
var ToolExecutionCaseIDs = []string{"EXE-001", "EXE-002", "EXE-003", "EXE-004"}

// ToolExecutionBundle is the Faz A.3 EXIT release.
//
// THE NAME WAS CHECKED AGAINST THE RELEASE INDEX BEFORE IT WAS CHOSEN, because this tree has already paid
// once for not doing that: committing `code-and-ship-0.1.0` — which sorts between `automation-0.1.0` and
// `coding-0.1.0` — displaced `extensions-0.1.0` as the indexed carrier of thirty Appendix-A ids and reddened
// all eight of the SHIPPED release-1.0.0-rc1 bundle's checksums (measured 2026-07-28). `tool-execution-0.1.0`
// sorts BEFORE `tools-memory-0.1.0` ('-' < 's'), so naming luck does NOT protect it and this release is
// carried by the as-of rule alone: the RC was captured 2026-07-26T11:00:00Z, this bundle 2026-08-04, and
// recomputeReleaseIndexFrom DROPS every carrier newer than the index it would appear in.
//
// That is the designed protection rather than a coincidence, and TestTheAsOfRuleIsWhatKeepsTheShippedRCGreen
// re-derives it from the committed corpus on every run — which is why the name was not changed to something
// that sorts late. A release protected by its spelling is protected until the next spelling.
const ToolExecutionBundle = "tool-execution-0.1.0"

// ToolExecutionMachine is what the machine in this release HONESTLY was, and the field exists so a bundle
// cannot quietly imply the thing a reader of "the tool runs on the attempt's machine" will assume: a SECOND
// computer.
//
// It was not. Every proof in this release runs a REAL `packages/runner` router behind a REAL lease wire
// (websocket, and for the workspace half a real mTLS enrolment against real PostgreSQL through the shipped
// gateway) — and both ends are THIS box. What is proven is that the control plane no longer executes the
// command itself: it sends it, and something else answers. What is NOT proven is that the answering process
// was ever on different hardware.
const ToolExecutionMachine = "one box, two processes: a REAL packages/runner router behind a REAL lease wire, with the control plane and the machine on the SAME host — no second computer was rented, enrolled or dialed anywhere in this release"

// ---------------------------------------------------------------------------------------------------------
// The Faz A.3 EXIT-gate proof type — the `tool-execution-0.1.0` sign-off.
//
// WHAT THIS BUNDLE CLAIMS, and every clause is narrower than the sentence a reader will want it to be:
//
//   - A SYNCHRONOUS COMMAND EXECUTES OFF THE CONTROL PLANE. `Orchestrator.shell` and `SetShellRunner` are
//     DELETED; the executor is derived per attempt from the attempt's own channel, and an attempt with no
//     machine is handed nothing and the tool refuses.
//   - THE SIX CODING TOOLS FOLLOW IT. `file` (read and write), `commit`, `push`, `pull_request` and
//     `show_media` act through `ws.*` on the machine, and the allocation and the clone are opened there too.
//   - THE NEGATIVE HALF IS WHAT MAKES THAT MEAN ANYTHING. With the machine's answer withheld, a workspace
//     tool REFUSES rather than falling back to this host's disk — asserted per tool, each shown red under a
//     perturbation that reddened its own line.
//   - BACKGROUND IS ADDRESSED TO THE MACHINE, including a machine that holds NO lease, and a machine that
//     cannot be reached is `lost` rather than `exited` and is NEVER signalled.
//   - THE RUNNER RUNS NATIVELY ON THE MAC, and the container runner is refused in the same bring-up from two
//     independent directions.
//
// AND THE CEILING THAT OUTRANKS ALL FIVE: NOT ONE OF THEM WAS PROVEN THROUGH A RUN. Every claim above is
// proven by COMPOSITION — a real wire here, a real router there, a real lease between them — and never once
// by a single model → engine → `tool.request` → dispatch → `exec.request` → machine transcript. The
// `uname` proof file this phase exists to turn green still has two legs outstanding, and this release names
// them rather than rounding them to zero.
// ---------------------------------------------------------------------------------------------------------

// ToolExecutionProof is the evidence a tool_execution_claim requires. Its groups are (a)..(g) plus the
// contract ledger, and EVERY counter is RE-DERIVED from the carried bytes: not one is trusted as declared.
//
//	(a) PlacementLedger / VerbsOnTheMachine / VerbsInTheControlPlane — every tool verb this phase moved,
//	    beside the shipped test that proves WHERE it ran. The second counter is NOT required to be zero and
//	    that is deliberate: A.3 moved seven verbs and left named surfaces behind (T5's §3), and a proof that
//	    could only say "all of it moved" would have to lie about the ones that did not.
//	(b) FallbackLedger / ToolsThatFellBackToThisHost / FallbacksWithAPerturbation — the negative half. The
//	    first MUST be zero and the second MUST equal the row count: a refusal nobody perturbed is a refusal
//	    that may be answering for an unrelated reason, which is this tree's most expensive recorded shape.
//	(c) BackgroundLedger / BackgroundVerbsOnTheMachine / SignalsToAnUnreachableMachine — all three verbs
//	    addressed to the machine, and the second MUST be zero: a pgid is a small integer and this reaper
//	    spans tenants, so a signal sent on an unproven handle need not have hit Palai's process at all.
//	(d) PostureLedger / PosturesTheRunnerCanBuild — BOTH, or a Linux pool's commands are still running
//	    beside the control plane and the phase's own exit criterion is unreachable by construction.
//	(e) UnameLedger / UnameLegsMeasured / UnameLegsOutstanding — THE HONEST ABSENCE, and the group this
//	    release would be dishonest without. Every OUTSTANDING leg must carry the reason it is absent rather
//	    than zero; a leg that just says "no" is refused.
//	(f) CeilingLedger / CeilingsCarried — the ceilings the seven tasks wrote in their own reports, each with
//	    the task that owns it and the line it was measured at. A report is not a record until a gate reads it.
//	(g) SupersededLedger / CeilingsSuperseded — the crown of this bundle: the PUBLISHED ceilings A.3
//	    invalidated, each naming the release that carries it, the clause that is now false, and the evidence.
type ToolExecutionProof struct {
	Machine string `json:"machine"`

	// (a) Where each tool verb executes.
	PlacementLedger        json.RawMessage `json:"placement_ledger"`
	VerbsOnTheMachine      int             `json:"verbs_on_the_machine"`
	VerbsInTheControlPlane int             `json:"verbs_in_the_control_plane"`

	// (b) The negative half: nothing silently falls back to this host's disk.
	FallbackLedger              json.RawMessage `json:"fallback_ledger"`
	ToolsThatFellBackToThisHost int             `json:"tools_that_fell_back_to_this_host"`
	FallbacksWithAPerturbation  int             `json:"fallbacks_with_a_perturbation"`

	// (c) Background is addressed to the machine, and an unreachable one is never signalled.
	BackgroundLedger              json.RawMessage `json:"background_ledger"`
	BackgroundVerbsOnTheMachine   int             `json:"background_verbs_on_the_machine"`
	SignalsToAnUnreachableMachine int             `json:"signals_to_an_unreachable_machine"`

	// (d) The runner can build both postures, so a Linux pool's commands can leave this process.
	PostureLedger             json.RawMessage `json:"posture_ledger"`
	PosturesTheRunnerCanBuild int             `json:"postures_the_runner_can_build"`

	// (e) The `uname` proof file's legs — measured and OUTSTANDING, with the reason for each absence.
	UnameLedger          json.RawMessage `json:"uname_ledger"`
	UnameLegsMeasured    int             `json:"uname_legs_measured"`
	UnameLegsOutstanding int             `json:"uname_legs_outstanding"`

	// (f) The seven tasks' own ceilings, carried into the record.
	CeilingLedger   json.RawMessage `json:"ceiling_ledger"`
	CeilingsCarried int             `json:"ceilings_carried"`

	// (g) The PUBLISHED ceilings this phase invalidated.
	SupersededLedger   json.RawMessage `json:"superseded_ledger"`
	CeilingsSuperseded int             `json:"ceilings_superseded"`

	// (h) The published contracts and on-tree measurements, anchored to the code table.
	Contracts       []ContractRequirement `json:"contracts"`
	ContractsDigest string                `json:"contracts_digest"`
}

// toolExecutionPlacementRow is one tool verb and where it executed.
type toolExecutionPlacementRow struct {
	Verb       string `json:"verb"`
	Surface    string `json:"surface"`
	ExecutedOn string `json:"executed_on"`
	Test       string `json:"test"`
}

// toolExecutionFallbackRow is one tool driven with the machine's answer withheld.
type toolExecutionFallbackRow struct {
	Tool               string `json:"tool"`
	WithTheMachineGone string `json:"with_the_machine_gone"`
	FellBackToThisHost bool   `json:"fell_back_to_this_host"`
	Perturbation       string `json:"perturbation"`
	ReddenedAt         string `json:"reddened_at"`
}

// toolExecutionBackgroundRow is one background verb or lifecycle answer.
type toolExecutionBackgroundRow struct {
	Verb                  string `json:"verb"`
	AddressedToTheMachine bool   `json:"addressed_to_the_machine"`
	Outcome               string `json:"outcome"`
	Signalled             bool   `json:"signalled"`
	Test                  string `json:"test"`
}

// toolExecutionPostureRow is one shell posture the runner's composition root can build.
type toolExecutionPostureRow struct {
	Posture string `json:"posture"`
	BuiltBy string `json:"built_by"`
	Test    string `json:"test"`
}

// toolExecutionUnameRow is one leg of palai-cloud/docs/measurements/faz-0-uname-proof.md.
//
// `Measured` is the discriminator and `WhyAbsent` is what stops this group being a rounding: an outstanding
// leg MUST say why it is absent, because "0" and "we could not take it" are different facts and only one of
// them is honest about what a reader should expect.
type toolExecutionUnameRow struct {
	Leg       string `json:"leg"`
	Expected  string `json:"expected"`
	Measured  bool   `json:"measured"`
	Answer    string `json:"answer"`
	WhyAbsent string `json:"why_absent"`
}

// toolExecutionCeilingRow is one ceiling a task wrote in its own report.
type toolExecutionCeilingRow struct {
	ID      string `json:"id"`
	Task    string `json:"task"`
	Source  string `json:"source"`
	Ceiling string `json:"ceiling"`
}

// toolExecutionSupersededRow is one PUBLISHED ceiling this phase invalidated.
//
// THE SHAPE IS THE POINT. A superseded ceiling is not a correction to be applied to the old text — that text
// was TRUE when its release shipped and rewriting it would be forging a record. It is a NEW record naming the
// old one: which release carries it, which clause is now false, what makes it false, and — the field that
// makes this checkable rather than assertable — the symbol the old reasoning RESTED ON, which a sweep can
// confirm is gone.
type toolExecutionSupersededRow struct {
	Release      string `json:"release"`
	Clause       string `json:"clause"`
	NowFalse     string `json:"now_false"`
	RestedOn     string `json:"rested_on"`
	RestedOnGone bool   `json:"rested_on_gone"`
	Evidence     string `json:"evidence"`
}

// SweepToolExecutionPlacement re-derives (a) from the ledger bytes.
func SweepToolExecutionPlacement(raw json.RawMessage) (onMachine, inControlPlane int, tests []string, err error) {
	var rows []toolExecutionPlacementRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return 0, 0, nil, err
	}
	for _, r := range rows {
		if r.Verb == "" || r.Surface == "" || r.Test == "" {
			return 0, 0, nil, errToolExecution("a placement row names no verb, no surface or no test")
		}
		switch r.ExecutedOn {
		case "machine":
			onMachine++
		case "control-plane":
			inControlPlane++
		default:
			return 0, 0, nil, errToolExecution("a placement row's executed_on is neither `machine` nor `control-plane`: " + r.ExecutedOn)
		}
		tests = append(tests, r.Test)
	}
	return onMachine, inControlPlane, tests, nil
}

// SweepToolExecutionFallbacks re-derives (b) from the ledger bytes.
func SweepToolExecutionFallbacks(raw json.RawMessage) (fellBack, withAPerturbation, rows int, err error) {
	var list []toolExecutionFallbackRow
	if err := json.Unmarshal(raw, &list); err != nil {
		return 0, 0, 0, err
	}
	for _, r := range list {
		if r.Tool == "" || r.WithTheMachineGone == "" {
			return 0, 0, 0, errToolExecution("a fallback row names no tool or no outcome")
		}
		if r.FellBackToThisHost {
			fellBack++
		}
		// A REFUSAL NOBODY PERTURBED IS A REFUSAL THAT MAY BE ANSWERING FOR AN UNRELATED REASON. This tree
		// recorded five of those in one day; requiring the perturbation and the line it reddened is the
		// cheapest available defence, and it is why this counter exists beside the zero rather than under it.
		if r.Perturbation != "" && r.ReddenedAt != "" {
			withAPerturbation++
		}
	}
	return fellBack, withAPerturbation, len(list), nil
}

// SweepToolExecutionBackground re-derives (c) from the ledger bytes.
func SweepToolExecutionBackground(raw json.RawMessage) (onMachine, signalsToUnreachable int, err error) {
	var rows []toolExecutionBackgroundRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return 0, 0, err
	}
	for _, r := range rows {
		if r.Verb == "" || r.Test == "" {
			return 0, 0, errToolExecution("a background row names no verb or no test")
		}
		if r.AddressedToTheMachine {
			onMachine++
		}
		if r.Outcome == "unreachable" && r.Signalled {
			signalsToUnreachable++
		}
	}
	return onMachine, signalsToUnreachable, nil
}

// SweepToolExecutionPostures re-derives (d) from the ledger bytes.
func SweepToolExecutionPostures(raw json.RawMessage) ([]string, error) {
	var rows []toolExecutionPostureRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	postures := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Posture == "" || r.BuiltBy == "" || r.Test == "" {
			return nil, errToolExecution("a posture row names no posture, no builder or no test")
		}
		if !slices.Contains(postures, r.Posture) {
			postures = append(postures, r.Posture)
		}
	}
	return postures, nil
}

// SweepToolExecutionUname re-derives (e) from the ledger bytes, and it is the sweep that REFUSES a rounded
// absence: an outstanding leg with no `why_absent` is an error rather than a zero.
func SweepToolExecutionUname(raw json.RawMessage) (measured, outstanding int, err error) {
	var rows []toolExecutionUnameRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return 0, 0, err
	}
	for _, r := range rows {
		if r.Leg == "" || r.Expected == "" {
			return 0, 0, errToolExecution("a uname row names no leg or no expectation")
		}
		if r.Measured {
			if r.Answer == "" {
				return 0, 0, errToolExecution("a MEASURED uname leg carries no answer, so nothing was measured: " + r.Leg)
			}
			measured++
			continue
		}
		if r.WhyAbsent == "" {
			return 0, 0, errToolExecution("an OUTSTANDING uname leg says only that it is missing and not why — \"absent\" and \"zero\" are different facts: " + r.Leg)
		}
		outstanding++
	}
	return measured, outstanding, nil
}

// SweepToolExecutionCeilings re-derives (f) from the ledger bytes.
func SweepToolExecutionCeilings(raw json.RawMessage) ([]string, error) {
	var rows []toolExecutionCeilingRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.ID == "" || r.Task == "" || r.Source == "" || r.Ceiling == "" {
			return nil, errToolExecution("a ceiling row names no id, no task, no source or no ceiling")
		}
		if slices.Contains(ids, r.ID) {
			return nil, errToolExecution("a ceiling id appears twice: " + r.ID)
		}
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// SweepToolExecutionSuperseded re-derives (g) from the ledger bytes.
//
// `rested_on_gone` IS CHECKED AGAINST THE TREE BY THE GATE, not here — this sweep only refuses a row that
// declares a supersession with no evidence behind it. A row claiming a published sentence is now false while
// naming neither the clause nor what makes it false is an opinion, and an opinion cannot supersede a record.
func SweepToolExecutionSuperseded(raw json.RawMessage) ([]string, error) {
	var rows []toolExecutionSupersededRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	symbols := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Release == "" || r.Clause == "" || r.NowFalse == "" || r.Evidence == "" {
			return nil, errToolExecution("a superseded row names no release, no clause, no reason or no evidence")
		}
		if r.RestedOn == "" {
			return nil, errToolExecution("a superseded row names no symbol its reasoning rested on, so nothing about it can be re-measured: " + r.Release)
		}
		symbols = append(symbols, r.RestedOn)
	}
	return symbols, nil
}

type toolExecutionError string

func (e toolExecutionError) Error() string { return string(e) }

func errToolExecution(msg string) error { return toolExecutionError(msg) }

// ToolExecutionPosturesRequired is both shell postures. The runner's composition root must be able to build
// each, or a Linux pool's commands are still executing beside the control plane and this phase's own exit
// criterion — `faz-0-uname-proof.md`'s second row answering `Linux` — is unreachable by construction rather
// than merely unmeasured. T1 shipped with only the host posture wired and recorded exactly that.
const ToolExecutionPosturesRequired = 2

// ToolExecutionBackgroundVerbs is the three verbs the background half owes: start, probe and kill. Fewer is a
// half-move, and a half-moved background is worse than an unmoved one — the row would name a machine the
// signal never reached.
const ToolExecutionBackgroundVerbs = 3

// carriesA3ToolExecutionCase is the family marker. It is the CASE IDS and not the tool_execution_claim this
// gate enforces: dispatching on the claim would mean a bundle that dropped the claim silently rerouted to a
// weaker gate that never asks for it, which is this repository's recorded promote-gate-family-dispatch defect.
func carriesA3ToolExecutionCase(c evidenceCase) bool {
	return slices.Contains(ToolExecutionCaseIDs, c.ID)
}

// ---------------------------------------------------------------------------------------------------------
// The canonical ledgers. EVERY BYTE A BUNDLE CARRIES IS READ FROM HERE rather than typed into the generator,
// so a proof that declared a number this tree does not support could not be written by the generator at all.
// ---------------------------------------------------------------------------------------------------------

// ToolExecutionPlacementLedger is every tool verb Faz A.3 moved, beside the shipped test that proves WHERE it
// ran — and the surfaces it did NOT move, named rather than omitted.
//
// THE CONTROL-PLANE ROWS ARE NOT AN OVERSIGHT AND THE COUNTER DOES NOT REQUIRE THEM TO BE ZERO. T5 measured
// them by line and handed them to A.4; a proof shaped to say "all of it moved" would have had to drop them,
// and a ledger that can only record success is a ledger nobody can be wrong in front of.
const ToolExecutionPlacementLedger = `[
  {"verb":"shell","surface":"palai.workspace.shell","executed_on":"machine",
   "test":"apps/control-plane/internal/execution/remote_shell_test.go:TestRemoteShellRunsTheCommandOnTheMachineThatHoldsTheLease"},
  {"verb":"shell (per attempt)","surface":"tool_dispatch.shellFor","executed_on":"machine",
   "test":"apps/control-plane/internal/execution/attempt_shell_test.go:TestTwoAttemptsOnTwoMachinesGetTwoAnswers"},
  {"verb":"file.write","surface":"palai.workspace.file","executed_on":"machine",
   "test":"apps/control-plane/internal/execution/remote_workspace_test.go:TestAllSixCodingToolsActOnTheMachineThatHoldsTheLease"},
  {"verb":"file.read","surface":"palai.workspace.file","executed_on":"machine",
   "test":"apps/control-plane/internal/execution/remote_workspace_test.go:TestAllSixCodingToolsActOnTheMachineThatHoldsTheLease"},
  {"verb":"commit","surface":"palai.workspace.commit","executed_on":"machine",
   "test":"apps/control-plane/internal/execution/remote_workspace_test.go:TestAllSixCodingToolsActOnTheMachineThatHoldsTheLease"},
  {"verb":"push","surface":"palai.workspace.push","executed_on":"machine",
   "test":"apps/control-plane/internal/execution/remote_workspace_test.go:TestAllSixCodingToolsActOnTheMachineThatHoldsTheLease"},
  {"verb":"pull_request","surface":"palai.workspace.pull_request","executed_on":"machine",
   "test":"apps/control-plane/internal/execution/remote_workspace_test.go:TestAllSixCodingToolsActOnTheMachineThatHoldsTheLease"},
  {"verb":"show_media","surface":"palai.workspace.show_media","executed_on":"machine",
   "test":"apps/control-plane/internal/execution/remote_workspace_test.go:TestAllSixCodingToolsActOnTheMachineThatHoldsTheLease"},
  {"verb":"allocation.open","surface":"packages/runner openLeaseWorkspace","executed_on":"machine",
   "test":"packages/runner/workspace_open_test.go:TestTheMachineCreatesAnAllocationTheControlPlaneNamedUnderItsRoot"},
  {"verb":"clone","surface":"ws.request{clone} -> repositories.Prepare","executed_on":"machine",
   "test":"apps/control-plane/internal/execution/remote_workspace_test.go:TestAllSixCodingToolsActOnTheMachineThatHoldsTheLease"},
  {"verb":"approval push","surface":"approval.go:322,331","executed_on":"control-plane",
   "test":"apps/control-plane/internal/execution/remote_workspace_test.go:TestAWorkspaceToolRefusesRatherThanFallingBackToThisHostsDisk"},
  {"verb":"changeset","surface":"changeset.go:93","executed_on":"control-plane",
   "test":"apps/control-plane/internal/execution/remote_workspace_test.go:TestAWorkspaceToolRefusesRatherThanFallingBackToThisHostsDisk"},
  {"verb":"finalize","surface":"finalize.go:233,239","executed_on":"control-plane",
   "test":"apps/control-plane/internal/execution/remote_workspace_test.go:TestAWorkspaceToolRefusesRatherThanFallingBackToThisHostsDisk"},
  {"verb":"snapshot","surface":"snapshot.go:147","executed_on":"control-plane",
   "test":"apps/control-plane/internal/execution/remote_workspace_test.go:TestAWorkspaceToolRefusesRatherThanFallingBackToThisHostsDisk"}
]`

// ToolExecutionFallbackLedger is the NEGATIVE half, and it is the group that makes the placement ledger mean
// anything. A green "the tool ran on the machine" says a tool was called; it does not say the tool would
// have refused had the machine not been there. Every row names the perturbation that reddened its own line.
const ToolExecutionFallbackLedger = `[
  {"tool":"file (write)","with_the_machine_gone":"refused","fell_back_to_this_host":false,
   "perturbation":"the machine refuses 'write'","reddened_at":"remote_workspace_test.go:185"},
  {"tool":"file (read)","with_the_machine_gone":"refused","fell_back_to_this_host":false,
   "perturbation":"the machine refuses 'read'","reddened_at":"remote_workspace_test.go:199"},
  {"tool":"commit","with_the_machine_gone":"refused","fell_back_to_this_host":false,
   "perturbation":"the machine refuses 'commit'","reddened_at":"remote_workspace_test.go:208"},
  {"tool":"push","with_the_machine_gone":"refused","fell_back_to_this_host":false,
   "perturbation":"the machine refuses 'head'","reddened_at":"remote_workspace_test.go:220"},
  {"tool":"pull_request","with_the_machine_gone":"refused","fell_back_to_this_host":false,
   "perturbation":"the machine refuses the SECOND 'head' (isolated from push)","reddened_at":"remote_workspace_test.go:223"},
  {"tool":"show_media","with_the_machine_gone":"refused","fell_back_to_this_host":false,
   "perturbation":"the machine refuses the '.png' read only","reddened_at":"remote_workspace_test.go:240"},
  {"tool":"the refusal sentinel itself","with_the_machine_gone":"refused","fell_back_to_this_host":false,
   "perturbation":"the machine omits the 'code' field, so 'refused' degrades to 'failed'","reddened_at":"remote_workspace_test.go:298"},
  {"tool":"RemoteWorkspace.Write","with_the_machine_gone":"refused","fell_back_to_this_host":false,
   "perturbation":"RemoteWorkspace.Write falls through to the local disk","reddened_at":"remote_workspace_test.go:272"},
  {"tool":"shell (an attempt with no machine)","with_the_machine_gone":"refused","fell_back_to_this_host":false,
   "perturbation":"shellFor returns a process-wide executor again","reddened_at":"attempt_shell_test.go:141"}
]`

// ToolExecutionBackgroundLedger is the three background verbs and the three lifecycle answers. The lifecycle
// rows are here rather than in a comment because a lifecycle rule written as prose is the evidence a reader
// accepts INSTEAD of reading the caller — this repository's most expensive recorded comment defect.
const ToolExecutionBackgroundLedger = `[
  {"verb":"bg.start","addressed_to_the_machine":true,"outcome":"started on the attempt's machine","signalled":false,
   "test":"apps/control-plane/internal/execution/background_machine_test.go:TestADeploymentWithNoGatewayStartsNoBackgroundTask"},
  {"verb":"bg.probe","addressed_to_the_machine":true,"outcome":"the machine's own answer is used","signalled":false,
   "test":"apps/control-plane/internal/execution/background_machine_test.go:TestAReachableMachineIsAskedAndItsAnswerUsed"},
  {"verb":"bg.kill","addressed_to_the_machine":true,"outcome":"the signal goes to the row's machine","signalled":true,
   "test":"apps/control-plane/internal/execution/background_machine_test.go:TestAnUnreachableMachinesTaskIsNeverSignalled"},
  {"verb":"probe a machine holding NO lease","addressed_to_the_machine":true,"outcome":"answered from the park loop","signalled":false,
   "test":"apps/control-plane/internal/execution/machine_call_test.go:TestAParkedMachineAnswersABackgroundProbe"},
  {"verb":"cordon","addressed_to_the_machine":true,"outcome":"answers","signalled":false,
   "test":"apps/control-plane/internal/execution/machine_call_test.go:TestACordonedMachineStillAnswers"},
  {"verb":"revoke","addressed_to_the_machine":false,"outcome":"unreachable","signalled":false,
   "test":"apps/control-plane/internal/execution/machine_call_test.go:TestARevokedMachineIsUnreachable"},
  {"verb":"an unknown machine","addressed_to_the_machine":false,"outcome":"unreachable","signalled":false,
   "test":"apps/control-plane/internal/execution/machine_call_test.go:TestAnUnknownMachineIsUnreachableRatherThanASilentWait"},
  {"verb":"a row naming no machine","addressed_to_the_machine":false,"outcome":"unreachable","signalled":false,
   "test":"apps/control-plane/internal/execution/background_machine_test.go:TestARowThatNamesNoMachineIsUnreachable"},
  {"verb":"a task on an unreachable machine","addressed_to_the_machine":false,"outcome":"unreachable","signalled":false,
   "test":"apps/control-plane/internal/execution/background_machine_test.go:TestATaskOnAnUnreachableMachineIsLostRatherThanExited"}
]`

// ToolExecutionPostureLedger is both shell postures, each with the composition root that builds it. T1
// shipped with only the host posture wired and SAID SO; T3 closed it by moving the derivation into a package
// both binaries read, which is also why the deployment catalogue can no longer express it (T3's §2 concern).
const ToolExecutionPostureLedger = `[
  {"posture":"unsandboxed-host","built_by":"adapters/sandboxes/posture.RunnerFromEnv",
   "test":"adapters/sandboxes/posture/darwin_machine_test.go:TestTheNativeMachineAnswersDarwin"},
  {"posture":"sandboxed-linux","built_by":"adapters/sandboxes/posture.RunnerFromEnv",
   "test":"apps/control-plane/internal/execution/tools/../../../../../deploy/compose/shell_posture_wiring_test.go:TestTheRunnerServiceCarriesTheShellPosture"}
]`

// ToolExecutionUnameLedger IS THE MOST IMPORTANT CONSTANT IN THIS FILE, because it is the one that says what
// this release did NOT do. The phase exists to make `faz-0-uname-proof.md`'s second row answer `Linux`; it
// does not yet, and neither leg was taken through a RUN.
//
// Every outstanding row carries `why_absent`, and SweepToolExecutionUname refuses one that does not. The
// distinction is not pedantry: "the Linux leg is zero" invites a reader to think it was tried, and
// "no Linux runner has been started on this host" tells them what would have to happen next.
const ToolExecutionUnameLedger = `[
  {"leg":"Mac, through the posture the native runner builds","expected":"Darwin","measured":true,
   "answer":"uname -s = Darwin, uname -m = arm64, whoami = salih, sw_vers = 26.3, executor = *host.Executor","why_absent":""},
  {"leg":"Linux, through a container runner's posture","expected":"Linux","measured":false,"answer":"",
   "why_absent":"no Linux runner has been started on this host by this phase. A.3 T2+T3 made the path REACHABLE — the runner's composition root now builds either posture through adapters/sandboxes/posture.RunnerFromEnv, and deploy/compose passes PALAI_SHELL_NATIVE to the runner service — but reachable is not measured, and this file carries only what was measured"},
  {"leg":"a whole RUN: model -> engine -> tool.request -> dispatch -> exec.request -> machine","expected":"the tool answer comes back from the machine's own executor","measured":false,"answer":"",
   "why_absent":"BLOCKED ON A SHARED HOST RATHER THAN ON THE TREE. The credential half was closed by the fake-script seam (commit f24b430e): a stack with no provider credential can now drive a scripted tool call, which is what made this leg possible at all. What blocks it is that ANOTHER AGENT'S STACK occupies this Mac (palai-a1e00dad-*, up 51 minutes at the time of measurement) and 'palai up --native' reconfigures the same compose project, so taking this leg would tear down work in flight. The proof was NOT fabricated and the epic's chain remains proven by COMPOSITION only"}
]`

// ToolExecutionCeilingLedger carries the seven tasks' own ceilings into the record. A report is a report
// until a gate reads it; after that it is a record. Each row names the task that owns it and the source it
// was measured at, so the next reader can re-measure rather than re-argue.
const ToolExecutionCeilingLedger = `[
  {"id":"A3-C1","task":"T1","source":"packages/runner/toolserver.go, cmd/runner/main.go",
   "ceiling":"ReceiveControllerFrame has NO production caller since serve.go moved to relayInbound; its three remaining callers are conformance tests driving it as an engine-frame surface. Kept deliberately, named here rather than left to be discovered"},
  {"id":"A3-C2","task":"T2","source":"apps/control-plane/internal/execution/remote_shell.go",
   "ceiling":"the machine's 'did not run' and its 'ran and failed' COLLAPSE at this seam: data.error becomes a Go error and tools/shell.go turns an error into an uncertain abort. Not a loss today because ToolServer.Handle already merges them, but the day the wire separates them a third answer shape is needed"},
  {"id":"A3-C3","task":"T3","source":"apps/control-plane/internal/execution/runner_gateway.go relayBacklog",
   "ceiling":"relayBacklog = 64 is a NUMBER and it can be wrong. The arithmetic is written down (the engine blocks on its own tool results while dispatch runs, so the backlog cannot exceed the batch it wrote) but a model's parallel tool-call count is not statically bounded. Overflow is NOT a hang: it ends the lease with a named error and the attempt retries"},
  {"id":"A3-C4","task":"T3","source":"apps/control-plane/internal/execution/settings catalogue, planeMatchesReader",
   "ceiling":"the deployment catalogue cannot express the shell posture, because it carries ONE plane per setting and adapters/sandboxes/posture is read by BOTH binaries. The three entries are filed under control-plane and their Effect text carries 'SET IT ON THE RUNNER', because the 'plane' field cannot say it. Whether the catalogue gains a 'both' plane is an open decision"},
  {"id":"A3-C5","task":"T4","source":"cmd/cli/internal/stack/native_runner.go",
   "ceiling":"a NATIVE runner still needs Docker. Once it holds a lease it supervises the engine in an OCI sandbox (PALAI_ENGINE_IMAGE), so 'palai up --native' removes tool execution from Docker and not Docker itself — which the word 'native' does not imply"},
  {"id":"A3-C6","task":"T5","source":"adapters/repositories/broker.go HostSpendable",
   "ceiling":"the repositories seal GREW BY ONE TARGET, and it is a security decision rather than a refactor. git reads a credential from a HELPER FILE beside the clone, so a repo on another machine means either the secret goes there or the clone does not happen. The extension is named, single-gated, TTL'd and revoked on every return path — but it is an extension"},
  {"id":"A3-C7","task":"T5","source":"storage/queries/workspaces.sql (grep -c runner_id -> 0, 2026-08-04)",
   "ceiling":"there is NO workspace affinity. Dial takes the next machine in the pool and never asks which machine holds the allocation, so in a two-machine pool a second run does not see the first's tree. Loud rather than silent — reuseAllocation reads HEAD over the wire and errors — and harmless in the single-machine pool that is today's only deployment"},
  {"id":"A3-C8","task":"T6","source":"apps/control-plane/internal/execution/orchestrator.go machineID = os.Hostname()",
   "ceiling":"os.Hostname() is good enough as a machine identity and is NOT unique. Two Macs can both be MacBook-Pro.local, and then B reads A's row as its own — the same defect this seam removed, through a narrower door. The stronger identity is the runner's enrolment SAN"},
  {"id":"A3-C9","task":"T6","source":"storage/migrations/000064_background_task_machine.up.sql",
   "ceiling":"rows that predate the machine column settle as 'lost' on the first sweep even if their process is genuinely running. The direction is safe (a lost handle is never signalled) but it is not silent: that run's model sees a task it was told is gone"},
  {"id":"A3-C10","task":"T7","source":"packages/runner/session.go, the park loop",
   "ceiling":"the park now reads NON-LEASE traffic, which is a surface widening. A control plane can keep a parked machine busy by writing to it; no rate limit and no concurrency ceiling were designed"},
  {"id":"A3-C11","task":"T7","source":"apps/control-plane/internal/execution/background.go:604",
   "ceiling":"the doc comment above probeOnMachine still documents 'reachesMachine' — a symbol commit 70a615f7 deleted — and asserts 'there is no protocol by which one control plane probes another machine's process group'. The function beneath it calls callMachine, which IS that protocol. Found by this acceptance gate; the comment claims an ABSENCE the code no longer has, at the exact line a reader would audit it"},
  {"id":"A3-C12","task":"T4 / E24 inherited","source":"docs/operations/known-gaps-1.0.md FLT-P2, and the RC's open §6 leg 2",
   "ceiling":"'linux/amd64' is STILL unverified, and a declared posture is COMPARED rather than attested — so an 'unsandboxed-host' pool does not stop a Linux container calling itself one. A.3 moved WHERE a command runs; it added no attestation of WHAT the machine is"}
]`

// ToolExecutionSupersededLedger IS THE CROWN OF THIS BUNDLE, and its shape is the argument.
//
// Two SHIPPED releases carry a ceiling this phase made false. Neither text is edited: `runner-fleet-0.1.0`
// and `fleet-console-0.1.0` were TRUE when they shipped, and rewriting a published record to keep it
// accurate is forging it. What a later release owes instead is a record naming the earlier one — which
// clause, what makes it false, and the symbol the old reasoning RESTED ON, because that last field is what a
// sweep can check rather than believe.
const ToolExecutionSupersededLedger = `[
  {"release":"runner-fleet-0.1.0",
   "clause":"E24 T7, the execution relay, WAS DEFERRED, so every tool still runs in the control plane's own process — a Mac is only a Mac when the control plane is ON it, and this bundle claims a fleet SPINE rather than remote execution",
   "now_false":"BOTH halves are now false, and they fail for different reasons. 'every tool still runs in the control plane's own process' — the shell and the six coding tools are dispatched over 'exec.request'/'ws.request' to the machine that holds the attempt's lease, and an attempt with no machine is handed no executor and the tool refuses rather than falling back here. 'a Mac is only a Mac when the control plane is ON it' — A.3 T4 runs the RUNNER natively on the Mac, so a Mac is a Mac when the RUNNER is on it, which is the opposite placement",
   "rested_on":"orch.SetShellRunner(shellRunnerFromEnv())",
   "rested_on_gone":true,
   "evidence":"grep -rn \"SetShellRunner\" --include='*.go' . | grep -v node_modules | wc -l -> 2 (2026-08-04), and BOTH survivors are prose inside tests/uat describing this very ceiling: tests/uat/evidence_fleet.go:22 and tests/uat/fleet/bundle_test.go:211. The production symbol was deleted by A.3 T3 (commit 7f652939) together with the Orchestrator.shell field; grep -rn 'o.shell' apps/control-plane/internal/execution/ | grep -v _test | wc -l -> 0"},
  {"release":"fleet-console-0.1.0",
   "clause":"FLT-P15 STANDS AND BOUNDS EVERYTHING ELSE — a lease.offer carries the ENGINE, every TOOL still executes in the control plane's own process, so this release makes a Mac pool CREATABLE and does not make a Mac run xcodebuild",
   "now_false":"the second clause only. A 'lease.offer' DOES still carry the engine — that half stands and is not superseded — but 'every TOOL still executes in the control plane's own process' does not, for the same reason as above. THE CONCLUSION IS NOT YET EARNED EITHER WAY: A.3 did not measure 'xcodebuild' on a Mac through a run, so this release replaces the ceiling with a NARROWER one rather than claiming the sentence's opposite",
   "rested_on":"SetShellRunner",
   "rested_on_gone":true,
   "evidence":"E28 quotes E24's FLT-P15 verbatim, so its clause rests on the SAME production fact and the SAME deleted symbol. AND THE OTHER HALF OF E24'S SENTENCE IS NOT NAMED HERE, BECAUSE IT DID NOT GO: E24 also grounded the ceiling on 'workspace.NewWorkspaceFS(env.WorkspaceRoot)' in the file and media tools, and that CONSTRUCTOR IS STILL IN PRODUCTION AND CORRECTLY SO — it is the machine-less path (tools/workspace_local.go:32) and the runner's OWN path (packages/runner/workspaceserver.go:166). What T5 deleted is the file tool CONSTRUCTING one inline: 'fs, err := workspace.NewWorkspaceFS(env.WorkspaceRoot)' became 'fs := env.Workspace', a per-attempt seam. Claiming the constructor gone was written into this ledger first and REFUSED by TestEverySupersededSymbolIsActuallyGoneFromTheTree, which counted 11 surviving production references — the guard caught this release overclaiming its own crown record. The behavioural evidence stands on its own: apps/control-plane/internal/execution/remote_workspace_test.go:TestAWorkspaceToolRefusesRatherThanFallingBackToThisHostsDisk, shown red by perturbing RemoteWorkspace.Write to the local disk (remote_workspace_test.go:272)"}
]`

// ToolExecutionContracts is the CANONICAL requirement ledger this release is anchored to — the gate's OWN
// copy. Every row carries the §3.5-style divergence id it accounts for and the source it was read from, and
// a bundle's contracts must EQUAL this table: a release cannot quietly drop the row that bounds it.
var ToolExecutionContracts = []ContractRequirement{
	{
		Divergence: "A3-D1",
		SourceURL:  "palai-cloud/docs/plans/2026-08-03-faz-a3-tool-yurutmesi-makineye.md#faz-a3-cikisi",
		Requirement: "THE PHASE'S OWN EXIT CRITERION, AND IT IS NOT MET: `faz-0-uname-proof.md`'s second row must " +
			"answer `Linux`. It does not. Two of its three legs are outstanding and this bundle names why for " +
			"each rather than reporting a zero — the group exists so a release cannot pass by omitting the " +
			"measurement it was cut to produce",
	},
	{
		Divergence: "A3-D2",
		SourceURL:  "palai-cloud/docs/plans/2026-08-03-faz-a3-tool-yurutmesi-makineye.md#faz-a3-cikisi",
		Requirement: "`grep -rn \"o.shell\" apps/control-plane/internal/execution/ | grep -v _test | wc -l` -> 0 " +
			"and `grep -rn \"SetShellRunner\" --include='*.go' . | grep -v node_modules | wc -l` -> 0. The first " +
			"holds. THE SECOND DOES NOT AND THE TWO SURVIVORS ARE THIS BUNDLE'S OWN SUBJECT: both are prose in " +
			"tests/uat describing the E24 ceiling A.3 superseded. The criterion was written against production " +
			"code and is met there; it is recorded as PARTIAL rather than green, because a criterion whose " +
			"wording no longer matches what it meant is a criterion nobody can check",
	},
	{
		Divergence: "A3-D3",
		SourceURL:  "evidence/releases/runner-fleet-0.1.0/manifest.json (63 lines, 2026-08-04)",
		Requirement: "A PUBLISHED CEILING MAY BE SUPERSEDED AND MUST NOT BE EDITED. E24's sentence was true when " +
			"runner-fleet-0.1.0 shipped; a later release records that it no longer holds, naming the clause, what " +
			"makes it false and the symbol the old reasoning rested on. Editing the shipped bytes would move an " +
			"anchor 63 case checksums recompute against and would forge a record besides",
	},
	{
		Divergence: "A3-D4",
		SourceURL:  "tests/uat/release_index_asof_test.go:TestTheAsOfRuleIsWhatKeepsTheShippedRCGreen",
		Requirement: "A NEW BUNDLE NAME MUST NOT MOVE A SHIPPED RELEASE'S ANCHOR. `tool-execution-0.1.0` sorts " +
			"BEFORE `tools-memory-0.1.0`, so naming luck does not protect the RC here — the as-of rule does: the " +
			"RC was captured 2026-07-26T11:00:00Z and this bundle 2026-08-04, and carriers newer than the index " +
			"are dropped. Measured by running the shipped test rather than by reading the rule",
	},
	{
		Divergence: "A3-D5",
		SourceURL:  "tests/uat/evidence.go CapabilityClaims / CapabilityTierOrder",
		Requirement: "NO TIER ADVANCES AND NO CAPABILITY IS OPENED. A member on CapabilityTierOrder would move " +
			"CapabilityClaimsDigest and redden every case checksum in every committed bundle, so this release " +
			"opens none — the E28 precedent, for the same reason. `apple-build` stays DISABLED, which is the " +
			"honest tier for a phase that moved WHERE a command runs and never once ran `xcodebuild` on a Mac",
	},
	{
		Divergence: "A3-D6",
		SourceURL:  "apps/control-plane/internal/execution/background.go:604 (read 2026-08-04)",
		Requirement: "A COMMENT MAY NOT ASSERT AN ABSENCE THE CODE NO LONGER HAS. The doc comment above " +
			"`probeOnMachine` still names `reachesMachine` — deleted by 70a615f7 — and states there is no " +
			"protocol by which one control plane probes another machine's process group, while the function " +
			"beneath it calls `callMachine`, which is that protocol. Carried as ceiling A3-C11 rather than " +
			"silently fixed, because the acceptance gate's job is to record what it found",
	},
}

func toolExecutionContractParts() []string {
	parts := make([]string, 0, 3*len(ToolExecutionContracts))
	for _, req := range ToolExecutionContracts {
		parts = append(parts, req.Divergence, req.SourceURL, req.Requirement)
	}
	return parts
}

// ToolExecutionContractsDigest is hashParts over the CANONICAL contract ledger — this release's checksum
// anchor. A dropped or reworded row moves every checksum in the release.
func ToolExecutionContractsDigest() string { return hashParts(toolExecutionContractParts()...) }

// Complete reports the groups hold AND re-derives (a) through (g) from the bytes the proof carries. A proof
// declaring a placement the ledger does not hold, a zero over a fallback ledger containing a fallback, two
// background verbs standing in for three, one posture standing in for two, an outstanding `uname` leg with no
// reason, or a superseded ceiling with no evidence fails HERE — in the shape verifier — rather than in a
// dedicated test somebody could forget to run.
func (p ToolExecutionProof) Complete() bool {
	if p.Machine != ToolExecutionMachine || p.ContractsDigest != ToolExecutionContractsDigest() ||
		!slices.Equal(p.Contracts, ToolExecutionContracts) {
		return false
	}

	// (a) Placement. The machine count must be positive and must match what the ledger holds; the
	// control-plane count is checked for AGREEMENT rather than for zero, because the surfaces A.4 inherits
	// are part of this record.
	onMachine, inCP, tests, err := SweepToolExecutionPlacement(p.PlacementLedger)
	if err != nil || onMachine < 1 || p.VerbsOnTheMachine != onMachine || p.VerbsInTheControlPlane != inCP {
		return false
	}
	if len(tests) != onMachine+inCP {
		return false
	}

	// (b) The negative half. Zero fallbacks, and EVERY row perturbed — a refusal nobody perturbed may be
	// answering for a reason unrelated to what it claims.
	fellBack, perturbed, rows, err := SweepToolExecutionFallbacks(p.FallbackLedger)
	if err != nil || fellBack != 0 || rows < 1 || perturbed != rows {
		return false
	}
	if p.ToolsThatFellBackToThisHost != fellBack || p.FallbacksWithAPerturbation != perturbed {
		return false
	}

	// (c) Background: all three verbs addressed to the machine, and no signal to one we could not reach.
	bgOnMachine, signals, err := SweepToolExecutionBackground(p.BackgroundLedger)
	if err != nil || signals != 0 || bgOnMachine < ToolExecutionBackgroundVerbs {
		return false
	}
	if p.BackgroundVerbsOnTheMachine != bgOnMachine || p.SignalsToAnUnreachableMachine != signals {
		return false
	}

	// (d) Both postures, or a Linux pool's commands never leave this process.
	postures, err := SweepToolExecutionPostures(p.PostureLedger)
	if err != nil || len(postures) != ToolExecutionPosturesRequired || p.PosturesTheRunnerCanBuild != len(postures) {
		return false
	}

	// (e) The `uname` legs. AN OUTSTANDING LEG IS ALLOWED — this release does not pretend otherwise — but a
	// ledger with no measured leg at all would be a bundle claiming a phase that measured nothing, and the
	// declared counts must equal what the bytes hold.
	measured, outstanding, err := SweepToolExecutionUname(p.UnameLedger)
	if err != nil || measured < 1 || p.UnameLegsMeasured != measured || p.UnameLegsOutstanding != outstanding {
		return false
	}

	// (f) The tasks' ceilings.
	ceilings, err := SweepToolExecutionCeilings(p.CeilingLedger)
	if err != nil || len(ceilings) < 1 || p.CeilingsCarried != len(ceilings) {
		return false
	}

	// (g) The superseded ceilings. At least one, or this bundle has no reason to be a bundle: the record it
	// exists to write is the record that E24's published ceiling no longer holds.
	superseded, err := SweepToolExecutionSuperseded(p.SupersededLedger)
	if err != nil || len(superseded) < 1 || p.CeilingsSuperseded != len(superseded) {
		return false
	}
	return true
}
