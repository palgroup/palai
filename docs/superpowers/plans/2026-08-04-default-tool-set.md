# Slack-Independent Default Tool Set — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `palai up` grants a Slack-independent default tool set from ONE canonical list, and no second copy of that list — not the CLI's, not the guard's, not Slack's card titles — can drift from it.

**Architecture:** The canonical lists move out of `cmd/cli/internal/stack/up.go` into a new `packages/toolset` package that the CLI, the control plane's guards, and the Slack relay all import. `palai up` stops *reading* the project policy to print a warning and starts *writing* `default_tools` into it. Two guards then bind the ends: every canonical name must resolve to a tool the broker can produce, and every canonical name must have a human title in the Slack relay.

**Tech Stack:** Go (single module `github.com/palgroup/palai`), `packages/` for cross-binary shared code, `go test` for guards.

## Global Constraints

- **Module layout is measured, not assumed:** `find . -name go.mod -maxdepth 3` → `./go.mod` and `./sdks/go/go.mod` (2026-08-04). The tree is one module named `github.com/palgroup/palai`.
- **`apps/control-plane/internal/...` is unreachable from `cmd/cli`** — Go's `internal` rule. The canonical list therefore CANNOT live under the control plane. `packages/` is the only shared home, and the CLI already imports from it (`cmd/cli/main.go:16` imports `packages/version`).
- **No Slack-named symbol in the canonical package.** The names are `Default`, `Repository`, `Publish` — not `slackDefaultTools`. This is the user's explicit requirement: the platform surface is generic; Slack is one consumer.
- **`palai.slack.search` MUST NOT be in `Default()`.** It is Slack-specific and is not registered in the broker's static set (see the §3 inventory below).
- **The working tree is shared and currently dirty.** `git status` showed 40+ modified files at plan time. Before every commit step, run `git status --porcelain` and stage ONLY the files named in that step. Use `git add <files>` then `git commit -m "..."` — never `git commit -- <path> -m` (all-pathspec form fails, and pathspec cannot commit a new file).

---

## §1 — Re-measure before starting (the first task's first act)

This plan's claims were measured on 2026-08-04. Re-run these before Task 1; a changed number means re-read the plan, not re-run it blindly.

```bash
# The RED this plan turns green — expect 4 FAILs (CORRECTED 2026-08-04: an earlier
# draft of this plan said 2. All FOUR tests in the file reach the regex helper, so
# all four fail on the same missing symbol.)
go test ./apps/control-plane/internal/execution/tools/ -v

# The lists the guard looks for — expect ZERO hits in up.go
grep -n 'var slackDefaultTools\|var slackRepositoryTools\|var slackPublishTools' \
  cmd/cli/internal/stack/up.go

# Tools the broker actually registers — expect 11 constructor calls
sed -n '659,675p' apps/control-plane/cmd/palai-control-plane/main.go

# Slack's own copy of the name set — expect 15 entries
sed -n '433,448p' apps/slack-bot/internal/relay/relay.go
```

**Expected at plan time (2026-08-04):**
- Both tests FAIL with `slackDefaultTools was not found in cmd/cli/internal/stack/up.go`
- The three `var` declarations do not exist (removed by commit `1e5fc63e`, "bring-up prepares a stack and nothing else", 2026-08-03)
- `toolTitles` has 15 entries

---

## §2 — Why it broke (so the fix is not re-broken)

Commit `1e5fc63e` removed Slack wiring from `palai up`, and the agent tool lists were part of the 24 symbols it took out. Nothing generic replaced them.

The consequence is not cosmetic. `apps/control-plane/internal/execution/config.go:120-137` computes a run's effective tool set as:

```
effective = ( project_baseline  ∪  revision_tool_sets )  ∩  revision_tools
```

The intersection runs LAST and is a ceiling. **An empty project baseline empties the result**, so a run completes, answers in one step, and calls nothing. `cmd/cli/internal/stack/up.go:1417-1428` detects exactly this and prints a warning naming the fix — but a manual step every operator must remember is a step that will be forgotten.

**The design decision this plan implements:** `palai up` writes the baseline itself. The warning stays as a fallback for projects the CLI did not provision.

---

## §3 — Inventory: what the broker can actually produce

Measured from `apps/control-plane/cmd/palai-control-plane/main.go:659-675` (`toolbroker.New(...)`) on 2026-08-04. **A name absent from this list cannot be granted** — it would be offered to the model and never resolve.

| Tool name | Constructor | Goes in |
|---|---|---|
| `palai.workspace.file` | `tools.FileTool()` | `Default()` |
| `palai.workspace.shell` | `tools.ShellTool()` | `Default()` |
| `palai.workspace.background_kill` | `tools.BackgroundKillTool()` | `Default()` |
| `palai.workspace.show_media` | `tools.MediaTool()` | `Default()` |
| `palai.research.fetch` | `tools.ResearchFetchTool()` | `Default()` |
| `palai.knowledge.retrieve` | `tools.KnowledgeRetrievalTool(...)` | `Default()` |
| `palai.workspace.commit` | `tools.CommitTool()` | `Repository()` |
| `palai.publish.push` | `tools.PushTool()` | `Publish()` |
| `palai.publish.pull_request` | `tools.PullRequestTool()` | `Publish()` |
| `palai.publish.merge_pull_request` | `tools.MergeTool()` | `Publish()` |
| *(conformance math)* | `toolbroker.ConformanceMathAdd()` | **none** — test fixture |

