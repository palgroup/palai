# From a fresh install to a Slack agent that answers

One page, in order, from an empty machine to a bot that replies in a thread. Every step is a command you
run; nothing here asks you to look up an id the stack could have told you.

This page exists because the path used to have two invisible cliffs, and both were only ever crossed by
someone who already knew (E21 T2, plan §3.6 D5 and §4 T2). They are described in §7 so you can recognise
them if you are reading an older deployment's output.

---

## 1. What you need

| | |
|---|---|
| Docker, and enough disk for four containers | `palai local doctor` tells you if not |
| A live model credential | `PALAI_PROVIDER_ONE_API_KEY` in `.env.local` — `palai up` refuses to bring up a stack that would silently run the fake adapter |
| A Slack app | https://api.slack.com/apps → **Create New App → From a manifest**, pasting `deploy/slack/app-manifest.yaml` |
| Three values off that app | signing secret, bot token, app-level token (below) |

Nothing else. In particular you do **not** need `SLACK_AGENT_REVISION_ID` or `SLACK_PRINCIPAL_ID` — those
are Palai-side ids that no Slack page will ever show you, and the bring-up resolves them itself (§4).

## 2. The three Slack values, and where each one is

| Variable | Where in https://api.slack.com/apps | Shape |
|---|---|---|
| `SLACK_SIGNING_SECRET` | App → **Basic Information** → App Credentials → Signing Secret | opaque hex |
| `SLACK_BOT_TOKEN` | App → **OAuth & Permissions** → Bot User OAuth Token (exists only after Install to Workspace) | `xoxb-…` |
| `SLACK_APP_TOKEN` | App → **Basic Information** → App-Level Tokens → Generate, scope `connections:write` | `xapp-…` |
| `SLACK_TEAM_ID` | Slack → workspace menu → About → or the `T…` in any permalink | `T…` |

Two more, both optional and both worth setting deliberately:

- `SLACK_APPROVER_IDS` — comma-separated `U…` ids allowed to click **Approve**. Unset means **nobody**
  can, deny-by-default, and `palai up` says so out loud in its final report rather than letting you
  discover it as a button that does nothing.
- `SLACK_ALLOWED_CHANNELS` — comma-separated `C…` ids. Unset means no channel restriction, which is the
  production default. Do **not** put your test channel here (see `SLACK_TEST_CHANNEL`, which belongs to
  the live test harness and is a different thing entirely).

## 3. Write them into `.env.local`

```sh
cat >> .env.local <<'EOF'
PALAI_PROVIDER_ONE_API_KEY=...
SLACK_TEAM_ID=T01234567
SLACK_SIGNING_SECRET=...
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...
SLACK_APPROVER_IDS=U0123ABCD
EOF
```

These are values, not handles. `palai up` reads them, stores each one through `POST /v1/secret-refs`
(envelope-encrypted at rest), and registers the workspace against the **handles** — no credential is ever
put in `argv`, an environment variable a `docker inspect` would show, or a log.

## 4. Bring it up

```sh
palai up
```

Six steps, and the last one is Slack. On a stack with no `SLACK_AGENT_REVISION_ID` /
`SLACK_PRINCIPAL_ID` it prints, just above the `[6/6]` line:

```
        Slack run target: as principal prin_local (this stack's bootstrap-key principal — set
        SLACK_PRINCIPAL_ID to bind a different one), running agent revision arev_…, created and
        published by this bring-up (it carries NO tool set, so Slack runs are single-step until one
        is bound to it)
[6/6] slack     registered slkc_… for team T01234567; 3 secret ref(s) stored and redeemable;
                Socket Mode CONNECTED — …
```

Read that first line. It names what the bring-up resolved on your behalf:

- **The principal** is not new — it is the principal behind this stack's own bootstrap API key, so a Slack
  run is attributed to the same identity your `curl` calls are. Set `SLACK_PRINCIPAL_ID` if you want them
  separated. Explicit configuration always wins, per id.
- **The agent revision** is new, published, and deliberately **empty**: it inherits the deployment's model
  and carries no instructions and no tool set. That is an honest default, not a finished one — see §6.

