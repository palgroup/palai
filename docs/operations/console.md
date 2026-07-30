# The admin console — the door, and the key behind it

The console (`apps/web-console`) is a Next.js app over the Palai **public API only**. The browser can address
exactly two same-origin paths that carry data — the `/v1` relay and the sign-in route — and the API key lives
solely in the server-side relay, never in a browser request, a chunk or a source map.

Until E25 T1 it had **no authentication of any kind**. This page is about what closed and, just as
importantly, what did not.

> **The door, the key, and the approval queue.** T1 wrote §1-§3, T5 wrote §4. The environment screen has its
> own page (`environments.md`); repository bindings and agents are written as those tasks land.

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

```sh
printf %s 'a-long-console-password' | node scripts/hash-password.mjs
# → PALAI_CONSOLE_PASSWORD_HASH=scrypt$16384$8$1$…$…
```

Append it straight to your env file:

```sh
printf %s 'a-long-console-password' | node scripts/hash-password.mjs >> apps/web-console/.env.local
```

From the repo root the same script is reachable as
`pnpm --filter @palai/web-console exec node scripts/hash-password.mjs`.

Notes that matter:

- **The hash is not a secret in the way the password is**, but it is not public either: the cookie's signing
  key is derived from it, so anyone holding it can mint sessions. Treat it as a credential.
- **`echo` adds a newline.** The script strips one trailing newline and says so on stderr; `printf %s` avoids
  the ambiguity entirely.
- **Changing the password invalidates every live session.** That is deliberate, and it is the *only*
  revocation this design has (see §6).
- The parameters (`16384$8$1`) are stored inside the hash, so raising them later does not invalidate a hash
  you generate today.

Then start the console with all three variables:

```sh
PALAI_BASE_URL=http://127.0.0.1:8080 \
PALAI_API_KEY=palai-sk-… \
PALAI_CONSOLE_PASSWORD_HASH='scrypt$16384$8$1$…$…' \
  pnpm --filter @palai/web-console dev
```

Open **`/login`** first. The console does not redirect an unauthenticated visitor: the gate is in the relay,
so `/` renders its frame and every panel shows its error state until you have a session. That is the honest
shape of "the door is shut" for a Next app, and it is the price of *not* putting a check in the layout
(where Partial Rendering would make it unreliable).

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

## 5. When it does not work

| Symptom | Cause | Fix |
|---|---|---|
| Every panel shows `503 console_not_configured` | no `PALAI_CONSOLE_PASSWORD_HASH`, or it is not a valid `scrypt$…` string | generate one (§2) and restart the process — it is read per request, but the process must have it in its env |
| Sign-in appears to succeed, then everything is `401` | the `Secure` cookie was dropped — plain HTTP on a non-loopback address | reach the console over loopback or put TLS in front (§2) |
| Sign-in returns `403 origin_mismatch` | the request's `Origin` does not match its `Host` — usually a reverse proxy rewriting `Host`, or a hand-rolled `curl` with no `Origin` | make the proxy preserve `Host` (Caddy's `reverse_proxy` does by default); for `curl`, send `-H "Origin: <the console's own origin>"` |
| A write returns `403 origin_mismatch` but reads work | same cause. Reads are not Origin-checked on purpose: a top-level navigation carries no `Origin`, and `SameSite=Strict` is what protects reads | as above |
| `401 the password was not accepted` and you are sure it is right | a trailing newline in the hashed password (`echo` instead of `printf %s`), or the hash belongs to a different password | re-generate with `printf %s` (§2) |
| Everything is `400 only /v1/* public-API paths are relayed` | the browser asked the relay for something that is not a `/v1` path | that is the public-API-only guard working; there is no backchannel to reach for |

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

## 7. Where the pieces live

| Piece | File |
|---|---|
| The gate, the cookie, the hash format reader | `apps/web-console/lib/session.ts` |
| The sign-in route (the ONE non-relay same-origin path) | `apps/web-console/app/api/console/login/route.ts` |
| The sign-in form | `apps/web-console/app/login/page.tsx` |
| The hash generator (stdin, zero dependencies) | `apps/web-console/scripts/hash-password.mjs` |
| The gated relay | `apps/web-console/app/api/palai/v1/[...path]/route.ts`, `app/api/palai/stream/route.ts` |
| The approval queue and one parked call | `apps/web-console/app/approvals/page.tsx`, `components/ApprovalRow.tsx` |
| The shared approval-screen derivation | `adapters/integrations/slack/approval_display.go` (reached from `apps/control-plane/internal/store/approvals.go`) |
| The approval routes and the capability | `apps/control-plane/api/approvals.go` |
| The proofs | `apps/web-console/tests/auth.spec.ts`, `tests/relay-gate.spec.ts`, `tests/public-api-only.spec.ts`, `tests/approval-queue.spec.ts` |

The control plane's public API is **125 method+path pairs** as of `c6a59658` (2026-07-30), all registered in
`apps/control-plane/api/router.go`. The console relays a subset of them and can reach no other path.

That number is dated on purpose, with the command that produces it, because it has now gone stale three
times in three documents:

```sh
grep -E '\.(Handle|HandleFunc)\("(GET|POST|PATCH|DELETE|PUT) /v1' apps/control-plane/api/*.go | grep -v _test | wc -l
```

It read 112 when `admin-console-feature-list.md` measured it, 115 after E23 T9 added the three approval
routes, and 125 after E24's runner-fleet routes landed. Quote the command, not the count.
