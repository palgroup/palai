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
        """Append the partial assistant turn of an interrupt-aborted step (spec §9.2), keeping
        whatever partial output the canceled call produced and marking it interrupted so the
        transcript records the boundary before the run resumes. An interrupt that aborted before
        any output streamed has nothing to record: skip it, because an assistant turn with neither
        content nor tool calls is not a valid model message — a real provider rejects the resumed
        request (the interrupted boundary is still journaled as model_step.interrupted.v1)."""
        if not data.get("output"):
            return
        self._messages.append({"role": "assistant", "content": data.get("output"), "interrupted": True})

    def add_tool_result(self, data: dict) -> None:
        self._messages.append(
            {
                "role": "tool",
                "tool_call_id": data.get("tool_call_id"),
                "content": data.get("content"),
            }
        )

    def add_child_result(self, data: dict, spec: dict) -> None:
        """Fold a delegation result into context as a TYPED result the parent's next model step
        sees (spec §25.19) — a completed child's output, or an optional child's skip note. It is a
        user-role turn (always a valid provider message, unlike a tool turn with no matching call)
        carrying the child run linkage, so the parent's final output can identify the delegation."""
        role, child_run = spec.get("role"), data.get("child_run_id")
        if data.get("status") == "completed":
            content = f"[delegation result role={role} child_run={child_run}] {data.get('output')}"
        else:
            content = f"[delegation skipped role={role} reason={data.get('reason')}]"
        self._messages.append({"role": "user", "content": content, "child_run_id": child_run})

    def last_tool_calls(self) -> list[dict]:
        """The tool_calls of the most recent assistant turn (spec §26.3): a restore re-derives the
        outstanding tool.requests from here, keeping their name/arguments. Empty when the last
        assistant turn proposed no tool calls."""
        for message in reversed(self._messages):
            if message.get("role") == "assistant" and message.get("tool_calls"):
                return message["tool_calls"]
        return []

    def capture(self) -> dict:
        """Snapshot the conversation for a checkpoint (spec §26.1). Plain lists of dicts —
        JSON-serializable and deterministic, so the same context always serializes identically."""
        return {"messages": self._messages, "pending": self._pending}

    def restore(self, state: dict) -> None:
        """Reconstruct the conversation from a captured snapshot (spec §26.3)."""
        self._messages = list(state["messages"])
        self._pending = list(state["pending"])

    def model_request(self) -> dict:
        """The brokered model call payload. Deterministic given the messages so far."""
        return {"messages": list(self._messages)}
