from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from pathlib import Path

from palai_engine import protocol
from palai_engine.context import Context
from palai_engine.loop import Loop, State
from palai_engine.protocol import Emitter

FRAME_ID = re.compile(r"^frm_[A-Za-z0-9_-]+$")
MREQ = re.compile(r"^mreq_[A-Za-z0-9_-]+$")
TCALL = re.compile(r"^tcall_[A-Za-z0-9_-]+$")
RFC3339 = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$")

FIXTURES = Path(__file__).parent / "fixtures"
SRC = Path(__file__).resolve().parents[1] / "src"


def make_loop(run_id: str = "run_1") -> Loop:
    return Loop(Emitter(run_id=run_id, attempt_id="att_1"), Context(run_id))


def ctrl(type_: str, data: dict, frame_id: str = "frm_c") -> dict:
    return {
        "protocol": "engine.v1",
        "id": frame_id,
        "type": type_,
        "sequence": 1,
        "time": "2026-07-16T12:00:00Z",
        "data": data,
    }


def run_start(text: str = "hello") -> dict:
    return ctrl("run.start", {"input": text}, frame_id="frm_start")


def test_run_start_requests_a_model_call() -> None:
    loop = make_loop()
    out = loop.handle(run_start())
    assert [f["type"] for f in out] == ["model.request"]
    assert loop.state is State.AWAITING_MODEL
    assert MREQ.match(out[0]["data"]["model_request_id"])
    assert out[0]["data"]["request_hash"]


def test_model_request_id_and_hash_are_stable_across_resume() -> None:
    first = make_loop("run_42").handle(run_start())[0]
    second = make_loop("run_42").handle(run_start())[0]
    assert first["data"]["model_request_id"] == second["data"]["model_request_id"]
    assert first["data"]["request_hash"] == second["data"]["request_hash"]


def test_tool_request_carries_stable_tool_call_id() -> None:
    loop = make_loop("run_7")
    req = loop.handle(run_start())[0]
    call = {"name": "search", "arguments": {"q": "x"}}
    out = loop.handle(
        ctrl("model.result", {"model_request_id": req["data"]["model_request_id"], "tool_calls": [call]}, "frm_mr")
    )
    assert [f["type"] for f in out] == ["tool.request"]
    assert loop.state is State.AWAITING_TOOLS
    tcall = out[0]["data"]["tool_call_id"]
    assert TCALL.match(tcall)

    resumed = make_loop("run_7")
    req2 = resumed.handle(run_start())[0]
    out2 = resumed.handle(
        ctrl("model.result", {"model_request_id": req2["data"]["model_request_id"], "tool_calls": [call]}, "frm_mr")
    )
    assert out2[0]["data"]["tool_call_id"] == tcall


def test_tool_result_resumes_with_the_next_model_request() -> None:
    loop = make_loop("run_9")
    req = loop.handle(run_start())[0]
    mrid = req["data"]["model_request_id"]
    treq = loop.handle(
        ctrl("model.result", {"model_request_id": mrid, "tool_calls": [{"name": "t", "arguments": {}}]}, "frm_mr")
    )[0]
    tcall = treq["data"]["tool_call_id"]

    out = loop.handle(ctrl("tool.result", {"tool_call_id": tcall, "content": "42"}, "frm_tr"))
    assert [f["type"] for f in out] == ["model.request"]
    assert out[0]["data"]["model_request_id"] != mrid
    assert loop.state is State.AWAITING_MODEL


def test_unknown_tool_result_is_rejected_without_resuming() -> None:
    loop = make_loop("run_9")
    req = loop.handle(run_start())[0]
    mrid = req["data"]["model_request_id"]
    loop.handle(ctrl("model.result", {"model_request_id": mrid, "tool_calls": [{"name": "t", "arguments": {}}]}, "frm_mr"))

    out = loop.handle(ctrl("tool.result", {"tool_call_id": "tcall_bogus", "content": "x"}, "frm_tr"))
    assert [f["type"] for f in out] == ["protocol.error"]
    assert loop.state is State.AWAITING_TOOLS


def test_delivered_message_folds_into_the_next_model_request() -> None:
    # A steered message mid-step is folded at the input boundary — it surfaces in the NEXT
    # model request (after the current step's tool results), never splitting the step.
    loop = make_loop("run_d")
    req = loop.handle(run_start("first"))[0]
    mrid = req["data"]["model_request_id"]
    treq = loop.handle(
        ctrl("model.result", {"model_request_id": mrid, "tool_calls": [{"name": "t", "arguments": {}}]}, "frm_mr")
    )[0]
    tcall = treq["data"]["tool_call_id"]

    out = loop.handle(ctrl("message.deliver", {"delivery": "steer", "message": "steered!"}, "frm_d"))
    assert out == []  # the fold emits no engine frame
    assert loop.state is State.AWAITING_TOOLS  # and does not advance the loop

    step2 = loop.handle(ctrl("tool.result", {"tool_call_id": tcall, "content": "42"}, "frm_tr"))[0]
    assert step2["type"] == "model.request"
    msgs = step2["data"]["messages"]
    tool_idx = next(i for i, m in enumerate(msgs) if m.get("role") == "tool")
    deliver_idx = next(i for i, m in enumerate(msgs) if m.get("content") == "steered!")
    assert deliver_idx > tool_idx  # folded AFTER the tool result: conversation order preserved


