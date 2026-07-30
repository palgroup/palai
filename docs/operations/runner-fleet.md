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
palai admin runner list            # every machine: pool, state, last seen
curl -H "Authorization: Bearer $PALAI_API_KEY" $PALAI_BASE_URL/v1/runner-pools
```

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

**Creating a pool is not a public route.** The API reads pools; it does not create them. A tenant gets one
`default` pool when it is created, and a second pool is an `INSERT INTO runner_pools` on the control-plane
host today.

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

With it **on**, a machine that presents a valid key for that pool:

- **is recorded `pending`**, and
- **receives its certificate** — which matters: `renew` authenticates with the certificate the machine
  already holds, so a machine with none could not survive a long wait and would have to start over, and
- **is never offered a lease.** It is not in its pool's queue at all, so no run can reach it however the
  placement rules are later changed. It stays connected and idle.

A run placed in a pool whose only machines are pending sees an **empty** pool, so it **parks** (§5) rather
than burning the retry ladder while you are asleep. The approval re-enters it.

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

## 7. Where the limits are written down

Every ceiling on this page is a row in [known-gaps-1.0.md](known-gaps-1.0.md) §10 (`FLT-P1`..`FLT-P13`),
with the measurement that produced it. The two worth reading before you rely on a fleet:

- **`FLT-P4`/`FLT-P12`** — with strict mode off, whoever holds a pool key can enrol a machine of their own
  into that pool and be offered real work. The defences are key secrecy and revocation speed, and there
  are exactly two.
- **`FLT-P2`** — the posture a machine declares is compared with the pool's and never verified. There is
  no attestation on this wire.
