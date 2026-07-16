#!/usr/bin/env bash
set -euo pipefail

required=(
  LICENSE
  README.md
  .github/CODEOWNERS
  CODE_OF_CONDUCT.md
  CONTRIBUTING.md
  SECURITY.md
  docs/adr/0000-template.md
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
