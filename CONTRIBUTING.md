# Contributing to Palai

Palai is an Apache-2.0 open-source project. By submitting a contribution, you
agree that it is licensed under the repository license and that you have the
right to submit it.

## Development contract

1. Create a focused feature branch or isolated worktree from current `main`.
2. Derive explicit success criteria from `MASTER-SPEC.md` and the active child
   plan under `docs/superpowers/plans/`.
3. For behavior changes, write the smallest failing test and observe the
   expected failure before production code.
4. Implement only enough to satisfy the behavior, then refactor while green.
5. Run `make bootstrap` once and `make verify` before every pull request.
6. Include generated files with their source schema and run
   `make check-generated` whenever contracts change.
7. Keep commits single-purpose. Do not mix formatting or unrelated cleanup.

## Security and data rules

- Never commit API keys, credentials, `.env` files, raw provider payloads,
  customer content, or unredacted evidence.
- Use fake credentials in fixtures and verify that secret scanners reject them.
- Do not weaken tenant checks, fencing, approvals, retention, or audit behavior
  to make a test pass.
- Report vulnerabilities through the private process in
  [SECURITY.md](SECURITY.md).

## Pull requests

Describe the requirement, test-first evidence, verification commands, contract
or migration impact, and any operator-facing change. Public contract changes
require a specification RFC; an ADR is sufficient only for implementation
choices that preserve the public contract.