def test_interrupted_model_result_resumes_in_a_new_step() -> None:
    # An interrupt aborts the in-flight step; the engine resumes in a NEW model step that folds
    # the delivered message in, instead of finishing on the incomplete result (spec §9.2).
    loop = make_loop("run_i")
    req = loop.handle(run_start("go"))[0]
    mrid = req["data"]["model_request_id"]
    loop.handle(ctrl("message.deliver", {"delivery": "interrupt", "message": "changed my mind"}, "frm_d"))
    out = loop.handle(ctrl("model.result", {"model_request_id": mrid, "interrupted": True}, "frm_int"))
    assert [f["type"] for f in out] == ["model.request"]
    assert out[0]["data"]["model_request_id"] != mrid  # a genuinely new step
    contents = [m.get("content") for m in out[0]["data"]["messages"]]
    assert "changed my mind" in contents  # the delivered message folded into the resumed step
    assert loop.state is State.AWAITING_MODEL


def test_message_deliver_before_run_start_is_rejected() -> None:
    loop = make_loop()
    out = loop.handle(ctrl("message.deliver", {"delivery": "queue", "message": "early"}, "frm_d"))
    assert [f["type"] for f in out] == ["protocol.error"]
    assert loop.state is State.AWAITING_START


def test_cancellation_terminates_once_at_a_safe_boundary() -> None:
    loop = make_loop()
    loop.handle(run_start())  # awaiting_model
    out = loop.handle(ctrl("run.cancel", {"reason": "user"}, "frm_cancel"))
    assert [f["type"] for f in out] == ["run.terminal"]
    assert out[0]["data"]["outcome"] == "canceled"
    assert loop.state is State.TERMINAL

    # one-terminal invariant: nothing after terminal produces another terminal
    after = loop.handle(ctrl("model.result", {"model_request_id": "mreq_x", "output": "late"}, "frm_late"))
    assert all(f["type"] != "run.terminal" for f in after)


def test_completion_emits_exactly_one_terminal_frame() -> None:
    loop = make_loop("run_c")
    req = loop.handle(run_start())[0]
    out = loop.handle(ctrl("model.result", {"model_request_id": req["data"]["model_request_id"], "output": "done"}, "frm_f"))
    types = [f["type"] for f in out]
    assert types.count("run.terminal") == 1
    assert "output.item" in types
    assert out[-1]["type"] == "run.terminal"
    assert out[-1]["data"]["outcome"] == "completed"
    assert loop.state is State.TERMINAL


def test_full_run_frame_sequences_are_contiguous_and_in_envelope() -> None:
    emitter = Emitter(run_id="run_seq", attempt_id="att_1")
    frames = [protocol.build_ready(emitter, {"id": "frm_h", "type": "supervisor.hello", "data": {}}, nonce="n")]
    loop = Loop(emitter, Context("run_seq"))
    req = loop.handle(run_start())[0]
    frames.append(req)
    frames += loop.handle(ctrl("model.result", {"model_request_id": req["data"]["model_request_id"], "output": "ok"}, "frm_f"))

    assert [f["sequence"] for f in frames] == list(range(1, len(frames) + 1))
    for frame in frames:
        assert frame["protocol"] == "engine.v1"
        assert FRAME_ID.match(frame["id"])
        assert RFC3339.match(frame["time"])


def test_process_stdout_contains_json_frames_only() -> None:
    env = {**os.environ, "PYTHONPATH": str(SRC), "PALAI_RUN_ID": "run_io", "PALAI_ATTEMPT_ID": "att_io"}
    mrid = protocol.model_request_id("run_io", 1)
    stdin = "\n".join(
        [
            (FIXTURES / "supervisor-hello.jsonl").read_text().strip(),
            json.dumps(run_start()),
            json.dumps(ctrl("model.result", {"model_request_id": mrid, "output": "done"}, "frm_f")),
        ]
    ) + "\n"

    proc = subprocess.run(
        [sys.executable, "-m", "palai_engine"],
        input=stdin,
        env=env,
        capture_output=True,
        text=True,
        timeout=15,
    )

    assert proc.returncode == 0, proc.stderr
    lines = [line for line in proc.stdout.splitlines() if line]
    parsed = [json.loads(line) for line in lines]  # raises if any line is not JSON
    assert parsed[0]["type"] == "engine.ready"
    assert [f["type"] for f in parsed].count("run.terminal") == 1
    assert proc.stderr  # structured human logs go to stderr, never stdout
