"""Stdio wiring for the reference engine.

stdin  = controller frames (JSONL, one per line)
stdout = engine frames (JSONL, one per line) and NOTHING else
stderr = structured, redacted human logs

The engine handshakes (supervisor.hello -> engine.ready) before it reads any run
input, then drives loop.Loop until it emits a terminal frame.
"""

from __future__ import annotations

import os
import re
import secrets
import sys
from collections.abc import Iterable

from . import protocol
from .context import Context
from .loop import Loop, State
from .protocol import Emitter, ProtocolError

_SECRET_KEY = re.compile(r"key|token|secret|password|authorization|credential", re.IGNORECASE)
_MAX_LOG_VALUE = 128


def redact(fields: dict) -> dict:
    """Mask secret-shaped keys and truncate long values before logging. The engine
    receives no secrets by design; this keeps that true even if a payload leaks in."""
    safe: dict = {}
    for key, value in fields.items():
        if _SECRET_KEY.search(key):
            safe[key] = "***"
        elif isinstance(value, str) and len(value) > _MAX_LOG_VALUE:
            safe[key] = value[:_MAX_LOG_VALUE] + "..."
        else:
            safe[key] = value
    return safe


def _log(event: str, **fields: object) -> None:
    record = {"event": event, **redact(fields)}
    print(protocol.encode(record), file=sys.stderr, flush=True)


def _emit(frames: Iterable[dict]) -> None:
    for frame in frames:
        sys.stdout.write(protocol.encode(frame) + "\n")
        sys.stdout.flush()


def main() -> int:
    run_id = os.environ.get("PALAI_RUN_ID", "")
    attempt_id = os.environ.get("PALAI_ATTEMPT_ID", "")
    emitter = Emitter(run_id=run_id, attempt_id=attempt_id)

    first = sys.stdin.readline()
    if not first:
        _log("handshake_missing")
        return 1
    try:
        hello = protocol.decode(first)
        protocol.require_hello(hello)
    except ProtocolError as exc:
        _emit([emitter.build("protocol.error", {"code": exc.code, "message": exc.message})])
        _log("handshake_denied", code=exc.code)
        return 1

    _emit([protocol.build_ready(emitter, hello, nonce=secrets.token_hex(8))])
    _log("engine_ready", run_id=run_id)

    loop = Loop(emitter, Context(run_id), log=_log)
    for line in sys.stdin:
        if not line.strip():
            continue
        try:
            frame = protocol.decode(line)
        except ProtocolError as exc:
            _emit([emitter.build("protocol.error", {"code": exc.code, "message": exc.message})])
            continue
        _emit(loop.handle(frame))
        if loop.state is State.TERMINAL:
            break
    return 0


if __name__ == "__main__":
    sys.exit(main())
