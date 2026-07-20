"""The deterministic reference run loop (spec §25.10).

Model and tool decisions advance only via controller result frames: this engine
never imports a provider SDK. Every transition rests at a safe boundary
(spec §25.11), so cancellation and the single-terminal invariant hold by
construction.
"""

from __future__ import annotations

from collections.abc import Callable
from enum import Enum

from . import commands, output, protocol
from .context import Context
from .protocol import Emitter


class State(str, Enum):
    AWAITING_START = "awaiting_start"
    BEFORE_MODEL = "before_model"
    AWAITING_MODEL = "awaiting_model"
    AWAITING_TOOLS = "awaiting_tools"
    AWAITING_CHILDREN = "awaiting_children"
    VALIDATING_OUTPUT = "validating_output"
    TERMINAL = "terminal"


def _noop(event: str, **fields: object) -> None:
    pass


# The built-in tool names that mean "delegate to a child" (spec §25.18, master plan line 410). A
# model tool_call with one of these becomes a child.request, not a tool.request.
_AGENT_TOOL_NAMES = ("agent", "spawn")


def _agent_call_to_spec(call: dict) -> dict:
    """Map an `agent`/`spawn` tool_call's arguments onto the delegation spec _request_children emits
    as a child.request — the same shape a config-seeded delegation carries (spec §25.18)."""
    args = call.get("arguments") or {}
    return {
        "role": args.get("role"),
        "objective": args.get("objective"),
        "model": args.get("model"),
        "tools": args.get("tools") or [],
        "budget": args.get("budget") or {},
        "workspace_mode": args.get("workspace_mode", "none"),
        "required": bool(args.get("required")),
        "deadline": args.get("deadline"),
    }


