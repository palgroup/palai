# Cross-language contract spike

This spike treats `schemas/fixture.json` as the canonical JSON Schema 2020-12 contract and `openapi-3.2.yaml` as the canonical API document. A mechanical projection changes only the OpenAPI document version to 3.1.2; tests compare the normalized `Fixture` schema in both documents to the canonical JSON Schema.

## Lossless corpus

The fixture deliberately exercises transport details that ordinary generated types often erase:

- optional `note` omitted, explicitly `null`, and empty string are three states;
- `status` is an open string with known-value documentation, not a closed enum;
- top-level and metadata objects preserve unknown fields;
- `sequence` is the integer `9007199254740993`, above JavaScript's safe integer limit; and
- `created_at` is RFC3339.

The generated Go codec uses an explicit presence/null wrapper, `int64` and `json.RawMessage` unknown bags. Python uses a missing sentinel, arbitrary-precision `int` and unknown dictionaries. TypeScript protects the raw integer token before `JSON.parse`, exposes `bigint`, and reinserts the exact integer token on encode. These templates intentionally cover only constructs in the corpus.

## External generator findings

All candidates are pinned and executed rather than assessed from documentation alone:

- `json-schema-to-typescript` 15.0.4 is partial because it emits `number` for the int64;
- `datamodel-code-generator` 0.68.1 is partial because `note = None` merges missing/null and the dataclass has no unknown-field bag;
- `go-jsonschema` 0.23.1 is partial because a pointer merges missing/null and decoded additional properties are not flattened during encode; and
- `oapi-codegen` 2.7.2 is rejected because it explicitly fails on the 3.1.2 nullable multi-type schema.

`candidate-findings.json` records these expectations. The evidence command verifies exit codes and emitted source patterns and hashes every candidate output. A changed generator behavior fails the spike until the finding is reviewed.

## Commands

Run deterministic generation and one semantic pass:

```bash
scripts/spikes/contract-toolchain quick
```

Run external candidates plus 20 repetitions and produce raw evidence from a clean commit:

```bash
PALAI_SPIKE_REPORT_OUT=spikes/.evidence/contract-toolchain.json \
  scripts/spikes/contract-toolchain evidence
```
