# The runner fleet

This is the operator's page for a fleet of machines: **pools**, the **key** a machine enrols with, what
**strict mode** does, how to take one machine **out of service**, and what happens to a run when there is
**no machine to run it on**.

It is about the control plane's side. Installing the runner on a host is
[`runner-host.md`](runner-host.md); a Mac specifically is [`palai-on-a-mac.md`](palai-on-a-mac.md).

**A deployment that configures none of this keeps working exactly as it did.** One runner, no pool
configuration, no key: every machine enrols into the default pool, every run is placed there, and nothing
below changes that. Read the rest when you have a second machine.

---

## 0. READ THIS BEFORE YOU CONFIGURE A MAC POOL

**A pool decides where a run's ENGINE goes. It does not decide where the run's TOOLS run.** Every tool — the
shell, the file tool, and everything built on them — executes in the **control plane's own process**, against
the workspace allocation the control plane holds. A `lease.offer` carries an engine image digest and nothing
about tools.

So, stated the way it will matter to you:

> **A Mac is only a Mac when the control plane is on it.** Placing a run in a `mac-pool` whose machines are
> Macs, while the control plane runs on Linux, will run `xcodebuild` **on the Linux box** — and fail there.

What a Mac pool *does* give you today is the thing the rest of this page is about: an identified, revocable
inventory, a queue per pool, a tenant boundary, and a run that waits instead of dying. What it does not yet
give you is remote execution.

**The supported way to run Mac work today is unchanged and is the one already documented:** put the control
plane on the Mac (`palai up --native`, see [`palai-on-a-mac.md`](palai-on-a-mac.md), whose own §2 says
`--native` *"selects **where the control plane runs** — nothing else"*). Multiple concurrent sessions on one
native Mac work; that is measured and shipped.

This is [`FLT-P15`](known-gaps-1.0.md), and it is a **deferral, not a defect**: the execution relay was
planned as E24 T7 and postponed with the scope written down (`T7a` shell relay, `T7b` workspace relay). No
release note claims otherwise, and the evidence bundle for this epic carries no counter about it, because
there is nothing yet to count.

---

## 1. A pool is a posture

A **pool** is a set of machines that are the same *kind* of machine, and the kind is the point:

| Posture | What it is |
|---|---|
| `sandboxed-linux` | today's runner: a Linux container. This is what the compose stack brings up |
| `unsandboxed-host` | a rented Mac: the host's own toolchain, no sandbox at all |

Placement is a **refusal, not a preference**. A run that needs a Mac is not nearly satisfied by a
container, so a run placed in a pool is offered machines from **that pool and no other** — there is no
fallback to "the nearest free machine". Structurally, each pool is its own queue, so a machine in pool A
cannot be reached by a run in pool B at all.

```bash
palai pool create --name mac-pool --posture unsandboxed-host --os darwin --arch arm64
palai pool list                    # every pool: posture, shape, strict mode, queue depth
palai admin runner list            # every machine: pool, state, last seen
```

**A pool's posture is fixed when you create it, and that is deliberate.** A machine **inherits** its pool's
posture when it enrols, so changing a populated pool's posture would retroactively change what the machines
already in it *are*. If you need a different posture, create a different pool.

`palai pool list` also prints **`waiting`** — how many runs are queued for that pool with no machine free to
take them. It is the answer to *"why is nothing running in my Mac pool"*, and until now there was none.
`waiting` is **absent** rather than `0` when the control plane has no runner listener bound: *"nobody could
ask"* and *"nothing is waiting"* are different answers.

Which pool a project's runs go to is `config_policy.pool`:

```bash
palai project set-policy prj_local --pool pool_mac
```

**`set-policy` REPLACES the whole policy document.** The write is an assignment, not a merge, so a call
that names one flag clears every other key the policy carried — pass every flag you want to keep:

```bash
palai project set-policy prj_local --pool pool_mac --approvers key:ak_… --default-tools palai.workspace.shell
palai project get prj_local                    # read it back; this is the only way to be sure
```

Resolution order, highest first: the run's own recorded pool (so a resumed run returns to the same
posture) → the agent revision's binding → the project policy → the tenant's own pool named `default`.
The second of those has nowhere to read from yet (`FLT-P3` in [known-gaps-1.0.md](known-gaps-1.0.md)).

**Creating a pool IS a public route** — `POST /v1/runner-pools`, which `palai pool create` fronts. This
paragraph used to say the opposite, and it was true: until E28 the API only read pools, a tenant got exactly
one `default` pool at birth, and a second one meant raw SQL on the control-plane host. Two code comments had
handed the work to "T5/T6" and both of those shipped without it.

**Deleting a pool is still not a route**, deliberately: `runner_pool_keys` cascades from `runner_pools`, so
deleting a pool would silently delete its enrolment keys, and what should become of the machines whose
`pool_id` names it is a separate decision. Nothing has asked for it.

## 2. Enrolment: one key per pool

A machine authenticates itself to the control plane with a **pool enrolment key**. The value is printed
**once**, at mint:

