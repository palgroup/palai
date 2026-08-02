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