**Deliberately excluded, with reasons:**
- `palai.slack.search` (`slackSearchToolName`, `tools/slack_search.go:31`) — Slack-specific, and NOT in the broker's static set; it resolves through the Slack lookup path only.
- `palai.task` / `palai.todo` (`tools/task.go:14,18`) — defined but **absent from `toolbroker.New(...)`**; granting them today would offer the model a tool that cannot resolve. Wiring them is out of scope for this plan; see §7.

---

## §4 — File Structure

| File | Responsibility |
|---|---|
| `packages/toolset/toolset.go` (**new**) | The canonical lists. One exported function per list, plus `All()`. No I/O, no dependencies. |
| `packages/toolset/toolset_test.go` (**new**) | Shape guards: non-empty, no duplicates, `palai.` prefix, no Slack-specific name in `Default()`. |
| `apps/control-plane/internal/execution/tools/default_set_test.go` (modify) | Stop parsing CLI source with a regex; import `packages/toolset` directly. |
| `cmd/cli/internal/stack/up.go` (modify) | Write `default_tools` into `prj_local`'s policy from `toolset.Default()`. |
| `apps/slack-bot/internal/relay/tool_titles_test.go` (**new**) | Every canonical name has a human title in `toolTitles`. |

---

## Task 1: The canonical tool-set package

**Files:**
- Create: `packages/toolset/toolset.go`
- Test: `packages/toolset/toolset_test.go`

**Interfaces:**
- Consumes: nothing
- Produces: `toolset.Default() []string`, `toolset.Repository() []string`, `toolset.Publish() []string`, `toolset.All() []string` — every one returns a fresh copy so a caller cannot mutate the package's state.

- [ ] **Step 1: Write the failing test**

Create `packages/toolset/toolset_test.go`:

```go
package toolset_test

import (
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/toolset"
)

// TestEveryListIsNonEmptyAndWellFormed pins the shape a grant depends on. A list that is empty, or
// that carries a name in the wrong namespace, empties or corrupts every effective tool set computed
// from it (execution/config.go:120-137 intersects LAST, so garbage in is silence out).
func TestEveryListIsNonEmptyAndWellFormed(t *testing.T) {
	lists := map[string][]string{
		"Default":    toolset.Default(),
		"Repository": toolset.Repository(),
		"Publish":    toolset.Publish(),
	}
	for name, list := range lists {
		if len(list) == 0 {
			t.Fatalf("%s() is empty: an empty baseline empties every effective tool set", name)
		}
		seen := map[string]bool{}
		for _, tool := range list {
			if !strings.HasPrefix(tool, "palai.") {
				t.Errorf("%s() carries %q, which is not in the palai. namespace", name, tool)
			}
			if seen[tool] {
				t.Errorf("%s() lists %q twice", name, tool)
			}
			seen[tool] = true
		}
	}
}

// TestDefaultCarriesNoSlackSpecificTool is the user's explicit requirement, asserted rather than
// assumed: the default set is the GENERIC platform surface. palai.slack.search is not in the
// broker's static set either, so granting it would offer a name that cannot resolve.
func TestDefaultCarriesNoSlackSpecificTool(t *testing.T) {
	for _, tool := range toolset.Default() {
		if strings.HasPrefix(tool, "palai.slack.") {
			t.Errorf("Default() carries the Slack-specific tool %q", tool)
		}
	}
}

// TestAllIsTheUnionAndTheListsAreDisjoint keeps All() honest as the single thing a guard can range
// over. Disjointness matters because up.go appends Repository()/Publish() onto Default(): an overlap
// would write a duplicate into the project policy.
func TestAllIsTheUnionAndTheListsAreDisjoint(t *testing.T) {
	seen := map[string]string{}
	for listName, list := range map[string][]string{
		"Default": toolset.Default(), "Repository": toolset.Repository(), "Publish": toolset.Publish(),
	} {
		for _, tool := range list {
			if prior, dup := seen[tool]; dup {
				t.Errorf("%q appears in both %s() and %s()", tool, prior, listName)
			}
			seen[tool] = listName
		}
	}
	all := toolset.All()
	if len(all) != len(seen) {
		t.Fatalf("All() has %d entries, the three lists have %d distinct", len(all), len(seen))
	}
	for _, tool := range all {
		if _, ok := seen[tool]; !ok {
			t.Errorf("All() carries %q, which is in none of the three lists", tool)
		}
	}
}

// TestCallersCannotMutateTheCanonicalLists — a returned slice backed by the package's own array lets
// one caller's append corrupt every later caller's grant.
func TestCallersCannotMutateTheCanonicalLists(t *testing.T) {
	first := toolset.Default()
	if len(first) == 0 {
		t.Fatal("Default() is empty")
	}
	first[0] = "palai.mutated"
	if toolset.Default()[0] == "palai.mutated" {
		t.Fatal("Default() returns its backing array: a caller mutated the canonical list")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./packages/toolset/ -v`
Expected: FAIL — the package does not exist (`no required module provides package`).

- [ ] **Step 3: Write the implementation**

Create `packages/toolset/toolset.go`:

```go
// Package toolset is the CANONICAL list of tools a palai bring-up grants.
//
// IT LIVES UNDER packages/ FOR A REASON THAT IS NOT STYLE: Go's internal rule makes
// apps/control-plane/internal/... unreachable from cmd/cli, and three binaries need this list — the
// CLI that writes the project baseline, the control plane's guard that proves every name resolves,
// and the Slack relay that renders a human title per name. A list copied into any of those three is a
// list that will drift; commit 1e5fc63e is the proof, having deleted the CLI's copy and left the
// guard hunting for it.
//
// THE NAMES ARE GENERIC, NOT SLACK'S. A bring-up binds a platform surface — a workspace, a shell, a
// publication path — and Slack is one consumer of that surface, not its shape.
package toolset

// defaultTools is what every bring-up binds, with or without a repository: read and write files, run
// a command, stop what that command started, show a human what happened, and reach for outside
// knowledge. Each name is registered in toolbroker.New (apps/control-plane/cmd/palai-control-plane/
// main.go:659-675) — a name that is not is a tool the model is offered and can never be given.
var defaultTools = []string{
	"palai.workspace.file",
	"palai.workspace.shell",
	"palai.workspace.background_kill",
	"palai.workspace.show_media",
	"palai.research.fetch",
	"palai.knowledge.retrieve",
}

// repositoryTools is the coding half a bring-up ADDS once it has bound a repository. Committing
// without a repository is not a weaker capability, it is a meaningless one.
var repositoryTools = []string{
	"palai.workspace.commit",
}

// publishTools is the publication path: the three operations that move work OUT of the workspace.
// They are their own list because they are the ones gated by a human decision (E23 T6).
var publishTools = []string{
	"palai.publish.push",
	"palai.publish.pull_request",
	"palai.publish.merge_pull_request",
}

// Default returns the tools every bring-up binds.
func Default() []string { return clone(defaultTools) }

// Repository returns the tools a bring-up adds when it bound a repository.
func Repository() []string { return clone(repositoryTools) }

// Publish returns the publication tools.
func Publish() []string { return clone(publishTools) }

// All returns every canonical name, in list order: Default, then Repository, then Publish. It is what
// a guard ranges over when it must prove something about the whole surface.
func All() []string {
	out := make([]string, 0, len(defaultTools)+len(repositoryTools)+len(publishTools))
	out = append(out, defaultTools...)
	out = append(out, repositoryTools...)
	return append(out, publishTools...)
}

// clone hands back a copy. Returning the package's own slice would let one caller's append or
// assignment rewrite the canonical list for every later caller in the process.
func clone(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./packages/toolset/ -v`
Expected: PASS — all four tests.

- [ ] **Step 5: Commit**

```bash
git status --porcelain packages/toolset/
git add packages/toolset/toolset.go packages/toolset/toolset_test.go
git commit -m "feat(toolset): one canonical tool list, named for the platform not for Slack"
```

---

## Task 2: Point the control-plane guard at the canonical package

> **COMPLETE (2026-08-04, commits `e19f1ddc`..`bb5b8e67`). The step text below is partly superseded** — it still names `TestTheSearchToolIsInTheDefaultSet` and `TestNoDefaultToolHasSideEffects`. Executing this task surfaced three contradictions between these steps and the plan's own Global Constraints; the human ruled, and §5 carries the outcome. The guard file now holds three tests: `TestEveryDefaultToolResolves`, `TestThePublishToolsAreTheirOwnListAndNeitherPublishes`, and `TestNoDefaultToolPublishes`.

The guard currently reads `cmd/cli/internal/stack/up.go` as **text** and regex-matches `var slackDefaultTools = []string{...}`. That is why it is RED: the variable is gone. Replacing the regex with an import both fixes the RED and removes a whole failure mode — a guard that can be defeated by moving a file or reformatting a declaration.

**Files:**
- Modify: `apps/control-plane/internal/execution/tools/default_set_test.go` (replace `readCLIToolList` and its three call sites at `:60`, `:96-97`, `:121`, `:127`)

**Interfaces:**
- Consumes: `toolset.Default()`, `toolset.Repository()`, `toolset.Publish()` from Task 1
- Produces: nothing — this is a guard

- [ ] **Step 1: Read the current guard end to end**

Run: `cat apps/control-plane/internal/execution/tools/default_set_test.go`

Note the four test functions and what each asserts. You are replacing only how the *names* are obtained; every assertion about them stays.

- [ ] **Step 2: Confirm the RED and its reason**

Run:
```bash
go test ./apps/control-plane/internal/execution/tools/ \
  -run 'TestEverySlackDefaultToolResolves|TestTheSearchToolIsInTheDefaultSet' -v
```
Expected: 2× FAIL, each with `slackDefaultTools was not found in cmd/cli/internal/stack/up.go`.

- [ ] **Step 3: Replace the source-reading helpers**

Delete `readCLIToolList` and `readCLISlackDefaultTools` entirely (the `os`, `path/filepath`, and `regexp` imports go with them if nothing else uses them — let the compiler tell you). Add the import `"github.com/palgroup/palai/packages/toolset"` and replace the call sites:

```go
// The canonical lists are read from packages/toolset, NOT parsed out of the CLI's source. The regex
// this replaces asserted the shape of a Go declaration in a file three directories away: it went RED
// the day commit 1e5fc63e deleted that declaration, and it would have gone silently WRONG the day
// somebody reformatted it. An import cannot drift from what the CLI actually grants, because it is
// what the CLI actually grants.
```

- `readCLISlackDefaultTools(t)` → `toolset.Default()`
- `readCLIToolList(t, "slackRepositoryTools")` → `toolset.Repository()`
- `readCLIToolList(t, "slackPublishTools")` → `toolset.Publish()`

Rename the tests so no Slack-named symbol survives in this file:
- `TestEverySlackDefaultToolResolves` → `TestEveryDefaultToolResolves`
- `TestNoDefaultSlackToolHasSideEffects` → `TestNoDefaultToolHasSideEffects`

- [ ] **Step 4: Run the guard**

Run: `go test ./apps/control-plane/internal/execution/tools/ -run 'TestEveryDefaultToolResolves|TestThePublishToolsAreTheirOwnListAndNeitherPublishes|TestTheSearchToolIsInTheDefaultSet|TestNoDefaultToolHasSideEffects' -v`
Expected: all PASS.

