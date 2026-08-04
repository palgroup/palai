# Code Search Tools (Grep + Glob) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A model working in a palai workspace can find code by name and by content, through typed tools that work identically whether the workspace lives on this host or on a remote machine.

**Architecture:** Grep and Glob become methods on `toolbroker.WorkspaceOps`, not shell invocations — the same seam Read/Write/List already use, so a workspace realized on a machine is searched on that machine. Each one touches five points: a protocol constant + handler in `packages/runner`, the interface, the remote client, the local implementation, and the tool itself. The schema validator gains `array`, `boolean`, and `enum` first, because Grep's schema needs all three and the validator currently **errors** on them.

**Tech Stack:** Go (single module `github.com/palgroup/palai`), ripgrep 14.1.1 (measured present) for the local content search, `go test` for guards.

## Global Constraints

- **The validator rejects, it does not ignore.** `packages/tool-broker/conformance_math.go:98` has `default: return fmt.Errorf("unsupported schema type %q", typ)`. A tool declaring `"type": "array"` today does not degrade — it **fails**. Task 1 is therefore a hard prerequisite for Tasks 4 and 6, not a tidy-up.
- **Never reach for this process's filesystem from a tool.** `ExecEnv.WorkspaceRoot` is a path on *whichever machine runs the command*, and `ExecEnv.Workspace` is nil when no workspace is bound. A tool with a nil `Workspace` answers `unavailable` (the `toolbroker.Answer` seam) — it never falls back to local disk. `tools/file.go:57-62` is the pattern to copy.
- **Every new canonical tool name must be registered in `toolbroker.New`** (`apps/control-plane/cmd/palai-control-plane/main.go`, one production call site) **and** carry a title in `apps/slack-bot/internal/relay/relay.go`'s `toolTitles`. Three existing guards enforce this and will fail otherwise: `TestEveryDefaultToolResolves`, `TestEveryCanonicalToolHasAHumanTitle`, `TestKnownNonGrantTitlesStaysDisjointFromCanonical`.
- **Search is read-only:** both tools are `toolbroker.ClassReversible`, never `ClassIrreversible`. This is what distinguishes them from today's workaround of running `rg` through `palai.workspace.shell`.
- **Do NOT edit anything under `tests/uat/`.** Released bundle evidence is history.
- **The working tree is shared and actively committed to by other sessions.** Stage only your own files by explicit path. Never `git add -A`, never `git add .`, never `git commit -- <path> -m` (the all-pathspec form fails in this repo).

---

## §1 — Re-measure before starting

Measured 2026-08-04. A changed number means re-read this plan rather than run it blindly.

```bash
# The validator's supported types — expect object/integer/number/string/"" and a rejecting default
grep -n 'case "' packages/tool-broker/conformance_math.go

# The workspace protocol's ops — expect 9, none of them search
grep -n 'WorkspaceOp[A-Z][a-z]* *=' packages/runner/workspaceserver.go

# The interface — expect Read/Write/List/Stat/Checksum/Head/Commit, no Grep, no Glob
grep -n "^	[A-Z][a-zA-Z]*(ctx" packages/tool-broker/workspace_ops.go

# ripgrep, for the local implementation
# ⚠️ CORRECTED 2026-08-04 — THIS MEASUREMENT WAS CONTAMINATED AND ITS ANSWER WAS WRONG.
# `rg --version` reported 14.1.1 during planning, so this plan specified shelling out to
# ripgrep. It is NOT installed: `rg` is a shell FUNCTION supplied by the Claude Code shell
# snapshot, which an exec'd subprocess never sees. `type rg` shows the function; the binary
# is absent from every PATH directory. Grep is therefore implemented in Go.
# The right form of this check is the one a subprocess would make:
env -i PATH="$PATH" sh -c 'command -v rg' || echo "ripgrep absent — implement in Go"

# The tools registered today — expect 11 constructor calls
grep -n "toolbroker.New(" -A 18 apps/control-plane/cmd/palai-control-plane/main.go
```

**Expected at plan time:** validator knows 5 type strings and errors on the rest; 9 workspace ops (`open`, `clone`, `read`, `write`, `list`, `stat`, `checksum`, `head`, `commit`); the interface has 7 methods; `ripgrep 14.1.1`.

