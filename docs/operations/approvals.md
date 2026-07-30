# Who may approve

An approval in Palai is a **one-shot decision on a specific operation**: a publication (push, PR) is
recorded pending, a human approves or denies it, and the approved operation is published. This page is
about the other half of that sentence — **which human**.

Everything below is machinery that already existed (E19 Slack decisions, E13 project policy) plus one new
list. There is **no migration**, and **a deployment that configures nothing behaves exactly as it did**.

---

## 1. There are TWO gates, and both must be passed

| Gate | Where it lives | Scope | Absent means |
|---|---|---|---|
| **Slack's own approver list** | `slack_connections.allowed_users` (per connection) | that workspace, and it also carries **channel** scope | **nobody** may approve |
| **The project approver list** | `projects.config_policy.approvers` | the project, **every surface** | **everybody** may approve |

They are not alternatives and neither replaces the other. The connection list can express something the
project list cannot — *"this user, in these channels, on this workspace"* — and the project list can
express something the connection list cannot: *"this API key"*, on a surface Slack knows nothing about.

**The two defaults point in opposite directions and that is deliberate.** `allowed_users` was born
deny-by-default and every operator who wired Slack configured it. `config_policy.approvers` is new, and
every project alive today has no such key: making its absence mean "nobody" would break every running
deployment at the moment of upgrade. An unset list is a **supported posture**, not a bug — it is just not
a gate, and `palai up` says so out loud on every bring-up.

## 2. What a principal is

One string, and its first segment names the surface that authenticated it:

| Surface | Principal | Where the ids come from |
|---|---|---|
| Slack | `slack:<team_id>:<user_id>` | `SLACK_TEAM_ID`; the user id from a Slack profile (`Uxxxxxxxx`) |
| HTTP | `key:<api_key_id>` | `palai admin apikey list` → the `id` field (`key_…`) |

**The Slack form is workspace-qualified on purpose.** A Slack user id is unique only *within* a workspace,
so a bare `U0123ABCD` in a list would admit whoever holds that id in some other workspace. A bare id
matches nothing.

**The HTTP form names the KEY, not the principal behind it.** `api_keys.principal_id` carries no unique
constraint, so several keys can share one principal — and an operator who revokes a key expects to have
revoked what that key could approve.

Every surface renders its identity through one function (`coordinator.ApproverPrincipal`), so there is one
format for an operator to match and one place for a new surface to join.

## 3. Configure it

```sh
palai admin project set-policy prj_local \
  --approvers 'slack:T0123ABCD:U0123ABCD,key:key_9f2c1d'
```

> **`set-policy` REPLACES the whole `config_policy`.** It is a PUT in a PATCH's clothing: a call that sends
> only `--approvers` leaves `allowed_models`, `allowed_tools` and `default_tools` **null**, which reads as
> *unrestricted*. If the project has model or tool allowlists, resend them in the same call:
>
> ```sh
> palai admin project set-policy prj_local \
>   --allowed-models 'claude-opus-5' --default-tools 'file,shell' \
>   --approvers 'slack:T0123ABCD:U0123ABCD,key:key_9f2c1d'
> ```
>
> Read the current policy back first with `palai admin project get prj_local`.

Over the API directly:

```sh
curl -X PATCH "$PALAI_BASE_URL/v1/projects/prj_local" \
  -H "Authorization: Bearer $PALAI_API_KEY" -H 'Content-Type: application/json' \
  -d '{"config_policy":{"approvers":["slack:T0123ABCD:U0123ABCD","key:key_9f2c1d"]}}'
```

The schema is strict: a mis-spelled key (`approvrs`) is a **400**, never a silently dropped write.

## 4. What changes once a list exists

- **Deny by default.** Only the principals named decide. Everyone else's approve or deny **transitions
  nothing** — the publication stays `pending_approval`.
- **A refusal still settles the command.** A refused approve does not sit queued and retry at every step
  boundary for the life of the run.
- **A denial is a decision too.** An unlisted principal cannot *block* a publication either.
- **Slack tells the clicker nothing.** The route answers `200` and records the refusal server-side;
  surfacing "you are not an approver" in the UI would hand an unmapped user a probe. Look in the
  control-plane log for `slack interactions: decision refused`.
- **The HTTP caller learns by reading back.** `POST /v1/sessions/{id}/commands` is *acceptance*, not
  application — a refused approve settles as `applied` with the publication still `pending_approval`. The
  publication's state is what says whether a decision landed.

## 5. The list is read LIVE, never frozen

The allow-list is read **at the moment a decision is applied**, not when the approval was created. So
removing an approver takes the pending approvals with it: every already-posted button they could have
pressed goes dead immediately.

This is the same argument the tree already makes for `allowed_channels` — an operator **narrowing** an
allow-list is what containing an incident looks like, and a narrowing that left in-flight approvals live
would not contain anything.