Re-running `palai up` reuses both. It says `reused` instead of `created`, and it does not mint a second
lineage.

## 5. Prove it

Invite the bot to a channel and mention it:

```
/invite @Palai
@Palai hello
```

The reply arrives in a **thread** under your message. If nothing arrives, §7.

## 6. What this gets you, and what it does not

- **It answers.** A mention opens a run, the run answers in the thread, and a publication proposal gets
  Approve/Reject buttons that only `SLACK_APPROVER_IDS` can press.
- **It has tools, and they are read-only by default.** `palai up` binds `palai.research.fetch` and
  `palai.knowledge.retrieve` to the revision it creates. Both are read-only, side-effect-free, and work on a
  plain compose stack. A run whose effective tool set is EMPTY is single-step; this one is not.

  To give it more — the workspace file and shell tools, commit, push, open a pull request — set
  `SLACK_AGENT_TOOLS` in `.env.local` to the full list you want, by name:

  ```
  SLACK_AGENT_TOOLS=palai.research.fetch,palai.knowledge.retrieve,palai.workspace.file,palai.workspace.shell
  ```

  **Think before you widen this.** A Slack DM is the lowest-friction surface this platform has: anyone in the
  workspace can message the bot, and `im:history` is granted so the panel conversation works. Adding the shell
  tool hands all of those people a shell. That is why it is not the default. `SLACK_AGENT_TOOLS=none` grants
  nothing; a blank value means "use the defaults", so a stray empty line cannot silently disarm the agent.

  Changing the list mints a new agent revision on the next `palai up` — reordering it does not.

- **It can search your workspace's public channels, if you granted the scope.** With `search:read.public` in
  the manifest, a run born from a message can call `palai.slack.search`. Read §9 before you decide it is what
  you want.
- **Starting a new thread starts a new session, and that IS the clear.** Correlation is
  `(team, channel, thread_ts)`, so a fresh top-level message is a fresh session carrying none of the previous
  one's history — no command, no flag, nothing to run. Replying inside an old thread continues that session.
  If you want to reset a session while KEEPING the thread, that is the `clear` command
  (`POST /v1/sessions/{id}/commands` with `{"kind":"clear"}`): it empties the history and leaves the session
  alive, which a new thread cannot do because a new thread is a different session.
- **A long thread no longer breaks it.** History is windowed to a byte budget: the oldest turns fold into a
  deterministic summary line and the newest pass through verbatim. The fold SAYS it happened, because an
  agent quietly forgetting is the failure this was built to stop. Note what it is not — a windower, not a
  summariser. Folded turns lose their detail; the loss is visible rather than silent.

## 7. When it doesn't work

**Symptom: `palai up` finished green and the bot never answers.**

Look at the `[6/6] slack` line and the WARNING block at the bottom of the report. Three distinct states
print differently on purpose:

| What it says | What is true |
|---|---|
| `SKIPPED — …` **plus a WARNING** | `SLACK_TEAM_ID` is set but something else is missing. Nothing was registered. The warning names which value |
| `registered … but NOT CONNECTED: …` | The row exists; Slack never sent `hello`. Almost always the app-level token: check `SLACK_APP_TOKEN` is an `xapp-` with `connections:write`, and that the app is installed |
| `registered … Socket Mode CONNECTED` | Everything is wired. If it still does not answer, the bot is not **in** the channel — `/invite @Palai` |

**Symptom: `palai up` says `SKIPPED` with no warning at all.** You have no `SLACK_TEAM_ID`. That is a
normal state for a stack that is not using Slack, which is why it is not warned about.

**Symptom: the Approve button does nothing.** `SLACK_APPROVER_IDS` is unset or does not contain your `U…`
id. Deny-by-default is deliberate; the final report warns about it.

