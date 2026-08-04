# Anthropic Text Editor Tool — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A model editing a palai workspace changes the lines it means to change, instead of regenerating whole files — by adopting Anthropic's client-side `text_editor_20250728` tool, whose schema and usage are trained into the model and whose `str_replace` requires exactly one match.

**Architecture:** Anthropic-defined tools are declared by `type` and `name` with **no** `input_schema` — the shape lives in the model. Palai's tool plumbing has no concept of a tool type: `toolbroker.Tool` has no `Type` field, `model-broker.ToolSchema` has none, and `provider_two/adapter.go:374` hard-codes every tool into the custom-tool shape `{name, input_schema}`. So the type must be threaded end-to-end before the tool can exist. The tool itself executes locally against `ExecEnv.Workspace`, exactly like every other coding tool.

**Tech Stack:** Go (single module `github.com/palgroup/palai`), Anthropic Messages API via `adapters/models/provider_two`.

## Global Constraints

- **`str_replace_based_edit_tool` is a literal.** That exact string is what the model was trained on. `provider_two`'s `wireToolName` (adapter.go:455) rewrites anything outside `[A-Za-z0-9_-]` to `_` and truncates at 64 — this name survives it unchanged, but **assert that** rather than assume it.
- **An Anthropic-defined tool must NOT carry `input_schema`.** Sending one alongside `type` is a different request shape than the model was trained on. The adapter must omit it, not send an empty map.
- **`provider_one` is OpenAI-shaped** (`adapter.go:27` → `https://api.openai.com/v1/chat/completions`). It cannot carry Anthropic-defined tools at all. A run routed there must **fail loudly at advertisement time**, never silently drop the tool and leave the model with no editor.
- **The workspace may be remote.** The tool acts through `ExecEnv.Workspace` (`toolbroker.WorkspaceOps`), never `os` calls on `WorkspaceRoot`. A nil `Workspace` answers `unavailable` — see `tools/file.go:57-62`.
- **`undo_edit` was removed from this tool version.** Do not implement it. The four commands are `view`, `create`, `str_replace`, `insert`.
- **Do NOT edit anything under `tests/uat/`.**
- **The working tree is shared and actively committed to by other sessions.** Stage only your own files by explicit path. Never `git add -A`, never `git add .`, never `git commit -- <path> -m`.

---

## §1 — Re-measure before starting

Measured 2026-08-04.

```bash
# The struct that must gain a Type — expect Name/Description/InputSchema/OutputSchema/ReplayClass/...
grep -n -A20 "^type Tool struct" packages/tool-broker/broker.go

# The wire type the adapter serialises — expect Name/Description/Parameters/Strict, no Type
sed -n '54,60p' packages/model-broker/types.go

# The hard-coded custom-tool shape — expect `{"name": wire, "input_schema": t.Parameters}`
sed -n '364,383p' adapters/models/provider_two/adapter.go

# Where a name is rewritten for the wire
sed -n '455,470p' adapters/models/provider_two/adapter.go

# Confirm provider_one is OpenAI-shaped
sed -n '27p' adapters/models/provider_one/adapter.go
```

**Expected:** `toolbroker.Tool` has 9 fields and no `Type`; `ToolSchema` has 4 and no `Type`; the adapter builds `{name, input_schema}` and conditionally adds `description`/`strict`; `wireToolName` sanitises to `[A-Za-z0-9_-]` and truncates at 64; `provider_one` targets `api.openai.com`.

---

## §2 — Why this is worth an adapter change

Today the only way a model writes a file is `palai.workspace.file` with `op: "write"`, which replaces the **whole file** (`tools/file.go:105-127`). Changing one line means regenerating every other line — every one of them a fresh opportunity to drop a function, reorder an import, or quietly alter something the model was not asked to touch. There is also no staleness check: nothing notices that the file changed since the model last read it.

`text_editor_20250728` replaces that with `str_replace`, whose contract the model already knows: the old string must appear **exactly once**; zero or several matches is an error, not a guess. That single rule is most of what makes Claude Code's editing reliable, and it arrives with the tool rather than having to be taught in a prompt.

