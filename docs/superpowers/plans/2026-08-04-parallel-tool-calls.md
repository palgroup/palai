# Parallel Tool Calls — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a model asks for several read-only tools in one turn, they run concurrently — so the model's assumption that it fanned out is true, instead of being quietly serialised into N round trips.

**Architecture:** A tool declares whether it is safe to run beside others (`toolbroker.Tool.ParallelSafe`). The orchestrator groups a turn's frames, dispatches the parallel-safe prefix concurrently, and keeps everything else strictly ordered. Results are delivered to the engine in the model's original call order regardless of completion order, and every ledger write keeps today's sequencing.

**Tech Stack:** Go (`apps/control-plane/internal/execution`, `packages/tool-broker`), `errgroup` or a bounded worker set for the fan-out.

## Global Constraints

- **`ReplayClass` is NOT a parallel-safety signal.** It has four values (`pure`, `idempotent`, `reversible`, `irreversible`) and describes *replay* semantics. `ClassReversible` includes the workspace **write** path — a file write is reversible via snapshot but is emphatically not safe to run beside another write to the same tree. Parallel safety needs its own declaration; do not overload this field.
- **Order of delivery is part of the contract.** The engine correlates results by call id, but a model reading its own transcript sees them in the order they arrive. Results must be delivered in the order the model called them, not the order they finished.
- **The durable ledger's sequencing must not change.** Every tool call is committed before its result is delivered; concurrency must not reorder or interleave those writes in a way that breaks the audit chain or the fence.
- **A parallel group is bounded.** An unbounded fan-out over a model-supplied list is a resource-exhaustion surface. Cap it, and make the cap a named constant with a stated reason.
- **Do NOT edit anything under `tests/uat/`.**
- **The working tree is shared and actively committed to by other sessions.** Stage only your own files by explicit path. Never `git add -A`, never `git add .`, never `git commit -- <path> -m`.

---

## §1 — Re-measure before starting

Measured 2026-08-04.

```bash
# Concurrency in dispatch today — expect NOTHING
grep -n "go func\|sync.WaitGroup\|errgroup" apps/control-plane/internal/execution/tool_dispatch.go

# The dispatch entry point — expect it to take ONE frame
grep -n "func (o \*Orchestrator) dispatchTool" apps/control-plane/internal/execution/tool_dispatch.go

# The replay classes — expect four, none of them about concurrency
grep -n "Class[A-Z][a-z]* ReplayClass" packages/tool-broker/broker.go

# Where the engine emits several frames for one turn
grep -n "frame\|yield" engines/reference/src/palai_engine/loop.py | sed -n '1,20p'
```

**Expected:** zero concurrency primitives in dispatch; `dispatchTool(ctx, st, frame)` handles a single frame; four replay classes.

---

## §2 — Why this is a mismatch, not just a missing optimisation

The engine emits N tool-call frames for a turn. The orchestrator hands them to `dispatchTool` one at a time, and there is not a single goroutine in that file. So a model that asks to read three files in one turn — the thing a coding model does constantly — waits three sequential round trips.

The cost is not only latency. The model **plans as though it fanned out**: it batches calls precisely because it expects them to overlap, and then reasons over results that arrived in a sequence it did not intend. Fixing this makes the system's behaviour match the model's model of it.

**Do the Grep/Glob plan first.** Today the only read a model can parallelise is `file` with `op: "read"`. With Grep and Glob landed, a turn like *"glob the tests, grep for the symbol, read the two candidates"* becomes ordinary — and that is the shape this plan pays off on.

---

## §3 — File Structure

| File | Responsibility |
|---|---|
| `packages/tool-broker/broker.go` (modify) | `Tool.ParallelSafe` — the declaration, defaulting to false. |
| `apps/control-plane/internal/execution/tools/*.go` (modify) | Each read-only tool declares itself parallel-safe. |
| `apps/control-plane/internal/execution/tool_dispatch.go` (modify) | Group a turn's frames, fan out the safe ones, preserve delivery order. |
| `apps/control-plane/internal/execution/tool_dispatch_test.go` (modify) | Concurrency, ordering, and the serial-fallback guards. |

---

## Task 1: A tool declares whether it may run beside another

**Files:**
- Modify: `packages/tool-broker/broker.go` (the `Tool` struct)
- Modify: the read-only tools under `apps/control-plane/internal/execution/tools/`
- Test: `packages/tool-broker/broker_test.go` and the tools package's tests

**Interfaces:**
- Consumes: nothing
- Produces: `toolbroker.Tool.ParallelSafe bool`, set on the read-only tools

- [ ] **Step 1: Write the failing test**

