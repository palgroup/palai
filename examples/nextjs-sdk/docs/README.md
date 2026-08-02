# The demo UI, in six screenshots — and the three screenshots that came before it

The `ui-*.png` set is the current demo: AI Elements wired to Palai, a repository picker, and the
approval standing between the agent and somebody's branch. The `chat-*.png` set below it is the
hand-built chat that preceded it, kept because two of its findings are still true.

Every screenshot in this directory was captured against a **live** stack — a native control plane with
workspaces on, a real provider, a real clone — by driving the page in a browser. None was captured by
calling a route. They live here rather than in `.shots/`, which is gitignored scratch nobody else can
read.

---

## The screen

The repository is the subject and the chat is its control surface, because the interesting thing to
watch is not a chat bubble — it is a file appearing in a tree, a shell command running, a commit
forming, and a human standing between an agent and a branch.

    ┌──────────────┬────────────────────────────────────┬──────────────────┐
    │ picker 256px │  the repository (file tree, diff,  │  chat            │
    │ rgb(13,13,13)│  shell transcript, evidence)       │  420px           │
    └──────────────┴────────────────────────────────────┴──────────────────┘

Below 1280px the chat moves under the repository; below 700px the rail stacks on top
(`ui-6-narrow-700px.png`). Focus rings are never removed and `prefers-reduced-motion` is honoured.

### `ui-1-repo-picked-clone-listed.png` — pick a repository, get that repository

`octocat/Hello-World` picked in the rail. The model wrote `repo/HELLO.md` and ran
`git -C repo status --short` **inside the run's clone**, exit 0, output `?? HELLO.md`. The tree shows
the added file, the diff is the run's real `patch` artifact, and the footer names the artifact ids so a
reader can `curl` the same bytes.

### `ui-2-approval-deployment-app.png` and `ui-3-approval-binding-credential.png` — WHERE, and AS WHOM

The same approval under the two identities Palai can publish under. Both rows carry the remote, the
branch, the base and the head SHA; they differ only in the last line:

    as   the deployment's GitHub App                    (binding has no connection_ref)
    as   this repository binding's own credential  demo-local-token

**None of those six facts is on the event stream.** `approval.requested.v1` for a publication carries
`{publication_id, operation, branch, request_hash, display}` and nothing else — not the remote, not the
base, not the head SHA, not the credential, and *not even the approval id*. They come from a
server-side join to `GET /v1/approvals`. Before this branch the chat read `d.approval_id` off the frame,
got `null`, and its Approve button answered its own 400 without ever reaching the control plane.

An empty `connection_ref` is rendered as *"the deployment's GitHub App"* rather than a blank, because it
is a different named identity and not a missing value.

### `ui-4-approved-but-push-not-confirmed.png` — an approval that applied, and a push that did not

The most important screenshot here. Approve was pressed, the relay returned 200,
`approval.approved.v1` arrived with a command id, the publications row moved to `approved`, and the
parked run woke and completed. **Nothing pushed.** The row's `receipt` stayed `NULL` and the git server
received no `git-receive-pack`.

    repositoryPublisherFromEnv (main.go:1231) returns nil unless PALAI_GITHUB_APP_ID,
    PALAI_GITHUB_APP_INSTALLATION_ID and PALAI_GITHUB_APP_PRIVATE_KEY_FILE are ALL set,
    and a nil publisher makes pumpApprovedPublications a no-op.

That function's own comment says the failure is *"INDISTINGUISHABLE from success on every surface a
human looks at"*, and reasons that a stack configuring nothing "never asked to publish". **A binding
carrying a `connection_ref` HAS asked**, under a named identity — and the credential-resolving
publisher is constructed *inside* the GitHub-App gate, so a binding's own credential cannot publish on
a stack with no App. The demo therefore watches the pair: `approval.approved.v1` is remembered,
`publication.published.v1` clears it, and anything outstanding at the run's terminal becomes the red
notice in this screenshot. An approval that applied is not a push that happened.

### `ui-5-tool-call-name-gap.png` — the tool card, with its hole visible

AI Elements' `ToolHeader` wants a name. Palai has none to give:

    tool_call.executing.v1 -> {run_id, replay_class, tool_call_id}
    tool_call.completed.v1 -> {run_id, tool_call_id}

Handed nothing usable, `ToolHeader` renders an **empty span** — which reads as a rendering bug — and a
placeholder reads as a tool actually called that. So the title is an explicit sentence, the body says
what is missing, and `ToolInput` is given the frame's *real* payload instead of a fabricated one. What
the frame does carry is shown: the call id and `replay_class: irreversible`.

Closing this is a control-plane change (put the name on the frame, or join the `tool_calls` ledger from
the events API), not something the adapter can fix by looking somewhere else.

### `ui-6-narrow-700px.png` — the same screen at 700px

---

## Two things a coding session hits on its first try