---

## §2 — Why this is the first of the two tool plans

A model that cannot search reads files by guessing their names. Today the only content search available to it is `palai.workspace.shell` running `rg` — which is `ClassIrreversible` (so it carries replay semantics meant for commands with side effects), has a 60-second budget, and returns an unparsed blob.

Editing without searching is blind editing, which is why this plan comes before the text-editor plan. It also has no external prerequisite: Grep and Glob are ordinary custom tools. The text-editor plan is blocked on an adapter change (`provider_two/adapter.go:374` never emits a `type` field), so it cannot start clean.

---

## §3 — File Structure

| File | Responsibility |
|---|---|
| `packages/tool-broker/conformance_math.go` (modify) | The schema validator gains `array` (with `items`), `boolean`, and `enum`. |
| `apps/control-plane/internal/execution/tools/shell.go` (modify) | First consumer of the new types: `argv`/`shell`/`background` get real `type` declarations. |
| `packages/runner/workspaceserver.go` (modify) | Two new protocol constants and their handlers. |
| `packages/tool-broker/workspace_ops.go` (modify) | Two new interface methods and their result types. |
| `apps/control-plane/internal/execution/remote_workspace.go` (modify) | The remote client for both ops. |
| `apps/control-plane/internal/execution/tools/workspace_local.go` (modify) | The local implementation — `filepath.WalkDir` for Glob, `rg` for Grep. |
| `apps/control-plane/internal/execution/tools/glob.go` (**new**) | `palai.workspace.glob`. |
| `apps/control-plane/internal/execution/tools/grep.go` (**new**) | `palai.workspace.grep`. |

---

## Task 1: The schema validator learns array, boolean, and enum

**Files:**
- Modify: `packages/tool-broker/conformance_math.go` (`validate`, lines 50-100)
- Test: `packages/tool-broker/conformance_math_test.go` (or the existing test file for this package — find it first)

**Interfaces:**
- Consumes: nothing
- Produces: `validate` accepting `{"type":"array","items":{...}}`, `{"type":"boolean"}`, and `{"enum":[...]}` on any schema

- [x] **Step 1: Find the existing validator tests**

Run: `grep -rn "func Test" packages/tool-broker/*_test.go | grep -i "valid\|schema"`

Read them. Match their construction style — do not invent a new fixture shape.

- [x] **Step 2: Write the failing tests**

Add to that file:

```go
// TestValidateAcceptsArrays covers the type shell.go's `argv` has needed since it was written: a JSON
// array of strings. The validator errored on it, so the field shipped UNTYPED and the model learned
// its shape only from an English sentence.
func TestValidateAcceptsArrays(t *testing.T) {
	schema := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	if err := validate(schema, []any{"go", "test", "./..."}); err != nil {
		t.Fatalf("a []any of strings was rejected: %v", err)
	}
	if err := validate(schema, []any{"go", 7}); err == nil {
		t.Fatal("an array carrying a non-string was accepted; items must be enforced")
	}
	if err := validate(schema, "not an array"); err == nil {
		t.Fatal("a string was accepted where an array was declared")
	}
	// An array with no `items` constrains only the container, exactly as an untyped schema does.
	if err := validate(map[string]any{"type": "array"}, []any{1, "two", true}); err != nil {
		t.Fatalf("an itemless array schema rejected a mixed array: %v", err)
	}
}

// TestValidateAcceptsBooleans — `shell` and `background` are both booleans that ship untyped today,
// so a model sending the STRING "true" is accepted and reaches the tool as a non-bool.
func TestValidateAcceptsBooleans(t *testing.T) {
	schema := map[string]any{"type": "boolean"}
	for _, ok := range []any{true, false} {
		if err := validate(schema, ok); err != nil {
			t.Errorf("%v was rejected: %v", ok, err)
		}
	}
	for _, bad := range []any{"true", 1, nil} {
		if err := validate(schema, bad); err == nil {
			t.Errorf("%#v was accepted where a boolean was declared", bad)
		}
	}
}

// TestValidateEnforcesEnum is what makes Grep's output_mode declarable. An enum constrains the VALUE
// and is independent of `type`, so it must be checked whether or not a type is present.
func TestValidateEnforcesEnum(t *testing.T) {
	schema := map[string]any{"type": "string", "enum": []any{"content", "files_with_matches", "count"}}
	if err := validate(schema, "content"); err != nil {
		t.Fatalf("a listed value was rejected: %v", err)
	}
	if err := validate(schema, "contents"); err == nil {
		t.Fatal("an unlisted value was accepted; a near-miss is exactly what an enum exists to catch")
	}
	// Enum without a type still constrains.
	if err := validate(map[string]any{"enum": []any{1, 2}}, 3); err == nil {
		t.Fatal("an unlisted value was accepted on a typeless enum schema")
	}
}

// TestValidateStillRejectsAnUnknownType pins the property the default arm carries: this validator
// REFUSES what it does not understand rather than waving it through. A tool declaring a type this
// subset lacks must fail loudly at registration rather than ship an unchecked field.
func TestValidateStillRejectsAnUnknownType(t *testing.T) {
	if err := validate(map[string]any{"type": "null"}, nil); err == nil {
		t.Fatal("an unsupported type was accepted; the rejecting default is load-bearing")
	}
}
```

