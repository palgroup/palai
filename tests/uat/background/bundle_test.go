// The E26 EXIT gate's bundle half (plan §T7): the background-execution-0.1.0 evidence bundle, its generator,
// and the shrink guards in both directions. Everything here is Docker-free pure logic, so it rides
// `make verify`; the Docker-bound backing legs are driven from journey_test.go through
// scripts/uat/background.
//
// WHAT THIS BUNDLE CLAIMS, and it is deliberately narrower than "background execution works": that a tool call
// can return a PROCESS rather than a result and that process outlives the call, in both sandbox postures,
// measured from the kernel and from a real Docker daemon; that THE MODEL IS NOT BLOCKED in the strong form —
// another tool call completed while the process was still alive; that every refusal starts zero processes and
// carries its own non-vacuity control; that an exit calls the model back exactly once across two ticks, two
// control planes and a restart; that a reaper enforces a ceiling, a cancellation, an adoption and a collector
// from the operating system rather than from our own record; and that a credential's value lands in none of
// the five places a background task could put one.
//
// AND THE HONEST CEILING, WHICH IS THE MOST IMPORTANT SENTENCE IN THIS PACKAGE, IN TWO PARTS. THERE IS NO LIVE
// PROGRESS STREAM: this release NARROWS `CAS-P2`, it does not close it, and a model still learns nothing about
// a running build unless it reads a file. AND A BACKGROUND TASK DOES NOT SURVIVE A CONTROL-PLANE MOVE: T5 DID
// ship restart-adopt, so §4.2's "T5b was skipped" sentence is not owed — what adoption cannot do is cross a
// machine, and that does not close without the execution relay the owner deferred.
//
// uat.BackgroundMachine is structurally the literal "local", and the field is `Machine` rather than `Peer`
// because this epic reaches no counterparty at all. NO TIER ADVANCES.
package background

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

func bundleDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "evidence", "releases", uat.BackgroundBundle)
}

// baselineManifest is the committed E25 EXIT bundle. E26 forked from `main` after admin-console-0.1.0 merged,
// so the inherited case set is derived from it rather than retyped — and that is also why an E26 bundle owes
// the admin-console proof, which PromoteGateFor's composition then judges.
const baselineManifest = uat.AdminConsoleBundle

// backgroundAnchorCaseID is the release-level entry the background proof hangs off. Like E25-ADMIN-CONSOLE,
// E23-TOOL-APPROVAL, E22-CODE-AND-SHIP, E21-TOOLS-MEMORY, E20-SURFACE, E19-WIRING and E17-TIER it is NOT a UAT
// case (it has no tests/uat/cases directory): "all six of the harness's semantics are replicated" is not a
// behaviour one case asserts, it is an observation over a whole surface.
const backgroundAnchorCaseID = "E26-BACKGROUND"

// The anchors carried forward from the releases underneath. A bundle carrying E25 cases must carry the
// admin-console anchor, one carrying E23 cases the tool-approval anchor, and so down — so all six ride along
// unchanged, and this release is judged on the tiers it did NOT move.
const (
	consoleAnchorCaseID  = "E25-ADMIN-CONSOLE"
	approvalAnchorCaseID = "E23-TOOL-APPROVAL"
	shipAnchorCaseID     = "E22-CODE-AND-SHIP"
	memoryAnchorCaseID   = "E21-TOOLS-MEMORY"
	surfaceAnchorCaseID  = "E20-SURFACE"
	wiringAnchorCaseID   = "E19-WIRING"
	tierAnchorCaseID     = "E17-TIER"
)

// hashParts reproduces the tests/uat construction (sha256 of each part followed by a NUL, hex-encoded,
// sha256:-prefixed) so this generator and the verifier derive the same re-derivable values.
// ponytail: the same 6-line copy the admin-console / fleet / tool-approval / code-and-ship / tools-memory /
// agent-surface / wiring / extensions gates keep. A drift between this copy and the verifier's shows up
// immediately as a bundle whose checksums do not recompute.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// syntheticImageDigest is an obviously-unservable digest, the admin-console / fleet / tool-approval precedent:
// E26's legs are component-tier against real processes and a real daemon, so there is no engine image to name
// and the shape verifier's required field carries a value no registry could ever serve.
var syntheticImageDigest = "sha256:" + strings.Repeat("e26", 21) + "e"