The converse also holds: *adding* an approver makes them able to decide approvals that were already
pending.

## 6. Where the check lives

One place: `coordinator.ApplyApprovalDecision`. Both surfaces pass through it —

```
Slack click  → …ApproverAuthorized → AcceptCommand → ApplyApprovalDecision → (approver list)
HTTP command → AcceptCommand → [queued] → boundary pump → ApplyApprovalDecision → (approver list)
```

— and putting the check in each caller instead is how the next caller forgets it. An untagged test
(`TestApproverAllowedHasExactlyOneProductionCallSite`) walks the tree and fails if a second call site
appears.

Because the HTTP surface is **asynchronous** (the POST only queues; the pump applies later, in another
process), the principal is stamped onto the durable command from the **verified API key** at accept time.
The request body has no approver field and cannot acquire one. The principal is therefore fixed at accept
while the **list** is read at apply — which is exactly what makes §5 work.

### 6.1 What "one place" means, and what it does not

`ApplyApprovalDecision` is the one throat for **decisions about a gated operation** — a tool call or a
publication. Its subject is keyed by a tool-call id and bound to a `request_hash`, which is what makes
"the arguments a human approved are the arguments that run" true.

**A runner enrolment approval (E24 T6) deliberately does NOT go through it, and that is a reading of this
rule's scope rather than an exception to it.** `POST /v1/runners/{id}/approve` admits a machine that a
strict pool is holding. There are no arguments, no parked tool call, and **no request hash to bind to** —
the certificate was issued before anybody was asked. Routing it through this throat would have meant
fabricating a tool call and a hash for every machine that boots, and the binding would then bind nothing.

What is **not** separate is the policy: that path asks `ConfigPolicy.ApproverAllowed`, the same function
this page is about, so everything in §4, §5 and §7 applies to it unchanged — including that an unset list
permits everybody. The `ApproverAllowed` call-site guard named above allows exactly those two paths and
carries this argument in its own header, so a third one has to be argued for the same way.

The fleet side of it — pools, keys, strict mode — is [`runner-fleet.md`](runner-fleet.md).

## 7. Honest ceilings

**PALAI HAS NO USER IDENTITY.** A principal is a Slack account or an API key. **It is not a person.**
Nothing links the same human's two identities across surfaces: if Ayşe approves from Slack and Ayşe
approves from the API, those are two unrelated principals, and Palai cannot tell that they are the same
human — or that they are not. End-user token exchange (`TEN-004`) is still post-1.0.

Consequences worth stating rather than discovering:

- **Two-person approval does not exist on this path.** One approval is one principal. (The release
  pipeline's `ApprovalGate` computes two-person review from CODEOWNERS; that is a different mechanism, in
  a different place, and it is not wired to this.)
- **An API key is a bearer token.** `key:key_9f2c1d` in the list means *whoever holds that key*. Scope it
  to one purpose, and revoke it rather than trying to un-approve.
- **A principal cannot be scoped to an operation.** The list says who may decide, not what they may decide
  — there is no "may approve a push but not a PR".
- **A session's oldest pending approval is the decidable one.** Two approvals open at once means the
  second waits its turn (inherited, unchanged).
- **`approvals.allowed_approver` is not this.** That column exists, is written with a value no caller ever
  sets, and is read by nothing. It is not a control and never was; this list is.

## Proofs

| Claim | Test |
|---|---|
| Any tenant-scoped key approves when no list is configured (bit-unchanged) | `TestApproverAnyTenantKeyApprovesWhenNoListIsConfigured` |
| An unlisted key decides nothing, and its command still settles | `TestApproverAKeyOutsideTheProjectListDecidesNothing` |
| An unlisted key cannot deny either | `TestApproverADenyFromAnUnlistedKeyDecidesNothingEither` |
| The request body cannot name its own approver | `TestApproverTheRequestBodyCannotNameItsOwnApprover` |
| Both lists must be passed (Slack) | `TestSlackApproverBothListsMustBePassed` |
| Slack's own list still refuses before any command exists | `TestSlackApproverConnectionListStillRunsFirst` |
| A bare Slack user id is not a principal | `TestSlackApproverAUserIdAloneIsNotAPrincipal` |
| No list ⇒ bit-unchanged on Slack too | `TestSlackApproverWithNoProjectListIsBitUnchanged` |
| Narrowing reaches approvals already in flight | `TestApproverTheListIsReadLiveSoNarrowingTakesPendingApprovalsWithIt`, `TestSlackApproverNarrowingTheProjectListTakesAPendingApprovalWithIt` |
| The check has exactly one production call site | `TestApproverAllowedHasExactlyOneProductionCallSite` |
| The list can actually be written | `TestConfigPolicyInputAcceptsAnApproverList` |
| `palai up` names what is open | `TestApproverListAbsenceIsSaidOutLoud` |

UAT case: **HIL-004**.
