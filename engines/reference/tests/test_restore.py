"""Reference-kernel restore path (spec §26.3 portable-checkpoint restore, E10 Task 4).

A run.restore frame reconstructs a fresh loop from a captured checkpoint and re-derives the
frame the run was awaiting when it was cut — the next model.request at a completed-tool boundary,
or the still-outstanding tool.requests at a pause boundary. The version is checked FIRST: an
unknown format_version is a protocol.error, never a silent half-restore.
"""

from __future__ import annotations

from palai_engine import checkpoint, protocol
from palai_engine.loop import State

from test_loop import ctrl, make_loop, run_start
from test_checkpoint import _drive_to_pending_tool


def _restore_frame(offer: dict, *, format_version: int | None = None) -> dict:
    data = offer["data"]
    return ctrl(
        "run.restore",
        {
            "format": data["format"],
            "format_version": data["format_version"] if format_version is None else format_version,
            "state": data["state"],
        },
        "frm_restore",
    )


def test_restore_roundtrip_resumes_from_tool_boundary() -> None:
    run_id = "run_restore1"
    loop, tcall = _drive_to_pending_tool(run_id)
    step = loop._step
    out = loop.handle(ctrl("tool.result", {"tool_call_id": tcall, "content": "42"}, "frm_tr"))
    offer = next(f for f in out if f["type"] == "checkpoint.offer")
    next_req = next(f for f in out if f["type"] == "model.request")

    restored = make_loop(run_id)
    rout = restored.handle(_restore_frame(offer))

    # The tool-boundary checkpoint (state=awaiting_tools, pending empty) resumes into the NEXT model
    # step: output is EXACTLY the model.request with the deterministic id for step+1.
    assert [f["type"] for f in rout] == ["model.request"]
    assert rout[0]["data"]["model_request_id"] == protocol.model_request_id(run_id, step + 1)
    assert rout[0]["data"]["model_request_id"] == next_req["data"]["model_request_id"]
    assert restored.state is State.AWAITING_MODEL

    # A restore advanced past AWAITING_START, so a following run.start is unexpected (no double-begin).
    err = restored.handle(run_start())
    assert err[0]["type"] == "protocol.error"
    assert err[0]["data"]["code"] == "unexpected_frame"


def test_restore_reemits_pending_tool_requests() -> None:
    run_id = "run_restore2"
    loop = make_loop(run_id)
    req = loop.handle(run_start())[0]
    mrid = req["data"]["model_request_id"]
    treqs = loop.handle(
        ctrl(
            "model.result",
            {
                "model_request_id": mrid,
                "tool_calls": [
                    {"name": "alpha", "arguments": {"x": 1}},
                    {"name": "beta", "arguments": {"y": 2}},
                ],
            },
            "frm_mr",
        )
    )
    tool_frames = [f for f in treqs if f["type"] == "tool.request"]
    assert len(tool_frames) == 2
    c0, c1 = tool_frames[0]["data"]["tool_call_id"], tool_frames[1]["data"]["tool_call_id"]

    # Answer the first tool; the second stays outstanding — a genuine "pause moment" mid-turn.
    loop.handle(ctrl("tool.result", {"tool_call_id": c0, "content": "done"}, "frm_t0"))
    assert loop.state is State.AWAITING_TOOLS
    offer = loop.handle(ctrl("checkpoint.request", {}, "frm_cpr"))[0]

    restored = make_loop(run_id)
    rout = restored.handle(_restore_frame(offer))

    # Only the still-outstanding tool is re-derived, with the SAME deterministic id and the
    # name/arguments from the last assistant turn. The completed tool gets NO frame (no double-run).
    assert [f["type"] for f in rout] == ["tool.request"]
    assert rout[0]["data"]["tool_call_id"] == c1
    assert rout[0]["data"]["name"] == "beta"
    assert rout[0]["data"]["arguments"] == {"y": 2}
    assert restored.state is State.AWAITING_TOOLS


def test_restore_rejects_malformed_state() -> None:
    # A decodable-but-malformed state (missing required keys) is a protocol.error, not a KeyError that
    # crashes the engine — restore_state is inside the try (minor 7).
    import base64
    import json

    bad = base64.b64encode(json.dumps({"state": "awaiting_model"}).encode()).decode()  # missing step/context/...
    restored = make_loop("run_bad")
    out = restored.handle(ctrl("run.restore", {"format": "reference-kernel", "format_version": 1, "state": bad}, "frm_r"))
    assert [f["type"] for f in out] == ["protocol.error"]
    assert out[0]["data"]["code"] == "invalid_checkpoint"


def test_restore_rejects_unknown_format_version() -> None:
    run_id = "run_restore3"
    loop, tcall = _drive_to_pending_tool(run_id)
    out = loop.handle(ctrl("tool.result", {"tool_call_id": tcall, "content": "42"}, "frm_tr"))
    offer = next(f for f in out if f["type"] == "checkpoint.offer")

    restored = make_loop(run_id)
    rout = restored.handle(_restore_frame(offer, format_version=99))

    assert [f["type"] for f in rout] == ["protocol.error"]
    assert rout[0]["data"]["code"] == "incompatible_checkpoint"
    # No silent half-restore: the loop stayed exactly where it started.
    assert restored.state is State.AWAITING_START
