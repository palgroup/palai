# The admin console — the door, and the key behind it

The console (`apps/web-console`) is a Next.js app over the Palai **public API only**. The browser can address
exactly two same-origin paths that carry data — the `/v1` relay and the sign-in route — and the API key lives
solely in the server-side relay, never in a browser request, a chunk or a source map.

Until E25 T1 it had **no authentication of any kind**. This page is about what closed and, just as
importantly, what did not.

> **Installation to configured agent, on one page.** §0 is the whole path in order; §1-§3 are the door and
> the key; §3b-§3e are the screens behind it — policy and keys, the fleet, metering, and the two read-only
> registries; §4 is the approval queue; §4b is tool registration. The environment screen keeps its own page
> (`environments.md`) and the MCP chain keeps its own runbook (`jira-mcp-connection.md`), because both are
> longer than a step; §0 links to them where they belong in the sequence.

---

## 0. From an empty install to a configured agent — the whole path

A fresh `palai up` with no Slack has **zero agents, zero repositories, zero environments and zero registered
tools**. This is the order that takes it to an agent that runs your code with your credentials, entirely from
the console. Each step names the screen and the page that goes deeper.

1. **Set the console password.** `printf %s '…' | node apps/web-console/scripts/hash-password.mjs --write`,
   which writes `PALAI_CONSOLE_PASSWORD_HASH` into `apps/web-console/.env.local` for you (§2). **Without it
   the console serves nothing** — not a read, not a write, not a sign-in. Restart the console after setting
   it: the hash is read from the process environment.
2. **Give the console a narrow key.** `palai apikey create --project <prj_id> --scope provision --scope
   approve` (§3), or mint it from `/policy` (§3b) — the screen offers the two capabilities by name and says
   what each opens. Skipping this leaves the console holding a key with EVERY capability; the door is shut,
   the key is not narrow, and `CON-P2` says how far that goes.
3. **Sign in** at `/login`, and confirm `/` shows your organization and project. If it shows an error panel
   instead, the console is running but its upstream is not — see §5.
4. **Create an environment and write its keys** at `/environments`. An environment is a named `KEY=value` set
   an agent's **shell commands** run against. A value is written once and **never read back** — not by you,
   not by the API, not by any screen. Full page: [`environments.md`](environments.md).
5. **Register a repository** at `/repositories`. `connection_ref` is a HANDLE chosen from your secret refs,
   never a typed credential. Registering checks nothing: the first thing that exercises a binding is a run.
6. **Register and approve tools**, if the agent needs any, at `/tools`. Register the MCP connection, discover
   its tools, read each draft revision's **description and input schema** — those are what you are approving,
   because that prose enters a model's context — publish the ones you want, pin them into a set, publish the
   set. The gate defaults **ON** for every tool (§4b). Full runbook, including the `curl` equivalents:
   [`jira-mcp-connection.md`](jira-mcp-connection.md).
7. **Create the agent and its first revision** at `/agents`. A revision names a model, an environment (step
   4), a tool set (step 6) and the MCP connections that CEILING it. **Both `tool_sets` and `mcp_connections`
   are required for a tool to reach a model** — the first is the grant, the second the ceiling, and each
   absence fails quietly.
8. **Publish the revision.** Publishing is what makes a run pinned to it reproducible, and it cannot be
   undone. A draft cannot be pinned: the API answers 409 and the screen says no run and no session were
   created, with no default substituted.
9. **Create the pool the project's runs will be placed in**, if the default is not what you want, at
   `/fleet` (§3c). A pool is a POSTURE — `sandboxed-linux` or `unsandboxed-host` — and it is decided once,
   because a machine inherits it at enrolment. Mint its enrolment key there too; the value is shown ONCE.
   Until E28 T1 there was no way to create a second pool at all, so every deployment had exactly one.
10. **Set the project's policy** at `/policy` (§3b) — the approver list that decides who may answer a gate,
   the tool ceiling, and the runner pool (step 9) the project's runs are placed in. **This form writes the
   WHOLE policy document**, which is what the API does; see §3b before you use it, because the same is true
   of `palai admin project set-policy` and there the read-first step is yours.
11. **Run it** at `/runs`, pinned to that revision. The run's shell commands now see the environment's keys.
12. **Answer its gates** at `/approvals` (§4) — and, if you took a pool strict in step 9, **admit its
   machines** at `/fleet` (§3c). **Read what it did** at `/history`.

**The two things this path does NOT include, on purpose.** There is no button that tests a model route or
validates an environment: verifying that a handle has a value behind it means USING the value, which is a run,
a credential spend and a budget decision. And there is no screen that writes a model connection or a
model route — `palai up` produces a working deployment-default route, and a second provider is a CLI job
today.

---

## 1. What the door is, in one paragraph

One operator, one password, one signed cookie. The password never reaches the console as a password: you
generate a scrypt hash and set it as `PALAI_CONSOLE_PASSWORD_HASH`. Signing in mints an `httpOnly` +
`Secure` + `SameSite=Strict` cookie whose contents are an expiry and a signature — there is no session
table on the server, and nothing in the cookie decodes into a credential. The check itself lives **inside
each relay method**, not in a `middleware.ts`/`proxy.ts` and not in a layout, and a test counts the relay's
exported methods against the number that pass through the gate so the two can never diverge.

**Without `PALAI_CONSOLE_PASSWORD_HASH` the console serves nothing.** Every relay method answers `503
console_not_configured` and no password opens it. A misconfiguration's silent state cannot be *open*, because
this origin is a write proxy holding a control-plane credential.

---

## 2. Set the password

The password is read from **stdin** — never from a command line, where it would land in `ps`, in your shell
history and in any process listing the machine keeps.

**One command, and it is the whole step:**

```sh
printf %s 'a-long-console-password' | node apps/web-console/scripts/hash-password.mjs --write
# → wrote PALAI_CONSOLE_PASSWORD_HASH to …/apps/web-console/.env.local (mode 600)
```

`--write` puts the line in `apps/web-console/.env.local` itself — the path is resolved from the script, so it
does not matter which directory you run it from — and it **replaces** any `PALAI_CONSOLE_PASSWORD_HASH=` line
already there instead of appending a second one. Every other line in the file (your `PALAI_API_KEY`) is kept.
Restart the console afterwards.

Without `--write` it prints the line instead, for a systemd unit, a secret store, or an env var you export
yourself:

```sh
printf %s 'a-long-console-password' | node apps/web-console/scripts/hash-password.mjs
# → PALAI_CONSOLE_PASSWORD_HASH=scrypt.16384.8.1.…….……
```

From the repo root the same script is reachable as
`pnpm --filter @palai/web-console exec node scripts/hash-password.mjs`.

Notes that matter:

- **The hash is not a secret in the way the password is**, but it is not public either: the cookie's signing
  key is derived from it, so anyone holding it can mint sessions. Treat it as a credential. `--write` sets the
  file to mode `600` for that reason.
- **`echo` adds a newline.** The script strips one trailing newline and says so on stderr; `printf %s` avoids
  the ambiguity entirely.
- **Changing the password invalidates every live session.** That is deliberate, and it is the *only*
  revocation this design has (see §6).
- The parameters (`16384.8.1`) are stored inside the hash, so raising them later does not invalidate a hash
  you generate today.
