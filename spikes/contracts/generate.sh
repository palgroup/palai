#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
contracts="$root/spikes/contracts"
profile="${1:-evidence}"
case "$profile" in
  quick|evidence) ;;
  *)
    echo "usage: spikes/contracts/generate.sh [quick|evidence]" >&2
    exit 2
    ;;
esac

go run ./spikes/contracts/generator -root "$contracts" -out "$contracts/generated"
pnpm --dir "$contracts/tooling" exec tsc --project "$contracts/generated/typescript/tsconfig.json"
PYTHONDONTWRITEBYTECODE=1 python3 "$contracts/generated/python/check.py" >/dev/null

if test "$profile" = quick; then
  echo "contract_generation=PASS profile=quick"
  exit 0
fi

candidates="$contracts/.build/candidates"
rm -rf "$candidates"
mkdir -p "$candidates"

set +e
pnpm --dir "$contracts/tooling" exec json2ts \
  -i "$contracts/schemas/fixture.json" \
  -o "$candidates/json-schema-to-typescript.ts" \
  --unknownAny \
  >"$candidates/json-schema-to-typescript.stdout" \
  2>"$candidates/json-schema-to-typescript.stderr"
printf '%s\n' "$?" >"$candidates/json-schema-to-typescript.status"

uv run --group spikes datamodel-codegen \
  --input "$contracts/schemas/fixture.json" \
  --input-file-type jsonschema \
  --schema-version 2020-12 \
  --schema-version-mode strict \
  --output "$candidates/datamodel-code-generator.py" \
  --output-model-type dataclasses.dataclass \
  --target-python-version 3.14 \
  --formatters builtin \
  --disable-timestamp \
  >"$candidates/datamodel-code-generator.stdout" \
  2>"$candidates/datamodel-code-generator.stderr"
printf '%s\n' "$?" >"$candidates/datamodel-code-generator.status"

go run github.com/atombender/go-jsonschema@v0.23.1 \
  --package candidate \
  --struct-name-from-title \
  --output "$candidates/go-jsonschema.go" \
  "$contracts/schemas/fixture.json" \
  >"$candidates/go-jsonschema.stdout" \
  2>"$candidates/go-jsonschema.stderr"
printf '%s\n' "$?" >"$candidates/go-jsonschema.status"

go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.7.2 \
  -generate types,skip-prune \
  -package candidate \
  -o "$candidates/oapi-codegen.go" \
  "$contracts/generated/openapi-3.1.2.yaml" \
  >"$candidates/oapi-codegen.stdout" \
  2>"$candidates/oapi-codegen.stderr"
printf '%s\n' "$?" >"$candidates/oapi-codegen.status"
set -e

go run ./spikes/contracts/cmd/candidate-check \
  -findings "$contracts/candidate-findings.json" \
  -candidate-dir "$candidates" \
  -out "$contracts/.build/candidate-summary.json"

echo "contract_generation=PASS profile=evidence"
