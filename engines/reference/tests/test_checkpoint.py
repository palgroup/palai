"""Reference-kernel checkpoint create-path (spec §26.1-26.2, §26.5, E10 Task 1).

The engine serializes its resumable loop state as deterministic, typed JSON bytes (NOT
pickle) so a checkpoint is portable and reproducible, and offers it at a safe boundary.
These tests pin the two deterministic obligations: a request produces an offer, and a
capture round-trips losslessly back to the same place in the run. The every-completed-tool
boundary offer and the durable persistence are proven by the gated fault smoke + the
control-plane component tests.
"""

from __future__ import annotations

import base64

from palai_engine import checkpoint
from palai_engine.loop import State

from test_loop import ctrl, make_loop, run_start


def _drive_to_pending_tool(run_id: str = "run_cp") -> tuple:
    """Advance a loop to AWAITING_TOOLS with one outstanding tool call, returning
    (loop, tool_call_id) — a genuine mid-run safe boundary to checkpoint from."""
    loop = make_loop(run_id)
    req = loop.handle(run_start())[0]
    mrid = req["data"]["model_request_id"]
    treq = loop.handle(
        ctrl("model.result", {"model_request_id": mrid, "tool_calls": [{"name": "t", "arguments": {}}]}, "frm_mr")
    )
    tcall = next(f for f in treq if f["type"] == "tool.request")["data"]["tool_call_id"]
    return loop, tcall


def test_checkpoint_request_frame_produces_offer() -> None:
    loop = make_loop("run_cp")
    loop.handle(run_start())  # step 1, AWAITING_MODEL
    before = loop.state

    out = loop.handle(ctrl("checkpoint.request", {}, "frm_cpr"))

    assert [f["type"] for f in out] == ["checkpoint.offer"]
    data = out[0]["data"]
    assert data["format"] == "reference-kernel"
    assert data["format_version"] == 1
    assert data["boundary_kind"] == "request"
    inner = checkpoint.decode(base64.b64decode(data["state"]))
    assert inner["step"] == 1
    # A checkpoint is non-mutating: the run stays exactly where it was.
    assert loop.state is before


def test_checkpoint_state_bytes_are_deterministic() -> None:
    # Same capture => same bytes => same content checksum (spec §26.2 content-addressing).
    loop_a, _ = _drive_to_pending_tool("run_det")
    loop_b, _ = _drive_to_pending_tool("run_det")
    assert checkpoint.encode(loop_a.capture_state()) == checkpoint.encode(loop_b.capture_state())


def test_checkpoint_roundtrip_restores_loop_state() -> None:
    loop, tcall = _drive_to_pending_tool("run_rt")
    assert loop.state is State.AWAITING_TOOLS

    raw = checkpoint.encode(loop.capture_state())
    restored = make_loop("run_rt")
    restored.restore_state(checkpoint.decode(raw))

    # Restored to the SAME place: same state, step, outstanding tools, and conversation.
    assert restored.state is loop.state
    assert restored._step == loop._step
    assert restored._pending_tools == loop._pending_tools
    assert restored.context._messages == loop.context._messages

    # And it genuinely resumes from the boundary: the tool result advances it to the next step,
    # exactly as the un-checkpointed loop would.
    out = restored.handle(ctrl("tool.result", {"tool_call_id": tcall, "content": "42"}, "frm_tr"))
    assert any(f["type"] == "model.request" for f in out)
    assert restored.state is State.AWAITING_MODEL


def test_completed_tool_boundary_offers_a_checkpoint() -> None:
    # The honest superset (spec §26.5, adjudicated E10 T1): the engine offers a checkpoint at
    # every completed-tool safe boundary. Per-tool side-effect classification (which offers the
    # control plane actually persists) is deferred to T7 — the offer is just an offer.
    loop, tcall = _drive_to_pending_tool("run_tb")
    out = loop.handle(ctrl("tool.result", {"tool_call_id": tcall, "content": "42"}, "frm_tr"))
    kinds = [f["type"] for f in out]
    # The next model step is requested, then a checkpoint of the resulting post-tool boundary offered.
    assert kinds == ["model.request", "checkpoint.offer"]
    assert out[-1]["data"]["boundary_kind"] == "tool"
