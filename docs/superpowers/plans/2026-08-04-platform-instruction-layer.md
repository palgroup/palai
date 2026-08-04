# Platform Instruction Layer — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every run carries platform-authored working discipline — how to explore, when to verify, what not to build — instead of the 27 words of protocol text that is all a model gets today.

**Architecture:** A new §25.12 **layer 1 (platform)** in `apps/control-plane/internal/execution/instructions.go`, resolved ahead of the agent-revision and run layers and injected by the same `applyInstructionLayers` seam. The engine's `KERNEL_INSTRUCTION` stays exactly as it is — it is the engine's own identity, not the platform's discipline, and a second engine would carry a different one.

**Tech Stack:** Go (`apps/control-plane/internal/execution`), the existing `modelbroker.Message` system-turn injection.

## Global Constraints

- **The text is platform-authored, never tenant-supplied.** Same rule `toolbroker.Tool.Description` already carries: a tenant string bound to this layer would be an untrusted claim in an operator-authority position.
- **Do not restate tool schemas or tool descriptions in the prompt.** A tool's description lives on the tool (`toolbroker.Tool.Description`) and reaches the model through the advertisement seam. Duplicating it here creates two copies that drift — the defect this tree has paid for repeatedly.
- **Do not copy Anthropic's leaked Claude Code system prompt.** Measured 2026-08-04: Anthropic never published it, an npm packaging error leaked it, and Anthropic filed 8,100+ DMCA notices; their Commercial Terms D.4 forbids using the Services "to build a competing product" and "reverse engineer or duplicate". Design *patterns* are not copyrightable and are free to apply — the wording must be ours. Legitimate sources to draw patterns from: `anthropics/claude-code-action` (**MIT**), and Anthropic's public "writing tools for agents" / "building effective agents" engineering posts.
- **Layer order is load-bearing.** Platform text must sit **after** the engine's kernel turn and **before** the agent revision's, so a revision can narrow platform guidance and a run can narrow the revision's. Reversing that lets a tenant string override platform safety text.
- **Do NOT edit anything under `tests/uat/`.**
- **The working tree is shared and actively committed to by other sessions.** Stage only your own files by explicit path. Never `git add -A`, never `git add .`, never `git commit -- <path> -m`.

---

## §1 — Re-measure before starting

Measured 2026-08-04.

```bash
# The whole prompt a model gets today — expect 2 sentences, 27 words, all protocol
sed -n '13,16p' engines/reference/src/palai_engine/context.py

# Where the engine puts it — expect it as messages[0], role system
grep -n "KERNEL_INSTRUCTION" engines/reference/src/palai_engine/context.py

# The layers that HAVE a writer today — expect agent_revision and run only
grep -n "layerInstructions" apps/control-plane/internal/execution/instructions.go

# The injection seam — expect it to insert after any leading system messages
grep -n -A20 "func applyInstructionLayers" apps/control-plane/internal/execution/instructions.go
```

**Expected:** `KERNEL_INSTRUCTION` is *"You are the Palai reference engine. Follow protocol and safety instructions; propose tool calls and produce final output, but never control lifecycle state."*; two layer constants (`agent_revision`, `run`); `applyInstructionLayers` skips leading system turns and inserts after them.

---

## §2 — What is actually missing

The 27 words say what the engine *is* and what it may not *control*. They say nothing about how to work. A model running in this platform is told nothing about:

- **Exploration** — search before reading, read before editing, prefer a narrow search to a broad file read
- **Verification** — run the test that covers the change; report a failure with its output rather than a summary
- **Restraint** — do not add abstractions, error handling for impossible states, or files nobody asked for
- **Reporting** — say what happened before saying how; state plainly when something is incomplete
- **When to stop** — what to do when a tool refuses, and when to ask rather than guess

`instructions.go` already anticipated this. Its own comment says layers 1, 2 and 4 are named so that "the next task that adds one has an obvious insertion point and an obvious ORDER to insert it at". This plan is that task, for layer 1.

**Why not extend `KERNEL_INSTRUCTION` instead:** that string is the *engine's* identity — this tree's architecture decision was an own-engine design where a second engine is expected. Discipline that must hold for every engine belongs on the control plane, which is the only thing all engines share.

---

## §3 — File Structure

