"""Mechanical pin: the hand-written envelope and limits must match the canonical
protocols/engine/engine.schema.json, so they cannot drift by human oversight."""

from __future__ import annotations

import json
from pathlib import Path

from palai_engine import protocol
from palai_engine.protocol import Emitter


def _schema() -> dict:
    for parent in Path(__file__).resolve().parents:
        candidate = parent / "protocols" / "engine" / "engine.schema.json"
        if candidate.exists():
            return json.loads(candidate.read_text())
    raise FileNotFoundError("engine.schema.json not found above the test file")


SCHEMA = _schema()

# The engine-to-controller types this engine actually emits (protocol.py / loop.py).
EMITTED_TYPES = {"engine.ready", "model.request", "tool.request", "output.item", "run.terminal", "protocol.error"}


def test_emitter_envelope_matches_schema() -> None:
    frame = Emitter(run_id="run_1", attempt_id="att_1").build("progress", {})
    required = set(SCHEMA["required"])
    properties = set(SCHEMA["properties"])
    assert required <= frame.keys(), f"missing required envelope fields: {required - frame.keys()}"
    assert frame.keys() <= properties, f"emitted fields absent from schema properties: {frame.keys() - properties}"


def test_max_line_bytes_matches_schema() -> None:
    assert protocol.MAX_LINE_BYTES == SCHEMA["$defs"]["limits"]["max_line_bytes"]


def test_emitted_types_are_declared_engine_types() -> None:
    assert EMITTED_TYPES <= set(SCHEMA["$defs"]["engine_types"]["enum"])