- **The separator used to be `$`, and if you have a hash from before, it still works.** The reader accepts
  both. You do not have to regenerate anything — but see the box below before you put an old one in a file.

> **Why the separator is a dot, and the one thing to watch for.** `scrypt$16384$8$1$…` appended to
> `.env.local` did not survive being read back: **every dotenv reader expands `$`**. Measured on 2026-07-31
> with Next's own loader (`@next/env`, which is what `next start` uses), an 83-character six-part hash on disk
> reached the console as **38 characters and one part** — `$16384`, `$8`, `$1` and the head of the salt were
> read as variable names and expanded to nothing. (Reproduce it and you will get a different *length*: how
> much of the salt is eaten depends on where its first `-` falls. Six parts collapsing to one is the
> invariant.) `set -a; . .env.local` destroyed it the same way, and
> quoting did not save it. The console then answered `503 console_not_configured` — *"is not set (or is not a
> valid scrypt hash)"* — about a variable that **was** set, which is an hour spent looking for the wrong
> thing. Four readers were measured — `@next/env`, `set -a; . file`, `node --env-file` and a shell
> `NAME='…'` export — and the dot form is the only encoding intact in all four (backslash-escaping fixes the
> first two and breaks the last two), so there is now one form of the value and it is correct everywhere.
> **If you are still carrying a `$` hash, keep it in an exported
> environment variable — do not append it to an env file.** `apps/web-console/tests/env-file.spec.ts` runs
> this whole path, generator to loader, on every suite run.

Then start the console with all three variables:

```sh
PALAI_BASE_URL=http://127.0.0.1:8080 \
PALAI_API_KEY=palai-sk-… \
PALAI_CONSOLE_PASSWORD_HASH='scrypt.16384.8.1.…….……' \
  pnpm --filter @palai/web-console dev
```

Open **`/login`** first. The console does not redirect an unauthenticated visitor: the gate is in the relay,
so `/` renders its frame and every panel shows its error state until you have a session. That is the honest
shape of "the door is shut" for a Next app, and it is the price of *not* putting a check in the layout
(where Partial Rendering would make it unreliable).

### `next dev` and `127.0.0.1` — a whole hour, and worth one paragraph

If you develop against the console, know this one. Next's dev server refuses cross-site requests to `/_next/*`
against an allow-list it builds as `['**.localhost', 'localhost', …allowedDevOrigins]` plus whatever
`--hostname` was passed (`server/lib/router-utils/block-cross-site-dev.ts`, Next 16.2.10). **`127.0.0.1` is a
different string and is in none of those by default**, so the HMR websocket — the one `/_next` request a
browser makes carrying an `Origin` header — is refused, and with Turbopack the page then **never hydrates**.

The failure is silent in the worst way: every chunk loads `200`, React's own DevTools banner prints, the
document says `complete`, and there is *no* hydration error — because hydration never started. What you see is
a console where nothing is clickable and the login form does a plain `GET /login?password=…`, putting the
password in the URL bar. `next start` on the same commit is unaffected; this is dev only.

`apps/web-console/next.config.ts` now sets `allowedDevOrigins: ["127.0.0.1"]`, which is dev-only and relaxes
nothing in production. Measured on 2026-07-31 with one `next dev` process, only the hostname differing:

| URL | `preventDefault` ran | `__reactFiber$` on the DOM | HMR upgrade |
|---|---|---|---|
| `http://127.0.0.1:3230/login` *(before)* | no | absent | refused |
| `http://localhost:3230/login` | yes | present | `101` |
| `http://127.0.0.1:3230/login` *(after)* | yes | present | `101` |

That single-variable comparison is also what rules out the other suspects — `turbopack.root`,
`transpilePackages`, a stale `.next`, the pnpm workspace layout — since all four are identical across the rows.

### `Secure` and plain HTTP

The cookie is always `Secure`. `localhost` and `127.0.0.1` are potentially-trustworthy origins in every
current browser, so `pnpm dev` and the loopback proofs work. **A console served over plain HTTP on a LAN
address will not keep the cookie**, and you will appear to sign in and immediately be signed out. Put it
behind TLS — `deploy/compose/production.yml` already has a Caddy edge — or reach it over loopback.

---

## 3. Give it a narrow key — and what is still unmeasured

The console's key is the one thing T1 did **not** narrow. If you exported the bootstrap key (which
`apps/web-console/README.md` tells you to do for the real-profile proofs), then the key behind the door has
**every capability**: `Scope.HasScope` (`apps/control-plane/api/middleware/auth.go:31-34`) returns true for
every capability when the scope set is empty, and `DIV-SHP-003` measured that the bootstrap key's `scopes` is
an empty array on a real stack.

Mint a narrower one:

```sh
palai apikey create --project <prj_id> --scope provision --scope approve
```

`--scope` repeats; the value is printed **once** and never again.

**Why two capabilities, and why `approve` is not covered by `provision`.** The console's admin surface is
gated on `provision`. The approval decision surface E23 T9 opened is gated on `approve`
(`apps/control-plane/api/approvals.go:43`) — and it is deliberately *not* `provision`, because
`PATCH /v1/projects/{id}` writes `config_policy`, `config_policy` is where `approvers` lives, and it is
`provision`-gated. A key that could provision could therefore **add itself to the approver list and then
approve**. Keeping the two capabilities separate is what lets you mint a key that can only *decide*
approvals and cannot rewrite the list it is checked against.

**Honest ceiling, and it is not small: which console pages actually work under a narrow key has not been
measured.** The recipe above is derived from the route table's gates, not from a run. It is a deferred
operator leg (plan §6 leg 4), and until someone measures it, the recipe is a starting point rather than a
tested configuration.

**Second ceiling:** the approver list is the *second* gate on an approval, and `HIL-P11`
(`docs/operations/known-gaps-1.0.md`) measured that an unconfigured `config_policy.approvers` **permits every
principal**. So a key holding `approve` on a project with no configured approvers is not additionally
constrained by that list.

---

## 3b. `/policy` — the whole policy document, and a key shown once

`/policy` is two halves of one job: the project's `config_policy`, and the API keys that reach it. They are
one screen because §3's argument above is a sentence about the approver field on this page — a key that could
provision could add itself to the list it is about to be checked against — and minting the narrow key and
naming its principal in `approvers` was two tools until now.

### The form writes the WHOLE document, because the API does

**`PATCH /v1/projects/{project_id}` is an ASSIGNMENT, not a merge.** `UpdateProjectPolicy`
(`apps/control-plane/internal/identity/store.go`) marshals the decoded `configPolicyInput` and hands the bytes
to `UpdateProjectConfigPolicy`; the four list fields are nil-able, so **a request naming only `pool` stores
`approvers: null`**. And `HIL-P11` measured that an empty approver list is *permissive*: every principal in
the project may approve. So the most innocent action an admin console can offer — putting a project on a Mac
pool — would silently open the approval gate, and your own action would succeed while it happened.

The screen's answer is not a repair, it is a shape: it **reads the current document first**, shows all five
fields, and **sends all five**. A field you cannot see is a field you did not send, and the page says so above
the form. Two tests hold it — one asserts the stored document after a pool-only edit still carries the
approvers, and one asserts the request BODY carried five keys, because a server that merged would let the
first pass over a form still sending one.