Both measured live on 2026-08-02, both worked around in the demo's opening turn, neither fixed here.

1. **`palai.workspace.shell` starts in the allocation root, not the clone.** `host/exec.go:152` sets
   `c.Dir = cmd.WorkspaceRoot`; the clone is one level down at `<root>/repo`. The tool's schema has no
   `cwd` field *and* no `additionalProperties: false`, so an invented one is accepted and ignored.
   Without a hint, gpt-4o-mini ran `git status`, got "not a git repository", ran **`git init` at the
   root**, and committed into a brand-new repository beside the clone. Nothing errored.

2. **The clone has no git identity.** `prepare.go:93-133` does init + fetch + checkout and never sets
   `user.email`/`user.name`, so the first `git commit` of every session dies with *"Author identity
   unknown"*. Observed twice; both times the model recovered by running `git config --global`, writing
   the **operator's** global git config on an unsandboxed host as a side effect of being asked to
   commit.

---

# The three screenshots that came before

All three were captured against a **live** stack — a native control plane with workspaces on, a real
provider, and a real clone of `octocat/Hello-World` — by driving the page in a browser, not by calling the
route. They live here rather than in `.shots/`, which is gitignored scratch.

## `chat-plain-turn.png` — a session with no repository

`useChat` from `@ai-sdk/react`, unmodified, talking to `/api/chat`. A session is opened on the first turn
and its id is shown; the answer comes back from a real provider. Nothing about this page is Palai-specific
— that all lives in the route handler.

## `chat-coding-turn.png` — a session with a repository

The same chat with a repository binding. The model ran a shell command **inside the run's cloned
workspace** and answered from its real output: *"The "repo" directory contains a .git subdirectory and a
README file."*

The tool call is rendered from the journal, and the line under it is the honest part:

> the tool's name is not carried on Palai's event stream

That is measured, not a placeholder. `tool_call.executing.v1` carries `{run_id, replay_class,
tool_call_id}` and `tool_call.completed.v1` carries `{run_id, tool_call_id}` — the name, the arguments and
the result live on the `tool_calls` ledger and the events API does not join it. The AI SDK's tool part
wants all three. **This is the one place Palai cannot drive a generic tool-rendering UI**, and closing it
is a control-plane change (put the name on the frame) rather than something the adapter should paper over
with a second lookup.

## `chat-tool-error-parks-the-run.png` — a tool error, shown rather than hidden

The model was asked to read a file that does not exist. The tool call is stuck at `running`, and the chat
says so in red:

> A tool call could not be resolved and this run is now parked waiting for a human. It will not continue on
> its own. (This is what a tool returning an error looks like from the chat.)

**This is a real defect and the screenshot exists to show it, not to hide it.** A workspace tool whose
`Exec` returns a Go error aborts the attempt; the ledger row is left unresolved, recovery escalates it to
`manual_resolution`, and nothing ever resolves it. A coding agent guessing a filename is the most ordinary
thing a coding agent does, so this is reachable within a few turns.

A separate task owns the fix. When it lands, this notice stops appearing with no change to the demo — the
adapter already renders whatever the journal says. That the two are separable is the point.

## Running it yourself

    cd examples/nextjs-sdk
    cat > .env.local <<'EOF'
    PALAI_BASE_URL=http://127.0.0.1:<api-port>
    PALAI_API_KEY=<key>
    EOF
    pnpm exec next build && pnpm exec next start -p 3100

`.env.local` is the path this file tells you to use, so it is the path the demo was last brought up on
— rather than an environment a harness injects and no operator ever has. (If your key contains a `$`,
note that dotenv expands it; this tree has been bitten by exactly that with a scrypt hash.)

`/chat` is the demo. `/compare` runs one operation through `@palai/sdk` and through a bare `fetch`
side by side, single-shot or in a shared session.

## The tests

    CI=1 pnpm exec playwright test     # 13 specs
    pnpm exec tsc --noEmit             # clean

Every form on `/chat` is driven by clicking the real control and filling the real field; none of the
specs posts to `/api/*` to stand in for a submit, and both forms assert `method="post"` **on the DOM**.
The deterministic upstream is `tests/fake-control-plane.mjs`, whose coding block replays shapes measured
on the live stack — including the three absences above, because a fake more generous than production
would delete the very defects these screens report.

Note for anyone extending this: `next build` prints **"Skipping validation of types"** on this project,
so it is not a type gate. `tsc --noEmit` is, and it only started working when `baseUrl` came out of
`tsconfig.json` (removed in typescript 7).

The key is read server-side only (`lib/palai.ts`, `lib/raw.ts`, both `server-only`). It appears in no page
and in no static chunk — the browser talks only to this app's own routes, because Palai has no
browser-direct token by design.

## The iOS chat (E30)

