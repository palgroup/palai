# Palai Project Agent Instructions

These instructions apply to the entire Palai repository. They narrow the global
workflow for this project so the full self-hosted plan can be completed without
repeating expensive agent, review, build, and evidence cycles.

## Objective and scope

- Implement the complete self-hosted plan and prove it with real local and
  cloud-deployable self-host evidence. Do not reduce product scope to gain speed.
- SaaS billing/managed-service implementation is out of scope for this plan.
- Real provider keys are requested only when a planned live-provider gate needs
  them. Never read, print, log, or commit `.env.local` values.

## Fast task protocol

Use one implementation writer per task. The controller owns sequencing and the
working tree.

1. Read only the exact task, directly relevant spec sections, and nearby repo
   conventions.
2. Start the smallest meaningful RED test promptly; do not spend an open-ended
   turn producing research summaries.
3. Implement the minimum complete behavior and run narrow tests while iterating.
4. Run one consolidated acceptance review before expensive final evidence.
5. Fix only reproduced Critical/Important findings that affect explicit
   requirements, correctness, security, resource cleanup, or truthful evidence.
6. Create one clean source commit after review fixes.
7. Run live evidence once from that source commit, then create one evidence-only
   commit when the task requires it.
8. The controller runs one final repository-wide verification and pushes.

Do not create intermediate evidence commits before review. Do not regenerate
expensive evidence unless source behavior that it proves changed.

## Agent and review budget

- Implementers must not spawn nested agents unless the controller explicitly
  authorizes a truly independent, bounded subtask. Default: no nested agents.
- Use one fresh consolidated reviewer per task. That reviewer covers spec
  compliance, code quality, security, tests, and evidence integrity in one pass
  and must not spawn child reviewers.
- For Palai, the consolidated review replaces separate spec-compliance and
  code-quality review passes.
- One fix-and-re-review loop is the default. A further loop is allowed only for
  an unresolved, reproducible Critical/Important defect.
- Minor/nice-to-have findings do not block progress. Record them only when they
  are concrete; fix them immediately only if the change is trivial and in scope.
- Reviewers must cite file/line evidence and, for blocking findings, a concrete
  failure mode or reproduction. Speculative hardening and stylistic preferences
  are non-blocking.

## Verification budget

- During development, run the narrowest relevant unit/integration command.
- Run heavyweight live evidence at the final task gate, not after every edit.
- Run `make verify` once per task at final controller verification. Do not repeat
  it independently in implementer, reviewer, and controller turns unless a
  reproduced failure requires it.
- Reuse installed dependencies, build caches, pulled immutable images, and
  existing clean artifacts when their identity is verified.
- Preserve fail-closed timeouts, bounded output, cleanup, secret scanning, and
  source/evidence provenance, but do not build a generic framework beyond the
  task's acceptance criteria.

## Engineering discipline

- Keep changes surgical and avoid unrelated refactors or formatting churn.
- Prefer existing report, script, contract, and test patterns.
- Protect user Docker, Git, and local files. Delete only resources created and
  uniquely labeled by the current task; never prune or reset shared state.
- A task is complete only when its explicit behavior and live-proof gate pass,
  the worktree is clean, and the committed evidence truthfully states what was
  and was not proven.

## Communication

- Keep checkpoints short: current phase, concrete result, blocker if any.
- Final task handoffs list commits, targeted/live tests, evidence identity, and
  remaining limitations. Do not repeat full plans or narrate routine exploration.