- [x] **Step 3: Run tests to verify they fail**

Run: `go test ./packages/tool-broker/ -run 'TestValidate' -v`
Expected: the array/boolean/enum tests FAIL (`unsupported schema type`), the unknown-type test PASSES already.

- [x] **Step 4: Implement**

In `validate`, add the two cases and hoist the enum check ahead of the type switch — an enum applies whether or not a type is declared:

```go
	// AN ENUM CONSTRAINS THE VALUE, NOT THE TYPE, so it is checked before the type switch and applies
	// to a typeless schema too. A near-miss ("contents" for "content") is the failure it exists to
	// catch, and it is the failure a description sentence never catches.
	if allowed, ok := schema["enum"].([]any); ok {
		if !slices.ContainsFunc(allowed, func(a any) bool { return a == value }) {
			return fmt.Errorf("value %#v is not one of %v", value, allowed)
		}
	}
```

and in the switch:

```go
	case "array":
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("expected array, got %T", value)
		}
		// `items` is OPTIONAL and its absence is not an error: an array with no item schema
		// constrains the container only, which is the same thing an untyped schema does for a scalar.
		itemSchema, hasItems := schema["items"].(map[string]any)
		if !hasItems {
			return nil
		}
		for i, item := range items {
			if err := validate(itemSchema, item); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
		return nil
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("expected boolean, got %v (%T)", value, value)
		}
		return nil
```

Update the doc comment above `validate` — it currently says the subset covers "integer, number, and string" and carries a `ponytail:` note about arrays and enums being future work. Both sentences become false with this change.

- [x] **Step 5: Run tests to verify they pass**

Run: `go test ./packages/tool-broker/ -v`
Expected: all PASS, including the pre-existing tests.

- [x] **Step 6: Commit**

```bash
git status --porcelain packages/tool-broker/
git add packages/tool-broker/conformance_math.go packages/tool-broker/<the test file>
git commit -m "feat(tool-broker): the schema validator understands arrays, booleans and enums"
```

---

## Task 2: Type the fields that shipped untyped

The validator's new capability is worth nothing until a tool declares the types. `shell.go` is the proof and the first beneficiary: three of its parameters carry no `type` at all today, so the model infers `argv`'s shape from prose and `background: "true"` would be accepted.

**Files:**
- Modify: `apps/control-plane/internal/execution/tools/shell.go` (the `InputSchema` at roughly lines 26-38)
- Test: `apps/control-plane/internal/execution/tools/shell_test.go`

**Interfaces:**
- Consumes: `validate`'s array/boolean support from Task 1
- Produces: nothing — this is a correctness fix

- [x] **Step 1: Write the failing test**

Add to `shell_test.go`:

```go
// TestTheShellSchemaDeclaresItsTypes is the first consumer of the validator's new capability, and it
// exists because the alternative shipped for a long time: `argv` carried the sentence "command and
// arguments as a JSON array of strings" and NO type, so the only thing standing between the model and
// a malformed call was English prose.
func TestTheShellSchemaDeclaresItsTypes(t *testing.T) {
	schema := ShellTool().InputSchema
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("the shell tool's schema has no properties map")
	}
	want := map[string]string{"argv": "array", "shell": "boolean", "background": "boolean"}
	for name, wantType := range want {
		field, ok := props[name].(map[string]any)
		if !ok {
			t.Errorf("property %q is missing", name)
			continue
		}
		got, _ := field["type"].(string)
		if got != wantType {
			t.Errorf("property %q declares type %q, want %q — an untyped field is checked by nothing", name, got, wantType)
		}
	}
	if items, ok := props["argv"].(map[string]any)["items"].(map[string]any); !ok || items["type"] != "string" {
		t.Error("argv declares no string `items`, so a call carrying [1,2] would pass validation")
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./apps/control-plane/internal/execution/tools/ -run TestTheShellSchemaDeclaresItsTypes -v`
Expected: FAIL — every property reports type `""`.

- [x] **Step 3: Add the types**

In `shell.go`'s `InputSchema`, add `"type"` to each property, keeping every existing `description` **unchanged** — those sentences carry measured behaviour and are not yours to reword:

```go
"argv":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": /* unchanged */},
"shell":      map[string]any{"type": "boolean", "description": /* unchanged */},
"background": map[string]any{"type": "boolean", "description": /* unchanged */},
```

- [x] **Step 4: Run the package**

Run: `go test ./apps/control-plane/internal/execution/tools/ -v`
Expected: all PASS. **If an existing shell test now fails**, it was passing a wrongly-typed argument that the validator previously ignored — that is a real defect the types just exposed. Report it rather than loosening the schema.

- [x] **Step 5: Sweep for other untyped fields**

Run:

```bash
grep -rn 'map\[string\]any{"description"' apps/control-plane/internal/execution/tools/*.go
```

Every hit is a property declared with a description and no type. Fix the ones in tools this plan touches; **list the rest in your report** rather than fixing them all — an unrelated tool's schema change belongs to whoever owns that tool.

- [x] **Step 6: Commit**

```bash
git status --porcelain apps/control-plane/internal/execution/tools/
git add apps/control-plane/internal/execution/tools/shell.go apps/control-plane/internal/execution/tools/shell_test.go
git commit -m "fix(tools): the shell schema declares its types instead of describing them in prose"
```

---

### Task 2 sweep result (measured 2026-08-04, after the shell fix landed)

`grep -rn 'map\[string\]any{"description"' apps/control-plane/internal/execution/tools/*.go` — seven properties still declare a description and no type, in two tools this plan does not own:

- `pull_request.go:25-26` — `title`, `body`; both plainly `string`.
- `task.go:35-39` — `action`, `key`, `title`, `status`, `detail`. **`status` is an enum in prose**: its description reads `open | in_progress | done | canceled`, which is now declarable and is exactly the near-miss case the validator's enum arm catches. `detail` is described as "optional metadata object".

Left untouched deliberately: `palai.task`/`palai.todo` are **not registered in `toolbroker.New`** (measured in the default-tool-set plan), so typing their schema changes nothing a model can reach today. Wiring those tools and typing their fields belong together, in whichever task claims them.

---

## Task 3: `WorkspaceOps.Glob` — protocol, interface, both implementations

A workspace op touches five points. This task does all five for Glob, because a protocol constant with no handler, or an interface method with one implementation, is not shippable in halves.

**Files:**
- Modify: `packages/runner/workspaceserver.go` (constants ~47-55, and the handler switch)
- Modify: `packages/tool-broker/workspace_ops.go` (interface + result type)
- Modify: `apps/control-plane/internal/execution/remote_workspace.go` (client)
- Modify: `apps/control-plane/internal/execution/tools/workspace_local.go` (local)
- Test: the existing test files for each

**Interfaces:**
- Consumes: nothing from earlier tasks
- Produces: `Glob(ctx context.Context, pattern string, limit int) (paths []string, truncated bool, err error)` on `toolbroker.WorkspaceOps`; protocol op `"glob"`