**This plan is second of the two tool plans.** Editing without searching is blind editing — do the Grep/Glob plan first.

---

## §3 — File Structure

| File | Responsibility |
|---|---|
| `packages/tool-broker/broker.go` (modify) | `Tool.Type` — the Anthropic tool type, empty for an ordinary custom tool. |
| `packages/model-broker/types.go` (modify) | `ToolSchema.Type` — the same value, on the wire-facing struct. |
| The advertisement seam (modify) | Wherever `toolbroker.Tool` becomes `model-broker.ToolSchema` — carry `Type` across. **Find it in Task 1 Step 1.** |
| `adapters/models/provider_two/adapter.go` (modify) | Branch: a typed tool serialises as `{type, name}`; a custom tool keeps today's `{name, input_schema, …}`. |
| `adapters/models/provider_one/adapter.go` (modify) | Refuse a typed tool with a named error instead of dropping it. |
| `apps/control-plane/internal/execution/tools/text_editor.go` (**new**) | The four commands, executed against `ExecEnv.Workspace`. |

---

## Task 1: Thread a tool type end to end

**Files:**
- Modify: `packages/tool-broker/broker.go` (the `Tool` struct)
- Modify: `packages/model-broker/types.go` (`ToolSchema`)
- Modify: the advertisement seam that converts between them
- Test: the existing test files for each

**Interfaces:**
- Consumes: nothing
- Produces: `toolbroker.Tool.Type string` and `modelbroker.ToolSchema.Type string`, carried across the advertisement seam

- [ ] **Step 1: Find the advertisement seam**

Run:

```bash
grep -rn "ToolSchema{" --include="*.go" . | grep -v _test
```

One of those sites converts registered `toolbroker.Tool`s into the `ToolSchema`s a request carries. Read it. **That conversion is the seam this task widens** — a `Type` added to both structs but dropped in between is exactly the "declared but not wired" defect this tree keeps paying for.

- [ ] **Step 2: Write the failing test**

Write it at the **seam**, not on the structs — a test that only checks two struct literals proves nothing about the conversion:

```go
// TestTheToolTypeReachesTheWire is the guard against the failure this tree names most often: a field
// added at both ends and dropped in the middle. An Anthropic-defined tool is IDENTIFIED by its type,
// so a conversion that loses it turns the tool into an unknown custom tool with no schema.
func TestTheToolTypeReachesTheWire(t *testing.T) {
	// ... build a registered tool with Type set, run it through the advertisement seam,
	// and assert the resulting ToolSchema carries the same Type.
	// Also assert an ordinary custom tool still arrives with Type == "".
}
```

Fill in the construction from the seam you found in Step 1 — use its real types, do not invent a fixture.

- [ ] **Step 3: Run test to verify it fails**

Expected: compile failure — `Type` undefined on one or both structs.

- [ ] **Step 4: Add the field to both structs and carry it across**

On `toolbroker.Tool`:

```go
// Type is the Anthropic-defined tool type (e.g. "text_editor_20250728"), empty for an ordinary
// custom tool. A typed tool's SHAPE LIVES IN THE MODEL rather than in InputSchema: the provider is
// sent {type, name} and no schema, because the schema it was trained on is not ours to restate.
// A tool carrying a Type must therefore leave InputSchema empty — Task 4's tool asserts that.
Type string
```

Mirror it on `ToolSchema` with `json:"type,omitempty"` — **`omitempty` is load-bearing**: an ordinary custom tool must not gain a `"type": ""` key it never had.

- [ ] **Step 5: Run test to verify it passes**

Run the seam's package tests plus `go build ./...`.
Expected: PASS, build clean.

- [ ] **Step 6: Commit**

```bash
git status --porcelain packages/tool-broker packages/model-broker <the seam's dir>
git add <the three files and their tests>
git commit -m "feat(tools): a tool can declare an Anthropic-defined type, and it reaches the wire"
```

---

## Task 2: `provider_two` serialises a typed tool correctly