**The same is true of the CLI, and there nothing reads first.** `palai admin project set-policy` sends exactly
the flags you pass, and its own help says the realistic accident is `--pool` without `--approvers`. If you use
it, pass every flag you want to keep:

```sh
palai admin project set-policy prj_local \
  --allowed-models 'claude-sonnet-5' --allowed-tools 'git.push' --default-tools 'git.push' \
  --approvers 'key:key_9f2c1d' --pool pool_mac
```

**An empty approver list is shown as permissive, in words**, right where you would leave it empty. Writing a
ceiling on screen does not close it; what it prevents is an empty box reading as a locked door.

### A minted key is shown once, and is retrievable from nowhere

`POST /v1/api-keys` returns the plaintext on the create response and on nothing else — `apiKeyView.Key`
carries `omitempty` and every read leaves it empty. The console mirrors that exactly: the value appears in one
DOM node, and after you dismiss it, it is in no storage, no URL, no later response body and no part of the
page. **If you lose it, mint a new one and revoke this one.** There is no reveal control anywhere, because
there is no route that could feed one.

**The copy button is only there when the browser can offer one.** `Clipboard.writeText` works only in a
**secure context**, so a console you reach over plain `http://` — which the base compose profile serves, TLS
being in the `production.yml` overlay — has no clipboard API at all. There the button is not rendered and a
"select the value and copy it" sentence stands in its place; the value is always selectable and nothing
blocks a copy. A clipboard refusal is shown with its error name rather than swallowed, and the value stays on
screen while you read it.

**Selecting no capability mints an UNLIMITED key.** An empty scope set holds *every* capability
(`Scope.HasScope`), so it is not a key that can do nothing — it is the bootstrap key's power in a new
credential. The screen says this next to the checkboxes.

### Revoking asks differently from cordoning, and that difference is a criterion

A revoke cannot be undone, so it gets a dialog that **reviews** what is about to die: the key's id, its
project and its capabilities. That is WCAG 2.2 SC 3.3.4's *Confirmed* leg, which is the only one of the three
a one-way action can satisfy. Reversible actions — unbinding an environment key, and the fleet's
cordon/resume — keep the browser's own `window.confirm`, which is keyboard-operable, announced and
focus-trapped for free. **Do not convert them**: a test drives the environment unbind and counts the native
dialog.

### Ceilings, named

- **`CON-P2` is not closed by this screen and it says so on the page.** Minting a narrow key here does not
  narrow the key the console process itself is holding — that one comes from `PALAI_API_KEY`, so narrowing it
  means restarting the console with a key minted here.
- **`FLC-P6`: the form is last-writer-wins.** Two operators saving one project inside one read-edit-save
  window means the second overwrites the first. `PATCH /v1/projects/{project_id}` carries no `If-Match` and
  the projection publishes no version, so there is nothing to refuse a stale write with.
- **`FLC-P5`: focus starts on *cancel* in the revoke dialog, and no accessibility requirement is claimed for
  that.** It is what `window.confirm` already does here, and matching it is how the change claims nothing.
- **The value passes through the console's Node process once, in memory.** The relay is a pass-through; there
  is no way for a control plane to hand a browser a credential without crossing the process serving that
  browser. This is [`environments.md`](environments.md)'s ceiling pointed the other way.

---

## 3c. `/fleet` — pools, keys, machines, and the room a machine waits in

`/fleet` is the console's screen for E24's runner registry plus E28 T1's write half. Before it, **no screen in
this console mentioned the fleet at all**: `grep -rc "runner-pools\|/v1/runners\|runner_pool"
apps/web-console/{app,lib,components}` answered 0 in every file, while E24 T6's approve route sat correctly
written and reachable from nothing.

Four sections, in the order an operator acquires them.

### 1. Pools — and a pool's posture is decided once

A pool is a **posture** plus the shape of machine expected in it. `sandboxed-linux` is a container the control
plane isolates; `unsandboxed-host` is a real machine, which is what a rented Mac is.

**Until E28 T1's form existed a tenant had exactly one pool, forever**: the only statement that wrote a pool
row wrote its name, its posture and its enrolment mode as literals, so a rented-Mac pool could not exist and
the waiting room could not be switched on.

**A revoked machine identity stays listed.** The machines table does not drop it, because decommissioning is
a fact worth keeping visible — a list that hid it would make the revocation invisible the moment it worked.

The create form is `POST /v1/runner-pools`. **The posture cannot be changed afterwards and that is
correctness, not a limitation**: a machine INHERITS its pool's posture when it enrols, so moving a populated
pool would retroactively change what the machines already in it are. The only field the PATCH route accepts is
`strict_enrollment`, which the **Require approval / Open enrolment** button on each row switches.

**The `Waiting` column carries a distinction the API is careful about and the screen does not flatten.** It is
`RunnerGateway.Waiting(pool_id)` — attempts queued for this pool with no free machine — and it is a POINTER on
the wire:

| What you see | What it means |
|---|---|
| `0 queued with no free machine` | The gateway was asked and nothing is waiting. **The pool is keeping up.** |
| `3 queued with no free machine` | Three runs are parked for want of a machine in this pool. |
| `— the gateway could not be asked` | This deployment bound **no runner listener**, so nobody counted. This is not zero. |

A console that printed `0` for the third row would tell you an empty Mac pool is healthy.

### 2. Enrolment keys — minted once, and revoked without stopping anything

The key's value is in the mint response **and nowhere else**: the store keeps no copy, so there is no route
that reads one back. The screen shows it once, in one DOM node, and it survives no reload — the same rule
`/policy`'s API key follows, for the same reason.

**Revoking a key does not stop the machines it already let in, and the screen counts them.** The revoke
response carries `enrolled_runners_still_running`, named for what it means, and the confirmation names each
machine. Revoking is how you stop the NEXT machine. Decommissioning the ones already in is the third section.

### 3. Machines — and there is no health column

`Last seen` is the last time the machine **authenticated** — enrolled, connected, or renewed its certificate.
**Nothing polls it and nothing expires a row**, so a stale stamp means *"has not authenticated since"*, not
*"is down"*. There is no `healthy` field on the API and there is no health column on this screen: a badge with
nothing behind it is worse than a column that is not there, because you would act on it.

**Cordon and revoke ask you differently, and the difference is a published criterion rather than a taste.**
WCAG 2.2 SC 3.3.4 is satisfied by any ONE of three legs. A cordon satisfies *Reversible* — Resume undoes it —
so it goes through the browser's own `window.confirm`, which is keyboard-operable, screen-reader-announced and
focus-trapped for free. A revoke cannot: *a revoked runner identity is decommissioned, not paused*. So it gets
the *Confirmed* leg, and the word **reviewing** inside that leg is a requirement — the dialog must show what
is about to die.

**Which is why opening that dialog makes a request.** `active_leases` — how many sessions the machine is
serving right now — is on `GET /v1/runners/{runner_id}` and **not** on the listing, because it is a live value
and a page of them would be a page of separate instants presented as one. The dialog is opened from that
answer, not from the row; if the read fails, no dialog appears and the screen says the machine could not be
read. `— the gateway could not be asked` appears here too, and means the same thing it means on a pool.

### 4. The waiting room — and why it is here rather than on `/approvals`

A machine that presents a valid key into a pool with strict enrolment waits until a human admits it.
**Admitting takes the `approve` capability, which `provision` deliberately does not cover** (§3's argument, on
its other subject: a key that could provision could add itself to the approver list it is about to be checked
against).