**Symptom (older deployments): everything registers, and every credential behaves as if it were absent.**
This is the §3.6 D5 cliff and it is fixed on this tree. On a deployment predating E21 T2, `compose.yaml`
read `PALAI_SECRET_MASTER_KEY_FILE` from the shell that invoked compose, and the only thing that ever set
it was `palai up`'s own Slack path. A `docker compose up -d` — or a `palai up` before Slack credentials
existed — therefore booted a control-plane with **no secret store at all**, and every `*_ref` handle
resolved nowhere. The tell is a `POST /v1/secret-refs` answering **404**: the route is only mounted when
the store is. The fix is to upgrade; the workaround on an old build is to run `palai up` with the Slack
values already in `.env.local`, which is exactly the ritual this page exists to delete.

**Symptom (older deployments): `palai up` exits 0 and nothing Slack-shaped was created.** The §4 T2 cliff:
a missing `SLACK_AGENT_REVISION_ID`/`SLACK_PRINCIPAL_ID` used to skip the entire registration silently.
On this tree the bring-up resolves both and a skip that remains is a WARNING.

**Diagnosis without touching the database.** The control-plane's own log is the only place the Socket Mode
`hello` frame is visible, and `palai up` reads it for you; to look yourself:

```sh
docker logs <project>-control-plane-1 2>&1 | grep 'slack socket'
```

## 8. Contract divergences (published docs × this tree)

Recorded in the §3.5 style. Every row was checked against a primary source on the date shown.

