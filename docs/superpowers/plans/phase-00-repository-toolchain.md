# Palai Repository and Toolchain Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Palai'yi bağımsız, public, Apache-2.0 lisanslı; local ve CI ortamında aynı deterministic foundation kontrollerini çalıştıran, korumalı bir repository haline getirmek.

**Architecture:** Foundation davranışı küçük Bash verifier'larında tutulur ve `Makefile` ile local/CI giriş noktalarına bağlanır. Dil toolchain manifest'leri yalnızca dependency kurulumu ve sonraki epic'lerin workspace sınırlarını tanımlar; runtime veya provider davranışı bu fazda eklenmez. GitHub workflow aynı `make verify` komutunu çalıştırır ve `main` branch protection bu check'i zorunlu kılar.

**Tech Stack:** Bash, GNU Make-compatible Makefile, Go 1.26.4, Node.js 22.22.2, pnpm 11.9.0, Python 3.14.3, uv 0.8.2, GitHub Actions, GitHub REST API.

---

## Locked file map

- `.github/CODEOWNERS`: public ownership boundary.
- `.github/workflows/ci.yml`: pinned-action foundation check.
- `.tool-versions`: exact local toolchain versions available on the reference development host.
- `CODE_OF_CONDUCT.md`: contributor conduct policy.
- `CONTRIBUTING.md`: local verification and pull-request contract.
- `SECURITY.md`: private vulnerability reporting policy; secrets are never accepted in issues.
- `Makefile`: stable command surface used locally and by CI.
- `go.mod`: Go module/toolchain identity; no runtime dependency yet.
- `package.json`, `pnpm-workspace.yaml`, `pnpm-lock.yaml`: JavaScript workspace identity and lock.
- `pyproject.toml`, `uv.lock`: Python workspace identity and lock.
- `scripts/test/foundation.sh`: black-box foundation regression suite.
- `scripts/verify/foundation.sh`: required file/version/license checks.
- `scripts/verify/repository-boundary.sh`: independent-repository and tracked-secret checks.
- `scripts/verify/repository-settings.sh`: GitHub visibility/default-branch/protection check.
- `docs/adr/0000-template.md`: mandatory ADR shape.

### Task 1: Executable repository-boundary contract

**Files:**

- Create: `scripts/test/foundation.sh`
- Create: `scripts/verify/repository-boundary.sh`
- Modify: `.gitignore`

- [ ] **Step 1: Write the failing black-box test**

Create `scripts/test/foundation.sh` with a test that executes `scripts/verify/repository-boundary.sh` from the repository root and rejects a copied checkout with a mismatched origin:

```bash
#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"

test -x scripts/verify/repository-boundary.sh
bash scripts/verify/repository-boundary.sh

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
git init -q "$tmp/repository"
git -C "$tmp/repository" remote add origin https://example.invalid/not-palai.git
if (cd "$tmp/repository" && PALAI_EXPECTED_ROOT="$tmp/repository" bash "$root/scripts/verify/repository-boundary.sh") >/dev/null 2>&1; then
  echo "repository boundary accepted a mismatched origin" >&2
  exit 1
fi
```

- [ ] **Step 2: Run the test and verify RED**

Run: `bash scripts/test/foundation.sh`

Expected: FAIL because `scripts/verify/repository-boundary.sh` does not exist or is not executable.

- [ ] **Step 3: Implement the minimum boundary verifier**

Create an executable `scripts/verify/repository-boundary.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
expected_root="${PALAI_EXPECTED_ROOT:-$PWD}"
expected_origin="${PALAI_EXPECTED_ORIGIN:-https://github.com/palgroup/palai.git}"

test "$root" = "$expected_root"
actual_origin="$(git remote get-url origin)"
test "${actual_origin%.git}" = "${expected_origin%.git}"
test ! -f .gitmodules
! git ls-files --stage | awk '$1 == "160000" { found=1 } END { exit !found }'

if git ls-files -z | while IFS= read -r -d '' path; do
  case "$path" in
    .env|.env.*|*/.env|*/.env.*|*credentials*|*secrets*)
      case "$path" in *.example|*/.env.example) ;; *) printf '%s\n' "$path"; esac
      ;;
  esac
done | grep -q .; then
  echo "tracked secret-like path found" >&2
  exit 1
fi
```

Keep `.worktrees/`, `.palai/`, raw evidence, lock-independent build output and secret-bearing `.env` files ignored.

