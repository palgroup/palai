"""The batch_size a turn's tool.requests carry (E31 parallel tool calls, Task 2).

The controller cannot see a turn's shape: it reads one frame at a time off a sequential channel, and
nothing in a tool.request has ever said whether another one was coming. So it ran each request as it
arrived and blocked on the result — three independent lookups cost three round trips even though the
model asked for them together.

batch_size is the missing sentence. These tests pin the one property that makes it safe to WAIT on:
the count is what the engine EMITS, so a controller that collects that many always gets that many.
"""

from __future__ import annotations

from palai_engine.loop import State

from test_loop import ctrl, make_loop, run_start
from test_restore import _restore_frame


def _tool_frames(frames: list[dict]) -> list[dict]:
    return [f for f in frames if f["type"] == "tool.request"]


def _turn(loop, calls: list[dict]) -> list[dict]:
    """Drive one model turn that asks for `calls`, returning the frames it emits."""
    req = loop.handle(run_start())[0]
    return loop.handle(
        ctrl("model.result", {"model_request_id": req["data"]["model_request_id"], "tool_calls": calls}, "frm_mr")
    )


def test_a_multi_call_turn_declares_its_size() -> None:
    frames = _tool_frames(
        _turn(
            make_loop("run_batch1"),
            [
                {"name": "palai.workspace.glob", "arguments": {"pattern": "**/*.go"}},
                {"name": "palai.workspace.grep", "arguments": {"pattern": "Handler"}},
                {"name": "palai.knowledge.retrieve", "arguments": {"q": "handler"}},
            ],
        )
    )
    assert len(frames) == 3
    for frame in frames:
        assert frame["data"]["batch_size"] == 3


def test_a_lone_call_declares_one() -> None:
    """The ordinary turn — every turn this tree has produced so far. It must stay a batch of one
    rather than gaining a special case, so the controller has exactly one code path."""
    frames = _tool_frames(_turn(make_loop("run_batch2"), [{"name": "palai.workspace.grep", "arguments": {}}]))
    assert len(frames) == 1
    assert frames[0]["data"]["batch_size"] == 1


def test_a_partial_restore_declares_what_it_reemits() -> None:
    """THE ONE THAT WOULD HANG. A restore re-emits only the calls still outstanding, so a turn of
    three can come back as one. If batch_size carried the TURN's size, a controller waiting for three
    would wait forever for two frames the engine deliberately never sent.

    The restored count must therefore be 1 — not 3, and not the 2 that were outstanding when the
    third answered."""
    run_id = "run_batch3"
    loop = make_loop(run_id)
    frames = _tool_frames(
        _turn(
            loop,
            [
                {"name": "alpha", "arguments": {"x": 1}},
                {"name": "beta", "arguments": {"y": 2}},
                {"name": "gamma", "arguments": {"z": 3}},
            ],
        )
    )
    assert [f["data"]["batch_size"] for f in frames] == [3, 3, 3]

    # Answer two of the three, then cut the run mid-turn.
    for frame in frames[:2]:
        loop.handle(ctrl("tool.result", {"tool_call_id": frame["data"]["tool_call_id"], "content": "done"}, "frm_t"))
    assert loop.state is State.AWAITING_TOOLS
    offer = loop.handle(ctrl("checkpoint.request", {}, "frm_cpr"))[0]

    reemitted = _tool_frames(make_loop(run_id).handle(_restore_frame(offer)))
    assert len(reemitted) == 1
    assert reemitted[0]["data"]["name"] == "gamma"
    assert reemitted[0]["data"]["batch_size"] == 1


def test_a_child_request_is_not_part_of_the_batch() -> None:
    """An agent tool_call becomes a child.request, which a ChildRun answers — the tool dispatcher
    never sees it. Counting it would make the controller await a tool frame that is not coming."""
    frames = _turn(
        make_loop("run_batch4"),
        [
            {"name": "palai.workspace.grep", "arguments": {"pattern": "x"}},
            {"name": "agent", "arguments": {"role": "reviewer", "prompt": "review"}},
        ],
    )
    assert [f["type"] for f in frames] == ["tool.request", "child.request"]
    assert frames[0]["data"]["batch_size"] == 1
    assert "batch_size" not in frames[1]["data"]