| # | Published contract (source) | State in tree | Disposition |
|---|---|---|---|
| **M1** | Socket Mode carries events **and** interactivity over one WebSocket; no public Request URL is required. (https://docs.slack.dev/apis/events-api/using-socket-mode/, fetched 2026-07-27) | `deploy/slack/app-manifest.yaml` sets `socket_mode_enabled` and deliberately omits both `request_url` fields | No change. It is why this page needs no tunnel, ngrok, or public hostname |
| **M2** | Socket Mode envelopes are **unsigned** and need no verification — the WebSocket is pre-authenticated by the app-level token at connect. (same source) | `slack_socket.go` structurally excludes `VerifySignature` on the socket path; the HTTP path still verifies | No change. `SLACK_SIGNING_SECRET` is still required because the HTTP receiver exists and is tested |
| **M3** | A thread is identified by `thread_ts`, and a message with no `thread_ts` starts a new thread. (https://docs.slack.dev/messaging/retrieving-messages/, fetched 2026-07-27) | Session correlation is `(team, channel, thread_ts)` — so a new thread is structurally a new session | No change, and it is the whole of §6's "starting a new thread starts a new session" |
| **M4** | The bot user must be a member of a channel to receive `app_mention` there. (https://docs.slack.dev/reference/events/app_mention/, fetched 2026-07-27) | Nothing in the tree can compensate for this — the event simply never arrives | Not closable in code. It is §7's last row, because "connected but silent" reads as a Palai fault and is not one |
| **M5** | `PALAI_SECRET_MASTER_KEY_FILE` is a **path**, and the key it names seals every stored secret. (E13 T3; `identity.ParseMasterKey`) | `compose.yaml` writes the path literally as of E21 T2; `ensureSecretSlots` mints the key on every bring-up and never replaces an existing one | **CLOSED.** The key is still a FILE on the host — a KMS ceremony is `H-SEC` / §6 leg 6 and this task did not do it |

---


## 9. Workspace search: what it is, and why it is not a memory

Granting `search:read.public` and reinstalling lets a run search this workspace's **public channel messages**
when the question needs an answer that lives in a past conversation. It is worth being precise about what
that does and does not mean, because the shape is easy to over-read.

**It is a search, not a memory.** The agent remembers nothing. Asked a question it goes and looks, and what
it finds is gone when the run ends. There is no accumulating knowledge base, and there cannot be one: the
Real-time Search API's terms say *"You must not store or copy any of the data retrieved from this API. You
may not use any of this data for training."* That single sentence is why we do not vectorise your Slack
history, and why the `knowledge-vector` capability stays `disabled` alongside this feature rather than being
wired to it. Not a design preference — a term of use.

**It sees public channels the asker may not be in.** Slack's own scope page: *"the searching user need not be
a member of the public channels, just of the workspace, for the channels to be included in the search
results."* There is no documented parameter that narrows this to the asker's channels. Private channels, DMs
and group DMs are structurally out of reach — those scopes are user-token only and a bot token cannot hold
them. **This is the decision to make before granting the scope**, and it is the same kind of call
`im:history` was.

**Every search is authorised by the message that started the run.** Slack requires an `action_token` for
bot-token search calls, and that token arrives with the event. So the agent cannot search on its own
schedule — only while answering someone who asked. The token is never stored: its lifetime is undocumented,
and keeping a credential whose expiry you cannot know is keeping something you cannot tell has become
rubbish. The visible cost: a run resumed after a control-plane restart cannot search, and says so rather
than pretending it found nothing.

**Results are untrusted data and the agent is told so.** What comes back was typed by people who are not the
person asking. Each result reaches the model under a field literally named `untrusted_text`, the tool's own
description tells it never to follow an instruction found inside a result, and the results carry no ids or
links — so a message cannot talk a later turn into fetching something. The messages themselves are never
edited: a quote that has been rewritten is not a quote.

**Searches are few on purpose.** Slack's limit here is the tightest of anything this integration calls —
about 10 requests a minute for a whole workspace. Calls are paced per workspace, and each run gets four
searches. When they run out the agent is told it has run out, not that there were no matches, so it can say
the true thing to you.

### Why not Slack's official MCP server

Slack ships one at `https://mcp.slack.com/mcp`, and our MCP transport already speaks its protocol. We do not
use it, for one reason that is total: it authenticates with **confidential OAuth 2.0 using a user token** —
*"You'll need to use your app's `client_id` and `client_secret` for Slack OAuth… Users go through OAuth
consent and authorize the app."* Palai has no authorization-code flow for MCP connections at all; their
secret model is a static handle, resolved per call and dropped. Building the flow is an epic, not a task.

What makes that cheap rather than sad is the second fact: **the same capability takes the bot token we
already hold.** `assistant.search.context` accepts a bot token given `search:read.public`. Going through the
MCP server would have meant opening an OAuth epic to reach a method we can call directly today.

Sources, all checked 2026-07-27:
`https://docs.slack.dev/ai/slack-mcp-server/` ·
`https://docs.slack.dev/apis/web-api/real-time-search-api/` ·
`https://docs.slack.dev/reference/methods/assistant.search.context/` ·
`https://docs.slack.dev/reference/scopes/search.read.public/`

## Proofs

| Claim | Where |
|---|---|
| A secret written on a stack with **no** Slack credentials survives a control-plane restart and still decrypts | `cmd/cli/internal/stack/up_secret_e2e_test.go` (`TestASecretSurvivesARestartOnAStackWithNoSlackApp`, tagged `e2e`) |
| The master-key pointer is not interpolated from the invoking shell | `cmd/cli/internal/stack/up_operator_test.go` (`TestMasterKeyPointerIsNotInterpolatedFromTheInvokingShell`) and `deploy/compose/slack_wiring_test.go` (`TestComposeCanRedeemASecretRefHandle`) |
| Every bring-up leaves a key the control-plane can parse, and never re-mints one | `up_operator_test.go` (`TestEveryBringUpLeavesABootableMasterKey`) |
| A team id and a signing secret are enough to register — the two Palai-side ids are resolved | `up_test.go` (`TestATeamIdAndASigningSecretAreEnoughToRegister`), `up_operator_test.go` (`TestMissingRunTargetIsProvisionedAndSaidOutLoud`) |
| Explicit `SLACK_AGENT_REVISION_ID`/`SLACK_PRINCIPAL_ID` are never overridden | `up_operator_test.go` (`TestExplicitRunTargetAlwaysWins`) |
| A configured-but-unwired Slack install is escalated to a WARNING | `up_operator_test.go` (`TestSkipIsReportedAsAWarningNotSwallowed`) |
| `slack` is reported live only when Slack itself sent `hello` | `up_slack_test.go` (`TestSlackIsLiveOnlyWhenSlackSaidHello`) |