- [ ] **Step 4: Run the regression test and boundary verifier**

Run: `bash scripts/test/foundation.sh && bash scripts/verify/repository-boundary.sh`

Expected: both commands exit 0; the negative-origin fixture is rejected.

- [ ] **Step 5: Commit**

```bash
git add .gitignore scripts/test/foundation.sh scripts/verify/repository-boundary.sh
git commit -m "test: enforce the Palai repository boundary"
```

### Task 2: Contribution, security, and decision governance

**Files:**

- Create: `.github/CODEOWNERS`
- Create: `CODE_OF_CONDUCT.md`
- Create: `CONTRIBUTING.md`
- Create: `SECURITY.md`
- Create: `docs/adr/0000-template.md`
- Create: `scripts/verify/foundation.sh`
- Modify: `scripts/test/foundation.sh`

- [ ] **Step 1: Extend the failing foundation test**

Append assertions to `scripts/test/foundation.sh`:

```bash
for path in \
  LICENSE README.md .github/CODEOWNERS CODE_OF_CONDUCT.md CONTRIBUTING.md \
  SECURITY.md docs/adr/0000-template.md scripts/verify/foundation.sh; do
  test -s "$path" || { echo "missing foundation file: $path" >&2; exit 1; }
done
bash scripts/verify/foundation.sh
```

- [ ] **Step 2: Run the test and verify RED**

Run: `bash scripts/test/foundation.sh`

Expected: FAIL naming `.github/CODEOWNERS` as the first missing file.

- [ ] **Step 3: Add the minimum governance documents and verifier**

Use `@pal-salih` as the initial CODEOWNER. `SECURITY.md` directs vulnerability reports to GitHub private vulnerability reporting and explicitly forbids secrets in issues. `CONTRIBUTING.md` requires feature branches, TDD, `make verify`, generated-drift checks, no secrets and scoped commits. `docs/adr/0000-template.md` requires status, context, evidence, decision, consequences and supersession.

Create executable `scripts/verify/foundation.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

required=(
  LICENSE README.md .github/CODEOWNERS CODE_OF_CONDUCT.md CONTRIBUTING.md
  SECURITY.md docs/adr/0000-template.md
)

for path in "${required[@]}"; do
  test -s "$path" || { echo "missing foundation file: $path" >&2; exit 1; }
done

grep -q 'Apache License' LICENSE
grep -q 'Private vulnerability reporting' SECURITY.md
```

- [ ] **Step 4: Run the narrow test**

Run: `bash scripts/test/foundation.sh`

Expected: PASS; all governance documents and their required policy markers are present.

- [ ] **Step 5: Commit**

```bash
git add .github/CODEOWNERS CODE_OF_CONDUCT.md CONTRIBUTING.md SECURITY.md docs/adr/0000-template.md scripts
git commit -m "docs: define Palai contribution and security governance"
```

### Task 3: Deterministic polyglot command surface

**Files:**

- Create: `.tool-versions`
- Create: `Makefile`
- Create: `go.mod`
- Create: `package.json`
- Create: `pnpm-workspace.yaml`
- Generate: `pnpm-lock.yaml`
- Create: `pyproject.toml`
- Generate: `uv.lock`
- Modify: `scripts/test/foundation.sh`
- Modify: `scripts/verify/foundation.sh`

- [ ] **Step 1: Add failing command-surface assertions**

Append to `scripts/test/foundation.sh`:

```bash
for target in bootstrap generate check-generated lint test-unit test-component test-e2e verify local-up local-down local-doctor uat-local-live; do
  make -n "$target" >/dev/null
done

go env GOTOOLCHAIN | grep -Eq '^(auto|local|go1\.26\.4)$'
node --version | grep -qx 'v22.22.2'
pnpm --version | grep -qx '11.9.0'
python3 --version | grep -qx 'Python 3.14.3'
uv --version | grep -Eq '^uv 0\.8\.2([[:space:]]|$)'
```

- [ ] **Step 2: Run the test and verify RED**

Run: `bash scripts/test/foundation.sh`

Expected: FAIL because `Makefile` and the root toolchain manifests are missing.

- [ ] **Step 3: Create minimal manifests and Make targets**

Create `go.mod` with module `github.com/palgroup/palai`, language version `1.26.0`, and `toolchain go1.26.4`. Create a private root `package.json` pinned to `pnpm@11.9.0` and Node `22.22.2`; the pnpm workspace contains `sdks/typescript` and `examples/nextjs-sdk`. Create a non-packaged uv root project requiring Python `>=3.14,<3.15`. Pin the same versions in `.tool-versions`.

