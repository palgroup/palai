# Operational runbooks

Five thin runbooks for the moments an operator is under time pressure. They are deliberately **short
and full of links**: the reference documentation for install, backup, upgrade, DR and observability
already exists in [`../`](../) and is not copied here. A runbook's job is to say *what to do, in what
order, and when to stop* — the detail lives one link away.

| Runbook | Fires when | Transcript |
|---|---|---|
| [`incident-response.md`](./incident-response.md) | Anything unexpected: an alert, a report, a suspicion | [`transcripts/incident-response.txt`](./transcripts/incident-response.txt) |
| [`key-compromise.md`](./key-compromise.md) | An API key, runner token, secret or signing key may be in the wrong hands | [`transcripts/key-compromise.txt`](./transcripts/key-compromise.txt) |
| [`backup-restore.md`](./backup-restore.md) | You need a backup, or you need to know a backup is good | [`transcripts/backup-restore.txt`](./transcripts/backup-restore.txt) |
| [`upgrade-rollback.md`](./upgrade-rollback.md) | An upgrade is planned, or one has gone wrong | [`transcripts/upgrade-rollback.txt`](./transcripts/upgrade-rollback.txt) |
| [`audit-integrity-alert.md`](./audit-integrity-alert.md) | `palai audit verify` raised `tamper`, `gap`, `stale` or `signature` | [`transcripts/audit-integrity-alert.txt`](./transcripts/audit-integrity-alert.txt) |

## Every command here was run

A runbook whose commands were never executed is a paper runbook. Every command in every file was run
once against a real stack and its **real** output is pinned in the matching transcript —
including the commands that fail, because a refusal is exactly what an operator needs to recognise.

- Recorder: [`../../../scripts/ops/record-runbook-transcripts.sh`](../../../scripts/ops/record-runbook-transcripts.sh)
- Guard: `TestRunbookCommandsWereExecuted` in `tests/docs/ops_docs_test.go` requires that **every**
  command line in a runbook appears in a recorded transcript. A command cannot be added to a runbook
  without being run.

There is exactly one escape hatch, and it is explicit in the source. A block preceded by
`<!-- drill: <make target> -->` declares "**the commands below are executed by `make <target>`, not in
this transcript**" — and the guard then requires that target to exist in the `Makefile`. That is how
the genuinely destructive steps (the N→N+1 swap, the rollback, restore into a second clean stack) stay
in the runbooks without either lying about having been run here or being re-run on every recording.

## The stack the transcripts came from, and what that does not cover

The transcripts were recorded against a `palai local up` stack — a real four-service compose stack
(control plane, runner, Postgres, object store) on macOS + Docker Desktop. Two things are therefore
**absent from the transcripts, honestly and by construction**:

1. **The secret-ref surface is unmounted** on a local stack, because the master key is a
   production-overlay concern ([`../install.md`](../install.md) step 2). The key-compromise runbook
   pins the resulting `404` rather than a fabricated rotation.
2. **`palai config validate` audits the production overlay**, so on a local stack it reports the
   missing `production.env` — that failing output is in the transcript, labelled.

Heavy destructive procedures — the N→N+1 swap, restore into a second clean stack, the DR drills — are
**delegated to the existing drills that already execute them** (`make upgrade-drill`,
`make uat-self-host`, `make evidence-verify`) rather than re-run here. Each delegation names its make
target in the runbook, and the guard checks the target exists.
