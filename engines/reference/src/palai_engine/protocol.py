"""engine.v1 JSONL frame protocol: encode, decode, handshake, and stable IDs.

The frame envelope and the engine.ready handshake are defined by
``protocols/engine/engine.schema.json`` (spec §25.5-25.6). This module owns the
wire discipline; the loop (loop.py) owns run state.
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from datetime import datetime, timezone

from . import checkpoint

PROTOCOL = "engine.v1"
# Maximum line size, one MiB, matching engine.schema.json $defs.limits.max_line_bytes.
MAX_LINE_BYTES = 1_048_576

ENGINE_NAME = "palai-reference"
ENGINE_VERSION = "0.1.0"

# The command kinds this engine can apply at a safe boundary, announced in engine.ready
# (spec §9.1, §22.4). T2 delivers send_message (the message.deliver frame); config.change and
# the lifecycle commands are contracted in the schema but land with later phases, so declaring
# only what is really handled keeps the announcement honest (the schema-pin test enforces it).
SUPPORTED_COMMANDS = ("send_message",)


class ProtocolError(Exception):
    """A frame that violates the wire protocol. Reported as a protocol.error frame."""

    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


def now_rfc3339() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _stable_id(prefix: str, *parts: str) -> str:
    digest = hashlib.sha256("\x1f".join(parts).encode()).hexdigest()[:24]
    return f"{prefix}_{digest}"


def frame_id(run_id: str, sequence: int) -> str:
    return _stable_id("frm", "frame", run_id, str(sequence))


def model_request_id(run_id: str, step: int) -> str:
    """Stable logical model-request ID for a run step (spec §25.9). A resumed run
    re-derives the same ID for the same step, so a retransmission is idempotent."""
    return _stable_id("mreq", "model", run_id, str(step))


def tool_call_id(run_id: str, step: int, index: int) -> str:
    """Stable logical tool-call ID for a (step, index) pair (spec §25.9)."""
    return _stable_id("tcall", "tool", run_id, str(step), str(index))


def child_request_id(run_id: str, step: int, index: int) -> str:
    """Stable logical child-request ID for a (step, index) delegation (spec §25.18), so a
    resumed run re-derives the same id and a retransmitted child.request is idempotent."""
    return _stable_id("chld", "child", run_id, str(step), str(index))


def content_hash(payload: object) -> str:
    """Canonical hash of request content, used for the §25.9 same-ID/same-hash rule."""
    canonical = json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    return "sha256:" + hashlib.sha256(canonical.encode()).hexdigest()


def decode(line: str) -> dict:
    """Parse one inbound JSONL line into a frame object."""
    if len(line.encode()) > MAX_LINE_BYTES:
        raise ProtocolError("frame_too_large", "inbound frame exceeds max line bytes")
    try:
        frame = json.loads(line)
    except json.JSONDecodeError as exc:
        raise ProtocolError("invalid_json", "inbound frame is not JSON") from exc
    if not isinstance(frame, dict):
        raise ProtocolError("invalid_frame", "inbound frame is not an object")
    return frame


def encode(frame: dict) -> str:
    """Serialize one outbound frame to a single physical line (no trailing newline)."""
    line = json.dumps(frame, separators=(",", ":"), ensure_ascii=False)
    if len(line.encode()) > MAX_LINE_BYTES:
        raise ProtocolError("frame_too_large", "outbound frame exceeds max line bytes")
    return line


@dataclass
class Emitter:
    """Builds outbound engine frames with a monotonic sequence and run identity.

    engine.ready and every loop frame share one Emitter so stdout sequence numbers
    are contiguous, as the supervisor requires (packages/runner/supervisor.go)."""

    run_id: str
    attempt_id: str
    _sequence: int = 0

    def build(self, type_: str, data: dict, *, reply_to: str | None = None) -> dict:
        self._sequence += 1
        frame: dict = {
            "protocol": PROTOCOL,
            "id": frame_id(self.run_id, self._sequence),
            "type": type_,
            "sequence": self._sequence,
            "time": now_rfc3339(),
            "data": data,
        }
        if self.run_id:
            frame["run_id"] = self.run_id
        if self.attempt_id:
            frame["attempt_id"] = self.attempt_id
        if reply_to is not None:
            frame["reply_to"] = reply_to
        return frame


def require_hello(frame: dict) -> None:
    """Guard the handshake: the first controller frame must be supervisor.hello.
    No run input is accepted before a successful handshake (spec §25.6)."""
    if frame.get("type") != "supervisor.hello":
        raise ProtocolError(
            "handshake_required",
            f"expected supervisor.hello before run input, got {frame.get('type')!r}",
        )


def build_ready(emitter: Emitter, hello: dict, *, nonce: str) -> dict:
    """Build the engine.ready response to supervisor.hello (schema-required fields)."""
    return emitter.build(
        "engine.ready",
        {
            "selected_protocol": PROTOCOL,
            "engine": {"name": ENGINE_NAME, "version": ENGINE_VERSION},
            "max_frame_bytes": MAX_LINE_BYTES,
            "nonce": nonce,
            # The checkpoint formats this engine can WRITE and restore (spec §26.4). The schema-pin
            # test keeps this equal to checkpoint.FORMAT_ID, so the advertised list can never drift
            # from the format the engine actually serializes.
            "checkpoint_formats": [checkpoint.FORMAT_ID],
            "commands": list(SUPPORTED_COMMANDS),
            "content_types": {"input": ["text"], "output": ["text"]},
        },
        reply_to=hello.get("id"),
    )
