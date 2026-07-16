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
repository_root="$(cd "$tmp/repository" && pwd -P)"
if (cd "$repository_root" && PALAI_EXPECTED_ROOT="$repository_root" bash "$root/scripts/verify/repository-boundary.sh") >/dev/null 2>&1; then
  echo "repository boundary accepted a mismatched origin" >&2
  exit 1
fi

git init -q "$tmp/with-gitmodules"
git -C "$tmp/with-gitmodules" remote add origin https://github.com/palgroup/palai.git
touch "$tmp/with-gitmodules/.gitmodules"
gitmodules_root="$(cd "$tmp/with-gitmodules" && pwd -P)"
if (cd "$gitmodules_root" && PALAI_EXPECTED_ROOT="$gitmodules_root" bash "$root/scripts/verify/repository-boundary.sh") >/dev/null 2>&1; then
  echo "repository boundary accepted .gitmodules" >&2
  exit 1
fi

git init -q "$tmp/with-gitlink"
git -C "$tmp/with-gitlink" remote add origin https://github.com/palgroup/palai.git
git -C "$tmp/with-gitlink" update-index --add --info-only --cacheinfo 160000,1111111111111111111111111111111111111111,embedded
gitlink_root="$(cd "$tmp/with-gitlink" && pwd -P)"
if (cd "$gitlink_root" && PALAI_EXPECTED_ROOT="$gitlink_root" bash "$root/scripts/verify/repository-boundary.sh") >/dev/null 2>&1; then
  echo "repository boundary accepted a gitlink" >&2
  exit 1
fi

secret_names=(
  .env .env.local credentials.json credentials.yml credentials.yaml
  secrets.json secrets.yml secrets.yaml .npmrc .pypirc
)
for index in "${!secret_names[@]}"; do
  fixture="$tmp/with-secret-$index"
  secret_name="${secret_names[$index]}"
  git init -q "$fixture"
  git -C "$fixture" remote add origin https://github.com/palgroup/palai.git
  touch "$fixture/$secret_name"
  git -C "$fixture" add -f "$secret_name"
  secret_root="$(cd "$fixture" && pwd -P)"
  if (cd "$secret_root" && PALAI_EXPECTED_ROOT="$secret_root" bash "$root/scripts/verify/repository-boundary.sh") >/dev/null 2>&1; then
    echo "repository boundary accepted tracked $secret_name" >&2
    exit 1
  fi
done

git init -q "$tmp/with-secret-example"
git -C "$tmp/with-secret-example" remote add origin https://github.com/palgroup/palai.git
touch "$tmp/with-secret-example/.env.example"
git -C "$tmp/with-secret-example" add -f .env.example
example_root="$(cd "$tmp/with-secret-example" && pwd -P)"
(cd "$example_root" && PALAI_EXPECTED_ROOT="$example_root" bash "$root/scripts/verify/repository-boundary.sh")

git init -q "$tmp/parent"
mkdir -p "$tmp/parent/palai"
touch "$tmp/parent/palai/tracked.txt"
git -C "$tmp/parent" add palai/tracked.txt
git init -q "$tmp/parent/palai"
git -C "$tmp/parent/palai" remote add origin https://github.com/palgroup/palai.git
nested_root="$(cd "$tmp/parent/palai" && pwd -P)"
if (cd "$nested_root" && PALAI_EXPECTED_ROOT="$nested_root" bash "$root/scripts/verify/repository-boundary.sh") >/dev/null 2>&1; then
  echo "repository boundary accepted files tracked by a parent repository" >&2
  exit 1
fi

for path in \
  LICENSE README.md .github/CODEOWNERS CODE_OF_CONDUCT.md CONTRIBUTING.md \
  SECURITY.md docs/adr/0000-template.md scripts/verify/foundation.sh; do
  test -s "$path" || { echo "missing foundation file: $path" >&2; exit 1; }
done
bash scripts/verify/foundation.sh

for target in bootstrap generate check-generated lint test-unit test-component test-e2e verify local-up local-down local-doctor uat-local-live; do
  make -n "$target" >/dev/null
done

go env GOTOOLCHAIN | grep -Eq '^(auto|go1\.26\.4)$'
node --version | grep -qx 'v22.22.2'
pnpm --version | grep -qx '11.9.0'
python3 --version | grep -qx 'Python 3.14.3'
uv --version | grep -q '^uv 0.8.2 '

test -s .github/workflows/ci.yml
if grep -Eq 'uses: [^#[:space:]]+@(main|master|v[0-9]+)([[:space:]]|$)' .github/workflows/ci.yml; then
  echo "GitHub Action is not pinned to a commit" >&2
  exit 1
fi
grep -q 'make verify' .github/workflows/ci.yml
test -x scripts/verify/repository-settings.sh
bash scripts/test/repository-settings.sh
