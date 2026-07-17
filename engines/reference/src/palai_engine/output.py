"""Output validation and terminal-frame data (spec §25.8, §25.10 step 4).

A final model result must carry output content; the loop turns validated items
into output.item frames and closes with exactly one run.terminal.
"""

from __future__ import annotations

from .protocol import ProtocolError


def output_items(model_result: dict) -> list[dict]:
    """Validate a final model result and return canonical output items."""
    output = model_result.get("output")
    if output is None or output == "":
        raise ProtocolError("empty_output", "final model result declared no output content")
    return [{"type": "message", "content": output}]


def terminal_data(outcome: str, *, output: object = None, reason: str | None = None) -> dict:
    """Build the data for a run.terminal frame. ``outcome`` is one of completed,
    failed, canceled, timed_out, or budget_exceeded (spec §25.8)."""
    data: dict = {"outcome": outcome}
    if output is not None:
        data["output"] = output
    if reason is not None:
        data["reason"] = reason
    return data
