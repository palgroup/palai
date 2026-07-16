#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
expected_root="${PALAI_EXPECTED_ROOT:-$PWD}"
expected_origin="${PALAI_EXPECTED_ORIGIN:-https://github.com/palgroup/palai.git}"

test "$root" = "$expected_root"
test "$(git remote get-url origin)" = "$expected_origin"
test ! -f .gitmodules

if git ls-files --stage | awk '$1 == "160000" { found=1 } END { exit !found }'; then
  echo "gitlink or submodule entry found" >&2
  exit 1
fi

secret_path_found=0
while IFS= read -r -d '' path; do
  basename="${path##*/}"
  case "$basename" in
    .env|.env.*|credentials.json|credentials.yml|credentials.yaml|secrets.json|secrets.yml|secrets.yaml|.npmrc|.pypirc)
      case "$basename" in
        *.example|*.sample|*.template) ;;
        *)
          echo "tracked secret-like path found: $path" >&2
          secret_path_found=1
          ;;
      esac
      ;;
  esac
done < <(git ls-files -z)

test "$secret_path_found" -eq 0

common_dir="$(git rev-parse --path-format=absolute --git-common-dir)"
checkout_root="$(dirname "$common_dir")"
checkout_parent="$(dirname "$checkout_root")"
if parent_root="$(git -C "$checkout_parent" rev-parse --show-toplevel 2>/dev/null)" && test "$parent_root" != "$checkout_root"; then
  relative_checkout="${checkout_root#"$parent_root"/}"
  if git -C "$parent_root" ls-files --stage -- "$relative_checkout" | grep -q .; then
    echo "parent repository tracks Palai at $relative_checkout" >&2
    exit 1
  fi
fi
