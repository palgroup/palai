"""The §22.7 output contract, at the boundary that owns the terminal frame.

Before 2026-08-01 `State.VALIDATING_OUTPUT` existed and named a check nothing performed: the loop
entered it, asserted only that the output was non-empty, and emitted `completed`. A caller who
demanded a JSON Schema received prose and a success status. These cases pin the check the state name
has always claimed, and pin the paths that must stay untouched for a run that demands nothing.
"""

from __future__ import annotations

import json

import pytest
from palai_engine import output
from palai_engine.context import Context
from palai_engine.loop import Loop, State
from palai_engine.protocol import Emitter, ProtocolError

CITY_SCHEMA = {
    "type": "object",
    "properties": {"city": {"type": "string"}, "population": {"type": "integer"}},
    "required": ["city", "population"],
    "additionalProperties": False,
}
CONTRACT = {"format": "json_schema", "name": "city_fact", "schema": CITY_SCHEMA, "strict": True}

# The literal answer the live stack returned on 2026-08-01 for a request carrying CITY_SCHEMA.
LIVE_PROSE = (
    "Ankara is the capital city of Turkey. As of my last update in October 2023, the approximate "
    "population of Ankara is around 5.7 million people."
)


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


def drive(contract: dict | None, answer: str) -> list[dict]:
    """Run one loop from run.start to terminal with a single final model result."""
    loop = make_loop()
    data = {"input": "Give the city of Ankara and its approximate population."}
    if contract is not None:
        data["output_contract"] = contract
    frames = loop.handle(ctrl("run.start", data, frame_id="frm_start"))
    request_id = frames[0]["data"]["model_request_id"]
    return loop.handle(ctrl("model.result", {"model_request_id": request_id, "output": answer}))


def terminal(frames: list[dict]) -> dict:
    return [f for f in frames if f["type"] == "run.terminal"][0]


# --- the loop --------------------------------------------------------------------------------


def test_prose_against_a_demanded_schema_fails_the_run():
    """The RED, at the engine boundary: this exact answer used to terminate `completed`."""
    term = terminal(drive(CONTRACT, LIVE_PROSE))
    assert term["data"]["outcome"] == "failed"
    assert term["data"]["reason"] == "schema_validation_failed"


def test_conforming_json_completes():
    term = terminal(drive(CONTRACT, '{"city":"Ankara","population":5663000}'))
    assert term["data"]["outcome"] == "completed"


def test_json_that_violates_the_schema_fails_the_run():
    """Parsing as JSON is not enough — the shape is the contract."""
    term = terminal(drive(CONTRACT, '{"city":"Ankara"}'))
    assert term["data"]["outcome"] == "failed"
    assert term["data"]["reason"] == "schema_validation_failed"


def test_a_run_with_no_contract_is_unchanged():
    """The opt-in fence: prose is a perfectly good answer when nothing was demanded."""
    term = terminal(drive(None, LIVE_PROSE))
    assert term["data"]["outcome"] == "completed"
    assert term["data"]["output"] == LIVE_PROSE


def test_the_failing_run_still_emits_exactly_one_terminal():
    """A rejected answer must not break the single-terminal invariant (spec §25.8)."""
    frames = drive(CONTRACT, LIVE_PROSE)
    assert len([f for f in frames if f["type"] == "run.terminal"]) == 1


def test_validating_output_state_is_actually_entered():
    loop = make_loop()
    frames = loop.handle(ctrl("run.start", {"input": "x", "output_contract": CONTRACT}, frame_id="frm_start"))
    request_id = frames[0]["data"]["model_request_id"]
    loop.handle(ctrl("model.result", {"model_request_id": request_id, "output": LIVE_PROSE}))
    assert loop.state is State.TERMINAL  # passed THROUGH validating_output to a terminal


# --- checkpoint/restore ----------------------------------------------------------------------


def test_the_contract_survives_capture_and_restore():
    """A restore that dropped the contract would resume a run whose answer is checked against
    nothing and terminate `completed` on prose — the defect, reintroduced by a crash."""
    loop = make_loop()
    loop.handle(ctrl("run.start", {"input": "x", "output_contract": CONTRACT}, frame_id="frm_start"))
    state = json.loads(json.dumps(loop.capture_state()))  # must survive a JSON round trip

    restored = make_loop()
    restored.restore_state(state)
    assert restored._output_contract == CONTRACT

    frames = restored.handle(
        ctrl("model.result", {"model_request_id": restored._model_request_id, "output": LIVE_PROSE})
    )
    assert terminal(frames)["data"]["reason"] == "schema_validation_failed"


def test_a_checkpoint_written_before_this_feature_restores_as_demanding_nothing():
    loop = make_loop()
    loop.handle(ctrl("run.start", {"input": "x"}, frame_id="frm_start"))
    state = loop.capture_state()
    del state["output_contract"]  # a pre-000052 checkpoint has no such key

    restored = make_loop()
    restored.restore_state(state)
    assert restored._output_contract is None


# --- the validator ---------------------------------------------------------------------------


def test_contract_that_demands_nothing_is_a_no_op():
    for contract in (None, {}, {"format": "text"}, {"format": "json_schema", "schema": {}}):
        output.validate_against_contract(contract, LIVE_PROSE)


@pytest.mark.parametrize(
    "answer,fragment",
    [
        (LIVE_PROSE, "not JSON"),
        ('{"city":"Ankara"}', 'missing required property "population"'),
        ('{"city":"Ankara","population":"lots"}', "should be an integer"),
        ('{"city":"Ankara","population":1.5}', "should be an integer"),
        ('{"city":"Ankara","population":1,"x":2}', "does not permit"),
        ('["Ankara"]', "should be an object"),
        ("   ", "no output"),
    ],
)
def test_validator_names_the_exact_failure(answer, fragment):
    with pytest.raises(ProtocolError) as excinfo:
        output.validate_against_contract(CONTRACT, answer)
    assert excinfo.value.code == "schema_validation_failed"
    assert fragment in excinfo.value.message


def test_a_boolean_does_not_satisfy_an_integer():
    """bool is a subclass of int in Python; `true` must not answer a schema asking for a count."""
    with pytest.raises(ProtocolError):
        output.validate_against_contract(CONTRACT, '{"city":"Ankara","population":true}')


def test_errors_are_in_the_callers_vocabulary():
    with pytest.raises(ProtocolError) as excinfo:
        output.validate_against_contract(CONTRACT, '{"city":123,"population":1}')
    message = excinfo.value.message
    # Python's own type names, as whole words — "str" must not match inside "a string".
    assert not set(message.replace(",", " ").split()) & {"dict", "str", "int", "float", "list", "NoneType"}
    assert "output.city" in message
    assert "should be a string, got an integer" in message


def test_nested_array_and_enum_are_enforced():
    schema = {
        "type": "object",
        "properties": {
            "meta": {
                "type": "object",
                "properties": {"region": {"type": "string", "enum": ["eu", "asia"]}},
                "required": ["region"],
            },
            "tags": {"type": "array", "items": {"type": "string"}, "maxItems": 2},
        },
        "required": ["meta"],
    }
    contract = {"format": "json_schema", "schema": schema}
    output.validate_against_contract(contract, '{"meta":{"region":"eu"},"tags":["a"]}')
    for bad in ('{"meta":{"region":"mars"}}', '{"meta":{"region":"eu"},"tags":["a","b","c"]}',
                '{"meta":{"region":"eu"},"tags":[1]}'):
        with pytest.raises(ProtocolError):
            output.validate_against_contract(contract, bad)