- [x] **Step 1: Read the shape of an existing op end to end**

Read all four sites for `checksum` — it is the simplest op with a scalar result:

```bash
grep -n "checksum\|Checksum" packages/runner/workspaceserver.go \
  packages/tool-broker/workspace_ops.go \
  apps/control-plane/internal/execution/remote_workspace.go \
  apps/control-plane/internal/execution/tools/workspace_local.go
```

**Copy that shape.** Do not invent a new error convention, a new parameter-encoding style, or a new place to put the confinement check.

- [x] **Step 2: Write the failing tests**

Write one test per implementation, both asserting the same behaviour. For the local one, create a temp workspace with a known tree; for the remote one, follow the existing remote-workspace test's fake-transport pattern.

Assert all four of these:
1. `**/*.go` matches nested files, `*.go` matches only the top level.
2. Results are **sorted by modification time, newest first** — the ordering Claude Code uses, so the most recently touched file is the first thing a model reads.
3. A `limit` of N returns N paths and `truncated == true` when more matched.
4. A pattern escaping the workspace (`../*`) is **refused**, not silently emptied.

- [x] **Step 3: Run tests to verify they fail**

Run: `go test ./packages/tool-broker/... ./apps/control-plane/internal/execution/... -run Glob -v`
Expected: compile failure — `Glob` undefined.

- [x] **Step 4: Implement all five points**

1. `workspaceserver.go`: `WorkspaceOpGlob = "glob"` beside its siblings, with a one-line comment matching theirs; add the handler arm.
2. `workspace_ops.go`: the interface method with a doc comment stating the ordering and the truncation contract.
3. `remote_workspace.go`: the client call, following `Checksum`'s shape.
4. `workspace_local.go`: `filepath.WalkDir` + `path.Match` semantics for `**`. **Do not shell out for Glob** — it is a filesystem walk and `rg --files` would add a dependency for no gain.
5. Confinement: the same guard every other op uses. A pattern is not a path, so **resolve each candidate result** and drop or refuse anything outside the root. This tree's history is that path comparisons in a verifier ship defeated — use the existing helper, do not write a new prefix check.

- [x] **Step 5: Run tests to verify they pass**

Run: `go test ./packages/tool-broker/... ./apps/control-plane/internal/execution/... -run Glob -v`
Expected: PASS on both implementations.

- [x] **Step 6: Prove the confinement bites**

Perturb: remove the confinement check in the local implementation and re-run.
Expected: the escape test FAILS.
**If it stays GREEN, the test is not reaching the guard** — fix the test, then restore the check and confirm green.

- [x] **Step 7: Commit**

```bash
git status --porcelain packages/runner packages/tool-broker apps/control-plane/internal/execution
git add packages/runner/workspaceserver.go packages/tool-broker/workspace_ops.go \
        apps/control-plane/internal/execution/remote_workspace.go \
        apps/control-plane/internal/execution/tools/workspace_local.go <the test files>
git commit -m "feat(workspace): glob is a workspace op, so a remote allocation is searched where it lives"
```

---

## Task 4: The `palai.workspace.glob` tool

**Files:**
- Create: `apps/control-plane/internal/execution/tools/glob.go`
- Test: `apps/control-plane/internal/execution/tools/glob_test.go`
- Modify: `apps/control-plane/cmd/palai-control-plane/main.go` (the `toolbroker.New` list)
- Modify: `packages/toolset/toolset.go` (add to `defaultTools`)
- Modify: `apps/slack-bot/internal/relay/relay.go` (add a title)

**Interfaces:**
- Consumes: `WorkspaceOps.Glob` from Task 3; the validator's `array`/`integer` support from Task 1
- Produces: tool name `palai.workspace.glob`

- [x] **Step 1: Write the failing test**

