"""Controller command handling at safe boundaries (spec §9.2, §22.4).

The engine applies a command only at a safe boundary (spec §25.11). The one command this
reference handles is send_message: a message.deliver frame folds a user message into context
so the NEXT model request carries it — the input boundary between a model result and the next
model request. Delivery is deterministic and idempotent per frame: the command pump sends each
accepted command's frame exactly once (its deliver-once guarantee), so the engine simply folds.
"""

from __future__ import annotations

from .context import Context


def deliver(context: Context, frame: dict) -> list[dict]:
    """Buffer a delivered message into context and emit no engine frame. The observable effect
    is the message appearing in the next model.request; the loop folds it in at that boundary."""
    data = frame.get("data") or {}
    context.queue_delivery(data.get("message"))
    return []
