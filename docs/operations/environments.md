# Environments — giving an agent the credentials it needs

An **environment** is a named group of `KEY=value` pairs that an agent's shell commands receive. If your
agent runs `curl -H "Authorization: Bearer $JIRA_TOKEN" …` or `./deploy.sh "$DEPLOY_TARGET"`, this is
where those two values come from.

**Read the ceiling first.** It is not a footnote and it decides whether this feature is appropriate for a
given credential:

> **Giving an agent a secret is the agent having that secret.**
>
> Palai guarantees that no human-facing surface returns an environment value, and that its own paths do
> not write one into a journal, a snapshot, an event or a log line. It does **not** guarantee — and
> cannot — that the agent will not disclose the value. A model that decides to print
> `$JIRA_TOKEN | base64` has printed it. Put in an environment only credentials you would be willing to
> give to the person the agent is standing in for, scoped as narrowly as the target service allows, and
> rotate them on the same cadence you would rotate a person's.

---

## The five routes

All five need an API key holding the `provision` capability.

```bash
# Create an environment. It starts with no keys — that is the normal flow.
curl -sX POST "$PALAI/v1/environments" -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"production","description":"Jira + the deploy target"}'
# → {"id":"env_…","object":"environment","name":"production",…}

# List them, with each one's KEY COUNT.
curl -s "$PALAI/v1/environments" -H "Authorization: Bearer $KEY"

# Read one: key NAMES, versions, and when each was last written. NEVER a value.
curl -s "$PALAI/v1/environments/env_…" -H "Authorization: Bearer $KEY"
# → {"id":"env_…","keys":[{"key":"JIRA_TOKEN","version":2,"updated_at":"…"}],…}

# Write a value. THIS IS ALSO HOW YOU ROTATE ONE — see below.
# The value goes in the BODY, never in the URL, so it cannot land in an access log.
curl -sX POST "$PALAI/v1/environments/env_…/values" -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"key":"JIRA_TOKEN","value":"…"}'
# → {"key":"JIRA_TOKEN","object":"environment_key","version":1,"updated_at":"…"}

# Remove a key from the environment. READ "What 'remove' means" below before using this.
curl -sX DELETE "$PALAI/v1/environments/env_…/values/JIRA_TOKEN" -H "Authorization: Bearer $KEY"
```

Bind the environment to an agent revision, then publish it:

```bash
curl -sX POST "$PALAI/v1/agents/aprof_…/revisions" -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"…","tools":["palai.workspace.shell"],"environment":"env_…"}'

curl -sX POST "$PALAI/v1/agents/aprof_…/revisions/arev_…/publish" -H "Authorization: Bearer $KEY"
```

A revision naming an environment that does not exist is **refused**, at create and again at publish, with
a 400. That refusal is deliberate: a run that silently receives no credential does not stop — `curl`
succeeds anonymously, `gh` reads the public repository, a deploy script writes to the default target.
A wrong answer that looks like a right one is worse than a refusal.

---

## Key names: what is allowed, and what is refused where

A key must match `^[A-Z][A-Z0-9_]*$` and must not start with `PALAI_` (that prefix names variables the
platform sets). Both rules are checked when you write the key, so you find out immediately.

**One class of name is accepted by the write route and refused at run time**, and the split is
intentional rather than an oversight. The sandbox reserves the variables it builds for itself, and
*which* ones those are depends on the posture:

| Posture | Reserved |
|---|---|
| Native (a Mac) | `PATH`, `LANG`, `DEVELOPER_DIR`, `HOME`, `TMPDIR`, `PALAI_SIMCTL_SET` |
| Container | `PATH`, `HOME` |

A deployment can change posture without touching your environments, so a list copied into the write route
would be a list that goes stale. Instead the executor derives the reserved set from the environment it
actually built plus the names it claims, and **refuses the command before any process starts** — nothing
runs, and the error names the key. So a key called `PATH` can be stored and will never be usable. If a
tool call fails with *"environment key … is reserved by the sandbox's own environment"*, rename the key.

