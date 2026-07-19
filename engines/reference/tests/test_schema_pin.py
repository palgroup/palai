"""Mechanical pin: the hand-written envelope and limits must match the canonical
protocols/engine/engine.schema.json, so they cannot drift by human oversight."""

from __future__ import annotations

import json
from pathlib import Path

from palai_engine import protocol
from palai_engine.protocol import Emitter

FIXTURES = Path(__file__).parent / "fixtures"


def _schema() -> dict:
    for parent in Path(__file__).resolve().parents:
        candidate = parent / "protocols" / "engine" / "engine.schema.json"
        if candidate.exists():
            return json.loads(candidate.read_text())
    raise FileNotFoundError("engine.schema.json not found above the test file")


SCHEMA = _schema()

# The engine-to-controller types this engine actually emits (protocol.py / loop.py). child.request
# is the T5 addition — a required delegation seeded in run.start is emitted as a child.request.
EMITTED_TYPES = {"engine.ready", "model.request", "tool.request", "child.request", "output.item", "run.terminal", "protocol.error"}

# The controller-to-engine types the loop accepts and acts on (loop.py). message.deliver is
# the T2 addition; child.result is the T5 addition — the controller replies a dispatched (or
# denied) delegation as a child.result the loop folds back or fails on.
HANDLED_CONTROLLER_TYPES = {"supervisor.hello", "run.start", "model.result", "tool.result", "run.cancel", "message.deliver", "child.result"}


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


def test_handled_controller_types_are_declared_controller_types() -> None:
    assert HANDLED_CONTROLLER_TYPES <= set(SCHEMA["$defs"]["controller_types"]["enum"])


def test_engine_ready_announces_the_supported_commands() -> None:
    # engine.ready.commands must declare exactly the command kinds the engine really applies
    # (spec §9.1); a drift here is a false capability advertisement.
    hello = json.loads((FIXTURES / "supervisor-hello.jsonl").read_text())
    ready = protocol.build_ready(Emitter(run_id="run_1", attempt_id="att_1"), hello, nonce="n")
    assert ready["data"]["commands"] == list(protocol.SUPPORTED_COMMANDS)
    assert "send_message" in ready["data"]["commands"]