Captured by driving the page — the same script fills the real textarea and clicks the real submit
button, as the specs do. The upstream is `tests/fake-control-plane.mjs`, whose tool results are
**bytes captured from the real tools** (`tests/fixtures/*.txt`, from Xcode 26.6 against a real iOS
Simulator destination on a Mac). So the failing build, the test counts and the device list on these
screens are what `xcodebuild` and `xcrun simctl` actually printed, not prose written to look like it.

| shot | what it shows |
| --- | --- |
| `ios-1-auto-approve-both-off.png` | A session as it is BORN: both halves off, and the panel says so — "Every gated call and every push parks the run and waits for you." |
| `ios-2-chat-with-ios-cards.png` | The whole screen: repo on the left, what changed in the middle, the chat driving it on the right. |
| `ios-3-build-failed-file-and-line.png` | A failing `xcodebuild`: **BUILD FAILED**, 1 error, `Greeter.swift:5:49`, and the compiler's own sentence. The tool cards above it are labelled `palai.workspace.shell` — the name the frames did not carry until E30 T2. |
| `ios-4-raw-output-behind-the-summary.png` | The disclosure open. Every parser can miss; a summary with no way back to the bytes turns a miss into a screen that quietly shows nothing. |
| `ios-5-test-results.png` | `2 passed`, with each case and its duration, off 934 bytes of real `xcodebuild test` output. |
| `ios-6-simulator-devices.png` | `simctl`, the devices and their states — `iPhone 17 Pro … Booted`. |
| `ios-7-tools-armed-pushes-not.png` | **THE SPLIT.** Build commands ON, pushes and pull requests still OFF, and a publication in the same run still asking a human for a decision. This is the screen the two columns exist for. |
| `ios-8-both-armed-warning-visible.png` | Both armed, with the publication half's own warning: "Writes to the repository with NO human in the loop. That write outlives this session." |

### And the same screens against the LIVE stack

`ios-live-*.png` are the same UI driving the real control plane on this Mac: a real repository binding,
a real clone, **Sonnet 5** as the model, and real `xcodebuild` / `xcrun simctl` on the host.

| shot | what it shows |
| --- | --- |
| `ios-live-1-armed-tools-only.png` | Build commands armed mid-run, pushes still off, under the principal the control plane stamped (`key:key_local`). |
| `ios-live-2-real-xcodebuild-succeeded.png` | The finished turn. |
| `ios-live-3-build-card.png` | **`** BUILD SUCCEEDED **`** from a real `xcodebuild` against `platform=iOS Simulator,name=iPhone 17 Pro`, with the real DerivedData path and the real DVT stderr — and beside it `2 passed`, `testGreetsByName` 0.002s, `testGreetsEmpty` 0.001s from a real `xcodebuild test`. |
| `ios-live-4-test-card.png` | The test card on its own. |
| `ios-live-5-simulator-card.png` | `simctl`, listing this Mac's actual simulators. |

Measured on that run through the endpoint this work added:

    GET /v1/responses/{id}/tool-calls
      3 rows, all palai.workspace.shell, results 38016 / 43443 / 337 bytes
      ** BUILD SUCCEEDED **   ** TEST SUCCEEDED **   Executed 2 tests, with 0 failures
    events.payload->>'tool_name'  ->  palai.workspace.shell x3

### The ceiling that had to be cleared first, because it will come back

The first live attempt did NOT work, and the reason is worth keeping:

    xcodebuild … build   -> exit 134, Abort trap: 6
    DVTDeveloperPaths: Failed to get length of DARWIN_USER_CACHE_DIR from confstr(3),
                       error = "Input/output error"
    xcrun simctl list devices -> exit 127, command not found

The shell tool was already running on the host — the probe returned the full host `PATH`, `ls`'d
`/usr/bin/xcodebuild` and `/usr/bin/xcrun`, and resolved `xcode-select -p`. But `whoami` returned
**`501`**, the numeric uid: `getpwuid()` was failing, and the same fault makes
`confstr(_CS_DARWIN_USER_CACHE_DIR)` fail, which is exactly what Xcode's DVT layer aborts on.

It was NOT the minimal environment `adapters/sandboxes/host/exec.go` builds. A real build under exactly
those variables succeeds:

    env -i PATH="$PATH" LANG=… HOME=<scratch> TMPDIR=<scratch> \
      xcodebuild -scheme PalaiDemo -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build
    -> ** BUILD SUCCEEDED **      (and `whoami` -> salih)

The control-plane process was `ppid 1, sess 0` — orphaned onto launchd — so its children had no
per-user bootstrap namespace. Restarting it from a live session fixed it: `whoami` -> `salih`, and the
builds above are the result. **`PALAI_SHELL_NATIVE=unsandboxed-host` being set is not the same as the
tools working; drive one run and read `whoami` before believing otherwise.**

(Session id alone is not the discriminator — an ordinary interactive shell is also `sess 0` and works.
The bootstrap namespace is, and its effect is observable without being attachable after the fact.)