```bash
palai poolkey create --pool pool_mac        # PRINTS THE VALUE ONCE
palai poolkey list   --pool pool_mac        # prefixes and state, never a value
palai poolkey revoke rpk_…                  # answers with the machines it did NOT stop
```

Write the value into the machine's `/etc/palai/runner/runner-token` and set `PALAI_RUNNER_POOL` to the pool
id. Declaring the pool on the machine is optional and worth doing: if the wrong pool's key was pasted
there, enrolment is **refused at the door** with a 409 instead of the machine quietly joining another
fleet, and the refusal is recorded in the enrolment journal.

**Revoking a key stops the NEXT enrolment and stops no machine that is already running.** That is
deliberate — a fleet credential has to be retirable without taking the fleet down — and it means
decommissioning a compromised machine is **two** calls: the machine (§4) and the key.

**From the console: `/fleet` (§3c of [`console.md`](console.md)).** The key panel mints the value and shows it
**once**, in one place on the screen, and it survives no reload — the browser mirrors the server, which keeps
no copy either. Revoking it there opens a confirmation naming the key, its pool and when it was last used, and
the result **counts the machines it already admitted and names them**, because an operator shown "revoked" and
nothing else reads it as "removed".

**What the key does not do:** nothing on the enrolment wire attests what the machine *is*. The machine
STATES its posture and the control plane COMPARES it with the pool's; it cannot verify it. So the key
catches an operator's mistake, not a lying machine (`FLT-P2`, `FLT-P4`).

## 3. Strict mode: the waiting room

`runner_pools.strict_enrollment` puts a **human** between a valid credential and a machine that can take
work.

**It is OFF by default and that is a deliberate availability decision.** Waiting for a person per machine
in a pool that scales on demand cancels the scaling: a rented Mac takes 6–20 minutes to arrive, and an
operator who added capacity to absorb load would find it parked behind an approval queue. So the default
is off, everywhere — the bootstrap pool, every new organization's `default` pool, and every pool that
existed before the column did.

**It can now be turned on, which until E28 it could not.** The column has existed since `000045` and the
waiting room since E24 T6, but nothing wrote it: the only statement that created a pool wrote `false` as a
literal and there was no `UPDATE` anywhere, so the only two places that ever set it were two test files
issuing raw SQL. The approve route below therefore decided a state no operator could reach. Two commands
reach it now:

```bash
palai pool create --name mac-pool --posture unsandboxed-host --strict   # born with the waiting room open
palai pool set-strict pool_…  --strict                                  # open an existing pool's
palai pool set-strict pool_…                                            # and close it again
```

**Or from the console.** `/fleet` (§3c of [`console.md`](console.md)) creates the pool with the same posture
choice, and each pool row carries a **Require approval / Open enrolment** button that is the `PATCH` above.
Switching a pool strict does **not** re-ask about the machines already in it.

With it **on**, a machine that presents a valid key for that pool:

- **is recorded `pending`**, and
- **receives its certificate** — which matters: `renew` authenticates with the certificate the machine
  already holds, so a machine with none could not survive a long wait and would have to start over, and
- **is never offered a lease.** It is not in its pool's queue at all, so no run can reach it however the
  placement rules are later changed. It stays connected and idle.

A run placed in a pool whose only machines are pending sees an **empty** pool, so it **parks** (§5) rather
than burning the retry ladder while you are asleep. The approval re-enters it.

**Turning it on affects the NEXT enrolment, not the machines already in the pool.** An enrolled machine
renews over the certificate it holds and never re-enrols, so switching the flag on does not put the running
fleet behind an approval — which is what makes it safe to turn on, and also means it is not a way to
re-gate machines you have doubts about. For those, §4.

```bash
palai up                                    # ... fleet   2 pool(s), 1 active runner(s), 1 pending approval
palai admin runner list                     # which machine, and its state
palai admin runner approve rnr_…            # admit it
```

**Who may approve.** Two independent gates, and both are the ones described in
[approvals.md](approvals.md):

| Gate | Where | Unset means |
|---|---|---|
| the key's `approve` capability | `api_keys.scopes` | an unrestricted admin key holds it |
| the project's approver list | `projects.config_policy.approvers` | **everybody** may approve |

The capability is deliberately **`approve` and not `provision`**: `provision` can write
`config_policy`, which is where the approver list lives, so a `provision` key could add itself to the list
it is checked against. A key that holds only `approve` can admit machines and can widen nothing.

```bash
palai project set-policy prj_local --approvers key:ak_…      # then approve with THAT key
```

**Two honest limits.**

1. **An approval admits an ENROLMENT, not a MACHINE.** The same Mac re-enrolling — a re-image, a
   re-provisioned host, an expired certificate recovered with the key — is a new identity and asks again.
   That is the correct subject (otherwise whoever holds the key could re-mint an approved identity at
   will), and it does mean a human per re-enrolment. One approval then carries the machine for the whole
   life of its certificate: rebooting the *control plane* does not re-ask, because the decision is a row.
2. **Strict mode asks who enrolled a machine, never what is inside it.** A Mac that serves several
   customers' sessions is still one machine to this control (`MAC-P6`), and the declared posture is still
   only compared (`FLT-P2`).

