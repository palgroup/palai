from __future__ import annotations

import json
import re
from pathlib import Path

import pytest

from palai_engine import protocol
from palai_engine.__main__ import redact
from palai_engine.protocol import Emitter, ProtocolError

FRAME_ID = re.compile(r"^frm_[A-Za-z0-9_-]+$")
RFC3339 = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$")

FIXTURES = Path(__file__).parent / "fixtures"


def hello_frame() -> dict:
    return json.loads((FIXTURES / "supervisor-hello.jsonl").read_text())


def test_hello_produces_engine_ready() -> None:
    emitter = Emitter(run_id="run_1", attempt_id="att_1")
    ready = protocol.build_ready(emitter, hello_frame(), nonce="nonce0")

    assert ready["protocol"] == "engine.v1"
    assert ready["type"] == "engine.ready"
    assert ready["sequence"] == 1
    assert FRAME_ID.match(ready["id"])
    assert RFC3339.match(ready["time"])
    assert ready["reply_to"] == "frm_hello1"

    data = ready["data"]
    assert data["selected_protocol"] == "engine.v1"
    assert data["engine"]["name"] and data["engine"]["version"]
    assert data["max_frame_bytes"] == protocol.MAX_LINE_BYTES
    assert data["nonce"] == "nonce0"


def test_run_start_before_hello_is_denied() -> None:
    run_start = {
        "protocol": "engine.v1",
        "id": "frm_x",
        "type": "run.start",
        "sequence": 1,
        "time": "2026-07-16T12:00:00Z",
        "data": {},
    }
    with pytest.raises(ProtocolError):
        protocol.require_hello(run_start)
    # a genuine hello is accepted
    protocol.require_hello(hello_frame())


def test_encode_is_a_single_physical_line() -> None:
    emitter = Emitter(run_id="run_1", attempt_id="att_1")
    frame = emitter.build("progress", {"text": "line1\nline2 üñ"})
    line = protocol.encode(frame)
    assert "\n" not in line
    assert json.loads(line)["data"]["text"] == "line1\nline2 üñ"
    assert len(line.encode()) <= protocol.MAX_LINE_BYTES


def test_decode_rejects_non_json() -> None:
    with pytest.raises(ProtocolError):
        protocol.decode("{not json")


def test_emitter_is_monotonic_and_stamps_identity() -> None:
    emitter = Emitter(run_id="run_1", attempt_id="att_1")
    first = emitter.build("progress", {})
    second = emitter.build("progress", {})
    assert (first["sequence"], second["sequence"]) == (1, 2)
    assert first["run_id"] == "run_1"
    assert first["attempt_id"] == "att_1"
    assert first["id"] != second["id"]


def test_stable_logical_ids_are_deterministic_and_prefixed() -> None:
    assert protocol.model_request_id("run_9", 1) == protocol.model_request_id("run_9", 1)
    assert protocol.model_request_id("run_9", 1) != protocol.model_request_id("run_9", 2)
    assert protocol.model_request_id("run_9", 1).startswith("mreq_")
    assert protocol.tool_call_id("run_9", 1, 0).startswith("tcall_")
    assert protocol.tool_call_id("run_9", 1, 0) != protocol.tool_call_id("run_9", 1, 1)


def test_redact_masks_secret_shaped_fields() -> None:
    out = redact({"tool_call_id": "tcall_1", "api_key": "sk-secret", "note": "x" * 500})
    assert out["api_key"] == "***"
    assert out["tool_call_id"] == "tcall_1"
    assert len(out["note"]) < 500
