// The Faz A.3 EXIT gate's bundle half: the tool-execution-0.1.0 evidence bundle, its generator, and the
// shrink guards in both directions. Everything here is Docker-free pure logic, so it rides `make verify`.
//
// WHAT THIS BUNDLE CLAIMS, and it is deliberately narrower than "the tool runs on the machine": that the
// CONTROL PLANE NO LONGER RUNS THE COMMAND — the process-wide executor and its setter are deleted, the
// executor is derived per attempt from the attempt's own channel, a synchronous command and all six coding
// tools cross a real lease wire to the machine that holds the lease, a workspace tool REFUSES rather than
// falling back to this host's disk, background start/probe/kill are addressed to that machine and an
// unreachable one is `lost` and never signalled, the runner's composition root can build both shell postures,
// and the runner runs natively on the Mac.
//
// AND THE HONEST CEILING, WHICH IS THE MOST IMPORTANT SENTENCE IN THIS PACKAGE: NOT ONE OF THOSE CLAIMS WAS
// PROVEN THROUGH A RUN. Every leg is proven by COMPOSITION — a real websocket here, a real mTLS lease there,
// the real packages/runner router between them — and never once by a single model → engine → `tool.request`
// → dispatch → `exec.request` → machine transcript. There was no second computer: the control plane and the
// machine are two processes on ONE box. `xcodebuild` was never executed on a Mac, `linux/amd64` remains
// unverified, and the phase's own exit criterion — `faz-0-uname-proof.md`'s second row answering `Linux` —
// IS NOT MET. NO TIER ADVANCES: `apple-build` stays DISABLED.
//
// AND THE RECORD THIS BUNDLE EXISTS TO WRITE: two SHIPPED releases carry a ceiling this phase made false.
// Neither text is edited — `runner-fleet-0.1.0` and `fleet-console-0.1.0` were true when they shipped, and
// rewriting a published record would move an anchor sixty-three case checksums recompute against and forge a
// record besides. The supersession is a NEW record naming the old one, down to the symbol its reasoning
// rested on, and TestEverySupersededSymbolIsActuallyGoneFromTheTree walks the tree for each.
package toolexecution

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

func bundleDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "evidence", "releases", uat.ToolExecutionBundle)
}

// baselineManifest is the committed E28 EXIT bundle. Faz A.3 forked from `main` after fleet-console-0.1.0
// merged, so the inherited case set is derived from it rather than retyped — and that is also why an A.3
// bundle owes the fleet-console proof, which PromoteGateFor's composition then judges.
const baselineManifest = uat.FleetConsoleBundle

// toolExecutionAnchorCaseID is the release-level entry the tool-execution proof hangs off. Like
// E28-FLEET-CONSOLE, E26-BACKGROUND, E25-ADMIN-CONSOLE and the rest it is NOT a UAT case (it has no
// tests/uat/cases directory): "a published ceiling no longer holds" is not a behaviour one case asserts, it
// is an observation over the whole record.
const toolExecutionAnchorCaseID = "A3-TOOL-EXECUTION"

// The anchors carried forward from the releases underneath. A bundle carrying E28 cases must carry the
// fleet-console anchor, one carrying E26 cases the background anchor, and so down — so all nine ride along
// unchanged, and this release is judged on the tiers it did NOT move.
const (
	fleetConsoleAnchorCaseID = "E28-FLEET-CONSOLE"
	backgroundAnchorCaseID   = "E26-BACKGROUND"
	consoleAnchorCaseID      = "E25-ADMIN-CONSOLE"
	approvalAnchorCaseID     = "E23-TOOL-APPROVAL"
	shipAnchorCaseID         = "E22-CODE-AND-SHIP"
	memoryAnchorCaseID       = "E21-TOOLS-MEMORY"
	surfaceAnchorCaseID      = "E20-SURFACE"
	wiringAnchorCaseID       = "E19-WIRING"
	tierAnchorCaseID         = "E17-TIER"
)

// hashParts reproduces the tests/uat construction (sha256 of each part followed by a NUL, hex-encoded,
// sha256:-prefixed) so this generator and the verifier derive the same re-derivable values.
// ponytail: the same 6-line copy the fleet-console / background / admin-console / fleet gates keep. A drift
// between this copy and the verifier's shows up immediately as a bundle whose checksums do not recompute.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// syntheticImageDigest is an obviously-unservable digest, the fleet-console / background precedent: A.3's
// legs are component-tier against real PostgreSQL and a real lease wire, so there is no engine image to name
// and the shape verifier's required field carries a value no registry could serve.
var syntheticImageDigest = "sha256:" + strings.Repeat("a3e", 21) + "e"