| File | Responsibility |
|---|---|
| `apps/control-plane/internal/execution/instructions.go` (modify) | The `platform` layer constant, its position in the order, and its resolution. |
| `apps/control-plane/internal/execution/platform_instructions.go` (**new**) | The text itself, as a single exported constant with its provenance recorded in the file's doc comment. |
| `apps/control-plane/internal/execution/instructions_test.go` (modify) | Order, presence, and the guards below. |

---

## Task 1: The platform layer exists and sits in the right place

**Files:**
- Modify: `apps/control-plane/internal/execution/instructions.go`
- Test: `apps/control-plane/internal/execution/instructions_test.go`

**Interfaces:**
- Consumes: nothing
- Produces: `layerInstructionsPlatform = "platform"`, resolved first in the layer slice

- [x] **Step 1: Read the resolution function and its tests**

```bash
grep -n "layerInstructionsRevision\|layerInstructionsRun" apps/control-plane/internal/execution/*.go
grep -n "func Test" apps/control-plane/internal/execution/instructions_test.go
```

Find the function that builds `[]InstructionLayer` and the tests that pin today's order. **Match their style.**

- [x] **Step 2: Write the failing test**

```go
// TestThePlatformLayerLeadsAndTenantTextFollows pins the ONE ordering property that matters for
// authority: platform discipline is stated first, and a revision or run may narrow it by speaking
// after it. Reversed, a tenant-supplied string would sit in the position that overrides platform
// safety text — which is the same untrusted-claim defect the tool description rule exists to prevent.
func TestThePlatformLayerLeadsAndTenantTextFollows(t *testing.T) {
	layers := /* call the resolver with a revision instruction AND a run instruction set */
	if len(layers) < 3 {
		t.Fatalf("got %d layers, want platform + revision + run", len(layers))
	}
	if layers[0].Layer != layerInstructionsPlatform {
		t.Errorf("layer 0 is %q, want the platform layer to lead", layers[0].Layer)
	}
	if layers[0].Text == "" {
		t.Error("the platform layer resolved to empty text; a layer that says nothing is not a layer")
	}
	for i, l := range layers[1:] {
		if l.Layer == layerInstructionsPlatform {
			t.Errorf("the platform layer appears again at position %d", i+1)
		}
	}
}

// TestThePlatformLayerIsPresentWithNoTenantText — the platform layer is unconditional. A run with no
// revision instruction and no run instruction still carries it, because working discipline is not
// something a tenant opts into.
func TestThePlatformLayerIsPresentWithNoTenantText(t *testing.T) {
	layers := /* call the resolver with NO revision and NO run instructions */
	if len(layers) != 1 || layers[0].Layer != layerInstructionsPlatform {
		t.Fatalf("got %v, want exactly the platform layer", layers)
	}
}
```

- [x] **Step 3: Run test to verify it fails**

Run: `go test ./apps/control-plane/internal/execution/ -run Platform -v`
Expected: compile failure — `layerInstructionsPlatform` undefined.

- [x] **Step 4: Implement**

Add the constant beside its siblings and prepend the layer during resolution. Update the file's header comment, which currently lists layers 1, 2 and 4 as having **no writer** — layer 1 now has one, and leaving that sentence unchanged would make it false the moment this lands.

- [x] **Step 5: Run test to verify it passes**

Run: `go test ./apps/control-plane/internal/execution/ -run Instruction -v`
Expected: all PASS, including the pre-existing ordering tests.

- [x] **Step 6: Commit**

```bash
git status --porcelain apps/control-plane/internal/execution/
git add apps/control-plane/internal/execution/instructions.go apps/control-plane/internal/execution/instructions_test.go
git commit -m "feat(execution): §25.12 layer 1 exists and leads the instruction stack"
```

---

## Task 2: The text

**Files:**
- Create: `apps/control-plane/internal/execution/platform_instructions.go`
- Test: `apps/control-plane/internal/execution/platform_instructions_test.go`

**Interfaces:**
- Consumes: Task 1's layer
- Produces: `platformInstructions` — the constant Task 1's resolver returns

- [x] **Step 1: Write the guards first**

These are the tests that keep the text honest as it is edited over time:

```go
// TestThePlatformTextNamesNoTool is the anti-duplication guard. A tool's description lives on the
// tool and reaches the model through the advertisement seam; naming one here creates a second copy
// that drifts the day the tool changes. Guidance may describe WHAT to do ("search before reading")
// and must not name WHICH tool does it.
func TestThePlatformTextNamesNoTool(t *testing.T) {
	for _, name := range toolset.All() {
		if strings.Contains(platformInstructions, name) {
			t.Errorf("the platform text names the tool %q; tool text belongs on the tool", name)
		}
	}
	if strings.Contains(platformInstructions, "palai.") {
		t.Error("the platform text carries a palai.* identifier")
	}
}

// TestThePlatformTextIsSubstantial — the failure this whole plan exists to fix was a 27-word prompt.
// A lower bound is a crude guard, but it is the one that would have caught the state we shipped for
// months, and it costs nothing.
func TestThePlatformTextIsSubstantial(t *testing.T) {
	if words := len(strings.Fields(platformInstructions)); words < 150 {
		t.Errorf("the platform text is %d words; the state this replaced was 27", words)
	}
}
```

- [x] **Step 2: Run tests to verify they fail**

Expected: compile failure — `platformInstructions` undefined.

- [x] **Step 3: Write the text**

Create the file. Its doc comment must record **where the patterns came from and where they did not**:

```go
// Package-level: platformInstructions is §25.12 layer 1 — the working discipline every run carries,
// authored by the platform and narrowable by a revision or a run speaking after it.
//
// PROVENANCE, because this is the kind of text whose origin gets asked about: the patterns here are
// drawn from Anthropic's public engineering writing on agent and tool design and from the MIT-licensed
// anthropics/claude-code-action, and the wording is ours. Nothing here is copied from the Claude Code
// system prompt, which Anthropic never published — it leaked through an npm packaging error in March
// 2026 and Anthropic filed 8,100+ DMCA notices over it. Patterns are not copyrightable; text is.
```

Then write the discipline. Cover, in your own words, at minimum:

1. **Exploration** — search for a symbol before reading a file whole; read the part you need rather than the whole file; a failed search is an answer, so narrow and retry rather than giving up.
2. **Editing** — read before changing; change the lines the task needs and leave the rest; match the surrounding code's conventions rather than importing your own.
3. **Restraint** — no abstraction for a single caller, no error handling for states that cannot occur, no files nobody asked for. If a simpler approach exists, say so.
4. **Verification** — run the check that covers what you changed; report a failure with its actual output; never call something done that you have not observed working.
5. **Reporting** — lead with what happened; state plainly what is incomplete and why; do not narrate steps the reader can already see.
6. **When to stop** — a tool refusal is information, not a wall: read it and adjust. Ask when two readings of the task would produce materially different work; otherwise decide and say what you decided.

Write it as instructions to a capable colleague, not as rules for a machine. Avoid `CRITICAL:`/`YOU MUST` framing — Anthropic's own migration guidance is that emphatic phrasing overtriggers on current models, and prompts written to overcome older models' reluctance now misfire.

- [x] **Step 4: Run tests to verify they pass**

Run: `go test ./apps/control-plane/internal/execution/ -run Platform -v`
Expected: PASS.

- [x] **Step 5: Commit**

```bash
git status --porcelain apps/control-plane/internal/execution/
git add apps/control-plane/internal/execution/platform_instructions.go apps/control-plane/internal/execution/platform_instructions_test.go
git commit -m "feat(execution): platform working discipline, in our own words"
```

---

## Task 3: Prove it reaches the model

Tasks 1 and 2 prove a layer exists and carries text. Neither proves a **model request** contains it — the difference between a route existing and a field being accepted, which this tree has paid for more than once.

**Files:**
- Test: `apps/control-plane/internal/execution/instructions_test.go` (or the dispatch test file — find where model requests are asserted)

**Interfaces:**
- Consumes: everything above

- [x] **Step 1: Find where a built model request is asserted**

```bash
grep -rn "applyInstructionLayers\|Messages\[0\]" apps/control-plane/internal/execution/*_test.go | head
```

Find the test that inspects the messages a dispatch actually sends. Extend that path rather than building a parallel harness.

- [x] **Step 2: Write the failing test**