Follow `file_test.go`'s construction style. Assert:
1. A nil `env.Workspace` answers `unavailable` — it does **not** touch this process's disk (copy `file.go:57-62`'s pattern and the test that pins it).
2. The result carries `{"paths": [...], "truncated": bool}`.
3. `ReplayClass` is `ClassReversible`.
4. A refused pattern comes back as a `toolbroker.Answer`, not a raw error — a search that failed changed nothing, so the model may try again.

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./apps/control-plane/internal/execution/tools/ -run Glob -v`
Expected: FAIL — `GlobTool` undefined.

- [x] **Step 3: Implement the tool**

Schema — now declarable because of Task 1:

```go
InputSchema: map[string]any{
	"type": "object",
	"properties": map[string]any{
		"pattern": map[string]any{"type": "string", "description": "glob pattern; ** matches any depth, e.g. `**/*.go` or `src/**/*.ts`"},
		"limit":   map[string]any{"type": "integer", "description": "maximum paths to return (default 100); results are newest-modified first, so a truncated answer still carries the most recently touched files"},
	},
	"required":             []any{"pattern"},
	"additionalProperties": false,
},
```

Write the description as onboarding documentation for someone who has never seen this codebase — Anthropic's measured guidance is that *"even small refinements to tool descriptions can yield dramatic improvements"*, and this one has to teach a model when to reach for Glob instead of Grep.

- [x] **Step 4: Register it in all three places**

The three guards from the default-tool-set plan will fail until you do:
1. `toolbroker.New(...)` in `main.go` — otherwise `TestEveryDefaultToolResolves` fails.
2. `toolset.defaultTools` — searching is not conditional on a repository.
3. `relay.go`'s `toolTitles` — otherwise `TestEveryCanonicalToolHasAHumanTitle` fails. Suggested phrase: `"Finding files"`.

- [x] **Step 5: Run the guards and the tool's own tests**

```bash
go test ./apps/control-plane/internal/execution/tools/ ./packages/toolset/ ./apps/slack-bot/internal/relay/ -v
```
Expected: all PASS, including the three cross-cutting guards.

- [x] **Step 6: Commit**

```bash
git status --porcelain
git add apps/control-plane/internal/execution/tools/glob.go apps/control-plane/internal/execution/tools/glob_test.go \
        apps/control-plane/cmd/palai-control-plane/main.go packages/toolset/toolset.go \
        apps/slack-bot/internal/relay/relay.go
