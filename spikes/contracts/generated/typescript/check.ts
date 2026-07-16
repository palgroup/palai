// Generated semantic checker for Fixture.

import { readFileSync } from "node:fs";
import { basename } from "node:path";

import { decodeFixture, encodeFixture } from "./fixture.js";

const results = process.argv.slice(2).map((path) => {
  const fixture = decodeFixture(readFileSync(path, "utf8"));
  return {
    name: basename(path, ".json"),
    note_state: fixture.note.state,
    status: fixture.status,
    sequence: fixture.sequence.toString(),
    has_extra: Object.prototype.hasOwnProperty.call(fixture.unknownFields, "future_top_level"),
    has_future_meta: Object.prototype.hasOwnProperty.call(fixture.metadata, "future_metadata"),
    encoded: encodeFixture(fixture),
  };
});

process.stdout.write(`${JSON.stringify(results)}\n`);