**It is not in `/approvals` and it cannot be.** That queue reads `PendingToolApprovals` and its decision route
requires a `request_hash`; a machine enrolment has none — there are no arguments, no parked call, and the
certificate was issued before anybody was asked. Both screens say so and each links to the other.

**Three refusals, three different fixes:**

| Refusal | What happened | What to do |
|---|---|---|
| **403 `insufficient_scope`** | The key has no `approve` capability | Mint one that does (§3) and restart the console with it |
| **403 `approver_not_authorized`** | The key holds `approve`, and the project's `config_policy.approvers` does not name its principal | Add `key:<api_key_id>` on `/policy` |
| **404** | Not yours, not there, or no longer admissible (cordoned or revoked) — **the three are indistinguishable on purpose** | Re-read the list before concluding which |

### Ceilings, named

- **`FLT-P15`, and it bounds everything else on the page.** A machine in a pool runs a run's **engine**;
  every tool — shell, files, git — still runs in the control plane's own process. **A Mac does not run
  `xcodebuild` unless the control plane is on it**, and enrolling more Macs does not change that. Remote
  execution was deferred and has never shipped. The screen says this above the first panel.
- **Concurrency is bounded by the control plane, and this console can only check half of that.**
  `palai up` warns when `PALAI_DISPATCH_WORKERS=1` **and** two or more machines are enrolled. The worker count
  is read by the control-plane process (`main.go` `dispatchWorkerCount`) and is on **no `/v1` route**, so the
  console cannot read it: the notice appears on the machine count alone and says which half it could not
  check. Filed as `FLC-P7`.
- **`FLT-P4`/`FLT-P12`: with enrolment open, the pool key IS the admission control.** Nothing attests what an
  enrolling host is — the posture it declares is compared, not verified — so the defences are the key's
  secrecy and how fast you revoke it.
- **`FLT-P13`: an admission admits an ENROLMENT, not a machine.** The same Mac asks again after every reboot.
- **`FLC-P3`: this screen polls nothing.** A machine that starts waiting while the browser is closed is here
  when you open it, and not before.

---

## 3d. `/usage` — what has been spent, and what caps it

`/usage` is the **read** half of the metering surface, over four routes that were all mounted long before a
screen showed them: `GET /v1/usage` (per-meter totals for the caller's scope), `GET /v1/usage/ledger` (the
settled rows), `GET /v1/budgets` and `GET /v1/quotas` (the limits admission enforces).

The screen carries one sentence per table. The rest of what it used to say is here, because it is about the
metering model rather than about the screen.

### The scope is the key's, and there is no selector

There is **no organization or project selector on this page and that is deliberate**: the scope comes from
the verified identity behind the console's own API key, never from a query parameter. A dropdown here would
be a control that either does nothing or names somebody else's tenant.

### What puts a row in each table

- **A meter** appears once a run *settles* usage. `coordinator/usage.go` names three: `run.admitted`
  (unit `run`, settled inside the admission transaction, so a real stack has this row the moment a run is
  created) and `model.input_tokens` / `model.output_tokens` (unit `token`).
- **A ledger row** is settled *exactly once* against the model request or run that produced it, so a
  redelivery adds nothing. **A zero-quantity fact is never written** — `settleUsage` skips it — which is the
  usual reason a completed run appears in `/history` and nowhere in the ledger.
- **A budget** is a cumulative cap on a meter prefix: admission refuses a run once settled usage since the
  period start reaches the limit. **A quota** is the same limit inside a rolling window.

### Ceilings, named

- **Nothing on this screen is a bill.** The metering surface reports consumption and caps it. It carries no
  price, no invoice and no adjustment entry, anywhere.
- **`GET /v1/usage` takes no time window.** `summaryView` answers lifetime totals for the scope and the route
  parses no bounds, so **"spent today" is not answerable from the summary**. The LEDGER is windowed
  (`?created_after` / `?created_before`) and the totals are not. This is why the overview's token card is
  labelled all-time rather than daily: a card labelled "today" would be a lifetime figure with a false label.
- **The write half is E26.** `POST /v1/budgets` and `POST /v1/quotas` exist, are gated on `provision`, and the
  relay would forward either — so the absence of a form is a *choice*, it is stated on the screen, and
  `tests/observability.spec.ts` asserts the absence rather than trusting it. There is no CLI verb for either.

---

## 3e. `/capabilities` and `/registry` — what is advertised, and how a model is reached

Two read-only screens, and neither has any write at all.

### `/capabilities` — the tiers are the API's, verbatim

`GET /v1/capabilities` is the discovery surface every client reads to learn what a deployment supports
without probing each route, and `palai up` has printed it to a terminal since E22.

**The SET differs between deployments by design.** `a2a`, `slack`, `queues`, `knowledge` and
`capability-workers` are advertised **only where the binary mounted them** (`api/capabilities.go`), so a
console rendering a fixed list of names would silently hide the one a deployment stopped mounting. A
capability this binary did not mount is **absent** from the table rather than shown as unavailable, because
discovery never claims what a deployment cannot serve.

**The tier word is not the console's to soften.** It is recomputed at the exit gate from per-case outcomes
(`uat.CapabilityTierProof`) and served bit-equal to that recompute, so a prettified word on the screen would
be a console disagreeing with the gate that decided it. `tests/observability.spec.ts` diffs the rendered
table against the route's own answer for exactly this reason.

The posture panel shows `maturity`, `isolation` and the configured `store:false` retention TTL — the two
fields `palai up` prints above the same table, plus the knob the reaper honours. **A TTL of 0 is the
*disabled* posture**, not a TTL of zero seconds, and the screen says so in words.

### `/registry` — model connections, routes, and knowledge bases

Read-only projections of how a model is reached. **`/v1` mounts no console-reachable create for any of the
three**, so the absence of a form is the API's shape rather than a simplification.

- **A model connection binds a provider to a secret REF — a NAME.** The value behind it is written
  server-side and is readable through no route, this console included.
- **A model route** is E16 T1's read-back of the E13 write-only route surface: a route is created by the
  provisioning API, and this screen is where it becomes visible.
- **A knowledge base** is what a retrieval tool searches, created and indexed outside this console. Its
  projection carries `name` and not `display_name` (`knowledge/views.go`), which the fixture got wrong until
  E25 T2 because a bootstrap stack's collection is empty and the conformance sweep skips an item comparison
  when either side has no row.

---

## 4. The approval queue — what it decides, and what it does not show

`/approvals` is the console's screen for the surface E23 T9 opened: **`GET /v1/approvals`**, plus
`POST /v1/approvals/{id}/approve|deny`. Before it existed, a gated tool call could only be seen — and therefore
only be answered — inside a Slack thread. A deployment with no Slack workspace parked its runs on questions
nobody could reach, and the expiry reaper released them half an hour later. It failed *closed*, which is exactly
why nobody noticed.

### What it holds, and what it deliberately does not

**Tool approvals only.** The list is `coordinator.PendingToolApprovals`: gated tool calls, oldest first (the
oldest question is the one closest to its deadline).

**Publication approvals are not here.** A push, a pull request or a merge — every side effect E22 produces —
is approved inside a **live run's event stream** (`/runs`), because the public API has no list route for them:
`API-3`/`API-4` in `known-gaps-1.0.md` have been filed three times and are still unapproved. The page says so in
a sentence and links to `/runs`, and that sentence is load-bearing: an operator who learns "approvals live here"
will otherwise miss the ones that do not.