**Files:**
- Modify: `adapters/models/provider_two/adapter.go` (the tool loop at ~364-383)
- Test: `adapters/models/provider_two/adapter_test.go`

**Interfaces:**
- Consumes: `ToolSchema.Type` from Task 1
- Produces: request bodies where a typed tool is `{type, name}` and a custom tool is unchanged

- [ ] **Step 1: Write the failing test**

Assert against the **built request body**, following the existing adapter tests' style:

1. A tool with `Type: "text_editor_20250728"`, `Name: "str_replace_based_edit_tool"` serialises to exactly `{"type": "text_editor_20250728", "name": "str_replace_based_edit_tool"}` — **no `input_schema` key at all**, not an empty one.
2. An ordinary custom tool's serialisation is **byte-identical to today's** — this change must not perturb the shape every existing tool relies on.
3. `str_replace_based_edit_tool` survives `wireToolName` **unchanged**. Assert the literal; the name is what the model was trained on and a silent rewrite would be undetectable at runtime.
4. A typed tool that also carries a non-empty `Parameters` is a **programming error** — assert the adapter returns an error naming the tool rather than sending both.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./adapters/models/provider_two/ -run Tool -v`
Expected: FAIL — the typed tool serialises with `input_schema` and no `type`.

- [ ] **Step 3: Implement the branch**

In the tool loop, before building the map:

```go
// AN ANTHROPIC-DEFINED TOOL IS DECLARED, NOT DESCRIBED. Its type identifies a schema the model
// already carries, so the request sends {type, name} and NO input_schema — sending one would be a
// different request shape than the one the tool's behaviour was trained on. `description` is
// likewise omitted: the platform does not get to restate what the tool does.
if t.Type != "" {
	if len(t.Parameters) > 0 {
		return nil, nil, fmt.Errorf("tool %q declares both a type (%q) and an input schema; an Anthropic-defined tool carries neither schema nor description", t.Name, t.Type)
	}
	tools = append(tools, map[string]any{"type": t.Type, "name": wire})
	continue
}
```

Keep the existing duplicate-wire-name check applying to typed tools too.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./adapters/models/provider_two/ -v`
Expected: all PASS, including the pre-existing adapter tests.

- [ ] **Step 5: Prove the custom-tool path is untouched**

Perturb: make the branch fire for **every** tool (drop the `t.Type != ""` condition) and re-run.
Expected: the custom-tool byte-identity test FAILS.
Restore and confirm green. This proves the test actually pins today's shape rather than accepting anything.

- [ ] **Step 6: Commit**

```bash
git status --porcelain adapters/models/provider_two/
git add adapters/models/provider_two/adapter.go adapters/models/provider_two/adapter_test.go
git commit -m "feat(provider_two): an Anthropic-defined tool is sent as {type,name} with no schema"
```

---

## Task 3: `provider_one` refuses a typed tool out loud

An OpenAI-shaped endpoint has no concept of `text_editor_20250728`. The failure mode to prevent is the quiet one: the tool is dropped, the model is never offered an editor, and the run looks fine while being unable to change a line.

**Files:**
- Modify: `adapters/models/provider_one/adapter.go` (its tool serialisation)
- Test: `adapters/models/provider_one/adapter_test.go`

**Interfaces:**
- Consumes: `ToolSchema.Type` from Task 1
- Produces: a named error when a typed tool is routed to an OpenAI-shaped provider

- [ ] **Step 1: Write the failing test**

Assert that building a request carrying a tool with a non-empty `Type` returns an error whose message names **both** the tool and the reason, and that a request with only custom tools is unaffected.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./adapters/models/provider_one/ -run Tool -v`
Expected: FAIL — no error; the type is silently ignored.

- [ ] **Step 3: Implement the refusal**

```go
// A TYPED TOOL CANNOT CROSS THIS ADAPTER, and it must not be dropped quietly. This provider speaks
// OpenAI's chat/completions shape, where an Anthropic-defined tool type has no meaning. Dropping it
// would leave the model with no editor and no sign that anything was missing — the run would simply
// stop being able to change a line. Failing here names the route as the thing to fix.
if t.Type != "" {
	return nil, nil, fmt.Errorf("tool %q declares the Anthropic-defined type %q, which this OpenAI-compatible provider cannot carry; route this agent to an Anthropic model or drop the tool from its set", t.Name, t.Type)
}
```

Check whether `adapters/models/openai_compatible` needs the same guard — if it shares this serialisation path, one fix covers both; if not, it needs its own. **Measure, do not assume.**

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./adapters/models/... -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git status --porcelain adapters/models/
git add <the files you changed and their tests>
git commit -m "fix(provider_one): a typed tool is refused by name instead of silently dropped"
```