- [ ] **Step 5: Prove the guard still bites (perturbation)**

Add a bogus name to `packages/toolset/toolset.go`'s `defaultTools` — `"palai.workspace.does_not_exist"` — and re-run Step 4.
Expected: `TestEveryDefaultToolResolves` FAILS naming that tool.

**If it stays GREEN, the guard is defeated — stop and fix the guard, not the list.** Then remove the bogus name and confirm green again.

- [ ] **Step 6: Commit**

```bash
git status --porcelain apps/control-plane/internal/execution/tools/default_set_test.go
git add apps/control-plane/internal/execution/tools/default_set_test.go
git commit -m "fix(tools): the default-set guard imports the list instead of regexing the CLI's source"
```

---

## Task 3: `palai up` writes the baseline it currently only warns about

**Files:**
- Modify: `cmd/cli/internal/stack/up.go` (the policy read at `:1402-1428`; `bootstrapProjectID` is at `:610`)

**Interfaces:**
- Consumes: `toolset.Default()` from Task 1
- Produces: a `prj_local` whose `config_policy.default_tools` is non-empty after `palai up`

- [ ] **Step 1: Read the existing writer, and the trap in it**

Run: `sed -n '265,295p' cmd/cli/internal/admin/admin.go`

The admin command already writes this policy. Measured on 2026-08-04, it builds a map and sends:

```go
c.do(http.MethodPatch, "/v1/projects/"+esc(pos[0]), body(map[string]any{"config_policy": policy}))
```

**THE WRITE REPLACES THE WHOLE DOCUMENT.** The comment above that line says it outright: `UpdateProjectConfigPolicy` is an assignment, not a merge, so *"a call that names one flag CLEARS every other key the policy carried."*

This is load-bearing for a bring-up, and it is the difference between a correct implementation and one that quietly destroys configuration:

- A **first** `palai up` writes into an empty policy. Sending only `default_tools` is harmless.
- A **second** `palai up` — a re-run, an upgrade, a repaired stack — hits a policy that may already carry `approvers`, `pool`, `allowed_models`. Sending only `default_tools` **deletes all three**. `approvers` is the approval allow-list; dropping it silently re-opens a surface an operator deliberately closed.

So the write must be **read-modify-write**, not a bare PATCH. Step 4 implements that.

- [ ] **Step 2: Write the failing test**

Create the test beside the existing bring-up tests in `cmd/cli/internal/stack/`. Name the file `up_default_tools_test.go`:

```go
package stack

import (
	"reflect"
	"testing"

	"github.com/palgroup/palai/packages/toolset"
)

// TestBringUpGrantsTheCanonicalDefaultSet pins the ONE fact this task exists for: the names the CLI
// writes into prj_local's policy are the canonical list, not a copy of it. A copy is what commit
// 1e5fc63e deleted and what left the guard hunting a symbol that no longer existed.
func TestBringUpGrantsTheCanonicalDefaultSet(t *testing.T) {
	granted := bootstrapDefaultTools()
	want := toolset.Default()
	if !reflect.DeepEqual(granted, want) {
		t.Fatalf("bring-up grants %v, canonical Default() is %v", granted, want)
	}
}

// TestTheGrantPreservesEveryOtherPolicyKey is the guard against the trap named in Step 1: the policy
// write REPLACES the document. A bring-up that sent only default_tools would delete approvers, pool,
// and allowed_models on its SECOND run — and approvers is an allow-list an operator deliberately
// closed. The first run is not where this bites, which is exactly why it needs a test.
func TestTheGrantPreservesEveryOtherPolicyKey(t *testing.T) {
	existing := map[string]any{
		"approvers":      []string{"key:apik_1"},
		"pool":           "mac-mini",
		"allowed_models": []string{"m1"},
	}
	merged := policyWithDefaultTools(existing, toolset.Default())

	for key, want := range existing {
		got, present := merged[key]
		if !present {
			t.Errorf("the write dropped %q: a re-run would clear it from the live policy", key)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the write changed %q: got %v, want %v", key, got, want)
		}
	}
	if !reflect.DeepEqual(merged["default_tools"], toolset.Default()) {
		t.Errorf("default_tools = %v, want the canonical set", merged["default_tools"])
	}
}

// TestTheGrantDoesNotMutateTheCallersPolicy — the caller holds the document it just read from the
// server; a merge that wrote through would make the "before" and "after" the same object and hide a
// diff the caller may want to log or skip on.
func TestTheGrantDoesNotMutateTheCallersPolicy(t *testing.T) {
	existing := map[string]any{"pool": "mac-mini"}
	_ = policyWithDefaultTools(existing, toolset.Default())
	if _, leaked := existing["default_tools"]; leaked {
		t.Fatal("policyWithDefaultTools wrote into the caller's map")
	}
}

// TestTheGrantAcceptsAnAbsentPolicy — a freshly provisioned project has no config_policy at all, and
// a nil map is where a merge written for the happy path panics.
func TestTheGrantAcceptsAnAbsentPolicy(t *testing.T) {
	merged := policyWithDefaultTools(nil, toolset.Default())
	if !reflect.DeepEqual(merged["default_tools"], toolset.Default()) {
		t.Fatalf("default_tools = %v on a nil policy, want the canonical set", merged["default_tools"])
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./cmd/cli/internal/stack/ -run 'TestBringUpGrants|TestTheGrant' -v`
Expected: FAIL to compile — `undefined: bootstrapDefaultTools`, `undefined: policyWithDefaultTools`.

- [ ] **Step 4: Add the accessor and the merge**

In `up.go`, add both functions the tests name:

