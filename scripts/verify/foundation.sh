#!/usr/bin/env bash
set -euo pipefail

required=(
  LICENSE
  README.md
  .github/CODEOWNERS
  .github/workflows/ci.yml
  CODE_OF_CONDUCT.md
  CONTRIBUTING.md
  SECURITY.md
  docs/adr/0000-template.md
  Makefile
  go.mod
  pyproject.toml
  package.json
  pnpm-workspace.yaml
  pnpm-lock.yaml
  uv.lock
  .tool-versions
  scripts/verify/repository-settings.sh
)

for path in "${required[@]}"; do
  test -s "$path" || {
    echo "missing foundation file: $path" >&2
    exit 1
  }
done

grep -q 'Apache License' LICENSE
grep -q 'Private vulnerability reporting' SECURITY.md
grep -q '^\* @pal-salih$' .github/CODEOWNERS
grep -q '^## Decision$' docs/adr/0000-template.md
grep -q 'github.com/palgroup/palai' go.mod
grep -q '"packageManager": "pnpm@11.9.0"' package.json
grep -q '^golang 1\.26\.4$' .tool-versions
grep -q '^nodejs 22\.22\.2$' .tool-versions
grep -q '^python 3\.14\.3$' .tool-versions
grep -q 'make verify' .github/workflows/ci.yml
if grep -Eq 'uses: [^#[:space:]]+@(main|master|v[0-9]+)([[:space:]]|$)' .github/workflows/ci.yml; then
  echo "GitHub Action is not pinned to a commit" >&2
  exit 1
fi
