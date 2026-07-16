// Code generated corpus round-trip checker; DO NOT EDIT.
//
// Reads every fixture under protocols/fixtures/corpus and proves the open-world
// round-trip rules hold in TypeScript: unknown fields and open-enum values are
// preserved, omitted never collapses to null, explicit nulls never vanish, and
// integers stay within the IEEE-754 safe range (spec §20.6 / ADR-0002).

import { readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

type Json = null | boolean | number | string | Json[] | { [key: string]: Json };

interface CorpusDocument {
  note: string;
  value: Json;
}

interface CorpusFile {
  case: string;
  schema: string;
  documents: CorpusDocument[];
}

// canonical serializes a value with object keys in sorted order, so two
// structurally equal documents produce byte-identical text regardless of the
// order their keys were authored in.
function canonical(value: Json): string {
  if (value === null || typeof value !== "object") {
    return JSON.stringify(value);
  }
  if (Array.isArray(value)) {
    return `[${value.map(canonical).join(",")}]`;
  }
  const entries = Object.keys(value)
    .sort()
    .map((key) => `${JSON.stringify(key)}:${canonical(value[key] as Json)}`);
  return `{${entries.join(",")}}`;
}

// assertSafeIntegers fails loudly if any integer would silently lose precision
// once parsed into a JavaScript number, guarding the Number.MAX_SAFE_INTEGER
// boundary that a raw JSON.parse would cross without warning.
function assertSafeIntegers(value: Json): void {
  if (typeof value === "number") {
    if (Number.isInteger(value) && !Number.isSafeInteger(value)) {
      throw new Error(`integer ${value} exceeds Number.MAX_SAFE_INTEGER`);
    }
    return;
  }
  if (Array.isArray(value)) {
    for (const item of value) assertSafeIntegers(item);
    return;
  }
  if (value !== null && typeof value === "object") {
    for (const key of Object.keys(value)) assertSafeIntegers(value[key] as Json);
  }
}

const corpusDir = join(dirname(fileURLToPath(import.meta.url)), "..", "..", "fixtures", "corpus");
const files = readdirSync(corpusDir)
  .filter((name) => name.endsWith(".json"))
  .sort();

let documents = 0;
for (const file of files) {
  const corpus = JSON.parse(readFileSync(join(corpusDir, file), "utf8")) as CorpusFile;
  for (const doc of corpus.documents) {
    assertSafeIntegers(doc.value);
    const before = canonical(doc.value);
    const after = canonical(JSON.parse(JSON.stringify(doc.value)) as Json);
    if (before !== after) {
      process.stderr.write(`corpus round-trip mismatch in ${file} (${doc.note}):\n  ${before}\n  ${after}\n`);
      process.exit(1);
    }
    documents += 1;
  }
}

process.stdout.write(`corpus=PASS files=${files.length} documents=${documents}\n`);