```go
// bootstrapDefaultTools is the baseline `palai up` grants prj_local. It is a straight pass-through to
// the canonical list ON PURPOSE — a bring-up that filtered or reordered here would be a second list,
// and a second list is what this whole change exists to remove.
func bootstrapDefaultTools() []string { return toolset.Default() }

// policyWithDefaultTools returns `existing` with default_tools set to `tools`, COPIED rather than
// mutated.
//
// THE COPY IS THE POINT, AND SO IS THE MERGE. `UpdateProjectConfigPolicy` is an assignment, not a
// merge (see cmd/cli/internal/admin/admin.go's set-policy comment), so a PATCH carrying only
// default_tools deletes approvers, pool, and allowed_models. On a first bring-up the policy is empty
// and nothing is lost, which is exactly why the defect would ship: it appears on the SECOND run, and
// what it drops includes the approver allow-list — a surface an operator closed on purpose.
func policyWithDefaultTools(existing map[string]any, tools []string) map[string]any {
	merged := make(map[string]any, len(existing)+1)
	for k, v := range existing {
		merged[k] = v
	}
	merged["default_tools"] = tools
	return merged
}
```

Then wire the write into the bring-up sequence, **after** the project exists and **before** the warning check at `:1411`:

1. `GET /v1/projects/{bootstrapProjectID}` and decode `config_policy` into a `map[string]any` (an absent policy decodes to nil — `policyWithDefaultTools` accepts that).
2. If the decoded policy already has a non-empty `default_tools`, **skip the write**: an operator who set their own baseline must not have it overwritten by a re-run.
3. Otherwise `PATCH /v1/projects/{bootstrapProjectID}` with body `{"config_policy": policyWithDefaultTools(existing, bootstrapDefaultTools())}`.

Reuse the CLI's existing request helper rather than building a fresh HTTP client — `grep -n "func (c \*client) do\|func body(" cmd/cli/internal/admin/admin.go` shows the shape the admin path uses.

- [ ] **Step 5: Keep the warning, and say why**

Do NOT delete the warning. Narrow its comment so a reader knows when it can still fire.

> **CORRECTED 2026-08-04, during execution.** This step originally supplied this comment text:
>
> > *"…this check runs against whatever project the key actually writes to. An operator pointed at a project this bring-up did not create still gets told."*
>
> **That was false and the implementer refused to write it.** `emptyToolBaselineWarning` contains `if p.ID != "prj_local" { continue }` (`up.go:1496-1499`), so it can never fire for another project. The sentence would have claimed a property the code does not have, on the exact line a reader would audit it — the defect this tree's CLAUDE.md records. Do not restore it.
>
> The accurate narrowing, and what the implementer wrote instead: the warning survives because it **re-reads the server after the write**, so it still fires when the grant stood down — the write failed, the project was absent, or an operator's own baseline was left in place.

- [ ] **Step 6: Run the tests and the package**

Run: `go test ./cmd/cli/internal/stack/ -run 'TestBringUpGrants|TestTheGrant' -v`
Expected: all four PASS.

Run: `go build ./cmd/cli/...`
Expected: exit 0.

- [ ] **Step 7: Commit**

```bash
git status --porcelain cmd/cli/internal/stack/
git add cmd/cli/internal/stack/up.go cmd/cli/internal/stack/up_default_tools_test.go
git commit -m "feat(cli): bring-up grants the default tool set instead of warning that it did not"
```

---

## Task 4: Slack cannot drift from the canonical list

This is the second half of the user's requirement. `apps/slack-bot/internal/relay/relay.go:433-448` holds `toolTitles`, a 15-entry map from tool name to human phrase. It is not a grant list — it is a *display* list — but it is keyed by the same names, and the fallback at `:453` titles an unmapped tool **with its raw name**. So a tool added to `Default()` with no title silently degrades every Slack card that shows it.

**Files:**
- Create: `apps/slack-bot/internal/relay/tool_titles_test.go`

**Interfaces:**
- Consumes: `toolset.All()` from Task 1; `toolTitles` (unexported, same package)
- Produces: nothing — this is a guard

- [ ] **Step 1: Confirm the map and the fallback**

Run: `sed -n '429,460p' apps/slack-bot/internal/relay/relay.go`

Confirm `toolTitles` is a package-level `map[string]string` and that the fallback uses the raw name. **The test must be in `package relay`** (not `relay_test`) to reach an unexported map.

- [ ] **Step 2: Write the failing test**

Create `apps/slack-bot/internal/relay/tool_titles_test.go`:

```go
package relay

import (
	"testing"

	"github.com/palgroup/palai/packages/toolset"
)

// TestEveryCanonicalToolHasAHumanTitle binds the two lists that would otherwise drift apart. The
// relay's fallback titles an unmapped tool with its RAW NAME, so a tool added to the canonical set
// with no phrase here does not fail — it degrades, and it degrades on the surface a human reads.
// This is the "no Slack copy that can disagree" requirement, asserted rather than trusted.
func TestEveryCanonicalToolHasAHumanTitle(t *testing.T) {
	for _, tool := range toolset.All() {
		title, ok := toolTitles[tool]
		if !ok {
			t.Errorf("%q is granted by a bring-up but has no title: a Slack card will show the raw name", tool)
			continue
		}
		if title == "" {
			t.Errorf("%q maps to an empty title", tool)
		}
		if title == tool {
			t.Errorf("%q maps to itself, which is what the fallback already does", tool)
		}
	}
}

// knownNonGrantTitles are the titles that legitimately sit outside the canonical grant set, each one
// NAMED rather than waved through by a blanket rule. palai.slack.search is Slack-specific and resolves
// through the Slack lookup path; palai.task and palai.todo are defined (tools/task.go:14,18) but are
// not in toolbroker.New; palai.web.search and palai.fs.write are titled here and registered nowhere.
//
// THE DAY ONE OF THESE IS REGISTERED AND GRANTED it moves to the canonical list and comes out of this
// map — the map shrinking is the signal that the surface grew.
var knownNonGrantTitles = map[string]bool{
	"palai.slack.search": true,
	"palai.task":         true,
	"palai.todo":         true,
	"palai.web.search":   true,
	"palai.fs.write":     true,
}

// TestNoTitleIsOrphaned sweeps the OTHER direction, and the direction is the whole point: a walk over
// the canonical list finds tools with no title, and ONLY this walk finds a title left behind by a tool
// that was renamed or removed. One direction cannot find both.
func TestNoTitleIsOrphaned(t *testing.T) {
	canonical := map[string]bool{}
	for _, tool := range toolset.All() {
		canonical[tool] = true
	}
	for tool := range toolTitles {
		if canonical[tool] || knownNonGrantTitles[tool] {
			continue
		}
		t.Errorf("toolTitles carries %q, which is neither granted nor a named exception: a renamed or "+
			"removed tool leaves its title behind, and this is the only check that sees it", tool)
	}
}
```

- [ ] **Step 3: Run test to verify the first one's behavior**

Run: `go test ./apps/slack-bot/internal/relay/ -run 'TestEveryCanonicalToolHasAHumanTitle|TestNoTitleIsOrphaned' -v`

Expected at plan time: **PASS** — all ten canonical names already have titles (`relay.go:433-448` covers every one). That is a real result, not a vacuous one: Step 4 proves it.

- [ ] **Step 4: Prove the guard bites (perturbation)**

Delete the `"palai.knowledge.retrieve": "Searching the knowledge base",` line from `toolTitles` and re-run Step 3.
Expected: `TestEveryCanonicalToolHasAHumanTitle` FAILS naming `palai.knowledge.retrieve`.

**If it stays GREEN, the test is not reaching the map — check the package clause.** Restore the line and confirm green.

- [ ] **Step 5: Commit**

```bash
git status --porcelain apps/slack-bot/internal/relay/
git add apps/slack-bot/internal/relay/tool_titles_test.go
git commit -m "test(relay): every granted tool has a human title, so Slack cannot drift from the grant list"
```

---

## Task 5: Prove a run is actually offered the tools

Tasks 1-4 prove the list is canonical, resolvable, written, and titled. None of them proves a **model** sees it. This task closes that gap — the difference between "a route exists" and "a field is accepted" that this tree has paid for before.

**Files:**
- Test: `apps/control-plane/internal/execution/config_test.go` (add to the existing effective-set tests)

**Interfaces:**
- Consumes: `toolset.Default()`; the effective-set computation in `execution/config.go:120-137`

- [ ] **Step 1: Read the neighbouring test**

Run: `sed -n '68,95p' apps/control-plane/internal/execution/config_test.go`

Measured on 2026-08-04: the file is `package execution` (no prefix on `Resolve`), the entry point is `Resolve(ResolveInput{...}) ConfigSnapshot`, the effective names land on `.Tools`, and a `hasTool(list, name)` helper already exists. **No new helper is needed** — call the real `Resolve` directly.

- [ ] **Step 2: Write the failing test**

Add to `config_test.go` (add `"github.com/palgroup/palai/packages/toolset"` to its imports):

```go
// TestTheBringUpBaselineSurvivesTheRevisionCeiling is the end this plan exists for. Every other guard
// proves a property of the LIST; this one proves the list survives the computation that consumes it.
//
// IT CALLS Resolve RATHER THAN RE-DERIVING THE INTERSECTION, because a test that recomputes the rule
// it is checking passes against its own arithmetic and would have stayed green through the very
// change that emptied every operator's tool set.
func TestTheBringUpBaselineSurvivesTheRevisionCeiling(t *testing.T) {
	baseline := toolset.Default()

	// A revision that declares the same surface: the ceiling admits all of it.
	granted := Resolve(ResolveInput{
		DeploymentModel:    "m",
		ProjectTools:       baseline,
		AgentRevisionID:    "arev_1",
		AgentRevisionTools: baseline,
	})
	if len(granted.Tools) != len(baseline) {
		t.Fatalf("effective tools = %v, want all %d baseline tools", granted.Tools, len(baseline))
	}
	for _, want := range baseline {
		if !hasTool(granted.Tools, want) {
			t.Errorf("the baseline tool %q did not survive a revision that declares it", want)
		}
	}

	// A revision declaring NOTHING (non-nil empty) empties the set. Asserted so the emptiness is a
	// KNOWN consequence of the ceiling rather than a surprise found in a live run — this is the exact
	// shape of the outage this plan closes: a run completes, answers in one step, and calls nothing.
	empty := Resolve(ResolveInput{
		DeploymentModel:    "m",
		ProjectTools:       baseline,
		AgentRevisionID:    "arev_1",
		AgentRevisionTools: []string{},
	})
	if len(empty.Tools) != 0 {
		t.Fatalf("a revision declaring no tools yielded %v, want none", empty.Tools)
	}
}

// TestAnEmptyProjectBaselineEmptiesTheEffectiveSet is the defect itself, pinned. This is what
// `palai up` produced between commit 1e5fc63e and this plan, and it must never again be reachable
// without a test naming it.
func TestAnEmptyProjectBaselineEmptiesTheEffectiveSet(t *testing.T) {
	got := Resolve(ResolveInput{
		DeploymentModel:    "m",
		ProjectTools:       nil,
		AgentRevisionID:    "arev_1",
		AgentRevisionTools: toolset.Default(),
	})
	if len(got.Tools) != 0 {
		t.Fatalf("effective tools = %v on an empty baseline, want none: the ceiling intersects LAST", got.Tools)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./apps/control-plane/internal/execution/ -run 'TestTheBringUpBaselineSurvivesTheRevisionCeiling|TestAnEmptyProjectBaselineEmptiesTheEffectiveSet' -v`
