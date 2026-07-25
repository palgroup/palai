# Runbook — incident response

**Fires when:** an alert, a user report, or a suspicion. Start here even when you already think you
know which of the other four runbooks you need; step 3 is what makes the rest reconstructible.

**Pinned transcript:** [`transcripts/incident-response.txt`](./transcripts/incident-response.txt)

## 0. Decide the severity before you touch anything

Use the ladder in [`../../security/vulnerability-process.md`](../../security/vulnerability-process.md)
§2 (which quotes `MASTER-SPEC.md` §62.2 verbatim). Two calls people get wrong under pressure:

- A run that **reported success for work that did not happen** is a **P0** (`false success`), not a
  degraded-behaviour P2.
- A control the [`../../security/threat-model.md`](../../security/threat-model.md) marks `not claimed`
  is **not a vulnerability in the shipped security model**. Record it as a gap in
  [`../known-gaps-1.0.md`](../known-gaps-1.0.md) and stop escalating.

## 1. Is the stack healthy, and which check is not?

```sh
palai local doctor
```

Fourteen checks. Read the *failing rows only*; a `fail` names its own remedy. The reference for each
check is [`../operability.md`](../operability.md). Non-zero exit means at least one row is not green —
that is the whole contract, so it is safe to wire into monitoring.

## 2. What is this stack running?

```sh
palai version
docker exec "$PG" psql -U palai -d palai -c "SELECT max(version) AS chain_head FROM schema_revisions;"
```

`$PG` is the Postgres container name (`<compose-project>-postgres-1`). The build stamp and the schema
chain head together identify the deployment; a chain head **below** the binary's head means a migration
chain has not finished — see [`../upgrade.md`](../upgrade.md).

## 3. Capture diagnostics BEFORE changing anything

```sh
palai support-bundle --out "$WORK/support-bundle.tar.gz" --tail 200
tar tzf "$WORK/support-bundle.tar.gz"
```

Five redacted parts: doctor output, stack config, `compose ps`, `compose config`, and logs. The bundle
is redacted by construction ([`../operability.md`](../operability.md)) and is the right attachment for
a security report. Take it **first** — restarting a container to "see if it helps" destroys the logs
that would have explained it.

## 4. Has anything touched the audit journal?

```sh
openssl ecparam -genkey -name prime256v1 -noout -out "$WORK/anchor.key"
palai audit checkpoint --out "$WORK/ir-anchor" --signing-key "$WORK/anchor.key"
mkdir -p "$WORK/oob" && cp "$WORK/ir-anchor/palai-audit-checkpoint.pub" "$WORK/oob/"
palai audit verify --checkpoint "$WORK/ir-anchor/audit-checkpoint.json" --pubkey "$WORK/oob/palai-audit-checkpoint.pub"
```

> In an incident you verify against the anchor you cut **before** the incident, with the public key you
> hold **out of band**. The four lines above are the shape of that check — the transcript records them
> cutting a fresh anchor because there was no prior one on a clean stack. A fresh anchor cut *after* an
> incident proves nothing about what happened before it.

Any alert → [`audit-integrity-alert.md`](./audit-integrity-alert.md). No alert is **not** an all-clear:
the checkpoint only anchors a prefix, and `unanchored` rows are reported, not vouched for.

## 5. Production posture (production installs)

```sh
palai config validate
```

Audits [`../../../deploy/compose/production.yml`](../../../deploy/compose/production.yml) against
`production.env`. On a local stack this reports the missing production env file — that is the pinned
output in the transcript, and it is the expected result there, not a finding. On a production install
every row must be green before bring-up ([`../install.md`](../install.md) step 4).

## 6. Contain

| Situation | Go to |
|---|---|
| A credential may be exposed | [`key-compromise.md`](./key-compromise.md) |
| The audit chain alerted | [`audit-integrity-alert.md`](./audit-integrity-alert.md) |
| Data may be lost or corrupt | [`backup-restore.md`](./backup-restore.md), then [`../dr-drills.md`](../dr-drills.md) |
| A bad build is deployed | [`upgrade-rollback.md`](./upgrade-rollback.md) |
| A runner or sandbox may be compromised | Below |

**Suspected sandbox escape or a compromised runner.** The platform already fences on its own: a failed
sandbox destroy quarantines the host and denies new placement, and a fence advance rejects a stale
writer (`SAN-006`, `SAN-008`). Confirm with `palai local doctor` (`host_quarantine` row), then stop the
runner host service. Cordon, drain and revoke are gateway lifecycle primitives used by
`palai upgrade` and by graceful shutdown — **there is no operator CLI for them today**; the operator
lever is stopping the runner and rotating its enrollment credential
([`key-compromise.md`](./key-compromise.md) §B). This limitation is stated in
[`../upgrade.md`](../upgrade.md).

## 7. Close out

1. Fix, with a test that fails without the fix
   ([`../../security/vulnerability-process.md`](../../security/vulnerability-process.md) §3).
2. If a documented mitigation broke, update its row in
   [`../../security/threat-model.md`](../../security/threat-model.md) — its guard fails if the row's
   evidence stops resolving.
3. Anything not fixed in this cycle becomes a row in [`../known-gaps-1.0.md`](../known-gaps-1.0.md)
   with a decision, an owner and (for a P2 waiver) an expiry. Nothing closes by being forgotten.
4. Advisory, if a released artifact was affected:
   [`../../security/vulnerability-process.md`](../../security/vulnerability-process.md) §4.

## Honest ceiling

There is no on-call rota, no paging integration and no automated incident timeline. `/metrics` and the
alert rules in [`../observability.md`](../observability.md) are the detection surface; wiring them to a
pager is an operator task. Continuous audit verification is a plan §6 operator leg — `palai audit
verify` is the mechanism, not a running watchdog.