```go
// TestThePlatformTextReachesTheModelRequest is the end this plan exists for. Everything above proves
// a layer resolves; only this proves the bytes are in the request. It also pins the ORDER on the wire:
// the engine's kernel turn first, then platform, then anything a revision or run added.
func TestThePlatformTextReachesTheModelRequest(t *testing.T) {
	msgs := /* build the messages a dispatch sends, with a revision instruction present */
	var systemTurns []string
	for _, m := range msgs {
		if m.Role == "system" {
			systemTurns = append(systemTurns, m.Content)
		}
	}
	if len(systemTurns) < 2 {
		t.Fatalf("got %d system turns, want the kernel turn plus the platform layer", len(systemTurns))
	}
	platformAt := slices.IndexFunc(systemTurns, func(s string) bool { return s == platformInstructions })
	if platformAt < 0 {
		t.Fatal("the platform text is in no system turn: the layer resolves but nothing sends it")
	}
	revisionAt := slices.IndexFunc(systemTurns, func(s string) bool { return strings.Contains(s, "<the revision instruction fixture>") })
	if revisionAt >= 0 && revisionAt < platformAt {
		t.Error("a revision instruction precedes the platform text; tenant text must not lead")
	}
}
```

- [x] **Step 3: Run test to verify it fails**

Expected: FAIL if the resolver is not wired into the dispatch path — which is precisely the failure worth catching.

- [x] **Step 4: Make it pass**

If it already passes because Task 1 wired the resolver into an existing call site, say so in your report and move to Step 5 — a test that passes on arrival is fine **once you have proven it can fail** (Step 5).

- [x] **Step 5: Perturb**

Remove the platform layer from the resolver's output and re-run.
Expected: the test FAILS naming the missing text.
Restore and confirm green.

- [x] **Step 6: Run the package and vet**

```bash
go test ./apps/control-plane/internal/execution/... -v
go vet -tags="component live security" ./...
```
Expected: PASS, vet exits 0.

- [x] **Step 7: Commit**

```bash
git status --porcelain apps/control-plane/internal/execution/
git add <the test file>
git commit -m "test(execution): the platform layer's bytes reach the model request, in order"
```

---

## §4.5 — Execution record (2026-08-04)

**Shipped in 2 commits: `b1d17575` (layer + text) and `ea637cf0` (the seam test).**

Tasks 1 and 2 landed together rather than separately, because they cannot be split as written: Task 1's own test asserts `layers[0].Text != ""`, so the layer does not compile green without the text Task 2 supplies. The plan's task boundary was wrong; the work was not.

**Where it went:** layer 1 now has two writers, and the file's header says so. The engine keeps `KERNEL_INSTRUCTION` (its identity and protocol rules); the control plane adds `platformInstructions` (how to work). The split is along the line that decides ownership — a second engine carries its own identity and still needs the same discipline.

**Perturbations observed RED, then restored:**
- A tool name added to the text → `TestThePlatformTextNamesNoTool` failed naming the `palai.*` identifier.
- The platform layer removed from the resolver → **two** guards failed: the seam test (`got 2 system turns, want the kernel turn + platform + revision`) and the unconditional-presence test (`got 0 layers`).

**Measured after:** `apps/control-plane/internal/execution` and `.../tools` both `ok`. `go vet -tags="component live security" ./...` exits 1 with 10 errors, **zero of them in files this plan touched** — they are the concurrent org/tenancy refactor's tagged callers, down from 17 earlier the same day.

---

## §5 — Definition of done

- [x] A `platform` layer resolves **unconditionally**, leads the layer slice, and appears exactly once
- [x] The text names no tool and carries no `palai.*` identifier
- [x] The text's provenance is recorded in its file's doc comment, naming the MIT/public sources and stating explicitly that nothing is copied from the leaked prompt
- [x] A built model request carries the text, after the engine's kernel turn and before any revision text
- [x] The Task 3 perturbation was observed RED and restored
- [x] `go vet -tags="component live security" ./...` exits 0

---

## §6 — What this plan does NOT do

- **It does not touch `KERNEL_INSTRUCTION`.** That is the engine's identity; a second engine would carry a different one.
- **It does not add layers 2 or 4** (project and session instructions). Both still have no writer, and `instructions.go` names them for exactly that reason. Adding one is a schema change plus an API surface — separate work.
- **It does not measure the text's effect.** Prompt changes want an eval harness, and this tree has one (`E17 T6`). Wiring a prompt eval is worth doing and is not in scope here.
- **It does not use the Agent SDK's `claude_code` preset.** That preset is the legitimate way to get Claude Code's actual prompt, but it is reachable only through the Python/TypeScript SDK driving the `claude` binary — not from this Go control plane. If palai ever runs agents through that SDK, the preset is the right answer there and this layer would be what you `append` to it.