Expected: FAIL to compile until the `toolset` import is added; then both PASS.

**If `TestAnEmptyProjectBaselineEmptiesTheEffectiveSet` FAILS**, the ceiling no longer intersects last — stop. That is a change to §2's premise and the rest of this plan needs re-reading before it is trusted.

- [ ] **Step 4: Confirm both pass**

Run: `go test ./apps/control-plane/internal/execution/ -run 'TestTheBringUpBaselineSurvives|TestAnEmptyProjectBaseline' -v`
Expected: both PASS.

- [ ] **Step 5: Run every guard this plan touched**

```bash
go test ./packages/toolset/... \
        ./apps/control-plane/internal/execution/... \
        ./apps/slack-bot/internal/relay/... \
        ./cmd/cli/internal/stack/...
```
Expected: all PASS. **Diff this against the two FAILs from §1** — that diff is the plan's deliverable.

- [ ] **Step 6: Build the tagged trees the plain run misses**

This tree ships seven build tags and a plain `go vet` covers three. A stale caller in a tagged test is invisible until it is not:

```bash
go vet -tags="component live security" ./...
```
Expected: exit 0.

- [ ] **Step 7: Commit**

```bash
git status --porcelain apps/control-plane/internal/execution/
git add apps/control-plane/internal/execution/config_test.go
git commit -m "test(execution): the bring-up baseline survives the revision ceiling"
```

---

## §5 — Definition of done