---

## What "rotate" means: it is the same call as "create"

There is no separate rotate route, and that is a consequence of how values are stored rather than a
missing feature. A value is a version in an append-only store: writing `JIRA_TOKEN` a second time creates
version 2, and the next command the agent runs receives version 2 — **with no restart**. Version 1's
bytes remain, sealed, for audit.

So: to rotate, write the key again.

## What "remove" means: the binding, not the bytes

`DELETE /v1/environments/{id}/values/{key}` removes the **binding** between the environment and the key.
It does **not** delete the stored value, and the API's own response says so.

The reason is the audit posture of the underlying store: nothing in Palai can delete a secret version,
because version history is retained on purpose. What removal actually achieves is precise and worth
stating: no run receives the key any more, and nothing names the stored versions (their address is derived
from the membership row you just removed). They are retained and unreachable.

**If a credential has leaked, removing it here is not enough.** Revoke it at the service that issued it.
That is true of every secrets manager, and it is true here.

An **environment row itself cannot be deleted** either. Its id is part of the address of every value it
groups, so deleting one would orphan them all. E25 ships no such operation.

---

## Doing all of that from the console

The admin console has an **Environments** page (`/environments`). It signs you in first — the console does
not open without `PALAI_CONSOLE_PASSWORD_HASH` — and everything it does goes through the same five routes
above, through the same relay, with the API key staying server-side.

What the page gives you, in the order you will use it:

1. **The list**, with each environment's key COUNT.
2. **Create an environment.** A name unique in your organization; it starts with no keys.
3. **Write a value.** Pick an environment from a dropdown, type a key name, type the value.
4. **Rotate.** The same form, the same button. Write the key again and the version goes up.
5. **Unbind.** One button per key, with a confirmation that says what it actually removes.

### The value field, and why it looks the way it does

It is `type="password"` with **`autocomplete="new-password"`**. That token is not decoration and it is not
`off`: browsers do not honour `off` on a password field, and MDN reserves it for CAPTCHA and one-time-token
fields. `new-password` is the token documented to *"avoid accidentally filling in an existing password"* —
which here means stopping your browser from dropping your **console password** into a box whose contents an
agent will then use as a credential.

**Paste is not blocked.** A real credential is forty random characters; retyping one by eye is how you get a
typo in a value nobody can read back to check. WCAG 2.2 §3.3.8 counts paste and password-manager support as
the mechanisms that satisfy it, so blocking either would be an accessibility failure as well as a bad idea.

**The field clears when you submit** — on success and on refusal. A refused write means retyping. That is
deliberate: a secret left sitting in a form field after a `400`, on a screen you may then walk away from, is
the leak this whole design is about.

### There is no "show value" button, and there never will be

The screen shows key **names**, **versions** and **when each version was written**. It shows no value and it
has no control that would reveal one — not a masked `••••` you can click, not a copy button. This is not a
UI preference: no route returns a value, the only query that decrypts one is reachable from no API route at
all, and the console's own test suite fails if a control appears whose label or test id looks like a reveal.

The practical consequence, stated so it is not a surprise later: **if you lose a value, you cannot get it
back from Palai.** Write a new one — that is what rotate is — and if the credential itself matters, get a
fresh one from the service that issued it.

### Three things about the console specifically

- **Your browser may offer to save the value.** `new-password` is documented to affect *filling*; whether
  it suppresses the *save* offer is not documented by any browser vendor and **we did not measure it**, so
  nothing here claims it does. If your password manager offers, decline it — or accept it knowingly. This is
  the accepted cost of typing a credential into a browser at all, and it is filed as `CON-P5`.
- **The value passes through the console's Node process, once, in memory.** The relay is a pass-through;
  there is no way to type a value into a browser and have it reach the control plane without crossing the
  process serving that browser. What that crossing gets: TLS in front of it, `Cache-Control: no-store` on
  every environment response, and a relay that logs no request body anywhere.
- **The screen says the value cannot be read back, in words, before you write one.** It also says the
  environment is org-scoped, for the reason the next section gives.