**And a machine waiting to be admitted is not here either.** A strict runner pool holds a new machine until a
human admits it, and that decision cannot ride this queue: `POST /v1/approvals/{id}/approve` **requires** a
`request_hash`, and a machine enrolment has none. Those are on `/fleet` (§3c), and both screens name the
other's.

### What you are reading when you decide

Five things, and every one of them is the **server's** computation rather than the console's:

| On screen | Where it comes from |
|---|---|
| Tool identity | the resolution that will EXECUTE the call — not a name off the model's frame |
| The operator's label | what a human wrote at registration; `(no operator label)` when nobody did |
| Arguments | the ledger row's committed bytes, canonically rendered, `truncated` flagged when cut |
| Request hash | the one-shot binding the decision carries |
| Deadline | `expires_at`, or a sentence saying the gate has none |

The console renders those bytes verbatim: no re-indenting, no JSON re-parse, no friendlier name for the tool.
Both this screen and the Slack one come from **one** derivation (`slack.DeriveApprovalDisplay`), so two
renderings of one row are byte-identical and the two surfaces cannot disagree about what a human approved.

Two consequences worth knowing before you read a screen:

- **There is no MCP `description` on this screen, and none on the wire.** The vendor documents `description` and
  `title` as the human-readable display text; they are also the two fields whose author has an interest in your
  answer. What you approve is the CALL.
- **The arguments carry a Slack-shaped escape.** Because the derivation is shared, `<!` and `<@` arrive as
  `&lt;!` and `&lt;@`. The console shows what it was given rather than "repairing" it — that is the price of one
  derivation, and it is a price rather than a bug.

### The hash comes from the row

The decision body is `{request_hash, reason?}` and **the hash is mandatory** — an approval id alone authorizes
nothing. The console reads it out of the row it displayed: it never asks you to type one, and there is no hidden
field carrying it. If the call's arguments change after the row was rendered, the binding no longer matches and
the decision authorizes **nothing** rather than authorizing something else.

### Denying takes a reason, and the reason reaches the model

`reason` rides a denial back to the model verbatim, so the console **requires** one. A denial with no reason is
a wall; a denial with one is an instruction the agent can act on. This is the one thing this surface does that
Slack's cannot: `HIL-P10` records that a Slack approver's typed deny reason reaches nothing (no
`view_submission` is routed) and the model is handed a constant sentence instead. On the HTTP surface that
ceiling is closed.

### The five refusals, and what each one means for you

| Answer | What it means | What to do |
|---|---|---|
| `400 invalid_request` | the decision carried no `request_hash` | reload the queue; the row arrived without a binding |
| `403 insufficient_scope` | the KEY does not hold `approve` | mint or fix the key (§3) |
| `403 not_an_approver` | the project's `config_policy.approvers` refuses this principal | add `key:<api_key_id>` to that list, or decide with a key already in it |
| `404 not_found` | unknown **or** another project's — indistinguishable on purpose | reload the queue; it says nothing about whether the id exists elsewhere |
| `409 approval_not_decidable` | already answered, deadline passed, or the arguments changed | read the arguments again — they may not be the ones you read |

The console gives each of these its own sentence. None of them is "something went wrong", because the next
action is different for all five.

### Configure BOTH gates, or any of your own keys can approve

Two independent gates stand between a key and a decision, and **both are permissive when unset**
(`HIL-P11`): an empty scope set holds every capability (`Scope.HasScope`) and an empty approver list permits
every principal (`ConfigPolicy.ApproverAllowed`). A deployment that configures neither has a decision surface
any of its own API keys can drive. E25 does **not** change that posture — changing it would be a behaviour
change for every project alive today. Closing both gates is two commands:

```sh
# 1. A key that can ONLY decide approvals. It holds `approve` and nothing else, so it cannot rewrite the list
#    it is checked against. The key is printed ONCE; the key ID it prints is what gate 2 names.
palai apikey create --project prj_local --scope approve

# 2. Name that key's principal in the project's approver list. `key:<api_key_id>` is the principal form the
#    server stamps for a bearer decision (coordinator.ApproverPrincipal).
palai admin project set-policy prj_local --approvers 'key:key_9f2c1d'
```

**`set-policy` REPLACES the whole `config_policy`** — read `docs/operations/approvals.md` §3 before running it,
because a call that names only `--approvers` leaves `allowed_models`, `allowed_tools` and `default_tools` null,
which reads as *unrestricted*. That page is the authority on both gates, on what a principal is, and on what
changes once a list exists; this section is only the console's half of it.

`PATCH /v1/projects/{id}` is `provision`-gated and `config_policy` is where `approvers` lives — which is
**why** `approve` is deliberately not `provision`. A key that could provision could add *itself* to the approver
list and then approve.

**The console's own key needs `approve`** (`--scope provision --scope approve`, §3). A bootstrap key carries it
implicitly because an empty scope set holds everything; a narrow key does not, and a key without it can neither
read this queue nor decide on it — reading and deciding are gated on the same capability.

### Ceilings, named

- **An approval is a KEY, not a person.** Every decision made here is recorded as `key:<api_key_id>` —
  `HIL-P2`: *"a principal is … an account on a surface, never a human"*. Everyone who holds the console password
  decides as the same key, and "two humans approved" is not expressible on this platform.
- **There is no push notification.** The queue is re-read on load, after every decision, and on a 10-second
  timer; a refresh returns to the first page. An approval that arrives while the browser is closed is there when
  you open it — a definite improvement on "only inside a live stream" — but a live event surface for this queue
  is a separate decision and does not exist.
- **The console cannot show you a run waking up.** Approving here applies the decision through
  `coordinator.DecideToolApproval`, which transitions the call and releases the parked run in one transaction;
  that release is proven against a real store at the component tier
  (`apps/control-plane/internal/execution/http_tool_approval_component_test.go`), not on this screen.
- **The queue is cut at twenty rows** like every other list on the public API, and the cut is stated in text
  with a control that continues from the server's own cursor.

---

## 4b. `/tools` — MCP connections and tool registration

`/tools` is the screen for the E12 T5/T6 registry: register an upstream MCP server, discover its tools,
**approve** the ones you want, pin them into a set, publish the set. The full API walkthrough is
[`jira-mcp-connection.md`](jira-mcp-connection.md) §3 — this screen makes the same seven calls.

### It is a REGISTRATION screen. `/approvals` is the approval screen. They are not the same decision

This distinction is written down because the two screens look adjacent and are not, and a reader who
conflates them will conclude that one of them is missing something.

| | `/tools` (this screen) | `/approvals` (§4) |
|---|---|---|
| **When** | once per tool, **before any run exists** | while a run is **parked**, waiting |
| **What is decided** | is this tool advertised to a model at all, and will calling it stop for a human | does *this* call, with *these* arguments, proceed now |
| **The server's `description`** | **SHOWN — it is what you are deciding about**: those words go into a model's context | **NOT ON THE WIRE AT ALL** |
| **The human sentence** | you WRITE it (`approval_label`) | you READ it |
| **Reversible** | no — publishing is permanent; supersede with a new revision | the decision applies once, to one call |