```go
// TestOnlyReadOnlyToolsAreParallelSafe pins the classification by NAMING the safe set rather than
// deriving it, because the derivation people reach for first is wrong: ReplayClass is about replay,
// and ClassReversible includes the workspace WRITE path. A write is reversible via snapshot and is
// still not safe to run beside another write.
func TestOnlyReadOnlyToolsAreParallelSafe(t *testing.T) {
	safe := map[string]bool{
		"palai.workspace.file": false, // read AND write behind one `op` — not safe as a whole
		// ... fill in from the registered set: the search tools and pure lookups are true,
		// anything that writes, commits, publishes, or runs a command is false
	}
	for _, tool := range /* the registered tool set */ {
		want, named := safe[tool.Name]
		if !named {
			t.Errorf("tool %q is not classified; a new tool must be judged, not defaulted", tool.Name)
			continue
		}
		if tool.ParallelSafe != want {
			t.Errorf("tool %q: ParallelSafe = %v, want %v", tool.Name, tool.ParallelSafe, want)
		}
	}
}

// TestParallelSafeDefaultsToFalse — the zero value must be the safe one. A tool added without
// thinking about concurrency must serialise, not fan out.
func TestParallelSafeDefaultsToFalse(t *testing.T) {
	var tool toolbroker.Tool
	if tool.ParallelSafe {
		t.Fatal("the zero value is parallel-safe; an unconsidered tool would fan out")
	}
}
```

**Note on `palai.workspace.file`:** it multiplexes read and write behind one `op` parameter, so the *tool* cannot be marked safe even though its read path would be. Record that in the test's comment — it is a concrete argument for the separate Read/Edit tools the text-editor plan introduces, and the classification should be revisited once those land.

- [ ] **Step 2: Run test to verify it fails**

Expected: compile failure — `ParallelSafe` undefined.

- [ ] **Step 3: Implement**

```go
// ParallelSafe marks a tool that may run CONCURRENTLY with other parallel-safe tools in the same
// turn. It is a separate field from ReplayClass on purpose: that one describes what happens on
// REPLAY, and its `reversible` value covers the workspace write path — reversible via snapshot, and
// still unsafe to run beside another write to the same tree. Conflating them would fan out writes.
//
// THE ZERO VALUE IS SERIAL, which is the property that matters when someone adds a tool without
// reading this comment.
ParallelSafe bool
```

Then set it on the read-only tools. Judge each one; do not pattern-match on the name.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./packages/tool-broker/... ./apps/control-plane/internal/execution/tools/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git status --porcelain packages/tool-broker apps/control-plane/internal/execution/tools
git add <the files and their tests>
git commit -m "feat(tools): a tool declares whether it may run beside another"
```

---

## Task 2: Dispatch a turn's parallel-safe frames concurrently

**Files:**
- Modify: `apps/control-plane/internal/execution/tool_dispatch.go`
- Test: `apps/control-plane/internal/execution/tool_dispatch_test.go`

**Interfaces:**
- Consumes: `Tool.ParallelSafe` from Task 1
- Produces: a turn-level dispatch that fans out safe frames and serialises the rest

- [ ] **Step 1: Find where frames become dispatch calls**

```bash
grep -rn "dispatchTool(" apps/control-plane/internal/execution/*.go | grep -v _test
```

Read the caller. **The grouping belongs there**, at the level that sees a whole turn — `dispatchTool` itself handles one frame and should keep doing so.

- [ ] **Step 2: Write the failing tests**

```go
// TestParallelSafeFramesRunConcurrently is the property this plan exists for. It must observe real
// overlap, not just a faster total: a test that only asserts elapsed time passes on a fast serial
// machine and fails on a slow parallel one.
func TestParallelSafeFramesRunConcurrently(t *testing.T) {
	// Use a tool fake whose Exec blocks on a barrier: each call registers its arrival and waits for
	// N arrivals. If dispatch is serial the barrier never opens and the test deadlocks (bound it with
	// a context deadline so a regression FAILS rather than hangs).
}

// TestAnUnsafeFrameSerialisesTheWholeTurn — one write in a turn must not overlap with the reads
// around it. The conservative rule is the correct one here: a mixed turn runs in order.
func TestAnUnsafeFrameSerialisesTheWholeTurn(t *testing.T) { /* ... */ }

// TestResultsAreDeliveredInCallOrder — concurrency changes completion order and must NOT change
// delivery order. The model reads its own transcript; a fast second call arriving before a slow
// first one rewrites what the model thinks it asked.
func TestResultsAreDeliveredInCallOrder(t *testing.T) {
	// Make the FIRST call the slowest and assert its result is still delivered first.
}

// TestTheFanOutIsBounded — the frame list comes from the model. An unbounded fan-out over
// model-supplied input is a resource-exhaustion surface.
func TestTheFanOutIsBounded(t *testing.T) { /* assert no more than maxParallelTools run at once */ }