---

## Two facts about scope that will surprise you if nobody says them

**1. An environment belongs to an ORGANIZATION, not to a project.** Two projects in the same organization
see the same environments. This follows the secret store beneath it, which is org-scoped: making the
grouping project-scoped while its values were org-scoped would have let one project resolve another
project's key by deriving the same address. If you need per-project separation, use separate
organizations.

**2. A run's environment comes from the revision it PINNED.** Republishing an agent against a different
environment does not move a run that is already going. But the KEY LIST is read live, so adding a key to
the pinned environment mid-run does reach that run — which is the same property that makes a rotation
take effect without a restart.

---

## What is redacted, and the exact limit of it

Anything an agent's command writes to stdout or stderr is scanned before it is returned or recorded, and
any literal occurrence of one of that attempt's own environment values is replaced with `***`. So a build
log that echoes its environment, a `curl -v` that prints its `Authorization` header, or a stack trace
carrying a connection string all arrive masked.

This is a **literal substring match** and its limit is exact: an agent that base64-encodes a value, prints
it one character per line, reverses it, or splits it across two commands defeats it completely. The
redaction is real protection against *accidental* leakage — which is the failure that happens by default —
and no protection at all against an agent that is trying. See the ceiling at the top of this page.

One visible consequence: a value that is *also* something you wanted to read — a hostname, a URL, a
branch name — comes back as `***` too. If a value is not a credential, do not put it in an environment;
pass it in the agent's instructions instead.

---

## What is NOT recorded, and one thing that should be

**Not recorded (by design):** the value. Not in the tool-call ledger, not in a `ConfigSnapshot`, not in a
checkpoint, not in an event, not in a log line. This is tracked in three independent places rather than
asserted — a sweep over the SQL, a field-set pin over the API projections, and a byte scan over the
console's responses.

**Not recorded (a gap, filed as `CON-P3`):** *when* a value was resolved and *by which run*. OWASP's
Secrets Management Cheat Sheet §2.6 asks for exactly that — *"When the secret was used and by whom/what"* —
and Palai does not have it: the secret store keeps no read audit and no API route exposes one. If you need
that record today, it has to come from the issuing service's own audit log.

Of OWASP's three relevant lines, one is closed and two are not:

| OWASP | Status here |
|---|---|
| §2.7.2 Rotation | **Closed.** Write the key again; the next command gets the new value with no restart. The console makes it one button, and it is the same button as create. |
| §2.3 Access Control / §6.3 least privilege | **Partly.** These routes need the `provision` capability. There are no per-environment permissions and no roles — any key that can provision can read every key NAME and write every value in the organization. The console does not narrow this: it holds one key, and anyone who knows the operator password acts as that key. |
| §2.6 Auditing | **Open — `CON-P3`.** No read audit exists. Nor does the console add one: it records who typed a value nowhere, because there is nowhere to record it. |

---

## The other ceilings, named

- **The master key is one key, held in the control-plane process** (`PALAI_SECRET_MASTER_KEY_FILE`). There
  is no KMS backend and no per-secret data key. A deployment with no master key configured mounts no
  environment routes at all — which is correct rather than awkward: an environment that cannot seal a
  value would be a list of names.
- **The value passes through the console's Node process in memory** when you type it into the browser
  (T4's screen). The relay is a pass-through; this is unavoidable. It is mitigated by TLS, by
  `Cache-Control: no-store`, and by the body never being logged.
- **Your browser may offer to save the value.** The field is `type="password"` with
  `autocomplete="new-password"`, which is the token MDN documents for exactly this case — it prevents an
  existing password being autofilled. Whether it also suppresses the *save* offer is **not documented by
  any browser vendor and we did not measure it**, so nothing here claims it does.
- **The agent's engine process environment is untouched.** `EngineRequest.Env` and its `^PALAI_[A-Z0-9_]+$`
  allow-list still have no users — an environment is a shell-tool feature, and this note exists so the
  next reader does not assume otherwise.