---

## Task 4: The text editor tool

**Files:**
- Create: `apps/control-plane/internal/execution/tools/text_editor.go`
- Test: `apps/control-plane/internal/execution/tools/text_editor_test.go`

**Interfaces:**
- Consumes: `toolbroker.Tool.Type` from Task 1; `ExecEnv.Workspace` (`WorkspaceOps`)
- Produces: tool `Type: "text_editor_20250728"`, `Name: "str_replace_based_edit_tool"`

- [ ] **Step 1: Read the command contract**

The model sends `command` plus command-specific fields:

| `command` | Other inputs | Behaviour |
|---|---|---|
| `view` | `path`, optional `view_range` | Return contents with line numbers, or a directory listing |
| `create` | `path`, `file_text` | Create or overwrite |
| `str_replace` | `path`, `old_str`, `new_str` | Replace **exactly one** occurrence; error on 0 or >1 |
| `insert` | `path`, `insert_line`, `insert_text` | Insert after `insert_line` (0 = start of file) |

`undo_edit` is **not** part of this version. Do not implement it.

- [ ] **Step 2: Write the failing tests**

One per property. The `str_replace` arity rules are the reason this tool exists — test them first:

```go
// TestStrReplaceDemandsExactlyOneMatch is the whole point of adopting this tool. Zero matches and
// several matches are BOTH errors, and the error text must say which — a model that is told "not
// found" widens its string, and one told "3 matches" narrows it. Collapsing them into one message
// makes the model guess.
func TestStrReplaceDemandsExactlyOneMatch(t *testing.T) {
	// zero matches  -> Answer, message says the string was not found
	// two matches   -> Answer, message says how many were found
	// exactly one   -> the file changes, and ONLY at that occurrence
}

// TestViewReturnsLineNumbers — the model reads a numbered view and then sends `old_str` back. If the
// numbering is off or absent, every subsequent str_replace is aimed at the wrong place.
func TestViewReturnsLineNumbers(t *testing.T) { /* ... */ }

// TestViewRangeSelectsLines covers the parameter that keeps a large file out of the context window.
func TestViewRangeSelectsLines(t *testing.T) { /* ... */ }

// TestInsertPlacesTextAfterTheGivenLine, including insert_line 0 meaning the start of the file.
func TestInsertPlacesTextAfterTheGivenLine(t *testing.T) { /* ... */ }

// TestAWorkspacelessEditorAnswersInsteadOfTouchingLocalDisk mirrors file.go's guard: a nil Workspace
// is `unavailable`, never a fallback to this process's own filesystem.
func TestAWorkspacelessEditorAnswersInsteadOfTouchingLocalDisk(t *testing.T) { /* ... */ }

// TestTheEditorDeclaresATypeAndNoSchema pins the shape Task 2's adapter branch depends on.
func TestTheEditorDeclaresATypeAndNoSchema(t *testing.T) {
	tool := TextEditorTool()
	if tool.Type != "text_editor_20250728" {
		t.Errorf("Type = %q", tool.Type)
	}
	if tool.Name != "str_replace_based_edit_tool" {
		t.Errorf("Name = %q — this literal is what the model was trained on", tool.Name)
	}
	if len(tool.InputSchema) != 0 {
		t.Errorf("InputSchema is non-empty; an Anthropic-defined tool carries no schema")
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./apps/control-plane/internal/execution/tools/ -run 'StrReplace|View|Insert|Editor' -v`
Expected: FAIL — `TextEditorTool` undefined.

