# The admin console — the door, and the key behind it

The console (`apps/web-console`) is a Next.js app over the Palai **public API only**. The browser can address
exactly two same-origin paths that carry data — the `/v1` relay and the sign-in route — and the API key lives
solely in the server-side relay, never in a browser request, a chunk or a source map.

Until E25 T1 it had **no authentication of any kind**. This page is about what closed and, just as
importantly, what did not.

> **First version — T1 only.** It covers the door and the key. The rest of the console's operator story
> (environments, the approval screen, repository bindings, agents) is written as those tasks land.

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
  revocation this design has (see §5).
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

## 4. When it does not work

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

## 5. What this is NOT — the ceilings, named

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

## 6. Where the pieces live

| Piece | File |
|---|---|
| The gate, the cookie, the hash format reader | `apps/web-console/lib/session.ts` |
| The sign-in route (the ONE non-relay same-origin path) | `apps/web-console/app/api/console/login/route.ts` |
| The sign-in form | `apps/web-console/app/login/page.tsx` |
| The hash generator (stdin, zero dependencies) | `apps/web-console/scripts/hash-password.mjs` |
| The gated relay | `apps/web-console/app/api/palai/v1/[...path]/route.ts`, `app/api/palai/stream/route.ts` |
| The proofs | `apps/web-console/tests/auth.spec.ts`, `tests/relay-gate.spec.ts`, `tests/public-api-only.spec.ts` |

The control plane's public API is **125 method+path pairs** as of `c6a59658` (2026-07-30), all registered in
`apps/control-plane/api/router.go`. The console relays a subset of them and can reach no other path.

That number is dated on purpose, with the command that produces it, because it has now gone stale three
times in three documents:

```sh
grep -E '\.(Handle|HandleFunc)\("(GET|POST|PATCH|DELETE|PUT) /v1' apps/control-plane/api/*.go | grep -v _test | wc -l
```

It read 112 when `admin-console-feature-list.md` measured it, 115 after E23 T9 added the three approval
routes, and 125 after E24's runner-fleet routes landed. Quote the command, not the count.
