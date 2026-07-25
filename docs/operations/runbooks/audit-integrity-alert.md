# Runbook — audit integrity alert

**Fires when:** `palai audit verify` exits non-zero with `ALERT [tamper]`, `[gap]`, `[stale]` or
`[signature]`.

**Pinned transcript:** [`transcripts/audit-integrity-alert.txt`](./transcripts/audit-integrity-alert.txt)
— a green verify followed by all three journal alerts, produced by really corrupting a throwaway
journal.

> ## Read this before you escalate
>
> **An authorised retention purge is indistinguishable from tampering.** The §22.2 reaper
> (`scrub_events`) **UPDATEs** an anchored row's payload to `{"purged": true}` — it does not delete the
> row. Same row, same `seq`, different bytes: that is precisely the `tamper` signature. So on a stack
> with `PALAI_RETENTION_STORE_FALSE_TTL` set, **routine maintenance raises the highest-severity alert**,
> and an attacker who edits a row gets "the reaper did it" for free.
>
> Arms 1 and 2 of the transcript are **byte-for-byte the same alert** — one from an edit, one from a
> purge. The behaviour is pinned by `TestARetentionPurgeIsIndistinguishableFromTamper`
> (`packages/audit/chain_test.go`) so it cannot change without this document going stale, and it is
> tracked as row `AUD-1` in [`../known-gaps-1.0.md`](../known-gaps-1.0.md).
>
> **The operational consequence: re-cut the checkpoint immediately after any purge.** Until you do, the
> alert cannot tell the reaper from an attacker, and neither can you.

## 1. Read which alert it is

```sh
palai audit verify --checkpoint "$WORK/anchor/audit-checkpoint.json" --pubkey "$WORK/oob2/palai-audit-checkpoint.pub"
```

| Alert | Means | First question |
|---|---|---|
| `[tamper]` | Every anchored row is present, but the bytes no longer chain to the anchored head | **Did a retention purge run since the checkpoint was cut?** |
| `[gap]` | Anchored rows have been **removed** | The reaper does not delete. Nothing in production code does `DELETE FROM events` |
| `[stale]` | The checkpoint is older than `--not-older-than` | Is this the current anchor, or one restored from an old backup? |
| `[signature]` | The signature does not verify against the key you supplied | Wrong key, wrong checkpoint, or a compromised signer |

The command's own `CEILING` lines carry the same caveats; they are printed on **every** run, green or
not, so an operator meets them before an incident rather than during one.

## 2. `[tamper]` — purge or attack?

1. **Is `PALAI_RETENTION_STORE_FALSE_TTL` set on this stack?** `palai local doctor` reports the
   `retention_ttl` row. If retention is disabled (`ttl=0`), the reaper cannot be the explanation and
   you are looking at an edit.
2. **Did a purge run in the window?** The reaper logs its passes; the alert names the affected session.
3. If a purge did run: re-cut the anchor (§4) and re-verify. Green afterwards means the purge explains
   it. **Not green** means something else also changed — escalate.
4. If no purge ran: this is an edit nobody in the retention path made. **P0** —
   [`../../security/vulnerability-process.md`](../../security/vulnerability-process.md) §2 — go to
   [`incident-response.md`](./incident-response.md) and capture the support bundle before touching
   anything else.

## 3. `[gap]` — always escalate

No production code path deletes from `events`. A legitimate **hole** (a rolled-back insert burns a
`seq` number) is *not* a gap and does not alert — that distinction is pinned by
`TestALegitimateHoleIsNotAGap`. A `[gap]` means anchored rows were removed after the anchor was cut:
treat as P0.

## 4. Re-cut the anchor (and where to keep it)

```sh
palai audit checkpoint --out "$WORK/anchor" --signing-key "$WORK/anchor.key"
mkdir -p "$WORK/oob2" && cp "$WORK/anchor/palai-audit-checkpoint.pub" "$WORK/oob2/"
```

Three rules the tool enforces or warns about, all visible in the transcript:

- **The checkpoint lives outside the database.** An anchor stored where the thing it anchors can be
  rewritten anchors nothing.
- **The public key travels out of band.** Verifying against a key sitting beside the checkpoint proves
  self-consistency and nothing else — hence the `cp` into a separate directory above, and the
  in-bundle-key refusal in the release verifier.
- **Keep the checkpoint somewhere a database restore cannot roll back with it.** An *old* validly
  signed checkpoint verifies green over its own prefix and turns everything since into merely
  "unanchored" — pass `--not-older-than` and `--min-anchored` to turn that into a `[stale]` alert
  (`TestAnOldValidlySignedCheckpointIsStale`).

Re-cut after: a retention purge, an upgrade, a restore, and on whatever routine cadence you choose.
Cadence is operator policy; the tool reports `unanchored` rows and explicitly does not vouch for them.

## 5. The drill (throwaway stacks only)

<!-- drill: uat-escape -->
```sh
make uat-escape
```

The transcript's three arms are reproduced by
[`../../../scripts/ops/record-runbook-transcripts.sh`](../../../scripts/ops/record-runbook-transcripts.sh),
which corrupts a throwaway journal on purpose so the alert has been seen before it matters. The
deterministic equivalents that run in CI are `TestAuditIntegrityFourArms`
(`tests/component/postgres/audit_integrity_test.go`) and the `packages/audit` unit arms;
`make uat-escape` is the sibling sandbox-escape suite an incident usually wants next.

## What this control does and does not cover

| | |
|---|---|
| Covers | The `events` session journal, chain recomputed **from the rows** against a signed out-of-DB anchor |
| Does **not** cover | `audit_events` (§50.3) — that table is protected by a `REVOKE` of UPDATE/DELETE instead. Neither control substitutes for the other |
| Does **not** cover | Rows written **since** the last cut. A checkpoint anchors a prefix |
| Does **not** cover | Continuous verification. This command is the mechanism; wiring it to alerting on a schedule is a plan §6 operator leg |
| Read-scope | Verification reads system-scoped; a row-level-scoped connection is refused, because a check that could not see a row could not notice its deletion (`TestAuditReadRefusesARowLevelScopedConnection`) |