The approval screen's absence of a description is deliberate and structural rather than an omission:
`api/approvals.go`'s `PendingApproval` carries six ledger fields and four screen fields, and its own comment
says *"WHAT IS NOT HERE is as deliberate as what is: no MCP `description`, no server-supplied `title`, no
model prose outside the arguments."* A console could not render it there if it wanted to. That is the whole
reason the label you type on `/tools` exists — it is the **only** human sentence on the approval screen.

### The description is an attacker's text, and it is rendered as text

Whoever operates the MCP server wrote it, and on this screen you are reading it precisely because you are
about to let it into a model's context. It is displayed **as bytes**: no HTML is interpreted, no URL becomes
a clickable link. A description containing `<script>` shows you `<script>`. That is asserted by attack rather
than by claim — a fixture description carrying a `<script>` and an `<a href>` is driven onto the page and the
spec then counts elements, anchors and navigations (`tests/mcp-tools.spec.ts`), and a token scan of every
`.tsx` in the console forbids `dangerouslySetInnerHTML` outright.

### The approval gate defaults ON, and Palai will not guess for you

Every draft revision arrives with **Require approval** checked. Turning it off is a deliberate click.

Palai does **not** classify tools as read or write and is not going to start: the MCP specification says
verbatim that *"clients MUST consider tool annotations to be untrusted unless they come from trusted
servers"*, so a server's own `destructiveHint` cannot decide this, and our client does not decode
`annotations` at all. The console therefore takes the safe answer as the default rather than the informed
one — **and it does not prevent you from removing it.** Publishing a write tool with the gate off succeeds,
silently, and the agent will call it with no human in the loop. That is `HIL-P5`, and this screen makes it
**visible** rather than closed.

### Ceilings, named

- **Two read routes are not an approval FLOW.** `GET /v1/tools/{tool_id}/revisions` and
  `GET /v1/tool-sets/{set}/revisions/{revision_id}` are what E25 T7 added; **who published a revision is
  recorded nowhere.** The decision is immutable and readable, and it is not attributed.
- **`discover` is the only control here that leaves the process**, and it reaches a real MCP server. There
  is nothing to discover on a stack that has registered no connection.
- **No DELETE and no PATCH for a connection.** The API mounts a create, two reads and a discover. This
  console registers and reads; it does not correct. A connection registered wrongly is superseded, not
  edited.
- **A published set grants nothing on its own.** Bind it on `/agents`, where a revision names **both** a
  tool set (the grant) and an MCP connection (the ceiling). Each one's absence fails quietly and
  differently: without the set the tool is never advertised; without the connection it resolves to nothing
  even when it is.
- **One set and one connection per revision on that form.** The API accepts several; the console offers one
  of each, and the field is an array on the wire.

---

## 4c. `/agents` — the lineage list, and `/agents/{id}` where it is changed

**Every paragraph in this section used to be on the screen.** The page-parity pass took four forms and three
explanatory paragraphs off `/agents` and left a list; what those paragraphs said is true, and it is here
rather than deleted. Measured before and after on the built console (2026-07-31): `<main>` fell from 3501
rendered characters to 1810, grey prose from 1158 to 57, forms on the page from 1 to 0, table columns from
**1** to **7**, and `<h2>` headings from 5 to 1.

### An agent is a name; a revision is the configuration

An agent profile is a **name with a lineage of immutable revisions**. A run is pinned to a **published**
revision, and that pin is what makes a run reproducible. There is no PATCH and no DELETE anywhere on this
surface (`api/router.go` mounts a create, three reads, a revision create and a publish) — so a revision is
**superseded, never edited**, and **publishing cannot be undone**: `000019_agents.up.sql` sets `published_at`
once and no route unsets it.

The console follows that shape: the list creates and reads, and every write that changes one agent is on
that agent's own page.

### Where each control lives now

| What | Where it was | Where it is |
|---|---|---|
| Create an agent | a form stacked under the list | a dialog behind `+ New agent` on `/agents` |
| Choose an agent | a `<select>` on `/agents` | **gone** — the row you click is the selection |
| Create a revision | a form stacked under that | `/agents/{id}`, the **Revisions** tab |
| Publish a revision | the same form's table | the same table, on `/agents/{id}` |
| Diff two revisions | panel five of five on `/agents` | `/agents/{id}?segment=compare`, the **Compare** tab |

### There is no Created column, and that is the API rather than the screen

`GET /v1/agents` answers `{id, object, name}` per row and `GET /v1/agents/{id}` answers the same three
fields. `storage/queries/agents.sql`'s `ListAgentProfiles` does select `created_at` and `api.ListRow` does
carry it — but `renderPage` puts only `row.Body` on the wire (`page.Data[i] = row.Body`), so the timestamp
exists solely to mint a pagination cursor. **No agent timestamp is readable through the public API at all**,
and neither is a revision's: the revision projection carries no `created_at` either. A "Created" or "Last
updated" column here would be a field the console invented.

The four lineage columns — Model, Revisions, Latest published, Status — therefore come from **one extra read
per row** of `GET /v1/agents/{id}/revisions`, bounded to the rows on screen and capped at six in flight. A
lineage still loading renders `…`; one that could not be read renders `— unreadable`, which is not the same
cell as `no revisions`.

### Both external fields read back, and one half is newer than it looks

A revision names a **tool set** (the GRANT — the published set revision whose tools a run may reach) and an
**MCP connection** (the CEILING — the servers it may reach at all). The MCP rider has been readable since
E22; the tool set was **write-only until E25 T7**, so a revision could name a set nobody could confirm. Both
are columns on the revisions table now, and each absence fails quietly and **differently**: without the set
the tool is never advertised, without the connection it resolves to nothing even when it is.

