# The demo, in three screenshots

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
    PALAI_API_KEY=<key> PALAI_BASE_URL=<control-plane> npx next start -p 3100

`/chat` is the chat above. `/compare` runs one operation through `@palai/sdk` and through a bare `fetch`
side by side, single-shot or in a shared session.

The key is read server-side only (`lib/palai.ts`, `lib/raw.ts`, both `server-only`). It appears in no page
and in no static chunk — the browser talks only to this app's own routes, because Palai has no
browser-direct token by design.
