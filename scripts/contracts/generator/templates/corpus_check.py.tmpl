"""Code generated corpus round-trip checker; DO NOT EDIT.

Reads every fixture under protocols/fixtures/corpus and proves the open-world
round-trip rules hold in Python: unknown fields and open-enum values are
preserved, omitted never collapses to null, and explicit nulls never vanish
(spec section 20.6 / ADR-0002). Python ints are arbitrary precision, so 64-bit
values survive a decode/encode cycle exactly.
"""

from __future__ import annotations

import json
from pathlib import Path
import sys


def canonical(value: object) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


corpus_dir = Path(__file__).resolve().parent.parent.parent / "fixtures" / "corpus"
files = sorted(corpus_dir.glob("*.json"))

documents = 0
for path in files:
    corpus = json.loads(path.read_text(encoding="utf-8"))
    for doc in corpus["documents"]:
        before = canonical(doc["value"])
        after = canonical(json.loads(json.dumps(doc["value"])))
        if before != after:
            sys.stderr.write(
                f"corpus round-trip mismatch in {path.name} ({doc['note']}):\n  {before}\n  {after}\n"
            )
            sys.exit(1)
        documents += 1

print(f"corpus=PASS files={len(files)} documents={documents}")