func newCaseIDs() []string { return uat.ToolExecutionCaseIDs }

// buildToolExecutionManifest assembles the bundle. Every inherited case comes from the committed E28
// baseline; every proof body comes from a canonical uat table. Nothing is typed twice.
func buildToolExecutionManifest(t *testing.T) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "evidence", "releases", baselineManifest, "manifest.json"))
	if err != nil {
		t.Fatalf("read the %s baseline (this release's inherited case set is derived from it, never retyped): %v", baselineManifest, err)
	}
	var base struct {
		Cases []map[string]any `json:"cases"`
	}
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatalf("decode the baseline: %v", err)
	}

	anchor := uat.ToolExecutionContractsDigest()
	cases := make([]map[string]any, 0, len(base.Cases)+len(newCaseIDs())+1)

	for _, bc := range base.Cases {
		id, _ := bc["id"].(string)
		if id == "" {
			t.Fatalf("the baseline carries a case with no id: %v", bc)
		}
		c := map[string]any{}
		for k, v := range bc {
			c[k] = v
		}
		runID := "run_a3_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c["run_id"] = runID
		c["checksum"] = hashParts(id, runID, anchor)
		c["db_assertions"] = append(append([]string{}, toStrings(t, id, bc["db_assertions"])...),
			"FAZ A.3 TOOL-EXECUTION RELEASE: this case's LOCAL seam is unchanged from "+baselineManifest+
				" — what changed is WHERE its tools would execute. Before this phase `Orchestrator.shell` was a "+
				"PROCESS-WIDE executor installed once at boot through `orch.SetShellRunner(shellRunnerFromEnv())`, "+
				"so every attempt in every tenant shared one executor and every command ran beside the control "+
				"plane whatever machine the run was placed on. Both symbols are DELETED: `grep -rn \"o.shell\" "+
				"apps/control-plane/internal/execution/ | grep -v _test | wc -l` answers 0, and the executor is "+
				"now derived per attempt from the attempt's own channel — an attempt with no machine is handed "+
				"nothing and the tool refuses rather than falling back here. WHAT THIS CHANGES FOR THIS CASE: "+
				"nothing about its own seam. No route it exercises moved, no capability was added — a member on "+
				"CapabilityTierOrder would move CapabilityClaimsDigest and redden every case checksum in every "+
				"committed bundle, which is why A.3 opened none. AND THE CEILING THAT OUTRANKS ALL OF IT: NOTHING "+
				"IN THIS RELEASE WAS PROVEN THROUGH A RUN. Every leg is proven by COMPOSITION — a real websocket, "+
				"a real mTLS lease, the real packages/runner router — and never once by a single model → engine → "+
				"`tool.request` → dispatch → `exec.request` → machine transcript. There was no second computer: "+
				"the control plane and the machine are two processes on ONE box. `xcodebuild` was never executed "+
				"on a Mac, `linux/amd64` remains unverified, and this phase's own exit criterion — "+
				"`faz-0-uname-proof.md`'s second row answering `Linux` — IS NOT MET and is recorded as "+
				"outstanding rather than rounded to zero. So this case's §6 operator legs are untouched and its "+
				"capability's tier does not move.")
		cases = append(cases, c)
	}

	// ---- the four ids Faz A.3 OPENED ----------------------------------------------------------------
	for _, id := range newCaseIDs() {
		runID := "run_a3_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c := map[string]any{
			"id": id, "status": "PASS", "proof_class": caseProofClass(t, id),
			"run_id":              runID,
			"image_digest":        syntheticImageDigest,
			"provider_request_id": "prov_a3_deterministic",
			"mtls_enroll":         "component-tier: a REAL mTLS enrolment against a real PostgreSQL and the shipped gateway, by a runner process on THIS host — no second computer is claimed anywhere in this release",
			"terminal":            map[string]any{"type": "response.completed", "count": 1},
			"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
			"db_assertions": []string{
				caseAssertion(t, id),
				"OPENED BY FAZ A.3 UNDER THE `EXE-` PREFIX, and that prefix is a GATE decision rather than a " +
					"naming preference: `FLT-` (E24), `CON-` (E25), `BGT-` (E26) and `FLC-` (E28) are ALL already " +
					"inside extensionIDPrefixes with their own owner lists and their own promote gates, so an " +
					"`FLT-006` would match carriesE24FleetCase in PromoteGateFor and dispatch to a WEAKER gate that " +
					"knows nothing about this phase's placement ledger, its no-fallback half, its background " +
					"addressing, its outstanding `uname` legs or its SUPERSEDED-ceiling record — and would pass a " +
					"bundle that dropped all five. E20, E21, E22, E25, E26 and E28 each paid for that reasoning " +
					"once. AND THE OTHER DIRECTION IS CLOSED IN THE SAME CHANGE RATHER THAN AFTERWARDS, which is " +
					"what makes this the first family with no correction paragraph: `HIL-`, `CON-`, `BGT-` and " +
					"`FLC-` each shipped case directories BEFORE their prefix entered tests/uat/extensions — the " +
					"ONLY sweep in the tree that walks tests/uat/cases — and every one of those directories was " +
					"resolved by nothing while every UAT package reported ok. `EXE-` entered extensionIDPrefixes " +
					"and uat.ToolExecutionCaseIDs in the same commit as EXE-001",
				"NOT in uat.CapabilityClaims and therefore NOT an input to any tier recompute. THE TIER ARGUMENT " +
					"IS THE STRONGEST ANY RELEASE IN THIS TREE HAS HAD FOR MOVING `apple-build`, and it is still " +
					"refused: the reason `apple-build` was disabled is that a Mac pool routed a run's ENGINE while " +
					"every tool executed in the control plane, and A.3 fixed exactly that. It is refused for three " +
					"reasons. (1) NOTHING WAS PROVEN THROUGH A RUN — a tier is a statement about what a user gets, " +
					"and a user gets runs; every leg here is composition. (2) `xcodebuild` WAS NEVER EXECUTED ON A " +
					"MAC by this phase: the `uname` ledger's Mac leg answers `Darwin` through the posture the " +
					"native runner builds, which is the executor and not a build, and the Linux leg was never " +
					"taken at all. (3) A DECLARED POSTURE IS STILL COMPARED AND NEVER ATTESTED (`FLT-P2`) — A.3 " +
					"moved WHERE a command runs and added no attestation of WHAT the machine is, so an " +
					"`unsandboxed-host` pool still does not stop a Linux container calling itself one. WHAT WOULD " +
					"HAVE HAD TO BE TRUE: one run, on a Mac pool, whose `xcodebuild` receipt came back from the " +
					"machine",
				"HONEST CEILING, AND THE FIRST PART IS THE ONE A READER OF \"THE TOOL RUNS ON THE ATTEMPT'S " +
					"MACHINE\" WILL ASSUME THE OPPOSITE OF. THERE WAS NO SECOND COMPUTER. The control plane and " +
					"the machine are two processes on ONE box, connected by a real lease wire; what is proven is " +
					"that the control plane no longer executes the command itself, and what is NOT proven is that " +
					"the answering process was ever on different hardware. NOTHING WAS DRIVEN THROUGH A RUN: the " +
					"chain model → engine → `tool.request` → dispatch → `exec.request` → machine holds by " +
					"composition and has never been observed end to end in one transcript. The credential half of " +
					"that blocker was closed by the fake-script seam, so the leg is now POSSIBLE; what blocked it " +
					"was another agent's stack occupying this host. `linux/amd64` is unverified. There is NO " +
					"workspace affinity — `grep -c runner_id storage/queries/workspaces.sql` answers 0 — so a " +
					"two-machine pool's second run does not see the first's tree. `os.Hostname()` is the machine " +
					"identity and is not unique. `relayBacklog = 64` is a number that can be wrong, though its " +
					"failure is a named attempt error rather than a hang. §6 legs are operator work and are NEVER " +
					"claimed here",
			},
		}
		c["checksum"] = hashParts(id, runID, anchor)
		cases = append(cases, c)
	}

	// ---- the tool-execution anchor: the move, the refusal, the absence and the supersession -------------
	toolExecutionCase := map[string]any{
		"id": toolExecutionAnchorCaseID, "status": "PASS", "proof_class": "component-real",
		"run_id":              "run_a3_tool_execution_anchor",
		"image_digest":        syntheticImageDigest,
		"provider_request_id": "prov_a3_deterministic",
		"mtls_enroll":         "component-tier: a REAL mTLS enrolment against a real PostgreSQL and the shipped gateway, by a runner process on THIS host — no second computer is claimed anywhere in this release",
		"terminal":            map[string]any{"type": "response.completed", "count": 1},
		"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		"db_assertions": []string{
			"RELEASE-LEVEL entry, not a UAT case (it has no tests/uat/cases directory): \"the control plane no longer runs the command\", \"nothing falls back to this host's disk\" and \"a published ceiling no longer holds\" are not behaviours one case asserts, they are observations over a whole record — the E28-FLEET-CONSOLE / E26-BACKGROUND / E25-ADMIN-CONSOLE / E23-TOOL-APPROVAL / E22-CODE-AND-SHIP / E21-TOOLS-MEMORY / E20-SURFACE / E19-WIRING / E17-TIER precedent",
			"THE FIRST CROWN CLAIM IS THAT THE CONTROL PLANE NO LONGER RUNS THE COMMAND, AND THE MEASUREMENT MATTERS MORE THAN THE FEATURE. `Orchestrator.shell` was a PROCESS-WIDE executor installed once at boot; every attempt in every tenant shared it, so `palai.workspace.shell` ran beside the control plane whatever machine the run was placed on. Both it and `SetShellRunner` are DELETED (`grep -rn \"o.shell\" apps/control-plane/internal/execution/ | grep -v _test | wc -l` -> 0) and the executor is derived PER ATTEMPT by a type assertion on the attempt's own channel. THE FAILED BRANCH IS THE CLAIM: it returns an UNTYPED nil rather than a typed nil `*RemoteShell`, because a typed nil would be a NON-nil interface and the tool's `env.Shell == nil` refusal would never fire — the shipped-but-unreachable-guard shape this repository keeps finding. An attempt with no machine is handed nothing and the tool answers `unavailable`, so the model is told rather than the command silently running here",
			"THE SECOND CROWN CLAIM IS THAT THE NEGATIVE HALF IS ASSERTED PER TOOL, AND IT IS WHAT MAKES THE FIRST ONE MEAN ANYTHING. A green \"the tool ran on the machine\" says a tool was called; it does not say the tool would have REFUSED had the machine not answered, and a build that tries the machine and quietly uses the local disk passes the first assertion and fails every user. Nine rows, each shown red by a perturbation that reddened ITS OWN LINE — the machine refusing `write` (:185), `read` (:199), `commit` (:208), `head` (:220), a SECOND `head` isolated for pull_request (:223), a `.png` read isolated for show_media (:240), the sentinel's `code` field (:298), `RemoteWorkspace.Write` falling to the local disk (:272), and `shellFor` returning a process-wide executor again (attempt_shell_test.go:141). AND ONE PERTURBATION STAYED GREEN AND THE PERTURBATION WAS THE GUILTY PARTY: a fallback added to `file.go` was never on the branch the test drove, which is the tree's own rule — when a perturbation stays green, ask first whether the PROPERTY actually broke",
			"THE THIRD CROWN CLAIM IS THAT BACKGROUND MOVED WHOLE RATHER THAN HALF. `background_tasks` carried thirteen columns and none said WHERE the process was: liveness is a pgid probe plus a start time, and both read ONE machine's kernel. A plane probing its own kernel for another machine's task cannot tell `never mine` from `exited`, and `exited` tells a model its build finished while it is still compiling. Migration 000064 puts `machine_id` on the row; all three verbs are addressed to that machine over a `bg.*` family correlated by `bg_id` rather than `exec_id`, so a stray `exec.result` can never satisfy a background wait; and a control plane can address a machine holding NO LEASE, which is the case the reconciler actually needs. Building that required making the runner's park a LOOP — it was a single `Read` awaiting a `lease.offer`, so a probe sent to a parked machine would have been consumed as a malformed offer and KILLED THE PARK, and shipping the gateway half alone could have dropped every parked runner in a fleet. AN UNREACHABLE MACHINE IS `lost` AND IS NEVER SIGNALLED, because a pgid is a small integer and this reaper spans tenants; and TWO UNKNOWNS ARE NOT A MATCH, because resolving the rows with the least evidence to a local probe is the wrong answer this seam exists to stop",
			"THE FOURTH CLAIM IS AN ABSENCE STATED BY NAME RATHER THAN A NUMBER ROUNDED TO ZERO, AND THIS RELEASE WOULD BE DISHONEST WITHOUT IT. The phase was cut to make `faz-0-uname-proof.md`'s second row answer `Linux`. IT DOES NOT. Three legs: the Mac leg is MEASURED (uname -s = Darwin, uname -m = arm64, whoami = salih, sw_vers = 26.3, executor = *host.Executor, through the posture the native runner builds); the LINUX leg is OUTSTANDING because no Linux runner has been started on this host by this phase — A.3 T2+T3 made the path reachable and reachable is not measured; and the WHOLE-RUN leg is OUTSTANDING because another agent's stack occupied this Mac and `palai up --native` reconfigures the same compose project. The sweep REFUSES an outstanding leg that carries no reason, because \"the Linux leg is zero\" invites a reader to think it was tried while \"no Linux runner has been started here\" tells them what would have to happen next. THE PROOF WAS NOT FABRICATED, and the credential half of the blocker is now closed: the fake-script seam lets a stack with no provider credential drive a scripted tool call, so the leg is POSSIBLE for the first time",
			"THE FIFTH CLAIM IS THE ONE THIS BUNDLE EXISTS TO WRITE: TWO SHIPPED RELEASES CARRY A CEILING THIS PHASE MADE FALSE, AND NEITHER TEXT IS EDITED. `runner-fleet-0.1.0` says on sixty-three lines that E24 T7 was deferred \"so every tool still runs in the control plane's own process — a Mac is only a Mac when the control plane is ON it\". BOTH HALVES ARE NOW FALSE AND THEY FAIL FOR DIFFERENT REASONS: the tools are dispatched to the lease-holding machine, and A.3 T4 runs the RUNNER natively on the Mac, so a Mac is a Mac when the RUNNER is on it — the opposite placement. `fleet-console-0.1.0` repeats the second half inside `FLT-P15`; its `lease.offer` clause STANDS and is not superseded. THAT TEXT WAS TRUE WHEN IT SHIPPED. Editing it would move an anchor sixty-three case checksums recompute against and would forge a record besides, so the supersession is a NEW record naming the old one — the release, the clause, what makes it false, and THE SYMBOL THE OLD REASONING RESTED ON, which is the field that makes this checkable rather than assertable. E24 grounded its sentence on `orch.SetShellRunner(shellRunnerFromEnv())`; A.3 T3 deleted it, and a test in this package walks the tree for every symbol the ledger claims is gone",
			"THE SIXTH CLAIM IS THAT THE SEVEN TASKS' OWN CEILINGS ARE IN THE RECORD RATHER THAN IN SEVEN REPORTS. Twelve rows, each naming the task that owns it and the line it was measured at — among them that `relayBacklog = 64` is a number that can be wrong (its failure is a named attempt error, not a hang), that the deployment catalogue cannot express the shell posture because it carries ONE plane per setting while `adapters/sandboxes/posture` is read by BOTH binaries, that there is NO workspace affinity, that `os.Hostname()` is not unique, that a NATIVE runner still needs Docker for the engine sandbox, and that the `repositories` seal grew by one target because git reads its credential from a helper FILE beside the clone. ELEVEN OF THE TWELVE ARE STILL OPEN AND THE TWELFTH IS WHY THIS LEDGER HAS A CLOSURE FIELD AT ALL. A3-C11 was found by this acceptance gate — the doc comment above `probeOnMachine` documented `reachesMachine`, a symbol 70a615f7 deleted, and asserted that \"there is no protocol by which one control plane probes another machine's process group\" while the function beneath it called `callMachine`, which IS that protocol — and it was CLOSED by `4d84ab0e` before this bundle was cut. The first version of this bundle carried it as open and was therefore stale on the day it shipped. OVERSTATING A CEILING IS AS WRONG AS UNDERSTATING ONE: this release was written to record that a published ceiling had become false, and it had begun making the same mistake in the opposite direction against itself. The ledger now carries `closed_by` and the proof re-derives OPEN ceilings separately from carried ones, so a bundle cannot report a defect that is already shut. The repair itself COUNTED THE BRANCHES rather than writing an unqualified clean sentence, which is this tree's own rule for correcting a comment",
			"NO TIER ADVANCES, and the counter-argument is recorded because it is the strongest one any release in this tree has had. THE ARGUMENT: `apple-build` was disabled precisely because a Mac pool routed a run's ENGINE while every tool executed in the control plane; A.3 fixed exactly that and the runner now runs natively on a Mac, so the blocker named in the tier table is gone. IT IS REFUSED THREE TIMES OVER. Nothing was proven through a RUN, and a tier is a statement about what a user gets. `xcodebuild` was never executed on a Mac by this phase. And a declared posture is still COMPARED and never attested, so an `unsandboxed-host` pool does not stop a Linux container calling itself one. `slack` closes preview, `knowledge-vector` disabled, `console` PREVIEW, `apple-build` DISABLED — and `apple-build` staying disabled in the very phase that moved execution is the honest reading of what moved: WHERE a command runs, not WHAT the machine is",
		},
		"tool_execution_claim": "tool execution proven off the control plane: the process-wide executor and its setter are DELETED and the executor is derived per attempt, a synchronous command and all six coding tools cross a real lease wire to the machine that holds the attempt's lease, each of nine tools REFUSES rather than falling back to this host's disk under a perturbation that reddened its own line, all three background verbs are addressed to that machine — including one holding no lease — an unreachable machine is `lost` and never signalled, the runner's composition root builds both shell postures and the runner runs natively on the Mac — with NO second computer, NO run-level transcript, NO `xcodebuild` on a Mac and NO verified `linux/amd64`, and with the two published ceilings this phase falsified recorded rather than edited",
		"tool_execution_proof": canonicalToolExecutionProof(),
	}
	toolExecutionCase["checksum"] = hashParts(toolExecutionAnchorCaseID, "run_a3_tool_execution_anchor", anchor)
	cases = append(cases, toolExecutionCase)

	manifest := map[string]any{
		"release":     uat.ToolExecutionBundle,
		"git_sha":     "a00f6c0",
		"api_version": "v1",
		// THE CHAIN HEAD IS A.3's OWN, and it is the first migration this phase opened: `machine_id` on
		// `background_tasks`. The field names the schema a bundle was CAPTURED AGAINST rather than one the
		// release added, but here they coincide. An empty string is refused by the shape verifier, and
		// rightly: "this release captured against no schema" is not a thing a manifest can honestly say.
		"migration":   "000064_background_task_machine",
		"captured_at": "2026-08-04T14:00:00Z",
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

// canonicalToolExecutionProof is the proof body the bundle carries. Every BYTE field is READ FROM A CANONICAL
// SOURCE in tests/uat rather than retyped here, and every declared COUNT is computed by the same sweep the
// gate runs — so a proof that declared a number the ledger does not support could not be written by this
// generator at all, let alone verify.
func canonicalToolExecutionProof() uat.ToolExecutionProof {
	onMachine, inCP, _, err := uat.SweepToolExecutionPlacement(json.RawMessage(uat.ToolExecutionPlacementLedger))
	if err != nil {
		panic("the placement ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	fellBack, perturbed, _, err := uat.SweepToolExecutionFallbacks(json.RawMessage(uat.ToolExecutionFallbackLedger))
	if err != nil {
		panic("the fallback ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	bgOnMachine, signals, err := uat.SweepToolExecutionBackground(json.RawMessage(uat.ToolExecutionBackgroundLedger))
	if err != nil {
		panic("the background ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	postures, err := uat.SweepToolExecutionPostures(json.RawMessage(uat.ToolExecutionPostureLedger))
	if err != nil {
		panic("the posture ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	measured, outstanding, err := uat.SweepToolExecutionUname(json.RawMessage(uat.ToolExecutionUnameLedger))
	if err != nil {
		panic("the uname ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	ceilings, openCeilings, err := uat.SweepToolExecutionCeilings(json.RawMessage(uat.ToolExecutionCeilingLedger))
	if err != nil {
		panic("the ceiling ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	superseded, err := uat.SweepToolExecutionSuperseded(json.RawMessage(uat.ToolExecutionSupersededLedger))
	if err != nil {
		panic("the superseded ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	return uat.ToolExecutionProof{
		Machine: uat.ToolExecutionMachine,

		// EVERY COUNT BELOW COMES FROM THE SWEEP, NEVER FROM A LITERAL. A literal here would be the one place
		// this proof could disagree with the bytes it carries — and two of these are ABSENCES rather than
		// achievements, which are exactly the numbers a generator is tempted to write by hand.
		PlacementLedger:        json.RawMessage(uat.ToolExecutionPlacementLedger),
		VerbsOnTheMachine:      onMachine,
		VerbsInTheControlPlane: inCP,

		FallbackLedger:              json.RawMessage(uat.ToolExecutionFallbackLedger),
		ToolsThatFellBackToThisHost: fellBack,
		FallbacksWithAPerturbation:  perturbed,

		BackgroundLedger:              json.RawMessage(uat.ToolExecutionBackgroundLedger),
		BackgroundVerbsOnTheMachine:   bgOnMachine,
		SignalsToAnUnreachableMachine: signals,

		PostureLedger:             json.RawMessage(uat.ToolExecutionPostureLedger),
		PosturesTheRunnerCanBuild: len(postures),

		UnameLedger:          json.RawMessage(uat.ToolExecutionUnameLedger),
		UnameLegsMeasured:    measured,
		UnameLegsOutstanding: outstanding,

		CeilingLedger:     json.RawMessage(uat.ToolExecutionCeilingLedger),
		CeilingsCarried:   len(ceilings),
		CeilingsStillOpen: openCeilings,

		SupersededLedger:   json.RawMessage(uat.ToolExecutionSupersededLedger),
		CeilingsSuperseded: len(superseded),

		Contracts:       uat.ToolExecutionContracts,
		ContractsDigest: uat.ToolExecutionContractsDigest(),
	}
}

func toStrings(t *testing.T, id string, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: db_assertions is not a list: %T", id, v)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("%s: a db_assertion is not a string: %T", id, e)
		}
		out = append(out, s)
	}
	return out
}

// TestCommittedToolExecutionBundleIsTheGeneratorOutput pins the committed bundle to the tree: it must be
// BYTE-identical to this generator's output, so a contract-ledger change, a ledger-row change, a case-set
// change or a baseline change cannot leave a stale bundle verifying green.
// Regenerate with: PALAI_WRITE_TOOL_EXECUTION_BUNDLE=1 go test ./tests/uat/tool-execution/
func TestCommittedToolExecutionBundleIsTheGeneratorOutput(t *testing.T) {
	want := buildToolExecutionManifest(t)
	path := filepath.Join(bundleDir(t), "manifest.json")

	if os.Getenv("PALAI_WRITE_TOOL_EXECUTION_BUNDLE") == "1" {
		if err := os.MkdirAll(bundleDir(t), 0o755); err != nil {
			t.Fatalf("create release dir: %v", err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatalf("write bundle: %v", err)
		}
		t.Logf("wrote %s", path)
		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the committed bundle: %v (regenerate with PALAI_WRITE_TOOL_EXECUTION_BUNDLE=1)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the committed %s bundle is not this generator's output — regenerate with PALAI_WRITE_TOOL_EXECUTION_BUNDLE=1", uat.ToolExecutionBundle)
	}
}

// TestToolExecutionReleaseVerifiesClean runs the committed bundle through the SHIPPED verifier and requires
// 0 failed / 0 missing / 0 secret.
func TestToolExecutionReleaseVerifiesClean(t *testing.T) {
	summary, err := uat.VerifyRelease(bundleDir(t), nil)
	if err != nil {
		t.Fatalf("verify the tool-execution release: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("the tool-execution bundle did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatal("the tool-execution bundle verified 0 passed cases — a zero-case bundle is not a clean bundle")
	}
	t.Logf("%s: %s", uat.ToolExecutionBundle, summary.String())
}

// TestTheFourNewCasesAreInTheBundle is the shrink guard the E18 T8 sweep taught: a release that OPENED four
// ids must carry all four, or the ids exist in the tree with no bundle certifying them.
func TestTheFourNewCasesAreInTheBundle(t *testing.T) {
	var m struct {
		Cases []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"cases"`
	}
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}
	carried := map[string]string{}
	for _, c := range m.Cases {
		carried[c.ID] = c.Status
	}
	for _, id := range uat.ToolExecutionCaseIDs {
		if status, ok := carried[id]; !ok {
			t.Errorf("%s is a case this phase OPENED but the bundle that certifies it does not carry it", id)
		} else if status != "PASS" {
			t.Errorf("%s is carried as %q; this release claims it green", id, status)
		}
	}
	for _, anchor := range []string{toolExecutionAnchorCaseID, fleetConsoleAnchorCaseID, backgroundAnchorCaseID, consoleAnchorCaseID, approvalAnchorCaseID, shipAnchorCaseID, memoryAnchorCaseID, surfaceAnchorCaseID, wiringAnchorCaseID, tierAnchorCaseID} {
		if _, ok := carried[anchor]; !ok {
			t.Errorf("%s is not in the bundle — every anchor underneath this release rides along, so the release is judged on the tiers it did NOT move", anchor)
		}
	}
}

// TestTheBundleClaimsNoRunLevelProofAndNoSecondComputer is this release's absence guard, and it exists
// because both absences are things a reader of "tool execution moved to the machine" will assume the
// opposite of. A second computer and an end-to-end run are the two things somebody will believe shipped
// here; neither did.
//
// STRUCTURAL, NOT A SUBSTRING SCAN — E24's own note on this shape: a byte scan for "machine" would pass on
// this bundle's honest paragraphs explaining what the machine was not.
func TestTheBundleClaimsNoRunLevelProofAndNoSecondComputer(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	var m struct {
		Cases []struct {
			ID            string                  `json:"id"`
			Claim         string                  `json:"tool_execution_claim"`
			ToolExecution *uat.ToolExecutionProof `json:"tool_execution_proof"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}
	proofs := 0
	for _, c := range m.Cases {
		if c.ToolExecution == nil {
			continue
		}
		proofs++
		if c.ToolExecution.Machine != uat.ToolExecutionMachine {
			t.Errorf("%s carries machine %q — the control plane and the machine are two processes on ONE box, and a field that could say \"remote\" is a field that could claim a second computer", c.ID, c.ToolExecution.Machine)
		}
		// THE ABSENCE IS IN THE PROOF ITSELF, as a count rather than as prose. A release whose `uname` ledger
		// reported everything measured would be claiming the run-level leg this phase did not take.
		if c.ToolExecution.UnameLegsOutstanding < 1 {
			t.Errorf("%s: the proof reports ZERO outstanding `uname` legs. This phase's own exit criterion — the second row answering `Linux` — is NOT met, and a bundle reporting it complete would be the overclaim this group exists to prevent", c.ID)
		}
		if _, _, err := uat.SweepToolExecutionUname(c.ToolExecution.UnameLedger); err != nil {
			t.Errorf("%s: the `uname` ledger no longer sweeps: %v", c.ID, err)
		}
	}
	if proofs != 1 {
		t.Errorf("the bundle carries %d tool-execution proof(s), want exactly 1 — a second could ride behind an honest one", proofs)
	}
}

// TestEverySupersededSymbolIsActuallyGoneFromTheTree IS THE GUARD THAT MAKES THE SUPERSESSION CHECKABLE
// RATHER THAN ASSERTABLE, and it is the second direction of a claim the promote gate can only check one way.
//
// The gate is pure over committed bytes, so it can read the ledger's `rested_on_gone` flag and no more. A
// flag is a declaration. THIS test walks the TREE: for every symbol the ledger says the old reasoning rested
// on, the production code must no longer contain it — because if it does, the earlier ceiling may well still
// stand and this release's central record is an opinion about a shipped bundle rather than a supersession of
// it.
//
// THE SEARCH EXCLUDES tests/uat DELIBERATELY AND THAT EXCLUSION IS THE INTERESTING PART. `SetShellRunner`
// still appears exactly twice in this tree and BOTH are prose inside tests/uat — evidence_fleet.go:22 and
// fleet/bundle_test.go:211 — which are the E24 bundle's own description of the very ceiling being superseded
// here. A sweep that counted those would refuse the supersession because the superseded text mentions its own
// subject, which is the substring-comparison defect this repository has shipped four times. The claim is
// about PRODUCTION code, so the search is over production code, and the exclusion is asserted rather than
// assumed: the test FAILS if excluding tests/uat stops changing the answer, because then it is filtering
// nothing and a future symbol would slip through unmeasured.
func TestEverySupersededSymbolIsActuallyGoneFromTheTree(t *testing.T) {
	symbols, err := uat.ToolExecutionSupersededSymbols()
	if err != nil {
		t.Fatalf("read the superseded ledger: %v", err)
	}
	if len(symbols) == 0 {
		t.Fatal("the superseded ledger names no symbol, so this release's central record cannot be checked against the tree")
	}
	root := repoRoot(t)
	for _, sym := range symbols {
		// The ledger names a call site or an expression; the SYMBOL is its identifier head, which is what a
		// tree search can actually resolve.
		needle := sym
		if i := strings.IndexAny(needle, "("); i > 0 {
			needle = needle[:i]
		}
		if i := strings.Index(needle, " "); i > 0 {
			needle = needle[:i]
		}
		if j := strings.LastIndex(needle, "."); j >= 0 {
			needle = needle[j+1:]
		}
		if strings.TrimSpace(needle) == "" {
			t.Errorf("%q reduces to no searchable symbol", sym)
			continue
		}
		all := grepCount(t, root, needle, false)
		production := grepCount(t, root, needle, true)
		if production != 0 {
			t.Errorf("the superseded ledger claims `%s` is gone, but %q still appears %d time(s) in PRODUCTION code. Then the ceiling it grounded may still stand, and this release's supersession of it is an opinion rather than a record", sym, needle, production)
		}
		if all == production {
			t.Errorf("excluding tests/uat changed nothing for %q (%d = %d): this filter exists because the SUPERSEDED bundle's own prose names its own subject, and a filter that removes nothing is a filter nobody can rely on for the next symbol", needle, all, production)
		}
	}
}

// grepCount counts the lines in the tree containing needle, optionally excluding the tests/uat prose that
// describes the very ceilings being superseded. It shells out to git-grep so the search honours .gitignore
// and never walks node_modules.
func grepCount(t *testing.T, root, needle string, excludeUAT bool) int {
	t.Helper()
	args := []string{"grep", "-I", "--fixed-strings", "-c", needle, "--", "*.go"}
	if excludeUAT {
		args = append(args, ":(exclude)tests/uat/*")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		// git-grep exits 1 with no output when nothing matches, which is the answer rather than a failure.
		if len(bytes.TrimSpace(out)) == 0 {
			return 0
		}
		t.Fatalf("git grep %q: %v", needle, err)
	}
	total := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		_, count, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n := 0
		for _, r := range count {
			if r < '0' || r > '9' {
				n = -1
				break
			}
			n = n*10 + int(r-'0')
		}
		if n > 0 {
			total += n
		}
	}
	return total
}