git commit -m "feat(tools): palai.workspace.glob, so a model can find files by name"
```

---

## Task 5: `WorkspaceOps.Grep` — protocol, interface, both implementations

Same five points as Task 3, for content search. The local implementation shells out to `rg` (measured present at 14.1.1); the remote one is a protocol call like every other op.

**Files:** the same five as Task 3, plus their tests.

**Interfaces:**
- Consumes: Task 3's shape (copy it)
- Produces: `Grep(ctx context.Context, req GrepRequest) (GrepResult, error)` on `toolbroker.WorkspaceOps`; protocol op `"grep"`

- [x] **Step 1: Design the request and result types**

Put them in `workspace_ops.go` beside the interface:

```go
// GrepRequest is one content search. The field set is deliberately the one a coding agent actually
// uses — pattern, where to look, how much context, and how much to return — and no more.
type GrepRequest struct {
	Pattern    string // ripgrep regex syntax, NOT POSIX: `interface{}` must be written `interface\{\}`
	Path       string // workspace-relative subtree to search; "" means the whole workspace
	Glob       string // optional filename filter, e.g. `**/*.go`
	OutputMode string // "content" | "files_with_matches" | "count"
	Before     int    // lines of leading context (content mode only)
	After      int    // lines of trailing context (content mode only)
	Multiline  bool   // let the pattern match across line boundaries
	Limit      int    // maximum entries returned
}
```

Define `GrepResult` to carry the three output modes without three shapes: matched lines with file+line number, or file paths, or per-file counts plus a total.

- [x] **Step 2: Write the failing tests**

Assert, on both implementations:
1. `content` mode returns file, line number, and the matching line.
2. `files_with_matches` returns paths only, and is the **default** when `OutputMode` is empty.
3. `count` returns a per-file count **and a total that counts every match**, even when `Limit` truncates the listed entries.
4. A pattern ripgrep rejects comes back as an error carrying ripgrep's own diagnostic — not as "no matches". *(This is a defect Claude Code shipped and fixed; do not reproduce it.)*
5. `.gitignore` is respected, and a path passed explicitly is searched anyway.
6. A `Path` escaping the workspace is refused.

- [x] **Step 3: Run tests to verify they fail**

Run: `go test ./packages/tool-broker/... ./apps/control-plane/internal/execution/... -run Grep -v`
Expected: compile failure — `Grep` undefined.

- [x] **Step 4: Implement all five points**

> **SUPERSEDED 2026-08-04 during execution.** This step said to shell out to `rg --json`, on the strength of the §1 measurement that reported ripgrep 14.1.1 present. That measurement was contaminated — `rg` is a shell function from the harness, not a binary a subprocess can exec — so **Grep is implemented in Go**, using `regexp` and a `filepath.WalkDir`.
>
> **The dialect concern this step raised does not apply to that substitution**, which is what makes it safe: Go's `regexp` and ripgrep both implement RE2, so a pattern written for one behaves identically in the other. Had the engines differed, depending on the binary would have been the right call.
>
> What the Go implementation does NOT reproduce: full `.gitignore` grammar. It handles the root file's plain directory and name entries plus `.git`, which covers the case that actually corrupts a search (a vendored or build directory), and it says so in its doc comment. Negation, nested ignore files, and globbed ignore rules are out of scope, and a caller who needs an ignored file can still name its path.

- [x] **Step 5: Run tests to verify they pass**

Run: `go test ./packages/tool-broker/... ./apps/control-plane/internal/execution/... -run Grep -v`
Expected: PASS on both implementations.

- [x] **Step 6: Prove the count-vs-truncation property**

Perturb: make `count` mode's total sum only the listed entries rather than every match, and re-run.
Expected: the count test FAILS.
Restore and confirm green. *(This is the exact bug Claude Code carried until v2.1.208 — a total that silently under-reports when the head limit trims the list.)*

- [x] **Step 7: Commit**

```bash
git status --porcelain packages/runner packages/tool-broker apps/control-plane/internal/execution
git add <the five files and their tests>
git commit -m "feat(workspace): grep is a workspace op, backed by ripgrep where the allocation lives"
```

---

## Task 6: The `palai.workspace.grep` tool

**Files:**
- Create: `apps/control-plane/internal/execution/tools/grep.go`
- Test: `apps/control-plane/internal/execution/tools/grep_test.go`
- Modify: `main.go`, `packages/toolset/toolset.go`, `relay.go` (the same three registration points)

**Interfaces:**
- Consumes: `WorkspaceOps.Grep` from Task 5; the validator's `enum`/`boolean`/`integer` support from Task 1
- Produces: tool name `palai.workspace.grep`

- [x] **Step 1: Write the failing test**

Assert the same four properties as Task 4's tool test (nil workspace → `unavailable`; result shape; `ClassReversible`; refusal is an `Answer`), plus:
5. `output_mode` rejects an unlisted value **through the schema validator**, not through a hand-written check in the tool. This is what Task 1 was for — assert it by calling the broker's validation path, not by asserting on a string.

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./apps/control-plane/internal/execution/tools/ -run Grep -v`
Expected: FAIL — `GrepTool` undefined.

- [x] **Step 3: Implement the tool**

Schema — every one of these types was impossible before Task 1:

```go
"pattern":     map[string]any{"type": "string", ...},
"path":        map[string]any{"type": "string", ...},
"glob":        map[string]any{"type": "string", ...},
"output_mode": map[string]any{"type": "string", "enum": []any{"content", "files_with_matches", "count"}, ...},
"-A":          map[string]any{"type": "integer", ...},
"-B":          map[string]any{"type": "integer", ...},
"multiline":   map[string]any{"type": "boolean", ...},
"head_limit":  map[string]any{"type": "integer", ...},
```

The description must say **ripgrep syntax, not POSIX**, and give the `interface\{\}` example — that specific confusion costs a Go-repo agent a wasted call every time.

- [x] **Step 4: Register it in all three places**

`main.go`, `toolset.defaultTools`, and `relay.go`'s `toolTitles` (suggested phrase: `"Searching the code"`).

- [x] **Step 5: Run every guard this plan touches**

