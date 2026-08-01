"""Output validation and terminal-frame data (spec §25.8, §25.10 step 4, §22.7).

A final model result must carry output content; the loop turns validated items
into output.item frames and closes with exactly one run.terminal.

THE SCHEMA HALF (§22.7). When run.start carried an `output_contract`, the answer must also PARSE as
JSON and satisfy that schema, and a run whose answer does not is `failed` with reason
schema_validation_failed — not `completed`. Before 2026-08-01 this module validated only that the
output was non-empty, while `State.VALIDATING_OUTPUT` existed in loop.py and named a check that was
never performed; a caller who demanded a schema received prose and a completed status.

WHY THE ENGINE VALIDATES AT ALL, given the controller does too. The engine owns the loop and emits
the terminal frame, so it is the only participant that can end the run at the boundary where the
answer is produced, and the only one that could later spend §22.7's bounded repair step here. The
controller's validation is the independent backstop: `engine` is a caller-selectable request field,
so a third-party engine that ignores the contract must still not be able to produce a completed run
that violates it. The two are deliberately redundant and the redundancy is the point.

THE SUPPORTED SUBSET IS NOT AN ACCIDENT. It mirrors packages/outputcontract exactly, and it can be a
small subset safely because the controller REFUSES at admission any schema carrying a keyword that
subset cannot enforce. So every schema that reaches this file is one both validators check fully.
"""

from __future__ import annotations

import json
import math

from .protocol import ProtocolError


def output_items(model_result: dict) -> list[dict]:
    """Validate a final model result and return canonical output items."""
    output = model_result.get("output")
    if output is None or output == "":
        raise ProtocolError("empty_output", "final model result declared no output content")
    return [{"type": "message", "content": output}]


def validate_against_contract(contract: dict | None, output: object) -> None:
    """Raise ProtocolError('schema_validation_failed') unless output satisfies the run's contract.

    A None/empty contract, or one whose format is not json_schema, demands nothing and returns —
    which is the path every run that does not opt in takes, unchanged.
    """
    if not contract or contract.get("format") != "json_schema":
        return
    schema = contract.get("schema") or {}
    if not schema:
        return
    if not isinstance(output, str):
        raise ProtocolError(
            "schema_validation_failed",
            f"a schema was demanded but the run produced {_kind(output)} rather than a text answer",
        )
    text = output.strip()
    if not text:
        raise ProtocolError("schema_validation_failed", "a schema was demanded but the run produced no output")
    try:
        decoded = json.loads(text)
    except ValueError as exc:
        raise ProtocolError(
            "schema_validation_failed",
            f"the output is not JSON ({exc}); a schema was demanded, so the answer had to be a JSON document",
        ) from exc
    problem = _validate(schema, decoded, "")
    if problem is not None:
        raise ProtocolError("schema_validation_failed", problem)


def _validate(schema: dict, value: object, path: str) -> str | None:
    """Return the first way value fails schema, or None. Mirrors packages/outputcontract.Validate."""
    where = _at(path)
    typ = schema.get("type")

    if "enum" in schema:
        permitted = schema["enum"]
        if isinstance(permitted, list) and not any(_json_equal(value, c) for c in permitted):
            return f"{where} is {_render(value)}, which is not one of the permitted values {_render(permitted)}"

    if typ == "object":
        if not isinstance(value, dict):
            return f"{where} should be an object, got {_kind(value)}"
        props = schema.get("properties") or {}
        for name in schema.get("required") or []:
            if name not in value:
                return f'{where} is missing required property "{name}"'
        if schema.get("additionalProperties") is False:
            for key in sorted(value):
                if key not in props:
                    return f'{where} has property "{key}", which the schema does not permit'
        for name in sorted(props):
            if name in value and isinstance(props[name], dict):
                problem = _validate(props[name], value[name], _join(path, name))
                if problem is not None:
                    return problem
    elif typ == "array":
        if not isinstance(value, list):
            return f"{where} should be an array, got {_kind(value)}"
        problem = _bound(schema, "minItems", "maxItems", len(value), f"{where} length")
        if problem is not None:
            return problem
        items = schema.get("items")
        if isinstance(items, dict):
            for i, elem in enumerate(value):
                problem = _validate(items, elem, f"{path}[{i}]")
                if problem is not None:
                    return problem
    elif typ == "string":
        if not isinstance(value, str):
            return f"{where} should be a string, got {_kind(value)}"
        return _bound(schema, "minLength", "maxLength", len(value), f"{where} length")
    elif typ == "integer":
        # bool is a subclass of int in Python and is NOT an integer here: a schema asking for a
        # count must not be satisfied by `true`.
        if isinstance(value, bool) or not isinstance(value, (int, float)) or value != math.trunc(value):
            return f"{where} should be an integer, got {_kind(value)}"
        return _bound_number(schema, float(value), where)
    elif typ == "number":
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            return f"{where} should be a number, got {_kind(value)}"
        return _bound_number(schema, float(value), where)
    elif typ == "boolean":
        if not isinstance(value, bool):
            return f"{where} should be a boolean, got {_kind(value)}"
    elif typ == "null":
        if value is not None:
            return f"{where} should be null, got {_kind(value)}"
    return None


def _bound(schema: dict, min_key: str, max_key: str, n: int, where: str) -> str | None:
    low, high = schema.get(min_key), schema.get(max_key)
    if isinstance(low, (int, float)) and n < low:
        return f"{where} is {n}, below the minimum {low}"
    if isinstance(high, (int, float)) and n > high:
        return f"{where} is {n}, above the maximum {high}"
    return None


def _bound_number(schema: dict, f: float, where: str) -> str | None:
    low, high = schema.get("minimum"), schema.get("maximum")
    if isinstance(low, (int, float)) and f < low:
        return f"{where} is {_number(f)}, below the minimum {low}"
    if isinstance(high, (int, float)) and f > high:
        return f"{where} is {_number(f)}, above the maximum {high}"
    return None


def _json_equal(a: object, b: object) -> bool:
    return json.dumps(a, sort_keys=True) == json.dumps(b, sort_keys=True)


def _kind(value: object) -> str:
    """Name a decoded JSON value's type in the CALLER's vocabulary, not Python's — a message saying
    'got dict' is about our decoder, not about their document."""
    if value is None:
        return "null"
    if isinstance(value, bool):
        return "a boolean"
    if isinstance(value, str):
        return "a string"
    if isinstance(value, list):
        return "an array"
    if isinstance(value, dict):
        return "an object"
    if isinstance(value, int) or (isinstance(value, float) and value == math.trunc(value)):
        return "an integer"
    if isinstance(value, float):
        return "a number"
    return type(value).__name__


def _number(f: float) -> str:
    return str(int(f)) if f == math.trunc(f) else str(f)


def _render(value: object) -> str:
    rendered = json.dumps(value)
    return rendered if len(rendered) <= 120 else rendered[:117] + "..."


def _at(path: str) -> str:
    return "the output" if not path else "output." + path.lstrip(".")


def _join(path: str, name: str) -> str:
    return name if not path else f"{path}.{name}"


def terminal_data(outcome: str, *, output: object = None, reason: str | None = None) -> dict:
    """Build the data for a run.terminal frame. ``outcome`` is one of completed,
    failed, canceled, timed_out, or budget_exceeded (spec §25.8)."""
    data: dict = {"outcome": outcome}
    if output is not None:
        data["output"] = output
    if reason is not None:
        data["reason"] = reason
    return data