// TestOneFailureDoesNotCancelItsSiblings — a refusal is an ANSWER in this tree (toolbroker.Answer),
// so a sibling that refused must not take down calls that would have succeeded. Each result stands
// on its own.
func TestOneFailureDoesNotCancelItsSiblings(t *testing.T) { /* ... */ }
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./apps/control-plane/internal/execution/ -run Parallel -v`
Expected: the concurrency test times out or fails; the others may pass trivially on serial code — that is expected, and Step 6 proves they can fail.

- [ ] **Step 4: Implement**

At the turn level:
1. Partition the frames into maximal runs of parallel-safe calls.
2. Run each such run concurrently, bounded by a named constant:

```go
// maxParallelTools bounds one turn's fan-out. The list comes from the MODEL, so an unbounded loop
// here is a resource-exhaustion surface reachable from a prompt. The number is a starting point, not
// a measurement — raise it when a real turn is observed hitting it.
const maxParallelTools = 8
```
3. Everything else keeps today's sequential path, unchanged.
4. Collect results and deliver them **in call order**.

**Leave the ledger write where it is.** Each call's durable pre-write must still happen before its result is delivered. If that write is not already safe to perform from several goroutines, serialise the writes and parallelise only the `Exec` — and say so in your report.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./apps/control-plane/internal/execution/... -v`
Expected: all PASS, including every pre-existing dispatch test.

- [ ] **Step 6: Prove each guard bites**

Three perturbations, each restored afterwards:
1. Force the serial path for every frame → the concurrency test FAILS.
2. Deliver results in completion order → the call-order test FAILS.
3. Remove the bound → the bounded test FAILS.

**If any stays GREEN, that guard is not reaching the behaviour it names** — fix the test before proceeding.

- [ ] **Step 7: Run under the race detector**

```bash
go test ./apps/control-plane/internal/execution/ -race -count=1
```
Expected: PASS with no race reports. **This is the one place in this plan where `-race` is not optional** — the change introduces shared state across goroutines for the first time in this file.

- [ ] **Step 8: Commit**

```bash
git status --porcelain apps/control-plane/internal/execution/
git add apps/control-plane/internal/execution/tool_dispatch.go apps/control-plane/internal/execution/tool_dispatch_test.go
git commit -m "feat(execution): a turn's read-only tools run concurrently, delivered in call order"
```

---

## Task 3: Tell the model it may fan out

A model batches calls only when it believes they overlap. Making the system parallel without saying so leaves the behaviour unused on the turns that would benefit most.

**Files:**
- Modify: `apps/control-plane/internal/execution/platform_instructions.go` **if the platform-instruction-layer plan has landed**; otherwise the parallel-safe tools' own `Description` fields
- Test: the corresponding test file

**Interfaces:**
- Consumes: Tasks 1-2

- [ ] **Step 1: Check which surface exists**

```bash
ls apps/control-plane/internal/execution/platform_instructions.go 2>/dev/null && echo "platform layer present" || echo "use tool descriptions"
```

- [ ] **Step 2: Write the sentence**

If the platform layer exists, add one line to it — in that file's voice, naming no tool:

> When several independent lookups would answer the same question, ask for them together in one turn rather than one at a time.

If it does not, add an equivalent clause to each parallel-safe tool's `Description`. Do **not** create a third instruction surface for this.

- [ ] **Step 3: Assert it**

Extend the relevant existing guard (the platform text's word-count/no-tool-name test, or the tool-description tests) so the guidance cannot silently disappear.

- [ ] **Step 4: Run and commit**

```bash
go test ./apps/control-plane/internal/execution/... -v
git status --porcelain apps/control-plane/internal/execution/
git add <the file and its test>
git commit -m "docs(execution): tell the model that independent lookups may be batched"
```

---

## §5 — Definition of done

- [ ] `Tool.ParallelSafe` exists, **defaults to false**, and every registered tool is explicitly classified (an unclassified tool fails the guard)
- [ ] A turn's parallel-safe frames run concurrently, observed by a **barrier**, not by elapsed time
- [ ] A turn containing one unsafe frame runs fully serially
- [ ] Results are delivered in **call order** regardless of completion order
- [ ] The fan-out is bounded by a named constant with a stated reason
- [ ] One refusal does not cancel its siblings
- [ ] All three Task 2 perturbations were observed RED and restored
- [ ] `go test ./apps/control-plane/internal/execution/ -race` passes
- [ ] `go vet -tags="component live security" ./...` exits 0

---

## §6 — What this plan does NOT do

- **It does not parallelise across turns.** One turn's frames overlap; the turn boundary stays a barrier.
- **It does not make `palai.workspace.file` parallel-safe.** It multiplexes read and write behind one `op`, so the tool cannot be classified safe as a whole. The text-editor plan splits that surface — revisit the classification once it lands.
- **It does not change the durable ledger's sequencing.** If pre-writes cannot be performed concurrently, they stay serial and only `Exec` fans out.
- **It does not measure the speedup.** The guard is overlap, not elapsed time, on purpose: a timing assertion is flaky on a loaded machine and passes trivially on a fast one.