Neither is offered as free text, on purpose. A `tool_sets` id is **not reference-checked** at create or at
publish (`automation/agents.go`, deliberately — a typo'd, draft or foreign id fails CLOSED), so a mistyped id
is accepted by every route and then grants nothing, silently, forever. Only **published** set revisions are
offered, for the same reason: a draft pinned there is a revision that will never advertise anything and never
say why.

### Ceilings, named

- **The list is forward-only and cut at twenty.** The cut is stated in words with the server's own cursor
  behind a `Load more`; there is no backward control because `beginList` refuses `?before=` with a 400.
- **The row menu holds two items and both are navigations.** No rename, no delete, no duplicate — the API
  mounts none of them, and a third entry would be a control that refuses.
- **An environment bound here reaches the agent's SHELL**, as `KEY=value` — never something the model is
  shown and never part of a prompt. Publishing a revision that names an environment this organization does
  not have is refused with a **400** (not a 404: the revision id in the path is real).
- **A browser cannot prove the keys arrive.** That loop is closed at the component tier, over the same HTTP
  routes this screen calls:
  `apps/control-plane/internal/execution/console_environment_run_component_test.go`.
- **One set and one connection per revision.** The field is an array and the API takes several; the console
  offers one of each.

---

## 4d. `/repositories` — the binding list, and `/repositories/{id}` for the whole record

Same pass, same three moves. Measured before and after on the built console (2026-07-31): `<main>` 2197
rendered characters with 1650 of grey prose and an eight-field registration form open on the page, under two
standing paragraphs.

**Provider + repository identity are the authoritative name.** A display name or a URL is not trusted as
one anywhere in this system, which is why the list shows the identity rather than the clone URL.

**No credential is on this surface, and it is structural rather than a courtesy.**
`RepositoryBindingCreate` carries a `connection_ref` and no credential field at all
(`api/repository_bindings.go:28-39`), and the read side of `secret_refs` projects `{name, version,
updated_at}` with no value (`identity/secrets.go`). The strongest thing this screen can leak is the NAME of
a credential — the name an operator chose. The ref is **chosen** from the secret-ref list and never typed:
a typo'd ref is accepted by a form and then fails at CLONE TIME, inside a run, with a refusal about git
authentication — as far from the field that caused it as a refusal can get.

**Registering a binding proves nothing.** Nothing is cloned on that screen, no credential is exercised and
no permission is checked — a wrong provider, a wrong identity or a revoked credential shows up at CLONE
TIME, inside a run. That sentence is now in the create dialog, where somebody is about to register one,
and in the status the create leaves behind (*"Nothing has been cloned: the first time this binding is
exercised is a run that names it"*).

**A binding cannot be changed or removed.** `api/router.go:44-46` mounts a create and two reads — no PATCH,
no DELETE. That sentence is now on `/repositories/{id}`, which is the page an operator opens looking for the
edit control. A binding registered wrongly is superseded by registering another and pointing runs at that
one.

**Four fields were written and never shown back.** The clone URL, the data classification, the region
constraint and the pass-through `policy` object are on the record page; they do not fit a list, and a
console that writes a field it never displays is the write-and-pray shape §2 forbids. The policy object is
rendered as pretty-printed JSON exactly as the API returned it — the console passes it through verbatim on
the way in and cannot know which keys this deployment reads, so it narrows nothing on the way out.

The clone URL is **not a link**. It is fetched by the control plane, never followed by a reader, and an
operator-supplied URL turned into a click target on the console's own origin is a defect this tree already
fixed once (artifact downloads, E17 T10).

---

## 5. When it does not work

| Symptom | Cause | Fix |
|---|---|---|
| Every panel shows `503 console_not_configured` | no `PALAI_CONSOLE_PASSWORD_HASH`, or it is not a valid `scrypt.…` string | generate one (§2) and restart the process — it is read per request, but the process must have it in its env |
| `503 console_not_configured` and the variable **is** in `.env.local` | an old `scrypt$…` hash in an env file: the reader expands `$16384`, `$8`, `$1` and part of the salt, and the console receives a stump (§2) | re-run the generator with `--write`; the format it writes now has no `$` in it |
| Sign-in appears to succeed, then everything is `401` | the `Secure` cookie was dropped — plain HTTP on a non-loopback address | reach the console over loopback or put TLS in front (§2) |
| Sign-in returns `403 origin_mismatch` | the request's `Origin` does not match its `Host` — usually a reverse proxy rewriting `Host`, or a hand-rolled `curl` with no `Origin` | make the proxy preserve `Host` (Caddy's `reverse_proxy` does by default); for `curl`, send `-H "Origin: <the console's own origin>"` |
| A write returns `403 origin_mismatch` but reads work | same cause. Reads are not Origin-checked on purpose: a top-level navigation carries no `Origin`, and `SameSite=Strict` is what protects reads | as above |
| `401 the password was not accepted` and you are sure it is right | a trailing newline in the hashed password (`echo` instead of `printf %s`), or the hash belongs to a different password | re-generate with `printf %s` (§2) |
| Everything is `400 only /v1/* public-API paths are relayed` | the browser asked the relay for something that is not a `/v1` path | that is the public-API-only guard working; there is no backchannel to reach for |
| Under `next dev`, nothing on the page responds and sign-in lands on `/login?password=…` | the page never hydrated: Next's dev server blocked the HMR websocket because the browser's origin is not in its dev allow-list (§2, "`next dev` and `127.0.0.1`") | `next.config.ts` sets `allowedDevOrigins: ["127.0.0.1"]`; for any other host add it there. Not a production symptom — `next start` is unaffected |

The refusal codes are stable and distinct on purpose: `503 console_not_configured` (no hash), `401
authentication_required` (no session), `403 origin_mismatch` (cross-origin write). A **sign-in** refusal,
though, is deliberately uniform — one credential means there is no half to be wrong about, so it always says
`the password was not accepted`.

---

## 6. What this is NOT — the ceilings, named

**This is a single-operator door, not an identity system.**

- **No users, no roles.** One password. Everyone who holds it is the same operator.
- **No audit trail of who did what.** A principal on the server side is `key:<api_key_id>`
  (`known-gaps-1.0.md` `HIL-P2`: *"a principal is … an account on a surface, never a human"*). **Every
  approval given through this console is recorded against the console's key**, not against a person.
- **No session revocation list.** A cookie is valid until it expires (12 hours) or the password changes.
  Changing `PALAI_CONSOLE_PASSWORD_HASH` invalidates all sessions at once, and that is the whole mechanism.
- **No rate limiting on sign-in.** scrypt at these parameters costs ~40 ms per attempt, which is a real cost
  to a guesser and no help against a long password being the only defense. Make the password long.
- **The hash lives in an environment variable, not a KMS.** Anything that can read the console process's
  environment can mint sessions — the same exposure `PALAI_API_KEY` already has.
- **The key behind the door is still unlimited unless you narrow it**, and narrowing it is unproven (§3).

**What T1 does claim**, precisely: no write and no read reaches the control plane without a session; the
console does not serve at all without a password; the gate is inside the relay and every exported method
passes through it, counted rather than asserted; a cross-origin write is refused even with a valid session;
and the operator session never rides an upstream request.

---

## 6b. Contract divergences — published docs × this console

Every row is a published requirement or an on-machine measurement this console ACTED on, with the source. The
canonical copy is `tests/uat/evidence_admin_console.go` (`AdminConsoleContracts`), and the
`admin-console-0.1.0` bundle's every checksum moves if a row is dropped or reworded — so this table cannot go
stale quietly.

| # | The published requirement | What this console does |
|---|---|---|
| `N2` | [GHSA-f82v-jwr5-mffw](https://github.com/advisories/GHSA-f82v-jwr5-mffw) (CVSS 9.1): an attacker could *"bypass authorization checks … if the authorization check occurs in middleware"* | The gate is the first two statements of every exported relay method. There is no `proxy.ts` and no layout check |
| `N5` | Next.js Partial Rendering: a layout is re-used across a client-side navigation rather than re-run | Second reason the gate is in the relay. It is also why the fail-closed control console is probed on `/login`: the static shell still RENDERS, so "does not serve" is a claim about data |
| `N7` | The vendor's own session-cookie examples use `SameSite=Lax` | **Deliberate divergence:** `Strict`. Cost paid by the operator — a link from another site lands on a console that looks signed out, one more click. Taken because this origin is a write proxy holding an unlimited credential |
| `N8` | Server Actions get CSRF protection for free; Route Handlers do not — and the vendor RECOMMENDS Server Actions for mutations | **Deliberate divergence:** no Server Actions, because they open a second write path the public-API-only proof cannot see. The protection given up is written by hand: an Origin-vs-Host comparison on every non-GET, refusing a missing Origin outright |
| `N10` | `wcag2a`/`wcag2aa` select **WCAG 2.0 rules only**; `wcag21a`, `wcag21aa` and `wcag22aa` are separate tags | All five tags are scanned. What the widening buys is MEASURED rather than assumed — exactly three rules (`autocomplete-valid`, `avoid-inline-spacing`, `target-size`), and `wcag21a` adds none, because an unknown tag selects no rules and reports clean |
| `N13` | [MDN](https://developer.mozilla.org/en-US/docs/Web/HTML/Reference/Attributes/autocomplete): `autocomplete="off"` does not stop a password manager offering to save or autofilling | `off` is NOT used, and a test fails if the secret field ever carries it |
| `N17` | MDN: `new-password` is the token for a NEW secret field — *"to avoid accidentally filling in an existing password"* — and `off` is for *"CAPTCHA or one-time token fields"* | The environment value field is `type="password"` + `autocomplete="new-password"`, uncontrolled, and **paste is not blocked** (WCAG 2.2 SC 3.3.8) |
| `N18` | [OWASP Secrets Management](https://cheatsheetseries.owasp.org/cheatsheets/Secrets_Management_Cheat_Sheet.html) §2.7.2 rotation, §2.3 least privilege, §2.6 auditing | Rotation is **closed** (a button on the same route as create). Least privilege is **bounded** (§3, and `CON-P2`). Auditing is **open and filed** (`CON-P3`): no resolve is recorded anywhere |
| `N20` | `playwright-core@1.51.1` writes its own default down: *"Defaults to `light`."* | A second Playwright project runs the WHOLE spec set in `dark`. Before it, every axe scan this console had ever run looked at one of the two palettes `globals.css` ships — which is how a 2.63:1 skip link, the first Tab stop on every page, stayed green |
| `N21` | axe-core has **no rule** for SC 1.4.11 (a control's own boundary); Radix Colors guarantees its steps in APCA, not WCAG 2.x | Contrast is measured with WCAG 2.2's own formula against the rendered page, on every route `lib/routes.ts` declares plus `/login`, in both schemes. It found `button.danger` at 1.55:1 on its first run over `/fleet` Radix's published role for step 7 re-measures at 1.53:1, so the step mapping here is ours: step 10 is the first usable control border |

**One row is deliberately ABSENT.** Whether a browser offers to SAVE a lone `type="password"` field outside a
login form — and whether `new-password` suppresses that offer — could not be read on any published page. It is
UNCONFIRMED, it entered no test, no sentence here and no bundle field, and it is filed as `CON-P5`. A vendor
silence is not a design freedom.

---

## 7. Where the pieces live

| Piece | File |
|---|---|
| The gate, the cookie, the hash format reader | `apps/web-console/lib/session.ts` |
| The sign-in route (the ONE non-relay same-origin path) | `apps/web-console/app/api/console/login/route.ts` |
| The sign-in form | `apps/web-console/app/login/page.tsx` |
| The hash generator (stdin, zero dependencies, `--write`) | `apps/web-console/scripts/hash-password.mjs` |
| The proof that the documented setup path works | `apps/web-console/tests/env-file.spec.ts` |
| The gated relay | `apps/web-console/app/api/palai/v1/[...path]/route.ts`, `app/api/palai/stream/route.ts` |
| The approval queue and one parked call | `apps/web-console/app/approvals/page.tsx`, `components/ApprovalRow.tsx` |
| The policy form, the fleet screen, and the two components they share | `apps/web-console/app/policy/page.tsx`, `app/fleet/page.tsx`, `components/RevealOnce.tsx`, `components/ConfirmDestructive.tsx` |
| The pool write half, the strict switch and the queue depth | `apps/control-plane/api/runners.go`, `internal/fleet/api.go`, `storage/queries/runners.sql` |
| The shared approval-screen derivation | `adapters/integrations/slack/approval_display.go` (reached from `apps/control-plane/internal/store/approvals.go`) |
| The approval routes and the capability | `apps/control-plane/api/approvals.go` |
| The proofs | `apps/web-console/tests/auth.spec.ts`, `tests/relay-gate.spec.ts`, `tests/public-api-only.spec.ts`, `tests/approval-queue.spec.ts` |

The control plane's public API is **135 method+path pairs over 102 distinct paths** as of `e38db20f`
(2026-07-31), all registered in `apps/control-plane/api/router.go`. The console relays a subset of them and
can reach no other path.

That number is dated on purpose, with the command that produces it, because it has now gone stale FIVE times
in three documents — and the fifth time was inside the epic that wrote this warning, for the second time:

```sh
grep -E '\.(Handle|HandleFunc)\("(GET|POST|PATCH|DELETE|PUT) /v1' apps/control-plane/api/*.go | grep -v _test | wc -l
```

It read 112 when `admin-console-feature-list.md` measured it, 115 after E23 T9 added the three approval
routes, 125 after E24's runner-fleet routes landed, **130 by the time E25 T3 mounted the environment
family** — which happened while this paragraph said 125 — 132 after E25 T7's two registry reads, and **133
once the tool-registry contract fix mounted `GET /v1/tools/{tool_id}/revisions/{revision_id}`**, which
happened while this paragraph said 132 — **and 135 once E28 T1 mounted `POST /v1/runner-pools` and
`PATCH /v1/runner-pools/{pool_id}`, which happened while this paragraph said 133.** That is the SIXTH
generation of this number, and its history is the whole argument: every one of the six was correct when it
was written. **Run the command. Do not read the number.**

**AND THERE IS A SECOND COUNT THIS FILE OWES, WHICH E28's PLAN §3.6 D10 MEASURED WRONG IN THREE DOCUMENTS AT
ONCE.** `admin-console-feature-list.md` said the console had **22 configuration surfaces** to cover, and E25's
plan quoted that figure into four of its own sections. It was already stale when it was written: the list
predates E24's fleet families and E25's `environments`, so `runner-pools`, `runner-pool-keys`, `runners` and
`environments` are all absent from it. Applying the feature list's own rule — *a family with a persistent
CONFIGURATION write, not a run/response/session action* — to today's tree:

```sh
# every /v1 family with a write verb (33 today)
grep -oE '"(POST|PATCH|DELETE|PUT) /v1/[a-z-]+' apps/control-plane/api/router.go \
  | awk '{print $2}' | sed 's|/v1/||' | sort -u
# minus the SEVEN that are run/response/session ACTIONS rather than configuration:
#   approvals, responses, sessions, inbound, slack, tool-callbacks, webhook-deliveries
```

**Twenty-six configuration surfaces today.** E25 opened seven to writing from a screen; **E28 opens three more
(`runner-pools`, `api-keys`, `projects`/`config_policy`) and two to reading (`runners`, `runner-pool-keys`),
leaving fourteen.** The next epic will want to quote *fourteen* — quote the commands instead, because that
number will be wrong by then too, and the reason it keeps being wrong is that it is a KIND of surface rather
than a fixed set: every epic that mounts a write family moves it.

**E25's own contribution is EIGHT routes, not the seven §T9 counts**: five for the environment family
(`GET`/`POST /v1/environments`, `GET /v1/environments/{id}`, `POST /v1/environments/{id}/values`,
`DELETE /v1/environments/{id}/values/{key}`), two revision reads from T7
(`GET /v1/tools/{tool_id}/revisions`, `GET /v1/tool-sets/{set}/revisions/{revision_id}`) and the eighth from
the contract fix that followed it. The other ten of the eighteen are E24's runner fleet, which ran in
parallel and is not this console's.
