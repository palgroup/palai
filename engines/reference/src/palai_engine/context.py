"""Deterministic context assembly (spec §25.10 step 2, §25.12 layers).

Context is assembled in a fixed precedence so the same inputs always produce the
same model request, which is what makes model_request_id/request_hash stable
across a resume. This reference keeps two layers: immutable kernel instructions,
then the appended conversation (input, model results, tool results).
"""

from __future__ import annotations

from dataclasses import dataclass, field

KERNEL_INSTRUCTION = (
    "You are the Palai reference engine. Follow protocol and safety instructions; "
    "propose tool calls and produce final output, but never control lifecycle state."
)


@dataclass
class Context:
    run_id: str
    _messages: list[dict] = field(default_factory=list)
    # Messages delivered mid-run (send_message) buffer here until the next model request folds
    # them in — after the current step's tool results, so conversation order stays valid.
    _pending: list[dict] = field(default_factory=list)

    def start(self, run_start_data: dict) -> None:
        self._messages = [{"role": "system", "content": KERNEL_INSTRUCTION}]
        for message in run_start_data.get("messages", []):
            self._messages.append(message)
        if "input" in run_start_data:
            self._messages.append({"role": "user", "content": run_start_data["input"]})

    def queue_delivery(self, content: object) -> None:
        """Buffer a delivered user message (spec §9.2). It is folded in at the next model
        request by flush_deliveries, not immediately, so it never splits a step's assistant
        turn from its tool results."""
        self._pending.append({"role": "user", "content": content})

    def flush_deliveries(self) -> None:
        """Fold buffered deliveries into the conversation. Called by the loop right before it
        builds the next model request — the input boundary where a queued message takes effect."""
        self._messages.extend(self._pending)
        self._pending = []

    def add_model_result(self, data: dict) -> None:
        self._messages.append(
            {
                "role": "assistant",
                "content": data.get("output"),
                "tool_calls": data.get("tool_calls") or [],
            }
        )

    def add_partial_result(self, data: dict) -> None:
        """Append the partial assistant turn of an interrupt-aborted step (spec §9.2). It keeps
        whatever partial output the canceled call produced (often none) and is marked
        interrupted, so the transcript records the boundary before the run resumes."""
        self._messages.append({"role": "assistant", "content": data.get("output"), "interrupted": True})

    def add_tool_result(self, data: dict) -> None:
        self._messages.append(
            {
                "role": "tool",
                "tool_call_id": data.get("tool_call_id"),
                "content": data.get("content"),
            }
        )

    def model_request(self) -> dict:
        """The brokered model call payload. Deterministic given the messages so far."""
        return {"messages": list(self._messages)}
