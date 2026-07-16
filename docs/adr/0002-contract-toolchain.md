# ADR-0002: Contract toolchain baseline

- Status: accepted
- Date: 2026-07-16
- Owners: Palai maintainers
- Supersedes: none
- Superseded by: none
- Hard-gate exceptions: none
- Production readiness: not established

## Context

Palai's HTTP contract must retain JSON Schema semantics even when current code
generators implement different OpenAPI versions. In particular, omitted versus
null values, open enums, unknown fields, and 64-bit integers cannot be weakened
to suit a generator.

## Evidence and options

- [Contract toolchain report](../../spikes/reports/contract-toolchain.json)
  records 20 consecutive lossless corpus passes in TypeScript, Python, and Go
  and an equivalent OpenAPI 3.2.0 to 3.1.2 projection.
- `oapi-codegen` 2.7.2 rejected the canonical nullable input and therefore
  cannot consume the canonical document directly in this baseline.
- `json-schema-to-typescript` 15.0.4 loses exact 64-bit integer behavior without
  a wrapper.
- `datamodel-code-generator` 0.68.1 and `go-jsonschema` 0.23.1 merge omitted and
  null or omit the required unknown-field representation without wrappers.

Semantic loss in any corpus fixture was a hard rejection criterion. Partial
generators are retained only as deterministic implementation components behind
lossless generated transport types and conformance fixtures.

## Decision

Keep JSON Schema 2020-12 and OpenAPI 3.2 as canonical inputs. Mechanically
produce an OpenAPI 3.1.2 compatibility projection, semantic-diff it against the
canonical model, and generate lossless per-language transport types with the
wrappers demonstrated by the corpus.

## Scope

This decision covers canonical HTTP schemas, their compatibility projection,
and TypeScript/Python/Go transport generation. It does not choose handwritten
SDK ergonomics or define AsyncAPI and engine-protocol details beyond requiring
them to preserve the same semantics.

## Version and digest policy

The accepted candidate versions are `json-schema-to-typescript` 15.0.4,
`datamodel-code-generator` 0.68.1, `go-jsonschema` 0.23.1, and `oapi-codegen`
2.7.2, executed with the repository's exact Go, Node, pnpm, Python, and uv
locks. Version changes require regenerated output, a clean semantic diff, and
20/20 corpus passes in all three languages. Tool images, when introduced, must
be selected by immutable digest.

## Consequences

- Generated files may include small deterministic wrappers rather than raw
  generator output.
- A generator upgrade cannot silently alter nullability, open enums, unknown
  fields, or integer precision.
- The 3.1.2 document is a compatibility artifact, never the canonical source.

## Verification

Run `scripts/spikes/contract-toolchain quick`, `scripts/spikes/check-reports`,
and `bash scripts/verify/e01.sh`.

## Revisit triggers

Revisit when a selected generator gains native OpenAPI 3.2 support, a canonical
schema construct cannot be represented by the mechanical projection, any
language corpus differs, or a generator upgrade changes stable output.