// newCaseIDs is the five ids E26 opened, read from the CANONICAL table rather than retyped so this bundle and
// the orphan guards can never disagree about which cases exist.
func newCaseIDs() []string { return uat.BackgroundCaseIDs }

// buildBackgroundManifest assembles the bundle. Every inherited case comes from the committed E25 baseline;
// every proof body comes from a canonical uat table. Nothing is typed twice.
func buildBackgroundManifest(t *testing.T) []byte {
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

	anchor := uat.BackgroundContractsDigest()
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
		runID := "run_e26_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c["run_id"] = runID
		c["checksum"] = hashParts(id, runID, anchor)
		c["db_assertions"] = append(append([]string{}, toStrings(t, id, bc["db_assertions"])...),
			"E26 BACKGROUND-EXECUTION RELEASE: this case's LOCAL seam is unchanged from "+baselineManifest+
				" — what changed is that a tool call can now return a PROCESS instead of a result, and that "+
				"process belongs to the RUN rather than to the call that started it. Before this epic a "+
				"background task was not a missing feature but a CONTRADICTION, and two postures made it one "+
				"for two entirely different reasons: the OCI driver force-removed its container on an "+
				"unconditional defer, and the host executor killed the whole process GROUP through the "+
				"attempt's context. A parked attempt leaves that call, so 'park the run and keep the build "+
				"going' could not be built on top of either. WHAT THIS CHANGES FOR THIS CASE: nothing about "+
				"its own seam, and one thing about the tool it may use — a `palai.workspace.shell` call "+
				"WITHOUT the background flag is field-for-field what it was, pinned against a literal "+
				"recorded at the fork point, covering both what the model receives and what the ledger row "+
				"stores. AND THE CEILING THAT OUTRANKS ALL OF IT: THERE IS STILL NO LIVE PROGRESS STREAM. A "+
				"model learns nothing about a running build unless it READS A FILE; nobody pushes it ten more "+
				"lines. `CAS-P2` narrows and does not close. Nor does a background task survive a "+
				"control-plane MOVE: adoption works on the same machine, and a host pgid means nothing on "+
				"another box while an OCI container stays on the old daemon — a direct consequence of there "+
				"being exactly ONE machine, because the execution relay planned for E24 T7 was never shipped. "+
				"So this case's §6 operator leg is untouched and its capability's tier does not move.")
		cases = append(cases, c)
	}

	// ---- the five ids E26 OPENED --------------------------------------------------------------------
	for _, id := range newCaseIDs() {
		runID := "run_e26_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c := map[string]any{
			"id": id, "status": "PASS", "proof_class": caseProofClass(t, id),
			"run_id":              runID,
			"image_digest":        syntheticImageDigest,
			"provider_request_id": "prov_e26_deterministic",
			"mtls_enroll":         "component-tier: real host processes and a real Docker daemon on this box; no machine boundary is crossed and none is claimed",
			"terminal":            map[string]any{"type": "response.completed", "count": 1},
			"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
			"db_assertions": []string{
				caseAssertion(t, id),
				"OPENED BY E26 UNDER THE `BGT-` PREFIX, and that prefix is a GATE decision rather than a naming " +
					"preference: an id under a prefix another family owns would either force the regeneration of " +
					"a committed historical release — rewriting the record of a run that happened — or fall " +
					"through PromoteGateFor to a WEAKER family gate that knows nothing about the six replicated " +
					"semantics, the refusal controls, the two ownership postures, the exactly-once notice, the " +
					"reaper's duties or the redaction sites, and would pass it. E20, E21 and E22 each paid for " +
					"that once. AND THE OTHER HALF WAS OPEN IN THIS VERY EPIC: `BGT-` was in NO prefix list " +
					"while four of these five directories shipped, so all four escaped tests/uat/extensions — " +
					"the ONLY sweep in the tree that walks tests/uat/cases — and every UAT package reported ok. " +
					"Both directions are closed here: `BGT-` is in extensionIDPrefixes, and tests/uat/background " +
					"refuses an id in uat.BackgroundCaseIDs with no directory or with a declared proof that is " +
					"not in the tree. THE SECOND DIRECTION IS THE ONE THAT FOUND SOMETHING A SWEEP COULD NOT: " +
					"BGT-003's directory did not exist at all, because T3 shipped nine proofs and opened no case",
				"NOT in uat.CapabilityClaims and therefore NOT an input to any tier recompute — which is the " +
					"whole tier argument in one line: making a tool ASYNCHRONOUS is not evidence about the PLANE " +
					"that tool runs on. Every tier in this tree is a statement about a plane (a provider was " +
					"reached, a signing identity exists, an index was served, a console was deployed) and E26 " +
					"changed the SHAPE of a call on a plane that is unchanged — the same host executor, the same " +
					"OCI driver, the same allocation, the same machine. Adding a `background` member to " +
					"CapabilityTierOrder would shift CapabilityClaimsDigest and redden every case checksum in " +
					"every committed bundle, which is why E26 opened none",
				"HONEST CEILING, IN TWO PARTS AND BOTH LOAD-BEARING. FIRST: there is NO live progress stream. " +
					"The model must read the file; nobody tells it ten more lines arrived. This release NARROWS " +
					"`CAS-P2` rather than closing it, and the remaining half — `task_update` chunks plus a " +
					"progress channel on toolbroker.ShellRunner — is E27's. SECOND: adoption works on the SAME " +
					"MACHINE only. Move the control plane to another host and a host pgid means nothing there " +
					"while an OCI container stays on the old daemon, so every row becomes `lost`. That follows " +
					"from there being exactly one machine — E24 T7's execution relay was never shipped — and " +
					"nothing closes it before that relay does. §6 legs 1-6 (a real xcodebuild backgrounded on a " +
					"real Mac with a measured duration, a restart through a real service manager, the overnight " +
					"orphan hunt, the real rate of pgid reuse, a credential's observed exposure, and an operator " +
					"seeing a PALAI_DISPATCH_WORKERS=0 stack land nothing) are operator work and are NEVER " +
					"claimed here",
			},
		}
		c["checksum"] = hashParts(id, runID, anchor)
		cases = append(cases, c)
	}

	// ---- the background anchor: the semantics, the refusals, the ownership and the absences -------------
	backgroundCase := map[string]any{
		"id": backgroundAnchorCaseID, "status": "PASS", "proof_class": "component-real",
		"run_id":              "run_e26_background_journey",
		"image_digest":        syntheticImageDigest,
		"provider_request_id": "prov_e26_deterministic",
		"mtls_enroll":         "component-tier: real host processes and a real Docker daemon on this box; no machine boundary is crossed and none is claimed",
		"terminal":            map[string]any{"type": "response.completed", "count": 1},
		"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		"db_assertions": []string{
			"RELEASE-LEVEL entry, not a UAT case (it has no tests/uat/cases directory): \"all six of the replicated semantics hold\", \"every refusal starts nothing\" and \"a credential value lands in none of five places\" are not behaviours one case asserts, they are observations over a whole surface — the E25-ADMIN-CONSOLE / E23-TOOL-APPROVAL / E22-CODE-AND-SHIP / E21-TOOLS-MEMORY / E20-SURFACE / E19-WIRING / E17-TIER precedent",
			"THE FIRST CROWN CLAIM IS THAT THE MODEL IS NOT BLOCKED, AND IT IS ASSERTED IN ITS STRONG FORM BECAUSE THE WEAK ONE IS SATISFIED BY THE THING THIS EPIC IS NOT. \"It ran in the background\" is true of a build that parks the run and waits for the process — the run would resume later and a second tool would eventually run — so the proof carries TWO assertions in one function and neither alone is the claim. THE PARK ASSERT: the spawning dispatch returned nil rather than a park error, the run reads `running` in the database afterwards, and the model was handed a tool.result to continue on. THE SECOND-TOOL ASSERT: a DIFFERENT tool call, made in the SAME attempt after the spawn, completed and wrote a real file, with the kernel confirming the background process alive both BEFORE and AFTER it. Everything is driven BY TOOL NAME through the production dispatcher, so it traverses the broker a deployment registers, the shipped approval gate, the shipped ledger row and the shipped frame — a test that builds its own config never sees the config production builds, which is how a shell wall time was simultaneously unbounded on the host and refusing every call in the container while every sandbox test stayed green",
			"THE SECOND CROWN CLAIM IS THAT PROCESS OWNERSHIP MOVED FROM THE CALL TO THE RUN, IN BOTH POSTURES AND FOR TWO DIFFERENT REASONS. Before this epic a background task was a CONTRADICTION rather than a missing feature: the OCI driver removed its container on an unconditional `defer ContainerRemove(Force:true, RemoveVolumes:true)` that ran on the error path too, and the host executor bound the whole process GROUP to the attempt's context with a SIGKILL cancel. Both measurements STAY IN THE SUITE, because the synchronous posture must keep doing exactly what they describe. What changed is one line per posture and nothing else: `exec.CommandContext` became `exec.Command`, and a THIRD container entry point appeared beside the batch and interactive ones — a separate method rather than a flag, because a conditional defer is a lifetime somebody gets backwards exactly once. `dockerDriver.Run` is byte-unchanged, pinned by the sha256 of its exact source bytes against a committed word rather than against a baseline the guard recomputes for itself, and the detached path's HARDENING is proven identical STRUCTURALLY by go/ast — it hands ContainerCreate the result of createOptions and constructs nothing of its own — which is a stronger claim than the fields currently matching",
			"THE THIRD CROWN CLAIM IS AN ABSENCE, AND IT IS THE SHARPEST ONE IN THIS RELEASE. PID numbers are reused, so a row that survived a restart names a pgid that may now belong to anything on the machine, and the only evidence it is still ours is the start time recorded beside it. When that evidence does not match, the handle is `lost` and Kill sends NO SIGNAL AT ALL — not a best-effort one, not a SIGTERM instead of a SIGKILL — and the measurement is a REAL process started outside this program, a fabricated start time, and that process still running afterwards. The container posture enforces the identical rule with the evidence it has: a container carrying no io.palai.bg label is refused and found still running. That leaves us the ORPHAN, deliberately, and the trade is the design rule rather than a regret: NOT KILLING A STRANGER'S PROCESS MATTERS MORE THAN REAPING OUR OWN",
			"THE FOURTH CLAIM IS THAT EVERY REFUSAL STARTS ZERO PROCESSES AND EVERY REFUSAL CARRIES ITS OWN POSITIVE HALF — and the second clause is the one worth reading, because this epic watched the zero be free. T2's approval-gate RED PASSED AT THE FORK POINT: there was no spawn path at all, so \"the gate's spawn counter reads zero\" was guaranteed, and a security proof that cannot fail reports the same green as one that passes. Five refusals each now name what the SAME harness produced with the condition lifted, with the UNIT stated — because for the kill switch the honest control is a SYNCHRONOUS run rather than a background spawn, a silent downgrade being that refusal's dangerous failure. AND THE EXIT GATE FOUND A SIXTH INSTANCE OF THE SAME SHAPE: the escaping-output-path guard asserted three refusals and nothing else, so an executor whose Start refused EVERY input satisfied it — demonstrated by mutation, and closed by starting one process down a legal path on the same executor",
			"THE FIFTH CLAIM IS THAT AN EXIT CALLS THE MODEL BACK EXACTLY ONCE AND NEVER INTERRUPTS A RUN THAT DID NOT PARK. Exactly-once is a ROW rather than a mutex, and the scenarios are built so a mutex could not pass them: two reconciler ticks, TWO CONTROL PLANES over one database swept concurrently, and a crash-restart plane that never saw the spawn. Each scenario names the mutation that reddens it, because a property nobody has broken is a property nobody has tested — removing `notified_at IS NULL` from the single-winner UPDATE produces FOUR notifications. The already-terminal run is the row a tidy build would have dropped: there is no turn left for a notice to fold into, and both easy answers are wrong (queued, it sits until a sweep expires it unread; dropped, it is the silent loss every line of known-gaps refuses), so it is STAMPED and an operator sees it. The wake itself is CALLED rather than written — wakeParkedRunTx, E23 T1's choreography's third user — because two waking paths mean two waking bugs",
			"THE SIXTH CLAIM IS THAT A CREDENTIAL'S LIFETIME IS NOW THE TASK, WHICH MADE A WRITTEN CLAIM IN THIS TREE FALSE AND IT WAS CORRECTED RATHER THAN LEFT TO ROT. E25 T3 wrote that \"the value's whole life is one Execute call\" and was right when it wrote it; the moment a task detaches, the value lives in the kernel's environ copy for the whole life of the process and killing the control plane does not take it back. So the scope+fence+expiry triple that task said was unnecessary is necessary here, and `deadline_at` is its expiry — a credentialled task CANNOT be created unbounded, enforced by a code gate rather than a CHECK because unbounded stays legitimate for a task carrying nothing. Redaction lives on the READ path, because the row holds key NAMES and never values: both landing sites T2 and T4 each recorded as open — the log the model reads and the notice excerpt that lands in commands.payload and delivered_messages — are masked from ONE place, for the reason T5 collapsed two setters into one. Every scan DECODES before it searches, because a raw-byte sweep over encoded output can never fail",
			"AND THE PLAN'S OWN CEILING DID NOT WORK ON THE PLATFORM THIS EPIC TARGETS, WHICH IS RECORDED BECAUSE OVERSTATING A CEILING IS AS WRONG AS UNDERSTATING ONE. §0.4 wrote that the same uid can read a task's environment with `ps -E` or `/proc/<pid>/environ`. On macOS 26.3 `ps -E` and `ps e` disclose no environment at all — not another process's and not your own — and there is no /proc: a sleep started with a sentinel in its environment, probed three ways under the same uid, returned ZERO hits. The exposure's DIRECTION is unchanged and is what the runbook states: background LENGTHENS the window rather than widening it, and its length is `deadline_at`. The container posture still has /proc/<pid>/environ, so the runbook states BOTH platforms rather than repeating a demonstration an operator would find does not run",
			"NO TIER ADVANCES, and the reason is a RULE rather than an omission: MAKING A TOOL ASYNCHRONOUS IS NOT EVIDENCE ABOUT THE PLANE THAT TOOL RUNS ON. E22 did not advance one for DELETING a boundary, E23 did not for ADDING one, E24 did not for adding a PLANE, E25 did not for adding a CONFIGURATION SURFACE, and E26 does not for changing the SHAPE OF A CALL. `slack` closes preview, `knowledge-vector` disabled, `apple-build` disabled, `console` preview. `apple-build` in particular is disabled because there is no signing identity anywhere in this project, which backgrounding an xcodebuild does not touch. WHAT WOULD HAVE HAD TO BE TRUE to move one: a background task running somewhere the control plane is NOT (E24 T7's execution relay, never shipped), or a real signed build on a real Mac with a captured transcript. Neither exists",
		},
		"background_claim": "background execution proven: a tool call returns a PROCESS that outlives it in both postures, the model calls another tool while that process is still running and the call completes, every refusal starts zero processes and carries its own non-vacuity control, an exit notifies exactly once across two ticks / two control planes / a restart without interrupting a run that never parked, a reaper enforces a ceiling / a cancellation / an adoption / a collector from the operating system, and a credential value lands in none of the five sites — with no live progress stream claimed and no task surviving a control-plane move",
		"background_proof": canonicalBackgroundProof(),
	}
	backgroundCase["checksum"] = hashParts(backgroundAnchorCaseID, "run_e26_background_journey", anchor)
	cases = append(cases, backgroundCase)

	manifest := map[string]any{
		"release":     uat.BackgroundBundle,
		"git_sha":     "873a6a4",
		"api_version": "v1",
		"migration":   "000047_background_tasks",
		"captured_at": "2026-07-31T00:00:00Z",
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

// canonicalBackgroundProof is the proof body the bundle carries. Every BYTE field is READ FROM A CANONICAL
// SOURCE in tests/uat rather than retyped here, and every declared COUNT is computed by the same sweep the
// gate runs — so a proof that declared a number the ledger does not support could not be written by this
// generator at all, let alone verify.
func canonicalBackgroundProof() uat.BackgroundProof {
	covered, fromTheMachine, _, err := uat.SweepBackgroundSemantics(json.RawMessage(uat.BackgroundSemanticsLedger))
	if err != nil {
		panic("the semantics ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	spawns, controlled, _, err := uat.SweepBackgroundRefusals(json.RawMessage(uat.BackgroundRefusalLedger))
	if err != nil {
		panic("the refusal ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	survived, signals, _, err := uat.SweepBackgroundOwnership(json.RawMessage(uat.BackgroundOwnershipLedger))
	if err != nil {
		panic("the ownership ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	exactlyOnce, _, _, err := uat.SweepBackgroundNotices(json.RawMessage(uat.BackgroundNoticeLedger))
	if err != nil {
		panic("the notice ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	_, reaperFromTheMachine, _, err := uat.SweepBackgroundReaper(json.RawMessage(uat.BackgroundReaperLedger))
	if err != nil {
		panic("the reaper ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	hits, _, _, err := uat.SweepBackgroundRedaction(json.RawMessage(uat.BackgroundRedactionLedger))
	if err != nil {
		panic("the redaction ledger cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	return uat.BackgroundProof{
		Machine: uat.BackgroundMachine,

		// EVERY COUNT BELOW COMES FROM THE SWEEP, NEVER FROM A LITERAL. A literal here would be the one place
		// this proof could disagree with the bytes it carries, and half of these are equalities rather than
		// zeros — an equality written by hand is an equality nobody checked.
		SemanticsLedger:                 json.RawMessage(uat.BackgroundSemanticsLedger),
		SemanticsReplicated:             len(covered),
		SemanticsMeasuredFromTheMachine: fromTheMachine,

		RefusalLedger:                  json.RawMessage(uat.BackgroundRefusalLedger),
		ProcessesStartedUnderARefusal:  spawns,
		RefusalsWithANonVacuityControl: controlled,

		OwnershipLedger:              json.RawMessage(uat.BackgroundOwnershipLedger),
		PosturesThatOutliveTheirCall: survived,
		SignalsToUnprovableHandles:   signals,

		NoticeLedger:                  json.RawMessage(uat.BackgroundNoticeLedger),
		ScenariosNotifyingExactlyOnce: exactlyOnce,

		ReaperLedger:                       json.RawMessage(uat.BackgroundReaperLedger),
		ReaperDutiesMeasuredFromTheMachine: reaperFromTheMachine,

		RedactionLedger:                   json.RawMessage(uat.BackgroundRedactionLedger),
		EnvironmentValuesInAnyLandingSite: hits,

		Contracts:       uat.BackgroundContracts,
		ContractsDigest: uat.BackgroundContractsDigest(),
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

// TestCommittedBackgroundBundleIsTheGeneratorOutput pins the committed bundle to the tree: it must be
// BYTE-identical to this generator's output, so a contract-ledger change, a ledger-row change, a case-set
// change or a baseline change cannot leave a stale bundle verifying green.
// Regenerate with: PALAI_WRITE_BACKGROUND_BUNDLE=1 go test ./tests/uat/background/
func TestCommittedBackgroundBundleIsTheGeneratorOutput(t *testing.T) {
	want := buildBackgroundManifest(t)
	path := filepath.Join(bundleDir(t), "manifest.json")

	if os.Getenv("PALAI_WRITE_BACKGROUND_BUNDLE") == "1" {
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
		t.Fatalf("read the committed bundle: %v (regenerate with PALAI_WRITE_BACKGROUND_BUNDLE=1)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the committed %s bundle is not this generator's output — regenerate with PALAI_WRITE_BACKGROUND_BUNDLE=1", uat.BackgroundBundle)
	}
}

// TestBackgroundReleaseVerifiesClean runs the committed bundle through the SHIPPED verifier and requires
// 0 failed / 0 missing / 0 secret. It is the `make evidence-verify RELEASE=background-execution-0.1.0` gate,
// in-process.
func TestBackgroundReleaseVerifiesClean(t *testing.T) {
	summary, err := uat.VerifyRelease(bundleDir(t), nil)
	if err != nil {
		t.Fatalf("verify the background release: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("the background bundle did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatal("the background bundle verified 0 passed cases — a zero-case bundle is not a clean bundle")
	}
	t.Logf("%s: %s", uat.BackgroundBundle, summary.String())
}

// TestTheFiveNewCasesAreInTheBundle is the shrink guard the E18 T8 sweep taught: a release that OPENED five
// ids must carry all five, or the ids exist in the tree with no bundle certifying them.
func TestTheFiveNewCasesAreInTheBundle(t *testing.T) {
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
	for _, id := range uat.BackgroundCaseIDs {
		if status, ok := carried[id]; !ok {
			t.Errorf("%s is a case this epic OPENED but the bundle that certifies it does not carry it", id)
		} else if status != "PASS" {
			t.Errorf("%s is carried as %q; this release claims it green", id, status)
		}
	}
	for _, anchor := range []string{backgroundAnchorCaseID, consoleAnchorCaseID, approvalAnchorCaseID, shipAnchorCaseID, memoryAnchorCaseID, surfaceAnchorCaseID, wiringAnchorCaseID, tierAnchorCaseID} {
		if _, ok := carried[anchor]; !ok {
			t.Errorf("%s is not in the bundle — every anchor underneath this release rides along, so the release is judged on the tiers it did NOT move", anchor)
		}
	}
}

// TestTheBundleClaimsNoLiveProgressStreamAndNoCrossMachineSurvival is this release's version of the
// absence guard, and it exists because both absences are things a reader will assume the opposite of. A
// background task that reports its progress and one that survives a control-plane move are the two features
// somebody will believe shipped here; neither did.
//
// STRUCTURAL, NOT A SUBSTRING SCAN — E24's own note on this shape, arrived at from the other side: a byte scan
// for "progress" would fail on this bundle's honest paragraph explaining that there is none.
func TestTheBundleClaimsNoLiveProgressStreamAndNoCrossMachineSurvival(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	var m struct {
		Cases []struct {
			ID         string                     `json:"id"`
			Claim      string                     `json:"background_claim"`
			Background *uat.BackgroundProof       `json:"background_proof"`
			Extra      map[string]json.RawMessage `json:"-"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}
	proofs := 0
	for _, c := range m.Cases {
		if c.Background == nil {
			continue
		}
		proofs++
		if c.Background.Machine != uat.BackgroundMachine {
			t.Errorf("%s carries machine %q — this bundle measured nothing across a boundary, and there is no peer to name because E24 T7's execution relay was never shipped", c.ID, c.Background.Machine)
		}
		// The ledgers may not carry a semantic, a reaper duty or a redaction site that would amount to a
		// progress-stream claim. The vocabulary is closed by the sweeps, so this asserts the closure holds
		// rather than searching prose.
		if _, _, _, err := uat.SweepBackgroundSemantics(c.Background.SemanticsLedger); err != nil {
			t.Errorf("%s: the semantics ledger no longer sweeps: %v", c.ID, err)
		}
	}
	if proofs != 1 {
		t.Errorf("the bundle carries %d background proof(s), want exactly 1 — a second could ride behind an honest one", proofs)
	}
}
