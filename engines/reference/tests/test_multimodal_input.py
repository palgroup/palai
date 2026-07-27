"""A run whose input is a CONTENT ARRAY (spec §25.10's richer shape) crosses the engine untouched.

This is the engine's whole part in a run that carries an image, and the point of testing it is that the
part is *nothing*: `input` has no type constraint in engine.schema.json, Context.start appends it verbatim,
and the resolution of an image_ref to bytes happens control-plane-side (the object-store credential never
reaches the engine, spec §24, and a screenshot could not cross the 1 MiB frame ceiling anyway).

So these tests exist to pin "untouched" rather than to exercise behaviour. The failure they guard against is
a future change here that helpfully normalises, flattens or stringifies the input — which is exactly what
model_dispatch's asJSONString used to do one layer down, and it presented the user's own message to the model
as a lump of JSON.
"""

from __future__ import annotations

import json

from test_loop import ctrl, make_loop
from test_schema_pin import SCHEMA

# The shape a Slack-born run with a shared screenshot actually carries.
IMAGE_INPUT = [
    {"type": "input_text", "text": "ne yazıyor burada"},
    {"type": "image_ref", "artifact_id": "art_0123456789abcdef"},
]


def _user_turn(loop) -> dict:
    """The user turn Context.start appended, read off the captured conversation."""
    messages = loop.context.capture()["messages"]
    users = [m for m in messages if m["role"] == "user"]
    assert len(users) == 1, f"expected exactly one user turn, got {users}"
    return users[0]


def test_content_array_input_reaches_the_conversation_unchanged() -> None:
    loop = make_loop("run_img1")
    loop.handle(ctrl("run.start", {"input": IMAGE_INPUT}, "frm_start"))
    assert _user_turn(loop)["content"] == IMAGE_INPUT


def test_content_array_input_rides_the_model_request_verbatim() -> None:
    """The controller resolves image_ref -> bytes from THIS payload, so anything the engine did to it on the
    way would be a change to what the model is shown."""
    loop = make_loop("run_img2")
    out = loop.handle(ctrl("run.start", {"input": IMAGE_INPUT}, "frm_start"))
    request = next(f for f in out if f["type"] == "model.request")
    users = [m for m in request["data"]["messages"] if m["role"] == "user"]
    assert users == [{"role": "user", "content": IMAGE_INPUT}]


def test_content_array_input_survives_a_checkpoint_round_trip() -> None:
    """A paused-and-resumed image run must still be looking at the same image. capture() promises
    JSON-serializable and deterministic; a nested list of dicts is both, and this says so out loud."""
    loop = make_loop("run_img3")
    loop.handle(ctrl("run.start", {"input": IMAGE_INPUT}, "frm_start"))
    state = loop.context.capture()

    # Deterministic AND actually JSON: the checkpoint is bytes on the wire, not a Python object.
    assert json.loads(json.dumps(state)) == state

    restored = make_loop("run_img3")
    restored.context.restore(json.loads(json.dumps(state)))
    assert _user_turn(restored)["content"] == IMAGE_INPUT


def test_a_string_input_is_still_a_string() -> None:
    """The regression guard: every run that carries no image must be byte-identical to before."""
    loop = make_loop("run_img4")
    loop.handle(ctrl("run.start", {"input": "merhaba"}, "frm_start"))
    assert _user_turn(loop)["content"] == "merhaba"


def test_the_schema_puts_no_type_on_run_start_input() -> None:
    """engine.schema.json DESCRIBES run.start's `input` and does not type it, so a content array is a
    conformant frame rather than something this engine tolerates by accident. Read off the canonical schema
    rather than asserted about this engine, because the openness is the protocol's promise, not ours: adding
    `"type": "string"` there would silently make every image run non-conformant."""
    start = next(
        rule["then"]["properties"]["data"]
        for rule in SCHEMA["allOf"]
        if rule["if"]["properties"]["type"].get("const") == "run.start"
    )
    assert "input" in start["required"]
    assert "type" not in start["properties"]["input"], (
        "engine.schema.json now constrains run.start's input type; a run carrying an image sends an ARRAY"
    )