Extend `scripts/verify/foundation.sh` with the toolchain-specific contract:

```bash
for path in Makefile go.mod pyproject.toml package.json pnpm-workspace.yaml pnpm-lock.yaml uv.lock .tool-versions; do
  test -s "$path" || { echo "missing toolchain file: $path" >&2; exit 1; }
done
grep -q 'github.com/palgroup/palai' go.mod
grep -q 'packageManager' package.json
```

The `Makefile` provides stable targets. `bootstrap` uses `go mod download`, `uv sync --locked` and `pnpm install --frozen-lockfile`. `verify` runs repository boundary, foundation, lint and available unit tests. Targets whose implementation belongs to later epics invoke their canonical script if present and otherwise fail with a precise `capability not implemented` message; `make -n` still proves that the command exists.

- [ ] **Step 4: Generate dependency locks**

Run: `uv lock && pnpm install --lockfile-only`

Expected: `uv.lock` and `pnpm-lock.yaml` are generated without application dependencies.

- [ ] **Step 5: Run bootstrap and verification**

Run: `make bootstrap && make verify`

Expected: exit 0 with repository, governance, lock and syntax checks green; no provider credential requested.

- [ ] **Step 6: Commit**

```bash
git add .tool-versions Makefile go.mod package.json pnpm-workspace.yaml pnpm-lock.yaml pyproject.toml uv.lock scripts
git commit -m "chore: establish deterministic Palai toolchains"
```

### Task 4: Pinned CI and remote repository policy

**Files:**

- Create: `.github/workflows/ci.yml`
- Create: `scripts/verify/repository-settings.sh`
- Modify: `scripts/test/foundation.sh`
- Modify: `README.md`

- [ ] **Step 1: Add failing CI/policy assertions**

Append to `scripts/test/foundation.sh`:

```bash
test -s .github/workflows/ci.yml
! grep -Eq 'uses: [^#[:space:]]+@(main|master|v[0-9]+)([[:space:]]|$)' .github/workflows/ci.yml
grep -q 'make verify' .github/workflows/ci.yml
test -x scripts/verify/repository-settings.sh
```

- [ ] **Step 2: Run the test and verify RED**

Run: `bash scripts/test/foundation.sh`

Expected: FAIL because `.github/workflows/ci.yml` is missing.

- [ ] **Step 3: Add pinned workflow and settings verifier**

Create `ci.yml` with one job named `Foundation` that checks out the repository, installs the exact Go/Node/pnpm/Python/uv versions, runs `make bootstrap`, then `make verify`. Pin every third-party action to a full commit SHA and retain the release tag only in an inline comment.

Create executable `scripts/verify/repository-settings.sh` that uses `gh api` and `jq` to assert:

```text
palgroup/palai is PUBLIC
default branch is main
main branch protection is enabled
required status check contains Foundation
force pushes and deletion are disabled
```

- [ ] **Step 4: Run local foundation verification**

Run: `bash scripts/test/foundation.sh && make verify`

Expected: exit 0; remote policy verification is not included until the branch is pushed and protection is configured.

- [ ] **Step 5: Commit and push the feature branch**

```bash
git add .github/workflows/ci.yml scripts README.md
git commit -m "ci: enforce the Palai foundation checks"
git push -u origin feat/self-host-foundation
```

- [ ] **Step 6: Configure and verify branch protection**

After the `Foundation` check has been registered, configure `main` protection with strict required status checks, administrator enforcement, required conversation resolution, blocked force-pushes and blocked deletion. Do not require a second reviewer until a second repository maintainer exists; release promotion remains a separate two-person policy.

Run: `bash scripts/verify/repository-settings.sh`

Expected: `repository_settings=PASS`.

## E00 exit audit

- [ ] `bash scripts/test/foundation.sh` passes including the negative-origin fixture.
- [ ] `make bootstrap && make verify` passes from the isolated worktree.
- [ ] `git diff --check` passes and no secret-like path is tracked.
- [ ] GitHub `Foundation` workflow passes on the feature branch.
- [ ] `main` protection requires `Foundation`, blocks force-push/delete and applies to administrators.
- [ ] E01 is the only next phase permitted to choose runtime, generator, runner transport, object store and build orchestration details.