- [ ] `go test ./packages/toolset/...` PASSES (4 tests)
- [ ] `TestEveryDefaultToolResolves` and `TestThePublishToolsAreTheirOwnListAndNeitherPublishes` PASS
- [ ] `TestTheSearchToolIsInTheDefaultSet` is **deleted** — decided 2026-08-04. Its premise was a Slack default list, and `1e5fc63e` removed that world. Its report was TRUE and is preserved in §7: nothing grants `palai.slack.search` today.
- [ ] `TestNoDefaultToolHasSideEffects` is **renamed `TestNoDefaultToolPublishes`**, keeps its `publish.` clause — now over `Default()` AND `Repository()` — and **drops** its file/shell clause. Decided 2026-08-04. The file/shell prohibition contradicts this plan's purpose, and its stated remedy (`SLACK_AGENT_TOOLS`) had exactly one occurrence in the tree: that test's own error string. The rename is load-bearing, not cosmetic — `Default()` grants `workspace.shell`, so the old name would print `--- PASS` for a claim the test no longer makes.
- [ ] `TestTheGrantPreservesEveryOtherPolicyKey` PASSES — the re-run does not clear `approvers`
- [ ] `TestAnEmptyProjectBaselineEmptiesTheEffectiveSet` PASSES — the outage's own shape is pinned
- [ ] `TestEveryCanonicalToolHasAHumanTitle` PASSES — Slack carries no name the grant list does not
- [ ] `grep -rn "slackDefaultTools\|slackRepositoryTools\|slackPublishTools" --include="*.go" .` returns hits ONLY in `tests/uat/` prose (the released bundle's wording, which is history and must not be edited)
- [ ] `go vet -tags="component live security" ./...` exits 0
- [ ] Both perturbations (Task 2 Step 5, Task 4 Step 4) were observed RED and then restored

---

## §5.5 — Execution record (2026-08-04) and what is still open

**Shipped in 14 commits.** All five tasks complete, each with a task review; a final whole-branch review returned "with fixes" and those fixes landed in `b9434879` + `b60c2cd8`.

**The plan was wrong three times and execution caught all three** — recorded because the corrections are more useful than the plan's original text:

1. §1 said two tests were RED. **Four** were: every test in the guard file reached the deleted regex helper.
2. Task 2's steps assumed a Slack default list still existed. Three contradictions surfaced, went to the human, and were ruled on — see §5.
3. Task 3 Step 5 supplied a comment claiming the warning "still fires for a project this bring-up did not provision". `emptyToolBaselineWarning` filters `if p.ID != "prj_local" { continue }`, so it cannot. The implementer refused to write it.

A fourth false claim — that a bring-up ADDS `Repository()`/`Publish()` — survived into seven comment sites and was caught by the final review. See §7 item 0b.

**Still open, in priority order:**

- [ ] **No live `palai up` transcript exists.** The grant's PATCH authorization for the bootstrap key is exercised nowhere. If the server refuses it, the feature stands down silently and only the warning speaks. **The next bring-up must look for the line naming the tools `prj_local` now grants and treat its absence as a bug.**
- [x] ~~Re-run §5's gate on a compiling tree~~ — **partly done, 2026-08-04, after the refactor landed.** `go build ./...` now exits 0, and all four packages this plan touched pass in isolation (`packages/toolset`, `execution/tools`, `slack-bot/relay`, `cli/stack`). The stale-symbol grep is clean: `slackDefaultTools`/`slackRepositoryTools`/`slackPublishTools` survive ONLY in `tests/uat/` prose, which §5 requires and forbids editing.
- [x] ~~The gate's third leg is still RED~~ — **CLOSED 2026-08-04.** The concurrent refactor landed its tagged callers; `go vet -tags="component live security" ./...` now exits **0** with zero errors. All three legs of §5's gate are green on the live tree.

<details><summary>What it looked like while it was red</summary>

**The gate's third leg was RED, and not from this plan.** `go vet -tags="component live security" ./...` exits **1** with **17** errors — every one of them a tagged test still calling the pre-refactor signatures (`tenant.Organization undefined`, `too many arguments in call to h.writer.Read`). **Zero name a file this plan touched** (checked by grepping the vet output for `toolset|default_set_test|tool_titles_test|up_default_tools_test|config_test`). This is the tree's own recorded trap: a plain `go build` covers the untagged world and leaves tagged tests broken but invisible. Re-run once whoever owns the refactor finishes the tagged callers.
</details>

- [ ] `grantDefaultToolBaseline`'s three stand-down paths have no test; an `httptest` server would cover GET-fail / skip / PATCH-fail. The destructive direction is structurally closed (a shape mismatch 400s and stands down), so this is coverage, not risk.

---

## §6 — What this plan does NOT do

Named so a later reader does not mistake absence for oversight:

- **It does not run `palai up` end to end.** Task 5 proves the computation, not the deployment. A live bring-up on a machine configured the way the docs describe is the honest final check, and it belongs to whoever next brings a stack up.
- **It does not register `palai.task` / `palai.todo`.** They are defined (`tools/task.go:14,18`) and absent from `toolbroker.New` — a one-line fix, but a different claim with its own guard, and granting them before registering them would put an unresolvable name in the baseline.
- **It does not touch the tool surface itself.** No Edit tool, no Grep, no Glob. See §7.
- **It does not wire a grant path for `toolset.Repository()` or `toolset.Publish()`.** `palai up` writes
  `Default()` only (`bootstrapDefaultTools()`, `cmd/cli/internal/stack/up.go:1414`). See §7 item 0b for
  the measurement and what remains.

---

## §7 — The next plan (not this one)

**0. `palai.slack.search` is defined and granted by nothing — found by this plan's own Task 2, 2026-08-04.**

The deleted `TestTheSearchToolIsInTheDefaultSet` was reporting a true fact, and deleting the test does not repair it. Measured:

```bash
grep -rn "palai\.slack\.search" --include="*.go" . | grep -v _test
# tools/slack_search.go:31        — the definition
# slack-bot/.../relay.go:444      — a card title
# tests/uat/evidence_tools_memory.go:275 — a released bundle's recorded JSON
```

No grant path. This is E21 T5's defect returning: a tool mounted, tested, and dead. It belongs to whoever restores Slack's own tool grant — **not** to `palai up`'s generic baseline, which is why the test could not stay. Do not close this by adding the name to `toolset.Default()`: a Slack-less stack would carry a name the broker's static set cannot resolve.

**0b. `toolset.Repository()` and `toolset.Publish()` are two more names with no grant path — found in this plan's final review, 2026-08-04.**

Task 1 built three canonical lists so a future conditional grant (repository-bound coding tools, publication tools) would have one place to name; Task 3 wired only `Default()` into `palai up`. That was the scoped decision (§1's `bootstrapDefaultTools` returns `toolset.Default()`, not `toolset.All()`), but three comments written during implementation stated the opposite — that a bring-up "ADDS" or "binds" the other two lists — and were corrected as part of this same review pass. Measured:

```bash
grep -rn "toolset\.Repository()\|toolset\.Publish()" --include="*.go" . | grep -v _test
# (no output) — every caller of Repository() and Publish() is a test
```

This is the same shape as item 0: a canonical name that exists, resolves, and titles cleanly, but that nothing in `palai up` grants. Unlike `palai.slack.search`, these two lists are not stray — `TestEveryDefaultToolResolves` and `TestNoDefaultToolPublishes` (`apps/control-plane/internal/execution/tools/default_set_test.go`) already prove they resolve and stay side-effect-scoped, so the remaining work is narrower: decide what "bound a repository" means operationally in `cmd/cli/internal/stack/up.go`, and add the conditional write. Do not close this by folding `Repository()`/`Publish()` into `Default()`: a run with no repository would be granted `palai.workspace.commit` and a run with no publication decision would be granted the publish tools unconditionally — the same posture change `TestNoDefaultToolPublishes` exists to catch.

---

The research that produced this plan measured six gaps. This plan closes the first because nothing else is observable until it is closed. The rest are a second plan, in dependency order:

1. **`text_editor_20250728`** — adopt Anthropic's client-side text editor (exact-string `str_replace`, `view` with ranges). Today the only write is whole-file replace (`tools/file.go:105-127`). **Blocker to measure first:** `adapters/models/provider_two/adapter.go:374` serialises every tool as a custom tool (`{name, input_schema}`) and never writes a `type` field — an Anthropic-defined tool needs `type` and no `input_schema`, so the adapter needs a branch before this is reachable. `provider_one` is OpenAI-shaped (`adapter.go:27`) and cannot carry these at all.
2. **Grep and Glob** — no content search and no pattern matching exist; the need falls to `palai.workspace.shell` (60s, irreversible).
3. **Schema validator types** — `packages/tool-broker/conformance_math.go:50-99` knows object/integer/number/string only. No array, no boolean, no enum, which is why `argv` is untyped (`tools/shell.go:29-36`).
4. **System prompt** — `engines/reference/src/palai_engine/context.py:13-16` is 23 words with no agentic discipline. Legitimate source: the Agent SDK's `claude_code` preset, or MIT `anthropics/claude-code-action`. **Not** the leaked npm source map.
5. **Parallel tool calls** — the engine emits N frames (`loop.py:204-220`); the controller executes them serially with no goroutine (`orchestrator.go:757-831`).
