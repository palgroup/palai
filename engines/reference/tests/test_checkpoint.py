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

from test_loop import _first_model_result, ctrl, make_loop, run_start


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
    #
    # The offer is emitted BEFORE the next model.request so the control plane persists it at the
    # tool-boundary journal seq and it is durable before the (long) provider call — the checkpoint
    # anchors AT the tool boundary, not one model step ahead.
    loop, tcall = _drive_to_pending_tool("run_tb")
    step_at_boundary = loop._step  # the step whose model call produced the tool_calls
    out = loop.handle(ctrl("tool.result", {"tool_call_id": tcall, "content": "42"}, "frm_tr"))

    assert [f["type"] for f in out] == ["checkpoint.offer", "model.request"]
    offer = out[0]
    assert offer["data"]["boundary_kind"] == "tool"
    # The captured state anchors at the tool boundary — the step is NOT advanced to the next one.
    captured = checkpoint.decode(base64.b64decode(offer["data"]["state"]))
    assert captured["step"] == step_at_boundary
    assert out[1]["data"]["model_request_id"] != offer  # the next model step follows the offer


def test_child_completion_offers_checkpoint_boundary() -> None:
    # Every completed step is a §26.5 checkpoint boundary — tool OR delegation — so the newest
    # checkpoint always sits at the last committed step. A delegation-completion resume that did NOT
    # offer would leave the checkpoint >=1 committed step behind; after a restore the controller's
    # restored-boundary gate would then mislabel a REPLAYED step's boundary as live, folding a fresh
    # message into a replayed step (§26.9 silent divergence). So child completion must offer, like tool
    # completion (MUST-FIX #1).
    loop = make_loop("run_childcp")
    spec = {"role": "r", "objective": "o", "model": "m", "required": False}
    out = _first_model_result(loop, [spec])
    child_req = next(f for f in out if f["type"] == "child.request")
    resume = loop.handle(
        ctrl(
            "child.result",
            {"child_request_id": child_req["data"]["child_request_id"], "status": "completed", "output": "done", "child_run_id": "run_kid"},
            "frm_cr",
        )
    )
    types = [f["type"] for f in resume]
    assert "checkpoint.offer" in types, f"child completion must offer a checkpoint boundary, got {types}"
    assert types.index("checkpoint.offer") < types.index("model.request")


def test_migrate_v1_to_v2_preserves_original() -> None:
    # ENG-011 (E10 Task 4): a sandboxed migration transforms a v1 captured state to v2 WITHOUT
    # mutating the original — the immutable v1 checkpoint is preserved and rollback stays possible.
    # The transform is a real, minimal shape change: v2 stamps the explicit state_version
    # discriminator that v1 checkpoints lacked. Honest ceiling: the production engine stays v1
    # (checkpoint_formats == ["reference-kernel/1"]); this proves the migration MECHANISM.
    loop, _ = _drive_to_pending_tool("run_mig")
    v1 = loop.capture_state()
    v1_bytes = checkpoint.encode(v1)

    v2 = checkpoint.migrate(v1)

    # A distinct object with a new content checksum, and the original left byte-for-byte untouched.
    assert v2 is not v1
    assert v2.get("state_version") == 2
    assert checkpoint.encode(v2) != v1_bytes
    assert checkpoint.encode(v1) == v1_bytes  # migrate did not mutate v1 in place

    # v2 restore-roundtrips: a fresh loop restores from the migrated bytes to the SAME boundary.
    restored = make_loop("run_mig")
    restored.restore_state(checkpoint.decode(checkpoint.encode(v2)))
    assert restored.state is loop.state
    assert restored._step == loop._step
    assert restored._pending_tools == loop._pending_tools
    assert restored.context._messages == loop.context._messages