- [ ] **Step 4: Implement**

Act through `env.Workspace` only. Classification follows `file.go`'s rule, which is already written down there and is worth re-reading before you copy it: **a read that failed changed nothing**, so `view` failures are `toolbroker.Answer`s the model can retry; a write that failed may have landed, so only provably pre-effect failures are answerable.

`str_replace` is a **read-modify-write** through `WorkspaceOps`: read, count occurrences, refuse unless exactly one, write. Count on the raw bytes — no normalisation, no trimming. A single whitespace difference must miss, because that is the contract the model is working to.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./apps/control-plane/internal/execution/tools/ -v`
Expected: all PASS.

- [ ] **Step 6: Prove the arity guard bites**

Perturb: make `str_replace` replace the **first** match instead of demanding uniqueness, and re-run.
Expected: the two-match test FAILS.
Restore and confirm green. A first-match editor is the silent-corruption mode this whole plan exists to avoid.

- [ ] **Step 7: Commit**

```bash
git status --porcelain apps/control-plane/internal/execution/tools/
git add apps/control-plane/internal/execution/tools/text_editor.go apps/control-plane/internal/execution/tools/text_editor_test.go
git commit -m "feat(tools): the Anthropic text editor, whose str_replace demands exactly one match"
```

---

## Task 5: Register it, and decide what happens to `palai.workspace.file`

**Files:**
- Modify: `apps/control-plane/cmd/palai-control-plane/main.go` (`toolbroker.New`)
- Modify: `packages/toolset/toolset.go`
- Modify: `apps/slack-bot/internal/relay/relay.go` (`toolTitles`)
- Modify: `apps/control-plane/internal/execution/tools/file.go` (its `description` only)
- Test: the three cross-cutting guards

**Interfaces:**
- Consumes: everything above
- Produces: `str_replace_based_edit_tool` in the canonical set

- [ ] **Step 1: Measure what still needs `palai.workspace.file`**

The two tools overlap on read and write. Before changing anything, find out what would break if the file tool lost them:

```bash
grep -rn "before_hash\|after_hash\|BeforeHash\|AfterHash" --include="*.go" apps/ packages/ | grep -v _test
```

`file.go`'s write returns the before/after hashes **the changeset consumes**. The text editor returns no such report. Whatever that grep finds is the reason `palai.workspace.file` cannot simply be deleted.

**Record the answer in your report.** It decides Step 3.

- [ ] **Step 2: Register the editor in all three places**

1. `toolbroker.New(...)` — otherwise `TestEveryDefaultToolResolves` fails.
2. `toolset.defaultTools` — editing is not conditional on a repository.
3. `relay.toolTitles` — otherwise `TestEveryCanonicalToolHasAHumanTitle` fails. Suggested phrase: `"Editing a file"`.

**Note the name is not `palai.`-prefixed.** `toolset`'s own test asserts every canonical name starts with `palai.` — this one cannot, because the literal is Anthropic's. Widen that assertion to admit exactly this name, with a comment saying why. Do **not** loosen it to accept any prefix.

- [ ] **Step 3: Narrow the file tool's description**

Both tools will be advertised together, so the model needs to know which to reach for. Rewrite `file.go`'s `Description` (line 27) to say what it is now **for** — listing, stat, checksum, and the hashed write the changeset consumes — and to point at the editor for ordinary reading and editing.

Do not change `file.go`'s behaviour in this task. If Step 1 showed nothing depends on its write path, removing it is a **separate** plan with its own guard.

- [ ] **Step 4: Run every guard**

```bash
go test ./packages/toolset/... ./packages/tool-broker/... ./packages/model-broker/... \
        ./adapters/models/... ./apps/control-plane/internal/execution/... \
        ./apps/slack-bot/internal/relay/... ./cmd/cli/internal/stack/...