Both are filed as `FLT-P12`/`FLT-P13` in [known-gaps-1.0.md](known-gaps-1.0.md).

## 4. Taking one machine out of service

Three verbs, each about **one** machine, each recorded in the database so a control-plane restart does not
forget it:

```bash
palai admin runner cordon  rnr_…    # no NEW leases; the session stays, an in-flight lease finishes
palai admin runner resume  rnr_…    # put it back
palai admin runner revoke  rnr_…    # IRREVERSIBLE: sessions cut, in-flight lease ended, reconnects refused
```

- **Cordon** is how you drain a Mac before unplugging it. `GET /v1/runners/{id}` reports
  `active_leases` — when it reaches 0 the machine is finished and safe to take away.
- **Revoke** is the hard stop for a machine you believe is compromised. The interrupted run is reclaimed
  by the ordinary recovery path, the same way a control-plane restart mid-run is.
- **Neither reaches a `pending` machine.** A machine in the waiting room is admitted with `approve` or
  refused with `revoke`; `cordon` and `resume` answer 404 for it, because a cordon would erase the fact
  that nobody had admitted it and the resume after it would then look legitimate.
- **Revoking a machine does not revoke the key it holds** (`FLT-P11`). A machine that still has a live pool
  key can enrol again as a new identity. Decommissioning means both calls.

**From the console, and the two ask differently on purpose.** On `/fleet` a **cordon** goes through the
browser's own confirmation, because a resume undoes it. A **revoke** opens a dialog that first reads
`GET /v1/runners/{runner_id}` and shows you the machine, its label and **how many leases it is serving right
now** — the drain question above, answered at the moment you are about to act on it — because an irreversible
action has to be reviewable and that number is not on the listing. If the read fails, no dialog opens.

## 5. When there is no machine

A run placed in a pool with **no machine of its tenant** does not fail: it **parks**, costing nothing, and
the next machine to join that pool wakes the oldest waiting run. That is the behaviour that makes
"bring a Mac up when load arrives" possible at all — before it, a run died in about two and a half minutes
while the machine was still booting.

A pool whose machines are all **busy** is a different case: that run rides the ordinary retry ladder
(`FLT-P8`).

**A parked run has no deadline unless you give it one.** There is no default, deliberately — any number
here would be a guess about how long *your* fleet takes to arrive:

```bash
PALAI_FLEET_PARK_TTL=30m       # unset means a park never expires, however old
```

With it set, a run parked longer than the TTL ends as a terminal `timed_out` whose problem detail says no
runner joined its pool, so the caller reads a reason instead of watching a stream that never ends. Leaving
it unset is a supported posture; a run parked in a pool that will never have a machine then waits forever.

## 6. What `palai up` tells you about the fleet

```
  fleet        2 pool(s), 1 active runner(s), 1 pending approval — a PENDING machine holds a
               certificate and takes NO work until a human admits it: `palai admin runner approve <id>`
```

It is on the report unconditionally, including when there is nothing to say, because the alternative is a
waiting room nobody is shown — and a machine an operator believes is working. If the read fails, the line
says *that*, rather than going quiet and reading as "nothing is waiting".

`palai up` also warns when you have more than one machine and `PALAI_DISPATCH_WORKERS=1`: concurrency is
bounded by the control plane there, not by the fleet, so the second machine parks and is never reached.

**The console carries the same three facts and one of them only half.** `/fleet` shows the pools, the active
machines and the waiting room, and it writes `FLT-P15` above all of them. The concurrency warning is the half
measure: `PALAI_DISPATCH_WORKERS` is read by the control-plane process and is published on **no `/v1` route**,
so the console shows the notice on the machine count alone and says which half of the condition it could not
check (`FLC-P7`). `palai up` reads the variable from its own environment; a browser cannot.

## 7. Where the limits are written down

Every ceiling on this page is a row in [known-gaps-1.0.md](known-gaps-1.0.md) §10 (`FLT-P1`..`FLT-P17`),
with the measurement that produced it. The three worth reading before you rely on a fleet:

- **`FLT-P15`** — **there is no remote execution.** §0 above is this row: a pool routes an engine, every tool
  runs in the control plane's process, and a Mac is only a Mac when the control plane is on it. Read it first,
  because it bounds what every other row on this page is worth.
- **`FLT-P4`/`FLT-P12`** — with strict mode off, whoever holds a pool key can enrol a machine of their own
  into that pool and be offered real work. The defences are key secrecy and revocation speed, and there
  are exactly two.
- **`FLT-P2`** — the posture a machine declares is compared with the pool's and never verified. There is
  no attestation on this wire.

And one that used to cost you time and no longer does: **`FLT-P14`** is **closed**. How many runs are queued
for a pool with no free machine is the `waiting` field on `palai pool list` / `GET /v1/runner-pools` (§1). The
count had existed inside the gateway since E24 and no surface read it, so the question §5 tells you to ask —
*"why is nothing running in my Mac pool"* — had no answer you could query.