class Loop:
    """One run's state machine. ``handle`` consumes one controller frame and returns
    the engine frames it triggers. The loop starts after a successful handshake."""

    def __init__(self, emitter: Emitter, context: Context, *, log: Callable[..., None] = _noop) -> None:
        self.emitter = emitter
        self.context = context
        self.state = State.AWAITING_START
        self.log = log
        self._step = 0
        self._model_request_id: str | None = None
        self._pending_tools: set[str] = set()
        # Required delegations seeded from run.start (spec §25.18): the controller carries them in
        # config so a real single-step run still delegates. Dispatched once, after the first model
        # step, then folded back as child.result before the run's final step.
        self._delegations: list[dict] = []
        self._delegations_dispatched = False
        self._pending_children: dict[str, dict] = {}

    def handle(self, frame: dict) -> list[dict]:
        type_ = frame.get("type")

        # One-terminal invariant: nothing after terminal produces another terminal.
        if self.state is State.TERMINAL:
            return [self._error("run_already_terminal", f"{type_!r} ignored after terminal")]

        # Cancellation is honored at the current safe boundary. Every awaiting_*
        # state is a boundary: the engine holds no in-flight side effect of its own.
        if type_ == "run.cancel":
            return self._cancel(frame)

        # A steered/queued message is folded at the current safe boundary (spec §9.2, §25.11):
        # it surfaces in the next model request, never mid-step. Rejected before run.start —
        # there is no conversation to fold into yet.
        if type_ == "message.deliver":
            if self.state is State.AWAITING_START:
                return [self._error("unexpected_frame", "message.deliver before run.start")]
            return commands.deliver(self.context, frame)

        if self.state is State.AWAITING_START and type_ == "run.start":
            return self._begin(frame)
        if self.state is State.AWAITING_MODEL and type_ == "model.result":
            return self._on_model_result(frame)
        if self.state is State.AWAITING_TOOLS and type_ == "tool.result":
            return self._on_tool_result(frame)
        if self.state is State.AWAITING_CHILDREN and type_ == "child.result":
            return self._on_child_result(frame)
        return [self._error("unexpected_frame", f"{type_!r} is not accepted in {self.state.value}")]

    def _begin(self, frame: dict) -> list[dict]:
        data = frame.get("data") or {}
        self._delegations = list(data.get("delegations") or [])
        self.context.start(data)
        return self._request_model()

    def _request_model(self) -> list[dict]:
        self.state = State.BEFORE_MODEL
        self._step += 1
        # Fold any messages delivered since the last request in here, at the input boundary,
        # so a queued/steered message becomes part of this step's context (spec §9.2).
        self.context.flush_deliveries()
        self.log("safe_boundary", boundary="before_model", step=self._step)
        payload = self.context.model_request()
        self._model_request_id = protocol.model_request_id(self.emitter.run_id, self._step)
        data = {
            "model_request_id": self._model_request_id,
            "request_hash": protocol.content_hash(payload),
            **payload,
        }
        frame = self.emitter.build("model.request", data)
        self.state = State.AWAITING_MODEL
        return [frame]

    def _on_model_result(self, frame: dict) -> list[dict]:
        data = frame.get("data") or {}
        if data.get("model_request_id") != self._model_request_id:
            return [self._error("uncorrelated_model_result", "model_request_id does not match the outstanding request")]
        self.log("safe_boundary", boundary="after_model", step=self._step)

        # An interrupted step was aborted in flight by the controller (spec §9.2, §25.11):
        # record the partial turn and resume in a NEW step, folding any delivered message in,
        # rather than finishing on this (incomplete) result.
        if data.get("interrupted"):
            self.context.add_partial_result(data)
            return self._request_model()

        self.context.add_model_result(data)

        tool_calls = data.get("tool_calls") or []
        # DEL-001 (spec §25.18, master plan line 410): an `agent`/`spawn` tool_call is MODEL-driven
        # delegation — it becomes a child.request the controller admits (admitChild/ChildRun), not a
        # tool.request. Ordinary tool_calls still dispatch as tools; a turn resolves its tools first
        # and the model can delegate on the next step (one kind of turn at a time).
        agent_calls = [c for c in tool_calls if c.get("name") in _AGENT_TOOL_NAMES]
        other_calls = [c for c in tool_calls if c.get("name") not in _AGENT_TOOL_NAMES]
        if other_calls:
            return self._request_tools(other_calls, reply_to=frame.get("id"))
        if agent_calls:
            return self._request_children([_agent_call_to_spec(c) for c in agent_calls], reply_to=frame.get("id"))
        # Dispatch any config-seeded delegations before finishing (spec §25.18): the run gathers its
        # child results, then a final model step folds them in. Skipped once dispatched, so the
        # resumed final step finishes normally.
        if self._delegations and not self._delegations_dispatched:
            self._delegations_dispatched = True
            return self._request_children(self._delegations, reply_to=frame.get("id"))
        return self._finish(data, reply_to=frame.get("id"))

    def _request_tools(self, tool_calls: list[dict], *, reply_to: str | None) -> list[dict]:
        self._pending_tools.clear()
        frames: list[dict] = []
        for index, call in enumerate(tool_calls):
            call_id = protocol.tool_call_id(self.emitter.run_id, self._step, index)
            self._pending_tools.add(call_id)
            self.log("safe_boundary", boundary="before_tool", tool_call_id=call_id)
            frames.append(
                self.emitter.build(
                    "tool.request",
                    {
                        "tool_call_id": call_id,
                        "name": call.get("name"),
                        "arguments": call.get("arguments") or {},
                        "request_hash": protocol.content_hash([call.get("name"), call.get("arguments") or {}]),
                    },
                    reply_to=reply_to,
                )
            )
        self.state = State.AWAITING_TOOLS
        return frames

    def _request_children(self, specs: list[dict], *, reply_to: str | None) -> list[dict]:
        """Emit one child.request per delegation spec (spec §25.18) — config-seeded or model-driven
        (an agent tool_call). The controller admits each against depth/fan-out/budget/capability and
        dispatches a ChildRun, then replies child.result."""
        self._pending_children = {}
        frames: list[dict] = []
        for index, spec in enumerate(specs):
            child_id = protocol.child_request_id(self.emitter.run_id, self._step, index)
            self._pending_children[child_id] = spec
            self.log("safe_boundary", boundary="before_child", child_request_id=child_id)
            data = {
                "child_request_id": child_id,
                "role": spec.get("role"),
                "objective": spec.get("objective"),
                "model": spec.get("model"),
                "tools": spec.get("tools") or [],
                "budget": spec.get("budget") or {},
                "workspace_mode": spec.get("workspace_mode", "none"),
                "required": bool(spec.get("required")),
                "request_hash": protocol.content_hash([spec.get("role"), spec.get("objective"), spec.get("model")]),
            }
            if spec.get("deadline") is not None:
                data["deadline"] = spec.get("deadline")
            frames.append(self.emitter.build("child.request", data, reply_to=reply_to))
        self.state = State.AWAITING_CHILDREN
        return frames

    def _on_child_result(self, frame: dict) -> list[dict]:
        data = frame.get("data") or {}
        child_id = data.get("child_request_id")
        if child_id not in self._pending_children:
            return [self._error("unknown_child_result", f"{child_id!r} is not an outstanding child request")]
        spec = self._pending_children.pop(child_id)
        self.log("safe_boundary", boundary="after_child", child_request_id=child_id)
        # A required delegation that could not be served (denied/failed) terminates the run failed —
        # no parent-only fake success (SUB-003, spec §25.18). An optional one is skipped and folded.
        if data.get("status") != "completed" and spec.get("required"):
            return [self._terminal("failed", reason=data.get("reason") or "required_delegation_unmet", reply_to=frame.get("id"))]
        self.context.add_child_result(data, spec)
        if self._pending_children:
            return []  # still awaiting the remaining children
        return self._request_model()  # resume: the final model step folds the child results in

    def _on_tool_result(self, frame: dict) -> list[dict]:
        data = frame.get("data") or {}
        call_id = data.get("tool_call_id")
        if call_id not in self._pending_tools:
            return [self._error("unknown_tool_call", f"{call_id!r} is not an outstanding tool call")]
        self.log("safe_boundary", boundary="after_tool", tool_call_id=call_id)
        self.context.add_tool_result(data)
        self._pending_tools.discard(call_id)
        if self._pending_tools:
            return []  # still awaiting the remaining tool results
        return self._request_model()  # resume: next model request for the next step

    def _finish(self, data: dict, *, reply_to: str | None) -> list[dict]:
        self.state = State.VALIDATING_OUTPUT
        try:
            items = output.output_items(data)
        except protocol.ProtocolError as exc:
            return [self._terminal("failed", reason=exc.code, reply_to=reply_to)]
        frames = [self.emitter.build("output.item", item, reply_to=reply_to) for item in items]
        frames.append(self._terminal("completed", output_value=data.get("output"), reply_to=reply_to))
        return frames

    def _cancel(self, frame: dict) -> list[dict]:
        reason = (frame.get("data") or {}).get("reason", "canceled")
        self.log("safe_boundary", boundary="cancel", state=self.state.value)
        return [self._terminal("canceled", reason=reason, reply_to=frame.get("id"))]

    def _terminal(self, outcome: str, *, output_value: object = None, reason: str | None = None, reply_to: str | None = None) -> dict:
        self.state = State.TERMINAL
        return self.emitter.build("run.terminal", output.terminal_data(outcome, output=output_value, reason=reason), reply_to=reply_to)

    def _error(self, code: str, message: str) -> dict:
        self.log("protocol_error", code=code)
        return self.emitter.build("protocol.error", {"code": code, "message": message})