go vet -tags="component live security" ./...
```
Expected: all PASS, vet exits 0.

- [ ] **Step 5: Commit**

```bash
git status --porcelain
git add <the registration files and file.go>
git commit -m "feat(tools): advertise the text editor, and say what the file tool is now for"
```

---

## §4.5 — Execution record (2026-08-04)

**Shipped in 7 commits:** `f3439d4f` + `1a…` (type threading and its gofmt), `fe567917` (provider_two branch), `36e321bb` (provider_one refusal), `4b200c78` (the editor), `cf4f2f37` (advertisement + changeset), plus two formatting follow-ups.

**The canonical default set is now 9 names**, one of which is not ours: `str_replace_based_edit_tool`. `toolset_test` admits it through a **named** exception map (`anthropicDefined`), not a loosened prefix rule — an unknown unprefixed name still fails.

### The dependency this plan missed, and the guard that now covers it

Task 5 Step 1 asked "what still needs `palai.workspace.file`" and answered it by grepping for `before_hash`. That found `changeset.go` — and **the field was never the constraint**. The real one was six lines above it:

```go
if row.Name != fileToolName { continue }
if s, _ := args["op"].(string); s != "write" { continue }
```

The changeset walk selected ledger rows **by tool name**. Adopting a second editing tool without touching it would have produced a run that edits files and reports a changeset with no ledger provenance — the files still appeared, via the workspace-scan fallback, but with **empty hashes and no tool-call id**, and nothing failing to say so. The publication path derives from that record.

Fixed by `isWorkspaceWriteRow(name, args)`, which knows that each tool names its writing operations differently — the file tool multiplexes on `op`, the editor on `command` — and that both carry read-only operations (`read`, `view`) that must never be recorded as changes. Two new tests pin both halves.

### Perturbations observed RED, then restored

- **`str_replace` made first-match** (the arity refusal deleted) → the two-match case returned `<nil>` where an answer was required. This is the silent-corruption mode the whole plan exists to prevent.
- **The typed branch fired for every tool** → the custom-tool byte-identity test failed, proving it pins today's shape rather than accepting anything.
- **`Type` dropped in the advertisement seam** → `Type = "", want it carried across the seam`. The test lives at the seam, not on the structs, precisely so this is catchable.

**Measured after:** every affected package `ok`, and `go vet -tags="component live security" ./...` exits **0** with zero errors.

---

## §5 — Definition of done

- [ ] `toolbroker.Tool.Type` and `ToolSchema.Type` exist **and the value survives the advertisement seam** (asserted at the seam, not on the structs)
- [ ] `provider_two` sends `{type, name}` with **no** `input_schema` for a typed tool, and custom tools serialise byte-identically to before
- [ ] `str_replace_based_edit_tool` is asserted to survive `wireToolName` unchanged
- [ ] `provider_one` (and `openai_compatible`, if it shares the path) **errors by name** on a typed tool
- [ ] `str_replace` refuses 0 matches and >1 matches with **different** messages, and the two-match perturbation was observed RED
- [ ] A nil `ExecEnv.Workspace` answers `unavailable` and touches no local disk
- [ ] All three cross-cutting guards pass; `go vet -tags="component live security" ./...` exits 0

---

## §6 — What this plan does NOT do

- **It does not run a live edit against a real Anthropic model.** Every assertion here is on the request shape and the local execution. One live transcript showing the model issuing a `str_replace` and the file changing is the honest final check, and it belongs to whoever next runs a stack.
- **It does not remove `palai.workspace.file`.** Task 5 Step 1 measures what still depends on its hashed write; removing it is separate work with its own guard.
- **It does not adopt `bash_20250124` or `memory_20250818`.** Both are Anthropic-defined tools that become declarable the moment Task 1 lands, and both are worth doing — the bash tool would replace `palai.workspace.shell`'s hand-written schema, and the memory tool is the durable-notes surface the owner has wanted. Neither is in scope here.

---

## §7 — After this: symbol-level editing

Grep and Glob (the search plan) plus this editor cover text. The next tier is symbols — jump to definition, find references, edit by symbol range rather than by string. That is an LSP surface: `gopls` exists for Go, and the multi-language answer is an LSP client driving per-language servers, the way Serena MCP does it. It is the largest remaining gap between this platform and a coding harness, and it should be planned only once text editing has a live transcript proving it works.