```bash
go test ./packages/tool-broker/... ./packages/toolset/... \
        ./apps/control-plane/internal/execution/... \
        ./apps/slack-bot/internal/relay/... ./cmd/cli/internal/stack/...
go vet -tags="component live security" ./...
```
Expected: all PASS, vet exits 0.

- [x] **Step 6: Commit**

```bash
git status --porcelain
git add <the tool, its test, and the three registration files>
git commit -m "feat(tools): palai.workspace.grep, so a model can find code by content"
```

---

## §4.5 — Execution record (2026-08-04)

**Shipped in 6 commits:** `a960d4d6` (validator), `5019e044` (shell types), `ff65767a` + `2de12640` (Glob), `db737d1f` + `5068abb8` (Grep), `c58bf450` + `10f0f0a8` (the two tools).

**The canonical default set went from 6 names to 8.** `palai.workspace.glob` and `palai.workspace.grep` are registered in `toolbroker.New`, in `toolset.Default()`, and in the Slack relay's titles.

**The plan's §1 measurement was contaminated and its answer was wrong.** `rg --version` reported ripgrep 14.1.1 during planning, so Task 5 specified shelling out to `rg --json`. Ripgrep is **not installed**: `rg` is a shell function from the Claude Code shell snapshot, which an exec'd subprocess never sees — `type rg` names the file it comes from, and no PATH directory holds the binary. Grep is implemented in Go instead. **The dialect concern that motivated the original choice does not apply to the substitution**: Go's `regexp` and ripgrep both implement RE2, so patterns behave the same. The corrected form of the check — `env -i PATH="$PATH" sh -c 'command -v rg'` — is in §1.

**Six perturbations observed RED, then restored:**
- Glob's pattern-escape refusal removed → `../*`, `/etc/*` and `src/../../*` all returned no error.
- The runner's `glob` handler removed → the remote test failed with `unknown workspace op "glob"`, proving the chain reaches the machine rather than this process's disk.
- Grep's count total recomputed from the truncated list → `truncated total = 2, want the untruncated 4`.
- The runner's `grep` handler removed → `unknown workspace op "grep"`.

**A defect this plan avoided by knowing about it:** Claude Code's Grep reported a rejected pattern as "no files found" until v2.1.208, and its `count` total summed only the listed entries when a head limit truncated them. Both are pinned by tests here.

**Measured after:** all six affected packages `ok`, and `go vet -tags="component live security" ./...` exits **0** — the concurrent org/tenancy refactor landed its tagged callers during this work, so the tree is fully green.

---

## §5 — Definition of done

- [x] `validate` accepts `array` (with optional `items`), `boolean`, and `enum`, and still **rejects** an unknown type
- [x] `shell.go`'s `argv`/`shell`/`background` declare real types; the untyped-field sweep's remaining hits are listed in a report
- [x] `WorkspaceOps` has `Glob` and `Grep`, each implemented in **both** `RemoteWorkspace` and `LocalWorkspace`, each with a protocol constant and handler
- [x] `palai.workspace.glob` and `palai.workspace.grep` are registered in `toolbroker.New`, in `toolset.Default()`, and in `relay.toolTitles`
- [x] All three cross-cutting guards pass: `TestEveryDefaultToolResolves`, `TestEveryCanonicalToolHasAHumanTitle`, `TestKnownNonGrantTitlesStaysDisjointFromCanonical`
- [x] Both perturbations (Task 3 Step 6, Task 5 Step 6) were observed RED and then restored
- [x] `go vet -tags="component live security" ./...` exits 0

---

## §6 — What this plan does NOT do

- **It does not remove `palai.workspace.shell`'s ability to run `rg`.** Nothing stops a model from doing that; what changes is that it no longer has to.
- **It does not add symbol-level search** (definitions, references, call hierarchies). That is an LSP/tree-sitter surface and a separate plan — see the text-editor plan's §7.
- **It does not index anything.** No embeddings, no trigram index. At this tree's size `rg` answers in milliseconds; if a monorepo ever makes that false, the next step is Zoekt (Apache-2.0, pure Go, embeddable), not vectors. Measured 2026-08-04: Anthropic removed RAG from Claude Code, Sourcegraph removed embeddings in Cody v5.3, and Cursor's 2026 answer to slow `rg` was a lexical index.
