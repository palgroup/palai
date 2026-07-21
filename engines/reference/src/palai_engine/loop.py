"""The deterministic reference run loop (spec §25.10).

Model and tool decisions advance only via controller result frames: this engine
never imports a provider SDK. Every transition rests at a safe boundary
(spec §25.11), so cancellation and the single-terminal invariant hold by
construction.
"""

from __future__ import annotations

import base64
from collections.abc import Callable
from enum import Enum

from . import checkpoint, commands, output, protocol
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


def _delegation_content(data: dict, spec: dict) -> str:
    """The typed delegation result string a child folds back as (spec §25.19): a completed child's
    output with its run linkage, or an optional child's skip note — so the parent can identify the
    delegation and its child_run. Mirrors context.add_child_result's user-role form for the tool-role
    answer a model-driven agent tool_call gets."""
    role, child_run = spec.get("role"), data.get("child_run_id")
    if data.get("status") == "completed":
        return f"[delegation result role={role} child_run={child_run}] {data.get('output')}"
    return f"[delegation skipped role={role} reason={data.get('reason')}]"


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
        # Model-driven agent tool_calls (spec §25.18, DEL-001): child_request_id -> (tool_call_id, spec).
        # Their child result answers the originating tool_call as a tool-role result, so the turn awaits
        # them alongside ordinary tools and no tool_call is left unanswered on a real provider.
        self._agent_children: dict[str, tuple[str, dict]] = {}

    def handle(self, frame: dict) -> list[dict]:
        type_ = frame.get("type")

        # One-terminal invariant: nothing after terminal produces another terminal.
        if self.state is State.TERMINAL:
            return [self._error("run_already_terminal", f"{type_!r} ignored after terminal")]

        # Cancellation is honored at the current safe boundary. Every awaiting_*
        # state is a boundary: the engine holds no in-flight side effect of its own.
        if type_ == "run.cancel":
            return self._cancel(frame)

        # An explicit checkpoint request (spec §26.5) is answered from the current safe boundary
        # without advancing the run: the offer captures the loop state as it is, and the run
        # continues. The control plane requests one before a pause/drain and on demand.
        if type_ == "checkpoint.request":
            return [self._checkpoint_offer("request")]

        # A steered/queued message is folded at the current safe boundary (spec §9.2, §25.11):
        # it surfaces in the next model request, never mid-step. Rejected before run.start —
        # there is no conversation to fold into yet.
        if type_ == "message.deliver":
            if self.state is State.AWAITING_START:
                return [self._error("unexpected_frame", "message.deliver before run.start")]
            return commands.deliver(self.context, frame)

        if self.state is State.AWAITING_START and type_ == "run.restore":
            return self._restore(frame)
        if self.state is State.AWAITING_START and type_ == "run.start":
            return self._begin(frame)
        if self.state is State.AWAITING_MODEL and type_ == "model.result":
            return self._on_model_result(frame)
        if self.state is State.AWAITING_TOOLS and type_ == "tool.result":
            return self._on_tool_result(frame)
        # child.result is accepted while awaiting children (config-seeded) OR while awaiting tools (a
        # model-driven agent tool_call's child answers its tool_call in a possibly-mixed tool turn).
        if type_ == "child.result" and self.state in (State.AWAITING_CHILDREN, State.AWAITING_TOOLS):
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
        self._model_request_id = protocol.model_request_id(self.emitter.run_id, self._step)
        return [self._model_request_frame()]

    def _model_request_frame(self) -> dict:
        """Build the model.request for the CURRENT step and enter AWAITING_MODEL. Shared by the
        forward path (after _request_model advances the step) and restore (re-emitting the model
        step a checkpoint was awaiting, at the SAME step — no re-advance, no re-flush)."""
        payload = self.context.model_request()
        data = {
            "model_request_id": self._model_request_id,
            "request_hash": protocol.content_hash(payload),
            **payload,
        }
        frame = self.emitter.build("model.request", data)
        self.state = State.AWAITING_MODEL
        return frame

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
        # A turn's tool_calls are dispatched together (spec §25.18, DEL-001): ordinary ones as
        # tool.requests, `agent`/`spawn` ones as child.requests whose result answers the tool_call.
        # Mixed turns are handled in one place, so no agent tool_call is ever dropped/unanswered.
        if tool_calls:
            return self._request_tools(tool_calls, reply_to=frame.get("id"))
        # Dispatch any config-seeded delegations before finishing (spec §25.18): the run gathers its
        # child results, then a final model step folds them in. Skipped once dispatched, so the
        # resumed final step finishes normally.
        if self._delegations and not self._delegations_dispatched:
            self._delegations_dispatched = True
            return self._request_children(self._delegations, reply_to=frame.get("id"))
        return self._finish(data, reply_to=frame.get("id"))

    def _request_tools(self, tool_calls: list[dict], *, reply_to: str | None) -> list[dict]:
        """Dispatch a turn's tool_calls (spec §25.18). An ordinary call becomes a tool.request; an
        `agent`/`spawn` call becomes a child.request whose result answers the SAME tool_call as a
        tool-role result. The whole turn awaits in _pending_tools, so a mixed turn resumes only once
        every tool_call — ordinary and agent alike — is answered (no dropped/unanswered tool_call)."""
        self._pending_tools.clear()
        self._agent_children = {}
        frames: list[dict] = []
        for index, call in enumerate(tool_calls):
            call_id = protocol.tool_call_id(self.emitter.run_id, self._step, index)
            self._pending_tools.add(call_id)
            if call.get("name") in _AGENT_TOOL_NAMES:
                child_id = protocol.child_request_id(self.emitter.run_id, self._step, index)
                spec = _agent_call_to_spec(call)
                self._agent_children[child_id] = (call_id, spec)
                self.log("safe_boundary", boundary="before_child", child_request_id=child_id)
                frames.append(self._child_request_frame(child_id, spec, reply_to))
                continue
            self.log("safe_boundary", boundary="before_tool", tool_call_id=call_id)
            frames.append(self._tool_request_frame(call_id, call, reply_to))
        self.state = State.AWAITING_TOOLS
        return frames

    def _tool_request_frame(self, call_id: str, call: dict, reply_to: str | None) -> dict:
        """Build one tool.request frame (shared by the forward dispatch and the restore re-emit, so
        a re-derived request is byte-identical in name/arguments/request_hash to the original)."""
        return self.emitter.build(
            "tool.request",
            {
                "tool_call_id": call_id,
                "name": call.get("name"),
                "arguments": call.get("arguments") or {},
                "request_hash": protocol.content_hash([call.get("name"), call.get("arguments") or {}]),
            },
            reply_to=reply_to,
        )

    def _request_children(self, specs: list[dict], *, reply_to: str | None) -> list[dict]:
        """Emit one child.request per CONFIG-SEEDED delegation spec (spec §25.18). The controller
        admits each against depth/fan-out/budget/capability and dispatches a ChildRun, then replies
        child.result, which folds as a typed user-role result. (Model-driven agent tool_calls go
        through _request_tools, where their result answers the tool_call instead.)"""
        self._pending_children = {}
        frames: list[dict] = []
        for index, spec in enumerate(specs):
            child_id = protocol.child_request_id(self.emitter.run_id, self._step, index)
            self._pending_children[child_id] = spec
            self.log("safe_boundary", boundary="before_child", child_request_id=child_id)
            frames.append(self._child_request_frame(child_id, spec, reply_to))
        self.state = State.AWAITING_CHILDREN
        return frames

    def _child_request_frame(self, child_id: str, spec: dict, reply_to: str | None) -> dict:
        """Build one child.request frame from a delegation spec (shared by the config-seeded and
        model-driven delegation paths)."""
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
        return self.emitter.build("child.request", data, reply_to=reply_to)

    def _on_child_result(self, frame: dict) -> list[dict]:
        data = frame.get("data") or {}
        child_id = data.get("child_request_id")
        # Model-driven delegation (DEL-001): the child answers an agent tool_call. A required child
        # that could not be served fails the run (SUB-003); otherwise its typed output folds as the
        # tool-role RESULT of that tool_call, and the turn resumes once every tool_call is answered.
        if child_id in self._agent_children:
            call_id, spec = self._agent_children.pop(child_id)
            self.log("safe_boundary", boundary="after_child", child_request_id=child_id)
            if data.get("status") != "completed" and spec.get("required"):
                return [self._terminal("failed", reason=data.get("reason") or "required_delegation_unmet", reply_to=frame.get("id"))]
            self.context.add_tool_result({"tool_call_id": call_id, "content": _delegation_content(data, spec)})
            self._pending_tools.discard(call_id)
            if self._pending_tools:
                return []  # still awaiting other tool results in this turn
            # A completed delegation turn is a §26.5 checkpoint boundary too — offer BEFORE the next
            # model step so the newest checkpoint always sits at the last committed step (MUST-FIX #1).
            # Without it, consecutive delegation steps leave the checkpoint behind committed steps, and
            # a restore's boundary gate would mislabel a replayed step's boundary as live (§26.9).
            return [self._checkpoint_offer("child"), *self._request_model()]
        # Config-seeded delegation (spec §25.18): folds as a typed user-role result.
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
        # Offer a checkpoint at this completed-delegation boundary before resuming (MUST-FIX #1): the
        # newest checkpoint must stay at the last committed step so a restore resumes at the live
        # frontier, never behind replayed steps.
        return [self._checkpoint_offer("child"), *self._request_model()]  # resume: fold the child results in

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
        # A completed tool turn is a §26.5 checkpoint boundary. Offer the checkpoint of the
        # tool-boundary state FIRST, THEN request the next model step: the control plane reads
        # frames in order, so it persists the checkpoint at the correct tool-boundary journal seq
        # and durably BEFORE the (possibly long) provider call — a crash mid-call keeps the offered
        # checkpoint. The engine is single-threaded, so this is pure emit order. On restore (T4) the
        # loop re-derives the next model request from the captured context (model_request_id is
        # deterministic per (run, step)). Every completed-tool offer is honest; which to persist
        # (external side-effecting tools only) is the control plane's call — per-tool side-effect
        # classification is T7. Delegation-completion resumes do not offer — a child spawn is not the
        # external side-effecting tool §26.5 names.
        offer = self._checkpoint_offer("tool")
        return [offer, *self._request_model()]  # resume: next model request for the next step

    def _finish(self, data: dict, *, reply_to: str | None) -> list[dict]:
        self.state = State.VALIDATING_OUTPUT
        try:
            items = output.output_items(data)
        except protocol.ProtocolError as exc:
            return [self._terminal("failed", reason=exc.code, reply_to=reply_to)]
        frames = [self.emitter.build("output.item", item, reply_to=reply_to) for item in items]
        frames.append(self._terminal("completed", output_value=data.get("output"), reply_to=reply_to))
        return frames

    def _checkpoint_offer(self, boundary_kind: str) -> dict:
        """Build a checkpoint.offer for the current loop state (spec §26.2). The bytes are opaque
        to the control plane; it stores + checksums them but never interprets them (§26.1)."""
        return self.emitter.build("checkpoint.offer", checkpoint.offer_data(self.capture_state(), boundary_kind))

    def capture_state(self) -> dict:
        """Snapshot the resumable loop + context state as a typed, JSON-serializable dict (spec
        §26.1). NOT pickle: the same state serializes deterministically, so a checkpoint is
        portable and content-addressable, and a restore reconstructs this exact boundary."""
        return {
            "state": self.state.value,
            "step": self._step,
            "model_request_id": self._model_request_id,
            "pending_tools": sorted(self._pending_tools),
            "delegations": self._delegations,
            "delegations_dispatched": self._delegations_dispatched,
            "pending_children": self._pending_children,
            # tuple -> list so it survives JSON; restore_state rebuilds the (call_id, spec) pair.
            "agent_children": {cid: [call_id, spec] for cid, (call_id, spec) in self._agent_children.items()},
            "context": self.context.capture(),
        }

    def restore_state(self, state: dict) -> None:
        """Reconstruct a loop from a captured state dict (spec §26.3 portable-checkpoint restore).
        Applied to a fresh loop with the same emitter/context identity; the run continues from the
        captured boundary as if it had never stopped."""
        self.state = State(state["state"])
        self._step = state["step"]
        self._model_request_id = state["model_request_id"]
        self._pending_tools = set(state["pending_tools"])
        self._delegations = list(state["delegations"])
        self._delegations_dispatched = state["delegations_dispatched"]
        self._pending_children = dict(state["pending_children"])
        self._agent_children = {cid: (pair[0], pair[1]) for cid, pair in state["agent_children"].items()}
        self.context.restore(state["context"])

    def _restore(self, frame: dict) -> list[dict]:
        """Reconstruct a fresh loop from a run.restore frame and re-derive the awaited boundary
        (spec §26.3 portable-checkpoint restore). The format_version is checked FIRST: an unknown
        one is a protocol.error and the loop is left untouched at AWAITING_START — never a silent
        half-restore. `state` is base64 of the opaque checkpoint bytes the control plane held."""
        data = frame.get("data") or {}
        fmt, version = data.get("format"), data.get("format_version")
        if fmt != checkpoint.FORMAT or version != checkpoint.FORMAT_VERSION:
            return [self._error("incompatible_checkpoint", f"cannot restore {fmt}/{version}: this engine writes {checkpoint.FORMAT_ID}")]
        try:
            captured = checkpoint.decode(base64.b64decode(data.get("state") or ""))
            # restore_state is INSIDE the try: a decodable-but-malformed state (a missing/typed key)
            # must be a protocol.error, never a KeyError that crashes the engine — "no silent
            # half-restore" (MUST-FIX #1 sibling, minor 7).
            self.restore_state(captured)
        except (ValueError, KeyError, TypeError) as exc:
            return [self._error("invalid_checkpoint", f"checkpoint state is not restorable: {exc}")]
        self.log("restored", state=self.state.value, step=self._step)
        return self._resume_frames()

    def _resume_frames(self) -> list[dict]:
        """Re-derive exactly the frames the run was awaiting when the checkpoint was cut (spec
        §26.3). Every awaiting_* state is a safe boundary; the resume re-emits only what the
        controller still needs to make progress, so completed effects are never replayed. On restore
        the model_request_id/tool_call_ids are deterministic per (run, step[, index]), so a
        re-derived request is idempotent with the original."""
        if self.state is State.AWAITING_MODEL:
            return [self._model_request_frame()]
        if self.state is State.AWAITING_TOOLS:
            if self._pending_tools:
                return self._reemit_pending_tools()
            # The tool turn was fully answered at the boundary → resume into the next model step.
            return self._request_model()
        if self.state is State.AWAITING_CHILDREN:
            return self._reemit_pending_children()
        return [self._error("unrestorable_state", f"cannot resume from {self.state.value}")]

    def _reemit_pending_tools(self) -> list[dict]:
        """Re-derive the tool.request (or child.request) for every STILL-outstanding call, from the
        last assistant turn's tool_calls filtered to _pending_tools — a completed tool was discarded
        from the set, so it gets no frame (no double-run). Agent tool_calls re-emit as child.requests
        (their answer is a tool-role result), ordinary ones as tool.requests."""
        agent_by_call = {call_id: (child_id, spec) for child_id, (call_id, spec) in self._agent_children.items()}
        frames: list[dict] = []
        for index, call in enumerate(self.context.last_tool_calls()):
            call_id = protocol.tool_call_id(self.emitter.run_id, self._step, index)
            if call_id not in self._pending_tools:
                continue
            if call_id in agent_by_call:
                child_id, spec = agent_by_call[call_id]
                frames.append(self._child_request_frame(child_id, spec, None))
            else:
                frames.append(self._tool_request_frame(call_id, call, None))
        return frames

    def _reemit_pending_children(self) -> list[dict]:
        """Re-derive the child.request for every outstanding config-seeded delegation (spec §25.18)."""
        return [self._child_request_frame(cid, spec, None) for cid, spec in self._pending_children.items()]

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
